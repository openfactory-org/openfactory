package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

type fakeGitHub struct {
	t          *testing.T
	key        *rsa.PrivateKey
	tokenCalls atomic.Int32
	mu         sync.Mutex
	requests   []string
	bodies     map[string]map[string]any
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &body)
		f.bodies[r.Method+" "+r.URL.Path] = body
	}
	f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	switch {
	case r.URL.Path == "/app/installations/42/access_tokens":
		f.tokenCalls.Add(1)
		j, err := forgeutil.ParseJWT(strings.TrimPrefix(auth, "Bearer "))
		if err != nil || j.VerifyRS256(&f.key.PublicKey) != nil || j.ClaimString("iss") != "123" {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"token":"ghs_install","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		return
	case auth != "Bearer ghs_install" && auth != "token ghp_pat":
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /repos/acme/api":
		fmt.Fprint(w, `{"default_branch":"main"}`)
	case "POST /repos/acme/api/pulls":
		fmt.Fprint(w, `{"number":7,"html_url":"https://github.com/acme/api/pull/7","state":"open","draft":true,"head":{"ref":"factory/x"}}`)
	case "GET /repos/acme/api/pulls/7":
		fmt.Fprint(w, `{"number":7,"html_url":"u","state":"closed","merged":true,"head":{"ref":"factory/x"},"labels":[{"name":"bug"}],"user":{"login":"alice"}}`)
	case "GET /repos/acme/api/pulls/7/files":
		fmt.Fprint(w, `[{"filename":"src/a.go"},{"filename":"factory/agents/foreman/agent.md"}]`)
	case "POST /repos/acme/api/issues/7/comments", "POST /repos/acme/api/check-runs", "POST /repos/acme/api/statuses/abc", "POST /repos/acme/api/issues/7/labels":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":99}`)
	case "GET /repos/acme/api/commits/main":
		fmt.Fprint(w, `{"sha":"c0ffee","commit":{"tree":{"sha":"tree1"}}}`)
	case "GET /repos/acme/api/git/trees/tree1":
		fmt.Fprint(w, `{"truncated":false,"tree":[
			{"path":"factory/factory.yaml","type":"blob","sha":"b1","size":10},
			{"path":"factory/agents","type":"tree","sha":"t2"},
			{"path":"factory/agents/foreman/agent.md","type":"blob","sha":"b2","size":10},
			{"path":"README.md","type":"blob","sha":"b3","size":10}]}`)
	case "GET /repos/acme/api/git/blobs/b1":
		fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte("name: f\n")))
	case "GET /repos/acme/api/git/blobs/b2":
		fmt.Fprintf(w, `{"encoding":"base64","content":%q}`, base64.StdEncoding.EncodeToString([]byte("# foreman")))
	default:
		http.NotFound(w, r)
	}
}

