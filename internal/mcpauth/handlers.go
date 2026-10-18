package mcpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ---------- metadata ----------

func (s *Service) handleResourceMetadata(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, ResourceMetadataPath)
	if path == "" {
		path = s.opts.ResourcePaths[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.opts.PublicURL + path,
		"authorization_servers":    []string{s.opts.PublicURL},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Open Factory Factory MCP",
		"resource_documentation":   s.opts.PublicURL + "/docs/factory-mcp",
	})
}

func (s *Service) handleServerMetadata(w http.ResponseWriter, _ *http.Request) {
	u := s.opts.PublicURL
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         u,
		"authorization_endpoint":                         u + AuthorizePath,
		"token_endpoint":                                 u + TokenPath,
		"registration_endpoint":                          u + RegisterPath,
		"revocation_endpoint":                            u + RevokePath,
		"scopes_supported":                               []string{Scope},
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// ---------- client registration (RFC 7591) ----------

const maxRedirectURIs = 10

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON client metadata document")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > maxRedirectURIs {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give 1 to 10 redirect_uris")
		return
	}
	for _, u := range in.RedirectURIs {
		if err := checkRedirectURI(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	if m := in.TokenEndpointAuthMethod; m != "" && m != "none" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only public clients are supported: token_endpoint_auth_method must be none")
		return
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant type "+g)
			return
		}
	}
	name := strings.TrimSpace(in.ClientName)
	if len(name) > 100 {
		name = name[:100]
	}
	if name == "" {
		name = "MCP client"
	}
	c := &Client{ID: randomToken("ofc_"), Name: name, RedirectURIs: in.RedirectURIs, CreatedAt: s.opts.Now().UTC()}
	if err := s.kvPut(r.Context(), clientKey(c.ID), c); err != nil {
		s.log.Error("store client", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not store the client")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      Scope,
	})
}

// checkRedirectURI accepts https URLs, http URLs on a loopback host, and
// private-use schemes of native apps (RFC 8252).
func checkRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" {
		return errors.New("redirect_uri must be an absolute URL without a fragment")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		if u.Host == "" {
			return errors.New("redirect_uri must have a host")
		}
	case "http":
		if !isLoopback(u.Hostname()) {
			return errors.New("an http redirect_uri must use a loopback host (localhost, 127.0.0.1 or [::1])")
		}
	case "javascript", "data", "file", "vbscript":
		return errors.New("redirect_uri scheme " + u.Scheme + " is not allowed")
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redirectAllowed compares redirect with the registered URIs. A loopback
// URI matches on any port (RFC 8252, section 7.3).
func redirectAllowed(c *Client, redirect string) bool {
	for _, reg := range c.RedirectURIs {
		if reg == redirect {
			return true
		}
		a, err1 := url.Parse(reg)
		b, err2 := url.Parse(redirect)
		if err1 != nil || err2 != nil {
			continue
		}
		if a.Scheme == "http" && b.Scheme == "http" && isLoopback(a.Hostname()) && a.Hostname() == b.Hostname() &&
			a.Path == b.Path && a.RawQuery == b.RawQuery {
			return true
		}
	}
	return false
}

// ---------- authorization ----------

func (s *Service) validResource(res string) bool {
	if res == "" {
		return true
	}
	res = strings.TrimRight(res, "/")
	if res == s.opts.PublicURL {
		return true
	}
	for _, p := range s.opts.ResourcePaths {
		if res == s.opts.PublicURL+p {
			return true
		}
	}
	return false
}

