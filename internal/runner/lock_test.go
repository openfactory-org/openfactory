package runner_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runner/runnertest"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/netlock"
)

type nopExec struct{}

func (nopExec) Run(context.Context, string, ...string) (string, error) { return "", nil }

// listExec answers "iptables -S" with the rules of p, like iptables after
// the lock applied them.
type listExec struct{ p netlock.RuleParams }

func (l listExec) Run(_ context.Context, name string, args ...string) (string, error) {
	if len(args) < 4 || args[2] != "-S" {
		return "", nil
	}
	if args[3] == "OUTPUT" {
		return "-P OUTPUT ACCEPT\n-A OUTPUT -j " + netlock.Chain + "\n", nil
	}
	var out []string
	for _, line := range strings.Split(netlock.Script(l.p), "\n") {
		if rest, ok := strings.CutPrefix(line, name+" -w 5 -A "); ok {
			out = append(out, "-A "+rest)
		}
	}
	return strings.Join(out, "\n"), nil
}

// fakeLocker returns a lock that runs nothing as root: no users, no rules,
// no netguard, and the agent is the user of the test.
func fakeLocker(t *testing.T, got *netlock.Params, mu *sync.Mutex) *netlock.Locker {
	t.Helper()
	me := netlock.Credential{UID: os.Getuid(), GID: os.Getgid(), Home: t.TempDir()}
	return &netlock.Locker{
		Agent: &me,
		Users: &netlock.Users{Exec: nopExec{}, SudoersDir: t.TempDir(), Lookup: func(name string) (int, bool, error) {
			if name == netlock.AgentUser {
				return me.UID, true, nil
			}
			return netlock.ProxyUserAvoiding(me.UID), true, nil
		}},
		Firewall: &netlock.Firewall{
			Exec:       listExec{netlock.RuleParams{AgentUID: me.UID, ProxyUID: netlock.ProxyUserAvoiding(me.UID), LoopbackDaemonPorts: []int{8911}}},
			HasProgram: func(string) bool { return true },
		},
		StartProxy: func(_ context.Context, p netlock.Params, _ int) (func(), error) {
			mu.Lock()
			*got = p
			mu.Unlock()
			return func() {}, nil
		},
		Probe: func(context.Context, netlock.Params, netlock.ProbeSpec) (netlock.ProbeResult, error) {
			return netlock.ProbeResult{Checks: []netlock.Check{{Name: "direct", OK: true}}}, nil
		},
		Chown: func(string, int, int) error { return nil },
	}
}

// The lock fails closed when the control plane sends no network policy.
func TestLockWithoutPolicyFailsClosed(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	cfg := e.config(execHarness("cat"))
	cfg.EgressLock = netlock.ModeIPTables
	err := waitResult(t, e.start(context.Background(), cfg), 10*time.Second)
	if !errors.Is(err, netlock.ErrNotProven) {
		t.Fatalf("err = %v, want ErrNotProven", err)
	}
	if e.ready() || e.session().State == runnertest.SessionEnded {
		t.Fatalf("session %+v: the runner must exit without a session end", e.session())
	}
}

// A lock that the control plane turns off fails closed too.
func TestLockOffFailsClosed(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{NetworkPolicy: &runnerproto.NetworkPolicy{Restricted: true}, EgressLock: runnerproto.EgressLockOff}, nil)
	cfg := e.config(execHarness("cat"))
	cfg.EgressLock = netlock.ModeIPTables
	if err := waitResult(t, e.start(context.Background(), cfg), 10*time.Second); !errors.Is(err, netlock.ErrNotProven) {
		t.Fatalf("err = %v, want ErrNotProven", err)
	}
}

