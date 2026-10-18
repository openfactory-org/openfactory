package dispatch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// namedBackend is a fakeBackend with its own name and a fixed task list.
type namedBackend struct {
	*fakeBackend
	name     string
	tasks    []core.TaskStatus
	statusEr error
	listErr  error
}

func (b *namedBackend) Name() string { return b.name }

func (b *namedBackend) Status(ctx context.Context, id string) (core.TaskStatus, error) {
	if b.statusEr != nil {
		return core.TaskStatus{}, b.statusEr
	}
	return b.fakeBackend.Status(ctx, id)
}

func (b *namedBackend) List(context.Context) ([]core.TaskStatus, error) { return b.tasks, b.listErr }

// fakeRouter gives one backend for the worker host "w1".
type fakeRouter struct {
	backend *namedBackend
	ready   bool
	asked   int
}

func (r *fakeRouter) BackendFor(run *core.Run) (core.TaskBackend, bool) {
	r.asked++
	if run.Config.WorkerHost != "w1" || !r.ready {
		return nil, false
	}
	return r.backend, true
}

func (r *fakeRouter) Backends() []core.TaskBackend { return []core.TaskBackend{r.backend} }

func routedEnv(t *testing.T) (*env, *fakeRouter) {
	t.Helper()
	e := newEnv(t)
	r := &fakeRouter{backend: &namedBackend{fakeBackend: newFakeBackend(), name: "worker:w1"}, ready: true}
	e.d.cfg.Router = r
	e.d.cfg.PublicControlURL = "https://public.test"
	return e, r
}

