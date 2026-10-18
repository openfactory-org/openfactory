// Package api implements the Open Factory REST API (/api/v1): factories,
// definitions, work items, inbox, agent runs (with SSE streaming), webhooks,
// secrets, environments, machines, workers, integrations, API keys, teams and
// users, scorers, benchmarks and metrics. Authentication uses bearer API keys
// of the form of_<prefix>_<secret>; authorization follows the role matrix
// user < editor < admin. A user can
// have a role override for one factory; routes about a factory check the
// effective role for that factory.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/links"
)

// BasePath is the mount point of every REST endpoint.
const BasePath = "/api/v1"

// Options injects the server's dependencies. Store and Engine are required;
// every other field is optional and the corresponding endpoints answer
// feature_not_available when it is nil.
type Options struct {
	Store   core.Store
	Engine  core.Engine
	Secrets core.SecretStore
	// ForgeCredentialsChanged drops the cached forge clients of a team. The
	// API calls it after a secret or an integration record changes, so that a
	// new token or setting applies at once. Optional.
	ForgeCredentialsChanged func(teamID string)
	Compile                 core.DefinitionCompiler
	Bus                     core.EventBus

	// Schemas returns the factory-file JSON Schema documents of the current
	// schema version keyed by document name (e.g. "factory.schema.json").
	Schemas func() map[string]json.RawMessage
	// SchemaVersion is the current schema version (default "v1").
	SchemaVersion string
	// TestFilter evaluates a webhook payload pattern against a delivery body.
	TestFilter func(pattern, body json.RawMessage) (bool, error)
	// NetworkPolicies returns the built-in network policy catalog.
	NetworkPolicies func() any
	// Metrics computes dashboard metrics for a factory.
	Metrics func(ctx context.Context, factoryID string, since time.Time) (any, error)
	// MCPSignIns lists and revokes Factory MCP browser sign-ins
	// (GET /mcp-sign-ins, DELETE /mcp-sign-ins/{id}). Optional.
	MCPSignIns MCPSignIns
	// Spend reports the model spend and the spend limits of a factory and
	// its team (GET /factories/{uid}/spend). Optional.
	Spend func(ctx context.Context, teamID, factoryID string) (any, error)
	// StartBenchmark launches a benchmark suite run.
	StartBenchmark func(ctx context.Context, factoryID, suite string, configs json.RawMessage) (*core.BenchmarkRun, error)
	// CancelBenchmark cancels a benchmark run (POST /benchmark-runs/{id}/cancel).
	CancelBenchmark func(ctx context.Context, id, actor string) (*core.BenchmarkRun, error)
	// BenchmarkTaskFromRun builds a benchmark task from a finished run
	// (POST /factories/{uid}/benchmarks/{suite}/tasks). It returns the changed
	// files and the full definition tree with the change.
	BenchmarkTaskFromRun func(ctx context.Context, req BenchmarkTaskRequest) (task any, files, definition map[string]string, err error)
	// Proposals reads and decides proposed definition versions. Optional.
	Proposals DefinitionProposals
	// DefaultDefinition builds the initial definition tree for a new factory.
	// When nil, a built-in generator (foreman + selected default agents) is used.
	DefaultDefinition func(req DefaultDefinitionRequest) map[string]string

	// PublicURL is the externally reachable base URL (OF_PUBLIC_URL), used to
	// build dashboard links and webhook ingress URLs.
	PublicURL string
	// MCPRegistry handles OAuth for registered MCP servers. Optional.
	MCPRegistry MCPRegistry
	Logger      *slog.Logger
	// Now overrides the clock (tests).
	// UserForgeTokens stores team members' personal code-forge tokens, used
	// by runs whose credentialStrategy is CREATOR (forge.UserTokens). Optional.
	UserForgeTokens UserForgeTokenStore
	Now             func() time.Time
	// SSEHeartbeat is the keep-alive interval of event streams (default 15s).
	SSEHeartbeat time.Duration

	// Authenticate resolves the caller from a request (for example a browser
	// session cookie, see internal/auth). When set it is consulted first on
	// every authenticated route; it returns (nil, nil) when the request carries
	// no credentials it understands, in which case the built-in bearer API key
	// check runs. Errors wrapping core.ErrUnauthorized map to 401 and errors
	// wrapping core.ErrForbidden (e.g. a missing CSRF header) to 403.
	Authenticate func(r *http.Request) (*Principal, error)
	// LoginProviders lists the interactive sign-in options shown by the
	// dashboard login page (GET /me and GET /login-providers).
	LoginProviders func(r *http.Request) []LoginProvider
	// ScoreRun scores one run on demand with a scorer (POST /runs/{id}/score).
	ScoreRun func(ctx context.Context, scorerID, runID string) (*core.Score, error)
	// RunSelfImprovement triggers an ad hoc self-improvement check
	// (POST /factories/{uid}/self-improvement).
	RunSelfImprovement func(ctx context.Context, factoryID string) error
	// Version describes the running build (GET /version).
	Version VersionInfo
	// Authorize replaces the built-in role check of authenticated routes
	// (an extension point for custom authorizers). It receives the route's
	// minimum built-in role and returns nil to allow, or an error wrapping
	// core.ErrForbidden. RouteFactoryID(r.Context()) names the factory the
	// request is about ("" for team-wide routes). When set, roles outside
	// admin/editor/user returned by Authenticate are passed through unchanged.
	Authorize func(r *http.Request, p *Principal, minRole core.Role) error
	// RunData serves run transcripts, artifacts and attachments from object
	// storage (GET /runs/{id}/transcript, .../artifacts, .../attachments).
	RunData RunDataStore
	// Runner persists sessions, runner events and the message queue of a
	// run. Optional. Without it, sandbox_running is false, session is 0, and
	// the endpoints /inputs, /timeline, /diagnostics and /sessions answer 501.
	Runner core.RunnerStore
	// PresignTTL is the lifetime of presigned run-data URLs (default 15m).
	PresignTTL time.Duration
	// CheckGitSource makes sure that the git branch of f compiles and passes
	// the preflight. It is called before a factory changes its source to git
	// or changes its git location. An error with a ValidationErrors method
	// gives the errors to the caller. Optional.
	CheckGitSource func(ctx context.Context, f *core.Factory) error
}

