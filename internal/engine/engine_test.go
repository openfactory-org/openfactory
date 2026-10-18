package engine

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestValidateMetadata(t *testing.T) {
	many := map[string]string{}
	for i := 0; i < 21; i++ {
		many[string(rune('a'+i))] = "v"
	}
	tests := []struct {
		name string
		md   map[string]string
		ok   bool
	}{
		{"nil", nil, true},
		{"valid", map[string]string{"setup_profile": "backend", "a.b-c": ""}, true},
		{"too many keys", many, false},
		{"empty key", map[string]string{"": "x"}, false},
		{"long key", map[string]string{strings.Repeat("k", 65): "x"}, false},
		{"bad char", map[string]string{"a b": "x"}, false},
		{"long value", map[string]string{"k": strings.Repeat("v", 257)}, false},
		{"max value", map[string]string{"k": strings.Repeat("v", 256)}, true},
		{"nul", map[string]string{"k": "a\x00b"}, false},
		{"invalid utf8", map[string]string{"k": "\xff"}, false},
		{"env collision", map[string]string{"a.b": "1", "a-b": "2"}, false},
		{"case collision", map[string]string{"Key": "1", "key": "2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateMetadata(tt.md)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidateMetadata() err=%v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("error should wrap ErrInvalid: %v", err)
			}
		})
	}
}

