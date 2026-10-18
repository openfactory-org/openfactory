package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

func TestClient(t *testing.T) {
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "glpat" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			bodies[r.Method+" "+r.URL.EscapedPath()] = m
		}
		switch r.Method + " " + r.URL.EscapedPath() {
		case "GET /api/v4/projects/acme%2Fplatform%2Fapi":
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case "POST /api/v4/projects/acme%2Fplatform%2Fapi/merge_requests":
			fmt.Fprint(w, `{"iid":3,"web_url":"https://gl/mr/3","state":"opened","draft":true,"source_branch":"factory/x"}`)
		case "GET /api/v4/projects/acme%2Fplatform%2Fapi/merge_requests/3":
			fmt.Fprint(w, `{"iid":3,"state":"merged","source_branch":"factory/x"}`)
		case "POST /api/v4/projects/acme%2Fplatform%2Fapi/merge_requests/3/notes",
			"POST /api/v4/projects/acme%2Fplatform%2Fapi/issues/4/notes",
			"POST /api/v4/projects/acme%2Fplatform%2Fapi/statuses/abc":
			w.WriteHeader(201)
			fmt.Fprint(w, `{}`)
		case "GET /api/v4/projects/acme%2Fplatform%2Fapi/repository/commits/main":
			fmt.Fprint(w, `{"id":"sha1"}`)
		case "GET /api/v4/projects/acme%2Fplatform%2Fapi/repository/tree":
			if r.URL.Query().Get("ref") != "sha1" || r.URL.Query().Get("path") != "def" {
				t.Errorf("tree query = %v", r.URL.Query())
			}
			fmt.Fprint(w, `[{"path":"def/factory.yaml","type":"blob"},{"path":"def/agents","type":"tree"}]`)
		case "GET /api/v4/projects/acme%2Fplatform%2Fapi/repository/files/def%2Ffactory.yaml/raw":
			fmt.Fprint(w, "name: x\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "glpat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	u, _ := c.CloneURL(ctx, "acme/platform", "api")
	if !strings.Contains(u, "oauth2:glpat@") || !strings.HasSuffix(u, "/acme/platform/api.git") {
		t.Fatalf("clone url = %s", u)
	}
	pr, err := c.CreatePullRequest(ctx, core.PullRequestInput{Owner: "acme/platform", Repo: "api", Head: "factory/x", Title: "Fix", Draft: true})
	if err != nil || pr.Number != 3 || pr.State != "draft" {
		t.Fatalf("pr = %+v %v", pr, err)
	}
	mr := bodies["POST /api/v4/projects/acme%2Fplatform%2Fapi/merge_requests"]
	if mr["title"] != "Draft: Fix" || mr["target_branch"] != "main" {
		t.Fatalf("mr body = %v", mr)
	}
	if got, _ := c.GetPullRequest(ctx, "acme/platform", "api", 3); got.State != "merged" {
		t.Fatalf("state = %s", got.State)
	}
	if err := c.Comment(ctx, "acme/platform", "api", 3, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := c.CommentIssue(ctx, "acme/platform", "api", 4, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCommitStatus(ctx, "acme/platform", "api", "abc", core.CommitStatus{Context: "openfactory/definition", State: "error"}); err != nil {
		t.Fatal(err)
	}
	if bodies["POST /api/v4/projects/acme%2Fplatform%2Fapi/statuses/abc"]["state"] != "failed" {
		t.Fatal("error should map to failed")
	}
	files, sha, err := c.GetFiles(ctx, "acme/platform", "api", "main", "def")
	if err != nil || sha != "sha1" || files["factory.yaml"] != "name: x\n" || len(files) != 1 {
		t.Fatalf("files = %v %s %v", files, sha, err)
	}
}

func post(t *testing.T, h *WebhookHandler, event, token string, payload any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(b)))
	req.Header.Set("X-Gitlab-Event", event)
	req.Header.Set("X-Gitlab-Token", token)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestWebhook(t *testing.T) {
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_gl", TeamID: "team_1", Provider: "gitlab",
		Config: map[string]string{"org": "acme", "factory_bots": "payments-of-01k=payments", "webhook_secret_ref": "GL_HOOK"}}}
	store.Factories = []core.Factory{{ID: "fac_1", TeamID: "team_1", Alias: "payments"}}
	secrets := forgetest.NewSecrets(map[string]string{"GL_HOOK": "tok"})
	bus := forgetest.NewBus()
	h := NewWebhookHandler(WebhookOptions{Store: store, Secrets: secrets, Bus: bus})
	project := map[string]any{"id": 1, "path_with_namespace": "acme/platform/api"}

	if rec := post(t, h, "Merge Request Hook", "nope", map[string]any{"object_kind": "merge_request", "project": project}); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	post(t, h, "Merge Request Hook", "tok", map[string]any{
		"object_kind": "merge_request", "user": map[string]any{"id": 2, "username": "alice"}, "project": project,
		"object_attributes": map[string]any{"iid": 3, "title": "T", "action": "open", "target_branch": "main", "source_branch": "f", "url": "https://gl/mr/3"},
		"labels":            []any{map[string]any{"title": "bug"}},
	})
	mr := bus.Find("merge_request")
	if mr == nil || mr.Attributes[eventattr.Action] != "open" || mr.Attributes[eventattr.BaseBranch] != "main" || mr.Attributes[eventattr.Repo] != "acme/platform/api" {
		t.Fatalf("mr event = %+v", mr)
	}
	if mr.Source.Key() != "gitlab:acme/platform/api#3" {
		t.Fatalf("key = %s", mr.Source.Key())
	}
	post(t, h, "Note Hook", "tok", map[string]any{
		"object_kind": "note", "user": map[string]any{"id": 2, "username": "alice"}, "project": project,
		"object_attributes": map[string]any{"note": "@payments-of-01k please review", "noteable_type": "MergeRequest", "action": "create"},
		"merge_request":     map[string]any{"iid": 3, "title": "T"},
	})
	bm := bus.Find("bot_mentioned")
	if bm == nil || bm.FactoryID != "fac_1" || bm.Attributes[eventattr.Mentioned].([]string)[0] != "payments-of-01k" {
		t.Fatalf("bot_mentioned = %+v", bm)
	}
	// The bot's own notes never trigger work.
	n := len(bus.Events())
	post(t, h, "Note Hook", "tok", map[string]any{
		"object_kind": "note", "user": map[string]any{"id": 3, "username": "payments-of-01k"}, "project": project,
		"object_attributes": map[string]any{"note": "@payments-of-01k done", "noteable_type": "MergeRequest"},
		"merge_request":     map[string]any{"iid": 3},
	})
	for _, ev := range bus.Events()[n:] {
		if ev.Type == "bot_mentioned" || ev.Attributes[eventattr.ActorIsSelf] != true {
			t.Fatalf("self activity not marked/ignored: %+v", ev)
		}
	}
	post(t, h, "Push Hook", "tok", map[string]any{"object_kind": "push", "ref": "refs/heads/main", "after": "abc", "user_username": "alice", "project": project,
		"commits": []any{map[string]any{"added": []string{"x"}}}})
	if p := bus.Find("push"); p == nil || p.Attributes[eventattr.Branch] != "main" {
		t.Fatalf("push = %+v", p)
	}
}
