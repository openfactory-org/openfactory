package runner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// fakeCoreHarness is a core.Harness that plays a list of events.
type fakeCoreHarness struct {
	mu     sync.Mutex
	script func(n int, req core.HarnessRequest) []core.HarnessEvent // n counts calls from 1
	err    error
	reqs   []core.HarnessRequest
	sbs    []core.Sandbox
}

func (f *fakeCoreHarness) Type() core.HarnessType { return core.HarnessClaude }

func (f *fakeCoreHarness) Run(_ context.Context, sb core.Sandbox, req core.HarnessRequest) (<-chan core.HarnessEvent, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.sbs = append(f.sbs, sb)
	n := len(f.reqs)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	evs := f.script(n, req)
	ch := make(chan core.HarnessEvent, len(evs))
	for _, ev := range evs {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

type captured struct {
	typ  runnerproto.EventType
	data json.RawMessage
}

type capture struct{ evs []captured }

func (c *capture) emit(typ runnerproto.EventType, data any) {
	raw, _ := json.Marshal(data)
	c.evs = append(c.evs, captured{typ, raw})
}

func (c *capture) types() []string {
	var out []string
	for _, e := range c.evs {
		out = append(out, string(e.typ))
	}
	return out
}

func (c *capture) all(typ runnerproto.EventType) []json.RawMessage {
	var out []json.RawMessage
	for _, e := range c.evs {
		if e.typ == typ {
			out = append(out, e.data)
		}
	}
	return out
}

func unmarshal[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func baseTurnRequest() TurnRequest {
	return TurnRequest{
		RunID:   "run_1",
		TurnID:  "t1",
		Input:   []runnerproto.InputItem{{Kind: runnerproto.KindUser, Content: "do it"}},
		Harness: runnerproto.Harness{Type: "claude", Model: "m1", ReasoningLevel: "high", SystemPrompt: "be brief"},
		WorkDir: "/work",
		Env:     []string{"A=1", "B=x=y", "NOEQUALS"},
	}
}

func TestCoreHarnessCapabilities(t *testing.T) {
	c := &CoreHarness{}
	if got := c.Capabilities(); !reflect.DeepEqual(got, []runnerproto.Capability{runnerproto.CapInterrupt}) {
		t.Fatalf("default caps %v", got)
	}
	c = &CoreHarness{Caps: []runnerproto.Capability{runnerproto.CapResume}}
	if got := c.Capabilities(); !reflect.DeepEqual(got, []runnerproto.Capability{runnerproto.CapResume}) {
		t.Fatalf("caps %v", got)
	}
}

func TestCoreHarnessEventsAndResume(t *testing.T) {
	f := &fakeCoreHarness{script: func(n int, req core.HarnessRequest) []core.HarnessEvent {
		if n == 1 {
			return []core.HarnessEvent{
				{Kind: "message", Role: "assistant", Content: "thinking", SessionID: "s1"},
				{Kind: "message", Role: "tool_result", Content: "odd role"},
				{Kind: "tool_call", Content: "ls {}", Data: map[string]any{"id": "c1", "name": "ls", "input": map[string]any{"path": "."}}},
				{Kind: "tool_result", Content: "a.txt", Data: map[string]any{"tool_use_id": "c1", "is_error": true}},
				{Kind: "tool_call", Content: "fallback name", Data: map[string]any{"call_id": "c2", "arguments": "x", "category": "shell"}},
				{Kind: "usage", Usage: &core.Usage{InputTokens: 10, OutputTokens: 2, CostUSD: 0.5}},
				{Kind: "log", Content: "a log line"},
				{Kind: "artifact", Content: "ignored"},
				{Kind: "usage", Usage: &core.Usage{InputTokens: 5, OutputTokens: 1, CostUSD: 0.25}, SessionID: "s1"},
				{Kind: "message", Content: "final words"},
				{Kind: "done", Content: "", SessionID: "s1", Usage: &core.Usage{InputTokens: 15, OutputTokens: 3}},
			}
		}
		return []core.HarnessEvent{
			{Kind: "usage", Usage: &core.Usage{InputTokens: 1, OutputTokens: 1}},
			{Kind: "done", Content: "second", SessionID: "s2", Usage: &core.Usage{InputTokens: 1, OutputTokens: 1}},
		}
	}}
	c := &CoreHarness{H: f}
	var cap1 capture
	res := c.Turn(context.Background(), baseTurnRequest(), cap1.emit)

	if res.Outcome != runnerproto.OutcomeDone || res.Result != "final words" {
		t.Fatalf("result %+v", res)
	}
	want := []string{"harness.session", "message", "message", "tool_call", "tool_result", "tool_call", "usage", "diagnostic", "usage", "message"}
	if !reflect.DeepEqual(cap1.types(), want) {
		t.Fatalf("types\n got %v\nwant %v", cap1.types(), want)
	}
	if s := unmarshal[runnerproto.HarnessSessionData](t, cap1.all(runnerproto.EventHarnessSession)[0]); s.SessionID != "s1" {
		t.Fatalf("session %+v", s)
	}
	msgs := cap1.all(runnerproto.EventMessage)
	if m := unmarshal[runnerproto.MessageData](t, msgs[0]); m.Role != "assistant" || m.Content != "thinking" {
		t.Fatalf("message 0 %+v", m)
	}
	if m := unmarshal[runnerproto.MessageData](t, msgs[1]); m.Role != "system" || m.Content != "odd role" {
		t.Fatalf("message 1 %+v", m)
	}
	if m := unmarshal[runnerproto.MessageData](t, msgs[2]); m.Role != "assistant" || m.Content != "final words" {
		t.Fatalf("message 2 %+v", m)
	}
	calls := cap1.all(runnerproto.EventToolCall)
	if d := unmarshal[runnerproto.ToolCallData](t, calls[0]); d.CallID != "c1" || d.Name != "ls" || string(d.Arguments) != `{"path":"."}` {
		t.Fatalf("tool call 0 %+v %s", d, d.Arguments)
	}
	if d := unmarshal[runnerproto.ToolCallData](t, calls[1]); d.CallID != "c2" || d.Name != "fallback name" || string(d.Arguments) != `"x"` || d.Category != "shell" {
		t.Fatalf("tool call 1 %+v", d)
	}
	if d := unmarshal[runnerproto.ToolResultData](t, cap1.all(runnerproto.EventToolResult)[0]); d.CallID != "c1" || d.Content != "a.txt" || !d.IsError {
		t.Fatalf("tool result %+v", d)
	}
	if d := unmarshal[runnerproto.DiagnosticData](t, cap1.all(runnerproto.EventDiagnostic)[0]); d.Code != "harness_log" || d.Message != "a log line" {
		t.Fatalf("diagnostic %+v", d)
	}
	// The usage of done repeats the usage events, so it adds nothing.
	us := cap1.all(runnerproto.EventUsage)
	if len(us) != 2 {
		t.Fatalf("%d usage events", len(us))
	}
	u1, u2 := unmarshal[runnerproto.UsageData](t, us[0]), unmarshal[runnerproto.UsageData](t, us[1])
	if u1.InputTokens != 10 || u1.OutputTokens != 2 || u1.CostUSD != 0.5 || !u1.Cumulative || u1.Coverage != "known" {
		t.Fatalf("usage 1 %+v", u1)
	}
	if u2.InputTokens != 15 || u2.OutputTokens != 3 || u2.CostUSD != 0.75 || !u2.Cumulative {
		t.Fatalf("usage 2 %+v", u2)
	}

	// The request of the first turn.
	r := f.reqs[0]
	if r.Run.ID != "run_1" || r.Run.SessionID != "" || r.Resume || r.Prompt != "do it" || r.SystemPrompt != "be brief" || r.WorkDir != "/work" {
		t.Fatalf("request %+v run %+v", r, r.Run)
	}
	if h := r.Run.Config.Harness; h.Type != core.HarnessClaude || h.Model != "m1" || h.ReasoningLevel != "high" {
		t.Fatalf("harness config %+v", h)
	}
	if !reflect.DeepEqual(r.Env, map[string]string{"A": "1", "B": "x=y"}) {
		t.Fatalf("env %v", r.Env)
	}
	if f.sbs[0].WorkDir() != "/work" || f.sbs[0].ID() != "local" {
		t.Fatalf("sandbox %q %q", f.sbs[0].WorkDir(), f.sbs[0].ID())
	}

	// The second turn resumes the session and continues the totals.
	var cap2 capture
	res = c.Turn(context.Background(), baseTurnRequest(), cap2.emit)
	if res.Outcome != runnerproto.OutcomeDone || res.Result != "second" {
		t.Fatalf("result 2 %+v", res)
	}
	r = f.reqs[1]
	if !r.Resume || r.Run.SessionID != "s1" {
		t.Fatalf("second request resume %v session %q", r.Resume, r.Run.SessionID)
	}
	if got := cap2.types(); !reflect.DeepEqual(got, []string{"usage", "harness.session"}) {
		t.Fatalf("types of turn 2: %v", got)
	}
	if u := unmarshal[runnerproto.UsageData](t, cap2.all(runnerproto.EventUsage)[0]); u.InputTokens != 16 || u.OutputTokens != 4 || u.CostUSD != 0.75 {
		t.Fatalf("turn 2 usage %+v", u)
	}
	if s := unmarshal[runnerproto.HarnessSessionData](t, cap2.all(runnerproto.EventHarnessSession)[0]); s.SessionID != "s2" {
		t.Fatalf("session %+v", s)
	}
}

func TestCoreHarnessDoneUsageWithoutUsageEvent(t *testing.T) {
	f := &fakeCoreHarness{script: func(int, core.HarnessRequest) []core.HarnessEvent {
		return []core.HarnessEvent{{Kind: "done", Content: "ok", Usage: &core.Usage{InputTokens: 7, OutputTokens: 1}}}
	}}
	var cp capture
	res := (&CoreHarness{H: f}).Turn(context.Background(), baseTurnRequest(), cp.emit)
	if res.Outcome != runnerproto.OutcomeDone || res.Result != "ok" {
		t.Fatalf("result %+v", res)
	}
	us := cp.all(runnerproto.EventUsage)
	if len(us) != 1 || unmarshal[runnerproto.UsageData](t, us[0]).InputTokens != 7 {
		t.Fatalf("usage events %s", us)
	}
}

func TestCoreHarnessResumeFromStartResponse(t *testing.T) {
	f := &fakeCoreHarness{script: func(int, core.HarnessRequest) []core.HarnessEvent {
		return []core.HarnessEvent{{Kind: "done", Content: "ok", SessionID: "old"}}
	}}
	req := baseTurnRequest()
	req.ResumeSessionID = "old"
	var cp capture
	(&CoreHarness{H: f}).Turn(context.Background(), req, cp.emit)
	if r := f.reqs[0]; !r.Resume || r.Run.SessionID != "old" {
		t.Fatalf("resume %v %q", r.Resume, r.Run.SessionID)
	}
	if len(cp.all(runnerproto.EventHarnessSession)) != 0 {
		t.Fatal("harness.session for the session that was resumed")
	}
}

func TestCoreHarnessErrors(t *testing.T) {
	cases := []struct {
		name     string
		events   []core.HarnessEvent
		startErr error
		code     string
		msg      string
	}{
		{"error event", []core.HarnessEvent{{Kind: "error", Content: "boom"}}, nil, "agent_failed", "boom"},
		{"auth error", []core.HarnessEvent{{Kind: "error", Content: "Invalid API key. Please check it"}}, nil, "harness_auth_failed", "Invalid API key. Please check it"},
		{"login error", []core.HarnessEvent{{Kind: "error", Content: "Not logged in - Please run /login"}}, nil, "harness_auth_failed", "Not logged in - Please run /login"},
		{"empty error", []core.HarnessEvent{{Kind: "error"}}, nil, "agent_failed", "harness reported an error"},
		{"no end", []core.HarnessEvent{{Kind: "message", Content: "hi"}}, nil, "agent_failed", "harness exited without completing the turn"},
		{"nothing", nil, nil, "agent_failed", "harness exited without completing the turn"},
		{"start error", nil, errors.New("prepare sandbox: disk full"), "agent_failed", "prepare sandbox: disk full"},
		{"start auth error", nil, errors.New("harness auth: cloud credentials require an AWS region"), "harness_auth_failed", "harness auth: cloud credentials require an AWS region"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCoreHarness{err: tc.startErr, script: func(int, core.HarnessRequest) []core.HarnessEvent { return tc.events }}
			var cp capture
			res := (&CoreHarness{H: f}).Turn(context.Background(), baseTurnRequest(), cp.emit)
			if res.Outcome != runnerproto.OutcomeError || res.FailureCode != tc.code || res.Message != tc.msg {
				t.Fatalf("result %+v", res)
			}
		})
	}
}

func TestCoreHarnessInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeCoreHarness{script: func(int, core.HarnessRequest) []core.HarnessEvent {
		cancel()
		return []core.HarnessEvent{{Kind: "message", Content: "late"}, {Kind: "error", Content: "context canceled"}}
	}}
	var cp capture
	res := (&CoreHarness{H: f}).Turn(ctx, baseTurnRequest(), cp.emit)
	if res.Outcome != runnerproto.OutcomeInterrupted || len(cp.evs) != 0 {
		t.Fatalf("result %+v events %v", res, cp.types())
	}
}

func TestCoreHarnessPassesInputs(t *testing.T) {
	f := &fakeCoreHarness{script: func(int, core.HarnessRequest) []core.HarnessEvent {
		return []core.HarnessEvent{{Kind: "done", Content: "ok"}}
	}}
	req := baseTurnRequest()
	req.Auth = map[string]string{"EXAMPLE_API_KEY": "k"}
	req.MCPHeaders = map[string]map[string]string{"srv": {"Authorization": "Bearer t"}}
	req.Harness.MCPServers = map[string]runnerproto.MCPServer{
		"srv":   {URL: "https://mcp.example/x", Auth: "session_token"},
		"local": {Command: "tool", Args: []string{"-v"}, Env: map[string]string{"K": "V"}},
	}
	req.Skills = []runnerproto.Skill{{Name: "review", Files: map[string]string{"SKILL.md": "be kind", "notes/a.md": "more"}}}
	var cp capture
	(&CoreHarness{H: f}).Turn(context.Background(), req, cp.emit)

	r := f.reqs[0]
	if !reflect.DeepEqual(r.Auth, req.Auth) {
		t.Fatalf("auth %v", r.Auth)
	}
	wantMCP := map[string]core.MCPServer{
		"srv":   {URL: "https://mcp.example/x", Headers: map[string]string{"Authorization": "Bearer t"}},
		"local": {Command: "tool", Args: []string{"-v"}, Env: map[string]string{"K": "V"}},
	}
	if !reflect.DeepEqual(r.MCPServers, wantMCP) {
		t.Fatalf("mcp servers %+v", r.MCPServers)
	}
	wantSkills := []core.Skill{{Name: "review", Content: "be kind", Files: map[string]string{"notes/a.md": "more"}, Path: ".openfactory/skills/review"}}
	if !reflect.DeepEqual(r.Skills, wantSkills) {
		t.Fatalf("skills %+v", r.Skills)
	}
	if r.SystemPrompt != "be brief" {
		t.Fatalf("system prompt %q", r.SystemPrompt)
	}
}

func TestCoreHarnessCutsLongDiagnostic(t *testing.T) {
	f := &fakeCoreHarness{script: func(int, core.HarnessRequest) []core.HarnessEvent {
		return []core.HarnessEvent{
			{Kind: "mystery", Content: strings.Repeat("x", 20<<10)},
			{Kind: "done", Content: "ok"},
		}
	}}
	var cp capture
	(&CoreHarness{H: f}).Turn(context.Background(), baseTurnRequest(), cp.emit)
	d := unmarshal[runnerproto.DiagnosticData](t, cp.all(runnerproto.EventDiagnostic)[0])
	if len(d.Message) != 16<<10 {
		t.Fatalf("diagnostic length %d", len(d.Message))
	}
}

func TestCoreHarnessDeclaredArtifacts(t *testing.T) {
	f := &fakeCoreHarness{script: func(int, core.HarnessRequest) []core.HarnessEvent {
		return []core.HarnessEvent{
			{Kind: "artifact", Data: map[string]any{"path": "out/a.txt"}},
			{Kind: "artifact", Data: map[string]any{"paths": []any{"b.txt", "c.txt"}}},
			{Kind: "artifact", Content: " d.txt "},
			{Kind: "done", Content: "ok"},
		}
	}}
	h := &CoreHarness{H: f}
	var cp capture
	h.Turn(context.Background(), baseTurnRequest(), cp.emit)
	got := strings.Join(h.TakeDeclaredArtifacts(), ",")
	if got != "out/a.txt,b.txt,c.txt,d.txt" {
		t.Errorf("declared = %q", got)
	}
	if again := h.TakeDeclaredArtifacts(); len(again) != 0 {
		t.Errorf("second take = %v", again)
	}
}