func TestMergeMetadataPrecedence(t *testing.T) {
	got, err := MergeMetadata(
		map[string]string{"a": "parent", "b": "parent", "c": "parent"},
		map[string]string{"b": "batch", "c": "batch"},
		map[string]string{"c": "child"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "parent", "b": "batch", "c": "child"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if MetadataEnvName("setup.profile-x") != "OF_METADATA_SETUP_PROFILE_X" {
		t.Errorf("unexpected env name %s", MetadataEnvName("setup.profile-x"))
	}
	if env := MetadataEnv(got); env["OF_METADATA_C"] != "child" {
		t.Errorf("MetadataEnv = %v", env)
	}
}

func TestApplyOverrides(t *testing.T) {
	base := core.AgentConfig{
		Name:       "implement",
		Harness:    core.HarnessConfig{Type: core.HarnessClaude, Model: "m1"},
		Secrets:    []string{"A"},
		MCPServers: map[string]core.MCPServer{"x": {URL: "http://x"}},
	}
	machines := map[string]core.MachineSpec{"big": {OS: "linux", VCPUs: 8}}
	env := "env_1"
	host := "wrk_1"
	idle := 30
	bad := 61
	tests := []struct {
		name    string
		ov      *core.AgentOverrides
		wantErr bool
		check   func(t *testing.T, c core.AgentConfig)
	}{
		{"nil", nil, false, func(t *testing.T, c core.AgentConfig) {
			if c.Harness.Model != "m1" {
				t.Error("model changed")
			}
		}},
		{"model", &core.AgentOverrides{Model: "m2"}, false, func(t *testing.T, c core.AgentConfig) {
			if c.Harness.Model != "m2" || c.Harness.Type != core.HarnessClaude {
				t.Errorf("harness = %+v", c.Harness)
			}
		}},
		{"harness with type replaces the whole harness", &core.AgentOverrides{Harness: &core.HarnessConfig{Type: core.HarnessCodex}}, false, func(t *testing.T, c core.AgentConfig) {
			if c.Harness.Type != core.HarnessCodex || c.Harness.Model != "" || c.Harness.Auth != nil {
				t.Errorf("harness = %+v", c.Harness)
			}
		}},
		{"partial harness without type", &core.AgentOverrides{Harness: &core.HarnessConfig{Model: "m9"}}, false, func(t *testing.T, c core.AgentConfig) {
			if c.Harness.Type != core.HarnessClaude || c.Harness.Model != "m9" {
				t.Errorf("harness = %+v", c.Harness)
			}
		}},
		{"model and harness", &core.AgentOverrides{Model: "m", Harness: &core.HarnessConfig{}}, true, nil},
		{"machine", &core.AgentOverrides{Machine: "big"}, false, func(t *testing.T, c core.AgentConfig) {
			if c.Machine == nil || c.Machine.VCPUs != 8 || c.Machine.Name != "big" {
				t.Errorf("machine = %+v", c.Machine)
			}
		}},
		{"unknown machine", &core.AgentOverrides{Machine: "nope"}, true, nil},
		{"env host idle secrets mcp", &core.AgentOverrides{EnvironmentID: &env, WorkerHost: &host, IdleTimeoutMinutes: &idle, Secrets: []string{"A", "B"}, MCPServers: map[string]core.MCPServer{"y": {Command: "y"}}}, false, func(t *testing.T, c core.AgentConfig) {
			if c.EnvironmentID != env || c.WorkerHost != host || c.IdleTimeoutMinutes != 30 {
				t.Errorf("cfg = %+v", c)
			}
			// secrets / mcpServers add to the agent's lists.
			if !slices.Equal(c.Secrets, []string{"A", "B"}) || len(c.MCPServers) != 2 || c.MCPServers["y"].Command != "y" || c.MCPServers["x"].URL != "http://x" {
				t.Errorf("secrets=%v mcp=%v", c.Secrets, c.MCPServers)
			}
		}},
		{"add secrets and mcp", &core.AgentOverrides{Secrets: []string{"B", "A", "B"}, MCPServers: map[string]core.MCPServer{"x": {URL: "http://x2"}, "z": {URL: "http://z"}}}, false, func(t *testing.T, c core.AgentConfig) {
			// Union without duplicates, in a stable order: the agent's list first.
			if !slices.Equal(c.Secrets, []string{"A", "B"}) {
				t.Errorf("secrets=%v", c.Secrets)
			}
			// A server with the same name is replaced by the override.
			if len(c.MCPServers) != 2 || c.MCPServers["x"].URL != "http://x2" || c.MCPServers["z"].URL != "http://z" {
				t.Errorf("mcp=%v", c.MCPServers)
			}
		}},
		{"empty lists remove nothing", &core.AgentOverrides{Secrets: []string{}, MCPServers: map[string]core.MCPServer{}}, false, func(t *testing.T, c core.AgentConfig) {
			if !slices.Equal(c.Secrets, []string{"A"}) || len(c.MCPServers) != 1 {
				t.Errorf("secrets=%v mcp=%v", c.Secrets, c.MCPServers)
			}
		}},
		{"unset lists keep agent", &core.AgentOverrides{Model: "m2"}, false, func(t *testing.T, c core.AgentConfig) {
			if !slices.Equal(c.Secrets, []string{"A"}) || len(c.MCPServers) != 1 {
				t.Errorf("secrets=%v mcp=%v", c.Secrets, c.MCPServers)
			}
		}},
		{"idle out of range", &core.AgentOverrides{IdleTimeoutMinutes: &bad}, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ApplyOverrides(base, tt.ov, machines)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
	if len(base.Secrets) != 1 || len(base.MCPServers) != 1 {
		t.Fatal("ApplyOverrides mutated its input")
	}
}

func TestCanSpawn(t *testing.T) {
	tests := []struct {
		name       string
		target     core.AgentConfig
		caller     string
		callerType core.AgentType
		want       bool
	}{
		{"nil allows foreman", core.AgentConfig{Type: core.AgentTriage}, "foreman", core.AgentForeman, true},
		{"nil denies others", core.AgentConfig{Type: core.AgentTriage}, "implement", core.AgentImplement, false},
		{"empty denies all", core.AgentConfig{Type: core.AgentTriage, SpawnableBy: []string{}}, "foreman", core.AgentForeman, false},
		{"explicit name", core.AgentConfig{Type: core.AgentCustom, SpawnableBy: []string{"implement"}}, "implement", core.AgentImplement, true},
		{"explicit excludes foreman", core.AgentConfig{Type: core.AgentCustom, SpawnableBy: []string{"implement"}}, "foreman", core.AgentForeman, false},
		{"never foreman", core.AgentConfig{Type: core.AgentForeman, SpawnableBy: []string{"foreman"}}, "foreman", core.AgentForeman, false},
	}
	for _, tt := range tests {
		if got := CanSpawn(tt.target, tt.caller, tt.callerType); got != tt.want {
			t.Errorf("%s: CanSpawn = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParseDecision(t *testing.T) {
	tests := []struct {
		text, want string
		ok         bool
	}{
		{"approve", "approved", true},
		{"@payments approve, looks good", "approved", true},
		{"<@U123> LGTM!", "approved", true},
		{"payments ship it", "approved", true},
		{"Approved.", "approved", true},
		{"yes", "approved", true},
		{"reject", "rejected", true},
		{"Request changes: make the field required", "rejected", true},
		{"no", "rejected", true},
		{"no idea, what do you think about X?", "", false},
		{"yes but also please make it required", "", false},
		{"approvers should be admins", "", false},
		{"", "", false},
		{"👍", "approved", true},
	}
	for _, tt := range tests {
		got, ok := ParseDecision(tt.text, "payments")
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseDecision(%q) = %q,%v want %q,%v", tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

func TestApplyDefinition(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	f := env.Factory
	if f.Alias != "payments" || f.Description != "Payments factory" || f.ActiveVersionID == "" {
		t.Fatalf("factory not updated: %+v", f)
	}
	fm := env.Agent(t, "foreman")
	if fm.ID != StableID("agt", f.ID, "foreman") || !strings.HasPrefix(fm.ID, "agt_") || len(fm.ID) != 24 {
		t.Fatalf("unexpected agent id %q", fm.ID)
	}
	autos, _ := env.Store.ListAutomations(ctx, f.ID)
	if len(autos) != 1 || autos[0].Agent != "foreman" || autos[0].FactoryID != f.ID || !strings.HasPrefix(autos[0].ID, "aut_") {
		t.Fatalf("automations = %+v", autos)
	}
	integ, _ := env.Store.GetFactoryIntegrations(ctx, f.ID)
	if integ["slack"]["channel"] != "C1" {
		t.Fatalf("integrations = %v", integ)
	}
	rt, err := LoadFactoryRuntime(ctx, env.Store, f.ID)
	if err != nil || len(rt.Repositories) != 1 || rt.Machines["big"].VCPUs != 8 || rt.VersionID != f.ActiveVersionID {
		t.Fatalf("runtime = %+v err=%v", rt, err)
	}
	if len(env.Bus.Records(core.TopicDefinition)) != 1 {
		t.Fatal("definition applied event not published")
	}

	// Re-apply keeps identities stable.
	v2, err := env.Engine.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok2"}, "a", "m", "sha")
	if err != nil || !v2.Valid || v2.AppliedAt == nil {
		t.Fatalf("reapply: %v %+v", err, v2)
	}
	if env.Agent(t, "foreman").ID != fm.ID {
		t.Fatal("agent id changed across applies")
	}

	// Invalid definitions are stored with errors and keep the last valid version.
	v3, err := env.Engine.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "invalid"}, "a", "bad", "")
	if !errors.Is(err, core.ErrInvalid) || v3 == nil || v3.Valid || len(v3.Errors) != 1 {
		t.Fatalf("invalid apply: err=%v v=%+v", err, v3)
	}
	stored, _ := env.Store.GetDefinitionVersion(ctx, v3.ID)
	if stored == nil || stored.Valid {
		t.Fatal("invalid version not stored")
	}
	cur, _ := env.Store.GetFactory(ctx, f.ID)
	if cur.ActiveVersionID != v2.ID {
		t.Fatalf("active version = %s, want %s", cur.ActiveVersionID, v2.ID)
	}
}

func TestApplyDefinitionAliasConflict(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	other := &core.Factory{ID: "fac_2", TeamID: "team1", Name: "other"}
	_ = env.Store.CreateFactory(ctx, other)
	def := DefaultTestDefinition()
	def.Alias = "payments"
	env.Engine.compile = CompilerFor(def)
	v, err := env.Engine.ApplyDefinition(ctx, other.ID, map[string]string{"factory.yaml": "x"}, "", "", "")
	if !errors.Is(err, core.ErrInvalid) || v.Valid || v.Errors[0].Field != "alias" {
		t.Fatalf("want alias conflict, got err=%v v=%+v", err, v)
	}
}

func TestDispatchCreatesWorkItemAndReusesIt(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	src := core.SourceRef{Provider: "slack", Kind: "thread", Channel: "C1", ThreadID: "123.4", Title: "Fix login"}
	wi, run, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "Fix the login bug", Source: src, Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if wi.Stage != core.StageIntake || wi.RootRunID != run.ID || wi.SourceKey != src.Key() || wi.Title != "Fix login" {
		t.Fatalf("work item = %+v", wi)
	}
	if run.State != core.RunQueued || run.AgentName != "foreman" || run.Config.Type != core.AgentForeman || run.Metadata["k"] != "v" {
		t.Fatalf("run = %+v", run)
	}
	msgs := env.Store.MessagesOf(run.ID)
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "Fix the login bug" {
		t.Fatalf("messages = %+v", msgs)
	}
	// Same source: continues the existing conversation.
	wi2, run2, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "Also the logout bug", Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if wi2.ID != wi.ID || run2.ID != run.ID {
		t.Fatalf("expected reuse; got wi=%s run=%s", wi2.ID, run2.ID)
	}
	if n := len(env.Store.MessagesOf(run.ID)); n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
	// Validation.
	if _, _, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("empty prompt: %v", err)
	}
	if _, _, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "x", AgentName: "ghost"}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown agent: %v", err)
	}
	if _, _, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "x", Metadata: map[string]string{"bad key": "v"}}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad metadata: %v", err)
	}
}

func TestHandleEventRouting(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()

	ev := IssueEvent("issue_labeled", "Invoices omit the PO number", "alice")
	ev.DeliveryID = "d1"
	runs, err := env.Engine.HandleEvent(ctx, ev)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	root := runs[0]
	if !strings.Contains(root.Prompt, "Coordinate this issue") || !strings.Contains(root.Prompt, "Invoices omit the PO number") || !strings.HasPrefix(root.TriggeredBy, "automation:") {
		t.Fatalf("prompt = %q triggered=%s", root.Prompt, root.TriggeredBy)
	}

	// Duplicate delivery is ignored.
	dup := IssueEvent("issue_labeled", "again", "alice")
	dup.DeliveryID = "d1"
	if runs, err := env.Engine.HandleEvent(ctx, dup); err != nil || len(runs) != 0 {
		t.Fatalf("duplicate delivery started runs=%v err=%v", runs, err)
	}

	// A comment on the same issue continues the work item.
	c := IssueEvent("issue_commented", "It should be optional", "bob")
	runs, err = env.Engine.HandleEvent(ctx, c)
	if err != nil || len(runs) != 1 || runs[0].ID != root.ID {
		t.Fatalf("follow-up routing: runs=%v err=%v", runs, err)
	}
	msgs := env.Store.MessagesOf(root.ID)
	if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, "It should be optional") || !strings.Contains(last.Content, "[follow-up]") {
		t.Fatalf("follow-up message = %q", last.Content)
	}

	// Comments of the product's own bot are dropped.
	b := IssueEvent("issue_commented", "factory bot says hi", "factory[bot]")
	b.Actor.IsBot, b.Actor.IsSelf = true, true
	if runs, _ := env.Engine.HandleEvent(ctx, b); len(runs) != 0 {
		t.Fatal("own bot comment should be ignored")
	}
	// Other bots are delivered with a bot mark.
	ob := IssueEvent("issue_commented", "coverage dropped by 2%", "coverage[bot]")
	ob.Actor.IsBot, ob.Actor.UserID = true, ""
	if runs, _ := env.Engine.HandleEvent(ctx, ob); len(runs) != 1 {
		t.Fatal("other bot comment should be delivered")
	}
	msgs = env.Store.MessagesOf(root.ID)
	if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, "a bot, not a linked team member") {
		t.Fatalf("bot message = %q", last.Content)
	}

	// Events for repositories outside the factory don't match.
	o := IssueEvent("issue_labeled", "x", "alice")
	o.Source.Repo, o.Source.Number = "acme/other", 1
	o.Attributes["repo"] = "acme/other"
	if runs, _ := env.Engine.HandleEvent(ctx, o); len(runs) != 0 {
		t.Fatal("foreign repo should not match")
	}
}