// LoginProvider is one interactive sign-in option (an SSO button on the login page).
type LoginProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	LoginURL    string `json:"login_url"`
}

// VersionInfo describes the server build.
type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
	Date    string `json:"date,omitempty"`
}

// Server serves the REST API.
type Server struct {
	opts       Options
	log        *slog.Logger
	routes     []route
	bootstrapM sync.Mutex
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, p *Principal) error

type route struct {
	Method  string
	Path    string    // relative to BasePath, Go 1.22 pattern syntax
	Role    core.Role // "" => unauthenticated
	handler handlerFunc
}

// RouteInfo describes one registered endpoint.
type RouteInfo struct {
	Method string
	Path   string // full path including BasePath
	Role   core.Role
}

// New constructs a Server.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.SchemaVersion == "" {
		opts.SchemaVersion = "v1"
	}
	if opts.SSEHeartbeat <= 0 {
		opts.SSEHeartbeat = 15 * time.Second
	}
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	s := &Server{opts: opts, log: opts.Logger.With("component", "api")}
	s.routes = s.buildRoutes()
	return s
}

// Routes lists every endpoint the server registers.
func (s *Server) Routes() []RouteInfo {
	out := make([]RouteInfo, 0, len(s.routes))
	for _, rt := range s.routes {
		out = append(out, RouteInfo{Method: rt.Method, Path: BasePath + rt.Path, Role: rt.Role})
	}
	return out
}

// Register mounts every endpoint on mux.
func (s *Server) Register(mux *http.ServeMux) {
	for _, rt := range s.routes {
		mux.Handle(rt.Method+" "+BasePath+rt.Path, s.wrap(rt))
	}
}

// Handler returns a standalone handler serving the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

func (s *Server) now() time.Time { return s.opts.Now() }

