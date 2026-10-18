package egress

import (
	"strings"
	"testing"
)

func TestLockScript(t *testing.T) {
	s := LockScript(65532)
	for _, want := range []string{
		"-N OF_EGRESS_LOCK",
		"-A OF_EGRESS_LOCK -o lo -j RETURN",
		"-A OF_EGRESS_LOCK -m owner --uid-owner 65532 -j RETURN",
		"-A OF_EGRESS_LOCK -p tcp -j REJECT --reject-with tcp-reset",
		"-A OUTPUT -p tcp -m multiport --dports 80,443 -j OF_EGRESS_LOCK",
		"lock iptables",
		"lock ip6tables",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	// The order matters: the loopback and proxy rules come before the reject rule.
	if strings.Index(s, "-o lo") > strings.Index(s, "REJECT") || strings.Index(s, "--uid-owner") > strings.Index(s, "REJECT") {
		t.Error("the reject rule must be the last rule of the chain")
	}
	if !strings.HasPrefix(s, "set -eu") {
		t.Error("the script must stop at the first error")
	}
}

func TestLockContainer(t *testing.T) {
	c := Lock(LockParams{Image: "img:1", PullPolicy: "Always", ProxyUser: 65533})
	if c["name"] != LockName || c["image"] != "img:1" || c["imagePullPolicy"] != "Always" {
		t.Fatalf("lock = %v", c)
	}
	if c["restartPolicy"] != nil {
		t.Error("the lock must end before the next container")
	}
	if !strings.Contains(c["command"].([]any)[2].(string), "--uid-owner 65533") {
		t.Errorf("command = %v", c["command"])
	}
}

func TestWithLockAndSidecarOrder(t *testing.T) {
	init := []any{map[string]any{"name": "copy"}, map[string]any{"name": LockName, "image": "old"}}
	init = WithSidecar(init, map[string]any{"name": ProxySidecarName})
	init = WithLock(init, map[string]any{"name": LockName, "image": "new"})
	var names []string
	for _, c := range init {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "egress-lock,egress-proxy,copy" {
		t.Fatalf("order = %v", names)
	}
	if init[0].(map[string]any)["image"] != "new" {
		t.Error("the lock of the template must be replaced")
	}
}

func TestProxyUserAvoiding(t *testing.T) {
	if got := ProxyUserAvoiding(65532, 65533, 1000); got != 65534 {
		t.Errorf("got %d, want 65534", got)
	}
	if got := ProxyUserAvoiding(); got != DefaultProxyUser {
		t.Errorf("got %d", got)
	}
}

func TestProxyUserFor(t *testing.T) {
	if ProxyUserFor(1000) != 65532 || ProxyUserFor(0) != 65532 || ProxyUserFor(65532) != 65533 {
		t.Error("the proxy user must differ from the task user")
	}
}

func TestLockControlViaProxy(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://of.example.com":      true,
		"http://of.example.com":       true,
		"https://of.example.com:8443": false,
		"http://of.svc:8080":          false,
		"http://of.svc:80":            true,
		"http://10.0.0.5:443":         true,
		"":                            false,
	} {
		if got := LockControlViaProxy(raw); got != want {
			t.Errorf("%q = %v, want %v", raw, got, want)
		}
	}
}

func TestValidateLock(t *testing.T) {
	for _, v := range []string{"", "none", "iptables", "IPTABLES"} {
		if err := ValidateLock(v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	if ValidateLock("nft") == nil {
		t.Error("an unknown lock must fail")
	}
	if !LockEnabled("iptables") || LockEnabled("none") || LockEnabled("") {
		t.Error("LockEnabled")
	}
}

func TestHardenTask(t *testing.T) {
	c := map[string]any{"name": "sandbox"}
	HardenTask(c)
	if c["securityContext"].(map[string]any)["allowPrivilegeEscalation"] != false {
		t.Error("allowPrivilegeEscalation must be false")
	}
	// A value of the template stays.
	c = map[string]any{"securityContext": map[string]any{"allowPrivilegeEscalation": true}}
	HardenTask(c)
	if c["securityContext"].(map[string]any)["allowPrivilegeEscalation"] != true {
		t.Error("an explicit template value must stay")
	}
	// runAsNonRoot and the dropped capabilities always win over the template.
	c = map[string]any{"securityContext": map[string]any{"runAsNonRoot": false,
		"capabilities": map[string]any{"add": []any{"SETUID", "CAP_SETGID", "NET_BIND_SERVICE"}, "drop": []any{"NET_RAW"}}}}
	HardenTask(c)
	sc := c["securityContext"].(map[string]any)
	if sc["runAsNonRoot"] != true {
		t.Errorf("runAsNonRoot = %v", sc["runAsNonRoot"])
	}
	caps := sc["capabilities"].(map[string]any)
	if d := caps["drop"].([]any); len(d) != 1 || d[0] != "ALL" {
		t.Errorf("drop = %v", d)
	}
	if a := caps["add"].([]any); len(a) != 1 || a[0] != "NET_BIND_SERVICE" {
		t.Errorf("add = %v", a)
	}
}

func TestSidecarAlwaysAllow(t *testing.T) {
	c := Sidecar(SidecarParams{Image: "i", User: 1, AlwaysAllow: []string{"of.example.com", ""}})
	cmd := c["command"].([]any)
	n := 0
	for i, a := range cmd {
		if a == "--always-allow" {
			n++
			if cmd[i+1] != "of.example.com" {
				t.Errorf("value = %v", cmd[i+1])
			}
		}
	}
	if n != 1 {
		t.Errorf("--always-allow appears %d times: %v", n, cmd)
	}
}
