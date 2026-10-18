package mcpregistry

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runauth"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
	"github.com/openfactory-org/openfactory/internal/secrets"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

var ctx = context.Background()

type fixture struct {
	t     *testing.T
	store *sqlstore.Store
	sec   *secrets.Store
	svc   *Service
	cp    *httptest.Server // control plane serving the run endpoints
	now   time.Time
	keys  *runnertoken.StaticKeys
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testdb.New(t)
	k, _ := secrets.GenerateKey()
	mk, _ := secrets.LoadKey(k)
	keys, err := runnertoken.NewStaticKeys("k1", map[string][]byte{"k1": bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, store: st, sec: secrets.New(st, mk), keys: keys, now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	mux := http.NewServeMux()
	f.cp = httptest.NewServer(mux)
	t.Cleanup(f.cp.Close)
	f.svc = New(Options{
		Store: st, Secrets: f.sec, Encrypter: f.sec, PublicURL: f.cp.URL,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      func() time.Time { return f.now },
		Verifier: &runauth.Verifier{Keys: keys, Runner: fakeSessions{latest: 1}},
	})
	f.svc.Register(mux)
	return f
}

// token returns the bearer token of the runner of a run: a session token for
// the session 1.
func (f *fixture) token(runID string) string {
	f.t.Helper()
	tok, _, err := runnertoken.Mint(f.keys, runnertoken.Claims{Team: "team1", Run: runID, Session: 1, Subject: "runner"}, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

// authHeaders returns the Authorization header that the runner of a run sends.
func (f *fixture) authHeaders(runID string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + f.token(runID)}
}

func (f *fixture) register(m *core.RegisteredMCPServer) *core.RegisteredMCPServer {
	f.t.Helper()
	m.TeamID = "team1"
	if err := Validate(m); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.CreateMCPServer(ctx, m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) run(servers map[string]core.MCPServer) *core.Run {
	f.t.Helper()
	r := &core.Run{ID: core.NewID("run"), TeamID: "team1", FactoryID: "fac1", Config: core.AgentConfig{MCPServers: servers}}
	if err := f.store.CreateRun(ctx, r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestValidate(t *testing.T) {
	bad := []core.RegisteredMCPServer{
		{Name: "", URL: "https://a.test"},
		{Name: "builtin-linear", URL: "https://a.test"},
		{Name: "a", URL: "https://a.test", Command: "x"},
		{Name: "a"},
		{Name: "a", URL: "ftp://a.test"},
		{Name: "a", URL: "https://u:p@a.test"},
		{Name: "a", URL: "https://a.test", Env: map[string]string{"A": "1"}},
		{Name: "a", Command: "x", Headers: map[string]string{"A": "1"}},
		{Name: "a", Command: "x", Connection: "proxy"},
		{Name: "a", URL: "https://a.test/sse", Connection: "proxy"},
		{Name: "a", URL: "https://a.test", Headers: map[string]string{"X-Api-Key": "abc"}},
		{Name: "a", Command: "x", Env: map[string]string{"GITHUB_TOKEN": "ghp_x"}},
		{Name: "a", URL: "https://a.test", OAuth: &core.MCPOAuthConfig{TokenEndpoint: "http://idp.test/token"}},
		{Name: "a", URL: "https://a.test", OAuth: &core.MCPOAuthConfig{ClientSecretName: "S"}},
		{Name: "a", URL: "https://a.test", OAuth: &core.MCPOAuthConfig{Scopes: []string{"a b"}}},
	}
	for i := range bad {
		if err := Validate(&bad[i]); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("case %d (%+v): want invalid, got %v", i, bad[i], err)
		}
	}
	ok := core.RegisteredMCPServer{Name: "a", URL: "https://a.test/mcp", Headers: map[string]string{"Authorization": "Bearer {{TOKEN}}", "X-Team": "eng"},
		OAuth: &core.MCPOAuthConfig{TokenEndpoint: "http://127.0.0.1:9/token"}}
	if err := Validate(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok.Proxied() || ok.Connection != core.MCPConnectionProxy || ok.OAuth.Status != core.MCPOAuthNotConnected {
		t.Fatalf("oauth must force the proxy and default the status: %+v", ok)
	}
}

// TestConnection covers the connection mode: proxy is the default of a
// new url server, and a legacy SSE server becomes direct with a warning.
func TestConnection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      core.RegisteredMCPServer
		want    string
		warning bool
	}{
		{"default proxy", core.RegisteredMCPServer{Name: "a", URL: "https://a.test/mcp"}, core.MCPConnectionProxy, false},
		{"explicit direct", core.RegisteredMCPServer{Name: "a", URL: "https://a.test/mcp", Connection: "direct"}, core.MCPConnectionDirect, false},
		{"legacy sse", core.RegisteredMCPServer{Name: "a", URL: "https://a.test/sse"}, core.MCPConnectionDirect, true},
		{"command", core.RegisteredMCPServer{Name: "a", Command: "npx"}, "", false},
	} {
		m := tc.in
		w := DefaultConnection(&m)
		if err := Validate(&m); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if m.Connection != tc.want || m.Proxied() != (tc.want == core.MCPConnectionProxy) || (w != "") != tc.warning {
			t.Errorf("%s: connection=%q proxied=%v warning=%q", tc.name, m.Connection, m.Proxied(), w)
		}
	}
	for _, bad := range []core.RegisteredMCPServer{
		{Name: "a", URL: "https://a.test/mcp", Connection: "tunnel"},
		{Name: "a", URL: "https://a.test/sse", Connection: "proxy"},
		{Name: "a", URL: "https://a.test/mcp", Connection: "direct", OAuth: &core.MCPOAuthConfig{}},
		{Name: "a", Command: "npx", Connection: "proxy"},
	} {
		if err := Validate(&bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%+v: want invalid, got %v", bad, err)
		}
	}
}

// TestDefaultProxyKeepsHeaderOutOfSandbox checks that a new
// url server with a secret header reaches a run through the proxy, and the
// resolved server (the harness configuration) has no header value.
func TestDefaultProxyKeepsHeaderOutOfSandbox(t *testing.T) {
	f := newFixture(t)
	if _, err := f.sec.Put(ctx, "team1", "", "SENTRY_TOKEN", core.SecretGeneric, []byte("sentry-secret-value")); err != nil {
		t.Fatal(err)
	}
	var upstreamAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer upstream.Close()
	m := &core.RegisteredMCPServer{Name: "sentry", URL: upstream.URL + "/mcp", Headers: map[string]string{"Authorization": "Bearer {{SENTRY_TOKEN}}"}}
	DefaultConnection(m)
	srv := f.register(m)
	run := f.run(map[string]core.MCPServer{"sentry": {ManagedID: "sentry"}})
	got, err := f.svc.Resolve(ctx, run, "sentry")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(got)
	if got.URL != f.cp.URL+ProxyPathPrefix+run.ID+"/"+srv.ID || strings.Contains(string(cfg), "sentry-secret-value") || strings.Contains(string(cfg), "SENTRY_TOKEN") {
		t.Fatalf("resolved server: %s", cfg)
	}
	req, _ := http.NewRequest(http.MethodPost, got.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+f.token(run.ID))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK || upstreamAuth.Load() != "Bearer sentry-secret-value" {
		t.Fatalf("proxy: status %d, upstream auth %v", res.StatusCode, upstreamAuth.Load())
	}
}

func TestResolve(t *testing.T) {
	f := newFixture(t)
	direct := f.register(&core.RegisteredMCPServer{Name: "docs", Command: "npx", Args: []string{"-y", "docs"}, Env: map[string]string{"DOCS_TOKEN": "{{DOCS}}"}})
	proxied := f.register(&core.RegisteredMCPServer{Name: "sentry", URL: "https://mcp.sentry.test/mcp", Connection: core.MCPConnectionProxy, Headers: map[string]string{"Authorization": "Bearer {{S}}"}})
	oauth := f.register(&core.RegisteredMCPServer{Name: "notion", URL: "https://mcp.notion.test/mcp", OAuth: &core.MCPOAuthConfig{}})
	run := f.run(nil)

	got, err := f.svc.Resolve(ctx, run, "docs")
	if err != nil || got.Command != "npx" || got.Env["DOCS_TOKEN"] != "{{DOCS}}" || len(got.Args) != 2 {
		t.Fatalf("direct by name: %+v %v", got, err)
	}
	if got, err := f.svc.Resolve(ctx, run, direct.ID); err != nil || got.Command != "npx" {
		t.Fatalf("direct by id: %+v %v", got, err)
	}
	got, err = f.svc.Resolve(ctx, run, "sentry")
	if err != nil || got.URL != f.cp.URL+ProxyPathPrefix+run.ID+"/"+proxied.ID || len(got.Headers) != 0 {
		t.Fatalf("proxied: %+v %v", got, err)
	}
	if _, err := f.svc.Resolve(ctx, run, "notion"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("unconnected oauth: %v", err)
	}
	_ = oauth
	got, err = f.svc.Resolve(ctx, run, "builtin/linear")
	if err != nil || got.URL != f.cp.URL+IntegrationPathPrefix+"linear/"+run.ID || len(got.Headers) != 0 {
		t.Fatalf("builtin: %+v %v", got, err)
	}
	if _, err := f.svc.Resolve(ctx, run, "builtin/nope"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("unknown builtin: %v", err)
	}
	if _, err := f.svc.Resolve(ctx, run, "missing"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	other := &core.Run{ID: "run_x", TeamID: "team2"}
	if _, err := f.svc.Resolve(ctx, other, direct.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cross-team: %v", err)
	}
}

// fakeProvider is an MCP server protected by OAuth, with its authorization
// server (metadata, dynamic registration, token endpoint).
type fakeProvider struct {
	srv          *httptest.Server
	tokens       atomic.Int32
	refreshes    atomic.Int32
	lastAuth     atomic.Value // last Authorization header seen by the MCP endpoint
	lastResource atomic.Value
	expiresIn    int
}

func newProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{expiresIn: 3600}
	mux := http.NewServeMux()
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	base := p.srv.URL
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer at-") {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p.lastAuth.Store(auth + "|" + r.Header.Get("X-Team") + "|" + r.Header.Get("Cookie"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "sess-1")
		body, _ := io.ReadAll(r.Body)
		io.WriteString(w, "event: message\ndata: "+string(body)+"\n\n")
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"resource": base + "/mcp", "authorization_servers": []string{base + "/as"}, "scopes_supported": []string{"read", "write"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/as", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": base + "/as", "authorization_endpoint": base + "/as/authorize",
			"token_endpoint": base + "/as/token", "registration_endpoint": base + "/as/register", "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/as/register", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "dyn-client", "redirect_uris": req["redirect_uris"]})
	})
	mux.HandleFunc("/as/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		p.lastResource.Store(r.Form.Get("resource"))
		if r.Form.Get("client_id") != "dyn-client" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if r.Form.Get("code") != "the-code" || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
		case "refresh_token":
			if !strings.HasPrefix(r.Form.Get("refresh_token"), "rt-") {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			p.refreshes.Add(1)
		}
		n := p.tokens.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + string('0'+n), "token_type": "Bearer", "expires_in": p.expiresIn, "refresh_token": "rt-" + string('0'+n)})
	})
	return p
}

