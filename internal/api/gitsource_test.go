package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

func TestGitSourceErrorsAreShort(t *testing.T) {
	var checkErr error
	e := newTestEnv(t, func(o *Options) {
		o.CheckGitSource = func(context.Context, *core.Factory) error { return checkErr }
	})
	ctx := context.Background()
	if err := e.store.UpsertIntegration(ctx, &core.Integration{TeamID: e.team.ID, Provider: "github", Name: "acme-github",
		Config: map[string]string{"org": "acme"}, SecretRef: "GH_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	refused := &forgeutil.APIError{Method: "GET", URL: "https://api.github.com/repos/acme/defs/branches/main", Status: http.StatusUnauthorized,
		Body: `{"message":"Bad credentials"}`}
	notFound := &forgeutil.APIError{Method: "GET", URL: "https://api.github.com/repos/acme/defs/branches/main", Status: http.StatusNotFound}
	wrap := func(err error) error {
		return fmt.Errorf("%w: could not read the definition from acme/defs branch \"main\": github: resolve main: %w", core.ErrInvalid, err)
	}
	cases := []struct {
		name    string
		repo    string
		err     error
		want    string
		notWant []string
	}{
		{"token refused", "acme/defs", wrap(refused),
			"The code host refused the token of integration acme-github (HTTP 401). Check the token secret. The source did not change.",
			[]string{"api.github.com", "Bad credentials"}},
		{"token refused, url source", "https://github.com/acme/defs", wrap(refused),
			"The code host refused the token of integration acme-github (HTTP 401).", nil},
		{"not found", "acme/defs", wrap(notFound),
			`The code host did not find branch "main" of acme/defs, or the token of integration acme-github cannot see the repository (HTTP 404).`, nil},
		{"no integration", "other/defs",
			fmt.Errorf("forge: team %s, repository other/defs: no github integration record for the owner \"other\": %w", e.team.ID, forgeutil.ErrNoIntegration),
			"No code host integration covers other/defs. Connect one under Integrations.", []string{"team_"}},
		{"disabled", "acme/defs", fmt.Errorf("forge: integration int_1 (github) is disabled: %w", core.ErrIntegrationDisabled),
			"The code host integration acme-github is not active. Fix it under Integrations.", []string{"int_1"}},
		{"invalid repository", "acme", fmt.Errorf("%w: git repository \"acme\" must name owner/repository", core.ErrInvalid),
			`Git repository "acme" must name owner/repository.`, nil},
		{"network error", "acme/defs", wrap(errors.New("dial tcp: lookup api.github.com: no such host")),
			`The definition on branch "main" of acme/defs could not be read. The server log has the details.`, []string{"dial tcp"}},
	}
	fid := e.createFactory("git-errors")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkErr = c.err
			r := e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"source": "git", "git_repo": c.repo}), http.StatusBadRequest)
			d := str(r.Body, "detail")
			if !strings.Contains(d, c.want) {
				t.Fatalf("detail = %q, want %q", d, c.want)
			}
			for _, nw := range c.notWant {
				if strings.Contains(d, nw) {
					t.Fatalf("detail %q has %q", d, nw)
				}
			}
		})
	}

	// The API error keeps the cause for errors.Is.
	f := &core.Factory{ID: fid, TeamID: e.team.ID, GitRepo: "acme/defs"}
	e.srv.opts.CheckGitSource = func(context.Context, *core.Factory) error { return wrap(refused) }
	err := e.srv.checkGitBranch(ctx, f, "the source did not change")
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != CodeInvalidRequest || !errors.Is(err, core.ErrInvalid) || forgeutil.StatusCode(err) != http.StatusUnauthorized {
		t.Fatalf("err = %v", err)
	}
}
