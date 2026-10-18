// Package secrets implements core.SecretStore with AES-256-GCM envelope
// encryption on top of a core.Store that only ever sees ciphertext.
//
// Each secret value is encrypted with a fresh random 256-bit data key. A
// core.KeyProvider wraps the data key and names the key version, and the
// envelope stores that version. The value is sealed with the secret's identity
// (team, factory scope, name and type) as additional data, so a ciphertext
// cannot be moved to another secret record without detection. The Rewrap
// method wraps a data key again with the current key, so that an operator can
// rotate the key.
//
// Values are never logged. Use Redact to scrub known values from text.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/keys"
)

// KeyEnv is the environment variable holding the base64 master key.
const KeyEnv = "OF_SECRET_KEY"

const (
	// envelopeVersion is the first byte of an envelope. The envelope names
	// the key version.
	envelopeVersion = 2
	keySize         = 32
	nonceSize       = 12
)

// Store is the encrypted secret store.
type Store struct {
	db       core.Store
	provider core.KeyProvider
	now      func() time.Time
}

var _ core.SecretStore = (*Store)(nil)

// New returns a secret store. masterKey should be 32 random bytes (see
// LoadKey / LoadOrCreateKey); keys of any other non-zero length are stretched
// with SHA-256. New panics on an empty key, which is always a configuration bug.
func New(store core.Store, masterKey []byte) *Store {
	return NewWithKeys(store, masterKey)
}

// NewWithKeys is New with previous keys. The store writes with masterKey and
// reads with masterKey and each previous key.
func NewWithKeys(store core.Store, masterKey []byte, previous ...[]byte) *Store {
	if len(masterKey) == 0 {
		panic("secrets: empty master key")
	}
	prev := make([][]byte, 0, len(previous))
	for _, p := range previous {
		if len(p) == 0 {
			panic("secrets: empty previous key")
		}
		prev = append(prev, stretch(p))
	}
	l, err := keys.NewLocal(stretch(masterKey), prev...)
	if err != nil {
		panic("secrets: " + err.Error())
	}
	return NewWithProvider(store, l)
}

// NewWithProvider returns a secret store that wraps data keys with p.
func NewWithProvider(store core.Store, p core.KeyProvider) *Store {
	if p == nil {
		panic("secrets: nil key provider")
	}
	return &Store{db: store, provider: p, now: time.Now}
}

func stretch(key []byte) []byte {
	if len(key) == keySize {
		return key
	}
	sum := sha256.Sum256(append([]byte("openfactory-secret-key:"), key...))
	return sum[:]
}

// LoadKey decodes a base64 (std or URL, padded or not) 32-byte key.
func LoadKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, errors.New("secrets: empty key")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(encoded); err == nil {
			if len(k) != keySize {
				return nil, fmt.Errorf("secrets: key must be %d bytes, got %d", keySize, len(k))
			}
			return k, nil
		}
	}
	return nil, errors.New("secrets: key is not valid base64")
}

// KeyFromEnv loads the master key from OF_SECRET_KEY.
func KeyFromEnv() ([]byte, error) {
	v, ok := os.LookupEnv(KeyEnv)
	if !ok || strings.TrimSpace(v) == "" {
		return nil, fmt.Errorf("secrets: %s is not set", KeyEnv)
	}
	return LoadKey(v)
}

// GenerateKey returns a new random base64-encoded 32-byte key.
func GenerateKey() (string, error) {
	k := make([]byte, keySize)
	if _, err := rand.Read(k); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k), nil
}

// LoadOrCreateKey reads a base64 key from path, creating it (mode 0600) with a
// random key when missing. Intended for development and single-node installs.
func LoadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return LoadKey(string(data))
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	enc, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return LoadOrCreateKey(path)
		}
		return nil, err
	}
	if _, err := f.WriteString(enc + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return LoadKey(enc)
}

