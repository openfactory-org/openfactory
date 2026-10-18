package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/internal/ws"
)

// closePolicy is the WebSocket close code for a refused hello.
const closePolicy = 1008

// callTimeout bounds a request to a worker when the caller sets no deadline.
const callTimeout = 60 * time.Second

// watchBuffer is the size of the channel of a Watch.
const watchBuffer = 16

// v2State holds the backends of the workers that speak version 2. The zero
// value is ready for use.
type v2State struct {
	mu       sync.Mutex
	backends map[string]*HubBackend
}

// HubBackend is the core.TaskBackend of one worker ID. It lives as long as
// the hub, so a task of a worker that is not connected keeps its last known
// status.
type HubBackend struct {
	h        *Hub
	workerID string

	mu        sync.Mutex
	conn      *v2Conn
	teamID    string
	principal string // of the last attached connection
	tasks     map[string]*taskEntry
}

var _ core.TaskBackend = (*HubBackend)(nil)

type taskEntry struct {
	status   core.TaskStatus
	reserved bool // Start sent task.assign and has no answer yet
	watchers map[*taskWatcher]struct{}
}

type taskWatcher struct {
	ch  chan core.TaskStatus
	fin chan struct{}
}

// backendFor returns the backend of workerID. It makes the backend when create is true.
func (h *Hub) v2Backend(workerID string, create bool) *HubBackend {
	h.v2.mu.Lock()
	defer h.v2.mu.Unlock()
	b := h.v2.backends[workerID]
	if b == nil && create {
		if h.v2.backends == nil {
			h.v2.backends = map[string]*HubBackend{}
		}
		b = &HubBackend{h: h, workerID: workerID, tasks: map[string]*taskEntry{}}
		h.v2.backends[workerID] = b
	}
	return b
}

// WorkerBackend returns the task backend of one worker ID. It is the same
// object for the life of the hub.
func (h *Hub) WorkerBackend(workerID string) *HubBackend { return h.v2Backend(workerID, true) }

// BackendFor implements the router of the dispatcher. It returns the backend
// of the worker that the run names, when that worker speaks version 2, is
// connected, belongs to the team of the run and has a free slot. A false
// result means that the run waits in the queue.
func (h *Hub) BackendFor(run *core.Run) (core.TaskBackend, bool) {
	host := run.Config.WorkerHost
	if host == "" {
		return nil, false
	}
	b := h.v2Backend(host, false)
	if b == nil || !b.available(run.TeamID) {
		return nil, false
	}
	return b, true
}

// Backends returns the backends of all workers that spoke version 2 since the
// hub started, sorted by name.
func (h *Hub) Backends() []core.TaskBackend {
	h.v2.mu.Lock()
	list := make([]*HubBackend, 0, len(h.v2.backends))
	for _, b := range h.v2.backends {
		list = append(list, b)
	}
	h.v2.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].workerID < list[j].workerID })
	out := make([]core.TaskBackend, len(list))
	for i, b := range list {
		out[i] = b
	}
	return out
}

// v2Connected reports whether a worker with this ID is connected with version 2.
func (h *Hub) v2Connected(workerID string) bool {
	b := h.v2Backend(workerID, false)
	return b != nil && b.liveConn() != nil
}

// v2Workers lists the live connections of version 2.
func (h *Hub) v2Workers() []ConnectedWorker {
	h.v2.mu.Lock()
	list := make([]*HubBackend, 0, len(h.v2.backends))
	for _, b := range h.v2.backends {
		list = append(list, b)
	}
	h.v2.mu.Unlock()
	var out []ConnectedWorker
	for _, b := range list {
		if c := b.liveConn(); c != nil {
			out = append(out, c.info())
		}
	}
	return out
}

// Name returns "worker:<id>".
func (b *HubBackend) Name() string { return WorkerBackendName(b.workerID) }

// WorkerID returns the worker ID of the backend.
func (b *HubBackend) WorkerID() string { return b.workerID }

func (b *HubBackend) liveConn() *v2Conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil && !b.conn.isClosed() {
		return b.conn
	}
	return nil
}

func (b *HubBackend) notConnected() error {
	return fmt.Errorf("worker %q is not connected: %w", b.workerID, core.ErrUnavailable)
}

// activeLocked counts the tasks that are not exited. The caller holds b.mu.
func (b *HubBackend) activeLocked() int {
	n := 0
	for _, e := range b.tasks {
		if e.status.State != core.TaskExited {
			n++
		}
	}
	return n
}

