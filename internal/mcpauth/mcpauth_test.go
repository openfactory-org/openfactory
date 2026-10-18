package mcpauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/mcpauth"
	"github.com/openfactory-org/openfactory/internal/mcpserver"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

var ctx = context.Background()

const sessionCookie = "test_session"

type fixture struct {
	t     *testing.T
	ts    *httptest.Server
	store core.Store
	svc   *mcpauth.Service
	user  *core.User
	now   time.Time
}

func newFixture(t *testing.T) *fixture { return newFixtureWith(t, testdb.New(t)) }

// eachStore runs fn with a fixture on SQLite in memory and on a SQLite file.
func eachStore(t *testing.T, fn func(t *testing.T, f *fixture)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { fn(t, newFixture(t)) })
	t.Run("sqlite", func(t *testing.T) {
		s, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "of.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		fn(t, newFixtureWith(t, s))
	})
}

func newFixtureWith(t *testing.T, st core.Store) *fixture {
	t.Helper()
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	f := &fixture{t: t, ts: ts, store: st, now: time.Now().UTC()}
	_ = st.CreateTeam(ctx, &core.Team{ID: "team1", Name: "Acme"})
	f.user = &core.User{ID: "usr_ada", TeamID: "team1", Email: "ada@example.test", Role: core.RoleEditor}
	if err := st.CreateUser(ctx, f.user); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.svc = mcpauth.New(mcpauth.Options{
		Store:         st,
		PublicURL:     ts.URL,
		ResourcePaths: []string{mcpserver.Path, mcpserver.AliasPath},
		Session: func(r *http.Request) (*core.User, error) {
			c, err := r.Cookie(sessionCookie)
			if err != nil {
				return nil, nil //nolint:nilerr // no session cookie means no session, not an error
			}
			return st.GetUser(r.Context(), c.Value)
		},
		Now:    func() time.Time { return f.now },
		Logger: log,
	})
	f.svc.Register(mux)
	srv := mcpserver.New(mcpserver.Options{Store: st, PublicURL: ts.URL, Logger: log, OAuth: f.svc})
	srv.Register(mux)
	return f
}

// browser plays the developer in the browser: signed in to the dashboard,
// it opens the authorization URL and presses Allow.
func (f *fixture) browser(signedIn bool, decision string) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
		if signedIn {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.user.ID})
		}
		res, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !signedIn {
			return nil, errors.New("redirected to " + res.Header.Get("Location"))
		}
		if res.StatusCode != http.StatusOK {
			return nil, errors.New("consent page: " + res.Status + ": " + string(body))
		}
		m := regexp.MustCompile(`name="request_id" value="([^"]+)"`).FindSubmatch(body)
		if m == nil {
			return nil, errors.New("no consent form: " + string(body))
		}
		form := url.Values{"request_id": {string(m[1])}, "decision": {decision}}
		post, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.ts.URL+mcpauth.AuthorizePath, strings.NewReader(form.Encode()))
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		post.AddCookie(&http.Cookie{Name: sessionCookie, Value: f.user.ID})
		res, err = hc.Do(post)
		if err != nil {
			return nil, err
		}
		res.Body.Close()
		loc, err := url.Parse(res.Header.Get("Location"))
		if err != nil || res.StatusCode != http.StatusFound {
			return nil, errors.New("no redirect after consent: " + res.Status)
		}
		q := loc.Query()
		if e := q.Get("error"); e != "" {
			return nil, errors.New(e)
		}
		return &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
	}
}

func (f *fixture) client(fetch auth.AuthorizationCodeFetcher) (*mcp.ClientSession, error) {
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "Test coding agent", RedirectURIs: []string{"http://127.0.0.1:33418/callback"},
			GrantTypes: []string{"authorization_code", "refresh_token"},
		}},
		RedirectURL:              "http://127.0.0.1:33418/callback",
		AuthorizationCodeFetcher: fetch,
		RequestRefreshToken:      true,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
	return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: f.ts.URL + mcpserver.Path, OAuthHandler: h}, nil)
}

