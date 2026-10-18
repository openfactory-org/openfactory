package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

type fakeMCPRegistry struct {
	started   []string
	forgotten []string
	complete  func(state, code string) (*core.RegisteredMCPServer, error)
}

func (f *fakeMCPRegistry) StartOAuth(_ context.Context, m *core.RegisteredMCPServer, userID string) (string, error) {
	f.started = append(f.started, m.ID+":"+userID)
	return "https://auth.example.test/authorize?state=s1", nil
}

func (f *fakeMCPRegistry) CompleteOAuth(_ context.Context, _, _, state, code, providerErr string) (*core.RegisteredMCPServer, error) {
	if providerErr != "" {
		return nil, errors.New("unexpected provider error")
	}
	return f.complete(state, code)
}

func (f *fakeMCPRegistry) Disconnect(_ context.Context, m *core.RegisteredMCPServer) error {
	m.OAuth.Status = core.MCPOAuthNotConnected
	return nil
}

func (f *fakeMCPRegistry) Forget(_ context.Context, id string) { f.forgotten = append(f.forgotten, id) }

func TestMCPServersCRUD(t *testing.T) {
	reg := &fakeMCPRegistry{}
	e := newTestEnv(t, func(o *Options) { o.MCPRegistry = reg })

	// Validation.
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "x"}), 400)
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "x", "url": "https://a.test/mcp", "command": "x"}), 400)
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "builtin-x", "url": "https://a.test/mcp"}), 400)
	lit := e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "x", "url": "https://a.test/mcp", "headers": map[string]string{"Authorization": "Bearer abc"}}), 400)
	if !strings.Contains(string(lit.Raw), "{{SECRET_NAME}}") {
		t.Fatalf("literal credential message: %s", lit.Raw)
	}
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "x", "command": "npx", "connection": "proxy"}), 400)
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "x", "url": "https://a.test/sse", "connection": "proxy"}), 400)
	e.must(e.do("POST", "/mcp-servers", e.viewer.Key, map[string]any{"name": "x", "url": "https://a.test/mcp"}), 403)

	c := e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{
		"name": "sentry", "url": "https://mcp.sentry.test/mcp",
		"headers": map[string]string{"Authorization": "Bearer {{SENTRY_TOKEN}}"},
	}), 201)
	id := str(c.Body, "id")
	// A url server uses the control-plane proxy by default.
	if c.Body["connection"] != core.MCPConnectionProxy || c.Body["created_by"] == "" {
		t.Fatal(c.Body)
	}
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "y", "url": "https://a.test/mcp", "connection": "sideways"}), 400)
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "y", "url": "https://a.test/mcp", "connection": "direct", "oauth": map[string]any{}}), 400)
	d := e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "private", "url": "http://10.0.0.5/mcp", "connection": "direct"}), 201)
	if d.Body["connection"] != core.MCPConnectionDirect || d.Body["warnings"] != nil {
		t.Fatal(d.Body)
	}
	// A legacy SSE server cannot use the proxy: it is registered direct, with a warning.
	sse := e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "legacy", "url": "https://c.test/sse"}), 201)
	if ws, _ := sse.Body["warnings"].([]any); sse.Body["connection"] != core.MCPConnectionDirect || len(ws) != 1 {
		t.Fatal(sse.Body)
	}
	if g := e.must(e.do("GET", "/mcp-servers/legacy", e.viewer.Key, nil), 200); g.Body["warnings"] != nil {
		t.Fatal(g.Body)
	}
	for _, n := range []string{"private", "legacy"} {
		e.must(e.do("DELETE", "/mcp-servers/"+n, e.editor.Key, nil), 204)
	}
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "sentry", "command": "x"}), 409)

	// OAuth forces the proxy on and starts not connected.
	o := e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{
		"name": "notion", "url": "https://mcp.notion.test/mcp", "oauth": map[string]any{"scopes": []string{"read"}},
	}), 201)
	oid := str(o.Body, "id")
	oa, _ := o.Body["oauth"].(map[string]any)
	if o.Body["connection"] != core.MCPConnectionProxy || oa["status"] != core.MCPOAuthNotConnected {
		t.Fatal(o.Body)
	}

	// Get by id and by name; other teams cannot see it.
	e.must(e.do("GET", "/mcp-servers/"+id, e.viewer.Key, nil), 200)
	if g := e.must(e.do("GET", "/mcp-servers/sentry", e.viewer.Key, nil), 200); g.Body["id"] != id {
		t.Fatal(g.Body)
	}
	e.must(e.do("GET", "/mcp-servers/"+id, e.other.Key, nil), 404)

	l := e.must(e.do("GET", "/mcp-servers", e.viewer.Key, nil), 200)
	if len(list(l.Body, "mcp_servers")) != 2 || len(list(l.Body, "builtin")) != len(core.BuiltinMCPProviders) {
		t.Fatal(l.Body)
	}

	// Patch: switch transports.
	u := e.must(e.do("PATCH", "/mcp-servers/"+id, e.editor.Key, map[string]any{"url": "", "headers": map[string]string{}, "command": "npx", "args": []string{"-y", "sentry-mcp"}}), 200)
	if u.Body["command"] != "npx" || u.Body["url"] != nil || u.Body["connection"] != nil {
		t.Fatal(u.Body)
	}

	// OAuth connect and callback.
	cn := e.must(e.do("POST", "/mcp-servers/"+oid+"/oauth/connect", e.editor.Key, nil), 200)
	if !strings.HasPrefix(str(cn.Body, "authorization_url"), "https://auth.example.test/") || len(reg.started) != 1 {
		t.Fatal(cn.Body, reg.started)
	}
	e.must(e.do("POST", "/mcp-servers/"+id+"/oauth/connect", e.editor.Key, nil), 400) // no oauth
	reg.complete = func(state, code string) (*core.RegisteredMCPServer, error) {
		if state != "s1" || code != "c1" {
			return nil, core.ErrInvalid
		}
		m, err := e.store.GetMCPServer(context.Background(), oid)
		if err != nil {
			return nil, err
		}
		m.OAuth.Status = core.MCPOAuthConnected
		return m, e.store.UpdateMCPServer(context.Background(), m)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+BasePath+"/mcp-servers/oauth/callback?state=s1&code=c1", nil)
	req.Header.Set("Authorization", "Bearer "+e.editor.Key)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusFound || loc.Path != "/mcp-servers/"+oid || loc.Query().Get("oauth") != "connected" {
		t.Fatalf("callback: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// Changing the OAuth client while connected disconnects and forgets the token.
	p := e.must(e.do("PATCH", "/mcp-servers/"+oid, e.editor.Key, map[string]any{"oauth": map[string]any{"client_id": "abc"}}), 200)
	if oa, _ := p.Body["oauth"].(map[string]any); oa["status"] != core.MCPOAuthNotConnected || reg.forgotten[len(reg.forgotten)-1] != oid {
		t.Fatal(p.Body, reg.forgotten)
	}
	e.must(e.do("PATCH", "/mcp-servers/"+oid, e.editor.Key, map[string]any{"oauth": map[string]any{"bogus": 1}}), 400)
	if p := e.must(e.do("PATCH", "/mcp-servers/"+oid, e.editor.Key, map[string]any{"oauth": nil, "connection": "direct"}), 200); p.Body["oauth"] != nil || p.Body["connection"] != core.MCPConnectionDirect {
		t.Fatal(p.Body)
	}

	e.must(e.do("DELETE", "/mcp-servers/"+id, e.editor.Key, nil), 204)
	e.must(e.do("GET", "/mcp-servers/"+id, e.viewer.Key, nil), 404)
	if reg.forgotten[len(reg.forgotten)-1] != id {
		t.Fatal(reg.forgotten)
	}
}

func TestMCPServersOAuthUnavailable(t *testing.T) {
	e := newTestEnv(t)
	o := e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "n", "url": "https://mcp.notion.test/mcp", "oauth": map[string]any{}}), 201)
	e.must(e.do("POST", "/mcp-servers/"+str(o.Body, "id")+"/oauth/connect", e.editor.Key, nil), 400)
}