// With the lock, the policy reaches netguard and the agent processes get
// the proxy variables, the home of the agent and the report URL.
func TestLockAppliesPolicyAndProxyEnv(t *testing.T) {
	policy := &runnerproto.NetworkPolicy{Restricted: true, AllowedHosts: []string{"api.example.com"}, AlwaysAllow: []string{"acct.r2.cloudflarestorage.com"}}
	e := newEnv(t, runnerproto.StartResponse{NetworkPolicy: policy, EgressLock: runnerproto.EgressLockRequired}, nil)
	var mu sync.Mutex
	var got netlock.Params
	cfg := e.config(execHarness(`echo "$HTTPS_PROXY|$NO_PROXY|$USER"`))
	cfg.EgressLock = netlock.ModeIPTables
	cfg.LoopbackDaemonPorts = []int{8911}
	cfg.Locker = fakeLocker(t, &got, &mu)
	done := e.start(context.Background(), cfg)
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })
	ev, _ := e.find(runnerproto.EventMessage, nil)
	if c := decode[runnerproto.MessageData](t, ev).Content; c != "http://127.0.0.1:3128|"+netlock.LocalNoProxy+"|agent" {
		t.Errorf("agent env = %q", c)
	}
	mu.Lock()
	if !got.Policy.Restricted || len(got.Policy.AllowedHosts) != 1 || got.ControlURL != e.cp.URL() ||
		len(got.AlwaysAllow) != 1 || len(got.LoopbackDaemonPorts) != 1 || got.Workspace != e.work {
		t.Errorf("lock params = %+v", got)
	}
	mu.Unlock()
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

// The control plane refuses another runner version, and the runner stops.
func TestRunnerVersionMismatch(t *testing.T) {
	cp := runnertest.New(t, runnertest.Options{RunnerVersion: "2.0.0"})
	boot := cp.AddSession(1, runnerproto.StartResponse{Harness: runnerproto.Harness{Type: "exec"}})
	cfg := runner.Config{ControlURL: cp.URL(), BootstrapToken: boot, WorkDir: filepath.Join(t.TempDir(), "w"), StateDir: t.TempDir(),
		Harnesses: map[string]runner.Harness{"exec": execHarness("cat")}, Version: "1.0.0", Logger: testLogger(), PollWait: 1}
	if err := runner.Run(context.Background(), cfg); !errors.Is(err, runner.ErrRunnerVersionMismatch) {
		t.Fatalf("err = %v", err)
	}
	// The start response can name the version too.
	e := newEnv(t, runnerproto.StartResponse{RunnerVersion: "9.9.9"}, nil)
	if err := waitResult(t, e.start(context.Background(), e.config(execHarness("cat"))), 10*time.Second); !errors.Is(err, runner.ErrRunnerVersionMismatch) {
		t.Fatalf("err = %v", err)
	}
}

// A clone borrows the objects of a mirror in the git cache directory.
func TestCloneUsesGitMirror(t *testing.T) {
	gitIdentity(t)
	src := makeRepo(t)
	mirrors := t.TempDir()
	// GitCacheKey needs a URL with a host; file://localhost has one.
	url := "file://localhost" + src
	mirror := filepath.Join(mirrors, runner.GitCacheKey(url)+".git")
	if out, err := exec.Command("git", "clone", "-q", "--bare", src, mirror).CombinedOutput(); err != nil {
		t.Skipf("git clone --bare: %v %s", err, out)
	}
	e := newEnv(t, runnerproto.StartResponse{Workspace: runnerproto.Workspace{Repositories: []runnerproto.Repository{{CloneURL: url, Path: "repo"}}}}, nil)
	cfg := e.config(execHarness("cat"))
	cfg.GitMirrorDir = mirrors
	done := e.start(context.Background(), cfg)
	waitFor(t, "ready", e.ready)
	if _, err := os.Stat(filepath.Join(e.work, "repo", "a.txt")); err != nil {
		t.Fatal(err)
	}
	// --dissociate leaves no alternates file.
	if _, err := os.Stat(filepath.Join(e.work, "repo", ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Errorf("alternates file: %v", err)
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestGitCacheKey(t *testing.T) {
	a := runner.GitCacheKey("https://GitHub.com/acme/app.git")
	for _, u := range []string{"https://github.com/acme/app", "https://user:pw@github.com/acme/app/", "HTTPS://github.com/acme/app.git"} {
		if got := runner.GitCacheKey(u); got != a {
			t.Errorf("GitCacheKey(%q) = %q, want %q", u, got, a)
		}
	}
	if len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("key %q is not 16 hex digits", a)
	}
	if runner.GitCacheKey("https://github.com/acme/other") == a || runner.GitCacheKey("not a url") != "" {
		t.Error("keys of different URLs are equal, or a bad URL has a key")
	}
}
