package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func (e *testEnv) activeFiles(fid string) map[string]string {
	e.t.Helper()
	f, err := e.store.GetFactory(context.Background(), fid)
	if err != nil {
		e.t.Fatal(err)
	}
	v, err := e.store.GetDefinitionVersion(context.Background(), f.ActiveVersionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return v.Files
}

func TestSessionAuthenticateHookAndMe(t *testing.T) {
	var gotCSRF bool
	e := newTestEnv(t, func(o *Options) {
		o.LoginProviders = func(*http.Request) []LoginProvider {
			return []LoginProvider{{Name: "oidc", DisplayName: "Acme SSO", LoginURL: "/auth/oidc/login"}}
		}
	})
	// Install the hook after seeding so it can resolve the seeded editor.
	e.srv.opts.Authenticate = func(r *http.Request) (*Principal, error) {
		ck, err := r.Cookie("of_session")
		if err != nil {
			return nil, nil //nolint:nilerr // no session cookie means no session, not an error
		}
		if ck.Value != "good" {
			return nil, fmt.Errorf("bad session: %w", core.ErrUnauthorized)
		}
		if r.Method != http.MethodGet && r.Header.Get("X-OF-CSRF") != "1" {
			return nil, fmt.Errorf("csrf: %w", core.ErrForbidden)
		}
		gotCSRF = gotCSRF || r.Header.Get("X-OF-CSRF") == "1"
		return &Principal{User: e.editor.User, TeamID: e.team.ID, Role: core.RoleEditor}, nil
	}
	call := func(method, path, cookie string, csrf bool) resp {
		req, _ := http.NewRequest(method, e.ts.URL+BasePath+path, strings.NewReader(`{"name":"Via session"}`))
		req.Header.Set("Content-Type", "application/json")
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: "of_session", Value: cookie})
		}
		if csrf {
			req.Header.Set("X-OF-CSRF", "1")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		out := resp{Status: res.StatusCode, Raw: raw}
		_ = json.Unmarshal(raw, &out.Body)
		return out
	}

	// Unauthenticated /me is 401 and carries the login providers.
	r := call("GET", "/me", "", false)
	if r.Status != 401 || !strings.Contains(string(r.Raw), "/auth/oidc/login") {
		t.Fatalf("anon /me = %d %s", r.Status, r.Raw)
	}
	r = call("GET", "/me", "bad", false)
	if r.Status != 401 {
		t.Fatalf("bad session = %d", r.Status)
	}
	r = call("GET", "/me", "good", false)
	if r.Status != 200 || str(r.Body, "role") != "editor" || str(r.Body, "user", "id") != e.editor.User.ID {
		t.Fatalf("/me = %d %s", r.Status, r.Raw)
	}
	if ps := list(r.Body, "login_providers"); len(ps) != 1 {
		t.Fatalf("login_providers = %v", ps)
	}
	if _, ok := r.Body["api_key"]; ok {
		t.Fatal("session /me must not include api_key")
	}
	// Unsafe method without CSRF header is forbidden; with it, allowed.
	if r := call("POST", "/factories", "good", false); r.Status != 403 {
		t.Fatalf("no csrf = %d %s", r.Status, r.Raw)
	}
	if r := call("POST", "/factories", "good", true); r.Status != 201 || !gotCSRF {
		t.Fatalf("csrf post = %d %s", r.Status, r.Raw)
	}
	// Bearer keys keep working alongside the hook.
	e.must(e.do("GET", "/me", e.viewer.Key, nil), 200)
	// Public login providers and version.
	lp := e.must(e.do("GET", "/login-providers", "", nil), 200)
	if len(list(lp.Body, "login_providers")) != 1 {
		t.Fatalf("login-providers = %s", lp.Raw)
	}
	v := e.must(e.do("GET", "/version", "", nil), 200)
	if str(v.Body, "version") != "dev" {
		t.Fatalf("version = %s", v.Raw)
	}
}

