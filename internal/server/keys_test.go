package server

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/keys"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

func k32(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func mintRunnerToken(t *testing.T, ks runnertoken.KeySet) string {
	t.Helper()
	tok, _, err := runnertoken.Mint(ks, runnertoken.Claims{Team: "team_1", Run: "run_1", Session: 1, Subject: "runner"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRunnerKeySetRotation(t *testing.T) {
	oldSet, err := runnerKeySet(k32(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := mintRunnerToken(t, oldSet)

	rotated, err := runnerKeySet(k32(2), [][]byte{k32(1)})
	if err != nil {
		t.Fatal(err)
	}
	// The key ID is the version of the derived key.
	kid, key := rotated.Current()
	if kid != keys.Version(key) || kid != keys.Version(config.DeriveKey(k32(2), config.LabelRunnerToken)) {
		t.Fatalf("current key ID %q is not the key version", kid)
	}
	if _, err := runnertoken.Verify(rotated, tok, time.Now()); err != nil {
		t.Fatalf("token of the previous key: %v", err)
	}
	if got, err := runnertoken.Verify(rotated, mintRunnerToken(t, rotated), time.Now()); err != nil || got.KeyID != kid {
		t.Fatalf("token of the current key: %+v %v", got, err)
	}

	removed, _ := runnerKeySet(k32(2), nil)
	if _, err := runnertoken.Verify(removed, tok, time.Now()); !errors.Is(err, runnertoken.ErrUnknownKey) {
		t.Fatalf("after removal: %v, want an unknown key error", err)
	}
	// A token with a key ID that no key has fails.
	if _, err := runnertoken.Verify(removed, mintRunnerToken(t, mustStatic(t, "zzzzzzzz", k32(9))), time.Now()); !errors.Is(err, runnertoken.ErrUnknownKey) {
		t.Fatalf("unknown key ID: %v", err)
	}
}

func mustStatic(t *testing.T, kid string, key []byte) *runnertoken.StaticKeys {
	t.Helper()
	s, err := runnertoken.NewStaticKeys(kid, map[string][]byte{kid: key})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// A change of the encryption key alone must not touch sessions or runner tokens.
func TestEncryptionKeyChangeKeepsSigning(t *testing.T) {
	const signing = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	mk := func(secret string) *config.Config {
		c := testConfig(t)
		c.Dev = false
		c.SecretKey = secret
		c.SigningKey = signing
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	before := mk("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	after := mk("QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8=")

	sb, spb, err := before.SigningKeys()
	if err != nil {
		t.Fatal(err)
	}
	sa, spa, err := after.SigningKeys()
	if err != nil {
		t.Fatal(err)
	}
	// Session cookies derive from the signing key.
	skb, _ := config.DeriveKeys(sb, spb, config.LabelWebSession)
	ska, _ := config.DeriveKeys(sa, spa, config.LabelWebSession)
	if !bytes.Equal(skb, ska) {
		t.Fatal("the session key changed with the encryption key")
	}
	// A runner token from before the change verifies after it.
	setBefore, _ := runnerKeySet(sb, spb)
	setAfter, _ := runnerKeySet(sa, spa)
	if _, err := runnertoken.Verify(setAfter, mintRunnerToken(t, setBefore), time.Now()); err != nil {
		t.Fatalf("runner token after the encryption key changed: %v", err)
	}
	// The encryption keys did change.
	eb, _, _ := before.EncryptionKeys()
	ea, _, _ := after.EncryptionKeys()
	if bytes.Equal(eb, ea) {
		t.Fatal("test setup: the encryption keys are equal")
	}
}

type countingProvider struct{ wraps int }

func (c *countingProvider) CurrentVersion() string { return "ext-1" }
func (c *countingProvider) Wrap(_ context.Context, dek []byte) ([]byte, string, error) {
	c.wraps++
	return append([]byte("X"), dek...), "ext-1", nil
}
func (c *countingProvider) Unwrap(_ context.Context, w []byte, v string) ([]byte, error) {
	if v != "ext-1" {
		return nil, core.ErrNotFound
	}
	return w[1:], nil
}

func TestWithKeyProviderWrapsSecrets(t *testing.T) {
	ctx := t.Context()
	p := &countingProvider{}
	srv, err := New(ctx, testConfig(t), WithLogger(quietLogger()), WithDefinitionSyncInterval(-1), WithKeyProvider(p))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.Store.CreateTeam(ctx, &core.Team{ID: "team_k", Name: "k"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Secrets.Put(ctx, "team_k", "", "TOKEN", core.SecretGeneric, []byte("value")); err != nil {
		t.Fatal(err)
	}
	if p.wraps != 1 {
		t.Fatalf("provider wrapped %d data keys, want 1", p.wraps)
	}
	if v, err := srv.Secrets.Get(ctx, "team_k", "", "TOKEN"); err != nil || string(v) != "value" {
		t.Fatalf("get: %q %v", v, err)
	}
}
