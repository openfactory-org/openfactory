package netlock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// fakeExec records commands. It answers "-S" with the rules it saw, the way
// iptables does, and fails the commands in fail.
type fakeExec struct {
	mu    sync.Mutex
	cmds  []string
	fail  map[string]bool
	rules map[string][]string // program -> "-A ..." lines
}

func newFakeExec() *fakeExec {
	return &fakeExec{fail: map[string]bool{}, rules: map[string][]string{}}
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.cmds = append(f.cmds, line)
	if f.fail[line] {
		return "", fmt.Errorf("%s failed", line)
	}
	if len(args) >= 3 && args[0] == "-w" {
		rest := args[2:]
		switch rest[0] {
		case "-A":
			// iptables adds implicit matches to its listing.
			l := strings.Join(rest, " ")
			l = strings.Replace(l, "-p tcp --dport", "-p tcp -m tcp --dport", 1)
			l = strings.Replace(l, "-p udp --dport", "-p udp -m udp --dport", 1)
			f.rules[name] = append(f.rules[name], l)
		case "-C":
			for _, r := range f.rules[name+"/OUTPUT"] {
				if r == "-A OUTPUT -j "+rest[len(rest)-1] {
					return "", nil
				}
			}
			return "", fmt.Errorf("no rule")
		case "-I":
			f.rules[name+"/OUTPUT"] = append([]string{"-A OUTPUT -j " + rest[len(rest)-1]}, f.rules[name+"/OUTPUT"]...)
		case "-S":
			if rest[1] == "OUTPUT" {
				return "-P OUTPUT ACCEPT\n" + strings.Join(f.rules[name+"/OUTPUT"], "\n") + "\n", nil
			}
			return "-N " + Chain + "\n" + strings.Join(f.rules[name], "\n") + "\n", nil
		}
	}
	return "", nil
}

func (f *fakeExec) has(line string) bool {
	for _, c := range f.cmds {
		if c == line {
			return true
		}
	}
	return false
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeNone, "none": ModeNone, " IPTables ": ModeIPTables, "netns": ModeNetns} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("nft"); !errors.Is(err, rtcore.ErrInvalid) {
		t.Errorf("ParseMode(nft) err = %v", err)
	}
	if ModeNone.Enabled() || !ModeIPTables.Enabled() {
		t.Error("Enabled")
	}
	ports, err := ParsePorts("8911, 6080,,22")
	if err != nil || fmt.Sprint(ports) != "[8911 6080 22]" {
		t.Errorf("ParsePorts = %v %v", ports, err)
	}
	if _, err := ParsePorts("70000"); err == nil {
		t.Error("ParsePorts accepted 70000")
	}
}

func TestScript(t *testing.T) {
	s := Script(RuleParams{LoopbackDaemonPorts: []int{8911}})
	want := []string{
		"set -eu",
		"iptables -w 5 -N OF_LOCK 2>/dev/null || iptables -w 5 -F OF_LOCK",
		"iptables -w 5 -A OF_LOCK -m owner --uid-owner 65532 -j RETURN",
		"iptables -w 5 -A OF_LOCK -o lo -m owner --uid-owner 1001 -p tcp --dport 8911 -j REJECT --reject-with tcp-reset",
		"iptables -w 5 -A OF_LOCK -o lo -m owner --uid-owner 1001 -p udp --dport 8911 -j REJECT",
		"iptables -w 5 -A OF_LOCK -o lo -m owner --uid-owner 1001 -j RETURN",
		"iptables -w 5 -A OF_LOCK -m owner --uid-owner 1001 -p tcp -j REJECT --reject-with tcp-reset",
		"iptables -w 5 -A OF_LOCK -m owner --uid-owner 1001 -j REJECT",
		"iptables -w 5 -C OUTPUT -j OF_LOCK 2>/dev/null || iptables -w 5 -I OUTPUT 1 -j OF_LOCK",
		"ip6tables -w 5 -N OF_LOCK 2>/dev/null || ip6tables -w 5 -F OF_LOCK",
		"ip6tables -w 5 -A OF_LOCK -o lo -m owner --uid-owner 1001 -j RETURN",
		"ip6tables -w 5 -C OUTPUT -j OF_LOCK 2>/dev/null || ip6tables -w 5 -I OUTPUT 1 -j OF_LOCK",
	}
	for _, w := range want {
		if !strings.Contains(s, w+"\n") {
			t.Errorf("script lacks %q:\n%s", w, s)
		}
	}
	// Order: the daemon rejects come before the loopback return, and the
	// agent rejects come last.
	iDaemon := strings.Index(s, "--dport 8911")
	iLo := strings.Index(s, "-o lo -m owner --uid-owner 1001 -j RETURN")
	iReject := strings.Index(s, "--uid-owner 1001 -j REJECT\n")
	if iDaemon >= iLo || iLo >= iReject {
		t.Errorf("rule order is wrong:\n%s", s)
	}
}

