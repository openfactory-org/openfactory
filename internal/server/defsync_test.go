package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/internal/forge/github"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// syncForge serves a definition tree and records commit statuses and check runs.
type syncForge struct {
	core.Forge
	mu        sync.Mutex
	files     map[string]string
	sha       string
	getErr    error
	statuses  []core.CommitStatus
	checkRuns []CheckRun
}

func (f *syncForge) GetFiles(context.Context, string, string, string, string) (map[string]string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, "", f.getErr
	}
	out := make(map[string]string, len(f.files))
	for k, v := range f.files {
		out[k] = v
	}
	return out, f.sha, nil
}

func (f *syncForge) SetCommitStatus(_ context.Context, _, _, _ string, st core.CommitStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = append(f.statuses, st)
	return nil
}

// checkRunForge also publishes check runs.
type checkRunForge struct{ *syncForge }

func (f checkRunForge) PublishCheckRun(_ context.Context, _, _ string, cr CheckRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkRuns = append(f.checkRuns, cr)
	return nil
}

type syncResolver struct{ f core.Forge }

func (r syncResolver) Forge(context.Context, core.ForgeLookup) (core.Forge, error) { return r.f, nil }

// syncSecrets is a SecretStore that knows secret names only.
type syncSecrets struct {
	core.SecretStore
	names []string
}

func (s syncSecrets) List(_ context.Context, teamID, factoryID string) ([]core.Secret, error) {
	if factoryID != "" {
		return nil, nil
	}
	var out []core.Secret
	for _, n := range s.names {
		out = append(out, core.Secret{Name: n, TeamID: teamID})
	}
	return out, nil
}

func newSyncFixture(t *testing.T, fg core.Forge) (*defSync, core.Store, *core.Factory) {
	t.Helper()
	ctx := context.Background()
	st := testdb.New(t)
	if err := st.CreateTeam(ctx, &core.Team{ID: "team1", Name: "team"}); err != nil {
		t.Fatal(err)
	}
	f := &core.Factory{ID: "fac_1", TeamID: "team1", Name: "payments", Source: core.DefinitionGit, GitRepo: "acme/defs", GitBranch: "main"}
	if err := st.CreateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng := engine.New(engine.Options{Store: st, Compile: definition.Compile, Secrets: syncSecrets{names: []string{"KNOWN"}}, Logger: log})
	return &defSync{store: st, forges: syncResolver{fg}, engine: eng, log: log}, st, f
}

func defFiles(t *testing.T, secret string) map[string]string {
	t.Helper()
	files, err := definition.DefaultDefinition(definition.DefaultDefinitionOptions{Name: "payments", Agents: []string{"foreman"}})
	if err != nil {
		t.Fatal(err)
	}
	if secret != "" {
		files["factory.yaml"] += "secrets:\n  - " + secret + "\n"
	}
	return files
}

func TestSyncStoresTheCommitOnlyWhenDone(t *testing.T) {
	ctx := context.Background()
	fg := &syncForge{files: defFiles(t, "KNOWN"), sha: "aaaaaaa1"}
	d, st, f := newSyncFixture(t, fg)
	last := func() string { v, _ := st.KVGet(ctx, d.lastKey(f.ID)); return v }

	if err := d.Sync(ctx, f); err != nil {
		t.Fatal(err)
	}
	if last() != "aaaaaaa1" {
		t.Fatalf("last sha = %q after a good apply", last())
	}

	// A validation error is final: the commit is stored as done.
	fg.files, fg.sha = defFiles(t, "MISSING"), "bbbbbbb2"
	f, _ = st.GetFactory(ctx, f.ID)
	active := f.ActiveVersionID
	if err := d.Sync(ctx, f); err != nil {
		t.Fatal(err)
	}
	if last() != "bbbbbbb2" {
		t.Fatalf("last sha = %q after a validation error", last())
	}
	if f2, _ := st.GetFactory(ctx, f.ID); f2.ActiveVersionID != active {
		t.Fatal("an invalid commit changed the active version")
	}

	// Any other error is tried again: the commit is not stored.
	fg.files, fg.sha = defFiles(t, "KNOWN"), "ccccccc3"
	broken := &brokenStore{Store: st, failVersion: true}
	d.engine = engine.New(engine.Options{Store: broken, Compile: definition.Compile, Secrets: syncSecrets{names: []string{"KNOWN"}}, Logger: d.log})
	d.store = broken
	if err := d.Sync(ctx, f); err == nil {
		t.Fatal("sync hid a store failure")
	}
	if last() != "bbbbbbb2" {
		t.Fatalf("last sha = %q after a transient failure", last())
	}
	broken.failVersion = false
	if err := d.Sync(ctx, f); err != nil {
		t.Fatal(err)
	}
	if last() != "ccccccc3" {
		t.Fatalf("last sha = %q after the retry", last())
	}
}

type brokenStore struct {
	core.Store
	failVersion bool
}