// TestBrowserSignIn: a developer adds only the URL to an MCP client and
// signs in through the browser.
func TestBrowserSignIn(t *testing.T) { eachStore(t, testBrowserSignIn) }

func testBrowserSignIn(t *testing.T, f *fixture) {
	sess, err := f.client(f.browser(true, "allow"))
	if err != nil {
		t.Fatalf("connect with browser sign-in: %v", err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("list tools: %v %v", tools, err)
	}
	grants, err := f.svc.ListGrants(ctx, f.user.ID)
	if err != nil || len(grants) != 1 || grants[0].ClientName != "Test coding agent" || grants[0].TeamID != "team1" {
		t.Fatalf("grants = %+v %v", grants, err)
	}

	// Revoking the sign-in ends access at once.
	if err := f.svc.Revoke(ctx, grants[0].ID); err != nil {
		t.Fatal(err)
	}
	if g, _ := f.svc.ListGrants(ctx, f.user.ID); len(g) != 0 {
		t.Fatalf("revoked sign-in still listed: %+v", g)
	}
}

func TestDeniedAndSignedOut(t *testing.T) {
	f := newFixture(t)
	if _, err := f.client(f.browser(true, "deny")); err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("deny: %v", err)
	}
	_, err := f.client(f.browser(false, ""))
	if err == nil || !strings.Contains(err.Error(), "/login?return_to=%2Foauth%2Fauthorize") {
		t.Fatalf("signed out must go to the dashboard login: %v", err)
	}
}

func TestUnauthorizedNamesResourceMetadata(t *testing.T) {
	f := newFixture(t)
	for _, path := range []string{mcpserver.Path, mcpserver.AliasPath} {
		res, err := http.Post(f.ts.URL+path, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		h := res.Header.Get("WWW-Authenticate")
		if res.StatusCode != 401 || !strings.Contains(h, `resource_metadata="`+f.ts.URL+mcpauth.ResourceMetadataPath+path+`"`) {
			t.Fatalf("%s: %d %q", path, res.StatusCode, h)
		}
		var meta struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
		}
		mr, err := http.Get(f.ts.URL + mcpauth.ResourceMetadataPath + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = json.NewDecoder(mr.Body).Decode(&meta)
		mr.Body.Close()
		if meta.Resource != f.ts.URL+path || len(meta.Servers) != 1 || meta.Servers[0] != f.ts.URL {
			t.Fatalf("metadata = %+v", meta)
		}
	}
}

// signIn runs the flow by hand and returns the token response.
func (f *fixture) signIn(t *testing.T) (clientID string, tok map[string]any) {
	t.Helper()
	reg, _ := json.Marshal(map[string]any{"client_name": "cli", "redirect_uris": []string{"http://localhost/cb"}})
	res, err := http.Post(f.ts.URL+mcpauth.RegisterPath, "application/json", strings.NewReader(string(reg)))
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	_ = json.NewDecoder(res.Body).Decode(&c)
	res.Body.Close()
	clientID, _ = c["client_id"].(string)
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {"http://localhost:5555/cb"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"state": {"xyz"}, "resource": {f.ts.URL + "/mcp"},
	}
	r, err := f.browser(true, "allow")(ctx, &auth.AuthorizationArgs{URL: f.ts.URL + mcpauth.AuthorizePath + "?" + q.Encode()})
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "xyz" || r.Iss != f.ts.URL {
		t.Fatalf("result = %+v", r)
	}
	// A wrong verifier fails, and the code is then used up.
	bad := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {r.Code}, "client_id": {clientID}, "code_verifier": {strings.Repeat("w", 50)}})
	if bad["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %v", bad)
	}
	r, _ = f.browser(true, "allow")(ctx, &auth.AuthorizationArgs{URL: f.ts.URL + mcpauth.AuthorizePath + "?" + q.Encode()})
	tok = f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {r.Code}, "client_id": {clientID}, "code_verifier": {verifier}, "redirect_uri": {"http://localhost:5555/cb"}})
	if tok["access_token"] == nil {
		t.Fatalf("token: %v", tok)
	}
	again := f.token(t, url.Values{"grant_type": {"authorization_code"}, "code": {r.Code}, "client_id": {clientID}, "code_verifier": {verifier}})
	if again["error"] != "invalid_grant" {
		t.Fatalf("a code must work once: %v", again)
	}
	return clientID, tok
}

