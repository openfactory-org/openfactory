package runner_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerapi"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestHappyPath(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{
		Workspace: runnerproto.Workspace{
			SetupCommands: []runnerproto.SetupCommand{{Origin: "machine", Command: "echo hi > made.txt"}},
			Skills:        []runnerproto.Skill{{Name: "review", Files: map[string]string{"SKILL.md": "be kind"}}},
		},
	}, nil)
	done := e.start(context.Background(), e.config(execHarness("cat")))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "hello")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })

	var seq []string
	for _, ev := range e.events() {
		switch ev.Type {
		case "setup.step":
			d := decode[runnerproto.SetupStepData](t, ev)
			seq = append(seq, "setup.step:"+d.Step+":"+d.Status)
		case "message":
			d := decode[runnerproto.MessageData](t, ev)
			seq = append(seq, "message:"+d.Role+":"+d.Content)
		case "turn.ended":
			d := decode[runnerproto.TurnEndedData](t, ev)
			seq = append(seq, "turn.ended:"+string(d.Outcome))
		case "idle":
			// The idle events are not part of the check.
		default:
			seq = append(seq, ev.Type)
		}
	}
	want := []string{
		"setup.step:skills:started", "setup.step:skills:ok",
		"setup.step:command:1:started", "setup.step:command:1:ok",
		"session.ready", "turn.started", "message:assistant:hello", "turn.ended:done",
	}
	if strings.Join(seq, "|") != strings.Join(want, "|") {
		t.Fatalf("events\n got %v\nwant %v", seq, want)
	}
	if b, err := os.ReadFile(filepath.Join(e.work, "made.txt")); err != nil || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("made.txt: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(e.work, ".openfactory", "skills", "review", "SKILL.md")); err != nil || string(b) != "be kind" {
		t.Fatalf("skill file: %q %v", b, err)
	}

	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{Reason: "shutdown"})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("stopped")
}

func TestIdleEndsSession(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{IdleTimeoutSeconds: 1}, nil)
	done := e.start(context.Background(), e.config(execHarness("cat")))
	if err := waitResult(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("idle")
	if !e.hasEvent(runnerproto.EventIdle) {
		t.Fatal("no idle event")
	}
}

// An idle timeout of 0 that the server set ends the session when the turn
// ends. It does not end the session before the first turn.
func TestIdleTimeoutZeroEndsAfterTurn(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{IdleTimeoutSet: true}, nil)
	done := e.start(context.Background(), e.config(execHarness("cat")))
	waitFor(t, "ready", e.ready)
	select {
	case err := <-done:
		t.Fatalf("session ended before the first turn: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	e.startTurn("t1", "hello")
	if err := waitResult(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("idle")
	if !e.hasEvent(runnerproto.EventTurnEnded) {
		t.Fatal("no turn.ended before the idle end")
	}
}

// A server that does not set the flag (an older server) sends 0 for "none":
// the runner default applies and the session stays open after the turn.
func TestIdleTimeoutZeroWithoutFlagIsDefault(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := e.start(ctx, e.config(execHarness("cat")))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "hello")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })
	select {
	case err := <-done:
		t.Fatalf("session ended after the turn: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{Reason: "shutdown"})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSetupFailure(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{
		Workspace: runnerproto.Workspace{SetupCommands: []runnerproto.SetupCommand{{Command: "echo oops; exit 3"}}},
	}, nil)
	done := e.start(context.Background(), e.config(execHarness("cat")))
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	s := e.wantEnded("setup_failed")
	if s.FailureCode != core.FailureEnvironmentSetupFailed {
		t.Fatalf("failure code %q", s.FailureCode)
	}
	ev, ok := e.find(runnerproto.EventSetupStep, func(ev core.RunnerEvent) bool {
		return decode[runnerproto.SetupStepData](t, ev).Status == "failed"
	})
	if !ok {
		t.Fatal("no failed setup.step")
	}
	if d := decode[runnerproto.SetupStepData](t, ev); !strings.Contains(d.Output, "oops") {
		t.Fatalf("output %q", d.Output)
	}
	if e.hasEvent(runnerproto.EventSessionReady) {
		t.Fatal("session.ready after a setup failure")
	}
}