func TestFactorySettingsSourceAndCredentials(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("Payments")
	e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"credential_strategy": "creator"}), 200)
	if fy := e.activeFiles(fid)["factory.yaml"]; !strings.Contains(fy, "credentialStrategy: CREATOR") {
		t.Fatalf("factory.yaml = %s", fy)
	}
	if r := e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"credential_strategy": "BOGUS"}); r.Status != 400 {
		t.Fatalf("bogus credential = %d", r.Status)
	}
	s := e.must(e.do("GET", "/factories/"+fid+"/settings", e.viewer.Key, nil), 200)
	if s.Body["editable"] != true || len(list(s.Body, "integration_types")) != 4 || len(list(s.Body, "repositories")) != 1 {
		t.Fatalf("settings = %s", s.Raw)
	}
	if r := e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"source": "git"}); r.Status != 400 {
		t.Fatalf("git without repo = %d %s", r.Status, r.Raw)
	}
	r := e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"source": "git", "git_repo": "github.com/acme/def"}), 200)
	if str(r.Body, "source") != "git" || str(r.Body, "git_branch") != "main" {
		t.Fatalf("switch = %s", r.Raw)
	}
	r = e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"source": "managed"}), 200)
	if str(r.Body, "source") != "managed" {
		t.Fatalf("switch back = %s", r.Raw)
	}
}

func TestFactoryIntegrationsPut(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("Payments")
	if r := e.do("PUT", "/factories/"+fid+"/integrations", e.editor.Key, map[string]any{"integrations": []string{"linear", "jira"}}); r.Status != 400 {
		t.Fatalf("linear+jira = %d", r.Status)
	}
	if r := e.do("PUT", "/factories/"+fid+"/integrations", e.viewer.Key, map[string]any{"integrations": []string{}}); r.Status != 403 {
		t.Fatalf("viewer = %d", r.Status)
	}
	e.must(e.do("PUT", "/factories/"+fid+"/integrations", e.editor.Key, map[string]any{"integrations": []string{"slack", "linear"}}), 200)
	fy := e.activeFiles(fid)["factory.yaml"]
	if !strings.Contains(fy, "type: slack") || !strings.Contains(fy, "type: linear") {
		t.Fatalf("factory.yaml = %s", fy)
	}
	got, _ := e.store.GetFactoryIntegrations(context.Background(), fid)
	if _, ok := got["slack"]; !ok {
		t.Fatalf("store integrations = %v", got)
	}
}

func TestAgentSpecEndpoints(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("Payments")
	agents := e.must(e.do("GET", "/factories/"+fid+"/agents", e.viewer.Key, nil), 200)
	name := str(list(agents.Body, "agents")[0].(map[string]any), "name")
	spec := e.must(e.do("GET", "/factories/"+fid+"/agents/"+name+"/spec", e.viewer.Key, nil), 200)
	if spec.Body["has_file"] != true || spec.Body["editable"] != true {
		t.Fatalf("spec = %s", spec.Raw)
	}
	if r := e.do("PUT", "/factories/"+fid+"/agents/"+name+"/spec", e.editor.Key, map[string]any{"harness_type": "nope"}); r.Status != 400 {
		t.Fatalf("bad harness = %d", r.Status)
	}
	if r := e.do("PUT", "/factories/"+fid+"/agents/"+name+"/spec", e.editor.Key, map[string]any{"idle_timeout_minutes": "99"}); r.Status != 400 {
		t.Fatalf("bad idle = %d", r.Status)
	}
	if r := e.do("PUT", "/factories/"+fid+"/agents/"+name+"/spec", e.editor.Key, map[string]any{"mcp_servers": "- not a mapping"}); r.Status != 400 || !strings.HasPrefix(str(r.Body, "detail"), "mcpServers: ") {
		t.Fatalf("bad mcpServers = %d %s", r.Status, r.Raw)
	}
	prompt := "Be precise."
	r := e.must(e.do("PUT", "/factories/"+fid+"/agents/"+name+"/spec", e.editor.Key, map[string]any{
		"description": "Coordinates", "harness_type": "claude", "model": "sonnet", "auth_secret": "ANTHROPIC_KEY",
		"secrets": []string{"GH_TOKEN", "GH_TOKEN"}, "mcp_servers": "linear:\n  url: https://mcp.example.test\n",
		"idle_timeout_minutes": "15", "prompt": prompt,
	}), 200)
	if str(r.Body, "harness_type") != "claude" || str(r.Body, "auth_secret") != "ANTHROPIC_KEY" || str(r.Body, "prompt") != "Be precise.\n" {
		t.Fatalf("updated spec = %s", r.Raw)
	}
	src := e.activeFiles(fid)[agentPath(name)]
	for _, want := range []string{"agentType:", "type: claude", "secretName: ANTHROPIC_KEY", "idleTimeoutMinutes: 15", "- GH_TOKEN", "url: https://mcp.example.test", "Be precise."} {
		if !strings.Contains(src, want) {
			t.Errorf("agent.md missing %q:\n%s", want, src)
		}
	}
	if strings.Count(src, "GH_TOKEN") != 1 {
		t.Errorf("secrets not deduplicated:\n%s", src)
	}
	// Omitting prompt keeps it.
	e.must(e.do("PUT", "/factories/"+fid+"/agents/"+name+"/spec", e.editor.Key, map[string]any{"harness_type": ""}), 200)
	if src := e.activeFiles(fid)[agentPath(name)]; !strings.Contains(src, "Be precise.") || strings.Contains(src, "harness:") {
		t.Fatalf("after reset:\n%s", src)
	}
	e.must(e.do("GET", "/factories/"+fid+"/agents/nope/spec", e.viewer.Key, nil), 404)
	e.must(e.do("GET", "/factories/"+fid+"/agents/"+name+"/spec", e.other.Key, nil), 404)
}

