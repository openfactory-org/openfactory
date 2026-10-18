// Package mcpregistry implements the team MCP server registry and the MCP
// endpoints the control plane serves for runs:
//
//   - Resolve turns an agent's mcpServers.<name>.managedId into a transport.
//     A registry entry resolves to the control-plane proxy (connection
//     proxy, the default for url servers), or to its own url or command
//     (connection direct, or a command server). "builtin/<provider>" resolves to
//     the built-in integration server.
//   - /mcp/proxy/{runId}/{serverId} forwards streamable-HTTP MCP traffic to a
//     registered server and adds its headers and OAuth access token there, so
//     credentials never enter the sandbox.
//   - /mcp/integrations/{provider}/{runId} serves integration tools (Linear,
//     Jira, Slack) with the factory's integration credentials.
//   - The OAuth connect flow (StartOAuth, CompleteOAuth) stores refresh
//     tokens encrypted with the secrets master key.
//
// Every run endpoint authenticates with the session token of the runner
// (runauth).
package mcpregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/mergeguard"
	"github.com/openfactory-org/openfactory/internal/runauth"
)

// Paths of the run endpoints.
const (
	ProxyPathPrefix       = "/mcp/proxy/"
	IntegrationPathPrefix = "/mcp/integrations/"
	// CallbackPath is the OAuth redirect path below the public URL.
	CallbackPath = "/api/v1/mcp-servers/oauth/callback"
)

// Encrypter seals OAuth tokens (implemented by *secrets.Store).
type Encrypter interface {
	Encrypt(teamID, factoryID, name string, typ core.SecretType, value []byte) ([]byte, error)
	Decrypt(teamID, factoryID, name string, typ core.SecretType, envelope []byte) ([]byte, error)
}

// Options configures a Service.
type Options struct {
	Store core.Store
	// Secrets expands {{SECRET}} placeholders and reads OAuth client secrets
	// and integration credentials.
	Secrets core.SecretStore
	// Encrypter seals stored OAuth tokens. Required for OAuth.
	Encrypter Encrypter
	// PublicURL is the control plane's external URL. Required for proxied,
	// OAuth and built-in servers.
	PublicURL string
	// SandboxURL is the base URL that sandboxes use for the proxy and the
	// built-in servers (OF_INTERNAL_URL). Default: PublicURL.
	SandboxURL string
	// HTTPClient is used for upstream MCP servers, OAuth endpoints and
	// integration APIs (default: a client with a 30s timeout for OAuth and
	// integration calls; proxied MCP streams use no overall timeout).
	HTTPClient *http.Client
	// Integration API base URLs (tests). Empty uses the provider defaults.
	LinearGraphQLURL string
	SlackAPIBaseURL  string
	Logger           *slog.Logger
	Now              func() time.Time
	// Verifier checks the bearer token of the run endpoints. Without it, the
	// endpoints refuse every request.
	Verifier *runauth.Verifier
	// AllowAgentMerge reports whether the factory of a run allows merge tool
	// calls (allowAgentMerge). Nil means never: the merge guard refuses each
	// merge call on the proxy and the built-in servers.
	AllowAgentMerge func(ctx context.Context, run *core.Run) bool
	// OnMergeCall records each merge tool call of a run with the decision
	// (the audit log). It can be nil.
	OnMergeCall func(ctx context.Context, run *core.Run, server, tool string, allowed bool)
}

// Service is the MCP server registry.
type Service struct {
	opts  Options
	log   *slog.Logger
	http  *http.Client // OAuth and integration API calls
	proxy *http.Client // proxied MCP streams
	cache *mcp.SchemaCache
	teams TeamsPoster // builtin/teams; set with SetTeamsPoster

	refreshMu sync.Mutex
	refreshes map[string]*sync.Mutex // server id -> refresh lock
}

// New creates a Service.
func New(opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	opts.SandboxURL = strings.TrimRight(opts.SandboxURL, "/")
	if opts.SandboxURL == "" {
		opts.SandboxURL = opts.PublicURL
	}
	s := &Service{
		opts:      opts,
		log:       opts.Logger.With("component", "mcp-registry"),
		cache:     mcp.NewSchemaCache(),
		refreshes: map[string]*sync.Mutex{},
	}
	if opts.HTTPClient != nil {
		s.http, s.proxy = opts.HTTPClient, opts.HTTPClient
	} else {
		s.http = &http.Client{Timeout: 30 * time.Second}
		s.proxy = &http.Client{}
	}
	return s
}

// Register mounts the run endpoints (proxy and built-in integration servers).
// The OAuth callback is served by the REST API.
func (s *Service) Register(mux *http.ServeMux) {
	mux.Handle(ProxyPathPrefix+"{runId}/{serverId}", s.authenticateRun(s.mergeGuard(http.HandlerFunc(s.serveProxy), "serverId")))
	mux.Handle(IntegrationPathPrefix+"{provider}/{runId}", s.authenticateRun(s.mergeGuard(s.integrationHandler(), "provider")))
}

