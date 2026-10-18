package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// fakeProver is the sign-in of a provider. Each code names the account
// that signed in.
type fakeProver struct{ accounts map[string]string }

func (p *fakeProver) AuthURL(state, redirectURI string) string {
	return "https://provider.test/authorize?" + url.Values{"state": {state}, "redirect_uri": {redirectURI}}.Encode()
}

func (p *fakeProver) Exchange(_ context.Context, code, _ string) (string, string, error) {
	id, ok := p.accounts[code]
	if !ok {
		return "", "", errors.New("bad code")
	}
	return id, "handle-" + id, nil
}

type linkEnv struct {
	t       *testing.T
	store   *forgetest.Store
	l       *Linker
	mux     *http.ServeMux
	current *core.User
	prover  Prover
	mu      sync.Mutex
	audits  []AuditRecord
}

func newLinkEnv(t *testing.T) *linkEnv {
	e := &linkEnv{t: t, store: forgetest.NewStore()}
	e.store.Users = []core.User{
		{ID: "usr_1", TeamID: "team_1", Email: "alice@x.com", Role: "user"},
		{ID: "usr_2", TeamID: "team_2", Email: "eve@y.com"},
		{ID: "usr_3", TeamID: "team_1", Email: "mallory@x.com", Role: "user"},
		{ID: "usr_4", TeamID: "team_1", Email: "admin@x.com", Role: "admin"},
	}
	e.l = &Linker{Store: e.store, Key: []byte("secret"), PublicURL: "https://of.example.com/",
		CurrentUser: func(*http.Request) (*core.User, error) {
			if e.current == nil {
				return nil, errors.New("anonymous")
			}
			return e.current, nil
		},
		Prover: func(context.Context, string, string) (Prover, error) { return e.prover, nil },
		Audit: func(_ context.Context, rec AuditRecord) {
			e.mu.Lock()
			e.audits = append(e.audits, rec)
			e.mu.Unlock()
		},
	}
	e.mux = http.NewServeMux()
	e.l.Register(e.mux)
	return e
}

func (e *linkEnv) as(id string) {
	e.current = nil
	for i := range e.store.Users {
		if e.store.Users[i].ID == id {
			e.current = &e.store.Users[i]
		}
	}
}

func (e *linkEnv) do(method, target string, form url.Values) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *linkEnv) linkedTo(provider, externalID string) string {
	x, err := e.store.FindExternalIdentity(context.Background(), provider, externalID)
	if err != nil {
		return ""
	}
	return x.UserID
}

func (e *linkEnv) audited(action, outcome, method string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.audits {
		if a.Action == action && a.Outcome == outcome && (method == "" || a.Method == method) {
			return true
		}
	}
	return false
}

func pathOf(u string) string { return strings.TrimPrefix(u, "https://of.example.com") }

// TestLinkPrivateURLNeedsConfirmation covers a provider without a sign-in:
// only a URL from a private message links, and only after a POST with the
// CSRF token. A GET never links.
func TestLinkPrivateURLNeedsConfirmation(t *testing.T) {
	e := newLinkEnv(t)
	path := pathOf(e.l.PrivateURL("team_1", "slack", "U1", "alice"))
	if !strings.HasPrefix(path, LinkPath) {
		t.Fatalf("url path %q", path)
	}
	if rec := e.do("GET", path, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	e.as("usr_2")
	if rec := e.do("GET", path, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("other team: %d", rec.Code)
	}
	if rec := e.do("GET", path[:len(path)-2]+"xx", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("tampered: %d", rec.Code)
	}
	e.as("usr_1")
	rec := e.do("GET", path, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alice") || !strings.Contains(rec.Body.String(), `method="post"`) {
		t.Fatalf("confirmation page: %d %s", rec.Code, rec.Body)
	}
	if e.linkedTo("slack", "U1") != "" {
		t.Fatal("a GET linked the account")
	}
	if rec := e.do("POST", path, url.Values{"csrf": {"wrong"}}); rec.Code != http.StatusForbidden || e.linkedTo("slack", "U1") != "" {
		t.Fatalf("bad csrf: %d", rec.Code)
	}
	// The CSRF token of another user does not work.
	if rec := e.do("POST", path, url.Values{"csrf": {e.l.csrf("usr_3", path[len(LinkPath):])}}); rec.Code != http.StatusForbidden {
		t.Fatalf("csrf of another user: %d", rec.Code)
	}
	if rec := e.do("POST", path, url.Values{"csrf": {e.l.csrf("usr_1", path[len(LinkPath):])}}); rec.Code != http.StatusOK {
		t.Fatalf("complete: %d %s", rec.Code, rec.Body)
	}
	if e.linkedTo("slack", "U1") != "usr_1" || !e.audited(AuditLink, "success", "private_message") {
		t.Fatal("not linked or not audited")
	}
	// A URL that was not made for a private message cannot complete a link
	// without the sign-in of the provider.
	pub := e.l.seal("link", linkClaims{TeamID: "team_1", Provider: "jira", ExternalID: "J1", Expires: time.Now().Add(time.Hour).Unix()})
	if rec := e.do("GET", LinkPath+pub, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("public url, confirm: %d", rec.Code)
	}
	if rec := e.do("POST", LinkPath+pub, url.Values{"csrf": {e.l.csrf("usr_1", pub)}}); rec.Code != http.StatusForbidden || e.linkedTo("jira", "J1") != "" {
		t.Fatalf("public url, complete: %d", rec.Code)
	}
	// An expired link is refused.
	e.l.Now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	old := pathOf(e.l.PrivateURL("team_1", "slack", "U7", ""))
	e.l.Now = nil
	if rec := e.do("GET", old, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("expired: %d", rec.Code)
	}
}

// signIn completes the sign-in of the provider after a POST and returns the
// response of the callback.
func (e *linkEnv) signIn(start *httptest.ResponseRecorder, code string) *httptest.ResponseRecorder {
	e.t.Helper()
	if start.Code != http.StatusSeeOther {
		e.t.Fatalf("start of the sign-in: %d %s", start.Code, start.Body)
	}
	loc, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		e.t.Fatal(err)
	}
	state := loc.Query().Get("state")
	return e.do("GET", LinkCallbackPath+"?"+url.Values{"state": {state}, "code": {code}}.Encode(), nil)
}

