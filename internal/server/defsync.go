package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/forge/github"
	"github.com/openfactory-org/openfactory/internal/links"
)

// defSync keeps git-backed factory definitions in sync with their production
// branch (push webhooks plus periodic polling, which also covers forges that
// deliver no webhooks) and publishes the definition check on pull requests.
type defSync struct {
	store     core.Store
	forges    core.ForgeResolver
	engine    *engine.Engine
	publicURL string
	log       *slog.Logger
	every     time.Duration
	// isLeader gates periodic polling to the leader replica (nil = always).
	isLeader func() bool
}

// GitLocation is a parsed Factory.GitRepo.
type GitLocation struct {
	CodeForge string
	Owner     string
	Repo      string
}

// ParseGitRepo maps a factory's git_repo ("github.com/acme/defs",
// "https://gitlab.example.com/group/sub/defs.git", "acme/defs") to a code
// forge and repository. Bare owner/name defaults to GitHub; unknown hosts use
// the generic git forge.
func ParseGitRepo(raw string) (GitLocation, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return GitLocation{}, fmt.Errorf("%w: empty git repository", core.ErrInvalid)
	}
	host := ""
	path := s
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		host, path = u.Hostname(), u.Path
	} else if i := strings.Index(s, "/"); i > 0 && strings.Contains(s[:i], ".") {
		host, path = s[:i], s[i+1:]
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, name := forgeutil.SplitRepo(path)
	if owner == "" || name == "" {
		return GitLocation{}, fmt.Errorf("%w: git repository %q must name owner/repository", core.ErrInvalid, raw)
	}
	forge := "GITHUB"
	h := strings.ToLower(host)
	switch {
	case h == "" || strings.Contains(h, "github"):
	case strings.Contains(h, "gitlab"):
		forge = "GITLAB"
	case strings.Contains(h, "dev.azure.com"), strings.Contains(h, "visualstudio.com"):
		forge = "AZURE_DEVOPS"
		// dev.azure.com/org/project/_git/repo → owner org/project
		owner = strings.Replace(owner, "/_git", "", 1)
	default:
		forge = "GIT"
	}
	return GitLocation{CodeForge: forge, Owner: owner, Repo: name}, nil
}

func branchOf(f *core.Factory) string {
	if f.GitBranch == "" {
		return "main"
	}
	return f.GitBranch
}

func (d *defSync) lastKey(factoryID string) string { return "defsync:" + factoryID + ":last_sha" }

// Sync fetches a git-backed factory's production branch and applies it when
// the commit differs from the last one applied or attempted.
func (d *defSync) Sync(ctx context.Context, f *core.Factory) error {
	if f.Source != core.DefinitionGit || f.GitRepo == "" {
		return nil
	}
	loc, err := ParseGitRepo(f.GitRepo)
	if err != nil {
		return err
	}
	fg, err := d.forges.Forge(ctx, core.ForgeLookup{TeamID: f.TeamID, FactoryID: f.ID, CodeForge: loc.CodeForge, Repo: loc.Owner + "/" + loc.Repo})
	if err != nil {
		return err
	}
	files, sha, err := fg.GetFiles(ctx, loc.Owner, loc.Repo, branchOf(f), f.GitPath)
	if err != nil {
		return fmt.Errorf("fetch definition: %w", err)
	}
	if sha != "" {
		if last, err := d.store.KVGet(ctx, d.lastKey(f.ID)); err == nil && last == sha {
			return nil
		}
		if f.ActiveVersionID != "" {
			if v, err := d.store.GetDefinitionVersion(ctx, f.ActiveVersionID); err == nil && v.CommitSHA == sha {
				_ = d.store.KVSet(ctx, d.lastKey(f.ID), sha)
				return nil
			}
		}
	}
	msg := "Sync " + branchOf(f)
	if sha != "" {
		msg += " @ " + short(sha)
	}
	v, err := d.engine.ApplyDefinition(ctx, f.ID, files, "git", msg, sha)
	if err != nil {
		if v != nil && errors.Is(err, core.ErrInvalid) {
			// A validation error is final for this commit: do not try it again.
			if sha != "" {
				_ = d.store.KVSet(ctx, d.lastKey(f.ID), sha)
			}
			d.log.Warn("git definition invalid; last valid version kept", "factory_id", f.ID, "sha", sha, "errors", len(v.Errors))
			return nil
		}
		// Any other error can be transient: the next sync tries the commit again.
		return err
	}
	if sha != "" {
		_ = d.store.KVSet(ctx, d.lastKey(f.ID), sha)
	}
	d.log.Info("git definition applied", "factory_id", f.ID, "sha", sha, "version_id", v.ID)
	return nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// gitFactories lists every git-backed factory.
func (d *defSync) gitFactories(ctx context.Context) ([]core.Factory, error) {
	teams, err := d.store.ListTeams(ctx)
	if err != nil {
		return nil, err
	}
	var out []core.Factory
	for _, t := range teams {
		fs, err := d.store.ListFactories(ctx, t.ID, "")
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			if f.Source == core.DefinitionGit && f.GitRepo != "" {
				out = append(out, f)
			}
		}
	}
	return out, nil
}

