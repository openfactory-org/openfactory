package rewrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
	"github.com/openfactory-org/openfactory/internal/mcpregistry"
	"github.com/openfactory-org/openfactory/internal/secrets"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

type fixture struct {
	db      *sqlstore.Store
	mcpID   string
	factory string
}

// seed writes secrets in team and factory scope, an OAuth token of an MCP
// server and a personal token of a user, all under the old key.
func seed(t *testing.T, oldKey []byte) *fixture {
	t.Helper()
	ctx := t.Context()
	db := testdb.New(t)
	old := secrets.NewWithKeys(db, oldKey)
	f := &fixture{db: db}
	if err := db.CreateTeam(ctx, &core.Team{ID: "team_1", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateUser(ctx, &core.User{ID: "usr_1", TeamID: "team_1", Email: "a@example.test", Role: core.RoleAdmin}); err != nil {
		t.Fatal(err)
	}
	fac := &core.Factory{ID: "fac_1", TeamID: "team_1", Name: "f", Alias: "f"}
	if err := db.CreateFactory(ctx, fac); err != nil {
		t.Fatal(err)
	}
	f.factory = fac.ID
	srv := &core.RegisteredMCPServer{ID: "mcp_1", TeamID: "team_1", Name: "docs", URL: "https://mcp.example.test"}
	if err := db.CreateMCPServer(ctx, srv); err != nil {
		t.Fatal(err)
	}
	f.mcpID = srv.ID

	if _, err := old.Put(ctx, "team_1", "", "TEAM_SECRET", core.SecretGeneric, []byte("team-value")); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Put(ctx, "team_1", fac.ID, "FACTORY_SECRET", core.SecretGeneric, []byte("factory-value")); err != nil {
		t.Fatal(err)
	}
	kvKey, name, typ := mcpregistry.TokenStorage(srv.ID)
	env, err := old.Encrypt("team_1", "", name, typ, []byte(`{"access_token":"oauth-access"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.KVSet(ctx, kvKey, base64.StdEncoding.EncodeToString(env)); err != nil {
		t.Fatal(err)
	}
	ut := &forge.UserTokens{Store: db, Cipher: old}
	if err := ut.Set(ctx, "team_1", "usr_1", forge.GitHub, "ghp_user"); err != nil {
		t.Fatal(err)
	}
	return f
}

// readAll reads every item with s and fails when one cannot be read.
func readAll(t *testing.T, f *fixture, s *secrets.Store) {
	t.Helper()
	ctx := t.Context()
	for scope, want := range map[string]string{"": "team-value", f.factory: "factory-value"} {
		name := "TEAM_SECRET"
		if scope != "" {
			name = "FACTORY_SECRET"
		}
		v, err := s.Get(ctx, "team_1", scope, name)
		if err != nil || string(v) != want {
			t.Fatalf("secret %s: %q %v", name, v, err)
		}
	}
	kvKey, name, typ := mcpregistry.TokenStorage(f.mcpID)
	raw, err := f.db.KVGet(ctx, kvKey)
	if err != nil {
		t.Fatal(err)
	}
	env, _ := base64.StdEncoding.DecodeString(raw)
	plain, err := s.Decrypt("team_1", "", name, typ, env)
	if err != nil || !strings.Contains(string(plain), "oauth-access") {
		t.Fatalf("oauth token: %q %v", plain, err)
	}
	ut := &forge.UserTokens{Store: f.db, Cipher: s}
	if tok, err := ut.Get(ctx, "team_1", "usr_1", forge.GitHub); err != nil || tok != "ghp_user" {
		t.Fatalf("user token: %q %v", tok, err)
	}
}

func TestRotateRewrapRemoveOldKey(t *testing.T) {
	ctx := t.Context()
	oldKey, newKey := key(1), key(2)
	f := seed(t, oldKey)

	// Step 1: new key current, old key previous. Everything still reads.
	rotated := secrets.NewWithKeys(f.db, newKey, oldKey)
	readAll(t, f, rotated)

	// Without the old key nothing reads yet, so the rewrap is needed.
	if _, err := secrets.NewWithKeys(f.db, newKey).Get(ctx, "team_1", "", "TEAM_SECRET"); err == nil {
		t.Fatal("a secret read with only the new key before the rewrap")
	}

	// A dry run counts and writes nothing.
	rep, err := Run(ctx, f.db, rotated, true)
	if err != nil || rep.Rewrapped != 4 || rep.Current != 0 || rep.Failed != 0 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if _, err := secrets.NewWithKeys(f.db, newKey).Get(ctx, "team_1", "", "TEAM_SECRET"); err == nil {
		t.Fatal("the dry run wrote")
	}

	// Step 2: rewrap.
	rep, err = Run(ctx, f.db, rotated, false)
	if err != nil || rep.Rewrapped != 4 || rep.Current != 0 || rep.Failed != 0 {
		t.Fatalf("rewrap: %+v %v", rep, err)
	}
	// A second run finds all current.
	rep, err = Run(ctx, f.db, rotated, false)
	if err != nil || rep.Rewrapped != 0 || rep.Current != 4 || rep.Failed != 0 {
		t.Fatalf("second run: %+v %v", rep, err)
	}

	// Step 3: remove the old key. Every secret and token still reads.
	readAll(t, f, secrets.NewWithKeys(f.db, newKey))
}

func TestRunCountsFailures(t *testing.T) {
	ctx := t.Context()
	f := seed(t, key(1))
	// A key that is neither current nor previous: every item fails.
	stranger := secrets.NewWithKeys(f.db, key(3), key(4))
	rep, err := Run(ctx, f.db, stranger, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed != 4 || rep.Rewrapped != 0 || len(rep.Errors) != 4 {
		t.Fatalf("report %+v", rep)
	}
	for _, e := range rep.Errors {
		if strings.Contains(e, "team-value") || strings.Contains(e, "oauth-access") {
			t.Fatalf("error holds a value: %s", e)
		}
	}
}

// writeDuringRewrap acts as a live replica: right after the rewrap reads an
// item, the user deletes the forge token or puts a new team secret.
type writeDuringRewrap struct {
	*sqlstore.Store
	tokenKey     string
	newKey       []byte
	deleted, put bool
}

func (w *writeDuringRewrap) KVGet(ctx context.Context, key string) (string, error) {
	v, err := w.Store.KVGet(ctx, key)
	if key == w.tokenKey && !w.deleted {
		w.deleted = true
		// DELETE /me/forge-tokens/GITHUB lands now.
		if err := (&forge.UserTokens{Store: w.Store}).Delete(ctx, "team_1", "usr_1", forge.GitHub); err != nil {
			return "", err
		}
	}
	return v, err
}

func (w *writeDuringRewrap) GetSecret(ctx context.Context, teamID, factoryID, name string) (*core.Secret, []byte, error) {
	sec, ct, err := w.Store.GetSecret(ctx, teamID, factoryID, name)
	if name == "TEAM_SECRET" && !w.put {
		w.put = true
		// A PUT of the secret with the new key lands now.
		if _, err := secrets.NewWithKeys(w.Store, w.newKey).Put(ctx, "team_1", "", "TEAM_SECRET", core.SecretGeneric, []byte("replica-value")); err != nil {
			return nil, nil, err
		}
	}
	return sec, ct, err
}

// TestRunKeepsWritesOfReplicas: a value that changes between the read and
// the write of the rewrap is skipped and counted. The rewrap does not bring
// back a deleted token or overwrite a new secret value.
func TestRunKeepsWritesOfReplicas(t *testing.T) {
	ctx := t.Context()
	oldKey, newKey := key(1), key(2)
	f := seed(t, oldKey)
	tokenKey, _, _ := forge.UserTokenStorage("team_1", "usr_1", forge.GitHub)
	w := &writeDuringRewrap{Store: f.db, tokenKey: tokenKey, newKey: newKey}
	rep, err := Run(ctx, w, secrets.NewWithKeys(f.db, newKey, oldKey), false)
	if err != nil || rep.Changed != 2 || rep.Rewrapped != 2 || rep.Failed != 0 {
		t.Fatalf("rewrap: %+v %v", rep, err)
	}
	cur := secrets.NewWithKeys(f.db, newKey)
	ut := &forge.UserTokens{Store: f.db, Cipher: cur}
	if _, err := ut.Get(ctx, "team_1", "usr_1", forge.GitHub); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("token deleted by the user: %v, want not found", err)
	}
	if v, err := cur.Get(ctx, "team_1", "", "TEAM_SECRET"); err != nil || string(v) != "replica-value" {
		t.Fatalf("secret put by a replica: %q %v", v, err)
	}
	if v, err := cur.Get(ctx, "team_1", f.factory, "FACTORY_SECRET"); err != nil || string(v) != "factory-value" {
		t.Fatalf("factory secret: %q %v", v, err)
	}
}
