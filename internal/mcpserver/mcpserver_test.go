package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/api/apitest"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

type bearer struct {
	key  string
	base http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(r)
}

type mcpEnv struct {
	t      *testing.T
	store  *sqlstore.Store
	engine *apitest.Engine
	ts     *httptest.Server
	team   *core.Team
	editor apitest.Identity
	viewer apitest.Identity
	admin  apitest.Identity
	other  apitest.Identity
}

func newMCPEnv(t *testing.T, mutate ...func(*Options)) *mcpEnv {
	t.Helper()
	ctx := context.Background()
	store := testdb.New(t)
	engine := apitest.NewEngine(store, nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := Options{Store: store, Engine: engine, Compile: apitest.Compile, PublicURL: "https://factory.example.test", Logger: logger,
		Schemas: func() map[string]json.RawMessage {
			return map[string]json.RawMessage{"factory.schema.json": json.RawMessage(`{"type":"object"}`), "agent.schema.json": json.RawMessage(`{}`)}
		}}
	for _, m := range mutate {
		m(&opts)
	}
	srv := New(opts)
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	team := &core.Team{ID: core.NewID("team"), Name: "Acme"}
	_ = store.CreateTeam(ctx, team)
	other := &core.Team{ID: core.NewID("team"), Name: "Other"}
	_ = store.CreateTeam(ctx, other)
	e := &mcpEnv{t: t, store: store, engine: engine, ts: ts, team: team}
	mk := func(teamID string, role core.Role) apitest.Identity {
		id, err := apitest.SeedUser(ctx, store, teamID, role, api.NewAPIKey)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	e.admin, e.editor, e.viewer, e.other = mk(team.ID, core.RoleAdmin), mk(team.ID, core.RoleEditor), mk(team.ID, core.RoleUser), mk(other.ID, core.RoleAdmin)
	return e
}

func (e *mcpEnv) connect(key, path string) *mcp.ClientSession {
	e.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	tr := &mcp.StreamableClientTransport{Endpoint: e.ts.URL + path, HTTPClient: &http.Client{Transport: bearer{key, http.DefaultTransport}},
		DisableStandaloneSSE: true, MaxRetries: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, tr, nil)
	if err != nil {
		e.t.Fatalf("connect: %v", err)
	}
	e.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call invokes a tool and decodes its JSON text result. It fails the test on
// protocol errors and returns isError for tool errors.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s: empty content", name)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: non-JSON result %q", name, text)
	}
	return out, res.IsError
}

func mustCall(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	out, isErr := call(t, cs, name, args)
	if isErr {
		t.Fatalf("%s returned error: %v", name, out)
	}
	return out
}

func errCode(out map[string]any) string {
	m, _ := out["error"].(map[string]any)
	c, _ := m["code"].(string)
	return c
}

func TestAuthRequired(t *testing.T) {
	e := newMCPEnv(t)
	for _, key := range []string{"", "of_abcdefgh_" + strings.Repeat("x", 40)} {
		req, _ := http.NewRequest("POST", e.ts.URL+Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("key %q: status %d", key, res.StatusCode)
		}
	}
}

func TestToolsListed(t *testing.T) {
	e := newMCPEnv(t)
	cs := e.connect(e.viewer.Key, AliasPath)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"list_factories", "get_factory_file_schema", "validate_factory_files", "list_teams", "create_team", "join_team",
		"list_forge_repositories", "list_tracker_scopes", "start_connection", "get_connection_status",
		"create_factory", "list_work_items", "search_work_items", "get_work_item", "message_foreman", "get_conversation", "send_work_item",
		"list_notification_routes", "complete_work_item"}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
		if tl.Description == "" || tl.InputSchema == nil {
			t.Errorf("tool %s lacks description or schema", tl.Name)
		}
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing tool %s", w)
		}
	}
	if len(res.Tools) != len(want) {
		t.Errorf("got %d tools, want %d", len(res.Tools), len(want))
	}
}