// TestLinkURLOpenedByOtherUserNeedsProof checks that a
// link URL that a different user opens does not bind the external account
// without a proof from the provider.
func TestLinkURLOpenedByOtherUserNeedsProof(t *testing.T) {
	e := newLinkEnv(t)
	e.prover = &fakeProver{accounts: map[string]string{"code-u1": "U1", "code-u9": "U9"}}
	// The link prompt was for U1. Mallory, a member of the team, opens it.
	path := pathOf(e.l.PrivateURL("team_1", "github", "U1", "alice"))
	tok := path[len(LinkPath):]
	e.as("usr_3")
	if rec := e.do("GET", path, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Continue with GitHub") {
		t.Fatalf("confirmation: %d %s", rec.Code, rec.Body)
	}
	// Mallory signs in to the provider with her own account U9.
	start := e.do("POST", path, url.Values{"csrf": {e.l.csrf("usr_3", tok)}})
	if rec := e.signIn(start, "code-u9"); rec.Code != http.StatusForbidden {
		t.Fatalf("sign-in with another account: %d %s", rec.Code, rec.Body)
	}
	if e.linkedTo("github", "U1") != "" || e.linkedTo("github", "U9") != "" {
		t.Fatal("an account was linked without a matching proof")
	}
	if !e.audited(AuditLink, "failure", "provider_sign_in") {
		t.Fatal("the failure was not audited")
	}
	// The state of Mallory's sign-in does not work for another signed-in user.
	start = e.do("POST", path, url.Values{"csrf": {e.l.csrf("usr_3", tok)}})
	e.as("usr_1")
	if rec := e.signIn(start, "code-u1"); rec.Code != http.StatusForbidden {
		t.Fatalf("state of another user: %d", rec.Code)
	}
	// Alice proves U1 and gets the link.
	start = e.do("POST", path, url.Values{"csrf": {e.l.csrf("usr_1", tok)}})
	if rec := e.signIn(start, "code-u1"); rec.Code != http.StatusOK {
		t.Fatalf("sign-in with the account: %d %s", rec.Code, rec.Body)
	}
	if e.linkedTo("github", "U1") != "usr_1" || !e.audited(AuditLink, "success", "provider_sign_in") {
		t.Fatal("not linked after the proof")
	}
	// A tampered state is refused.
	if rec := e.do("GET", LinkCallbackPath+"?state=abc.def&code=code-u1", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("tampered state: %d", rec.Code)
	}
}

// TestLinkFromDashboard covers a link that the signed-in user starts from
// the dashboard: the account is the one the provider proves.
func TestLinkFromDashboard(t *testing.T) {
	e := newLinkEnv(t)
	e.as("usr_1")
	if rec := e.do("GET", LinkStartPath+"github", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no sign-in configured: %d", rec.Code)
	}
	e.prover = &fakeProver{accounts: map[string]string{"c": "1001"}}
	start := e.do("GET", LinkStartPath+"github", nil)
	if rec := e.signIn(start, "c"); rec.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	x, _ := e.store.FindExternalIdentity(context.Background(), "github", "1001")
	if x == nil || x.UserID != "usr_1" || x.Handle != "handle-1001" {
		t.Fatalf("identity %+v", x)
	}
}

