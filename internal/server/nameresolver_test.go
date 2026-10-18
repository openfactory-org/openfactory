package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
	"github.com/openfactory-org/openfactory/internal/integrations/teams"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

type stubResolver struct{ got []string }

func (s *stubResolver) ResolveNames(_ context.Context, teamID, provider, kind string, names []string, _ map[string][]string) (map[string][]string, error) {
	s.got = append(s.got, teamID+"/"+provider+"/"+kind)
	return map[string][]string{names[0]: {"X1"}}, nil
}

func TestNameResolversRouteByProvider(t *testing.T) {
	s := &stubResolver{}
	m := nameResolvers{"slack": s}
	got, err := m.ResolveNames(context.Background(), "team1", "Slack", "channels", []string{"dev"}, nil)
	if err != nil || !reflect.DeepEqual(got["dev"], []string{"X1"}) || len(s.got) != 1 || s.got[0] != "team1/Slack/channels" {
		t.Fatalf("got %v, %v, calls %v", got, err, s.got)
	}
	if _, err := m.ResolveNames(context.Background(), "team1", "linear", "teams", []string{"Eng"}, nil); err == nil || !strings.Contains(err.Error(), "cannot list linear objects") {
		t.Fatalf("err = %v", err)
	}
}

// Each provider with name filters has a resolver. A team without an
// integration of the provider gets the error of that provider's resolver,
// not "this server cannot list ... objects".
func TestNewNameResolversCoverAllProviders(t *testing.T) {
	st := testdb.New(t)
	m := newNameResolvers(st, nil, teams.NewHandler(teams.Options{Store: st}), nil)
	for provider, kind := range map[string]string{"slack": "channels", "Linear": "teams", "jira": "projects", "teams": "channels"} {
		_, err := m.ResolveNames(context.Background(), "team1", provider, kind, []string{"x"}, nil)
		if err == nil || strings.Contains(err.Error(), "this server cannot list") || !strings.Contains(err.Error(), "no active") {
			t.Errorf("%s: err = %v, want the error of the resolver of the provider", provider, err)
		}
	}
	if _, err := m.ResolveNames(context.Background(), "team1", "gitlab", "repos", []string{"x"}, nil); err == nil || !strings.Contains(err.Error(), "cannot list gitlab objects") {
		t.Fatalf("err = %v", err)
	}
}

// A Jira and a Teams name filter resolve through the real resolvers. The
// Teams trigger has the provider "microsoft_teams", as the definition stores it.
func TestNameResolversResolveJiraAndTeamsNames(t *testing.T) {
	const graphTeam = "11111111-1111-4111-8111-111111111111"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/rest/api/3/project/search":
			fmt.Fprint(w, `{"values":[{"key":"WEB","name":"Website"}],"isLast":true}`)
		case r.URL.Path == "/rest/api/3/status":
			fmt.Fprint(w, `[{"id":"10001","name":"Done"}]`)
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			fmt.Fprint(w, `{"access_token":"graph-token","expires_in":3600}`)
		case r.URL.Path == "/v1.0/teams/"+graphTeam+"/channels":
			fmt.Fprint(w, `{"value":[{"id":"19:inc@thread.tacv2","displayName":"Incidents"}]}`)
		case r.URL.Path == "/v1.0/teams/"+graphTeam+"/members":
			fmt.Fprint(w, `{"value":[{"userId":"aad-alice","displayName":"Alice Adams","email":"alice@example.com"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	st := forgetest.NewStore()
	st.Integrations = []core.Integration{
		{ID: "int_j", TeamID: "team1", Provider: "jira", SecretRef: "S", Config: map[string]string{"base_url": srv.URL, "auth": "oauth"}},
		{ID: "int_t", TeamID: "team1", Provider: "microsoft_teams", SecretRef: "S", Config: map[string]string{"team_id": graphTeam, "tenant_id": "tenant-1", "app_id": "app-1"}},
	}
	secrets := forgetest.NewSecrets(map[string]string{"S": "secret"})
	h := teams.NewHandler(teams.Options{Store: st, Secrets: secrets, LoginURL: srv.URL, GraphURL: srv.URL + "/v1.0"})
	m := newNameResolvers(st, secrets, h, nil)
	ctx := context.Background()

	got, err := m.ResolveNames(ctx, "team1", "jira", "projects", []string{"website"}, nil)
	if err != nil || !reflect.DeepEqual(got["website"], []string{"WEB"}) {
		t.Fatalf("jira projects: %v, %v", got, err)
	}
	got, err = m.ResolveNames(ctx, "team1", "jira", "statuses", []string{"Done"}, nil)
	if err != nil || !reflect.DeepEqual(got["Done"], []string{"10001"}) {
		t.Fatalf("jira statuses: %v, %v", got, err)
	}
	scope := map[string][]string{"team_ids": {graphTeam}}
	got, err = m.ResolveNames(ctx, "team1", "microsoft_teams", "channels", []string{"#incidents"}, scope)
	if err != nil || !reflect.DeepEqual(got["#incidents"], []string{"19:inc@thread.tacv2"}) {
		t.Fatalf("teams channels: %v, %v", got, err)
	}
	got, err = m.ResolveNames(ctx, "team1", "microsoft_teams", "users", []string{"Alice Adams", "ALICE@example.com"}, scope)
	if err != nil || !reflect.DeepEqual(got, map[string][]string{"Alice Adams": {"aad-alice"}, "ALICE@example.com": {"aad-alice"}}) {
		t.Fatalf("teams users: %v, %v", got, err)
	}
}
