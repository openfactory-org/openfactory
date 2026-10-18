package openfactory

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/auth"
	"github.com/openfactory-org/openfactory/internal/objectstore"
)

// ---------- authentication and authorization ----------

type (
	// Principal is the authenticated caller of a request.
	Principal = auth.Principal
	// Identity is a user asserted by a login provider.
	Identity = auth.Identity
	// LoginProvider is an interactive login method (OIDC in the core; SAML
	// and others in extensions).
	LoginProvider = auth.LoginProvider
	// LoginCompleter is handed to login providers: it provisions users,
	// issues sessions and records audit events.
	LoginCompleter = auth.LoginCompleter
	// LoginError is a login failure with a code shown on the login page.
	LoginError = auth.LoginError
	// Authorizer decides whether a principal may perform an action.
	Authorizer = auth.Authorizer
	// Action is the operation being authorized ("read", "operate", "write",
	// "admin", optionally with a ":verb" suffix).
	Action = auth.Action
	// Resource identifies what an action applies to.
	Resource = auth.Resource
	// AuditSink receives audit events.
	AuditSink = auth.AuditSink
	// AuditEvent is one security-relevant event.
	AuditEvent = auth.AuditEvent
	// TenantResolver picks the team of a user logging in.
	TenantResolver = auth.TenantResolver
	// RoleAuthorizer is the built-in authorizer (admin > editor > user). It
	// applies the role override of a user for one factory (Resource.FactoryID).
	RoleAuthorizer = auth.RoleAuthorizer
	// SlogAuditSink is the built-in audit sink (structured logs).
	SlogAuditSink = auth.SlogAuditSink
	// SingleTeamResolver is the built-in tenant resolver (one team per install).
	SingleTeamResolver = auth.SingleTeamResolver
	// OIDCConfig configures the built-in OIDC login provider.
	OIDCConfig = auth.OIDCConfig
)

// Action categories.
const (
	ActionRead    = auth.ActionRead
	ActionOperate = auth.ActionOperate
	ActionWrite   = auth.ActionWrite
	ActionAdmin   = auth.ActionAdmin
)

// Audit outcomes.
const (
	OutcomeSuccess = auth.OutcomeSuccess
	OutcomeFailure = auth.OutcomeFailure
)

// Authentication methods (Principal.Method).
const (
	MethodSession = auth.MethodSession
	MethodAPIKey  = auth.MethodAPIKey
)

// PrincipalFrom returns the principal attached to a request context by the
// auth middleware, or nil.
func PrincipalFrom(ctx context.Context) *Principal { return auth.PrincipalFrom(ctx) }

// NewOIDCProvider builds an OIDC login provider (for example a second IdP
// registered with WithLoginProvider).
func NewOIDCProvider(cfg OIDCConfig) (LoginProvider, error) { return auth.NewOIDCProvider(cfg) }

// ---------- object storage ----------

type (
	// ObjectStore is a run-data object store (S3, GCS, Azure Blob, local disk
	// or a custom implementation).
	ObjectStore = objectstore.Store
	// ObjectInfo describes a stored object.
	ObjectInfo = objectstore.ObjectInfo
	// ObjectStoreOptions configures OpenObjectStore.
	ObjectStoreOptions = objectstore.Options
)

// OpenObjectStore opens a built-in object store by URL: s3://bucket/prefix,
// gs://bucket/prefix, azblob://account/container/prefix or file:///path.
func OpenObjectStore(ctx context.Context, rawURL string, opts ObjectStoreOptions) (ObjectStore, error) {
	return objectstore.Open(ctx, rawURL, opts)
}