func TestAutomationEndpoints(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("Payments")
	base := "/factories/" + fid + "/automations/"
	e.must(e.do("GET", base+"nightly", e.viewer.Key, nil), 404)
	if r := e.do("PUT", base+"new", e.editor.Key, map[string]any{"content": NewAutomationTemplate}); r.Status != 400 {
		t.Fatalf("reserved name = %d", r.Status)
	}
	if r := e.do("PUT", base+"nightly", e.editor.Key, map[string]any{"content": "---\nunterminated"}); r.Status != 400 || len(list(r.Body, "errors")) != 1 {
		t.Fatalf("bad content = %d %s", r.Status, r.Raw)
	}
	v := e.must(e.do("POST", base+"nightly/validate", e.viewer.Key, map[string]any{"content": "---\nunterminated"}), 200)
	if v.Body["valid"] != false {
		t.Fatalf("validate bad = %s", v.Raw)
	}
	v = e.must(e.do("POST", base+"nightly/validate", e.viewer.Key, map[string]any{"content": NewAutomationTemplate}), 200)
	if v.Body["valid"] != true {
		t.Fatalf("validate good = %s", v.Raw)
	}
	e.must(e.do("PUT", base+"nightly", e.editor.Key, map[string]any{"content": NewAutomationTemplate}), 200)
	g := e.must(e.do("GET", base+"nightly", e.viewer.Key, nil), 200)
	if str(g.Body, "content") != NewAutomationTemplate || g.Body["editable"] != true {
		t.Fatalf("get = %s", g.Raw)
	}
	e.must(e.do("DELETE", base+"nightly", e.editor.Key, nil), 204)
	if _, ok := e.activeFiles(fid)[automationPath("nightly")]; ok {
		t.Fatal("automation not deleted")
	}
	e.must(e.do("DELETE", base+"nightly", e.editor.Key, nil), 404)
}

func TestRestoreDefinitionVersion(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("Payments")
	vs := e.must(e.do("GET", "/factories/"+fid+"/definition/versions", e.viewer.Key, nil), 200)
	first := str(list(vs.Body, "versions")[0].(map[string]any), "id")
	e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"description": "changed"}), 200)
	r := e.must(e.do("POST", "/factories/"+fid+"/definition/versions/"+first+"/restore", e.editor.Key, nil), 200)
	if str(r.Body, "id") == first || strings.Contains(e.activeFiles(fid)["factory.yaml"], "changed") {
		t.Fatalf("restore = %s", r.Raw)
	}
	e.must(e.do("POST", "/factories/"+fid+"/definition/versions/nope/restore", e.editor.Key, nil), 404)
	e.must(e.do("POST", "/factories/"+fid+"/definition/versions/"+first+"/restore", e.viewer.Key, nil), 403)
}

