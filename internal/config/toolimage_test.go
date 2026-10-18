package config

import (
	"reflect"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestToolImageFor(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		harness string
		want    string
	}{
		{"setting wins", Config{SandboxBackend: BackendKubernetes, RunnerImage: "r.test/openfactory:1", ClaudeToolImage: "m.test/cli:2"}, "claude", "m.test/cli:2"},
		{"setting on local backend", Config{SandboxBackend: BackendLocal, CodexToolImage: "m.test/codex:2"}, "codex", "m.test/codex:2"},
		{"derived claude", Config{SandboxBackend: BackendKubernetes, RunnerImage: "ghcr.io/openfactory-org/openfactory:7e64762-dirty"}, "claude", "ghcr.io/openfactory-org/tool-claude:7e64762-dirty"},
		{"derived codex", Config{SandboxBackend: BackendKubernetes, RunnerImage: "r.test:5000/of/openfactory:1"}, "codex", "r.test:5000/of/tool-codex:1"},
		{"derived without tag", Config{SandboxBackend: BackendKubernetes, RunnerImage: "r.test/openfactory"}, "claude", "r.test/tool-claude"},
		{"derived for a plugin backend", Config{SandboxBackend: "myplugin", RunnerImage: "r.test/openfactory:1"}, "claude", "r.test/tool-claude:1"},
		{"no derived name on the local backend", Config{SandboxBackend: BackendLocal, RunnerImage: "r.test/openfactory:1"}, "claude", ""},
		{"no derived name for a digest", Config{SandboxBackend: BackendKubernetes, RunnerImage: "r.test/openfactory@sha256:abc"}, "claude", ""},
		{"no derived name for another repository", Config{SandboxBackend: BackendKubernetes, RunnerImage: "r.test/my-runner:1"}, "claude", ""},
		{"no runner image", Config{SandboxBackend: BackendKubernetes}, "claude", ""},
		{"unknown harness", Config{SandboxBackend: BackendKubernetes, RunnerImage: "r.test/openfactory:1", ClaudeToolImage: "x"}, "open", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ToolImageFor(tc.harness); got != tc.want {
				t.Errorf("ToolImageFor(%q) = %q, want %q", tc.harness, got, tc.want)
			}
		})
	}
}

func TestPreflightToolImages(t *testing.T) {
	c := Config{SandboxBackend: BackendKubernetes, RunnerImage: "r.test/openfactory:1"}
	if got := c.PreflightToolImages(); got != nil {
		t.Errorf("derived names are in the preflight: %+v", got)
	}
	c.ClaudeToolImage = " m.test/cli:2 "
	want := []core.TaskImage{{Name: "claude", Image: "m.test/cli:2"}}
	if got := c.PreflightToolImages(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestToolImageSettingsFromEnv(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{
		"OF_DEV": "1", "OF_DATA_DIR": t.TempDir(),
		"OF_CLAUDE_TOOL_IMAGE": "m.test/claude:1", "OF_CODEX_TOOL_IMAGE": "m.test/codex:1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ClaudeToolImage != "m.test/claude:1" || c.CodexToolImage != "m.test/codex:1" {
		t.Errorf("got %q %q", c.ClaudeToolImage, c.CodexToolImage)
	}
}

func TestDefaultRunnerImage(t *testing.T) {
	for _, tc := range []struct {
		backend, set, want string
	}{
		{BackendKubernetes, "", DefaultRunnerImage},
		{"myplugin", "", DefaultRunnerImage},
		{BackendKubernetes, " r.test/openfactory:1 ", "r.test/openfactory:1"},
		{BackendLocal, "", ""},
	} {
		vars := map[string]string{"OF_DEV": "1", "OF_DATA_DIR": t.TempDir(), "OF_SANDBOX_BACKEND": tc.backend}
		if tc.set != "" {
			vars["OF_RUNNER_IMAGE"] = tc.set
		}
		c, err := LoadFrom(nil, env(vars))
		if c == nil {
			t.Fatalf("%s: %v", tc.backend, err)
		}
		if c.RunnerImage != tc.want {
			t.Errorf("%s %q: runner image %q, want %q", tc.backend, tc.set, c.RunnerImage, tc.want)
		}
	}
	if got := (&Config{SandboxBackend: BackendKubernetes, RunnerImage: DefaultRunnerImage}).ToolImageFor("claude"); got != "ghcr.io/openfactory-org/tool-claude:latest" {
		t.Errorf("tool image of the default runner image: %q", got)
	}
}
