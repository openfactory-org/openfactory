package prompts

import (
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func roster() []core.AgentConfig {
	return []core.AgentConfig{
		{ID: "agt_f", Name: "foreman", Type: core.AgentForeman},
		{ID: "agt_t", Name: "triage", Type: core.AgentTriage, Description: "Investigates\nrequests"},
		{ID: "agt_i", Name: "implement", Type: core.AgentImplement, Description: "Writes code"},
		{ID: "agt_d", Name: "docs", Type: core.AgentCustom, Description: "Docs", SpawnableBy: []string{"implement"}},
		{ID: "agt_n", Name: "locked", Type: core.AgentCustom, SpawnableBy: []string{}},
	}
}

func TestComposeForeman(t *testing.T) {
	cfg := core.AgentConfig{ID: "agt_f", FactoryID: "fac", Name: "foreman", Type: core.AgentForeman, Instructions: "Always be kind.",
		Skills: []core.Skill{{Name: "github", Description: "Use gh", Path: "/workspace/.openfactory/skills/github/SKILL.md"}}}
	wi := &core.WorkItem{ID: "wi_1", Title: "PO number", Stage: core.StageTriage,
		Source:       core.SourceRef{Provider: "github", Kind: "issue", Repo: "acme/api", Number: 7, URL: "https://github.com/acme/api/issues/7"},
		PullRequests: []core.PullRequestRef{{URL: "https://github.com/acme/api/pull/42", Branch: "po"}}}
	run := &core.Run{ID: "run_1", Name: "foreman", Metadata: map[string]string{"setup_profile": "backend"}}
	out := Compose(cfg, wi, run, roster())
	for _, want := range []string{
		"Role: foreman", "run_agents", "list_agents", "request_approval", "untrusted input",
		"**triage** (TRIAGE) — agent_identity_uid: `agt_t` — Investigates requests",
		"`agt_i`", "PO number", "acme/api #7", "https://github.com/acme/api/pull/42 (open, branch po)",
		"setup_profile = backend", "**github** — Use gh (`/workspace/.openfactory/skills/github/SKILL.md`)",
		"# Agent instructions\n\nAlways be kind.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"`agt_d`", "`agt_n`", "`agt_f`", "Default policy", "Never merge", "Spec approval", "merge notification"} {
		if strings.Contains(out, notWant) {
			t.Errorf("foreman should not list %s", notWant)
		}
	}
	if strings.Index(out, "Role: foreman") > strings.Index(out, "# Agent instructions") {
		t.Error("instructions must come after the preamble")
	}
}

func TestComposeStageAgent(t *testing.T) {
	cfg := core.AgentConfig{ID: "agt_i", FactoryID: "fac", Name: "implement", Type: core.AgentImplement, Instructions: "Use TDD."}
	out := Compose(cfg, nil, nil, roster())
	for _, want := range []string{"Role: implement agent (implement)", "final message is delivered to the foreman", "link_pull_request", "untrusted input", "`agt_d`", "Use TDD."} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "`agt_t`") {
		t.Error("implement should not be able to start triage")
	}
	custom := Compose(core.AgentConfig{FactoryID: "fac", Name: "docs", Type: core.AgentCustom, Description: "Updates reference pages"}, nil, nil, roster())
	if !strings.Contains(custom, "Your responsibility: Updates reference pages") {
		t.Errorf("custom preamble:\n%s", custom)
	}
}

// TestPolicyComesFromInstructions checks that the platform adds no workflow
// policy, so a team that removes spec approval from agent.md gets a prompt
// without an approval rule.
func TestPolicyComesFromInstructions(t *testing.T) {
	cfg := core.AgentConfig{FactoryID: "fac", Name: "foreman", Type: core.AgentForeman,
		Instructions: "Dispatch implement for every request. Hand off the pull request."}
	out := Compose(cfg, nil, nil, roster())
	low := strings.ToLower(out)
	for _, notWant := range []string{"spec approval", "approval of the spec", "obtain explicit human approval", "never merge", "only after a merge"} {
		if strings.Contains(low, notWant) {
			t.Errorf("prompt has the policy text %q:\n%s", notWant, out)
		}
	}
	if !strings.Contains(out, "Dispatch implement for every request.") {
		t.Errorf("instructions missing:\n%s", out)
	}
}

func TestSkillDir(t *testing.T) {
	tests := map[string]string{
		"github":      ".openfactory/skills/github",
		"my skill/..": ".openfactory/skills/my-skill",
		"../../etc":   ".openfactory/skills/etc",
		"":            ".openfactory/skills/skill",
	}
	for in, want := range tests {
		if got := SkillDir(in); got != want {
			t.Errorf("SkillDir(%q) = %q, want %q", in, got, want)
		}
	}
}
