package mcpregistry

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// forwardedHeaders are the request headers passed from the run to the
// upstream server. Everything else, notably the run's Authorization header,
// is dropped.
var forwardedHeaders = []string{
	"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-ID", "User-Agent",
}

// serveProxy forwards one streamable-HTTP MCP request of a run to a
// registered server, adding the server's headers and OAuth token.
func (s *Service) serveProxy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	run := runFrom(ctx)
	m, err := s.opts.Store.GetMCPServer(ctx, r.PathValue("serverId"))
	if err != nil || m.TeamID != run.TeamID {
		writeError(w, http.StatusNotFound, "not_found", "MCP server not found")
		return
	}
	if granted, _ := grantedTo(run, m.ID, m.Name); !granted {
		writeError(w, http.StatusForbidden, "forbidden", "this run was not granted MCP server "+m.Name)
		return
	}
	if !m.Proxied() || m.URL == "" {
		writeError(w, http.StatusBadRequest, "not_proxied", "MCP server "+m.Name+" is not proxied")
		return
	}
	upstream, err := url.Parse(m.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "bad_upstream", "invalid upstream URL")
		return
	}
	headers := http.Header{}
	for k, v := range m.Headers {
		if strings.Contains(v, "{{") {
			if s.opts.Secrets == nil {
				writeError(w, http.StatusBadGateway, "secrets", "no secret store configured")
				return
			}
			if v, err = s.opts.Secrets.Expand(ctx, run.TeamID, run.FactoryID, v); err != nil {
				s.log.Warn("mcp proxy: header secret", "server", m.Name, "run", run.ID, "header", k, "err", err)
				writeError(w, http.StatusBadGateway, "secrets", "a secret referenced by MCP server "+m.Name+" is missing")
				return
			}
		}
		headers.Set(k, v)
	}
	if m.OAuth != nil {
		tok, err := s.AccessToken(ctx, m)
		if err != nil {
			code := "upstream_auth"
			if errors.Is(err, ErrNotConnected) {
				code = "not_connected"
			}
			writeError(w, http.StatusBadGateway, code, "MCP server "+m.Name+" is not connected: an editor must reconnect it in the dashboard")
			return
		}
		headers.Set("Authorization", "Bearer "+tok)
	}

	rp := &httputil.ReverseProxy{
		Transport:     s.proxy.Transport,
		FlushInterval: -1, // stream SSE responses as they arrive
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			u := *upstream
			out.URL = &u
			out.Host = u.Host
			fresh := http.Header{}
			for _, h := range forwardedHeaders {
				if v := pr.In.Header.Values(h); len(v) > 0 {
					fresh[http.CanonicalHeaderKey(h)] = v
				}
			}
			for k, v := range headers {
				fresh[k] = v
			}
			out.Header = fresh
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				// Do not hand the upstream's OAuth challenge to the agent's MCP
				// client: credentials are managed here, not in the sandbox.
				resp.Header.Del("WWW-Authenticate")
				s.log.Warn("mcp proxy: upstream rejected credentials", "server", m.Name, "run", run.ID, "status", resp.StatusCode)
				if m.OAuth != nil {
					s.setStatus(ctx, m, core.MCPOAuthError, "the server rejected the access token (HTTP "+http.StatusText(resp.StatusCode)+"); reconnect it")
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if ctx.Err() == nil {
				s.log.Warn("mcp proxy: upstream error", "server", m.Name, "run", run.ID, "err", err)
			}
			writeError(w, http.StatusBadGateway, "upstream", "MCP server "+m.Name+" is unreachable")
		},
	}
	if rp.Transport == nil {
		rp.Transport = http.DefaultTransport
	}
	s.log.Debug("mcp proxy request", "server", m.Name, "run", run.ID, "method", r.Method)
	rp.ServeHTTP(w, r)
}