func newFake(t *testing.T) (*fakeGitHub, *httptest.Server, []byte) {
	k, pemKey := testKey(t)
	f := &fakeGitHub{t: t, key: k, bodies: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv, pemKey
}

func TestAppClient(t *testing.T) {
	f, srv, pemKey := newFake(t)
	c, err := New(Config{BaseURL: srv.URL, AppID: "123", PrivateKey: pemKey, InstallationID: 42})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if b, err := c.DefaultBranch(ctx, "acme", "api"); err != nil || b != "main" {
		t.Fatalf("DefaultBranch = %q, %v", b, err)
	}
	u, err := c.CloneURL(ctx, "acme", "api")
	if err != nil || !strings.Contains(u, "x-access-token:ghs_install@") || !strings.HasSuffix(u, "/acme/api.git") {
		t.Fatalf("CloneURL = %q, %v", u, err)
	}
	if n := f.tokenCalls.Load(); n != 1 {
		t.Fatalf("installation token minted %d times, want 1 (cached)", n)
	}
	pr, err := c.CreatePullRequest(ctx, core.PullRequestInput{Owner: "acme", Repo: "api", Head: "factory/x", Base: "main", Title: "T", Draft: true})
	if err != nil || pr.Number != 7 || pr.State != "draft" || pr.Forge != "GITHUB" {
		t.Fatalf("CreatePullRequest = %+v, %v", pr, err)
	}
	if f.bodies["POST /repos/acme/api/pulls"]["draft"] != true {
		t.Fatal("draft flag not sent")
	}
	got, err := c.GetPullRequest(ctx, "acme", "api", 7)
	if err != nil || got.State != "merged" {
		t.Fatalf("GetPullRequest = %+v, %v", got, err)
	}
	if err := c.Comment(ctx, "acme", "api", 7, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCommitStatus(ctx, "acme", "api", "abc", core.CommitStatus{Context: CheckName("factory"), State: "failure", Description: "bad"}); err != nil {
		t.Fatal(err)
	}
	cr := f.bodies["POST /repos/acme/api/check-runs"]
	if cr["name"] != "openfactory/definition (factory)" || cr["conclusion"] != "failure" || cr["head_sha"] != "abc" {
		t.Fatalf("check run body = %v", cr)
	}
	files, sha, err := c.GetFiles(ctx, "acme", "api", "main", "factory")
	if err != nil || sha != "c0ffee" {
		t.Fatalf("GetFiles: %v %v", sha, err)
	}
	if len(files) != 2 || files["factory.yaml"] != "name: f\n" || files["agents/foreman/agent.md"] != "# foreman" {
		t.Fatalf("files = %v", files)
	}
}

func TestPATUsesStatuses(t *testing.T) {
	f, srv, _ := newFake(t)
	c, err := New(Config{BaseURL: srv.URL, Token: "ghp_pat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetCommitStatus(context.Background(), "acme", "api", "abc", core.CommitStatus{Context: "openfactory/definition", State: "success", Description: strings.Repeat("x", 200)}); err != nil {
		t.Fatal(err)
	}
	b := f.bodies["POST /repos/acme/api/statuses/abc"]
	if b["state"] != "success" || len([]rune(b["description"].(string))) != 140 {
		t.Fatalf("status body = %v", b)
	}
}

func TestDeriveWebURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com":          "https://github.com",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com",
	} {
		if got := deriveWebURL(strings.TrimRight(in, "/")); got != want {
			t.Errorf("deriveWebURL(%q) = %q want %q", in, got, want)
		}
	}
}

// ---------- webhook ----------

type staticResolver struct{ f core.Forge }

func (s staticResolver) Forge(context.Context, core.ForgeLookup) (core.Forge, error) { return s.f, nil }

func deliver(t *testing.T, h *WebhookHandler, event, secret string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(b)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", core.NewID("d"))
	req.Header.Set("X-Hub-Signature-256", "sha256="+forgeutil.HMACSHA256Hex([]byte(secret), b))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	h.Drain()
	return rec
}

func webhookFixture(t *testing.T) (*WebhookHandler, *forgetest.Store, *forgetest.Bus, *fakeGitHub, *[]string, *[]DefinitionPR) {
	f, srv, _ := newFake(t)
	gh, err := New(Config{BaseURL: srv.URL, Token: "ghp_pat"})
	if err != nil {
		t.Fatal(err)
	}
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_gh", TeamID: "team_1", Provider: "github", Config: map[string]string{"installation_id": "42", "org": "acme", "app_id": "7", "app_slug": "openfactory", "webhook_secret_ref": "GH_WEBHOOK"}}}
	store.Factories = []core.Factory{
		{ID: "fac_1", TeamID: "team_1", Alias: "payments", Source: core.DefinitionGit, GitRepo: "github.com/acme/api", GitBranch: "main", GitPath: "factory"},
	}
	store.Users = []core.User{{ID: "usr_1", TeamID: "team_1", Email: "alice@example.com"}}
	store.Identities["github|1001"] = core.ExternalIdentity{UserID: "usr_1", Provider: "github", ExternalID: "1001", Handle: "alice"}
	bus := forgetest.NewBus()
	var pushes []string
	var prs []DefinitionPR
	var mu sync.Mutex
	h := NewWebhookHandler(WebhookOptions{
		Store: store, Secrets: forgetest.NewSecrets(map[string]string{"GH_WEBHOOK": "whsec"}), Bus: bus, Resolver: staticResolver{gh},
		OnDefinitionPush: func(_ context.Context, repo, branch, sha string) {
			mu.Lock()
			pushes = append(pushes, repo+"@"+branch+"="+sha)
			mu.Unlock()
		},
		OnDefinitionPR: func(_ context.Context, pr DefinitionPR) {
			mu.Lock()
			prs = append(prs, pr)
			mu.Unlock()
		},
	})
	return h, store, bus, f, &pushes, &prs
}

var repo = map[string]any{"full_name": "acme/api"}
var installation = map[string]any{"id": 42}

func TestWebhookSignature(t *testing.T) {
	h, _, bus, _, _, _ := webhookFixture(t)
	rec := deliver(t, h, "issues", "wrong", map[string]any{"action": "opened", "repository": repo, "installation": installation})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(bus.Events()) != 0 {
		t.Fatal("events published for unsigned delivery")
	}
	rec = deliver(t, h, "ping", "whsec", map[string]any{"zen": "hi", "repository": repo, "installation": installation})
	if rec.Code != http.StatusOK {
		t.Fatalf("ping status = %d", rec.Code)
	}
	// The ping of an app has no installation and no repository: the secret
	// of a record of that app checks it.
	rec = deliver(t, h, "ping", "whsec", map[string]any{"zen": "hi", "hook": map[string]any{"app_id": 7}})
	if rec.Code != http.StatusOK {
		t.Fatalf("app ping status = %d", rec.Code)
	}
	rec = deliver(t, h, "ping", "whsec", map[string]any{"zen": "hi", "hook": map[string]any{"app_id": 8}})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("ping of another app: status = %d", rec.Code)
	}
}