func TestHandleEventMention(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	ev := &core.Event{
		ID: "e1", TeamID: "team1", Provider: "slack", Type: "app_mention",
		Actor:      core.Actor{Provider: "slack", Handle: "carol", UserID: "usr_carol"},
		Source:     core.SourceRef{Provider: "slack", Kind: "thread", Channel: "C1", ThreadID: "1.1"},
		Attributes: map[string]any{"alias": "@payments"},
		Text:       "@payments the checkout page is slow",
	}
	runs, err := env.Engine.HandleEvent(ctx, ev)
	if err != nil || len(runs) != 1 || runs[0].AgentName != "foreman" || runs[0].TriggeredBy != "mention" {
		t.Fatalf("runs=%+v err=%v", runs, err)
	}
	wi, _ := env.Store.GetWorkItem(ctx, runs[0].WorkItemID)
	if wi.CreatorID != "usr_carol" {
		t.Fatalf("creator = %q", wi.CreatorID)
	}
	// Slack events for factories without the slack integration are not routed.
	env2 := NewTestEnv(t, func(d *core.CompiledDefinition) { d.Integrations = nil })
	if runs, _ := env2.Engine.HandleEvent(ctx, ev); len(runs) != 0 {
		t.Fatal("factory without slack integration should not receive slack events")
	}
}

