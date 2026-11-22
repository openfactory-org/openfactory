package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/machine/backend"
	"github.com/openfactory-org/openfactory/internal/machine/config"
	"github.com/openfactory-org/openfactory/internal/machine/identity"
	"github.com/openfactory-org/openfactory/internal/machine/store"
	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
)

// Status is the answer of the control socket and of worker status.
type Status struct {
	WorkerID    string              `json:"worker_id"`
	Name        string              `json:"name,omitempty"`
	ControlURL  string              `json:"control_url"`
	Draining    bool                `json:"draining"`
	Slots       int                 `json:"slots"`
	SlotsUsed   int                 `json:"slots_used"`
	LastWelcome *time.Time          `json:"last_welcome,omitempty"`
	Running     bool                `json:"running"`
	Tasks       []rtcore.TaskStatus `json:"tasks"`
}

// Controller is what the control socket serves.
type Controller interface {
	Status(ctx context.Context) Status
	SetDraining(bool)
}

type statusBox struct {
	cfg config.Config
	id  identity.Identity
	be  *backend.Backend

	mu   sync.Mutex
	last *time.Time
}

func (s *statusBox) welcome(direct.Welcome) {
	now := time.Now().UTC()
	s.mu.Lock()
	s.last = &now
	s.mu.Unlock()
}

func (s *statusBox) Status(ctx context.Context) Status {
	tasks, _ := s.be.List(ctx)
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	return Status{
		WorkerID: s.id.WorkerID, Name: s.cfg.Name, ControlURL: s.cfg.ControlURL, Draining: s.be.Draining(),
		Slots: s.cfg.Slots, SlotsUsed: s.be.SlotsUsed(), LastWelcome: last, Running: true, Tasks: tasks,
	}
}

func (s *statusBox) SetDraining(d bool) { s.be.SetDraining(d) }

// ServeControl serves GET /status and POST /drain on a root-only unix socket.
func ServeControl(path string, c Controller) (func(), error) {
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status(r.Context()))
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Draining bool `json:"draining"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		c.SetDraining(body.Draining)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c.Status(r.Context()))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close(); _ = os.Remove(path) }, nil
}

// Client talks to the control socket of a running agent.
type Client struct {
	hc *http.Client
}

// NewClient returns a client of the control socket at path.
func NewClient(path string) *Client {
	return &Client{hc: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}}
}

// Status reads the status of the agent.
func (c *Client) Status(ctx context.Context) (Status, error) {
	return c.do(ctx, http.MethodGet, "/status", nil)
}

// Drain changes the drain state of the agent.
func (c *Client) Drain(ctx context.Context, draining bool) (Status, error) {
	body, _ := json.Marshal(map[string]bool{"draining": draining})
	return c.do(ctx, http.MethodPost, "/drain", body)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte) (Status, error) {
	var st Status
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, strings.NewReader(string(body)))
	if err != nil {
		return st, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return st, fmt.Errorf("the worker agent does not answer (is openfactory-worker running?): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("worker agent: %s", resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// OfflineStatus reads the status from the task records when the agent
// does not run.
func OfflineStatus(cfg config.Config) (Status, error) {
	id, err := identity.Load(cfg.IdentityDir())
	if err != nil {
		return Status{}, err
	}
	st := Status{WorkerID: id.WorkerID, Name: cfg.Name, ControlURL: id.ControlURL, Slots: cfg.Slots}
	s := &store.Store{Dir: filepath.Join(cfg.WorkerDir(), "tasks")}
	recs, _ := s.List()
	for _, r := range recs {
		st.Tasks = append(st.Tasks, r.Status)
		if r.Status.State != rtcore.TaskExited {
			st.SlotsUsed++
		}
	}
	return st, nil
}
