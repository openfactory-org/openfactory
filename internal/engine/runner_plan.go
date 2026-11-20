package engine

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/cloudid"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/prompts"
)

// runnerGitCredentialTTL is the life that the control plane reports for a git
// credential. Forge app tokens last one hour, so the runner asks again after
// 50 minutes.
const runnerGitCredentialTTL = 50 * time.Minute

// RunnerRepo is one repository of a runner session. CloneURL has no credentials.
type RunnerRepo struct{ Forge, Owner, Name, Path, Ref, CloneURL string }

// RunnerPlan is the content of a runner session.
type RunnerPlan struct {
	Env         map[string]string // no secret values
	SecretEnv   map[string]string // secret values of r.Config.Secrets
	HarnessAuth map[string]string
	// Credential tells where HarnessAuth comes from. It holds no value.
	Credential   HarnessCredential
	Repos        []RunnerRepo
	WorkDir      string       // relative to the workspace; "" or the single repository path
	Skills       []core.Skill // merged factory and agent skills
	SystemPrompt string
	MCPServers   map[string]core.MCPServer // resolved: secret placeholders are expanded
	// SessionTokenServers names the servers of the control plane. The runner
	// sends its session token to them, so they have no Authorization header.
	SessionTokenServers map[string]bool
	SetupCommands       []string
	// EnvSetupCommands are the setup_commands of the run's environment. They
	// run after SetupCommands.
	EnvSetupCommands []string
	// Cloud is the cloud identity of the factory, without tokens. Nil means none.
	Cloud *cloudid.Setup
}

