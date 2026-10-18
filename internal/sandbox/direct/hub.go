package direct

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/internal/ws"
)

// HubOptions configures a Hub.
type HubOptions struct {
	// Authenticate validates a worker's bearer token (a team API key) and
	// returns the team id and the principal (the user or service account of
	// the key). Required. While a worker ID has a live connection, a new
	// connection with that ID must come from the same team and principal;
	// the same principal can reconnect and replace the old connection.
	Authenticate func(ctx context.Context, token string) (teamID, principal string, err error)
	// AuthorizeWorker, when set, is called with the bearer token and the
	// worker ID of a hello. A non-nil error refuses the worker. It lets a
	// worker key that is limited to worker IDs work. The API key model has no
	// such limit yet, so the server does not set it.
	AuthorizeWorker func(ctx context.Context, token, workerID string) error
	// HeartbeatInterval is announced to workers (default 15s). Connections
	// silent for 3 intervals are closed.
	HeartbeatInterval time.Duration
	// HelloTimeout bounds the wait for the worker's hello (default 10s).
	HelloTimeout time.Duration
	Logger       *slog.Logger
}

// Hub accepts worker connections. It gives the dispatcher a core.TaskBackend
// for each worker ID (BackendFor, Backends).
type Hub struct {
	store core.Store
	opts  HubOptions
	log   *slog.Logger

	reqSeq atomic.Uint64
	v2     v2State // the backends of the workers
}

// NewHub creates a hub. store receives worker registrations and heartbeats.
func NewHub(store core.Store, opts HubOptions) *Hub {
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 15 * time.Second
	}
	if opts.HelloTimeout <= 0 {
		opts.HelloTimeout = 10 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Hub{store: store, opts: opts, log: log.With("component", "worker-hub")}
}

// Register mounts the worker connection endpoint.
func (h *Hub) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+ConnectPath, h.handleConnect)
}

// ConnectedWorker describes a live worker connection.
type ConnectedWorker struct {
	ConnectionID string
	TeamID       string
	Hello        Hello
	Active       int
	ConnectedAt  time.Time
	LastSeen     time.Time
}

// Workers lists live connections sorted by worker id.
func (h *Hub) Workers() []ConnectedWorker {
	out := h.v2Workers()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hello.WorkerID == out[j].Hello.WorkerID {
			return out[i].ConnectionID < out[j].ConnectionID
		}
		return out[i].Hello.WorkerID < out[j].Hello.WorkerID
	})
	return out
}

// Connected reports whether a worker with this id is connected.
func (h *Hub) Connected(workerID string) bool { return h.v2Connected(workerID) }

// WaitConnected blocks until workerID connects or ctx ends.
func (h *Hub) WaitConnected(ctx context.Context, workerID string) error {
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for !h.Connected(workerID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (h *Hub) handleConnect(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token == "" || h.opts.Authenticate == nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "worker API key required")
		return
	}
	teamID, principal, err := h.opts.Authenticate(r.Context(), token)
	if errors.Is(err, core.ErrForbidden) {
		writeJSONError(w, http.StatusForbidden, "forbidden", "the worker API key does not have the role to connect a worker")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid worker API key")
		return
	}
	conn, err := ws.Upgrade(w, r, []string{SubprotocolV2})
	if err != nil {
		return
	}
	if conn.Subprotocol != SubprotocolV2 {
		// The client did not offer the protocol of this server.
		_ = conn.CloseWithStatus(ws.CloseProtocolError, "unsupported worker protocol: this server needs worker protocol v2")
		return
	}
	h.serveV2(r.Context(), conn, teamID, principal, token)
}

var errDisconnected = errors.New("worker disconnected")