func TestOAuthConnectRefreshAndProxy(t *testing.T) {
	f := newFixture(t)
	p := newProvider(t)
	_, _ = f.sec.Put(ctx, "team1", "", "TEAM_HEADER", core.SecretGeneric, []byte("eng"))
	m := f.register(&core.RegisteredMCPServer{Name: "notion", URL: p.srv.URL + "/mcp", Headers: map[string]string{"X-Team": "{{TEAM_HEADER}}"}, OAuth: &core.MCPOAuthConfig{}})

	authURL, err := f.svc.StartOAuth(ctx, m, "usr1")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	q := u.Query()
	if u.Path != "/as/authorize" || q.Get("client_id") != "dyn-client" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != f.cp.URL+CallbackPath || q.Get("resource") != p.srv.URL+"/mcp" || q.Get("scope") != "read write" {
		t.Fatalf("authorization URL: %s", authURL)
	}
	state := q.Get("state")
	if raw, _ := f.store.KVGet(ctx, stateKey(state)); raw == "" || strings.Contains(raw, "verifier") || strings.Contains(raw, "dyn-client") {
		t.Fatalf("authorization state must be stored encrypted: %q", raw)
	}

	if _, err := f.svc.CompleteOAuth(ctx, "team1", "someone-else", state, "the-code", ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("other user: %v", err)
	}
	// State is single use: the failed attempt consumed it.
	if _, err := f.svc.CompleteOAuth(ctx, "team1", "usr1", state, "the-code", ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("reused state: %v", err)
	}
	authURL, _ = f.svc.StartOAuth(ctx, m, "usr1")
	u, _ = url.Parse(authURL)
	got, err := f.svc.CompleteOAuth(ctx, "team1", "usr1", u.Query().Get("state"), "the-code", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.OAuth.Status != core.MCPOAuthConnected || got.OAuth.ConnectedBy != "usr1" {
		t.Fatalf("status: %+v", got.OAuth)
	}
	if v, _ := p.lastResource.Load().(string); v != p.srv.URL+"/mcp" {
		t.Fatalf("token request resource = %q", v)
	}
	raw, _ := f.store.KVGet(ctx, tokenKey(m.ID))
	if raw == "" || strings.Contains(raw, "at-1") {
		t.Fatalf("token must be stored encrypted: %q", raw)
	}

	// The run resolves to the proxy and the proxy adds the token and headers.
	run := f.run(map[string]core.MCPServer{"notes": {ManagedID: "notion"}})
	srv, err := f.svc.Resolve(ctx, run, "notion")
	if err != nil {
		t.Fatal(err)
	}
	call := func(auth string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Authorization", auth)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Cookie", "leak=1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	res, body := call("Bearer " + f.token(run.ID))
	if res.StatusCode != 200 || !strings.Contains(body, `"method":"ping"`) || res.Header.Get("Mcp-Session-Id") != "sess-1" {
		t.Fatalf("proxy: %d %s %v", res.StatusCode, body, res.Header)
	}
	if v := p.lastAuth.Load(); v != "Bearer at-1|eng|" {
		t.Fatalf("upstream saw %q", v)
	}
	if res, _ := call("Bearer wrong"); res.StatusCode != 401 {
		t.Fatalf("bad run token: %d", res.StatusCode)
	}

	// A run that did not declare the server is refused.
	other := f.run(nil)
	req, _ := http.NewRequest(http.MethodPost, f.cp.URL+ProxyPathPrefix+other.ID+"/"+m.ID, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+f.token(other.ID))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatalf("ungranted run: %d", res.StatusCode)
	}

	// Near expiry, the token is refreshed once.
	f.now = f.now.Add(59*time.Minute + 30*time.Second)
	if _, _ = call("Bearer " + f.token(run.ID)); p.refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d", p.refreshes.Load())
	}
	if v := p.lastAuth.Load(); v != "Bearer at-2|eng|" {
		t.Fatalf("after refresh upstream saw %q", v)
	}
	if _, _ = call("Bearer " + f.token(run.ID)); p.refreshes.Load() != 1 {
		t.Fatalf("unexpected second refresh")
	}

	// Disconnect deletes the token; the proxy then reports not connected.
	if err := f.svc.Disconnect(ctx, got); err != nil {
		t.Fatal(err)
	}
	if res, body := call("Bearer " + f.token(run.ID)); res.StatusCode != http.StatusBadGateway || !strings.Contains(body, "not_connected") {
		t.Fatalf("after disconnect: %d %s", res.StatusCode, body)
	}
}

