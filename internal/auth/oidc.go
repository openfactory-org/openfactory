package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/openfactory-org/openfactory/internal/core"
)

// OIDC callback path, relative to the public URL.
const OIDCCallbackPath = "/auth/oidc/callback"

// oidcFlowTTL bounds the time between the login redirect and the callback.
const oidcFlowTTL = 10 * time.Minute

// OIDCConfig configures the OIDC login provider.
type OIDCConfig struct {
	Issuer       string   // OF_OIDC_ISSUER (required)
	ClientID     string   // OF_OIDC_CLIENT_ID (required)
	ClientSecret string   // OF_OIDC_CLIENT_SECRET (empty for public PKCE clients)
	RedirectURL  string   // OF_OIDC_REDIRECT_URL (required; default ${OF_PUBLIC_URL}/auth/oidc/callback)
	Scopes       []string // OF_OIDC_SCOPES (default openid, profile, email; openid is always added)
	// AllowedDomains restricts logins to these email domains (case-insensitive).
	AllowedDomains []string // OF_OIDC_ALLOWED_DOMAINS
	GroupsClaim    string   // OF_OIDC_GROUPS_CLAIM (default "groups")
	AdminGroups    []string // OF_OIDC_ADMIN_GROUPS
	EditorGroups   []string // OF_OIDC_EDITOR_GROUPS
	// DefaultRole is given to users not in an admin or editor group
	// (OF_OIDC_DEFAULT_ROLE, default user). Other names are refused.
	DefaultRole core.Role
	DisplayName string // OF_OIDC_DISPLAY_NAME (default "Single sign-on")
	// RequireVerifiedEmail rejects tokens whose email_verified claim is not
	// true when AllowedDomains is set, and always blocks linking to an
	// existing account by email (OF_OIDC_REQUIRE_VERIFIED_EMAIL, default true).
	// Set it to false only for IdPs that omit email_verified but own the
	// email addresses they assert.
	RequireVerifiedEmail bool

	// HTTPClient is used for discovery, JWKS and token requests (default: a
	// client with a 15s timeout).
	HTTPClient *http.Client
	// Now overrides the clock used for ID-token validation (tests).
	Now func() time.Time
}

// RoleManaged reports whether roles come from IdP groups: true when admin or
// editor groups are configured. Then every login re-applies the mapping;
// otherwise roles are managed in Open Factory and only new users get
// DefaultRole.
func (c OIDCConfig) RoleManaged() bool { return len(c.AdminGroups) > 0 || len(c.EditorGroups) > 0 }

// OIDCConfigFromEnv reads the OF_OIDC_* variables. enabled is false when
// OF_OIDC_ISSUER is unset. OF_PUBLIC_URL provides the default redirect URL.
func OIDCConfigFromEnv(getenv func(string) string) (cfg OIDCConfig, enabled bool, err error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	cfg = OIDCConfig{
		Issuer:         get("OF_OIDC_ISSUER"),
		ClientID:       get("OF_OIDC_CLIENT_ID"),
		ClientSecret:   get("OF_OIDC_CLIENT_SECRET"),
		RedirectURL:    get("OF_OIDC_REDIRECT_URL"),
		Scopes:         SplitList(get("OF_OIDC_SCOPES")),
		AllowedDomains: SplitList(get("OF_OIDC_ALLOWED_DOMAINS")),
		GroupsClaim:    get("OF_OIDC_GROUPS_CLAIM"),
		AdminGroups:    SplitList(get("OF_OIDC_ADMIN_GROUPS")),
		EditorGroups:   SplitList(get("OF_OIDC_EDITOR_GROUPS")),
		DefaultRole:    core.Role(strings.ToLower(get("OF_OIDC_DEFAULT_ROLE"))),
		DisplayName:    get("OF_OIDC_DISPLAY_NAME"),

		RequireVerifiedEmail: true,
	}
	if cfg.Issuer == "" {
		return cfg, false, nil
	}
	if v := get("OF_OIDC_REQUIRE_VERIFIED_EMAIL"); v != "" {
		b, perr := strconv.ParseBool(v)
		if perr != nil {
			return cfg, true, fmt.Errorf("OF_OIDC_REQUIRE_VERIFIED_EMAIL: %w", perr)
		}
		cfg.RequireVerifiedEmail = b
	}
	if cfg.RedirectURL == "" {
		if pub := strings.TrimRight(get("OF_PUBLIC_URL"), "/"); pub != "" {
			cfg.RedirectURL = pub + OIDCCallbackPath
		}
	}
	return cfg, true, cfg.normalize()
}

// SplitList splits a comma- or whitespace-separated list, dropping empties.
func SplitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
}

