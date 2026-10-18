package worker

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metric names exported by workers (Prometheus text exposition).
const (
	MetricConnected     = "of_worker_connected"
	MetricTasksActive   = "of_worker_sandboxes_active"
	MetricTasksMax      = "of_worker_sandboxes_max_concurrent"
	MetricTasksClaimed  = "of_worker_sandboxes_claimed_total"
	MetricTasksRejected = "of_worker_sandboxes_rejected_total"
	MetricTasksDone     = "of_worker_sandboxes_completed_total"
	MetricTaskDuration  = "of_worker_sandbox_duration_seconds"
	MetricTaskFailures  = "of_worker_sandbox_failures_total"
	MetricReconnects    = "of_worker_websocket_reconnects_total"
	MetricInfo          = "of_worker_info"
)

// Task results for MetricTasksDone.
const (
	ResultSucceeded  = "succeeded"
	ResultFailed     = "failed"
	ResultCancelled  = "cancelled"
	ResultDispatched = "dispatched"
)

// DefaultDurationBuckets are task duration histogram buckets in seconds.
var DefaultDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800}

type metricKind int

const (
	kindCounter metricKind = iota
	kindGauge
	kindHistogram
)

type series struct {
	labels  []string // values, aligned with family.labelNames
	value   float64
	buckets []uint64 // histogram cumulative counts per bucket (non-cumulative stored)
	count   uint64
	sum     float64
}

type family struct {
	name       string
	help       string
	kind       metricKind
	labelNames []string
	buckets    []float64
	series     map[string]*series
}

