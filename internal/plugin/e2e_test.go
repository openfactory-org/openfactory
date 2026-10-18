package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// buildPlugin compiles a plugin main package into dir as openfactory-plugin-<name>.
func buildPlugin(t *testing.T, dir, name, pkg string) {
	t.Helper()
	out, err := exec.Command("go", "build", "-o", filepath.Join(dir, BinaryPrefix+name), pkg).CombinedOutput()
	if err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, out)
	}
}

// fakeGitLab is a minimal GitLab REST v4 server.
func fakeGitLab(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "glpat" {
			http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.EscapedPath()
		mu.Lock()
		reqs = append(reqs, key+" "+string(body))
		mu.Unlock()
		const p = "/api/v4/projects/acme%2Fplatform%2Fapi"
		switch key {
		case "GET " + p:
			fmt.Fprint(w, `{"default_branch":"main"}`)
		case "POST " + p + "/merge_requests":
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			fmt.Fprintf(w, `{"iid":3,"web_url":"https://gl/mr/3","state":"opened","draft":true,"source_branch":%q}`, in["source_branch"])
		case "GET " + p + "/merge_requests/3":
			fmt.Fprint(w, `{"iid":3,"state":"merged","source_branch":"factory/x"}`)
		case "GET " + p + "/merge_requests/99":
			http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
		case "POST " + p + "/merge_requests/3/notes", "POST " + p + "/issues/4/notes", "POST " + p + "/statuses/abc":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		case "GET " + p + "/repository/commits/main":
			fmt.Fprint(w, `{"id":"sha1"}`)
		case "GET " + p + "/repository/tree":
			fmt.Fprint(w, `[{"path":"def/factory.yaml","type":"blob"}]`)
		case "GET " + p + "/repository/files/def%2Ffactory.yaml/raw":
			fmt.Fprint(w, "name: x\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), reqs...) }
}

// TestGitLabPluginEndToEnd builds plugins/gitlab, launches it through the
// host and exercises the Forge and Ingress services against a fake GitLab.
func TestGitLabPluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin binary")
	}
	dir := t.TempDir()
	buildPlugin(t, dir, "gitlab", "github.com/openfactory-org/openfactory/plugins/gitlab")
	gl, requests := fakeGitLab(t)

	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_gl", TeamID: "team_1", Provider: "gitlab", SecretRef: "GL_TOKEN",
		Config: map[string]string{"base_url": gl.URL, "org": "acme", "factory_bots": "payments-of-01k=payments", "webhook_secret_ref": "GL_HOOK"}}}
	store.Factories = []core.Factory{{ID: "fac_1", TeamID: "team_1", Alias: "payments"}}
	store.Identities["gitlab|2"] = core.ExternalIdentity{Provider: "gitlab", ExternalID: "2", UserID: "usr_alice"}
	secrets := forgetest.NewSecrets(map[string]string{"GL_TOKEN": "glpat", "GL_HOOK": "hook-token"})
	published := make(chan core.Event, 32)
	host := New(Options{
		Dir: dir, Store: store, Secrets: secrets,
		Publish:        func(_ context.Context, ev core.Event) error { published <- ev; return nil },
		HealthInterval: 100 * time.Millisecond, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
	})
	defer host.Close()
	ctx := context.Background()
	if err := host.Start(ctx); err != nil {
		t.Fatal(err)
	}
	infos := host.Plugins()
	if len(infos) != 1 || infos[0].Name != "gitlab" || !infos[0].Running || infos[0].ForgeProviders[0] != "gitlab" {
		t.Fatalf("plugins = %+v", infos)
	}

	resolver := forge.NewResolver(store, secrets, forge.Options{Plugins: host})
	exerciseForge := func(t *testing.T) {
		f, err := resolver.Forge(ctx, core.ForgeLookup{TeamID: "team_1", CodeForge: forge.GitLab, Repo: "acme/platform/api"})
		if err != nil {
			t.Fatal(err)
		}
		if f.Name() != forge.GitLab {
			t.Fatalf("name = %s", f.Name())
		}
		u, err := f.CloneURL(ctx, "acme/platform", "api")
		if err != nil || !strings.Contains(u, "oauth2:glpat@") || !strings.HasSuffix(u, "/acme/platform/api.git") {
			t.Fatalf("clone url = %s %v", u, err)
		}
		pr, err := f.CreatePullRequest(ctx, core.PullRequestInput{Owner: "acme/platform", Repo: "api", Head: "factory/x", Title: "Fix", Draft: true})
		if err != nil || pr.Number != 3 || pr.State != "draft" || pr.Forge != forge.GitLab || pr.Branch != "factory/x" {
			t.Fatalf("pr = %+v %v", pr, err)
		}
		if got, err := f.GetPullRequest(ctx, "acme/platform", "api", 3); err != nil || got.State != "merged" {
			t.Fatalf("get pr = %+v %v", got, err)
		}
		if _, err := f.GetPullRequest(ctx, "acme/platform", "api", 99); err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("missing pr: %v", err)
		}
		if err := f.Comment(ctx, "acme/platform", "api", 3, "on the MR"); err != nil {
			t.Fatal(err)
		}
		if err := f.(forge.IssueCommenter).CommentIssue(ctx, "acme/platform", "api", 4, "on the issue"); err != nil {
			t.Fatal(err)
		}
		if err := f.SetCommitStatus(ctx, "acme/platform", "api", "abc", core.CommitStatus{Context: "openfactory/definition", State: "error"}); err != nil {
			t.Fatal(err)
		}
		files, sha, err := f.GetFiles(ctx, "acme/platform", "api", "main", "def")
		if err != nil || sha != "sha1" || files["factory.yaml"] != "name: x\n" || len(files) != 1 {
			t.Fatalf("files = %v %s %v", files, sha, err)
		}
	}
	t.Run("forge", func(t *testing.T) {
		exerciseForge(t)
		all := strings.Join(requests(), "\n")
		for _, want := range []string{`"title":"Draft: Fix"`, `"target_branch":"main"`, `"state":"failed"`, "/issues/4/notes", `"body":"on the MR"`} {
			if !strings.Contains(all, want) {
				t.Errorf("fake GitLab did not see %s in:\n%s", want, all)
			}
		}
	})

	mux := http.NewServeMux()
	host.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(path, event, token string, payload any) int {
		b, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(string(b)))
		req.Header.Set("X-Gitlab-Event", event)
		req.Header.Set("X-Gitlab-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	next := func(typ string) core.Event {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev := <-published:
				if ev.Type == typ {
					return ev
				}
			case <-timeout:
				t.Fatalf("no %s event", typ)
			}
		}
	}
	project := map[string]any{"id": 1, "path_with_namespace": "acme/platform/api"}

	t.Run("ingress", func(t *testing.T) {
		if code := post("/plugins/gitlab/webhook", "Merge Request Hook", "nope", map[string]any{"object_kind": "merge_request", "project": project}); code != http.StatusUnauthorized {
			t.Fatalf("bad token status = %d", code)
		}
		code := post("/plugins/gitlab/webhook", "Merge Request Hook", "hook-token", map[string]any{
			"object_kind": "merge_request", "user": map[string]any{"id": 2, "username": "alice"}, "project": project,
			"object_attributes": map[string]any{"iid": 3, "title": "T", "action": "open", "target_branch": "main", "source_branch": "f", "url": "https://gl/mr/3"},
			"labels":            []any{map[string]any{"title": "bug"}},
		})
		if code != http.StatusAccepted {
			t.Fatalf("status = %d", code)
		}
		mr := next("merge_request")
		labels, _ := mr.Attributes[eventattr.Labels].([]string)
		if mr.TeamID != "team_1" || mr.Attributes[eventattr.Action] != "open" || mr.Attributes[eventattr.PRNumber] != 3 ||
			len(labels) != 1 || mr.Source.Key() != "gitlab:acme/platform/api#3" || mr.Actor.UserID != "usr_alice" {
			t.Fatalf("mr event = %+v", mr)
		}
		// A note on the merge request mentions the factory.
		code = post("/plugins/gitlab/webhook", "Note Hook", "hook-token", map[string]any{
			"object_kind": "note", "user": map[string]any{"id": 2, "username": "alice"}, "project": project,
			"object_attributes": map[string]any{"note": "@payments-of-01k please review", "noteable_type": "MergeRequest", "action": "create"},
			"merge_request":     map[string]any{"iid": 3, "title": "T"},
		})
		if code != http.StatusAccepted {
			t.Fatalf("note status = %d", code)
		}
		bm := next("bot_mentioned")
		if m, _ := bm.Attributes[eventattr.Mentioned].([]string); bm.FactoryID != "fac_1" || len(m) != 1 || m[0] != "payments-of-01k" {
			t.Fatalf("bot_mentioned = %+v", bm)
		}
	})

	t.Run("restart after crash", func(t *testing.T) {
		in, _ := host.plugin("gitlab")
		in.mu.RLock()
		kill := in.cur.kill
		in.mu.RUnlock()
		kill()
		deadline := time.Now().Add(20 * time.Second)
		for {
			if inf := host.Plugins()[0]; inf.Running && inf.Restarts >= 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("plugin not restarted: %+v", host.Plugins())
			}
			time.Sleep(20 * time.Millisecond)
		}
		exerciseForge(t) // the cached forge keeps working against the new process
	})
}