func foremanRun(t *testing.T, env *TestEnv) *core.Run {
	t.Helper()
	_, run, err := env.Engine.Dispatch(context.Background(), core.DispatchRequest{
		FactoryID: env.Factory.ID, Prompt: "work", Source: IssueEvent("x", "", "").Source, Metadata: map[string]string{"team": "core", "shared": "parent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRunAgents(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	triage := env.Agent(t, "triage")
	impl := env.Agent(t, "implement")

	runs, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{
		ParentRunID: root.ID, Summary: "triage+implement", BasePrompt: "Base.",
		Metadata: map[string]string{"shared": "batch", "b": "1"},
		AgentRunConfigs: []core.ChildRunConfig{
			{Name: "triage-1", AgentIdentityUID: triage.ID, Prompt: "Investigate."},
			{Name: "backend-implementation", AgentIdentityUID: impl.ID, Prompt: "Fix.", Metadata: map[string]string{"shared": "child", "setup_profile": "backend"}, Overrides: &core.AgentOverrides{Machine: "big"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d", len(runs))
	}
	c := runs[1]
	if c.ParentRunID != root.ID || c.WorkItemID != root.WorkItemID || c.Name != "backend-implementation" || c.AgentName != "implement" || c.Prompt != "Base.\n\nFix." {
		t.Fatalf("child = %+v", c)
	}
	if c.Metadata["shared"] != "child" || c.Metadata["team"] != "core" || c.Metadata["b"] != "1" || runs[0].Metadata["shared"] != "batch" {
		t.Fatalf("metadata = %v / %v", c.Metadata, runs[0].Metadata)
	}
	if c.Config.Machine == nil || c.Config.Machine.VCPUs != 8 {
		t.Fatalf("machine override not applied: %+v", c.Config.Machine)
	}
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageBuilding {
		t.Fatalf("stage = %s, want building (inferred from implement)", wi.Stage)
	}

	// Explicit stage wins over the next inference.
	if err := env.Engine.SetStage(ctx, wi.ID, core.StagePlanning, "run:"+root.ID, "back to planning"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "review").ID, Prompt: "r"}}}); err != nil {
		t.Fatal(err)
	}
	if wi, _ = env.Store.GetWorkItem(ctx, wi.ID); wi.Stage != core.StagePlanning {
		t.Fatalf("stage = %s, want planning (explicit)", wi.Stage)
	}
	if _, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "review").ID, Prompt: "r"}}}); err != nil {
		t.Fatal(err)
	}
	if wi, _ = env.Store.GetWorkItem(ctx, wi.ID); wi.Stage != core.StageReviewing {
		t.Fatalf("stage = %s, want reviewing", wi.Stage)
	}

	errCases := []struct {
		name string
		req  core.RunAgentsRequest
		want error
	}{
		{"no children", core.RunAgentsRequest{ParentRunID: root.ID}, core.ErrInvalid},
		{"unknown agent", core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: "agt_nope", Prompt: "x"}}}, core.ErrInvalid},
		{"no prompt", core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: triage.ID}}}, core.ErrInvalid},
		{"foreman not spawnable", core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "foreman").ID, Prompt: "x"}}}, core.ErrForbidden},
		{"bad metadata", core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: triage.ID, Prompt: "x", Metadata: map[string]string{"a.b": "1", "a_b": "2"}}}}, core.ErrInvalid},
		{"child not spawnable by child", core.RunAgentsRequest{ParentRunID: runs[0].ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: impl.ID, Prompt: "x"}}}, core.ErrForbidden},
	}
	for _, tc := range errCases {
		if _, err := env.Engine.RunAgents(ctx, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}

	// Agents from another factory are rejected.
	other := &core.Factory{ID: "fac_9", TeamID: "team1", Name: "other"}
	_ = env.Store.CreateFactory(ctx, other)
	_ = env.Store.ReplaceFactoryResources(ctx, other.ID, []core.AgentConfig{{ID: "agt_foreign", FactoryID: other.ID, Name: "x", Type: core.AgentTriage}}, nil, nil, nil)
	if _, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: "agt_foreign", Prompt: "x"}}}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("foreign agent: %v", err)
	}
}

