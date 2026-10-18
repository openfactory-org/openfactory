package egress

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestNewPeer(t *testing.T) {
	p, err := NewPeer("proxy", "ns", "app=proxy, tier = edge")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Labels, map[string]any{"app": "proxy", "tier": "edge"}) || p.Namespace != "ns" || p.Empty() {
		t.Errorf("peer = %+v", p)
	}
	if p, err := NewPeer("proxy", "", ""); err != nil || !p.Empty() {
		t.Errorf("empty selector: %+v %v", p, err)
	}
	for _, c := range [][2]string{{"ns", ""}, {"", "novalue"}, {"", "=x"}} {
		if _, err := NewPeer("proxy", c[0], c[1]); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("NewPeer(%q, %q) err = %v", c[0], c[1], err)
		}
	}
}

func TestManifestRules(t *testing.T) {
	cp, _ := NewPeer("cp", "of", "app=of")
	m := Manifest(PolicyParams{
		Name: "np", Namespace: "ns", SelectorKey: "k", SelectorValue: "v",
		ControlPlane: cp, ControlURL: "http://10.0.0.1:8080", ExtraURLs: []string{"http://10.0.0.2:3128", "http://name.example"},
		Sidecar: true,
	})
	eg := m["spec"].(map[string]any)["egress"].([]any)
	// DNS, control plane peer, one extra IP rule, proxy ports. The control URL is ignored when a peer exists.
	if len(eg) != 4 {
		t.Fatalf("egress = %v", eg)
	}
	if !reflect.DeepEqual(m["spec"].(map[string]any)["podSelector"], map[string]any{"matchLabels": map[string]any{"k": "v"}}) {
		t.Errorf("podSelector = %v", m["spec"])
	}
	// Without a peer, an IP control URL gives a rule.
	m = Manifest(PolicyParams{Name: "np", ControlURL: "https://10.0.0.1"})
	if eg = m["spec"].(map[string]any)["egress"].([]any); len(eg) != 2 {
		t.Errorf("egress = %v", eg)
	}
	// A host name gives no rule.
	m = Manifest(PolicyParams{Name: "np", ControlURL: "https://factory.example"})
	if eg = m["spec"].(map[string]any)["egress"].([]any); len(eg) != 1 {
		t.Errorf("egress = %v", eg)
	}
}

func TestRestricted(t *testing.T) {
	if Restricted(nil) || Restricted(&core.TaskNetworkPolicy{Mode: ModeUnrestricted}) {
		t.Error("unrestricted counts as restricted")
	}
	if !Restricted(&core.TaskNetworkPolicy{Mode: "restricted"}) || !Restricted(&core.TaskNetworkPolicy{Mode: "unknown"}) {
		t.Error("an unknown mode must fail closed")
	}
}

func TestEnv(t *testing.T) {
	e := Env("http://p:1", "control.example", "")
	if e["NO_PROXY"] != "control.example,"+LocalNoProxy || e["https_proxy"] != "http://p:1" {
		t.Errorf("env = %v", e)
	}
}

func TestSidecarAndWithSidecar(t *testing.T) {
	sc := Sidecar(SidecarParams{Image: "img", PullPolicy: "Always", User: 1000,
		Policy: &core.TaskNetworkPolicy{AllowedHosts: []string{"a.test"}, DeniedHosts: []string{"b.test"}}})
	cmd := sc["command"].([]any)
	got := make([]string, len(cmd))
	for i, c := range cmd {
		got[i] = c.(string)
	}
	if !strings.Contains(strings.Join(got, " "), "--allow a.test --deny b.test") || sc["restartPolicy"] != "Always" {
		t.Errorf("sidecar = %v", sc)
	}
	// The proxy listens on the loopback address of the Pod. The kubelet makes
	// a tcpSocket probe from the node, so the startup probe must be an exec
	// probe, or it never passes.
	probe, _ := sc["startupProbe"].(map[string]any)
	if _, ok := probe["tcpSocket"]; ok {
		t.Errorf("the startup probe is a tcpSocket probe, which the kubelet makes from the node: %v", probe)
	}
	exec, _ := probe["exec"].(map[string]any)
	if cmd, _ := exec["command"].([]any); len(cmd) == 0 || cmd[len(cmd)-1] != "127.0.0.1:"+strconv.Itoa(ProxyPort) {
		t.Errorf("the startup probe does not dial the proxy on the loopback address: %v", probe)
	}
	out := WithSidecar([]any{map[string]any{"name": "x"}, map[string]any{"name": ProxySidecarName, "image": "old"}}, sc)
	if len(out) != 2 || out[0].(map[string]any)["image"] != "img" {
		t.Errorf("init containers = %v", out)
	}
}

func TestCheckAccess(t *testing.T) {
	review := func(deny map[string]bool) func(context.Context, map[string]any, any) error {
		return func(_ context.Context, body map[string]any, out any) error {
			verb := body["spec"].(map[string]any)["resourceAttributes"].(map[string]any)["verb"].(string)
			o := out.(*struct {
				Status struct {
					Allowed bool `json:"allowed"`
				} `json:"status"`
			})
			o.Status.Allowed = !deny[verb]
			return nil
		}
	}
	denied, err := CheckAccess(context.Background(), "ns", review(map[string]bool{"delete": true, "create": true}))
	if err != nil || !reflect.DeepEqual(denied, []string{"create", "delete"}) {
		t.Errorf("denied = %v, err = %v", denied, err)
	}
	denied, err = CheckAccess(context.Background(), "ns", review(nil))
	if err != nil || len(denied) != 0 {
		t.Errorf("denied = %v, err = %v", denied, err)
	}
	boom := func(context.Context, map[string]any, any) error { return errors.New("boom") }
	if _, err := CheckAccess(context.Background(), "ns", boom); err == nil {
		t.Error("a failed review must give an error")
	}
}
