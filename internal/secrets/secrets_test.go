package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/keys"
)

type fakeDB struct {
	core.Store
	mu   sync.Mutex
	rows map[string]row // key team|factory|name
}

type row struct {
	sec core.Secret
	ct  []byte
}

func newFakeDB() *fakeDB { return &fakeDB{rows: map[string]row{}} }

func (f *fakeDB) PutSecret(_ context.Context, s *core.Secret, ct []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[s.TeamID+"|"+s.FactoryID+"|"+s.Name] = row{*s, append([]byte(nil), ct...)}
	return nil
}

func (f *fakeDB) GetSecret(_ context.Context, team, factory, name string) (*core.Secret, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[team+"|"+factory+"|"+name]; ok {
		s := r.sec
		return &s, r.ct, nil
	}
	if r, ok := f.rows[team+"||"+name]; ok {
		s := r.sec
		return &s, r.ct, nil
	}
	return nil, nil, core.ErrNotFound
}

func (f *fakeDB) ListSecrets(_ context.Context, team, factory string) ([]core.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Secret
	for _, r := range f.rows {
		if r.sec.TeamID == team && (r.sec.FactoryID == "" || r.sec.FactoryID == factory) {
			out = append(out, r.sec)
		}
	}
	return out, nil
}

func (f *fakeDB) DeleteSecret(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, r := range f.rows {
		if r.sec.ID == id {
			delete(f.rows, k)
			return nil
		}
	}
	return core.ErrNotFound
}

func testKey() []byte { return bytes.Repeat([]byte{7}, 32) }

