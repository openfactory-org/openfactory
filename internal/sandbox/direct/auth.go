package direct

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// AuthAlg is the signature algorithm of AuthResponse.
const AuthAlg = "ed25519"

// AuthMessage returns the bytes that a worker signs to answer a challenge.
func AuthMessage(workerID, connectionID, nonce string) []byte {
	return []byte("openfactory-worker-auth-v1\n" + workerID + "\n" + connectionID + "\n" + nonce + "\n")
}

// SignChallenge answers a challenge with key.
func SignChallenge(key ed25519.PrivateKey, workerID string, ch AuthChallenge) (AuthResponse, error) {
	nonce, err := base64.RawURLEncoding.DecodeString(ch.Nonce)
	if err != nil || len(nonce) != 32 {
		return AuthResponse{}, fmt.Errorf("%w: the challenge nonce is not 32 bytes of base64url", rtcore.ErrInvalid)
	}
	if ch.ConnectionID == "" {
		return AuthResponse{}, fmt.Errorf("%w: the challenge has no connection_id", rtcore.ErrInvalid)
	}
	sig := ed25519.Sign(key, AuthMessage(workerID, ch.ConnectionID, ch.Nonce))
	return AuthResponse{WorkerID: workerID, Alg: AuthAlg, Sig: base64.RawURLEncoding.EncodeToString(sig)}, nil
}

// VerifyChallenge checks the answer of a worker. The hub of the control
// plane does the same check with WebCrypto.
func VerifyChallenge(pub ed25519.PublicKey, workerID string, ch AuthChallenge, resp AuthResponse) error {
	if resp.Alg != AuthAlg {
		return fmt.Errorf("%w: alg %q", rtcore.ErrUnauthorized, resp.Alg)
	}
	if resp.WorkerID != workerID {
		return fmt.Errorf("%w: the answer is for worker %q", rtcore.ErrUnauthorized, resp.WorkerID)
	}
	sig, err := base64.RawURLEncoding.DecodeString(resp.Sig)
	if err != nil || !ed25519.Verify(pub, AuthMessage(workerID, ch.ConnectionID, ch.Nonce), sig) {
		return fmt.Errorf("%w: bad signature", rtcore.ErrUnauthorized)
	}
	return nil
}

// LoadWorkerKey reads the Ed25519 private key of a worker. The file holds a
// PEM "PRIVATE KEY" block (PKCS #8), or the 32-byte seed in base64 or
// base64url on one line.
func LoadWorkerKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: an operator-given key path
	if err != nil {
		return nil, fmt.Errorf("read the worker key: %w", err)
	}
	return ParseWorkerKey(data)
}

// ParseWorkerKey parses the content of a worker key file.
func ParseWorkerKey(data []byte) (ed25519.PrivateKey, error) {
	if block, _ := pem.Decode(data); block != nil {
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse the worker key: %w", err)
		}
		ek, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, errors.New("the worker key is not an Ed25519 key")
		}
		return ek, nil
	}
	s := strings.TrimSpace(string(data))
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if seed, err := enc.DecodeString(s); err == nil && len(seed) == ed25519.SeedSize {
			return ed25519.NewKeyFromSeed(seed), nil
		}
	}
	return nil, errors.New("the worker key file holds no PEM block and no 32-byte base64 seed")
}
