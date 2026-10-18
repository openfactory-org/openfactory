// Package teams connects factories to Microsoft Teams through the Bot
// Framework: the activities endpoint (Bot Connector JWT validation with
// OpenID metadata + JWKS), mentions, sign in / sign out / help commands,
// channel / team / author attributes, and a core.Responder that replies in
// the originating thread through the Bot Connector REST API.
package teams

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// Bot Framework constants.
const (
	DefaultOpenIDMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	BotFrameworkIssuer       = "https://api.botframework.com"
	DefaultTokenTenant       = "botframework.com" //nolint:gosec // G101: a tenant domain, not a credential
	DefaultLoginURL          = "https://login.microsoftonline.com"
	BotConnectorScope        = "https://api.botframework.com/.default"
	GraphScope               = "https://graph.microsoft.com/.default"
	clockSkew                = 5 * time.Minute
)

// KeySet validates Bot Connector tokens using the OpenID metadata document and its JWKS.
type KeySet struct {
	MetadataURL string
	HTTPClient  *http.Client
	Now         func() time.Time

	mu      sync.Mutex
	keys    map[string]keyEntry
	fetched time.Time
}

type keyEntry struct {
	pub          *rsa.PublicKey
	endorsements []string
}

func (k *KeySet) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

func (k *KeySet) refresh(ctx context.Context) error {
	c := &forgeutil.Client{HTTP: k.HTTPClient, UserAgent: "openfactory-teams"}
	mu := k.MetadataURL
	if mu == "" {
		mu = DefaultOpenIDMetadataURL
	}
	var meta struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := c.Do(ctx, http.MethodGet, mu, nil, &meta); err != nil {
		return fmt.Errorf("teams: openid metadata: %w", err)
	}
	if meta.JWKSURI == "" {
		return errors.New("teams: openid metadata has no jwks_uri")
	}
	var jwks struct {
		Keys []forgeutil.JWK `json:"keys"`
	}
	if err := c.Do(ctx, http.MethodGet, meta.JWKSURI, nil, &jwks); err != nil {
		return fmt.Errorf("teams: jwks: %w", err)
	}
	keys := map[string]keyEntry{}
	for _, j := range jwks.Keys {
		pub, err := j.RSAPublicKey()
		if err != nil {
			continue
		}
		keys[j.Kid] = keyEntry{pub: pub, endorsements: j.Endorsements}
	}
	k.mu.Lock()
	k.keys, k.fetched = keys, k.now()
	k.mu.Unlock()
	return nil
}

func (k *KeySet) key(ctx context.Context, kid string) (keyEntry, error) {
	k.mu.Lock()
	e, ok := k.keys[kid]
	stale := k.now().Sub(k.fetched) > 24*time.Hour
	recent := k.now().Sub(k.fetched) < time.Minute
	k.mu.Unlock()
	if ok && !stale {
		return e, nil
	}
	if !ok && recent && !stale && k.keys != nil {
		return keyEntry{}, fmt.Errorf("teams: unknown signing key %q", kid)
	}
	if err := k.refresh(ctx); err != nil {
		if ok {
			return e, nil // keep using a known key if metadata is temporarily unavailable
		}
		return keyEntry{}, err
	}
	k.mu.Lock()
	e, ok = k.keys[kid]
	k.mu.Unlock()
	if !ok {
		return keyEntry{}, fmt.Errorf("teams: unknown signing key %q", kid)
	}
	return e, nil
}

