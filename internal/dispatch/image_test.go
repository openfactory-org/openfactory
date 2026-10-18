package dispatch

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func dispatcherWith(t *testing.T, imageFor func(*core.Run) string) *Dispatcher {
	t.Helper()
	e := newEnv(t)
	d, err := New(Config{
		Store: e.mem, Runner: e.mem, Backend: e.backend,
		ControlURL: "http://control.test", Image: "default-img", ImageFor: imageFor,
		Now: func() time.Time { return *e.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestBuildTaskImage(t *testing.T) {
	run := &core.Run{ID: "run_1", TeamID: "team"}
	tests := []struct {
		name     string
		imageFor func(*core.Run) string
		want     string
	}{
		{"no function", nil, "default-img"},
		{"empty result", func(*core.Run) string { return "" }, "default-img"},
		{"blank result", func(*core.Run) string { return "  " }, "default-img"},
		{"image of the run", func(r *core.Run) string { return "example.test/" + r.ID + ":1" }, "example.test/run_1:1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := dispatcherWith(t, tc.imageFor).buildTask(run, 1, "tok").Image; got != tc.want {
				t.Fatalf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildTaskImageIsPerRun(t *testing.T) {
	d := dispatcherWith(t, func(r *core.Run) string {
		if r.Config.EnvironmentID == "env_a" {
			return "example.test/a:1"
		}
		return ""
	})
	a := &core.Run{ID: "run_a", TeamID: "team", Config: core.AgentConfig{EnvironmentID: "env_a"}}
	b := &core.Run{ID: "run_b", TeamID: "team"}
	if got := d.buildTask(a, 1, "t").Image; got != "example.test/a:1" {
		t.Errorf("run a image = %q", got)
	}
	if got := d.buildTask(b, 1, "t").Image; got != "default-img" {
		t.Errorf("run b image = %q", got)
	}
}

func TestBuildTaskNetworkPolicy(t *testing.T) {
	d := dispatcherWith(t, nil)
	free := &core.Run{ID: "run_f", TeamID: "team"}
	if got := d.buildTask(free, 1, "t").NetworkPolicy; got != nil {
		t.Errorf("unrestricted run has policy %+v", got)
	}
	r := &core.Run{ID: "run_r", TeamID: "team", Config: core.AgentConfig{NetworkPolicy: core.EffectiveNetworkPolicy{
		Restricted: true, AllowedHosts: []string{"a.example"}, DeniedHosts: []string{"b.example"},
	}}}
	got := d.buildTask(r, 1, "t").NetworkPolicy
	want := &core.TaskNetworkPolicy{Mode: "restricted", AllowedHosts: []string{"a.example"}, DeniedHosts: []string{"b.example"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("policy = %+v, want %+v", got, want)
	}
	// The task does not share the slices of the run.
	got.AllowedHosts[0] = "changed"
	if r.Config.NetworkPolicy.AllowedHosts[0] != "a.example" {
		t.Error("the task shares the host list of the run")
	}
}

// A backend that refuses a task as invalid (for example a restricted policy
// that it cannot enforce) ends the run with invalid_configuration, without a retry.
func TestStartInvalidGivesInvalidConfiguration(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.backend.startErr = fmt.Errorf("k8stask: create network policy: %w: forbidden", core.ErrInvalid)
	e.tick()
	r := e.run(run.ID)
	if r.State != core.RunFailed || r.Error != string(core.FailureInvalidConfiguration) {
		t.Fatalf("run = state %s, error %q", r.State, r.Error)
	}
	if s := e.session(run.ID, 1); s.EndReason != "not_started" {
		t.Errorf("session = %+v", s)
	}
	if !errors.Is(e.backend.startErr, core.ErrInvalid) {
		t.Fatal("setup")
	}
}

func TestBuildTaskToolImages(t *testing.T) {
	e := newEnv(t)
	imagesFor := func(r *core.Run) []core.TaskImage {
		if r.Config.Harness.Type == core.HarnessClaude {
			return []core.TaskImage{{Name: "claude", Image: "example.test/tool-claude:1"}}
		}
		return nil
	}
	d, err := New(Config{
		Store: e.mem, Runner: e.mem, Backend: e.backend,
		ControlURL: "http://control.test", Image: "ubuntu:22.04", ToolImagesFor: imagesFor,
		Now: func() time.Time { return *e.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	claude := &core.Run{ID: "run_c", TeamID: "team", Config: core.AgentConfig{Harness: core.HarnessConfig{Type: core.HarnessClaude}}}
	got := d.buildTask(claude, 1, "t")
	want := []core.TaskImage{{Name: "claude", Image: "example.test/tool-claude:1"}}
	if !reflect.DeepEqual(got.ToolImages, want) {
		t.Errorf("claude tool images = %+v, want %+v", got.ToolImages, want)
	}
	if got.Image != "ubuntu:22.04" {
		t.Errorf("sandbox image = %q", got.Image)
	}
	open := &core.Run{ID: "run_o", TeamID: "team", Config: core.AgentConfig{Harness: core.HarnessConfig{Type: core.HarnessType("open")}}}
	if got := d.buildTask(open, 1, "t").ToolImages; len(got) != 0 {
		t.Errorf("unknown harness tool images = %+v", got)
	}
	// Without the function there is no tool image.
	if got := dispatcherWith(t, nil).buildTask(claude, 1, "t").ToolImages; got != nil {
		t.Errorf("tool images without a function = %+v", got)
	}
}

// A queued claude run reaches the backend with its tool images.
func TestStartedTaskHasToolImages(t *testing.T) {
	e := newEnv(t)
	d, err := New(Config{
		Store: e.mem, Runner: e.mem, Backend: e.backend,
		ControlURL: "http://control.test",
		ToolImagesFor: func(*core.Run) []core.TaskImage {
			return []core.TaskImage{{Name: "claude", Image: "example.test/tool-claude:1"}}
		},
		Now: func() time.Time { return *e.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.d = d
	e.newRun(core.RunQueued, "p")
	e.tick()
	tasks := e.backend.startedTasks()
	if len(tasks) != 1 {
		t.Fatalf("started %d tasks", len(tasks))
	}
	if got := tasks[0].ToolImages; len(got) != 1 || got[0].Name != "claude" {
		t.Errorf("tool images = %+v", got)
	}
}
