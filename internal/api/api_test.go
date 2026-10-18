package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/api/apitest"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

type testEnv struct {
	t      *testing.T
	store  *sqlstore.Store
	engine *apitest.Engine
	bus    *apitest.Bus
	srv    *Server
	ts     *httptest.Server
	team   *core.Team
	admin  apitest.Identity
	editor apitest.Identity
	viewer apitest.Identity
	other  apitest.Identity // admin of another team

	runFac string // factory of the runs that tests store directly
}

func newTestEnv(t *testing.T, mutate ...func(*Options)) *testEnv {
	t.Helper()
	ctx := context.Background()
	store := testdb.New(t)
	bus := apitest.NewBus()
	engine := apitest.NewEngine(store, bus)
	opts := Options{
		Store: store, Engine: engine, Secrets: &apitest.Secrets{Store: store}, Compile: apitest.Compile, Bus: bus,
		PublicURL:    "https://factory.example.test/",
		SSEHeartbeat: 20 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Schemas: func() map[string]json.RawMessage {
			return map[string]json.RawMessage{"factory.schema.json": json.RawMessage(`{"type":"object"}`)}
		},
		TestFilter: func(pattern, body json.RawMessage) (bool, error) {
			var p, b map[string]any
			if err := json.Unmarshal(pattern, &p); err != nil {
				return false, err
			}
			_ = json.Unmarshal(body, &b)
			for k, v := range p {
				vals, _ := v.([]any)
				ok := false
				for _, x := range vals {
					ok = ok || x == b[k]
				}
				if !ok {
					return false, nil
				}
			}
			return true, nil
		},
	}
	for _, m := range mutate {
		m(&opts)
	}
	srv := New(opts)
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	team := &core.Team{ID: core.NewID("team"), Name: "Acme"}
	other := &core.Team{ID: core.NewID("team"), Name: "Other"}
	_ = store.CreateTeam(ctx, team)
	_ = store.CreateTeam(ctx, other)
	e := &testEnv{t: t, store: store, engine: engine, bus: bus, srv: srv, ts: ts, team: team}
	mk := func(teamID string, role core.Role) apitest.Identity {
		id, err := apitest.SeedUser(ctx, store, teamID, role, NewAPIKey)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	e.admin, e.editor, e.viewer = mk(team.ID, core.RoleAdmin), mk(team.ID, core.RoleEditor), mk(team.ID, core.RoleUser)
	e.other = mk(other.ID, core.RoleAdmin)
	return e
}

type resp struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

func (e *testEnv) do(method, path, key string, body any) resp {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rdr = strings.NewReader(b)
		default:
			data, _ := json.Marshal(b)
			rdr = bytes.NewReader(data)
		}
	}
	req, _ := http.NewRequest(method, e.ts.URL+BasePath+path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := resp{Status: res.StatusCode, Header: res.Header, Raw: raw}
	_ = json.Unmarshal(raw, &out.Body)
	return out
}

func (e *testEnv) must(r resp, status int) resp {
	e.t.Helper()
	if r.Status != status {
		e.t.Fatalf("status = %d, want %d; body: %s", r.Status, status, r.Raw)
	}
	return r
}

func errCode(r resp) string {
	c, _ := r.Body["code"].(string)
	return c
}

// createFactory creates a managed factory with the default definition.
func (e *testEnv) createFactory(name string) string {
	e.t.Helper()
	r := e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{
		"name": name, "repositories": []map[string]string{{"owner": "acme", "name": "api"}},
	}), http.StatusCreated)
	return r.Body["factory"].(map[string]any)["uid"].(string)
}

// runFactory returns the ID of a factory of the test team for runs that a
// test stores directly. The store requires each run to have a factory.
func (e *testEnv) runFactory() string {
	e.t.Helper()
	if e.runFac == "" {
		f := &core.Factory{ID: core.NewID("fac"), TeamID: e.team.ID, Name: "runs"}
		if err := e.store.CreateFactory(context.Background(), f); err != nil {
			e.t.Fatal(err)
		}
		e.runFac = f.ID
	}
	return e.runFac
}

// runBody is the body of POST /runs for a run of the triage agent of the
// factory fid.
func runBody(fid, prompt string) map[string]any {
	return map[string]any{"prompt": prompt, "agent_identity_uid": "agt_" + fid + "_triage"}
}

func str(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

// list returns the items of a list response. key is the member of a body
// that is not in the list form (for example "messages"); a list body has
// "items".
func list(m map[string]any, key string) []any {
	if l, ok := m[key].([]any); ok {
		return l
	}
	l, _ := m["items"].([]any)
	return l
}

// pageInfo returns has_next_page and next_cursor of a list response.
func pageInfo(m map[string]any) (bool, string) {
	pi, _ := m["page_info"].(map[string]any)
	more, _ := pi["has_next_page"].(bool)
	next, _ := pi["next_cursor"].(string)
	return more, next
}

func nextCursor(m map[string]any) any {
	if _, n := pageInfo(m); n != "" {
		return n
	}
	return nil
}

// ---------- keys & auth ----------

func TestGenerateAndParseKey(t *testing.T) {
	g, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(g.Key, "of_"+g.Prefix+"_") {
		t.Fatalf("key %q lacks prefix %q", g.Key, g.Prefix)
	}
	if p, ok := ParseKey(g.Key); !ok || p != g.Prefix {
		t.Fatalf("ParseKey(%q) = %q, %v", g.Key, p, ok)
	}
	if g.Hash != HashKey(g.Key) || len(g.Hash) != 64 {
		t.Fatalf("bad hash %q", g.Hash)
	}
	for _, bad := range []string{"", "of_", "wk-123", "of_ABCDEFGH_" + strings.Repeat("a", 40), "of_abcdefgh_short",
		"of_abcdefgh_" + strings.Repeat("!", 40), "xx_abcdefgh_" + strings.Repeat("a", 40)} {
		if _, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}

func TestAuthentication(t *testing.T) {
	e := newTestEnv(t)
	// Change the last character of the secret; pick a replacement that differs from it.
	tampered := e.viewer.Key[:len(e.viewer.Key)-1] + "Z"
	if strings.HasSuffix(e.viewer.Key, "Z") {
		tampered = e.viewer.Key[:len(e.viewer.Key)-1] + "Y"
	}
	cases := []struct {
		name, header string
		status       int
		code         string
	}{
		{"missing", "", 401, CodeAuthenticationRequired},
		{"wrong scheme", "Basic abc", 401, CodeAuthenticationRequired},
		{"malformed key", "Bearer nope", 401, CodeAuthenticationRequired},
		{"unknown key", "Bearer of_abcdefgh_" + strings.Repeat("A", 40), 401, CodeAuthenticationRequired},
		{"tampered secret", "Bearer " + tampered, 401, CodeAuthenticationRequired},
		{"valid", "Bearer " + e.viewer.Key, 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/me", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != c.status {
				t.Fatalf("status %d, want %d", res.StatusCode, c.status)
			}
			if c.code != "" {
				var p map[string]any
				_ = json.NewDecoder(res.Body).Decode(&p)
				if p["code"] != c.code || str(p, "error", "code") != c.code {
					t.Fatalf("problem = %v", p)
				}
				if ct := res.Header.Get("Content-Type"); ct != "application/problem+json" {
					t.Fatalf("content-type %q", ct)
				}
				if p["instance"] != "/api/v1/me" || p["status"].(float64) != 401 || p["retryable"] != false || p["request_id"] == nil || p["trace_id"] != nil {
					t.Fatalf("problem fields = %v", p)
				}
			}
		})
	}
}

func TestRoleMatrix(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("payments")
	cases := []struct {
		role         string
		key          string
		method, path string
		body         any
		want         int
	}{
		{"viewer can list", e.viewer.Key, "GET", "/factories", nil, 200},
		{"viewer can dispatch", e.viewer.Key, "POST", "/factories/" + fid + "/runs", map[string]any{"prompt": "x"}, 201},
		{"viewer cannot create factory", e.viewer.Key, "POST", "/factories", map[string]any{"name": "x"}, 403},
		{"viewer cannot edit definition", e.viewer.Key, "PUT", "/factories/" + fid + "/definition", map[string]any{"files": map[string]string{}}, 403},
		{"viewer cannot create secret", e.viewer.Key, "POST", "/secrets", map[string]any{"name": "A", "value": "b"}, 403},
		{"editor cannot delete factory", e.editor.Key, "DELETE", "/factories/" + fid, nil, 403},
		{"editor cannot create user", e.editor.Key, "POST", "/users", map[string]any{"email": "a@b.test"}, 403},
		{"editor cannot create integration", e.editor.Key, "POST", "/integrations", map[string]any{"provider": "slack"}, 403},
		{"other team cannot see factory", e.other.Key, "GET", "/factories/" + fid, nil, 404},
		{"admin can delete factory", e.admin.Key, "DELETE", "/factories/" + fid, nil, 204},
	}
	for _, c := range cases {
		t.Run(c.role, func(t *testing.T) {
			r := e.do(c.method, c.path, c.key, c.body)
			if r.Status != c.want {
				t.Fatalf("status %d, want %d: %s", r.Status, c.want, r.Raw)
			}
			if c.want == 403 && errCode(r) != CodeNotAuthorized {
				t.Fatalf("code %q", errCode(r))
			}
		})
	}
}

