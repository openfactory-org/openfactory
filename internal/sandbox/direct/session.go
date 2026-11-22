package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/internal/ws"
)

// readTimeoutBeats is the number of heartbeat intervals without a frame from
// the hub after which the worker drops the connection.
const readTimeoutBeats = 3

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