func TestOnboardingTools(t *testing.T) {
	e := newMCPEnv(t, func(o *Options) {
		o.ListRepositories = func(_ context.Context, teamID, forge, q string) ([]Repository, error) {
			return []Repository{{CodeForge: forge, Owner: "acme", Name: "payments"}}, nil
		}
	})
	ctx := context.Background()
	cs := e.connect(e.editor.Key, Path)

	teams := mustCall(t, cs, "list_teams", nil)
	if m := teams["memberships"].([]any); len(m) != 1 || m[0].(map[string]any)["team_id"] != e.team.ID {
		t.Fatalf("teams = %v", teams)
	}
	if out, isErr := call(t, cs, "create_team", map[string]any{"name": "New", "confirmed": false}); !isErr || errCode(out) != api.CodeInvalidRequest {
		t.Fatalf("create_team unconfirmed = %v", out)
	}
	if out, isErr := call(t, cs, "create_team", map[string]any{"name": "New", "confirmed": true}); !isErr || errCode(out) != api.CodeConflict {
		t.Fatalf("create_team = %v", out)
	}
	if out := mustCall(t, cs, "join_team", map[string]any{"team_id": e.team.ID}); out["status"] != "already_member" {
		t.Fatal(out)
	}
	if out, isErr := call(t, cs, "join_team", map[string]any{"team_id": "team_x"}); !isErr {
		t.Fatal(out)
	}
	repos := mustCall(t, cs, "list_forge_repositories", map[string]any{"code_forge": "GITHUB"})
	if len(repos["repositories"].([]any)) != 1 {
		t.Fatal(repos)
	}
	if out, isErr := call(t, cs, "list_tracker_scopes", map[string]any{"provider": "linear"}); !isErr || errCode(out) != api.CodeIntegrationNotConfigured {
		t.Fatal(out)
	}

	// Connections: editors can't start new ones, admins get a pending flow, then it completes.
	if out, isErr := call(t, cs, "start_connection", map[string]any{"provider": "slack"}); !isErr || errCode(out) != api.CodeNotAuthorized {
		t.Fatal(out)
	}
	adm := e.connect(e.admin.Key, Path)
	conn := mustCall(t, adm, "start_connection", map[string]any{"provider": "slack"})
	if conn["status"] != "pending" || !strings.Contains(conn["authorize_url"].(string), "/integrations?connection="+conn["connection_id"].(string)+"&provider=slack") {
		t.Fatal(conn)
	}
	st := mustCall(t, adm, "get_connection_status", map[string]any{"connection_id": conn["connection_id"]})
	if st["status"] != "pending" {
		t.Fatal(st)
	}
	_ = e.store.UpsertIntegration(ctx, &core.Integration{ID: "int_1", TeamID: e.team.ID, Provider: "slack"})
	st = mustCall(t, adm, "get_connection_status", map[string]any{"connection_id": conn["connection_id"]})
	if st["status"] != "connected" || st["integration_id"] != "int_1" {
		t.Fatal(st)
	}
	if out := mustCall(t, cs, "start_connection", map[string]any{"provider": "slack"}); out["status"] != "connected" {
		t.Fatal(out)
	}
	if out, isErr := call(t, cs, "start_connection", map[string]any{"provider": "myspace"}); !isErr {
		t.Fatal(out)
	}
	otherSession := e.connect(e.other.Key, Path)
	if out, isErr := call(t, otherSession, "get_connection_status", map[string]any{"connection_id": conn["connection_id"]}); !isErr || errCode(out) != api.CodeResourceNotFound {
		t.Fatal(out)
	}

	// Create a factory.
	created := mustCall(t, cs, "create_factory", map[string]any{
		"name": "Payments", "repositories": []map[string]any{{"owner": "acme", "name": "payments"}},
		"integrations": []string{"slack"}, "agents": []string{"implement", "review"},
	})
	fac := created["factory"].(map[string]any)
	if fac["alias"] != "payments" || !strings.HasPrefix(created["dashboard_url"].(string), "https://factory.example.test/f/") {
		t.Fatal(created)
	}
	viewerSession := e.connect(e.viewer.Key, Path)
	if out, isErr := call(t, viewerSession, "create_factory", map[string]any{"name": "Nope"}); !isErr || errCode(out) != api.CodeNotAuthorized {
		t.Fatal(out)
	}
	if out, isErr := call(t, cs, "create_factory", map[string]any{"name": "Bad", "integrations": []string{"linear", "jira"}}); !isErr || errCode(out) != api.CodeInvalidRequest {
		t.Fatal(out)
	}
	fl := mustCall(t, cs, "list_factories", map[string]any{"search": "pay"})
	if len(fl["factories"].([]any)) != 1 {
		t.Fatal(fl)
	}
	if fl := mustCall(t, otherSession, "list_factories", nil); len(fl["factories"].([]any)) != 0 {
		t.Fatal("cross-team leak")
	}
}

