package direct

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/internal/ws"
	"github.com/openfactory-org/openfactory/internal/worker"
)

// WorkerV2Options configures RunWorkerV2.
type WorkerV2Options struct {
	// ServerURL is the control plane base URL (http(s)://host[:port]).
	ServerURL string
	// APIKey authenticates the worker (a worker key).
	APIKey string
	// WorkerID is the routing ID (the value agents use as workerHost).
	WorkerID string
	Name     string
	Labels   map[string]string
	// Capacity limits tasks that are not exited (0 means no limit).
	Capacity int
	// Backend runs the tasks: tasklocal (dev worker) or the microVM backend.
	Backend rtcore.TaskBackend
	Version string
	// OneShot makes the worker take exactly one task and return when it exited.
	OneShot bool
	// TaskEnv holds variables that the worker adds to the environment of each
	// task, for example OF_IDLE_TIMEOUT_MAX_SECONDS. A variable that the task
	// already has is not replaced.
	TaskEnv map[string]string
	// DefaultImage replaces an empty task image and the image "local" (the
	// default image of a control plane that has no image of its own).
	DefaultImage string
	// Metrics, when set, is updated with worker metrics.
	Metrics *worker.Metrics
	// HTTPClient is used for the WebSocket dial (TLS settings, proxies).
	HTTPClient *http.Client
	// MinBackoff and MaxBackoff bound reconnect delays (defaults 1s and 60s).
	MinBackoff time.Duration
	MaxBackoff time.Duration
	// HeartbeatInterval overrides the interval that the server announces.
	HeartbeatInterval time.Duration
	Logger            *slog.Logger
	// KeyFile is the Ed25519 private key file of an enrolled worker (see
	// LoadWorkerKey). With a key, the worker waits for the auth.challenge
	// of the hub and answers it before the hello.
	KeyFile string
	// Key is the private key itself; it wins over KeyFile.
	Key ed25519.PrivateKey
	// Facts describes the host. Nil means the facts that the Go runtime
	// knows: arch, OS, CPUs and the version.
	Facts *WorkerFacts
	// OnWelcome, when set, gets the welcome of each connection, for example
	// to read its access token.
	OnWelcome func(Welcome)
	// Slots and Slot describe the slots of a microVM worker.
	Slots int
	Slot  *SlotSize
	// Features lists the optional features that the worker announces in
	// hello, for example "git_cache" and "snapshot_restore".
	Features []string
}

var (
	// ErrUnauthorized means the server rejected the worker API key.
	ErrUnauthorized = errors.New("worker API key rejected by server")
	// ErrRefused is returned by RunWorkerV2 when the server refuses the hello,
	// for example because the worker key is not valid for the worker ID.
	ErrRefused = errors.New("worker refused by server")
	// ErrProtocolUnsupported is returned by RunWorkerV2 when the server does
	// not speak worker protocol version 2.
	ErrProtocolUnsupported = errors.New("server does not speak worker protocol version 2")
	// ErrKeyRequired is returned by RunWorkerV2 when the hub sends a
	// challenge and the worker has no key.
	ErrKeyRequired = errors.New("the control plane asks for the key of an enrolled worker; set the key file")

	// errUnknownMethod is the error of a request with an unknown method.
	errUnknownMethod = errors.New("unknown method")
)

// readTimeoutBeats is the number of heartbeat intervals without a frame from
// the hub after which the worker drops the connection.
const readTimeoutBeats = 3

// oneShotFlushWait is the longest time that a one-shot worker waits to report
// the end of its task when it has no connection.
const oneShotFlushWait = 30 * time.Second

type workerV2 struct {
	opts   WorkerV2Options
	log    *slog.Logger
	cancel context.CancelFunc // ends RunWorkerV2
	ctx    context.Context    // lives as long as RunWorkerV2, not as long as one connection

	mu       sync.Mutex
	status   map[string]rtcore.TaskStatus
	watching map[string]bool
	cur      *v2Client
	accepted int    // one-shot: tasks accepted
	oneTask  string // one-shot: the task
	result   *rtcore.TaskStatus
	draining bool // the hub asked the worker to take no new task; unused with a rtcore.Drainable backend
}