func TestRunMCPServerAllowWriteValidation(t *testing.T) {
	cases := map[string]core.MCPServer{
		"unknown builtin":      {ManagedID: "builtin/nope"},
		"allowWrite non-built": {ManagedID: "sentry", AllowWrite: true},
	}
	for name, srv := range cases {
		if err := ValidateMCPServers(map[string]core.MCPServer{"a": srv}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := ValidateMCPServers(map[string]core.MCPServer{"a": {ManagedID: "builtin/linear", AllowWrite: true}}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckManagedMCP(t *testing.T) {
	e := newTestEnv(t)
	e.must(e.do("POST", "/mcp-servers", e.editor.Key, map[string]any{"name": "sentry", "url": "https://mcp.sentry.test/mcp"}), 201)
	shared := map[string]core.MCPServer{"docs": {ManagedID: "docs-mcp"}}
	def := &core.CompiledDefinition{
		MCPServers: shared,
		Agents: []core.AgentConfig{
			{Name: "foreman", MCPServers: map[string]core.MCPServer{"docs": {ManagedID: "docs-mcp"}, "errors": {ManagedID: "sentry"}, "linear": {ManagedID: "builtin/linear"}}},
			{Name: "review", MCPServers: map[string]core.MCPServer{"docs": {ManagedID: "docs-mcp"}, "pager": {ManagedID: "pagerduty"}}},
		},
	}
	err := e.srv.checkManagedMCP(context.Background(), e.team.ID, def)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("want validation error, got %v", err)
	}
	verrs, _ := apiErr.Extra["errors"].([]core.ValidationError)
	if len(verrs) != 2 || verrs[0].File != "agent review" || verrs[1].File != "factory.yaml" {
		t.Fatalf("errors = %+v", verrs)
	}
	if err := e.srv.checkManagedMCP(context.Background(), e.team.ID, &core.CompiledDefinition{Agents: []core.AgentConfig{{Name: "a", MCPServers: map[string]core.MCPServer{"e": {ManagedID: "sentry"}}}}}); err != nil {
		t.Fatal(err)
	}
}