func TestProxyUserAvoiding(t *testing.T) {
	if got := ProxyUserAvoiding(AgentUID); got != DefaultProxyUID {
		t.Errorf("got %d", got)
	}
	if got := ProxyUserAvoiding(65532, 65533); got != 65534 {
		t.Errorf("got %d", got)
	}
}

func TestFirewallApplyAndVerify(t *testing.T) {
	fx := newFakeExec()
	fw := &Firewall{Exec: fx, HasProgram: func(string) bool { return true }}
	p := RuleParams{LoopbackDaemonPorts: []int{22}}
	if err := fw.Apply(context.Background(), p); err != nil {
		t.Fatalf("Apply: %v\n%s", err, strings.Join(fx.cmds, "\n"))
	}
	for _, w := range []string{
		"iptables -w 5 -N OF_LOCK",
		"iptables -w 5 -I OUTPUT 1 -j OF_LOCK",
		"ip6tables -w 5 -I OUTPUT 1 -j OF_LOCK",
		"ip6tables -w 5 -S OF_LOCK",
	} {
		if !fx.has(w) {
			t.Errorf("missing command %q", w)
		}
	}
	// A rule that changed later fails the check.
	fx.rules["iptables"][len(fx.rules["iptables"])-1] = "-A OF_LOCK -j ACCEPT"
	if err := fw.Verify(context.Background(), p); err == nil {
		t.Error("Verify accepted a changed chain")
	}
}

func TestFirewallIPv6(t *testing.T) {
	has := func(name string) bool { return name == "iptables" }
	fw := &Firewall{Exec: newFakeExec(), HasProgram: has, IPv6Interfaces: func() ([]string, error) { return []string{"lo", "eth0"}, nil }}
	if err := fw.Apply(context.Background(), RuleParams{}); !errors.Is(err, ErrNoIPv6Lock) {
		t.Fatalf("Apply without ip6tables on an IPv6 sandbox: %v", err)
	}
	fw.IPv6Interfaces = func() ([]string, error) { return []string{"lo"}, nil }
	if err := fw.Apply(context.Background(), RuleParams{}); err != nil {
		t.Fatalf("Apply without ip6tables, IPv6 only on lo: %v", err)
	}
	fw.HasProgram = func(string) bool { return false }
	if err := fw.Apply(context.Background(), RuleParams{}); err == nil {
		t.Fatal("Apply without iptables succeeded")
	}
}

