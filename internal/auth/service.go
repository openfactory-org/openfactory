package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Cookie, header and route contract shared with the dashboard.
const (
	SessionCookie = "of_session"
	CSRFHeader    = "X-OF-CSRF"

	RouteSession = "/auth/session"
	RouteLogout  = "/auth/logout"
	RouteMe      = "/api/v1/me"
)

// APIKeyLookup validates a plaintext "of_..." API key and returns its owner and
// key record. It must return an error wrapping core.ErrUnauthorized (or
// core.ErrNotFound) for unknown, malformed or revoked keys. The REST API's
// key authenticator fits after a small adapter.
type APIKeyLookup func(ctx context.Context, key string) (*core.User, *core.APIKey, error)

// Config configures a Service.
type Config struct {
	Store core.Store // required
	// SessionKey is the 32-byte master key for session and login-flow cookies.
	// It must be the same on every replica and stable across restarts.
	SessionKey []byte // required
	// PreviousSessionKeys only open cookies that earlier keys sealed (key rotation).
	PreviousSessionKeys [][]byte
	// PublicURL is the external base URL. An https URL makes cookies Secure;
	// its origin is accepted by the same-origin CSRF check.
	PublicURL string
	// SessionTTL is the absolute lifetime of a session (default 7 days).
	SessionTTL time.Duration
	// APIKeyLookup enables bearer authentication and POST /auth/session.
	APIKeyLookup APIKeyLookup
	// Providers are the interactive login methods (e.g. the OIDC provider).
	Providers []LoginProvider

	Authorizer Authorizer     // default RoleAuthorizer{}
	Audit      AuditSink      // default SlogAuditSink
	Tenants    TenantResolver // default *SingleTeamResolver{Create: true}

	// LoginErrorURL is where failed interactive logins land, with
	// ?error=<code> appended (default "/login").
	LoginErrorURL string
	// MountMe also mounts GET /api/v1/me on Register. Leave false when the
	// REST API serves /me itself (it must not be registered twice).
	MountMe bool

	Logger *slog.Logger
	Now    func() time.Time
}

// Service implements sessions, the auth routes, Authenticator and
// LoginCompleter.
type Service struct {
	cfg       Config
	log       *slog.Logger
	sessions  *sealer
	flows     *sealer
	secure    bool
	origin    string // scheme://host of PublicURL ("" when unset)
	providers []LoginProvider
}

var (
	_ Authenticator  = (*Service)(nil)
	_ LoginCompleter = (*Service)(nil)
)

// New constructs a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth: Config.Store is required")
	}
	sessions, err := newSealer(cfg.SessionKey, "session", cfg.PreviousSessionKeys...)
	if err != nil {
		return nil, err
	}
	flows, err := newSealer(cfg.SessionKey, "login-flow", cfg.PreviousSessionKeys...)
	if err != nil {
		return nil, err
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 7 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Authorizer == nil {
		cfg.Authorizer = RoleAuthorizer{}
	}
	if cfg.Audit == nil {
		cfg.Audit = SlogAuditSink{Logger: cfg.Logger}
	}
	if cfg.Tenants == nil {
		cfg.Tenants = &SingleTeamResolver{Store: cfg.Store, Create: true, Now: cfg.Now}
	}
	if cfg.LoginErrorURL == "" {
		cfg.LoginErrorURL = "/login"
	}
	s := &Service{cfg: cfg, log: cfg.Logger.With("component", "auth"), sessions: sessions, flows: flows}
	if cfg.PublicURL != "" {
		u, err := url.Parse(strings.TrimRight(cfg.PublicURL, "/"))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("auth: PublicURL %q must be an absolute http(s) URL", cfg.PublicURL)
		}
		s.secure = u.Scheme == "https"
		s.origin = u.Scheme + "://" + u.Host
	}
	seen := map[string]bool{}
	for _, p := range cfg.Providers {
		n := p.Name()
		if n == "" || strings.ContainsAny(n, "/?#{} ") || n == "session" || n == "logout" {
			return nil, fmt.Errorf("auth: invalid login provider name %q", n)
		}
		if seen[n] {
			return nil, fmt.Errorf("auth: duplicate login provider %q", n)
		}
		seen[n] = true
		s.providers = append(s.providers, p)
	}
	return s, nil
}

