// Package server wires every Open Factory component into one HTTP server
// with its background loops: store, durable event bus and outbox, leader
// lease, secrets, definition compiler, engine, the dispatcher and the runner
// API (the runner in each task runs the agent), orchestration MCP, triggers,
// webhooks, the task backends and the remote worker hub, forges and gRPC
// plugins, chat and tracker integrations,
// notifications, object storage for run data, authentication (sessions, API
// keys, OIDC), REST API, Factory MCP, scoring, metrics and the embedded
// dashboard.
//
// Every replica runs the same components; the leader lease
// gates the scheduler, git definition polling and run-data retention to one
// replica.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/auth"
	"github.com/openfactory-org/openfactory/internal/cloudid"
	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/events"
	"github.com/openfactory-org/openfactory/internal/forge"
	"github.com/openfactory-org/openfactory/internal/forge/github"
	"github.com/openfactory-org/openfactory/internal/integrations"
	"github.com/openfactory-org/openfactory/internal/integrations/jira"
	"github.com/openfactory-org/openfactory/internal/integrations/linear"
	"github.com/openfactory-org/openfactory/internal/integrations/slack"
	"github.com/openfactory-org/openfactory/internal/integrations/teams"
	"github.com/openfactory-org/openfactory/internal/mcpauth"
	"github.com/openfactory-org/openfactory/internal/mcpregistry"
	"github.com/openfactory-org/openfactory/internal/mcpserver"
	"github.com/openfactory-org/openfactory/internal/metrics"
	"github.com/openfactory-org/openfactory/internal/networkpolicy"
	"github.com/openfactory-org/openfactory/internal/notify"
	"github.com/openfactory-org/openfactory/internal/objectstore"
	"github.com/openfactory-org/openfactory/internal/orchestration"
	"github.com/openfactory-org/openfactory/internal/plugin"
	"github.com/openfactory-org/openfactory/internal/runauth"
	"github.com/openfactory-org/openfactory/internal/runnerapi"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/direct"
	"github.com/openfactory-org/openfactory/internal/scoring"
	"github.com/openfactory-org/openfactory/internal/secrets"
	"github.com/openfactory-org/openfactory/internal/store"
	"github.com/openfactory-org/openfactory/internal/store/coord"
	"github.com/openfactory-org/openfactory/internal/triggers"
	"github.com/openfactory-org/openfactory/internal/webhooks"
	"github.com/openfactory-org/openfactory/internal/webui"
)

// Option customizes New (tests, embedding and custom builds through
// pkg/openfactory).
type Option func(*options)

type options struct {
	logger       *slog.Logger
	version      string
	commit, date string
	forges       core.ForgeResolver
	store        core.Store
	stdout       io.Writer
	pollEvery    time.Duration
	syncEvery    time.Duration
	retainEvery  time.Duration
	classifier   scoring.ClassifyFunc

	loginProviders []auth.LoginProvider
	authorizer     auth.Authorizer
	audit          auth.AuditSink
	tenants        auth.TenantResolver
	objectStore    objectstore.Store
	pluginPaths    []string
	routes         []func(*http.ServeMux)
	bus            core.EventBus
	queue          core.Queue
	leadership     core.Leadership
	keyProvider    core.KeyProvider
}

// WithKeyProvider replaces the local key provider (OF_SECRET_KEY) that wraps
// the data keys of the secret store.
func WithKeyProvider(p core.KeyProvider) Option { return func(o *options) { o.keyProvider = p } }

// WithLogger sets the logger (default: built from the config).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithVersion sets the version reported by /healthz, metrics and the Factory MCP.
func WithVersion(v string) Option { return func(o *options) { o.version = v } }

// WithBuildInfo sets the commit and build date reported by GET /api/v1/version.
func WithBuildInfo(commit, date string) Option {
	return func(o *options) { o.commit, o.date = commit, date }
}

// WithForgeResolver replaces the integration-based forge resolver.
func WithForgeResolver(r core.ForgeResolver) Option { return func(o *options) { o.forges = r } }

// WithStore uses an already opened store instead of OF_DATABASE_URL. The
// server closes it on Close.
func WithStore(s core.Store) Option { return func(o *options) { o.store = s } }

// WithStdout sets where the one-time dev bootstrap key is printed (default os.Stdout).
func WithStdout(w io.Writer) Option { return func(o *options) { o.stdout = w } }

// WithPollInterval sets how often the dispatcher looks at the queue (default 2s).
func WithPollInterval(d time.Duration) Option { return func(o *options) { o.pollEvery = d } }

// WithDefinitionSyncInterval sets how often git-backed definitions are polled (default 5m; <0 disables polling).
func WithDefinitionSyncInterval(d time.Duration) Option {
	return func(o *options) { o.syncEvery = d }
}

// WithClassifier replaces the LLM judge used by scorers.
func WithClassifier(f scoring.ClassifyFunc) Option { return func(o *options) { o.classifier = f } }

// WithLoginProvider adds an interactive login method (mounted at
// /auth/{name}/login and /auth/{name}/callback) next to the built-in OIDC
// provider.
func WithLoginProvider(p auth.LoginProvider) Option {
	return func(o *options) { o.loginProviders = append(o.loginProviders, p) }
}

// WithAuthorizer replaces the built-in role authorizer for REST API routes
// and the auth service.
func WithAuthorizer(a auth.Authorizer) Option { return func(o *options) { o.authorizer = a } }

// WithAuditSink replaces the default audit sink (structured logs).
func WithAuditSink(a auth.AuditSink) Option { return func(o *options) { o.audit = a } }

// WithTenantResolver replaces the single-team resolver used at login.
func WithTenantResolver(t auth.TenantResolver) Option { return func(o *options) { o.tenants = t } }