// isDraining reports whether the worker takes no new task. A backend that
// implements rtcore.Drainable holds the state.
func (w *workerV2) isDraining() bool {
	if d, ok := w.opts.Backend.(rtcore.Drainable); ok {
		return d.Draining()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.draining
}

func (w *workerV2) setDraining(draining bool) {
	if d, ok := w.opts.Backend.(rtcore.Drainable); ok {
		d.SetDraining(draining)
		return
	}
	w.mu.Lock()
	w.draining = draining
	w.mu.Unlock()
}

// v2Client is one live connection of the worker.
type v2Client struct {
	ws *ws.Conn
}

func (c *v2Client) send(m *message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.ws.WriteMessage(ws.OpText, data)
}

// RunWorkerV2 connects to the server with worker protocol version 2 and
// serves task requests with opts.Backend until ctx ends. It reconnects with
// exponential backoff. Tasks keep running while the worker has no connection.
//
// It returns ctx.Err() when ctx ends, ErrUnauthorized when the server rejects
// the key, ErrRefused or ErrProtocolUnsupported. In one-shot mode it returns
// nil when the task ended with the reason "completed" and a *OneShotError for
// any other end.
func RunWorkerV2(parent context.Context, opts WorkerV2Options) error {
	if opts.ServerURL == "" {
		return fmt.Errorf("%w: server URL required", rtcore.ErrInvalid)
	}
	if opts.Backend == nil {
		return fmt.Errorf("%w: worker backend required", rtcore.ErrInvalid)
	}
	if err := ValidateWorkerID(opts.WorkerID); err != nil {
		return err
	}
	if opts.Capacity < 0 {
		opts.Capacity = 0
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = time.Minute
	}
	if opts.Key == nil && opts.KeyFile != "" {
		k, err := LoadWorkerKey(opts.KeyFile)
		if err != nil {
			return err
		}
		opts.Key = k
	}
	if opts.APIKey == "" && opts.Key == nil {
		return fmt.Errorf("%w: worker API key or worker key required", rtcore.ErrInvalid)
	}
	if opts.Facts == nil {
		opts.Facts = &WorkerFacts{Arch: runtime.GOARCH, OS: runtime.GOOS, VCPUs: runtime.NumCPU(), RuntimeVersion: opts.Version}
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	w := &workerV2{
		opts: opts, ctx: ctx, cancel: cancel,
		log:      log.With("worker", opts.WorkerID, "backend", opts.Backend.Name(), "protocol", "v2"),
		status:   map[string]rtcore.TaskStatus{},
		watching: map[string]bool{},
	}
	w.adopt()

	backoff := opts.MinBackoff
	for {
		connected, err := w.session(ctx)
		if res := w.finished(); res != nil {
			if OneShotExitCode(*res) == 0 {
				return nil
			}
			return &OneShotError{Status: *res}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrRefused) || errors.Is(err, ErrProtocolUnsupported) || errors.Is(err, ErrKeyRequired) {
			return err
		}
		reason := "remote_close"
		if !connected {
			reason = "dial_failed"
		} else {
			backoff = opts.MinBackoff
		}
		if opts.Metrics != nil {
			opts.Metrics.Reconnect(reason)
		}
		delay := backoff/2 + rand.N(backoff/2+1) //nolint:gosec // reconnect jitter, not a secret
		w.log.Warn("worker connection lost; reconnecting", "err", err, "retry_in", delay.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		backoff = min(backoff*2, opts.MaxBackoff)
	}
}

func (w *workerV2) finished() *rtcore.TaskStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.result
}

// adopt takes the tasks that the backend already holds, for example after a
// restart of the worker process, and watches them.
func (w *workerV2) adopt() {
	list, err := w.opts.Backend.List(w.ctx)
	if err != nil {
		w.log.Warn("list tasks of the backend", "err", err)
		return
	}
	for _, st := range list {
		w.setStatus(st)
		w.watch(st.TaskID)
	}
}

func (w *workerV2) setStatus(st rtcore.TaskStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if old, ok := w.status[st.TaskID]; ok && (old.State == rtcore.TaskExited || stateRank(st.State) < stateRank(old.State)) {
		return
	}
	w.status[st.TaskID] = st
}

func (w *workerV2) activeLocked() int {
	n := 0
	for _, st := range w.status {
		if st.State != rtcore.TaskExited {
			n++
		}
	}
	return n
}

func (w *workerV2) active() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activeLocked()
}

// snapshot returns the tasks for a hello. The backend is the source of truth.
func (w *workerV2) snapshot(ctx context.Context) []HelloTask {
	if list, err := w.opts.Backend.List(ctx); err == nil {
		held := map[string]bool{}
		for _, st := range list {
			held[st.TaskID] = true
			w.setStatus(st)
		}
		// A task that the backend no longer lists is gone. A slot that Start
		// reserved and the backend has not stored yet stays.
		w.mu.Lock()
		for id, st := range w.status {
			if !held[id] && st.State != rtcore.TaskAccepted {
				delete(w.status, id)
			}
		}
		w.mu.Unlock()
	} else {
		w.log.Warn("list tasks of the backend", "err", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]HelloTask, 0, len(w.status))
	for id, st := range w.status {
		out = append(out, HelloTask{TaskID: id, Status: st})
	}
	return out
}

// emit sends task.state on the current connection. It reports whether a connection took it.
func (w *workerV2) emit(st rtcore.TaskStatus) bool {
	w.mu.Lock()
	c := w.cur
	w.mu.Unlock()
	if c == nil {
		return false
	}
	p, _ := json.Marshal(statusBody{Status: st})
	return c.send(&message{Type: typeEvent, Method: eventTaskState, Params: p}) == nil
}

// watch follows one task of the backend and reports each change to the hub.
func (w *workerV2) watch(taskID string) {
	w.mu.Lock()
	if w.watching[taskID] {
		w.mu.Unlock()
		return
	}
	w.watching[taskID] = true
	w.mu.Unlock()
	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.watching, taskID)
			w.mu.Unlock()
		}()
		ch, err := w.opts.Backend.Watch(w.ctx, taskID)
		if err != nil {
			if errors.Is(err, rtcore.ErrNotFound) {
				w.mu.Lock()
				delete(w.status, taskID)
				w.mu.Unlock()
			}
			w.log.Warn("watch task", "task", taskID, "err", err)
			return
		}
		var started time.Time
		for st := range ch {
			if st.TaskID == "" {
				st.TaskID = taskID
			}
			if started.IsZero() {
				started = time.Now()
			}
			w.setStatus(st)
			w.emit(st)
			if st.State != rtcore.TaskExited {
				continue
			}
			if m := w.opts.Metrics; m != nil {
				m.TaskCompleted(taskResult(st.Reason), time.Since(started), w.active())
				if phase, ok := failurePhase(st.Reason); ok {
					m.TaskFailed(phase, string(st.Reason))
				}
			}
			w.mu.Lock()
			one := w.opts.OneShot && w.oneTask == taskID
			w.mu.Unlock()
			if one {
				w.finishOneShot(st)
			}
			return
		}
	}()
}

