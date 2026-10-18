// Package auth is the login, session and authorization layer of Open Factory.
//
// It defines the extension points that the enterprise build plugs into
// (LoginProvider, Authenticator, Authorizer, AuditSink, TenantResolver) and
// ships the free-core implementations: OIDC login, encrypted cookie sessions,
// API-key sessions for the dashboard, the admin/editor/user role
// authorizer, a slog audit sink and a single-team tenant resolver.
//
// These interfaces will be re-exported under pkg/ so that modules outside
// this repository can implement them.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Authentication methods recorded on a Principal.
const (
	MethodSession = "session" // of_session cookie
	MethodAPIKey  = "api_key" // Authorization: Bearer of_...
)

// Provider names recorded on sessions and audit events.
const (
	ProviderAPIKey = "api_key"
	ProviderOIDC   = "oidc"
)

var (
	// ErrInvalidCredentials is returned when presented credentials (a bearer
	// key or an API key submitted for login) are unknown, malformed or revoked.
	ErrInvalidCredentials = fmt.Errorf("%w: invalid credentials", core.ErrUnauthorized)
	// ErrCSRF is returned for a cookie-authenticated unsafe request that
	// carries neither the X-OF-CSRF header nor a same-origin marker.
	ErrCSRF = fmt.Errorf("%w: missing %s header", core.ErrForbidden, CSRFHeader)
)

// Principal is the authenticated caller of a request.
type Principal struct {
	User   *core.User
	TeamID string
	Role   core.Role
	// Method is MethodSession or MethodAPIKey.
	Method string
	// Provider is the login provider that created the session ("oidc",
	// "api_key", ...). For bearer requests it is ProviderAPIKey.
	Provider string
	// APIKey is set for bearer-authenticated requests only.
	APIKey *core.APIKey
}

// RoleFor returns the effective role of the principal for one factory: the
// role override of the user for that factory if one exists, else the
// team-wide Role. An empty factoryID returns the team-wide Role.
func (p *Principal) RoleFor(factoryID string) core.Role {
	if p == nil {
		return ""
	}
	if factoryID != "" && p.User != nil {
		if r, ok := p.User.FactoryRoles[factoryID]; ok && ValidRole(r) {
			return core.NormalizeRole(r)
		}
	}
	return core.NormalizeRole(p.Role)
}

// UserID returns the authenticated user's ID ("" when unknown).
func (p *Principal) UserID() string {
	if p == nil || p.User == nil {
		return ""
	}
	return p.User.ID
}

// ActorID returns the identity recorded as actor on engine operations.
func (p *Principal) ActorID() string {
	if p == nil || p.User == nil {
		return "anonymous"
	}
	return "user:" + p.User.ID
}

// CookieAuthenticated reports whether the request was authenticated by the
// session cookie (and therefore needs CSRF protection).
func (p *Principal) CookieAuthenticated() bool { return p != nil && p.Method == MethodSession }

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored in ctx, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// ---------- extension interfaces ----------

// LoginProvider is an interactive login method (OIDC in the free core; SAML
// and others in the enterprise build). Its handlers are mounted at
// GET /auth/{Name}/login and GET /auth/{Name}/callback.
type LoginProvider interface {
	// Name is the URL-safe identifier, e.g. "oidc".
	Name() string
	// DisplayName labels the login button, e.g. "Single sign-on".
	DisplayName() string
	// LoginHandler starts the login (usually a redirect to the IdP). The
	// "return_to" query parameter carries the local path to land on afterwards.
	LoginHandler(c LoginCompleter) http.Handler
	// CallbackHandler finishes the login and must end by calling exactly one
	// of c.CompleteLogin or c.FailLogin.
	CallbackHandler(c LoginCompleter) http.Handler
}

// LoginCompleter is implemented by Service and handed to login providers. It
// owns user provisioning, session issuance and auditing, so every provider
// behaves the same way.
type LoginCompleter interface {
	// CompleteLogin provisions or links the user for id, starts a session and
	// redirects to returnTo (a local path; anything else falls back to "/").
	CompleteLogin(w http.ResponseWriter, r *http.Request, id Identity, returnTo string)
	// FailLogin records the failure and redirects to the login error page.
	FailLogin(w http.ResponseWriter, r *http.Request, provider string, err error)
	// SaveFlow stores short-lived provider state (state, nonce, PKCE verifier,
	// return path...) JSON-encoded in an encrypted, HttpOnly cookie scoped to
	// /auth/{provider}/.
	SaveFlow(w http.ResponseWriter, provider string, v any, ttl time.Duration) error
	// LoadFlow decodes the state saved by SaveFlow into v and clears the
	// cookie. It fails when the cookie is missing, tampered with or expired.
	LoadFlow(w http.ResponseWriter, r *http.Request, provider string, v any) error
}

