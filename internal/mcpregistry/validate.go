package mcpregistry

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
)

var (
	nameRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	placeholderRe = regexp.MustCompile(`\{\{\s*[A-Z_][A-Z0-9_]*\s*\}\}`)
	sensitiveRe   = regexp.MustCompile(`(?i)(authorization|token|secret|password|passwd|api[-_]?key|apikey|credential|cookie|private)`)
)

// DefaultConnection sets the connection mode of a new url server that names
// none: MCPConnectionProxy, or MCPConnectionDirect with a warning for a
// legacy SSE URL, which the proxy cannot serve. It returns the warning, or "".
// Call it before Validate when a server is created.
func DefaultConnection(m *core.RegisteredMCPServer) string {
	if strings.TrimSpace(m.URL) == "" || m.Connection != "" {
		return ""
	}
	if m.OAuth == nil && harness.IsSSEURL(strings.TrimSpace(m.URL)) {
		m.Connection = core.MCPConnectionDirect
		return "the legacy SSE transport cannot use the control-plane proxy: the server is registered with connection direct, so its headers go to the sandbox"
	}
	m.Connection = core.MCPConnectionProxy
	return ""
}

// Validate checks a registry entry and normalizes it (trimmed fields,
// connection proxy forced for OAuth). An empty Connection of a url server
// becomes direct, or proxy with OAuth. It does not check name uniqueness.
func Validate(m *core.RegisteredMCPServer) error {
	m.Name = strings.TrimSpace(m.Name)
	m.URL = strings.TrimSpace(m.URL)
	m.Command = strings.TrimSpace(m.Command)
	m.Connection = strings.ToLower(strings.TrimSpace(m.Connection))
	switch m.Connection {
	case "", core.MCPConnectionProxy, core.MCPConnectionDirect:
	default:
		return invalid("connection %q: want proxy or direct", m.Connection)
	}
	if !nameRe.MatchString(m.Name) {
		return invalid("name %q must be 1-64 letters, digits, '.', '_' or '-' and start with a letter or digit", m.Name)
	}
	if strings.HasPrefix(strings.ToLower(m.Name), "builtin") {
		return invalid("name %q: names starting with \"builtin\" are reserved for built-in servers", m.Name)
	}
	if (m.URL == "") == (m.Command == "") {
		return invalid("set exactly one of url and command")
	}
	if m.URL != "" {
		if err := checkURL(m.URL, false); err != nil {
			return invalid("url: %v", err)
		}
		if len(m.Args) > 0 || len(m.Env) > 0 {
			return invalid("args and env apply only to command servers")
		}
	}
	if m.Command != "" {
		if len(m.Headers) > 0 {
			return invalid("headers apply only to url servers")
		}
		if m.Connection == core.MCPConnectionProxy || m.OAuth != nil {
			return invalid("connection proxy and oauth apply only to url servers")
		}
		m.Connection = ""
	}
	if m.URL != "" && m.Connection == "" {
		m.Connection = core.MCPConnectionDirect
		if m.OAuth != nil {
			m.Connection = core.MCPConnectionProxy
		}
	}
	if m.OAuth != nil {
		if m.Connection == core.MCPConnectionDirect {
			return invalid("an OAuth server needs connection proxy: its tokens stay in the control plane")
		}
		m.Connection = core.MCPConnectionProxy
		o := m.OAuth
		o.ClientID = strings.TrimSpace(o.ClientID)
		o.ClientSecretName = strings.TrimSpace(o.ClientSecretName)
		if o.ClientSecretName != "" && o.ClientID == "" {
			return invalid("oauth.client_secret_name needs oauth.client_id")
		}
		for _, f := range []struct{ name, v string }{{"authorization_endpoint", o.AuthorizationEndpoint}, {"token_endpoint", o.TokenEndpoint}} {
			if f.v == "" {
				continue
			}
			if err := checkURL(f.v, true); err != nil {
				return invalid("oauth.%s: %v", f.name, err)
			}
		}
		for _, sc := range o.Scopes {
			if strings.TrimSpace(sc) == "" || strings.ContainsAny(sc, " \t\n") {
				return invalid("oauth.scopes entries must be single non-empty scope values")
			}
		}
		if o.Status == "" {
			o.Status = core.MCPOAuthNotConnected
		}
	}
	if m.Connection == core.MCPConnectionProxy && harness.IsSSEURL(m.URL) {
		return invalid("proxied servers must use the streamable HTTP transport; legacy SSE URLs (ending in /sse) cannot be proxied: set connection direct")
	}
	for k, v := range m.Headers {
		if err := checkCredential("headers", k, v); err != nil {
			return err
		}
	}
	for k, v := range m.Env {
		if err := checkCredential("env", k, v); err != nil {
			return err
		}
	}
	return nil
}

// checkCredential rejects literal values for credential-like keys: they must
// reference a secret with a {{SECRET}} placeholder.
func checkCredential(field, key, value string) error {
	if strings.TrimSpace(key) == "" {
		return invalid("%s keys must be non-empty", field)
	}
	if sensitiveRe.MatchString(key) && !placeholderRe.MatchString(value) {
		return invalid("%s.%s looks like a credential: store it as a secret and reference it as {{SECRET_NAME}}", field, key)
	}
	return nil
}

// checkURL requires an absolute http(s) URL. With httpsOnly, plain http is
// allowed only for loopback hosts.
func checkURL(raw string, httpsOnly bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("must be an absolute http or https URL")
	}
	if u.User != nil {
		return fmt.Errorf("must not embed credentials")
	}
	if httpsOnly && u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("must use https")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", core.ErrInvalid, fmt.Sprintf(format, args...))
}
