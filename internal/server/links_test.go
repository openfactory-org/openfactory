package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/links"
	"github.com/openfactory-org/openfactory/internal/webui"
)

// TestDashboardLinksResolve is a regression test: every dashboard URL the
// REST API, the Factory MCP server, scoring and forge commit statuses emit
// must reach the dashboard catch-all (not a server-owned route) and match a
// client-side route of the React app (web/src/App.tsx).
func TestDashboardLinksResolve(t *testing.T) {
	cfg := testConfig(t)
	cfg.PublicURL = "https://factory.example.test"
	srv, err := New(t.Context(), cfg, WithLogger(quietLogger()), WithVersion("test"), WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	routes := appRoutes(t)

	emitted := map[string]string{
		"api factory url":         srv.API.FactoryURL("fac_1"),
		"api task url":            srv.API.WorkItemURL("fac_1", "wi_1"),
		"api run url":             srv.API.RunURL("run_1"),
		"mcp authorize url":       links.Integrations(cfg.PublicURL, "slack", "conn_1"),
		"mcp inbox route":         links.Inbox(cfg.PublicURL),
		"definition check status": links.Factory(cfg.PublicURL, "fac_1"),
		"factory settings":        links.FactorySettings(cfg.PublicURL, "fac_1"),
		"factory definition":      links.FactoryDefinition(cfg.PublicURL, "fac_1"),
		"factory webhook":         links.FactoryWebhook(cfg.PublicURL, "fac_1", "wh_1"),
	}
	for name, raw := range emitted {
		u, err := url.Parse(raw)
		if err != nil || !strings.HasPrefix(raw, cfg.PublicURL+"/") {
			t.Errorf("%s: %q is not under OF_PUBLIC_URL", name, raw)
			continue
		}
		req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
		if _, pattern := srv.mux.Handler(req); pattern != "/" || webui.IsReserved(u.Path) {
			t.Errorf("%s: %s is not served by the dashboard (matched %q)", name, u.Path, pattern)
			continue
		}
		if !matchesAppRoute(routes, u.Path) {
			t.Errorf("%s: %s matches no route of web/src/App.tsx", name, u.Path)
		}
	}
}

// appRoutes extracts the route patterns of web/src/App.tsx, expanding the
// routes nested under the factory layout (f/:fid).
func appRoutes(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "App.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`<Route (index|path="([^"]*)")`)
	var out []string
	prefix := ""
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "</Route>") {
			prefix = ""
			continue
		}
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		p := m[2]
		if m[1] == "index" {
			p = ""
		}
		full := "/" + strings.Trim(prefix+"/"+p, "/")
		out = append(out, full)
		if !strings.HasSuffix(strings.TrimSpace(line), "/>") && p != "" { // an opening tag: children are relative to it
			prefix = p
		}
	}
	if len(out) < 10 {
		t.Fatalf("parsed only %d routes from App.tsx: %v", len(out), out)
	}
	return out
}

func matchesAppRoute(routes []string, path string) bool {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, r := range routes {
		rs := strings.Split(strings.Trim(r, "/"), "/")
		if len(rs) != len(segs) || slices.Contains(rs, "*") {
			continue
		}
		ok := true
		for i := range rs {
			if !strings.HasPrefix(rs[i], ":") && rs[i] != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