// Identity is an externally authenticated user, as asserted by a provider.
type Identity struct {
	Provider      string // provider name, stored as core.ExternalIdentity.Provider
	Subject       string // stable IdP subject, stored as core.ExternalIdentity.ExternalID
	Email         string
	EmailVerified bool
	Name          string
	Handle        string // preferred username, if any
	Groups        []string
	// Role is the role the provider mapped from the IdP (groups). It is only
	// applied when RoleManaged is true; otherwise new users get DefaultRole and
	// existing users keep the role stored in Open Factory.
	Role        core.Role
	RoleManaged bool
	DefaultRole core.Role
}

// Authenticator resolves the caller of a request. It returns (nil, nil) when
// the request carries no credentials it recognizes.
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// Action is an operation to authorize. It is "<category>" or
// "<category>:<verb>", e.g. "write" or "write:factory.update". The default
// RoleAuthorizer only looks at the category; fine-grained verbs exist for
// custom-role authorizers.
type Action string

// Action categories. Every existing API route maps onto one of them:
// user routes that only read are ActionRead, user routes that mutate the
// caller's own work (dispatching runs, messages, own API keys) are
// ActionOperate, editor routes are ActionWrite and admin routes ActionAdmin.
const (
	ActionRead    Action = "read"
	ActionOperate Action = "operate"
	ActionWrite   Action = "write"
	ActionAdmin   Action = "admin"
)

// Category returns the part of the action before the first ':'.
func (a Action) Category() Action {
	for i := 0; i < len(a); i++ {
		if a[i] == ':' {
			return a[:i]
		}
	}
	return a
}

// ActionForRole converts the minimum role of an existing API route into an
// action. User routes with unsafe methods map to ActionOperate.
func ActionForRole(method string, min core.Role) Action {
	switch min {
	case core.RoleAdmin:
		return ActionAdmin
	case core.RoleEditor:
		return ActionWrite
	default:
		if isSafeMethod(method) {
			return ActionRead
		}
		return ActionOperate
	}
}

// Resource identifies what an action applies to. Empty fields are wildcards.
type Resource struct {
	Type   string // "factory", "run", "secret", ...
	ID     string
	TeamID string // owning team; must match the principal's team when set
	// FactoryID is the factory the request is about (the factory itself, or
	// the factory of a work item, inbox item, run or webhook). Empty for
	// team-wide requests. The role check uses Principal.RoleFor(FactoryID).
	FactoryID string
}

// Authorizer decides whether a principal may perform an action. It returns
// nil to allow, or an error wrapping core.ErrForbidden (or
// core.ErrUnauthorized for a nil principal).
type Authorizer interface {
	Allow(ctx context.Context, p *Principal, a Action, res Resource) error
}

// AuditEvent is one security-relevant event.
type AuditEvent struct {
	Action     string            `json:"action"`  // "auth.login", "auth.logout", "auth.session", ...
	Outcome    string            `json:"outcome"` // OutcomeSuccess or OutcomeFailure
	ActorID    string            `json:"actor_id,omitempty"`
	UserID     string            `json:"user_id,omitempty"`
	TeamID     string            `json:"team_id,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Resource   *Resource         `json:"resource,omitempty"`
	RemoteAddr string            `json:"remote_addr,omitempty"`
	UserAgent  string            `json:"user_agent,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Detail     map[string]string `json:"detail,omitempty"`
}

// Audit outcomes.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// AuditSink receives audit events. Implementations must not block for long;
// the free core logs them (no export or retention).
type AuditSink interface {
	Record(ctx context.Context, e AuditEvent)
}

// TenantResolver picks the team a logging-in identity belongs to.
type TenantResolver interface {
	ResolveTeam(ctx context.Context, r *http.Request, id Identity) (*core.Team, error)
}

// errorCode extracts the stable code of a login error ("internal" otherwise).
func errorCode(err error) string {
	var le *LoginError
	if errors.As(err, &le) {
		return le.Code
	}
	return "internal"
}

// LoginError is a login failure with a stable, user-presentable code. The code
// is appended to the login error URL as ?error=<code>.
type LoginError struct {
	Code string
	Err  error
}

func (e *LoginError) Error() string {
	if e.Err == nil {
		return "login failed: " + e.Code
	}
	return "login failed: " + e.Code + ": " + e.Err.Error()
}

func (e *LoginError) Unwrap() error { return e.Err }

// Login error codes.
const (
	LoginErrAccessDenied        = "access_denied"
	LoginErrInvalidState        = "invalid_state"
	LoginErrNonceMismatch       = "nonce_mismatch"
	LoginErrDomainNotAllowed    = "domain_not_allowed"
	LoginErrEmailNotVerified    = "email_not_verified"
	LoginErrProviderError       = "provider_error"
	LoginErrProviderUnavailable = "provider_unavailable"
	LoginErrAccountConflict     = "account_conflict"
)

func loginErr(code string, err error) *LoginError { return &LoginError{Code: code, Err: err} }

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}