// finishOneShot reports the end of the task of a one-shot worker, removes the
// sandbox of a task that needs no retention and ends the worker.
func (w *workerV2) finishOneShot(st rtcore.TaskStatus) {
	deadline := time.Now().Add(oneShotFlushWait)
	for !w.emit(st) && time.Now().Before(deadline) {
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	if st.Reason == rtcore.SandboxCompleted || st.Reason == rtcore.SandboxCancelled {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := w.opts.Backend.Destroy(dctx, st.TaskID); err != nil {
			w.log.Warn("destroy the one-shot task", "task", st.TaskID, "err", err)
		}
		cancel()
	}
	w.mu.Lock()
	w.result = &st
	w.mu.Unlock()
	w.cancel()
}

// session runs one connection. connected reports whether the handshake succeeded.
func (w *workerV2) session(ctx context.Context) (connected bool, err error) {
	u := strings.TrimRight(w.opts.ServerURL, "/") + ConnectPath
	h := http.Header{}
	if w.opts.APIKey != "" {
		h.Set("Authorization", "Bearer "+w.opts.APIKey)
	}
	// The control plane routes the upgrade by worker ID before any frame.
	h.Set(HeaderWorkerID, w.opts.WorkerID)
	h.Set("User-Agent", "openfactory-worker/"+w.opts.Version)
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	wsc, _, err := ws.Dial(dialCtx, w.opts.HTTPClient, u, h, []string{SubprotocolV2}) //nolint:bodyclose // Dial closes the body on failure; on success the body is the connection, closed by wsc.Close
	cancelDial()
	if err != nil {
		var he *ws.HandshakeError
		if errors.As(err, &he) && (he.StatusCode == http.StatusUnauthorized || he.StatusCode == http.StatusForbidden) {
			return false, fmt.Errorf("%w: %w", ErrUnauthorized, err)
		}
		return false, err
	}
	defer wsc.Close()
	if wsc.Subprotocol != SubprotocolV2 {
		return false, ErrProtocolUnsupported
	}
	type readRes struct {
		data []byte
		err  error
	}
	frames := make(chan readRes, 1)
	readNext := func() {
		go func() {
			_, d, err := wsc.ReadMessage()
			frames <- readRes{d, err}
		}()
	}
	// wait reads the next frame of the handshake.
	wait := func(what string) (message, error) {
		select {
		case r := <-frames:
			if r.err != nil {
				var ce *ws.CloseError
				if errors.As(r.err, &ce) && ce.Code == closePolicy {
					return message{}, fmt.Errorf("%w: %s", ErrRefused, ce.Reason)
				}
				return message{}, fmt.Errorf("waiting for %s: %w", what, r.err)
			}
			var m message
			if json.Unmarshal(r.data, &m) != nil {
				return message{}, fmt.Errorf("invalid %s from server", what)
			}
			return m, nil
		case <-time.After(15 * time.Second):
			return message{}, fmt.Errorf("timed out waiting for %s", what)
		case <-ctx.Done():
			return message{}, ctx.Err()
		}
	}
	if w.opts.Key != nil {
		// An enrolled worker: the hub speaks first.
		readNext()
		m, err := wait("challenge")
		if err != nil {
			return false, err
		}
		var ch AuthChallenge
		if m.Type != typeAuthChallenge || json.Unmarshal(m.Params, &ch) != nil {
			return false, fmt.Errorf("%w: expected %s, got %q", ErrRefused, typeAuthChallenge, m.Type)
		}
		resp, err := SignChallenge(w.opts.Key, w.opts.WorkerID, ch)
		if err != nil {
			return false, fmt.Errorf("%w: %w", ErrRefused, err)
		}
		rp, _ := json.Marshal(resp)
		ad, _ := json.Marshal(&message{Type: typeAuthResponse, Params: rp})
		if err := wsc.WriteMessage(ws.OpText, ad); err != nil {
			return false, err
		}
	}
	hello := w.hello(ctx)
	hp, _ := json.Marshal(hello)
	data, _ := json.Marshal(&message{Type: typeHello, Params: hp})
	if err := wsc.WriteMessage(ws.OpText, data); err != nil {
		return false, err
	}
	readNext()
	var welcome Welcome
	m, err := wait("welcome")
	if err != nil {
		return false, err
	}
	if m.Type == typeAuthChallenge {
		return false, ErrKeyRequired
	}
	if m.Type != typeWelcome || json.Unmarshal(m.Result, &welcome) != nil {
		return false, errors.New("invalid welcome from server")
	}
	w.log.Info("connected to server", "connection", welcome.ConnectionID, "tasks", len(hello.Tasks))
	if w.opts.OnWelcome != nil {
		w.opts.OnWelcome(welcome)
	}
	if w.opts.Metrics != nil {
		w.opts.Metrics.SetConnected(true)
		defer w.opts.Metrics.SetConnected(false)
	}

	cctx, stop := context.WithCancel(ctx)
	defer stop()
	client := &v2Client{ws: wsc}
	w.mu.Lock()
	w.cur = client
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if w.cur == client {
			w.cur = nil
		}
		w.mu.Unlock()
	}()

	interval := w.opts.HeartbeatInterval
	if interval <= 0 {
		interval = time.Duration(welcome.HeartbeatMS) * time.Millisecond
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	go w.heartbeats(cctx, client, interval)
	go func() {
		<-cctx.Done()
		_ = wsc.Close()
	}()
	// The hub answers each ping with a pong, so a connection that brings no
	// frame for readTimeoutBeats heartbeats is half-open: drop it and reconnect.
	defer func() { _ = wsc.SetReadDeadline(time.Time{}) }()

	for {
		_ = wsc.SetReadDeadline(time.Now().Add(readTimeoutBeats * interval))
		_, data, err := wsc.ReadMessage()
		if err != nil {
			var ce *ws.CloseError
			if errors.As(err, &ce) && ce.Code == closePolicy {
				return true, fmt.Errorf("%w: %s", ErrRefused, ce.Reason)
			}
			return true, err
		}
		var m message
		if json.Unmarshal(data, &m) != nil || m.Type != typeRequest {
			continue
		}
		go func(m message) {
			// The worker context, not the connection context: a dropped
			// connection must not stop a Start or a Destroy halfway.
			result, err := w.handle(w.ctx, &m)
			resp := &message{Type: typeResponse, ID: m.ID}
			if err != nil {
				resp.Error = errorForV2(err)
			} else if result != nil {
				resp.Result, _ = json.Marshal(result)
			}
			_ = client.send(resp)
		}(m)
	}
}

func (w *workerV2) heartbeats(ctx context.Context, c *v2Client, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// The hub answers this exact text without waking up. The pong
			// keeps the read deadline of session from expiring.
			if err := c.ws.WriteMessage(ws.OpText, []byte(pingFrame)); err != nil {
				return
			}
			hb, _ := json.Marshal(Heartbeat{Active: w.active(), Capacity: w.opts.Capacity})
			if err := c.send(&message{Type: typeHeartbeat, Params: hb}); err != nil {
				return
			}
		}
	}
}

