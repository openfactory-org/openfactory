package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func newPlanRun(t *testing.T) (*planFixture, *core.Run) {
	t.Helper()
	f := newPlanFixture(t, nil, func(d *core.CompiledDefinition) {
		d.Agents[0].Secrets = []string{"DB_URL"}
		d.Agents[0].Skills = []core.Skill{{Name: "slack", Description: "Post to Slack", Content: "# Slack"}}
	})
	ctx := context.Background()
	_, _ = f.Secrets.Put(ctx, "team1", "", "DB_URL", core.SecretGeneric, []byte("postgres://db"))
	_, run, err := f.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: f.Factory.ID, Prompt: "Fix it", Source: IssueEvent("x", "", "").Source})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.Store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, r
}

func TestRunnerPlan(t *testing.T) {
	f, r := newPlanRun(t)
	p, err := f.x.RunnerPlan(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repos) != 1 {
		t.Fatalf("repos = %+v", p.Repos)
	}
	repo := p.Repos[0]
	if repo.CloneURL != "https://github.com/acme/api.git" || repo.Path != "api" || repo.Forge != "GITHUB" {
		t.Errorf("repo = %+v", repo)
	}
	if strings.Contains(repo.CloneURL, "@") || strings.Contains(repo.CloneURL, "tok") {
		t.Errorf("clone URL has credentials: %q", repo.CloneURL)
	}
	if p.WorkDir != "api" {
		t.Errorf("WorkDir = %q", p.WorkDir)
	}
	if p.SecretEnv["DB_URL"] != "postgres://db" {
		t.Errorf("SecretEnv = %v", p.SecretEnv)
	}
	if _, ok := p.Env["DB_URL"]; ok {
		t.Errorf("Env holds the secret: %v", p.Env)
	}
	if p.Env["OF_RUN_ID"] != r.ID {
		t.Errorf("Env = %v", p.Env)
	}
	if p.HarnessAuth["ANTHROPIC_API_KEY"] != "sk-ant-test" {
		t.Errorf("auth = %v", p.HarnessAuth)
	}
	orch, ok := p.MCPServers[OrchestrationServerName]
	if !ok {
		t.Fatalf("orchestration server is not in the plan")
	}
	if !strings.HasSuffix(orch.URL, "/mcp/run/"+r.ID) {
		t.Errorf("orchestration URL = %q", orch.URL)
	}
	if len(orch.Headers) != 0 {
		t.Errorf("orchestration headers = %v", orch.Headers)
	}
	if !p.SessionTokenServers[OrchestrationServerName] {
		t.Errorf("SessionTokenServers = %v", p.SessionTokenServers)
	}
	names := map[string]bool{}
	for _, s := range p.Skills {
		names[s.Name] = true
		if s.Path != "" {
			t.Errorf("skill %s has a path %q", s.Name, s.Path)
		}
	}
	if !names["conventions"] || !names["slack"] {
		t.Errorf("skills = %v", names)
	}
	if !strings.Contains(p.SystemPrompt, "Instructions for foreman.") {
		t.Errorf("system prompt = %q", p.SystemPrompt)
	}
}

func TestRunnerGitCredential(t *testing.T) {
	f, r := newPlanRun(t)
	ctx := context.Background()
	for _, tc := range []struct{ host, path string }{
		{"github.com", "acme/api"},
		{"github.com", "acme/api.git"},
		{"GitHub.com", "acme/api"},
	} {
		user, pw, exp, err := f.x.RunnerGitCredential(ctx, r, tc.host, tc.path)
		if err != nil {
			t.Fatalf("%v: %v", tc, err)
		}
		if user != "x-access-token" || pw != "tok" || !exp.After(f.x.now()) {
			t.Errorf("%v: got %q %q %v", tc, user, pw, exp)
		}
	}
	for _, tc := range []struct{ host, path string }{
		{"github.com", "acme/other"},
		{"evil.test", "acme/api"},
		{"github.com", ""},
	} {
		_, _, _, err := f.x.RunnerGitCredential(ctx, r, tc.host, tc.path)
		if !errors.Is(err, core.ErrForbidden) {
			t.Errorf("%v: err = %v, want forbidden", tc, err)
		}
	}
}

func TestSplitCloneURL(t *testing.T) {
	got, user, err := splitCloneURL("https://u:p@host.test/a/b.git")
	if err != nil || got != "https://host.test/a/b.git" || user == nil {
		t.Errorf("got %q %v %v", got, user, err)
	}
	if got, _, err := splitCloneURL("git@host:a/b"); err == nil {
		t.Errorf("got %q, want error", got)
	}
	if got, _, err := splitCloneURL("ssh-host:a/b"); err != nil || got != "ssh-host:a/b" {
		t.Errorf("got %q %v", got, err)
	}
}

func TestSessionTokenServers(t *testing.T) {
	f, _ := newPlanRun(t)
	base := f.x.opts.SandboxURL
	mcp := map[string]core.MCPServer{
		"proxied": {URL: base + "/mcp/proxy/run_1/srv", Headers: map[string]string{"authorization": "Bearer old", "X-Team": "a"}},
		"builtin": {URL: base + "/mcp/integrations/linear/run_1", Headers: map[string]string{"Authorization": "Bearer old"}},
		"outside": {URL: "https://example.test/mcp/x", Headers: map[string]string{"Authorization": "Bearer keep"}},
		"command": {Command: "tool"},
	}
	got := f.x.sessionTokenServers(mcp)
	if len(got) != 2 || !got["proxied"] || !got["builtin"] {
		t.Fatalf("marked = %v", got)
	}
	if h := mcp["proxied"].Headers; len(h) != 1 || h["X-Team"] != "a" {
		t.Errorf("proxied headers = %v", h)
	}
	if len(mcp["builtin"].Headers) != 0 {
		t.Errorf("builtin headers = %v", mcp["builtin"].Headers)
	}
	if mcp["outside"].Headers["Authorization"] != "Bearer keep" {
		t.Errorf("outside headers = %v", mcp["outside"].Headers)
	}
}

// The environment's setup_commands come after the machine's (docs:
// machines-and-environments.md, step 4).
func TestRunnerPlanEnvironmentSetup(t *testing.T) {
	f, r := newPlanRun(t)
	ctx := context.Background()
	env := &core.Environment{ID: "env_setup", TeamID: r.TeamID, Name: "setup", Repos: []string{"github:acme/api"}, SetupCommands: []string{"make deps", "make seed"}}
	if err := f.Store.CreateEnvironment(ctx, env); err != nil {
		t.Fatal(err)
	}
	r.Config.EnvironmentID = env.ID
	r.Config.Machine = &core.MachineSpec{SetupCommands: []string{"echo runner"}}
	p, err := f.x.RunnerPlan(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(p.SetupCommands, ";") != "echo runner" || strings.Join(p.EnvSetupCommands, ";") != "make deps;make seed" {
		t.Fatalf("setup = %q, environment setup = %q", p.SetupCommands, p.EnvSetupCommands)
	}
}
