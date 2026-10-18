package direct

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/internal/ws"
	"github.com/openfactory-org/openfactory/internal/worker"
)

// fakeStore records the workers that the hub reports.
type fakeStore struct {
	core.Store
	mu      sync.Mutex
	workers map[string]core.Worker
	upserts int
}

func (f *fakeStore) UpsertWorker(_ context.Context, w *core.Worker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.workers == nil {
		f.workers = map[string]core.Worker{}
	}
	f.workers[w.ID] = *w
	f.upserts++
	return nil
}

// fakeTasks is a core.TaskBackend that the test controls.
type fakeTasks struct {
	mu        sync.Mutex
	tasks     map[string]core.TaskStatus
	started   []core.Task
	cancelled map[string]int
	destroyed []string
	watchers  map[string][]chan core.TaskStatus
	startErr  error
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{tasks: map[string]core.TaskStatus{}, cancelled: map[string]int{}, watchers: map[string][]chan core.TaskStatus{}}
}

func (f *fakeTasks) Name() string { return "fake" }

func (f *fakeTasks) Start(_ context.Context, t core.Task) (core.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return core.TaskStatus{}, f.startErr
	}
	if st, ok := f.tasks[t.TaskID]; ok {
		return st, nil
	}
	f.started = append(f.started, t)
	st := core.TaskStatus{TaskID: t.TaskID, State: core.TaskStarting}
	f.tasks[t.TaskID] = st
	return st, nil
}

func (f *fakeTasks) Status(_ context.Context, id string) (core.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.tasks[id]
	if !ok {
		return core.TaskStatus{}, core.ErrNotFound
	}
	return st, nil
}