// WithObjectStore stores every run-data category (transcripts, artifacts,
// attachments) in s instead of the stores named by OF_OBJECT_STORE_*.
func WithObjectStore(s objectstore.Store) Option { return func(o *options) { o.objectStore = s } }

// WithPlugins adds plugin executables to launch, in addition to OF_PLUGIN_DIR
// and OF_PLUGINS.
func WithPlugins(paths ...string) Option {
	return func(o *options) { o.pluginPaths = append(o.pluginPaths, paths...) }
}

// WithRoutes registers extra HTTP routes on the server mux. They are mounted
// after the built-in routes and before the dashboard catch-all.
func WithRoutes(register func(mux *http.ServeMux)) Option {
	return func(o *options) { o.routes = append(o.routes, register) }
}

// withRetentionInterval sets how often run-data retention runs (tests).
func withRetentionInterval(d time.Duration) Option { return func(o *options) { o.retainEvery = d } }

// Server is a fully wired Open Factory control plane.
type Server struct {
	cfg     *config.Config
	log     *slog.Logger
	version string
	stdout  io.Writer

	Store core.Store
	// Bus is the event bus: the built-in durable bus (*events.Durable) unless
	// WithEventBus or WithQueue replaced it.
	Bus core.EventBus
	// Leader is the leader lease: *coord.Leader unless WithLeadership replaced it.
	Leader    core.Leadership
	Secrets   *secrets.Store
	MCP       *mcpregistry.Service
	Engine    *engine.Engine
	Hub       *direct.Hub
	Forges    core.ForgeResolver
	Plugins   *plugin.Host
	Auth      *auth.Service
	API       *api.Server
	Scoring   *scoring.Service
	Scheduler *triggers.Scheduler
	Notify    *notify.Router
	// RunData stores transcripts, artifacts and attachments.
	RunData *objectstore.RunData
	// OIDC is the cloud identity token issuer (nil without OF_PUBLIC_URL).
	OIDC *cloudid.Issuer

	// durable is the built-in bus when it is in use (for metrics and Close).
	durable *events.Durable
	// closers are option values closed on Close (a WithEventBus bus).
	closers     []io.Closer
	replicaID   string
	pollEvery   time.Duration
	retention   time.Duration
	retainEvery time.Duration
	webhooks    *webhooks.Handler
	github      *github.WebhookHandler
	defsync     *defSync
	metrics     *metricsCollector
	runner      *runnerMode // the dispatcher, the runner API and the planner

	mux     *http.ServeMux
	handler http.Handler

	started  atomic.Bool
	ready    atomic.Bool
	cancel   context.CancelFunc
	bgWG     sync.WaitGroup
	closeErr error
	closed   sync.Once
}