func TestFactoryTestFilter(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("Payments")
	wr := e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, map[string]any{"name": "deploys"}), 201)
	whID := str(wr.Body, "webhook", "id")
	d := &core.WebhookDelivery{ID: core.NewID("dlv"), WebhookID: whID, DeliveryID: "x1", Status: "accepted", Body: []byte(`{"env":"prod"}`), ReceivedAt: time.Now()}
	if err := e.store.CreateWebhookDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"json object", map[string]any{"delivery_id": d.ID, "filter": map[string]any{"webhook_ids": []string{whID}, "payload": map[string]any{"env": []string{"prod"}}}}, true},
		{"yaml string by name", map[string]any{"delivery_id": d.ID, "filter": "webhook_ids: [deploys]\npayload:\n  env: [staging]\n"}, false},
		{"automation", map[string]any{"delivery_id": d.ID, "automation": "---\ntriggers:\n  - provider: webhook\n    filter:\n      webhook_ids: [deploys]\n      payload:\n        env: [prod]\n---\nGo.\n"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.must(e.do("POST", "/factories/"+fid+"/test-filter", e.viewer.Key, tc.body), 200)
			if r.Body["would_trigger"] != tc.want {
				t.Fatalf("result = %s", r.Raw)
			}
		})
	}
	e.must(e.do("POST", "/factories/"+fid+"/test-filter", e.viewer.Key, map[string]any{"delivery_id": d.ID}), 400)
	e.must(e.do("POST", "/factories/"+fid+"/test-filter", e.viewer.Key, map[string]any{"delivery_id": "nope", "filter": map[string]any{}}), 404)
}

func TestScoreRunAndSelfImprovement(t *testing.T) {
	var scored, improved string
	var improveRuns []string
	e := newTestEnv(t, func(o *Options) {
		o.ScoreRun = func(_ context.Context, scorerID, runID string) (*core.Score, error) {
			scored = scorerID + "/" + runID
			return &core.Score{ID: "sc_1", ScorerID: scorerID, RunID: runID, Passed: true, Label: "good"}, nil
		}
		o.RunSelfImprovement = func(_ context.Context, fid string) ([]string, error) {
			if fid == "" {
				return nil, errors.New("empty")
			}
			improved = fid
			return improveRuns, nil
		}
	})
	ctx := context.Background()
	fid := e.createFactory("Payments")
	scorer := core.Scorer{ID: core.NewID("scr"), FactoryID: fid, Slug: "quality", Name: "Quality"}
	if err := e.store.ReplaceFactoryResources(ctx, fid, nil, nil, []core.Scorer{scorer}, nil); err != nil {
		t.Fatal(err)
	}
	d := e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, map[string]any{"prompt": "fix it"}), 201)
	runID := str(d.Body, "run_id")
	if runID == "" {
		runID = str(d.Body, "run", "run_id")
	}
	r := e.must(e.do("POST", "/runs/"+runID+"/score", e.editor.Key, map[string]any{"scorer_id": "quality"}), 200)
	if scored != scorer.ID+"/"+runID || str(r.Body, "label") != "good" {
		t.Fatalf("scored %q body %s", scored, r.Raw)
	}
	e.must(e.do("POST", "/runs/"+runID+"/score", e.editor.Key, map[string]any{"scorer_id": "nope"}), 404)
	e.must(e.do("POST", "/runs/"+runID+"/score", e.viewer.Key, map[string]any{"scorer_id": "quality"}), 403)
	// The only scorer has selfImprovement off: nothing starts and the
	// response says why.
	r = e.must(e.do("POST", "/factories/"+fid+"/self-improvement", e.editor.Key, nil), 200)
	if b, _ := r.Body["started"].(bool); b || str(r.Body, "reason") == "" || improved != "" {
		t.Fatalf("scorer without selfImprovement: improved %q body %s", improved, r.Raw)
	}
	scorer.SelfImprovement = true
	if err := e.store.ReplaceFactoryResources(ctx, fid, nil, nil, []core.Scorer{scorer}, nil); err != nil {
		t.Fatal(err)
	}
	// No failed score waits for review.
	r = e.must(e.do("POST", "/factories/"+fid+"/self-improvement", e.editor.Key, nil), 200)
	if b, _ := r.Body["started"].(bool); b || str(r.Body, "reason") == "" || improved != fid {
		t.Fatalf("no failures: improved %q body %s", improved, r.Raw)
	}
	improveRuns = []string{"run_improve"}
	r = e.must(e.do("POST", "/factories/"+fid+"/self-improvement", e.editor.Key, nil), 202)
	if b, _ := r.Body["started"].(bool); !b || r.Body["reason"] != nil || !strings.Contains(string(r.Raw), "run_improve") {
		t.Fatalf("started: body %s", r.Raw)
	}
	// A factory without scorers.
	fidEmpty := e.createFactory("Empty")
	r = e.must(e.do("POST", "/factories/"+fidEmpty+"/self-improvement", e.editor.Key, nil), 200)
	if !strings.Contains(str(r.Body, "reason"), "no scorers") {
		t.Fatalf("no scorers: body %s", r.Raw)
	}
	// Without the hooks the endpoints report feature_not_available.
	e2 := newTestEnv(t)
	fid2 := e2.createFactory("Other")
	if r := e2.do("POST", "/factories/"+fid2+"/self-improvement", e2.editor.Key, nil); errCode(r) != CodeFeatureNotAvailable {
		t.Fatalf("no hook = %s", r.Raw)
	}
}

