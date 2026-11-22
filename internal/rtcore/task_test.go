package rtcore

import (
	"strings"
	"testing"
)

func validTask() Task {
	return Task{
		TaskID: "run_a.1", RunID: "run_a", Session: 1, TeamID: "team", Image: "img",
		Platform: TaskPlatform{OS: "linux", Arch: "amd64"},
		Env: map[string]string{
			TaskEnvControlURL: "https://cp.example", TaskEnvRunID: "run_a",
			TaskEnvSession: "1", TaskEnvBootstrapToken: "secret",
		},
		MaxSessionSeconds: 60,
	}
}

func TestTaskValidate(t *testing.T) {
	if err := validTask().Validate(); err != nil {
		t.Fatalf("valid task: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Task)
		want   string
	}{
		{"no run", func(t *Task) { t.RunID = "" }, "run_id"},
		{"bad id", func(t *Task) { t.TaskID = "x" }, "task_id"},
		{"os", func(t *Task) { t.Platform.OS = "darwin" }, "platform.os"},
		{"arch", func(t *Task) { t.Platform.Arch = "386" }, "platform.arch"},
		{"token", func(t *Task) { delete(t.Env, TaskEnvBootstrapToken) }, TaskEnvBootstrapToken},
		{"max", func(t *Task) { t.MaxSessionSeconds = 0 }, "max_session_seconds"},
	}
	for _, tt := range tests {
		task := validTask()
		tt.mutate(&task)
		err := task.Validate()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to name %q", tt.name, err, tt.want)
		}
	}
}

func TestTaskIDRoundTrip(t *testing.T) {
	run, n, err := ParseTaskID(TaskID("run_x.y", 3))
	if err != nil || run != "run_x.y" || n != 3 {
		t.Fatalf("ParseTaskID = %q %d %v", run, n, err)
	}
	for _, bad := range []string{"run", ".1", "run.0", "run.x"} {
		if _, _, err := ParseTaskID(bad); err == nil {
			t.Errorf("ParseTaskID(%q) succeeded", bad)
		}
	}
}

func TestRedacted(t *testing.T) {
	task := validTask()
	r := task.Redacted()
	if r.Env[TaskEnvBootstrapToken] != "***" || task.Env[TaskEnvBootstrapToken] != "secret" {
		t.Fatalf("Redacted: %v / %v", r.Env, task.Env)
	}
}

func TestRestricted(t *testing.T) {
	if Restricted(nil) || Restricted(&TaskNetworkPolicy{Mode: ModeUnrestricted}) {
		t.Error("unrestricted policy reported as restricted")
	}
	if !Restricted(&TaskNetworkPolicy{}) || !Restricted(&TaskNetworkPolicy{Mode: "restricted"}) {
		t.Error("restricted policy reported as unrestricted")
	}
}