// New builds and wires every component. It does not start background loops
// or listen; call Run (or Start plus Handler for embedding).
func New(ctx context.Context, cfg *config.Config, opts ...Option) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("server: nil config")
	}
	o := options{stdout: os.Stdout, pollEvery: 2 * time.Second, syncEvery: 5 * time.Minute, retainEvery: time.Hour}
	for _, fn := range opts {
		fn(&o)
	}
	log := o.logger
	if log == nil {
		log = cfg.Logger()
	}
	if o.version == "" {
		o.version = "dev"
	}
	s := &Server{cfg: cfg, log: log, pollEvery: o.pollEvery, version: o.version, stdout: o.stdout, retainEvery: o.retainEvery}

	// Key material. The encryption key wraps the data keys of the secret
	// store. The signing key derives the keys of sessions, tokens and links.
	encKey, encPrev, err := cfg.EncryptionKeys()
	if err != nil {
		return nil, fmt.Errorf("secret key: %w", err)
	}
	signKey, signPrev, err := cfg.SigningKeys()
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	sessionKey, sessionPrev := config.DeriveKeys(signKey, signPrev, config.LabelWebSession)
	linkKey := config.DeriveKey(signKey, config.LabelAccountLink)
	// OIDC issuer for cloud identity federation (cloudProviders): the signing
	// key derives from the signing key, the issuer is OF_OIDC_ISSUER_URL
	// (default OF_PUBLIC_URL). The key set lists the previous keys too.
	oidcKey, oidcPrev := config.DeriveKeys(signKey, signPrev, config.LabelOIDCSigning)
	oidc, err := cloudid.NewIssuer(oidcKey, cfg.IssuerURL(), oidcPrev...)
	if err != nil {
		log.Warn("cloud identity disabled", "err", err)
		oidc = nil
	}
	s.OIDC = oidc

	leaderTTL, err := optDuration("OF_LEADER_LEASE_TTL", cfg.LeaderLeaseTTL)
	if err != nil {
		return nil, err
	}

	// ---------- persistence, coordination, bus, secrets ----------
	st := o.store
	if st == nil {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
		if st, err = store.Open(ctx, cfg.DatabaseURL); err != nil {
			return nil, fmt.Errorf("open store: %w", err)
		}
	}
	s.Store = st
	fail := func(err error) (*Server, error) {
		if s.Plugins != nil {
			_ = s.Plugins.Close()
		}
		s.closeBus()
		_ = st.Close()
		return nil, err
	}
	rid := coord.ReplicaID(cfg.ReplicaID)
	s.replicaID = rid
	coordinator := coord.From(st)
	if o.bus != nil {
		s.Bus = o.bus
		if c, ok := o.bus.(io.Closer); ok {
			s.closers = append(s.closers, c)
		}
	} else {
		s.durable = events.NewDurable(coordinator, st, events.DurableOptions{ReplicaID: rid, MaxAttempts: cfg.OutboxMaxAttempts, Logger: log})
		s.Bus = s.durable
	}
	if o.queue != nil {
		s.Bus = newQueuedBus(s.Bus, o.queue, log.With("component", "queue"))
	}
	if o.leadership != nil {
		s.Leader = o.leadership
	} else {
		s.Leader = coord.NewLeader(coordinator, coord.LeaderOptions{Holder: coord.NewHolder(rid), TTL: leaderTTL, Logger: log})
	}
	if o.keyProvider != nil {
		s.Secrets = secrets.NewWithProvider(st, o.keyProvider)
	} else {
		s.Secrets = secrets.NewWithKeys(st, encKey, encPrev...)
	}

	// ---------- run data (object storage) ----------
	if s.RunData, s.retention, err = openRunData(ctx, cfg, signKey, o.objectStore, &o.routes); err != nil {
		return fail(fmt.Errorf("object storage: %w", err))
	}

	// ---------- plugins (gRPC: GitLab, Azure DevOps, community) ----------
	factoryRepos := func(ctx context.Context, factoryID string) ([]core.RepoRef, error) {
		rt, err := engine.LoadFactoryRuntime(ctx, st, factoryID)
		if err != nil {
			return nil, err
		}
		return rt.Repositories, nil
	}
	s.Plugins = plugin.New(plugin.Options{
		Dir:   cfg.PluginDir,
		Paths: append(cfg.PluginPaths(), o.pluginPaths...),
		// Plugin processes get a minimal environment, not the server's.
		EnvAllow:    cfg.PluginEnvAllowList(),
		EnvByPlugin: cfg.PluginEnvMap(),
		Store:       st,
		Secrets:     s.Secrets,
		Logger:      log,
		Publish: func(ctx context.Context, ev core.Event) error {
			s.Bus.Publish(ctx, core.TopicInboundEvent, &ev)
			return nil
		},
		FactoryRepos: factoryRepos,
	})
	if err := s.Plugins.Start(ctx); err != nil {
		log.Warn("some plugins failed to start", "err", err)
	}
	for _, info := range s.Plugins.Plugins() {
		log.Info("plugin loaded", "plugin", info.Name)
	}

	// ---------- forges ----------
	userTokens := &forge.UserTokens{Store: st, Cipher: s.Secrets}
	if o.forges != nil {
		s.Forges = o.forges
	} else {
		s.Forges = forge.NewResolver(st, s.Secrets, forge.Options{UserTokens: userTokens, Plugins: s.Plugins})
	}

	// ---------- engine ----------
	if s.runner, err = newRunnerMode(cfg, signKey, signPrev, st, s.Bus, s.RunData, cloudTokens(oidc, st), o.audit, s.Plugins, log); err != nil {
		return fail(err)
	}
	s.Engine = engine.New(engine.Options{
		Runner:    s.runner.route(),
		PublicURL: cfg.PublicURL,
		Store:     st,
		Bus:       s.Bus,
		Compile:   definition.Compile,
		Match:     triggers.Match,
		Secrets:   s.Secrets,
		Forges:    s.Forges,
		Logger:    log,
		ReplicaID: rid,
		Leader:    s.Leader,

		MaxOrchestrationDepth: cfg.MaxOrchestrationDepthValue(),
	})

	// ---------- workers ----------
	apiKeyLookup := func(ctx context.Context, key string) (*core.User, *core.APIKey, error) {
		p, err := api.Authenticate(ctx, st, key)
		if err != nil {
			return nil, nil, err
		}
		return p.User, p.APIKey, nil
	}
	s.Hub = direct.NewHub(st, direct.HubOptions{
		// A worker receives run plans, secrets and forge credentials of the
		// team, so its key must have the team-wide admin role (a user or a
		// service account). The hub keeps a live worker ID for its principal.
		Authenticate: func(ctx context.Context, token string) (string, string, error) {
			p, err := api.Authenticate(ctx, st, token)
			if err != nil {
				return "", "", err
			}
			if !p.Can(core.RoleAdmin) {
				return "", "", core.ErrForbidden
			}
			return p.TeamID, p.UserID(), nil
		},
		Logger: log,
	})
	s.runner.setRouter(s.Hub)
	// The MCP paths accept the session token of the active session of a run.
	verifier := &runauth.Verifier{}
	verifier.Keys, verifier.Runner = s.runner.verifierParts()
	s.MCP = mcpregistry.New(mcpregistry.Options{
		Verifier:   verifier,
		Store:      st,
		Secrets:    s.Secrets,
		Encrypter:  s.Secrets,
		PublicURL:  cfg.PublicURL,
		SandboxURL: cfg.SandboxURL(),
		Logger:     log,
		// Agents never merge, unless the factory sets allowAgentMerge.
		AllowAgentMerge: func(ctx context.Context, run *core.Run) bool {
			rt, err := engine.LoadFactoryRuntime(ctx, st, run.FactoryID)
			return err == nil && rt.AllowAgentMerge
		},
		OnMergeCall: mergeAudit(o.audit, log),
	})
	spendTracker := newSpendTracker(cfg, st, s.Bus, log)
	// The planner computes the content of each session of the runner API.
	planner := engine.NewPlanner(engine.PlannerOptions{
		Store:             st,
		Secrets:           s.Secrets,
		Forges:            s.Forges,
		SandboxURL:        cfg.SandboxURL(),
		ResolveManagedMCP: s.MCP.Resolve,
		Logger:            log,
	})
	s.runner.setPlanner(planner)

	// ---------- scoring ----------
	s.Scoring, err = scoring.NewService(scoring.Options{
		Store: st,
		Bus:   s.Bus,
		// The judge of each scorer is a run in a sandbox.
		StartJudge:      s.Engine.StartJudgeRun,
		Classify:        o.classifier,
		CheckDefinition: s.Engine.CheckDefinition,
		RequestReview: func(ctx context.Context, teamID string, pr core.PullRequestRef, userID string) error {
			rr, ok := s.Forges.(interface {
				RequestReview(ctx context.Context, teamID string, pr core.PullRequestRef, userID string) error
			})
			if !ok {
				return core.ErrOperationNotSupported
			}
			return rr.RequestReview(ctx, teamID, pr, userID)
		},
		Engine:    s.Engine,
		Compile:   definition.Compile,
		Logger:    log,
		PublicURL: cfg.PublicURL,
	})
	if err != nil {
		return fail(fmt.Errorf("scoring: %w", err))
	}

	// ---------- authentication ----------
	providers := []auth.LoginProvider{}
	if oc, enabled := oidcConfig(cfg); enabled {
		p, err := auth.NewOIDCProvider(oc)
		if err != nil {
			return fail(err)
		}
		providers = append(providers, p)
	}
	providers = append(providers, o.loginProviders...)
	s.Auth, err = auth.New(auth.Config{
		Store:               st,
		SessionKey:          sessionKey,
		PreviousSessionKeys: sessionPrev,
		PublicURL:           cfg.PublicURL,
		APIKeyLookup:        apiKeyLookup,
		Providers:           providers,
		Authorizer:          o.authorizer,
		Audit:               o.audit,
		Tenants:             o.tenants,
		Logger:              log,
	})
	if err != nil {
		return fail(fmt.Errorf("auth: %w", err))
	}

	metricsFn := func(ctx context.Context, factoryID string, since time.Time) (any, error) {
		return metrics.Compute(ctx, st, factoryID, since, time.Now().UTC())
	}
	startBenchmarkAPI := func(ctx context.Context, factoryID, suite string, body json.RawMessage) (*core.BenchmarkRun, error) {
		var req api.StartBenchmarkRequest
		if len(body) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				return nil, fmt.Errorf("%w: %w", core.ErrInvalid, err)
			}
		}
		return s.Scoring.StartBenchmarkWithOptions(ctx, scoring.BenchmarkRequest{
			FactoryID: factoryID, Suite: suite, Configurations: req.Configurations,
			Repetitions: req.Repetitions, Scorers: req.Scorers,
		})
	}

	// ---------- REST API ----------
	var runData api.RunDataStore
	if s.RunData != nil {
		runData = s.RunData
	}
	presignTTL, _ := optDuration("OF_OBJECT_STORE_PRESIGN_TTL", cfg.ObjectStorePresignTTL)
	// Browser sign-in of the Factory MCP server.
	mcpAuth := mcpauth.New(mcpauth.Options{
		Store:         st,
		PublicURL:     cfg.PublicURL,
		ResourcePaths: []string{mcpserver.Path, mcpserver.AliasPath},
		Session: func(r *http.Request) (*core.User, error) {
			p, err := s.Auth.Authenticate(r)
			if err != nil || p == nil || p.Method != auth.MethodSession {
				return nil, err
			}
			return p.User, nil
		},
		Audit: func(ctx context.Context, action, outcome string, g *mcpauth.Grant, detail map[string]string) {
			ev := auth.AuditEvent{Action: action, Outcome: outcome, UserID: g.UserID, TeamID: g.TeamID, Detail: map[string]string{"client_id": g.ClientID, "client_name": g.ClientName}}
			if g.UserID != "" {
				ev.ActorID = "user:" + g.UserID
			}
			if g.ID != "" {
				ev.Detail["sign_in_id"] = g.ID
			}
			for k, v := range detail {
				ev.Detail[k] = v
			}
			s.Auth.Audit().Record(ctx, ev)
		},
		Logger: log,
	})
	apiOpts := api.Options{
		Store:   st,
		Engine:  s.Engine,
		Secrets: s.Secrets,
		ForgeCredentialsChanged: func(teamID string) {
			if inv, ok := s.Forges.(interface{ Invalidate(teamID string) }); ok {
				inv.Invalidate(teamID)
			}
		},
		Compile:       definition.Compile,
		Bus:           s.Bus,
		Schemas:       definition.Schemas,
		SchemaVersion: definition.SchemaVersion,
		TestFilter: func(pattern, body json.RawMessage) (bool, error) {
			return triggers.MatchPayload(pattern, body)
		},
		NetworkPolicies: func() any { return networkpolicy.Catalog() },
		Metrics:         metricsFn,
		Spend: func(ctx context.Context, teamID, factoryID string) (any, error) {
			return spendTracker.Report(ctx, teamID, factoryID)
		},
		StartBenchmark:  startBenchmarkAPI,
		CancelBenchmark: s.Scoring.CancelBenchmark,
		BenchmarkTaskFromRun: func(ctx context.Context, req api.BenchmarkTaskRequest) (any, map[string]string, map[string]string, error) {
			t, err := s.Scoring.MakeTaskFromRun(ctx, scoring.TaskFromRunRequest{
				FactoryID: req.FactoryID, Suite: req.Suite, RunID: req.FromRunID,
				Slug: req.Slug, Title: req.Title, SuccessCriteria: req.SuccessCriteria, Tags: req.Tags,
			})
			if err != nil {
				return nil, nil, nil, err
			}
			return t.Task, t.Files, t.Definition, nil
		},
		Proposals:       s.Scoring,
		UserForgeTokens: userTokens,
		MCPSignIns:      mcpAuth,
		PublicURL:       cfg.PublicURL,
		MCPRegistry:     s.MCP,
		Logger:          log,
		Authenticate:    s.apiAuthenticate,
		LoginProviders:  s.apiLoginProviders,
		ScoreRun:        s.Scoring.ScoreRun,
		RunSelfImprovement: func(ctx context.Context, factoryID string) ([]string, error) {
			// "Run now" ignores failedRunThreshold: any agent with an
			// unreviewed failure gets a run.
			runs, err := s.Scoring.TriggerSelfImprovement(ctx, factoryID, "")
			ids := make([]string, 0, len(runs))
			for _, r := range runs {
				ids = append(ids, r.ID)
			}
			return ids, err
		},
		Version:    api.VersionInfo{Version: o.version, Commit: o.commit, Date: o.date},
		RunData:    runData,
		PresignTTL: presignTTL,
		// s.defsync is set below; the API calls this only after New returns.
		CheckGitSource: func(ctx context.Context, f *core.Factory) error { return s.defsync.CheckBranch(ctx, f) },
	}
	if o.authorizer != nil {
		apiOpts.Authorize = s.apiAuthorize
	}
	_, apiOpts.Runner = s.runner.verifierParts()
	s.API = api.New(apiOpts)

	// ---------- integrations ----------
	linker := &integrations.Linker{
		Store:     st,
		Key:       linkKey,
		PublicURL: cfg.PublicURL,
		CurrentUser: func(r *http.Request) (*core.User, error) {
			p, err := s.Auth.Authenticate(r)
			if err != nil {
				return nil, err
			}
			if p == nil || p.User == nil {
				return nil, core.ErrUnauthorized
			}
			return p.User, nil
		},
		LoginURL: func(next string) string { return "/login?return_to=" + url.QueryEscape(next) },
		// The proof of a link is the sign-in of the provider, configured on
		// its integration record.
		Prover: func(ctx context.Context, teamID, provider string) (integrations.Prover, error) {
			return integrations.TeamProver(ctx, st, s.Secrets, teamID, provider, nil)
		},
		Audit: func(ctx context.Context, rec integrations.AuditRecord) {
			s.Auth.Audit().Record(ctx, auth.AuditEvent{
				Action: rec.Action, Outcome: rec.Outcome, ActorID: rec.ActorID, UserID: rec.UserID, TeamID: rec.TeamID,
				Provider: rec.Provider, Reason: rec.Reason,
				Detail: map[string]string{"external_id": rec.ExternalID, "method": rec.Method},
			})
		},
		Logger: log,
	}
	slackH := slack.NewHandler(slack.Options{
		Store: st, Secrets: s.Secrets, Bus: s.Bus, Linker: linker,
		OnAction: func(ctx context.Context, inboxID, option, actor string) error {
			// actor is the ID of the linked team member who pressed the button.
			ctx = core.WithResolver(ctx, core.Resolver{UserID: actor, Via: core.InboxViaChat})
			return s.Engine.ResolveInbox(ctx, inboxID, option, actor)
		},
		Logger: log,
	})
	factoryOf := func(ctx context.Context, workItemID string) (string, error) {
		wi, err := st.GetWorkItem(ctx, workItemID)
		if err != nil {
			return "", err
		}
		return wi.FactoryID, nil
	}
	slackR := slack.NewResponder(slack.ResponderOptions{Store: st, Secrets: s.Secrets, FactoryOf: factoryOf, Logger: log})
	s.Engine.AddEventContext(slackR) // thread history and files for the agent
	// The review thread and the diff of the file for a review comment.
	s.Engine.AddEventContext(eventctx.ReviewBuilders(s.Forges)...)
	teamsH := teams.NewHandler(teams.Options{Store: st, Secrets: s.Secrets, Bus: s.Bus, Linker: linker, Logger: log})
	s.MCP.SetTeamsPoster(teamsH) // builtin/teams
	linearH := linear.NewHandler(linear.Options{Store: st, Secrets: s.Secrets, Bus: s.Bus, Linker: linker, Logger: log})
	jiraH := jira.NewHandler(jira.Options{Store: st, Secrets: s.Secrets, Bus: s.Bus, Linker: linker, Logger: log})
	// The preflight resolves the names in the filters of automations.
	s.Engine.SetNameResolver(newNameResolvers(st, s.Secrets, teamsH, log))
	s.Engine.AddEventContext(teamsH) // the thread of a Teams message

	// ---------- notifications ----------
	responders := append(forge.Responders(s.Forges), s.Plugins.Responders()...)
	responders = append(responders, slackR, teamsH, linearH, jiraH)
	s.Notify = notify.NewRouter(s.Bus, responders...)
	s.Notify.Logger = log
	s.Notify.TeamOf = func(ctx context.Context, workItemID string) (string, error) {
		fid, err := factoryOf(ctx, workItemID)
		if err != nil {
			return "", err
		}
		f, err := st.GetFactory(ctx, fid)
		if err != nil {
			return "", err
		}
		return f.TeamID, nil
	}

	// ---------- forge webhooks & git-backed definitions ----------
	s.defsync = &defSync{store: st, forges: s.Forges, engine: s.Engine, publicURL: cfg.PublicURL, log: log.With("component", "definition-sync"), every: o.syncEvery, isLeader: s.Leader.IsLeader}
	s.github = github.NewWebhookHandler(github.WebhookOptions{
		Store: st, Secrets: s.Secrets, Bus: s.Bus,
		Resolver:         s.Forges,
		OnDefinitionPush: s.defsync.onPush,
		OnDefinitionPR:   s.defsync.onPullRequest,
		Logger:           log,
	})

	// ---------- custom webhooks, schedules ----------
	s.webhooks = webhooks.NewHandler(st, s.Secrets, s.Bus, webhooks.Options{Logger: log, Dispatch: s.Engine.HandleEvent})
	s.Scheduler = triggers.NewScheduler(st, s.Bus, triggers.SchedulerOptions{Logger: log, Leader: s.Leader})

	// ---------- Factory MCP ----------
	mcpSrv := mcpserver.New(mcpserver.Options{
		Store: st, Engine: s.Engine, Compile: definition.Compile, Schemas: definition.Schemas,
		API: s.API, PublicURL: cfg.PublicURL, Version: o.version, Logger: log,
		OAuth: mcpAuth,
	})
	orch := orchestration.NewServer(s.Engine, st)
	orch.SetLogger(log)
	orch.SetVerifier(verifier)
	orch.SetRedactor(planner.RedactFor)
	orch.SetTurnSource(s.runner.activeTurnSource())
	orch.SetProposer(func(ctx context.Context, runID string, files map[string]string, del []string, message string) (*core.DefinitionVersion, error) {
		return s.Scoring.ProposeDefinitionChange(ctx, scoring.ProposalRequest{RunID: runID, Files: files, Delete: del, Message: message})
	})

	s.metrics = newMetricsCollector(s)

	// ---------- routes ----------
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.metrics.serveHTTP)
	s.API.Register(mux) // also serves GET /api/v1/me
	s.Auth.Register(mux)
	mcpSrv.Register(mux)
	if mcpAuth.Enabled() {
		mcpAuth.Register(mux) // /.well-known/oauth-*, /oauth/*
	}
	orch.Register(mux)
	mux.Handle(runnerproto.PathPrefix+"/", s.runner.service.Handler())
	s.MCP.Register(mux) // /mcp/proxy/..., /mcp/integrations/...
	s.Hub.Register(mux)
	s.webhooks.Register(mux)
	s.github.Register(mux)
	s.Plugins.Register(mux) // POST /plugins/{name}/webhook
	slackH.Register(mux)
	teamsH.Register(mux)
	linearH.Register(mux)
	jiraH.Register(mux)
	linker.Register(mux)
	if oidc != nil {
		oidc.Register(mux) // /.well-known/openid-configuration, /.well-known/jwks.json
	}
	for _, register := range o.routes {
		register(mux)
	}
	webui.Register(mux) // dashboard catch-all: must stay last
	s.mux = mux
	s.handler = s.middleware(mux)
	return s, nil
}

