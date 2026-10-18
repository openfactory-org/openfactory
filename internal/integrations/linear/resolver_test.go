package linear

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// workspaceAPI is a Linear GraphQL fake with teams, projects, states and issues.
type workspaceAPI struct {
	mu    sync.Mutex
	calls map[string]int
	auth  string
	fail  int  // HTTP status for all calls, when not zero
	pages bool // return the teams in pages of one
	teams [][2]string
}

func (a *workspaceAPI) count(k string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[k]
}

func (a *workspaceAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var req struct {
		Query string         `json:"query"`
		Vars  map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(b, &req)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.auth != "" && r.Header.Get("Authorization") != a.auth {
		fmt.Fprint(w, `{"errors":[{"message":"Authentication required"}]}`)
		return
	}
	if a.fail != 0 {
		w.WriteHeader(a.fail)
		return
	}
	q := req.Query
	switch {
	case strings.Contains(q, "{ teams(first"):
		a.calls["teams"]++
		list := a.teams
		next := ""
		if a.pages {
			i := 0
			if c, ok := req.Vars["after"].(string); ok {
				fmt.Sscanf(c, "p%d", &i)
			}
			list = a.teams[i : i+1]
			if i+1 < len(a.teams) {
				next = fmt.Sprintf("p%d", i+1)
			}
		}
		var items []string
		for _, t := range list {
			items = append(items, fmt.Sprintf(`{"id":%q,"name":%q}`, t[0], t[1]))
		}
		fmt.Fprintf(w, `{"data":{"teams":{"nodes":[%s],"pageInfo":{"hasNextPage":%v,"endCursor":%q}}}}`, strings.Join(items, ","), next != "", next)
	case strings.Contains(q, "{ projects(first"):
		a.calls["projects"]++
		fmt.Fprint(w, `{"data":{"projects":{"nodes":[
			{"id":"p1","name":"Launch","teams":{"nodes":[{"id":"t1"}]}},
			{"id":"p2","name":"Launch","teams":{"nodes":[{"id":"t2"},{"id":"t3"}]}},
			{"id":"p3","name":"Cleanup","teams":{"nodes":[{"id":"t1"},{"id":"t2"}]}}],
			"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	case strings.Contains(q, "{ workflowStates(first"):
		a.calls["states"]++
		fmt.Fprint(w, `{"data":{"workflowStates":{"nodes":[
			{"id":"s1","name":"Todo","team":{"id":"t1"}},
			{"id":"s2","name":"Todo","team":{"id":"t2"}},
			{"id":"s3","name":"In Review","team":{"id":"t1"}}],
			"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	case strings.Contains(q, "{ issues(first"):
		a.calls["issues"]++
		switch req.Vars["n"] {
		case float64(42):
			fmt.Fprint(w, `{"data":{"issues":{"nodes":[{"id":"i1","team":{"id":"t1"}},{"id":"i2","team":{"id":"t2"}}]}}}`)
		case float64(7):
			fmt.Fprint(w, `{"data":{"issues":{"nodes":[{"id":"i3","team":{"id":"t1"}}]}}}`)
		default:
			fmt.Fprint(w, `{"data":{"issues":{"nodes":[]}}}`)
		}
	default:
		fmt.Fprint(w, `{"errors":[{"message":"unknown query"}]}`)
	}
}

type resolverFixture struct {
	api *workspaceAPI
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
	api := &workspaceAPI{calls: map[string]int{}, auth: "lin_api_test",
		teams: [][2]string{{"t1", "Engineering"}, {"t2", "Design"}, {"t3", "Ops"}}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	st := forgetest.NewStore()
	st.Integrations = []core.Integration{{ID: "int_l", TeamID: "team_1", Provider: "linear", SecretRef: "LINEAR_KEY"}}
	f := &resolverFixture{api: api, st: st, now: time.Unix(1_700_000_000, 0)}
	f.r = NewNameResolver(NameResolverOptions{
		Store: st, Secrets: forgetest.NewSecrets(map[string]string{"LINEAR_KEY": "lin_api_test"}), GraphQLURL: srv.URL,
		Now: func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now },
	})
	return f
}

func (f *resolverFixture) resolve(t *testing.T, kind string, names []string, scope map[string][]string) map[string][]string {
	t.Helper()
	got, err := f.r.ResolveNames(context.Background(), "team_1", "linear", kind, names, scope)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestResolveTeamNames(t *testing.T) {
	f := newResolverFixture(t)
	got := f.resolve(t, "teams", []string{"engineering", " Design ", "nope"}, nil)
	want := map[string][]string{"engineering": {"t1"}, " Design ": {"t2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestResolveProjectsAndStatesUseTheTeamScope(t *testing.T) {
	f := newResolverFixture(t)
	// Without a scope, "Launch" is ambiguous.
	if got := f.resolve(t, "projects", []string{"Launch"}, nil); !reflect.DeepEqual(got["Launch"], []string{"p1", "p2"}) {
		t.Fatalf("no scope: %v", got)
	}
	// With the team, it is one project.
	if got := f.resolve(t, "projects", []string{"Launch", "Cleanup"}, map[string][]string{"team_ids": {"t1"}}); !reflect.DeepEqual(got, map[string][]string{"Launch": {"p1"}, "Cleanup": {"p3"}}) {
		t.Fatalf("scope t1: %v", got)
	}
	if got := f.resolve(t, "projects", []string{"Launch"}, map[string][]string{"team_ids": {"t3"}}); !reflect.DeepEqual(got, map[string][]string{"Launch": {"p2"}}) {
		t.Fatalf("scope t3: %v", got)
	}
	if got := f.resolve(t, "states", []string{"todo", "In Review"}, map[string][]string{"team_ids": {"t2"}}); !reflect.DeepEqual(got, map[string][]string{"todo": {"s2"}}) {
		t.Fatalf("states t2: %v", got)
	}
	if got := f.resolve(t, "states", []string{"todo"}, map[string][]string{"team_ids": {"t1", "t2"}}); !reflect.DeepEqual(got["todo"], []string{"s1", "s2"}) {
		t.Fatalf("states t1+t2: %v", got)
	}
	// Each scope has its own list, and a list is read once.
	f.resolve(t, "states", []string{"todo"}, map[string][]string{"team_ids": {"t2"}})
	if n := f.api.count("states"); n != 2 {
		t.Fatalf("workflowStates calls = %d, want 2 (one for each scope)", n)
	}
}

func TestResolveIssueNumbers(t *testing.T) {
	f := newResolverFixture(t)
	got := f.resolve(t, "issues", []string{"42", "7", "999", "ENG-1", "#7"}, map[string][]string{"team_ids": {"t1"}})
	want := map[string][]string{"42": {"i1"}, "7": {"i3"}, "#7": {"i3"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Without a team, a number that exists in two teams gives both issues.
	if got := f.resolve(t, "issues", []string{"42"}, nil); !reflect.DeepEqual(got["42"], []string{"i1", "i2"}) {
		t.Fatalf("no scope: %v", got)
	}
}

func TestResolverCachesAndRefreshesOnAMiss(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t, "teams", []string{"Design"}, nil)
	f.resolve(t, "teams", []string{"Design"}, nil)
	if n := f.api.count("teams"); n != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", n)
	}
	f.api.mu.Lock()
	f.api.teams = append(f.api.teams, [2]string{"t4", "Data"})
	f.api.mu.Unlock()
	if got := f.resolve(t, "teams", []string{"Data"}, nil); len(got) != 0 {
		t.Fatalf("found %v within the refresh limit", got)
	}
	if n := f.api.count("teams"); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
	f.advance(time.Minute)
	if got := f.resolve(t, "teams", []string{"Data"}, nil); !reflect.DeepEqual(got["Data"], []string{"t4"}) {
		t.Fatalf("got %v", got)
	}
	f.advance(10 * time.Minute)
	f.resolve(t, "teams", []string{"Design"}, nil)
	if n := f.api.count("teams"); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
}

func TestResolverUsesTheOldListWhenTheAPIFails(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t, "teams", []string{"Design"}, nil)
	f.api.mu.Lock()
	f.api.fail = http.StatusBadGateway
	f.api.mu.Unlock()
	f.advance(10 * time.Minute)
	got, err := f.r.ResolveNames(context.Background(), "team_1", "linear", "teams", []string{"Design"}, nil)
	if err != nil || !reflect.DeepEqual(got["Design"], []string{"t2"}) {
		t.Fatalf("got %v, %v; want the cached list", got, err)
	}
	f.advance(2 * time.Hour)
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "linear", "teams", []string{"Design"}, nil); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want the API error", err)
	}
}

func TestResolverErrorsAreReportedAndBackedOff(t *testing.T) {
	f := newResolverFixture(t)
	f.api.auth = "other-key" // the fake refuses the credential of the record
	ctx := context.Background()
	_, err := f.r.ResolveNames(ctx, "team_1", "linear", "teams", []string{"Design"}, nil)
	if err == nil || !strings.Contains(err.Error(), "refused the credential") {
		t.Fatalf("err = %v, want a hint about the credential", err)
	}
	if _, err := f.r.ResolveNames(ctx, "team_1", "linear", "teams", []string{"Design"}, nil); err == nil {
		t.Fatal("want the error again")
	}
	f.api.mu.Lock()
	n := f.api.calls["teams"]
	f.api.auth = ""
	f.api.mu.Unlock()
	if n != 0 {
		t.Fatalf("teams listed %d times, want 0", n)
	}
	f.advance(time.Minute)
	if got, err := f.r.ResolveNames(ctx, "team_1", "linear", "teams", []string{"Design"}, nil); err != nil || len(got["Design"]) != 1 {
		t.Fatalf("got %v, %v after the back-off", got, err)
	}
}

func TestResolverBackOffCountsCalls(t *testing.T) {
	f := newResolverFixture(t)
	f.api.fail = http.StatusInternalServerError
	for i := 0; i < 3; i++ {
		if _, err := f.r.ResolveNames(context.Background(), "team_1", "linear", "teams", []string{"Design"}, nil); err == nil {
			t.Fatal("want an error")
		}
	}
	// The API works again, but the resolver waits for the back-off.
	f.api.mu.Lock()
	f.api.fail = 0
	f.api.mu.Unlock()
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "linear", "teams", []string{"Design"}, nil); err == nil {
		t.Fatal("want the error during the back-off, without a call")
	}
	if n := f.api.count("teams"); n != 0 {
		t.Fatalf("teams calls = %d during the back-off", n)
	}
	f.advance(time.Minute)
	if got := f.resolve(t, "teams", []string{"Design"}, nil); len(got["Design"]) != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestResolverFollowsPages(t *testing.T) {
	f := newResolverFixture(t)
	f.api.pages = true
	got := f.resolve(t, "teams", []string{"Ops"}, nil)
	if !reflect.DeepEqual(got["Ops"], []string{"t3"}) {
		t.Fatalf("got %v", got)
	}
	if n := f.api.count("teams"); n != 3 {
		t.Fatalf("pages = %d, want 3", n)
	}
}

func TestResolverNeedsAnActiveIntegration(t *testing.T) {
	f := newResolverFixture(t)
	ctx := context.Background()
	f.st.Integrations[0].Status = core.IntegrationDisabled
	if _, err := f.r.ResolveNames(ctx, "team_1", "linear", "teams", []string{"Design"}, nil); err == nil || !strings.Contains(err.Error(), "no active Linear integration") {
		t.Fatalf("err = %v", err)
	}
	f.st.Integrations[0].Status = ""
	if _, err := f.r.ResolveNames(ctx, "team_1", "slack", "teams", []string{"x"}, nil); err == nil {
		t.Fatal("want an error for another provider")
	}
	if _, err := f.r.ResolveNames(ctx, "team_1", "linear", "labels", []string{"x"}, nil); err == nil {
		t.Fatal("want an error for an unknown kind")
	}
}

func TestResolverSecondIntegrationCoversForAFailingOne(t *testing.T) {
	f := newResolverFixture(t)
	f.st.Integrations = append(f.st.Integrations, core.Integration{ID: "int_bad", TeamID: "team_1", Provider: "linear", SecretRef: "MISSING"})
	got := f.resolve(t, "teams", []string{"Design"}, nil)
	if !reflect.DeepEqual(got["Design"], []string{"t2"}) {
		t.Fatalf("got %v", got)
	}
}