// SyncAll syncs every git-backed factory. Only the leader replica polls.
func (d *defSync) SyncAll(ctx context.Context) {
	if d.isLeader != nil && !d.isLeader() {
		return
	}
	fs, err := d.gitFactories(ctx)
	if err != nil {
		d.log.Warn("list git-backed factories", "err", err)
		return
	}
	for i := range fs {
		if err := d.Sync(ctx, &fs[i]); err != nil && ctx.Err() == nil {
			d.log.Warn("sync git definition", "factory_id", fs[i].ID, "repo", fs[i].GitRepo, "err", err)
		}
	}
}

func (d *defSync) loop(ctx context.Context) {
	if d.every < 0 {
		return
	}
	every := d.every
	if every == 0 {
		every = 5 * time.Minute
	}
	first := time.NewTimer(2 * time.Second)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	d.SyncAll(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.SyncAll(ctx)
		}
	}
}

// onPush handles a push to a git-backed factory's production branch.
func (d *defSync) onPush(ctx context.Context, repo, branch, sha string) {
	fs, err := d.gitFactories(ctx)
	if err != nil {
		d.log.Warn("definition push", "err", err)
		return
	}
	want := github.NormalizeRepo(repo)
	for i := range fs {
		f := &fs[i]
		if github.NormalizeRepo(f.GitRepo) != want || branchOf(f) != branch {
			continue
		}
		if err := d.Sync(ctx, f); err != nil {
			d.log.Warn("sync git definition after push", "factory_id", f.ID, "sha", sha, "err", err)
		}
	}
}

// onPullRequest validates a pull request's definition and publishes the
// openfactory/definition commit status with a diff summary.
func (d *defSync) onPullRequest(ctx context.Context, pr github.DefinitionPR) {
	if err := d.checkPullRequest(ctx, pr); err != nil {
		d.log.Warn("definition pull request check", "factory_id", pr.FactoryID, "repo", pr.Repo, "pr", pr.Number, "err", err)
	}
}

// CheckRun is a check run with a title, a summary and a text, for forges
// that support check runs.
type CheckRun = core.CheckRun

// checkRunPublisher is implemented by forge clients that can publish check
// runs. Other forges get a commit status with a short description.
type checkRunPublisher interface {
	PublishCheckRun(ctx context.Context, owner, repo string, cr CheckRun) error
}

// The GitHub client publishes check runs.
var _ checkRunPublisher = (*github.Client)(nil)

// definitionCheckResult is the outcome of the definition check of one
// commit, ready to publish as a check run or a commit status.
type definitionCheckResult struct {
	conclusion string // success, failure, error
	title      string
	summary    string
}

// checkFiles runs the compiler and the preflight on files for factory f and
// renders the result with the resource diff against the active version.
func (d *defSync) checkFiles(ctx context.Context, f *core.Factory, files map[string]string) (definitionCheckResult, error) {
	chk, err := d.engine.CheckDefinition(ctx, f.ID, files)
	if err != nil {
		return definitionCheckResult{}, err
	}
	var b strings.Builder
	writeIssues := func(head string, list []core.ValidationError) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s:\n", head)
		for _, e := range list {
			fmt.Fprintf(&b, "- %s\n", issueText(e))
		}
	}
	if !chk.Valid() {
		res := definitionCheckResult{conclusion: "failure", title: fmt.Sprintf("%d error(s)", len(chk.Errors))}
		if len(chk.Errors) > 0 {
			res.title += ": " + issueText(chk.Errors[0])
		}
		writeIssues("Errors", chk.Errors)
		writeIssues("Warnings", chk.Warnings)
		res.summary = strings.TrimSpace(b.String())
		return res, nil
	}
	var old *core.CompiledDefinition
	if f.ActiveVersionID != "" {
		if v, err := d.store.GetDefinitionVersion(ctx, f.ActiveVersionID); err == nil {
			old, _ = definition.Compile(v.Files)
		}
	}
	diff := definition.Diff(old, chk.Definition)
	res := definitionCheckResult{conclusion: "success", title: "Definition is valid"}
	if diff.Empty() {
		res.title += "; no resource changes"
	} else {
		a, r, c := diff.Counts()
		res.title += fmt.Sprintf(": %d added, %d removed, %d changed", a, r, c)
	}
	b.WriteString(diff.Summary())
	writeIssues("Warnings", chk.Warnings)
	if len(chk.Skipped) > 0 {
		b.WriteString("\nChecks not done:\n")
		for _, sk := range chk.Skipped {
			fmt.Fprintf(&b, "- %s: %s\n", sk.Check, sk.Reason)
		}
	}
	res.summary = strings.TrimSpace(b.String())
	return res, nil
}