func (c *OIDCConfig) normalize() error {
	var errs []error
	if c.Issuer == "" {
		errs = append(errs, errors.New("OF_OIDC_ISSUER is required"))
	}
	if c.ClientID == "" {
		errs = append(errs, errors.New("OF_OIDC_CLIENT_ID is required"))
	}
	if u, err := url.Parse(c.RedirectURL); c.RedirectURL == "" || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		errs = append(errs, errors.New("OF_OIDC_REDIRECT_URL (or OF_PUBLIC_URL) must be an absolute http(s) URL"))
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	} else if !slices.Contains(c.Scopes, oidc.ScopeOpenID) {
		c.Scopes = append([]string{oidc.ScopeOpenID}, c.Scopes...)
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.DefaultRole == "" {
		c.DefaultRole = core.RoleUser
	}
	if ValidRole(c.DefaultRole) {
		c.DefaultRole = core.NormalizeRole(c.DefaultRole)
	} else {
		errs = append(errs, fmt.Errorf("OF_OIDC_DEFAULT_ROLE %q must be admin, editor or user", c.DefaultRole))
	}
	if c.DisplayName == "" {
		c.DisplayName = "Single sign-on"
	}
	for i, d := range c.AllowedDomains {
		c.AllowedDomains[i] = strings.ToLower(strings.TrimPrefix(d, "@"))
	}
	return errors.Join(errs...)
}

// OIDCProvider is the OpenID Connect LoginProvider (authorization-code flow
// with PKCE). Discovery is lazy: the first login fetches the issuer's
// metadata, so an unreachable IdP does not stop the server from starting.
type OIDCProvider struct {
	cfg    OIDCConfig
	client *http.Client

	mu       sync.Mutex
	provider *oidc.Provider
}

var _ LoginProvider = (*OIDCProvider)(nil)

// NewOIDCProvider validates cfg and returns the provider. It does not contact
// the issuer; call Discover to fail fast at startup if desired.
func NewOIDCProvider(cfg OIDCConfig) (*OIDCProvider, error) {
	if err := cfg.normalize(); err != nil {
		return nil, fmt.Errorf("auth: oidc: %w", err)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &OIDCProvider{cfg: cfg, client: client}, nil
}

// Name implements LoginProvider.
func (p *OIDCProvider) Name() string { return ProviderOIDC }

// DisplayName implements LoginProvider.
func (p *OIDCProvider) DisplayName() string { return p.cfg.DisplayName }

// Discover fetches (and caches) the issuer's discovery document.
func (p *OIDCProvider) Discover(ctx context.Context) (*oidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	// The provider keeps this context for later JWKS refreshes, so it must
	// not be the (soon cancelled) request context. The client's timeout
	// bounds every call; ctx only bounds this first fetch.
	base := oidc.ClientContext(context.WithoutCancel(ctx), p.client)
	type result struct {
		prov *oidc.Provider
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		prov, err := oidc.NewProvider(base, p.cfg.Issuer)
		ch <- result{prov, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		p.provider = res.prov
		return res.prov, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *OIDCProvider) oauth2Config(prov *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.cfg.ClientID,
		ClientSecret: p.cfg.ClientSecret,
		RedirectURL:  p.cfg.RedirectURL,
		Endpoint:     prov.Endpoint(),
		Scopes:       p.cfg.Scopes,
	}
}

// oidcFlow is the state kept in the encrypted flow cookie between the login
// redirect and the callback.
type oidcFlow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
}

// LoginHandler implements LoginProvider: GET /auth/oidc/login?return_to=/path.
func (p *OIDCProvider) LoginHandler(c LoginCompleter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prov, err := p.Discover(r.Context())
		if err != nil {
			c.FailLogin(w, r, p.Name(), loginErr(LoginErrProviderUnavailable, err))
			return
		}
		flow := oidcFlow{
			State:    randomToken(32),
			Nonce:    randomToken(32),
			Verifier: oauth2.GenerateVerifier(),
			ReturnTo: SafeReturnTo(r.URL.Query().Get("return_to")),
		}
		if err := c.SaveFlow(w, p.Name(), flow, oidcFlowTTL); err != nil {
			c.FailLogin(w, r, p.Name(), err)
			return
		}
		u := p.oauth2Config(prov).AuthCodeURL(flow.State, oidc.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier))
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, u, http.StatusFound)
	})
}

// CallbackHandler implements LoginProvider: GET /auth/oidc/callback.
func (p *OIDCProvider) CallbackHandler(c LoginCompleter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var flow oidcFlow
		if err := c.LoadFlow(w, r, p.Name(), &flow); err != nil {
			c.FailLogin(w, r, p.Name(), err)
			return
		}
		id, err := p.exchange(r, flow)
		if err != nil {
			c.FailLogin(w, r, p.Name(), err)
			return
		}
		c.CompleteLogin(w, r, id, flow.ReturnTo)
	})
}