func aad(teamID, factoryID, name string, typ core.SecretType) []byte {
	return []byte("openfactory-secret-v1\x00" + teamID + "\x00" + factoryID + "\x00" + name + "\x00" + string(typ))
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

// Encrypt produces an envelope: version(1) | len(key version)(1) | key
// version | len(wrapped dek)(2, big endian) | wrapped dek | sealed value.
func (s *Store) Encrypt(teamID, factoryID, name string, typ core.SecretType, value []byte) ([]byte, error) {
	ad := aad(teamID, factoryID, name, typ)
	dek := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, err
	}
	wrapped, version, err := s.provider.Wrap(context.Background(), dek)
	if err != nil {
		return nil, fmt.Errorf("secrets: wrap data key: %w", err)
	}
	body, err := seal(dek, value, ad)
	if err != nil {
		return nil, err
	}
	return buildEnvelope(version, wrapped, body)
}

func buildEnvelope(version string, wrapped, body []byte) ([]byte, error) {
	if len(version) == 0 || len(version) > 255 || len(wrapped) > 0xFFFF {
		return nil, errors.New("secrets: key provider returned a version or a wrapped key that is too long")
	}
	out := make([]byte, 0, 4+len(version)+len(wrapped)+len(body))
	out = append(out, envelopeVersion, byte(len(version))) //nolint:gosec // len(version) <= 255 is checked above
	out = append(out, version...)
	out = binary.BigEndian.AppendUint16(out, uint16(len(wrapped))) //nolint:gosec // len(wrapped) <= 0xFFFF is checked above
	out = append(out, wrapped...)
	out = append(out, body...)
	return out, nil
}

// ErrWrongKey is returned when a ciphertext was produced under another master key.
var ErrWrongKey = errors.New("secrets: ciphertext was encrypted with a different master key")

// parsed is an envelope in parts.
type parsed struct {
	version string // key version
	wrapped []byte
	body    []byte
}

func parseEnvelope(envelope []byte) (parsed, error) {
	var p parsed
	if len(envelope) < 2 {
		return p, errors.New("secrets: malformed ciphertext")
	}
	switch envelope[0] {
	case envelopeVersion:
		n := int(envelope[1])
		rest := envelope[2:]
		if n == 0 || len(rest) < n+2 {
			return p, errors.New("secrets: malformed ciphertext")
		}
		p.version = string(rest[:n])
		rest = rest[n:]
		w := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if len(rest) < w {
			return p, errors.New("secrets: malformed ciphertext")
		}
		p.wrapped, p.body = rest[:w], rest[w:]
	default:
		return p, fmt.Errorf("secrets: unsupported envelope version %d", envelope[0])
	}
	return p, nil
}

// unwrap returns the data key of a parsed envelope.
func (s *Store) unwrap(ctx context.Context, p parsed, ad []byte) ([]byte, error) {
	dek, err := s.provider.Unwrap(ctx, p.wrapped, p.version)
	if errors.Is(err, core.ErrNotFound) {
		// Do not wrap core.ErrNotFound: callers would read it as a missing secret.
		return nil, fmt.Errorf("%w (key version %q is not configured)", ErrWrongKey, p.version)
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: cannot unwrap data key: %w", err)
	}
	return dek, nil
}

// Decrypt opens an envelope produced by Encrypt.
func (s *Store) Decrypt(teamID, factoryID, name string, typ core.SecretType, envelope []byte) ([]byte, error) {
	p, err := parseEnvelope(envelope)
	if err != nil {
		return nil, err
	}
	ad := aad(teamID, factoryID, name, typ)
	dek, err := s.unwrap(context.Background(), p, ad)
	if err != nil {
		return nil, err
	}
	plain, err := open(dek, p.body, ad)
	if err != nil {
		return nil, errors.New("secrets: cannot decrypt value (tampered ciphertext)")
	}
	return plain, nil
}

// CurrentVersion returns the key version of new writes.
func (s *Store) CurrentVersion() string { return s.provider.CurrentVersion() }