func TestStreamSSE(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("Payments")
	r := e.must(e.do("POST", "/runs", e.viewer.Key, runBody(fid, "hello")), 201)
	runID := str(r.Body, "run_id")

	open := func(query string) (*bufio.Reader, func()) {
		req, _ := http.NewRequest("GET", e.ts.URL+BasePath+"/stream"+query, nil)
		req.Header.Set("Authorization", "Bearer "+e.viewer.Key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatalf("stream status %d %s", res.StatusCode, res.Header.Get("Content-Type"))
		}
		return bufio.NewReader(res.Body), func() { res.Body.Close() }
	}
	// next reads events until one of the wanted type arrives.
	next := func(rd *bufio.Reader, want string) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		ev := ""
		for time.Now().Before(deadline) {
			line, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("read: %v (waiting for %s)", err, want)
			}
			line = strings.TrimRight(line, "\n")
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok && ev == want {
				return v
			}
		}
		t.Fatalf("timed out waiting for %s", want)
		return ""
	}

	rd, closeFn := open("?run_id=" + runID)
	defer closeFn()
	next(rd, "ready")
	m := &core.RunMessage{RunID: runID, Role: "assistant", Content: "streamed hello"}
	_ = e.store.AppendRunMessage(ctx, m)
	e.bus.Publish(ctx, core.TopicRunMessage, m)
	if data := next(rd, "message"); !strings.Contains(data, `"seq":1`) {
		t.Fatalf("replayed message data = %s", data) // the run's prompt, replayed first
	}
	if data := next(rd, "message"); !strings.Contains(data, "streamed hello") {
		t.Fatalf("message data = %s", data)
	}
	run, _ := e.store.GetRun(ctx, runID)
	run.State = core.RunRunning
	e.bus.Publish(ctx, core.TopicRunUpdated, run)
	if data := next(rd, "run"); !strings.Contains(data, `"RUNNING"`) {
		t.Fatalf("run data = %s", data)
	}

	// Inbox events are team scoped: another team's item is never delivered.
	rd2, close2 := open("?topics=inbox")
	defer close2()
	next(rd2, "ready")
	otherF := &core.Factory{ID: core.NewID("fac"), TeamID: "team_other", Name: "X", Alias: "x", Source: core.DefinitionManaged}
	_ = e.store.CreateFactory(ctx, otherF)
	e.bus.Publish(ctx, core.TopicInbox, &core.InboxItem{ID: "in_other", FactoryID: otherF.ID, Title: "secret"})
	e.bus.Publish(ctx, core.TopicInbox, &core.InboxItem{ID: "in_mine", FactoryID: fid, Title: "approve me"})
	if data := next(rd2, "inbox"); !strings.Contains(data, "in_mine") {
		t.Fatalf("inbox data = %s", data)
	}

	// Validation.
	e.must(e.do("GET", "/stream?topics=bogus", e.viewer.Key, nil), 400)
	e.must(e.do("GET", "/stream?run_id=nope", e.viewer.Key, nil), 404)
	e.must(e.do("GET", "/stream?run_id="+runID, e.other.Key, nil), 404)
}