func TestUsersEnsure(t *testing.T) {
	fx := newFakeExec()
	dir := t.TempDir()
	known := map[string]int{}
	u := Users{Exec: fx, SudoersDir: dir, Lookup: func(name string) (int, bool, error) {
		id, ok := known[name]
		return id, ok, nil
	}}
	if err := u.Ensure(context.Background(), AgentUID, DefaultProxyUID); err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"groupadd -g 1001 agent",
		"useradd -u 1001 -g 1001 -m -d /home/agent -s /bin/bash agent",
		"useradd -u 65532 -g 65532 -r -M -d /nonexistent -s /usr/sbin/nologin ofproxy",
	} {
		if !fx.has(w) {
			t.Errorf("missing %q in %v", w, fx.cmds)
		}
	}
	// An existing agent with another uid fails.
	known["agent"] = 1000
	if err := u.Ensure(context.Background(), AgentUID, DefaultProxyUID); err == nil {
		t.Error("Ensure accepted agent uid 1000")
	}
	// A sudo rule for the agent fails.
	known["agent"], known["ofproxy"] = AgentUID, DefaultProxyUID
	if err := os.MkdirAll(filepath.Join(dir, "sudoers.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sudoers.d", "agent"), []byte("# x\nagent ALL=(ALL) NOPASSWD:ALL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := u.Ensure(context.Background(), AgentUID, DefaultProxyUID); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Errorf("Ensure with a sudo rule: %v", err)
	}
}

func TestNetguardArgs(t *testing.T) {
	p := Params{
		Policy:       rtcore.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: []string{"api.example.com"}, DeniedHosts: []string{"bad.example"}},
		ControlURL:   "https://cp.example.com",
		AlwaysAllow:  []string{"acct.r2.cloudflarestorage.com"},
		PrivateAllow: []string{"git.internal"},
	}
	got := strings.Join(NetguardArgs(p), " ")
	want := "netguard --listen 127.0.0.1:3128 --report-listen 127.0.0.1:3129 --allow api.example.com --deny bad.example " +
		"--always-allow acct.r2.cloudflarestorage.com --always-allow cp.example.com " +
		"--allow-private-host git.internal --allow-private-host cp.example.com"
	if got != want {
		t.Errorf("args:\n got %s\nwant %s", got, want)
	}
	p.Policy.Restricted = false
	if !strings.Contains(strings.Join(NetguardArgs(p), " "), "--unrestricted") {
		t.Error("unrestricted policy lacks --unrestricted")
	}
}

func TestProbeSpecFor(t *testing.T) {
	spec := ProbeSpecFor(Params{ControlURL: "http://cp.example:8080", LoopbackDaemonPorts: []int{22}, Policy: rtcore.EffectiveNetworkPolicy{Restricted: true}})
	if spec.ControlHost != "cp.example:8080" || !spec.Restricted || len(spec.DaemonPorts) != 1 {
		t.Fatalf("spec = %+v", spec)
	}
	if last := spec.DirectTargets[len(spec.DirectTargets)-1]; last != "cp.example:8080" {
		t.Errorf("the control host is not a direct target: %v", spec.DirectTargets)
	}
}