// mergeGuard refuses merge tool calls of runs (see internal/mergeguard).
func (s *Service) mergeGuard(next http.Handler, serverParam string) http.Handler {
	return mergeguard.Wrap(next, mergeguard.Policy{
		Allow: func(r *http.Request) bool {
			return s.opts.AllowAgentMerge != nil && s.opts.AllowAgentMerge(r.Context(), runFrom(r.Context()))
		},
		Record: func(r *http.Request, tool string, allowed bool) {
			run := runFrom(r.Context())
			s.log.Warn("agent merge call", "run", run.ID, "factory", run.FactoryID, "server", r.PathValue(serverParam), "tool", tool, "allowed", allowed)
			if s.opts.OnMergeCall != nil {
				s.opts.OnMergeCall(r.Context(), run, r.PathValue(serverParam), tool, allowed)
			}
		},
	})
}

// Lookup finds a team's registry entry by name, then by id.
func (s *Service) Lookup(ctx context.Context, teamID, ref string) (*core.RegisteredMCPServer, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("%w: empty MCP server reference", core.ErrInvalid)
	}
	list, err := s.opts.Store.ListMCPServers(ctx, teamID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == ref {
			return &list[i], nil
		}
	}
	m, err := s.opts.Store.GetMCPServer(ctx, ref)
	if err == nil && m.TeamID == teamID {
		return m, nil
	}
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	return nil, fmt.Errorf("%w: no MCP server %q is registered for this team", core.ErrNotFound, ref)
}

// Resolve implements engine.PlannerOptions.ResolveManagedMCP. The servers of
// the control plane that it returns have no Authorization header: the runner
// sends its session token to them.
func (s *Service) Resolve(ctx context.Context, run *core.Run, managedID string) (core.MCPServer, error) {
	if provider, ok := core.BuiltinMCPProvider(managedID); ok {
		if s.opts.PublicURL == "" {
			return core.MCPServer{}, errors.New("built-in MCP servers need the server's public URL (OF_PUBLIC_URL)")
		}
		return core.MCPServer{URL: s.opts.SandboxURL + IntegrationPathPrefix + provider + "/" + run.ID}, nil
	} else if strings.HasPrefix(managedID, core.BuiltinMCPPrefix) {
		return core.MCPServer{}, fmt.Errorf("%w: unknown built-in MCP server %q", core.ErrInvalid, managedID)
	}
	m, err := s.Lookup(ctx, run.TeamID, managedID)
	if err != nil {
		return core.MCPServer{}, err
	}
	if m.Proxied() {
		if s.opts.PublicURL == "" {
			return core.MCPServer{}, fmt.Errorf("MCP server %q is proxied, which needs the server's public URL (OF_PUBLIC_URL)", m.Name)
		}
		if m.OAuth != nil && m.OAuth.Status != core.MCPOAuthConnected {
			return core.MCPServer{}, fmt.Errorf("MCP server %q is not connected: an editor must connect it (OAuth) in the dashboard", m.Name)
		}
		return core.MCPServer{URL: s.opts.SandboxURL + ProxyPathPrefix + run.ID + "/" + m.ID}, nil
	}
	out := core.MCPServer{URL: m.URL, Command: m.Command}
	if len(m.Args) > 0 {
		out.Args = append([]string(nil), m.Args...)
	}
	if len(m.Headers) > 0 {
		out.Headers = copyMap(m.Headers)
	}
	if len(m.Env) > 0 {
		out.Env = copyMap(m.Env)
	}
	return out, nil
}

// grantedTo reports whether run declares an MCP server whose managedId
// selects ref (a registry entry's id or name, or a builtin id), and whether
// any such declaration allows writes.
func grantedTo(run *core.Run, refs ...string) (granted, allowWrite bool) {
	for _, srv := range run.Config.MCPServers {
		for _, r := range refs {
			if r != "" && srv.ManagedID == r {
				granted = true
				allowWrite = allowWrite || srv.AllowWrite
			}
		}
	}
	return granted, allowWrite
}

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type runKey struct{}

// authenticateRun checks the session token of /mcp/.../{runId} requests and puts
// the run in the request context.
func (s *Service) authenticateRun(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("runId")
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			token = ""
		}
		v := s.opts.Verifier
		if v == nil {
			runauth.WriteError(w, runauth.ErrUnauthorized)
			return
		}
		if err := v.Verify(r.Context(), runID, token); err != nil {
			if !errors.Is(err, runauth.ErrUnauthorized) && !errors.Is(err, runauth.ErrSuperseded) {
				s.log.Error("check session token", "run", runID, "err", err)
			}
			runauth.WriteError(w, err)
			return
		}
		run, err := s.opts.Store.GetRun(r.Context(), runID)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "run not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "could not load run")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), runKey{}, run)))
	})
}

func runFrom(ctx context.Context) *core.Run {
	r, _ := ctx.Value(runKey{}).(*core.Run)
	return r
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	type body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.NewEncoder(w).Encode(map[string]body{"error": {Code: code, Message: msg}})
}