// freeLocked reports whether the connection can take one more task. The caller holds b.mu.
func (b *HubBackend) freeLocked(c *v2Conn) bool {
	capacity, assigned := c.limits()
	if c.hello.OneShot && assigned >= 1 {
		return false
	}
	return capacity <= 0 || b.activeLocked() < capacity
}

func (b *HubBackend) available(teamID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.conn
	if c == nil || c.isClosed() || b.teamID != teamID {
		return false
	}
	return b.freeLocked(c)
}

// Start sends task.assign. It returns an error that wraps core.ErrAtCapacity
// when the worker is not connected, has no free slot, or is a one-shot worker
// that has its task.
func (b *HubBackend) Start(ctx context.Context, task core.Task) (core.TaskStatus, error) {
	b.mu.Lock()
	if e, ok := b.tasks[task.TaskID]; ok {
		st := e.status
		b.mu.Unlock()
		return st, nil
	}
	c := b.conn
	if c == nil || c.isClosed() {
		b.mu.Unlock()
		return core.TaskStatus{}, fmt.Errorf("worker %q is not connected: %w: %w", b.workerID, core.ErrAtCapacity, core.ErrUnavailable)
	}
	if b.teamID != task.TeamID {
		b.mu.Unlock()
		return core.TaskStatus{}, fmt.Errorf("worker %q belongs to another team: %w", b.workerID, core.ErrInvalid)
	}
	if !b.freeLocked(c) {
		b.mu.Unlock()
		return core.TaskStatus{}, fmt.Errorf("worker %q has no free slot: %w", b.workerID, core.ErrAtCapacity)
	}
	b.tasks[task.TaskID] = &taskEntry{status: core.TaskStatus{TaskID: task.TaskID, State: core.TaskAccepted}, reserved: true, watchers: map[*taskWatcher]struct{}{}}
	c.addAssigned(1)
	b.mu.Unlock()

	var body statusBody
	err := c.call(ctx, methodTaskAssign, assignParams{Task: task}, &body)
	if err != nil {
		b.mu.Lock()
		if e, ok := b.tasks[task.TaskID]; ok && e.reserved {
			b.closeWatchersLocked(e)
			delete(b.tasks, task.TaskID)
		}
		b.mu.Unlock()
		c.addAssigned(-1)
		if errors.Is(err, errDisconnected) {
			return core.TaskStatus{}, fmt.Errorf("worker %q disconnected during task.assign: %w: %w", b.workerID, core.ErrAtCapacity, core.ErrUnavailable)
		}
		return core.TaskStatus{}, err
	}
	body.Status.TaskID = task.TaskID
	b.mu.Lock()
	if e, ok := b.tasks[task.TaskID]; ok {
		e.reserved = false
	}
	st := b.mergeLocked(body.Status)
	b.mu.Unlock()
	return st, nil
}

// mergeLocked stores a status and tells the watchers. An older state never
// replaces a newer one. It returns the status that the backend now holds. The
// caller holds b.mu.
func (b *HubBackend) mergeLocked(st core.TaskStatus) core.TaskStatus {
	e := b.tasks[st.TaskID]
	if e == nil {
		e = &taskEntry{watchers: map[*taskWatcher]struct{}{}}
		b.tasks[st.TaskID] = e
		e.status = st
	} else {
		if e.status.State == core.TaskExited || stateRank(st.State) < stateRank(e.status.State) {
			return e.status
		}
		if sameStatus(e.status, st) {
			return e.status
		}
		e.status = st
	}
	for w := range e.watchers {
		pushLatest(w.ch, st)
	}
	if st.State == core.TaskExited {
		b.closeWatchersLocked(e)
	}
	return e.status
}

// sameStatus tells whether two statuses carry the same facts.
func sameStatus(a, b core.TaskStatus) bool {
	same := func(x, y *int) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }
	return a.State == b.State && a.Reason == b.Reason && a.Message == b.Message && a.SandboxID == b.SandboxID && same(a.ExitCode, b.ExitCode)
}

// pushLatest sends st and drops the oldest queued status when the channel is full.
func pushLatest(ch chan core.TaskStatus, st core.TaskStatus) {
	for {
		select {
		case ch <- st:
			return
		default:
		}
		select {
		case <-ch:
		default:
		}
	}
}

func (b *HubBackend) closeWatchersLocked(e *taskEntry) {
	for w := range e.watchers {
		close(w.ch)
		close(w.fin)
	}
	e.watchers = map[*taskWatcher]struct{}{}
}

