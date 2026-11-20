// Package claude implements the Claude Code harness: it runs the `claude` CLI in print
// mode inside the run's sandbox and translates its stream-json output into core.HarnessEvents.
//
// Invocation (one process per turn):
//
//	claude -p --output-format stream-json --verbose --input-format text
//	       [--resume <session>] [--model <m>] [--effort <level>]
//	       [--append-system-prompt <s> | --append-system-prompt-file <f>]
//	       [--mcp-config <file> --strict-mcp-config]
//	       --permission-mode bypassPermissions [--max-turns N]
//
// The prompt is written to stdin. Sandboxes are disposable, so permission prompts are
// bypassed; IS_SANDBOX=1 lets the CLI accept the bypass when running as root.
//
// Reasoning level: Claude Code exposes `--effort low|medium|high|xhigh|max`, which maps 1:1 to
// our reasoningLevel values. With Options.ThinkingTokensEnv the level is instead mapped to
// MAX_THINKING_TOKENS (low 4096, medium 10000, high 31999, xhigh/max 63999) for older CLI builds.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
)

// Files written into the sandbox (relative to the work dir).
const (
	MCPConfigPath    = harness.StateDir + "/claude/mcp.json"
	SystemPromptPath = harness.StateDir + "/claude/system-prompt.md"
	// inlineSystemPromptLimit keeps argv well below Linux's per-argument limit (128 KiB).
	inlineSystemPromptLimit = 64 << 10
)

// Options configures the harness.
type Options struct {
	// Binary is the CLI executable (default "claude").
	Binary string
	// MaxTurns, when > 0, passes --max-turns.
	MaxTurns int
	// PermissionMode passed via --permission-mode (default "bypassPermissions").
	PermissionMode string
	// DangerouslySkipPermissions uses --dangerously-skip-permissions instead of --permission-mode.
	DangerouslySkipPermissions bool
	// ThinkingTokensEnv maps reasoningLevel to MAX_THINKING_TOKENS instead of --effort.
	ThinkingTokensEnv bool
	// DisableRepoSkills skips writing skills to .claude/skills in the work dir.
	DisableRepoSkills bool
	// ExtraArgs are appended to the command line.
	ExtraArgs []string
	// ExtraEnv is merged last into the process environment.
	ExtraEnv map[string]string
	// Timeout bounds one turn (0 = no limit beyond ctx).
	Timeout time.Duration
	Logger  *slog.Logger
}

// Harness is the Claude Code harness.
type Harness struct{ opts Options }

// New returns a Claude Code harness.
func New(opts Options) *Harness {
	if opts.Binary == "" {
		opts.Binary = "claude"
	}
	if opts.PermissionMode == "" {
		opts.PermissionMode = "bypassPermissions"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Harness{opts: opts}
}

// Type implements core.Harness.
func (h *Harness) Type() core.HarnessType { return core.HarnessClaude }

// ResolveModel maps a configured model to a CLI model name. "" and
// "default" select the CLI default. Other names pass through unchanged.
func ResolveModel(model string) string {
	m := strings.TrimSpace(model)
	if strings.EqualFold(m, "default") {
		return ""
	}
	return m
}

func thinkingTokens(level string) string {
	switch level {
	case "low":
		return "4096"
	case "medium":
		return "10000"
	case "high":
		return "31999"
	default:
		return "63999"
	}
}

// BuildArgs returns the argv for one turn (exported for tests and diagnostics).
func (h *Harness) BuildArgs(req core.HarnessRequest, systemPromptFile string, mcpConfigFile string) ([]string, map[string]string) {
	args := []string{h.opts.Binary, "-p", "--output-format", "stream-json", "--verbose", "--input-format", "text"}
	env := map[string]string{}
	var cfg core.HarnessConfig
	if req.Run != nil {
		cfg = req.Run.Config.Harness
	}
	if req.Resume && req.Run != nil && req.Run.SessionID != "" {
		args = append(args, "--resume", req.Run.SessionID)
	}
	if model := ResolveModel(cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	if effort := strings.ToLower(cfg.ReasoningLevel); effort != "" {
		if h.opts.ThinkingTokensEnv {
			env["MAX_THINKING_TOKENS"] = thinkingTokens(effort)
		} else {
			args = append(args, "--effort", effort)
		}
	}
	if systemPromptFile != "" {
		args = append(args, "--append-system-prompt-file", systemPromptFile)
	} else if sp := strings.TrimSpace(req.SystemPrompt); sp != "" {
		args = append(args, "--append-system-prompt", sp)
	}
	if mcpConfigFile != "" {
		args = append(args, "--mcp-config", mcpConfigFile, "--strict-mcp-config")
	}
	if h.opts.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--permission-mode", h.opts.PermissionMode)
	}
	if h.opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(h.opts.MaxTurns))
	}
	args = append(args, h.opts.ExtraArgs...)
	return args, env
}

