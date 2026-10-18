package runnertoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// BootstrapPrefix starts every bootstrap token.
	BootstrapPrefix = "ofb_"

	bootstrapBytes  = 32
	instanceIDBytes = 16
)

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("runnertoken: random source: %w", err)
	}
	return b, nil
}

// NewBootstrap makes a bootstrap token and its hash. The server stores only
// the hash.
func NewBootstrap() (token string, hash string, err error) {
	b, err := randomBytes(bootstrapBytes)
	if err != nil {
		return "", "", err
	}
	token = BootstrapPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashString(token), nil
}

// HashBootstrap returns the hash of a bootstrap token, or ErrMalformed if
// the token does not have the right form.
func HashBootstrap(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, BootstrapPrefix)
	if !ok {
		return "", fmt.Errorf("%w: wrong prefix", ErrMalformed)
	}
	b, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", fmt.Errorf("%w: encoding: %w", ErrMalformed, err)
	}
	if len(b) != bootstrapBytes {
		return "", fmt.Errorf("%w: wrong length", ErrMalformed)
	}
	return hashString(token), nil
}

// NewInstanceID makes a random runner instance ID of 128 bits.
func NewInstanceID() (string, error) {
	b, err := randomBytes(instanceIDBytes)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashInstanceID returns the lower-case hex SHA-256 hash of an instance ID.
func HashInstanceID(id string) string {
	return hashString(id)
}

// EqualHash compares two hex hash strings in constant time.
func EqualHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