// TestAzureDevOpsPluginEndToEnd builds plugins/azuredevops and delivers a
// service hook through the host (basic auth checked against an integration
// secret read over the Host service).
func TestAzureDevOpsPluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a plugin binary")
	}
	dir := t.TempDir()
	buildPlugin(t, dir, "azuredevops", "github.com/openfactory-org/openfactory/plugins/azuredevops")
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_ado", TeamID: "team_1", Provider: "azure_devops", SecretRef: "ADO_PAT",
		Config: map[string]string{"org": "contoso", "hook_username": "hook", "hook_password_ref": "ADO_HOOK"}}}
	secrets := forgetest.NewSecrets(map[string]string{"ADO_PAT": "pat", "ADO_HOOK": "pw"})
	published := make(chan core.Event, 8)
	host := New(Options{Dir: dir, Store: store, Secrets: secrets,
		Publish: func(_ context.Context, ev core.Event) error { published <- ev; return nil }})
	defer host.Close()
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !host.HasForge("azure_devops") {
		t.Fatalf("plugins = %+v", host.Plugins())
	}
	f, err := forge.NewResolver(store, secrets, forge.Options{Plugins: host}).Forge(context.Background(), core.ForgeLookup{TeamID: "team_1", CodeForge: forge.AzureDevOps, Repo: "proj/repo"})
	if err != nil || f.Name() != forge.AzureDevOps {
		t.Fatalf("forge = %v %v", f, err)
	}
	if u, err := f.CloneURL(context.Background(), "proj", "repo"); err != nil || !strings.Contains(u, "dev.azure.com/contoso/proj/_git/repo") {
		t.Fatalf("clone url = %s %v", u, err)
	}

	mux := http.NewServeMux()
	host.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	payload := `{"id":"d1","eventType":"git.push","resourceContainers":{"account":{"baseUrl":"https://dev.azure.com/contoso/"}},
		"resource":{"pushedBy":{"id":"11111111-1111-1111-1111-111111111111","uniqueName":"alice@contoso.com"},
		"repository":{"id":"R1","name":"repo","project":{"name":"proj"}},"refUpdates":[{"name":"refs/heads/main","newObjectId":"abc"}]}}`
	send := func(path, pass string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(payload))
		req.SetBasicAuth("hook", pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if code := send("/plugins/azuredevops/webhook", "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("bad password status = %d", code)
	}
	if code := send("/plugins/azuredevops/webhook", "pw"); code != http.StatusAccepted {
		t.Fatalf("status = %d", code)
	}
	select {
	case ev := <-published:
		if ev.Type != "push" || ev.TeamID != "team_1" || ev.Attributes[eventattr.Branch] != "main" || ev.Source.Repo != "proj/repo" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
}
