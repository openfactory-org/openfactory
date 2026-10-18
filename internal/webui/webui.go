// Package webui serves the Open Factory dashboard: the React single-page app
// in web/, built by `npm run build` into internal/webui/dist and embedded in
// the binary so `go build` needs no Node toolchain.
//
// Mount it last, as the catch-all:
//
//	webui.Register(mux) // registers "/" without a method
//
// Hashed build assets (/assets/*) are served with a one-year immutable cache;
// index.html and other root files are revalidated on every load. Any other GET
// that is not a file falls back to index.html so client-side routes such as
// /f/{id}/runs work on reload. Server-owned prefixes (/api/, /auth/, /mcp,
// /webhooks/, /plugins/, /objects/, /healthz, /readyz, /metrics,
// /.well-known/) are never answered with the app: unmatched requests there get
// a plain 404.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed dist
var distFS embed.FS

// reservedPrefixes are server-owned paths the SPA fallback never shadows. A
// prefix ending in "/" matches that directory; others match the exact path
// and anything below it.
var reservedPrefixes = []string{
	"/api/", "/auth/", "/mcp", "/webhooks/", "/plugins/", "/objects/",
	"/healthz", "/readyz", "/metrics", "/.well-known/",
}

// staticExt are extensions that always name a file: a missing one is a 404,
// never the app shell.
var staticExt = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".map": true, ".png": true, ".jpg": true,
	".jpeg": true, ".gif": true, ".svg": true, ".ico": true, ".webp": true, ".woff": true,
	".woff2": true, ".ttf": true, ".json": true, ".txt": true, ".webmanifest": true,
}

// IsReserved reports whether p belongs to a server-owned route prefix.
func IsReserved(p string) bool {
	for _, pre := range reservedPrefixes {
		if strings.HasSuffix(pre, "/") {
			if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
				return true
			}
			continue
		}
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// Dist returns the embedded production build (rooted at dist/).
func Dist() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err) // the embed pattern guarantees dist exists
	}
	return sub
}

// Available reports whether the embedded build contains the app shell.
func Available() bool {
	_, err := fs.Stat(Dist(), "index.html")
	return err == nil
}

// Handler serves the embedded dashboard build.
func Handler() http.Handler { return NewHandler(Dist()) }

// Register mounts the dashboard as the catch-all "/" pattern on mux. More
// specific patterns registered by the API, auth, MCP and webhook handlers
// take precedence.
func Register(mux *http.ServeMux) { mux.Handle("/", Handler()) }

type handler struct {
	fsys      fs.FS
	index     []byte
	indexETag string
	modTime   time.Time
}

// NewHandler serves a dashboard build from fsys (its root holds index.html).
func NewHandler(fsys fs.FS) http.Handler {
	h := &handler{fsys: fsys, modTime: time.Now()}
	if b, err := fs.ReadFile(fsys, "index.html"); err == nil {
		h.index = b
		sum := sha256.Sum256(b)
		h.indexETag = `"` + hex.EncodeToString(sum[:8]) + `"`
	}
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if IsReserved(p) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := path.Clean(p)
	name := strings.TrimPrefix(clean, "/")
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(h.fsys, name); err == nil && !st.IsDir() {
			h.serveFile(w, r, name)
			return
		}
		if strings.HasPrefix(clean, "/assets/") || staticExt[strings.ToLower(path.Ext(clean))] {
			http.NotFound(w, r)
			return
		}
	}
	h.serveIndex(w, r)
}

func setSecurityHeaders(w http.ResponseWriter) {
	hd := w.Header()
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "same-origin")
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	data, err := fs.ReadFile(h.fsys, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setSecurityHeaders(w)
	if strings.HasPrefix(name, "assets/") {
		// Vite content-hashes every file under assets/: safe to cache forever.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, name, h.modTime, bytes.NewReader(data))
}

func (h *handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	if h.index == nil {
		http.Error(w, "dashboard not built: run `npm ci && npm run build` in web/", http.StatusServiceUnavailable)
		return
	}
	setSecurityHeaders(w)
	hd := w.Header()
	hd.Set("Cache-Control", "no-cache")
	hd.Set("ETag", h.indexETag)
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
	hd.Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", h.modTime, bytes.NewReader(h.index))
}