// Status returns the last known status. For an unknown task it asks the worker.
func (b *HubBackend) Status(ctx context.Context, taskID string) (core.TaskStatus, error) {
	b.mu.Lock()
	if e, ok := b.tasks[taskID]; ok {
		st := e.status
		b.mu.Unlock()
		return st, nil
	}
	b.mu.Unlock()
	c := b.liveConn()
	if c == nil {
		return core.TaskStatus{}, b.notConnected()
	}
	var body statusBody
	if err := c.call(ctx, methodTaskStatus, taskRef{TaskID: taskID}, &body); err != nil {
		if errors.Is(err, errDisconnected) {
			return core.TaskStatus{}, fmt.Errorf("worker %q disconnected: %w", b.workerID, core.ErrUnavailable)
		}
		return core.TaskStatus{}, err
	}
	body.Status.TaskID = taskID
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mergeLocked(body.Status), nil
}

// Watch sends the current status and each change until the task exited.
func (b *HubBackend) Watch(ctx context.Context, taskID string) (<-chan core.TaskStatus, error) {
	if _, err := b.Status(ctx, taskID); err != nil {
		return nil, err
	}
	w := &taskWatcher{ch: make(chan core.TaskStatus, watchBuffer), fin: make(chan struct{})}
	b.mu.Lock()
	e := b.tasks[taskID]
	if e == nil {
		b.mu.Unlock()
		return nil, fmt.Errorf("task %q: %w", taskID, core.ErrNotFound)
	}
	w.ch <- e.status
	if e.status.State == core.TaskExited {
		close(w.ch)
		b.mu.Unlock()
		return w.ch, nil
	}
	e.watchers[w] = struct{}{}
	b.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			defer b.mu.Unlock()
			if cur := b.tasks[taskID]; cur != nil {
				if _, ok := cur.watchers[w]; ok {
					delete(cur.watchers, w)
					close(w.ch)
					close(w.fin)
				}
			}
		case <-w.fin:
		}
	}()
	return w.ch, nil
}

// Cancel sends task.cancel.
func (b *HubBackend) Cancel(ctx context.Context, taskID string, graceSeconds int) error {
	c := b.liveConn()
	if c == nil {
		return b.notConnected()
	}
	err := c.call(ctx, methodTaskCancel, cancelParams{TaskID: taskID, GraceSeconds: graceSeconds}, nil)
	if errors.Is(err, errDisconnected) {
		return fmt.Errorf("worker %q disconnected: %w", b.workerID, core.ErrUnavailable)
	}
	return err
}

// Destroy sends task.destroy. A task that the worker does not know counts as destroyed.
func (b *HubBackend) Destroy(ctx context.Context, taskID string) error {
	c := b.liveConn()
	if c == nil {
		return b.notConnected()
	}
	err := c.call(ctx, methodTaskDestroy, taskRef{TaskID: taskID}, nil)
	if errors.Is(err, errDisconnected) {
		return fmt.Errorf("worker %q disconnected: %w", b.workerID, core.ErrUnavailable)
	}
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	b.mu.Lock()
	if e, ok := b.tasks[taskID]; ok {
		b.closeWatchersLocked(e)
		delete(b.tasks, taskID)
	}
	b.mu.Unlock()
	return nil
}

