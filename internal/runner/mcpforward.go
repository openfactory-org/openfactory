package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	// authSessionToken is the value of the field auth of an MCP server that
	// takes the session token.
	authSessionToken = "session_token"
	// mcpForwardPrefix is the only path prefix that the forwarder passes on.
	mcpForwardPrefix = "/mcp/"
)

// mcpForwarder is a local reverse proxy to the MCP paths of the control
// plane. It adds the current session token to each request. The harness
// reads its MCP configuration at the start of a turn, but the session token
// changes every hour. So the harness calls the forwarder and never sees the
// token.
type mcpForwarder struct {
	base string // http://127.0.0.1:<port>
	srv  *http.Server
	done chan struct{}
}

// newMCPForwarder starts a forwarder on a free local port. token returns the
// session token to send now.
func newMCPForwarder(controlURL string, token func() string, log *slog.Logger) (*mcpForwarder, error) {
	target, err := url.Parse(strings.TrimRight(controlURL, "/"))
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("bad control URL %q", controlURL)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("Authorization", "Bearer "+token())
		},
		FlushInterval: -1, // stream each write
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Warn("mcp forwarder: upstream request failed", "error", err)
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !forwardable(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		rp.ServeHTTP(w, r)
	})
	f := &mcpForwarder{
		base: "http://" + ln.Addr().String(),
		srv:  &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second},
		done: make(chan struct{}),
	}
	go func() {
		defer close(f.done)
		if err := f.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("mcp forwarder stopped", "error", err)
		}
	}()
	return f, nil
}

// forwardable reports whether the forwarder passes a path on. The path must
// be below /mcp/ and must not have dot segments, so that it cannot leave /mcp/.
func forwardable(p string) bool {
	if !strings.HasPrefix(p, mcpForwardPrefix) {
		return false
	}
	return path.Clean(p) == strings.TrimSuffix(p, "/")
}

// localURL maps the URL of a server on the control plane to the forwarder.
func (f *mcpForwarder) localURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if !forwardable(u.Path) {
		return "", fmt.Errorf("path %q is not below %s", u.Path, mcpForwardPrefix)
	}
	out := f.base + u.EscapedPath()
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, nil
}

// Close stops the forwarder and its open connections.
func (f *mcpForwarder) Close() {
	if f == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.srv.Shutdown(ctx); err != nil {
		_ = f.srv.Close()
	}
	<-f.done
}

// hasSessionTokenServer reports whether a server of the start response takes
// the session token.
func hasSessionTokenServer(servers map[string]runnerproto.MCPServer) bool {
	for _, s := range servers {
		if s.URL != "" && s.Auth == authSessionToken {
			return true
		}
	}
	return false
}

// startMCPForward starts the forwarder of the session if one server needs it.
// If it cannot start, it emits a diagnostic, and the turns get no server
// that needs the session token.
func (r *runner) startMCPForward() {
	if !hasSessionTokenServer(r.start.Harness.MCPServers) {
		return
	}
	f, err := newMCPForwarder(r.cfg.ControlURL, r.client.Token, r.log)
	if err != nil {
		r.log.Error("start mcp forwarder", "error", err)
		r.diagnostic("mcp_forward_failed", "the local MCP forwarder did not start: "+err.Error())
		return
	}
	r.mcpForward = f
}

// turnHarness returns the harness part of the start response for a turn. It
// is a copy. A server with auth session_token gets the URL of the local
// forwarder. If there is no forwarder, the server is left out. The stored
// start response does not change.
func (r *runner) turnHarness() runnerproto.Harness {
	h := r.start.Harness
	if !hasSessionTokenServer(h.MCPServers) {
		return h
	}
	servers := make(map[string]runnerproto.MCPServer, len(h.MCPServers))
	for name, s := range h.MCPServers {
		if s.URL == "" || s.Auth != authSessionToken {
			servers[name] = s
			continue
		}
		if r.mcpForward == nil {
			continue
		}
		local, err := r.mcpForward.localURL(s.URL)
		if err != nil {
			r.log.Warn("mcp server left out", "server", name, "error", err)
			continue
		}
		s.URL, s.Auth = local, ""
		servers[name] = s
	}
	h.MCPServers = servers
	return h
}