func TestBootstrap(t *testing.T) {
	store := testdb.New(t)
	srv := New(Options{Store: store, Engine: apitest.NewEngine(store, nil)})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	post := func(body string) (*http.Response, map[string]any) {
		res, err := http.Post(ts.URL+"/api/v1/bootstrap", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(res.Body).Decode(&m)
		return res, m
	}
	if res, _ := post(`{"team_name":"Acme"}`); res.StatusCode != 400 {
		t.Fatalf("missing email: %d", res.StatusCode)
	}
	res, m := post(`{"team_name":"Acme","admin_email":"Admin@Example.test"}`)
	if res.StatusCode != 201 {
		t.Fatalf("bootstrap: %d %v", res.StatusCode, m)
	}
	key := m["key"].(string)
	if str(m, "user", "role") != "admin" || str(m, "user", "email") != "admin@example.test" {
		t.Fatalf("user = %v", m["user"])
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	me, _ := http.DefaultClient.Do(req)
	me.Body.Close()
	if me.StatusCode != 200 {
		t.Fatalf("me with bootstrap key: %d", me.StatusCode)
	}
	if res, m := post(`{"team_name":"Again","admin_email":"x@example.test"}`); res.StatusCode != 409 || m["code"] != CodeConflict {
		t.Fatalf("second bootstrap: %d %v", res.StatusCode, m)
	}
}

// ---------- factories ----------

func TestFactoryLifecycle(t *testing.T) {
	e := newTestEnv(t)
	r := e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{
		"name":         "Payments Factory",
		"repositories": []map[string]string{{"owner": "acme", "name": "payments"}},
		"integrations": []map[string]any{{"type": "slack"}, {"type": "linear"}},
		"agents":       []string{"triage", "implement"},
	}), 201)
	fid := str(r.Body, "factory", "uid")
	if str(r.Body, "factory", "alias") != "payments-factory" {
		t.Fatalf("alias = %q", str(r.Body, "factory", "alias"))
	}
	if !strings.HasPrefix(str(r.Body, "factory", "url"), "https://factory.example.test/f/") {
		t.Fatalf("url = %q", str(r.Body, "factory", "url"))
	}
	def := e.must(e.do("GET", "/factories/"+fid+"/definition", e.viewer.Key, nil), 200)
	files := def.Body["files"].(map[string]any)
	for _, want := range []string{"factory.yaml", "agents/foreman/agent.md", "agents/triage/agent.md", "agents/implement/agent.md"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s in %v", want, files)
		}
	}
	if _, ok := files["agents/review/agent.md"]; ok {
		t.Error("review agent should not be generated")
	}
	if fy := files["factory.yaml"].(string); !strings.Contains(fy, "type: linear") || !strings.Contains(fy, "codeForge: GITHUB") {
		t.Errorf("factory.yaml = %s", fy)
	}

	// Errors.
	e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "dup", "alias": "payments-factory"}), 409)
	bad := e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "x", "integrations": []map[string]any{{"type": "linear"}, {"type": "jira"}}}), 400)
	if errCode(bad) != CodeInvalidRequest {
		t.Fatal(bad.Body)
	}
	e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "x", "agents": []string{"wizard"}}), 400)
	e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "x", "alias": "bad/alias"}), 400)
	e.must(e.do("POST", "/factories", e.editor.Key, `{"name":`), 400)
	inv := e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "x", "files": map[string]string{"factory.yaml": "schemaVersion: v2\nname: x\n"}}), 400)
	if len(list(inv.Body, "errors")) == 0 {
		t.Fatalf("expected validation errors: %s", inv.Raw)
	}

	// Files-based creation derives the name from factory.yaml.
	r = e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"definition": map[string]any{"files": apitest.MinimalFiles("from-files")}}), 201)
	if str(r.Body, "factory", "name") != "from-files" {
		t.Fatalf("name = %q", str(r.Body, "factory", "name"))
	}

	// Update rewrites factory.yaml.
	r = e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"name": "Payments", "description": "Handles payments"}), 200)
	if str(r.Body, "name") != "Payments" || str(r.Body, "description") != "Handles payments" {
		t.Fatalf("patched = %v", r.Body)
	}
	def = e.must(e.do("GET", "/factories/"+fid+"/definition", e.viewer.Key, nil), 200)
	if fy := def.Body["files"].(map[string]any)["factory.yaml"].(string); !strings.Contains(fy, "name: Payments\n") || !strings.Contains(fy, "description: Handles payments") {
		t.Fatalf("factory.yaml after patch:\n%s", fy)
	}
	e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"git_branch": "prod"}), 422)

	// Agents.
	ag := e.must(e.do("GET", "/factories/"+fid+"/agents", e.viewer.Key, nil), 200)
	if len(list(ag.Body, "agents")) != 3 {
		t.Fatalf("agents = %v", ag.Body)
	}
	e.must(e.do("GET", "/factories/"+fid+"/agents/foreman", e.viewer.Key, nil), 200)
	e.must(e.do("GET", "/factories/"+fid+"/agents/nobody", e.viewer.Key, nil), 404)
	e.must(e.do("GET", "/factories/"+fid+"/automations", e.viewer.Key, nil), 200)

	// Delete.
	e.must(e.do("DELETE", "/factories/"+fid, e.admin.Key, nil), 204)
	e.must(e.do("GET", "/factories/"+fid, e.admin.Key, nil), 404)
}

func TestFactoryListPaginationAndSearch(t *testing.T) {
	e := newTestEnv(t)
	for _, n := range []string{"alpha", "beta", "gamma", "payments-api"} {
		e.createFactory(n)
		time.Sleep(time.Millisecond)
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		path := "/factories?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := e.must(e.do("GET", path, e.viewer.Key, nil), 200)
		pages++
		for _, f := range list(r.Body, "factories") {
			id := f.(map[string]any)["uid"].(string)
			if seen[id] {
				t.Fatalf("duplicate %s", id)
			}
			seen[id] = true
		}
		_, cursor = pageInfo(r.Body)
		if cursor == "" {
			if more, _ := pageInfo(r.Body); more {
				t.Fatal("has_more should be false")
			}
			break
		}
	}
	if len(seen) != 4 || pages != 2 {
		t.Fatalf("seen %d factories over %d pages", len(seen), pages)
	}
	r := e.must(e.do("GET", "/factories?search=PAYMENTS", e.viewer.Key, nil), 200)
	if fs := list(r.Body, "factories"); len(fs) != 1 || fs[0].(map[string]any)["name"] != "payments-api" {
		t.Fatalf("search = %v", r.Body)
	}
	e.must(e.do("GET", "/factories?cursor=!!!", e.viewer.Key, nil), 400)
	// Other teams see nothing.
	r = e.must(e.do("GET", "/factories", e.other.Key, nil), 200)
	if len(list(r.Body, "factories")) != 0 {
		t.Fatal("cross-team leak")
	}
}

