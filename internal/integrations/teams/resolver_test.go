package teams

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

const (
	otherGraphTeam = "22222222-2222-4222-8222-222222222222"
	channelsJSON   = `{"value":[{"id":"19:inc@thread.tacv2","displayName":"Incidents"},{"id":"19:dev@thread.tacv2","displayName":"Dev"}]}`
	membersJSON    = `{"value":[
		{"userId":"aad-alice","displayName":"Alice Adams","email":"Alice@Example.com"},
		{"userId":"aad-bob","displayName":"Bob Brown","email":"bob@example.com"}]}`
)

type teamsResolverFixture struct {
	g   *graphAPI
	r   *NameResolver
	now time.Time
	mu  sync.Mutex
}

func (f *teamsResolverFixture) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func newTeamsResolverFixture(t *testing.T) (*teamsResolverFixture, *Handler) {
	t.Helper()
	g := newGraphAPI(t)
	g.lists = map[string]string{"channels": channelsJSON, "members": membersJSON}
	h, _ := g.handler(t, map[string]string{"tenant_id": tenant})
	f := &teamsResolverFixture{g: g, now: time.Unix(1_700_000_000, 0)}
	f.r = NewNameResolver(h, func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now })
	return f, h
}

func (f *teamsResolverFixture) resolve(t *testing.T, kind string, names []string, scope map[string][]string) map[string][]string {
	t.Helper()
	got, err := f.r.ResolveNames(context.Background(), "team_1", "teams", kind, names, scope)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (f *teamsResolverFixture) calls(kind string) int {
	f.g.mu.Lock()
	defer f.g.mu.Unlock()
	return f.g.listCall[kind]
}

func TestResolveChannelNames(t *testing.T) {
	f, _ := newTeamsResolverFixture(t)
	got := f.resolve(t, "channels", []string{"#incidents", " dev ", "nope"}, nil)
	want := map[string][]string{"#incidents": {"19:inc@thread.tacv2"}, " dev ": {"19:dev@thread.tacv2"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// The Graph token has the Graph scope and the tenant of the record.
	if len(f.g.tokens) != 1 || f.g.tokens[0] != tenant+" "+GraphScope {
		t.Fatalf("tokens = %v", f.g.tokens)
	}
	if !strings.HasPrefix(f.g.requests[0], "/v1.0/teams/"+graphTeam+"/channels") {
		t.Fatalf("requests = %v", f.g.requests)
	}
}

func TestResolveUserNamesAndEmails(t *testing.T) {
	f, _ := newTeamsResolverFixture(t)
	got := f.resolve(t, "users", []string{"alice adams", "BOB@example.com", "alice@example.com", "carol"}, nil)
	want := map[string][]string{"alice adams": {"aad-alice"}, "BOB@example.com": {"aad-bob"}, "alice@example.com": {"aad-alice"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTeamsResolverScopeAndDuplicates(t *testing.T) {
	f, h := newTeamsResolverFixture(t)
	// A second connected team with a channel of the same name.
	st := h.opts.Store.(interface {
		UpsertIntegration(context.Context, *core.Integration) error
	})
	if err := st.UpsertIntegration(context.Background(), &core.Integration{ID: "int_2", TeamID: "team_1", Provider: IntegrationProvider, SecretRef: "TEAMS_PW",
		Config: map[string]string{"team_id": otherGraphTeam, "tenant_id": tenant, "app_id": appID}}); err != nil {
		t.Fatal(err)
	}
	f.g.mu.Lock()
	f.g.lists[otherGraphTeam+"/channels"] = `{"value":[{"id":"19:dev2@thread.tacv2","displayName":"dev"}]}`
	f.g.mu.Unlock()
	both := f.resolve(t, "channels", []string{"dev"}, nil)
	if !reflect.DeepEqual(both["dev"], []string{"19:dev2@thread.tacv2", "19:dev@thread.tacv2"}) {
		t.Fatalf("both teams: %v (want one ID of each team)", both)
	}
	if n := f.calls("channels"); n != 2 {
		t.Fatalf("channel lists read = %d, want 2 (one for each team)", n)
	}
	one := f.resolve(t, "channels", []string{"dev"}, map[string][]string{"team_ids": {otherGraphTeam}})
	if !reflect.DeepEqual(one["dev"], []string{"19:dev2@thread.tacv2"}) {
		t.Fatalf("scope: %v", one)
	}
	one = f.resolve(t, "channels", []string{"dev"}, map[string][]string{"team_ids": {strings.ToUpper(graphTeam)}})
	if !reflect.DeepEqual(one["dev"], []string{"19:dev@thread.tacv2"}) {
		t.Fatalf("scope (upper case): %v", one)
	}
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "teams", "channels", []string{"dev"}, map[string][]string{"team_ids": {"33333333-3333-4333-8333-333333333333"}}); err == nil {
		t.Fatal("want an error for a team that is not connected")
	}
}

func TestTeamsResolverCachesAndRefreshesOnAMiss(t *testing.T) {
	f, _ := newTeamsResolverFixture(t)
	f.resolve(t, "channels", []string{"dev"}, nil)
	f.resolve(t, "channels", []string{"dev"}, nil)
	if n := f.calls("channels"); n != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", n)
	}
	f.g.mu.Lock()
	f.g.lists["channels"] = `{"value":[{"id":"19:inc@thread.tacv2","displayName":"Incidents"},{"id":"19:dev@thread.tacv2","displayName":"Dev"},{"id":"19:new@thread.tacv2","displayName":"New"}]}`
	f.g.mu.Unlock()
	if got := f.resolve(t, "channels", []string{"new"}, nil); len(got) != 0 {
		t.Fatalf("found %v within the refresh limit", got)
	}
	f.advance(time.Minute)
	if got := f.resolve(t, "channels", []string{"new"}, nil); !reflect.DeepEqual(got["new"], []string{"19:new@thread.tacv2"}) {
		t.Fatalf("got %v", got)
	}
	f.advance(10 * time.Minute)
	f.resolve(t, "channels", []string{"dev"}, nil)
	if n := f.calls("channels"); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
}

func TestTeamsResolverOldListAndBackOff(t *testing.T) {
	f, _ := newTeamsResolverFixture(t)
	f.resolve(t, "users", []string{"alice adams"}, nil)
	f.g.mu.Lock()
	f.g.status = http.StatusServiceUnavailable
	f.g.mu.Unlock()
	f.advance(10 * time.Minute)
	if got := f.resolve(t, "users", []string{"alice adams"}, nil); !reflect.DeepEqual(got["alice adams"], []string{"aad-alice"}) {
		t.Fatalf("got %v; want the cached list", got)
	}
	n := len(f.g.requests)
	f.resolve(t, "users", []string{"alice adams"}, nil)
	if len(f.g.requests) != n {
		t.Fatalf("Graph was called during the back-off")
	}
	f.advance(2 * time.Hour)
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "teams", "users", []string{"alice adams"}, nil); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the API error", err)
	}
}

func TestTeamsResolverPermissionHintAndRules(t *testing.T) {
	f, _ := newTeamsResolverFixture(t)
	f.g.status = http.StatusForbidden
	_, err := f.r.ResolveNames(context.Background(), "team_1", "teams", "channels", []string{"dev"}, nil)
	if err == nil || !strings.Contains(err.Error(), "ChannelSettings.Read.Group") {
		t.Fatalf("err = %v, want a hint about the permission", err)
	}
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "slack", "channels", []string{"dev"}, nil); err == nil {
		t.Fatal("want an error for another provider")
	}
	if _, err := f.r.ResolveNames(context.Background(), "team_1", "teams", "teams", []string{"dev"}, nil); err == nil {
		t.Fatal("want an error for an unknown kind")
	}
	if _, err := f.r.ResolveNames(context.Background(), "team_other", "teams", "channels", []string{"dev"}, nil); err == nil || !strings.Contains(err.Error(), "no active Teams integration") {
		t.Fatalf("err = %v", err)
	}
}
