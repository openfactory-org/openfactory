package jira

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// siteAPI is a Jira REST fake with projects (in pages of two) and statuses.
type siteAPI struct {
	mu       sync.Mutex
	calls    map[string]int
	fail     int
	projects [][2]string // key, name
}

func (a *siteAPI) count(k string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[k]
}

func (a *siteAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("bot@example.com:tok")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/rest/api/3/project/search":
		a.calls["projects"]++
		if a.fail != 0 {
			w.WriteHeader(a.fail)
			return
		}
		var start int
		fmt.Sscanf(r.URL.Query().Get("startAt"), "%d", &start)
		end := start + 2
		if end > len(a.projects) {
			end = len(a.projects)
		}
		var items []string
		for _, p := range a.projects[start:end] {
			items = append(items, fmt.Sprintf(`{"key":%q,"name":%q}`, p[0], p[1]))
		}
		fmt.Fprintf(w, `{"values":[%s],"isLast":%v}`, strings.Join(items, ","), end >= len(a.projects))
	case "/rest/api/3/status":
		a.calls["statuses"]++
		fmt.Fprint(w, `[{"id":"1","name":"To Do"},{"id":"3","name":"In Progress"},{"id":"10001","name":"Done"},{"id":"10002","name":"done"}]`)
	default:
		http.NotFound(w, r)
	}
}

type resolverFixture struct {
	api *siteAPI
	r   *NameResolver
	st  *forgetest.Store
	now time.Time
	mu  sync.Mutex
}

func (f *resolverFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func newResolverFixture(t *testing.T) *resolverFixture {
	t.Helper()
	api := &siteAPI{calls: map[string]int{}, projects: [][2]string{{"ENG", "Engineering"}, {"OPS", "Operations"}, {"WEB", "Website"}}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	st := forgetest.NewStore()
	st.Integrations = []core.Integration{{ID: "int_j", TeamID: "team_1", Provider: "jira", SecretRef: "JIRA_TOKEN",
		Config: map[string]string{"base_url": srv.URL, "email": "bot@example.com"}}}
	f := &resolverFixture{api: api, st: st, now: time.Unix(1_700_000_000, 0)}
	f.r = NewNameResolver(NameResolverOptions{
		Store: st, Secrets: forgetest.NewSecrets(map[string]string{"JIRA_TOKEN": "tok"}),
		Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	})
	return f
}

func (f *resolverFixture) resolve(t *testing.T, kind string, names ...string) map[string][]string {
	t.Helper()
	got, err := f.r.ResolveNames(context.Background(), "team_1", "jira", kind, names, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestResolveProjectNamesToKeysAcrossPages(t *testing.T) {
	f := newResolverFixture(t)
	got := f.resolve(t, "projects", "website", " Engineering ", "nope")
	want := map[string][]string{"website": {"WEB"}, " Engineering ": {"ENG"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if n := f.api.count("projects"); n != 2 {
		t.Fatalf("pages = %d, want 2", n)
	}
}

func TestResolveStatusNames(t *testing.T) {
	f := newResolverFixture(t)
	got := f.resolve(t, "statuses", "To Do", "done")
	if !reflect.DeepEqual(got, map[string][]string{"To Do": {"1"}, "done": {"10001", "10002"}}) {
		t.Fatalf("got %v (two statuses with one name give both IDs)", got)
	}
}

func TestJiraResolverCachesAndRefreshesOnAMiss(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t, "projects", "Website")
	f.resolve(t, "projects", "Website")
	if n := f.api.count("projects"); n != 2 { // one read of two pages
		t.Fatalf("calls = %d, want 2 (cached)", n)
	}
	f.api.mu.Lock()
	f.api.projects = append(f.api.projects, [2]string{"NEW", "Fresh"})
	f.api.mu.Unlock()
	if got := f.resolve(t, "projects", "Fresh"); len(got) != 0 {
		t.Fatalf("found %v within the refresh limit", got)
	}
	f.advance(time.Minute)
	if got := f.resolve(t, "projects", "Fresh"); !reflect.DeepEqual(got["Fresh"], []string{"NEW"}) {
		t.Fatalf("got %v", got)
	}
}

func TestJiraResolverOldListAndBackOff(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t, "projects", "Website")
	f.api.mu.Lock()
	f.api.fail = http.StatusServiceUnavailable
	before := f.api.calls["projects"]
	f.api.mu.Unlock()
	f.advance(10 * time.Minute)
	if got := f.resolve(t, "projects", "Website"); !reflect.DeepEqual(got["Website"], []string{"WEB"}) {
		t.Fatalf("got %v; want the cached list", got)
	}
	// During the back-off the API is not called again.
	f.resolve(t, "projects", "Website")
	if n := f.api.count("projects"); n != before+1 {
		t.Fatalf("calls = %d, want %d", n, before+1)
	}
	f.advance(2 * time.Hour)
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "jira", "projects", []string{"Website"}, nil); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the API error", err)
	}
}

func TestJiraResolverRefusedCredential(t *testing.T) {
	f := newResolverFixture(t)
	f.st.Integrations[0].Config["email"] = "other@example.com"
	_, err := f.r.ResolveNames(context.Background(), "team_1", "jira", "statuses", []string{"Done"}, nil)
	if err == nil || !strings.Contains(err.Error(), "refused the credential") {
		t.Fatalf("err = %v", err)
	}
}

func TestJiraResolverRules(t *testing.T) {
	f := newResolverFixture(t)
	ctx := context.Background()
	if _, err := f.r.ResolveNames(ctx, "team_1", "linear", "projects", []string{"x"}, nil); err == nil {
		t.Fatal("want an error for another provider")
	}
	if _, err := f.r.ResolveNames(ctx, "team_1", "jira", "users", []string{"x"}, nil); err == nil {
		t.Fatal("want an error for an unknown kind")
	}
	f.st.Integrations[0].Status = core.IntegrationDisabled
	if _, err := f.r.ResolveNames(ctx, "team_1", "jira", "projects", []string{"x"}, nil); err == nil || !strings.Contains(err.Error(), "no active Jira integration") {
		t.Fatalf("err = %v", err)
	}
	f.st.Integrations[0].Status = ""
	f.st.Integrations = append(f.st.Integrations, core.Integration{ID: "int_bad", TeamID: "team_1", Provider: "jira", SecretRef: "MISSING", Config: map[string]string{"base_url": "http://127.0.0.1:1"}})
	if got := f.resolve(t, "projects", "Website"); !reflect.DeepEqual(got["Website"], []string{"WEB"}) {
		t.Fatalf("a failing second integration: got %v", got)
	}
}