func TestFollowUpAndCancel(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	children, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "triage").ID, Prompt: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	child := children[0]

	// A follow-up on an idle run goes to the message queue of the run. The
	// dispatcher starts the next turn, so the engine does not change the state.
	r, _ := env.Store.GetRun(ctx, child.ID)
	r.State = core.RunIdle
	_ = env.Store.UpdateRun(ctx, r)
	if err := env.Engine.FollowUp(ctx, child.ID, "revise", "run:"+root.ID); err != nil {
		t.Fatal(err)
	}
	if got := env.Route.sentTo(child.ID); len(got) != 1 || got[0].Content != "revise" || got[0].Kind != "agent" {
		t.Fatalf("queued messages = %+v", got)
	}
	if r, _ = env.Store.GetRun(ctx, child.ID); r.State != core.RunIdle {
		t.Fatalf("state = %s", r.State)
	}
	// A follow-up on a running run goes to the queue too.
	r.State = core.RunRunning
	_ = env.Store.UpdateRun(ctx, r)
	_ = env.Engine.FollowUp(ctx, child.ID, "more", "x")
	if got := env.Route.sentTo(child.ID); len(got) != 2 {
		t.Fatalf("queued messages = %+v", got)
	}
	if r, _ = env.Store.GetRun(ctx, child.ID); r.State != core.RunRunning {
		t.Fatalf("state = %s", r.State)
	}
	if err := env.Engine.FollowUp(ctx, child.ID, " ", "x"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("empty follow-up: %v", err)
	}

	// An open question on the root is cleared by cancellation.
	if _, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Q?", "body", nil); err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.Cancel(ctx, root.ID, "usr_1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root.ID, child.ID} {
		if r, _ := env.Store.GetRun(ctx, id); r.State != core.RunCancelled || r.FinishedAt == nil {
			t.Fatalf("run %s state = %s", id, r.State)
		}
	}
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageCancelled {
		t.Fatalf("work item stage = %s", wi.Stage)
	}
	if open, _ := env.Store.ListInboxItems(ctx, env.Factory.ID, false); len(open) != 0 {
		t.Fatalf("open inbox items after cancel: %+v", open)
	}
	// A message to a cancelled run continues it in a new session.
	if err := env.Engine.FollowUp(ctx, root.ID, "hi", "x"); err != nil {
		t.Fatalf("follow-up on cancelled run: %v", err)
	}
	if got := env.Route.sentTo(root.ID); len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("queued messages = %+v", got)
	}
}

func TestSetStage(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	id := root.WorkItemID
	steps := []struct {
		stage core.Stage
		want  error
	}{
		{"bogus", core.ErrInvalid},
		{core.StageTriage, nil},
		{core.StageBuilding, nil},
		{core.StagePlanning, nil}, // going back is allowed
		{core.StagePlanning, nil}, // no-op
		{core.StageHandoff, nil},
		{core.StageComplete, nil},
		{core.StageBuilding, core.ErrConflict},
	}
	for _, s := range steps {
		if err := env.Engine.SetStage(ctx, id, s.stage, "usr", "n"); !errors.Is(err, s.want) {
			t.Fatalf("SetStage(%s) = %v, want %v", s.stage, err, s.want)
		}
	}
	evs, _ := env.Store.ListWorkItemEvents(ctx, id)
	changes := 0
	for _, e := range evs {
		if e.Kind == "stage_changed" {
			changes++
		}
	}
	if changes != 6 { // intake + 5 transitions
		t.Fatalf("stage_changed events = %d", changes)
	}
}

