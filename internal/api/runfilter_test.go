package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestRunSourceAndFilters(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	mk := func(id, trig, model string, creator *core.Principal, i int) {
		r := &core.Run{ID: id, TeamID: e.team.ID, FactoryID: e.runFactory(), State: core.RunSucceeded, TriggeredBy: trig, Creator: creator,
			Config: core.AgentConfig{Harness: core.HarnessConfig{Model: model}}, CreatedAt: base.Add(time.Duration(i) * time.Second)}
		if err := e.store.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	me := &core.Principal{Type: core.PrincipalUser, ID: e.viewer.User.ID}
	mk("run_a", "automation:aut_1", "m1", nil, 1)
	mk("run_b", "api", "m2", me, 2)
	mk("run_c", "run:run_b", "m1", me, 3)
	mk("run_d", "benchmark:bench_1", "m1", nil, 4)
	mk("run_e", "mcp", "M1", me, 5)

	ids := func(path string) []string {
		r := e.must(e.do("GET", "/runs"+path, e.viewer.Key, nil), 200)
		var out []string
		for _, it := range list(r.Body, "runs") {
			out = append(out, it.(map[string]any)["run_id"].(string))
		}
		return out
	}
	one := e.must(e.do("GET", "/runs/run_a", e.viewer.Key, nil), 200)
	if one.Body["source"] != "AUTOMATION" || one.Body["source_ref"] != "aut_1" || one.Body["triggered_by"] != nil {
		t.Fatalf("view = %s", one.Raw)
	}
	if got := ids("?source=automation,orchestration"); !slices.Equal(got, []string{"run_c", "run_a"}) {
		t.Fatalf("source = %v", got)
	}
	if got := ids("?model=m1"); !slices.Equal(got, []string{"run_e", "run_d", "run_c", "run_a"}) {
		t.Fatalf("model = %v", got)
	}
	if got := ids("?creator=me"); !slices.Equal(got, []string{"run_e", "run_c", "run_b"}) {
		t.Fatalf("creator = %v", got)
	}
	if got := ids("?creator=me&model=m1&sort_order=asc"); !slices.Equal(got, []string{"run_c", "run_e"}) {
		t.Fatalf("creator+model asc = %v", got)
	}
	// A stored source wins over TriggeredBy, in the view and in the filter.
	stored := &core.Run{ID: "run_s", TeamID: e.team.ID, FactoryID: e.runFactory(), State: core.RunSucceeded, TriggeredBy: "automation:aut_2",
		Source: core.SourceSchedule, SourceRef: "aut_2", CreatedAt: base.Add(10 * time.Second)}
	if err := e.store.CreateRun(ctx, stored); err != nil {
		t.Fatal(err)
	}
	sv := e.must(e.do("GET", "/runs/run_s", e.viewer.Key, nil), 200)
	if sv.Body["source"] != "SCHEDULE" || sv.Body["source_ref"] != "aut_2" {
		t.Fatalf("stored view = %s", sv.Raw)
	}
	if got := ids("?source=schedule"); !slices.Equal(got, []string{"run_s"}) {
		t.Fatalf("source=schedule = %v", got)
	}
	if got := ids("?source=automation"); !slices.Equal(got, []string{"run_a"}) {
		t.Fatalf("source=automation = %v", got)
	}
	e.must(e.do("GET", "/runs?source=nonsense", e.viewer.Key, nil), 400)
	e.must(e.do("GET", "/runs?sort_by=title", e.viewer.Key, nil), 400)
	e.must(e.do("GET", "/runs?sort_order=sideways", e.viewer.Key, nil), 400)

	// A filtered list pages with the cursor of the last item.
	p1 := e.must(e.do("GET", "/runs?creator=me&limit=2", e.viewer.Key, nil), 200)
	more, next := pageInfo(p1.Body)
	if len(list(p1.Body, "runs")) != 2 || !more || next == "" {
		t.Fatalf("page 1 = %s", p1.Raw)
	}
	p2 := e.must(e.do("GET", "/runs?creator=me&limit=2&cursor="+next, e.viewer.Key, nil), 200)
	if l := list(p2.Body, "runs"); len(l) != 1 || l[0].(map[string]any)["run_id"] != "run_b" {
		t.Fatalf("page 2 = %s", p2.Raw)
	}
	if more, _ := pageInfo(p2.Body); more {
		t.Fatalf("page 2 more = %s", p2.Raw)
	}
	// Ascending order without extra filters goes to the store.
	if got := ids("?sort_order=asc&limit=2"); !slices.Equal(got, []string{"run_a", "run_b"}) {
		t.Fatalf("asc = %v", got)
	}
}

func TestRequestSource(t *testing.T) {
	cases := []struct {
		ua   string
		p    *Principal
		want string
	}{
		{"openfactory/1.0", &Principal{User: &core.User{ID: "u"}, APIKey: &core.APIKey{}}, "cli"},
		{"curl/8", &Principal{User: &core.User{ID: "u"}, APIKey: &core.APIKey{}}, "api"},
		{"Mozilla/5.0", &Principal{User: &core.User{ID: "u"}}, "dashboard"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("User-Agent", c.ua)
		if got := requestSource(withRequestSource(r.Context(), r, c.p)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.ua, got, c.want)
		}
	}
	if got := requestSource(context.Background()); got != "api" {
		t.Errorf("default = %q", got)
	}
}

type authErr struct{ provider string }

func (a authErr) Error() string        { return "no link for " + a.provider }
func (a authErr) Unwrap() error        { return core.ErrExternalAuthRequired }
func (a authErr) AuthProvider() string { return a.provider }

func TestExternalAuthError(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/v1/runs", nil)
	writeError(w, r, nil, fmt.Errorf("start run: %w", authErr{provider: "GitHub"}))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", w.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["code"] != CodeExternalAuthRequired || body["provider"] != "github" || body["auth_url"] != "/api/v1/me/forge-tokens/github" || body["reason"] != "no_linked_account" {
		t.Fatalf("body = %s", w.Body.String())
	}
	if _, ok := errorSpecs["budget_exceeded"]; ok {
		t.Fatal("billing codes must be gone")
	}
}