func TestDefinitionEndpoints(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("defs")
	cur := e.must(e.do("GET", "/factories/"+fid+"/definition", e.viewer.Key, nil), 200)
	baseVersion := str(cur.Body, "version", "id")
	files := apitest.MinimalFiles("defs")
	files["machines/linux.yaml"] = "os: linux\narch: x86_64\n"

	e.must(e.do("POST", "/factories/"+fid+"/definition/validate", e.viewer.Key, map[string]any{"files": map[string]string{"factory.yaml": "name: x"}}), 200)
	v := e.must(e.do("POST", "/factories/"+fid+"/definition/validate", e.viewer.Key, map[string]any{"files": files}), 200)
	if v.Body["valid"] != true || len(list(v.Body["summary"].(map[string]any), "machines")) != 1 {
		t.Fatalf("validate = %s", v.Raw)
	}
	inv := e.must(e.do("POST", "/factories/"+fid+"/definition/validate", e.viewer.Key, map[string]any{"files": map[string]string{"factory.yaml": "name: x"}}), 200)
	if inv.Body["valid"] != false || len(list(inv.Body, "errors")) == 0 {
		t.Fatalf("invalid validate = %s", inv.Raw)
	}

	e.must(e.do("PUT", "/factories/"+fid+"/definition", e.editor.Key, map[string]any{"files": files, "base_version_id": "defv_stale"}), 409)
	bad := e.must(e.do("PUT", "/factories/"+fid+"/definition", e.editor.Key, map[string]any{"files": map[string]string{"factory.yaml": "schemaVersion: v1\nname: x\n"}}), 400)
	if len(list(bad.Body, "errors")) == 0 {
		t.Fatalf("expected errors: %s", bad.Raw)
	}
	put := e.must(e.do("PUT", "/factories/"+fid+"/definition", e.editor.Key, map[string]any{"files": files, "message": "add machine", "base_version_id": baseVersion}), 200)
	newVersion := str(put.Body, "version", "id")
	if newVersion == "" || newVersion == baseVersion {
		t.Fatalf("put = %s", put.Raw)
	}

	vs := e.must(e.do("GET", "/factories/"+fid+"/definition/versions?limit=1", e.viewer.Key, nil), 200)
	if len(list(vs.Body, "versions")) != 1 || nextCursor(vs.Body) == nil || vs.Body["active_version_id"] != newVersion {
		t.Fatalf("versions = %s", vs.Raw)
	}
	if files := list(vs.Body, "versions")[0].(map[string]any)["files"]; files != nil {
		t.Fatal("files should be omitted by default")
	}
	e.must(e.do("GET", "/factories/"+fid+"/definition/versions/"+baseVersion, e.viewer.Key, nil), 200)
	e.must(e.do("GET", "/factories/"+fid+"/definition/versions/defv_nope", e.viewer.Key, nil), 404)

	rn := e.must(e.do("GET", "/machines?factory_id="+fid, e.viewer.Key, nil), 200)
	if rs := list(rn.Body, "machines"); len(rs) != 1 || rs[0].(map[string]any)["name"] != "linux" {
		t.Fatalf("machines = %s", rn.Raw)
	}

	// Git-backed factories are read-only through the API.
	g := e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "git-backed", "source": "git", "git_repo": "github.com/acme/def"}), 201)
	gid := str(g.Body, "factory", "uid")
	if str(g.Body, "factory", "git_branch") != "main" {
		t.Fatalf("git factory = %s", g.Raw)
	}
	r := e.must(e.do("PUT", "/factories/"+gid+"/definition", e.editor.Key, map[string]any{"files": files}), 422)
	if errCode(r) != CodeOperationNotSupported {
		t.Fatal(r.Body)
	}
}

func TestDispatch(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("dispatch")
	r := e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, map[string]any{
		"prompt":     "Investigate and fix the flaky payment webhook retry test\nmore detail",
		"ticket_ref": "linear:PAY-123", "ticket_url": "https://linear.example.test/PAY-123",
		"metadata": map[string]string{"team": "payments"},
	}), 201)
	if r.Body["run_id"] == "" || r.Body["state"] != StateQueued || !strings.Contains(str(r.Body, "work_item_url"), "/f/"+str(r.Body, "factory_id")+"/work/") {
		t.Fatalf("dispatch = %s", r.Raw)
	}
	calls := e.engine.CallsTo("Dispatch")
	req := calls[len(calls)-1].Args[0].(core.DispatchRequest)
	if req.Source.Provider != "linear" || req.Source.ExternalID != "PAY-123" || req.Source.URL == "" ||
		req.Title != "Investigate and fix the flaky payment webhook retry test" || req.CreatorID != e.viewer.User.ID || req.TriggeredBy != "api" {
		t.Fatalf("dispatch request = %+v", req)
	}
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"missing prompt", map[string]any{"title": "x"}},
		{"bad ticket ref", map[string]any{"prompt": "x", "ticket_ref": "PAY-123"}},
		{"bad metadata key", map[string]any{"prompt": "x", "metadata": map[string]string{"bad key": "v"}}},
		{"metadata collision", map[string]any{"prompt": "x", "metadata": map[string]string{"a.b": "1", "a-b": "2"}}},
		{"metadata too long", map[string]any{"prompt": "x", "metadata": map[string]string{"k": strings.Repeat("v", 257)}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, c.body), 400)
		})
	}
	e.must(e.do("POST", "/factories/fac_nope/runs", e.viewer.Key, map[string]any{"prompt": "x"}), 404)
}

func TestValidateMetadataLimits(t *testing.T) {
	many := map[string]string{}
	for i := 0; i < 21; i++ {
		many[strings.Repeat("k", i+1)] = "v"
	}
	if err := ValidateMetadata(many); err == nil {
		t.Fatal("21 keys accepted")
	}
	if err := ValidateMetadata(map[string]string{"k": "a\x00b"}); err == nil {
		t.Fatal("NUL accepted")
	}
	if got := MetadataEnvName("ticket.id-x"); got != "OF_METADATA_TICKET_ID_X" {
		t.Fatalf("env = %s", got)
	}
}

// ---------- work items & inbox ----------

func TestWorkItems(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("work")
	d := e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, map[string]any{"prompt": "Do the thing"}), 201)
	wid := str(d.Body, "work_item_id")
	e.must(e.do("POST", "/factories/"+fid+"/runs", e.editor.Key, map[string]any{"prompt": "Another thing"}), 201)

	l := e.must(e.do("GET", "/factories/"+fid+"/work-items?creator=me", e.viewer.Key, nil), 200)
	if items := list(l.Body, "work_items"); len(items) != 1 || items[0].(map[string]any)["id"] != wid {
		t.Fatalf("list = %s", l.Raw)
	}
	l = e.must(e.do("GET", "/factories/"+fid+"/work-items?stage=intake&limit=1", e.viewer.Key, nil), 200)
	if len(list(l.Body, "work_items")) != 1 || nextCursor(l.Body) == nil {
		t.Fatalf("paged list = %s", l.Raw)
	}
	e.must(e.do("GET", "/factories/"+fid+"/work-items?stage=bogus", e.viewer.Key, nil), 400)

	_ = e.store.CreateInboxItem(ctx, &core.InboxItem{ID: "inb_1", FactoryID: fid, WorkItemID: wid, Kind: core.InboxApproval, Title: "Approve spec", Options: []string{"Approve", "Reject"}})
	g := e.must(e.do("GET", "/work-items/"+wid, e.viewer.Key, nil), 200)
	if len(list(g.Body, "runs")) != 1 || len(list(g.Body, "inbox")) != 1 || g.Body["timeline"] == nil {
		t.Fatalf("detail = %s", g.Raw)
	}
	e.must(e.do("GET", "/work-items/"+wid, e.other.Key, nil), 404)

	m := e.must(e.do("POST", "/work-items/"+wid+"/messages", e.viewer.Key, map[string]any{"message": "Use the v2 API"}), 202)
	if len(e.engine.CallsTo("FollowUp")) != 1 || m.Body["run_id"] == "" {
		t.Fatalf("follow-up not sent: %s", m.Raw)
	}
	// The answer is the record of the message.
	if !strings.HasPrefix(str(m.Body, "message_id"), "msg_") || m.Body["outcome"] == "" {
		t.Fatalf("no message record: %s", m.Raw)
	}
	e.must(e.do("POST", "/work-items/"+wid+"/messages", e.viewer.Key, map[string]any{"message": " "}), 400)

	e.must(e.do("POST", "/work-items/"+wid+"/stage", e.viewer.Key, map[string]any{"stage": "building"}), 403)
	e.must(e.do("POST", "/work-items/"+wid+"/stage", e.editor.Key, map[string]any{"stage": "nope"}), 400)
	s := e.must(e.do("POST", "/work-items/"+wid+"/stage", e.editor.Key, map[string]any{"stage": "building", "note": "manual"}), 200)
	if s.Body["stage"] != "building" {
		t.Fatal(s.Body)
	}
	c := e.must(e.do("POST", "/work-items/"+wid+"/complete", e.viewer.Key, map[string]any{"note": "done"}), 200)
	if c.Body["stage"] != "complete" {
		t.Fatal(c.Body)
	}
	e.must(e.do("POST", "/work-items/"+wid+"/complete", e.viewer.Key, nil), 409)
	e.must(e.do("POST", "/work-items/"+wid+"/cancel", e.viewer.Key, nil), 409)

	d2 := e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, map[string]any{"prompt": "Cancel me"}), 201)
	cc := e.must(e.do("POST", "/work-items/"+str(d2.Body, "work_item_id")+"/cancel", e.viewer.Key, map[string]any{"reason": "dup"}), 200)
	if cc.Body["stage"] != "cancelled" || len(e.engine.CallsTo("Cancel")) != 1 {
		t.Fatalf("cancel = %s", cc.Raw)
	}
}

