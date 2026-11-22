package worker

import (
	"strings"
	"testing"
	"time"
)

func TestMetricsText(t *testing.T) {
	m := NewMetrics("w1", "docker", "v1", 4)
	m.SetConnected(true)
	m.TaskClaimed(1)
	m.TaskRejected("at_capacity")
	m.TaskCompleted(ResultSucceeded, 42*time.Second, 0)
	m.TaskFailed("setup", "image_pull")
	m.Reconnect("dial_failed")
	var b strings.Builder
	if err := m.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"# TYPE of_worker_connected gauge\nof_worker_connected 1\n",
		"of_worker_sandboxes_max_concurrent 4\n",
		"of_worker_sandboxes_claimed_total 1\n",
		`of_worker_sandboxes_rejected_total{reason="at_capacity"} 1`,
		`of_worker_sandboxes_completed_total{result="succeeded"} 1`,
		`of_worker_sandboxes_completed_total{result="failed"} 0`,
		`of_worker_sandbox_duration_seconds_bucket{result="succeeded",le="30"} 0`,
		`of_worker_sandbox_duration_seconds_bucket{result="succeeded",le="60"} 1`,
		`of_worker_sandbox_duration_seconds_bucket{result="succeeded",le="+Inf"} 1`,
		`of_worker_sandbox_duration_seconds_sum{result="succeeded"} 42`,
		`of_worker_sandbox_failures_total{phase="setup",reason="image_pull"} 1`,
		`of_worker_websocket_reconnects_total{reason="dial_failed"} 1`,
		`of_worker_info{version="v1",backend="docker",worker_id="w1"} 1`,
		"# TYPE of_worker_sandbox_duration_seconds histogram",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if m.Value(MetricTasksClaimed) != 1 {
		t.Fatal("Value")
	}
}
