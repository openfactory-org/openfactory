package orchestration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runauth"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

type fakeStore struct {
	core.Store
	runs   map[string]*core.Run
	agents []core.AgentConfig
	wi     *core.WorkItem
	mu     sync.Mutex
	msgs   map[string][]core.RunMessage
}

func (s *fakeStore) AppendRunMessage(_ context.Context, m *core.RunMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.msgs == nil {
		s.msgs = map[string][]core.RunMessage{}
	}
	m.Seq = int64(len(s.msgs[m.RunID]) + 1)
	s.msgs[m.RunID] = append(s.msgs[m.RunID], *m)
	return nil
}
func (s *fakeStore) ListRunMessages(_ context.Context, runID string, afterSeq int64) ([]core.RunMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.RunMessage
	for _, m := range s.msgs[runID] {
		if m.Seq > afterSeq {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *fakeStore) GetRun(_ context.Context, id string) (*core.Run, error) {
	r, ok := s.runs[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	cp := *r
	return &cp, nil
}
func (s *fakeStore) ListAgents(context.Context, string) ([]core.AgentConfig, error) {
	return s.agents, nil
}
func (s *fakeStore) GetWorkItem(_ context.Context, id string) (*core.WorkItem, error) {
	if s.wi == nil || s.wi.ID != id {
		return nil, core.ErrNotFound
	}
	cp := *s.wi
	return &cp, nil
}
func (s *fakeStore) ListRuns(_ context.Context, f core.RunFilter) ([]core.Run, error) {
	var out []core.Run
	for _, r := range s.runs {
		if (f.WorkItemID == "" || r.WorkItemID == f.WorkItemID) && (f.ParentRunID == "" || r.ParentRunID == f.ParentRunID) {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (s *fakeStore) ListInboxItems(context.Context, string, bool) ([]core.InboxItem, error) {
	return []core.InboxItem{{ID: "inb_1", WorkItemID: "wi_1", Kind: core.InboxQuestion, Title: "Q?"}}, nil
}
func (s *fakeStore) ListWorkItemEvents(context.Context, string) ([]core.WorkItemEvent, error) {
	return []core.WorkItemEvent{{Kind: "stage_changed", Data: json.RawMessage(`{"to":"triage"}`)}}, nil
}

type call struct {
	Method string
	Args   []any
}

type fakeEngine struct {
	core.Engine
	mu    sync.Mutex
	calls []call
	err   error
	// store and now make FollowUp store the message as the engine does.
	store core.Store
	now   func() time.Time
}

func (e *fakeEngine) record(m string, args ...any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, call{m, args})
	return e.err
}
func (e *fakeEngine) last() call {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.calls) == 0 {
		return call{}
	}
	return e.calls[len(e.calls)-1]
}
func (e *fakeEngine) RunAgents(_ context.Context, req core.RunAgentsRequest) ([]*core.Run, error) {
	if err := e.record("RunAgents", req); err != nil {
		return nil, err
	}
	var out []*core.Run
	for i, c := range req.AgentRunConfigs {
		out = append(out, &core.Run{ID: "run_c" + string(rune('0'+i)), Name: c.Name, AgentName: "triage", State: core.RunQueued})
	}
	return out, nil
}
func (e *fakeEngine) FollowUp(ctx context.Context, runID, msg, actor string) error {
	if err := e.record("FollowUp", runID, msg, actor); err != nil {
		return err
	}
	if e.store == nil {
		return nil
	}
	data, _ := json.Marshal(actor)
	return e.store.AppendRunMessage(ctx, &core.RunMessage{RunID: runID, Role: "user", Content: msg, Data: data, CreatedAt: e.now()})
}
func (e *fakeEngine) RequestHuman(_ context.Context, runID string, kind core.InboxKind, title, body string, options []string) (*core.InboxItem, error) {
	if err := e.record("RequestHuman", runID, kind, title, body, options); err != nil {
		return nil, err
	}
	return &core.InboxItem{ID: "inb_new", Kind: kind}, nil
}
func (e *fakeEngine) PostUpdate(_ context.Context, runID, text, kind string) error {
	return e.record("PostUpdate", runID, text, kind)
}
func (e *fakeEngine) SetStage(_ context.Context, wi string, st core.Stage, actor, note string) error {
	return e.record("SetStage", wi, st, actor, note)
}
func (e *fakeEngine) LinkPullRequest(_ context.Context, wi string, pr core.PullRequestRef) error {
	return e.record("LinkPullRequest", wi, pr)
}
func (e *fakeEngine) CompleteWorkItem(_ context.Context, wi, actor, note string) error {
	return e.record("CompleteWorkItem", wi, actor, note)
}

var testKeys = func() *runnertoken.StaticKeys {
	k, err := runnertoken.NewStaticKeys("k1", map[string][]byte{"k1": bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		panic(err)
	}
	return k
}()

// tokenFor returns the session token of session 1 of a run.
func tokenFor(runID string) string {
	tok, _, err := runnertoken.Mint(testKeys, runnertoken.Claims{Team: "t", Run: runID, Session: 1, Subject: "runner"}, time.Now())
	if err != nil {
		panic(err)
	}
	return tok
}

// newServer makes a Server that accepts the session tokens of tokenFor while
// the latest session of each run is latest (default 1).
func newServer(eng core.Engine, st core.Store, latest ...int) *Server {
	n := 1
	if len(latest) > 0 {
		n = latest[0]
	}
	srv := NewServer(eng, st)
	srv.SetVerifier(&runauth.Verifier{Keys: testKeys, Runner: fakeSessions{latest: n}})
	return srv
}

func newFixture(t *testing.T) (*fakeEngine, *fakeStore, *httptest.Server) {
	t.Helper()
	st := &fakeStore{
		runs: map[string]*core.Run{
			"run_root":  {ID: "run_root", FactoryID: "fac", WorkItemID: "wi_1", AgentName: "foreman", Config: core.AgentConfig{Name: "foreman", Type: core.AgentForeman}, State: core.RunRunning},
			"run_child": {ID: "run_child", FactoryID: "fac", WorkItemID: "wi_1", ParentRunID: "run_root", AgentName: "implement", Config: core.AgentConfig{Name: "implement", Type: core.AgentImplement}, State: core.RunIdle},
			"run_other": {ID: "run_other", FactoryID: "fac", WorkItemID: "wi_2", AgentName: "triage", State: core.RunIdle},
			"run_dead":  {ID: "run_dead", FactoryID: "fac", WorkItemID: "wi_1", State: core.RunCancelled},
		},
		agents: []core.AgentConfig{
			{ID: "agt_f", Name: "foreman", Type: core.AgentForeman},
			{ID: "agt_t", Name: "triage", Type: core.AgentTriage, Description: "Investigates"},
			{ID: "agt_i", Name: "implement", Type: core.AgentImplement},
			{ID: "agt_d", Name: "docs", Type: core.AgentCustom, SpawnableBy: []string{"implement"}},
			{ID: "agt_n", Name: "nobody", Type: core.AgentCustom, SpawnableBy: []string{}},
		},
		wi: &core.WorkItem{ID: "wi_1", FactoryID: "fac", Title: "Fix", Stage: core.StageTriage},
	}
	eng := &fakeEngine{}
	mux := http.NewServeMux()
	newServer(eng, st).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return eng, st, ts
}

type authRT struct{ token string }

func (a authRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, ts *httptest.Server, runID string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp/run/" + runID,
		HTTPClient:           &http.Client{Transport: authRT{tokenFor(runID)}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callText(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestAuthentication(t *testing.T) {
	_, _, ts := newFixture(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	tests := []struct {
		name, path, token string
		want              int
	}{
		{"missing token", "/mcp/run/run_root", "", http.StatusUnauthorized},
		{"wrong run token", "/mcp/run/run_root", tokenFor("run_child"), http.StatusUnauthorized},
		{"unknown run", "/mcp/run/run_nope", tokenFor("run_nope"), http.StatusNotFound},
	}
	for _, tt := range tests {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+tt.path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tt.token != "" {
			req.Header.Set("Authorization", "Bearer "+tt.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != tt.want || e.Error.Code == "" {
			t.Errorf("%s: status %d code %q, want %d", tt.name, resp.StatusCode, e.Error.Code, tt.want)
		}
	}
}

func TestToolsList(t *testing.T) {
	_, _, ts := newFixture(t)
	sess := connect(t, ts, "run_root")
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		names[tool.Name] = tool
	}
	for _, n := range []string{"run_agents", "list_agents", "ask_human", "request_approval", "post_update", "set_stage", "link_pull_request", "get_work_item", "complete_work_item", "follow_up_agent", "send_message"} {
		if names[n] == nil {
			t.Errorf("missing tool %s", n)
		}
	}
	schema, _ := json.Marshal(names["run_agents"].InputSchema)
	for _, f := range []string{"summary", "base_prompt", "remote", "metadata", "agent_run_configs", "agent_identity_uid"} {
		if !strings.Contains(string(schema), `"`+f+`"`) {
			t.Errorf("run_agents schema missing %s: %s", f, schema)
		}
	}
}

func TestListAgentsFiltersBySpawnableBy(t *testing.T) {
	_, _, ts := newFixture(t)
	out, isErr := callText(t, connect(t, ts, "run_root"), "list_agents", nil)
	if isErr || !strings.Contains(out, "agt_t") || !strings.Contains(out, "agt_i") || strings.Contains(out, "agt_d") || strings.Contains(out, "agt_f") || strings.Contains(out, "agt_n") {
		t.Fatalf("foreman list = %s", out)
	}
	out, _ = callText(t, connect(t, ts, "run_child"), "list_agents", nil)
	if !strings.Contains(out, "agt_d") || strings.Contains(out, "agt_t") {
		t.Fatalf("implement list = %s", out)
	}
}

func TestRunAgentsTool(t *testing.T) {
	eng, _, ts := newFixture(t)
	sess := connect(t, ts, "run_root")
	out, isErr := callText(t, sess, "run_agents", map[string]any{
		"summary": "s", "base_prompt": "b", "remote": map[string]any{}, "metadata": map[string]any{"k": "v"},
		"agent_run_configs": []map[string]any{{"name": "backend-implementation", "agent_identity_uid": "agt_i", "prompt": "p", "metadata": map[string]any{"setup_profile": "backend"}, "model": "m2"}},
	})
	if isErr || !strings.Contains(out, "backend-implementation") {
		t.Fatalf("out = %s", out)
	}
	req := eng.last().Args[0].(core.RunAgentsRequest)
	if req.ParentRunID != "run_root" || req.Summary != "s" || req.BasePrompt != "b" || req.Metadata["k"] != "v" {
		t.Fatalf("req = %+v", req)
	}
	c := req.AgentRunConfigs[0]
	if c.AgentIdentityUID != "agt_i" || c.Metadata["setup_profile"] != "backend" || c.Overrides == nil || c.Overrides.Model != "m2" {
		t.Fatalf("child = %+v", c)
	}
	eng.err = core.ErrForbidden
	out, isErr = callText(t, sess, "run_agents", map[string]any{"summary": "s", "agent_run_configs": []map[string]any{{"name": "x", "agent_identity_uid": "agt_t", "prompt": "p"}}})
	if !isErr || !strings.Contains(out, "forbidden") {
		t.Fatalf("expected tool error, got %v %s", isErr, out)
	}
}

func TestHumanAndWorkItemTools(t *testing.T) {
	eng, _, ts := newFixture(t)
	sess := connect(t, ts, "run_root")

	if out, isErr := callText(t, sess, "ask_human", map[string]any{"question": "Required?", "context": "API field"}); isErr || !strings.Contains(out, "inb_new") {
		t.Fatalf("ask_human = %s", out)
	}
	if c := eng.last(); c.Method != "RequestHuman" || c.Args[1] != core.InboxQuestion || c.Args[3] != "Required?\n\nAPI field" {
		t.Fatalf("call = %+v", c)
	}
	callText(t, sess, "request_approval", map[string]any{"title": "Spec", "summary": "Approve", "links": []string{"https://x/pull/1"}})
	if c := eng.last(); c.Args[1] != core.InboxApproval || !strings.Contains(c.Args[3].(string), "https://x/pull/1") {
		t.Fatalf("call = %+v", c)
	}
	callText(t, sess, "post_update", map[string]any{"text": "hi", "kind": "handoff"})
	if c := eng.last(); c.Method != "PostUpdate" || c.Args[2] != "handoff" {
		t.Fatalf("call = %+v", c)
	}
	if _, isErr := callText(t, sess, "post_update", map[string]any{"text": "hi", "kind": "shout"}); !isErr {
		t.Fatal("bad kind accepted")
	}
	callText(t, sess, "set_stage", map[string]any{"stage": "Planning", "note": "n"})
	if c := eng.last(); c.Method != "SetStage" || c.Args[0] != "wi_1" || c.Args[1] != core.StagePlanning || c.Args[2] != "run:run_root" {
		t.Fatalf("call = %+v", c)
	}
	if _, isErr := callText(t, sess, "set_stage", map[string]any{"stage": "done"}); !isErr {
		t.Fatal("bad stage accepted")
	}
	callText(t, sess, "link_pull_request", map[string]any{"url": "https://gitlab.com/g/sub/proj/-/merge_requests/9"})
	if c := eng.last(); c.Args[1].(core.PullRequestRef) != (core.PullRequestRef{Forge: "GITLAB", Repo: "g/sub/proj", Number: 9, URL: "https://gitlab.com/g/sub/proj/-/merge_requests/9"}) {
		t.Fatalf("pr = %+v", c.Args[1])
	}
	callText(t, sess, "complete_work_item", map[string]any{"note": "merged"})
	if c := eng.last(); c.Method != "CompleteWorkItem" || c.Args[2] != "merged" {
		t.Fatalf("call = %+v", c)
	}
	out, isErr := callText(t, sess, "get_work_item", nil)
	if isErr || !strings.Contains(out, `"stage": "triage"`) || !strings.Contains(out, "run_child") || !strings.Contains(out, "inb_1") {
		t.Fatalf("get_work_item = %s", out)
	}

	// Follow-ups only to descendants.
	if _, isErr := callText(t, sess, "follow_up_agent", map[string]any{"run_id": "run_child", "message": "revise"}); isErr {
		t.Fatal("follow-up to child rejected")
	}
	if c := eng.last(); c.Method != "FollowUp" || c.Args[0] != "run_child" || c.Args[2] != "run:run_root" {
		t.Fatalf("call = %+v", c)
	}
	if _, isErr := callText(t, sess, "follow_up_agent", map[string]any{"run_id": "run_other", "message": "x"}); !isErr {
		t.Fatal("follow-up to unrelated run accepted")
	}

	// A cancelled run.
	if out, isErr := callText(t, connect(t, ts, "run_dead"), "post_update", map[string]any{"text": "x"}); !isErr || !strings.Contains(out, "cancelled") {
		t.Fatalf("cancelled run: %s", out)
	}
}

func TestParsePullRequestURL(t *testing.T) {
	tests := []struct {
		url, forge, repo string
		n                int
		ok               bool
	}{
		{"https://github.com/acme/api/pull/42", "GITHUB", "acme/api", 42, true},
		{"https://github.com/acme/api/pull/42/files", "GITHUB", "acme/api", 42, true},
		{"https://gitlab.com/g/sub/proj/-/merge_requests/9", "GITLAB", "g/sub/proj", 9, true},
		{"https://dev.azure.com/org/proj/_git/repo/pullrequest/7", "AZURE_DEVOPS", "org/proj/repo", 7, true},
		{"https://example.com/nothing", "", "", 0, false},
		{"not a url", "", "", 0, false},
	}
	for _, tt := range tests {
		f, r, n, ok := ParsePullRequestURL(tt.url)
		if f != tt.forge || r != tt.repo || n != tt.n || ok != tt.ok {
			t.Errorf("ParsePullRequestURL(%q) = %q %q %d %v", tt.url, f, r, n, ok)
		}
	}
}

// fakeSessions is a runner store with a fixed active session and latest session.
type fakeSessions struct {
	core.RunnerStore
	latest int
}

func (f fakeSessions) GetRunSession(_ context.Context, runID string, n int) (*core.RunSession, error) {
	return &core.RunSession{RunID: runID, Session: n, State: core.SessionActive}, nil
}

func (f fakeSessions) LatestRunSession(_ context.Context, runID string) (*core.RunSession, error) {
	return &core.RunSession{RunID: runID, Session: f.latest}, nil
}

func (f fakeSessions) TouchRunSession(context.Context, string, int, time.Time) error { return nil }

// oldRunToken is the old run token: an HMAC of the run ID. The server refuses it.
func oldRunToken(runID string) string {
	m := hmac.New(sha256.New, []byte("k"))
	m.Write([]byte("run:" + runID))
	return hex.EncodeToString(m.Sum(nil))
}

func TestSessionTokenAuthentication(t *testing.T) {
	st := &fakeStore{runs: map[string]*core.Run{"run_root": {ID: "run_root", State: core.RunRunning}}}
	mint := func(run string, session int) string {
		tok, _, err := runnertoken.Mint(testKeys, runnertoken.Claims{Team: "t", Run: run, Session: session, Subject: "runner"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	for _, tt := range []struct {
		name   string
		latest int
		token  string
		want   int
		code   string
	}{
		{"active session", 1, mint("run_root", 1), http.StatusOK, ""},
		{"hmac token", 1, oldRunToken("run_root"), http.StatusUnauthorized, "unauthorized"},
		{"superseded session", 2, mint("run_root", 1), http.StatusConflict, "session_superseded"},
		{"other run", 1, mint("run_child", 1), http.StatusUnauthorized, "unauthorized"},
	} {
		mux := http.NewServeMux()
		srv := newServer(&fakeEngine{}, st, tt.latest)
		srv.Register(mux)
		ts := httptest.NewServer(mux)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp/run/run_root", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+tt.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var e struct {
			Error struct{ Code string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		ts.Close()
		if resp.StatusCode != tt.want || e.Error.Code != tt.code {
			t.Errorf("%s: status %d code %q, want %d %q", tt.name, resp.StatusCode, e.Error.Code, tt.want, tt.code)
		}
	}
}

// A session token opens an MCP session on /mcp/run/{runId}.
func TestSessionTokenConnects(t *testing.T) {
	st := &fakeStore{runs: map[string]*core.Run{"run_root": {ID: "run_root", State: core.RunRunning}}}
	tok := tokenFor("run_root")
	mux := http.NewServeMux()
	newServer(&fakeEngine{}, st).Register(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	sess, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp/run/run_root",
		HTTPClient:           &http.Client{Transport: authRT{tok}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer sess.Close()
	if _, err := sess.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}
