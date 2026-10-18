package mcpregistry

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// OAuth for registered MCP servers follows the MCP authorization spec:
// protected resource metadata (RFC 9728) names the authorization server,
// authorization server metadata (RFC 8414 / OpenID discovery) gives its
// endpoints, dynamic client registration (RFC 7591) supplies a client when
// none is configured, and the authorization code flow uses PKCE (S256) and
// the resource indicator (RFC 8707). Tokens are stored encrypted in the
// store's key/value space; nothing is written to the definition or sandbox.

const (
	tokenSecretType  core.SecretType = "mcp_oauth_token" //nolint:gosec // G101: a secret type name, not a credential
	stateSecretType  core.SecretType = "mcp_oauth_state" //nolint:gosec // G101: a secret type name, not a credential
	stateTTL                         = 10 * time.Minute
	refreshSkew                      = 60 * time.Second
	maxMetadataBytes                 = 1 << 20
	startTimeout                     = 30 * time.Second // discovery and registration in StartOAuth
)

// ErrNotConnected is returned when a server has no usable OAuth token.
var ErrNotConnected = errors.New("mcp server is not connected")

// tokenSet is the stored OAuth state of one server.
type tokenSet struct {
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	TokenType     string    `json:"token_type,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitzero"`
	Scope         string    `json:"scope,omitempty"`
	TokenEndpoint string    `json:"token_endpoint"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"` // from dynamic registration
	AuthMethod    string    `json:"auth_method,omitempty"`   // none, client_secret_basic, client_secret_post
	Resource      string    `json:"resource,omitempty"`
}