func TestSchemaAndValidation(t *testing.T) {
	e := newMCPEnv(t)
	cs := e.connect(e.viewer.Key, Path)
	cat := mustCall(t, cs, "get_factory_file_schema", nil)
	if docs := cat["documents"].([]any); len(docs) != 2 || docs[0] != "agent.schema.json" {
		t.Fatal(cat)
	}
	one := mustCall(t, cs, "get_factory_file_schema", map[string]any{"document": "factory"})
	if one["schema"].(map[string]any)["type"] != "object" {
		t.Fatal(one)
	}
	if out, isErr := call(t, cs, "get_factory_file_schema", map[string]any{"document": "nope.schema.json"}); !isErr || errCode(out) != api.CodeResourceNotFound {
		t.Fatal(out)
	}
	v := mustCall(t, cs, "validate_factory_files", map[string]any{"files": apitest.MinimalFiles("x")})
	if v["valid"] != true {
		t.Fatal(v)
	}
	v = mustCall(t, cs, "validate_factory_files", map[string]any{"files": map[string]string{"factory.yaml": "name: x"}})
	if v["valid"] != false || len(v["errors"].([]any)) == 0 {
		t.Fatal(v)
	}
}

func TestWorkItemLifecycle(t *testing.T) {
	e := newMCPEnv(t)
	ctx := context.Background()
	cs := e.connect(e.editor.Key, Path)
	fac := mustCall(t, cs, "create_factory", map[string]any{"name": "Work", "repositories": []map[string]any{{"owner": "acme", "name": "api"}}, "integrations": []string{"linear"}})
	fid := fac["factory"].(map[string]any)["uid"].(string)

	routes := mustCall(t, cs, "list_notification_routes", map[string]any{"factory_id": fid})
	rs := routes["routes"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["id"] != "inbox" {
		t.Fatalf("routes = %v", routes)
	}
	_ = e.store.SetFactoryIntegrations(ctx, fid, []string{"linear"}, nil)
	if rs := mustCall(t, cs, "list_notification_routes", map[string]any{"factory_id": fid})["routes"].([]any); len(rs) != 2 {
		t.Fatalf("routes = %v", rs)
	}

	// New work item.
	sent := mustCall(t, cs, "send_work_item", map[string]any{"factory_id": fid, "title": "Fix flaky retry test",
		"note": "The webhook retry test flakes on CI. Fix the race and add a regression test.", "notify_route": "linear_issue"})
	workItemID := sent["work_item_id"].(string)
	if sent["status"] != "created" || !strings.Contains(sent["url"].(string), workItemID) {
		t.Fatal(sent)
	}
	wi, _ := e.store.GetWorkItem(ctx, workItemID)
	if wi.Labels["notify_route"] != "linear_issue" || wi.Source.Provider != "mcp" || wi.CreatorID != e.editor.User.ID {
		t.Fatalf("work item = %+v", wi)
	}
	if out, isErr := call(t, cs, "send_work_item", map[string]any{"factory_id": fid, "note": "x", "notify_route": "pager"}); !isErr {
		t.Fatal(out)
	}
	if out, isErr := call(t, cs, "send_work_item", map[string]any{"note": "x"}); !isErr || errCode(out) != api.CodeInvalidRequest {
		t.Fatal(out)
	}

	// List / search.
	lt := mustCall(t, cs, "list_work_items", map[string]any{"factory_id": fid, "creator": "me", "stage": "intake"})
	if len(lt["work_items"].([]any)) != 1 {
		t.Fatal(lt)
	}
	if out, isErr := call(t, cs, "list_work_items", map[string]any{"factory_id": fid, "stage": "dreaming"}); !isErr {
		t.Fatal(out)
	}
	st := mustCall(t, cs, "search_work_items", map[string]any{"query": "flaky"})
	if len(st["work_items"].([]any)) != 1 {
		t.Fatal(st)
	}

	// Get work item by ID, by run URL; start working.
	got := mustCall(t, cs, "get_work_item", map[string]any{"work_item": workItemID})
	if got["work_item"].(map[string]any)["id"] != workItemID || got["active_runs"].(float64) != 1 {
		t.Fatal(got)
	}
	runID := got["runs"].([]any)[0].(map[string]any)["run_id"].(string)
	byURL := mustCall(t, cs, "get_work_item", map[string]any{"work_item": "https://factory.example.test/runs/" + runID})
	if byURL["work_item"].(map[string]any)["id"] != workItemID {
		t.Fatal(byURL)
	}
	sw := mustCall(t, cs, "get_work_item", map[string]any{"work_item": workItemID, "start_working": true})
	setup := sw["local_setup"].(map[string]any)
	repos := setup["repositories"].([]any)
	if len(repos) != 1 {
		t.Fatalf("setup = %v", setup)
	}
	r0 := repos[0].(map[string]any)
	if r0["clone_url"] != "https://github.com/acme/api.git" || !strings.HasPrefix(r0["branch"].(string), "factory/") ||
		!strings.Contains(strings.Join(toStrings(r0["commands"]), "\n"), "git worktree add -b") {
		t.Fatalf("repo setup = %v", r0)
	}
	if sw["work_item"].(map[string]any)["local_owner"] != e.editor.User.ID {
		t.Fatal(sw["work_item"])
	}
	if len(setup["warnings"].([]any)) != 2 {
		t.Fatalf("warnings = %v", setup["warnings"])
	}

	// Conversation.
	mustCall(t, cs, "message_foreman", map[string]any{"work_item_id": workItemID, "message": "Picking this up locally."})
	_ = e.store.AppendRunMessage(ctx, &core.RunMessage{RunID: runID, Role: "assistant", Content: "Thanks, I'll pause."})
	_ = e.store.AppendRunMessage(ctx, &core.RunMessage{RunID: runID, Role: "tool_call", Content: "noise"})
	conv := mustCall(t, cs, "get_conversation", map[string]any{"work_item_id": workItemID})
	msgs := conv["messages"].([]any)
	if len(msgs) != 3 || msgs[2].(map[string]any)["content"] != "Thanks, I'll pause." {
		t.Fatalf("conversation = %v", conv)
	}
	if c := mustCall(t, cs, "get_conversation", map[string]any{"work_item_id": workItemID, "after": 2}); len(c["messages"].([]any)) != 1 {
		t.Fatal(c)
	}

	// Hand back with a PR.
	hb := mustCall(t, cs, "send_work_item", map[string]any{"work_item_id": workItemID, "branch": "factory/fix-retry",
		"pull_request_url": "https://github.com/acme/api/pull/42", "note": "Fixed the race; tests pass. Remaining: docs."})
	if hb["status"] != "handed_back" {
		t.Fatal(hb)
	}
	wi, _ = e.store.GetWorkItem(ctx, workItemID)
	if wi.LocalOwner != "" || len(wi.PullRequests) != 1 || wi.PullRequests[0].Number != 42 || wi.PullRequests[0].Branch != "factory/fix-retry" {
		t.Fatalf("after hand-back = %+v", wi)
	}
	fu := e.engine.CallsTo("FollowUp")
	if last := fu[len(fu)-1].Args[1].(string); !strings.Contains(last, "Work handed back") || !strings.Contains(last, "pull/42") {
		t.Fatalf("follow-up = %q", last)
	}
	if out, isErr := call(t, cs, "send_work_item", map[string]any{"work_item_id": workItemID, "note": "x", "pull_request_url": "https://example.test/nope"}); !isErr {
		t.Fatal(out)
	}

	// Resolve by PR URL, short ref, and branch.
	for _, ref := range []string{"https://github.com/acme/api/pull/42", "acme/api#42", "factory/fix-retry"} {
		got := mustCall(t, cs, "get_work_item", map[string]any{"work_item": ref})
		if got["work_item"].(map[string]any)["id"] != workItemID {
			t.Fatalf("ref %q resolved to %v", ref, got["work_item"])
		}
	}
	// Start working again now picks the existing PR branch.
	sw = mustCall(t, cs, "get_work_item", map[string]any{"work_item": workItemID, "start_working": true})
	r0 = sw["local_setup"].(map[string]any)["repositories"].([]any)[0].(map[string]any)
	if r0["branch"] != "factory/fix-retry" || r0["existing_branch"] != true {
		t.Fatalf("existing branch setup = %v", r0)
	}
	if out, isErr := call(t, cs, "get_work_item", map[string]any{"work_item": "no-such-branch"}); !isErr || errCode(out) != api.CodeResourceNotFound {
		t.Fatal(out)
	}

	// Other teams cannot see the work item.
	other := e.connect(e.other.Key, Path)
	if out, isErr := call(t, other, "get_work_item", map[string]any{"work_item": workItemID}); !isErr {
		t.Fatal(out)
	}

	// Complete.
	done := mustCall(t, cs, "complete_work_item", map[string]any{"work_item_id": workItemID, "note": "Merged"})
	if done["work_item"].(map[string]any)["stage"] != "complete" {
		t.Fatal(done)
	}
	if out, isErr := call(t, cs, "complete_work_item", map[string]any{"work_item_id": workItemID}); !isErr || errCode(out) != api.CodeConflict {
		t.Fatal(out)
	}
	if out, isErr := call(t, cs, "send_work_item", map[string]any{"work_item_id": workItemID, "note": "more"}); !isErr || errCode(out) != api.CodeConflict {
		t.Fatal(out)
	}
}

func TestResolveExternalReferences(t *testing.T) {
	e := newMCPEnv(t)
	ctx := context.Background()
	cs := e.connect(e.viewer.Key, Path)
	f := &core.Factory{ID: "fac_ext", TeamID: e.team.ID, Name: "ext", Alias: "ext", Source: core.DefinitionManaged, CreatedAt: time.Now()}
	_ = e.store.CreateFactory(ctx, f)
	mk := func(id string, src core.SourceRef) {
		_ = e.store.CreateWorkItem(ctx, &core.WorkItem{ID: id, FactoryID: f.ID, Title: id, Stage: core.StageBuilding, Source: src,
			SourceKey: src.Key(), CreatedAt: time.Now(), UpdatedAt: time.Now()})
	}
	mk("wi_slack", core.SourceRef{Provider: "slack", Channel: "C123", ThreadID: "1700000000.123456"})
	mk("wi_linear", core.SourceRef{Provider: "linear", ExternalID: "ENG-7"})
	mk("wi_jira", core.SourceRef{Provider: "jira", ExternalID: "OPS-9"})
	mk("wi_issue", core.SourceRef{Provider: "github", Repo: "acme/api", Number: 5})
	cases := map[string]string{
		"https://acme.slack.com/archives/C123/p1700000000123456": "wi_slack",
		"https://linear.app/acme/issue/ENG-7/fix-the-thing":      "wi_linear",
		"ENG-7": "wi_linear",
		"https://acme.atlassian.net/browse/OPS-9":                           "wi_jira",
		"https://github.com/acme/api/issues/5":                              "wi_issue",
		"https://factory.example.test/factories/fac_ext/work-items/wi_jira": "wi_jira",
		"https://factory.example.test/f/fac_ext/work/wi_issue":              "wi_issue",
	}
	for ref, want := range cases {
		got := mustCall(t, cs, "get_work_item", map[string]any{"work_item": ref})
		if id := got["work_item"].(map[string]any)["id"]; id != want {
			t.Errorf("%s -> %v, want %s", ref, id, want)
		}
	}
}

func TestParsePullRequestURL(t *testing.T) {
	cases := []struct {
		in    string
		forge string
		repo  string
		n     int
		ok    bool
	}{
		{"https://github.com/acme/api/pull/42", "GITHUB", "acme/api", 42, true},
		{"https://github.com/acme/api/pull/42/files", "", "", 0, false},
		{"https://gitlab.com/acme/platform/deploy/-/merge_requests/7", "GITLAB", "acme/platform/deploy", 7, true},
		{"https://dev.azure.com/org/proj/_git/repo/pullrequest/12", "AZURE_DEVOPS", "org/proj/repo", 12, true},
		{"https://github.com/acme/api/issues/42", "", "", 0, false},
		{"not a url", "", "", 0, false},
	}
	for _, c := range cases {
		ref, ok := parsePullRequestURL(c.in)
		if ok != c.ok || (ok && (ref.Forge != c.forge || ref.Repo != c.repo || ref.Number != c.n)) {
			t.Errorf("%s -> %+v %v", c.in, ref, ok)
		}
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// restrictFollowupsToStarter: hand-back through send_work_item must be
// refused for a user who did not start the work item, like message_foreman.
func TestHandBackRestrictFollowups(t *testing.T) {
	e := newMCPEnv(t)
	ctx := context.Background()
	ed := e.connect(e.editor.Key, Path)
	fac := mustCall(t, ed, "create_factory", map[string]any{"name": "Locked"})
	fid := fac["factory"].(map[string]any)["uid"].(string)
	files := apitest.MinimalFiles("Locked")
	files["factory.yaml"] += "restrictFollowupsToStarter: true\n"
	if v, err := e.engine.ApplyDefinition(ctx, fid, files, "test", "lock", ""); err != nil || !v.Valid {
		t.Fatalf("apply: %v %+v", err, v)
	}
	sent := mustCall(t, ed, "send_work_item", map[string]any{"factory_id": fid, "title": "T", "note": "do it"})
	wid := sent["work_item_id"].(string)

	other := e.connect(e.viewer.Key, Path)
	out, isErr := call(t, other, "message_foreman", map[string]any{"work_item_id": wid, "message": "hi"})
	if !isErr || errCode(out) != api.CodeNotAuthorized {
		t.Fatalf("control: message_foreman by non-starter = %v (isErr %v)", out, isErr)
	}
	before := len(e.engine.CallsTo("FollowUp"))
	out, isErr = call(t, other, "send_work_item", map[string]any{"work_item_id": wid, "note": "injected follow-up"})
	if !isErr {
		t.Fatalf("send_work_item hand-back by non-starter was accepted: %v; FollowUp calls %d -> %d",
			out, before, len(e.engine.CallsTo("FollowUp")))
	}
}
