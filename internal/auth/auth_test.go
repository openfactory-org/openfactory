package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

const publicURL = "https://of.example"

type harness struct {
	t     *testing.T
	idp   *fakeIdP
	store *sqlstore.Store
	svc   *Service
	mux   *http.ServeMux
	team  *core.Team
	audit *recordingSink

	mu  sync.Mutex
	now time.Time
}

func (h *harness) clock() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

type recordingSink struct {
	mu     sync.Mutex
	events []AuditEvent
}

func (r *recordingSink) Record(_ context.Context, e AuditEvent) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recordingSink) last() AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return AuditEvent{}
	}
	return r.events[len(r.events)-1]
}

func newHarness(t *testing.T, mutate func(*OIDCConfig)) *harness {
	t.Helper()
	h := &harness{t: t, idp: newFakeIdP(t), store: testdb.New(t), now: time.Now(), audit: &recordingSink{}}
	ctx := context.Background()
	h.team = &core.Team{ID: "team_1", Name: "Acme", CreatedAt: h.now}
	if err := h.store.CreateTeam(ctx, h.team); err != nil {
		t.Fatal(err)
	}
	cfg := OIDCConfig{
		Issuer: h.idp.issuer(), ClientID: h.idp.clientID, ClientSecret: h.idp.secret,
		RedirectURL: publicURL + OIDCCallbackPath, RequireVerifiedEmail: true,
		HTTPClient: h.idp.srv.Client(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	prov, err := NewOIDCProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, SessionKeySize)
	copy(key, "0123456789abcdef0123456789abcdef")
	h.svc, err = New(Config{
		Store: h.store, SessionKey: key, PublicURL: publicURL,
		SessionTTL: time.Hour, APIKeyLookup: testKeyLookup(h.store), Providers: []LoginProvider{prov},
		Audit: h.audit, MountMe: true, Now: h.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.mux = http.NewServeMux()
	h.svc.Register(h.mux)
	// A protected mutation endpoint for CSRF tests.
	h.mux.Handle("POST /api/v1/things", h.svc.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if err := h.svc.Authorizer().Allow(r.Context(), p, ActionWrite, Resource{Type: "thing", TeamID: h.team.ID}); err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))
	return h
}

// testKeyLookup mirrors the REST API's key format: of_<8 prefix>_<secret>,
// SHA-256 hashed.
func testKeyLookup(st core.Store) APIKeyLookup {
	return func(ctx context.Context, key string) (*core.User, *core.APIKey, error) {
		rest, ok := strings.CutPrefix(key, "of_")
		prefix, _, ok2 := strings.Cut(rest, "_")
		if !ok || !ok2 {
			return nil, nil, core.ErrUnauthorized
		}
		k, err := st.GetAPIKeyByPrefix(ctx, prefix)
		if err != nil {
			return nil, nil, err
		}
		if k.HashedKey != hashKey(key) {
			return nil, nil, core.ErrUnauthorized
		}
		u, err := st.GetUser(ctx, k.UserID)
		if err != nil {
			return nil, nil, err
		}
		return u, k, nil
	}
}

func hashKey(k string) string { s := sha256.Sum256([]byte(k)); return hex.EncodeToString(s[:]) }

// addUserWithKey creates a user and an API key; it returns the plaintext key.
func (h *harness) addUserWithKey(email string, role core.Role, prefix string) (*core.User, *core.APIKey, string) {
	h.t.Helper()
	ctx := context.Background()
	u := &core.User{ID: core.NewID("usr"), TeamID: h.team.ID, Email: email, Name: email, Role: role, CreatedAt: h.now}
	if err := h.store.CreateUser(ctx, u); err != nil {
		h.t.Fatal(err)
	}
	plain := "of_" + prefix + "_" + strings.Repeat("A", 40)
	k := &core.APIKey{ID: core.NewID("key"), TeamID: h.team.ID, UserID: u.ID, Name: "k", Prefix: prefix, HashedKey: hashKey(plain), CreatedAt: h.now}
	if err := h.store.CreateAPIKey(ctx, k); err != nil {
		h.t.Fatal(err)
	}
	return u, k, plain
}

func (h *harness) do(method, target, cookies string, hdr map[string]string, body string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(method, publicURL+target, strings.NewReader(body))
	if cookies != "" {
		req.Header.Set("Cookie", cookies)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

type loginOpts struct {
	claims     map[string]any
	nonce      string // override the nonce the IdP puts in the token
	state      string // override the state sent back to the callback
	dropCookie bool
	tamperFlow bool
}

// oidcLogin runs the full browser flow and returns the callback response.
func (h *harness) oidcLogin(o loginOpts) *httptest.ResponseRecorder {
	h.t.Helper()
	start := h.do("GET", "/auth/oidc/login?return_to=/factories/f1", "", nil, "")
	if start.Code != http.StatusFound {
		h.t.Fatalf("login start: status %d body %s", start.Code, start.Body)
	}
	loc, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	q := loc.Query()
	if got := loc.Scheme + "://" + loc.Host + loc.Path; got != h.idp.issuer()+"/authorize" {
		h.t.Fatalf("redirected to %s", got)
	}
	for k, want := range map[string]string{
		"response_type": "code", "client_id": h.idp.clientID, "redirect_uri": publicURL + OIDCCallbackPath,
		"code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			h.t.Fatalf("authorize %s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.Contains(q.Get("scope"), "openid") || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		h.t.Fatalf("authorize query incomplete: %v", q)
	}
	nonce := q.Get("nonce")
	if o.nonce != "" {
		nonce = o.nonce
	}
	code := h.idp.authorize(q.Get("code_challenge"), nonce, o.claims)
	state := q.Get("state")
	if o.state != "" {
		state = o.state
	}
	cookies := start.Result().Cookies()
	var flow *http.Cookie
	for _, c := range cookies {
		if c.Name == "of_flow_oidc" {
			flow = c
		}
	}
	if flow == nil || !flow.HttpOnly || !flow.Secure || flow.Path != "/auth/oidc/" {
		h.t.Fatalf("flow cookie missing or weak: %+v", flow)
	}
	ck := cookieHeader(cookies)
	if o.dropCookie {
		ck = ""
	}
	if o.tamperFlow {
		ck = "of_flow_oidc=" + tamper(flow.Value)
	}
	return h.do("GET", OIDCCallbackPath+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), ck, nil, "")
}

func tamper(v string) string {
	b, _ := base64.RawURLEncoding.DecodeString(v)
	b[len(b)/2] ^= 0x01
	return base64.RawURLEncoding.EncodeToString(b)
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookie && c.MaxAge >= 0 && c.Value != "" {
			return c
		}
	}
	return nil
}

type meResp struct {
	User           core.User      `json:"user"`
	Team           core.Team      `json:"team"`
	Role           core.Role      `json:"role"`
	AuthMethod     string         `json:"auth_method"`
	Provider       string         `json:"provider"`
	LoginProviders []ProviderInfo `json:"login_providers"`
	Code           string         `json:"code"`
}

func (h *harness) me(cookie string) (int, meResp) {
	h.t.Helper()
	rec := h.do("GET", "/api/v1/me", cookie, nil, "")
	var m meResp
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		h.t.Fatalf("me body %q: %v", rec.Body, err)
	}
	return rec.Code, m
}

// ---------- OIDC ----------

func TestOIDCLoginProvisionsUserAndSession(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.oidcLogin(loginOpts{claims: map[string]any{
		"sub": "idp-123", "email": "ada@acme.test", "email_verified": true, "name": "Ada", "preferred_username": "ada",
	}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/factories/f1" {
		t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// PKCE: the verifier sent to the token endpoint matched the challenge (the
	// fake enforces it) and is a proper high-entropy verifier.
	if len(h.idp.lastVerifier) < 43 {
		t.Fatalf("verifier too short: %q", h.idp.lastVerifier)
	}
	sc := sessionFrom(t, rec)
	if sc == nil || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" {
		t.Fatalf("session cookie: %+v", sc)
	}
	code, m := h.me(SessionCookie + "=" + sc.Value)
	if code != 200 || m.User.Email != "ada@acme.test" || m.User.Name != "Ada" || m.Role != core.RoleUser ||
		m.Team.ID != h.team.ID || m.AuthMethod != MethodSession || m.Provider != ProviderOIDC {
		t.Fatalf("me: %d %+v", code, m)
	}
	x, err := h.store.FindExternalIdentity(context.Background(), ProviderOIDC, "idp-123")
	if err != nil || x.UserID != m.User.ID || x.Handle != "ada" {
		t.Fatalf("identity link: %+v %v", x, err)
	}
	if ev := h.audit.last(); ev.Action != "auth.login" || ev.Outcome != OutcomeSuccess || ev.UserID != m.User.ID {
		t.Fatalf("audit: %+v", ev)
	}
	// Second login reuses the user.
	rec = h.oidcLogin(loginOpts{claims: map[string]any{"sub": "idp-123", "email": "ada@acme.test", "email_verified": true}})
	_, m2 := h.me(SessionCookie + "=" + sessionFrom(t, rec).Value)
	if m2.User.ID != m.User.ID {
		t.Fatalf("second login created a new user: %s vs %s", m2.User.ID, m.User.ID)
	}
}

func TestOIDCLoginFailures(t *testing.T) {
	verified := map[string]any{"sub": "s1", "email": "bob@acme.test", "email_verified": true}
	tests := []struct {
		name   string
		mutate func(*OIDCConfig)
		opts   loginOpts
		want   string
	}{
		{"nonce mismatch", nil, loginOpts{claims: verified, nonce: "attacker-nonce"}, LoginErrNonceMismatch},
		{"state mismatch", nil, loginOpts{claims: verified, state: "forged"}, LoginErrInvalidState},
		{"flow cookie missing", nil, loginOpts{claims: verified, dropCookie: true}, LoginErrInvalidState},
		{"flow cookie tampered", nil, loginOpts{claims: verified, tamperFlow: true}, LoginErrInvalidState},
		{"wrong audience", nil, loginOpts{claims: map[string]any{"sub": "s1", "aud": "someone-else"}}, LoginErrProviderError},
		{"expired id token", nil, loginOpts{claims: map[string]any{"sub": "s1", "exp": time.Now().Add(-time.Hour).Unix()}}, LoginErrProviderError},
		{"domain not allowed", func(c *OIDCConfig) { c.AllowedDomains = []string{"acme.test"} },
			loginOpts{claims: map[string]any{"sub": "s1", "email": "eve@evil.test", "email_verified": true}}, LoginErrDomainNotAllowed},
		{"domain allowed but unverified", func(c *OIDCConfig) { c.AllowedDomains = []string{"ACME.test"} },
			loginOpts{claims: map[string]any{"sub": "s1", "email": "eve@acme.test", "email_verified": false}}, LoginErrEmailNotVerified},
		{"domain missing email", func(c *OIDCConfig) { c.AllowedDomains = []string{"acme.test"} },
			loginOpts{claims: map[string]any{"sub": "s1"}}, LoginErrDomainNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mutate)
			rec := h.oidcLogin(tc.opts)
			if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?error="+tc.want {
				t.Fatalf("got %d %q, want error=%s", rec.Code, rec.Header().Get("Location"), tc.want)
			}
			if sessionFrom(t, rec) != nil {
				t.Fatal("failed login set a session")
			}
			if ev := h.audit.last(); ev.Outcome != OutcomeFailure || ev.Action != "auth.login" {
				t.Fatalf("audit: %+v", ev)
			}
		})
	}
}