func TestInbox(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("inbox")
	d := e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, map[string]any{"prompt": "mine"}), 201)
	wid := str(d.Body, "work_item_id")
	_ = e.store.CreateInboxItem(ctx, &core.InboxItem{ID: "inb_a", FactoryID: fid, WorkItemID: wid, Kind: core.InboxApproval, Title: "Approve spec", Options: []string{"Approve", "Reject"}, Recipients: []string{e.viewer.User.ID}})
	_ = e.store.CreateInboxItem(ctx, &core.InboxItem{ID: "inb_q", FactoryID: fid, Kind: core.InboxQuestion, Title: "Which DB?"})
	_ = e.store.CreateInboxItem(ctx, &core.InboxItem{ID: "inb_f", FactoryID: fid, Kind: core.InboxFailure, Title: "Failed", AssigneeID: e.editor.User.ID})

	ids := func(r resp) []string {
		var out []string
		for _, it := range list(r.Body, "inbox_items") {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		slices.Sort(out)
		return out
	}
	// The default is the personal queue.
	r := e.must(e.do("GET", "/factories/"+fid+"/inbox", e.viewer.Key, nil), 200)
	if got := ids(r); !slices.Equal(got, []string{"inb_a"}) {
		t.Fatalf("viewer mine = %v", got)
	}
	// An item without recipients is for the editors and admins.
	r = e.must(e.do("GET", "/inbox", e.editor.Key, nil), 200)
	if got := ids(r); !slices.Equal(got, []string{"inb_f", "inb_q"}) {
		t.Fatalf("editor mine = %v", got)
	}
	var f map[string]any
	for _, it := range list(r.Body, "inbox_items") {
		if it.(map[string]any)["id"] == "inb_f" {
			f = it.(map[string]any)
		}
	}
	if rc, _ := f["recipients"].([]any); len(rc) != 1 || rc[0] != e.editor.User.ID || f["assignee_id"] != nil || f["is_read"] != false {
		t.Fatalf("view = %v", f)
	}
	e.must(e.do("GET", "/inbox?scope=team", e.viewer.Key, nil), 403)
	e.must(e.do("GET", "/inbox?scope=everyone", e.viewer.Key, nil), 400)
	r = e.must(e.do("GET", "/inbox?scope=team", e.editor.Key, nil), 200)
	if len(ids(r)) != 3 {
		t.Fatalf("team = %s", r.Raw)
	}
	r = e.must(e.do("GET", "/inbox?scope=team&kind=question", e.editor.Key, nil), 200)
	if got := ids(r); !slices.Equal(got, []string{"inb_q"}) {
		t.Fatalf("kind = %v", got)
	}
	e.must(e.do("GET", "/inbox?kind=nonsense", e.editor.Key, nil), 400)

	// Read state for each user.
	rd := e.must(e.do("POST", "/inbox/inb_a/read", e.viewer.Key, nil), 200)
	if rd.Body["is_read"] != true || rd.Body["read_by"] != nil {
		t.Fatalf("read = %s", rd.Raw)
	}
	if got := ids(e.must(e.do("GET", "/inbox?unread=true", e.viewer.Key, nil), 200)); len(got) != 0 {
		t.Fatalf("unread = %v", got)
	}
	if g := e.must(e.do("GET", "/inbox/inb_a", e.admin.Key, nil), 200); g.Body["is_read"] != false {
		t.Fatalf("admin read state = %s", g.Raw)
	}
	rd = e.must(e.do("POST", "/inbox/inb_a/read", e.viewer.Key, map[string]any{"read": false}), 200)
	if rd.Body["is_read"] != false {
		t.Fatalf("unread = %s", rd.Raw)
	}

	e.must(e.do("POST", "/inbox/inb_a/resolve", e.viewer.Key, map[string]any{"resolution": "maybe"}), 400)
	e.must(e.do("POST", "/inbox/inb_q/resolve", e.viewer.Key, map[string]any{}), 400)
	ok := e.must(e.do("POST", "/inbox/inb_a/resolve", e.viewer.Key, map[string]any{"resolution": "approve"}), 200)
	if ok.Body["resolved"] != true || ok.Body["resolution"] != "Approve" {
		t.Fatalf("resolve = %s", ok.Raw)
	}
	e.must(e.do("POST", "/inbox/inb_a/resolve", e.viewer.Key, map[string]any{"resolution": "Approve"}), 409)
	e.must(e.do("GET", "/inbox/inb_a", e.other.Key, nil), 404)
	r = e.must(e.do("GET", "/factories/"+fid+"/inbox?include_resolved=true&scope=team", e.editor.Key, nil), 200)
	if len(ids(r)) != 3 {
		t.Fatal(r.Body)
	}

	// Undo a resolve.
	un := e.must(e.do("POST", "/inbox/inb_a/unresolve", e.viewer.Key, nil), 200)
	if un.Body["resolved"] != false || un.Body["resolution"] != nil {
		t.Fatalf("unresolve = %s", un.Raw)
	}
	e.must(e.do("POST", "/inbox/inb_a/unresolve", e.viewer.Key, nil), 409)

	// Questions with an ID, a type and options.
	_ = e.store.CreateInboxItem(ctx, &core.InboxItem{ID: "inb_s", FactoryID: fid, Kind: core.InboxQuestion, Title: "Which DB?", Recipients: []string{e.viewer.User.ID},
		Questions: []core.Question{{ID: "q1", Text: "Which DB?", Type: core.QuestionSingleSelect, Options: []string{"Postgres", "SQLite"}, RecommendedOption: "Postgres"}}})
	g := e.must(e.do("GET", "/inbox/inb_s", e.viewer.Key, nil), 200)
	if q := list(g.Body, "questions"); len(q) != 1 || q[0].(map[string]any)["question_id"] != "q1" || q[0].(map[string]any)["recommended_option"] != "Postgres" {
		t.Fatalf("questions = %s", g.Raw)
	}
	e.must(e.do("POST", "/inbox/inb_s/resolve", e.viewer.Key, map[string]any{"answers": []map[string]any{{"question_id": "q1", "selected": []string{"Postgres", "SQLite"}}}}), 400)
	e.must(e.do("POST", "/inbox/inb_s/resolve", e.viewer.Key, map[string]any{"answers": []map[string]any{{"question_id": "q9", "selected": []string{"Postgres"}}}}), 400)
	e.must(e.do("POST", "/inbox/inb_s/resolve", e.viewer.Key, map[string]any{"answers": []map[string]any{{"question_id": "q1", "selected": []string{"MySQL"}}}}), 400)
	a := e.must(e.do("POST", "/inbox/inb_s/resolve", e.viewer.Key, map[string]any{"answers": []map[string]any{{"question_id": "q1", "selected": []string{"postgres"}}}}), 200)
	if a.Body["resolution"] != "Postgres" {
		t.Fatalf("answers = %s", a.Raw)
	}
}

// ---------- webhooks ----------