// List returns the tasks that the worker holds, from its hello and its events.
func (b *HubBackend) List(context.Context) ([]core.TaskStatus, error) {
	if b.liveConn() == nil {
		return nil, b.notConnected()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]core.TaskStatus, 0, len(b.tasks))
	for _, e := range b.tasks {
		out = append(out, e.status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out, nil
}

// Preflight returns nil when the worker is connected.
func (b *HubBackend) Preflight(context.Context) error {
	if b.liveConn() == nil {
		return b.notConnected()
	}
	return nil
}

// checkOwner refuses a connection of another team or principal while a live
// connection holds the worker ID.
func (b *HubBackend) checkOwner(teamID, principal string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.checkOwnerLocked(teamID, principal)
}

func (b *HubBackend) checkOwnerLocked(teamID, principal string) error {
	if b.conn == nil || b.conn.isClosed() {
		return nil
	}
	if b.teamID != teamID {
		return fmt.Errorf("worker id %q is in use by another team", b.workerID)
	}
	if b.principal != principal {
		return fmt.Errorf("worker id %q is in use by another principal", b.workerID)
	}
	return nil
}

// attach makes c the connection of the backend and takes its tasks from the
// hello. The hello is the truth about what the worker holds: a task that it
// does not list is gone. It returns the connection that c replaced.
func (b *HubBackend) attach(c *v2Conn, teamID string) (old *v2Conn, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.checkOwnerLocked(teamID, c.principal); err != nil {
		return nil, err
	}
	b.principal = c.principal
	if b.teamID != teamID {
		for _, e := range b.tasks {
			b.closeWatchersLocked(e)
		}
		b.tasks = map[string]*taskEntry{}
		b.teamID = teamID
	}
	old, b.conn = b.conn, c
	held := map[string]bool{}
	for _, t := range c.hello.Tasks {
		id := t.TaskID
		if id == "" {
			id = t.Status.TaskID
		}
		if id == "" {
			continue
		}
		held[id] = true
		t.Status.TaskID = id
		if e, ok := b.tasks[id]; ok && e.status.State != core.TaskExited {
			// The hello is newer than the cache, also when it shows an older state.
			e.status = t.Status
			for w := range e.watchers {
				pushLatest(w.ch, t.Status)
			}
			if t.Status.State == core.TaskExited {
				b.closeWatchersLocked(e)
			}
			continue
		}
		if _, ok := b.tasks[id]; ok {
			continue
		}
		b.mergeLocked(t.Status)
	}
	for id, e := range b.tasks {
		if !held[id] && !e.reserved {
			b.closeWatchersLocked(e)
			delete(b.tasks, id)
		}
	}
	return old, nil
}

// detach removes c when it is still the connection of the backend.
func (b *HubBackend) detach(c *v2Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == c {
		b.conn = nil
	}
}

// ---------- connection ----------

type v2Conn struct {
	b           *HubBackend
	conn        *ws.Conn
	id          string
	teamID      string
	principal   string
	hello       HelloV2
	connectedAt time.Time
	lastSeen    atomic.Int64

	mu       sync.Mutex
	capacity int
	active   int // from the last heartbeat
	assigned int
	pending  map[string]chan *message
	closed   bool
	done     chan struct{}
}

func (c *v2Conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *v2Conn) limits() (capacity, assigned int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacity, c.assigned
}

func (c *v2Conn) addAssigned(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.assigned = max(0, c.assigned+n)
}

func (c *v2Conn) info() ConnectedWorker {
	c.b.mu.Lock()
	active := c.b.activeLocked()
	c.b.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	h := Hello{WorkerID: c.hello.WorkerID, Name: c.hello.Name, Backend: c.hello.Backend, Labels: c.hello.Labels,
		Capacity: c.capacity, Active: active, Version: c.hello.Version}
	return ConnectedWorker{ConnectionID: c.id, TeamID: c.teamID, Hello: h, Active: active,
		ConnectedAt: c.connectedAt, LastSeen: time.Unix(0, c.lastSeen.Load())}
}

func (c *v2Conn) send(m *message) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(ws.OpText, data)
}

// call sends a request and waits for the response.
func (c *v2Conn) call(ctx context.Context, method string, params, out any) error {
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	id := fmt.Sprintf("r%d", c.b.h.reqSeq.Add(1))
	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errDisconnected
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(&message{Type: typeRequest, ID: id, Method: method, Params: p}); err != nil {
		return fmt.Errorf("%w: %w", errDisconnected, err)
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error.toErrorV2()
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return errDisconnected
	}
}

func (c *v2Conn) watchdog() {
	interval := c.b.h.opts.HeartbeatInterval
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			if time.Since(time.Unix(0, c.lastSeen.Load())) > 3*interval {
				c.b.h.log.Warn("worker heartbeat timeout", "worker", c.b.workerID, "connection", c.id)
				_ = c.conn.CloseWithStatus(ws.CloseGoingAway, "heartbeat timeout")
				return
			}
		}
	}
}

func (c *v2Conn) readLoop(ctx context.Context) {
	defer func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
		_ = c.conn.Close()
	}()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.lastSeen.Store(time.Now().UnixNano())
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			c.b.h.log.Warn("invalid worker message", "worker", c.b.workerID, "err", err)
			continue
		}
		switch m.Type {
		case typeResponse:
			c.mu.Lock()
			ch := c.pending[m.ID]
			c.mu.Unlock()
			if ch != nil {
				ch <- &m
			}
		case typeHeartbeat:
			var hb Heartbeat
			if json.Unmarshal(m.Params, &hb) == nil {
				c.mu.Lock()
				c.active = hb.Active
				c.capacity = hb.Capacity
				c.mu.Unlock()
			}
			c.b.h.upsertV2(ctx, c)
		case typeEvent:
			if m.Method != eventTaskState {
				continue
			}
			var body statusBody
			if json.Unmarshal(m.Params, &body) != nil || body.Status.TaskID == "" {
				c.b.h.log.Warn("invalid task.state event", "worker", c.b.workerID)
				continue
			}
			c.b.mu.Lock()
			if c.b.conn == c {
				c.b.mergeLocked(body.Status)
			}
			c.b.mu.Unlock()
		}
	}
}