func (s *Server) wrap(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = strings.TrimPrefix(core.NewID("req"), "req_")
		}
		w.Header().Set("X-Request-Id", rid)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, rid))

		var p *Principal
		if rt.Role != "" {
			var err error
			p, err = s.authenticate(r)
			if err != nil {
				writeError(w, r, s.log, err)
				return
			}
			if p == nil {
				ae := Errf(CodeAuthenticationRequired, "missing credentials: sign in or send an Authorization: Bearer header")
				if s.opts.LoginProviders != nil {
					ae.Extra = map[string]any{"login_providers": s.loginProviders(r)}
				}
				writeProblem(w, r, ae)
				return
			}
			fid := s.routeFactory(r, p)
			if fid != "" {
				r = r.WithContext(context.WithValue(r.Context(), routeFactoryKey{}, fid))
			}
			if s.opts.Authorize != nil {
				if err := s.opts.Authorize(r, p, rt.Role); err != nil {
					writeError(w, r, s.log, err)
					return
				}
			} else if !p.CanOn(fid, rt.Role) {
				if fid != "" {
					writeProblem(w, r, errForbidden("this operation requires the %s role; your role for this factory is %s", rt.Role, p.RoleFor(fid)))
				} else {
					writeProblem(w, r, errForbidden("this operation requires the %s role; your role is %s", rt.Role, p.RoleFor("")))
				}
				return
			}
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		if err := rt.handler(w, r, p); err != nil {
			writeError(w, r, s.log, err)
		}
	})
}

// authenticate resolves the caller: the Authenticate hook first (sessions),
// then the bearer API key. It returns (nil, nil) when no credentials are present.
func (s *Server) authenticate(r *http.Request) (*Principal, error) {
	if s.opts.Authenticate != nil {
		p, err := s.opts.Authenticate(r)
		if err != nil {
			return nil, err
		}
		if p != nil {
			if s.opts.Authorize == nil {
				if ValidRole(p.Role) {
					p.Role = core.NormalizeRole(p.Role)
				} else {
					p.Role = core.RoleUser
				}
			}
			return p, nil
		}
	}
	tok := BearerToken(r)
	if tok == "" {
		return nil, nil
	}
	return Authenticate(r.Context(), s.opts.Store, tok)
}

type routeFactoryKey struct{}

// RouteFactoryID returns the ID of the factory that the current request is
// about, as found by the route wrapper: the {uid} of the path, or the factory
// of the work item, inbox item, webhook, benchmark run or run of the path.
// It is "" for team-wide routes and for objects that do not exist.
func RouteFactoryID(ctx context.Context) string {
	id, _ := ctx.Value(routeFactoryKey{}).(string)
	return id
}

// routeFactory finds the factory a request is about from its path values.
// Objects of another team, and objects that do not exist, give "" (the
// handler then answers 404). Body fields are not read: a route that takes
// the factory from the body is checked with the team-wide role.
func (s *Server) routeFactory(r *http.Request, p *Principal) string {
	ctx := r.Context()
	st := s.opts.Store
	if st == nil || p == nil {
		return ""
	}
	if uid := r.PathValue("uid"); uid != "" {
		if f, err := st.GetFactory(ctx, uid); err == nil && f.TeamID == p.TeamID {
			return f.ID
		}
		return ""
	}
	if runID := r.PathValue("runId"); runID != "" {
		if run, err := st.GetRun(ctx, runID); err == nil && run.TeamID == p.TeamID {
			return run.FactoryID
		}
		return ""
	}
	id := r.PathValue("id")
	if id == "" {
		return ""
	}
	path := r.URL.Path
	var factoryID string
	switch {
	case strings.HasPrefix(path, BasePath+"/work-items/"):
		if wi, err := st.GetWorkItem(ctx, id); err == nil {
			factoryID = wi.FactoryID
		}
	case strings.HasPrefix(path, BasePath+"/inbox/"):
		if it, err := st.GetInboxItem(ctx, id); err == nil {
			factoryID = it.FactoryID
		}
	case strings.HasPrefix(path, BasePath+"/webhooks/"):
		if wh, err := st.GetWebhook(ctx, id); err == nil {
			factoryID = wh.FactoryID
		}
	case strings.HasPrefix(path, BasePath+"/benchmark-runs/"):
		if br, err := st.GetBenchmarkRun(ctx, id); err == nil {
			factoryID = br.FactoryID
		}
	default:
		return ""
	}
	if factoryID == "" {
		return ""
	}
	if f, err := st.GetFactory(ctx, factoryID); err == nil && f.TeamID == p.TeamID {
		return f.ID
	}
	return ""
}

