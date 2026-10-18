package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	maxDiagnosticSize = 16 << 10
	skillFile         = "SKILL.md"
	skillsDir         = ".openfactory/skills"
)

// CoreHarness runs a harness of internal/harness (claude, codex or open) inside the runner.
// The sandbox of the harness is the local machine, in the workspace.
// One value serves one session: it keeps the harness session ID and the
// usage totals between turns. It runs one turn at a time.
type CoreHarness struct {
	H    core.Harness
	Caps []runnerproto.Capability // capabilities to report; nil means {interrupt}

	mu        sync.Mutex
	sessionID string     // newest harness session ID seen in this process
	usage     core.Usage // totals of all turns
	declared  []string   // artifact paths that the harness reported since the last take
}

// Capabilities returns the capabilities of the harness.
func (c *CoreHarness) Capabilities() []runnerproto.Capability {
	if c.Caps == nil {
		return []runnerproto.Capability{runnerproto.CapInterrupt}
	}
	return c.Caps
}

// Turn runs one turn of the wrapped harness and maps its events.
func (c *CoreHarness) Turn(ctx context.Context, req TurnRequest, emit Emitter) TurnResult {
	resume := c.resumeID(req)
	hreq := core.HarnessRequest{
		Run: &core.Run{
			ID:        req.RunID,
			SessionID: resume,
			Config: core.AgentConfig{Harness: core.HarnessConfig{
				Type:           core.HarnessType(req.Harness.Type),
				Model:          req.Harness.Model,
				ReasoningLevel: req.Harness.ReasoningLevel,
			}},
		},
		Prompt:       RenderInput(req.Input),
		SystemPrompt: req.Harness.SystemPrompt,
		Resume:       resume != "",
		WorkDir:      req.WorkDir,
		Env:          envMap(req.Env),
		MCPServers:   coreMCPServers(req.Harness.MCPServers, req.MCPHeaders, req.MCPEnv),
		Skills:       coreSkills(req.Skills),
		Auth:         req.Auth,
	}
	sb := newHostSandbox(req.SandboxID, req.WorkDir, req.Env)
	ch, err := c.H.Run(ctx, sb, hreq)
	if err != nil {
		if ctx.Err() != nil {
			return TurnResult{Outcome: runnerproto.OutcomeInterrupted}
		}
		return TurnResult{
			Outcome:     runnerproto.OutcomeError,
			FailureCode: startFailureCode(err.Error()),
			Message:     err.Error(),
		}
	}

	t := &coreTurn{c: c, emit: emit, known: resume}
	for ev := range ch {
		if ctx.Err() != nil {
			continue // drain; the runner reports interrupted
		}
		t.handle(ev)
	}
	if ctx.Err() != nil {
		return TurnResult{Outcome: runnerproto.OutcomeInterrupted}
	}
	if t.result != nil {
		return *t.result
	}
	return agentError("harness exited without completing the turn")
}

// resumeID returns the session to resume: the newest one of this process,
// else the one of the start response.
func (c *CoreHarness) resumeID(req TurnRequest) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID != "" {
		return c.sessionID
	}
	return req.ResumeSessionID
}

// coreTurn is the state of the event mapping of one turn.
type coreTurn struct {
	c         *CoreHarness
	emit      Emitter
	known     string // session ID that the control plane knows
	lastReply string // last non-empty assistant content
	usageSeen bool   // a usage event came in this turn
	result    *TurnResult
}

func (t *coreTurn) handle(ev core.HarnessEvent) {
	if ev.SessionID != "" {
		t.session(ev.SessionID)
	}
	switch ev.Kind {
	case "session":
		// The session ID is handled above.
	case "message":
		t.message(ev)
	case "tool_call":
		t.toolCall(ev)
	case "tool_result":
		t.toolResult(ev)
	case "usage":
		if ev.Usage != nil {
			t.usageSeen = true
			t.addUsage(ev.Usage)
		}
	case "artifact":
		// The runner uploads the file at the end of the turn (TakeDeclaredArtifacts).
		t.c.declareArtifacts(ev)
	case "done":
		t.addDoneUsage(ev.Usage)
		res := ev.Content
		if res == "" {
			res = t.lastReply
		}
		res, _ = cutContent(res)
		t.result = &TurnResult{Outcome: runnerproto.OutcomeDone, Result: res}
	case "error":
		t.addDoneUsage(ev.Usage)
		msg := ev.Content
		if msg == "" {
			msg = "harness reported an error"
		}
		code := string(core.FailureAgentFailed)
		if authMessage(msg) {
			code = string(core.FailureHarnessAuthFailed)
		}
		t.result = &TurnResult{Outcome: runnerproto.OutcomeError, FailureCode: code, Message: msg}
	default:
		if ev.Content != "" { // log and unknown kinds
			t.emit(runnerproto.EventDiagnostic, runnerproto.DiagnosticData{
				Code:    "harness_log",
				Message: cutBytes(ev.Content, maxDiagnosticSize),
			})
		}
	}
}