func TestWebhooks(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("hooks")

	c := e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, map[string]any{"name": "internal-ci", "delivery_id_header": "X-CI-Run-Id"}), 201)
	whID := str(c.Body, "webhook", "id")
	secret := str(c.Body, "secret")
	if !strings.HasPrefix(secret, "whsec_") || str(c.Body, "ingress_url") != "https://factory.example.test/webhooks/w/"+whID {
		t.Fatalf("create = %s", c.Raw)
	}
	if v, _ := e.srv.opts.Secrets.Get(ctx, e.team.ID, "", "INTERNAL_CI_WEBHOOK_SECRET"); string(v) != secret {
		t.Fatal("secret not stored")
	}
	def := e.must(e.do("GET", "/factories/"+fid+"/definition", e.viewer.Key, nil), 200)
	yml := def.Body["files"].(map[string]any)["webhooks/internal-ci.yaml"].(string)
	if !strings.Contains(yml, "authMode: token") || !strings.Contains(yml, "secretName: INTERNAL_CI_WEBHOOK_SECRET") || !strings.Contains(yml, "deliveryIdHeader: X-CI-Run-Id") {
		t.Fatalf("webhook yaml:\n%s", yml)
	}

	ut := e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, map[string]any{"name": "grafana", "auth_mode": "url_token"}), 201)
	if !strings.HasSuffix(str(ut.Body, "ingress_url"), "/"+str(ut.Body, "secret")) {
		t.Fatalf("url token = %s", ut.Raw)
	}
	g := e.must(e.do("GET", "/webhooks/"+str(ut.Body, "webhook", "id"), e.viewer.Key, nil), 200)
	if !strings.HasSuffix(str(g.Body, "ingress_url"), "/{token}") {
		t.Fatalf("url token view = %s", g.Raw)
	}

	v := e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, map[string]any{"name": "vercel", "auth_mode": "signature", "signature_scheme": "vercel"}), 201)
	if v.Body["awaiting_secret"] != true || str(v.Body, "secret") != "" || v.Body["webhook"].(map[string]any)["enabled"] != false {
		t.Fatalf("vercel = %s", v.Raw)
	}
	vp := e.must(e.do("PATCH", "/webhooks/"+str(v.Body, "webhook", "id"), e.editor.Key, map[string]any{"secret": "provider-secret"}), 200)
	if vp.Body["enabled"] != true {
		t.Fatalf("vercel patch = %s", vp.Raw)
	}

	for _, bad := range []map[string]any{
		{"name": "Bad Name"},
		{"name": "x", "auth_mode": "magic"},
		{"name": "x", "auth_mode": "signature", "signature_scheme": "unknown"},
		{"name": "x", "auth_mode": "signature", "signature_scheme": "github"},
		{"name": "x", "signature_scheme": "github"},
	} {
		e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, bad), 400)
	}
	e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, map[string]any{"name": "internal-ci"}), 409)
	e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.viewer.Key, map[string]any{"name": "y"}), 403)

	l := e.must(e.do("GET", "/factories/"+fid+"/webhooks", e.viewer.Key, nil), 200)
	if len(list(l.Body, "webhooks")) != 3 {
		t.Fatal(l.Body)
	}
	e.must(e.do("PATCH", "/webhooks/"+whID, e.editor.Key, map[string]any{"name": "renamed"}), 422)
	e.must(e.do("PATCH", "/webhooks/"+whID, e.editor.Key, map[string]any{"auth_mode": "url_token"}), 422)
	p := e.must(e.do("PATCH", "/webhooks/"+whID, e.editor.Key, map[string]any{"enabled": false}), 200)
	if p.Body["enabled"] != false || str(p.Body, "id") != whID {
		t.Fatalf("patch = %s", p.Raw)
	}

	rot := e.must(e.do("POST", "/webhooks/"+whID+"/rotate-secret", e.editor.Key, nil), 200)
	if ns := str(rot.Body, "secret"); ns == "" || ns == secret {
		t.Fatalf("rotate = %s", rot.Raw)
	}

	// Deliveries and test-filter.
	t0 := time.Now().UTC()
	_ = e.store.CreateWebhookDelivery(ctx, &core.WebhookDelivery{ID: "dlv_1", WebhookID: whID, Status: "accepted", Body: json.RawMessage(`{"event":"deploy.finished","env":"production"}`), ReceivedAt: t0})
	_ = e.store.CreateWebhookDelivery(ctx, &core.WebhookDelivery{ID: "dlv_2", WebhookID: whID, Status: "rejected", Reason: "auth", ReceivedAt: t0.Add(time.Second)})
	dl := e.must(e.do("GET", "/webhooks/"+whID+"/deliveries", e.viewer.Key, nil), 200)
	ds := list(dl.Body, "deliveries")
	if len(ds) != 2 || ds[0].(map[string]any)["label"] != "Rejected (auth)" || ds[1].(map[string]any)["body"] != nil {
		t.Fatalf("deliveries = %s", dl.Raw)
	}
	dg := e.must(e.do("GET", "/webhooks/"+whID+"/deliveries/dlv_1", e.viewer.Key, nil), 200)
	if dg.Body["body"] == nil || dg.Body["label"] != "Accepted" {
		t.Fatalf("delivery = %s", dg.Raw)
	}
	e.must(e.do("GET", "/webhooks/"+whID+"/deliveries/dlv_nope", e.viewer.Key, nil), 404)

	// Re-enable for trigger evaluation.
	e.must(e.do("PATCH", "/webhooks/"+whID, e.editor.Key, map[string]any{"enabled": true}), 200)
	tf := e.must(e.do("POST", "/webhooks/"+whID+"/test-filter", e.viewer.Key, map[string]any{
		"filter": map[string]any{"webhook_ids": []string{whID}, "payload": map[string]any{"event": []string{"deploy.finished"}}},
	}), 200)
	if tf.Body["would_trigger"] != true || tf.Body["result"] != "Would trigger" || tf.Body["delivery_id"] != "dlv_1" {
		t.Fatalf("test filter = %s", tf.Raw)
	}
	tf = e.must(e.do("POST", "/webhooks/"+whID+"/test-filter", e.viewer.Key, map[string]any{
		"filter": map[string]any{"webhook_ids": []string{"wh_other"}, "payload": map[string]any{"env": []string{"staging"}}},
	}), 200)
	if tf.Body["would_trigger"] != false || tf.Body["webhook_check"].(map[string]any)["passed"] != false || tf.Body["payload_check"].(map[string]any)["passed"] != false {
		t.Fatalf("test filter (negative) = %s", tf.Raw)
	}
	tf = e.must(e.do("POST", "/webhooks/"+whID+"/test-filter", e.viewer.Key, map[string]any{
		"pattern": map[string]any{"env": []string{"staging"}}, "body": map[string]any{"env": "staging"},
	}), 200)
	if tf.Body["would_trigger"] != true {
		t.Fatalf("test filter (body) = %s", tf.Raw)
	}

	e.must(e.do("DELETE", "/webhooks/"+whID, e.editor.Key, nil), 204)
	e.must(e.do("GET", "/webhooks/"+whID, e.viewer.Key, nil), 404)

	// Git-backed factories manage webhooks in their repository.
	gf := e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "gitf", "source": "git", "git_repo": "github.com/acme/def"}), 201)
	e.must(e.do("POST", "/factories/"+str(gf.Body, "factory", "uid")+"/webhooks", e.editor.Key, map[string]any{"name": "x"}), 422)
}

// TestWebhookCreateReusesDefaultSecret checks that a webhook created without
// secret_name reuses an existing secret with the default name and never
// overwrites it.
func TestWebhookCreateReusesDefaultSecret(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("hooks")
	if _, err := e.srv.opts.Secrets.Put(ctx, e.team.ID, "", "DEPLOYS_WEBHOOK_SECRET", core.SecretGeneric, []byte("kept-value")); err != nil {
		t.Fatal(err)
	}
	c := e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, map[string]any{"name": "deploys"}), 201)
	if str(c.Body, "secret") != "" {
		t.Fatalf("an existing secret is revealed or replaced: %s", c.Raw)
	}
	if v, _ := e.srv.opts.Secrets.Get(ctx, e.team.ID, "", "DEPLOYS_WEBHOOK_SECRET"); string(v) != "kept-value" {
		t.Fatalf("default secret overwritten: %q", v)
	}
}

func TestDeliveryViewLabels(t *testing.T) {
	for reason, want := range map[string]string{
		"auth": "Rejected (auth)", "invalid_json": "Rejected (invalid JSON)", "too_large": "Rejected (too large)",
		"rate_limited": "Rejected (rate limited)", "secret_unavailable": "Rejected (secret unavailable)",
	} {
		if got := deliveryView(core.WebhookDelivery{Status: "rejected", Reason: reason}, false).Label; got != want {
			t.Errorf("label for %q = %q, want %q", reason, got, want)
		}
	}
}

// ---------- runs ----------

// idle_timeout_minutes 0 is a value ("end the session after the turn"), not
// "unset": the config must say that it was set.
func TestAgentRunIdleTimeoutZero(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("idle")
	body := runBody(fid, "x")
	body["config"] = map[string]any{"idle_timeout_minutes": 0}
	e.must(e.do("POST", "/runs", e.viewer.Key, body), 201)
	ov := e.engine.CallsTo("Dispatch")[0].Args[0].(core.DispatchRequest).Overrides
	if ov == nil || ov.IdleTimeoutMinutes == nil || *ov.IdleTimeoutMinutes != 0 {
		t.Fatalf("idle 0 overrides = %+v", ov)
	}
}