func (s *Service) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	c, err := s.client(ctx, q.Get("client_id"))
	if err != nil {
		s.log.Error("load client", "err", err)
		s.page(w, http.StatusInternalServerError, pageData{Title: "Sign-in failed", Message: "The server could not read the client. Try again."})
		return
	}
	redirect := q.Get("redirect_uri")
	// Errors before the redirect URI is known go to the user, not to the client.
	if c == nil {
		s.page(w, http.StatusBadRequest, pageData{Title: "Unknown MCP client", Message: "This client is not registered. Remove the server from your MCP client and add it again."})
		return
	}
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	if !redirectAllowed(c, redirect) {
		s.page(w, http.StatusBadRequest, pageData{Title: "Wrong redirect address", Message: "The redirect_uri of this request is not registered for the client."})
		return
	}
	state := q.Get("state")
	fail := func(code, desc string) { redirectError(w, r, redirect, state, s.opts.PublicURL, code, desc) }
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "response_type must be code")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method S256 is required")
		return
	}
	if !s.validResource(q.Get("resource")) {
		fail("invalid_target", "the resource is not this server's Factory MCP endpoint")
		return
	}
	if sc := q.Get("scope"); sc != "" {
		for _, x := range strings.Fields(sc) {
			if x != Scope {
				fail("invalid_scope", "the only scope is "+Scope)
				return
			}
		}
	}
	u, err := s.opts.Session(r)
	if err != nil && !errors.Is(err, core.ErrUnauthorized) {
		s.log.Warn("session for MCP sign-in", "err", err)
	}
	if u == nil {
		// Sign in to the dashboard first (session login or OIDC), then come back.
		back := r.URL.Path + "?" + r.URL.RawQuery
		http.Redirect(w, r, s.opts.LoginPath+"?return_to="+url.QueryEscape(back), http.StatusFound)
		return
	}
	if u.IsServiceAccount() {
		s.page(w, http.StatusForbidden, pageData{Title: "Not allowed", Message: "A service account cannot sign in. Use an API key."})
		return
	}
	req := &authRequest{
		ID: randomToken("ofq_"), ClientID: c.ID, RedirectURI: redirect, State: state,
		CodeChallenge: q.Get("code_challenge"), Resource: q.Get("resource"), UserID: u.ID,
		Expires: s.opts.Now().UTC().Add(requestTTL),
	}
	if err := s.kvPut(ctx, requestKey(req.ID), req); err != nil {
		s.log.Error("store authorization request", "err", err)
		s.page(w, http.StatusInternalServerError, pageData{Title: "Sign-in failed", Message: "The server could not store the request. Try again."})
		return
	}
	host := redirect
	if pu, err := url.Parse(redirect); err == nil && pu.Host != "" {
		host = pu.Scheme + "://" + pu.Host
	}
	s.page(w, http.StatusOK, pageData{
		Title: "Allow " + c.Name + "?", Consent: true, RequestID: req.ID,
		ClientName: c.Name, UserLabel: userLabel(u), RedirectHost: host, Action: AuthorizePath,
	})
}

func userLabel(u *core.User) string {
	if u.Email != "" {
		return u.Email
	}
	if u.Name != "" {
		return u.Name
	}
	return u.ID
}

func (s *Service) handleConsent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		s.page(w, http.StatusBadRequest, pageData{Title: "Sign-in failed", Message: "The form could not be read."})
		return
	}
	var req authRequest
	ok, err := s.kvGet(ctx, requestKey(r.PostForm.Get("request_id")), &req)
	if err != nil || !ok || !s.opts.Now().UTC().Before(req.Expires) {
		s.page(w, http.StatusBadRequest, pageData{Title: "Request expired", Message: "Start the sign-in again from your MCP client."})
		return
	}
	_ = s.kvDelete(ctx, requestKey(req.ID)) // one answer per request
	u, err := s.opts.Session(r)
	if err != nil || u == nil || u.ID != req.UserID {
		s.page(w, http.StatusForbidden, pageData{Title: "Not allowed", Message: "Sign in to the dashboard as the same user and start again."})
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		s.audit(ctx, "mcp.signin.authorize", "failure", &Grant{UserID: u.ID, TeamID: u.TeamID, ClientID: req.ClientID}, map[string]string{"reason": "denied"})
		redirectError(w, r, req.RedirectURI, req.State, s.opts.PublicURL, "access_denied", "the user denied the request")
		return
	}
	code := randomToken("ofk_")
	rec := codeRecord{
		ClientID: req.ClientID, RedirectURI: req.RedirectURI, CodeChallenge: req.CodeChallenge,
		UserID: u.ID, TeamID: u.TeamID, Expires: s.opts.Now().UTC().Add(codeTTL),
	}
	if err := s.kvPut(ctx, codeKey(hashOf(code)), rec); err != nil {
		s.log.Error("store code", "err", err)
		redirectError(w, r, req.RedirectURI, req.State, s.opts.PublicURL, "server_error", "could not store the code")
		return
	}
	v := url.Values{"code": {code}, "iss": {s.opts.PublicURL}}
	if req.State != "" {
		v.Set("state", req.State)
	}
	http.Redirect(w, r, withQuery(req.RedirectURI, v), http.StatusFound)
}

