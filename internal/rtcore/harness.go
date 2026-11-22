package rtcore

import "context"

// HarnessType names a harness adapter.
type HarnessType string

const (
	HarnessClaude HarnessType = "claude"
	HarnessCodex  HarnessType = "codex"
)

// HarnessAuth names where the credential of a harness comes from.
type HarnessAuth struct {
	Source     string `json:"source" yaml:"source"` // managedSecret | workerEnvironment
	SecretName string `json:"secretName,omitempty" yaml:"secretName,omitempty"`
}

// HarnessConfig is the harness part of an agent configuration.
type HarnessConfig struct {
	Type           HarnessType  `json:"type"`
	Model          string       `json:"model"`
	ReasoningLevel string       `json:"reasoningLevel,omitempty"`
	Auth           *HarnessAuth `json:"auth,omitempty"`
}

// MCPServer is one MCP server transport. Exactly one of ManagedID/Command/URL is set.
type MCPServer struct {
	ManagedID string            `json:"managedId,omitempty" yaml:"managedId,omitempty"`
	Command   string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args      []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	// AllowWrite enables the write tools of a built-in integration server
	// (managedId "builtin/<provider>"). Valid only with such a managedId.
	AllowWrite bool `json:"allowWrite,omitempty" yaml:"allowWrite,omitempty"`
}

// Skill is one skill of an agent.
type Skill struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Path        string            `json:"path"`            // path inside the definition tree
	Content     string            `json:"content"`         // full SKILL.md
	Files       map[string]string `json:"files,omitempty"` // supporting files, relative to skill dir
	Scope       string            `json:"scope"`           // "factory" or agent name
}

// Usage is the token, cost and time accounting of a run, a session or one
// harness turn. DurationMS is agent time: the sum of the turn durations.
type Usage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
	// Coverage is empty when the counts are complete, or UsagePartial when
	// at least one turn ended before its harness reported totals (for
	// example a cancelled turn). Partial counts are estimates.
	Coverage string `json:"coverage,omitempty"`
}

// Usage coverage values.
const (
	UsageKnown   = "known"
	UsagePartial = "partial"
)

// CoverageLabel returns the coverage for the API: UsageKnown when Coverage
// is empty.
func (u Usage) CoverageLabel() string {
	if u.Coverage == "" {
		return UsageKnown
	}
	return u.Coverage
}

// HarnessRequest is everything a harness needs to run one turn of an agent session.
type HarnessRequest struct {
	// RunID is the ID of the run.
	RunID string
	// SessionID is the harness session to continue when Resume is set.
	SessionID string
	// Harness is the harness configuration of the agent: type, model,
	// reasoning level and the source of its credential.
	Harness      HarnessConfig
	Prompt       string               // user message for this turn (initial prompt or follow-up)
	SystemPrompt string               // composed instructions (agent instructions + role preamble + skills index)
	Resume       bool                 // continue SessionID
	WorkDir      string               // path inside the sandbox
	Env          map[string]string    // includes OF_METADATA_*, secrets materialized as env when requested
	MCPServers   map[string]MCPServer // already includes the orchestration MCP server
	Skills       []Skill
	Auth         map[string]string // resolved credentials (e.g. ANTHROPIC_API_KEY)
}

// HarnessEvent is streamed while the harness runs.
type HarnessEvent struct {
	Kind      string // message, tool_call, tool_result, log, usage, session, done, error
	Role      string
	Content   string
	SessionID string
	Usage     *Usage
	Data      map[string]any
}

// Harness runs agent turns inside a sandbox.
type Harness interface {
	Type() HarnessType
	// Run executes a single turn and streams events; it must emit a final "done" or "error" event and close the channel.
	Run(ctx context.Context, sb Sandbox, req HarnessRequest) (<-chan HarnessEvent, error)
}