func TestAgentRuns(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	env := &core.Environment{ID: "env_1", TeamID: e.team.ID, Name: "default"}
	_ = e.store.CreateEnvironment(ctx, env)
	fid := e.createFactory("agents")

	r := e.must(e.do("POST", "/runs", e.viewer.Key, map[string]any{
		"prompt":     "Scan the repo for outdated dependencies.",
		"factory_id": fid, "agent_name": "triage",
		"config": map[string]any{
			"environment_id": "env_1", "worker_host": "worker-1",
			"harness":     map[string]any{"type": "claude-code", "model": "claude-sonnet", "auth": map[string]any{"source": "managedSecret", "secretName": "ANTHROPIC_KEY"}},
			"mcp_servers": map[string]any{"sentry": map[string]any{"url": "https://mcp.example.test", "headers": map[string]string{"Authorization": "Bearer {{SENTRY}}"}}},
			"secrets":     []string{"METABASE_API_KEY"}, "idle_timeout_minutes": 15, "machine": "linux",
		},
		"metadata": map[string]string{"ticket": "ENG-1"},
	}), 201)
	runID := str(r.Body, "run_id")
	if r.Body["state"] != StateQueued || str(r.Body, "run_url") != "https://factory.example.test/runs/"+runID {
		t.Fatalf("create = %s", r.Raw)
	}
	if r.Body["factory_id"] != fid || r.Body["agent_name"] != "triage" {
		t.Fatalf("create = %s", r.Raw)
	}
	req := e.engine.CallsTo("Dispatch")[0].Args[0].(core.DispatchRequest)
	ov := req.Overrides
	if req.FactoryID != fid || req.AgentName != "triage" || ov.Harness == nil || ov.Harness.Type != core.HarnessClaude ||
		ov.WorkerHost == nil || *ov.WorkerHost != "worker-1" || ov.IdleTimeoutMinutes == nil || *ov.IdleTimeoutMinutes != 15 ||
		ov.Machine != "linux" || ov.EnvironmentID == nil || *ov.EnvironmentID != "env_1" {
		t.Fatalf("dispatch = %+v overrides = %+v", req, ov)
	}

	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"no prompt", map[string]any{"agent_identity_uid": "agt_" + fid + "_triage"}},
		{"no agent", map[string]any{"prompt": "x"}},
		{"factory without agent name", map[string]any{"prompt": "x", "factory_id": fid}},
		{"bad harness", map[string]any{"prompt": "x", "config": map[string]any{"harness": map[string]any{"type": "gpt"}}}},
		{"open with auth", map[string]any{"prompt": "x", "config": map[string]any{"harness": map[string]any{"type": "open", "auth": map[string]any{"source": "managedSecret", "secretName": "K"}}}}},
		{"managed secret without name", map[string]any{"prompt": "x", "config": map[string]any{"harness": map[string]any{"type": "codex", "auth": map[string]any{"source": "managedSecret"}}}}},
		{"worker env without worker", map[string]any{"prompt": "x", "config": map[string]any{"harness": map[string]any{"type": "codex", "auth": map[string]any{"source": "workerEnvironment"}}}}},
		{"two transports", map[string]any{"prompt": "x", "config": map[string]any{"mcp_servers": map[string]any{"a": map[string]any{"url": "https://x.test", "command": "x"}}}}},
		{"relative url", map[string]any{"prompt": "x", "config": map[string]any{"mcp_servers": map[string]any{"a": map[string]any{"url": "/mcp"}}}}},
		{"idle timeout", map[string]any{"prompt": "x", "config": map[string]any{"idle_timeout_minutes": 61}}},
		{"unknown env", map[string]any{"prompt": "x", "config": map[string]any{"environment_id": "env_nope"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e.must(e.do("POST", "/runs", e.viewer.Key, c.body), 400)
		})
	}

	// A run by the UID of the agent.
	ar := e.must(e.do("POST", "/runs", e.viewer.Key, map[string]any{"prompt": "triage this", "agent_identity_uid": "agt_" + fid + "_triage"}), 201)
	if ar.Body["factory_id"] != fid || ar.Body["agent_name"] != "triage" {
		t.Fatalf("agent run = %s", ar.Raw)
	}

	// List with filters + pagination.
	l := e.must(e.do("GET", "/runs?name=triage", e.viewer.Key, nil), 200)
	if len(list(l.Body, "runs")) != 2 {
		t.Fatal(l.Body)
	}
	l = e.must(e.do("GET", "/runs?limit=1", e.viewer.Key, nil), 200)
	if len(list(l.Body, "runs")) != 1 || nextCursor(l.Body) == nil {
		t.Fatal(l.Body)
	}
	l2 := e.must(e.do("GET", "/runs?limit=1&cursor="+nextCursor(l.Body).(string), e.viewer.Key, nil), 200)
	if len(list(l2.Body, "runs")) != 1 || nextCursor(l2.Body) != nil {
		t.Fatalf("page 2 = %s", l2.Raw)
	}
	l = e.must(e.do("GET", "/runs?state=QUEUED&factory_id="+fid, e.viewer.Key, nil), 200)
	if len(list(l.Body, "runs")) != 2 {
		t.Fatal(l.Body)
	}
	e.must(e.do("GET", "/runs?state=SLEEPING", e.viewer.Key, nil), 400)
	if l := e.must(e.do("GET", "/runs", e.other.Key, nil), 200); len(list(l.Body, "runs")) != 0 {
		t.Fatal("cross-team runs leak")
	}

	g := e.must(e.do("GET", "/runs/"+runID, e.viewer.Key, nil), 200)
	if g.Body["prompt"] != "Scan the repo for outdated dependencies." || g.Body["phase"] != "queued" {
		t.Fatal(g.Body)
	}
	e.must(e.do("GET", "/runs/"+runID, e.other.Key, nil), 404)

	_ = e.store.AppendRunMessage(ctx, &core.RunMessage{RunID: runID, Role: "assistant", Content: "Looking"})
	m := e.must(e.do("GET", "/runs/"+runID+"/messages?after=1", e.viewer.Key, nil), 200)
	if msgs := list(m.Body, "messages"); len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "Looking" {
		t.Fatalf("messages = %s", m.Raw)
	}
	m = e.must(e.do("GET", "/runs/"+runID+"/messages?limit=1", e.viewer.Key, nil), 200)
	if m.Body["has_more"] != true || m.Body["next_after"].(float64) != 1 {
		t.Fatalf("messages page = %s", m.Raw)
	}

	e.must(e.do("POST", "/runs/"+runID+"/followups", e.viewer.Key, map[string]any{"message": ""}), 400)
	e.must(e.do("POST", "/runs/"+runID+"/followups", e.viewer.Key, map[string]any{"message": "Also check dev deps"}), 202)
	cr := e.must(e.do("POST", "/runs/"+runID+"/cancel", e.viewer.Key, nil), 200)
	if cr.Body["run_id"] != runID {
		t.Fatal(cr.Body)
	}
	// A second cancel is accepted: it ends a live session.
	e.must(e.do("POST", "/runs/"+runID+"/cancel", e.viewer.Key, nil), 200)
	e.must(e.do("POST", "/runs/"+runID+"/followups", e.viewer.Key, map[string]any{"message": "again"}), 422)
}

func TestRunEventsSSE(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	r := e.must(e.do("POST", "/runs", e.viewer.Key, runBody(e.createFactory("sse"), "hello")), 201)
	runID := str(r.Body, "run_id")

	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/runs/"+runID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+e.viewer.Key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		run, _ := e.store.GetRun(ctx, runID)
		setState := func(st core.RunState) {
			run.State = st
			_ = e.store.UpdateRun(ctx, run)
			snapshot := *run
			e.bus.Publish(ctx, core.TopicRunUpdated, &snapshot)
		}
		setState(core.RunRunning)
		m := &core.RunMessage{RunID: runID, Role: "assistant", Content: "Hi there"}
		_ = e.store.AppendRunMessage(ctx, m)
		e.bus.Publish(ctx, core.TopicRunMessage, m)
		setState(core.RunSucceeded)
	}()

	done := make(chan struct{})
	var events []string
	var datas []string
	go func() {
		defer close(done)
		data, _ := io.ReadAll(res.Body)
		for _, block := range strings.Split(string(data), "\n\n") {
			for _, line := range strings.Split(block, "\n") {
				if v, ok := strings.CutPrefix(line, "event: "); ok {
					events = append(events, v)
				}
				if v, ok := strings.CutPrefix(line, "data: "); ok {
					datas = append(datas, v)
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end")
	}
	joined := strings.Join(events, ",")
	if !strings.HasPrefix(joined, "run,message") || !strings.HasSuffix(joined, "done") || strings.Count(joined, "message") != 2 {
		t.Fatalf("events = %s", joined)
	}
	if !strings.Contains(strings.Join(datas, "\n"), "Hi there") {
		t.Fatalf("data = %v", datas)
	}

	// Resuming after the last seq on a finished run replays nothing and ends immediately.
	req2, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/runs/"+runID+"/events", nil)
	req2.Header.Set("Authorization", "Bearer "+e.viewer.Key)
	req2.Header.Set("Last-Event-ID", "2")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res2.Body)
	res2.Body.Close()
	if strings.Contains(string(body), "event: message") || !strings.Contains(string(body), "event: done") {
		t.Fatalf("resume body = %s", body)
	}
}

// ---------- platform ----------

func TestSecrets(t *testing.T) {
	e := newTestEnv(t)
	c := e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "METABASE_API_KEY", "value": "s3cret"}), 201)
	if _, leaked := c.Body["value"]; leaked || c.Body["type"] != "generic" {
		t.Fatalf("create = %s", c.Raw)
	}
	e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "METABASE_API_KEY", "value": "x"}), 409)
	e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "1BAD", "value": "x"}), 400)
	e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "EMPTY", "value": ""}), 400)
	e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "BIG", "value": strings.Repeat("x", 131072)}), 400)
	e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "T", "value": "x", "type": "nope"}), 400)
	l := e.must(e.do("GET", "/secrets", e.viewer.Key, nil), 200)
	if !strings.Contains(string(l.Raw), "METABASE_API_KEY") || strings.Contains(string(l.Raw), "s3cret") {
		t.Fatalf("list = %s", l.Raw)
	}
	e.must(e.do("GET", "/secrets/METABASE_API_KEY", e.viewer.Key, nil), 200)
	e.must(e.do("PUT", "/secrets/METABASE_API_KEY", e.editor.Key, map[string]any{"value": "rotated"}), 200)
	e.must(e.do("PUT", "/secrets/MISSING", e.editor.Key, map[string]any{"value": "x"}), 404)
	if v, _ := e.srv.opts.Secrets.Get(context.Background(), e.team.ID, "", "METABASE_API_KEY"); string(v) != "rotated" {
		t.Fatal("not rotated")
	}
	fid := e.createFactory("sec")
	e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "SCOPED", "value": "x", "factory_id": fid}), 201)
	e.must(e.do("GET", "/secrets/SCOPED?factory_id="+fid, e.viewer.Key, nil), 200)
	e.must(e.do("GET", "/secrets/SCOPED", e.viewer.Key, nil), 404)
	e.must(e.do("DELETE", "/secrets/METABASE_API_KEY", e.editor.Key, nil), 204)
	e.must(e.do("GET", "/secrets/METABASE_API_KEY", e.viewer.Key, nil), 404)
}