func (s *brokenStore) CreateDefinitionVersion(ctx context.Context, v *core.DefinitionVersion) error {
	if s.failVersion && v.Valid {
		return errors.New("database is down")
	}
	return s.Store.CreateDefinitionVersion(ctx, v)
}

func TestPullRequestCheckRunsThePreflight(t *testing.T) {
	ctx := context.Background()
	fg := &syncForge{files: defFiles(t, "MISSING"), sha: "head1"}
	d, _, f := newSyncFixture(t, fg)
	pr := github.DefinitionPR{FactoryID: f.ID, Repo: "acme/defs", Number: 4, HeadSHA: "head1", CheckName: "openfactory/definition"}
	if err := d.checkPullRequest(ctx, pr); err != nil {
		t.Fatal(err)
	}
	got := fg.statuses[len(fg.statuses)-1]
	if got.State != "failure" || !strings.Contains(got.Description, "MISSING") {
		t.Fatalf("status = %+v", got)
	}

	// A forge with check runs gets the full summary of the changes.
	cf := checkRunForge{&syncForge{files: defFiles(t, "KNOWN"), sha: "head2"}}
	d.forges = syncResolver{cf}
	pr.HeadSHA = "head2"
	if err := d.checkPullRequest(ctx, pr); err != nil {
		t.Fatal(err)
	}
	if len(cf.statuses) != 0 || len(cf.checkRuns) != 2 {
		t.Fatalf("statuses = %+v, check runs = %+v", cf.statuses, cf.checkRuns)
	}
	cr := cf.checkRuns[1]
	if cr.Status != "completed" || cr.Conclusion != "success" || !strings.Contains(cr.Summary, "+ agent foreman") {
		t.Fatalf("check run = %+v", cr)
	}
}

func TestCheckBranchBeforeTheSourceChanges(t *testing.T) {
	ctx := context.Background()
	fg := &syncForge{files: defFiles(t, "MISSING"), sha: "x"}
	d, _, f := newSyncFixture(t, fg)
	err := d.CheckBranch(ctx, f)
	var bie *BranchInvalidError
	if !errors.As(err, &bie) || !errors.Is(err, core.ErrInvalid) || len(bie.ValidationErrors()) == 0 {
		t.Fatalf("CheckBranch = %v", err)
	}
	fg.files = defFiles(t, "KNOWN")
	if err := d.CheckBranch(ctx, f); err != nil {
		t.Fatal(err)
	}
	fg.getErr = errors.New("branch not found")
	if err := d.CheckBranch(ctx, f); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("CheckBranch = %v", err)
	}
}

// ghCheckForge serves the files from a fake and publishes check runs with
// the real GitHub client, which talks to a fake of the GitHub API.
type ghCheckForge struct {
	*syncForge
	gh *github.Client
}

func (f ghCheckForge) PublishCheckRun(ctx context.Context, owner, repo string, cr CheckRun) error {
	return f.gh.PublishCheckRun(ctx, owner, repo, cr)
}

func TestPullRequestCheckRunReachesGitHub(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var calls []string
	bodies := map[string]map[string]any{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := r.Method + " " + r.URL.Path
		calls = append(calls, key)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies[key] = body
		switch key {
		case "POST /app/installations/42/access_tokens":
			fmt.Fprintf(w, `{"token":"ghs_t","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "POST /repos/acme/defs/check-runs":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":321}`)
		case "PATCH /repos/acme/defs/check-runs/321":
			fmt.Fprint(w, `{"id":321}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	gh, err := github.New(github.Config{BaseURL: api.URL, AppID: "1", PrivateKey: pemKey, InstallationID: 42})
	if err != nil {
		t.Fatal(err)
	}

	fg := ghCheckForge{&syncForge{files: defFiles(t, "MISSING"), sha: "head1"}, gh}
	d, _, f := newSyncFixture(t, fg)
	pr := github.DefinitionPR{FactoryID: f.ID, Repo: "acme/defs", Number: 4, HeadSHA: "head1", CheckName: "openfactory/definition"}
	if err := d.checkPullRequest(ctx, pr); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fg.statuses) != 0 {
		t.Fatalf("a forge with check runs got commit statuses: %+v", fg.statuses)
	}
	want := []string{"POST /app/installations/42/access_tokens", "POST /repos/acme/defs/check-runs", "PATCH /repos/acme/defs/check-runs/321"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if bodies["POST /repos/acme/defs/check-runs"]["status"] != "in_progress" {
		t.Fatalf("first body = %v", bodies["POST /repos/acme/defs/check-runs"])
	}
	done := bodies["PATCH /repos/acme/defs/check-runs/321"]
	out, _ := done["output"].(map[string]any)
	if done["conclusion"] != "failure" || !strings.Contains(fmt.Sprint(out["summary"]), "MISSING") {
		t.Fatalf("final body = %v", done)
	}
}