func (f *fixture) token(t *testing.T, form url.Values) map[string]any {
	t.Helper()
	res, err := http.PostForm(f.ts.URL+mcpauth.TokenPath, form)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return out
}

func TestTokenLifecycle(t *testing.T) { eachStore(t, testTokenLifecycle) }

func testTokenLifecycle(t *testing.T, f *fixture) {
	clientID, tok := f.signIn(t)
	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	if !strings.HasPrefix(access, mcpauth.AccessTokenPrefix) || !strings.HasPrefix(refresh, mcpauth.RefreshTokenPrefix) || tok["expires_in"].(float64) != 3600 {
		t.Fatalf("tokens = %v", tok)
	}
	g, u, err := f.svc.Verify(ctx, access)
	if err != nil || u.ID != f.user.ID || g.ClientID != clientID {
		t.Fatalf("verify: %+v %+v %v", g, u, err)
	}

	// The access token expires after 1 hour; the refresh token rotates.
	f.now = f.now.Add(61 * time.Minute)
	if _, _, err := f.svc.Verify(ctx, access); !errors.Is(err, mcpauth.ErrInvalidToken) {
		t.Fatalf("expired token: %v", err)
	}
	next := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	newAccess, _ := next["access_token"].(string)
	if newAccess == "" || next["refresh_token"] == refresh {
		t.Fatalf("refresh: %v", next)
	}
	if _, _, err := f.svc.Verify(ctx, newAccess); err != nil {
		t.Fatal(err)
	}
	if old := f.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}); old["error"] != "invalid_grant" {
		t.Fatalf("a used refresh token must fail: %v", old)
	}

	// RFC 7009 revocation ends the sign-in.
	res, err := http.PostForm(f.ts.URL+mcpauth.RevokePath, url.Values{"token": {next["refresh_token"].(string)}})
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("revoke: %v %v", res, err)
	}
	if _, _, err := f.svc.Verify(ctx, newAccess); !errors.Is(err, mcpauth.ErrInvalidToken) {
		t.Fatalf("revoked token: %v", err)
	}

	// A removed user cannot use a sign-in.
	_, tok2 := f.signIn(t)
	f.user.TeamID = "team2"
	_ = f.store.UpdateUser(ctx, f.user)
	if _, _, err := f.svc.Verify(ctx, tok2["access_token"].(string)); !errors.Is(err, mcpauth.ErrInvalidToken) {
		t.Fatalf("user moved out of the team: %v", err)
	}
}

func TestRegistrationRules(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		uris []string
		ok   bool
	}{
		{[]string{"http://127.0.0.1:8080/cb"}, true},
		{[]string{"https://app.example.test/cb"}, true},
		{[]string{"com.example.agent://oauth/callback"}, true},
		{[]string{"http://evil.example.test/cb"}, false},
		{[]string{"javascript:alert(1)"}, false},
		{nil, false},
	} {
		body, _ := json.Marshal(map[string]any{"redirect_uris": tc.uris})
		res, err := http.Post(f.ts.URL+mcpauth.RegisterPath, "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if (res.StatusCode == http.StatusCreated) != tc.ok {
			t.Errorf("%v: status %d", tc.uris, res.StatusCode)
		}
	}
	var meta map[string]any
	res, _ := http.Get(f.ts.URL + mcpauth.ServerMetadataPath)
	_ = json.NewDecoder(res.Body).Decode(&meta)
	res.Body.Close()
	if meta["issuer"] != f.ts.URL || meta["token_endpoint"] != f.ts.URL+mcpauth.TokenPath {
		t.Fatalf("server metadata = %v", meta)
	}
}