// Run implements core.Harness.
func (h *Harness) Run(ctx context.Context, sb core.Sandbox, req core.HarnessRequest) (<-chan core.HarnessEvent, error) {
	workDir := harness.WorkDir(sb, req)
	authEnv, err := harness.ClaudeAuthEnv(req.Auth)
	if err != nil {
		return nil, fmt.Errorf("claude auth: %w", err)
	}
	if err := harness.EnsureStateDir(ctx, sb, workDir); err != nil {
		return nil, fmt.Errorf("prepare sandbox: %w", err)
	}
	// The CLI reads CLAUDE.md and .claude/skills of the work directory
	// itself. The other rule files and skill directories of the
	// repositories are added here, so each harness gets the same
	// instructions.
	repo := harness.DiscoverRepo(ctx, sb, workDir)
	var repoSkills []core.Skill
	for _, s := range repo.Skills {
		if !strings.HasPrefix(s.Path, harness.ClaudeSkillsDir+"/") {
			repoSkills = append(repoSkills, s)
		}
	}
	req.Skills = harness.MergeRepoSkills(req.Skills, repoSkills)
	if instr := harness.RepoInstructions(repo.Rules, harness.RootFile("CLAUDE.md")); instr != "" {
		req.SystemPrompt = strings.TrimSpace(req.SystemPrompt + "\n\n" + instr)
	}
	if len(req.Skills) > 0 {
		if _, err := harness.WriteSkills(ctx, sb, workDir, req.Skills, harness.SkillWriteOptions{}); err != nil {
			return nil, err
		}
		if !h.opts.DisableRepoSkills {
			if _, err := harness.WriteSkills(ctx, sb, workDir, req.Skills, harness.SkillWriteOptions{
				Dirs: []string{harness.ClaudeSkillsDir}, SkipExisting: true, IgnoreInGit: true,
			}); err != nil {
				return nil, err
			}
		}
	}
	var logs []string
	mcpFile := ""
	if len(req.MCPServers) > 0 {
		cfg, warnings := harness.ClaudeMCPConfig(req.MCPServers)
		logs = append(logs, warnings...)
		mcpFile = harness.ResolvePath(workDir, MCPConfigPath)
		if err := harness.WriteFile(ctx, sb, mcpFile, cfg, 0o600); err != nil {
			return nil, fmt.Errorf("write mcp config: %w", err)
		}
	}
	system := strings.TrimSpace(req.SystemPrompt)
	if len(req.Skills) > 0 {
		system += "\n\nSkill files are also available under `" + harness.SkillsDir + "/<name>/SKILL.md` (with supporting files)."
		system = strings.TrimSpace(system)
	}
	spFile := ""
	if len(system) > inlineSystemPromptLimit {
		spFile = harness.ResolvePath(workDir, SystemPromptPath)
		if err := harness.WriteFile(ctx, sb, spFile, []byte(system), 0o600); err != nil {
			return nil, fmt.Errorf("write system prompt: %w", err)
		}
	}
	r := req
	r.SystemPrompt = system
	args, extraEnv := h.BuildArgs(r, spFile, mcpFile)
	env := harness.MergeEnv(req.Env, authEnv, map[string]string{
		"IS_SANDBOX":          "1",
		"DISABLE_AUTOUPDATER": "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}, extraEnv, h.opts.ExtraEnv)

	em, ch := harness.NewEmitter(ctx)
	go func() {
		defer em.Close()
		for _, l := range logs {
			em.Log("%s", l)
		}
		runCtx := ctx
		if h.opts.Timeout > 0 {
			var cancel context.CancelFunc
			runCtx, cancel = context.WithTimeout(ctx, h.opts.Timeout)
			defer cancel()
		}
		p := newParser()
		p.hint = harness.CredentialHint(req)
		start := time.Now()
		res, err := harness.StreamLines(runCtx, sb, core.ExecRequest{Cmd: args, Dir: workDir, Env: env}, req.Prompt, func(line []byte) {
			for _, ev := range p.handle(line) {
				em.Emit(ev)
			}
		})
		if p.finished {
			return
		}
		// The turn ended without a result message (cancelled, crashed or
		// timed out). Report the tokens of the assistant messages so far.
		if u := p.partialUsage(); u != nil {
			em.Emit(core.HarnessEvent{Kind: harness.EventUsage, Usage: u, SessionID: p.sessionID})
		}
		msg := ""
		switch {
		case err != nil:
			msg = fmt.Sprintf("claude: %v", err)
		case res.ExitCode != 0:
			msg = fmt.Sprintf("claude exited with code %d", res.ExitCode)
		default:
			msg = "claude exited without a result"
		}
		if tail := harness.LastLines(res.Stderr, 20); tail != "" {
			msg += ": " + tail
		}
		msg += harness.CredentialHint(req)
		h.opts.Logger.Warn("claude harness failed", "sandbox", sb.ID(), "error", msg, "elapsed", time.Since(start))
		em.Emit(core.HarnessEvent{Kind: harness.EventError, Content: msg, SessionID: p.sessionID})
	}()
	return ch, nil
}

// parser converts stream-json lines into harness events.
type parser struct {
	hint      string // added to the error of a failed result (harness.CredentialHint)
	sessionID string
	lastText  string
	toolNames map[string]string
	finished  bool
	// msgUsage is the newest usage of each assistant message of the turn,
	// by message ID. The CLI repeats the usage of a message on each of its
	// content blocks, so the last one counts.
	msgUsage map[string]usageJSON
	msgOrder []string
}

func newParser() *parser {
	return &parser{toolNames: map[string]string{}, msgUsage: map[string]usageJSON{}}
}

// noteUsage records the usage of an assistant message.
func (p *parser) noteUsage(m *streamInner) {
	if m.Usage == nil {
		return
	}
	id := m.ID
	if id == "" {
		id = fmt.Sprintf("anon-%d", len(p.msgOrder))
	}
	if _, ok := p.msgUsage[id]; !ok {
		p.msgOrder = append(p.msgOrder, id)
	}
	p.msgUsage[id] = *m.Usage
}

// partialUsage returns the sum of the usage of the assistant messages, marked
// partial, or nil without tokens. The stream has no cost for single messages,
// so the cost is 0. The duration is 0: the runner measures the turn.
func (p *parser) partialUsage() *core.Usage {
	var u core.Usage
	for _, id := range p.msgOrder {
		m := p.msgUsage[id]
		u.InputTokens += m.InputTokens + m.CacheCreationInputTokens + m.CacheReadInputTokens
		u.OutputTokens += m.OutputTokens
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}
	u.Coverage = core.UsagePartial
	return &u
}

type streamMsg struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID string          `json:"parent_tool_use_id"`
	Model           string          `json:"model"`
	Tools           []string        `json:"tools"`
	MCPServers      []mcpStatus     `json:"mcp_servers"`
	Message         *streamInner    `json:"message"`
	Result          string          `json:"result"`
	IsError         bool            `json:"is_error"`
	TotalCostUSD    float64         `json:"total_cost_usd"`
	DurationMS      int64           `json:"duration_ms"`
	NumTurns        int             `json:"num_turns"`
	Usage           *usageJSON      `json:"usage"`
	Errors          json.RawMessage `json:"errors"`
}

type mcpStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

type streamInner struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *usageJSON      `json:"usage"`
}