// Metrics is a small, dependency-free metrics registry rendering the
// Prometheus text exposition format. It is safe for concurrent use.
type Metrics struct {
	mu       sync.Mutex
	families []*family
	byName   map[string]*family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Metrics { return &Metrics{byName: map[string]*family{}} }

// NewMetrics returns a registry pre-seeded with the worker metric catalog so
// every series exists from startup, before any task runs.
func NewMetrics(workerID, backend, version string, maxConcurrent int) *Metrics {
	m := NewRegistry()
	m.register(MetricConnected, "1 while the worker has an active connection to the control plane, 0 otherwise.", kindGauge)
	m.register(MetricTasksActive, "Tasks currently executing on this worker.", kindGauge)
	m.register(MetricTasksMax, "Configured concurrency limit (0 means unlimited).", kindGauge)
	m.register(MetricTasksClaimed, "Total tasks accepted since process start.", kindCounter)
	m.register(MetricTasksRejected, "Tasks the worker declined.", kindCounter, "reason")
	m.register(MetricTasksDone, "Completed tasks by result.", kindCounter, "result")
	m.RegisterHistogram(MetricTaskDuration, "Wall-clock task duration on the worker.", DefaultDurationBuckets, "result")
	m.register(MetricTaskFailures, "Task failures by execution phase and bounded reason.", kindCounter, "phase", "reason")
	m.register(MetricReconnects, "Connection reconnect attempts.", kindCounter, "reason")
	m.register(MetricInfo, "Build and runtime metadata (constant 1).", kindGauge, "version", "backend", "worker_id")

	m.Set(MetricConnected, 0)
	m.Set(MetricTasksActive, 0)
	m.Set(MetricTasksMax, float64(maxConcurrent))
	m.Add(MetricTasksClaimed, 0)
	m.Add(MetricTasksRejected, 0, "at_capacity")
	for _, r := range []string{ResultSucceeded, ResultFailed, ResultCancelled, ResultDispatched} {
		m.Add(MetricTasksDone, 0, r)
	}
	for _, r := range []string{"dial_failed", "remote_close"} {
		m.Add(MetricReconnects, 0, r)
	}
	m.Set(MetricInfo, 1, version, backend, workerID)
	return m
}

// register declares a counter or gauge family.
func (m *Metrics) register(name, help string, kind metricKind, labelNames ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[name]; ok {
		return
	}
	f := &family{name: name, help: help, kind: kind, labelNames: labelNames, series: map[string]*series{}}
	m.families = append(m.families, f)
	m.byName[name] = f
}

// RegisterCounter declares a counter family.
func (m *Metrics) RegisterCounter(name, help string, labelNames ...string) {
	m.register(name, help, kindCounter, labelNames...)
}

// RegisterGauge declares a gauge family.
func (m *Metrics) RegisterGauge(name, help string, labelNames ...string) {
	m.register(name, help, kindGauge, labelNames...)
}

// RegisterHistogram declares a histogram family.
func (m *Metrics) RegisterHistogram(name, help string, buckets []float64, labelNames ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byName[name]; ok {
		return
	}
	b := append([]float64(nil), buckets...)
	sort.Float64s(b)
	f := &family{name: name, help: help, kind: kindHistogram, labelNames: labelNames, buckets: b, series: map[string]*series{}}
	m.families = append(m.families, f)
	m.byName[name] = f
}

func (m *Metrics) get(name string, labels []string) *series {
	f, ok := m.byName[name]
	if !ok {
		panic("worker: unknown metric " + name)
	}
	if len(labels) != len(f.labelNames) {
		panic(fmt.Sprintf("worker: metric %s expects %d labels, got %d", name, len(f.labelNames), len(labels)))
	}
	key := strings.Join(labels, "\xff")
	s, ok := f.series[key]
	if !ok {
		s = &series{labels: append([]string(nil), labels...)}
		if f.kind == kindHistogram {
			s.buckets = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Add increments a counter or gauge by v.
func (m *Metrics) Add(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.get(name, labels).value += v
}

// Inc increments by 1.
func (m *Metrics) Inc(name string, labels ...string) { m.Add(name, 1, labels...) }

// Set sets a gauge.
func (m *Metrics) Set(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.get(name, labels).value = v
}

// Observe records a histogram observation.
func (m *Metrics) Observe(name string, v float64, labels ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.byName[name]
	s := m.get(name, labels)
	for i, ub := range f.buckets {
		if v <= ub {
			s.buckets[i]++
			break
		}
	}
	s.count++
	s.sum += v
}

// Value returns the current value of a counter/gauge series (0 if absent).
func (m *Metrics) Value(name string, labels ...string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.byName[name]
	if !ok {
		return 0
	}
	if s, ok := f.series[strings.Join(labels, "\xff")]; ok {
		return s.value
	}
	return 0
}

// Convenience helpers for the worker catalog.

// SetConnected records the control-plane connection state.
func (m *Metrics) SetConnected(up bool) {
	if up {
		m.Set(MetricConnected, 1)
	} else {
		m.Set(MetricConnected, 0)
	}
}

// TaskClaimed records an accepted task and the new active count.
func (m *Metrics) TaskClaimed(active int) {
	m.Inc(MetricTasksClaimed)
	m.Set(MetricTasksActive, float64(active))
}

// TaskRejected records a declined task.
func (m *Metrics) TaskRejected(reason string) { m.Inc(MetricTasksRejected, reason) }

// TaskCompleted records a finished task.
func (m *Metrics) TaskCompleted(result string, d time.Duration, active int) {
	m.Inc(MetricTasksDone, result)
	m.Observe(MetricTaskDuration, d.Seconds(), result)
	m.Set(MetricTasksActive, float64(active))
}

// TaskFailed records a classified failure.
func (m *Metrics) TaskFailed(phase, reason string) { m.Inc(MetricTaskFailures, phase, reason) }

// Reconnect records a reconnect attempt.
func (m *Metrics) Reconnect(reason string) { m.Inc(MetricReconnects, reason) }

// WriteText renders all metrics in the Prometheus text exposition format.
func (m *Metrics) WriteText(w io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	for _, f := range m.families {
		typ := map[metricKind]string{kindCounter: "counter", kindGauge: "gauge", kindHistogram: "histogram"}[f.kind]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, typ)
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			if f.kind != kindHistogram {
				fmt.Fprintf(&b, "%s%s %s\n", f.name, labelString(f.labelNames, s.labels, "", ""), formatFloat(s.value))
				continue
			}
			var cum uint64
			for i, ub := range f.buckets {
				cum += s.buckets[i]
				fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelString(f.labelNames, s.labels, "le", formatFloat(ub)), cum)
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", f.name, labelString(f.labelNames, s.labels, "le", "+Inf"), s.count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", f.name, labelString(f.labelNames, s.labels, "", ""), formatFloat(s.sum))
			fmt.Fprintf(&b, "%s_count%s %d\n", f.name, labelString(f.labelNames, s.labels, "", ""), s.count)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// Handler serves WriteText (mount at /metrics).
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = m.WriteText(w)
	})
}

func labelString(names, values []string, extraName, extraValue string) string {
	if len(names) == 0 && extraName == "" {
		return ""
	}
	parts := make([]string, 0, len(names)+1)
	for i, n := range names {
		parts = append(parts, n+`="`+escapeLabel(values[i])+`"`)
	}
	if extraName != "" {
		parts = append(parts, extraName+`="`+extraValue+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