// apiAuthenticate adapts the auth service (session cookies with CSRF
// checks, then API keys) to the REST API. It returns (nil, nil) for
// anonymous requests.
func (s *Server) apiAuthenticate(r *http.Request) (*api.Principal, error) {
	p, err := s.Auth.Authenticate(r)
	if err != nil || p == nil {
		return nil, err
	}
	return &api.Principal{User: p.User, TeamID: p.TeamID, Role: p.Role, APIKey: p.APIKey}, nil
}

func (s *Server) apiLoginProviders(r *http.Request) []api.LoginProvider {
	var out []api.LoginProvider
	for _, p := range s.Auth.LoginProviders(r) {
		out = append(out, api.LoginProvider{Name: p.Name, DisplayName: p.DisplayName, LoginURL: p.LoginURL})
	}
	return out
}

// apiAuthorize routes REST API authorization through the configured
// auth.Authorizer (installed only when a custom authorizer is set). For a
// request about a factory, the resource names that factory, so that the
// authorizer can apply the role override of the user for it.
func (s *Server) apiAuthorize(r *http.Request, p *api.Principal, min core.Role) error {
	ap := &auth.Principal{User: p.User, TeamID: p.TeamID, Role: p.Role, APIKey: p.APIKey, Method: auth.MethodSession}
	if p.APIKey != nil {
		ap.Method, ap.Provider = auth.MethodAPIKey, auth.ProviderAPIKey
	}
	res := auth.Resource{TeamID: p.TeamID}
	if fid := api.RouteFactoryID(r.Context()); fid != "" {
		res.Type, res.ID, res.FactoryID = "factory", fid, fid
	}
	return s.Auth.Authorizer().Allow(r.Context(), ap, auth.ActionForRole(r.Method, min), res)
}