// ---------- token endpoint ----------

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

func (s *Service) handleToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be form encoded")
		return
	}
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "refresh_token":
		s.refresh(w, r)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func pkceOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func (s *Service) exchangeCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	f := r.PostForm
	key := codeKey(hashOf(f.Get("code")))
	s.mu.Lock()
	var rec codeRecord
	ok, err := s.kvGet(ctx, key, &rec)
	if err == nil && ok {
		_ = s.kvDelete(ctx, key) // a code works once
	}
	s.mu.Unlock()
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not read the code")
		return
	}
	now := s.opts.Now().UTC()
	switch {
	case !ok || !now.Before(rec.Expires):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, used or expired")
		return
	case f.Get("client_id") != rec.ClientID:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
		return
	case f.Get("redirect_uri") != "" && f.Get("redirect_uri") != rec.RedirectURI:
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	case !pkceOK(f.Get("code_verifier"), rec.CodeChallenge):
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the code_verifier does not match the code_challenge")
		return
	}
	c, err := s.client(ctx, rec.ClientID)
	if err != nil || c == nil {
		oauthError(w, http.StatusBadRequest, "invalid_client", "unknown client")
		return
	}
	g := &grantRecord{Grant: Grant{
		ID: core.NewID("mcps"), UserID: rec.UserID, TeamID: rec.TeamID,
		ClientID: c.ID, ClientName: c.Name, CreatedAt: now,
	}}
	s.mu.Lock()
	access, refresh, err := s.issue(ctx, g)
	if err == nil {
		err = s.addToIndex(ctx, userIndex(g.UserID), g.ID)
	}
	if err == nil {
		err = s.addToIndex(ctx, teamIndex(g.TeamID), g.ID)
	}
	s.mu.Unlock()
	if err != nil {
		s.log.Error("issue tokens", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	s.audit(ctx, "mcp.signin.authorize", "success", g.public(), nil)
	s.log.Info("MCP sign-in", "grant", g.ID, "user", g.UserID, "client", c.Name)
	writeJSON(w, http.StatusOK, tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(s.opts.AccessTTL.Seconds()), RefreshToken: refresh, Scope: Scope})
}

func (s *Service) refresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok := r.PostForm.Get("refresh_token")
	if !strings.HasPrefix(tok, RefreshTokenPrefix) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	h := hashOf(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.opts.Store.KVGet(ctx, refreshKey(h))
	if errors.Is(err, core.ErrNotFound) || (err == nil && id == "") {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown, used or revoked")
		return
	}
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not read the token")
		return
	}
	g, err := s.loadGrant(ctx, id)
	now := s.opts.Now().UTC()
	if err != nil || g.RefreshHash != h || !g.Active(now) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the sign-in ended; sign in again")
		return
	}
	if cid := r.PostForm.Get("client_id"); cid != "" && cid != g.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
		return
	}
	u, err := s.opts.Store.GetUser(ctx, g.UserID)
	if err != nil || u.TeamID != g.TeamID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the user of this sign-in no longer exists")
		return
	}
	access, refresh, err := s.issue(ctx, g)
	if err != nil {
		s.log.Error("refresh tokens", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int64(s.opts.AccessTTL.Seconds()), RefreshToken: refresh, Scope: Scope})
}

