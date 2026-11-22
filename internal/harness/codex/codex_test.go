package codex

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/harness/harnesstest"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

const fixture = `{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"**Thinking**"}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","aggregated_output":"a.go\n","exit_code":0,"status":"completed"}}
{"type":"item.started","item":{"id":"item_2","type":"mcp_tool_call","server":"factory","tool":"post_update","arguments":{"text":"hi"},"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_2","type":"mcp_tool_call","server":"factory","tool":"post_update","arguments":{"text":"hi"},"result":{"content":[{"type":"text","text":"posted"}]},"status":"completed"}}
{"type":"item.completed","item":{"id":"item_3","type":"file_change","changes":[{"path":"a.go","kind":"update"}],"status":"completed"}}
{"type":"item.updated","item":{"id":"item_4","type":"todo_list","items":[{"text":"x","completed":false}]}}
{"type":"item.completed","item":{"id":"item_5","type":"some_future_item","foo":1}}
{"type":"brand.new.event"}
{"type":"item.completed","item":{"id":"item_6","type":"agent_message","text":"All done."}}
{"type":"turn.completed","usage":{"input_tokens":1000000,"cached_input_tokens":0,"output_tokens":100000}}
`

func collect(t *testing.T, ch <-chan rtcore.HarnessEvent) []rtcore.HarnessEvent {
	t.Helper()
	var evs []rtcore.HarnessEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatal("timeout")
		}
	}
}

func kinds(evs []rtcore.HarnessEvent) []string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestRunParsesJSONL(t *testing.T) {
	sb := harnesstest.New("/work")
	var proc *harnesstest.Process
	sb.StartFunc = func(context.Context, rtcore.ExecRequest) (rtcore.Process, error) {
		proc = harnesstest.NewProcess(fixture, "", 0)
		return proc, nil
	}
	req := rtcore.HarnessRequest{
		Harness:      rtcore.HarnessConfig{Type: rtcore.HarnessCodex, Model: "gpt-5", ReasoningLevel: "high"},
		Prompt:       "do it",
		SystemPrompt: "You implement.",
		Auth:         map[string]string{"type": "openai_api_key", "value": "sk-open"},
		MCPServers: map[string]rtcore.MCPServer{
			"factory":  {URL: "https://of.example/mcp/run/r1", Headers: map[string]string{"Authorization": "Bearer tok"}},
			"my tools": {Command: "uvx", Args: []string{"tools-mcp", "--flag=\"x\""}, Env: map[string]string{"TOKEN": "s3"}},
		},
		Skills: []rtcore.Skill{{Name: "triage", Content: "# T"}},
	}
	ch, err := New(Options{}).Run(context.Background(), sb, req)
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)
	want := []string{"session", "tool_call", "tool_result", "tool_call", "tool_result", "tool_call", "tool_result", "message", "usage", "done"}
	if !slices.Equal(kinds(evs), want) {
		t.Fatalf("kinds = %v", kinds(evs))
	}
	if evs[0].SessionID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Fatalf("session = %q", evs[0].SessionID)
	}
	if evs[2].Content != "a.go\n" || evs[2].Data["is_error"] != false {
		t.Fatalf("command result = %+v", evs[2])
	}
	if evs[3].Data["name"] != "mcp__factory__post_update" || evs[4].Content != "posted" {
		t.Fatalf("mcp = %+v / %+v", evs[3], evs[4])
	}
	if evs[6].Content != "update a.go" {
		t.Fatalf("file change = %+v", evs[6])
	}
	if evs[9].Content != "All done." {
		t.Fatalf("done = %+v", evs[9])
	}
	if u := evs[8].Usage; u.InputTokens != 1000000 || u.OutputTokens != 100000 || u.CostUSD < 2.24 || u.CostUSD > 2.26 {
		t.Fatalf("usage = %+v", u)
	}

	start := sb.StartRequests()[0]
	joined := strings.Join(start.Cmd, " ")
	for _, w := range []string{"codex exec --json --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox", "-m gpt-5", `-c model_reasoning_effort="high"`, "-C /work"} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing %q in %v", w, start.Cmd)
		}
	}
	if start.Cmd[len(start.Cmd)-1] != "-" || strings.Contains(joined, "resume") {
		t.Fatalf("cmd = %v", start.Cmd)
	}
	if start.Env["CODEX_HOME"] != "/work/.openfactory/codex" || start.Env["OPENAI_API_KEY"] != "sk-open" || start.Env["CODEX_API_KEY"] != "sk-open" {
		t.Fatalf("env = %v", start.Env)
	}
	if proc.StdinBuf.String() != "do it" {
		t.Fatalf("stdin = %q", proc.StdinBuf.String())
	}
	cfg := sb.File("/work/.openfactory/codex/config.toml")
	for _, w := range []string{
		"[mcp_servers.factory]\nurl = \"https://of.example/mcp/run/r1\"",
		"[mcp_servers.factory.http_headers]\nAuthorization = \"Bearer tok\"",
		"[mcp_servers.\"my tools\"]\ncommand = \"uvx\"\nargs = [\"tools-mcp\", \"--flag=\\\"x\\\"\"]",
		"[mcp_servers.\"my tools\".env]\nTOKEN = \"s3\"",
		"ignore_default_excludes = true",
	} {
		if !strings.Contains(cfg, w) {
			t.Errorf("config.toml missing %q:\n%s", w, cfg)
		}
	}
	if !strings.HasPrefix(sb.File("/work/.openfactory/codex/AGENTS.md"), "You implement.") {
		t.Fatal("AGENTS.md not written")
	}
	if sb.File("/work/.openfactory/codex/skills/triage/SKILL.md") != "# T" || sb.File("/work/.openfactory/skills/triage/SKILL.md") != "# T" {
		t.Fatal("skills not written")
	}
}

