package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// fakeCipher binds ciphertexts to their identity like secrets.Store does.
type fakeCipher struct{}

func (fakeCipher) Encrypt(teamID, factoryID, name string, typ core.SecretType, v []byte) ([]byte, error) {
	return []byte(teamID + "|" + name + "|" + string(v)), nil
}
func (fakeCipher) Decrypt(teamID, factoryID, name string, typ core.SecretType, env []byte) ([]byte, error) {
	prefix := teamID + "|" + name + "|"
	if !strings.HasPrefix(string(env), prefix) {
		return nil, errors.New("identity mismatch")
	}
	return env[len(prefix):], nil
}

// TestForgeForUser covers credentialStrategy CREATOR: a run acts with the
// creator's personal token when the integration supports per-user tokens,
// fails without that token, and uses the team integration when the
// integration does not support per-user tokens.
func TestForgeForUser(t *testing.T) {
	ctx := context.Background()
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{
		{ID: "i1", TeamID: "team_1", Provider: "github", Config: map[string]string{"base_url": "https://ghe.local/api/v3", "org": "acme"}, SecretRef: "GH_TOKEN"},
		{ID: "i2", TeamID: "team_1", Provider: "gitlab", Config: map[string]string{"user_tokens": "false", "org": "acme"}, SecretRef: "GL_TOKEN"},
		{ID: "i3", TeamID: "team_1", Provider: "git", Config: map[string]string{"base_url": "https://git.example.com"}, SecretRef: "GIT_TOKEN"},
		{ID: "i4", TeamID: "team_1", Provider: "azure_devops", Config: map[string]string{"org": "contoso"}, SecretRef: "ADO_TOKEN"},
	}
	ut := &UserTokens{Store: store, Cipher: fakeCipher{}}
	plugins := &fakePlugins{names: map[string]string{"gitlab": GitLab, "azure_devops": AzureDevOps}}
	r := NewResolver(store, forgetest.NewSecrets(map[string]string{"GH_TOKEN": "team-gh", "GL_TOKEN": "team-gl", "GIT_TOKEN": "team-git", "ADO_TOKEN": "team-ado"}),
		Options{UserTokens: ut, Plugins: plugins})

	if err := ut.Set(ctx, "team_1", "usr_alice", "GITHUB", " alice-pat \n"); err != nil {
		t.Fatal(err)
	}
	_ = ut.Set(ctx, "team_1", "usr_alice", "GITLAB", "alice-gl")
	_ = ut.Set(ctx, "team_1", "usr_alice", "AZURE_DEVOPS", "alice-ado")
	if err := ut.Set(ctx, "team_1", "usr_alice", "GIT", "x"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("generic git must not accept user tokens: %v", err)
	}
	if got, bad, err := ut.List(ctx, "team_1", "usr_alice"); err != nil || strings.Join(got, ",") != "GITHUB,GITLAB,AZURE_DEVOPS" || len(bad) != 0 {
		t.Fatalf("list = %v %v %v", got, bad, err)
	}
	if _, err := ut.Get(ctx, "team_2", "usr_alice", "GITHUB"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other team: %v", err)
	}

	tests := []struct {
		name, forge, user string
		wantUser          bool
		wantTok           string
	}{
		{"creator token", "GITHUB", "usr_alice", true, "x-access-token:alice-pat@ghe.local"},
		{"executor (no user)", "GITHUB", "", false, "x-access-token:team-gh@ghe.local"},
		{"integration opted out", "GITLAB", "usr_alice", false, "team-gl"},
		{"unsupported forge", "GIT", "usr_alice", false, "team-git"},
		{"plugin forge creator token", "AZURE_DEVOPS", "usr_alice", true, "alice-ado"},
	}
	for _, tc := range tests {
		f, used, err := r.ForgeForUser(ctx, core.ForgeLookup{TeamID: "team_1", CodeForge: tc.forge, Repo: "acme/api"}, tc.user)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		u, err := f.CloneURL(ctx, "acme", "api")
		if err != nil {
			t.Fatalf("%s: clone url: %v", tc.name, err)
		}
		if used != tc.wantUser || !strings.Contains(u, tc.wantTok) {
			t.Errorf("%s: used=%v url=%s", tc.name, used, u)
		}
	}
	// A user without a token gets external_authentication_required, not
	// the team credential.
	if _, used, err := r.ForgeForUser(ctx, core.ForgeLookup{TeamID: "team_1", CodeForge: "GITHUB", Repo: "acme/api"}, "usr_bob"); used || !core.IsExternalAuthRequired(err) {
		t.Fatalf("no token: used=%v err=%v", used, err)
	}
	if got := plugins.targets[len(plugins.targets)-1]; !got.UserToken || got.Token != "alice-ado" || got.Config["org"] != "contoso" {
		t.Fatalf("plugin user-token target = %+v", got)
	}
	if err := ut.Delete(ctx, "team_1", "usr_alice", "GITHUB"); err != nil {
		t.Fatal(err)
	}
	if _, used, _ := r.ForgeForUser(ctx, core.ForgeLookup{TeamID: "team_1", CodeForge: "GITHUB", Repo: "acme/api"}, "usr_alice"); used {
		t.Fatal("deleted token still used")
	}
}

// TestUserTokenListUnreadable: a stored token that cannot be decrypted (the
// key changed) is listed and marked unreadable, not hidden as "not linked".
func TestUserTokenListUnreadable(t *testing.T) {
	ctx := context.Background()
	store := forgetest.NewStore()
	ut := &UserTokens{Store: store, Cipher: fakeCipher{}}
	if err := ut.Set(ctx, "team_1", "usr_alice", GitHub, "alice-pat"); err != nil {
		t.Fatal(err)
	}
	if err := ut.Set(ctx, "team_1", "usr_alice", GitLab, "alice-gl"); err != nil {
		t.Fatal(err)
	}
	// Seal the GitHub entry for another identity: decrypt now fails.
	key, _, _ := UserTokenStorage("team_1", "usr_alice", GitHub)
	if err := store.KVSet(ctx, key, base64.StdEncoding.EncodeToString([]byte("other|x|y"))); err != nil {
		t.Fatal(err)
	}
	if _, err := ut.Get(ctx, "team_1", "usr_alice", GitHub); err == nil || errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get = %v, want a decrypt error", err)
	}
	got, bad, err := ut.List(ctx, "team_1", "usr_alice")
	if err != nil || strings.Join(got, ",") != "GITHUB,GITLAB" || strings.Join(bad, ",") != "GITHUB" {
		t.Fatalf("list = %v unreadable = %v err = %v", got, bad, err)
	}
	// A deleted token is neither listed nor unreadable.
	if err := ut.Delete(ctx, "team_1", "usr_alice", GitHub); err != nil {
		t.Fatal(err)
	}
	if got, bad, err := ut.List(ctx, "team_1", "usr_alice"); err != nil || strings.Join(got, ",") != "GITLAB" || len(bad) != 0 {
		t.Fatalf("after delete: list = %v unreadable = %v err = %v", got, bad, err)
	}
}
