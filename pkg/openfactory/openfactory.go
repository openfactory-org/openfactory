// Package openfactory is the public extension layer of Open Factory: it
// builds a complete control-plane server from Go code, so a separate module
// can compile a custom openfactory binary with its own login providers,
// authorizer, audit sink, tenancy, store, object storage, harnesses, sandbox
// backends, plugins and HTTP routes, without importing internal/ packages.
//
// EXPERIMENTAL: this API is not covered by the v1 compatibility promise.
// It may change in any release until it is declared stable in
// a v1.x release. Run `make go-apidiff` to see changes.
//
// A minimal custom binary:
//
//	cfg, err := openfactory.LoadConfig(os.LookupEnv)
//	if err != nil { log.Fatal(err) }
//	srv, err := openfactory.New(ctx, cfg,
//		openfactory.WithAuthorizer(myAuthorizer{}),
//		openfactory.WithAuditSink(mySink{}),
//	)
//	if err != nil { log.Fatal(err) }
//	log.Fatal(srv.Run(ctx))
//
// The types re-exported here are aliases of the server's own types, so
// values flow between the extension and the server without conversion.
package openfactory

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/spf13/pflag"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/server"
	"github.com/openfactory-org/openfactory/internal/store"
)

// Config is the server configuration (OF_* environment variables, YAML file
// and flags). Build one with LoadConfig or DefaultConfig.
type Config = config.Config

// DefaultConfig returns the built-in defaults. Adjust fields, then call
// cfg.Normalize() to derive the remaining defaults (data directory, sandbox
// backend, ...) and validate.
func DefaultConfig() *Config {
	c := config.Defaults()
	return &c
}

// LoadConfig reads the configuration from lookup (usually os.LookupEnv),
// including the YAML file named by OF_CONFIG, then applies derived defaults
// and validates it.
func LoadConfig(lookup func(string) (string, bool)) (*Config, error) {
	return config.LoadFrom(nil, lookup)
}

// BindFlags registers the configuration flags (--listen, --database-url,
// ...) on fs; pass fs to LoadConfigFlags after parsing.
func BindFlags(fs *pflag.FlagSet) { config.BindFlags(fs) }

// LoadConfigFlags is LoadConfig with flag overrides from fs (see BindFlags).
func LoadConfigFlags(fs *pflag.FlagSet, lookup func(string) (string, bool)) (*Config, error) {
	return config.LoadFrom(fs, lookup)
}

// OpenStore opens a store by URL: sqlite:///path, sqlite://:memory: or postgres://...
func OpenStore(ctx context.Context, url string) (Store, error) { return store.Open(ctx, url) }

// Option customizes New.
type Option = server.Option

// Server is a wired Open Factory control plane.
type Server struct {
	s *server.Server
}

// New builds a server from cfg and opts. It does not start background loops
// or listen; call Run, Serve, or Start plus Handler.
func New(ctx context.Context, cfg *Config, opts ...Option) (*Server, error) {
	s, err := server.New(ctx, cfg, opts...)
	if err != nil {
		return nil, err
	}
	return &Server{s: s}, nil
}

// Handler is the root HTTP handler (API, auth, webhooks, dashboard, ...).
func (s *Server) Handler() http.Handler { return s.s.Handler() }

// Start runs the dev bootstrap (in dev mode) and starts every background loop.
func (s *Server) Start(ctx context.Context) error { return s.s.Start(ctx) }

// Run starts the server, listens on cfg.Listen and blocks until ctx is
// cancelled or SIGINT/SIGTERM arrives, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error { return s.s.Run(ctx) }

// Serve is Run with a caller-provided listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error { return s.s.Serve(ctx, ln) }

// Close stops background loops and closes the store.
func (s *Server) Close() error { return s.s.Close() }

// Store returns the server's store.
func (s *Server) Store() Store { return s.s.Store }

// Secrets returns the server's secret store. The server has no settings for
// integration credentials: each team has integration records and team
// secrets. An extension that serves many teams writes the credential of each
// team with Secrets and its record with Store().UpsertIntegration.
func (s *Server) Secrets() SecretStore { return s.s.Secrets }

// ---------- options ----------

// WithLogger sets the logger (default: built from the config).
func WithLogger(l *slog.Logger) Option { return server.WithLogger(l) }

// WithVersion sets the version reported by /healthz, /api/v1/version and metrics.
func WithVersion(v string) Option { return server.WithVersion(v) }

// WithBuildInfo sets the commit and build date reported by /api/v1/version.
func WithBuildInfo(commit, date string) Option { return server.WithBuildInfo(commit, date) }

// WithStdout sets where the one-time dev bootstrap API key is printed.
func WithStdout(w io.Writer) Option { return server.WithStdout(w) }

// WithPollInterval sets how often the dispatcher looks at the queue (default 2s).
func WithPollInterval(d time.Duration) Option { return server.WithPollInterval(d) }

// WithLoginProvider adds an interactive login method, mounted at
// /auth/{name}/login and /auth/{name}/callback and listed on the login page.
func WithLoginProvider(p LoginProvider) Option { return server.WithLoginProvider(p) }

// WithAuthorizer replaces the built-in role authorizer for every REST API
// route and the auth service.
func WithAuthorizer(a Authorizer) Option { return server.WithAuthorizer(a) }

// WithAuditSink receives security events (logins, logouts, session issuance).
func WithAuditSink(a AuditSink) Option { return server.WithAuditSink(a) }