// Authorizer returns the configured authorizer.
func (s *Service) Authorizer() Authorizer { return s.cfg.Authorizer }

// Audit returns the configured audit sink.
func (s *Service) Audit() AuditSink { return s.cfg.Audit }

func (s *Service) now() time.Time { return s.cfg.Now() }

// ---------- routes ----------

// Register mounts the auth routes on mux:
//
//	POST /auth/session            API-key login (JSON {"api_key": "of_..."})
//	POST /auth/logout             clear the session
//	GET  /auth/{provider}/login   start an interactive login (?return_to=/path)
//	GET  /auth/{provider}/callback
//	GET  /api/v1/me               only when Config.MountMe is true
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+RouteSession, s.handleSessionLogin)
	mux.HandleFunc("POST "+RouteLogout, s.handleLogout)
	for _, p := range s.providers {
		mux.Handle("GET /auth/"+p.Name()+"/login", p.LoginHandler(s))
		mux.Handle("GET /auth/"+p.Name()+"/callback", p.CallbackHandler(s))
	}
	if s.cfg.MountMe {
		mux.Handle("GET "+RouteMe, s.Middleware(http.HandlerFunc(s.HandleMe)))
	}
}

// ProviderInfo describes a login provider for the dashboard's login page.
type ProviderInfo struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	LoginURL    string `json:"login_url"`
}

// LoginProviders lists the interactive login providers plus "api_key" when
// API-key sessions are enabled (its LoginURL is the POST /auth/session route).
func (s *Service) LoginProviders(*http.Request) []ProviderInfo {
	out := make([]ProviderInfo, 0, len(s.providers)+1)
	for _, p := range s.providers {
		out = append(out, ProviderInfo{Name: p.Name(), DisplayName: p.DisplayName(), LoginURL: "/auth/" + p.Name() + "/login"})
	}
	if s.cfg.APIKeyLookup != nil {
		out = append(out, ProviderInfo{Name: ProviderAPIKey, DisplayName: "API key", LoginURL: RouteSession})
	}
	return out
}

// ---------- sessions ----------

type sessionClaims struct {
	UserID    string `json:"u"`
	TeamID    string `json:"t"`
	Provider  string `json:"p"`
	KeyID     string `json:"k,omitempty"`  // API-key sessions: the key that created it
	KeyPrefix string `json:"kp,omitempty"` // and its lookup prefix
	IssuedAt  int64  `json:"iat"`
}

