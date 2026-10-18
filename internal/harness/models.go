package harness

import (
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Model is one entry of a harness model catalog.
type Model struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Provider        string   `json:"provider"` // anthropic | openai
	Default         bool     `json:"default,omitempty"`
	ReasoningLevels []string `json:"reasoning_levels,omitempty"`
}

// Reasoning levels accepted per harness.
var (
	claudeReasoningLevels = []string{"low", "medium", "high", "xhigh", "max"}
	codexReasoningLevels  = []string{"minimal", "low", "medium", "high", "xhigh"}
)

func claudeCatalog() []Model {
	m := func(id, label string, def bool) Model {
		return Model{ID: id, Label: label, Provider: "anthropic", Default: def, ReasoningLevels: claudeReasoningLevels}
	}
	return []Model{
		m("claude-opus-5-5", "Claude Opus 5.5", true),
		m("claude-sonnet-5-5", "Claude Sonnet 5.5", false),
		m("claude-haiku-5-5", "Claude Haiku 5.5", false),
		m("claude-fable-5-1", "Claude Fable 5.1", false),
		m("claude-opus-5", "Claude Opus 5", false),
		m("claude-sonnet-5", "Claude Sonnet 5", false),
		m("claude-opus-4-8", "Claude Opus 4.8", false),
		m("claude-opus-4-7", "Claude Opus 4.7", false),
		m("claude-opus-4-6", "Claude Opus 4.6", false),
		m("claude-sonnet-4-6", "Claude Sonnet 4.6", false),
		m("claude-haiku-4-5", "Claude Haiku 4.5", false),
		m("opus", "Opus (latest alias)", false),
		m("sonnet", "Sonnet (latest alias)", false),
		m("haiku", "Haiku (latest alias)", false),
	}
}

func codexCatalog() []Model {
	m := func(id, label string, def bool) Model {
		return Model{ID: id, Label: label, Provider: "openai", Default: def, ReasoningLevels: codexReasoningLevels}
	}
	return []Model{
		m("gpt-5.3-codex", "GPT-5.3 Codex", true),
		m("gpt-5.2-codex", "GPT-5.2 Codex", false),
		m("gpt-5.1-codex-max", "GPT-5.1 Codex Max", false),
		m("gpt-5.1-codex-mini", "GPT-5.1 Codex Mini", false),
		m("gpt-5.2", "GPT-5.2", false),
		m("gpt-5.1", "GPT-5.1", false),
		m("gpt-5-codex", "GPT-5 Codex", false),
		m("gpt-5", "GPT-5", false),
	}
}

// Models returns the model catalog for a harness type (aliases accepted).
// The catalogs are advisory: harnesses pass any non-empty model through to the
// underlying CLI.
func Models(t core.HarnessType) []Model {
	switch NormalizeType(string(t)) {
	case core.HarnessClaude:
		return claudeCatalog()
	case core.HarnessCodex:
		return codexCatalog()
	}
	return nil
}

// DefaultModel returns the catalog default for t.
func DefaultModel(t core.HarnessType) string {
	for _, m := range Models(t) {
		if m.Default {
			return m.ID
		}
	}
	return ""
}

// ReasoningLevels returns the accepted reasoningLevel values for t (nil for
// an unknown harness).
func ReasoningLevels(t core.HarnessType) []string {
	switch NormalizeType(string(t)) {
	case core.HarnessClaude:
		return append([]string(nil), claudeReasoningLevels...)
	case core.HarnessCodex:
		return append([]string(nil), codexReasoningLevels...)
	}
	return nil
}

// ValidateReasoningLevel checks a reasoningLevel against the harness. An
// unknown harness rejects any value.
func ValidateReasoningLevel(t core.HarnessType, level string) error {
	if level == "" {
		return nil
	}
	levels := ReasoningLevels(t)
	if levels == nil {
		return fmt.Errorf("harness %q does not accept reasoningLevel", NormalizeType(string(t)))
	}
	for _, l := range levels {
		if strings.EqualFold(l, level) {
			return nil
		}
	}
	return fmt.Errorf("invalid reasoningLevel %q for harness %q (allowed: %s)", level, NormalizeType(string(t)), strings.Join(levels, ", "))
}

// HarnessForModel selects the harness of a model name, for the shorthand
// `model: X` of a definition. A Claude model name selects claude. An OpenAI
// model name selects codex. The result is "" for any other name.
func HarnessForModel(model string) core.HarnessType {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "anthropic.")
	switch {
	case strings.HasPrefix(m, "claude-"), m == "opus", m == "sonnet", m == "haiku":
		return core.HarnessClaude
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "codex-"), strings.HasPrefix(m, "chatgpt-"):
		return core.HarnessCodex
	case len(m) >= 2 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9':
		// o-series reasoning models: o3, o4-mini, ...
		return core.HarnessCodex
	}
	return ""
}