func TestApprovalFlow(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	it, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxApproval, "Spec: PO field", "Please review the spec PR", nil)
	if err != nil {
		t.Fatal(err)
	}
	if it.WorkItemID != root.WorkItemID || len(it.Options) != 2 {
		t.Fatalf("inbox item = %+v", it)
	}
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageAwaitingApproval {
		t.Fatalf("stage = %s", wi.Stage)
	}
	notices := env.Bus.Notices()
	if len(notices) == 0 || notices[len(notices)-1].Kind != "approval_request" || notices[len(notices)-1].Source.Repo != "acme/api" {
		t.Fatalf("notices = %+v", notices)
	}

	// Approval typed in the issue resolves the inbox item and wakes the foreman.
	r, _ := env.Store.GetRun(ctx, root.ID)
	r.State = core.RunIdle
	_ = env.Store.UpdateRun(ctx, r)
	runs, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "@payments approve", "maintainer"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	got, _ := env.Store.GetInboxItem(ctx, it.ID)
	if !got.Resolved || got.Resolution != "approved" || got.ResolvedBy != "maintainer" {
		t.Fatalf("inbox = %+v", got)
	}
	msgs := env.Store.MessagesOf(root.ID)
	if last := msgs[len(msgs)-1].Content; !strings.Contains(last, "approved") || !strings.Contains(last, "Spec: PO field") {
		t.Fatalf("decision message = %q", last)
	}
	// The decision goes to the message queue of the foreman.
	if got := env.Route.sentTo(root.ID); len(got) != 1 || !strings.Contains(got[0].Content, "approved") {
		t.Fatalf("queued messages = %+v", got)
	}
	if err := env.Engine.ResolveInbox(ctx, it.ID, "again", "x"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("double resolve: %v", err)
	}

	// A reply while a question is open answers it.
	q, _ := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Required?", "Is the field required?", nil)
	if _, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "Optional, default empty.", "maintainer")); err != nil {
		t.Fatal(err)
	}
	if got, _ := env.Store.GetInboxItem(ctx, q.ID); !got.Resolved || got.Resolution != "Optional, default empty." {
		t.Fatalf("question = %+v", got)
	}
}

// A child run reports the end of each turn once, to the message queue of its
// parent, as a lifecycle message.
func TestChildCompletionIsReportedOnce(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	kids, _ := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{{Name: "impl", AgentIdentityUID: env.Agent(t, "implement").ID, Prompt: "x"}}})
	child, _ := env.Store.GetRun(ctx, kids[0].ID)
	child.State = core.RunSucceeded
	child.Turns = 1
	child.Result = "Opened https://github.com/acme/api/pull/42 with tests."
	_ = env.Store.UpdateRun(ctx, child)

	env.Engine.onRunUpdated(ctx, child)
	env.Engine.onRunUpdated(ctx, child) // duplicate notification for the same turn
	got := env.Route.sentTo(root.ID)
	if len(got) != 1 || got[0].Kind != "lifecycle" || got[0].Sender.ID != child.ID {
		t.Fatalf("lifecycle messages = %+v", got)
	}
	if !strings.Contains(got[0].Content, `"impl"`) || !strings.Contains(got[0].Content, "succeeded") {
		t.Errorf("content = %q", got[0].Content)
	}
	// A new turn that fails is reported.
	child.State = core.RunFailed
	child.Turns = 2
	child.Error = "tests failed"
	_ = env.Store.UpdateRun(ctx, child)
	env.Engine.onRunUpdated(ctx, child)
	got = env.Route.sentTo(root.ID)
	if len(got) != 2 || !strings.Contains(got[1].Content, "failed") {
		t.Fatalf("failure not reported: %+v", got)
	}
}

func TestRootFailureCreatesInboxItem(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	r, _ := env.Store.GetRun(ctx, root.ID)
	r.State, r.Error = core.RunFailed, "sandbox creation failed"
	_ = env.Store.UpdateRun(ctx, r)
	env.Engine.onRunUpdated(ctx, r)
	items, _ := env.Store.ListInboxItems(ctx, env.Factory.ID, false)
	if len(items) != 1 || items[0].Kind != core.InboxFailure {
		t.Fatalf("inbox = %+v", items)
	}
	n := env.Bus.Notices()
	if n[len(n)-1].Kind != "error" {
		t.Fatalf("notice = %+v", n[len(n)-1])
	}
}