// session sends harness.session once for each new session ID.
func (t *coreTurn) session(id string) {
	t.c.mu.Lock()
	t.c.sessionID = id
	t.c.mu.Unlock()
	if id != t.known {
		t.known = id
		t.emit(runnerproto.EventHarnessSession, runnerproto.HarnessSessionData{SessionID: id})
	}
}

func (t *coreTurn) message(ev core.HarnessEvent) {
	if ev.Content == "" {
		return
	}
	role := ev.Role
	switch role {
	case "":
		role = "assistant"
	case "assistant", "system":
	default:
		role = "system"
	}
	if role == "assistant" {
		t.lastReply = ev.Content
	}
	content, cut := cutContent(ev.Content)
	t.emit(runnerproto.EventMessage, runnerproto.MessageData{Role: role, Content: content, Truncated: cut})
}

func (t *coreTurn) toolCall(ev core.HarnessEvent) {
	d := runnerproto.ToolCallData{
		CallID:   dataString(ev.Data, "call_id", "id"),
		Name:     dataString(ev.Data, "name"),
		Category: dataString(ev.Data, "category"),
	}
	if d.Name == "" {
		d.Name = ev.Content
	}
	d.Arguments = json.RawMessage("{}")
	for _, k := range []string{"arguments", "input"} {
		if v, ok := ev.Data[k]; ok && v != nil {
			if raw, err := json.Marshal(v); err == nil {
				d.Arguments = raw
			}
			break
		}
	}
	t.emit(runnerproto.EventToolCall, d)
}

func (t *coreTurn) toolResult(ev core.HarnessEvent) {
	content, cut := cutContent(ev.Content)
	isErr, _ := ev.Data["is_error"].(bool)
	t.emit(runnerproto.EventToolResult, runnerproto.ToolResultData{
		CallID:    dataString(ev.Data, "call_id", "tool_use_id"),
		Content:   content,
		IsError:   isErr,
		Truncated: cut,
	})
}

// addDoneUsage adds the usage of a final event. The harnesses send the same
// numbers in a usage event first, so it counts only without one.
func (t *coreTurn) addDoneUsage(u *core.Usage) {
	if u != nil && !t.usageSeen {
		t.usageSeen = true
		t.addUsage(u)
	}
}

// addUsage adds u to the totals and sends the new totals.
func (t *coreTurn) addUsage(u *core.Usage) {
	t.c.mu.Lock()
	t.c.usage.InputTokens += u.InputTokens
	t.c.usage.OutputTokens += u.OutputTokens
	t.c.usage.CostUSD += u.CostUSD
	total := t.c.usage
	t.c.mu.Unlock()
	t.emit(runnerproto.EventUsage, runnerproto.UsageData{
		InputTokens:  total.InputTokens,
		OutputTokens: total.OutputTokens,
		CostUSD:      total.CostUSD,
		Cumulative:   true,
		Coverage:     "known",
	})
}

// dataString returns the first non-empty string value of the keys.
func dataString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// envMap turns KEY=VALUE entries into a map.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok && k != "" {
			m[k] = v
		}
	}
	return m
}

// coreMCPServers converts the servers of the start response and adds the
// headers and the environment of the secrets response. It copies the maps.
func coreMCPServers(servers map[string]runnerproto.MCPServer, headers, env map[string]map[string]string) map[string]core.MCPServer {
	if len(servers) == 0 {
		return nil
	}
	out := make(map[string]core.MCPServer, len(servers))
	for name, s := range servers {
		cs := core.MCPServer{URL: s.URL, Command: s.Command, Args: s.Args, Env: mergeEnv(s.Env, env[name])}
		if h := headers[name]; len(h) > 0 {
			cs.Headers = make(map[string]string, len(h))
			for k, v := range h {
				cs.Headers[k] = v
			}
		}
		out[name] = cs
	}
	return out
}

// mergeEnv returns a new map with the values of base and then extra.
func mergeEnv(base, extra map[string]string) map[string]string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// coreSkills converts the skills. The runner wrote them below .openfactory/skills.
func coreSkills(skills []runnerproto.Skill) []core.Skill {
	if len(skills) == 0 {
		return nil
	}
	out := make([]core.Skill, 0, len(skills))
	for _, s := range skills {
		cs := core.Skill{Name: s.Name, Content: s.Files[skillFile], Path: skillsDir + "/" + s.Name}
		for name, content := range s.Files {
			if name == skillFile {
				continue
			}
			if cs.Files == nil {
				cs.Files = map[string]string{}
			}
			cs.Files[name] = content
		}
		out = append(out, cs)
	}
	return out
}