func TestSetup(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspace")
	var chowned []string
	var proxyStarted, proxyStopped bool
	newLocker := func(probe ProbeResult, probeErr error) *Locker {
		fx := newFakeExec()
		return &Locker{
			Exec:     fx,
			Users:    &Users{Exec: fx, SudoersDir: t.TempDir(), Lookup: func(string) (int, bool, error) { return 0, false, nil }},
			Firewall: &Firewall{Exec: fx, HasProgram: func(string) bool { return true }},
			StartProxy: func(context.Context, Params, int) (func(), error) {
				proxyStarted = true
				return func() { proxyStopped = true }, nil
			},
			Probe: func(context.Context, Params, ProbeSpec) (ProbeResult, error) { return probe, probeErr },
			Chown: func(dir string, uid, gid int) error {
				chowned = append(chowned, fmt.Sprintf("%s:%d:%d", dir, uid, gid))
				return nil
			},
		}
	}
	p := Params{Mode: ModeIPTables, Workspace: ws, ControlURL: "https://cp.example"}

	// ModeNone does nothing.
	if l, err := newLocker(ProbeResult{}, nil).Setup(context.Background(), Params{Mode: ModeNone}); l != nil || err != nil {
		t.Fatalf("ModeNone: %v %v", l, err)
	}
	// netns is reserved.
	if _, err := newLocker(ProbeResult{}, nil).Setup(context.Background(), Params{Mode: ModeNetns}); !errors.Is(err, ErrNotProven) {
		t.Fatalf("ModeNetns: %v", err)
	}

	ok := ProbeResult{Checks: []Check{{Name: "direct", OK: true}}}
	l, err := newLocker(ok, nil).Setup(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !proxyStarted || l.Agent.UID != AgentUID || l.Env()["HTTPS_PROXY"] != "http://127.0.0.1:3128" {
		t.Fatalf("lock = %+v", l)
	}
	if len(chowned) != 1 || chowned[0] != ws+":1001:1001" {
		t.Errorf("chowned = %v", chowned)
	}
	l.Close()
	if !proxyStopped {
		t.Error("Close did not stop netguard")
	}

	// A failed check fails closed and stops netguard.
	proxyStopped = false
	bad := ProbeResult{Checks: []Check{{Name: "direct 1.1.1.1:443", OK: false, Detail: "connected"}}}
	if _, err := newLocker(bad, nil).Setup(context.Background(), p); !errors.Is(err, ErrNotProven) || !strings.Contains(err.Error(), "1.1.1.1") {
		t.Fatalf("bad probe: %v", err)
	}
	if !proxyStopped {
		t.Error("netguard runs after a failed probe")
	}
	// A probe that could not run fails closed.
	if _, err := newLocker(ProbeResult{}, errors.New("exec failed")).Setup(context.Background(), p); !errors.Is(err, ErrNotProven) {
		t.Fatalf("probe error: %v", err)
	}
	// An empty result fails closed.
	if _, err := newLocker(ProbeResult{}, nil).Setup(context.Background(), p); !errors.Is(err, ErrNotProven) {
		t.Fatalf("empty probe: %v", err)
	}
}

// TestRunProbe runs the probe against a fake network: every direct dial
// fails, the proxy answers 200 for the control host and 403 for others.
func TestRunProbe(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() {
		_ = http.Serve(proxy, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect && r.Host == "cp.example:443" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusForbidden)
		}))
	}()
	locked := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		if addr == proxy.Addr().String() || (host == "127.0.0.1" && !strings.HasSuffix(addr, ":22")) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, addr)
		}
		return nil, errors.New("connection refused")
	}
	spec := ProbeSpec{
		ProxyURL:      "http://" + proxy.Addr().String(),
		DirectTargets: []string{"1.1.1.1:443", "cp.example:443"},
		DaemonPorts:   []int{22},
		ControlHost:   "cp.example:443",
		Restricted:    true,
		TimeoutMS:     500,
	}
	res := RunProbe(context.Background(), spec, locked)
	if err := res.Err(); err != nil {
		t.Fatalf("locked network: %v (%+v)", err, res)
	}
	// An open network fails: a direct dial works.
	open := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "1.1.1.1:443" {
			c1, c2 := net.Pipe()
			_ = c2.Close()
			return c1, nil
		}
		return locked(ctx, network, addr)
	}
	if err := RunProbe(context.Background(), spec, open).Err(); err == nil || !strings.Contains(err.Error(), "1.1.1.1:443") {
		t.Fatalf("open network: %v", err)
	}
}

func TestEnvAndControlViaProxy(t *testing.T) {
	e := Env("http://127.0.0.1:3128", "cp.example")
	if e["NO_PROXY"] != "cp.example,"+LocalNoProxy || e["https_proxy"] != "http://127.0.0.1:3128" {
		t.Errorf("env = %v", e)
	}
	for u, want := range map[string]bool{"https://cp.example": true, "http://cp.example:80": true, "http://cp.example:8080": false, "::": false} {
		if got := ControlViaProxy(u); got != want {
			t.Errorf("ControlViaProxy(%q) = %v", u, got)
		}
	}
}

func TestChownTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "a", "link")); err != nil {
		t.Fatal(err)
	}
	// The own user is always allowed; the walk must not follow the link.
	if err := ChownTree(dir, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
}