func TestOIDCDomainAllowed(t *testing.T) {
	h := newHarness(t, func(c *OIDCConfig) { c.AllowedDomains = []string{"acme.test"} })
	rec := h.oidcLogin(loginOpts{claims: map[string]any{"sub": "s1", "email": "Ann@ACME.test", "email_verified": "true"}})
	if sessionFrom(t, rec) == nil {
		t.Fatalf("allowed domain rejected: %s", rec.Header().Get("Location"))
	}
}

func TestOIDCIdPError(t *testing.T) {
	h := newHarness(t, nil)
	start := h.do("GET", "/auth/oidc/login", "", nil, "")
	loc, _ := url.Parse(start.Header().Get("Location"))
	rec := h.do("GET", OIDCCallbackPath+"?error=access_denied&state="+loc.Query().Get("state"), cookieHeader(start.Result().Cookies()), nil, "")
	if rec.Header().Get("Location") != "/login?error="+LoginErrAccessDenied {
		t.Fatalf("got %q", rec.Header().Get("Location"))
	}
}

func TestOIDCProviderUnavailable(t *testing.T) {
	h := newHarness(t, nil)
	h.idp.srv.Close()
	rec := h.do("GET", "/auth/oidc/login", "", nil, "")
	if rec.Header().Get("Location") != "/login?error="+LoginErrProviderUnavailable {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestOIDCReturnToIsLocal(t *testing.T) {
	h := newHarness(t, nil)
	for _, rt := range []string{"https://evil.test/x", "//evil.test", "/\\evil.test"} {
		start := h.do("GET", "/auth/oidc/login?return_to="+url.QueryEscape(rt), "", nil, "")
		loc, _ := url.Parse(start.Header().Get("Location"))
		q := loc.Query()
		code := h.idp.authorize(q.Get("code_challenge"), q.Get("nonce"), map[string]any{"sub": "s1"})
		rec := h.do("GET", OIDCCallbackPath+"?code="+code+"&state="+q.Get("state"), cookieHeader(start.Result().Cookies()), nil, "")
		if rec.Header().Get("Location") != "/" {
			t.Errorf("return_to %q redirected to %q", rt, rec.Header().Get("Location"))
		}
	}
}

func TestOIDCGroupRoleMapping(t *testing.T) {
	managed := func(c *OIDCConfig) {
		c.GroupsClaim = "roles"
		c.AdminGroups = []string{"of-admins"}
		c.EditorGroups = []string{"of-editors", "eng"}
	}
	tests := []struct {
		name   string
		mutate func(*OIDCConfig)
		groups any
		want   core.Role
	}{
		{"admin group", managed, []string{"eng", "of-admins"}, core.RoleAdmin},
		{"editor group", managed, []string{"eng"}, core.RoleEditor},
		{"no match uses default", managed, []string{"sales"}, core.RoleUser},
		{"string claim", managed, "sales of-editors", core.RoleEditor},
		{"missing claim", managed, nil, core.RoleUser},
		{"custom default role", func(c *OIDCConfig) { managed(c); c.DefaultRole = core.RoleEditor }, []string{"x"}, core.RoleEditor},
		{"unmanaged default", func(c *OIDCConfig) { c.DefaultRole = core.RoleEditor }, []string{"of-admins"}, core.RoleEditor},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.mutate)
			claims := map[string]any{"sub": "s1", "email": "g@acme.test", "email_verified": true}
			if tc.groups != nil {
				claims["roles"] = tc.groups
				claims["groups"] = tc.groups
			}
			rec := h.oidcLogin(loginOpts{claims: claims})
			sc := sessionFrom(t, rec)
			if sc == nil {
				t.Fatalf("no session: %s", rec.Header().Get("Location"))
			}
			if _, m := h.me(SessionCookie + "=" + sc.Value); m.Role != tc.want {
				t.Fatalf("role %s, want %s", m.Role, tc.want)
			}
		})
	}
}

