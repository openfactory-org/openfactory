// Package directtest is a fake hub of worker protocol v2 for tests of the
// worker side. The real hub is WorkerHubDO in the TypeScript control plane.
// The fake accepts one worker connection at a time at ConnectPath,
// keeps the last hello and the task statuses that the worker reports, and
// sends requests (task.assign, task.status, task.cancel, task.destroy,
// task.list, drain) to the worker.
package directtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/internal/ws"
)

// The package does not import package direct, so the tests of direct can use
// it. TestDirecttestConstants in direct checks that these values match.
const (
	// ConnectPath is the worker connection endpoint.
	ConnectPath = "/api/v1/workers/connect"
	// Subprotocol is the WebSocket subprotocol of worker protocol v2.
	Subprotocol = "openfactory.worker.v2"
)

// Hello is the part of a worker hello that tests read.
type Hello struct {
	WorkerID      string      `json:"worker_id"`
	Name          string      `json:"name,omitempty"`
	Backend       string      `json:"backend"`
	Capacity      int         `json:"capacity"`
	Version       string      `json:"version,omitempty"`
	OneShot       bool        `json:"one_shot,omitempty"`
	Tasks         []HelloTask `json:"tasks"`
	ProtocolMinor int         `json:"protocol_minor,omitempty"`
	Features      []string    `json:"features,omitempty"`
	Facts         *Facts      `json:"facts,omitempty"`
	Slots         int         `json:"slots,omitempty"`
	Draining      bool        `json:"draining,omitempty"`
}

// Facts is the part of the worker facts that tests read.
type Facts struct {
	Arch  string `json:"arch"`
	OS    string `json:"os,omitempty"`
	VCPUs int    `json:"vcpus"`
}

// HelloTask is one task of a hello or of a task.list result.
type HelloTask struct {
	TaskID string            `json:"task_id"`
	Status rtcore.TaskStatus `json:"status"`
}

// ErrNotConnected is returned by the request methods when no worker is
// connected.
var ErrNotConnected = fmt.Errorf("no worker connection: %w", rtcore.ErrUnavailable)

// closePolicy is the WebSocket close code of a refused connection.
const closePolicy = 1008

// Options configure a Hub.
type Options struct {
	// APIKey is the bearer token that the hub accepts. Other tokens get 401.
	// Empty accepts any token.
	APIKey string
	// HeartbeatMS is the heartbeat interval that the welcome announces.
	HeartbeatMS int64
	// Refuse, when set, returns a reason to refuse a hello. The hub then
	// closes the connection with the policy code (1008).
	Refuse func(Hello) string
}

type envelope struct {
	Type   string          `json:"type"`
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type conn struct {
	id      int
	ws      *ws.Conn
	mu      sync.Mutex
	pending map[string]chan envelope
	closed  chan struct{}
}

// Hub is the fake hub.
type Hub struct {
	opts Options
	srv  *httptest.Server

	mu       sync.Mutex
	cur      *conn
	conns    int
	hellos   []Hello
	statuses map[string]rtcore.TaskStatus
	beats    int
	nextID   int
}

// New starts a hub on a test server. The server closes at the end of the test.
func New(t testing.TB, opts Options) *Hub {
	t.Helper()
	h := &Hub{opts: opts, statuses: map[string]rtcore.TaskStatus{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ConnectPath, h.serve)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		h.Drop()
		h.srv.Close()
	})
	return h
}

// URL is the base URL of the hub.
func (h *Hub) URL() string { return h.srv.URL }

// Addr is the host:port of the hub.
func (h *Hub) Addr() string { return h.srv.Listener.Addr().String() }

func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	if h.opts.APIKey != "" && r.Header.Get("Authorization") != "Bearer "+h.opts.APIKey {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	wsc, err := ws.Upgrade(w, r, []string{Subprotocol})
	if err != nil {
		return
	}
	defer wsc.Close()
	if wsc.Subprotocol != Subprotocol {
		_ = wsc.CloseWithStatus(closePolicy, "this hub speaks only worker protocol v2")
		return
	}
	_, data, err := wsc.ReadMessage()
	if err != nil {
		return
	}
	var m envelope
	var hello Hello
	if json.Unmarshal(data, &m) != nil || m.Type != "hello" || json.Unmarshal(m.Params, &hello) != nil {
		_ = wsc.CloseWithStatus(closePolicy, "expected hello")
		return
	}
	if h.opts.Refuse != nil {
		if reason := h.opts.Refuse(hello); reason != "" {
			_ = wsc.CloseWithStatus(closePolicy, reason)
			return
		}
	}
	h.mu.Lock()
	h.conns++
	c := &conn{id: h.conns, ws: wsc, pending: map[string]chan envelope{}, closed: make(chan struct{})}
	if old := h.cur; old != nil {
		_ = old.ws.Close()
	}
	h.cur = c
	h.hellos = append(h.hellos, hello)
	// The hello lists every task that the worker holds: a task that it does
	// not list is gone.
	h.statuses = map[string]rtcore.TaskStatus{}
	for _, t := range hello.Tasks {
		st := t.Status
		st.TaskID = t.TaskID
		h.statuses[t.TaskID] = st
	}
	h.mu.Unlock()
	defer func() {
		close(c.closed)
		h.mu.Lock()
		if h.cur == c {
			h.cur = nil
		}
		h.mu.Unlock()
	}()
	welcome, _ := json.Marshal(map[string]any{"connection_id": "conn_" + strconv.Itoa(c.id), "heartbeat_ms": h.opts.HeartbeatMS})
	if err := writeJSON(wsc, envelope{Type: "welcome", Result: welcome}); err != nil {
		return
	}
	for {
		_, data, err := wsc.ReadMessage()
		if err != nil {
			return
		}
		var m envelope
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch m.Type {
		case "response":
			c.mu.Lock()
			ch := c.pending[m.ID]
			delete(c.pending, m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case "event":
			var body struct {
				Status rtcore.TaskStatus `json:"status"`
			}
			if m.Method == "task.state" && json.Unmarshal(m.Params, &body) == nil {
				h.record(body.Status)
			}
		case "heartbeat":
			h.mu.Lock()
			h.beats++
			h.mu.Unlock()
		case "ping":
			_ = wsc.WriteMessage(ws.OpText, []byte(`{"type":"pong"}`))
		}
	}
}

func writeJSON(c *ws.Conn, m envelope) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.WriteMessage(ws.OpText, data)
}