// authMessage reports whether a message says that the API key or the login
// is missing or not valid.
func authMessage(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{
		"invalid api key", "incorrect api key", "invalid x-api-key", "missing api key",
		"no api key", "api key is empty", "api key not set", "not logged in",
		"please run /login", "authentication_error", "authentication failed",
		"credential type not supported",
	} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// startFailureCode gives the failure code for an error of the harness start.
// The harnesses report a bad credential as "<name> auth: ...".
func startFailureCode(msg string) string {
	if authMessage(msg) || strings.Contains(msg, " auth: ") {
		return string(core.FailureHarnessAuthFailed)
	}
	return string(core.FailureAgentFailed)
}

// cutBytes cuts s to at most n bytes.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// StateFiles returns the session files of the harness, relative to home. It
// returns no credential files.
func (c *CoreHarness) StateFiles(home, workDir string) ([]string, error) {
	var roots []string
	switch c.H.Type() {
	case core.HarnessClaude:
		dirs := []string{workDir}
		if real, err := filepath.EvalSymlinks(workDir); err == nil && real != workDir {
			dirs = append(dirs, real)
		}
		for _, d := range dirs {
			roots = append(roots, ".claude/projects/"+encodeClaudeDir(d))
		}
	case core.HarnessCodex:
		roots = []string{".codex/sessions"}
	default:
		return nil, nil
	}
	var out []string
	for _, root := range roots {
		err := filepath.WalkDir(filepath.Join(home, filepath.FromSlash(root)), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() || isCredentialFile(d.Name()) {
				return nil
			}
			rel, err := filepath.Rel(home, p)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// encodeClaudeDir replaces each character that is not an ASCII letter or
// digit with '-'.
func encodeClaudeDir(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func isCredentialFile(name string) bool {
	return name == ".credentials.json" || name == "auth.json" || strings.HasPrefix(name, "credentials")
}

// declareArtifacts keeps the paths of an artifact event of the harness.
func (c *CoreHarness) declareArtifacts(ev core.HarnessEvent) {
	var paths []string
	if p, ok := ev.Data["path"].(string); ok && p != "" {
		paths = append(paths, p)
	}
	switch ps := ev.Data["paths"].(type) {
	case []string:
		paths = append(paths, ps...)
	case []any:
		for _, p := range ps {
			if s, ok := p.(string); ok && s != "" {
				paths = append(paths, s)
			}
		}
	}
	if len(paths) == 0 && strings.TrimSpace(ev.Content) != "" {
		paths = append(paths, strings.TrimSpace(ev.Content))
	}
	c.mu.Lock()
	c.declared = append(c.declared, paths...)
	c.mu.Unlock()
}

// TakeDeclaredArtifacts returns the artifact paths that the harness reported
// since the last call. A path is absolute or relative to the work directory.
func (c *CoreHarness) TakeDeclaredArtifacts() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.declared
	c.declared = nil
	return out
}

// TranscriptPath returns the raw session log of the harness, or "" when there
// is none. The claude harness keeps it in
// .claude/projects/<encoded work dir>/<session id>.jsonl, the codex harness in
// .codex/sessions/<date>/rollout-*.jsonl. Without a known session ID, the
// newest file wins.
func (c *CoreHarness) TranscriptPath(home, workDir string) string {
	c.mu.Lock()
	id := c.sessionID
	c.mu.Unlock()
	var roots []string
	switch c.H.Type() {
	case core.HarnessClaude:
		dirs := []string{workDir}
		if real, err := filepath.EvalSymlinks(workDir); err == nil && real != workDir {
			dirs = append(dirs, real)
		}
		for _, d := range dirs {
			roots = append(roots, filepath.Join(home, ".claude", "projects", encodeClaudeDir(d)))
		}
	case core.HarnessCodex:
		roots = []string{filepath.Join(home, ".codex", "sessions")}
	default:
		return ""
	}
	var best string
	var bestTime time.Time
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".jsonl") {
				return nil //nolint:nilerr // best-effort search: an unreadable entry or a missing root is skipped
			}
			if id != "" && !strings.Contains(d.Name(), id) {
				return nil
			}
			if info, err := d.Info(); err == nil && (best == "" || info.ModTime().After(bestTime)) {
				best, bestTime = p, info.ModTime()
			}
			return nil
		})
	}
	return best
}
