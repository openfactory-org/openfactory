package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/openfactory-org/openfactory/internal/api/apitest"
	"github.com/openfactory-org/openfactory/internal/core"
)

// checkingEngine adds the preflight to the fake engine.
type checkingEngine struct {
	*apitest.Engine
	missing string // a secret name that the preflight reports as missing
}

func (c *checkingEngine) CheckDefinition(_ context.Context, _ string, files map[string]string) (*core.DefinitionCheck, error) {
	def, verrs := apitest.Compile(files)
	chk := &core.DefinitionCheck{Definition: def, Errors: verrs,
		Warnings: []core.ValidationError{{File: "automations/a/automation.md", Field: "triggers[0].filter.teams", Message: "names are not checked now"}},
		Skipped:  []core.SkippedCheck{{Check: "secrets", Reason: "no secret store is configured"}}}
	if c.missing != "" {
		chk.Errors = append(chk.Errors, core.ValidationError{File: "factory.yaml", Field: "secrets", Message: "secret " + c.missing + " does not exist"})
	}
	return chk, nil
}

type branchErr struct{ errs []core.ValidationError }

func (b branchErr) Error() string                            { return "branch invalid" }
func (b branchErr) ValidationErrors() []core.ValidationError { return b.errs }

func TestValidateRunsThePreflight(t *testing.T) {
	var ce *checkingEngine
	e := newTestEnv(t, func(o *Options) {
		ce = &checkingEngine{Engine: o.Engine.(*apitest.Engine)}
		o.Engine = ce
	})
	fid := e.createFactory("pre")
	files := apitest.MinimalFiles("pre")
	v := e.must(e.do("POST", "/factories/"+fid+"/definition/validate", e.viewer.Key, map[string]any{"files": files}), http.StatusOK)
	if v.Body["valid"] != true || len(list(v.Body, "warnings")) != 1 || len(list(v.Body, "skipped")) != 1 || v.Body["summary"] == nil {
		t.Fatalf("validate = %s", v.Raw)
	}
	ce.missing = "DB_URL"
	v = e.must(e.do("POST", "/factories/"+fid+"/definition/validate", e.viewer.Key, map[string]any{"files": files}), http.StatusOK)
	if v.Body["valid"] != false || len(list(v.Body, "errors")) != 1 || v.Body["summary"] != nil {
		t.Fatalf("validate = %s", v.Raw)
	}
}

func TestSourceChangeToGitChecksTheBranch(t *testing.T) {
	var checkErr error
	calls := 0
	e := newTestEnv(t, func(o *Options) {
		o.CheckGitSource = func(context.Context, *core.Factory) error { calls++; return checkErr }
	})
	fid := e.createFactory("to-git")
	checkErr = branchErr{errs: []core.ValidationError{{File: "factory.yaml", Message: "secret X does not exist"}}}
	r := e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"source": "git", "git_repo": "github.com/acme/defs"}), http.StatusBadRequest)
	if len(list(r.Body, "errors")) != 1 {
		t.Fatalf("body = %s", r.Raw)
	}
	f, _ := e.store.GetFactory(context.Background(), fid)
	if f.Source != core.DefinitionManaged {
		t.Fatalf("source = %s", f.Source)
	}
	checkErr = errors.New("no forge")
	e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"source": "git", "git_repo": "github.com/acme/defs"}), http.StatusBadRequest)
	checkErr = nil
	e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"source": "git", "git_repo": "github.com/acme/defs"}), http.StatusOK)
	if f, _ := e.store.GetFactory(context.Background(), fid); f.Source != core.DefinitionGit || calls != 3 {
		t.Fatalf("source = %s, calls = %d", f.Source, calls)
	}
	// Other settings do not check the branch again.
	e.must(e.do("PATCH", "/factories/"+fid, e.editor.Key, map[string]any{"description": "x"}), http.StatusOK)
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestCreateWithGitSourceChecksTheBranch(t *testing.T) {
	var checkErr error
	var seen *core.Factory
	e := newTestEnv(t, func(o *Options) {
		o.CheckGitSource = func(_ context.Context, f *core.Factory) error { c := *f; seen = &c; return checkErr }
	})
	body := map[string]any{"name": "from-git", "source": "git", "git_repo": "github.com/acme/defs", "git_branch": "release"}
	count := func() int {
		fs, _ := e.store.ListFactories(context.Background(), e.team.ID, "")
		return len(fs)
	}
	before := count()

	checkErr = branchErr{errs: []core.ValidationError{{File: "factory.yaml", Message: "secret X does not exist"}}}
	r := e.must(e.do("POST", "/factories", e.editor.Key, body), http.StatusBadRequest)
	if len(list(r.Body, "errors")) != 1 {
		t.Fatalf("body = %s", r.Raw)
	}
	if seen == nil || seen.GitBranch != "release" || seen.GitRepo != "github.com/acme/defs" || seen.Source != core.DefinitionGit {
		t.Fatalf("the check saw %+v", seen)
	}
	if count() != before {
		t.Fatal("a refused factory must not stay in the store")
	}

	checkErr = errors.New("no forge")
	e.must(e.do("POST", "/factories", e.editor.Key, body), http.StatusBadRequest)
	if count() != before {
		t.Fatal("a factory whose branch cannot be checked must not stay in the store")
	}

	checkErr = nil
	e.must(e.do("POST", "/factories", e.editor.Key, body), http.StatusCreated)
	if count() != before+1 {
		t.Fatal("the factory was not created")
	}
	// A managed factory does not check any branch.
	seen = nil
	checkErr = errors.New("must not be called")
	e.must(e.do("POST", "/factories", e.editor.Key, map[string]any{"name": "managed-one"}), http.StatusCreated)
	if seen != nil {
		t.Fatal("a managed factory must not check a branch")
	}
}