func (s *Server) loginProviders(r *http.Request) []LoginProvider {
	out := []LoginProvider{}
	if s.opts.LoginProviders != nil {
		out = append(out, s.opts.LoginProviders(r)...)
	}
	return out
}

func (s *Server) buildRoutes() []route {
	const (
		pub    core.Role = ""
		user             = core.RoleUser
		editor           = core.RoleEditor
		admin            = core.RoleAdmin
	)
	return []route{
		// Discovery / unauthenticated
		{"GET", "/openapi.json", pub, s.handleOpenAPI},
		{"POST", "/bootstrap", pub, s.handleBootstrap},
		{"GET", "/factory-files/schemas", pub, s.handleSchemaVersions},
		{"GET", "/factory-files/schemas/{version}", pub, s.handleSchemaVersion},
		{"GET", "/factory-files/schemas/{version}/{document}", pub, s.handleSchemaDocument},
		{"GET", "/login-providers", pub, s.handleLoginProviders},
		{"GET", "/version", pub, s.handleVersion},

		// Live updates (Server-Sent Events)
		{"GET", "/stream", user, s.handleStream},

		// Identity
		{"GET", "/me", user, s.handleMe},
		{"GET", "/teams", user, s.handleListTeams},
		{"GET", "/teams/{id}", user, s.handleGetTeam},
		{"GET", "/users", user, s.handleListUsers},
		{"POST", "/users", admin, s.handleCreateUser},
		{"GET", "/users/{id}", user, s.handleGetUser},
		{"PATCH", "/users/{id}", admin, s.handleUpdateUser},
		{"GET", "/mcp-sign-ins", user, s.handleListMCPSignIns},
		{"DELETE", "/mcp-sign-ins/{id}", user, s.handleRevokeMCPSignIn},
		{"GET", "/api-keys", user, s.handleListAPIKeys},
		{"POST", "/api-keys", user, s.handleCreateAPIKey},
		{"DELETE", "/api-keys/{id}", user, s.handleDeleteAPIKey},
		{"GET", "/service-accounts", admin, s.handleListServiceAccounts},
		{"POST", "/service-accounts", admin, s.handleCreateServiceAccount},
		{"GET", "/service-accounts/{id}", admin, s.handleGetServiceAccount},
		{"PATCH", "/service-accounts/{id}", admin, s.handleUpdateServiceAccount},
		{"DELETE", "/service-accounts/{id}", admin, s.handleDeleteServiceAccount},
		{"GET", "/service-accounts/{id}/keys", admin, s.handleListServiceAccountKeys},
		{"POST", "/service-accounts/{id}/keys", admin, s.handleCreateServiceAccountKey},
		{"DELETE", "/service-accounts/{id}/keys/{keyId}", admin, s.handleDeleteServiceAccountKey},

		// Factories
		{"GET", "/factories", user, s.handleListFactories},
		{"POST", "/factories", editor, s.handleCreateFactory},
		{"GET", "/factories/{uid}", user, s.handleGetFactory},
		{"PATCH", "/factories/{uid}", editor, s.handleUpdateFactory},
		{"DELETE", "/factories/{uid}", admin, s.handleDeleteFactory},
		{"POST", "/factories/{uid}/runs", user, s.handleDispatch},
		{"GET", "/factories/{uid}/definition", user, s.handleGetDefinition},
		{"PUT", "/factories/{uid}/definition", editor, s.handlePutDefinition},
		{"POST", "/factories/{uid}/definition/validate", user, s.handleValidateDefinition},
		{"GET", "/factories/{uid}/definition/versions", user, s.handleListDefinitionVersions},
		{"GET", "/factories/{uid}/definition/versions/{versionId}", user, s.handleGetDefinitionVersion},
		{"GET", "/factories/{uid}/agents", user, s.handleListAgents},
		{"GET", "/factories/{uid}/agents/{name}", user, s.handleGetAgent},
		{"GET", "/factories/{uid}/automations", user, s.handleListAutomations},
		{"GET", "/factories/{uid}/integrations", user, s.handleGetFactoryIntegrations},
		{"PUT", "/factories/{uid}/integrations", editor, s.handlePutFactoryIntegrations},
		{"GET", "/factories/{uid}/settings", user, s.handleGetFactorySettings},
		{"GET", "/factories/{uid}/spend", user, s.handleGetSpend},
		{"POST", "/factories/{uid}/definition/versions/{versionId}/restore", editor, s.handleRestoreDefinitionVersion},
		{"GET", "/factories/{uid}/agents/{name}/spec", user, s.handleGetAgentSpec},
		{"PUT", "/factories/{uid}/agents/{name}/spec", editor, s.handlePutAgentSpec},
		{"GET", "/factories/{uid}/automations/{name}", user, s.handleGetAutomation},
		{"PUT", "/factories/{uid}/automations/{name}", editor, s.handlePutAutomation},
		{"DELETE", "/factories/{uid}/automations/{name}", editor, s.handleDeleteAutomation},
		{"POST", "/factories/{uid}/automations/{name}/validate", user, s.handleValidateAutomation},
		{"GET", "/factories/{uid}/schedules", user, s.handleListSchedules},
		{"POST", "/factories/{uid}/schedules/{id}/pause", editor, s.handlePauseSchedule},
		{"POST", "/factories/{uid}/schedules/{id}/resume", editor, s.handleResumeSchedule},
		{"POST", "/automations/{id}/run", user, s.handleRunAutomation},
		{"POST", "/factories/{uid}/test-filter", user, s.handleFactoryTestFilter},
		{"POST", "/factories/{uid}/self-improvement", editor, s.handleSelfImprovement},

		// Work items
		{"GET", "/factories/{uid}/work-items", user, s.handleListWorkItems},
		{"GET", "/work-items/{id}", user, s.handleGetWorkItem},
		{"POST", "/work-items/{id}/messages", user, s.handleWorkItemMessage},
		{"POST", "/work-items/{id}/cancel", user, s.handleCancelWorkItem},
		{"POST", "/work-items/{id}/complete", user, s.handleCompleteWorkItem},
		{"POST", "/work-items/{id}/stage", editor, s.handleSetWorkItemStage},

		// Inbox
		{"GET", "/inbox", user, s.handleListTeamInbox},
		{"GET", "/factories/{uid}/inbox", user, s.handleListInbox},
		{"GET", "/inbox/{id}", user, s.handleGetInboxItem},
		{"POST", "/inbox/{id}/resolve", user, s.handleResolveInbox},
		{"POST", "/inbox/{id}/read", user, s.handleReadInbox},
		{"POST", "/inbox/{id}/unresolve", user, s.handleUnresolveInbox},

		// Webhooks
		{"GET", "/factories/{uid}/webhooks", user, s.handleListWebhooks},
		{"POST", "/factories/{uid}/webhooks", editor, s.handleCreateWebhook},
		{"GET", "/webhooks/{id}", user, s.handleGetWebhook},
		{"PATCH", "/webhooks/{id}", editor, s.handleUpdateWebhook},
		{"DELETE", "/webhooks/{id}", editor, s.handleDeleteWebhook},
		{"POST", "/webhooks/{id}/rotate-secret", editor, s.handleRotateWebhookSecret},
		{"GET", "/webhooks/{id}/deliveries", user, s.handleListDeliveries},
		{"GET", "/webhooks/{id}/deliveries/{deliveryId}", user, s.handleGetDelivery},
		{"POST", "/webhooks/{id}/test-filter", user, s.handleTestFilter},

		// Measure & improve
		{"GET", "/factories/{uid}/scorers", user, s.handleListScorers},
		{"GET", "/factories/{uid}/scores", user, s.handleListScores},
		{"GET", "/factories/{uid}/benchmarks", user, s.handleListBenchmarkSuites},
		{"POST", "/factories/{uid}/benchmarks/{suite}/runs", editor, s.handleStartBenchmark},
		{"GET", "/factories/{uid}/benchmarks/{suite}/runs", user, s.handleListBenchmarkRuns},
		{"GET", "/factories/{uid}/benchmark-runs", user, s.handleListBenchmarkRuns},
		{"GET", "/benchmark-runs/{id}", user, s.handleGetBenchmarkRun},
		{"POST", "/benchmark-runs/{id}/cancel", editor, s.handleCancelBenchmarkRun},
		{"POST", "/factories/{uid}/benchmarks/{suite}/tasks", editor, s.handleBenchmarkTaskFromRun},
		{"GET", "/factories/{uid}/definition/proposals", user, s.handleListProposals},
		{"POST", "/factories/{uid}/definition/proposals/{id}/approve", editor, s.handleApproveProposal},
		{"POST", "/factories/{uid}/definition/proposals/{id}/reject", editor, s.handleRejectProposal},
		{"GET", "/factories/{uid}/metrics", user, s.handleMetrics},

		// Agent runs
		{"POST", "/runs", user, s.handleCreateRun},
		{"GET", "/runs", user, s.handleListRuns},
		{"GET", "/runs/{runId}", user, s.handleGetRun},
		{"GET", "/runs/{runId}/messages", user, s.handleRunMessages},
		{"GET", "/runs/{runId}/events", user, s.handleRunEvents},
		{"POST", "/runs/{runId}/messages", user, s.handlePostRunMessage},
		{"GET", "/runs/{runId}/inputs", user, s.handleRunInputs},
		{"GET", "/runs/{runId}/timeline", user, s.handleRunTimeline},
		{"GET", "/runs/{runId}/diagnostics", user, s.handleRunDiagnostics},
		{"GET", "/runs/{runId}/sessions", user, s.handleRunSessions},
		{"GET", "/runs/{runId}/conversation", user, s.handleRunConversation},
		{"POST", "/runs/{runId}/followups", user, s.handleFollowUp},
		{"POST", "/attachments", user, s.handleUploadAttachment},
		{"POST", "/runs/{runId}/cancel", user, s.handleCancelRun},
		{"POST", "/runs/{runId}/interrupt", user, s.handleInterruptRun},
		{"POST", "/runs/{runId}/score", editor, s.handleScoreRun},
		{"GET", "/runs/{runId}/transcript", user, s.handleRunTranscript},
		{"GET", "/runs/{runId}/artifacts", user, s.handleListRunArtifacts},
		{"GET", "/runs/{runId}/artifacts/{name}", user, s.handleGetRunArtifact},
		{"GET", "/runs/{runId}/attachments", user, s.handleListRunAttachments},
		{"POST", "/runs/{runId}/attachments", user, s.handleCreateRunAttachment},
		{"GET", "/runs/{runId}/attachments/{name}", user, s.handleGetRunAttachment},

		// Platform
		{"GET", "/secrets", user, s.handleListSecrets},
		{"POST", "/secrets", editor, s.handleCreateSecret},
		{"GET", "/secrets/{name}", user, s.handleGetSecret},
		{"PUT", "/secrets/{name}", editor, s.handleUpdateSecret},
		{"DELETE", "/secrets/{name}", editor, s.handleDeleteSecret},
		{"GET", "/environments", user, s.handleListEnvironments},
		{"POST", "/environments", editor, s.handleCreateEnvironment},
		{"GET", "/environments/{id}", user, s.handleGetEnvironment},
		{"PATCH", "/environments/{id}", editor, s.handleUpdateEnvironment},
		{"DELETE", "/environments/{id}", editor, s.handleDeleteEnvironment},
		{"GET", "/mcp-servers", user, s.handleListMCPServers},
		{"POST", "/mcp-servers", editor, s.handleCreateMCPServer},
		{"GET", "/mcp-servers/oauth/callback", editor, s.handleMCPOAuthCallback},
		{"GET", "/mcp-servers/{id}", user, s.handleGetMCPServer},
		{"PATCH", "/mcp-servers/{id}", editor, s.handleUpdateMCPServer},
		{"DELETE", "/mcp-servers/{id}", editor, s.handleDeleteMCPServer},
		{"POST", "/mcp-servers/{id}/oauth/connect", editor, s.handleConnectMCPServer},
		{"POST", "/mcp-servers/{id}/oauth/disconnect", editor, s.handleDisconnectMCPServer},
		{"GET", "/machines", user, s.handleListMachines},
		{"GET", "/workers", user, s.handleListWorkers},
		{"GET", "/outbox/dead", admin, s.handleListDeadOutbox},
		{"POST", "/outbox/{id}/redrive", admin, s.handleRedriveOutbox},

		// Personal forge tokens (credentialStrategy CREATOR)
		{"GET", "/me/forge-tokens", user, s.handleListUserForgeTokens},
		{"PUT", "/me/forge-tokens/{forge}", user, s.handleSetUserForgeToken},
		{"DELETE", "/me/forge-tokens/{forge}", user, s.handleDeleteUserForgeToken},
		{"GET", "/workers/{id}", user, s.handleGetWorker},
		{"GET", "/integrations", user, s.handleListIntegrations},
		{"POST", "/integrations", admin, s.handleCreateIntegration},
		{"GET", "/integrations/{id}", user, s.handleGetIntegration},
		{"PATCH", "/integrations/{id}", admin, s.handleUpdateIntegration},
		{"DELETE", "/integrations/{id}", admin, s.handleDeleteIntegration},
		{"GET", "/networking/policies", user, s.handleNetworkPolicies},
	}
}

