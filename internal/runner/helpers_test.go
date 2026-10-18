package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runnerapi"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// fakeConfigurer returns a start response that a test can set.
type fakeConfigurer struct {
	mu   sync.Mutex
	resp runnerproto.StartResponse
}

func (f *fakeConfigurer) StartConfig(context.Context, *core.Run, *core.RunSession) (runnerproto.StartResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resp, nil
}

type fakeSecrets struct{ resp runnerproto.SecretsResponse }

func (f fakeSecrets) Secrets(context.Context, *core.Run, *core.RunSession) (runnerproto.SecretsResponse, error) {
	return f.resp, nil
}

type testEnv struct {
	t     *testing.T
	mem   *sqlstore.Store
	svc   *runnerapi.Service
	srv   *httptest.Server
	run   *core.Run
	boot  string
	work  string
	state string
}

// newEnv makes a control plane with one run and one session that waits for a runner.
func newEnv(t *testing.T, resp runnerproto.StartResponse, secrets runnerapi.SecretsProvider) *testEnv {
	t.Helper()
	return newEnvWith(t, resp, secrets, nil)
}

// newEnvWith is newEnv with a change of the service configuration.
func newEnvWith(t *testing.T, resp runnerproto.StartResponse, secrets runnerapi.SecretsProvider, mod func(*runnerapi.Config)) *testEnv {
	t.Helper()
	keys, err := runnertoken.NewStaticKeys("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Harness.Type == "" {
		resp.Harness.Type = "exec"
	}
	e := &testEnv{t: t, mem: testdb.New(t), work: filepath.Join(t.TempDir(), "work"), state: t.TempDir()}
	cfg := runnerapi.Config{
		Store:        e.mem,
		Runner:       e.mem,
		Keys:         keys,
		Session:      &fakeConfigurer{resp: resp},
		Secrets:      secrets,
		PollInterval: 50 * time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	e.svc, err = runnerapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.svc.Handler())
	t.Cleanup(e.srv.Close)
	e.run = &core.Run{TeamID: "team_1", FactoryID: "fac_test"}
	ctx := context.Background()
	if err := e.mem.CreateRun(ctx, e.run); err != nil {
		t.Fatal(err)
	}
	tok, hash, err := runnerapi.IssueBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	e.boot = tok
	s := &core.RunSession{
		RunID: e.run.ID, Session: 1, TaskID: "task", State: core.SessionAssigned,
		BootstrapHash: hash, BootstrapExpiresAt: time.Now().Add(time.Hour),
	}
	if err := e.mem.CreateRunSession(ctx, s); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *testEnv) config(h runner.Harness) runner.Config {
	return runner.Config{
		ControlURL:     e.srv.URL,
		RunID:          e.run.ID,
		Session:        1,
		BootstrapToken: e.boot,
		WorkDir:        e.work,
		StateDir:       e.state,
		Harnesses:      map[string]runner.Harness{"exec": h},
		Version:        "1.0.0",
		SandboxID:      "test",
		Logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
		PollWait:       1,
	}
}

// start runs the runner in a goroutine. The result channel gets the result of Run.
func (e *testEnv) start(ctx context.Context, cfg runner.Config) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- runner.Run(ctx, cfg) }()
	return ch
}

func (e *testEnv) session() *core.RunSession {
	e.t.Helper()
	s, err := e.mem.GetRunSession(context.Background(), e.run.ID, 1)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *testEnv) events() []core.RunnerEvent {
	e.t.Helper()
	ev, err := e.mem.ListRunnerEvents(context.Background(), e.run.ID, 1, 0, 10000)
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *testEnv) find(typ runnerproto.EventType, pred func(core.RunnerEvent) bool) (core.RunnerEvent, bool) {
	for _, ev := range e.events() {
		if ev.Type == string(typ) && (pred == nil || pred(ev)) {
			return ev, true
		}
	}
	return core.RunnerEvent{}, false
}

func (e *testEnv) hasEvent(typ runnerproto.EventType) bool {
	_, ok := e.find(typ, nil)
	return ok
}

func (e *testEnv) ready() bool { return e.session().ReadyAt != nil }

func (e *testEnv) command(typ runnerproto.CommandType, data any) {
	e.t.Helper()
	if _, err := e.svc.EnqueueCommand(context.Background(), e.run.ID, 1, typ, data); err != nil {
		e.t.Fatal(err)
	}
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

func decode[T any](t *testing.T, ev core.RunnerEvent) T {
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

func (e *testEnv) wantEnded(reason string) *core.RunSession {
	e.t.Helper()
	s := e.session()
	if s.State != core.SessionEnded || s.EndReason != reason {
		e.t.Fatalf("session state %q reason %q, want ended %q", s.State, s.EndReason, reason)
	}
	return s
}

func runnerAPIBootstrap() (string, string, error) { return runnerapi.IssueBootstrap() }