// hello returns the hello of a new connection.
func (w *workerV2) hello(ctx context.Context) HelloV2 {
	tasks := w.snapshot(ctx)
	draining := w.isDraining()
	return HelloV2{
		WorkerID: w.opts.WorkerID, Name: w.opts.Name, Backend: w.opts.Backend.Name(), Labels: w.opts.Labels,
		Capacity: w.opts.Capacity, Version: w.opts.Version, OneShot: w.opts.OneShot, Tasks: tasks,
		ProtocolMinor: ProtocolMinorV2, Features: w.opts.Features, Facts: w.opts.Facts, Slots: w.opts.Slots, Slot: w.opts.Slot,
		Draining: draining,
	}
}

// handle serves one request of the hub.
func (w *workerV2) handle(ctx context.Context, m *message) (any, error) {
	switch m.Method {
	case methodTaskAssign:
		var p assignParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, fmt.Errorf("%w: bad task.assign params: %w", rtcore.ErrInvalid, err)
		}
		st, err := w.assign(ctx, p.Task, p.Worker)
		if err != nil {
			return nil, err
		}
		return statusBody{Status: st}, nil
	case methodTaskStatus:
		var p taskRef
		if err := json.Unmarshal(m.Params, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("%w: bad task.status params", rtcore.ErrInvalid)
		}
		st, err := w.opts.Backend.Status(ctx, p.TaskID)
		if err != nil {
			return nil, err
		}
		st.TaskID = p.TaskID
		return statusBody{Status: st}, nil
	case methodTaskCancel:
		var p cancelParams
		if err := json.Unmarshal(m.Params, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("%w: bad task.cancel params", rtcore.ErrInvalid)
		}
		return nil, w.opts.Backend.Cancel(ctx, p.TaskID, p.GraceSeconds)
	case methodTaskDestroy:
		var p taskRef
		if err := json.Unmarshal(m.Params, &p); err != nil || p.TaskID == "" {
			return nil, fmt.Errorf("%w: bad task.destroy params", rtcore.ErrInvalid)
		}
		if err := w.opts.Backend.Destroy(ctx, p.TaskID); err != nil {
			return nil, err
		}
		w.mu.Lock()
		delete(w.status, p.TaskID)
		w.mu.Unlock()
		return nil, nil
	case methodTaskList:
		return TaskListResult{Tasks: w.snapshot(ctx)}, nil
	case methodDrain:
		var p DrainParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, fmt.Errorf("%w: bad drain params", rtcore.ErrInvalid)
		}
		w.setDraining(p.Draining)
		w.log.Info("drain changed", "draining", p.Draining)
		return struct{}{}, nil
	}
	return nil, fmt.Errorf("%w %q", errUnknownMethod, m.Method)
}