// idClaims are the ID-token claims the provider reads besides groups.
type idClaims struct {
	Email             string `json:"email"`
	EmailVerified     any    `json:"email_verified"` // bool, or "true" on some IdPs
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
}

func (p *OIDCProvider) exchange(r *http.Request, flow oidcFlow) (Identity, error) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		code := LoginErrProviderError
		if e == "access_denied" {
			code = LoginErrAccessDenied
		}
		return Identity{}, loginErr(code, fmt.Errorf("identity provider returned %s: %s", e, q.Get("error_description")))
	}
	if q.Get("state") == "" || !constantTimeEqual(q.Get("state"), flow.State) {
		return Identity{}, loginErr(LoginErrInvalidState, errors.New("state mismatch"))
	}
	code := q.Get("code")
	if code == "" {
		return Identity{}, loginErr(LoginErrProviderError, errors.New("callback without code"))
	}
	ctx := r.Context()
	prov, err := p.Discover(ctx)
	if err != nil {
		return Identity{}, loginErr(LoginErrProviderUnavailable, err)
	}
	tok, err := p.oauth2Config(prov).Exchange(oidc.ClientContext(ctx, p.client), code, oauth2.VerifierOption(flow.Verifier))
	if err != nil {
		return Identity{}, loginErr(LoginErrProviderError, fmt.Errorf("token exchange: %w", err))
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return Identity{}, loginErr(LoginErrProviderError, errors.New("token response without id_token"))
	}
	vcfg := &oidc.Config{ClientID: p.cfg.ClientID, Now: p.cfg.Now}
	idt, err := prov.Verifier(vcfg).Verify(oidc.ClientContext(ctx, p.client), raw)
	if err != nil {
		return Identity{}, loginErr(LoginErrProviderError, fmt.Errorf("verify id_token: %w", err))
	}
	if idt.Nonce == "" || !constantTimeEqual(idt.Nonce, flow.Nonce) {
		return Identity{}, loginErr(LoginErrNonceMismatch, errors.New("id_token nonce does not match"))
	}
	if idt.AccessTokenHash != "" {
		if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
			return Identity{}, loginErr(LoginErrProviderError, fmt.Errorf("at_hash: %w", err))
		}
	}
	var cl idClaims
	if err := idt.Claims(&cl); err != nil {
		return Identity{}, loginErr(LoginErrProviderError, fmt.Errorf("decode claims: %w", err))
	}
	var all map[string]any
	if err := idt.Claims(&all); err != nil {
		return Identity{}, loginErr(LoginErrProviderError, fmt.Errorf("decode claims: %w", err))
	}
	return p.identity(idt.Subject, cl, all)
}

func (p *OIDCProvider) identity(sub string, cl idClaims, all map[string]any) (Identity, error) {
	verified := false
	switch v := cl.EmailVerified.(type) {
	case bool:
		verified = v
	case string:
		verified = strings.EqualFold(v, "true")
	}
	email := strings.TrimSpace(cl.Email)
	if !p.cfg.RequireVerifiedEmail && email != "" {
		verified = true
	}
	if len(p.cfg.AllowedDomains) > 0 {
		_, domain, ok := strings.Cut(strings.ToLower(email), "@")
		if !ok || !slices.Contains(p.cfg.AllowedDomains, domain) {
			return Identity{}, loginErr(LoginErrDomainNotAllowed, fmt.Errorf("email %q is not in an allowed domain", email))
		}
		if !verified {
			return Identity{}, loginErr(LoginErrEmailNotVerified, fmt.Errorf("email %q is not verified", email))
		}
	}
	groups := claimStrings(all[p.cfg.GroupsClaim])
	return Identity{
		Provider:      p.Name(),
		Subject:       sub,
		Email:         email,
		EmailVerified: verified,
		Name:          cl.Name,
		Handle:        cl.PreferredUsername,
		Groups:        groups,
		Role:          p.MapRole(groups),
		RoleManaged:   p.cfg.RoleManaged(),
		DefaultRole:   p.cfg.DefaultRole,
	}, nil
}

// MapRole maps IdP groups to a role: admin if any admin group matches, else
// editor if any editor group matches, else the default role. Matching is
// exact and case-sensitive (group IDs are often opaque).
func (p *OIDCProvider) MapRole(groups []string) core.Role {
	has := func(want []string) bool {
		for _, g := range groups {
			if slices.Contains(want, g) {
				return true
			}
		}
		return false
	}
	switch {
	case has(p.cfg.AdminGroups):
		return core.RoleAdmin
	case has(p.cfg.EditorGroups):
		return core.RoleEditor
	}
	return p.cfg.DefaultRole
}

// claimStrings accepts a JSON array of strings or a single (comma- or
// space-separated) string.
func claimStrings(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return t
	case string:
		return SplitList(t)
	}
	return nil
}