type usageJSON struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (p *parser) handle(line []byte) []core.HarnessEvent {
	var m streamMsg
	if err := json.Unmarshal(line, &m); err != nil {
		return []core.HarnessEvent{{Kind: harness.EventLog, Role: "log", Content: harness.Truncate(string(line), 4000)}}
	}
	if m.SessionID != "" {
		p.sessionID = m.SessionID
	}
	switch m.Type {
	case "system":
		return p.system(m)
	case "assistant":
		return p.assistant(m)
	case "user":
		return p.user(m)
	case "result":
		return p.result(m)
	}
	return nil // stream_event, rate_limit_event and future types are ignored
}

func (p *parser) system(m streamMsg) []core.HarnessEvent {
	if m.Subtype != "init" {
		if m.Subtype == "" {
			return nil
		}
		return []core.HarnessEvent{{Kind: harness.EventLog, Role: "log", Content: "claude: " + m.Subtype, Data: map[string]any{"subtype": m.Subtype}}}
	}
	evs := []core.HarnessEvent{{
		Kind:      harness.EventSession,
		SessionID: m.SessionID,
		Data:      map[string]any{"model": m.Model, "tools": m.Tools},
	}}
	for _, s := range m.MCPServers {
		if s.Status != "" && s.Status != "connected" {
			evs = append(evs, core.HarnessEvent{Kind: harness.EventLog, Role: "log", Content: fmt.Sprintf("mcp server %q status: %s", s.Name, s.Status)})
		}
	}
	return evs
}