// assign starts a task on the backend. It reserves its slot first.
func (w *workerV2) assign(ctx context.Context, task rtcore.Task, extras json.RawMessage) (rtcore.TaskStatus, error) {
	if err := task.Validate(); err != nil {
		return rtcore.TaskStatus{}, fmt.Errorf("%w: %w", rtcore.ErrInvalid, err)
	}
	draining := w.isDraining()
	w.mu.Lock()
	if st, ok := w.status[task.TaskID]; ok {
		w.mu.Unlock()
		return st, nil
	}
	if draining {
		w.mu.Unlock()
		w.rejected("draining")
		return rtcore.TaskStatus{}, fmt.Errorf("worker is draining: %w", rtcore.ErrAtCapacity)
	}
	if w.opts.OneShot && w.accepted >= 1 {
		w.mu.Unlock()
		w.rejected("one_shot")
		return rtcore.TaskStatus{}, fmt.Errorf("one-shot worker has its task: %w", rtcore.ErrAtCapacity)
	}
	if w.opts.Capacity > 0 && w.activeLocked() >= w.opts.Capacity {
		w.mu.Unlock()
		w.rejected("at_capacity")
		return rtcore.TaskStatus{}, fmt.Errorf("worker has %d tasks: %w", w.opts.Capacity, rtcore.ErrAtCapacity)
	}
	w.status[task.TaskID] = rtcore.TaskStatus{TaskID: task.TaskID, State: rtcore.TaskAccepted}
	w.accepted++
	if w.opts.OneShot {
		w.oneTask = task.TaskID
	}
	w.mu.Unlock()

	task.Env = maps.Clone(task.Env)
	for k, v := range w.opts.TaskEnv {
		if _, ok := task.Env[k]; !ok {
			task.Env[k] = v
		}
	}
	if w.opts.DefaultImage != "" && (task.Image == "" || task.Image == "local") {
		task.Image = w.opts.DefaultImage
	}
	var st rtcore.TaskStatus
	var err error
	if eb, ok := w.opts.Backend.(ExtrasBackend); ok {
		st, err = eb.StartWithExtras(ctx, task, extras)
	} else {
		st, err = w.opts.Backend.Start(ctx, task)
	}
	if err != nil {
		w.mu.Lock()
		delete(w.status, task.TaskID)
		w.accepted--
		if w.oneTask == task.TaskID {
			w.oneTask = ""
		}
		w.mu.Unlock()
		return rtcore.TaskStatus{}, err
	}
	st.TaskID = task.TaskID
	w.setStatus(st)
	if m := w.opts.Metrics; m != nil {
		m.TaskClaimed(w.active())
	}
	w.watch(task.TaskID)
	return st, nil
}