func (f *fakeTasks) Watch(ctx context.Context, id string) (<-chan core.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.tasks[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	ch := make(chan core.TaskStatus, 16)
	ch <- st
	if st.State == core.TaskExited {
		close(ch)
		return ch, nil
	}
	f.watchers[id] = append(f.watchers[id], ch)
	return ch, nil
}

func (f *fakeTasks) Cancel(_ context.Context, id string, grace int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tasks[id]; !ok {
		return core.ErrNotFound
	}
	f.cancelled[id] = grace
	return nil
}

func (f *fakeTasks) Destroy(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroyed = append(f.destroyed, id)
	delete(f.tasks, id)
	return nil
}

func (f *fakeTasks) List(context.Context) ([]core.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.TaskStatus
	for _, st := range f.tasks {
		out = append(out, st)
	}
	return out, nil
}

func (f *fakeTasks) Preflight(context.Context) error { return nil }

// set changes the state of a task and tells the watchers.
func (f *fakeTasks) set(st core.TaskStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[st.TaskID] = st
	for _, ch := range f.watchers[st.TaskID] {
		ch <- st
		if st.State == core.TaskExited {
			close(ch)
		}
	}
	if st.State == core.TaskExited {
		delete(f.watchers, st.TaskID)
	}
}

func exited(id string, reason core.SandboxReason) core.TaskStatus {
	code := 0
	if reason != core.SandboxCompleted {
		code = 3
	}
	return core.TaskStatus{TaskID: id, State: core.TaskExited, Reason: reason, ExitCode: &code}
}

func testTask(n int) core.Task {
	run := "run_" + strconv.Itoa(n)
	return core.Task{
		TaskID: core.TaskID(run, 1), RunID: run, Session: 1, TeamID: "team_1", Image: "img",
		Platform: core.TaskPlatform{OS: "linux", Arch: "amd64"},
		Env: map[string]string{
			core.TaskEnvControlURL: "https://control.test", core.TaskEnvRunID: run,
			core.TaskEnvSession: "1", core.TaskEnvBootstrapToken: "tok",
		},
		MaxSessionSeconds: 60,
	}
}

type v2Env struct {
	hub     *Hub
	srv     *httptest.Server
	backend *fakeTasks
	hb      *HubBackend
	cancel  context.CancelFunc
	done    chan error
}

type v2Opts struct {
	capacity  int
	oneShot   bool
	authorize func(ctx context.Context, token, workerID string) error
	taskEnv   map[string]string
	image     string
	metrics   *worker.Metrics
}

func startV2(t *testing.T, o v2Opts) *v2Env {
	t.Helper()
	hub := NewHub(&fakeStore{}, HubOptions{
		HeartbeatInterval: 100 * time.Millisecond,
		AuthorizeWorker:   o.authorize,
		Authenticate: func(_ context.Context, token string) (string, string, error) {
			if token == "good-key" {
				return "team_1", "usr_1", nil
			}
			return "", "", core.ErrUnauthorized
		},
	})
	mux := http.NewServeMux()
	hub.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	e := &v2Env{hub: hub, srv: srv, backend: newFakeTasks(), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	t.Cleanup(cancel)
	go func() {
		e.done <- RunWorkerV2(ctx, WorkerV2Options{
			ServerURL: srv.URL, APIKey: "good-key", WorkerID: "edge-1", Capacity: o.capacity,
			Backend: e.backend, OneShot: o.oneShot, TaskEnv: o.taskEnv, DefaultImage: o.image, Metrics: o.metrics,
			MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		})
	}()
	e.hb = hub.WorkerBackend("edge-1")
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	for e.hb.liveConn() == nil {
		select {
		case <-wctx.Done():
			t.Fatal("worker did not connect")
		case <-time.After(10 * time.Millisecond):
		}
	}
	return e
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestV2AssignStatusAndExit(t *testing.T) {
	e := startV2(t, v2Opts{taskEnv: map[string]string{"OF_IDLE_TIMEOUT_MAX_SECONDS": "300"}, image: "worker-img"})
	if e.hb.Name() != "worker:edge-1" {
		t.Fatalf("name %q", e.hb.Name())
	}
	task := testTask(1)
	task.Image = "local"
	st, err := e.hb.Start(context.Background(), task)
	if err != nil || st.State != core.TaskStarting {
		t.Fatalf("start = %+v, %v", st, err)
	}
	e.backend.mu.Lock()
	got := e.backend.started[0]
	e.backend.mu.Unlock()
	if got.Env["OF_IDLE_TIMEOUT_MAX_SECONDS"] != "300" || got.Image != "worker-img" || got.Env[core.TaskEnvBootstrapToken] != "tok" {
		t.Fatalf("task on the worker = %+v", got)
	}
	// Start twice returns the same task.
	if st2, err := e.hb.Start(context.Background(), task); err != nil || st2.TaskID != task.TaskID {
		t.Fatalf("second start = %+v, %v", st2, err)
	}
	ch, err := e.hb.Watch(context.Background(), task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if first := <-ch; first.State != core.TaskStarting {
		t.Fatalf("first watch status %+v", first)
	}
	e.backend.set(core.TaskStatus{TaskID: task.TaskID, State: core.TaskRunning})
	if s := <-ch; s.State != core.TaskRunning {
		t.Fatalf("watch status %+v", s)
	}
	eventually(t, "running status", func() bool {
		s, err := e.hb.Status(context.Background(), task.TaskID)
		return err == nil && s.State == core.TaskRunning
	})
	e.backend.set(exited(task.TaskID, core.SandboxCompleted))
	s := <-ch
	if s.State != core.TaskExited || s.Reason != core.SandboxCompleted || s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("exit status %+v", s)
	}
	if _, open := <-ch; open {
		t.Fatal("watch channel must close after exited")
	}
	if s, _ := e.hb.Status(context.Background(), task.TaskID); s.State != core.TaskExited {
		t.Fatalf("status %+v", s)
	}
	list, err := e.hb.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if err := e.hb.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ws := e.hub.Workers(); len(ws) != 1 || ws[0].Hello.WorkerID != "edge-1" || !e.hub.Connected("edge-1") {
		t.Fatalf("workers = %+v", ws)
	}
}

func TestV2StatusAsksWorkerForUnknownTask(t *testing.T) {
	e := startV2(t, v2Opts{})
	if _, err := e.hb.Status(context.Background(), "run_x.1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	// A task that the worker holds but the hub never saw.
	e.backend.set(core.TaskStatus{TaskID: "run_y.1", State: core.TaskRunning})
	st, err := e.hb.Status(context.Background(), "run_y.1")
	if err != nil || st.State != core.TaskRunning {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestV2CancelAndDestroyReachWorker(t *testing.T) {
	e := startV2(t, v2Opts{})
	task := testTask(1)
	if _, err := e.hb.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := e.hb.Cancel(context.Background(), task.TaskID, 120); err != nil {
		t.Fatal(err)
	}
	e.backend.mu.Lock()
	grace, ok := e.backend.cancelled[task.TaskID]
	e.backend.mu.Unlock()
	if !ok || grace != 120 {
		t.Fatalf("cancel grace = %d, %v", grace, ok)
	}
	if err := e.hb.Cancel(context.Background(), "run_none.1", 0); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cancel unknown: %v", err)
	}
	if err := e.hb.Destroy(context.Background(), task.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := e.hb.Destroy(context.Background(), task.TaskID); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
	e.backend.mu.Lock()
	n := len(e.backend.destroyed)
	e.backend.mu.Unlock()
	if n != 2 {
		t.Fatalf("destroy calls = %d", n)
	}
	if list, _ := e.hb.List(context.Background()); len(list) != 0 {
		t.Fatalf("list after destroy = %v", list)
	}
}

func TestV2AtCapacity(t *testing.T) {
	e := startV2(t, v2Opts{capacity: 1})
	run := &core.Run{TeamID: "team_1"}
	run.Config.WorkerHost = "edge-1"
	if _, ok := e.hub.BackendFor(run); !ok {
		t.Fatal("a free worker must give a backend")
	}
	if _, err := e.hb.Start(context.Background(), testTask(1)); err != nil {
		t.Fatal(err)
	}
	_, err := e.hb.Start(context.Background(), testTask(2))
	if !errors.Is(err, core.ErrAtCapacity) {
		t.Fatalf("second task: %v", err)
	}
	if _, ok := e.hub.BackendFor(run); ok {
		t.Fatal("a full worker must not give a backend")
	}
	other := &core.Run{TeamID: "team_2"}
	other.Config.WorkerHost = "edge-1"
	if _, ok := e.hub.BackendFor(other); ok {
		t.Fatal("a worker of another team must not give a backend")
	}
	// The worker itself also refuses: ask it directly, past the hub check.
	c := e.hb.liveConn()
	e.hb.mu.Lock()
	delete(e.hb.tasks, "run_1.1")
	e.hb.mu.Unlock()
	err = c.call(context.Background(), methodTaskAssign, assignParams{Task: testTask(2)}, nil)
	if !errors.Is(err, core.ErrAtCapacity) {
		t.Fatalf("worker refusal: %v", err)
	}
	// Free the slot.
	e.backend.set(exited("run_1.1", core.SandboxCompleted))
	eventually(t, "free slot", func() bool { _, ok := e.hub.BackendFor(run); return ok })
}

func TestV2NotConnected(t *testing.T) {
	hub := NewHub(&fakeStore{}, HubOptions{})
	b := hub.WorkerBackend("gone")
	_, err := b.Start(context.Background(), testTask(1))
	if !errors.Is(err, core.ErrAtCapacity) || !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("start: %v", err)
	}
	if _, err := b.Status(context.Background(), "run_1.1"); !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("status: %v", err)
	}
	if err := b.Cancel(context.Background(), "run_1.1", 0); !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := b.List(context.Background()); !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("list: %v", err)
	}
	if err := b.Preflight(context.Background()); !errors.Is(err, core.ErrUnavailable) {
		t.Fatalf("preflight: %v", err)
	}
	run := &core.Run{TeamID: "team_1"}
	run.Config.WorkerHost = "gone"
	if _, ok := hub.BackendFor(run); ok {
		t.Fatal("no connection must give no backend")
	}
	if got := hub.Backends(); len(got) != 1 || got[0].Name() != "worker:gone" {
		t.Fatalf("backends = %v", got)
	}
}

func TestV2ReconnectRestoresStatuses(t *testing.T) {
	e := startV2(t, v2Opts{})
	a, b := testTask(1), testTask(2)
	for _, task := range []core.Task{a, b} {
		if _, err := e.hb.Start(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	e.backend.set(core.TaskStatus{TaskID: a.TaskID, State: core.TaskRunning})
	eventually(t, "running", func() bool {
		s, _ := e.hb.Status(context.Background(), a.TaskID)
		return s.State == core.TaskRunning
	})
	// Drop the connection. The hub keeps the last known statuses.
	old := e.hb.liveConn()
	_ = old.conn.Close()
	eventually(t, "disconnect", func() bool { return old.isClosed() })
	if s, err := e.hb.Status(context.Background(), a.TaskID); err != nil || s.State != core.TaskRunning {
		t.Fatalf("status while disconnected = %+v, %v", s, err)
	}
	if err := e.hb.Cancel(context.Background(), a.TaskID, 0); !errors.Is(err, core.ErrUnavailable) && e.hb.liveConn() == nil {
		t.Fatalf("cancel while disconnected: %v", err)
	}
	// The task exits while the worker has no connection, and the other one disappears.
	e.backend.set(exited(a.TaskID, core.SandboxProcessFailed))
	e.backend.mu.Lock()
	delete(e.backend.tasks, b.TaskID)
	e.backend.mu.Unlock()
	eventually(t, "reconnect", func() bool { c := e.hb.liveConn(); return c != nil && c != old })
	eventually(t, "restored status", func() bool {
		s, err := e.hb.Status(context.Background(), a.TaskID)
		return err == nil && s.State == core.TaskExited && s.Reason == core.SandboxProcessFailed
	})
	if _, err := e.hb.Status(context.Background(), b.TaskID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("lost task: %v", err)
	}
	list, _ := e.hb.List(context.Background())
	if len(list) != 1 || list[0].TaskID != a.TaskID {
		t.Fatalf("list = %v", list)
	}
}

func TestV2OneShot(t *testing.T) {
	e := startV2(t, v2Opts{oneShot: true})
	run := &core.Run{TeamID: "team_1"}
	run.Config.WorkerHost = "edge-1"
	task := testTask(1)
	if _, err := e.hb.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hb.Start(context.Background(), testTask(2)); !errors.Is(err, core.ErrAtCapacity) {
		t.Fatalf("second task: %v", err)
	}
	if _, ok := e.hub.BackendFor(run); ok {
		t.Fatal("a one-shot worker with its task must give no backend")
	}
	e.backend.set(exited(task.TaskID, core.SandboxCompleted))
	select {
	case err := <-e.done:
		if err != nil {
			t.Fatalf("worker result after completed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("one-shot worker did not exit")
	}
	eventually(t, "exited status at the hub", func() bool {
		s, err := e.hb.Status(context.Background(), task.TaskID)
		return err == nil && s.State == core.TaskExited && s.Reason == core.SandboxCompleted
	})
	e.backend.mu.Lock()
	destroyed := len(e.backend.destroyed)
	e.backend.mu.Unlock()
	if destroyed != 1 {
		t.Fatalf("a completed one-shot task must be destroyed, got %d calls", destroyed)
	}
}

func TestV2OneShotFailureReturnsError(t *testing.T) {
	e := startV2(t, v2Opts{oneShot: true})
	task := testTask(1)
	if _, err := e.hb.Start(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	e.backend.set(exited(task.TaskID, core.SandboxProcessFailed))
	select {
	case err := <-e.done:
		var oe *OneShotError
		if !errors.As(err, &oe) || oe.Status.Reason != core.SandboxProcessFailed {
			t.Fatalf("worker result = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("one-shot worker did not exit")
	}
	e.backend.mu.Lock()
	destroyed := len(e.backend.destroyed)
	e.backend.mu.Unlock()
	if destroyed != 0 {
		t.Fatal("a failed one-shot task must stay for its retention time")
	}
}

func TestOneShotExitCode(t *testing.T) {
	for _, tc := range []struct {
		st   core.TaskStatus
		want int
	}{
		{exited("t.1", core.SandboxCompleted), 0},
		{exited("t.1", core.SandboxProcessFailed), 1},
		{exited("t.1", core.SandboxCancelled), 1},
		{exited("t.1", core.SandboxContainerOOM), 1},
		{exited("t.1", core.SandboxUnknown), 1},
		{core.TaskStatus{TaskID: "t.1", State: core.TaskRunning}, 1},
	} {
		if got := OneShotExitCode(tc.st); got != tc.want {
			t.Errorf("%s/%s: exit code %d, want %d", tc.st.State, tc.st.Reason, got, tc.want)
		}
	}
}

func TestV2WorkerKeyLimitedToWorkerIDs(t *testing.T) {
	hub := NewHub(&fakeStore{}, HubOptions{
		HeartbeatInterval: 100 * time.Millisecond,
		Authenticate:      func(context.Context, string) (string, string, error) { return "team_1", "usr_1", nil },
		AuthorizeWorker: func(_ context.Context, _, id string) error {
			if id == "allowed" {
				return nil
			}
			return core.ErrForbidden
		},
	})
	mux := http.NewServeMux()
	hub.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunWorkerV2(ctx, WorkerV2Options{ServerURL: srv.URL, APIKey: "k", WorkerID: "other", Backend: newFakeTasks()})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("other id: %v", err)
	}
	if hub.Connected("other") {
		t.Fatal("refused worker is connected")
	}
}

func TestV2WorkerIDOfAnotherTeamIsRefused(t *testing.T) {
	e := startV2(t, v2Opts{})
	// A second connection of another team with the same ID.
	hub2 := e.hub
	hub2.opts.Authenticate = func(context.Context, string) (string, string, error) { return "team_2", "usr_1", nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunWorkerV2(ctx, WorkerV2Options{ServerURL: e.srv.URL, APIKey: "k", WorkerID: "edge-1", Backend: newFakeTasks()})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("same id, other team: %v", err)
	}
}

func TestV2WorkerIDOfAnotherPrincipalIsRefused(t *testing.T) {
	e := startV2(t, v2Opts{})
	e.hub.opts.Authenticate = func(_ context.Context, token string) (string, string, error) {
		if token == "other-key" {
			return "team_1", "usr_2", nil
		}
		return "team_1", "usr_1", nil
	}
	first := e.hb.liveConn().id
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunWorkerV2(ctx, WorkerV2Options{ServerURL: e.srv.URL, APIKey: "other-key", WorkerID: "edge-1", Backend: newFakeTasks()})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("same id, other principal: %v", err)
	}
	if c := e.hb.liveConn(); c == nil || c.id != first {
		t.Fatal("a key of another principal replaced the live connection")
	}
	// The same principal can reconnect and replace the connection.
	go func() {
		_ = RunWorkerV2(ctx, WorkerV2Options{ServerURL: e.srv.URL, APIKey: "good-key", WorkerID: "edge-1", Backend: newFakeTasks(),
			MinBackoff: time.Hour, MaxBackoff: time.Hour})
	}()
	eventually(t, "reconnect of the same principal", func() bool {
		c := e.hb.liveConn()
		return c != nil && c.id != first
	})
}

func TestV2BadKeyIsUnauthorized(t *testing.T) {
	e := startV2(t, v2Opts{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunWorkerV2(ctx, WorkerV2Options{ServerURL: e.srv.URL, APIKey: "bad", WorkerID: "edge-2", Backend: newFakeTasks()})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad key: %v", err)
	}
}

func TestV2InvalidSpecIsRefused(t *testing.T) {
	e := startV2(t, v2Opts{})
	c := e.hb.liveConn()
	bad := testTask(1)
	bad.Image = ""
	err := c.call(context.Background(), methodTaskAssign, assignParams{Task: bad}, nil)
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("invalid task: %v", err)
	}
	e.backend.mu.Lock()
	e.backend.startErr = core.ErrAtCapacity
	e.backend.mu.Unlock()
	if _, err := e.hb.Start(context.Background(), testTask(2)); !errors.Is(err, core.ErrAtCapacity) {
		t.Fatalf("backend at capacity: %v", err)
	}
	if list, _ := e.hb.List(context.Background()); len(list) != 0 {
		t.Fatalf("a failed start must free its slot: %v", list)
	}
}

// TestWorkerIDValidation checks the rules for the ID of a worker.
func TestWorkerIDValidation(t *testing.T) {
	for id, ok := range map[string]bool{"prod-runner-1": true, "ci.worker_2": true, "": false, "-x": false, "local": false, "managed": false, "openfactory-x": false, strings.Repeat("a", 65): false, "a b": false} {
		if err := ValidateWorkerID(id); (err == nil) != ok {
			t.Errorf("ValidateWorkerID(%q) = %v", id, err)
		}
	}
}

// TestV1WorkerIsRefused checks that a worker that offers only the removed
// protocol version 1 gets a clear close reason, and that the hub keeps no
// connection for it.
func TestV1WorkerIsRefused(t *testing.T) {
	e := startV2(t, v2Opts{})
	u := "ws" + strings.TrimPrefix(e.srv.URL, "http") + ConnectPath
	h := http.Header{}
	h.Set("Authorization", "Bearer good-key")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := ws.Dial(ctx, nil, e.srv.URL+ConnectPath, h, []string{"openfactory.worker.v1"})
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	defer conn.Close()
	if conn.Subprotocol != "" {
		t.Fatalf("the hub selected the subprotocol %q", conn.Subprotocol)
	}
	_, _, err = conn.ReadMessage()
	var ce *ws.CloseError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "protocol v2") {
		t.Fatalf("read after the handshake: %v", err)
	}
	if e.hub.Connected("edge-v1") {
		t.Fatal("the hub holds a v1 connection")
	}
}
