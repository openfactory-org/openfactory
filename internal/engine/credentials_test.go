package engine

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
)

// fakeGitHubApp is a GitHub API that issues installation tokens. Each token
// lives one hour on the test clock.
type fakeGitHubApp struct {
	srv   *httptest.Server
	clock func() time.Time

	mu     sync.Mutex
	n      int
	bodies []map[string]any
	status int // when not 0, the token request fails with it
}

func newFakeGitHubApp(t *testing.T, clock func() time.Time) *fakeGitHubApp {
	g := &fakeGitHubApp{clock: clock}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.status != 0 {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		g.n++
		g.bodies = append(g.bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("ghs_%d", g.n), "expires_at": g.clock().Add(time.Hour).UTC().Format(time.RFC3339)})
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHubApp) lastBody() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		return nil
	}
	return g.bodies[len(g.bodies)-1]
}

func appKeyPEM(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// appFixture is a planner whose forge credentials come from a GitHub App
// through the real forge resolver.
type appFixture struct {
	*planFixture
	gh       *fakeGitHubApp
	resolver *forge.Resolver
}

func newAppFixture(t *testing.T, modify func(*PlannerOptions), defMods ...func(*core.CompiledDefinition)) *appFixture {
	t.Helper()
	af := &appFixture{}
	f := newPlanFixture(t, func(o *PlannerOptions) {
		if modify != nil {
			modify(o)
		}
	}, defMods...)
	af.planFixture = f
	af.gh = newFakeGitHubApp(t, f.clock.Now)
	ctx := context.Background()
	if _, err := f.Secrets.Put(ctx, "team1", "", "GH_APP_KEY", core.SecretGeneric, appKeyPEM(t)); err != nil {
		t.Fatal(err)
	}
	f.Store.connectIntegration(core.Integration{ID: "int_team", TeamID: "team1", Provider: "github", SecretRef: "GH_APP_KEY",
		Config: map[string]string{"org": "acme", "app_id": "1", "installation_id": "42", "base_url": af.gh.srv.URL, "web_url": "https://github.test"}})
	af.resolver = forge.NewResolver(f.Store, f.Secrets, forge.Options{Now: f.clock.Now})
	f.x.opts.Forges = af.resolver
	return af
}

func (af *appFixture) dispatch(t *testing.T) *core.Run {
	t.Helper()
	_, run, err := af.Engine.Dispatch(context.Background(), core.DispatchRequest{FactoryID: af.Factory.ID, Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// TestGitCredentialAfterTwoHours checks git credentials: the
// credential helper of a sandbox that started 2 hours ago gets a valid
// token, so a push at that time works.
func TestGitCredentialAfterTwoHours(t *testing.T) {
	af := newAppFixture(t, nil)
	run := af.dispatch(t)
	ctx := context.Background()
	start := af.clock.Now()

	user, first, exp, err := af.x.RunnerGitCredential(ctx, run, "github.test", "acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if user != "x-access-token" || first != "ghs_1" || !exp.After(start) {
		t.Fatalf("credential %q %q %v", user, first, exp)
	}
	af.clock.Advance(2 * time.Hour)
	_, later, exp2, err := af.x.RunnerGitCredential(ctx, run, "github.test", "acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if later == first {
		t.Fatalf("the token of the sandbox start was given again after 2 hours")
	}
	if !exp2.After(af.clock.Now()) {
		t.Fatalf("expiry %v is not after now %v", exp2, af.clock.Now())
	}
}

// TestCachedForgeTokenKeepsTenMinutes checks that the forge client does not
// give a token that expires in less than 10 minutes.
func TestCachedForgeTokenKeepsTenMinutes(t *testing.T) {
	af := newAppFixture(t, nil)
	run := af.dispatch(t)
	ctx := context.Background()
	_, a, _, _ := af.x.RunnerGitCredential(ctx, run, "github.test", "acme/api.git")
	af.clock.Advance(5 * time.Minute) // inside the resolver cache, 55 minutes left
	_, b, _, _ := af.x.RunnerGitCredential(ctx, run, "github.test", "acme/api.git")
	if a != b {
		t.Fatalf("a token with 55 minutes left was not reused: %q %q", a, b)
	}
}

// TestFactoryCannotCloneOtherFactoryRepository checks that the
// token that a run of factory A gets is requested for the
// repositories of factory A only, and the platform refuses a credential for
// a repository that only factory B lists.
func TestFactoryCannotCloneOtherFactoryRepository(t *testing.T) {
	af := newAppFixture(t, nil)
	run := af.dispatch(t)
	ctx := context.Background()
	if _, _, _, err := af.x.RunnerGitCredential(ctx, run, "github.test", "acme/api.git"); err != nil {
		t.Fatal(err)
	}
	body := af.gh.lastBody()
	repos, _ := body["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "api" {
		t.Fatalf("token request repositories = %v", body["repositories"])
	}
	// acme/billing is a repository of factory B only.
	if _, _, _, err := af.x.RunnerGitCredential(ctx, run, "github.test", "acme/billing.git"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("credential for a repository of another factory: %v", err)
	}
	f, err := af.resolver.Forge(ctx, core.ForgeLookup{TeamID: "team1", FactoryID: af.Factory.ID, CodeForge: "GITHUB", Repo: "acme/api", Scope: []string{"acme/api"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.CloneURL(ctx, "acme", "billing"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("clone URL for a repository of another factory: %v", err)
	}
}

// TestFactoryRecordWinsOverTeamRecord checks the choice of the record.
func TestFactoryRecordWinsOverTeamRecord(t *testing.T) {
	af := newAppFixture(t, nil)
	other := newFakeGitHubApp(t, af.clock.Now)
	af.Store.connectIntegration(core.Integration{ID: "int_fac", TeamID: "team1", Provider: "github", FactoryID: af.Factory.ID, SecretRef: "GH_APP_KEY",
		Config: map[string]string{"org": "acme", "app_id": "2", "installation_id": "43", "base_url": other.srv.URL, "web_url": "https://github.test"}})
	af.Store.connectIntegration(core.Integration{ID: "int_b", TeamID: "team1", Provider: "github", FactoryID: "fac_b", SecretRef: "missing", Config: map[string]string{"org": "acme"}})
	run := af.dispatch(t)
	if _, _, _, err := af.x.RunnerGitCredential(context.Background(), run, "github.test", "acme/api.git"); err != nil {
		t.Fatal(err)
	}
	if other.lastBody() == nil || af.gh.lastBody() != nil {
		t.Fatal("the record of the factory was not used")
	}
}

// TestDisabledIntegrationFailsRun checks that a run whose integration is
// not active fails with integration_disabled before a task.
func TestDisabledIntegrationFailsRun(t *testing.T) {
	af := newAppFixture(t, nil)
	ctx := context.Background()
	in, _ := af.Store.GetIntegration(ctx, "int_team")
	in.Status, in.StatusReason = core.IntegrationDisabled, "the app was removed"
	_ = af.Store.UpsertIntegration(ctx, in)
	af.resolver.Invalidate("team1")

	run := af.dispatch(t)
	err := af.x.CredentialPreflight(ctx, run)
	var fe *core.FailureCodeError
	if !errors.As(err, &fe) || fe.Code != core.FailureIntegrationDisabled {
		t.Fatalf("preflight = %v", err)
	}
}

// TestAuthFailureMarksIntegration checks that a refused app credential sets
// the status auth_failed.
func TestAuthFailureMarksIntegration(t *testing.T) {
	af := newAppFixture(t, nil)
	af.gh.mu.Lock()
	af.gh.status = http.StatusNotFound // the installation was removed
	af.gh.mu.Unlock()
	run := af.dispatch(t)
	ctx := context.Background()
	if _, _, _, err := af.x.RunnerGitCredential(ctx, run, "github.test", "acme/api.git"); err == nil {
		t.Fatal("want an error")
	}
	in, _ := af.Store.GetIntegration(ctx, "int_team")
	if in.Status != core.IntegrationAuthFailed || in.StatusUpdatedAt == nil {
		t.Fatalf("integration %+v", in)
	}
	if err := af.x.CredentialPreflight(ctx, run); err == nil {
		t.Fatal("preflight must fail after an auth failure")
	}
}

// TestRunClonesEachRepositoryWithItsOwnerRecord checks the selection rule of
// forge records for a run: a run with one repository of each of
// two owners clones each one with the token of its owner.
func TestRunClonesEachRepositoryWithItsOwnerRecord(t *testing.T) {
	f := newPlanFixture(t, nil, func(d *core.CompiledDefinition) {
		d.Repositories = append(d.Repositories, core.RepoRef{CodeForge: "GITHUB", Owner: "beta", Name: "web"})
	})
	ctx := context.Background()
	for name, v := range map[string]string{"GH_ACME": "tok-acme", "GH_BETA": "tok-beta"} {
		if _, err := f.Secrets.Put(ctx, "team1", "", name, core.SecretGeneric, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	f.Store.connectIntegration(core.Integration{ID: "int_beta", TeamID: "team1", Provider: "github", SecretRef: "GH_BETA", Config: map[string]string{"org": "beta"}})
	f.Store.connectIntegration(core.Integration{ID: "int_acme", TeamID: "team1", Provider: "github", SecretRef: "GH_ACME", Config: map[string]string{"org": "acme"}})
	f.x.opts.Forges = forge.NewResolver(f.Store, f.Secrets, forge.Options{Now: f.clock.Now})
	_, run, err := f.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: f.Factory.ID, Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := LoadFactoryRuntime(ctx, f.Store, run.FactoryID)
	if err != nil {
		t.Fatal(err)
	}
	cos, err := f.x.checkouts(ctx, run, rt)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"acme/api": "x-access-token:tok-acme@", "beta/web": "x-access-token:tok-beta@"}
	if len(cos) != len(want) {
		t.Fatalf("checkouts = %+v", cos)
	}
	for _, co := range cos {
		if w := want[co.Owner+"/"+co.Name]; w == "" || !strings.Contains(co.CloneURL, w) {
			t.Errorf("%s/%s: clone url %s, want %s", co.Owner, co.Name, co.CloneURL, w)
		}
	}
}