// endHooks records the end request of the session.
type endHooks struct {
	mu  sync.Mutex
	req *runnerproto.EndRequest
}

func (h *endHooks) SessionStarted(context.Context, *core.RunSession)                   {}
func (h *endHooks) EventsStored(context.Context, *core.RunSession, []core.RunnerEvent) {}
func (h *endHooks) SessionEnded(_ context.Context, _ *core.RunSession, r runnerproto.EndRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.req = &r
}

// The end request of a failed setup names the step and has the end of its
// output, with the secrets redacted. The run shows this text.
func TestSetupFailureMessage(t *testing.T) {
	hooks := &endHooks{}
	e := newEnvWith(t, runnerproto.StartResponse{
		Workspace: runnerproto.Workspace{SetupCommands: []runnerproto.SetupCommand{
			{Command: "echo ok"},
			{Command: "yes noise | head -c 20000; echo token=s3cret; echo build failed >&2; exit 3"},
		}},
	}, fakeSecrets{runnerproto.SecretsResponse{Redact: []string{"s3cret"}}}, func(c *runnerapi.Config) { c.Hooks = hooks })
	done := e.start(context.Background(), e.config(execHarness("cat")))
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("setup_failed")
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	if hooks.req == nil {
		t.Fatal("no end request")
	}
	m := hooks.req.Message
	if !strings.HasPrefix(m, "step command:2 failed: exit status 3\n") || !strings.Contains(m, "build failed") {
		t.Errorf("message %.200q", m)
	}
	if strings.Contains(m, "s3cret") {
		t.Errorf("message has the secret: %q", m)
	}
	if len(m) > 4200 {
		t.Errorf("message has %d bytes", len(m))
	}
}

func TestUnknownHarness(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{Harness: runnerproto.Harness{Type: "nope"}}, nil)
	done := e.start(context.Background(), e.config(execHarness("cat")))
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	s := e.wantEnded("setup_failed")
	if s.FailureCode != core.FailureEnvironmentSetupFailed {
		t.Fatalf("failure code %q", s.FailureCode)
	}
}