func (h *Hub) record(st rtcore.TaskStatus) {
	if st.TaskID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.statuses[st.TaskID]; ok && old.State == rtcore.TaskExited {
		return
	}
	h.statuses[st.TaskID] = st
}

// Connected reports whether a worker is connected.
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur != nil
}

// Connections is the number of accepted connections so far.
func (h *Hub) Connections() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns
}

// Hello returns the last hello.
func (h *Hub) Hello() (Hello, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.hellos) == 0 {
		return Hello{}, false
	}
	return h.hellos[len(h.hellos)-1], true
}

// Heartbeats is the number of heartbeats received.
func (h *Hub) Heartbeats() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.beats
}

// Status returns the last known status of a task: from task.state events,
// replies to requests and the task list of the last hello.
func (h *Hub) Status(taskID string) (rtcore.TaskStatus, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.statuses[taskID]
	return st, ok
}

// Drop closes the current connection, as a network failure would.
func (h *Hub) Drop() {
	h.mu.Lock()
	c := h.cur
	h.mu.Unlock()
	if c != nil {
		_ = c.ws.Close()
		<-c.closed
	}
}

// Call sends one request to the worker and decodes the result into out
// (when out is not nil). Wire errors map onto rtcore.ErrNotFound,
// rtcore.ErrInvalid and rtcore.ErrAtCapacity.
func (h *Hub) Call(ctx context.Context, method string, params, out any) error {
	h.mu.Lock()
	c := h.cur
	h.nextID++
	id := "req_" + strconv.Itoa(h.nextID)
	h.mu.Unlock()
	if c == nil {
		return ErrNotConnected
	}
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	ch := make(chan envelope, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := writeJSON(c.ws, envelope{Type: "request", ID: id, Method: method, Params: p}); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return wireErr(m.Error)
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-c.closed:
		return ErrNotConnected
	case <-ctx.Done():
		return ctx.Err()
	}
}

func wireErr(e *wireError) error {
	switch e.Code {
	case "not_found":
		return fmt.Errorf("%s: %w", e.Message, rtcore.ErrNotFound)
	case "invalid_spec":
		return fmt.Errorf("%s: %w", e.Message, rtcore.ErrInvalid)
	case "at_capacity":
		return fmt.Errorf("%s: %w", e.Message, rtcore.ErrAtCapacity)
	}
	return errors.New(e.Code + ": " + e.Message)
}

type statusBody struct {
	Status rtcore.TaskStatus `json:"status"`
}

// Assign sends task.assign. extras, when not nil, is the "worker" field.
func (h *Hub) Assign(ctx context.Context, task rtcore.Task, extras json.RawMessage) (rtcore.TaskStatus, error) {
	var out statusBody
	params := struct {
		Task   rtcore.Task     `json:"task"`
		Worker json.RawMessage `json:"worker,omitempty"`
	}{task, extras}
	if err := h.Call(ctx, "task.assign", params, &out); err != nil {
		return rtcore.TaskStatus{}, err
	}
	out.Status.TaskID = task.TaskID
	h.record(out.Status)
	return out.Status, nil
}

// TaskStatus sends task.status.
func (h *Hub) TaskStatus(ctx context.Context, taskID string) (rtcore.TaskStatus, error) {
	var out statusBody
	if err := h.Call(ctx, "task.status", map[string]string{"task_id": taskID}, &out); err != nil {
		return rtcore.TaskStatus{}, err
	}
	return out.Status, nil
}

// Cancel sends task.cancel.
func (h *Hub) Cancel(ctx context.Context, taskID string, graceSeconds int) error {
	return h.Call(ctx, "task.cancel", map[string]any{"task_id": taskID, "grace_seconds": graceSeconds}, nil)
}

// Destroy sends task.destroy and forgets the task.
func (h *Hub) Destroy(ctx context.Context, taskID string) error {
	if err := h.Call(ctx, "task.destroy", map[string]string{"task_id": taskID}, nil); err != nil {
		return err
	}
	h.mu.Lock()
	delete(h.statuses, taskID)
	h.mu.Unlock()
	return nil
}

// List sends task.list.
func (h *Hub) List(ctx context.Context) ([]HelloTask, error) {
	var out struct {
		Tasks []HelloTask `json:"tasks"`
	}
	if err := h.Call(ctx, "task.list", struct{}{}, &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// Drain sends drain.
func (h *Hub) Drain(ctx context.Context, draining bool) error {
	return h.Call(ctx, "drain", map[string]bool{"draining": draining}, nil)
}
