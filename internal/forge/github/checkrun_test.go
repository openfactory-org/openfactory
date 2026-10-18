package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// checkRunAPI is a small GitHub API fake for check runs.
type checkRunAPI struct {
	mu       sync.Mutex
	calls    []string
	bodies   []map[string]any
	existing string // JSON of the check_runs list for the GET call
	patch404 bool
}

func (a *checkRunAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r.Method+" "+r.URL.RequestURI())
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &body)
	}
	a.bodies = append(a.bodies, body)
	switch {
	case r.URL.Path == "/app/installations/42/access_tokens":
		fmt.Fprintf(w, `{"token":"ghs_install","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/check-runs":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":501}`)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/acme/api/check-runs/"):
		if a.patch404 {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":501}`)
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/commits/abc/check-runs":
		if a.existing == "" {
			fmt.Fprint(w, `{"check_runs":[]}`)
			return
		}
		fmt.Fprint(w, a.existing)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/statuses/abc":
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":1}`)
	default:
		http.NotFound(w, r)
	}
}

func (a *checkRunAPI) count(prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func appClient(t *testing.T, api *checkRunAPI) *Client {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	_, pemKey := testKey(t)
	c, err := New(Config{BaseURL: srv.URL, AppID: "123", PrivateKey: pemKey, InstallationID: 42})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var _ interface {
	PublishCheckRun(context.Context, string, string, core.CheckRun) error
} = (*Client)(nil)

func TestPublishCheckRunUpdatesTheRunInProgress(t *testing.T) {
	api := &checkRunAPI{}
	c := appClient(t, api)
	ctx := context.Background()
	name := CheckName("factory")
	if err := c.PublishCheckRun(ctx, "acme", "api", core.CheckRun{Name: name, HeadSHA: "abc", Status: "in_progress", Title: "Validating"}); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishCheckRun(ctx, "acme", "api", core.CheckRun{Name: name, HeadSHA: "abc", Status: "completed", Conclusion: "failure",
		Title: "1 error(s)", Summary: "Errors:\n- secret missing", Text: "details", DetailsURL: "https://x/f"}); err != nil {
		t.Fatal(err)
	}
	if n := api.count("POST /repos/acme/api/check-runs"); n != 1 {
		t.Fatalf("created %d check runs, want 1", n)
	}
	if n := api.count("PATCH /repos/acme/api/check-runs/501"); n != 1 {
		t.Fatalf("calls = %v, want one update of run 501", api.calls)
	}
	var patch map[string]any
	for i, c := range api.calls {
		if strings.HasPrefix(c, "PATCH") {
			patch = api.bodies[i]
		}
	}
	out, _ := patch["output"].(map[string]any)
	if patch["status"] != "completed" || patch["conclusion"] != "failure" || out["summary"] != "Errors:\n- secret missing" || out["text"] != "details" || patch["details_url"] != "https://x/f" {
		t.Fatalf("update body = %v", patch)
	}
	if api.count("GET ") != 0 {
		t.Fatalf("looked up a run that the client kept: %v", api.calls)
	}
}

func TestPublishCheckRunFindsTheRunAfterARestart(t *testing.T) {
	api := &checkRunAPI{existing: `{"check_runs":[{"id":77,"app":{"id":999}},{"id":501,"app":{"id":123}}]}`}
	c := appClient(t, api)
	err := c.PublishCheckRun(context.Background(), "acme", "api", core.CheckRun{Name: "n", HeadSHA: "abc", Status: "completed", Conclusion: "success", Title: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if api.count("PATCH /repos/acme/api/check-runs/501") != 1 || api.count("POST /repos/acme/api/check-runs") != 0 {
		t.Fatalf("calls = %v", api.calls)
	}
	found := false
	for _, c := range api.calls {
		if strings.Contains(c, "check_name=n") && strings.Contains(c, "filter=latest") {
			found = true
		}
	}
	if !found {
		t.Fatalf("lookup query missing: %v", api.calls)
	}
}

func TestPublishCheckRunCreatesWhenNoneExists(t *testing.T) {
	api := &checkRunAPI{}
	c := appClient(t, api)
	err := c.PublishCheckRun(context.Background(), "acme", "api", core.CheckRun{Name: "n", HeadSHA: "abc", Status: "completed", Conclusion: "success", Title: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if api.count("POST /repos/acme/api/check-runs") != 1 || api.count("PATCH") != 0 {
		t.Fatalf("calls = %v", api.calls)
	}
}

func TestPublishCheckRunCreatesWhenTheRunIsGone(t *testing.T) {
	api := &checkRunAPI{patch404: true, existing: `{"check_runs":[{"id":501,"app":{"id":123}}]}`}
	c := appClient(t, api)
	err := c.PublishCheckRun(context.Background(), "acme", "api", core.CheckRun{Name: "n", HeadSHA: "abc", Status: "completed", Conclusion: "success", Title: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if api.count("POST /repos/acme/api/check-runs") != 1 {
		t.Fatalf("calls = %v", api.calls)
	}
}

func TestPublishCheckRunErrorIsReturned(t *testing.T) {
	api := &checkRunAPI{}
	c := appClient(t, api)
	err := c.PublishCheckRun(context.Background(), "acme", "other", core.CheckRun{Name: "n", HeadSHA: "abc", Status: "in_progress"})
	if err == nil {
		t.Fatal("want an error from the API")
	}
}

func TestPublishCheckRunWithATokenSetsACommitStatus(t *testing.T) {
	api := &checkRunAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "ghp_pat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.PublishCheckRun(ctx, "acme", "api", core.CheckRun{Name: "n", HeadSHA: "abc", Status: "in_progress", Title: "Validating"}); err != nil {
		t.Fatal(err)
	}
	if err := c.PublishCheckRun(ctx, "acme", "api", core.CheckRun{Name: "n", HeadSHA: "abc", Status: "completed", Conclusion: "failure", Title: "bad"}); err != nil {
		t.Fatal(err)
	}
	if api.count("POST /repos/acme/api/statuses/abc") != 2 || api.count("POST /repos/acme/api/check-runs") != 0 {
		t.Fatalf("calls = %v", api.calls)
	}
	if api.bodies[1]["state"] != "failure" || api.bodies[0]["state"] != "pending" || api.bodies[1]["context"] != "n" {
		t.Fatalf("bodies = %v", api.bodies)
	}
}