// handleRevoke implements RFC 7009. It answers 200 also for unknown tokens.
func (s *Service) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be form encoded")
		return
	}
	ctx := r.Context()
	tok := r.PostForm.Get("token")
	var key string
	switch {
	case strings.HasPrefix(tok, AccessTokenPrefix):
		key = accessKey(hashOf(tok))
	case strings.HasPrefix(tok, RefreshTokenPrefix):
		key = refreshKey(hashOf(tok))
	}
	if key != "" {
		if id, err := s.opts.Store.KVGet(ctx, key); err == nil && id != "" {
			if err := s.Revoke(ctx, id); err != nil && !errors.Is(err, core.ErrNotFound) {
				s.log.Warn("revoke MCP sign-in", "err", err)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

func withQuery(raw string, v url.Values) string {
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + v.Encode()
}

func redirectError(w http.ResponseWriter, r *http.Request, redirect, state, iss, code, desc string) {
	v := url.Values{"error": {code}, "error_description": {desc}, "iss": {iss}}
	if state != "" {
		v.Set("state", state)
	}
	http.Redirect(w, r, withQuery(redirect, v), http.StatusFound) //nolint:gosec // G710: redirect is one of the client's registered redirect URIs (checked in the authorize handler)
}

type pageData struct {
	Title, Message                                         string
	Consent                                                bool
	RequestID, ClientName, UserLabel, RedirectHost, Action string
}

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Open Factory</title>
<style>
:root{--bg:#f7f7f8;--surface:#fff;--border:#e2e4e8;--text:#1b1d22;--muted:#5d6370;--accent:#3355dd;--accent-contrast:#fff;color-scheme:light}
@media (prefers-color-scheme: dark){:root{--bg:#111317;--surface:#181b20;--border:#2c313a;--text:#e8eaee;--muted:#a0a6b2;--accent:#7d97ff;--accent-contrast:#0d1020;color-scheme:dark}}
body{margin:0;background:var(--bg);color:var(--text);font:16px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;display:grid;place-items:center;min-height:100vh;padding:16px;box-sizing:border-box}
main{background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:24px;max-width:440px;width:100%}
h1{font-size:1.25rem;margin:0 0 12px}p{margin:0 0 12px}.muted{color:var(--muted);font-size:.9rem}
.row{display:flex;gap:8px;margin-top:20px}button{flex:1;font:inherit;padding:10px 12px;border-radius:8px;border:1px solid var(--border);background:var(--surface);color:var(--text);cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-contrast)}code{font-size:.9em}
</style></head><body><main>
<h1>{{.Title}}</h1>
{{if .Consent}}
<p><strong>{{.ClientName}}</strong> asks to use Factory MCP as <strong>{{.UserLabel}}</strong>.</p>
<p>It can do what you can do in your factories: list and read tasks, send work, send messages to foremen and close tasks.</p>
<p class="muted">After you allow it, your browser goes back to <code>{{.RedirectHost}}</code>. You can end this sign-in at any time in the dashboard, on the MCP servers page.</p>
<form method="post" action="{{.Action}}">
<input type="hidden" name="request_id" value="{{.RequestID}}">
<div class="row"><button type="submit" name="decision" value="deny">Deny</button><button class="primary" type="submit" name="decision" value="allow">Allow</button></div>
</form>
{{else}}<p>{{.Message}}</p>{{end}}
</main></body></html>`))

func (s *Service) page(w http.ResponseWriter, status int, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.WriteHeader(status)
	if err := pageTmpl.Execute(w, d); err != nil {
		s.log.Warn("render sign-in page", "err", err)
	}
}
