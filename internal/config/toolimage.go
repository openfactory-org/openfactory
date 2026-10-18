package config

import (
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ToolImageFor returns the tool image of a harness CLI ("claude" or "codex").
// The result is empty when the task needs none.
//
// The setting OF_CLAUDE_TOOL_IMAGE or OF_CODEX_TOOL_IMAGE wins. Without it, on
// the kubernetes backend and on plugin backends, the name comes from
// OF_RUNNER_IMAGE: when its repository is named "openfactory" (the server
// image), the image is the repository "tool-claude" or "tool-codex" with the
// same registry and tag. A runner image that has a digest gives no derived
// name, because the tool image has another digest.
func (c *Config) ToolImageFor(harness string) string {
	var set string
	switch harness {
	case "claude":
		set = c.ClaudeToolImage
	case "codex":
		set = c.CodexToolImage
	default:
		return ""
	}
	if v := strings.TrimSpace(set); v != "" {
		return v
	}
	if c.SandboxBackend != BackendKubernetes && !c.IsPluginBackend() {
		return ""
	}
	return deriveToolImage(strings.TrimSpace(c.RunnerImage), "tool-"+harness)
}

// deriveToolImage replaces the repository name "openfactory" of an image reference
// with name. It returns "" when the repository has another name or the
// reference has a digest.
func deriveToolImage(runner, name string) string {
	if runner == "" || strings.Contains(runner, "@") {
		return ""
	}
	slash := strings.LastIndex(runner, "/")
	last := runner[slash+1:]
	repo, tag := last, ""
	if i := strings.Index(last, ":"); i >= 0 {
		repo, tag = last[:i], last[i:]
	}
	if repo != "openfactory" {
		return ""
	}
	return runner[:slash+1] + name + tag
}

// PreflightToolImages returns the tool images that the user set. The startup
// check of the kubernetes backend copies them like a real task does. It
// ignores the names that ToolImageFor derives: a derived image can be missing
// in a registry, and a missing image of a harness that nobody uses must not
// stop the server.
func (c *Config) PreflightToolImages() []core.TaskImage {
	var out []core.TaskImage
	for _, t := range []struct{ name, image string }{
		{"claude", c.ClaudeToolImage},
		{"codex", c.CodexToolImage},
	} {
		if v := strings.TrimSpace(t.image); v != "" {
			out = append(out, core.TaskImage{Name: t.name, Image: v})
		}
	}
	return out
}