func TestLinkPullRequestAndMerge(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 42, URL: "https://github.com/acme/api/pull/42", Branch: "fix"}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, core.PullRequestRef{URL: pr.URL, State: "draft"}); err != nil {
		t.Fatal(err)
	}
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if len(wi.PullRequests) != 1 || wi.PullRequests[0].State != "draft" || wi.PullRequests[0].Branch != "fix" {
		t.Fatalf("prs = %+v", wi.PullRequests)
	}
	// A review on the linked PR is routed to the work item.
	review := &core.Event{ID: "r1", TeamID: "team1", Provider: "github", Type: "pull_request_review_submitted",
		Actor:  core.Actor{Handle: "rev"},
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 42}, Text: "Please add a migration test"}
	runs, err := env.Engine.HandleEvent(ctx, review)
	if err != nil || len(runs) != 1 || runs[0].ID != root.ID {
		t.Fatalf("review routing runs=%v err=%v", runs, err)
	}
	// A merge gives the foreman a turn. The work item stays open.
	merged := &core.Event{ID: "m1", TeamID: "team1", Provider: "github", Type: "pull_request_merged",
		Actor:  core.Actor{Handle: "merge-queue[bot]", IsBot: true},
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 42, URL: pr.URL}}
	// The root run is idle before the merge.
	r, _ := env.Store.GetRun(ctx, root.ID)
	r.State, r.Turns = core.RunSucceeded, 1
	_ = env.Store.UpdateRun(ctx, r)
	if _, err := env.Engine.HandleEvent(ctx, merged); err != nil {
		t.Fatal(err)
	}
	wi, _ = env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage.Terminal() || wi.PullRequests[0].State != "merged" {
		t.Fatalf("work item after merge = %+v", wi)
	}
	msgs := env.Store.MessagesOf(root.ID)
	if last := msgs[len(msgs)-1].Content; !strings.Contains(last, "was merged") || !strings.Contains(last, "complete_work_item") {
		t.Fatalf("merge message = %q", last)
	}
	// The foreman's turn ends without a completion: the platform completes the work item.
	r, _ = env.Store.GetRun(ctx, root.ID)
	r.State, r.Turns = core.RunSucceeded, 2
	_ = env.Store.UpdateRun(ctx, r)
	env.Engine.onRunUpdated(ctx, r)
	wi, _ = env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageComplete || wi.CompletedAt == nil {
		t.Fatalf("work item = %+v", wi)
	}
}

// TestMergeTurnForemanCompletes checks that the fallback does nothing when
// the foreman completes the work item in its merge turn, and that a turn end
// before the merge turn does not complete the work item.
func TestMergeTurnForemanCompletes(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 43, URL: "https://github.com/acme/api/pull/43"}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
		t.Fatal(err)
	}
	r, _ := env.Store.GetRun(ctx, root.ID)
	r.State, r.Turns = core.RunRunning, 4
	_ = env.Store.UpdateRun(ctx, r)
	merged := &core.Event{ID: "m2", TeamID: "team1", Provider: "github", Type: "pull_request_merged", Actor: core.Actor{Handle: "dana"},
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 43, URL: pr.URL}}
	if _, err := env.Engine.HandleEvent(ctx, merged); err != nil {
		t.Fatal(err)
	}
	// The turn that ran during the merge ends: it did not see the message.
	r, _ = env.Store.GetRun(ctx, root.ID)
	r.State, r.Turns = core.RunBlocked, 5
	_ = env.Store.UpdateRun(ctx, r)
	env.Engine.onRunUpdated(ctx, r)
	if wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID); wi.Stage.Terminal() {
		t.Fatalf("completed before the merge turn: %+v", wi)
	}
	// In the merge turn the foreman completes the work item with its own note.
	if err := env.Engine.CompleteWorkItem(ctx, root.WorkItemID, "run:"+root.ID, "Landed; follow-up issue filed."); err != nil {
		t.Fatal(err)
	}
	r, _ = env.Store.GetRun(ctx, root.ID)
	r.State, r.Turns = core.RunBlocked, 6
	_ = env.Store.UpdateRun(ctx, r)
	env.Engine.onRunUpdated(ctx, r)
	if v, _ := env.Store.KVGet(ctx, mergeCompletionKey(root.WorkItemID)); v != "" {
		t.Fatalf("pending completion not cleared: %q", v)
	}
	evs, _ := env.Store.ListWorkItemEvents(ctx, root.WorkItemID)
	for _, e := range evs {
		if strings.Contains(string(e.Data), "platform completed it") {
			t.Fatalf("fallback ran after the foreman completed: %s", e.Data)
		}
	}
}

// TestReviewWithoutBodyReachesForeman checks that an approval without text is
// delivered to the foreman.
func TestReviewWithoutBodyReachesForeman(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 44, URL: "https://github.com/acme/api/pull/44"}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
		t.Fatal(err)
	}
	before := len(env.Store.MessagesOf(root.ID))
	ev := &core.Event{ID: "rv", TeamID: "team1", Provider: "github", Type: "pull_request_review_submitted", Actor: core.Actor{Handle: "rev"},
		Attributes: map[string]any{"review_state": "approved"},
		Source:     core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 44}}
	runs, err := env.Engine.HandleEvent(ctx, ev)
	if err != nil || len(runs) != 1 || runs[0].ID != root.ID {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	if n := len(env.Store.MessagesOf(root.ID)); n != before+1 {
		t.Fatalf("messages = %d, want %d", n, before+1)
	}
}

// GitHub sends an empty review in state "commented" with each inline comment.
// The comment is its own event, so the empty review must not start a turn.
func TestEmptyCommentedReviewDoesNotReachForeman(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 45, URL: "https://github.com/acme/api/pull/45"}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
		t.Fatal(err)
	}
	before := len(env.Store.MessagesOf(root.ID))
	ev := &core.Event{ID: "rvc", TeamID: "team1", Provider: "github", Type: "pull_request_review_submitted", Actor: core.Actor{Handle: "rev"},
		Attributes: map[string]any{"review_state": "commented"},
		Source:     core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 45}}
	runs, err := env.Engine.HandleEvent(ctx, ev)
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	if n := len(env.Store.MessagesOf(root.ID)); n != before {
		t.Fatalf("messages = %d, want %d", n, before)
	}
}