// oidcConfig builds the OIDC login provider configuration from the server
// config (OF_OIDC_*). Login is enabled when an issuer is set.
func oidcConfig(cfg *config.Config) (auth.OIDCConfig, bool) {
	if cfg.OIDCIssuer == "" {
		return auth.OIDCConfig{}, false
	}
	oc := auth.OIDCConfig{
		Issuer:               cfg.OIDCIssuer,
		ClientID:             cfg.OIDCClientID,
		ClientSecret:         cfg.OIDCClientSecret,
		RedirectURL:          cfg.OIDCRedirectURL,
		Scopes:               auth.SplitList(cfg.OIDCScopes),
		AllowedDomains:       auth.SplitList(cfg.OIDCAllowedDomains),
		GroupsClaim:          cfg.OIDCGroupsClaim,
		AdminGroups:          auth.SplitList(cfg.OIDCAdminGroups),
		EditorGroups:         auth.SplitList(cfg.OIDCEditorGroups),
		DefaultRole:          core.Role(cfg.OIDCDefaultRole),
		DisplayName:          cfg.OIDCDisplayName,
		RequireVerifiedEmail: cfg.OIDCRequireVerifiedEmail,
	}
	if oc.RedirectURL == "" && cfg.PublicURL != "" {
		oc.RedirectURL = cfg.PublicURL + auth.OIDCCallbackPath
	}
	return oc, true
}

