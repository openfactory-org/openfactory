package claude

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/harness/harnesstest"
)

const fixture = `{"type":"system","subtype":"init","session_id":"sess-123","model":"claude-sonnet-4-6","tools":["Bash","Read"],"mcp_servers":[{"name":"factory","status":"connected"},{"name":"broken","status":"failed"}]}
{"type":"assistant","session_id":"sess-123","message":{"id":"m1","content":[{"type":"thinking","thinking":""},{"type":"text","text":"I'll list files."},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","session_id":"sess-123","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"a.go\nb.go"}],"is_error":false}]}}
not json at all
{"type":"stream_event","event":{}}
{"type":"assistant","session_id":"sess-123","message":{"id":"m2","content":[{"type":"text","text":"Done: two files."}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Done: two files.","session_id":"sess-123","total_cost_usd":0.0123,"duration_ms":4567,"num_turns":2,"usage":{"input_tokens":100,"cache_creation_input_tokens":10,"cache_read_input_tokens":1000,"output_tokens":50}}
`

func collect(t *testing.T, ch <-chan core.HarnessEvent) []core.HarnessEvent {
	t.Helper()
	var evs []core.HarnessEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatal("timeout waiting for events")
		}
	}
}

func kinds(evs []core.HarnessEvent) []string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestRunParsesStreamJSON(t *testing.T) {
	sb := harnesstest.New("/work")
	var proc *harnesstest.Process
	sb.StartFunc = func(_ context.Context, req core.ExecRequest) (core.Process, error) {
		proc = harnesstest.NewProcess(fixture, "", 0)
		return proc, nil
	}
	h := New(Options{MaxTurns: 7})
	req := core.HarnessRequest{
		Run: &core.Run{ID: "run_1", SessionID: "sess-prev", Config: core.AgentConfig{Harness: core.HarnessConfig{
			Type: core.HarnessClaude, Model: "claude-sonnet-4-6", ReasoningLevel: "xhigh",
		}}},
		Prompt:       "list files",
		SystemPrompt: "You are the implement agent.",
		Resume:       true,
		Env:          map[string]string{"OF_METADATA_TICKET": "ENG-1"},
		Auth:         map[string]string{"ANTHROPIC_API_KEY": "sk-ant"},
		MCPServers: map[string]core.MCPServer{
			"factory": {URL: "https://of.example/mcp/run/run_1", Headers: map[string]string{"Authorization": "Bearer t"}},
			"dbt":     {Command: "uvx", Args: []string{"dbt-mcp"}, Env: map[string]string{"X": "1"}},
			"managed": {ManagedID: "abc"},
		},
		Skills: []core.Skill{{Name: "Security Review", Description: "checks", Content: "# SR", Files: map[string]string{"ref/check.md": "x", "../../evil": "y"}, Scope: "factory"}},
	}
	ch, err := h.Run(context.Background(), sb, req)
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)

	want := []string{"log", "session", "log", "message", "tool_call", "tool_result", "log", "message", "usage", "done"}
	if !slices.Equal(kinds(evs), want) {
		t.Fatalf("kinds = %v, want %v", kinds(evs), want)
	}
	if evs[1].SessionID != "sess-123" {
		t.Fatalf("session = %q", evs[1].SessionID)
	}
	if evs[4].Data["name"] != "Bash" || evs[4].Data["id"] != "tu_1" {
		t.Fatalf("tool_call data = %v", evs[4].Data)
	}
	if evs[5].Content != "a.go\nb.go" || evs[5].Data["name"] != "Bash" {
		t.Fatalf("tool_result = %+v", evs[5])
	}
	u := evs[8].Usage
	if u.InputTokens != 1110 || u.OutputTokens != 50 || u.CostUSD != 0.0123 || u.DurationMS != 4567 {
		t.Fatalf("usage = %+v", u)
	}
	if evs[9].Content != "Done: two files." || evs[9].SessionID != "sess-123" {
		t.Fatalf("done = %+v", evs[9])
	}

	starts := sb.StartRequests()
	if len(starts) != 1 {
		t.Fatalf("starts = %d", len(starts))
	}
	args := starts[0].Cmd
	joined := strings.Join(args, " ")
	for _, wantArg := range []string{
		"claude -p --output-format stream-json --verbose --input-format text",
		"--resume sess-prev", "--model claude-sonnet-4-6", "--effort xhigh",
		"--mcp-config /work/.openfactory/claude/mcp.json --strict-mcp-config",
		"--permission-mode bypassPermissions", "--max-turns 7",
	} {
		if !strings.Contains(joined, wantArg) {
			t.Errorf("args missing %q: %v", wantArg, args)
		}
	}
	i := slices.Index(args, "--append-system-prompt")
	if i < 0 || !strings.HasPrefix(args[i+1], "You are the implement agent.") {
		t.Fatalf("system prompt arg missing: %v", args)
	}
	if starts[0].Dir != "/work" {
		t.Fatalf("dir = %q", starts[0].Dir)
	}
	env := starts[0].Env
	if env["ANTHROPIC_API_KEY"] != "sk-ant" || env["IS_SANDBOX"] != "1" || env["OF_METADATA_TICKET"] != "ENG-1" {
		t.Fatalf("env = %v", env)
	}
	if proc.StdinBuf.String() != "list files" {
		t.Fatalf("stdin = %q", proc.StdinBuf.String())
	}

	var mcp struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(sb.File("/work/.openfactory/claude/mcp.json")), &mcp); err != nil {
		t.Fatal(err)
	}
	if mcp.MCPServers["factory"]["type"] != "http" || mcp.MCPServers["dbt"]["type"] != "stdio" {
		t.Fatalf("mcp config = %v", mcp)
	}
	if _, ok := mcp.MCPServers["managed"]; ok {
		t.Fatal("managed-only server should be skipped")
	}
	if !strings.Contains(evs[0].Content, "managed") {
		t.Fatalf("expected warning log, got %q", evs[0].Content)
	}
	if sb.File("/work/.openfactory/skills/security-review/SKILL.md") != "# SR" ||
		sb.File("/work/.openfactory/skills/security-review/ref/check.md") != "x" ||
		sb.File("/work/.claude/skills/security-review/SKILL.md") != "# SR" ||
		sb.File("/work/.claude/skills/security-review/.gitignore") != "*\n" {
		t.Fatalf("skills not written: %v", keys(sb.Files))
	}
	if sb.File("/work/.openfactory/skills/security-review/evil") != "y" {
		t.Fatalf("escaping path should be confined to skill dir: %v", keys(sb.Files))
	}
	if sb.File("/work/.openfactory/.gitignore") != "*\n" {
		t.Fatal("state dir .gitignore missing")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestRunErrorResult(t *testing.T) {
	sb := harnesstest.New("/w")
	sb.StartFunc = func(context.Context, core.ExecRequest) (core.Process, error) {
		return harnesstest.NewProcess(`{"type":"system","subtype":"init","session_id":"s1"}
{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"s1","num_turns":3,"total_cost_usd":0.5}
`, "", 1), nil
	}
	ch, err := New(Options{}).Run(context.Background(), sb, core.HarnessRequest{Run: &core.Run{}, Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)
	last := evs[len(evs)-1]
	if last.Kind != harness.EventError || !strings.Contains(last.Content, "maximum number of turns") || last.SessionID != "s1" {
		t.Fatalf("last = %+v", last)
	}
}

func TestRunProcessCrash(t *testing.T) {
	sb := harnesstest.New("/w")
	sb.StartFunc = func(context.Context, core.ExecRequest) (core.Process, error) {
		return harnesstest.NewProcess("", "Error: Invalid API key\n", 1), nil
	}
	ch, _ := New(Options{}).Run(context.Background(), sb, core.HarnessRequest{Run: &core.Run{}, Prompt: "x"})
	evs := collect(t, ch)
	if len(evs) != 1 || evs[0].Kind != harness.EventError || !strings.Contains(evs[0].Content, "Invalid API key") {
		t.Fatalf("evs = %+v", evs)
	}
}

func TestAuthMismatch(t *testing.T) {
	_, err := New(Options{}).Run(context.Background(), harnesstest.New("/w"), core.HarnessRequest{
		Run: &core.Run{}, Auth: map[string]string{"type": "openai_api_key", "value": "sk"},
	})
	if err == nil {
		t.Fatal("expected auth error")
	}
}

func TestBuildArgsVariants(t *testing.T) {
	h := New(Options{DangerouslySkipPermissions: true, ThinkingTokensEnv: true})
	args, env := h.BuildArgs(core.HarnessRequest{Run: &core.Run{Config: core.AgentConfig{Harness: core.HarnessConfig{Model: "default", ReasoningLevel: "high"}}}}, "/w/sp.md", "")
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--model") || strings.Contains(joined, "--resume") || strings.Contains(joined, "--effort") {
		t.Fatalf("unexpected args %v", args)
	}
	if !strings.Contains(joined, "--append-system-prompt-file /w/sp.md") || !strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Fatalf("args = %v", args)
	}
	if env["MAX_THINKING_TOKENS"] != "31999" {
		t.Fatalf("env = %v", env)
	}
}