// TestWebhookMentionNeedsRecordSlug: without config.app_slug on the record,
// "@openfactory" is not a mention of the app.
func TestWebhookMentionNeedsRecordSlug(t *testing.T) {
	routed := func(withSlug bool) bool {
		h, store, bus, _, _, _ := webhookFixture(t)
		if !withSlug {
			delete(store.Integrations[0].Config, "app_slug")
		}
		deliver(t, h, "issue_comment", "whsec", map[string]any{
			"action": "created", "repository": repo, "installation": installation,
			"sender":  map[string]any{"id": 1001, "login": "alice", "type": "User"},
			"issue":   map[string]any{"number": 5, "title": "Crash", "html_url": "https://github.com/acme/api/issues/5", "user": map[string]any{"login": "alice"}},
			"comment": map[string]any{"id": 1, "body": "@openfactory please fix", "user": map[string]any{"login": "alice"}},
		})
		for _, ev := range bus.Events() {
			if ev.FactoryID == "fac_1" {
				return true
			}
		}
		return false
	}
	if !routed(true) {
		t.Fatal("@<app_slug> mention was not routed with config.app_slug on the record")
	}
	if routed(false) {
		t.Fatal("@openfactory mention was routed without config.app_slug")
	}
}

func TestWebhookIssueOpened(t *testing.T) {
	h, _, bus, _, _, _ := webhookFixture(t)
	rec := deliver(t, h, "issues", "whsec", map[string]any{
		"action": "opened", "repository": repo, "installation": installation,
		"sender": map[string]any{"id": 1001, "login": "alice", "type": "User"},
		"issue": map[string]any{
			"number": 5, "title": "Crash", "body": "@openfactory please fix\n```\n@nobody\n```", "html_url": "https://github.com/acme/api/issues/5",
			"user": map[string]any{"login": "alice"}, "labels": []any{map[string]any{"name": "factory:payments"}, map[string]any{"name": "bug"}},
			"assignees": []any{map[string]any{"login": "bob"}},
		},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	types := strings.Join(bus.EventTypes(), ",")
	if types != "issue_created,issue_labeled,issue_mentioned" {
		t.Fatalf("types = %s", types)
	}
	ev := bus.Find("issue_mentioned")
	if ev.TeamID != "team_1" || ev.FactoryID != "fac_1" || ev.Actor.UserID != "usr_1" || ev.Actor.IsBot {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Source.Key() != "github:acme/api#5" || ev.Source.Kind != "issue" {
		t.Fatalf("source = %+v", ev.Source)
	}
	m := ev.Attributes[eventattr.Mentioned].([]string)
	if len(m) != 1 || m[0] != "openfactory" {
		t.Fatalf("mentioned = %v", m)
	}
	lab := bus.Find("issue_labeled")
	if added := lab.Attributes[eventattr.AddedLabels].([]string); len(added) != 2 {
		t.Fatalf("added labels = %v", added)
	}
	if lab.Attributes[eventattr.Repo] != "acme/api" || lab.Attributes[eventattr.Author] != "alice" {
		t.Fatalf("attrs = %v", lab.Attributes)
	}
}

func TestWebhookPullRequestEvents(t *testing.T) {
	h, _, bus, f, _, prs := webhookFixture(t)
	pr := map[string]any{
		"number": 7, "title": "Change", "body": "", "html_url": "https://github.com/acme/api/pull/7", "draft": false, "merged": true,
		"user": map[string]any{"login": "dependabot[bot]", "type": "Bot"}, "labels": []any{},
		"head": map[string]any{"ref": "feature", "sha": "abc"}, "base": map[string]any{"ref": "main"},
	}
	sender := map[string]any{"id": 5, "login": "dependabot[bot]", "type": "Bot"}
	deliver(t, h, "pull_request", "whsec", map[string]any{"action": "opened", "repository": repo, "installation": installation, "sender": sender, "pull_request": pr})
	if got := strings.Join(bus.EventTypes(), ","); got != "pull_request_opened,pull_request_ready" {
		t.Fatalf("types = %s", got)
	}
	ev := bus.Find("pull_request_opened")
	if !ev.Actor.IsBot || ev.Attributes[eventattr.BaseBranch] != "main" || ev.Attributes[eventattr.PRNumber] != 7 {
		t.Fatalf("attrs = %v", ev.Attributes)
	}
	paths, _ := ev.Attributes[eventattr.Paths].([]string)
	if len(paths) != 2 {
		t.Fatalf("paths not enriched: %v", ev.Attributes[eventattr.Paths])
	}
	// The PR touches factory/agents/... so the definition check hook fires.
	if len(*prs) != 1 || (*prs)[0].CheckName != "openfactory/definition (factory)" || (*prs)[0].HeadSHA != "abc" || len((*prs)[0].ChangedPaths) != 1 {
		t.Fatalf("definition PRs = %+v", *prs)
	}
	_ = f

	deliver(t, h, "pull_request", "whsec", map[string]any{"action": "closed", "repository": repo, "installation": installation, "sender": sender, "pull_request": pr})
	if bus.Find("pull_request_merged") == nil || bus.Find("pull_request_closed").Attributes[eventattr.Merged] != true {
		t.Fatal("merge events missing")
	}

	review := map[string]any{"body": "LGTM @carol", "state": "APPROVED", "user": map[string]any{"login": "dave"}}
	deliver(t, h, "pull_request_review", "whsec", map[string]any{"action": "submitted", "repository": repo, "installation": installation, "sender": map[string]any{"id": 9, "login": "dave"}, "pull_request": pr, "review": review})
	rs := bus.Find("pull_request_review_submitted")
	if rs == nil || rs.Attributes[eventattr.ReviewState] != "approved" || rs.Attributes[eventattr.Author] != "dave" || rs.Text != "LGTM @carol" {
		t.Fatalf("review event = %+v", rs)
	}
}

func TestWebhookUnchangedDefinitionPasses(t *testing.T) {
	h, store, _, f, _, prs := webhookFixture(t)
	store.Factories[0].GitPath = "other"
	pr := map[string]any{"number": 7, "head": map[string]any{"ref": "x", "sha": "abc"}, "base": map[string]any{"ref": "main"}, "user": map[string]any{"login": "a"}}
	deliver(t, h, "pull_request", "whsec", map[string]any{"action": "synchronize", "repository": repo, "installation": installation, "sender": map[string]any{"login": "a"}, "pull_request": pr})
	if len(*prs) != 0 {
		t.Fatal("definition hook called for unrelated change")
	}
	b := f.bodies["POST /repos/acme/api/statuses/abc"]
	if b["state"] != "success" || b["context"] != "openfactory/definition (other)" {
		t.Fatalf("expected immediate success status, got %v", b)
	}
}

func TestWebhookPushAndCI(t *testing.T) {
	h, _, bus, _, pushes, _ := webhookFixture(t)
	deliver(t, h, "push", "whsec", map[string]any{
		"ref": "refs/heads/main", "after": "deadbeef", "repository": repo, "installation": installation,
		"sender":  map[string]any{"login": "alice"},
		"commits": []any{map[string]any{"added": []string{"a.go"}, "modified": []string{"b.go"}, "removed": []string{}}},
	})
	if len(*pushes) != 1 || (*pushes)[0] != "acme/api@main=deadbeef" {
		t.Fatalf("pushes = %v", *pushes)
	}
	ev := bus.Find("push")
	if ev == nil || ev.Attributes[eventattr.Branch] != "main" || len(ev.Attributes[eventattr.Paths].([]string)) != 2 {
		t.Fatalf("push event = %+v", ev)
	}
	deliver(t, h, "workflow_run", "whsec", map[string]any{
		"action": "completed", "repository": repo, "installation": installation, "sender": map[string]any{"login": "github-actions[bot]", "type": "Bot"},
		"workflow_run": map[string]any{"id": 1, "name": "CI", "head_branch": "feature", "conclusion": "failure",
			"pull_requests": []any{map[string]any{"number": 7, "base": map[string]any{"ref": "main"}}}},
	})
	wr := bus.Find("workflow_run_completed")
	if wr == nil || wr.Attributes[eventattr.Workflow] != "CI" || wr.Attributes[eventattr.Author] != "alice" || wr.Attributes[eventattr.LinkedPRCount] != 1 {
		t.Fatalf("workflow event = %+v", wr)
	}
	deliver(t, h, "status", "whsec", map[string]any{"state": "pending", "sha": "x", "context": "ci", "repository": repo, "installation": installation, "sender": map[string]any{"login": "a"}})
	if bus.Find("commit_status") != nil {
		t.Fatal("pending status must not emit an event")
	}
}

func TestWebhookUnlabeledMentionRouting(t *testing.T) {
	h, store, bus, f, _, _ := webhookFixture(t)
	store.Factories[0].Source = core.DefinitionManaged
	deliver(t, h, "issue_comment", "whsec", map[string]any{
		"action": "created", "repository": repo, "installation": installation,
		"sender":  map[string]any{"id": 1001, "login": "alice"},
		"issue":   map[string]any{"number": 7, "title": "x", "user": map[string]any{"login": "alice"}, "labels": []any{}},
		"comment": map[string]any{"body": "@openfactory take this", "user": map[string]any{"login": "alice"}},
	})
	ev := bus.Find("issue_mentioned")
	if ev == nil || ev.FactoryID != "fac_1" || ev.Attributes[eventattr.Alias] != "payments" {
		t.Fatalf("mention not routed: %+v", ev)
	}
	if lb := f.bodies["POST /repos/acme/api/issues/7/labels"]; lb == nil {
		t.Fatal("factory label not added")
	}
	if c := bus.Find("issue_commented"); c.Attributes[eventattr.Author] != "alice" {
		t.Fatalf("comment author = %v", c.Attributes[eventattr.Author])
	}
}