// openRunData opens the run-data object stores. A custom store (WithObjectStore)
// serves every category; otherwise OF_OBJECT_STORE_* select the stores, and
// presign handlers of local stores are added to routes.
func openRunData(ctx context.Context, cfg *config.Config, signKey []byte, custom objectstore.Store, routes *[]func(*http.ServeMux)) (*objectstore.RunData, time.Duration, error) {
	retention, err := optDuration("OF_OBJECT_STORE_RETENTION", cfg.ObjectStoreRetention)
	if err != nil {
		return nil, 0, err
	}
	if custom != nil {
		rd, err := objectstore.NewRunData(custom, nil, nil, nil, nil)
		return rd, retention, err
	}
	oc := objectstore.Config{
		URL:            cfg.ObjectStoreURL,
		TranscriptsURL: cfg.ObjectStoreTranscriptsURL,
		ArtifactsURL:   cfg.ObjectStoreArtifactsURL,
		AttachmentsURL: cfg.ObjectStoreAttachmentsURL,
		SnapshotsURL:   cfg.ObjectStoreSnapshotsURL,
		Retention:      retention,
		SigningKey:     []byte(cfg.ObjectStoreSigningKey),
	}
	if oc.URL == "" {
		dir, err := filepath.Abs(filepath.Join(cfg.DataDir, "objects"))
		if err != nil {
			return nil, 0, err
		}
		oc.URL = "file://" + filepath.ToSlash(dir)
	}
	if len(oc.SigningKey) == 0 {
		oc.SigningKey = config.DeriveKey(signKey, config.LabelObjectSigning)
	}
	rd, stores, err := oc.OpenRunData(ctx, objectstore.Options{SigningKey: oc.SigningKey, BaseURL: cfg.PublicURL})
	if err != nil {
		return nil, 0, err
	}
	handlers := objectstore.FileHandlers(stores)
	*routes = append([]func(*http.ServeMux){func(mux *http.ServeMux) {
		for _, h := range handlers {
			h.Register(mux)
		}
	}}, *routes...)
	return rd, retention, nil
}