func TestOIDCRoleSync(t *testing.T) {
	t.Run("managed roles follow the IdP", func(t *testing.T) {
		h := newHarness(t, func(c *OIDCConfig) { c.AdminGroups = []string{"admins"} })
		claims := map[string]any{"sub": "s1", "email": "r@acme.test", "email_verified": true, "groups": []string{"admins"}}
		h.oidcLogin(loginOpts{claims: claims})
		claims["groups"] = []string{}
		sc := sessionFrom(t, h.oidcLogin(loginOpts{claims: claims}))
		if _, m := h.me(SessionCookie + "=" + sc.Value); m.Role != core.RoleUser {
			t.Fatalf("role not demoted: %s", m.Role)
		}
	})
	t.Run("unmanaged roles are kept", func(t *testing.T) {
		h := newHarness(t, nil)
		u, _, _ := h.addUserWithKey("boss@acme.test", core.RoleAdmin, "bossbos1")
		sc := sessionFrom(t, h.oidcLogin(loginOpts{claims: map[string]any{"sub": "s9", "email": "boss@acme.test", "email_verified": true}}))
		code, m := h.me(SessionCookie + "=" + sc.Value)
		if code != 200 || m.User.ID != u.ID || m.Role != core.RoleAdmin {
			t.Fatalf("linking by verified email: %d %+v", code, m)
		}
	})
	t.Run("unverified email does not link", func(t *testing.T) {
		h := newHarness(t, nil)
		h.addUserWithKey("boss@acme.test", core.RoleAdmin, "bossbos1")
		rec := h.oidcLogin(loginOpts{claims: map[string]any{"sub": "s9", "email": "boss@acme.test", "email_verified": false}})
		if rec.Header().Get("Location") != "/login?error="+LoginErrEmailNotVerified {
			t.Fatalf("got %q", rec.Header().Get("Location"))
		}
	})
}

