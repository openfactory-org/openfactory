package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// OAuthProver is a Prover for an OAuth 2.0 or OpenID Connect provider: the
// authorization code flow, then a user-info request that returns the
// account ID.
type OAuthProver struct {
	AuthorizeURL string
	TokenURL     string
	UserInfoURL  string
	ClientID     string
	ClientSecret string
	Scopes       []string
	// IDField is the key of the account ID in the user-info document, for
	// example "id" or "sub". HandleField is the key of a display name.
	IDField     string
	HandleField string
	// ScopeSeparator joins the scopes in the authorization URL. The default
	// is a space; Linear wants a comma.
	ScopeSeparator string
	// AuthParams are extra query parameters of the authorization URL.
	AuthParams map[string]string
	// TokenJSON sends the token request as a JSON body, not as a form.
	TokenJSON bool
	// UserInfoBody, if it is set, makes the user-info request a POST with
	// this JSON body (a GraphQL query). IDField and HandleField can then be
	// dotted paths into the answer, such as "data.viewer.id".
	UserInfoBody string
	HTTPClient   *http.Client
}

var _ Prover = (*OAuthProver)(nil)

// AuthURL implements Prover.
func (p *OAuthProver) AuthURL(state, redirectURI string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {p.ClientID},
		"redirect_uri":  {redirectURI},
		"state":         {state},
	}
	if len(p.Scopes) > 0 {
		sep := p.ScopeSeparator
		if sep == "" {
			sep = " "
		}
		q.Set("scope", strings.Join(p.Scopes, sep))
	}
	for k, v := range p.AuthParams {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(p.AuthorizeURL, "?") {
		sep = "&"
	}
	return p.AuthorizeURL + sep + q.Encode()
}

func (p *OAuthProver) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// Exchange implements Prover.
func (p *OAuthProver) Exchange(ctx context.Context, code, redirectURI string) (string, string, error) {
	if code == "" {
		return "", "", errors.New("no authorization code")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
	}
	body, contentType := form.Encode(), "application/x-www-form-urlencoded"
	if p.TokenJSON {
		m := make(map[string]string, len(form))
		for k := range form {
			m[k] = form.Get(k)
		}
		b, _ := json.Marshal(m)
		body, contentType = string(b), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(body)) //nolint:gosec // G704: the URL comes from the integration config that a team admin sets
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := p.doJSON(req, &tok); err != nil {
		return "", "", fmt.Errorf("token request: %w", err)
	}
	if tok.AccessToken == "" {
		return "", "", fmt.Errorf("token request: no access token (%s)", tok.Error)
	}
	if p.UserInfoBody != "" {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, p.UserInfoURL, strings.NewReader(p.UserInfoBody)) //nolint:gosec // G704: the URL comes from the integration config that a team admin sets
		if req != nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoURL, nil) //nolint:gosec // G704: the URL comes from the integration config that a team admin sets
	}
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	var info map[string]any
	if err := p.doJSON(req, &info); err != nil {
		return "", "", fmt.Errorf("user info: %w", err)
	}
	id := fieldString(info, p.IDField)
	if id == "" {
		return "", "", fmt.Errorf("user info: no %q", p.IDField)
	}
	return id, fieldString(info, p.HandleField), nil
}

func (p *OAuthProver) doJSON(req *http.Request, out any) error {
	resp, err := p.client().Do(req) //nolint:gosec // G704: the URL comes from the integration config that a team admin sets
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// fieldString returns a field of a JSON object as a string. A number is
// written without a fraction (account IDs). The key is a field name, or a
// dotted path into nested objects. A field name wins over a path, so a key
// that holds dots (a URL) still works.
func fieldString(m map[string]any, key string) string {
	if key == "" {
		return ""
	}
	val, ok := m[key]
	for rest := key; !ok && strings.Contains(rest, "."); {
		head, tail, _ := strings.Cut(rest, ".")
		sub, isMap := m[head].(map[string]any)
		if !isMap {
			break
		}
		m, rest = sub, tail
		val, ok = m[rest]
	}
	switch v := val.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case json.Number:
		return v.String()
	}
	return ""
}

