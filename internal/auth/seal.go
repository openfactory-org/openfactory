package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SessionKeySize is the required length of the injected session key.
const SessionKeySize = 32

var errBadToken = errors.New("auth: invalid or expired token")

// sealer encrypts and authenticates small JSON payloads with AES-256-GCM.
// Each purpose (session, login flow) uses its own subkey derived from the
// master key, and the cookie name is bound as additional data so a value can
// not be replayed under another cookie.
type sealer struct {
	aead cipher.AEAD
	// old open values that earlier keys sealed (key rotation). They never seal.
	old []cipher.AEAD
}

func newAEAD(master []byte, purpose string) (cipher.AEAD, error) {
	if len(master) != SessionKeySize {
		return nil, fmt.Errorf("auth: session key must be %d bytes, got %d", SessionKeySize, len(master))
	}
	m := hmac.New(sha256.New, master)
	m.Write([]byte("openfactory/auth/" + purpose))
	block, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func newSealer(master []byte, purpose string, previous ...[]byte) (*sealer, error) {
	aead, err := newAEAD(master, purpose)
	if err != nil {
		return nil, err
	}
	s := &sealer{aead: aead}
	for _, p := range previous {
		old, err := newAEAD(p, purpose)
		if err != nil {
			return nil, fmt.Errorf("previous key: %w", err)
		}
		s.old = append(s.old, old)
	}
	return s, nil
}

// envelope wraps every sealed payload with an expiry.
type envelope struct {
	Exp  int64           `json:"exp"`
	Data json.RawMessage `json:"d"`
}

func (s *sealer) seal(v any, aad string, exp time.Time) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(envelope{Exp: exp.Unix(), Data: data})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plain)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, plain, []byte(aad))
	return base64.RawURLEncoding.EncodeToString(out), nil
}

func (s *sealer) open(tok, aad string, now time.Time, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) < s.aead.NonceSize()+s.aead.Overhead() {
		return errBadToken
	}
	ns := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], []byte(aad))
	for i := 0; err != nil && i < len(s.old); i++ {
		plain, err = s.old[i].Open(nil, raw[:ns], raw[ns:], []byte(aad))
	}
	if err != nil {
		return errBadToken
	}
	var env envelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return errBadToken
	}
	if now.Unix() >= env.Exp {
		return errBadToken
	}
	if err := json.Unmarshal(env.Data, v); err != nil {
		return errBadToken
	}
	return nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
