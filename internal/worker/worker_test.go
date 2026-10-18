package worker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestMetadataEnv(t *testing.T) {
	env, err := MetadataEnv(map[string]string{"setup_profile": "backend", "team.name": "x", "ticket-id": "ENG-1"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"OF_METADATA_SETUP_PROFILE": "backend", "OF_METADATA_TEAM_NAME": "x", "OF_METADATA_TICKET_ID": "ENG-1"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func TestValidateMetadata(t *testing.T) {
	many := map[string]string{}
	for i := 0; i < 21; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	tests := []struct {
		name string
		meta map[string]string
		ok   bool
	}{
		{"empty", nil, true},
		{"valid", map[string]string{"a.b-c_D9": strings.Repeat("v", 256)}, true},
		{"empty value", map[string]string{"k": ""}, true},
		{"too many", many, false},
		{"empty key", map[string]string{"": "v"}, false},
		{"long key", map[string]string{strings.Repeat("k", 65): "v"}, false},
		{"bad char", map[string]string{"a b": "v"}, false},
		{"long value", map[string]string{"k": strings.Repeat("v", 257)}, false},
		{"nul", map[string]string{"k": "a\x00b"}, false},
		{"utf8", map[string]string{"k": "\xff"}, false},
		{"collision", map[string]string{"a.b": "1", "a-b": "2"}, false},
		{"case collision", map[string]string{"Key": "1", "KEY": "2"}, false},
	}
	for _, tt := range tests {
		err := ValidateMetadata(tt.meta)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok=%v", tt.name, err, tt.ok)
		}
		if err != nil && !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%s: not ErrInvalid", tt.name)
		}
	}
}

func TestMergeMetadata(t *testing.T) {
	got, err := MergeMetadata(
		map[string]string{"a": "parent", "b": "parent", "c": "parent"},
		map[string]string{"b": "batch", "c": "batch"},
		map[string]string{"c": "child"},
	)
	if err != nil || got["a"] != "parent" || got["b"] != "batch" || got["c"] != "child" {
		t.Fatalf("merge = %v %v", got, err)
	}
	parent := map[string]string{}
	for i := 0; i < 20; i++ {
		parent[strings.Repeat("p", i+1)] = "v"
	}
	if _, err := MergeMetadata(parent, nil, map[string]string{"extra": "v"}); err == nil {
		t.Fatal("limit must include inherited keys")
	}
	if err := CheckNoMetadataEnv(map[string]string{"of_metadata_x": "1"}); err == nil {
		t.Fatal("reserved prefix accepted")
	}
}

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