// TestSecondLinkFails checks that a second link
// for the same external account fails and names the owner. An admin removes
// the first link.
func TestSecondLinkFails(t *testing.T) {
	e := newLinkEnv(t)
	e.store.Identities["slack|U1"] = core.ExternalIdentity{UserID: "usr_1", Provider: "slack", ExternalID: "U1"}
	path := pathOf(e.l.PrivateURL("team_1", "slack", "U1", "alice"))
	tok := path[len(LinkPath):]
	e.as("usr_3")
	if rec := e.do("GET", path, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "alice@x.com") {
		t.Fatalf("confirmation of a linked account: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", path, url.Values{"csrf": {e.l.csrf("usr_3", tok)}}); rec.Code != http.StatusConflict {
		t.Fatalf("second link: %d", rec.Code)
	}
	if e.linkedTo("slack", "U1") != "usr_1" {
		t.Fatal("the first link was replaced")
	}
	if !e.audited(AuditLink, "failure", "private_message") {
		t.Fatal("the failure was not audited")
	}
	// The email link does not replace a link either.
	if u, err := e.l.LinkByEmail(context.Background(), e.store, "team_1", EmailCandidate{Provider: "slack", ExternalID: "U1", Email: "mallory@x.com"}); u != nil || err != nil {
		t.Fatalf("email link over a link: %v %v", u, err)
	}
	if e.linkedTo("slack", "U1") != "usr_1" {
		t.Fatal("the email link replaced the link")
	}

	// Only an admin of the team removes a link.
	if rec := e.do("DELETE", LinksPath+"slack/U1", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("remove by a user: %d", rec.Code)
	}
	e.as("usr_4")
	if rec := e.do("DELETE", LinksPath+"slack/U1", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove by an admin: %d %s", rec.Code, rec.Body)
	}
	if e.linkedTo("slack", "U1") != "" || !e.audited(AuditUnlink, "success", "admin") {
		t.Fatal("not removed or not audited")
	}
	if rec := e.do("DELETE", LinksPath+"slack/U1", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("remove again: %d", rec.Code)
	}
	// Now the second user can link the account.
	e.as("usr_3")
	if rec := e.do("POST", path, url.Values{"csrf": {e.l.csrf("usr_3", tok)}}); rec.Code != http.StatusOK || e.linkedTo("slack", "U1") != "usr_3" {
		t.Fatalf("link after the removal: %d", rec.Code)
	}
}

func TestMemberAndUnlink(t *testing.T) {
	e := newLinkEnv(t)
	ctx := context.Background()
	e.store.Identities["slack|U1"] = core.ExternalIdentity{UserID: "usr_1", Provider: "slack", ExternalID: "U1"}
	if m, err := Member(ctx, e.store, "team_1", "slack", "U1"); err != nil || m == nil || m.ID != "usr_1" {
		t.Fatalf("member = %v %v", m, err)
	}
	if m, _ := Member(ctx, e.store, "team_2", "slack", "U1"); m != nil {
		t.Fatal("member of the wrong team")
	}
	if err := e.l.Unlink(ctx, e.store, "slack", "U1", ""); err != nil {
		t.Fatal(err)
	}
	if m, _ := Member(ctx, e.store, "team_1", "slack", "U1"); m != nil {
		t.Fatal("still linked")
	}
	if !e.audited(AuditUnlink, "success", "self") {
		t.Fatal("unlink not audited")
	}
	if h := e.l.DashboardHint("jira"); strings.Contains(h, LinkPath) || !strings.Contains(h, "https://of.example.com/integrations") {
		t.Fatalf("hint %q", h)
	}
}

func TestProverFromIntegration(t *testing.T) {
	ctx := context.Background()
	secrets := forgetest.NewSecrets(map[string]string{"GH_OAUTH": "s3cret"})
	if p, err := ProverFromIntegration(ctx, secrets, core.Integration{Provider: "github", Config: map[string]string{}}, nil); p != nil || err != nil {
		t.Fatalf("no client id: %v %v", p, err)
	}
	// A fake provider: token and user-info endpoints.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			_ = r.ParseForm()
			if r.PostForm.Get("client_secret") != "s3cret" || r.PostForm.Get("code") != "abc" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"gho_x"}`))
		case "/api/user":
			if r.Header.Get("Authorization") != "Bearer gho_x" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"id":1001,"login":"alice"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	in := core.Integration{ID: "int_1", TeamID: "team_1", Provider: "github", Config: map[string]string{
		ConfigOAuthClientID: "Iv1.abc", ConfigOAuthClientSecretRef: "GH_OAUTH", "web_url": srv.URL, "base_url": srv.URL + "/api"}}
	p, err := ProverFromIntegration(ctx, secrets, in, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if u := p.AuthURL("st", "https://of.example.com/cb"); !strings.HasPrefix(u, srv.URL+"/login/oauth/authorize?") || !strings.Contains(u, "client_id=Iv1.abc") || !strings.Contains(u, "state=st") {
		t.Fatalf("auth url %s", u)
	}
	id, handle, err := p.Exchange(ctx, "abc", "https://of.example.com/cb")
	if err != nil || id != "1001" || handle != "alice" {
		t.Fatalf("exchange = %q %q %v", id, handle, err)
	}
	if _, _, err := p.Exchange(ctx, "bad", "x"); err == nil {
		t.Fatal("want an error for a bad code")
	}
	// Another provider needs its endpoints.
	if _, err := ProverFromIntegration(ctx, secrets, core.Integration{Provider: "gitlab", Config: map[string]string{ConfigOAuthClientID: "x", ConfigOAuthClientSecretRef: "GH_OAUTH"}}, nil); err == nil {
		t.Fatal("want an error without endpoints")
	}
}