func TestEnvironmentsWorkersIntegrations(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	c := e.must(e.do("POST", "/environments", e.editor.Key, map[string]any{"name": "payments", "repos": []string{"github:acme/payments"}, "setup_commands": []string{"make deps"}}), 201)
	id := str(c.Body, "id")
	e.must(e.do("POST", "/environments", e.editor.Key, map[string]any{"name": "x", "repos": []string{"nope"}}), 400)
	e.must(e.do("POST", "/environments", e.editor.Key, map[string]any{}), 400)
	u := e.must(e.do("PATCH", "/environments/"+id, e.editor.Key, map[string]any{"image": "golang:1.27"}), 200)
	if u.Body["image"] != "golang:1.27" || u.Body["name"] != "payments" {
		t.Fatal(u.Body)
	}
	if l := e.must(e.do("GET", "/environments", e.viewer.Key, nil), 200); len(list(l.Body, "environments")) != 1 {
		t.Fatal(l.Body)
	}
	e.must(e.do("GET", "/environments/"+id, e.other.Key, nil), 404)
	e.must(e.do("DELETE", "/environments/"+id, e.editor.Key, nil), 204)

	_ = e.store.UpsertWorker(ctx, &core.Worker{ID: "wk_1", TeamID: e.team.ID, Name: "w1", Backend: "docker", LastHeartbeat: time.Now().UTC()})
	_ = e.store.UpsertWorker(ctx, &core.Worker{ID: "wk_2", TeamID: e.team.ID, Name: "w2", Backend: "direct", LastHeartbeat: time.Now().Add(-time.Hour)})
	w := e.must(e.do("GET", "/workers", e.viewer.Key, nil), 200)
	if len(list(w.Body, "workers")) != 2 {
		t.Fatal(w.Body)
	}
	if g := e.must(e.do("GET", "/workers/wk_1", e.viewer.Key, nil), 200); g.Body["status"] != "online" {
		t.Fatal(g.Body)
	}
	if g := e.must(e.do("GET", "/workers/wk_2", e.viewer.Key, nil), 200); g.Body["status"] != "offline" {
		t.Fatal(g.Body)
	}

	in := e.must(e.do("POST", "/integrations", e.admin.Key, map[string]any{"provider": "slack", "config": map[string]string{"team_id": "T1"}}), 201)
	iid := str(in.Body, "id")
	e.must(e.do("POST", "/integrations", e.admin.Key, map[string]any{"provider": "myspace"}), 400)
	e.must(e.do("PATCH", "/integrations/"+iid, e.admin.Key, map[string]any{"name": "Acme Slack"}), 200)
	if l := e.must(e.do("GET", "/integrations?provider=slack", e.viewer.Key, nil), 200); len(list(l.Body, "integrations")) != 1 {
		t.Fatal(l.Body)
	}
	e.must(e.do("DELETE", "/integrations/"+iid, e.editor.Key, nil), 403)
	e.must(e.do("DELETE", "/integrations/"+iid, e.admin.Key, nil), 204)
}

func TestAPIKeysAndUsers(t *testing.T) {
	e := newTestEnv(t)
	k := e.must(e.do("POST", "/api-keys", e.viewer.Key, map[string]any{"name": "ci"}), 201)
	newKey := str(k.Body, "key")
	if _, ok := ParseKey(newKey); !ok {
		t.Fatalf("key %q", newKey)
	}
	e.must(e.do("GET", "/me", newKey, nil), 200)
	l := e.must(e.do("GET", "/api-keys", e.viewer.Key, nil), 200)
	if len(list(l.Body, "api_keys")) != 2 || strings.Contains(string(l.Raw), "hashed") {
		t.Fatalf("viewer keys = %s", l.Raw)
	}
	if la := e.must(e.do("GET", "/api-keys", e.admin.Key, nil), 200); len(list(la.Body, "api_keys")) != 4 {
		t.Fatalf("admin keys = %s", la.Raw)
	}
	e.must(e.do("POST", "/api-keys", e.viewer.Key, map[string]any{"name": "x", "user_id": e.editor.User.ID}), 403)
	e.must(e.do("POST", "/api-keys", e.admin.Key, map[string]any{"name": "for-editor", "user_id": e.editor.User.ID}), 201)
	adminKeys := e.must(e.do("GET", "/api-keys", e.admin.Key, nil), 200)
	var editorKeyID string
	for _, it := range list(adminKeys.Body, "api_keys") {
		m := it.(map[string]any)
		if m["user_id"] == e.editor.User.ID {
			editorKeyID = m["id"].(string)
		}
	}
	e.must(e.do("DELETE", "/api-keys/"+editorKeyID, e.viewer.Key, nil), 403)
	e.must(e.do("DELETE", "/api-keys/"+str(k.Body, "api_key", "id"), e.viewer.Key, nil), 204)
	e.must(e.do("GET", "/me", newKey, nil), 401)

	cu := e.must(e.do("POST", "/users", e.admin.Key, map[string]any{"email": "New@Example.test", "role": "user", "create_api_key": true}), 201)
	if str(cu.Body, "user", "email") != "new@example.test" || str(cu.Body, "key") == "" {
		t.Fatal(cu.Body)
	}
	e.must(e.do("POST", "/users", e.admin.Key, map[string]any{"email": "new@example.test"}), 409)
	e.must(e.do("POST", "/users", e.admin.Key, map[string]any{"email": "not-an-email"}), 400)
	e.must(e.do("POST", "/users", e.admin.Key, map[string]any{"email": "r@example.test", "role": "owner"}), 400)
	if lu := e.must(e.do("GET", "/users", e.viewer.Key, nil), 200); len(list(lu.Body, "users")) != 4 {
		t.Fatal(lu.Body)
	}
	e.must(e.do("GET", "/users/"+e.other.User.ID, e.viewer.Key, nil), 404)

	uid := str(cu.Body, "user", "id")
	up := e.must(e.do("PATCH", "/users/"+uid, e.admin.Key, map[string]any{"name": "Newbie", "role": "editor"}), 200)
	if str(up.Body, "name") != "Newbie" || str(up.Body, "role") != "editor" {
		t.Fatal(up.Body)
	}
	e.must(e.do("PATCH", "/users/"+uid, e.editor.Key, map[string]any{"role": "admin"}), 403)
	e.must(e.do("PATCH", "/users/"+uid, e.admin.Key, map[string]any{"role": "owner"}), 400)
	e.must(e.do("PATCH", "/users/"+e.other.User.ID, e.admin.Key, map[string]any{"name": "x"}), 404)
	e.must(e.do("PATCH", "/users/"+e.admin.User.ID, e.admin.Key, map[string]any{"role": "user"}), 409)
	if tm := e.must(e.do("GET", "/teams", e.viewer.Key, nil), 200); len(list(tm.Body, "teams")) != 1 {
		t.Fatal(tm.Body)
	}
	e.must(e.do("GET", "/teams/"+e.team.ID, e.viewer.Key, nil), 200)
	e.must(e.do("GET", "/teams/team_other", e.viewer.Key, nil), 404)
}

func TestSchemasAndOptionalFeatures(t *testing.T) {
	e := newTestEnv(t)
	v := e.must(e.do("GET", "/factory-files/schemas", "", nil), 200)
	if v.Body["current_version"] != "v1" {
		t.Fatal(v.Body)
	}
	all := e.must(e.do("GET", "/factory-files/schemas/v1", "", nil), 200)
	if _, ok := all.Body["factory.schema.json"]; !ok {
		t.Fatal(all.Body)
	}
	doc := e.must(e.do("GET", "/factory-files/schemas/v1/factory", "", nil), 200)
	if doc.Body["type"] != "object" || doc.Header.Get("Content-Type") != "application/schema+json" {
		t.Fatal(doc.Body)
	}
	e.must(e.do("GET", "/factory-files/schemas/v9/factory.schema.json", "", nil), 404)
	e.must(e.do("GET", "/factory-files/schemas/v1/nope", "", nil), 404)

	fid := e.createFactory("feat")
	for _, path := range []string{"/networking/policies", "/factories/" + fid + "/metrics"} {
		if r := e.must(e.do("GET", path, e.viewer.Key, nil), 501); errCode(r) != CodeFeatureNotAvailable {
			t.Fatal(r.Body)
		}
	}
	e.must(e.do("POST", "/factories/"+fid+"/benchmarks/x/runs", e.editor.Key, nil), 501)
}

