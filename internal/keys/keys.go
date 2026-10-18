// Package keys holds the local key provider. It wraps the data keys of the
// secret store with keys that the operator gives in the settings
// (OF_SECRET_KEY and OF_SECRET_KEY_PREVIOUS). Each key has a version. The
// version is not secret. It is the first 8 hex characters of the SHA-256 of
// the key.
package keys

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/openfactory-org/openfactory/internal/core"
)

// KeySize is the required length of a key.
const KeySize = 32

const nonceSize = 12

// wrapAAD is the additional data of a wrapped data key.
var wrapAAD = []byte("openfactory-dek-wrap-v2")

// Version returns the version name of a key.
func Version(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

// Local is a core.KeyProvider with keys in memory. The first key is the
// current key. The other keys only read.
type Local struct {
	current string
	order   []string
	keys    map[string][]byte
}

var _ core.KeyProvider = (*Local)(nil)

// NewLocal returns a provider. Each key must have 32 bytes. A previous key
// that has the same version as an earlier key is skipped.
func NewLocal(current []byte, previous ...[]byte) (*Local, error) {
	l := &Local{keys: map[string][]byte{}}
	for i, k := range append([][]byte{current}, previous...) {
		if len(k) != KeySize {
			return nil, fmt.Errorf("keys: key %d must have %d bytes, got %d", i, KeySize, len(k))
		}
		v := Version(k)
		if _, dup := l.keys[v]; dup {
			continue
		}
		l.keys[v] = append([]byte(nil), k...)
		l.order = append(l.order, v)
	}
	l.current = l.order[0]
	return l, nil
}

// CurrentVersion implements core.KeyProvider.
func (l *Local) CurrentVersion() string { return l.current }

// Versions returns the versions of all keys, the current version first.
func (l *Local) Versions() []string { return append([]string(nil), l.order...) }

// Wrap implements core.KeyProvider.
func (l *Local) Wrap(_ context.Context, dataKey []byte) ([]byte, string, error) {
	out, err := seal(l.keys[l.current], dataKey, wrapAAD)
	if err != nil {
		return nil, "", err
	}
	return out, l.current, nil
}

// Unwrap implements core.KeyProvider. An empty version tries each key in order.
func (l *Local) Unwrap(_ context.Context, wrapped []byte, version string) ([]byte, error) {
	if version == "" {
		for _, v := range l.order {
			if dek, err := open(l.keys[v], wrapped, wrapAAD); err == nil {
				return dek, nil
			}
		}
		return nil, errors.New("keys: no key can unwrap the data key")
	}
	key, ok := l.keys[version]
	if !ok {
		return nil, fmt.Errorf("%w: key version %q", core.ErrNotFound, version)
	}
	dek, err := open(key, wrapped, wrapAAD)
	if err != nil {
		return nil, errors.New("keys: cannot unwrap the data key")
	}
	return dek, nil
}

func seal(key, plaintext, additional []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, additional), nil
}

func open(key, sealed, additional []byte) ([]byte, error) {
	if len(sealed) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, sealed[:nonceSize], sealed[nonceSize:], additional)
}
