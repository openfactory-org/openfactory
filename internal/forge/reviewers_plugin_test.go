package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// pluginProvider is a ForgeProvider with one fake forge for each provider.
// The forge reads the account of a personal token, and requests a review with
// the team credential, like the GitLab and Azure DevOps plugins.
type pluginProvider struct {
	mu        sync.Mutex
	logins    map[string]string // personal token -> account
	requested []string
	noLogin   bool
}

func (p *pluginProvider) HasForge(provider string) bool {
	return provider == "gitlab" || provider == "azure_devops"
}

func (p *pluginProvider) ForgeFor(_ context.Context, t ForgeTarget) (core.Forge, error) {
	return &pluginFake{p: p, t: t}, nil
}

type pluginFake struct {
	core.Forge
	p *pluginProvider
	t ForgeTarget
}

func (f *pluginFake) Name() string { return strings.ToUpper(f.t.Provider) }

func (f *pluginFake) Login(context.Context) (string, error) {
	if f.p.noLogin {
		return "", fmt.Errorf("plugin: %w", core.ErrOperationNotSupported)
	}
	if l := f.p.logins[f.t.Token]; f.t.UserToken && l != "" {
		return l, nil
	}
	return "", errors.New("the token is not a personal token")
}

func (f *pluginFake) RequestReviewers(_ context.Context, owner, repo string, n int, logins []string) error {
	if f.t.UserToken {
		return errors.New("the review request must use the team credential")
	}
	f.p.mu.Lock()
	defer f.p.mu.Unlock()
	f.p.requested = append(f.p.requested, fmt.Sprintf("%s %s/%s#%d %s as %s", f.t.Provider, owner, repo, n, strings.Join(logins, ","), f.t.Token))
	return nil
}

// TestRequestReviewPluginForges shows that a review is requested on GitLab
// and on Azure DevOps through the plugin forge: the account is read with
// the personal token of the member, and the request uses the team credential.
func TestRequestReviewPluginForges(t *testing.T) {
	ctx := context.Background()
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{
		{ID: "i1", TeamID: "team_1", Provider: "gitlab", Config: map[string]string{"org": "acme"}, SecretRef: "GL"},
		{ID: "i2", TeamID: "team_1", Provider: "azure_devops", Config: map[string]string{"org": "contoso"}, SecretRef: "ADO"},
	}
	plug := &pluginProvider{logins: map[string]string{"alice-gl": "alice", "alice-ado": "11111111-2222"}}
	ut := &UserTokens{Store: store, Cipher: fakeCipher{}}
	r := NewResolver(store, forgetest.NewSecrets(map[string]string{"GL": "team-gl", "ADO": "team-ado"}), Options{UserTokens: ut, Plugins: plug})
	if err := ut.Set(ctx, "team_1", "usr_alice", "GITLAB", "alice-gl"); err != nil {
		t.Fatal(err)
	}
	if err := ut.Set(ctx, "team_1", "usr_alice", "AZURE_DEVOPS", "alice-ado"); err != nil {
		t.Fatal(err)
	}

	glPR := core.PullRequestRef{Forge: "GITLAB", Repo: "acme/sub/def", Number: 3, URL: "https://gitlab.example.com/acme/sub/def/-/merge_requests/3"}
	if err := r.RequestReview(ctx, "team_1", glPR, "usr_alice"); err != nil {
		t.Fatal(err)
	}
	adoPR := core.PullRequestRef{Forge: "AZURE_DEVOPS", Repo: "proj/web", Number: 11}
	if err := r.RequestReview(ctx, "team_1", adoPR, "usr_alice"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"gitlab acme/sub/def#3 alice as team-gl",
		"azure_devops proj/web#11 11111111-2222 as team-ado",
	}
	if fmt.Sprint(plug.requested) != fmt.Sprint(want) {
		t.Fatalf("requested = %q, want %q", plug.requested, want)
	}

	// A member without a linked account gets an external-authentication error.
	if err := r.RequestReview(ctx, "team_1", glPR, "usr_bob"); !core.IsExternalAuthRequired(err) {
		t.Fatalf("no linked account: %v", err)
	}
	// A plugin that cannot read the login or request a review is "not supported".
	plug.noLogin = true
	if err := r.RequestReview(ctx, "team_1", glPR, "usr_alice"); !errors.Is(err, core.ErrOperationNotSupported) {
		t.Fatalf("no login support: %v", err)
	}
}