// Validate checks an "Authorization: Bearer ..." header sent by the Bot Connector:
// RS256 signature from the published keys, issuer, audience (the bot's app ID),
// lifetime, channel endorsement and the serviceUrl claim.
func (k *KeySet) Validate(ctx context.Context, authHeader string, appIDs []string, channelID, serviceURL string) error {
	tok, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok || tok == "" {
		return errors.New("teams: missing bearer token")
	}
	j, err := forgeutil.ParseJWT(tok)
	if err != nil {
		return err
	}
	e, err := k.key(ctx, j.HeaderString("kid"))
	if err != nil {
		return err
	}
	if err := j.VerifyRS256(e.pub); err != nil {
		return err
	}
	if iss := j.ClaimString("iss"); iss != BotFrameworkIssuer {
		return fmt.Errorf("teams: unexpected issuer %q", iss)
	}
	audOK := false
	for _, a := range appIDs {
		if a != "" && j.HasAudience(a) {
			audOK = true
			break
		}
	}
	if !audOK {
		return errors.New("teams: token audience does not match the bot app id")
	}
	if err := j.VerifyTime(k.now(), clockSkew); err != nil {
		return err
	}
	if len(e.endorsements) > 0 && channelID != "" {
		found := false
		for _, en := range e.endorsements {
			if strings.EqualFold(en, channelID) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("teams: signing key not endorsed for channel %q", channelID)
		}
	}
	if su := j.ClaimString("serviceurl"); su != "" && serviceURL != "" && !strings.EqualFold(strings.TrimRight(su, "/"), strings.TrimRight(serviceURL, "/")) {
		return errors.New("teams: serviceUrl claim does not match the activity")
	}
	return nil
}

// TokenSource obtains Bot Connector access tokens with the OAuth client-credentials grant.
type TokenSource struct {
	AppID       string
	AppPassword string
	// Tenant is the token tenant: "botframework.com" for multi-tenant bots, or the Entra tenant ID for single-tenant bots.
	Tenant string
	// FallbackTenant is tried when a token request against Tenant fails (for
	// example a single-tenant bot asking the botframework.com tenant). When it
	// succeeds, it becomes the Tenant for later requests.
	FallbackTenant string
	// Scope is the scope of the token (default BotConnectorScope).
	Scope      string
	LoginURL   string
	HTTPClient *http.Client
	Now        func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token returns a cached or fresh access token.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	t.mu.Lock()
	if t.token != "" && now().Before(t.expires) {
		tok := t.token
		t.mu.Unlock()
		return tok, nil
	}
	tenant, fallback := t.Tenant, t.FallbackTenant
	t.mu.Unlock()
	if tenant == "" {
		tenant = DefaultTokenTenant
	}
	tok, ttl, err := t.fetch(ctx, tenant)
	if err != nil && fallback != "" && !strings.EqualFold(fallback, tenant) {
		var ferr error
		if tok, ttl, ferr = t.fetch(ctx, fallback); ferr != nil {
			return "", fmt.Errorf("%w (tenant %s: %w)", err, fallback, ferr)
		}
		err = nil
		t.mu.Lock()
		t.Tenant, t.FallbackTenant = fallback, ""
		t.mu.Unlock()
	}
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	t.token, t.expires = tok, now().Add(ttl-5*time.Minute)
	t.mu.Unlock()
	return tok, nil
}

// TokenTenant returns the tenant the source currently requests tokens from.
func (t *TokenSource) TokenTenant() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Tenant == "" {
		return DefaultTokenTenant
	}
	return t.Tenant
}

// fetch performs one client-credentials token request against tenant.
func (t *TokenSource) fetch(ctx context.Context, tenant string) (string, time.Duration, error) {
	login := t.LoginURL
	if login == "" {
		login = DefaultLoginURL
	}
	c := &forgeutil.Client{HTTP: t.HTTPClient, UserAgent: "openfactory-teams"}
	var resp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	scope := t.Scope
	if scope == "" {
		scope = BotConnectorScope
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.AppID},
		"client_secret": {t.AppPassword},
		"scope":         {scope},
	}
	if err := c.Do(ctx, http.MethodPost, strings.TrimRight(login, "/")+"/"+url.PathEscape(tenant)+"/oauth2/v2.0/token", form, &resp); err != nil {
		return "", 0, fmt.Errorf("teams: bot token (tenant %s): %w", tenant, err)
	}
	if resp.AccessToken == "" {
		return "", 0, errors.New("teams: empty bot token")
	}
	ttl := time.Duration(resp.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return resp.AccessToken, ttl, nil
}