func TestMeasureEndpoints(t *testing.T) {
	var gotSince time.Time
	e := newTestEnv(t, func(o *Options) {
		o.Metrics = func(_ context.Context, fid string, since time.Time) (any, error) {
			gotSince = since
			return map[string]any{"total_runs": 3}, nil
		}
		o.NetworkPolicies = func() any {
			return map[string]any{"policies": []any{map[string]any{"name": "builtin/package-registries"}}}
		}
		o.StartBenchmark = func(_ context.Context, fid, suite string, cfg json.RawMessage) (*core.BenchmarkRun, error) {
			return &core.BenchmarkRun{ID: "bench_1", FactoryID: fid, Suite: suite, State: "running", CreatedAt: time.Now().UTC()}, nil
		}
	})
	ctx := context.Background()
	fid := e.createFactory("measure")
	m := e.must(e.do("GET", "/factories/"+fid+"/metrics?since=7d", e.viewer.Key, nil), 200)
	if str(m.Body, "factory_id") != fid || m.Body["metrics"].(map[string]any)["total_runs"].(float64) != 3 {
		t.Fatal(m.Body)
	}
	if d := time.Since(gotSince); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("since = %v", gotSince)
	}
	e.must(e.do("GET", "/factories/"+fid+"/metrics?since=yesterday-ish", e.viewer.Key, nil), 400)
	e.must(e.do("GET", "/networking/policies", e.viewer.Key, nil), 200)

	files := apitest.MinimalFiles("measure")
	files["benchmarks/pr-review/suite.yaml"] = "name: PR review\nagent: foreman\n"
	e.must(e.do("PUT", "/factories/"+fid+"/definition", e.editor.Key, map[string]any{"files": files}), 200)
	if b := e.must(e.do("GET", "/factories/"+fid+"/benchmarks", e.viewer.Key, nil), 200); len(list(b.Body, "benchmarks")) != 1 {
		t.Fatal(b.Body)
	}
	e.must(e.do("POST", "/factories/"+fid+"/benchmarks/unknown/runs", e.editor.Key, nil), 404)
	e.must(e.do("POST", "/factories/"+fid+"/benchmarks/pr-review/runs", e.viewer.Key, nil), 403)
	e.must(e.do("POST", "/factories/"+fid+"/benchmarks/pr-review/runs", e.editor.Key, map[string]any{"configurations": []map[string]any{{}}}), 400)
	br := e.must(e.do("POST", "/factories/"+fid+"/benchmarks/pr-review/runs", e.editor.Key, map[string]any{"configurations": []map[string]any{{"name": "baseline"}}, "repetitions": 2}), 201)
	if br.Body["suite"] != "pr-review" {
		t.Fatal(br.Body)
	}
	_ = e.store.CreateBenchmarkRun(ctx, &core.BenchmarkRun{ID: "bench_2", FactoryID: fid, Suite: "pr-review", State: "complete", CreatedAt: time.Now().UTC()})
	if l := e.must(e.do("GET", "/factories/"+fid+"/benchmarks/pr-review/runs", e.viewer.Key, nil), 200); len(list(l.Body, "benchmark_runs")) != 1 {
		t.Fatal(l.Body)
	}
	e.must(e.do("GET", "/benchmark-runs/bench_2", e.viewer.Key, nil), 200)
	e.must(e.do("GET", "/benchmark-runs/bench_2", e.other.Key, nil), 404)

	_ = e.store.ReplaceFactoryResources(ctx, fid, nil, nil, []core.Scorer{{ID: "sc_1", FactoryID: fid, Slug: "tests-run"}}, nil)
	_ = e.store.CreateScore(ctx, &core.Score{ID: "s1", ScorerID: "sc_1", AgentName: "implement", Passed: true})
	_ = e.store.CreateScore(ctx, &core.Score{ID: "s2", ScorerID: "sc_1", AgentName: "implement", Passed: false})
	if l := e.must(e.do("GET", "/factories/"+fid+"/scorers", e.viewer.Key, nil), 200); len(list(l.Body, "scorers")) != 1 {
		t.Fatal(l.Body)
	}
	if l := e.must(e.do("GET", "/factories/"+fid+"/scores?scorer=tests-run&passed=false", e.viewer.Key, nil), 200); len(list(l.Body, "scores")) != 1 {
		t.Fatal(l.Body)
	}
	e.must(e.do("GET", "/factories/"+fid+"/scores?scorer=nope", e.viewer.Key, nil), 404)
}

// ---------- OpenAPI ----------

func TestOpenAPICoversRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                                `json:"openapi"`
		Paths   map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(OpenAPISpec(), &doc); err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version %q", doc.OpenAPI)
	}
	srv := New(Options{})
	registered := map[string]bool{}
	for _, rt := range srv.Routes() {
		rel := strings.TrimPrefix(rt.Path, BasePath)
		key := strings.ToLower(rt.Method) + " " + rel
		registered[key] = true
		ops, ok := doc.Paths[rel]
		if !ok {
			t.Errorf("route %s %s missing from openapi.json", rt.Method, rel)
			continue
		}
		raw, ok := ops[strings.ToLower(rt.Method)]
		if !ok {
			t.Errorf("method %s of %s missing from openapi.json", rt.Method, rel)
			continue
		}
		var op struct {
			Security []any  `json:"security"`
			Role     string `json:"x-required-role"`
		}
		_ = json.Unmarshal(raw, &op)
		if rt.Role == "" && op.Security == nil {
			t.Errorf("%s %s is public but the spec requires auth", rt.Method, rel)
		}
		if rt.Role != "" && op.Role != string(rt.Role) {
			t.Errorf("%s %s requires %s but the spec says %q", rt.Method, rel, rt.Role, op.Role)
		}
	}
	for path, ops := range doc.Paths {
		for m := range ops {
			if !registered[m+" "+path] {
				t.Errorf("openapi.json documents %s %s which is not registered", strings.ToUpper(m), path)
			}
		}
	}
	e := newTestEnv(t)
	r := e.must(e.do("GET", "/openapi.json", "", nil), 200)
	if servers := r.Body["servers"].([]any); servers[0].(map[string]any)["url"] != "https://factory.example.test/api/v1" {
		t.Fatalf("servers = %v", servers)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"2026-11-01T00:00:00Z": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		"2026-11-01":           time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"2w":                   now.Add(-14 * 24 * time.Hour),
		"36h":                  now.Add(-36 * time.Hour),
	}
	for in, want := range cases {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseSince("soon", now); err == nil {
		t.Error("accepted garbage")
	}
}

func TestGenerateDefaultDefinitionCompiles(t *testing.T) {
	files := GenerateDefaultDefinition(DefaultDefinitionRequest{
		Name: "x", Alias: "x", Agents: DefaultAgentNames,
		Harness:      &core.HarnessConfig{Type: core.HarnessCodex, Model: "gpt-5", Auth: &core.HarnessAuth{Source: "managedSecret", SecretName: "K"}},
		Integrations: []core.IntegrationDecl{{Type: "slack", Settings: map[string]string{"autoRespondToThreadReplies": "false"}}},
	})
	if len(files) != 6 {
		t.Fatalf("files = %v", files)
	}
	if _, errs := apitest.Compile(files); len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	fy := files["factory.yaml"]
	for _, want := range []string{"type: codex", "secretName: K", "autoRespondToThreadReplies: false", "repositories: []"} {
		if !strings.Contains(fy, want) {
			t.Errorf("factory.yaml lacks %q:\n%s", want, fy)
		}
	}
}

func TestInterruptRun(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("interrupt")
	d := e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, map[string]any{"prompt": "go"}), 201)
	runID := str(d.Body, "id")
	if runID == "" {
		t.Fatalf("run = %s", d.Raw)
	}
	r := e.must(e.do("POST", "/runs/"+runID+"/interrupt", e.viewer.Key, nil), 202)
	if r.Body["status"] != "interrupting" || len(e.engine.CallsTo("Interrupt")) != 1 {
		t.Fatalf("interrupt = %s", r.Raw)
	}
	e.engine.InterruptErr = &core.InterruptError{Reason: core.InterruptNoActiveTurn}
	c := e.must(e.do("POST", "/runs/"+runID+"/interrupt", e.viewer.Key, nil), 409)
	if c.Body["reason"] != "no_active_turn" {
		t.Fatalf("conflict = %s", c.Raw)
	}
	e.engine.InterruptErr = &core.InterruptError{Reason: core.InterruptUnsupportedHarness}
	c = e.must(e.do("POST", "/runs/"+runID+"/interrupt", e.viewer.Key, nil), 409)
	if c.Body["reason"] != "unsupported_harness" {
		t.Fatalf("conflict = %s", c.Raw)
	}
	e.must(e.do("POST", "/runs/run_missing/interrupt", e.viewer.Key, nil), 404)
}