// pendingAuth is the state of an authorization in progress, keyed by state.
type pendingAuth struct {
	ServerID      string    `json:"server_id"`
	TeamID        string    `json:"team_id"`
	UserID        string    `json:"user_id"`
	Verifier      string    `json:"verifier"`
	TokenEndpoint string    `json:"token_endpoint"`
	ClientID      string    `json:"client_id"`
	ClientSecret  string    `json:"client_secret,omitempty"`
	AuthMethod    string    `json:"auth_method,omitempty"`
	Resource      string    `json:"resource,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type resourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
}

func tokenKey(serverID string) string { return "mcp:oauth:token:" + serverID }
func stateKey(state string) string    { return "mcp:oauth:state:" + state }
func stateName(state string) string   { return "mcp-oauth-state:" + state }
func tokenName(serverID string) string {
	return "mcp-oauth:" + serverID
}

// TokenStorage says where the sealed OAuth token of a server is stored: the
// key in the KV space, and the name and type that the ciphertext binds. The
// command "secret rewrap" uses it.
func TokenStorage(serverID string) (kvKey, name string, typ core.SecretType) {
	return tokenKey(serverID), tokenName(serverID), tokenSecretType
}

// RedirectURI is the OAuth callback URL registered with providers.
func (s *Service) RedirectURI() string { return s.opts.PublicURL + CallbackPath }

// StartOAuth begins the authorization code flow for m on behalf of userID
// and returns the provider's authorization URL to send the browser to.
func (s *Service) StartOAuth(ctx context.Context, m *core.RegisteredMCPServer, userID string) (string, error) {
	if m.OAuth == nil {
		return "", invalid("MCP server %q has no oauth configuration", m.Name)
	}
	if s.opts.PublicURL == "" {
		return "", errors.New("OAuth needs the server's public URL (OF_PUBLIC_URL) for the redirect URI")
	}
	if s.opts.Encrypter == nil {
		return "", errors.New("OAuth needs the secret store to encrypt tokens")
	}
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	resource := canonicalResource(m.URL)
	prm := s.discoverResource(ctx, m.URL)
	issuer := originOf(m.URL)
	if prm != nil && len(prm.AuthorizationServers) > 0 {
		issuer = prm.AuthorizationServers[0]
		if prm.Resource != "" {
			resource = prm.Resource
		}
	}
	as, err := s.discoverAuthServer(ctx, issuer)
	if err != nil {
		return "", err
	}
	if m.OAuth.AuthorizationEndpoint != "" {
		as.AuthorizationEndpoint = m.OAuth.AuthorizationEndpoint
	}
	if m.OAuth.TokenEndpoint != "" {
		as.TokenEndpoint = m.OAuth.TokenEndpoint
	}
	for _, e := range []struct{ name, v string }{{"authorization endpoint", as.AuthorizationEndpoint}, {"token endpoint", as.TokenEndpoint}} {
		if err := checkURL(e.v, true); err != nil {
			return "", fmt.Errorf("oauth %s %q: %w", e.name, e.v, err)
		}
	}
	if len(as.CodeChallengeMethodsSupported) > 0 && !slices.Contains(as.CodeChallengeMethodsSupported, "S256") {
		return "", errors.New("the authorization server does not support PKCE with S256")
	}

	p := &pendingAuth{ServerID: m.ID, TeamID: m.TeamID, UserID: userID, TokenEndpoint: as.TokenEndpoint, Resource: resource}
	switch {
	case m.OAuth.ClientID != "":
		p.ClientID = m.OAuth.ClientID
		if m.OAuth.ClientSecretName != "" {
			sec, err := s.opts.Secrets.Get(ctx, m.TeamID, "", m.OAuth.ClientSecretName)
			if err != nil {
				return "", fmt.Errorf("oauth client secret %s: %w", m.OAuth.ClientSecretName, err)
			}
			p.ClientSecret = string(sec)
			p.AuthMethod = pickAuthMethod(as.TokenEndpointAuthMethodsSupported)
		} else {
			p.AuthMethod = "none"
		}
	case as.RegistrationEndpoint != "":
		if err := checkURL(as.RegistrationEndpoint, true); err != nil {
			return "", fmt.Errorf("oauth registration endpoint %q: %w", as.RegistrationEndpoint, err)
		}
		if err := s.registerClient(ctx, as.RegistrationEndpoint, p); err != nil {
			return "", err
		}
	default:
		return "", invalid("the authorization server does not support dynamic client registration: register a client with the provider and set oauth.client_id (and oauth.client_secret_name)")
	}

	verifier, err := randomString(48)
	if err != nil {
		return "", err
	}
	state, err := randomString(32)
	if err != nil {
		return "", err
	}
	p.Verifier = verifier
	p.ExpiresAt = s.opts.Now().Add(stateTTL)
	b, _ := json.Marshal(p) //nolint:gosec // G117: the state is encrypted below before it is stored
	env, err := s.opts.Encrypter.Encrypt(m.TeamID, "", stateName(state), stateSecretType, b)
	if err != nil {
		return "", err
	}
	if err := s.opts.Store.KVSet(ctx, stateKey(state), base64.StdEncoding.EncodeToString(env)); err != nil {
		return "", err
	}

	scopes := m.OAuth.Scopes
	if len(scopes) == 0 && prm != nil {
		scopes = prm.ScopesSupported
	}
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {s.RedirectURI()},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if resource != "" {
		q.Set("resource", resource)
	}
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	u, err := url.Parse(as.AuthorizationEndpoint)
	if err != nil {
		return "", err
	}
	existing := u.Query()
	for k, v := range q {
		existing[k] = v
	}
	u.RawQuery = existing.Encode()
	return u.String(), nil
}

// CompleteOAuth finishes the flow started by StartOAuth: it checks state
// (same team and user), exchanges code for tokens and marks the server
// connected. providerErr is the callback's error parameter, if any.
func (s *Service) CompleteOAuth(ctx context.Context, teamID, userID, state, code, providerErr string) (*core.RegisteredMCPServer, error) {
	if state == "" {
		return nil, invalid("missing state")
	}
	raw, err := s.opts.Store.KVGet(ctx, stateKey(state))
	if err != nil || raw == "" {
		return nil, invalid("unknown or expired authorization state; start the connection again")
	}
	_ = s.opts.Store.KVSet(ctx, stateKey(state), "") // single use
	if s.opts.Encrypter == nil {
		return nil, errors.New("OAuth needs the secret store to encrypt tokens")
	}
	env, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, invalid("corrupt authorization state")
	}
	// The state is sealed to the team that started it: another team cannot open it.
	plain, err := s.opts.Encrypter.Decrypt(teamID, "", stateName(state), stateSecretType, env)
	if err != nil {
		return nil, invalid("this authorization was started by another user")
	}
	var p pendingAuth
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, invalid("corrupt authorization state")
	}
	if p.TeamID != teamID || p.UserID != userID {
		return nil, invalid("this authorization was started by another user")
	}
	if s.opts.Now().After(p.ExpiresAt) {
		return nil, invalid("the authorization expired; start the connection again")
	}
	m, err := s.opts.Store.GetMCPServer(ctx, p.ServerID)
	if err != nil || m.TeamID != teamID {
		return nil, fmt.Errorf("%w: MCP server no longer exists", core.ErrNotFound)
	}
	if m.OAuth == nil {
		return nil, invalid("MCP server %q no longer uses OAuth", m.Name)
	}
	if providerErr != "" {
		s.setStatus(ctx, m, core.MCPOAuthError, "authorization failed: "+providerErr)
		return m, invalid("the provider returned an error: %s", providerErr)
	}
	if code == "" {
		return nil, invalid("missing authorization code")
	}
	ts := &tokenSet{TokenEndpoint: p.TokenEndpoint, ClientID: p.ClientID, ClientSecret: p.ClientSecret, AuthMethod: p.AuthMethod, Resource: p.Resource}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.RedirectURI()},
		"code_verifier": {p.Verifier},
	}
	if err := s.tokenRequest(ctx, ts, form); err != nil {
		s.setStatus(ctx, m, core.MCPOAuthError, err.Error())
		return m, err
	}
	if err := s.saveToken(ctx, m, ts); err != nil {
		return m, err
	}
	now := s.opts.Now().UTC()
	m.OAuth.Status, m.OAuth.Error, m.OAuth.ConnectedBy, m.OAuth.ConnectedAt = core.MCPOAuthConnected, "", userID, &now
	m.UpdatedAt = now
	if err := s.opts.Store.UpdateMCPServer(ctx, m); err != nil {
		return m, err
	}
	s.log.Info("mcp server connected", "server", m.Name, "team", teamID, "user", userID)
	return m, nil
}

// Disconnect deletes a server's stored tokens and marks it not connected.
func (s *Service) Disconnect(ctx context.Context, m *core.RegisteredMCPServer) error {
	if err := s.opts.Store.KVSet(ctx, tokenKey(m.ID), ""); err != nil {
		return err
	}
	if m.OAuth != nil {
		m.OAuth.Status, m.OAuth.Error, m.OAuth.ConnectedBy, m.OAuth.ConnectedAt = core.MCPOAuthNotConnected, "", "", nil
		m.UpdatedAt = s.opts.Now().UTC()
		return s.opts.Store.UpdateMCPServer(ctx, m)
	}
	return nil
}

// Forget deletes stored tokens without touching the record (server deleted).
func (s *Service) Forget(ctx context.Context, serverID string) {
	_ = s.opts.Store.KVSet(ctx, tokenKey(serverID), "")
}

// AccessToken returns a valid access token for m, refreshing it when it is
// about to expire.
func (s *Service) AccessToken(ctx context.Context, m *core.RegisteredMCPServer) (string, error) {
	ts, err := s.loadToken(ctx, m)
	if err != nil {
		return "", err
	}
	if !s.expiring(ts) {
		return ts.AccessToken, nil
	}
	lock := s.refreshLock(m.ID)
	lock.Lock()
	defer lock.Unlock()
	// Another request (or replica) may have refreshed while we waited.
	if ts, err = s.loadToken(ctx, m); err != nil {
		return "", err
	}
	if !s.expiring(ts) {
		return ts.AccessToken, nil
	}
	if ts.RefreshToken == "" {
		s.setStatus(ctx, m, core.MCPOAuthError, "the access token expired and the provider issued no refresh token; reconnect the server")
		return "", ErrNotConnected
	}
	used := ts.RefreshToken
	err = s.tokenRequest(ctx, ts, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {used}})
	if err != nil {
		// A replica that refreshed at the same time may have rotated the
		// refresh token: use its result when there is one.
		if again, lerr := s.loadToken(ctx, m); lerr == nil && again.RefreshToken != used && !s.expiring(again) {
			return again.AccessToken, nil
		}
		s.setStatus(ctx, m, core.MCPOAuthError, "token refresh failed: "+err.Error())
		return "", fmt.Errorf("%w: %w", ErrNotConnected, err)
	}
	if ts.RefreshToken == "" {
		ts.RefreshToken = used // providers may omit an unrotated refresh token
	}
	if err := s.saveToken(ctx, m, ts); err != nil {
		return "", err
	}
	return ts.AccessToken, nil
}

func (s *Service) expiring(ts *tokenSet) bool {
	return !ts.ExpiresAt.IsZero() && s.opts.Now().Add(refreshSkew).After(ts.ExpiresAt)
}

func (s *Service) refreshLock(id string) *sync.Mutex {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	l := s.refreshes[id]
	if l == nil {
		l = &sync.Mutex{}
		s.refreshes[id] = l
	}
	return l
}

func (s *Service) loadToken(ctx context.Context, m *core.RegisteredMCPServer) (*tokenSet, error) {
	if s.opts.Encrypter == nil {
		return nil, ErrNotConnected
	}
	raw, err := s.opts.Store.KVGet(ctx, tokenKey(m.ID))
	if errors.Is(err, core.ErrNotFound) || (err == nil && raw == "") {
		return nil, ErrNotConnected
	}
	if err != nil {
		return nil, err
	}
	env, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("stored oauth token: %w", err)
	}
	plain, err := s.opts.Encrypter.Decrypt(m.TeamID, "", tokenName(m.ID), tokenSecretType, env)
	if err != nil {
		return nil, fmt.Errorf("stored oauth token: %w", err)
	}
	var ts tokenSet
	if err := json.Unmarshal(plain, &ts); err != nil {
		return nil, fmt.Errorf("stored oauth token: %w", err)
	}
	if ts.AccessToken == "" {
		return nil, ErrNotConnected
	}
	return &ts, nil
}

func (s *Service) saveToken(ctx context.Context, m *core.RegisteredMCPServer, ts *tokenSet) error {
	plain, err := json.Marshal(ts) //nolint:gosec // G117: the token set is encrypted below before it is stored
	if err != nil {
		return err
	}
	env, err := s.opts.Encrypter.Encrypt(m.TeamID, "", tokenName(m.ID), tokenSecretType, plain)
	if err != nil {
		return err
	}
	return s.opts.Store.KVSet(ctx, tokenKey(m.ID), base64.StdEncoding.EncodeToString(env))
}

func (s *Service) setStatus(ctx context.Context, m *core.RegisteredMCPServer, status, msg string) {
	if m.OAuth == nil {
		return
	}
	if len(msg) > 500 {
		msg = msg[:500]
	}
	m.OAuth.Status, m.OAuth.Error = status, msg
	m.UpdatedAt = s.opts.Now().UTC()
	if err := s.opts.Store.UpdateMCPServer(ctx, m); err != nil {
		s.log.Warn("could not record mcp oauth status", "server", m.Name, "err", err)
	}
	if status == core.MCPOAuthError {
		s.log.Warn("mcp server oauth error", "server", m.Name, "team", m.TeamID, "error", msg)
	}
}

// tokenRequest posts form to the token endpoint and stores the response in ts.
func (s *Service) tokenRequest(ctx context.Context, ts *tokenSet, form url.Values) error {
	if ts.Resource != "" {
		form.Set("resource", ts.Resource)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.TokenEndpoint, nil) //nolint:gosec // G704: the token endpoint is checked by checkURL (https or loopback) when the flow starts
	if err != nil {
		return err
	}
	switch ts.AuthMethod {
	case "client_secret_basic":
		req.SetBasicAuth(url.QueryEscape(ts.ClientID), url.QueryEscape(ts.ClientSecret))
	case "client_secret_post":
		form.Set("client_id", ts.ClientID)
		form.Set("client_secret", ts.ClientSecret)
	default:
		form.Set("client_id", ts.ClientID)
	}
	body := form.Encode()
	req.Body = io.NopCloser(strings.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req) //nolint:gosec // G704: the token endpoint is checked by checkURL (https or loopback) when the flow starts
	if err != nil {
		return fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	var out struct {
		AccessToken      string `json:"access_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        any    `json:"expires_in"`
		RefreshToken     string `json:"refresh_token"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	trimmed := bytes.TrimSpace(raw)
	if ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); ct == "application/x-www-form-urlencoded" || (len(trimmed) > 0 && trimmed[0] != '{') {
		// Some providers answer with a form-encoded body.
		v, _ := url.ParseQuery(string(trimmed))
		out.AccessToken, out.TokenType, out.RefreshToken, out.Scope = v.Get("access_token"), v.Get("token_type"), v.Get("refresh_token"), v.Get("scope")
		out.ExpiresIn, out.Error, out.ErrorDescription = v.Get("expires_in"), v.Get("error"), v.Get("error_description")
	} else if err := json.Unmarshal(trimmed, &out); err != nil && resp.StatusCode < 300 {
		return fmt.Errorf("token endpoint: invalid response: %w", err)
	}
	if resp.StatusCode >= 300 || out.Error != "" || out.AccessToken == "" {
		msg := out.Error
		if out.ErrorDescription != "" {
			msg += ": " + out.ErrorDescription
		}
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return fmt.Errorf("token endpoint: %s", msg)
	}
	if out.TokenType != "" && !strings.EqualFold(out.TokenType, "bearer") {
		return fmt.Errorf("token endpoint: unsupported token type %q", out.TokenType)
	}
	ts.AccessToken, ts.TokenType, ts.Scope = out.AccessToken, out.TokenType, out.Scope
	ts.RefreshToken = out.RefreshToken
	ts.ExpiresAt = time.Time{}
	if secs := expiresIn(out.ExpiresIn); secs > 0 {
		ts.ExpiresAt = s.opts.Now().Add(time.Duration(secs) * time.Second).UTC()
	}
	return nil
}

func expiresIn(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	}
	return 0
}

// registerClient performs dynamic client registration and fills p's client.
func (s *Service) registerClient(ctx context.Context, endpoint string, p *pendingAuth) error {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Open Factory",
		"redirect_uris":              []string{s.RedirectURI()},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("client registration: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	var out struct {
		ClientID                string `json:"client_id"`
		ClientSecret            string `json:"client_secret"`
		TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 || out.ClientID == "" {
		msg := strings.TrimSpace(out.Error + " " + out.ErrorDescription)
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return fmt.Errorf("client registration failed: %s", msg)
	}
	p.ClientID, p.ClientSecret = out.ClientID, out.ClientSecret
	switch {
	case out.ClientSecret == "":
		p.AuthMethod = "none"
	case out.TokenEndpointAuthMethod == "client_secret_post":
		p.AuthMethod = "client_secret_post"
	default:
		p.AuthMethod = "client_secret_basic"
	}
	return nil
}

func pickAuthMethod(supported []string) string {
	if len(supported) > 0 && !slices.Contains(supported, "client_secret_basic") && slices.Contains(supported, "client_secret_post") {
		return "client_secret_post"
	}
	return "client_secret_basic"
}

// discoverResource finds the protected resource metadata of an MCP server:
// from the WWW-Authenticate header of an unauthenticated request, then from
// the well-known locations. It returns nil when there is none.
func (s *Service) discoverResource(ctx context.Context, mcpURL string) *resourceMetadata {
	var candidates []string
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"openfactory","version":"1"}}}`
	if req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, strings.NewReader(init)); err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if resp, err := s.http.Do(req); err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxMetadataBytes))
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				if u := authParam(resp.Header.Values("WWW-Authenticate"), "resource_metadata"); u != "" {
					candidates = append(candidates, u)
				}
			}
		}
	}
	if u, err := url.Parse(mcpURL); err == nil {
		origin := u.Scheme + "://" + u.Host
		if p := strings.TrimRight(u.Path, "/"); p != "" {
			candidates = append(candidates, origin+"/.well-known/oauth-protected-resource"+p)
		}
		candidates = append(candidates, origin+"/.well-known/oauth-protected-resource")
	}
	for _, c := range candidates {
		var md resourceMetadata
		if s.getJSON(ctx, c, &md) == nil && len(md.AuthorizationServers) > 0 {
			return &md
		}
	}
	return nil
}

