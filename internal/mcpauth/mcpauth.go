// Package mcpauth is the browser sign-in of the Factory MCP server. It
// is a small OAuth 2.1 authorization server, as the MCP authorization
// specification describes:
//
//   - GET /.well-known/oauth-protected-resource[/mcp] is the protected
//     resource metadata (RFC 9728) of the Factory MCP endpoints. A 401 from
//     /mcp points to it in WWW-Authenticate.
//   - GET /.well-known/oauth-authorization-server is the authorization
//     server metadata (RFC 8414).
//   - POST /oauth/register registers a public client (RFC 7591).
//   - GET /oauth/authorize asks the signed-in dashboard user (session login
//     or OIDC login) to allow the client. POST /oauth/authorize records the
//     answer and returns an authorization code. PKCE with S256 is required.
//   - POST /oauth/token exchanges a code, or a refresh token, for a
//     short-lived access token bound to the user, and a new refresh token.
//   - POST /oauth/revoke revokes a token (RFC 7009).
//
// One sign-in is a Grant. Access tokens (prefix ofm_) live 1 hour, refresh
// tokens (prefix ofr_) 30 days and rotate at each use. Only SHA-256 hashes
// of tokens and codes are stored, in the KV space of the store. API keys
// stay valid on /mcp for clients without a person, such as CI.
package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Token prefixes.
const (
	AccessTokenPrefix  = "ofm_"
	RefreshTokenPrefix = "ofr_"
)

// Scope is the one scope of the Factory MCP server.
const Scope = "mcp"

// Default lifetimes.
const (
	DefaultAccessTTL  = time.Hour
	DefaultRefreshTTL = 30 * 24 * time.Hour
	codeTTL           = 5 * time.Minute
	requestTTL        = 10 * time.Minute
	touchEvery        = 5 * time.Minute
)

// Paths.
const (
	ResourceMetadataPath = "/.well-known/oauth-protected-resource"
	ServerMetadataPath   = "/.well-known/oauth-authorization-server"
	RegisterPath         = "/oauth/register"
	AuthorizePath        = "/oauth/authorize"
	TokenPath            = "/oauth/token" //nolint:gosec // G101: a URL path, not a credential
	RevokePath           = "/oauth/revoke"
)

// ErrInvalidToken means that a token is unknown, expired or revoked.
var ErrInvalidToken = errors.New("mcpauth: invalid or expired token")

// Options configures a Service.
type Options struct {
	Store core.Store
	// PublicURL is OF_PUBLIC_URL. It is the issuer and the base of every URL.
	PublicURL string
	// ResourcePaths are the paths of the protected MCP endpoints, for
	// example "/mcp" and "/api/v1/mcp/factory".
	ResourcePaths []string
	// Session returns the signed-in dashboard user of a browser request, or
	// nil when nobody is signed in. A POST must pass the CSRF check of the
	// dashboard session.
	Session func(r *http.Request) (*core.User, error)
	// LoginPath is the dashboard sign-in page. It gets ?return_to=. Default /login.
	LoginPath string
	// Audit records sign-in events. It can be nil.
	Audit      func(ctx context.Context, action, outcome string, g *Grant, detail map[string]string)
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	Now        func() time.Time
	Logger     *slog.Logger
}

// Client is a registered OAuth client (an MCP client on a developer machine).
type Client struct {
	ID           string    `json:"client_id"`
	Name         string    `json:"client_name,omitempty"`
	RedirectURIs []string  `json:"redirect_uris"`
	CreatedAt    time.Time `json:"created_at"`
}

