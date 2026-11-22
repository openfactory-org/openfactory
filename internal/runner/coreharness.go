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

	"github.com/openfactory-org/openfactory/internal/rtcore"
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
	H    rtcore.Harness
	Caps []runnerproto.Capability // capabilities to report; nil means {interrupt}

	mu        sync.Mutex
	sessionID string       // newest harness session ID seen in this process
	usage     rtcore.Usage // totals of all turns
	declared  []string     // artifact paths that the harness reported since the last take

	now func() time.Time // clock for the turn wall time; nil means time.Now
}

func (c *CoreHarness) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
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
	hreq := rtcore.HarnessRequest{
		RunID:     req.RunID,
		SessionID: resume,
		Harness: rtcore.HarnessConfig{
			Type:           rtcore.HarnessType(req.Harness.Type),
			Model:          req.Harness.Model,
			ReasoningLevel: req.Harness.ReasoningLevel,
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
	sb.agent = req.Agent
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

	t := &coreTurn{c: c, emit: emit, known: resume, start: c.clock()}
	for ev := range ch {
		if ctx.Err() != nil {
			// Drain; the runner reports interrupted. The usage of the
			// stopped turn still counts.
			if ev.Kind == "usage" && ev.Usage != nil {
				t.handle(ev)
			}
			continue
		}
		t.handle(ev)
	}
	t.endDuration()
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

	start      time.Time // start of the turn
	durationMS int64     // turn duration that the harness reported; 0 means none
}

func (t *coreTurn) handle(ev rtcore.HarnessEvent) {
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
		code := string(rtcore.FailureAgentFailed)
		if authMessage(msg) {
			code = string(rtcore.FailureHarnessAuthFailed)
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

func (t *coreTurn) message(ev rtcore.HarnessEvent) {
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

func (t *coreTurn) toolCall(ev rtcore.HarnessEvent) {
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

func (t *coreTurn) toolResult(ev rtcore.HarnessEvent) {
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
func (t *coreTurn) addDoneUsage(u *rtcore.Usage) {
	if u != nil && !t.usageSeen {
		t.usageSeen = true
		t.addUsage(u)
	}
}

// addUsage adds u to the totals and sends the new totals. The totals of a
// session are partial from the first partial turn on. The duration of the
// turn counts at its end (endDuration); until then the totals include the
// duration that the harness reported for the turn.
func (t *coreTurn) addUsage(u *rtcore.Usage) {
	if u.DurationMS > 0 {
		t.durationMS = u.DurationMS
	}
	t.c.mu.Lock()
	t.c.usage.InputTokens += u.InputTokens
	t.c.usage.OutputTokens += u.OutputTokens
	t.c.usage.CostUSD += u.CostUSD
	if u.Coverage == rtcore.UsagePartial {
		t.c.usage.Coverage = rtcore.UsagePartial
	}
	total := t.c.usage
	t.c.mu.Unlock()
	total.DurationMS += t.durationMS
	t.sendUsage(total)
}

// endDuration adds the duration of the turn to the totals: the duration that
// the harness reported, else the wall time of the turn. It sends the totals
// when the turn sent none with this duration.
func (t *coreTurn) endDuration() {
	d, reported := t.durationMS, t.durationMS > 0
	if !reported {
		d = t.c.clock().Sub(t.start).Milliseconds()
		if d < 0 {
			d = 0
		}
	}
	t.c.mu.Lock()
	t.c.usage.DurationMS += d
	total := t.c.usage
	t.c.mu.Unlock()
	if !reported && d > 0 {
		t.sendUsage(total)
	}
}

func (t *coreTurn) sendUsage(total rtcore.Usage) {
	t.emit(runnerproto.EventUsage, runnerproto.UsageData{
		InputTokens:  total.InputTokens,
		OutputTokens: total.OutputTokens,
		CostUSD:      total.CostUSD,
		DurationMS:   total.DurationMS,
		Cumulative:   true,
		Coverage:     total.CoverageLabel(),
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
func coreMCPServers(servers map[string]runnerproto.MCPServer, headers, env map[string]map[string]string) map[string]rtcore.MCPServer {
	if len(servers) == 0 {
		return nil
	}
	out := make(map[string]rtcore.MCPServer, len(servers))
	for name, s := range servers {
		cs := rtcore.MCPServer{URL: s.URL, Command: s.Command, Args: s.Args, Env: mergeEnv(s.Env, env[name])}
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
func coreSkills(skills []runnerproto.Skill) []rtcore.Skill {
	if len(skills) == 0 {
		return nil
	}
	out := make([]rtcore.Skill, 0, len(skills))
	for _, s := range skills {
		cs := rtcore.Skill{Name: s.Name, Content: s.Files[skillFile], Path: skillsDir + "/" + s.Name}
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
		return string(rtcore.FailureHarnessAuthFailed)
	}
	return string(rtcore.FailureAgentFailed)
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
	case rtcore.HarnessClaude:
		dirs := []string{workDir}
		if real, err := filepath.EvalSymlinks(workDir); err == nil && real != workDir {
			dirs = append(dirs, real)
		}
		for _, d := range dirs {
			roots = append(roots, ".claude/projects/"+encodeClaudeDir(d))
		}
	case rtcore.HarnessCodex:
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
func (c *CoreHarness) declareArtifacts(ev rtcore.HarnessEvent) {
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
	case rtcore.HarnessClaude:
		dirs := []string{workDir}
		if real, err := filepath.EvalSymlinks(workDir); err == nil && real != workDir {
			dirs = append(dirs, real)
		}
		for _, d := range dirs {
			roots = append(roots, filepath.Join(home, ".claude", "projects", encodeClaudeDir(d)))
		}
	case rtcore.HarnessCodex:
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
