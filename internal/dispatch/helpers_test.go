package dispatch

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// fakeBackend is a core.TaskBackend that records calls.
type fakeBackend struct {
	mu        sync.Mutex
	started   []core.Task
	cancelled []string
	destroyed []string
	startErr  error
	statuses  map[string]core.TaskStatus
	listed    []core.TaskStatus // what List returns
}

func newFakeBackend() *fakeBackend { return &fakeBackend{statuses: map[string]core.TaskStatus{}} }

func (b *fakeBackend) Name() string { return "fake" }

func (b *fakeBackend) Start(_ context.Context, t core.Task) (core.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return core.TaskStatus{}, b.startErr
	}
	b.started = append(b.started, t)
	st := core.TaskStatus{TaskID: t.TaskID, State: core.TaskStarting}
	b.statuses[t.TaskID] = st
	return st, nil
}

func (b *fakeBackend) Status(_ context.Context, id string) (core.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.statuses[id]
	if !ok {
		return core.TaskStatus{}, core.ErrNotFound
	}
	return st, nil
}

func (b *fakeBackend) Watch(context.Context, string) (<-chan core.TaskStatus, error) {
	return nil, core.ErrNotFound
}

func (b *fakeBackend) Cancel(_ context.Context, id string, _ int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cancelled = append(b.cancelled, id)
	return nil
}

func (b *fakeBackend) Destroy(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.destroyed = append(b.destroyed, id)
	return nil
}

func (b *fakeBackend) List(context.Context) ([]core.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]core.TaskStatus(nil), b.listed...), nil
}
func (b *fakeBackend) Preflight(context.Context) error { return nil }

func (b *fakeBackend) setStatus(st core.TaskStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.statuses[st.TaskID] = st
}

func (b *fakeBackend) startedTasks() []core.Task {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]core.Task(nil), b.started...)
}

func (b *fakeBackend) destroyedIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.destroyed...)
}

// fakeCommander stores commands in the runner store, like the service does.
type fakeCommander struct {
	runner core.RunnerStore
	err    error
	mu     sync.Mutex
	calls  int
}

func (c *fakeCommander) EnqueueCommand(ctx context.Context, runID string, session int, typ runnerproto.CommandType, data any) (core.RunnerCommand, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.err != nil {
		return core.RunnerCommand{}, c.err
	}
	cmd := core.RunnerCommand{RunID: runID, Session: session, Type: string(typ)}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return core.RunnerCommand{}, err
		}
		cmd.Data = raw
	}
	if err := c.runner.AppendRunnerCommand(ctx, &cmd); err != nil {
		return core.RunnerCommand{}, err
	}
	return cmd, nil
}

type env struct {
	t       *testing.T
	ctx     context.Context
	mem     *sqlstore.Store
	backend *fakeBackend
	cmd     *fakeCommander
	d       *Dispatcher
	clock   *time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	e := &env{t: t, ctx: context.Background(), mem: testdb.New(t), backend: newFakeBackend(), clock: &start}
	e.cmd = &fakeCommander{runner: e.mem}
	d, err := New(Config{
		Store:      e.mem,
		Runner:     e.mem,
		Backend:    e.backend,
		ControlURL: "http://control.test",
		Image:      "img",
		Now:        func() time.Time { return *e.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	d.SetCommander(e.cmd)
	e.d = d
	return e
}

func (e *env) advance(by time.Duration) { *e.clock = e.clock.Add(by) }

func (e *env) newRun(state core.RunState, prompt string) *core.Run {
	e.t.Helper()
	r := &core.Run{TeamID: "team", FactoryID: "fac", Prompt: prompt, State: state, CreatedAt: *e.clock}
	if err := e.mem.CreateRun(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) run(id string) *core.Run {
	e.t.Helper()
	r, err := e.mem.GetRun(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.d.Tick(e.ctx); err != nil {
		e.t.Fatalf("tick: %v", err)
	}
}

func (e *env) session(runID string, n int) *core.RunSession {
	e.t.Helper()
	s, err := e.mem.GetRunSession(e.ctx, runID, n)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) updateSession(runID string, n int, f func(*core.RunSession)) {
	e.t.Helper()
	s := e.session(runID, n)
	f(s)
	if err := e.mem.UpdateRunSession(e.ctx, s); err != nil {
		e.t.Fatal(err)
	}
}

// makeActive marks a session as active and ready.
func (e *env) makeActive(runID string, n int, caps ...string) {
	e.updateSession(runID, n, func(s *core.RunSession) {
		now := *e.clock
		s.State = core.SessionActive
		s.StartedAt = &now
		s.ReadyAt = &now
		s.Capabilities = caps
	})
}

func (e *env) endSessionRaw(runID string, n int) {
	e.updateSession(runID, n, func(s *core.RunSession) {
		now := *e.clock
		s.State = core.SessionEnded
		s.EndedAt = &now
		s.EndReason = "idle"
	})
}

func (e *env) commands(runID string, n int) []core.RunnerCommand {
	e.t.Helper()
	list, err := e.mem.ListRunnerCommands(e.ctx, runID, n, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func (e *env) inputs(runID string, states ...core.RunInputState) []core.RunInput {
	e.t.Helper()
	list, err := e.mem.ListRunInputs(e.ctx, runID, 0, states...)
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func event(t *testing.T, seq int64, typ runnerproto.EventType, turnID string, data any) core.RunnerEvent {
	t.Helper()
	ev, err := runnerproto.NewEvent(seq, typ, turnID, time.Now(), data)
	if err != nil {
		t.Fatal(err)
	}
	return core.RunnerEvent{Seq: ev.Seq, Type: string(ev.Type), TurnID: ev.TurnID, At: ev.At, Data: ev.Data}
}

func (e *env) events(s *core.RunSession, evs ...core.RunnerEvent) {
	e.d.EventsStored(e.ctx, s, evs)
}