func issueText(e core.ValidationError) string {
	loc := e.File
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", e.File, e.Line)
	}
	if e.Field != "" {
		loc += " " + e.Field
	}
	return loc + ": " + e.Message
}

// CheckBranch makes sure that the branch of a git source compiles and passes
// the preflight. It is called before a factory changes its source to git.
func (d *defSync) CheckBranch(ctx context.Context, f *core.Factory) error {
	loc, err := ParseGitRepo(f.GitRepo)
	if err != nil {
		return err
	}
	fg, err := d.forges.Forge(ctx, core.ForgeLookup{TeamID: f.TeamID, FactoryID: f.ID, CodeForge: loc.CodeForge, Repo: loc.Owner + "/" + loc.Repo})
	if err != nil {
		return err
	}
	files, _, err := fg.GetFiles(ctx, loc.Owner, loc.Repo, branchOf(f), f.GitPath)
	if err != nil {
		return fmt.Errorf("%w: could not read the definition from %s branch %q: %w", core.ErrInvalid, f.GitRepo, branchOf(f), err)
	}
	chk, err := d.engine.CheckDefinition(ctx, f.ID, files)
	if err != nil {
		return err
	}
	if !chk.Valid() {
		return &BranchInvalidError{Errors: chk.Errors}
	}
	return nil
}

// BranchInvalidError reports that the definition on a git branch has errors.
type BranchInvalidError struct{ Errors []core.ValidationError }

func (e *BranchInvalidError) Error() string {
	msg := fmt.Sprintf("the definition on the branch has %d error(s)", len(e.Errors))
	if len(e.Errors) > 0 {
		msg += ": " + issueText(e.Errors[0])
	}
	return msg
}

// ValidationErrors returns the errors of the definition.
func (e *BranchInvalidError) ValidationErrors() []core.ValidationError { return e.Errors }

// Unwrap makes the error match core.ErrInvalid.
func (e *BranchInvalidError) Unwrap() error { return core.ErrInvalid }

func (d *defSync) checkPullRequest(ctx context.Context, pr github.DefinitionPR) error {
	f, err := d.store.GetFactory(ctx, pr.FactoryID)
	if err != nil {
		return err
	}
	fg, err := d.forges.Forge(ctx, core.ForgeLookup{TeamID: f.TeamID, FactoryID: f.ID, CodeForge: "GITHUB", Repo: pr.Repo})
	if err != nil {
		return err
	}
	owner, name := forgeutil.SplitRepo(pr.Repo)
	checkName := pr.CheckName
	if checkName == "" {
		checkName = github.CheckName(f.GitPath)
	}
	target := links.Factory(d.publicURL, f.ID)
	crp, _ := fg.(checkRunPublisher)
	publish := func(res definitionCheckResult) error {
		if crp != nil {
			cr := CheckRun{Name: checkName, HeadSHA: pr.HeadSHA, Status: "completed", Conclusion: res.conclusion,
				Title: truncateDesc(res.title), Summary: res.summary, DetailsURL: target}
			if res.conclusion == "error" {
				cr.Conclusion = "failure"
			}
			return crp.PublishCheckRun(ctx, owner, name, cr)
		}
		return fg.SetCommitStatus(ctx, owner, name, pr.HeadSHA, core.CommitStatus{Context: checkName, State: res.conclusion, Description: truncateDesc(res.title), TargetURL: target})
	}
	if crp != nil {
		_ = crp.PublishCheckRun(ctx, owner, name, CheckRun{Name: checkName, HeadSHA: pr.HeadSHA, Status: "in_progress", Title: "Validating factory definition", DetailsURL: target})
	} else {
		_ = fg.SetCommitStatus(ctx, owner, name, pr.HeadSHA, core.CommitStatus{Context: checkName, State: "pending", Description: "Validating factory definition", TargetURL: target})
	}

	files, _, err := fg.GetFiles(ctx, owner, name, pr.HeadSHA, f.GitPath)
	if err != nil {
		return publish(definitionCheckResult{conclusion: "error", title: "Could not read the definition: " + err.Error()})
	}
	res, err := d.checkFiles(ctx, f, files)
	if err != nil {
		return publish(definitionCheckResult{conclusion: "error", title: "Could not check the definition: " + err.Error()})
	}
	if res.conclusion == "failure" {
		d.log.Info("definition pull request check failed", "factory_id", f.ID, "pr", pr.Number, "title", res.title)
	}
	return publish(res)
}

func truncateDesc(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= 140 {
		return s
	}
	cut := 137
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}
