package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/github"
)

// TokenCipher encrypts per-user tokens (implemented by secrets.Store).
type TokenCipher interface {
	Encrypt(teamID, factoryID, name string, typ core.SecretType, value []byte) ([]byte, error)
	Decrypt(teamID, factoryID, name string, typ core.SecretType, envelope []byte) ([]byte, error)
}

// UserTokens stores personal code-forge tokens of team members, encrypted
// with the server's secret key, in the Store's KV space. They are never
// exposed as team secrets, so agents cannot request them by name.
type UserTokens struct {
	Store  core.Store
	Cipher TokenCipher
}

// UserTokenForges are the forges whose integrations support per-user tokens
// (personal access / OAuth tokens on the same API). Generic git does not.
var UserTokenForges = []string{GitHub, GitLab, AzureDevOps}

// SupportsUserTokens reports whether codeForge supports per-user tokens.
func SupportsUserTokens(codeForge string) bool {
	p := IntegrationProvider(codeForge)
	return p == "github" || p == "gitlab" || p == "azure_devops"
}

func userTokenKey(teamID, userID, provider string) string {
	return "forge:user-token:" + teamID + ":" + userID + ":" + provider
}

func userTokenName(userID, provider string) string {
	return "user-forge-token/" + userID + "/" + provider
}

// UserTokenStorage says where the sealed token of a user is stored: the key in
// the KV space, and the name and type that the ciphertext binds. The command
// "secret rewrap" uses it.
func UserTokenStorage(teamID, userID, codeForge string) (kvKey, name string, typ core.SecretType) {
	p := IntegrationProvider(codeForge)
	return userTokenKey(teamID, userID, p), userTokenName(userID, p), core.SecretGeneric
}

// Set stores (or replaces) a user's token for codeForge.
func (u *UserTokens) Set(ctx context.Context, teamID, userID, codeForge, token string) error {
	if u == nil || u.Store == nil || u.Cipher == nil {
		return errors.New("forge: user tokens are not configured")
	}
	if !SupportsUserTokens(codeForge) {
		return fmt.Errorf("%w: %s does not support per-user tokens (supported: %s)", core.ErrInvalid, codeForge, strings.Join(UserTokenForges, ", "))
	}
	token = strings.TrimSpace(token)
	if teamID == "" || userID == "" || token == "" {
		return fmt.Errorf("%w: team, user and token are required", core.ErrInvalid)
	}
	p := IntegrationProvider(codeForge)
	env, err := u.Cipher.Encrypt(teamID, "", userTokenName(userID, p), core.SecretGeneric, []byte(token))
	if err != nil {
		return err
	}
	return u.Store.KVSet(ctx, userTokenKey(teamID, userID, p), base64.StdEncoding.EncodeToString(env))
}

// Delete removes a user's token for codeForge.
func (u *UserTokens) Delete(ctx context.Context, teamID, userID, codeForge string) error {
	if u == nil || u.Store == nil {
		return errors.New("forge: user tokens are not configured")
	}
	return u.Store.KVSet(ctx, userTokenKey(teamID, userID, IntegrationProvider(codeForge)), "")
}

// Get returns a user's token for codeForge (core.ErrNotFound when none).
func (u *UserTokens) Get(ctx context.Context, teamID, userID, codeForge string) (string, error) {
	if u == nil || u.Store == nil || u.Cipher == nil || userID == "" {
		return "", core.ErrNotFound
	}
	p := IntegrationProvider(codeForge)
	v, err := u.Store.KVGet(ctx, userTokenKey(teamID, userID, p))
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", core.ErrNotFound
	}
	env, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return "", fmt.Errorf("forge: user token: %w", err)
	}
	tok, err := u.Cipher.Decrypt(teamID, "", userTokenName(userID, p), core.SecretGeneric, env)
	if err != nil {
		return "", err
	}
	return string(tok), nil
}

// List returns the forges for which the user stored a token. unreadable
// names the forges whose stored token cannot be decrypted (for example after
// the secret key changed); runs that need such a token fail until the user
// stores it again. err reports a failed read of the store.
func (u *UserTokens) List(ctx context.Context, teamID, userID string) (forges, unreadable []string, err error) {
	for _, f := range UserTokenForges {
		_, err := u.Get(ctx, teamID, userID, f)
		switch {
		case err == nil:
			forges = append(forges, f)
		case errors.Is(err, core.ErrNotFound):
		case u.stored(ctx, teamID, userID, f):
			forges = append(forges, f)
			unreadable = append(unreadable, f)
		default:
			return nil, nil, err
		}
	}
	return forges, unreadable, nil
}

// stored reports whether a non-empty token entry exists for codeForge.
func (u *UserTokens) stored(ctx context.Context, teamID, userID, codeForge string) bool {
	v, err := u.Store.KVGet(ctx, userTokenKey(teamID, userID, IntegrationProvider(codeForge)))
	return err == nil && v != ""
}

// ForgeForUser returns a client acting as userID for one forge action (the
// same lookup as Forge). It selects the record of the owner of l.Repo.
// When the record supports per-user tokens (github,
// gitlab, azure_devops, and the record does not set config user_tokens:
// "false") and the user stored a token, the client uses that token against
// the API address of the record. When the record supports per-user tokens
// and the user has none, it returns an error that wraps
// core.ErrExternalAuthRequired. Otherwise it returns the client of the
// record (Forge). The boolean reports whether the user's token is used.
func (r *Resolver) ForgeForUser(ctx context.Context, l core.ForgeLookup, userID string) (core.Forge, bool, error) {
	codeForge := l.CodeForge
	if userID == "" || r.opts.UserTokens == nil || !SupportsUserTokens(codeForge) {
		f, err := r.Forge(ctx, l)
		return f, false, err
	}
	provider := IntegrationProvider(codeForge)
	in, err := r.Select(ctx, l)
	if err != nil {
		return nil, false, err
	}
	if !in.Active() {
		return nil, false, disabledError(in)
	}
	if strings.EqualFold(in.Config["user_tokens"], "false") {
		f, err := r.Forge(ctx, l)
		return f, false, err
	}
	teamID := l.TeamID
	tok, err := r.opts.UserTokens.Get(ctx, teamID, userID, codeForge)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			return nil, false, err
		}
		// No silent fallback to the team credential.
		return nil, false, fmt.Errorf("%w: user %s has no linked %s account: link it in the dashboard (Settings, Accounts)", core.ErrExternalAuthRequired, userID, strings.ToLower(codeForge))
	}
	cfg := in.Config
	var f core.Forge
	switch provider {
	case "github":
		f, err = github.New(github.Config{BaseURL: cfg["base_url"], WebURL: cfg["web_url"], Token: tok, HTTPClient: r.opts.HTTPClient})
	default:
		// Plugin forges get the integration config with UserToken set; the
		// GitLab plugin then uses config user_token_type instead of token_type.
		f, err = r.pluginForge(ctx, ForgeTarget{Provider: provider, IntegrationID: in.ID, TeamID: teamID, Config: cfg, Token: tok, UserToken: true})
	}
	if err != nil {
		return nil, false, err
	}
	return f, true, nil
}