func TestOAuthNeedsClientWithoutRegistration(t *testing.T) {
	f := newFixture(t)
	mux := http.NewServeMux()
	up := httptest.NewServer(mux)
	defer up.Close()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"authorization_endpoint": "https://idp.test/authorize", "token_endpoint": "https://idp.test/token"})
	})
	m := f.register(&core.RegisteredMCPServer{Name: "x", URL: up.URL + "/mcp", OAuth: &core.MCPOAuthConfig{}})
	if _, err := f.svc.StartOAuth(ctx, m, "usr1"); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("want client_id hint, got %v", err)
	}
	_, _ = f.sec.Put(ctx, "team1", "", "X_CLIENT_SECRET", core.SecretGeneric, []byte("shh"))
	m.OAuth.ClientID, m.OAuth.ClientSecretName = "static", "X_CLIENT_SECRET"
	authURL, err := f.svc.StartOAuth(ctx, m, "usr1")
	if err != nil || !strings.HasPrefix(authURL, "https://idp.test/authorize?") || !strings.Contains(authURL, "client_id=static") {
		t.Fatalf("static client: %s %v", authURL, err)
	}
}

func TestBuiltinLinear(t *testing.T) {
	f := newFixture(t)
	var lastQuery atomic.Value
	gql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		lastQuery.Store(r.Header.Get("Authorization") + "|" + req.Query)
		switch {
		case strings.Contains(req.Query, "commentCreate"):
			io.WriteString(w, `{"data":{"commentCreate":{"success":true}}}`)
		default:
			io.WriteString(w, `{"data":{"issue":{"identifier":"ENG-1","title":"Broken login"}}}`)
		}
	}))
	defer gql.Close()
	f.svc.opts.LinearGraphQLURL = gql.URL
	_, _ = f.sec.Put(ctx, "team1", "", "LINEAR_TOKEN", core.SecretGeneric, []byte("lin_api_x"))
	_ = f.store.UpsertIntegration(ctx, &core.Integration{ID: "int1", TeamID: "team1", Provider: "linear", SecretRef: "LINEAR_TOKEN", Config: map[string]string{}})

	connect := func(run *core.Run) (*mcp.ClientSession, error) {
		srv, err := f.svc.Resolve(ctx, run, "builtin/linear")
		if err != nil {
			return nil, err
		}
		hc := &http.Client{Transport: headerRT{h: map[string]string{"Authorization": "Bearer " + f.token(run.ID)}}}
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		return c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc}, nil)
	}

	// The factory does not attach linear yet.
	ro := f.run(map[string]core.MCPServer{"linear": {ManagedID: "builtin/linear"}})
	if _, err := connect(ro); err == nil {
		t.Fatal("want refusal when the factory does not attach the integration")
	}
	if err := f.store.CreateFactory(ctx, &core.Factory{ID: "fac1", TeamID: "team1", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetFactoryIntegrations(ctx, "fac1", []string{"linear"}, nil); err != nil {
		t.Fatal(err)
	}

	sess, err := connect(ro)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if !names["linear_get_issue"] || !names["linear_search_issues"] || names["linear_create_comment"] {
		t.Fatalf("read-only tools: %v", names)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "linear_get_issue", Arguments: map[string]any{"id": "ENG-1"}})
	if err != nil || res.IsError {
		t.Fatalf("get issue: %+v %v", res, err)
	}
	if txt := res.Content[0].(*mcp.TextContent).Text; !strings.Contains(txt, "Broken login") {
		t.Fatalf("result: %s", txt)
	}
	if v, _ := lastQuery.Load().(string); !strings.HasPrefix(v, "lin_api_x|") {
		t.Fatalf("credential not used: %q", v)
	}

	rw := f.run(map[string]core.MCPServer{"linear": {ManagedID: "builtin/linear", AllowWrite: true}})
	sess2, err := connect(rw)
	if err != nil {
		t.Fatal(err)
	}
	defer sess2.Close()
	res, err = sess2.CallTool(ctx, &mcp.CallToolParams{Name: "linear_create_comment", Arguments: map[string]any{"issue_id": "ENG-1", "body": "Looking into it"}})
	if err != nil || res.IsError {
		t.Fatalf("comment: %+v %v", res, err)
	}

	// A run without the grant is refused even with a valid token.
	none := f.run(nil)
	req, _ := http.NewRequest(http.MethodPost, f.cp.URL+IntegrationPathPrefix+"linear/"+none.ID, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+f.token(none.ID))
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 403 {
		t.Fatalf("ungranted: %d", res.StatusCode)
	}
}

