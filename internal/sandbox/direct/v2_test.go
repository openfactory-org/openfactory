package direct

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct/directtest"
	"github.com/openfactory-org/openfactory/internal/worker"
)

// The worker side of protocol v2 against the fake hub of directtest. The
// hub side is tested in apps/control-plane (WorkerHubDO).

// fakeTasks is a rtcore.TaskBackend that the test controls.
type fakeTasks struct {
	mu        sync.Mutex
	tasks     map[string]rtcore.TaskStatus
	started   []rtcore.Task
	cancelled map[string]int
	destroyed []string
	watchers  map[string][]chan rtcore.TaskStatus
	startErr  error
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{tasks: map[string]rtcore.TaskStatus{}, cancelled: map[string]int{}, watchers: map[string][]chan rtcore.TaskStatus{}}
}

func (f *fakeTasks) Name() string { return "fake" }

func (f *fakeTasks) Start(_ context.Context, t rtcore.Task) (rtcore.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return rtcore.TaskStatus{}, f.startErr
	}
	if st, ok := f.tasks[t.TaskID]; ok {
		return st, nil
	}
	f.started = append(f.started, t)
	st := rtcore.TaskStatus{TaskID: t.TaskID, State: rtcore.TaskStarting}
	f.tasks[t.TaskID] = st
	return st, nil
}

func (f *fakeTasks) Status(_ context.Context, id string) (rtcore.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.tasks[id]
	if !ok {
		return rtcore.TaskStatus{}, rtcore.ErrNotFound
	}
	return st, nil
}