// RunnerPlan computes the session content of a run for the runner path.
func (x *Planner) RunnerPlan(ctx context.Context, r *core.Run) (*RunnerPlan, error) {
	var wi *core.WorkItem
	if r.WorkItemID != "" {
		var err error
		if wi, err = x.store.GetWorkItem(ctx, r.WorkItemID); err != nil {
			return nil, fmt.Errorf("load work item: %w", err)
		}
	}
	rt, err := LoadFactoryRuntime(ctx, x.store, r.FactoryID)
	if err != nil {
		return nil, err
	}
	env, err := x.runEnv(ctx, r)
	if err != nil {
		return nil, err
	}
	auth, cred, err := x.resolveHarnessAuth(ctx, r, env)
	if err != nil {
		return nil, err
	}
	mcp, err := x.mcpServers(ctx, r)
	if err != nil {
		return nil, err
	}
	sessionServers := x.sessionTokenServers(mcp)

	repos, err := x.runnerRepos(ctx, r, rt)
	if err != nil {
		return nil, err
	}
	agents, err := x.store.ListAgents(ctx, r.FactoryID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	// The runner writes the skills to .openfactory/skills/<name>, so the
	// skill paths stay unset here.
	cfg := r.Config
	cfg.Skills = x.runSkills(ctx, r, rt, wi)

	p := &RunnerPlan{
		Env:          map[string]string{},
		SecretEnv:    map[string]string{},
		HarnessAuth:  auth,
		Credential:   cred,
		Repos:        repos,
		Skills:       cfg.Skills,
		SystemPrompt: prompts.Compose(cfg, wi, r, agents),
		MCPServers:   mcp,

		SessionTokenServers: sessionServers,
		Cloud:               cloudid.SetupFor(rt.CloudProviders),
	}
	secretNames := map[string]bool{}
	for _, n := range r.Config.Secrets {
		secretNames[n] = true
	}
	for k, v := range env {
		if secretNames[k] {
			p.SecretEnv[k] = v
		} else {
			p.Env[k] = v
		}
	}
	if paths := x.repoPaths(rt, r); len(paths) == 1 {
		p.WorkDir = paths[0]
	}
	if r.Config.Machine != nil {
		p.SetupCommands = slices.Clone(r.Config.Machine.SetupCommands)
	}
	if r.Config.EnvironmentID != "" {
		envr, err := teamEnvironment(ctx, x.store, r.TeamID, r.Config.EnvironmentID)
		if err != nil {
			return nil, fmt.Errorf("environment %q: %w", r.Config.EnvironmentID, err)
		}
		p.EnvSetupCommands = slices.Clone(envr.SetupCommands)
	}
	return p, nil
}

// LogHarnessCredential logs the default model credential of a run with the
// secret name only, and warns when more than one secret fits. The runner
// API calls it once for each secrets read of a session.
func LogHarnessCredential(ctx context.Context, log *slog.Logger, r *core.Run, c HarnessCredential) {
	switch {
	case c.IsDefault():
		log.InfoContext(ctx, "model credential: default secret", "run_id", r.ID, "harness", r.Config.Harness.Type,
			"secret", c.SecretName, "scope", c.Scope, "type", c.Type)
	case c.Reason == CredentialReasonAmbiguous:
		log.WarnContext(ctx, "model credential: more than one secret fits, none is used; set harness.auth", "run_id", r.ID,
			"harness", r.Config.Harness.Type, "scope", c.Scope, "candidates", strings.Join(c.Candidates, ","))
	}
}

// sessionTokenServers finds the servers that point at the control plane. It
// removes their Authorization header, which holds the old run token, and
// returns their names.
func (x *Planner) sessionTokenServers(mcp map[string]core.MCPServer) map[string]bool {
	out := map[string]bool{}
	if x.opts.SandboxURL == "" {
		return out
	}
	prefix := x.opts.SandboxURL + "/mcp/"
	for name, srv := range mcp {
		if !strings.HasPrefix(srv.URL, prefix) {
			continue
		}
		headers := make(map[string]string, len(srv.Headers))
		for k, v := range srv.Headers {
			if !strings.EqualFold(k, "Authorization") {
				headers[k] = v
			}
		}
		srv.Headers = headers
		mcp[name] = srv
		out[name] = true
	}
	return out
}

// runnerRepos returns the repositories of the run with clean clone URLs.
func (x *Planner) runnerRepos(ctx context.Context, r *core.Run, rt *FactoryRuntime) ([]RunnerRepo, error) {
	repos, err := x.checkouts(ctx, r, rt)
	if err != nil {
		return nil, err
	}
	recordStartCommits(ctx, x.store, r.ID, repos)
	out := make([]RunnerRepo, 0, len(repos))
	for _, c := range repos {
		clean, _, err := splitCloneURL(c.CloneURL)
		if err != nil {
			return nil, fmt.Errorf("repository %s/%s: %w", c.Owner, c.Name, err)
		}
		out = append(out, RunnerRepo{Forge: c.Forge, Owner: c.Owner, Name: c.Name, Path: c.Path, Ref: c.Ref, CloneURL: clean})
	}
	return out, nil
}

// splitCloneURL returns the clone URL without user info, and the user info.
func splitCloneURL(raw string) (clean string, user *url.Userinfo, err error) {
	u, perr := url.Parse(raw)
	if perr != nil {
		if strings.Contains(raw, "@") {
			return "", nil, fmt.Errorf("clone URL cannot be parsed and may hold credentials")
		}
		return raw, nil, nil
	}
	user = u.User
	u.User = nil
	return u.String(), user, nil
}

// RunnerGitCredential returns the git credential for one repository of the run.
// It returns core.ErrForbidden when host and path name no repository of the run.
func (x *Planner) RunnerGitCredential(ctx context.Context, r *core.Run, host, path string) (username, password string, expires time.Time, err error) {
	rt, err := LoadFactoryRuntime(ctx, x.store, r.FactoryID)
	if err != nil {
		return "", "", time.Time{}, err
	}
	repos, err := x.checkouts(ctx, r, rt)
	if err != nil {
		return "", "", time.Time{}, err
	}
	want := strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	for _, c := range repos {
		u, perr := url.Parse(c.CloneURL)
		if perr != nil {
			continue
		}
		got := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
		if !strings.EqualFold(u.Host, host) || got != want {
			continue
		}
		if u.User == nil {
			return "", "", time.Time{}, fmt.Errorf("%w: the forge gave no credential for %s", core.ErrForbidden, got)
		}
		pw, _ := u.User.Password()
		if pw == "" {
			return "", "", time.Time{}, fmt.Errorf("%w: the forge gave no credential for %s", core.ErrForbidden, got)
		}
		return u.User.Username(), pw, x.now().Add(runnerGitCredentialTTL), nil
	}
	return "", "", time.Time{}, fmt.Errorf("%w: %s/%s is not a repository of this run", core.ErrForbidden, host, want)
}