// Rewrap wraps the data key of an envelope again with the current key. It
// returns the new envelope, and changed is false when the envelope already
// uses the current key version. The sealed value does not change. Rewrap
// opens the value first, so it never gives back an envelope that does not decrypt.
func (s *Store) Rewrap(ctx context.Context, teamID, factoryID, name string, typ core.SecretType, envelope []byte) (out []byte, changed bool, err error) {
	p, err := parseEnvelope(envelope)
	if err != nil {
		return nil, false, err
	}
	if p.version == s.provider.CurrentVersion() {
		return envelope, false, nil
	}
	ad := aad(teamID, factoryID, name, typ)
	dek, err := s.unwrap(ctx, p, ad)
	if err != nil {
		return nil, false, err
	}
	if _, err := open(dek, p.body, ad); err != nil {
		return nil, false, errors.New("secrets: cannot decrypt value (tampered ciphertext)")
	}
	wrapped, version, err := s.provider.Wrap(ctx, dek)
	if err != nil {
		return nil, false, fmt.Errorf("secrets: wrap data key: %w", err)
	}
	out, err = buildEnvelope(version, wrapped, p.body)
	return out, err == nil, err
}

// Put implements core.SecretStore. It validates the name and the typed value,
// then stores the encrypted value. Writing an existing name in the same scope
// rotates the value and keeps the secret's id.
func (s *Store) Put(ctx context.Context, teamID, factoryID, name string, typ core.SecretType, value []byte) (*core.Secret, error) {
	if teamID == "" {
		return nil, fmt.Errorf("%w: team id required", core.ErrInvalid)
	}
	if typ == "" {
		typ = core.SecretGeneric
	}
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if err := ValidateValue(name, typ, value); err != nil {
		return nil, err
	}
	now := s.now().UTC()
	sec := &core.Secret{ID: core.NewID("sec"), TeamID: teamID, FactoryID: factoryID, Name: name, Type: typ, CreatedAt: now, UpdatedAt: now}
	if existing, _, err := s.db.GetSecret(ctx, teamID, factoryID, name); err == nil && existing.FactoryID == factoryID && existing.TeamID == teamID {
		sec.ID = existing.ID
		sec.CreatedAt = existing.CreatedAt
	} else if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	ct, err := s.Encrypt(teamID, factoryID, name, typ, value)
	if err != nil {
		return nil, err
	}
	if err := s.db.PutSecret(ctx, sec, ct); err != nil {
		return nil, err
	}
	return sec, nil
}

// GetSecret returns metadata and the decrypted value (factory scope falls back to team scope).
func (s *Store) GetSecret(ctx context.Context, teamID, factoryID, name string) (*core.Secret, []byte, error) {
	sec, ct, err := s.db.GetSecret(ctx, teamID, factoryID, name)
	if err != nil {
		return nil, nil, err
	}
	val, err := s.Decrypt(sec.TeamID, sec.FactoryID, sec.Name, sec.Type, ct)
	if err != nil {
		return nil, nil, fmt.Errorf("secret %s: %w", name, err)
	}
	return sec, val, nil
}

// Get implements core.SecretStore.
func (s *Store) Get(ctx context.Context, teamID, factoryID, name string) ([]byte, error) {
	_, v, err := s.GetSecret(ctx, teamID, factoryID, name)
	return v, err
}

// List implements core.SecretStore (metadata only).
func (s *Store) List(ctx context.Context, teamID, factoryID string) ([]core.Secret, error) {
	return s.db.ListSecrets(ctx, teamID, factoryID)
}

// Delete implements core.SecretStore.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.db.DeleteSecret(ctx, id)
}

// Expand implements core.SecretStore: every {{NAME}} placeholder is replaced by
// the secret's value. Missing secrets produce one error listing every missing name.
func (s *Store) Expand(ctx context.Context, teamID, factoryID, text string) (string, error) {
	return ExpandWith(text, func(name string) (string, error) {
		v, err := s.Get(ctx, teamID, factoryID, name)
		if err != nil {
			return "", err
		}
		return string(v), nil
	})
}

// Values resolves several secrets into a name→value map (e.g. for env injection).
func (s *Store) Values(ctx context.Context, teamID, factoryID string, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	var missing []string
	for _, n := range names {
		v, err := s.Get(ctx, teamID, factoryID, n)
		if errors.Is(err, core.ErrNotFound) {
			missing = append(missing, n)
			continue
		}
		if err != nil {
			return nil, err
		}
		out[n] = string(v)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: secrets not found: %s", core.ErrNotFound, strings.Join(missing, ", "))
	}
	return out, nil
}