func (w *workerV2) rejected(reason string) {
	if w.opts.Metrics != nil {
		w.opts.Metrics.TaskRejected(reason)
	}
}

// taskResult maps a sandbox end reason to the result label of the completed
// and duration metrics: succeeded, failed or cancelled.
func taskResult(reason rtcore.SandboxReason) string {
	switch reason {
	case rtcore.SandboxCompleted:
		return worker.ResultSucceeded
	case rtcore.SandboxCancelled:
		return worker.ResultCancelled
	default:
		return worker.ResultFailed
	}
}

// failurePhase tells in which phase of the task a sandbox end reason is a
// failure, for of_worker_sandbox_failures_total. A task that ended with
// completed or cancelled has no failure. The reasons image_pull,
// unschedulable, network_policy_timeout and invalid_spec mean that the sandbox never started: phase
// "setup". Each other reason means that the sandbox started and then ended
// (an exit code, a memory limit, an eviction, a lost node, a deadline): phase
// "run".
func failurePhase(reason rtcore.SandboxReason) (string, bool) {
	switch reason {
	case rtcore.SandboxCompleted, rtcore.SandboxCancelled, "":
		return "", false
	case rtcore.SandboxImagePull, rtcore.SandboxUnschedulable, rtcore.SandboxNetworkPolicyTimeout, rtcore.SandboxInvalidSpec:
		return "setup", true
	default:
		return "run", true
	}
}
