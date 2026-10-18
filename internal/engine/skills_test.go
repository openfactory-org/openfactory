package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func skillNames(list []core.Skill) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name+"="+s.Scope)
	}
	return strings.Join(out, ",")
}

// TestRunSkillsOrder checks the order: agent skills, factory skills, then built-in
// skills picked by the agent type, the forges and the integrations.
func TestRunSkillsOrder(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	rt, err := LoadFactoryRuntime(ctx, env.Store, env.Factory.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreman := env.Agent(t, "foreman")
	r := &core.Run{FactoryID: env.Factory.ID, Config: foreman}
	r.Config.Skills = []core.Skill{{Name: "github", Scope: "foreman", Content: "team github procedure"}}
	got := runSkillsFor(ctx, env.Store, r, rt, nil)
	if s := skillNames(got); s != "github=foreman,conventions=,chat=builtin" {
		t.Fatalf("foreman skills = %s", s)
	}
	impl := env.Agent(t, "implement")
	got = runSkillsFor(ctx, env.Store, &core.Run{FactoryID: env.Factory.ID, Config: impl}, rt, nil)
	if s := skillNames(got); s != "conventions=,github=builtin" {
		t.Fatalf("implement skills = %s", s)
	}
	// A Self-improvement run gets the definition skill.
	wi := &core.WorkItem{Source: core.SourceRef{Provider: "self_improvement"}}
	got = runSkillsFor(ctx, env.Store, &core.Run{FactoryID: env.Factory.ID, Config: impl}, rt, wi)
	if !strings.Contains(skillNames(got), "factory-definition=builtin") {
		t.Fatalf("self-improvement skills = %s", skillNames(got))
	}
}

type skillForge struct {
	core.Forge
	files map[string]map[string]string // dir -> files
}

func (f skillForge) GetFiles(_ context.Context, owner, repo, _, dir string) (map[string]string, string, error) {
	if owner != "acme" || repo != "tools" {
		return nil, "", errors.New("not found")
	}
	if fs, ok := f.files[dir]; ok {
		return fs, "sha", nil
	}
	return map[string]string{}, "sha", nil
}

type skillForges struct{ f skillForge }

func (s skillForges) Forge(_ context.Context, l core.ForgeLookup) (core.Forge, error) {
	if l.CodeForge != "GITHUB" {
		return nil, errors.New("not connected")
	}
	return s.f, nil
}

func TestResolveSkillSpec(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	env.Engine.forges = skillForges{f: skillForge{files: map[string]map[string]string{
		".agents/skills/deploy": {
			".agents/skills/deploy/SKILL.md":       "---\nname: deploy\ndescription: Deploys.\n---\nRun make deploy.",
			".agents/skills/deploy/scripts/run.sh": "make deploy",
		},
	}}}
	s, err := env.Engine.ResolveSkill(ctx, "team1", "acme/tools:deploy")
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "deploy" || s.Description != "Deploys." || s.Files["scripts/run.sh"] != "make deploy" {
		t.Fatalf("skill = %+v", s)
	}
	if s2, err := env.Engine.ResolveSkill(ctx, "team1", "acme/tools:.agents/skills/deploy/SKILL.md"); err != nil || s2.Name != "deploy" {
		t.Fatalf("by path: %+v, %v", s2, err)
	}
	for _, bad := range []string{"acme:deploy", "acme/tools", "acme/tools:../x", "acme/tools/x:y"} {
		if _, err := env.Engine.ResolveSkill(ctx, "team1", bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	if _, err := env.Engine.ResolveSkill(ctx, "team1", "acme/tools:missing"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("missing: err = %v", err)
	}
}

// TestRunAgentsAttachesSkills checks that run_agents can attach skills to a
// child, and that an unknown skill is an error.
func TestRunAgentsAttachesSkills(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	impl := env.Agent(t, "implement")
	runs, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{
		{Name: "impl", AgentIdentityUID: impl.ID, Prompt: "Fix.", Skills: []string{"conventions", "factory-definition"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := skillNames(runs[0].Config.Skills)
	if !strings.HasPrefix(got, "conventions=implement,factory-definition=implement") {
		t.Fatalf("child skills = %s", got)
	}
	_, err = env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID, AgentRunConfigs: []core.ChildRunConfig{
		{Name: "impl", AgentIdentityUID: impl.ID, Prompt: "Fix.", Skills: []string{"no-such-skill"}},
	}})
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("unknown skill: err = %v", err)
	}
}