// ---------- sessions ----------

func TestSessionTamperAndExpiry(t *testing.T) {
	h := newHarness(t, nil)
	_, _, key := h.addUserWithKey("s@acme.test", core.RoleEditor, "sessabc1")
	rec := h.do("POST", RouteSession, "", map[string]string{CSRFHeader: "1"}, `{"api_key":"`+key+`"}`)
	sc := sessionFrom(t, rec)
	if rec.Code != 200 || sc == nil {
		t.Fatalf("session login: %d %s", rec.Code, rec.Body)
	}
	otherKey := make([]byte, SessionKeySize)
	other, _ := newSealer(otherKey, "session")
	forged, _ := other.seal(sessionClaims{UserID: "x", TeamID: h.team.ID}, SessionCookie, h.now.Add(time.Hour))
	flowSealed, _ := h.svc.flows.seal(sessionClaims{UserID: "x", TeamID: h.team.ID}, SessionCookie, h.now.Add(time.Hour))

	tests := []struct{ name, value string }{
		{"tampered", tamper(sc.Value)},
		{"truncated", sc.Value[:len(sc.Value)-4]},
		{"garbage", "not-a-session"},
		{"other key", forged},
		{"other purpose key", flowSealed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do("GET", "/api/v1/me", SessionCookie+"="+tc.value, nil, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d", rec.Code)
			}
			if c := rec.Result().Cookies(); len(c) == 0 || c[0].MaxAge >= 0 {
				t.Fatal("invalid session cookie not cleared")
			}
		})
	}
	if code, _ := h.me(SessionCookie + "=" + sc.Value); code != 200 {
		t.Fatalf("valid session rejected: %d", code)
	}
	h.advance(2 * time.Hour)
	if code, _ := h.me(SessionCookie + "=" + sc.Value); code != http.StatusUnauthorized {
		t.Fatalf("expired session accepted: %d", code)
	}
}

