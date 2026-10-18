package runnertoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// SessionPrefix starts every session token.
	SessionPrefix = "ofs_"
	// SessionTTL is the life of a session token.
	SessionTTL = time.Hour
	// SessionVersion is the payload version.
	SessionVersion = 1

	maxTokenLen   = 4096
	minKeyLen     = 32
	clockSkewSecs = 60
)

var (
	// ErrMalformed means the token does not have the expected form.
	ErrMalformed = errors.New("runnertoken: malformed token")
	// ErrUnknownKey means the key ID of the token is not in the key set.
	ErrUnknownKey = errors.New("runnertoken: unknown key")
	// ErrBadSignature means the signature does not match the payload.
	ErrBadSignature = errors.New("runnertoken: bad signature")
	// ErrExpired means the token is past its expiry time.
	ErrExpired = errors.New("runnertoken: token expired")
	// ErrNotYetValid means the token was issued too far in the future.
	ErrNotYetValid = errors.New("runnertoken: token not yet valid")
)

// Claims is the payload of a session token.
type Claims struct {
	Version   int    `json:"v"`
	KeyID     string `json:"kid"`
	Team      string `json:"team"`
	Run       string `json:"run"`
	Session   int    `json:"ses"`
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// Expiry returns the expiry time of the token in UTC.
func (c Claims) Expiry() time.Time {
	return time.Unix(c.ExpiresAt, 0).UTC()
}

// KeySet gives the signing keys by key ID.
type KeySet interface {
	// Current returns the key that signs new tokens.
	Current() (kid string, key []byte)
	// Lookup returns the key for a key ID that is not retired.
	Lookup(kid string) (key []byte, ok bool)
}

// StaticKeys is a KeySet with a fixed set of keys.
type StaticKeys struct {
	current string
	keys    map[string][]byte
}

// NewStaticKeys makes a KeySet from a copy of keys. The current key ID must
// be in the map, and each key must have at least 32 bytes.
func NewStaticKeys(current string, keys map[string][]byte) (*StaticKeys, error) {
	if current == "" {
		return nil, errors.New("runnertoken: current key ID is empty")
	}
	if _, ok := keys[current]; !ok {
		return nil, fmt.Errorf("runnertoken: current key %q is not in the key set", current)
	}
	cp := make(map[string][]byte, len(keys))
	for kid, k := range keys {
		if len(k) < minKeyLen {
			return nil, fmt.Errorf("runnertoken: key %q is shorter than %d bytes", kid, minKeyLen)
		}
		cp[kid] = append([]byte(nil), k...)
	}
	return &StaticKeys{current: current, keys: cp}, nil
}

// Current returns the key ID and a copy of the key that signs new tokens.
func (s *StaticKeys) Current() (string, []byte) {
	return s.current, append([]byte(nil), s.keys[s.current]...)
}

// IDs returns the key IDs of all keys, in no fixed order.
func (s *StaticKeys) IDs() []string {
	ids := make([]string, 0, len(s.keys))
	for kid := range s.keys {
		ids = append(ids, kid)
	}
	return ids
}

// Lookup returns a copy of the key for kid.
func (s *StaticKeys) Lookup(kid string) ([]byte, bool) {
	k, ok := s.keys[kid]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), k...), true
}

func sign(key []byte, p string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(p))
	return m.Sum(nil)
}

// Mint makes a signed session token for c. It sets the version, key ID and
// times itself, and returns the token with its expiry time.
func Mint(keys KeySet, c Claims, now time.Time) (string, time.Time, error) {
	if c.Team == "" || c.Run == "" || c.Subject == "" {
		return "", time.Time{}, errors.New("runnertoken: team, run and subject are required")
	}
	if c.Session < 1 {
		return "", time.Time{}, errors.New("runnertoken: session must be 1 or more")
	}
	kid, key := keys.Current()
	c.Version = SessionVersion
	c.KeyID = kid
	c.IssuedAt = now.Unix()
	c.ExpiresAt = now.Add(SessionTTL).Unix()
	raw, err := json.Marshal(c)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("runnertoken: encode claims: %w", err)
	}
	p := base64.RawURLEncoding.EncodeToString(raw)
	s := base64.RawURLEncoding.EncodeToString(sign(key, p))
	return SessionPrefix + p + "." + s, c.Expiry(), nil
}

// Verify checks a session token and returns its claims. On any error the
// claims are the zero value.
func Verify(keys KeySet, token string, now time.Time) (Claims, error) {
	if len(token) > maxTokenLen {
		return Claims{}, fmt.Errorf("%w: token too long", ErrMalformed)
	}
	rest, ok := strings.CutPrefix(token, SessionPrefix)
	if !ok {
		return Claims{}, fmt.Errorf("%w: wrong prefix", ErrMalformed)
	}
	p, s, ok := strings.Cut(rest, ".")
	if !ok || p == "" || s == "" || strings.Contains(s, ".") {
		return Claims{}, fmt.Errorf("%w: wrong shape", ErrMalformed)
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: payload encoding: %w", ErrMalformed, err)
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Claims{}, fmt.Errorf("%w: payload json: %w", ErrMalformed, err)
	}
	if c.Version != SessionVersion {
		return Claims{}, fmt.Errorf("%w: unsupported version %d", ErrMalformed, c.Version)
	}
	key, ok := keys.Lookup(c.KeyID)
	if !ok {
		return Claims{}, fmt.Errorf("%w: %q", ErrUnknownKey, c.KeyID)
	}
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: signature encoding: %w", ErrMalformed, err)
	}
	if !hmac.Equal(sig, sign(key, p)) {
		return Claims{}, ErrBadSignature
	}
	if c.Team == "" || c.Run == "" || c.Subject == "" || c.Session < 1 {
		return Claims{}, fmt.Errorf("%w: missing claims", ErrMalformed)
	}
	n := now.Unix()
	if n >= c.ExpiresAt {
		return Claims{}, ErrExpired
	}
	if c.IssuedAt > n+clockSkewSecs {
		return Claims{}, ErrNotYetValid
	}
	return c, nil
}
