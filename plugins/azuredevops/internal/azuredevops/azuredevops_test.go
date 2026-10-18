package azuredevops

import (
	"context"
	"encoding/base64"
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
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":pat"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.URL.Query().Get("api-version"), "7.1") {
			t.Errorf("missing api-version on %s", r.URL)
		}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			bodies[r.Method+" "+r.URL.Path] = m
		}
		base := "/contoso/proj/_apis/git/repositories/web"
		switch r.Method + " " + r.URL.Path {
		case "GET " + base:
			fmt.Fprint(w, `{"defaultBranch":"refs/heads/main"}`)
		case "POST " + base + "/pullrequests":
			fmt.Fprint(w, `{"pullRequestId":11,"status":"active","isDraft":true,"sourceRefName":"refs/heads/factory/x","repository":{"webUrl":"https://dev.azure.com/contoso/proj/_git/web"}}`)
		case "GET " + base + "/pullrequests/11":
			fmt.Fprint(w, `{"pullRequestId":11,"status":"completed"}`)
		case "POST " + base + "/pullRequests/11/threads", "POST " + base + "/commits/abc/statuses", "POST /contoso/proj/_apis/wit/workItems/5/comments":
			fmt.Fprint(w, `{}`)
		case "GET " + base + "/items":
			fmt.Fprint(w, `{"value":[{"path":"/def","isFolder":true,"gitObjectType":"tree","commitId":"c1"},{"path":"/def/factory.yaml","gitObjectType":"blob","objectId":"o1","commitId":"c1"}]}`)
		case "GET " + base + "/blobs/o1":
			fmt.Fprint(w, "name: y\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Config{Organization: "contoso", BaseURL: srv.URL, Token: "pat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if u, _ := c.CloneURL(ctx, "proj", "web"); !strings.HasSuffix(u, "/contoso/proj/_git/web") || !strings.Contains(u, "pat:pat@") {
		t.Fatalf("clone = %s", u)
	}
	pr, err := c.CreatePullRequest(ctx, core.PullRequestInput{Owner: "proj", Repo: "web", Head: "factory/x", Title: "T", Draft: true})
	if err != nil || pr.Number != 11 || pr.State != "draft" || pr.URL != "https://dev.azure.com/contoso/proj/_git/web/pullrequest/11" {
		t.Fatalf("pr = %+v %v", pr, err)
	}
	b := bodies["POST /contoso/proj/_apis/git/repositories/web/pullrequests"]
	if b["targetRefName"] != "refs/heads/main" || b["sourceRefName"] != "refs/heads/factory/x" || b["isDraft"] != true {
		t.Fatalf("pr body = %v", b)
	}
	if got, _ := c.GetPullRequest(ctx, "proj", "web", 11); got.State != "merged" {
		t.Fatalf("state = %s", got.State)
	}
	if err := c.Comment(ctx, "proj", "web", 11, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := c.CommentWorkItem(ctx, "proj", 5, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCommitStatus(ctx, "proj", "web", "abc", core.CommitStatus{Context: "openfactory/definition", State: "success"}); err != nil {
		t.Fatal(err)
	}
	st := bodies["POST /contoso/proj/_apis/git/repositories/web/commits/abc/statuses"]
	if st["state"] != "succeeded" || st["context"].(map[string]any)["genre"] != "openfactory" {
		t.Fatalf("status = %v", st)
	}
	files, sha, err := c.GetFiles(ctx, "proj", "web", "main", "def")
	if err != nil || sha != "c1" || files["factory.yaml"] != "name: y\n" {
		t.Fatalf("files = %v %s %v", files, sha, err)
	}
}

const factoryIdentity = "22222222-2222-4222-8222-222222222222"

func hook(t *testing.T, h *WebhookHandler, user, pass string, payload any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(string(b)))
	req.SetBasicAuth(user, pass)
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

func TestWebhook(t *testing.T) {
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_ado", TeamID: "team_1", Provider: "azure_devops",
		Config: map[string]string{"org": "contoso", "hook_username": "of", "hook_password_ref": "ADO_HOOK", "factory_identities": factoryIdentity + "=payments"}}}
	store.Factories = []core.Factory{{ID: "fac_1", TeamID: "team_1", Alias: "payments"}}
	bus := forgetest.NewBus()
	h := NewWebhookHandler(WebhookOptions{Store: store, Secrets: forgetest.NewSecrets(map[string]string{"ADO_HOOK": "pw"}), Bus: bus})
	containers := map[string]any{"account": map[string]any{"baseUrl": "https://dev.azure.com/contoso/"}}
	pr := map[string]any{
		"pullRequestId": 11, "status": "active", "title": "T", "targetRefName": "refs/heads/main", "sourceRefName": "refs/heads/f",
		"createdBy":  map[string]any{"id": "u1", "uniqueName": "alice@contoso.com"},
		"repository": map[string]any{"id": "11111111-1111-4111-8111-111111111111", "name": "web", "project": map[string]any{"name": "proj"}},
	}
	if rec := hook(t, h, "of", "bad", map[string]any{"eventType": "git.pullrequest.created", "resource": pr, "resourceContainers": containers}); rec.Code != 401 {
		t.Fatalf("status = %d", rec.Code)
	}
	hook(t, h, "of", "pw", map[string]any{"id": "n1", "eventType": "git.pullrequest.created", "resource": pr, "resourceContainers": containers})
	ev := bus.Find("pull_request_created")
	if ev == nil || ev.Attributes[eventattr.Repo] != "11111111-1111-4111-8111-111111111111" || ev.Attributes[eventattr.BaseBranch] != "main" || ev.Source.Repo != "proj/web" {
		t.Fatalf("pr event = %+v", ev)
	}
	comment := map[string]any{"comment": map[string]any{"content": "@<" + strings.ToUpper(factoryIdentity) + "> please review", "author": map[string]any{"id": "u1", "uniqueName": "alice@contoso.com"}}, "pullRequest": pr}
	hook(t, h, "of", "pw", map[string]any{"id": "n2", "eventType": "ms.vss-code.git-pullrequest-comment-event", "resource": comment, "resourceContainers": containers})
	m := bus.Find("pull_request_mentioned")
	if m == nil || m.FactoryID != "fac_1" || m.Attributes[eventattr.Mentioned].([]string)[0] != factoryIdentity {
		t.Fatalf("mention = %+v", m)
	}
	// merged via updated + merged deliveries: only one event
	done := map[string]any{}
	for k, v := range pr {
		done[k] = v
	}
	done["status"] = "completed"
	hook(t, h, "of", "pw", map[string]any{"id": "n3", "eventType": "git.pullrequest.updated", "resource": done, "resourceContainers": containers})
	hook(t, h, "of", "pw", map[string]any{"id": "n4", "eventType": "git.pullrequest.merged", "resource": done, "resourceContainers": containers})
	count := 0
	for _, e := range bus.Events() {
		if e.Type == "pull_request_merged" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("merged events = %d", count)
	}
	wi := map[string]any{
		"workItemId": 5,
		"fields": map[string]any{
			"System.AssignedTo": map[string]any{"oldValue": nil, "newValue": map[string]any{"uniqueName": "factory@contoso.com"}},
			"System.Tags":       map[string]any{"oldValue": "a", "newValue": "a; factory-ready"},
		},
		"revisedBy": map[string]any{"id": "u1", "uniqueName": "alice@contoso.com"},
		"revision": map[string]any{"fields": map[string]any{
			"System.TeamProject": "proj", "System.Title": "Bug", "System.WorkItemType": "Bug", "System.Tags": "a; factory-ready",
			"System.CreatedBy": "Alice <alice@contoso.com>",
		}},
	}
	hook(t, h, "of", "pw", map[string]any{"id": "n5", "eventType": "workitem.updated", "resource": wi, "resourceContainers": containers})
	as := bus.Find("work_item_assigned")
	lb := bus.Find("work_item_labeled")
	if as == nil || as.Attributes[eventattr.Assignees].([]string)[0] != "factory@contoso.com" || as.Source.Key() != "azure_devops:proj#5" {
		t.Fatalf("assigned = %+v", as)
	}
	if lb == nil || lb.Attributes[eventattr.AddedLabels].([]string)[0] != "factory-ready" || lb.Attributes[eventattr.Author] != "alice@contoso.com" || lb.Attributes[eventattr.WorkItemType] != "Bug" {
		t.Fatalf("labeled = %+v", lb)
	}
}

// TestWorkItemRouting is a regression test: Azure DevOps work item events
// carry a project rather than a repository and used to never reach a factory.
// They now resolve FactoryID from a factory identity assignment, the
// factory of the integration record, or the single factory covering the
// project. The old key config.factory_id is not read.
func TestWorkItemRouting(t *testing.T) {
	containers := map[string]any{"account": map[string]any{"baseUrl": "https://dev.azure.com/contoso/"}}
	created := func(project string) map[string]any {
		return map[string]any{"id": 9, "fields": map[string]any{
			"System.TeamProject": project, "System.Title": "Crash", "System.WorkItemType": "Bug",
			"System.CreatedBy": map[string]any{"id": "u1", "uniqueName": "alice@contoso.com"},
		}}
	}
	repos := map[string][]core.RepoRef{
		"fac_pay":  {{CodeForge: "AZURE_DEVOPS", Owner: "payments-proj", Name: "api"}},
		"fac_web":  {{CodeForge: "AZURE_DEVOPS", Owner: "contoso/web-proj", Name: "site"}},
		"fac_web2": {{CodeForge: "AZURE_DEVOPS", Owner: "web-proj", Name: "admin"}},
		"fac_gh":   {{CodeForge: "GITHUB", Owner: "payments-proj", Name: "api"}},
	}
	tests := []struct {
		name    string
		config  map[string]string
		factory string // the factory of the record ("" is the team record)
		teamToo bool   // a team record for the same org exists too
		payload map[string]any
		typ     string
		want    string
	}{
		{"project with one factory", nil, "", false, map[string]any{"id": "r1", "eventType": "workitem.created", "resource": created("payments-proj")}, "work_item_created", "fac_pay"},
		{"project with two factories", nil, "", false, map[string]any{"id": "r2", "eventType": "workitem.created", "resource": created("web-proj")}, "work_item_created", ""},
		{"unknown project", nil, "", false, map[string]any{"id": "r3", "eventType": "workitem.created", "resource": created("other")}, "work_item_created", ""},
		{"record of a factory", nil, "fac_web", false, map[string]any{"id": "r4", "eventType": "workitem.created", "resource": created("web-proj")}, "work_item_created", "fac_web"},
		{"team record wins at intake", nil, "fac_web", true, map[string]any{"id": "r6", "eventType": "workitem.created", "resource": created("web-proj")}, "work_item_created", ""},
		{"old config factory_id not read", map[string]string{"factory_id": "fac_web"}, "", false, map[string]any{"id": "r7", "eventType": "workitem.created", "resource": created("web-proj")}, "work_item_created", ""},
		{"assigned to factory identity", nil, "", false, map[string]any{"id": "r5", "eventType": "workitem.updated", "resource": map[string]any{
			"workItemId": 7,
			"fields":     map[string]any{"System.AssignedTo": map[string]any{"newValue": map[string]any{"id": strings.ToUpper(factoryIdentity), "uniqueName": "builder@contoso.com"}}},
			"revisedBy":  map[string]any{"id": "u1", "uniqueName": "alice@contoso.com"},
			"revision":   map[string]any{"fields": map[string]any{"System.TeamProject": "web-proj", "System.Title": "Bug"}},
		}}, "work_item_assigned", "fac_web2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := forgetest.NewStore()
			cfg := map[string]string{"org": "contoso", "hook_username": "of", "hook_password_ref": "ADO_HOOK", "factory_identities": factoryIdentity + "=builder"}
			for k, v := range tc.config {
				cfg[k] = v
			}
			store.Integrations = []core.Integration{{ID: "int_ado", TeamID: "team_1", Provider: "azure_devops", Config: cfg, FactoryID: tc.factory}}
			if tc.teamToo {
				store.Integrations = append(store.Integrations, core.Integration{ID: "int_team", TeamID: "team_1", Provider: "azure_devops", Config: cfg})
			}
			store.Factories = []core.Factory{
				{ID: "fac_pay", TeamID: "team_1", Alias: "payments"}, {ID: "fac_web", TeamID: "team_1", Alias: "web"},
				{ID: "fac_web2", TeamID: "team_1", Alias: "builder"}, {ID: "fac_gh", TeamID: "team_1", Alias: "gh"},
			}
			bus := forgetest.NewBus()
			h := NewWebhookHandler(WebhookOptions{Store: store, Secrets: forgetest.NewSecrets(map[string]string{"ADO_HOOK": "pw"}), Bus: bus,
				FactoryRepos: func(_ context.Context, id string) ([]core.RepoRef, error) { return repos[id], nil }})
			tc.payload["resourceContainers"] = containers
			hook(t, h, "of", "pw", tc.payload)
			ev := bus.Find(tc.typ)
			if ev == nil {
				t.Fatalf("no %s event in %v", tc.typ, bus.Events())
			}
			if ev.FactoryID != tc.want {
				t.Fatalf("FactoryID = %q, want %q", ev.FactoryID, tc.want)
			}
		})
	}
}