func TestSessionEndsWhenUserOrKeyGone(t *testing.T) {
	h := newHarness(t, nil)
	_, k, key := h.addUserWithKey("k@acme.test", core.RoleUser, "keygone1")
	sc := sessionFrom(t, h.do("POST", RouteSession, "", map[string]string{CSRFHeader: "1"}, `{"api_key":"`+key+`"}`))
	if code, _ := h.me(SessionCookie + "=" + sc.Value); code != 200 {
		t.Fatal("session not valid")
	}
	if err := h.store.DeleteAPIKey(context.Background(), k.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.me(SessionCookie + "=" + sc.Value); code != http.StatusUnauthorized {
		t.Fatalf("session survived key deletion: %d", code)
	}
}

func TestAPIKeySessionLogin(t *testing.T) {
	h := newHarness(t, nil)
	u, _, key := h.addUserWithKey("api@acme.test", core.RoleEditor, "apikey01")
	tests := []struct {
		name string
		hdr  map[string]string
		body string
		want int
	}{
		{"ok", map[string]string{CSRFHeader: "1"}, `{"api_key":"` + key + `"}`, 200},
		{"ok same origin", map[string]string{"Origin": publicURL}, `{"api_key":"` + key + `"}`, 200},
		{"wrong key", map[string]string{CSRFHeader: "1"}, `{"api_key":"of_apikey01_` + strings.Repeat("B", 40) + `"}`, 401},
		{"unknown key", map[string]string{CSRFHeader: "1"}, `{"api_key":"of_nokey000_` + strings.Repeat("B", 40) + `"}`, 401},
		{"bad body", map[string]string{CSRFHeader: "1"}, `nope`, 400},
		{"empty key", map[string]string{CSRFHeader: "1"}, `{}`, 400},
		{"no csrf header", nil, `{"api_key":"` + key + `"}`, 403},
		{"cross origin", map[string]string{"Origin": "https://evil.test"}, `{"api_key":"` + key + `"}`, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do("POST", RouteSession, "", tc.hdr, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			sc := sessionFrom(t, rec)
			if (tc.want == 200) != (sc != nil) {
				t.Fatalf("session cookie presence wrong: %v", sc)
			}
			if tc.want != 200 {
				return
			}
			var m meResp
			_ = json.Unmarshal(rec.Body.Bytes(), &m)
			if m.User.ID != u.ID || m.Role != core.RoleEditor || m.Provider != ProviderAPIKey {
				t.Fatalf("response: %+v", m)
			}
		})
	}
}

func TestMeUnauthenticatedListsProviders(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.do("GET", "/api/v1/me", "", nil, "")
	if rec.Code != 401 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var m meResp
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m.Code != "authentication_required" || len(m.LoginProviders) != 2 ||
		m.LoginProviders[0] != (ProviderInfo{Name: "oidc", DisplayName: "Single sign-on", LoginURL: "/auth/oidc/login"}) ||
		m.LoginProviders[1].Name != ProviderAPIKey {
		t.Fatalf("body: %s", rec.Body)
	}
}

func TestBearerAuthentication(t *testing.T) {
	h := newHarness(t, nil)
	u, _, key := h.addUserWithKey("b@acme.test", core.RoleUser, "bearer01")
	code, m := func() (int, meResp) {
		rec := h.do("GET", "/api/v1/me", "", map[string]string{"Authorization": "Bearer " + key}, "")
		var m meResp
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}()
	if code != 200 || m.User.ID != u.ID || m.AuthMethod != MethodAPIKey {
		t.Fatalf("bearer me: %d %+v", code, m)
	}
	rec := h.do("GET", "/api/v1/me", "", map[string]string{"Authorization": "Bearer of_bearer01_" + strings.Repeat("Z", 40)}, "")
	if rec.Code != 401 {
		t.Fatalf("bad bearer: %d", rec.Code)
	}
	// Non-of_ bearer tokens are left to other authenticators.
	rec = h.do("GET", "/api/v1/me", "", map[string]string{"Authorization": "Bearer worker-token"}, "")
	if rec.Code != 401 {
		t.Fatalf("foreign bearer: %d", rec.Code)
	}
	if p, err := h.svc.Authenticate(httptest.NewRequest("GET", "/", nil)); p != nil || err != nil {
		t.Fatalf("anonymous: %v %v", p, err)
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t, nil)
	_, _, key := h.addUserWithKey("l@acme.test", core.RoleUser, "logout01")
	sc := sessionFrom(t, h.do("POST", RouteSession, "", map[string]string{CSRFHeader: "1"}, `{"api_key":"`+key+`"}`))
	ck := SessionCookie + "=" + sc.Value
	if rec := h.do("POST", RouteLogout, ck, nil, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF header: %d", rec.Code)
	}
	rec := h.do("POST", RouteLogout, ck, map[string]string{CSRFHeader: "1"}, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != SessionCookie || c[0].MaxAge >= 0 {
		t.Fatalf("cookie not cleared: %+v", c)
	}
	if ev := h.audit.last(); ev.Action != "auth.logout" || ev.UserID == "" {
		t.Fatalf("audit: %+v", ev)
	}
}

// ---------- CSRF ----------

func TestCSRF(t *testing.T) {
	h := newHarness(t, nil)
	_, _, key := h.addUserWithKey("c@acme.test", core.RoleEditor, "csrftest")
	sc := sessionFrom(t, h.do("POST", RouteSession, "", map[string]string{CSRFHeader: "1"}, `{"api_key":"`+key+`"}`))
	ck := SessionCookie + "=" + sc.Value
	tests := []struct {
		name   string
		cookie string
		hdr    map[string]string
		want   int
	}{
		{"cookie without header", ck, nil, 403},
		{"cookie with header", ck, map[string]string{CSRFHeader: "1"}, 200},
		{"cookie with wrong header value", ck, map[string]string{CSRFHeader: "yes"}, 403},
		{"cookie same origin", ck, map[string]string{"Origin": publicURL}, 200},
		{"cookie cross origin", ck, map[string]string{"Origin": "https://evil.test"}, 403},
		{"cookie cross origin with fetch metadata", ck, map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"cookie same-origin fetch metadata", ck, map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"bearer needs no header", "", map[string]string{"Authorization": "Bearer " + key}, 200},
		{"anonymous", "", nil, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if rec := h.do("POST", "/api/v1/things", tc.cookie, tc.hdr, ""); rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
	_, err := h.svc.Authenticate(func() *http.Request {
		r := httptest.NewRequest("DELETE", "/x", nil)
		r.Header.Set("Cookie", ck)
		return r
	}())
	if !errors.Is(err, ErrCSRF) || !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("Authenticate CSRF error: %v", err)
	}

	// Standalone RequireCSRF.
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for _, tc := range []struct {
		method string
		hdr    map[string]string
		want   int
	}{
		{"POST", nil, 403}, {"GET", nil, 200}, {"POST", map[string]string{CSRFHeader: "1"}, 200},
	} {
		r := httptest.NewRequest(tc.method, "/x", nil)
		r.Header.Set("Cookie", ck)
		for k, v := range tc.hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.svc.RequireCSRF(ok).ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("RequireCSRF %s %v: %d, want %d", tc.method, tc.hdr, rec.Code, tc.want)
		}
	}
}

// ---------- authorizer, tenants, config ----------

func TestRoleAuthorizer(t *testing.T) {
	ctx := context.Background()
	p := func(r core.Role) *Principal { return &Principal{User: &core.User{ID: "u"}, TeamID: "t1", Role: r} }
	tests := []struct {
		p     *Principal
		a     Action
		res   Resource
		allow bool
	}{
		{p(core.RoleUser), ActionRead, Resource{}, true},
		{p(core.RoleUser), ActionOperate, Resource{}, true},
		{p(core.RoleUser), ActionWrite, Resource{}, false},
		{p(core.RoleUser), ActionAdmin, Resource{}, false},
		{p(core.RoleUser), ActionRead, Resource{}, true},
		{p(core.RoleUser), ActionOperate, Resource{}, true},
		{p(core.RoleUser), ActionWrite, Resource{}, false},
		{withOverride(p(core.RoleUser), "fac_a", core.RoleEditor), ActionWrite, Resource{FactoryID: "fac_a"}, true},
		{withOverride(p(core.RoleUser), "fac_a", core.RoleEditor), ActionWrite, Resource{FactoryID: "fac_b"}, false},
		{withOverride(p(core.RoleUser), "fac_a", core.RoleEditor), ActionWrite, Resource{}, false},
		{withOverride(p(core.RoleEditor), "fac_a", core.RoleUser), ActionWrite, Resource{FactoryID: "fac_a"}, false},
		{withOverride(p(core.RoleEditor), "fac_a", "bogus"), ActionWrite, Resource{FactoryID: "fac_a"}, true},
		{p(core.RoleEditor), "write:factory.update", Resource{TeamID: "t1"}, true},
		{p(core.RoleEditor), ActionAdmin, Resource{}, false},
		{p(core.RoleAdmin), ActionAdmin, Resource{}, true},
		{p(core.RoleAdmin), ActionRead, Resource{TeamID: "t2"}, false},
		{p(core.RoleAdmin), "launch", Resource{}, false},
		{p("custom"), ActionRead, Resource{}, false},
		{nil, ActionRead, Resource{}, false},
	}
	for _, tc := range tests {
		err := RoleAuthorizer{}.Allow(ctx, tc.p, tc.a, tc.res)
		if (err == nil) != tc.allow {
			t.Errorf("%v %s %+v: err=%v, want allow=%v", tc.p, tc.a, tc.res, err, tc.allow)
		}
		if err != nil && tc.p != nil && !errors.Is(err, core.ErrForbidden) {
			t.Errorf("error does not wrap ErrForbidden: %v", err)
		}
	}
	for _, tc := range []struct {
		method string
		role   core.Role
		want   Action
	}{
		{"GET", core.RoleUser, ActionRead}, {"POST", core.RoleUser, ActionOperate},
		{"GET", core.RoleUser, ActionRead}, {"POST", core.RoleUser, ActionOperate},
		{"PUT", core.RoleEditor, ActionWrite}, {"DELETE", core.RoleAdmin, ActionAdmin},
	} {
		if got := ActionForRole(tc.method, tc.role); got != tc.want {
			t.Errorf("ActionForRole(%s,%s)=%s want %s", tc.method, tc.role, got, tc.want)
		}
	}
}

func TestSingleTeamResolverCreates(t *testing.T) {
	st := testdb.New(t)
	r := &SingleTeamResolver{Store: st, Create: true}
	t1, err := r.ResolveTeam(context.Background(), nil, Identity{})
	if err != nil || t1.Name != "Default" {
		t.Fatalf("%+v %v", t1, err)
	}
	t2, _ := r.ResolveTeam(context.Background(), nil, Identity{})
	if t2.ID != t1.ID {
		t.Fatal("second call created another team")
	}
	if _, err := (&SingleTeamResolver{Store: testdb.New(t)}).ResolveTeam(context.Background(), nil, Identity{}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("no-create: %v", err)
	}
}

func TestOIDCConfigFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cfg, on, err := OIDCConfigFromEnv(env(nil))
	if on || err != nil {
		t.Fatalf("disabled: %v %v", on, err)
	}
	cfg, on, err = OIDCConfigFromEnv(env(map[string]string{
		"OF_OIDC_ISSUER": "https://idp.test", "OF_OIDC_CLIENT_ID": "c", "OF_PUBLIC_URL": "https://of.test/",
		"OF_OIDC_SCOPES": "email, groups", "OF_OIDC_ALLOWED_DOMAINS": "@Acme.test,b.test",
		"OF_OIDC_ADMIN_GROUPS": "a1,a2", "OF_OIDC_DEFAULT_ROLE": "Editor", "OF_OIDC_REQUIRE_VERIFIED_EMAIL": "false",
	}))
	if !on || err != nil {
		t.Fatalf("enabled: %v %v", on, err)
	}
	if cfg.RedirectURL != "https://of.test/auth/oidc/callback" || strings.Join(cfg.Scopes, " ") != "openid email groups" ||
		strings.Join(cfg.AllowedDomains, ",") != "acme.test,b.test" || cfg.DefaultRole != core.RoleEditor ||
		cfg.GroupsClaim != "groups" || len(cfg.AdminGroups) != 2 || cfg.RequireVerifiedEmail || !cfg.RoleManaged() {
		t.Fatalf("cfg: %+v", cfg)
	}
	for name, m := range map[string]map[string]string{
		"no client":    {"OF_OIDC_ISSUER": "https://idp.test", "OF_PUBLIC_URL": "https://of.test"},
		"no redirect":  {"OF_OIDC_ISSUER": "https://idp.test", "OF_OIDC_CLIENT_ID": "c"},
		"bad role":     {"OF_OIDC_ISSUER": "https://idp.test", "OF_OIDC_CLIENT_ID": "c", "OF_PUBLIC_URL": "https://of.test", "OF_OIDC_DEFAULT_ROLE": "owner"},
		"bad bool":     {"OF_OIDC_ISSUER": "https://idp.test", "OF_OIDC_CLIENT_ID": "c", "OF_PUBLIC_URL": "https://of.test", "OF_OIDC_REQUIRE_VERIFIED_EMAIL": "maybe"},
		"bad redirect": {"OF_OIDC_ISSUER": "https://idp.test", "OF_OIDC_CLIENT_ID": "c", "OF_OIDC_REDIRECT_URL": "/cb"},
	} {
		if _, _, err := OIDCConfigFromEnv(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewValidation(t *testing.T) {
	st := testdb.New(t)
	good := make([]byte, SessionKeySize)
	if _, err := New(Config{Store: st, SessionKey: good[:16]}); err == nil {
		t.Error("short key accepted")
	}
	if _, err := New(Config{SessionKey: good}); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := New(Config{Store: st, SessionKey: good, PublicURL: "of.example"}); err == nil {
		t.Error("relative public URL accepted")
	}
	s, err := New(Config{Store: st, SessionKey: good, PublicURL: "http://localhost:8080"})
	if err != nil || s.secure {
		t.Errorf("http public URL: secure=%v err=%v", s != nil && s.secure, err)
	}
}

func withOverride(p *Principal, factoryID string, r core.Role) *Principal {
	u := *p.User
	u.FactoryRoles = map[string]core.Role{factoryID: r}
	p.User = &u
	return p
}

func TestRoleNames(t *testing.T) {
	if RoleRank(core.RoleUser) != 1 || RoleRank("guest") != 0 {
		t.Fatalf("rank of user = %d, of guest = %d", RoleRank(core.RoleUser), RoleRank("guest"))
	}
	if core.NormalizeRole(" User ") != core.RoleUser {
		t.Fatalf("NormalizeRole(User) = %q", core.NormalizeRole(" User "))
	}
	for _, in := range []core.Role{"", "user"} {
		c := OIDCConfig{Issuer: "https://idp.example.test", ClientID: "c", RedirectURL: "https://of.example.test/auth/oidc/callback", DefaultRole: in}
		if err := c.normalize(); err != nil {
			t.Fatalf("default role %q: %v", in, err)
		}
		if c.DefaultRole != core.RoleUser {
			t.Errorf("default role %q became %q, want user", in, c.DefaultRole)
		}
	}
	bad := OIDCConfig{Issuer: "https://idp.example.test", ClientID: "c", RedirectURL: "https://of.example.test/auth/oidc/callback", DefaultRole: "reader"}
	if err := bad.normalize(); err == nil {
		t.Error("unknown default role accepted")
	}
}
