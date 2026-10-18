package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestAutoFollowUpDisabled(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	runs, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_labeled", "Invoices omit the PO number", "alice"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	before := len(env.Store.MessagesOf(runs[0].ID))
	for _, v := range []any{false, "false"} {
		c := IssueEvent("issue_commented", "just chatting", "bob")
		c.Attributes["auto_follow_up"] = v
		got, err := env.Engine.HandleEvent(ctx, c)
		if err != nil || len(got) != 0 {
			t.Fatalf("auto_follow_up=%v: runs=%v err=%v", v, got, err)
		}
	}
	if n := len(env.Store.MessagesOf(runs[0].ID)); n != before {
		t.Fatalf("messages %d -> %d", before, n)
	}
	c := IssueEvent("issue_commented", "now deliver", "bob")
	c.Attributes["auto_follow_up"] = true
	if got, _ := env.Engine.HandleEvent(ctx, c); len(got) != 1 {
		t.Fatal("auto_follow_up=true should deliver")
	}
}

func TestOutboundNoticeCarriesInboxAndFactory(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	runs, _ := env.Engine.HandleEvent(ctx, IssueEvent("issue_labeled", "x", "alice"))
	it, err := env.Engine.RequestHuman(ctx, runs[0].ID, core.InboxApproval, "Spec approval", "ok?", nil)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range env.Bus.Notices() {
		if n.Kind == "approval_request" {
			found = true
			if n.InboxID != it.ID || n.FactoryID != env.Factory.ID {
				t.Fatalf("notice = %+v", n)
			}
		}
	}
	if !found {
		t.Fatal("no approval notice")
	}
}

func TestDispatchStartingRefsAndAgentPins(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	f := newPlanFixture(t, nil)
	ctx := context.Background()
	_, run, err := f.Engine.Dispatch(ctx, core.DispatchRequest{
		FactoryID: f.Factory.ID, Prompt: "trial",
		Source:           core.SourceRef{Provider: "benchmark", Kind: "task", ExternalID: "b/t1"},
		StartingRepoRefs: []core.RepoAtCommit{{CodeForge: "GITHUB", Owner: "acme", Repo: "api", Ref: sha}, {CodeForge: "GITHUB", Owner: "acme", Repo: "docs", Ref: sha}},
		AgentPins:        map[string]core.AgentOverrides{"review": {Model: "pinned-review"}, "foreman": {Model: "pinned-foreman"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Config.Harness.Model != "pinned-foreman" {
		t.Fatalf("foreman pin not applied: %q", run.Config.Harness.Model)
	}
	plan, err := f.x.RunnerPlan(ctx, f.run(t, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	repos := plan.Repos
	if len(repos) != 2 || repos[0].Name != "api" || repos[0].Ref != sha || repos[1].Name != "docs" || repos[1].Ref != sha || repos[1].CloneURL == "" {
		t.Fatalf("repos = %+v", repos)
	}
	r := f.run(t, run.ID)
	r.State = core.RunRunning
	_ = f.Store.UpdateRun(ctx, r)
	kids, err := f.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: run.ID, AgentRunConfigs: []core.ChildRunConfig{
		{AgentIdentityUID: "review", Prompt: "review", Overrides: &core.AgentOverrides{Model: "caller-choice"}},
		{AgentIdentityUID: "triage", Prompt: "triage"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if kids[0].Config.Harness.Model != "pinned-review" || kids[1].Config.Harness.Model != "claude-sonnet-4-5" {
		t.Fatalf("child models %q %q", kids[0].Config.Harness.Model, kids[1].Config.Harness.Model)
	}
}

func TestManagedMCPServers(t *testing.T) {
	f := newPlanFixture(t, nil, func(d *core.CompiledDefinition) {
		d.Agents[0].MCPServers = map[string]core.MCPServer{"tracker": {ManagedID: "linear", Headers: map[string]string{"X-Extra": "{{SENTRY_TOKEN}}"}}}
	})
	ctx := context.Background()
	registered := map[string]core.MCPServer{"linear": {URL: "https://mcp.linear.test", Headers: map[string]string{"Authorization": "Bearer x"}}}
	var resolvedFor string
	resolve := func(_ context.Context, run *core.Run, id string) (core.MCPServer, error) {
		resolvedFor = run.TeamID
		if s, ok := registered[id]; ok {
			return s, nil
		}
		return core.MCPServer{}, core.ErrNotFound
	}
	_, _ = f.Secrets.Put(ctx, "team1", "", "SENTRY_TOKEN", core.SecretGeneric, []byte("s3"))

	// Without a resolver the plan fails clearly.
	_, run, _ := f.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: f.Factory.ID, Prompt: "go"})
	if _, err := f.x.RunnerPlan(ctx, f.run(t, run.ID)); err == nil || !strings.Contains(err.Error(), `managedId "linear"`) {
		t.Fatalf("plan error = %v", err)
	}

	f.x.opts.ResolveManagedMCP = resolve
	plan, err := f.x.RunnerPlan(ctx, f.run(t, run.ID))
	if err != nil {
		t.Fatal(err)
	}
	got := plan.MCPServers["tracker"]
	if got.URL != "https://mcp.linear.test" || got.ManagedID != "" || got.Headers["Authorization"] != "Bearer x" || got.Headers["X-Extra"] != "s3" {
		t.Fatalf("tracker = %+v", got)
	}
	if resolvedFor != "team1" {
		t.Fatalf("resolver got team %q", resolvedFor)
	}
}