// optDuration parses an optional duration setting ("" = 0, the built-in default).
func optDuration(name, v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s %q: want a duration such as 30s", name, v)
	}
	return d, nil
}

// Handler returns the server's root HTTP handler (with logging and recovery).
func (s *Server) Handler() http.Handler { return s.handler }

// Config returns the configuration the server was built with.
func (s *Server) Config() *config.Config { return s.cfg }

// Start runs the dev bootstrap (when applicable) and launches every
// background loop. Loops stop when ctx is cancelled or Close is called.
func (s *Server) Start(ctx context.Context) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("server: already started")
	}
	if s.cfg.Dev {
		if err := s.devBootstrap(ctx); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	if l, ok := s.Leader.(interface{ Start(context.Context) }); ok {
		l.Start(ctx)
	}
	if err := s.Engine.Start(ctx); err != nil {
		cancel()
		return err
	}
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		s.runner.loop(ctx, s.pollEvery, s.log)
	}()
	if err := s.Scheduler.Start(ctx); err != nil {
		cancel()
		return fmt.Errorf("start scheduler: %w", err)
	}
	if err := s.Scoring.Start(ctx); err != nil {
		cancel()
		return fmt.Errorf("start scoring: %w", err)
	}
	s.Notify.Start(ctx)
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		s.defsync.loop(ctx)
	}()
	if s.RunData != nil && s.retention > 0 {
		s.bgWG.Add(1)
		go func() {
			defer s.bgWG.Done()
			s.retentionLoop(ctx)
		}()
	}
	if _, ok := s.Store.(core.WebhookDeliveryPruner); ok {
		s.bgWG.Add(1)
		go func() {
			defer s.bgWG.Done()
			webhooks.RetentionLoop(ctx, s.Store, s.Leader.IsLeader, s.retainEvery, nil, s.log.With("component", "webhooks"))
		}()
	}
	s.ready.Store(true)
	s.log.Info("server started", "version", s.version, "replica", s.replicaID, "leader", s.Leader.IsLeader(),
		"public_url", s.cfg.PublicURL, "sandbox_backend", s.cfg.SandboxBackend, "dev", s.cfg.Dev)
	return nil
}

// Close stops background loops, waits for in-flight work and closes the store.
func (s *Server) Close() error {
	s.closed.Do(func() {
		s.ready.Store(false)
		if s.cancel != nil {
			s.cancel()
		}
		if s.started.Load() {
			s.Engine.Wait()
			s.Notify.Stop()
			s.bgWG.Wait()
			s.runner.stopTasks(s.log)
			if l, ok := s.Leader.(interface{ Done() <-chan struct{} }); ok {
				<-l.Done() // the lease is released before the store closes
			}
		}
		s.webhooks.Wait()
		s.github.Drain()
		if err := s.Scoring.Close(); err != nil {
			s.log.Warn("close scoring", "err", err)
		}
		if err := s.Plugins.Close(); err != nil {
			s.log.Warn("close plugins", "err", err)
		}
		s.closeBus()
		s.closeErr = s.Store.Close()
	})
	return s.closeErr
}

// Run starts the server, listens on OF_LISTEN and blocks until ctx is
// cancelled or SIGINT/SIGTERM arrives, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.cfg.Listen)
	if err != nil {
		_ = s.Close()
		return fmt.Errorf("listen on %s: %w", s.cfg.Listen, err)
	}
	return s.Serve(ctx, ln)
}

// Serve is Run with a caller-provided listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	defer s.Close()
	if err := s.Start(ctx); err != nil {
		_ = ln.Close()
		return err
	}
	hs := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	s.log.Info("listening", "addr", ln.Addr().String())
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	s.log.Info("shutting down")
	s.ready.Store(false)
	// Keep serving for the drain time: a load balancer (a Kubernetes Service)
	// sends new connections until it removes this replica, and a closed
	// listener refuses them.
	if d := s.cfg.ShutdownDrainDuration(); d > 0 {
		s.log.Info("draining before the listener closes", "for", d.String())
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case err := <-errc:
			t.Stop()
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		s.log.Warn("graceful shutdown incomplete", "err", err)
		_ = hs.Close()
	}
	return nil
}