func TestPostUpdateHandoff(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	if err := env.Engine.PostUpdate(ctx, root.ID, "Triage done", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.PostUpdate(ctx, root.ID, "PR ready: https://github.com/acme/api/pull/42", "handoff"); err != nil {
		t.Fatal(err)
	}
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageHandoff {
		t.Fatalf("stage = %s", wi.Stage)
	}
	items, _ := env.Store.ListInboxItems(ctx, env.Factory.ID, false)
	if len(items) != 1 || items[0].Kind != core.InboxHandoff {
		t.Fatalf("inbox = %+v", items)
	}
	kinds := []string{}
	for _, n := range env.Bus.Notices() {
		kinds = append(kinds, n.Kind)
	}
	if strings.Join(kinds, ",") != "update,handoff" {
		t.Fatalf("notice kinds = %v", kinds)
	}
	if err := env.Engine.PostUpdate(ctx, root.ID, "", ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("empty update should be invalid")
	}
}

func TestStartLoopsHandleBusEvents(t *testing.T) {
	env := NewTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); env.Engine.Wait() }()
	if err := env.Engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	env.Bus.Publish(ctx, core.TopicInboundEvent, IssueEvent("issue_labeled", "via bus", "alice"))
	WaitFor(t, 2e9, nil, func() bool {
		runs, _ := env.Store.ListRuns(ctx, core.RunFilter{FactoryID: env.Factory.ID})
		return len(runs) == 1
	})
}

// TestOverridesAddAndFactoryWideApplies is a regression test for automation
// execution overrides: secrets/mcpServers add to the agent's lists, and
// factory-wide entries always apply. An explicit empty list survives JSON
// storage and removes nothing.
func TestOverridesAddAndFactoryWideApplies(t *testing.T) {
	agent := core.AgentConfig{Name: "implement", Secrets: []string{"AGENT_ONLY", "FW"},
		MCPServers: map[string]core.MCPServer{"agent-mcp": {URL: "http://a"}, "fw-mcp": {URL: "http://fw"}}}
	rt := &FactoryRuntime{Secrets: []string{"FW"}, MCPServers: map[string]core.MCPServer{"fw-mcp": {URL: "http://fw"}}}

	raw, err := json.Marshal(core.AgentOverrides{Secrets: []string{}, MCPServers: map[string]core.MCPServer{}})
	if err != nil {
		t.Fatal(err)
	}
	var stored core.AgentOverrides
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		ov          *core.AgentOverrides
		wantSecrets []string
		wantMCP     []string
	}{
		{"add", &core.AgentOverrides{Secrets: []string{"AUTO"}, MCPServers: map[string]core.MCPServer{"auto-mcp": {URL: "http://auto"}}}, []string{"AGENT_ONLY", "FW", "AUTO"}, []string{"agent-mcp", "auto-mcp", "fw-mcp"}},
		{"empty after storage removes nothing", &stored, []string{"AGENT_ONLY", "FW"}, []string{"agent-mcp", "fw-mcp"}},
		{"not set", &core.AgentOverrides{Model: "m"}, []string{"AGENT_ONLY", "FW"}, []string{"agent-mcp", "fw-mcp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ApplyOverrides(agent, tc.ov, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg = withFactoryWide(cfg, rt)
			if !slices.Equal(cfg.Secrets, tc.wantSecrets) {
				t.Errorf("secrets = %v, want %v", cfg.Secrets, tc.wantSecrets)
			}
			if got := slices.Sorted(maps.Keys(cfg.MCPServers)); !slices.Equal(got, tc.wantMCP) {
				t.Errorf("mcp = %v, want %v", got, tc.wantMCP)
			}
		})
	}
}

// TestFactoryCoversAzureDevOpsWorkItem is a regression test: Azure DevOps work
// item events carry the project in Source.Repo, which never equalled a
// factory repository's "project/repo" full name, so they reached no factory.
func TestFactoryCoversAzureDevOpsWorkItem(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	if err := saveFactoryRuntime(ctx, env.Store, env.Factory.ID, &FactoryRuntime{Repositories: []core.RepoRef{{CodeForge: "AZURE_DEVOPS", Owner: "payments-proj", Name: "api"}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		src  core.SourceRef
		want bool
	}{
		{"work item in project", core.SourceRef{Provider: "azure_devops", Kind: "work_item", Repo: "Payments-Proj", Number: 3}, true},
		{"work item elsewhere", core.SourceRef{Provider: "azure_devops", Kind: "work_item", Repo: "other", Number: 3}, false},
		{"pull request by repo", core.SourceRef{Provider: "azure_devops", Kind: "pull_request", Repo: "payments-proj/api", Number: 3}, true},
		{"pull request project only", core.SourceRef{Provider: "azure_devops", Kind: "pull_request", Repo: "payments-proj", Number: 3}, false},
	} {
		ev := &core.Event{Provider: "azure_devops", Type: "work_item_created", Source: tc.src}
		if got := env.Engine.factoryCovers(ctx, env.Factory, ev); got != tc.want {
			t.Errorf("%s: covers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
