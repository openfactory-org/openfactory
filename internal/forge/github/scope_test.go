package github

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestScopedAppTokenNamesRepositories(t *testing.T) {
	f, srv, pemKey := newFake(t)
	c, err := New(Config{BaseURL: srv.URL, AppID: "123", PrivateKey: pemKey, InstallationID: 42})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := c.Scoped([]string{"acme/api", "acme/web", "other/x"})
	if s2 := c.Scoped([]string{"acme/web", "ACME/api", "other/x"}); s2 != s {
		t.Fatal("the same repository list must give the same scoped client")
	}
	if _, err := s.CloneURL(ctx, "acme", "api"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	body := f.bodies["POST /app/installations/42/access_tokens"]
	f.mu.Unlock()
	repos, _ := body["repositories"].([]any)
	if len(repos) != 2 || repos[0] != "api" || repos[1] != "web" {
		t.Fatalf("token request repositories = %v", body["repositories"])
	}
	if _, err := s.CloneURL(ctx, "acme", "billing"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("clone URL outside the scope: %v", err)
	}
	if _, err := s.Token(ctx, "nobody", ""); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("owner outside the scope: %v", err)
	}
	// A personal access token is refused outside the scope too.
	p, _ := New(Config{BaseURL: srv.URL, Token: "ghp_pat"})
	if _, err := p.Scoped([]string{"acme/api"}).CloneURL(ctx, "acme", "billing"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("pat outside the scope: %v", err)
	}
	if c.Scoped(nil) != c {
		t.Fatal("an empty scope must return the client")
	}
}

func TestAuthFailureCallback(t *testing.T) {
	_, srv, pemKey := newFake(t)
	var got []error
	// Installation 7 does not exist on the fake: GitHub answers 404.
	c, err := New(Config{BaseURL: srv.URL, AppID: "123", PrivateKey: pemKey, InstallationID: 7, OnAuthFailure: func(err error) { got = append(got, err) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(context.Background(), "acme", "api"); err == nil {
		t.Fatal("want an error")
	}
	if len(got) != 1 {
		t.Fatalf("callback calls = %d", len(got))
	}
}

func TestInstallationEventsSetStatus(t *testing.T) {
	h, store, bus, _, _, _ := webhookFixture(t)
	store.Integrations = append(store.Integrations, core.Integration{ID: "int_fac", TeamID: "team_1", Provider: "github", FactoryID: "fac_1", Config: map[string]string{"installation_id": "42", "org": "acme"}})
	rec := deliver(t, h, "installation", "whsec", map[string]any{"action": "deleted", "installation": installation})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	for _, id := range []string{"int_gh", "int_fac"} {
		in, _ := store.GetIntegration(context.Background(), id)
		if in.Status != core.IntegrationDisabled || in.StatusReason != "the app was removed" {
			t.Fatalf("%s after deleted: %+v", id, in)
		}
	}
	if n := len(bus.Events()); n != 0 {
		t.Fatalf("lifecycle events published %d events", n)
	}
	deliver(t, h, "installation", "whsec", map[string]any{"action": "unsuspend", "installation": installation})
	if in, _ := store.GetIntegration(context.Background(), "int_gh"); !in.Active() {
		t.Fatalf("after unsuspend: %+v", in)
	}
	// A wrong signature changes nothing.
	deliver(t, h, "installation", "bad", map[string]any{"action": "suspend", "installation": installation})
	if in, _ := store.GetIntegration(context.Background(), "int_gh"); !in.Active() {
		t.Fatalf("after a forged event: %+v", in)
	}
}
