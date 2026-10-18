package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store"
	"github.com/openfactory-org/openfactory/internal/worker"
)

// middleware adds panic recovery and request logging.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))
				if !rec.wroteHeader {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal server error"}}`))
				}
				rec.status = http.StatusInternalServerError
			}
			lvl := slog.LevelInfo
			switch {
			case rec.status >= 500:
				lvl = slog.LevelError
			case r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" || strings.HasPrefix(r.URL.Path, "/static/"):
				lvl = slog.LevelDebug
			}
			s.log.Log(r.Context(), lvl, "http request", "method", r.Method, "path", r.URL.Path, "status", rec.code(),
				"bytes", rec.bytes, "duration_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
		}()
		next.ServeHTTP(rec, r)
	})
}

// statusRecorder captures the status code while keeping streaming (Flush) and
// connection upgrades (Hijack) working.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.status, r.wroteHeader = http.StatusOK, true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	if !r.wroteHeader {
		r.status, r.wroteHeader = http.StatusSwitchingProtocols, true
	}
	return h.Hijack()
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.Store.ListTeams(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "error": "store: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// ---------- Prometheus metrics ----------

var allRunStates = []core.RunState{core.RunQueued, core.RunStarting, core.RunRunning, core.RunIdle, core.RunPending, core.RunClaimed, core.RunBlocked, core.RunSucceeded, core.RunFailed, core.RunError, core.RunCancelled}

// maxCountedRuns caps per-state run counting.
const maxCountedRuns = 100_000

type metricsCollector struct {
	s   *Server
	mu  sync.Mutex
	at  time.Time
	buf []byte
}

func newMetricsCollector(s *Server) *metricsCollector { return &metricsCollector{s: s} }

func (m *metricsCollector) serveHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buf == nil || time.Since(m.at) > 5*time.Second {
		b, err := m.collect(r.Context())
		if err != nil {
			http.Error(w, "collect metrics: "+err.Error(), http.StatusInternalServerError)
			return
		}
		m.buf, m.at = b, time.Now()
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(m.buf)
}

func (m *metricsCollector) collect(ctx context.Context) ([]byte, error) {
	s := m.s
	reg := worker.NewRegistry()
	reg.RegisterGauge("of_build_info", "Build information.", "version")
	reg.RegisterGauge("of_runs", "Agent runs by state.", "state")
	reg.RegisterGauge("of_run_queue_depth", "Runs waiting to be claimed.")
	reg.RegisterGauge("of_workers_connected", "Connected remote workers.")
	reg.RegisterGauge("of_worker_connections", "Live connections per remote worker.", "worker", "backend")
	reg.RegisterGauge("of_worker_capacity", "Sandbox capacity per remote worker (0 = unlimited).", "worker")
	reg.RegisterGauge("of_worker_active_sandboxes", "Active sandboxes per remote worker.", "worker")
	reg.RegisterGauge("of_event_bus_published_total", "Events published on this replica.")
	reg.RegisterGauge("of_event_bus_dropped_total", "Live events dropped for slow subscribers.")
	reg.RegisterGauge("of_outbox_deliveries_total", "Durable outbox deliveries handled by this replica, by result.", "result")
	reg.RegisterGauge("of_leader", "1 when this replica holds the leader lease.", "replica")
	reg.RegisterGauge("of_plugins_loaded", "gRPC plugins running in this replica.")
	reg.RegisterGauge("of_notifications_total", "Outbound notification deliveries by result.", "result")

	reg.Set("of_build_info", 1, s.version)
	for _, st := range allRunStates {
		n, err := countRuns(ctx, s.Store, st)
		if err != nil {
			return nil, err
		}
		reg.Set("of_runs", float64(n), string(st))
		if st == core.RunQueued {
			reg.Set("of_run_queue_depth", float64(n))
		}
	}
	workers := map[string]bool{}
	type agg struct{ conns, capacity, active int }
	per := map[string]*agg{}
	backends := map[string]string{}
	for _, w := range s.Hub.Workers() {
		id := w.Hello.WorkerID
		workers[id] = true
		a := per[id]
		if a == nil {
			a = &agg{}
			per[id] = a
		}
		a.conns++
		a.capacity += w.Hello.Capacity
		a.active += w.Active
		backends[id] = w.Hello.Backend
	}
	reg.Set("of_workers_connected", float64(len(workers)))
	for id, a := range per {
		reg.Set("of_worker_connections", float64(a.conns), id, backends[id])
		reg.Set("of_worker_capacity", float64(a.capacity), id)
		reg.Set("of_worker_active_sandboxes", float64(a.active), id)
	}
	if s.durable != nil { // built-in bus only; a replaced bus reports its own metrics
		bs := s.durable.DurableStats()
		reg.Set("of_event_bus_published_total", float64(bs.Published))
		reg.Set("of_event_bus_dropped_total", float64(bs.Dropped))
		reg.Set("of_outbox_deliveries_total", float64(bs.Delivered), "delivered")
		reg.Set("of_outbox_deliveries_total", float64(bs.Retried), "retried")
		reg.Set("of_outbox_deliveries_total", float64(bs.DeadLettered), "dead_lettered")
		reg.Set("of_outbox_deliveries_total", float64(bs.Fallbacks), "in_process_fallback")
	}
	leader := 0.0
	if s.Leader.IsLeader() {
		leader = 1
	}
	reg.Set("of_leader", leader, s.replicaID)
	reg.Set("of_plugins_loaded", float64(len(s.Plugins.Plugins())))
	ns := s.Notify.Stats()
	reg.Set("of_notifications_total", float64(ns.Delivered), "delivered")
	reg.Set("of_notifications_total", float64(ns.Failed), "failed")
	reg.Set("of_notifications_total", float64(ns.Unroutable), "unroutable")
	reg.Set("of_notifications_total", float64(ns.Retries), "retried")

	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func countRuns(ctx context.Context, st core.Store, state core.RunState) (int, error) {
	n := 0
	f := core.RunFilter{States: []core.RunState{state}, ListOpts: core.ListOpts{Limit: store.MaxLimit}}
	for n < maxCountedRuns {
		runs, err := st.ListRuns(ctx, f)
		if err != nil {
			return n, err
		}
		n += len(runs)
		if len(runs) < store.MaxLimit {
			break
		}
		f.Cursor = store.RunsCursor(runs)
		if f.Cursor == "" {
			break
		}
	}
	return n, nil
}