func (e *env) workerRun() *core.Run {
	e.t.Helper()
	r := &core.Run{TeamID: "team", FactoryID: "fac", Prompt: "p", State: core.RunQueued, CreatedAt: *e.clock}
	r.Config.WorkerHost = "w1"
	if err := e.mem.CreateRun(e.ctx, r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func TestWorkerRunUsesRouterBackend(t *testing.T) {
	e, r := routedEnv(t)
	run := e.workerRun()
	e.tick()
	if got := e.backend.startedTasks(); len(got) != 0 {
		t.Fatalf("the built-in backend got %d tasks", len(got))
	}
	started := r.backend.startedTasks()
	if len(started) != 1 || started[0].TaskID != run.ID+".1" {
		t.Fatalf("worker backend tasks = %+v", started)
	}
	if started[0].Env[core.TaskEnvControlURL] != "https://public.test" {
		t.Fatalf("control URL = %q, want the public URL", started[0].Env[core.TaskEnvControlURL])
	}
	if s := e.session(run.ID, 1); s.Backend != "worker:w1" {
		t.Fatalf("session backend = %q", s.Backend)
	}
}

func TestLocalRunKeepsInternalControlURL(t *testing.T) {
	e, r := routedEnv(t)
	run := e.newRun(core.RunQueued, "p") // no worker host
	e.tick()
	started := e.backend.startedTasks()
	if len(started) != 1 || started[0].Env[core.TaskEnvControlURL] != "http://control.test" {
		t.Fatalf("tasks = %+v", started)
	}
	if len(r.backend.startedTasks()) != 0 || r.asked != 0 {
		t.Fatal("the router must not be asked for a run without a worker host")
	}
	if s := e.session(run.ID, 1); s.Backend != "fake" {
		t.Fatalf("session backend = %q", s.Backend)
	}
	local := e.newRun(core.RunQueued, "p")
	local.Config.WorkerHost = "server"
	if err := e.mem.UpdateRun(e.ctx, local); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if r.asked != 0 {
		t.Fatal("the worker host server is a built-in host")
	}
}

func TestWorkerRunWaitsWithoutWorker(t *testing.T) {
	e, r := routedEnv(t)
	r.ready = false
	run := e.workerRun()
	for i := 0; i < 3; i++ {
		e.tick() // must not fail
	}
	if list, _ := e.mem.ListRunSessions(e.ctx, run.ID); len(list) != 0 {
		t.Fatalf("a run without a worker got %d sessions", len(list))
	}
	if got := e.run(run.ID); got.State != core.RunQueued {
		t.Fatalf("run state = %s", got.State)
	}
	if len(r.backend.startedTasks()) != 0 || len(e.backend.startedTasks()) != 0 {
		t.Fatal("no backend must get the task")
	}
	// The worker connects: the next tick starts the task.
	r.ready = true
	e.tick()
	if len(r.backend.startedTasks()) != 1 {
		t.Fatal("task must start when the worker is ready")
	}
}

func TestWorkerAtCapacityLeavesRunQueued(t *testing.T) {
	e, r := routedEnv(t)
	r.backend.startErr = fmt.Errorf("worker \"w1\" has no free slot: %w: %w", core.ErrAtCapacity, core.ErrUnavailable)
	run := e.workerRun()
	e.tick() // must not fail
	s := e.session(run.ID, 1)
	if s.State != core.SessionEnded || s.EndReason != "not_started" {
		t.Fatalf("session = %+v", s)
	}
	if got := e.run(run.ID); got.State != core.RunQueued {
		t.Fatalf("run = %s %s", got.State, got.FailureCode)
	}
	// It stays queued and is tried again, with no failure.
	e.tick()
	if got := e.run(run.ID); got.State != core.RunQueued {
		t.Fatalf("run state after second tick = %s", got.State)
	}
	r.backend.mu.Lock()
	r.backend.startErr = nil
	r.backend.mu.Unlock()
	e.tick()
	if len(r.backend.startedTasks()) != 1 {
		t.Fatal("task must start when the slot is free")
	}
}

func TestCancelGoesToOwningBackend(t *testing.T) {
	e, r := routedEnv(t)
	run := e.workerRun()
	e.tick()
	if err := e.d.StopSession(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	r.backend.mu.Lock()
	cancelled := slices.Clone(r.backend.cancelled)
	r.backend.mu.Unlock()
	if !slices.Contains(cancelled, run.ID+".1") {
		t.Fatalf("worker backend cancelled %v", cancelled)
	}
	e.backend.mu.Lock()
	n := len(e.backend.cancelled)
	e.backend.mu.Unlock()
	if n != 0 {
		t.Fatal("the built-in backend must not get the cancel")
	}
}

func TestLivenessAsksOwningBackend(t *testing.T) {
	e, r := routedEnv(t)
	run := e.workerRun()
	e.tick()
	task := run.ID + ".1"
	r.backend.setStatus(core.TaskStatus{TaskID: task, State: core.TaskExited, Reason: core.SandboxContainerOOM})
	e.advance(e.d.cfg.LostAfter + time.Second)
	e.tick()
	if s := e.session(run.ID, 1); s.State != core.SessionEnded || s.EndReason != "lost" {
		t.Fatalf("session = %+v", s)
	}
	if !slices.Contains(r.backend.destroyedIDs(), task) {
		t.Fatalf("worker backend destroyed %v", r.backend.destroyedIDs())
	}
	if len(e.backend.destroyedIDs()) != 0 {
		t.Fatal("the built-in backend must not get the destroy")
	}
}

func TestUnavailableBackendGivesNoAnswer(t *testing.T) {
	e, r := routedEnv(t)
	run := e.workerRun()
	e.tick()
	e.makeActive(run.ID, 1)
	r.backend.statusEr = fmt.Errorf("worker is not connected: %w", core.ErrUnavailable)
	e.advance(e.d.cfg.LostAfter + time.Second)
	e.tick() // no error, no loss
	if s := e.session(run.ID, 1); s.State != core.SessionActive {
		t.Fatalf("session = %+v", s)
	}
	// After the second wait the run is lost with runner_lost, like any silent runner.
	e.advance(2 * e.d.cfg.LostAfter)
	e.tick()
	if s := e.session(run.ID, 1); s.State != core.SessionEnded {
		t.Fatalf("session = %+v", s)
	}
}

func TestSessionOfUnknownBackendIsUnavailable(t *testing.T) {
	e, _ := routedEnv(t)
	s := &core.RunSession{Backend: "worker:gone", TaskID: "r.1"}
	_, err := e.d.backendOf(s).Status(e.ctx, "r.1")
	if !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("status: %v", err)
	}
	if got := e.d.backendOf(&core.RunSession{Backend: "fake"}); got.Name() != "fake" {
		t.Fatalf("backend = %s", got.Name())
	}
	if got := e.d.backendOf(&core.RunSession{}); got.Name() != "fake" {
		t.Fatalf("backend of a session without name = %s", got.Name())
	}
}

func TestSweepCoversRouterBackends(t *testing.T) {
	e, r := routedEnv(t)
	old := e.clock.Add(-10 * time.Minute)
	r.backend.tasks = []core.TaskStatus{{TaskID: "orphan.1", StartedAt: &old}}
	e.tick()
	if !slices.Contains(r.backend.destroyedIDs(), "orphan.1") {
		t.Fatalf("worker backend destroyed %v", r.backend.destroyedIDs())
	}
	// A worker that is not connected is skipped without an error.
	r.backend.listErr = fmt.Errorf("not connected: %w", core.ErrUnavailable)
	e.advance(2 * time.Minute)
	e.tick()
}

func TestMaxSandboxesIgnoresWorkerSessions(t *testing.T) {
	e, r := routedEnv(t)
	e.d.cfg.MaxSandboxes = 1
	e.workerRun()
	e.newRun(core.RunQueued, "p")
	e.tick()
	if len(r.backend.startedTasks()) != 1 || len(e.backend.startedTasks()) != 1 {
		t.Fatalf("worker tasks %d, local tasks %d", len(r.backend.startedTasks()), len(e.backend.startedTasks()))
	}
	// A second local run waits, a second worker run does not.
	e.newRun(core.RunQueued, "p")
	e.workerRun()
	e.tick()
	if len(e.backend.startedTasks()) != 1 {
		t.Fatal("the limit holds for the built-in backend")
	}
	if len(r.backend.startedTasks()) != 2 {
		t.Fatalf("worker tasks = %d, want 2", len(r.backend.startedTasks()))
	}
}

func TestFollowerClaimsOnlyWorkerRuns(t *testing.T) {
	e, _ := routedEnv(t)
	local := e.newRun(core.RunQueued, "local")
	remote := e.workerRun()
	// A replica that is not the leader claims only the runs of the workers
	// connected to it.
	if err := e.d.claimQueued(e.ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := e.run(local.ID).State; got != core.RunQueued {
		t.Errorf("local run = %s, want queued", got)
	}
	if got := e.run(remote.ID).State; got != core.RunPending {
		t.Errorf("worker run = %s, want pending", got)
	}
	// The leader claims the rest.
	if err := e.d.claimQueued(e.ctx, true); err != nil {
		t.Fatal(err)
	}
	if got := e.run(local.ID).State; got != core.RunPending {
		t.Errorf("local run after leader = %s, want pending", got)
	}
}