func TestRoundTripScopesAndRotation(t *testing.T) {
	db := newFakeDB()
	s := New(db, testKey())
	ctx := context.Background()
	sec, err := s.Put(ctx, "team", "", "METABASE_API_KEY", core.SecretGeneric, []byte("team-value"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "team", "fac", "METABASE_API_KEY", "", []byte("factory-value")); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(ctx, "team", "fac", "METABASE_API_KEY"); string(v) != "factory-value" {
		t.Fatalf("factory scope = %q", v)
	}
	if v, _ := s.Get(ctx, "team", "other", "METABASE_API_KEY"); string(v) != "team-value" {
		t.Fatalf("fallback = %q", v)
	}
	// Ciphertext never contains the plaintext.
	for _, r := range db.rows {
		if bytes.Contains(r.ct, []byte("value")) {
			t.Fatal("plaintext stored")
		}
	}
	// Rotation keeps the id.
	rot, err := s.Put(ctx, "team", "", "METABASE_API_KEY", core.SecretGeneric, []byte("rotated"))
	if err != nil || rot.ID != sec.ID || !rot.CreatedAt.Equal(sec.CreatedAt) {
		t.Fatalf("rotation: %+v %v (orig %s)", rot, err, sec.ID)
	}
	if v, _ := s.Get(ctx, "team", "", "METABASE_API_KEY"); string(v) != "rotated" {
		t.Fatalf("rotated = %q", v)
	}
	if _, err := s.Get(ctx, "team", "", "MISSING"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Delete(ctx, sec.ID); err != nil {
		t.Fatal(err)
	}
	list, _ := s.List(ctx, "team", "fac")
	if len(list) != 1 || list[0].FactoryID != "fac" {
		t.Fatalf("list after delete = %+v", list)
	}
}

func TestTamperAndWrongKey(t *testing.T) {
	s := New(newFakeDB(), testKey())
	ct, err := s.Encrypt("t", "f", "NAME", core.SecretGeneric, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Decrypt("t", "f", "NAME", core.SecretGeneric, ct); err != nil || string(v) != "hello" {
		t.Fatalf("decrypt: %q %v", v, err)
	}
	// Moving the ciphertext to another identity fails.
	if _, err := s.Decrypt("t", "f", "OTHER", core.SecretGeneric, ct); err == nil {
		t.Fatal("identity not bound")
	}
	bad := append([]byte(nil), ct...)
	bad[len(bad)-1] ^= 1
	if _, err := s.Decrypt("t", "f", "NAME", core.SecretGeneric, bad); err == nil {
		t.Fatal("tampering not detected")
	}
	other := New(newFakeDB(), bytes.Repeat([]byte{8}, 32))
	if _, err := other.Decrypt("t", "f", "NAME", core.SecretGeneric, ct); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
	// Two encryptions differ (random DEK + nonce).
	ct2, _ := s.Encrypt("t", "f", "NAME", core.SecretGeneric, []byte("hello"))
	if bytes.Equal(ct, ct2) {
		t.Fatal("deterministic ciphertext")
	}
	// Short keys are stretched rather than rejected.
	short := New(newFakeDB(), []byte("dev"))
	c3, _ := short.Encrypt("t", "", "N", core.SecretGeneric, []byte("x"))
	if v, err := short.Decrypt("t", "", "N", core.SecretGeneric, c3); err != nil || string(v) != "x" {
		t.Fatal("stretched key round trip failed")
	}
}

func TestKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "secret.key")
	k1, err := LoadOrCreateKey(path)
	if err != nil || len(k1) != 32 {
		t.Fatalf("create: %v", err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	k2, err := LoadOrCreateKey(path)
	if err != nil || !bytes.Equal(k1, k2) {
		t.Fatal("reload mismatch")
	}
	t.Setenv(KeyEnv, base64.RawURLEncoding.EncodeToString(k1))
	if k3, err := KeyFromEnv(); err != nil || !bytes.Equal(k3, k1) {
		t.Fatalf("env key: %v", err)
	}
	if _, err := LoadKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestValidateName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"METABASE_API_KEY", true}, {"_X", true}, {"A1", true},
		{"", false}, {"lower", false}, {"1ABC", false}, {"WITH-DASH", false},
		{"OF_METADATA_X", false}, {"OF_TOKEN", false}, {strings.Repeat("A", 129), false},
	}
	for _, tt := range tests {
		if err := ValidateName(tt.name); (err == nil) != tt.ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestValidateValue(t *testing.T) {
	big := strings.Repeat("a", MaxEnvEntry-len("NAME")-2)
	tests := []struct {
		typ   core.SecretType
		value string
		ok    bool
	}{
		{core.SecretGeneric, "x", true},
		{core.SecretGeneric, "", false},
		{core.SecretGeneric, "a\x00b", false},
		{core.SecretGeneric, big, true},
		{core.SecretGeneric, big + "a", false},
		{core.SecretAnthropicKey, "sk-ant-123", true},
		{core.SecretAnthropicKey, "sk ant", false},
		{core.SecretDockerRegistry, `{"registry":"https://registry.example.com/","username":"u","password":"p"}`, true},
		{core.SecretDockerRegistry, `{"registry":"registry.example.com","username":"u"}`, false},
		{core.SecretDockerRegistry, `{"registry":"r.example.com","username":"u","password":"p","extra":1}`, false},
		{core.SecretAWSECR, `{"region":"us-east-1","role_arn":"arn:aws:iam::123456789012:role/pull"}`, true},
		{core.SecretAWSECR, `{"region":"us-east-1","access_key_id":"A","secret_access_key":"B","registry":"123456789012.dkr.ecr.us-east-1.amazonaws.com"}`, true},
		{core.SecretAWSECR, `{"region":"us-east-1","access_key_id":"A","secret_access_key":"B","registry":"123456789012.dkr.ecr.eu-west-1.amazonaws.com"}`, false},
		{core.SecretAWSECR, `{"region":"us-east-1","access_key_id":"A","secret_access_key":"B"}`, false},
		{core.SecretAWSECR, `{"region":"nowhere","role_arn":"arn:aws:iam::123456789012:role/pull"}`, false},
		{core.SecretAWSECR, `{"region":"us-east-1","role_arn":"arn:aws:iam::12:role/pull"}`, false},
		{core.SecretBedrockAccess, `{"access_key_id":"A","secret_access_key":"B","region":"us-west-2"}`, true},
		{"bogus", "x", false},
	}
	for _, tt := range tests {
		if err := ValidateValue("NAME", tt.typ, []byte(tt.value)); (err == nil) != tt.ok {
			t.Errorf("ValidateValue(%s, %.60q) = %v, want ok=%v", tt.typ, tt.value, err, tt.ok)
		}
	}
	c, _ := ParseECR([]byte(`{"region":"eu-west-1","role_arn":"arn:aws:iam::123456789012:role/pull"}`))
	if c.Registry != "123456789012.dkr.ecr.eu-west-1.amazonaws.com" {
		t.Fatalf("derived registry %q", c.Registry)
	}
}

func TestExpand(t *testing.T) {
	db := newFakeDB()
	s := New(db, testKey())
	ctx := context.Background()
	_, _ = s.Put(ctx, "t", "", "TOKEN", core.SecretGeneric, []byte("abc"))
	got, err := s.Expand(ctx, "t", "f", "Bearer {{TOKEN}} / {{ TOKEN }} {plain}")
	if err != nil || got != "Bearer abc / abc {plain}" {
		t.Fatalf("expand = %q %v", got, err)
	}
	_, err = s.Expand(ctx, "t", "f", "{{A}} {{TOKEN}} {{B}}")
	if !errors.Is(err, core.ErrNotFound) || !strings.Contains(err.Error(), "A, B") {
		t.Fatalf("missing err = %v", err)
	}
	if got, _ := s.Expand(ctx, "t", "f", "no placeholders"); got != "no placeholders" {
		t.Fatal(got)
	}
	m, err := ExpandMap(map[string]string{"Authorization": "Bearer {{TOKEN}}"}, func(n string) (string, error) { return "v-" + n, nil })
	if err != nil || m["Authorization"] != "Bearer v-TOKEN" {
		t.Fatalf("ExpandMap = %v %v", m, err)
	}
	vals, err := s.Values(ctx, "t", "f", []string{"TOKEN"})
	if err != nil || vals["TOKEN"] != "abc" {
		t.Fatalf("Values = %v %v", vals, err)
	}
}

func TestRedact(t *testing.T) {
	got := Redact("token=supersecret and super", []string{"supersecret", "super", "ab"})
	if got != "token=[REDACTED] and [REDACTED]" {
		t.Fatalf("redact = %q", got)
	}
	r := NewRedactor("hunter22")
	var buf bytes.Buffer
	_, _ = io.WriteString(r.Writer(&buf), "pw is hunter22\n")
	if buf.String() != "pw is [REDACTED]\n" {
		t.Fatalf("writer = %q", buf.String())
	}
}

func TestImageHost(t *testing.T) {
	tests := map[string]string{
		"ubuntu:22.04":                      "docker.io",
		"library/ubuntu":                    "docker.io",
		"ghcr.io/openfactory-org/sandbox:1": "ghcr.io",
		"localhost:5000/x":                  "localhost:5000",
		"localhost/x":                       "localhost",
		"index.docker.io/library/ubuntu":    "docker.io",
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/app@sha256:abc": "123456789012.dkr.ecr.us-east-1.amazonaws.com",
	}
	for img, want := range tests {
		if got := ImageHost(img); got != want {
			t.Errorf("ImageHost(%q) = %q, want %q", img, got, want)
		}
	}
}

func TestEnvelopeNamesKeyVersion(t *testing.T) {
	s := New(newFakeDB(), testKey())
	ct, err := s.Encrypt("t", "f", "N", core.SecretGeneric, []byte("v"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseEnvelope(ct)
	if err != nil || p.version != s.CurrentVersion() {
		t.Fatalf("parsed %+v err %v, current %q", p, err, s.CurrentVersion())
	}
}

func TestPreviousKeyReads(t *testing.T) {
	ctx := t.Context()
	oldKey, newKey := testKey(), bytes.Repeat([]byte{8}, 32)
	old := New(newFakeDB(), oldKey)
	oldCT, _ := old.Encrypt("t", "f", "A", core.SecretGeneric, []byte("alpha"))

	rotated := NewWithKeys(newFakeDB(), newKey, oldKey)
	if got, err := rotated.Decrypt("t", "f", "A", core.SecretGeneric, oldCT); err != nil || string(got) != "alpha" {
		t.Fatalf("previous key: %q %v", got, err)
	}

	// Without the old key the read fails with ErrWrongKey, and the error is
	// not core.ErrNotFound (callers read that as "secret missing").
	only := NewWithKeys(newFakeDB(), newKey)
	_, err := only.Decrypt("t", "f", "A", core.SecretGeneric, oldCT)
	if !errors.Is(err, ErrWrongKey) || errors.Is(err, core.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), keysVersion(oldKey)) {
		t.Fatalf("error does not name the key version: %v", err)
	}

	// Rewrap moves the value to the current key. The old key can then go.
	for n, ct := range map[string][]byte{"A": oldCT} {
		out, changed, err := rotated.Rewrap(ctx, "t", "f", n, core.SecretGeneric, ct)
		if err != nil || !changed {
			t.Fatalf("rewrap %s: changed=%v err=%v", n, changed, err)
		}
		if _, err := only.Decrypt("t", "f", n, core.SecretGeneric, out); err != nil {
			t.Fatalf("read %s after rewrap with only the new key: %v", n, err)
		}
		again, changed, err := rotated.Rewrap(ctx, "t", "f", n, core.SecretGeneric, out)
		if err != nil || changed || !bytes.Equal(again, out) {
			t.Fatalf("second rewrap of %s: changed=%v err=%v", n, changed, err)
		}
	}
	// A wrong identity makes Rewrap fail and not write a bad envelope.
	if _, _, err := rotated.Rewrap(ctx, "t", "f", "WRONG", core.SecretGeneric, oldCT); err == nil {
		t.Fatal("rewrap ignored the identity")
	}
}

func keysVersion(k []byte) string { return keys.Version(k) }

type fakeProvider struct{ wraps, unwraps int }

func (f *fakeProvider) CurrentVersion() string { return "kms-1" }
func (f *fakeProvider) Wrap(_ context.Context, dek []byte) ([]byte, string, error) {
	f.wraps++
	return append([]byte("W"), dek...), "kms-1", nil
}
func (f *fakeProvider) Unwrap(_ context.Context, w []byte, v string) ([]byte, error) {
	f.unwraps++
	if v != "kms-1" {
		return nil, fmt.Errorf("%w: version %s", core.ErrNotFound, v)
	}
	return w[1:], nil
}

func TestCustomKeyProvider(t *testing.T) {
	fp := &fakeProvider{}
	s := NewWithProvider(newFakeDB(), fp)
	ct, err := s.Encrypt("t", "", "N", core.SecretGeneric, []byte("x"))
	if err != nil || fp.wraps != 1 {
		t.Fatalf("encrypt: %v wraps=%d", err, fp.wraps)
	}
	if v, err := s.Decrypt("t", "", "N", core.SecretGeneric, ct); err != nil || string(v) != "x" || fp.unwraps != 1 {
		t.Fatalf("decrypt: %q %v", v, err)
	}
}
