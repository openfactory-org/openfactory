package secrets_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/secrets"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// TypeOf must find a team-scoped secret when a factory ID is given. The real
// stores list one exact scope, so a single List call misses team secrets.
func TestTypeOfScopes(t *testing.T) {
	ctx := context.Background()
	s := secrets.New(testdb.New(t), bytes.Repeat([]byte{7}, 32))
	if _, err := s.Put(ctx, "team1", "", "TOKEN", core.SecretAnthropicOAuth, []byte("tok")); err != nil {
		t.Fatal(err)
	}
	if got := secrets.TypeOf(ctx, s, "team1", "fac1", "TOKEN"); got != core.SecretAnthropicOAuth {
		t.Fatalf("team secret from factory scope: got %q", got)
	}
	if got := secrets.TypeOf(ctx, s, "team1", "", "TOKEN"); got != core.SecretAnthropicOAuth {
		t.Fatalf("team secret from team scope: got %q", got)
	}
	if _, err := s.Put(ctx, "team1", "fac1", "TOKEN", core.SecretAnthropicKey, []byte("key")); err != nil {
		t.Fatal(err)
	}
	if got := secrets.TypeOf(ctx, s, "team1", "fac1", "TOKEN"); got != core.SecretAnthropicKey {
		t.Fatalf("factory secret must win: got %q", got)
	}
	if got := secrets.TypeOf(ctx, s, "team1", "fac1", "MISSING"); got != "" {
		t.Fatalf("missing secret: got %q", got)
	}
}
