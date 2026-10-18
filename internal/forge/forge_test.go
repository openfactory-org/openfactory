package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
	"github.com/openfactory-org/openfactory/internal/forge/github"
)

func TestIntegrationProvider(t *testing.T) {
	for in, want := range map[string]string{"GITHUB": "github", "gitlab": "gitlab", "AZURE_DEVOPS": "azure_devops", "azure-devops": "azure_devops", "GIT": "git", "": "github"} {
		if got := IntegrationProvider(in); got != want {
			t.Errorf("IntegrationProvider(%q) = %q want %q", in, got, want)
		}
	}
	if ForgeFromProvider("azure_devops") != AzureDevOps {
		t.Fatal("ForgeFromProvider")
	}
}

func TestResolver(t *testing.T) {
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{
		{ID: "i1", TeamID: "team_1", Provider: "github", Config: map[string]string{"base_url": "https://ghe.local/api/v3", "org": "a"}, SecretRef: "GH_TOKEN"},
		{ID: "i2", TeamID: "team_1", Provider: "gitlab", Config: map[string]string{"org": "a"}, SecretRef: "GL_TOKEN"},
		{ID: "i3", TeamID: "team_1", Provider: "azure_devops", Config: map[string]string{"org": "contoso"}, SecretRef: "ADO"},
		{ID: "i4", TeamID: "team_1", Provider: "git", Config: map[string]string{"base_url": "https://bitbucket.org"}},
	}
	secrets := forgetest.NewSecrets(map[string]string{"GH_TOKEN": "ghp", "GL_TOKEN": "glpat", "ADO": "pat"})
	plugins := &fakePlugins{names: map[string]string{"gitlab": GitLab, "azure_devops": AzureDevOps}}
	r := NewResolver(store, secrets, Options{Plugins: plugins})
	ctx := context.Background()
	look := func(team, codeForge string) core.ForgeLookup {
		return core.ForgeLookup{TeamID: team, CodeForge: codeForge, Repo: "a/b"}
	}
	for _, fname := range []string{GitHub, GitLab, AzureDevOps, Git} {
		f, err := r.Forge(ctx, look("team_1", fname))
		if err != nil {
			t.Fatalf("%s: %v", fname, err)
		}
		if f.Name() != fname {
			t.Fatalf("got %s want %s", f.Name(), fname)
		}
	}
	gh, _ := r.Forge(ctx, look("team_1", "GITHUB"))
	gh2, _ := r.Forge(ctx, look("team_1", "github"))
	if gh != gh2 {
		t.Fatal("client not cached")
	}
	if u, _ := gh.CloneURL(ctx, "a", "b"); !strings.HasPrefix(u, "https://x-access-token:ghp@ghe.local/a/b.git") {
		t.Fatalf("clone url = %s", u)
	}
	if _, err := r.Forge(ctx, look("team_2", "GITHUB")); !errors.Is(err, ErrNoIntegration) {
		t.Fatalf("expected ErrNoIntegration, got %v", err)
	}
	// Plugin forges receive the integration config and resolved token.
	if got := plugins.targets[len(plugins.targets)-1]; got.Provider != "azure_devops" || got.Token != "pat" || got.Config["org"] != "contoso" || got.IntegrationID != "i3" || got.UserToken {
		t.Fatalf("plugin target = %+v", got)
	}

	// Without plugins, plugin-only providers fail with a clear error.
	bare := NewResolver(store, secrets, Options{})
	_, err := bare.Forge(ctx, look("team_1", GitLab))
	if !errors.Is(err, ErrPluginNotInstalled) || !strings.Contains(err.Error(), "openfactory-plugin-gitlab") {
		t.Fatalf("gitlab without plugin: %v", err)
	}
	store.Integrations = append(store.Integrations, core.Integration{ID: "i5", TeamID: "team_1", Provider: "gitea", Config: map[string]string{"org": "a"}})
	if _, err := r.Forge(ctx, look("team_1", "gitea")); !errors.Is(err, ErrPluginNotInstalled) {
		t.Fatalf("unknown provider: %v", err)
	}
}

// fakePlugins is a ForgeProvider serving fake forges.
type fakePlugins struct {
	mu      sync.Mutex
	names   map[string]string // provider -> forge name
	targets []ForgeTarget
}

func (p *fakePlugins) HasForge(provider string) bool { _, ok := p.names[provider]; return ok }

func (p *fakePlugins) ForgeFor(_ context.Context, t ForgeTarget) (core.Forge, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = append(p.targets, t)
	return fakeForge{name: p.names[t.Provider], token: t.Token}, nil
}

type fakeForge struct {
	core.Forge
	name, token string
}

func (f fakeForge) Name() string { return f.name }

func (f fakeForge) CloneURL(_ context.Context, owner, repo string) (string, error) {
	return "https://oauth2:" + f.token + "@plugin.local/" + owner + "/" + repo + ".git", nil
}