// Config keys of the provider sign-in of an integration record.
const (
	ConfigOAuthClientID        = "oauth_client_id"
	ConfigOAuthClientSecretRef = "oauth_client_secret_ref" //nolint:gosec // G101: a config key name, not a credential
	ConfigOAuthAuthorizeURL    = "oauth_authorize_url"
	ConfigOAuthTokenURL        = "oauth_token_url" //nolint:gosec // G101: a config key name, not a credential
	ConfigOAuthUserInfoURL     = "oauth_userinfo_url"
	ConfigOAuthIDField         = "oauth_id_field"
	ConfigOAuthHandleField     = "oauth_handle_field"
	ConfigOAuthScopes          = "oauth_scopes"
)

// ProverFromIntegration builds the sign-in of a provider from an integration
// record. The record needs oauth_client_id and oauth_client_secret_ref (a
// secret name). GitHub, Slack, Microsoft Teams, Linear and Jira have default
// endpoints and scopes, and the oauth_* keys override them. Another provider
// needs oauth_authorize_url, oauth_token_url, oauth_userinfo_url and
// oauth_id_field. It returns nil, nil when the record has no client ID.
func ProverFromIntegration(ctx context.Context, secrets core.SecretStore, in core.Integration, httpClient *http.Client) (Prover, error) {
	cfg := in.Config
	if cfg[ConfigOAuthClientID] == "" {
		return nil, nil
	}
	secret, err := forgeutil.IntegrationSecret(ctx, secrets, in, ConfigOAuthClientSecretRef)
	if err != nil {
		return nil, err
	}
	p := &OAuthProver{ClientID: cfg[ConfigOAuthClientID], ClientSecret: string(secret), HTTPClient: httpClient}
	switch strings.ToLower(in.Provider) {
	case "github":
		web := strings.TrimRight(cfg["web_url"], "/")
		if web == "" {
			web = "https://github.com"
		}
		api := strings.TrimRight(cfg["base_url"], "/")
		if api == "" {
			api = "https://api.github.com"
		}
		p.AuthorizeURL, p.TokenURL, p.UserInfoURL = web+"/login/oauth/authorize", web+"/login/oauth/access_token", api+"/user"
		p.IDField, p.HandleField = "id", "login"
	case "slack":
		// Sign in with Slack (OpenID Connect).
		p.AuthorizeURL, p.TokenURL, p.UserInfoURL = "https://slack.com/openid/connect/authorize", "https://slack.com/api/openid.connect.token", "https://slack.com/api/openid.connect.userInfo"
		p.IDField, p.HandleField, p.Scopes = "https://slack.com/user_id", "name", []string{"openid", "profile"}
	case "microsoft_teams", "teams":
		// Microsoft identity platform. The account ID is the Entra object ID
		// of the user: the same value as aadObjectId in a Teams message.
		// tenant_id limits the sign-in to one tenant; without it, any work
		// or school account can sign in.
		tenant := strings.TrimSpace(cfg["tenant_id"])
		if tenant == "" {
			tenant = "organizations"
		}
		login := "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0"
		p.AuthorizeURL, p.TokenURL, p.UserInfoURL = login+"/authorize", login+"/token", "https://graph.microsoft.com/v1.0/me"
		p.IDField, p.HandleField, p.Scopes = "id", "displayName", []string{"openid", "profile", "User.Read"}
	case "linear":
		// Linear has no user-info endpoint. A GraphQL query returns the
		// viewer. The ID is the user ID that Linear webhooks name as actor.
		p.AuthorizeURL, p.TokenURL, p.UserInfoURL = "https://linear.app/oauth/authorize", "https://api.linear.app/oauth/token", "https://api.linear.app/graphql"
		p.UserInfoBody = `{"query":"{ viewer { id name } }"}`
		p.IDField, p.HandleField, p.Scopes, p.ScopeSeparator = "data.viewer.id", "data.viewer.name", []string{"read"}, ","
	case "jira":
		// Atlassian OAuth 2.0 (3LO). The account ID is the accountId that
		// Jira webhooks name as actor.
		p.AuthorizeURL, p.TokenURL, p.UserInfoURL = "https://auth.atlassian.com/authorize", "https://auth.atlassian.com/oauth/token", "https://api.atlassian.com/me"
		p.IDField, p.HandleField, p.Scopes = "account_id", "name", []string{"read:me"}
		p.AuthParams = map[string]string{"audience": "api.atlassian.com", "prompt": "consent"}
		p.TokenJSON = true
	}
	for key, dst := range map[string]*string{
		ConfigOAuthAuthorizeURL: &p.AuthorizeURL, ConfigOAuthTokenURL: &p.TokenURL, ConfigOAuthUserInfoURL: &p.UserInfoURL,
		ConfigOAuthIDField: &p.IDField, ConfigOAuthHandleField: &p.HandleField,
	} {
		if v := strings.TrimSpace(cfg[key]); v != "" {
			*dst = v
		}
	}
	if v := strings.TrimSpace(cfg[ConfigOAuthScopes]); v != "" {
		p.Scopes = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	if p.AuthorizeURL == "" || p.TokenURL == "" || p.UserInfoURL == "" || p.IDField == "" {
		return nil, fmt.Errorf("integration %s: the %s sign-in needs %s, %s, %s and %s", in.ID, in.Provider,
			ConfigOAuthAuthorizeURL, ConfigOAuthTokenURL, ConfigOAuthUserInfoURL, ConfigOAuthIDField)
	}
	return p, nil
}

// TeamProver returns the sign-in of a provider for a team. It uses the
// record that selectSignIn selects. An account link belongs to the team,
// not to a factory, so it selects with no factory. Provider names of
// account links ("teams") match their integration provider
// ("microsoft_teams"). It returns nil, nil when no record has a sign-in.
func TeamProver(ctx context.Context, store core.Store, secrets core.SecretStore, teamID, provider string, httpClient *http.Client) (Prover, error) {
	ints, err := store.ListIntegrations(ctx, teamID)
	if err != nil {
		return nil, err
	}
	in, ok, err := selectSignIn(ints, provider, "")
	if err != nil || !ok {
		return nil, err
	}
	if !in.Active() {
		return nil, fmt.Errorf("%s: the integration record %s has the sign-in and is not active (%s)", provider, in.ID, in.Status)
	}
	return ProverFromIntegration(ctx, secrets, in, httpClient)
}

// selectSignIn selects the record that holds the sign-in of a provider
// (oauth_client_id), with the integration selection rule. Only the records
// of the provider with a client ID count. The record of factoryID wins over
// the team record (no factory). With no factory, a team with no team record
// uses its record of one factory, when it has exactly one. Two records at
// the same level give an error that names the provider. The workspace key
// (or org) of a record does not help here: a link names no workspace. The
// rule never picks by order. ok is false when no record has a sign-in. The
// caller checks the status of the record.
func selectSignIn(ints []core.Integration, provider, factoryID string) (core.Integration, bool, error) {
	var fac, team, other []core.Integration
	for _, in := range ints {
		if !core.SameIntegrationProvider(in.Provider, provider) || strings.TrimSpace(in.Config[ConfigOAuthClientID]) == "" {
			continue
		}
		switch {
		case in.FactoryID == "":
			team = append(team, in)
		case factoryID != "" && in.FactoryID == factoryID:
			fac = append(fac, in)
		case factoryID == "":
			other = append(other, in)
		}
	}
	for _, level := range [][]core.Integration{fac, team, other} {
		switch len(level) {
		case 0:
			continue
		case 1:
			return level[0], true, nil
		}
		ids := make([]string, 0, len(level))
		for _, in := range level {
			ids = append(ids, in.ID)
		}
		return core.Integration{}, false, fmt.Errorf("%s: %d integration records (%s) have a sign-in (%s): keep it on one record: %w",
			provider, len(level), strings.Join(ids, ", "), ConfigOAuthClientID, ErrNoIntegration)
	}
	return core.Integration{}, false, nil
}