// WithTenantResolver picks the team of a user logging in (default: one team
// per install).
func WithTenantResolver(t TenantResolver) Option { return server.WithTenantResolver(t) }

// WithStore uses an already opened store instead of cfg.DatabaseURL. The
// server closes it on Close. A store without coordination support runs as a
// single replica.
func WithStore(s Store) Option { return server.WithStore(s) }

// WithObjectStore stores all run data (transcripts, artifacts, attachments)
// in s instead of the stores named by OF_OBJECT_STORE_*.
func WithObjectStore(s ObjectStore) Option { return server.WithObjectStore(s) }

// WithPlugins launches extra gRPC plugin executables, in addition to
// OF_PLUGIN_DIR and OF_PLUGINS.
func WithPlugins(paths ...string) Option { return server.WithPlugins(paths...) }

// WithEventBus replaces the built-in durable event bus (database outbox plus
// live notifications) for every component, for example with a message
// broker. A bus that also implements Queue gets at-least-once inbound event
// handling (the engine acknowledges a job only after handling it); otherwise
// the engine subscribes to TopicInboundEvent. A bus that implements
// io.Closer is closed on Close. Durable topics (DurableTopics) must be
// delivered at least once to one replica; live topics fan out to every
// replica.
func WithEventBus(b EventBus) Option { return server.WithEventBus(b) }

// WithQueue routes the durable topics (inbound events and outbound notices)
// through q instead of the built-in database outbox; live topics still use
// the event bus. q must hand every job to one replica at least once and
// retry it when a handler fails. The server does not close q.
func WithQueue(q Queue) Option { return server.WithQueue(q) }

// WithLeadership replaces the built-in database leader lease that gates the
// scheduler, git definition polling and run-data retention
// to one replica. If l has a Start(context.Context) method it is called by
// Start; if it has a Done() <-chan struct{} method Close waits for it.
func WithLeadership(l Leadership) Option { return server.WithLeadership(l) }

// WithKeyProvider replaces the local key provider (OF_SECRET_KEY and
// OF_SECRET_KEY_PREVIOUS) that wraps the data keys of the secret store. A
// provider for a key management service can live in another module.
// OF_SIGNING_KEY still signs sessions and tokens.
func WithKeyProvider(p KeyProvider) Option { return server.WithKeyProvider(p) }

// WithRoutes registers extra HTTP routes. They are mounted after the built-in
// routes and before the dashboard catch-all, so they may not reuse a
// built-in pattern.
func WithRoutes(register func(mux *http.ServeMux)) Option { return server.WithRoutes(register) }

// ---------- core types ----------

type (
	// Store persists all control-plane state.
	Store = core.Store
	// KeyProvider wraps and unwraps the data keys of the secret store.
	KeyProvider = core.KeyProvider
	// SecretStore holds the encrypted secrets of the teams and factories.
	SecretStore = core.SecretStore
	// SecretType is the kind of a secret.
	SecretType = core.SecretType
	// Integration is an integration record: one connection to a provider,
	// with its config and the name of the team secret of its credential.
	Integration = core.Integration
	// User is a team member.
	User = core.User
	// Team is a tenant.
	Team = core.Team
	// Role is a built-in role: admin, editor or user.
	Role = core.Role
	// APIKey is a team API key (the secret is never stored).
	APIKey = core.APIKey

	// EventBus publishes events by topic (see WithEventBus).
	EventBus = core.EventBus
	// Queue is a durable, at-least-once work queue (see WithQueue).
	Queue = core.Queue
	// Job is one durable message handed to a JobHandler.
	Job = core.Job
	// JobHandler processes a Job; returning an error schedules a retry.
	JobHandler = core.JobHandler
	// Leadership reports whether this replica is the leader (see WithLeadership).
	Leadership = core.Leadership
	// Event is an inbound event (payload of TopicInboundEvent).
	Event = core.Event
	// OutboundMessage is an outbound notice (payload of TopicOutboundNotice).
	OutboundMessage = core.OutboundMessage
)

// Event bus topics. Durable topics are delivered at least once through the
// Queue; the others are live, lossy fan-out notifications.
const (
	TopicInboundEvent   = core.TopicInboundEvent
	TopicOutboundNotice = core.TopicOutboundNotice
	TopicRunUpdated     = core.TopicRunUpdated
	TopicRunMessage     = core.TopicRunMessage
	TopicWorkItem       = core.TopicWorkItem
	TopicInbox          = core.TopicInbox
	TopicDefinition     = core.TopicDefinition
)

// SecretGeneric is the type of a secret with no special use, for example the
// key of an app or a webhook secret.
const SecretGeneric = core.SecretGeneric

// DurableTopics lists the topics delivered through the Queue.
func DurableTopics() []string { return append([]string(nil), core.DurableTopics...) }

// Built-in roles.
const (
	RoleAdmin  = core.RoleAdmin
	RoleEditor = core.RoleEditor
	RoleUser   = core.RoleUser
)

// Sentinel errors. Authorizers return errors wrapping ErrForbidden (403) or
// ErrUnauthorized (401).
var (
	ErrNotFound     = core.ErrNotFound
	ErrConflict     = core.ErrConflict
	ErrInvalid      = core.ErrInvalid
	ErrUnauthorized = core.ErrUnauthorized
	ErrForbidden    = core.ErrForbidden
)