// ---------- shared lookups with team scoping ----------

// factoryFor loads a factory and checks it belongs to the caller's team.
// Cross-team access is reported as not found to avoid leaking existence.
func (s *Server) factoryFor(ctx context.Context, p *Principal, id string) (*core.Factory, error) {
	f, err := s.opts.Store.GetFactory(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, errNotFound("factory " + id)
		}
		return nil, err
	}
	if f.TeamID != p.TeamID {
		return nil, errNotFound("factory " + id)
	}
	return f, nil
}

func (s *Server) workItemFor(ctx context.Context, p *Principal, id string) (*core.WorkItem, *core.Factory, error) {
	wi, err := s.opts.Store.GetWorkItem(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, nil, errNotFound("work item " + id)
		}
		return nil, nil, err
	}
	f, err := s.factoryFor(ctx, p, wi.FactoryID)
	if err != nil {
		return nil, nil, errNotFound("work item " + id)
	}
	return wi, f, nil
}

func (s *Server) runFor(ctx context.Context, p *Principal, id string) (*core.Run, error) {
	run, err := s.opts.Store.GetRun(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, errNotFound("run " + id)
		}
		return nil, err
	}
	if run.TeamID != p.TeamID {
		return nil, errNotFound("run " + id)
	}
	return run, nil
}

