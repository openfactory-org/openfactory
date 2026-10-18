package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func (e *env) newRunIn(factory string) *core.Run {
	e.t.Helper()
	r := &core.Run{TeamID: "team", FactoryID: factory, Prompt: "p", State: core.RunQueued, CreatedAt: *e.clock}
	if err := e.mem.CreateRun(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) setRunState(id string, st core.RunState) {
	e.t.Helper()
	r := e.run(id)
	r.State = st
	if err := e.mem.UpdateRun(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) hasSession(runID string) bool {
	e.t.Helper()
	_, err := e.mem.LatestRunSession(e.ctx, runID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		e.t.Fatal(err)
	}
	return err == nil
}

func TestQueueTimeout(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.advance(31 * time.Minute)
	e.tick()
	got := e.run(run.ID)
	if got.State != core.RunError || got.FailureCode != core.FailureInfrastructureTimeout {
		t.Fatalf("run = %s %s", got.State, got.FailureCode)
	}
	if e.hasSession(run.ID) || len(e.backend.startedTasks()) != 0 {
		t.Fatal("a timed out run must get no session and no task")
	}
}

func TestQueueTimeoutCountsFromQueuedAt(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.advance(time.Hour)
	r := e.run(run.ID)
	now := *e.clock
	r.QueuedAt = &now
	if err := e.mem.UpdateRun(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if got := e.run(run.ID).State; got != core.RunPending {
		t.Fatalf("run state = %s, want pending (queued 0 minutes ago)", got)
	}
}

func TestMoveToQueuedSetsQueuedAt(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunSucceeded, "p")
	e.advance(time.Hour)
	if err := e.d.requeue(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	got := e.run(run.ID)
	if got.QueuedAt == nil || !got.QueuedAt.Equal(*e.clock) {
		t.Fatalf("QueuedAt = %v, want %v", got.QueuedAt, *e.clock)
	}
	e.tick() // one hour after CreatedAt, but only now in the queue
	if got := e.run(run.ID).State; got != core.RunPending {
		t.Fatalf("run state = %s, want pending", got)
	}
}

func TestMaxSandboxes(t *testing.T) {
	e := newEnv(t)
	e.d.cfg.MaxSandboxes = 1
	a := e.newRun(core.RunQueued, "a")
	e.tick()
	b := e.newRun(core.RunQueued, "b")
	e.tick()
	if got := e.run(b.ID).State; got != core.RunQueued || e.hasSession(b.ID) {
		t.Fatalf("b = %s, want queued without a session", got)
	}
	e.endSessionRaw(a.ID, 1)
	e.tick()
	if got := e.run(b.ID).State; got != core.RunPending {
		t.Fatalf("b = %s, want pending after a slot is free", got)
	}
}

func TestMaxSandboxesWithinOneTick(t *testing.T) {
	e := newEnv(t)
	e.d.cfg.MaxSandboxes = 1
	a := e.newRun(core.RunQueued, "a")
	e.advance(time.Second)
	b := e.newRun(core.RunQueued, "b")
	e.tick()
	if e.run(a.ID).State != core.RunPending || e.run(b.ID).State != core.RunQueued {
		t.Fatalf("a = %s, b = %s", e.run(a.ID).State, e.run(b.ID).State)
	}
}

func TestFactoryLimit(t *testing.T) {
	e := newEnv(t)
	e.d.cfg.FactoryLimit = func(_ context.Context, f string) (int, error) {
		if f == "limited" {
			return 1, nil
		}
		return 0, nil
	}
	l1 := e.newRunIn("limited")
	e.advance(time.Second)
	l2 := e.newRunIn("limited")
	o1 := e.newRunIn("other")
	e.tick()
	if e.run(l1.ID).State != core.RunPending || e.run(l2.ID).State != core.RunQueued || e.run(o1.ID).State != core.RunPending {
		t.Fatalf("l1 = %s, l2 = %s, o1 = %s", e.run(l1.ID).State, e.run(l2.ID).State, e.run(o1.ID).State)
	}
	e.tick()
	if e.run(l2.ID).State != core.RunQueued {
		t.Fatal("l2 must stay queued while the factory is at its limit")
	}
	e.endSessionRaw(l1.ID, 1)
	e.tick()
	if e.run(l2.ID).State != core.RunPending {
		t.Fatalf("l2 = %s, want pending", e.run(l2.ID).State)
	}
}

// activeSession claims a run and makes its session active.
func (e *env) activeSession(state core.RunState, turn string) *core.Run {
	e.t.Helper()
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	e.makeActive(run.ID, 1)
	e.updateSession(run.ID, 1, func(s *core.RunSession) { s.ActiveTurnID = turn })
	e.setRunState(run.ID, state)
	return run
}

func (e *env) stopCommands(runID string) []core.RunnerCommand {
	var out []core.RunnerCommand
	for _, c := range e.commands(runID, 1) {
		if c.Type == string(runnerproto.CommandSessionStop) {
			out = append(out, c)
		}
	}
	return out
}

func TestMaxSessionTimeWithActiveTurn(t *testing.T) {
	e := newEnv(t)
	e.d.cfg.MaxSessionSeconds = 3600
	run := e.activeSession(core.RunRunning, "turn-1")
	e.advance(59 * time.Minute)
	e.seen(run.ID)
	e.tick()
	if len(e.stopCommands(run.ID)) != 0 {
		t.Fatal("no stop before the deadline")
	}
	e.advance(2 * time.Minute)
	e.seen(run.ID)
	e.tick()
	stops := e.stopCommands(run.ID)
	if len(stops) != 1 {
		t.Fatalf("stop commands = %d, want 1", len(stops))
	}
	var data runnerproto.SessionStopData
	if err := json.Unmarshal(stops[0].Data, &data); err != nil || data.Reason != "deadline" || !data.Snapshot {
		t.Fatalf("stop data = %+v, %v", data, err)
	}
	got := e.run(run.ID)
	if got.State != core.RunError || got.FailureCode != core.FailureInfrastructureTimeout {
		t.Fatalf("run = %s %s", got.State, got.FailureCode)
	}
	e.advance(10 * time.Second)
	e.seen(run.ID)
	e.tick()
	if len(e.stopCommands(run.ID)) != 1 {
		t.Fatal("the stop must be sent once")
	}
	// The runner does not end the session. After the grace the task is stopped.
	e.advance(e.d.cfg.StopGrace + e.d.cfg.LostAfter + time.Minute)
	e.tick()
	if s := e.session(run.ID, 1); s.State != core.SessionEnded || s.EndReason != "lost" {
		t.Fatalf("session = %s %s", s.State, s.EndReason)
	}
	if !slices.Contains(e.backend.cancelled, run.ID+".1") || !slices.Contains(e.backend.destroyedIDs(), run.ID+".1") {
		t.Fatalf("cancelled %v destroyed %v", e.backend.cancelled, e.backend.destroyedIDs())
	}
	if got := e.run(run.ID); got.State != core.RunError {
		t.Fatalf("run state = %s", got.State)
	}
}

func TestMaxSessionTimeWithoutActiveTurn(t *testing.T) {
	e := newEnv(t)
	e.d.cfg.MaxSessionSeconds = 3600
	run := e.activeSession(core.RunSucceeded, "")
	e.advance(61 * time.Minute)
	e.seen(run.ID)
	e.tick()
	if len(e.stopCommands(run.ID)) != 1 {
		t.Fatalf("stop commands = %d, want 1", len(e.stopCommands(run.ID)))
	}
	if got := e.run(run.ID).State; got != core.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded", got)
	}
	e.advance(e.d.cfg.StopGrace + e.d.cfg.LostAfter + time.Minute)
	e.tick()
	if s := e.session(run.ID, 1); s.State != core.SessionEnded || s.EndReason != "lost" {
		t.Fatalf("session = %s %s", s.State, s.EndReason)
	}
	if got := e.run(run.ID).State; got != core.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded", got)
	}
	if !slices.Contains(e.backend.destroyedIDs(), run.ID+".1") {
		t.Fatal("task must be destroyed")
	}
}

// listBackend is a fakeBackend with a fixed task list.
type listBackend struct {
	*fakeBackend
	tasks []core.TaskStatus
}

func (b *listBackend) List(context.Context) ([]core.TaskStatus, error) { return b.tasks, nil }

func TestSweepDestroysOrphans(t *testing.T) {
	e := newEnv(t)
	live := e.newRun(core.RunQueued, "p")
	e.tick() // session 1 of live is assigned
	ended := e.newRun(core.RunQueued, "p")
	e.tick()
	e.endSessionRaw(ended.ID, 1)
	e.advance(10 * time.Minute)
	old := e.clock.Add(-5 * time.Minute)
	fresh := *e.clock
	e.d.cfg.Backend = &listBackend{fakeBackend: e.backend, tasks: []core.TaskStatus{
		{TaskID: live.ID + ".1", StartedAt: &old},
		{TaskID: "orphan-run.1", StartedAt: &old},
		{TaskID: "orphan-nostart.1"},
		{TaskID: "young-orphan.1", StartedAt: &fresh},
		{TaskID: ended.ID + ".1", StartedAt: &old},
		{TaskID: "not-ours", StartedAt: &old},
	}}
	e.tick()
	got := e.backend.destroyedIDs()
	for _, want := range []string{"orphan-run.1", "orphan-nostart.1", ended.ID + ".1"} {
		if !slices.Contains(got, want) {
			t.Errorf("task %s must be destroyed; destroyed %v", want, got)
		}
	}
	for _, keep := range []string{live.ID + ".1", "young-orphan.1", "not-ours"} {
		if slices.Contains(got, keep) {
			t.Errorf("task %s must be kept; destroyed %v", keep, got)
		}
	}
}

func TestSweepRunsEveryInterval(t *testing.T) {
	e := newEnv(t)
	b := &countBackend{fakeBackend: e.backend}
	e.d.cfg.Backend = b
	e.tick()
	e.tick()
	if b.lists != 1 {
		t.Fatalf("lists = %d, want 1 within the interval", b.lists)
	}
	e.advance(2 * time.Minute)
	e.tick()
	if b.lists != 2 {
		t.Fatalf("lists = %d, want 2", b.lists)
	}
}

type countBackend struct {
	*fakeBackend
	lists int
}

func (b *countBackend) List(context.Context) ([]core.TaskStatus, error) {
	b.lists++
	return nil, nil
}

// seen marks session 1 of a run as heard from now, as a live runner does.
func (e *env) seen(runID string) {
	e.t.Helper()
	if err := e.d.cfg.Runner.TouchRunSession(context.Background(), runID, 1, *e.clock); err != nil {
		e.t.Fatal(err)
	}
}