// Grant is one MCP sign-in: a user allowed a client.
type Grant struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	TeamID     string     `json:"team_id"`
	ClientID   string     `json:"client_id"`
	ClientName string     `json:"client_name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// ExpiresAt is the end of the current refresh token. Without a refresh
	// before this time, the sign-in ends.
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// grantRecord is the stored form of a Grant, with the token hashes.
type grantRecord struct {
	Grant
	AccessHash    string    `json:"access_hash"`
	AccessExpires time.Time `json:"access_expires"`
	RefreshHash   string    `json:"refresh_hash"`
}

func (g *grantRecord) public() *Grant {
	out := g.Grant
	return &out
}

// Active reports whether the sign-in can still be used.
func (g *Grant) Active(now time.Time) bool { return g.RevokedAt == nil && now.Before(g.ExpiresAt) }

// authRequest is a pending authorization request between the consent page
// and the answer of the user.
type authRequest struct {
	ID            string    `json:"id"`
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	State         string    `json:"state,omitempty"`
	CodeChallenge string    `json:"code_challenge"`
	Resource      string    `json:"resource,omitempty"`
	UserID        string    `json:"user_id"`
	Expires       time.Time `json:"expires"`
}

// codeRecord is an issued authorization code.
type codeRecord struct {
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	CodeChallenge string    `json:"code_challenge"`
	UserID        string    `json:"user_id"`
	TeamID        string    `json:"team_id"`
	Expires       time.Time `json:"expires"`
}

// Service is the authorization server.
type Service struct {
	opts Options
	log  *slog.Logger
	mu   sync.Mutex // serializes the read-modify-write of KV indexes and grants on this replica
}

// New returns a Service.
func New(opts Options) *Service {
	opts.PublicURL = strings.TrimRight(opts.PublicURL, "/")
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.AccessTTL <= 0 {
		opts.AccessTTL = DefaultAccessTTL
	}
	if opts.RefreshTTL <= 0 {
		opts.RefreshTTL = DefaultRefreshTTL
	}
	if opts.LoginPath == "" {
		opts.LoginPath = "/login"
	}
	if len(opts.ResourcePaths) == 0 {
		opts.ResourcePaths = []string{"/mcp"}
	}
	return &Service{opts: opts, log: opts.Logger.With("component", "mcp-auth")}
}

// Enabled reports whether browser sign-in can work: it needs the public URL.
func (s *Service) Enabled() bool { return s != nil && s.opts.PublicURL != "" }

// ResourceMetadataURL returns the metadata URL of the resource at path, for
// the WWW-Authenticate header of a 401.
func (s *Service) ResourceMetadataURL(path string) string {
	return s.opts.PublicURL + ResourceMetadataPath + path
}

// Register mounts the endpoints.
func (s *Service) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+ResourceMetadataPath, s.handleResourceMetadata)
	for _, p := range s.opts.ResourcePaths {
		mux.HandleFunc("GET "+ResourceMetadataPath+p, s.handleResourceMetadata)
	}
	mux.HandleFunc("GET "+ServerMetadataPath, s.handleServerMetadata)
	mux.HandleFunc("POST "+RegisterPath, s.handleRegister)
	mux.HandleFunc("GET "+AuthorizePath, s.handleAuthorize)
	mux.HandleFunc("POST "+AuthorizePath, s.handleConsent)
	mux.HandleFunc("POST "+TokenPath, s.handleToken)
	mux.HandleFunc("POST "+RevokePath, s.handleRevoke)
}

// ---------- tokens and storage ----------

func randomToken(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *Service) kvGet(ctx context.Context, key string, v any) (bool, error) {
	raw, err := s.opts.Store.KVGet(ctx, key)
	if errors.Is(err, core.ErrNotFound) || (err == nil && raw == "") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if v == nil {
		return true, nil
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}
	return true, nil
}

func (s *Service) kvPut(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.opts.Store.KVSet(ctx, key, string(b))
}

// kvDelete empties a key. The KV space has no delete.
func (s *Service) kvDelete(ctx context.Context, key string) error {
	return s.opts.Store.KVSet(ctx, key, "")
}

func clientKey(id string) string  { return "mcpauth:client:" + id }
func requestKey(id string) string { return "mcpauth:request:" + id }
func codeKey(hash string) string  { return "mcpauth:code:" + hash }
func grantKey(id string) string   { return "mcpauth:grant:" + id }
func accessKey(h string) string   { return "mcpauth:access:" + h }
func refreshKey(h string) string  { return "mcpauth:refresh:" + h }
func userIndex(id string) string  { return "mcpauth:user:" + id }
func teamIndex(id string) string  { return "mcpauth:team:" + id }

func (s *Service) client(ctx context.Context, id string) (*Client, error) {
	var c Client
	ok, err := s.kvGet(ctx, clientKey(id), &c)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &c, nil
}

func (s *Service) loadGrant(ctx context.Context, id string) (*grantRecord, error) {
	var g grantRecord
	ok, err := s.kvGet(ctx, grantKey(id), &g)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, core.ErrNotFound
	}
	return &g, nil
}

func (s *Service) addToIndex(ctx context.Context, key, id string) error {
	var ids []string
	if _, err := s.kvGet(ctx, key, &ids); err != nil {
		return err
	}
	if slices.Contains(ids, id) {
		return nil
	}
	return s.kvPut(ctx, key, append(ids, id))
}

func (s *Service) removeFromIndex(ctx context.Context, key, id string) error {
	var ids []string
	if _, err := s.kvGet(ctx, key, &ids); err != nil {
		return err
	}
	out := slices.DeleteFunc(ids, func(x string) bool { return x == id })
	return s.kvPut(ctx, key, out)
}

// issue stores a new access token and refresh token for g and returns them.
func (s *Service) issue(ctx context.Context, g *grantRecord) (access, refresh string, err error) {
	now := s.opts.Now().UTC()
	if g.AccessHash != "" {
		_ = s.kvDelete(ctx, accessKey(g.AccessHash))
	}
	if g.RefreshHash != "" {
		_ = s.kvDelete(ctx, refreshKey(g.RefreshHash))
	}
	access, refresh = randomToken(AccessTokenPrefix), randomToken(RefreshTokenPrefix)
	g.AccessHash, g.RefreshHash = hashOf(access), hashOf(refresh)
	g.AccessExpires = now.Add(s.opts.AccessTTL)
	g.ExpiresAt = now.Add(s.opts.RefreshTTL)
	if err := s.kvPut(ctx, grantKey(g.ID), g); err != nil {
		return "", "", err
	}
	if err := s.opts.Store.KVSet(ctx, accessKey(g.AccessHash), g.ID); err != nil {
		return "", "", err
	}
	if err := s.opts.Store.KVSet(ctx, refreshKey(g.RefreshHash), g.ID); err != nil {
		return "", "", err
	}
	return access, refresh, nil
}

// Verify checks an access token and returns its sign-in and the current
// user. The user must still exist in the team of the sign-in.
func (s *Service) Verify(ctx context.Context, token string) (*Grant, *core.User, error) {
	if !strings.HasPrefix(token, AccessTokenPrefix) {
		return nil, nil, ErrInvalidToken
	}
	h := hashOf(token)
	id, err := s.opts.Store.KVGet(ctx, accessKey(h))
	if errors.Is(err, core.ErrNotFound) || (err == nil && id == "") {
		return nil, nil, ErrInvalidToken
	}
	if err != nil {
		return nil, nil, err
	}
	g, err := s.loadGrant(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil, ErrInvalidToken
	}
	if err != nil {
		return nil, nil, err
	}
	now := s.opts.Now().UTC()
	if g.AccessHash != h || !now.Before(g.AccessExpires) || !g.Active(now) {
		return nil, nil, ErrInvalidToken
	}
	u, err := s.opts.Store.GetUser(ctx, g.UserID)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil, ErrInvalidToken
	}
	if err != nil {
		return nil, nil, err
	}
	if u.TeamID != g.TeamID || u.IsServiceAccount() {
		return nil, nil, ErrInvalidToken
	}
	if g.LastUsedAt == nil || now.Sub(*g.LastUsedAt) >= touchEvery {
		s.mu.Lock()
		if cur, err := s.loadGrant(ctx, g.ID); err == nil && cur.AccessHash == h {
			cur.LastUsedAt = &now
			_ = s.kvPut(ctx, grantKey(cur.ID), cur)
		}
		s.mu.Unlock()
		g.LastUsedAt = &now
	}
	return g.public(), u, nil
}

// ListGrants returns the active sign-ins of a user, newest first.
func (s *Service) ListGrants(ctx context.Context, userID string) ([]Grant, error) {
	return s.list(ctx, userIndex(userID))
}

// ListTeamGrants returns the active sign-ins of a team, newest first.
func (s *Service) ListTeamGrants(ctx context.Context, teamID string) ([]Grant, error) {
	return s.list(ctx, teamIndex(teamID))
}

func (s *Service) list(ctx context.Context, index string) ([]Grant, error) {
	var ids []string
	if _, err := s.kvGet(ctx, index, &ids); err != nil {
		return nil, err
	}
	now := s.opts.Now().UTC()
	out := []Grant{}
	for _, id := range ids {
		g, err := s.loadGrant(ctx, id)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if g.Active(now) {
			out = append(out, *g.public())
		}
	}
	slices.SortFunc(out, func(a, b Grant) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// GetGrant returns one sign-in (core.ErrNotFound when unknown).
func (s *Service) GetGrant(ctx context.Context, id string) (*Grant, error) {
	g, err := s.loadGrant(ctx, id)
	if err != nil {
		return nil, err
	}
	return g.public(), nil
}

// Revoke ends a sign-in: its access token and refresh token stop working at once.
func (s *Service) Revoke(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.loadGrant(ctx, id)
	if err != nil {
		return err
	}
	if g.RevokedAt == nil {
		now := s.opts.Now().UTC()
		g.RevokedAt = &now
	}
	_ = s.kvDelete(ctx, accessKey(g.AccessHash))
	_ = s.kvDelete(ctx, refreshKey(g.RefreshHash))
	if err := s.kvPut(ctx, grantKey(g.ID), g); err != nil {
		return err
	}
	_ = s.removeFromIndex(ctx, userIndex(g.UserID), g.ID)
	_ = s.removeFromIndex(ctx, teamIndex(g.TeamID), g.ID)
	s.audit(ctx, "mcp.signin.revoke", "success", g.public(), nil)
	return nil
}

func (s *Service) audit(ctx context.Context, action, outcome string, g *Grant, detail map[string]string) {
	if s.opts.Audit != nil {
		s.opts.Audit(ctx, action, outcome, g, detail)
	}
}