func (h *Hub) upsertV2(ctx context.Context, c *v2Conn) {
	if h.store == nil {
		return
	}
	connected := !c.isClosed()
	c.b.mu.Lock()
	active := c.b.activeLocked()
	c.b.mu.Unlock()
	capacity, _ := c.limits()
	if !connected {
		active = 0
	}
	w := &core.Worker{
		ID: c.hello.WorkerID, TeamID: c.teamID, Name: c.hello.Name, Backend: c.hello.Backend,
		Labels: c.hello.Labels, Capacity: capacity, Active: active,
		LastHeartbeat: time.Unix(0, c.lastSeen.Load()).UTC(), Version: c.hello.Version,
	}
	if w.Name == "" {
		w.Name = w.ID
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := h.store.UpsertWorker(uctx, w); err != nil {
		h.log.Warn("record worker heartbeat", "worker", w.ID, "err", err)
	}
}

// serveV2 runs one connection of version 2.
func (h *Hub) serveV2(ctx context.Context, conn *ws.Conn, teamID, principal, token string) {
	ctx = context.WithoutCancel(ctx)
	helloCh := make(chan *message, 1)
	go func() {
		_, data, err := conn.ReadMessage()
		if err != nil {
			helloCh <- nil
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil || m.Type != typeHello {
			helloCh <- nil
			return
		}
		helloCh <- &m
	}()
	var hm *message
	select {
	case hm = <-helloCh:
	case <-time.After(h.opts.HelloTimeout):
	}
	if hm == nil {
		_ = conn.CloseWithStatus(ws.CloseProtocolError, "expected hello")
		return
	}
	var hello HelloV2
	if err := json.Unmarshal(hm.Params, &hello); err != nil {
		_ = conn.CloseWithStatus(ws.CloseProtocolError, "invalid hello")
		return
	}
	if err := ValidateWorkerID(hello.WorkerID); err != nil {
		_ = conn.CloseWithStatus(closePolicy, err.Error())
		return
	}
	if hello.Capacity < 0 {
		hello.Capacity = 0
	}
	if h.opts.AuthorizeWorker != nil {
		if err := h.opts.AuthorizeWorker(ctx, token, hello.WorkerID); err != nil {
			h.log.Warn("worker key refused for worker id", "worker", hello.WorkerID, "team", teamID)
			_ = conn.CloseWithStatus(closePolicy, "worker key is not valid for this worker id")
			return
		}
	}
	b := h.v2Backend(hello.WorkerID, true)
	now := time.Now().UTC()
	c := &v2Conn{
		b: b, conn: conn, id: core.NewID("wconn"), teamID: teamID, principal: principal, hello: hello,
		connectedAt: now, capacity: hello.Capacity,
		pending: map[string]chan *message{}, done: make(chan struct{}),
	}
	c.lastSeen.Store(now.UnixNano())
	if err := b.checkOwner(teamID, principal); err != nil {
		h.log.Warn("worker refused", "worker", hello.WorkerID, "team", teamID, "err", err)
		_ = conn.CloseWithStatus(closePolicy, err.Error())
		return
	}
	welcome, _ := json.Marshal(Welcome{ConnectionID: c.id, HeartbeatMS: h.opts.HeartbeatInterval.Milliseconds()})
	if err := c.send(&message{Type: typeWelcome, Result: welcome}); err != nil {
		_ = conn.Close()
		return
	}
	old, err := b.attach(c, teamID)
	if err != nil {
		h.log.Warn("worker refused", "worker", hello.WorkerID, "team", teamID, "err", err)
		_ = conn.CloseWithStatus(closePolicy, err.Error())
		return
	}
	if old != nil {
		_ = old.conn.CloseWithStatus(ws.CloseGoingAway, "replaced by a new connection")
	}
	h.upsertV2(ctx, c)
	h.log.Info("worker connected", "worker", hello.WorkerID, "protocol", "v2", "team", teamID, "backend", hello.Backend,
		"capacity", hello.Capacity, "one_shot", hello.OneShot, "tasks", len(hello.Tasks), "connection", c.id)

	go c.watchdog()
	c.readLoop(ctx)

	b.detach(c)
	h.upsertV2(ctx, c)
	h.log.Info("worker disconnected", "worker", hello.WorkerID, "protocol", "v2", "connection", c.id)
}
