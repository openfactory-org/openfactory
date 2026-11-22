package runner_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runner/runnertest"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// testRun names the run of a test environment.
type testRun struct{ ID string }

type testEnv struct {
	t     *testing.T
	cp    *runnertest.Server
	run   testRun
	boot  string
	work  string
	state string
}

// newEnv makes a control plane with one run and one session that waits for a runner.
func newEnv(t *testing.T, resp runnerproto.StartResponse, secrets *runnerproto.SecretsResponse) *testEnv {
	t.Helper()
	return newEnvWith(t, resp, secrets, nil)
}

// newEnvWith is newEnv with a change of the fake control plane.
func newEnvWith(t *testing.T, resp runnerproto.StartResponse, secrets *runnerproto.SecretsResponse, mod func(*runnertest.Server)) *testEnv {
	t.Helper()
	if resp.Harness.Type == "" {
		resp.Harness.Type = "exec"
	}
	cp := runnertest.New(t, runnertest.Options{})
	if secrets != nil {
		cp.SetSecrets(*secrets)
	}
	if mod != nil {
		mod(cp)
	}
	e := &testEnv{t: t, cp: cp, run: testRun{ID: cp.RunID()}, work: filepath.Join(t.TempDir(), "work"), state: t.TempDir()}
	e.boot = cp.AddSession(1, resp)
	return e
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func (e *testEnv) config(h runner.Harness) runner.Config {
	return runner.Config{
		ControlURL:     e.cp.URL(),
		RunID:          e.run.ID,
		Session:        1,
		BootstrapToken: e.boot,
		WorkDir:        e.work,
		StateDir:       e.state,
		Harnesses:      map[string]runner.Harness{"exec": h},
		Version:        "1.0.0",
		SandboxID:      "test",
		Logger:         testLogger(),
		PollWait:       1,
	}
}

// start runs the runner in a goroutine. The result channel gets the result of Run.
func (e *testEnv) start(ctx context.Context, cfg runner.Config) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- runner.Run(ctx, cfg) }()
	return ch
}

func (e *testEnv) session() runnertest.Session { return e.cp.Session(1) }

func (e *testEnv) events() []runnertest.Event { return e.cp.Events(1) }

func (e *testEnv) find(typ runnerproto.EventType, pred func(runnertest.Event) bool) (runnertest.Event, bool) {
	for _, ev := range e.events() {
		if ev.Type == string(typ) && (pred == nil || pred(ev)) {
			return ev, true
		}
	}
	return runnertest.Event{}, false
}

func (e *testEnv) hasEvent(typ runnerproto.EventType) bool {
	_, ok := e.find(typ, nil)
	return ok
}

func (e *testEnv) ready() bool { return e.session().ReadyAt != nil }

func (e *testEnv) command(typ runnerproto.CommandType, data any) {
	e.t.Helper()
	e.cp.Enqueue(1, typ, data)
}

func (e *testEnv) startTurn(id, content string) {
	e.t.Helper()
	e.command(runnerproto.CommandTurnStart, runnerproto.TurnStartData{
		TurnID: id,
		Input: []runnerproto.InputItem{{
			MessageID: "msg_1", Kind: runnerproto.KindUser,
			Sender: runnerproto.Sender{Type: "user", Name: "Dana"}, Content: content,
			Attachments: []runnerproto.Attachment{},
		}},
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func waitResult(t *testing.T, ch <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		t.Fatal("timeout waiting for Run to return")
		return nil
	}
}

func decode[T any](t *testing.T, ev runnertest.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(ev.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func execHarness(script string) runner.ExecHarness {
	return runner.ExecHarness{Command: []string{"sh", "-c", script}}
}

func (e *testEnv) turnEnded() (runnerproto.TurnEndedData, bool) {
	ev, ok := e.find(runnerproto.EventTurnEnded, nil)
	if !ok {
		return runnerproto.TurnEndedData{}, false
	}
	return decode[runnerproto.TurnEndedData](e.t, ev), true
}

func (e *testEnv) wantEnded(reason string) runnertest.Session {
	e.t.Helper()
	s := e.session()
	if s.State != runnertest.SessionEnded || s.EndReason != reason {
		e.t.Fatalf("session state %q reason %q, want ended %q", s.State, s.EndReason, reason)
	}
	return s
}