func TestInterrupt(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	done := e.start(context.Background(), e.config(execHarness("sleep 30")))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.started", func() bool { return e.hasEvent(runnerproto.EventTurnStarted) })
	e.command(runnerproto.CommandTurnInterrupt, runnerproto.TurnInterruptData{TurnID: "t1"})
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })
	d, _ := e.turnEnded()
	if d.Outcome != runnerproto.OutcomeInterrupted {
		t.Fatalf("outcome %q", d.Outcome)
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTurnError(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	done := e.start(context.Background(), e.config(execHarness("echo bad >&2; exit 2")))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })
	d, _ := e.turnEnded()
	if d.Outcome != runnerproto.OutcomeError || d.FailureCode != string(core.FailureAgentFailed) || !strings.Contains(d.Message, "bad") {
		t.Fatalf("turn.ended %+v", d)
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestTurnActiveDiagnostic(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	done := e.start(context.Background(), e.config(execHarness("sleep 30")))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.started", func() bool { return e.hasEvent(runnerproto.EventTurnStarted) })
	e.startTurn("t2", "again")
	waitFor(t, "diagnostic", func() bool {
		ev, ok := e.find(runnerproto.EventDiagnostic, nil)
		return ok && decode[runnerproto.DiagnosticData](t, ev).Code == "turn_active"
	})
	e.command(runnerproto.CommandSessionCancel, nil)
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	done := e.start(context.Background(), e.config(execHarness("sleep 30")))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.started", func() bool { return e.hasEvent(runnerproto.EventTurnStarted) })
	e.command(runnerproto.CommandSessionCancel, nil)
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("cancelled")
	if d, ok := e.turnEnded(); !ok || d.Outcome != runnerproto.OutcomeInterrupted {
		t.Fatalf("turn.ended %+v %v", d, ok)
	}
}

func TestGracefulStop(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := e.start(ctx, e.config(execHarness("cat")))
	waitFor(t, "ready", e.ready)
	cancel()
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("stopped")
}

func TestRedaction(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, fakeSecrets{runnerproto.SecretsResponse{Redact: []string{"s3cret"}}})
	done := e.start(context.Background(), e.config(execHarness("echo token=s3cret")))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })
	ev, ok := e.find(runnerproto.EventMessage, nil)
	if !ok {
		t.Fatal("no message event")
	}
	data := string(ev.Data)
	if strings.Contains(data, "s3cret") || !strings.Contains(data, "***") {
		t.Fatalf("message data %s", data)
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSecretEnvReachesHarness(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{Env: map[string]string{"FROM_START": "a"}},
		fakeSecrets{runnerproto.SecretsResponse{Env: map[string]string{"FROM_SECRETS": "b"}}})
	t.Setenv(core.TaskEnvBootstrapToken, "ofb_should_not_leak")
	done := e.start(context.Background(), e.config(execHarness(`echo "$FROM_START$FROM_SECRETS/${OF_BOOTSTRAP_TOKEN:-none}"`)))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })
	ev, _ := e.find(runnerproto.EventMessage, nil)
	if d := decode[runnerproto.MessageData](t, ev); d.Content != "ab/none" {
		t.Fatalf("content %q", d.Content)
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

// The harness needs a git identity, or the agent cannot commit. A value that
// the control plane sets wins over the default.
func TestHarnessGetsDefaultGitIdentity(t *testing.T) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	e := newEnv(t, runnerproto.StartResponse{Env: map[string]string{"GIT_AUTHOR_NAME": "Set By Plane"}}, nil)
	done := e.start(context.Background(), e.config(execHarness(`echo "$GIT_AUTHOR_NAME|$GIT_AUTHOR_EMAIL|$GIT_COMMITTER_NAME|$GIT_COMMITTER_EMAIL"`)))
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "go")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })
	ev, _ := e.find(runnerproto.EventMessage, nil)
	want := "Set By Plane|openfactory@localhost|openfactory|openfactory@localhost"
	if d := decode[runnerproto.MessageData](t, ev); d.Content != want {
		t.Fatalf("content %q, want %q", d.Content, want)
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledRunReturnsQuickly(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	e.run.State = core.RunCancelled
	if err := e.mem.UpdateRun(context.Background(), e.run); err != nil {
		t.Fatal(err)
	}
	done := e.start(context.Background(), e.config(execHarness("cat")))
	if err := waitResult(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if e.session().State != core.SessionAssigned {
		t.Fatalf("session state %q", e.session().State)
	}
}

func TestBadBootstrapTokenIsAnError(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{}, nil)
	cfg := e.config(execHarness("cat"))
	tok, _, err := runnerAPIBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	cfg.BootstrapToken = tok
	if err := waitResult(t, e.start(context.Background(), cfg), 5*time.Second); err == nil {
		t.Fatal("want an error")
	}
}

func TestClone(t *testing.T) {
	src := t.TempDir()
	genv := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	gitRun := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		cmd.Env = genv
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitRun("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-q", "-m", "first")
	gitRun("tag", "v1")
	if err := os.WriteFile(filepath.Join(src, "b.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun("add", ".")
	gitRun("commit", "-q", "-m", "second")

	e := newEnv(t, runnerproto.StartResponse{
		Workspace: runnerproto.Workspace{Repositories: []runnerproto.Repository{
			{CloneURL: src, Path: "repo", Ref: "v1"},
		}},
	}, nil)
	done := e.start(context.Background(), e.config(execHarness("cat")))
	waitFor(t, "ready", e.ready)
	if b, err := os.ReadFile(filepath.Join(e.work, "repo", "a.txt")); err != nil || string(b) != "one" {
		t.Fatalf("a.txt: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(e.work, "repo", "b.txt")); err == nil {
		t.Fatal("b.txt exists: the ref v1 was not checked out")
	}
	if _, ok := e.find(runnerproto.EventSetupStep, func(ev core.RunnerEvent) bool {
		d := decode[runnerproto.SetupStepData](t, ev)
		return d.Step == "clone:repo" && d.Status == "ok"
	}); !ok {
		t.Fatal("no ok step for the clone")
	}
	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestBadRepositoryPathFailsSetup(t *testing.T) {
	e := newEnv(t, runnerproto.StartResponse{
		Workspace: runnerproto.Workspace{Repositories: []runnerproto.Repository{{CloneURL: "x", Path: "../evil"}}},
	}, nil)
	done := e.start(context.Background(), e.config(execHarness("cat")))
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("setup_failed")
}
