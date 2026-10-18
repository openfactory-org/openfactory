package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// seenRequest is one request of a prover to a fake provider.
type seenRequest struct {
	Host, Method, Path, Auth, ContentType, Body string
}

// reroute sends every request to the fake server and records the original
// host. It shows that the built-in default endpoints are the ones called.
type reroute struct {
	target *url.URL
	seen   *[]seenRequest
}

func (rt reroute) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	*rt.seen = append(*rt.seen, seenRequest{Host: r.URL.Host, Method: r.Method, Path: r.URL.Path,
		Auth: r.Header.Get("Authorization"), ContentType: r.Header.Get("Content-Type"), Body: string(body)})
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = rt.target.Scheme, rt.target.Host
	r2.Host = rt.target.Host
	r2.Body = io.NopCloser(strings.NewReader(string(body)))
	return http.DefaultTransport.RoundTrip(r2)
}

func fakeProvider(t *testing.T, userInfo string) (*http.Client, *[]seenRequest) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "token") {
			_, _ = w.Write([]byte(`{"access_token":"tok_1"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok_1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(userInfo))
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	seen := &[]seenRequest{}
	return &http.Client{Transport: reroute{target: u, seen: seen}}, seen
}

func signInRecord(provider string, extra map[string]string) core.Integration {
	cfg := map[string]string{ConfigOAuthClientID: "cid", ConfigOAuthClientSecretRef: "OAUTH"}
	for k, v := range extra {
		cfg[k] = v
	}
	return core.Integration{ID: "int_1", TeamID: "team_1", Provider: provider, Config: cfg}
}

func TestTeamsSignInDefaults(t *testing.T) {
	ctx := context.Background()
	secrets := forgetest.NewSecrets(map[string]string{"OAUTH": "s3cret"})
	client, seen := fakeProvider(t, `{"id":"11111111-2222-3333-4444-555555555555","displayName":"Alice A","userPrincipalName":"alice@corp.test"}`)

	p, err := ProverFromIntegration(ctx, secrets, signInRecord("microsoft_teams", map[string]string{"tenant_id": "tenant-9"}), client)
	if err != nil || p == nil {
		t.Fatalf("prover = %v, %v", p, err)
	}
	u, _ := url.Parse(p.AuthURL("st", "https://of.example.com/integrations/link-callback"))
	if u.Host != "login.microsoftonline.com" || u.Path != "/tenant-9/oauth2/v2.0/authorize" {
		t.Fatalf("auth url %s", u)
	}
	if q := u.Query(); q.Get("scope") != "openid profile User.Read" || q.Get("client_id") != "cid" || q.Get("state") != "st" || q.Get("response_type") != "code" {
		t.Fatalf("auth query %v", q)
	}
	id, handle, err := p.Exchange(ctx, "code1", "https://of.example.com/integrations/link-callback")
	if err != nil || id != "11111111-2222-3333-4444-555555555555" || handle != "Alice A" {
		t.Fatalf("exchange = %q %q %v", id, handle, err)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests %+v", *seen)
	}
	tok, me := (*seen)[0], (*seen)[1]
	if tok.Host != "login.microsoftonline.com" || tok.Path != "/tenant-9/oauth2/v2.0/token" || tok.Method != "POST" {
		t.Fatalf("token request %+v", tok)
	}
	if f, _ := url.ParseQuery(tok.Body); f.Get("client_secret") != "s3cret" || f.Get("code") != "code1" || f.Get("grant_type") != "authorization_code" {
		t.Fatalf("token form %q", tok.Body)
	}
	if me.Host != "graph.microsoft.com" || me.Path != "/v1.0/me" || me.Method != "GET" {
		t.Fatalf("me request %+v", me)
	}

	// Without tenant_id any work or school account can sign in.
	p, err = ProverFromIntegration(ctx, secrets, signInRecord("microsoft_teams", nil), client)
	if err != nil {
		t.Fatal(err)
	}
	if u := p.AuthURL("st", "x"); !strings.HasPrefix(u, "https://login.microsoftonline.com/organizations/oauth2/v2.0/authorize?") {
		t.Fatalf("auth url %s", u)
	}
}

func TestLinearSignInDefaults(t *testing.T) {
	ctx := context.Background()
	secrets := forgetest.NewSecrets(map[string]string{"OAUTH": "s3cret"})
	client, seen := fakeProvider(t, `{"data":{"viewer":{"id":"7c1f-uuid","name":"Bob B"}}}`)

	p, err := ProverFromIntegration(ctx, secrets, signInRecord("linear", nil), client)
	if err != nil || p == nil {
		t.Fatalf("prover = %v, %v", p, err)
	}
	u, _ := url.Parse(p.AuthURL("st", "https://of.example.com/cb"))
	if u.Host != "linear.app" || u.Path != "/oauth/authorize" || u.Query().Get("scope") != "read" {
		t.Fatalf("auth url %s", u)
	}
	id, handle, err := p.Exchange(ctx, "code1", "https://of.example.com/cb")
	if err != nil || id != "7c1f-uuid" || handle != "Bob B" {
		t.Fatalf("exchange = %q %q %v", id, handle, err)
	}
	tok, info := (*seen)[0], (*seen)[1]
	if tok.Host != "api.linear.app" || tok.Path != "/oauth/token" {
		t.Fatalf("token request %+v", tok)
	}
	if info.Host != "api.linear.app" || info.Path != "/graphql" || info.Method != "POST" || info.Auth != "Bearer tok_1" || info.ContentType != "application/json" {
		t.Fatalf("user request %+v", info)
	}
	var q struct{ Query string }
	if err := json.Unmarshal([]byte(info.Body), &q); err != nil || !strings.Contains(q.Query, "viewer") {
		t.Fatalf("graphql body %q", info.Body)
	}

	// Several scopes are joined with commas.
	p, _ = ProverFromIntegration(ctx, secrets, signInRecord("linear", map[string]string{ConfigOAuthScopes: "read write"}), client)
	if got, _ := url.Parse(p.AuthURL("st", "x")); got.Query().Get("scope") != "read,write" {
		t.Fatalf("scope %q", got.Query().Get("scope"))
	}
}

func TestJiraSignInDefaults(t *testing.T) {
	ctx := context.Background()
	secrets := forgetest.NewSecrets(map[string]string{"OAUTH": "s3cret"})
	client, seen := fakeProvider(t, `{"account_id":"5b10ac8d82e05b22cc7d4ef5","name":"Carol C","email":"carol@x.test"}`)

	p, err := ProverFromIntegration(ctx, secrets, signInRecord("jira", nil), client)
	if err != nil || p == nil {
		t.Fatalf("prover = %v, %v", p, err)
	}
	u, _ := url.Parse(p.AuthURL("st", "https://of.example.com/cb"))
	q := u.Query()
	if u.Host != "auth.atlassian.com" || u.Path != "/authorize" || q.Get("audience") != "api.atlassian.com" || q.Get("prompt") != "consent" || q.Get("scope") != "read:me" {
		t.Fatalf("auth url %s", u)
	}
	id, handle, err := p.Exchange(ctx, "code1", "https://of.example.com/cb")
	if err != nil || id != "5b10ac8d82e05b22cc7d4ef5" || handle != "Carol C" {
		t.Fatalf("exchange = %q %q %v", id, handle, err)
	}
	tok, me := (*seen)[0], (*seen)[1]
	if tok.Host != "auth.atlassian.com" || tok.Path != "/oauth/token" || tok.ContentType != "application/json" {
		t.Fatalf("token request %+v", tok)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(tok.Body), &body); err != nil || body["client_secret"] != "s3cret" || body["code"] != "code1" || body["grant_type"] != "authorization_code" || body["redirect_uri"] != "https://of.example.com/cb" {
		t.Fatalf("token body %q (%v)", tok.Body, err)
	}
	if me.Host != "api.atlassian.com" || me.Path != "/me" || me.Method != "GET" {
		t.Fatalf("me request %+v", me)
	}
}

// The generic oauth_* keys override each built-in default.
func TestSignInDefaultsAreOverridable(t *testing.T) {
	ctx := context.Background()
	secrets := forgetest.NewSecrets(map[string]string{"OAUTH": "s3cret"})
	client, seen := fakeProvider(t, `{"sub":"u-1","nick":"dan"}`)
	override := map[string]string{
		ConfigOAuthAuthorizeURL: "https://idp.test/auth", ConfigOAuthTokenURL: "https://idp.test/token", ConfigOAuthUserInfoURL: "https://idp.test/userinfo",
		ConfigOAuthIDField: "sub", ConfigOAuthHandleField: "nick", ConfigOAuthScopes: "openid,email",
	}
	for _, provider := range []string{"microsoft_teams", "linear", "jira"} {
		*seen = nil
		p, err := ProverFromIntegration(ctx, secrets, signInRecord(provider, override), client)
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if u := p.AuthURL("st", "x"); !strings.HasPrefix(u, "https://idp.test/auth?") || !strings.Contains(u, "scope=openid") {
			t.Fatalf("%s: auth url %s", provider, u)
		}
		id, handle, err := p.Exchange(ctx, "c", "x")
		if err != nil || id != "u-1" || handle != "dan" {
			t.Fatalf("%s: exchange = %q %q %v", provider, id, handle, err)
		}
		if (*seen)[0].Host != "idp.test" || (*seen)[1].Host != "idp.test" {
			t.Fatalf("%s: requests %+v", provider, *seen)
		}
	}
}

func TestFieldStringPath(t *testing.T) {
	m := map[string]any{
		"https://slack.com/user_id": "U1",
		"data":                      map[string]any{"viewer": map[string]any{"id": "v1", "n": float64(42)}},
	}
	for key, want := range map[string]string{
		"https://slack.com/user_id": "U1", "data.viewer.id": "v1", "data.viewer.n": "42", "data.viewer.none": "", "data.x.id": "", "none": "", "": "",
	} {
		if got := fieldString(m, key); got != want {
			t.Errorf("fieldString(%q) = %q, want %q", key, got, want)
		}
	}
}

// An account link of provider "teams" finds the sign-in of the
// microsoft_teams record, and Linear and Jira records need no endpoint keys.
func TestTeamProverUsesBuiltInDefaults(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	secrets := forgetest.NewSecrets(map[string]string{"OAUTH": "s3cret"})
	for _, in := range []core.Integration{
		{ID: "int_t", TeamID: "team_1", Provider: "microsoft_teams", Name: "t", Config: map[string]string{ConfigOAuthClientID: "c", ConfigOAuthClientSecretRef: "OAUTH"}},
		{ID: "int_l", TeamID: "team_1", Provider: "linear", Name: "l", Config: map[string]string{ConfigOAuthClientID: "c", ConfigOAuthClientSecretRef: "OAUTH"}},
		{ID: "int_j", TeamID: "team_1", Provider: "jira", Name: "j", Config: map[string]string{ConfigOAuthClientID: "c", ConfigOAuthClientSecretRef: "OAUTH"}},
	} {
		if err := st.UpsertIntegration(ctx, &in); err != nil {
			t.Fatal(err)
		}
	}
	for provider, host := range map[string]string{"teams": "login.microsoftonline.com", "linear": "linear.app", "jira": "auth.atlassian.com"} {
		p, err := TeamProver(ctx, st, secrets, "team_1", provider, nil)
		if err != nil || p == nil {
			t.Fatalf("%s: prover = %v, %v", provider, p, err)
		}
		if u, _ := url.Parse(p.AuthURL("s", "r")); u.Host != host {
			t.Fatalf("%s: host %s", provider, u.Host)
		}
	}
}

// The sign-in record follows the integration selection rule: the factory
// record wins over the team record, and two records at one level give an
// error that names the provider. The order of the records never matters.
func TestSelectSignIn(t *testing.T) {
	rec := func(id, provider, factory string, signIn bool) core.Integration {
		cfg := map[string]string{"team_id": id}
		if signIn {
			cfg[ConfigOAuthClientID] = "cid_" + id
		}
		return core.Integration{ID: id, TeamID: "team_1", Provider: provider, FactoryID: factory, Config: cfg}
	}
	tests := []struct {
		name     string
		ints     []core.Integration
		provider string
		factory  string
		want     string // "" is no record
		wantErr  bool
	}{
		{"no record", nil, "slack", "", "", false},
		{"no sign-in", []core.Integration{rec("a", "slack", "", false)}, "slack", "", "", false},
		{"one team record", []core.Integration{rec("b", "slack", "", false), rec("a", "slack", "", true)}, "slack", "", "a", false},
		{"other provider ignored", []core.Integration{rec("a", "github", "", true), rec("b", "slack", "", true)}, "slack", "", "b", false},
		{"teams alias", []core.Integration{rec("a", "microsoft_teams", "", true)}, "teams", "", "a", false},
		{"factory record wins", []core.Integration{rec("t", "slack", "", true), rec("f", "slack", "fac_1", true)}, "slack", "fac_1", "f", false},
		{"factory record wins, other order", []core.Integration{rec("f", "slack", "fac_1", true), rec("t", "slack", "", true)}, "slack", "fac_1", "f", false},
		{"record of another factory not used", []core.Integration{rec("t", "slack", "", true), rec("g", "slack", "fac_2", true)}, "slack", "fac_1", "t", false},
		{"team record wins with no factory", []core.Integration{rec("f", "slack", "fac_1", true), rec("t", "slack", "", true)}, "slack", "", "t", false},
		{"one factory record and no team record", []core.Integration{rec("f", "slack", "fac_1", true)}, "slack", "", "f", false},
		{"two team records", []core.Integration{rec("a", "slack", "", true), rec("b", "slack", "", true)}, "slack", "", "", true},
		{"two factory records and no team record", []core.Integration{rec("f", "slack", "fac_1", true), rec("g", "slack", "fac_2", true)}, "slack", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, ok, err := selectSignIn(tc.ints, tc.provider, tc.factory)
			if tc.wantErr {
				if err == nil || !errors.Is(err, ErrNoIntegration) || !strings.Contains(err.Error(), tc.provider) {
					t.Fatalf("err = %v, want an error that names %s", err, tc.provider)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := ""; ok {
				got = in.ID
				if got != tc.want {
					t.Fatalf("record = %q, want %q", got, tc.want)
				}
			} else if tc.want != "" {
				t.Fatalf("no record, want %q", tc.want)
			}
		})
	}
}

// TeamProver returns an error, not one of the records, when two team
// records have a sign-in, and an error when the selected record is disabled.
func TestTeamProverNeverPicksByOrder(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	secrets := forgetest.NewSecrets(map[string]string{"OAUTH": "s3cret"})
	for _, in := range []core.Integration{
		{ID: "int_a", TeamID: "team_1", Provider: "slack", Name: "a", Config: map[string]string{"team_id": "T1", ConfigOAuthClientID: "c1", ConfigOAuthClientSecretRef: "OAUTH"}},
		{ID: "int_b", TeamID: "team_1", Provider: "slack", Name: "b", Config: map[string]string{"team_id": "T2", ConfigOAuthClientID: "c2", ConfigOAuthClientSecretRef: "OAUTH"}},
		{ID: "int_j", TeamID: "team_1", Provider: "jira", Name: "j", Status: core.IntegrationDisabled, Config: map[string]string{"site": "acme.example.net", ConfigOAuthClientID: "c", ConfigOAuthClientSecretRef: "OAUTH"}},
	} {
		if err := st.UpsertIntegration(ctx, &in); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := TeamProver(ctx, st, secrets, "team_1", "slack", nil); err == nil || p != nil || !strings.Contains(err.Error(), "slack") {
		t.Fatalf("slack: prover = %v, err = %v", p, err)
	}
	if p, err := TeamProver(ctx, st, secrets, "team_1", "jira", nil); err == nil || p != nil {
		t.Fatalf("jira: prover = %v, err = %v", p, err)
	}
}
