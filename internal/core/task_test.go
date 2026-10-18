package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func validTask() Task {
	return Task{
		TaskID:   "run_abc.2",
		RunID:    "run_abc",
		Session:  2,
		TeamID:   "team_1",
		Image:    "example.com/build:1.4",
		Platform: TaskPlatform{OS: "linux", Arch: "amd64"},
		Env: map[string]string{
			TaskEnvControlURL:     "https://factory.internal",
			TaskEnvRunID:          "run_abc",
			TaskEnvSession:        "2",
			TaskEnvBootstrapToken: "ofb_secret_value",
		},
		ToolImages:        []TaskImage{{Name: "claude", Image: "example.com/cli:1"}},
		Labels:            map[string]string{"openfactory.dev/run": "run_abc"},
		MaxSessionSeconds: 28800,
	}
}

func TestTaskID(t *testing.T) {
	if got := TaskID("run_abc", 2); got != "run_abc.2" {
		t.Fatalf("got %q", got)
	}
	for _, c := range []struct {
		run     string
		session int
	}{{"run_abc", 2}, {"run.x", 7}} {
		id := TaskID(c.run, c.session)
		run, s, err := ParseTaskID(id)
		if err != nil || run != c.run || s != c.session {
			t.Fatalf("round trip %q: got (%q, %d, %v)", id, run, s, err)
		}
	}
	for _, bad := range []string{"", "run_abc", ".2", "run_abc.0", "run_abc.x", "run_abc.-1"} {
		t.Run("bad_"+bad, func(t *testing.T) {
			if _, _, err := ParseTaskID(bad); err == nil {
				t.Fatalf("no error for %q", bad)
			}
		})
	}
}

func TestTaskValidate(t *testing.T) {
	if err := validTask().Validate(); err != nil {
		t.Fatalf("valid task: %v", err)
	}
	tests := []struct {
		name   string
		field  string
		mutate func(*Task)
	}{
		{"run id", "run_id", func(x *Task) { x.RunID = "" }},
		{"session", "session", func(x *Task) { x.Session = 0 }},
		{"task id", "task_id", func(x *Task) { x.TaskID = "other.2" }},
		{"team id", "team_id", func(x *Task) { x.TeamID = "" }},
		{"image", "image", func(x *Task) { x.Image = "" }},
		{"os", "platform.os", func(x *Task) { x.Platform.OS = "windows" }},
		{"arch", "platform.arch", func(x *Task) { x.Platform.Arch = "riscv" }},
		{"env control url", TaskEnvControlURL, func(x *Task) { x.Env[TaskEnvControlURL] = "" }},
		{"env run id missing", TaskEnvRunID, func(x *Task) { delete(x.Env, TaskEnvRunID) }},
		{"env session", TaskEnvSession, func(x *Task) { x.Env[TaskEnvSession] = "" }},
		{"env token", TaskEnvBootstrapToken, func(x *Task) { delete(x.Env, TaskEnvBootstrapToken) }},
		{"env run id mismatch", TaskEnvRunID, func(x *Task) { x.Env[TaskEnvRunID] = "run_zzz" }},
		{"env session mismatch", TaskEnvSession, func(x *Task) { x.Env[TaskEnvSession] = "3" }},
		{"max session", "max_session_seconds", func(x *Task) { x.MaxSessionSeconds = 0 }},
		{"retention", "failure_retention_seconds", func(x *Task) { x.FailureRetentionSeconds = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := validTask()
			tt.mutate(&task)
			err := task.Validate()
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("error %q does not name %q", err, tt.field)
			}
		})
	}
}

func TestTaskRedacted(t *testing.T) {
	orig := validTask()
	cp := orig.Redacted()
	if cp.Env[TaskEnvBootstrapToken] != "***" {
		t.Fatalf("copy token = %q", cp.Env[TaskEnvBootstrapToken])
	}
	if orig.Env[TaskEnvBootstrapToken] != "ofb_secret_value" {
		t.Fatal("original token changed")
	}
	cp.Env[TaskEnvRunID] = "changed"
	cp.Labels["x"] = "y"
	cp.ToolImages[0].Name = "changed"
	if orig.Env[TaskEnvRunID] != "run_abc" || len(orig.Labels) != 1 || orig.ToolImages[0].Name != "claude" {
		t.Fatal("change to copy reached original")
	}
	// A task without a token key stays without it.
	empty := Task{Env: map[string]string{}}.Redacted()
	if _, ok := empty.Env[TaskEnvBootstrapToken]; ok {
		t.Fatal("token key added")
	}
}

func TestTaskJSON(t *testing.T) {
	task := validTask()
	task.Env[TaskEnvBootstrapToken] = "ofb_tok"
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"task_id", "run_id", "session", "team_id", "image", "platform", "resources", "env", "max_session_seconds", "failure_retention_seconds"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("JSON contains secret: %s", b)
	}
}

func TestTaskStatusJSON(t *testing.T) {
	b, err := json.Marshal(TaskStatus{TaskID: "run_abc.1", State: TaskRunning})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "exit_code") {
		t.Fatalf("nil exit code present: %s", b)
	}
	zero := 0
	b, err = json.Marshal(TaskStatus{TaskID: "run_abc.1", State: TaskExited, ExitCode: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"exit_code":0`) {
		t.Fatalf("exit code 0 missing: %s", b)
	}
	if !(TaskStatus{State: TaskExited}).Exited() || (TaskStatus{State: TaskRunning}).Exited() {
		t.Fatal("Exited wrong")
	}
}
