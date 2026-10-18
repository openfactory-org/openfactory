package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(`<!doctype html><div id="root"></div>`)},
		"favicon.svg":          {Data: []byte(`<svg/>`)},
		"assets/index-abc.js":  {Data: []byte(`console.log(1)`)},
		"assets/index-abc.css": {Data: []byte(`body{}`)},
	}
}

func TestHandlerRouting(t *testing.T) {
	h := NewHandler(testFS())
	cases := []struct {
		name, method, path string
		status             int
		body               string // substring
		cache              string // Cache-Control
		ctype              string // Content-Type prefix
	}{
		{"root serves shell", "GET", "/", 200, `id="root"`, "no-cache", "text/html"},
		{"client route falls back", "GET", "/f/fac_123/runs", 200, `id="root"`, "no-cache", "text/html"},
		{"deep client route", "GET", "/f/fac_1/definition/versions/def_2", 200, `id="root"`, "no-cache", "text/html"},
		{"run route", "GET", "/runs/run_abc", 200, `id="root"`, "no-cache", "text/html"},
		{"head on route", "HEAD", "/inbox", 200, "", "no-cache", "text/html"},
		{"hashed asset immutable", "GET", "/assets/index-abc.js", 200, "console.log", "public, max-age=31536000, immutable", "text/javascript"},
		{"css asset", "GET", "/assets/index-abc.css", 200, "body{}", "public, max-age=31536000, immutable", "text/css"},
		{"root file", "GET", "/favicon.svg", 200, "<svg/>", "public, max-age=3600", "image/svg+xml"},
		{"missing asset 404", "GET", "/assets/missing.js", 404, "", "", ""},
		{"missing static file 404", "GET", "/robots.txt", 404, "", "", ""},
		{"index.html direct", "GET", "/index.html", 200, `id="root"`, "no-cache", "text/html"},
		{"post rejected", "POST", "/factories", 405, "", "", ""},
		{"path traversal stays in fs", "GET", "/../../etc/passwd", 200, `id="root"`, "no-cache", "text/html"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.body != "" && !strings.Contains(rec.Body.String(), tc.body) {
				t.Errorf("body = %q, want substring %q", rec.Body.String(), tc.body)
			}
			if tc.cache != "" && rec.Header().Get("Cache-Control") != tc.cache {
				t.Errorf("Cache-Control = %q, want %q", rec.Header().Get("Cache-Control"), tc.cache)
			}
			if tc.ctype != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tc.ctype) {
				t.Errorf("Content-Type = %q, want prefix %q", rec.Header().Get("Content-Type"), tc.ctype)
			}
		})
	}
}

func TestIndexHeaders(t *testing.T) {
	h := NewHandler(testFS())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/factories", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	req := httptest.NewRequest("GET", "/factories", nil)
	req.Header.Set("If-None-Match", etag)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", rec.Code)
	}
}

func TestReservedPrefixesNeverShadowed(t *testing.T) {
	h := NewHandler(testFS())
	for _, p := range []string{
		"/api", "/api/v1/unknown", "/auth/oidc/login", "/mcp", "/mcp/sse", "/webhooks/w/wh_1",
		"/plugins/x", "/objects/a/b", "/healthz", "/readyz", "/metrics", "/.well-known/openid-configuration",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "root") {
			t.Errorf("%s = %d %q, want plain 404", p, rec.Code, rec.Body.String())
		}
	}
	for _, p := range []string{"/apis", "/mcpx", "/metrics-dashboard", "/authors", "/webhooks"} {
		if p == "/webhooks" {
			if !IsReserved(p) {
				t.Errorf("IsReserved(%q) = false", p)
			}
			continue
		}
		if IsReserved(p) {
			t.Errorf("IsReserved(%q) = true, want false", p)
		}
	}
}

func TestRegisterYieldsToSpecificRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("api")) })
	mux.HandleFunc("/webhooks/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("hook")) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("/", NewHandler(testFS())) // as Register does, with a test build
	for path, want := range map[string]string{
		"/api/v1/me":      "api",
		"/webhooks/w/abc": "hook",
		"/healthz":        "ok",
		"/f/fac_1":        `id="root"`,
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s -> %q, want %q", path, rec.Body.String(), want)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown API path = %d, want 404", rec.Code)
	}
}

func TestEmbeddedBuild(t *testing.T) {
	if !Available() {
		t.Fatal("embedded dist has no index.html: run `npm run build` in web/")
	}
	b, err := fs.ReadFile(Dist(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `id="root"`) || !strings.Contains(string(b), "/assets/") {
		t.Fatalf("unexpected index.html: %s", b)
	}
	mux := http.NewServeMux()
	Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/inbox", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="root"`) {
		t.Fatalf("embedded /inbox = %d", rec.Code)
	}
}

func TestMissingBuild(t *testing.T) {
	h := NewHandler(fstest.MapFS{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
}