func TestResponder(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path+" "+string(b))
		mu.Unlock()
		w.WriteHeader(201)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	gh, err := github.New(github.Config{BaseURL: srv.URL, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	rs := NewResponder(staticResolver{gh}, "github")
	ctx := context.Background()
	err = rs.Reply(ctx, "team_1", core.OutboundMessage{
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 7},
		Text:   "Done", Kind: "question", Options: []string{"yes"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Pushes report on the work item: no comment.
	if err := rs.Reply(ctx, "team_1", core.OutboundMessage{Source: core.SourceRef{Provider: "github", Kind: "push", Repo: "acme/api"}}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || !strings.HasPrefix(paths[0], "POST /repos/acme/api/issues/7/comments") || !strings.Contains(paths[0], "Reply with one of") {
		t.Fatalf("requests = %v", paths)
	}
	if len(Responders(staticResolver{gh})) != 3 || rs.Provider() != "github" {
		t.Fatal("responders")
	}
}

type staticResolver struct{ f core.Forge }

func (s staticResolver) Forge(context.Context, core.ForgeLookup) (core.Forge, error) { return s.f, nil }

// threadForge records review-thread replies and comments.
type threadForge struct {
	core.Forge
	mu        sync.Mutex
	replies   []string
	comments  []string
	supported bool
}

func (f *threadForge) Name() string { return "GITHUB" }

func (f *threadForge) ReplyInThread(_ context.Context, owner, repo string, number int, threadRef, body string) error {
	if !f.supported {
		return core.ErrOperationNotSupported
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, fmt.Sprintf("%s/%s#%d@%s", owner, repo, number, threadRef))
	return nil
}

func (f *threadForge) Comment(_ context.Context, owner, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, fmt.Sprintf("%s/%s#%d", owner, repo, number))
	return nil
}

func TestResponderRepliesInThread(t *testing.T) {
	ctx := context.Background()
	msg := core.OutboundMessage{Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 7}, Text: "Fixed", ThreadRef: "9001"}

	f := &threadForge{supported: true}
	if err := NewResponder(staticResolver{f}, "github").Reply(ctx, "t", msg); err != nil {
		t.Fatal(err)
	}
	if len(f.replies) != 1 || f.replies[0] != "acme/api#7@9001" || len(f.comments) != 0 {
		t.Fatalf("replies=%v comments=%v", f.replies, f.comments)
	}
	// A forge without threads gets a top-level comment.
	f = &threadForge{}
	if err := NewResponder(staticResolver{f}, "github").Reply(ctx, "t", msg); err != nil {
		t.Fatal(err)
	}
	if len(f.replies) != 0 || len(f.comments) != 1 {
		t.Fatalf("replies=%v comments=%v", f.replies, f.comments)
	}
}

// The integration selection rule. The team has a team
// record for acme, a record of factory 1 for acme, and no record for other.
// Factory 1 gets its own record for acme/api, factory 2 gets the team record,
// and each factory gets an error that names other for other/x. A record with
// another owner is never used, even when it is listed first.
func TestResolverSelectsRecordOfRepositoryOwner(t *testing.T) {
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{
		{ID: "beta", TeamID: "team_1", Provider: "github", Config: map[string]string{"org": "beta"}},
		{ID: "team-acme", TeamID: "team_1", Provider: "github", Config: map[string]string{"org": "acme"}, SecretRef: "GH_TEAM"},
		{ID: "fac1-acme", TeamID: "team_1", Provider: "github", FactoryID: "fac_1", Config: map[string]string{"org": "acme"}, SecretRef: "GH_FAC1"},
	}
	secrets := forgetest.NewSecrets(map[string]string{"GH_TEAM": "ghp_team", "GH_FAC1": "ghp_fac1"})
	r := NewResolver(store, secrets, Options{})
	ctx := context.Background()
	for _, c := range []struct{ factory, want string }{
		{"fac_1", "x-access-token:ghp_fac1@"},
		{"fac_2", "x-access-token:ghp_team@"},
		{"", "x-access-token:ghp_team@"},
	} {
		f, err := r.Forge(ctx, core.ForgeLookup{TeamID: "team_1", FactoryID: c.factory, CodeForge: "GITHUB", Repo: "acme/api", Scope: []string{"acme/api", "beta/web"}})
		if err != nil {
			t.Fatalf("factory %q: %v", c.factory, err)
		}
		if u, _ := f.CloneURL(ctx, "acme", "api"); !strings.Contains(u, c.want) {
			t.Fatalf("factory %q: clone url = %s, want %s", c.factory, u, c.want)
		}
	}
	for _, fac := range []string{"fac_1", "fac_2"} {
		_, err := r.Forge(ctx, core.ForgeLookup{TeamID: "team_1", FactoryID: fac, CodeForge: "GITHUB", Repo: "other/x"})
		if !errors.Is(err, ErrNoIntegration) || !strings.Contains(err.Error(), `"other"`) {
			t.Fatalf("factory %s, other/x: err = %v, want ErrNoIntegration naming other", fac, err)
		}
	}
	// A record that is not active fails; the server does not try another record.
	store.Integrations[2].Status = core.IntegrationDisabled
	if _, err := r.Forge(ctx, core.ForgeLookup{TeamID: "team_1", FactoryID: "fac_1", CodeForge: "GITHUB", Repo: "acme/api"}); !errors.Is(err, core.ErrIntegrationDisabled) {
		t.Fatalf("disabled record: %v", err)
	}
}