func (s *Server) webhookFor(ctx context.Context, p *Principal, id string) (*core.Webhook, *core.Factory, error) {
	wh, err := s.opts.Store.GetWebhook(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, nil, errNotFound("webhook " + id)
		}
		return nil, nil, err
	}
	f, err := s.factoryFor(ctx, p, wh.FactoryID)
	if err != nil {
		return nil, nil, errNotFound("webhook " + id)
	}
	return wh, f, nil
}

func isNotFound(err error) bool { return err != nil && errorsIs(err, core.ErrNotFound) }

// ---------- dashboard links ----------

// FactoryURL is the dashboard link of a factory.
func (s *Server) FactoryURL(factoryID string) string {
	return links.Factory(s.opts.PublicURL, factoryID)
}

// WorkItemURL is the dashboard link of a work item (task).
func (s *Server) WorkItemURL(factoryID, workItemID string) string {
	return links.WorkItem(s.opts.PublicURL, factoryID, workItemID)
}

// RunURL is the dashboard link of a run.
func (s *Server) RunURL(runID string) string {
	return links.Run(s.opts.PublicURL, runID)
}

// WebhookIngressURL is the URL external systems POST deliveries to.
func (s *Server) WebhookIngressURL(webhookID string) string {
	return s.opts.PublicURL + "/webhooks/w/" + webhookID
}

func featureUnavailable(what string) *Error {
	return Errf(CodeFeatureNotAvailable, "%s is not configured on this server", what)
}

// SchemaVersion returns the current definition schema version.
func (s *Server) SchemaVersion() string { return s.opts.SchemaVersion }

// forgeCredentialsChanged tells the server that a secret or an integration
// record of the team changed.
func (s *Server) forgeCredentialsChanged(teamID string) {
	if s.opts.ForgeCredentialsChanged != nil {
		s.opts.ForgeCredentialsChanged(teamID)
	}
}