func TestResumeArgs(t *testing.T) {
	args := New(Options{}).BuildArgs(rtcore.HarnessRequest{
		Resume:    true,
		SessionID: "thread-1", Harness: rtcore.HarnessConfig{Model: "default", ReasoningLevel: "max"},
	}, "/work")
	want := []string{"codex", "exec", "resume", "--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="xhigh"`, "thread-1", "-"}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %v", args)
	}
}

func TestTurnFailed(t *testing.T) {
	sb := harnesstest.New("/w")
	sb.StartFunc = func(context.Context, rtcore.ExecRequest) (rtcore.Process, error) {
		return harnesstest.NewProcess(`{"type":"thread.started","thread_id":"t1"}
{"type":"error","message":"stream disconnected; retrying 1/5"}
{"type":"turn.failed","error":{"message":"401 Unauthorized"}}
`, "", 1), nil
	}
	ch, err := New(Options{}).Run(context.Background(), sb, rtcore.HarnessRequest{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, ch)
	last := evs[len(evs)-1]
	if last.Kind != harness.EventError || !strings.Contains(last.Content, "401") || last.SessionID != "t1" {
		t.Fatalf("last = %+v (all %v)", last, kinds(evs))
	}
}

func TestExitWithoutTurn(t *testing.T) {
	sb := harnesstest.New("/w")
	sb.StartFunc = func(context.Context, rtcore.ExecRequest) (rtcore.Process, error) {
		return harnesstest.NewProcess(`{"type":"error","message":"model not found"}`+"\n", "boom", 1), nil
	}
	ch, _ := New(Options{}).Run(context.Background(), sb, rtcore.HarnessRequest{Prompt: "x"})
	evs := collect(t, ch)
	last := evs[len(evs)-1]
	if last.Kind != harness.EventError || !strings.Contains(last.Content, "model not found") || !strings.Contains(last.Content, "boom") {
		t.Fatalf("last = %+v", last)
	}
}

func TestAuthRejectsAnthropic(t *testing.T) {
	_, err := New(Options{}).Run(context.Background(), harnesstest.New("/w"), rtcore.HarnessRequest{Auth: map[string]string{"ANTHROPIC_API_KEY": "x"}})
	if err == nil {
		t.Fatal("expected error")
	}
}