// IssueSession sets the session cookie for user. provider names the login
// method; key is the API key that created the session, if any (the session
// ends when the key is deleted).
func (s *Service) IssueSession(w http.ResponseWriter, u *core.User, provider string, key *core.APIKey) error {
	now := s.now()
	exp := now.Add(s.cfg.SessionTTL)
	c := sessionClaims{UserID: u.ID, TeamID: u.TeamID, Provider: provider, IssuedAt: now.Unix()}
	if key != nil {
		c.KeyID, c.KeyPrefix = key.ID, key.Prefix
	}
	v, err := s.sessions.seal(c, SessionCookie, exp)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite are set; Secure follows the https public URL, so plain-http local use works
		Name: SessionCookie, Value: v, Path: "/", Expires: exp, MaxAge: int(s.cfg.SessionTTL / time.Second),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// ClearSession expires the session cookie.
func (s *Service) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite are set; Secure follows the https public URL, so plain-http local use works
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// sessionPrincipal decodes the session cookie. It returns (nil, nil) for a
// missing, tampered, expired or stale session.
func (s *Service) sessionPrincipal(r *http.Request) (*Principal, error) {
	ck, err := r.Cookie(SessionCookie)
	if err != nil || ck.Value == "" {
		return nil, nil //nolint:nilerr // a missing session cookie means no session, not an error
	}
	var c sessionClaims
	if err := s.sessions.open(ck.Value, SessionCookie, s.now(), &c); err != nil {
		return nil, nil //nolint:nilerr // a tampered or expired session cookie means no session, not an error
	}
	ctx := r.Context()
	u, err := s.cfg.Store.GetUser(ctx, c.UserID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if u.TeamID != c.TeamID {
		return nil, nil
	}
	if c.KeyID != "" {
		k, err := s.cfg.Store.GetAPIKeyByPrefix(ctx, c.KeyPrefix)
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		if k.ID != c.KeyID || k.UserID != u.ID {
			return nil, nil
		}
	}
	return &Principal{User: u, TeamID: u.TeamID, Role: effectiveRole(u.Role), Method: MethodSession, Provider: c.Provider}, nil
}

func effectiveRole(r core.Role) core.Role {
	if ValidRole(r) {
		return core.NormalizeRole(r)
	}
	return core.RoleUser
}

// ---------- Authenticator ----------

// Authenticate implements Authenticator. It tries the session cookie first,
// then "Authorization: Bearer of_..." (or "ofa_..." for a service account).
// Bearer tokens without one of these prefixes are ignored (left to other authenticators). It returns (nil, nil) without
// credentials, ErrInvalidCredentials for a bad bearer key and ErrCSRF for a
// cookie-authenticated unsafe request that fails the CSRF check.
func (s *Service) Authenticate(r *http.Request) (*Principal, error) {
	if tok := bearerToken(r); strings.HasPrefix(tok, "of_") || strings.HasPrefix(tok, "ofa_") {
		return s.apiKeyPrincipal(r.Context(), tok)
	}
	p, err := s.sessionPrincipal(r)
	if err != nil || p == nil {
		return nil, err
	}
	if !isSafeMethod(r.Method) && !s.CSRFOK(r) {
		return nil, ErrCSRF
	}
	return p, nil
}

func (s *Service) apiKeyPrincipal(ctx context.Context, key string) (*Principal, error) {
	if s.cfg.APIKeyLookup == nil {
		return nil, ErrInvalidCredentials
	}
	u, k, err := s.lookupKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return &Principal{User: u, TeamID: k.TeamID, Role: effectiveRole(u.Role), Method: MethodAPIKey, Provider: ProviderAPIKey, APIKey: k}, nil
}

func (s *Service) lookupKey(ctx context.Context, key string) (*core.User, *core.APIKey, error) {
	u, k, err := s.cfg.APIKeyLookup(ctx, key)
	if err != nil {
		if errors.Is(err, core.ErrUnauthorized) || errors.Is(err, core.ErrNotFound) {
			return nil, nil, ErrInvalidCredentials
		}
		return nil, nil, err
	}
	if u == nil || k == nil || u.TeamID != k.TeamID || k.UserID != u.ID {
		return nil, nil, ErrInvalidCredentials
	}
	return u, k, nil
}

func bearerToken(r *http.Request) string {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// ---------- CSRF ----------

// CSRFOK reports whether an unsafe request proves it comes from our own pages:
// it carries "X-OF-CSRF: 1" (a custom header cannot be sent cross-origin
// without a CORS preflight, which the server never grants), or its Origin is
// the public URL's origin (or the request's own host), or the browser marks
// it Sec-Fetch-Site: same-origin.
func (s *Service) CSRFOK(r *http.Request) bool {
	if r.Header.Get(CSRFHeader) == "1" {
		return true
	}
	if o := r.Header.Get("Origin"); o != "" && o != "null" {
		if s.origin != "" && strings.EqualFold(o, s.origin) {
			return true
		}
		if u, err := url.Parse(o); err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host) {
			return true
		}
		return false
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// RequireCSRF rejects cookie-authenticated unsafe requests that fail CSRFOK.
// Requests without a session cookie, or with a bearer key, pass through.
// Middleware already applies it; use it standalone for handlers mounted
// outside Middleware.
func (s *Service) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) && bearerToken(r) == "" && !s.CSRFOK(r) {
			if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" {
				writeProblem(w, r, http.StatusForbidden, "not_authorized", "missing "+CSRFHeader+": 1 header", nil)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- middleware ----------

// Middleware authenticates every request and attaches the Principal (if any)
// to its context; read it with PrincipalFrom. Anonymous requests pass through
// (handlers decide). An invalid bearer key gets 401 and a failed CSRF check
// gets 403, both as problem+json. An invalid or expired session cookie is
// treated as anonymous and cleared.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.Authenticate(r)
		switch {
		case errors.Is(err, ErrCSRF):
			writeProblem(w, r, http.StatusForbidden, "not_authorized", "missing "+CSRFHeader+": 1 header", nil)
			return
		case errors.Is(err, core.ErrUnauthorized):
			writeProblem(w, r, http.StatusUnauthorized, "authentication_required", "invalid API key", nil)
			return
		case err != nil:
			s.log.Error("auth: authenticate", "err", err)
			writeProblem(w, r, http.StatusInternalServerError, "internal_error", "", nil)
			return
		}
		if p == nil {
			if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" {
				s.ClearSession(w)
			}
		} else {
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- handlers ----------

// HandleMe serves GET /api/v1/me: {user, team, role, auth_method, provider,
// login_providers}. Without a principal in the context it answers 401
// problem+json that still lists login_providers. Mount it behind Middleware.
func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p == nil {
		writeProblem(w, r, http.StatusUnauthorized, "authentication_required", "not logged in",
			map[string]any{"login_providers": s.LoginProviders(r)})
		return
	}
	s.writeMe(w, r, p)
}

func (s *Service) writeMe(w http.ResponseWriter, r *http.Request, p *Principal) {
	team, err := s.cfg.Store.GetTeam(r.Context(), p.TeamID)
	if err != nil {
		s.log.Error("auth: load team", "team_id", p.TeamID, "err", err)
		writeProblem(w, r, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": p.User, "team": team, "role": p.Role,
		"auth_method": p.Method, "provider": p.Provider,
		"login_providers": s.LoginProviders(r),
	})
}

type sessionRequest struct {
	APIKey string `json:"api_key"`
}

func (s *Service) handleSessionLogin(w http.ResponseWriter, r *http.Request) {
	ev := s.event(r, "auth.session", ProviderAPIKey)
	// Login CSRF: a cross-site form must not log the victim into another account.
	if !s.CSRFOK(r) {
		writeProblem(w, r, http.StatusForbidden, "not_authorized", "missing "+CSRFHeader+": 1 header", nil)
		return
	}
	if s.cfg.APIKeyLookup == nil {
		writeProblem(w, r, http.StatusNotImplemented, "feature_not_available", "API-key login is not enabled", nil)
		return
	}
	var req sessionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.APIKey) == "" {
		writeProblem(w, r, http.StatusBadRequest, "invalid_request", `body must be JSON {"api_key": "of_..."}`, nil)
		return
	}
	u, k, err := s.lookupKey(r.Context(), strings.TrimSpace(req.APIKey))
	if err != nil {
		ev.Outcome, ev.Reason = OutcomeFailure, err.Error()
		s.cfg.Audit.Record(r.Context(), ev)
		if errors.Is(err, core.ErrUnauthorized) {
			writeProblem(w, r, http.StatusUnauthorized, "authentication_required", "invalid API key", nil)
			return
		}
		s.log.Error("auth: api key lookup", "err", err)
		writeProblem(w, r, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	if err := s.IssueSession(w, u, ProviderAPIKey, k); err != nil {
		s.log.Error("auth: issue session", "err", err)
		writeProblem(w, r, http.StatusInternalServerError, "internal_error", "", nil)
		return
	}
	ev.Outcome, ev.UserID, ev.TeamID, ev.ActorID = OutcomeSuccess, u.ID, u.TeamID, "user:"+u.ID
	ev.Detail = map[string]string{"api_key_id": k.ID}
	s.cfg.Audit.Record(r.Context(), ev)
	s.writeMe(w, r, &Principal{User: u, TeamID: u.TeamID, Role: effectiveRole(u.Role), Method: MethodSession, Provider: ProviderAPIKey})
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.CSRFOK(r) {
		writeProblem(w, r, http.StatusForbidden, "not_authorized", "missing "+CSRFHeader+": 1 header", nil)
		return
	}
	ev := s.event(r, "auth.logout", "")
	if p, _ := s.sessionPrincipal(r); p != nil {
		ev.UserID, ev.TeamID, ev.ActorID, ev.Provider = p.UserID(), p.TeamID, p.ActorID(), p.Provider
	}
	s.ClearSession(w)
	ev.Outcome = OutcomeSuccess
	s.cfg.Audit.Record(r.Context(), ev)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) event(r *http.Request, action, provider string) AuditEvent {
	return AuditEvent{Action: action, Provider: provider, RemoteAddr: r.RemoteAddr, UserAgent: r.UserAgent()}
}

// ---------- LoginCompleter ----------

// CompleteLogin implements LoginCompleter: it resolves the team, provisions
// or links the user just in time, starts a session and redirects.
func (s *Service) CompleteLogin(w http.ResponseWriter, r *http.Request, id Identity, returnTo string) {
	u, err := s.provision(r, id)
	if err != nil {
		s.FailLogin(w, r, id.Provider, err)
		return
	}
	if err := s.IssueSession(w, u, id.Provider, nil); err != nil {
		s.FailLogin(w, r, id.Provider, err)
		return
	}
	ev := s.event(r, "auth.login", id.Provider)
	ev.Outcome, ev.UserID, ev.TeamID, ev.ActorID = OutcomeSuccess, u.ID, u.TeamID, "user:"+u.ID
	ev.Detail = map[string]string{"subject": id.Subject, "role": string(u.Role)}
	s.cfg.Audit.Record(r.Context(), ev)
	http.Redirect(w, r, SafeReturnTo(returnTo), http.StatusSeeOther)
}

// FailLogin implements LoginCompleter.
func (s *Service) FailLogin(w http.ResponseWriter, r *http.Request, provider string, err error) {
	code := errorCode(err)
	ev := s.event(r, "auth.login", provider)
	ev.Outcome, ev.Reason = OutcomeFailure, err.Error()
	s.cfg.Audit.Record(r.Context(), ev)
	if code == "internal" {
		s.log.Error("auth: login failed", "provider", provider, "err", err)
	}
	target := s.cfg.LoginErrorURL
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	http.Redirect(w, r, target+sep+"error="+url.QueryEscape(code), http.StatusSeeOther)
}

// provision finds the user for an external identity (by identity link, then by
// verified email) or creates one, and keeps name and IdP-managed role in sync.
func (s *Service) provision(r *http.Request, id Identity) (*core.User, error) {
	ctx := r.Context()
	st := s.cfg.Store
	if id.Provider == "" || id.Subject == "" {
		return nil, errors.New("auth: identity without provider or subject")
	}
	team, err := s.cfg.Tenants.ResolveTeam(ctx, r, id)
	if err != nil {
		return nil, fmt.Errorf("auth: resolve team: %w", err)
	}

	var u *core.User
	x, err := st.FindExternalIdentity(ctx, id.Provider, id.Subject)
	switch {
	case err == nil:
		if u, err = st.GetUser(ctx, x.UserID); err != nil && !errors.Is(err, core.ErrNotFound) {
			return nil, err
		}
	case !errors.Is(err, core.ErrNotFound):
		return nil, err
	}
	if u == nil && id.Email != "" {
		existing, err := st.GetUserByEmail(ctx, id.Email)
		switch {
		case err == nil:
			// Linking to an existing account is only safe on a verified email.
			if !id.EmailVerified {
				return nil, loginErr(LoginErrEmailNotVerified, fmt.Errorf("email %s is not verified by the identity provider", id.Email))
			}
			u = existing
		case !errors.Is(err, core.ErrNotFound):
			return nil, err
		}
	}

	if u == nil {
		role := id.DefaultRole
		if id.RoleManaged {
			role = id.Role
		}
		u = &core.User{
			ID: core.NewID("usr"), TeamID: team.ID, Email: id.Email, Name: displayName(id),
			Role: effectiveRole(role), CreatedAt: s.now().UTC(),
		}
		if err := st.CreateUser(ctx, u); err != nil {
			return nil, fmt.Errorf("auth: create user: %w", err)
		}
	} else {
		if u.TeamID != team.ID {
			return nil, loginErr(LoginErrAccountConflict, fmt.Errorf("user %s belongs to another team", u.ID))
		}
		changed := false
		if n := displayName(id); n != "" && n != u.Name {
			u.Name, changed = n, true
		}
		if id.RoleManaged && ValidRole(id.Role) && id.Role != u.Role {
			u.Role, changed = id.Role, true
		}
		if changed {
			if err := st.UpdateUser(ctx, u); err != nil {
				return nil, fmt.Errorf("auth: update user: %w", err)
			}
		}
	}
	link := &core.ExternalIdentity{UserID: u.ID, Provider: id.Provider, ExternalID: id.Subject, Handle: id.Handle}
	if link.Handle == "" {
		link.Handle = id.Email
	}
	if err := st.UpsertExternalIdentity(ctx, link); err != nil {
		return nil, fmt.Errorf("auth: link identity: %w", err)
	}
	return u, nil
}

func displayName(id Identity) string {
	switch {
	case id.Name != "":
		return id.Name
	case id.Handle != "":
		return id.Handle
	}
	return id.Email
}

// SafeReturnTo only allows local absolute paths as post-login targets.
func SafeReturnTo(p string) string {
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

// ---------- JSON helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeProblem renders an RFC 7807 document shaped like the REST API's errors.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, extra map[string]any) {
	title := http.StatusText(status)
	msg := title
	if detail != "" {
		msg = title + " (" + detail + ")"
	}
	body := map[string]any{
		"type":     "/errors/" + strings.ReplaceAll(code, "_", "-"),
		"title":    title,
		"status":   status,
		"code":     code,
		"error":    map[string]string{"code": code, "message": msg},
		"instance": r.URL.Path,
	}
	if detail != "" {
		body["detail"] = detail
	}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// constantTimeEqual compares two strings without leaking their content timing.
func constantTimeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---------- login-flow cookies ----------

func flowCookieName(provider string) string { return "of_flow_" + provider }

// SaveFlow implements LoginCompleter.
func (s *Service) SaveFlow(w http.ResponseWriter, provider string, v any, ttl time.Duration) error {
	name := flowCookieName(provider)
	val, err := s.flows.seal(v, name, s.now().Add(ttl))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite are set; Secure follows the https public URL, so plain-http local use works
		Name: name, Value: val, Path: "/auth/" + provider + "/", MaxAge: int(ttl / time.Second),
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// LoadFlow implements LoginCompleter.
func (s *Service) LoadFlow(w http.ResponseWriter, r *http.Request, provider string, v any) error {
	name := flowCookieName(provider)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite are set; Secure follows the https public URL, so plain-http local use works
		Name: name, Value: "", Path: "/auth/" + provider + "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	ck, err := r.Cookie(name)
	if err != nil {
		return loginErr(LoginErrInvalidState, errors.New("login flow cookie missing (expired, or cookies blocked)"))
	}
	if err := s.flows.open(ck.Value, name, s.now(), v); err != nil {
		return loginErr(LoginErrInvalidState, err)
	}
	return nil
}