type headerRT struct{ h map[string]string }

func (rt headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range rt.h {
		r.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(r)
}

type fakeSessions struct {
	core.RunnerStore
	latest int
}

func (f fakeSessions) GetRunSession(_ context.Context, runID string, n int) (*core.RunSession, error) {
	return &core.RunSession{RunID: runID, Session: n, State: core.SessionActive}, nil
}

func (f fakeSessions) LatestRunSession(_ context.Context, runID string) (*core.RunSession, error) {
	return &core.RunSession{RunID: runID, Session: f.latest}, nil
}

func (f fakeSessions) TouchRunSession(context.Context, string, int, time.Time) error { return nil }

func TestSessionTokenOnRunEndpoints(t *testing.T) {
	keys, err := runnertoken.NewStaticKeys("k1", map[string][]byte{"k1": bytes.Repeat([]byte{5}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		latest int
		old    bool
		want   func(int) bool
		desc   string
	}{
		{"active session", 1, false, func(c int) bool { return c != 401 && c != 409 }, "not 401 or 409"},
		{"old run token", 1, true, func(c int) bool { return c == 401 }, "401: only a session token passes"},
		{"superseded session", 2, false, func(c int) bool { return c == 409 }, "409"},
	} {
		f := newFixture(t)
		r := f.run(nil)
		mux := http.NewServeMux()
		cp := httptest.NewServer(mux)
		svc := New(Options{
			Store: f.store, Secrets: f.sec, Encrypter: f.sec, PublicURL: cp.URL,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Verifier: &runauth.Verifier{Keys: keys, Runner: fakeSessions{latest: tt.latest}},
		})
		svc.Register(mux)
		tok, _, err := runnertoken.Mint(keys, runnertoken.Claims{Team: "team1", Run: r.ID, Session: 1, Subject: "runner"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if tt.old {
			m := hmac.New(sha256.New, []byte("server-key-0123456789abcdef"))
			m.Write([]byte("run:" + r.ID))
			tok = hex.EncodeToString(m.Sum(nil)) // the old run token
		}
		for _, path := range []string{ProxyPathPrefix + r.ID + "/nope", IntegrationPathPrefix + "linear/" + r.ID} {
			req, _ := http.NewRequest(http.MethodPost, cp.URL+path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+tok)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if !tt.want(res.StatusCode) {
				t.Errorf("%s %s: status %d, want %s", tt.name, path, res.StatusCode, tt.desc)
			}
		}
		cp.Close()
	}
}