// discoverAuthServer reads authorization server metadata for issuer. Without
// metadata it falls back to /authorize, /token and /register on the issuer's
// origin, as the 2025-03-26 MCP spec allows.
func (s *Service) discoverAuthServer(ctx context.Context, issuer string) (*asMetadata, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid authorization server %q", issuer)
	}
	origin := u.Scheme + "://" + u.Host
	p := strings.TrimRight(u.Path, "/")
	candidates := []string{origin + "/.well-known/oauth-authorization-server" + p, origin + "/.well-known/openid-configuration" + p}
	if p != "" {
		candidates = append(candidates, origin+p+"/.well-known/openid-configuration")
	}
	for _, c := range candidates {
		var md asMetadata
		if s.getJSON(ctx, c, &md) == nil && md.AuthorizationEndpoint != "" && md.TokenEndpoint != "" {
			return &md, nil
		}
	}
	return &asMetadata{
		Issuer:                origin,
		AuthorizationEndpoint: origin + "/authorize",
		TokenEndpoint:         origin + "/token",
		RegistrationEndpoint:  origin + "/register",
	}, nil
}

func (s *Service) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(out)
}

// authParam extracts a parameter from Bearer WWW-Authenticate challenges.
func authParam(values []string, name string) string {
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if i := strings.IndexByte(part, ' '); i > 0 && strings.EqualFold(part[:i], "bearer") {
				part = strings.TrimSpace(part[i+1:])
			}
			k, val, ok := strings.Cut(part, "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), name) {
				return strings.Trim(strings.TrimSpace(val), `"`)
			}
		}
	}
	return ""
}

func canonicalResource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment, u.RawQuery = "", ""
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return u.String()
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Scheme + "://" + u.Host
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
