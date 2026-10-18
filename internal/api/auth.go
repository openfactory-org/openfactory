package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// API key format: of_<prefix>_<secret> for a user, ofa_<prefix>_<secret>
// for a service account.
//
//   - prefix: 8 lowercase alphanumerics, stored in clear and used for lookup
//   - secret: 40 alphanumerics (~238 bits of entropy)
//
// Only the SHA-256 hash of the full key is persisted.
const (
	KeyPrefix = "of_"
	// ServiceAccountKeyPrefix starts the API key of a service account.
	ServiceAccountKeyPrefix = "ofa_"
	keyPrefixLen            = 8
	keySecretLen            = 40
	keyLowerAlnum           = "abcdefghijklmnopqrstuvwxyz0123456789"
	keyAlnum                = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	touchResolution         = time.Minute
)

// GeneratedKey is a freshly minted API key. Key is shown to the user once.
type GeneratedKey struct {
	Key    string // full secret key: of_<prefix>_<secret>
	Prefix string // lookup prefix
	Hash   string // hex SHA-256 of Key, to persist in core.APIKey.HashedKey
}

// GenerateKey creates a new random API key of a user.
func GenerateKey() (GeneratedKey, error) { return generateKey(KeyPrefix) }

func generateKey(kind string) (GeneratedKey, error) {
	prefix, err := randomString(keyLowerAlnum, keyPrefixLen)
	if err != nil {
		return GeneratedKey{}, err
	}
	secret, err := randomString(keyAlnum, keySecretLen)
	if err != nil {
		return GeneratedKey{}, err
	}
	key := kind + prefix + "_" + secret
	return GeneratedKey{Key: key, Prefix: prefix, Hash: HashKey(key)}, nil
}

// HashKey returns the hex-encoded SHA-256 of a key.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ParseKey validates the key format and returns its lookup prefix. It
// accepts user keys (of_) and service account keys (ofa_).
func ParseKey(key string) (prefix string, ok bool) {
	rest, found := strings.CutPrefix(key, KeyPrefix)
	if !found {
		rest, found = strings.CutPrefix(key, ServiceAccountKeyPrefix)
	}
	if !found {
		return "", false
	}
	prefix, secret, found := strings.Cut(rest, "_")
	if !found || len(prefix) != keyPrefixLen || len(secret) != keySecretLen {
		return "", false
	}
	if !allIn(prefix, keyLowerAlnum) || !allIn(secret, keyAlnum) {
		return "", false
	}
	return prefix, true
}

// IsAPIKey reports whether a bearer token has the form of an API key of a
// user or a service account.
func IsAPIKey(tok string) bool {
	return strings.HasPrefix(tok, KeyPrefix) || strings.HasPrefix(tok, ServiceAccountKeyPrefix)
}

// NewAPIKey mints a key for a user and returns the record to store plus the plaintext key.
func NewAPIKey(teamID, userID, name string) (*core.APIKey, string, error) {
	return newAPIKey(KeyPrefix, teamID, userID, name)
}

// NewServiceAccountKey mints a key (ofa_) for a service account.
func NewServiceAccountKey(teamID, accountID, name string) (*core.APIKey, string, error) {
	return newAPIKey(ServiceAccountKeyPrefix, teamID, accountID, name)
}

func newAPIKey(kind, teamID, userID, name string) (*core.APIKey, string, error) {
	g, err := generateKey(kind)
	if err != nil {
		return nil, "", err
	}
	return &core.APIKey{
		ID:        core.NewID("key"),
		TeamID:    teamID,
		UserID:    userID,
		Name:      name,
		Prefix:    g.Prefix,
		HashedKey: g.Hash,
		CreatedAt: time.Now().UTC(),
	}, g.Key, nil
}

func randomString(alphabet string, n int) (string, error) {
	max := big.NewInt(int64(len(alphabet)))
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("api: random: %w", err)
		}
		b[i] = alphabet[v.Int64()]
	}
	return string(b), nil
}

func allIn(s, alphabet string) bool {
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// Principal is the authenticated caller of a request.
type Principal struct {
	User   *core.User
	TeamID string
	Role   core.Role
	APIKey *core.APIKey
}

// ActorID returns the identity recorded as actor on engine operations.
func (p *Principal) ActorID() string {
	if p == nil || p.User == nil {
		return "api"
	}
	return "user:" + p.User.ID
}

// UserID returns the authenticated user's ID ("" when unknown).
func (p *Principal) UserID() string {
	if p == nil || p.User == nil {
		return ""
	}
	return p.User.ID
}

// Can reports whether the principal's team-wide role is at least min.
func (p *Principal) Can(min core.Role) bool {
	return p != nil && roleRank(p.Role) >= roleRank(min)
}

// RoleFor returns the effective role of the principal for one factory: the
// role override of the user for that factory if one exists, else the
// team-wide role. An empty factoryID returns the team-wide role.
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

// CanOn reports whether the effective role of the principal for a factory
// is at least min. An empty factoryID checks the team-wide role.
func (p *Principal) CanOn(factoryID string, min core.Role) bool {
	return p != nil && roleRank(p.RoleFor(factoryID)) >= roleRank(min)
}

func roleRank(r core.Role) int { return core.RoleRank(r) }

// ValidRole reports whether r is a known role.
func ValidRole(r core.Role) bool { return roleRank(r) > 0 }

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored in ctx, if any.
func PrincipalFrom(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// ErrInvalidKey is returned by Authenticate for unknown, malformed or revoked keys.
var ErrInvalidKey = fmt.Errorf("%w: invalid API key", core.ErrUnauthorized)

// Authenticate verifies a bearer API key against the store and resolves the
// caller. The hash comparison is constant time.
func Authenticate(ctx context.Context, store core.Store, key string) (*Principal, error) {
	prefix, ok := ParseKey(key)
	if !ok {
		return nil, ErrInvalidKey
	}
	k, err := store.GetAPIKeyByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, ErrInvalidKey
		}
		return nil, err
	}
	want := []byte(k.HashedKey)
	got := []byte(HashKey(key))
	if len(want) != len(got) || subtle.ConstantTimeCompare(want, got) != 1 {
		return nil, ErrInvalidKey
	}
	u, err := store.GetUser(ctx, k.UserID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil, ErrInvalidKey
		}
		return nil, err
	}
	if u.TeamID != k.TeamID {
		return nil, ErrInvalidKey
	}
	// A user key never authenticates a service account, and the reverse.
	if strings.HasPrefix(key, ServiceAccountKeyPrefix) != u.IsServiceAccount() {
		return nil, ErrInvalidKey
	}
	now := time.Now().UTC()
	if k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) >= touchResolution {
		_ = store.TouchAPIKey(ctx, k.ID, now)
		k.LastUsedAt = &now
	}
	role := core.NormalizeRole(u.Role)
	if !ValidRole(role) {
		role = core.RoleUser
	}
	return &Principal{User: u, TeamID: k.TeamID, Role: role, APIKey: k}, nil
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}