// devBootstrap creates a default team, admin and API key on an empty store
// and prints the key once.
func (s *Server) devBootstrap(ctx context.Context) error {
	teams, err := s.Store.ListTeams(ctx)
	if err != nil {
		return fmt.Errorf("dev bootstrap: %w", err)
	}
	if len(teams) > 0 {
		return nil
	}
	res, err := s.API.Bootstrap(ctx, "Default", "admin@localhost", "Admin", "dev-bootstrap")
	if err != nil {
		return fmt.Errorf("dev bootstrap: %w", err)
	}
	fmt.Fprintf(s.stdout, "\nOpen Factory dev bootstrap: created team %q and admin %s.\nAPI key (shown once): %s\n  export OF_SERVER=%s OF_API_KEY=%s\n\n",
		res.Team.Name, res.User.Email, res.Key, s.cfg.PublicURL, res.Key)
	return nil
}

// retentionLoop deletes run data older than OF_OBJECT_STORE_RETENTION. Only
// the leader prunes.
func (s *Server) retentionLoop(ctx context.Context) {
	every := s.retainEvery
	if v, err := time.ParseDuration(s.cfg.ObjectStoreRetentionEvery); err == nil && v >= time.Minute {
		every = v
	}
	if every <= 0 {
		every = time.Hour
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !s.Leader.IsLeader() {
			continue
		}
		s.pruneRunData(ctx)
	}
}

// pruneRunData deletes the run data and the uploaded attachments that are
// older than the retention period, and the metadata of the attachments.
// Attachments are not linked to a run: only their age decides (see
// objectstore.RunData.PruneAttachments).
func (s *Server) pruneRunData(ctx context.Context) {
	pruned, err := s.RunData.Prune(ctx, s.retention)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("run data retention", "err", err)
	}
	if len(pruned) > 0 {
		s.log.Info("run data retention pruned runs", "runs", len(pruned), "older_than", s.retention)
	}
	ids, err := s.RunData.PruneAttachments(ctx, s.retention)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("attachment retention", "err", err)
	}
	if len(ids) > 0 {
		s.log.Info("attachment retention pruned attachments", "attachments", len(ids), "older_than", s.retention)
	}
	// The metadata rows have the time of the upload, like the files. A store
	// without core.KVExpirer keeps them; the rows are small.
	if kv, ok := s.Store.(core.KVExpirer); ok {
		before := time.Now().Add(-s.retention)
		if _, err := kv.KVDeleteOlder(ctx, core.AttachmentKVKey(""), before); err != nil && ctx.Err() == nil {
			s.log.Warn("attachment metadata retention", "err", err)
		}
	}
}

// closeBus closes the built-in durable bus, or a WithEventBus bus that
// implements io.Closer.
func (s *Server) closeBus() {
	if s.durable != nil {
		_ = s.durable.Close()
	}
	for _, c := range s.closers {
		_ = c.Close()
	}
}

// cloudTokens adapts the OIDC issuer to the cloud-token endpoint of the runner
// API. It returns nil when no issuer exists.
func cloudTokens(oidc *cloudid.Issuer, st core.Store) runnerapi.CloudTokenSource {
	if oidc == nil {
		return nil
	}
	return cloudTokenSource{iss: oidc, store: st}
}

type cloudTokenSource struct {
	iss   *cloudid.Issuer
	store core.Store
}

// CloudToken issues a token only for an audience that the factory allows.
// It adds the creator of the work item and the
// worker of the session as claims.
func (c cloudTokenSource) CloudToken(ctx context.Context, run *core.Run, sess *core.RunSession, audience string, ttl time.Duration) (string, time.Time, error) {
	rt, err := engine.LoadFactoryRuntime(ctx, c.store, run.FactoryID)
	if err != nil {
		return "", time.Time{}, err
	}
	if !cloudid.AllowedAudience(rt.CloudProviders, audience) {
		return "", time.Time{}, fmt.Errorf("%w: audience %q is not allowed", core.ErrForbidden, audience)
	}
	return c.iss.Token(run, audience, ttl, runTokenOptions(ctx, c.store, run, sess))
}

// runTokenOptions returns the claims of a cloud identity token that do not
// come from the run itself.
func runTokenOptions(ctx context.Context, st core.Store, run *core.Run, sess *core.RunSession) cloudid.TokenOptions {
	var o cloudid.TokenOptions
	if run.Creator != nil {
		o.Creator = run.Creator.ID
	} else if run.RunAs == nil && run.WorkItemID != "" && st != nil {
		// A run from before Run.Creator: the creator of its work item.
		if wi, err := st.GetWorkItem(ctx, run.WorkItemID); err == nil {
			o.Creator = wi.CreatorID
		}
	}
	switch {
	case sess != nil && sess.WorkerID != "":
		o.WorkerHost = sess.WorkerID
	case run.Config.WorkerHost != "":
		o.WorkerHost = run.Config.WorkerHost
	case sess != nil:
		o.WorkerHost = sess.Backend
	}
	return o
}

// mergeAudit records each merge tool call of an agent in the audit log
// (action "agent.merge"). A refused call has the outcome failure.
func mergeAudit(sink auth.AuditSink, log *slog.Logger) func(ctx context.Context, run *core.Run, server, tool string, allowed bool) {
	if sink == nil {
		sink = auth.SlogAuditSink{Logger: log}
	}
	return func(ctx context.Context, run *core.Run, server, tool string, allowed bool) {
		ev := auth.AuditEvent{
			Action:  "agent.merge",
			Outcome: auth.OutcomeSuccess,
			ActorID: "run:" + run.ID,
			TeamID:  run.TeamID,
			Detail: map[string]string{
				"run_id": run.ID, "factory_id": run.FactoryID, "agent": run.AgentName,
				"mcp_server": server, "tool": tool,
			},
		}
		if !allowed {
			ev.Outcome = auth.OutcomeFailure
			ev.Reason = "allowAgentMerge is false"
		}
		sink.Record(ctx, ev)
	}
}