func (f *fakeTasks) Watch(ctx context.Context, id string) (<-chan rtcore.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.tasks[id]
	if !ok {
		return nil, rtcore.ErrNotFound
	}
	ch := make(chan rtcore.TaskStatus, 16)
	ch <- st
	if st.State == rtcore.TaskExited {
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
		return rtcore.ErrNotFound
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

func (f *fakeTasks) List(context.Context) ([]rtcore.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rtcore.TaskStatus
	for _, st := range f.tasks {
		out = append(out, st)
	}
	return out, nil
}

func (f *fakeTasks) Preflight(context.Context) error { return nil }

// set changes the state of a task and tells the watchers.
func (f *fakeTasks) set(st rtcore.TaskStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[st.TaskID] = st
	for _, ch := range f.watchers[st.TaskID] {
		ch <- st
		if st.State == rtcore.TaskExited {
			close(ch)
		}
	}
	if st.State == rtcore.TaskExited {
		delete(f.watchers, st.TaskID)
	}
}

func exited(id string, reason rtcore.SandboxReason) rtcore.TaskStatus {
	code := 0
	if reason != rtcore.SandboxCompleted {
		code = 3
	}
	return rtcore.TaskStatus{TaskID: id, State: rtcore.TaskExited, Reason: reason, ExitCode: &code}
}

func testTask(n int) rtcore.Task {
	run := "run_" + strconv.Itoa(n)
	return rtcore.Task{
		TaskID: rtcore.TaskID(run, 1), RunID: run, Session: 1, TeamID: "team_1", Image: "img",
		Platform: rtcore.TaskPlatform{OS: "linux", Arch: "amd64"},
		Env: map[string]string{
			rtcore.TaskEnvControlURL: "https://control.test", rtcore.TaskEnvRunID: run,
			rtcore.TaskEnvSession: "1", rtcore.TaskEnvBootstrapToken: "tok",
		},
		MaxSessionSeconds: 60,
	}
}

// drainTasks is a fakeTasks that holds the drain state (rtcore.Drainable),
// like the microVM backend.
type drainTasks struct {
	*fakeTasks
	dmu      sync.Mutex
	draining bool
}

func (d *drainTasks) Draining() bool {
	d.dmu.Lock()
	defer d.dmu.Unlock()
	return d.draining
}

func (d *drainTasks) SetDraining(v bool) {
	d.dmu.Lock()
	d.draining = v
	d.dmu.Unlock()
}

type v2Env struct {
	hub     *directtest.Hub
	backend *fakeTasks
	drain   *drainTasks
	cancel  context.CancelFunc
	done    chan error
}

type v2Opts struct {
	capacity int
	oneShot  bool
	taskEnv  map[string]string
	image    string
	metrics  *worker.Metrics
	features []string
	// drainable makes the backend hold the drain state.
	drainable bool
	// wrap replaces the backend with a wrapper of the fake.
	wrap func(*fakeTasks) rtcore.TaskBackend
}

func startV2(t *testing.T, o v2Opts) *v2Env {
	t.Helper()
	hub := directtest.New(t, directtest.Options{APIKey: "good-key", HeartbeatMS: 100})
	e := &v2Env{hub: hub, backend: newFakeTasks(), done: make(chan error, 1)}
	var backend rtcore.TaskBackend = e.backend
	if o.drainable {
		e.drain = &drainTasks{fakeTasks: e.backend}
		backend = e.drain
	}
	if o.wrap != nil {
		backend = o.wrap(e.backend)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	t.Cleanup(cancel)
	go func() {
		e.done <- RunWorkerV2(ctx, WorkerV2Options{
			ServerURL: hub.URL(), APIKey: "good-key", WorkerID: "edge-1", Capacity: o.capacity,
			Backend: backend, OneShot: o.oneShot, TaskEnv: o.taskEnv, DefaultImage: o.image, Metrics: o.metrics,
			Features: o.features, MinBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		})
	}()
	eventually(t, "worker connection", hub.Connected)
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

func hubState(e *v2Env, id string, state rtcore.TaskState) func() bool {
	return func() bool {
		s, ok := e.hub.Status(id)
		return ok && s.State == state
	}
}

func TestV2HelloAndHeartbeat(t *testing.T) {
	e := startV2(t, v2Opts{capacity: 3, features: []string{"git_cache", "snapshot_restore"}})
	h, ok := e.hub.Hello()
	if !ok || h.WorkerID != "edge-1" || h.Backend != "fake" || h.Capacity != 3 || h.ProtocolMinor != ProtocolMinorV2 {
		t.Fatalf("hello = %+v", h)
	}
	if !slices.Equal(h.Features, []string{"git_cache", "snapshot_restore"}) {
		t.Fatalf("features = %v", h.Features)
	}
	if h.Facts == nil || h.Facts.VCPUs == 0 || h.Facts.Arch == "" {
		t.Fatalf("default facts = %+v", h.Facts)
	}
	eventually(t, "heartbeat", func() bool { return e.hub.Heartbeats() > 0 })
}

func TestV2AssignStatusAndExit(t *testing.T) {
	e := startV2(t, v2Opts{taskEnv: map[string]string{"OF_IDLE_TIMEOUT_MAX_SECONDS": "300"}, image: "worker-img"})
	ctx := context.Background()
	task := testTask(1)
	task.Image = "local"
	st, err := e.hub.Assign(ctx, task, nil)
	if err != nil || st.State != rtcore.TaskStarting {
		t.Fatalf("assign = %+v, %v", st, err)
	}
	e.backend.mu.Lock()
	got := e.backend.started[0]
	e.backend.mu.Unlock()
	if got.Env["OF_IDLE_TIMEOUT_MAX_SECONDS"] != "300" || got.Image != "worker-img" || got.Env[rtcore.TaskEnvBootstrapToken] != "tok" {
		t.Fatalf("task on the worker = %+v", got)
	}
	// A second assign of the same task returns its status and starts nothing.
	if st2, err := e.hub.Assign(ctx, task, nil); err != nil || st2.TaskID != task.TaskID {
		t.Fatalf("second assign = %+v, %v", st2, err)
	}
	e.backend.mu.Lock()
	starts := len(e.backend.started)
	e.backend.mu.Unlock()
	if starts != 1 {
		t.Fatalf("backend starts = %d", starts)
	}
	e.backend.set(rtcore.TaskStatus{TaskID: task.TaskID, State: rtcore.TaskRunning})
	eventually(t, "running event", hubState(e, task.TaskID, rtcore.TaskRunning))
	if s, err := e.hub.TaskStatus(ctx, task.TaskID); err != nil || s.State != rtcore.TaskRunning {
		t.Fatalf("task.status = %+v, %v", s, err)
	}
	e.backend.set(exited(task.TaskID, rtcore.SandboxCompleted))
	eventually(t, "exited event", hubState(e, task.TaskID, rtcore.TaskExited))
	s, _ := e.hub.Status(task.TaskID)
	if s.Reason != rtcore.SandboxCompleted || s.ExitCode == nil || *s.ExitCode != 0 {
		t.Fatalf("exit status %+v", s)
	}
	list, err := e.hub.List(ctx)
	if err != nil || len(list) != 1 || list[0].TaskID != task.TaskID || list[0].Status.State != rtcore.TaskExited {
		t.Fatalf("task.list = %+v, %v", list, err)
	}
}

func TestV2StatusAsksBackendForUnknownTask(t *testing.T) {
	e := startV2(t, v2Opts{})
	if _, err := e.hub.TaskStatus(context.Background(), "run_x.1"); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("unknown task: %v", err)
	}
	// A task that the backend holds but the worker never assigned.
	e.backend.set(rtcore.TaskStatus{TaskID: "run_y.1", State: rtcore.TaskRunning})
	st, err := e.hub.TaskStatus(context.Background(), "run_y.1")
	if err != nil || st.State != rtcore.TaskRunning {
		t.Fatalf("status = %+v, %v", st, err)
	}
}

func TestV2CancelAndDestroyReachBackend(t *testing.T) {
	e := startV2(t, v2Opts{})
	ctx := context.Background()
	task := testTask(1)
	if _, err := e.hub.Assign(ctx, task, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.hub.Cancel(ctx, task.TaskID, 120); err != nil {
		t.Fatal(err)
	}
	e.backend.mu.Lock()
	grace, ok := e.backend.cancelled[task.TaskID]
	e.backend.mu.Unlock()
	if !ok || grace != 120 {
		t.Fatalf("cancel grace = %d, %v", grace, ok)
	}
	if err := e.hub.Cancel(ctx, "run_none.1", 0); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("cancel unknown: %v", err)
	}
	if err := e.hub.Destroy(ctx, task.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := e.hub.Destroy(ctx, task.TaskID); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
	e.backend.mu.Lock()
	n := len(e.backend.destroyed)
	e.backend.mu.Unlock()
	if n != 2 {
		t.Fatalf("destroy calls = %d", n)
	}
	if list, _ := e.hub.List(ctx); len(list) != 0 {
		t.Fatalf("list after destroy = %v", list)
	}
}

func TestV2AtCapacity(t *testing.T) {
	m := worker.NewMetrics("edge-1", "fake", "test", 1)
	e := startV2(t, v2Opts{capacity: 1, metrics: m})
	ctx := context.Background()
	if _, err := e.hub.Assign(ctx, testTask(1), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hub.Assign(ctx, testTask(2), nil); !errors.Is(err, rtcore.ErrAtCapacity) {
		t.Fatalf("second task: %v", err)
	}
	if v := m.Value(worker.MetricTasksRejected, "at_capacity"); v != 1 {
		t.Errorf("rejected metric = %v", v)
	}
	// An exited task frees its slot.
	e.backend.set(exited("run_1.1", rtcore.SandboxCompleted))
	eventually(t, "exited", hubState(e, "run_1.1", rtcore.TaskExited))
	if _, err := e.hub.Assign(ctx, testTask(2), nil); err != nil {
		t.Fatalf("after the slot is free: %v", err)
	}
}

func TestV2Drain(t *testing.T) {
	e := startV2(t, v2Opts{})
	ctx := context.Background()
	if err := e.hub.Drain(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hub.Assign(ctx, testTask(1), nil); !errors.Is(err, rtcore.ErrAtCapacity) {
		t.Fatalf("assign while draining: %v", err)
	}
	// A reconnect announces the drain in hello.
	e.hub.Drop()
	eventually(t, "reconnect", func() bool { return e.hub.Connections() == 2 && e.hub.Connected() })
	if h, _ := e.hub.Hello(); !h.Draining {
		t.Fatal("hello after reconnect must say draining")
	}
	if err := e.hub.Drain(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hub.Assign(ctx, testTask(1), nil); err != nil {
		t.Fatalf("assign after resume: %v", err)
	}
}

func TestV2ReconnectRestoresStatuses(t *testing.T) {
	e := startV2(t, v2Opts{})
	ctx := context.Background()
	a, b := testTask(1), testTask(2)
	for _, task := range []rtcore.Task{a, b} {
		if _, err := e.hub.Assign(ctx, task, nil); err != nil {
			t.Fatal(err)
		}
	}
	e.backend.set(rtcore.TaskStatus{TaskID: a.TaskID, State: rtcore.TaskRunning})
	eventually(t, "running", hubState(e, a.TaskID, rtcore.TaskRunning))
	// Drop the connection. The task exits while the worker has no
	// connection, and the other one disappears from the backend.
	e.hub.Drop()
	e.backend.set(exited(a.TaskID, rtcore.SandboxProcessFailed))
	e.backend.mu.Lock()
	delete(e.backend.tasks, b.TaskID)
	e.backend.mu.Unlock()
	eventually(t, "reconnect", func() bool { return e.hub.Connections() == 2 && e.hub.Connected() })
	eventually(t, "restored status", func() bool {
		s, ok := e.hub.Status(a.TaskID)
		return ok && s.State == rtcore.TaskExited && s.Reason == rtcore.SandboxProcessFailed
	})
	if _, err := e.hub.TaskStatus(ctx, b.TaskID); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("lost task: %v", err)
	}
}

func TestV2OneShot(t *testing.T) {
	e := startV2(t, v2Opts{oneShot: true})
	ctx := context.Background()
	if h, _ := e.hub.Hello(); !h.OneShot {
		t.Fatal("hello must say one_shot")
	}
	task := testTask(1)
	if _, err := e.hub.Assign(ctx, task, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.hub.Assign(ctx, testTask(2), nil); !errors.Is(err, rtcore.ErrAtCapacity) {
		t.Fatalf("second task: %v", err)
	}
	e.backend.set(exited(task.TaskID, rtcore.SandboxCompleted))
	select {
	case err := <-e.done:
		if err != nil {
			t.Fatalf("worker result after completed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("one-shot worker did not exit")
	}
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
	if _, err := e.hub.Assign(context.Background(), task, nil); err != nil {
		t.Fatal(err)
	}
	e.backend.set(exited(task.TaskID, rtcore.SandboxProcessFailed))
	select {
	case err := <-e.done:
		var oe *OneShotError
		if !errors.As(err, &oe) || oe.Status.Reason != rtcore.SandboxProcessFailed {
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
		st   rtcore.TaskStatus
		want int
	}{
		{exited("t.1", rtcore.SandboxCompleted), 0},
		{exited("t.1", rtcore.SandboxProcessFailed), 1},
		{exited("t.1", rtcore.SandboxCancelled), 1},
		{exited("t.1", rtcore.SandboxContainerOOM), 1},
		{exited("t.1", rtcore.SandboxUnknown), 1},
		{rtcore.TaskStatus{TaskID: "t.1", State: rtcore.TaskRunning}, 1},
	} {
		if got := OneShotExitCode(tc.st); got != tc.want {
			t.Errorf("%s/%s: exit code %d, want %d", tc.st.State, tc.st.Reason, got, tc.want)
		}
	}
}

// A hello that the hub refuses (close code 1008) ends the worker with
// ErrRefused; it does not reconnect.
func TestV2RefusedHello(t *testing.T) {
	hub := directtest.New(t, directtest.Options{Refuse: func(h directtest.Hello) string {
		if h.WorkerID != "allowed" {
			return "worker key is not valid for this worker id"
		}
		return ""
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunWorkerV2(ctx, WorkerV2Options{ServerURL: hub.URL(), APIKey: "k", WorkerID: "other", Backend: newFakeTasks()})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("other id: %v", err)
	}
	if hub.Connected() {
		t.Fatal("refused worker is connected")
	}
}

func TestV2BadKeyIsUnauthorized(t *testing.T) {
	e := startV2(t, v2Opts{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := RunWorkerV2(ctx, WorkerV2Options{ServerURL: e.hub.URL(), APIKey: "bad", WorkerID: "edge-2", Backend: newFakeTasks()})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bad key: %v", err)
	}
}

func TestV2InvalidSpecIsRefused(t *testing.T) {
	e := startV2(t, v2Opts{})
	ctx := context.Background()
	bad := testTask(1)
	bad.Image = ""
	if _, err := e.hub.Assign(ctx, bad, nil); !errors.Is(err, rtcore.ErrInvalid) {
		t.Fatalf("invalid task: %v", err)
	}
	e.backend.mu.Lock()
	e.backend.startErr = rtcore.ErrAtCapacity
	e.backend.mu.Unlock()
	if _, err := e.hub.Assign(ctx, testTask(2), nil); !errors.Is(err, rtcore.ErrAtCapacity) {
		t.Fatalf("backend at capacity: %v", err)
	}
	if list, _ := e.hub.List(ctx); len(list) != 0 {
		t.Fatalf("a failed start must free its slot: %v", list)
	}
}

func TestV2UnknownMethod(t *testing.T) {
	e := startV2(t, v2Opts{})
	err := e.hub.Call(context.Background(), "task.exec", struct{}{}, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown_method") {
		t.Fatalf("unknown method: %v", err)
	}
}

// The fake hub does not import this package, so it repeats two constants.
func TestDirecttestConstants(t *testing.T) {
	if directtest.ConnectPath != ConnectPath || directtest.Subprotocol != SubprotocolV2 {
		t.Fatalf("directtest uses %s %s", directtest.ConnectPath, directtest.Subprotocol)
	}
}

// TestWorkerIDValidation checks the rules for the ID of a worker.
func TestWorkerIDValidation(t *testing.T) {
	for id, ok := range map[string]bool{"prod-runner-1": true, "ci.worker_2": true, "": false, "-x": false, "local": false, "managed": false, "server": false, "daytona": false, "cloudflare-sandbox": false, "boatdev": false, "openfactory-x": false, strings.Repeat("a", 65): false, "a b": false} {
		if err := ValidateWorkerID(id); (err == nil) != ok {
			t.Errorf("ValidateWorkerID(%q) = %v", id, err)
		}
	}
}

// TestV2TaskFailureMetricHasPhaseRun checks that a sandbox that ends after it
// started (eviction) counts in of_worker_sandbox_failures_total with phase "run".
func TestV2TaskFailureMetricHasPhaseRun(t *testing.T) {
	m := worker.NewMetrics("edge-1", "fake", "test", 0)
	e := startV2(t, v2Opts{metrics: m})
	for i, reason := range []rtcore.SandboxReason{rtcore.SandboxEvicted, rtcore.SandboxImagePull, rtcore.SandboxCompleted} {
		task := testTask(i + 1)
		if _, err := e.hub.Assign(context.Background(), task, nil); err != nil {
			t.Fatal(err)
		}
		e.backend.set(rtcore.TaskStatus{TaskID: task.TaskID, State: rtcore.TaskRunning})
		e.backend.set(exited(task.TaskID, reason))
	}
	eventually(t, "failure metrics", func() bool {
		return m.Value(worker.MetricTaskFailures, "run", "evicted") == 1 && m.Value(worker.MetricTaskFailures, "setup", "image_pull") == 1
	})
	eventually(t, "three tasks done", func() bool {
		return m.Value(worker.MetricTasksDone, worker.ResultSucceeded) == 1 && m.Value(worker.MetricTasksDone, worker.ResultFailed) == 2
	})
	if v := m.Value(worker.MetricTasksDone, "completed"); v != 0 {
		t.Errorf("the end reason is a result label: %v", v)
	}
	if v := m.Value(worker.MetricTaskFailures, "run", "completed"); v != 0 {
		t.Errorf("a completed task counts as a failure: %v", v)
	}
}

// With a rtcore.Drainable backend, the hub and the operator change one
// drain state.
func TestV2DrainSharedWithTheBackend(t *testing.T) {
	e := startV2(t, v2Opts{drainable: true})
	ctx := context.Background()
	// The hub drains: the backend refuses new tasks too.
	if err := e.hub.Drain(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !e.drain.Draining() {
		t.Fatal("the hub drain did not reach the backend")
	}
	if err := e.hub.Drain(ctx, false); err != nil {
		t.Fatal(err)
	}
	// The operator drains on the host (worker drain): the worker refuses
	// task.assign and the next hello says draining.
	e.drain.SetDraining(true)
	if _, err := e.hub.Assign(ctx, testTask(1), nil); !errors.Is(err, rtcore.ErrAtCapacity) {
		t.Fatalf("assign while the operator drains: %v", err)
	}
	e.hub.Drop()
	eventually(t, "reconnect", func() bool { return e.hub.Connections() == 2 && e.hub.Connected() })
	if h, _ := e.hub.Hello(); !h.Draining {
		t.Fatal("hello must say draining after an operator drain")
	}
}

// slowDestroy holds Destroy until the test releases it and records whether
// its context ended.
type slowDestroy struct {
	*fakeTasks
	started chan struct{}
	release chan struct{}
	ctxErr  chan error
}

func (s *slowDestroy) Destroy(ctx context.Context, id string) error {
	close(s.started)
	<-s.release
	s.ctxErr <- ctx.Err()
	return s.fakeTasks.Destroy(ctx, id)
}

// A dropped connection does not cancel a request that the worker serves.
func TestV2RequestOutlivesTheConnection(t *testing.T) {
	sd := &slowDestroy{started: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1)}
	e := startV2(t, v2Opts{wrap: func(f *fakeTasks) rtcore.TaskBackend { sd.fakeTasks = f; return sd }})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = e.hub.Destroy(ctx, "run_1.1")
	}()
	<-sd.started
	e.hub.Drop()
	eventually(t, "reconnect", func() bool { return e.hub.Connections() == 2 && e.hub.Connected() })
	close(sd.release)
	if err := <-sd.ctxErr; err != nil {
		t.Fatalf("the destroy context ended with the connection: %v", err)
	}
}

// The worker pings on each heartbeat and drops a connection that brings no
// frame for readTimeoutBeats heartbeats.
func TestV2HalfOpenConnection(t *testing.T) {
	e := startV2(t, v2Opts{}) // heartbeat 100 ms: read deadline 300 ms
	// An idle connection whose hub answers the pings stays.
	time.Sleep(600 * time.Millisecond)
	if n := e.hub.Connections(); n != 1 {
		t.Fatalf("an idle healthy connection was dropped: %d connections", n)
	}
	// A hub that stops answering looks half-open: the worker reconnects.
	e.hub.Mute(true)
	eventually(t, "reconnect after a silent hub", func() bool { return e.hub.Connections() >= 2 })
	e.hub.Mute(false)
	eventually(t, "connected", e.hub.Connected)
}
