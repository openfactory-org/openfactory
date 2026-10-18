package server

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// The image of a task follows the precedence of sandbox.SelectImage: environment
// image, then machine image, then the default (an empty result).
func TestTaskImageFor(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	if err := st.CreateEnvironment(ctx, &core.Environment{ID: "env_img", TeamID: "team", Name: "a", Image: " example.test/env:1 "}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateEnvironment(ctx, &core.Environment{ID: "env_plain", TeamID: "team", Name: "b"}); err != nil {
		t.Fatal(err)
	}
	f := taskImageFor(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	machine := &core.MachineSpec{Image: "example.test/machine:1"}
	tests := []struct {
		name string
		cfg  core.AgentConfig
		want string
	}{
		{"nothing set", core.AgentConfig{}, ""},
		{"machine image", core.AgentConfig{Machine: machine}, "example.test/machine:1"},
		{"environment beats machine", core.AgentConfig{Machine: machine, EnvironmentID: "env_img"}, "example.test/env:1"},
		{"environment without image", core.AgentConfig{Machine: machine, EnvironmentID: "env_plain"}, "example.test/machine:1"},
		{"environment without image or machine", core.AgentConfig{EnvironmentID: "env_plain"}, ""},
		{"missing environment", core.AgentConfig{Machine: machine, EnvironmentID: "env_gone"}, "example.test/machine:1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := f(&core.Run{ID: "run_1", Config: tc.cfg}); got != tc.want {
				t.Fatalf("image = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolImagesFor(t *testing.T) {
	cfg := &config.Config{SandboxBackend: config.BackendKubernetes, RunnerImage: "registry.test/of/openfactory:1.2"}
	f := toolImagesFor(cfg)
	run := func(h core.HarnessType) *core.Run {
		return &core.Run{Config: core.AgentConfig{Harness: core.HarnessConfig{Type: h}}}
	}
	want := []core.TaskImage{{Name: "claude", Image: "registry.test/of/tool-claude:1.2"}}
	if got := f(run(core.HarnessClaude)); !reflect.DeepEqual(got, want) {
		t.Errorf("claude = %+v, want %+v", got, want)
	}
	want = []core.TaskImage{{Name: "codex", Image: "registry.test/of/tool-codex:1.2"}}
	if got := f(run(core.HarnessCodex)); !reflect.DeepEqual(got, want) {
		t.Errorf("codex = %+v, want %+v", got, want)
	}
	if got := f(run(core.HarnessType("open"))); got != nil {
		t.Errorf("open = %+v", got)
	}
	cfg.ClaudeToolImage = "mirror.test/cli:7"
	if got := f(run(core.HarnessClaude)); len(got) != 1 || got[0].Image != "mirror.test/cli:7" {
		t.Errorf("setting = %+v", got)
	}
}