func parseBlocks(raw json.RawMessage) []block {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var bs []block
	_ = json.Unmarshal(raw, &bs)
	return bs
}

func (p *parser) baseData(m streamMsg) map[string]any {
	d := map[string]any{}
	if m.ParentToolUseID != "" {
		d["parent_tool_use_id"] = m.ParentToolUseID
	}
	return d
}

func (p *parser) assistant(m streamMsg) []core.HarnessEvent {
	if m.Message == nil {
		return nil
	}
	p.noteUsage(m.Message)
	var evs []core.HarnessEvent
	for _, b := range parseBlocks(m.Message.Content) {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			if m.ParentToolUseID == "" {
				p.lastText = b.Text
			}
			evs = append(evs, core.HarnessEvent{Kind: harness.EventMessage, Role: "assistant", Content: b.Text, SessionID: p.sessionID, Data: p.baseData(m)})
		case "tool_use":
			p.toolNames[b.ID] = b.Name
			d := p.baseData(m)
			d["id"], d["name"] = b.ID, b.Name
			var input any
			_ = json.Unmarshal(b.Input, &input)
			d["input"] = input
			evs = append(evs, core.HarnessEvent{Kind: harness.EventToolCall, Role: "tool_call", Content: toolCallSummary(b.Name, b.Input), SessionID: p.sessionID, Data: d})
		}
	}
	return evs
}

func toolCallSummary(name string, input json.RawMessage) string {
	in := strings.TrimSpace(string(input))
	if in == "" || in == "null" {
		in = "{}"
	}
	return name + " " + harness.Truncate(in, 2000)
}

func (p *parser) user(m streamMsg) []core.HarnessEvent {
	if m.Message == nil {
		return nil
	}
	var evs []core.HarnessEvent
	for _, b := range parseBlocks(m.Message.Content) {
		if b.Type != "tool_result" {
			continue
		}
		d := p.baseData(m)
		d["tool_use_id"], d["name"], d["is_error"] = b.ToolUseID, p.toolNames[b.ToolUseID], b.IsError
		evs = append(evs, core.HarnessEvent{Kind: harness.EventToolResult, Role: "tool_result", Content: harness.Truncate(resultText(b.Content), 16000), SessionID: p.sessionID, Data: d})
	}
	return evs
}

func resultText(raw json.RawMessage) string {
	var parts []string
	for _, b := range parseBlocks(raw) {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// result reports the totals of the result message. They replace the usage
// of the assistant messages, so nothing counts twice.
func (p *parser) result(m streamMsg) []core.HarnessEvent {
	p.finished = true
	u := &core.Usage{CostUSD: m.TotalCostUSD, DurationMS: m.DurationMS}
	if m.Usage != nil {
		u.InputTokens = m.Usage.InputTokens + m.Usage.CacheCreationInputTokens + m.Usage.CacheReadInputTokens
		u.OutputTokens = m.Usage.OutputTokens
	}
	evs := []core.HarnessEvent{{Kind: harness.EventUsage, Usage: u, SessionID: p.sessionID, Data: map[string]any{"num_turns": m.NumTurns}}}
	result := m.Result
	if result == "" {
		result = p.lastText
	}
	if m.IsError || (m.Subtype != "" && m.Subtype != "success") {
		msg := result
		if msg == "" || m.Subtype == "error_max_turns" {
			msg = describeSubtype(m.Subtype)
			if len(m.Errors) > 0 && string(m.Errors) != "null" && string(m.Errors) != "[]" {
				msg += ": " + string(m.Errors)
			}
		}
		evs = append(evs, core.HarnessEvent{Kind: harness.EventError, Content: msg + p.hint, SessionID: p.sessionID, Usage: u, Data: map[string]any{"subtype": m.Subtype, "num_turns": m.NumTurns}})
		return evs
	}
	evs = append(evs, core.HarnessEvent{Kind: harness.EventDone, Role: "assistant", Content: result, SessionID: p.sessionID, Usage: u, Data: map[string]any{"num_turns": m.NumTurns}})
	return evs
}

func describeSubtype(s string) string {
	switch s {
	case "error_max_turns":
		return "claude stopped: maximum number of turns reached"
	case "error_during_execution":
		return "claude failed during execution"
	case "":
		return "claude reported an error"
	}
	return "claude: " + s
}
