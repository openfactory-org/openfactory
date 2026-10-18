package engine

import (
	"context"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// checkWith runs the preflight of pf's factory on def.
func (pf *principalFixture) checkWith(t *testing.T, def *core.CompiledDefinition) *core.DefinitionCheck {
	t.Helper()
	pf.eng.compile = CompilerFor(def)
	chk, err := pf.eng.CheckDefinition(context.Background(), pf.f.ID, map[string]string{"factory.yaml": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	return chk
}

func (pf *principalFixture) def() *core.CompiledDefinition {
	def := DefaultTestDefinition()
	def.Automations = nil
	return def
}

func automation(name, runAs string) core.Automation {
	return core.Automation{Name: name, Enabled: true, Prompt: "go", RunAs: runAs,
		Triggers: []core.Trigger{{Provider: "github", Event: "issue_labeled"}}}
}

func TestPreflightRunAsNamesAPrincipal(t *testing.T) {
	pf := newPrincipalFixture(t, "EXECUTOR")
	def := pf.def()
	def.Automations = []core.Automation{
		automation("ok-user", "user:dana@example.com"),
		automation("ok-sa", "serviceAccount:ci"),
		automation("ok-factory", "factory"),
		automation("none", ""),
		automation("no-user", "user:ghost@example.com"),
		automation("no-sa", "serviceAccount:ghost"),
		automation("sa-as-user", "user:ci"), // a service account has no email
	}
	chk := pf.checkWith(t, def)
	for _, name := range []string{"no-user", "no-sa"} {
		if !hasIssue(chk.Errors, "automations/"+name+"/automation.md", "runAs", "names no principal of the team") {
			t.Errorf("missing runAs error for %s: %+v", name, chk.Errors)
		}
	}
	for _, e := range chk.Errors {
		for _, ok := range []string{"ok-user", "ok-sa", "ok-factory", "none"} {
			if e.File == "automations/"+ok+"/automation.md" {
				t.Errorf("unexpected error for %s: %+v", ok, e)
			}
		}
	}
	if chk.Valid() {
		t.Fatal("a runAs that names no principal must make the definition invalid")
	}
}

func TestPreflightCreatorWithoutLinkedAccountWarns(t *testing.T) {
	// dana has a linked forge account, erin has none.
	pf := newPrincipalFixture(t, "CREATOR")
	def := pf.def()
	def.CredentialStrategy = "CREATOR"
	def.Automations = []core.Automation{
		automation("as-dana", "user:dana@example.com"),
		automation("as-erin", "user:erin@example.com"),
		automation("as-ci", "serviceAccount:ci"),
	}
	chk := pf.checkWith(t, def)
	if !chk.Valid() {
		t.Fatalf("a missing link is a warning, not an error: %+v", chk.Errors)
	}
	if !hasIssue(chk.Warnings, "automations/as-erin/automation.md", "runAs", "no linked forge account") {
		t.Errorf("missing warning for erin: %+v", chk.Warnings)
	}
	for _, name := range []string{"as-dana", "as-ci"} {
		if hasIssue(chk.Warnings, "automations/"+name+"/automation.md", "runAs", "") {
			t.Errorf("unexpected warning for %s: %+v", name, chk.Warnings)
		}
	}

	// With the strategy EXECUTOR the link does not matter.
	def.CredentialStrategy = "EXECUTOR"
	chk = pf.checkWith(t, def)
	if hasIssue(chk.Warnings, "automations/as-erin/automation.md", "runAs", "") {
		t.Errorf("warning with strategy EXECUTOR: %+v", chk.Warnings)
	}
}

func TestPreflightApproversAreTeamPrincipals(t *testing.T) {
	pf := newPrincipalFixture(t, "EXECUTOR")
	def := pf.def()
	def.Approvers = []string{"Dana@Example.com", "ci", "ghost@example.com", "erin"}
	chk := pf.checkWith(t, def)
	if chk.Valid() {
		t.Fatal("unknown approvers must make the definition invalid")
	}
	for _, bad := range []string{"ghost@example.com", "erin"} { // erin is a person: only her email matches
		if !hasIssue(chk.Errors, "factory.yaml", "approvers", `"`+bad+`"`) {
			t.Errorf("missing error for %q: %+v", bad, chk.Errors)
		}
	}
	for _, good := range []string{"Dana@Example.com", `"ci"`} {
		if hasIssue(chk.Errors, "factory.yaml", "approvers", good) {
			t.Errorf("unexpected error for %q: %+v", good, chk.Errors)
		}
	}
	def.Approvers = []string{"dana@example.com", "ci"}
	if chk = pf.checkWith(t, def); !chk.Valid() {
		t.Fatalf("errors = %+v", chk.Errors)
	}
	// A rejected list stops an apply and keeps the old version.
	def.Approvers = []string{"ghost@example.com"}
	pf.eng.compile = CompilerFor(def)
	if _, err := pf.eng.ApplyDefinition(context.Background(), pf.f.ID, map[string]string{"factory.yaml": "ok"}, "tester", "bad", ""); err == nil {
		t.Fatal("apply with an unknown approver succeeded")
	}
}

func TestApproversMatchServiceAccountNames(t *testing.T) {
	ci := &core.User{ID: "sa_ci", Name: "ci", Kind: core.UserKindServiceAccount}
	person := &core.User{ID: "usr_x", Name: "ci", Email: "x@example.com"}
	if !IsApprover([]string{"ci"}, ci) {
		t.Error("a service account name must match")
	}
	if IsApprover([]string{"ci"}, person) {
		t.Error("a person must not match by name")
	}
	if !IsApprover([]string{"X@example.com"}, person) {
		t.Error("a person matches by email")
	}
}

func TestPreflightWarnsAboutUnknownModels(t *testing.T) {
	pf := newPrincipalFixture(t, "EXECUTOR")
	def := pf.def()
	def.Agents[0].Harness.Model = "claude-sonnet-5-5"
	def.Agents[1].Harness.Model = "claude-sonet-5" // typing error
	def.Agents[2].Harness.Model = "claude-sonnet-5-5-20260101"
	def.Agents[3].Harness = core.HarnessConfig{Type: core.HarnessCodex, Model: "gpt-5.3-codex"}
	def.Agents[4].Harness = core.HarnessConfig{Type: core.HarnessClaude, Model: "sonet"}
	def.Automations = []core.Automation{{Name: "tuned", Enabled: true, Prompt: "go", Agent: "spec",
		Overrides: &core.AgentOverrides{Model: "haiku-x"},
		Triggers:  []core.Trigger{{Provider: "github", Event: "issue_labeled"}}}}
	chk := pf.checkWith(t, def)
	if !chk.Valid() {
		t.Fatalf("an unknown model is a warning: %+v", chk.Errors)
	}
	for _, want := range []struct{ file, field, text string }{
		{"agents/triage/agent.md", "harness.model", `"claude-sonet-5"`},
		{"agents/review/agent.md", "harness.model", `"sonet"`},
		{"automations/tuned/automation.md", "model", `"haiku-x"`},
	} {
		if !hasIssue(chk.Warnings, want.file, want.field, want.text) {
			t.Errorf("missing warning %+v in %+v", want, chk.Warnings)
		}
	}
	for _, file := range []string{"agents/spec/agent.md", "agents/implement/agent.md", "agents/foreman/agent.md"} {
		if hasIssue(chk.Warnings, file, "harness.model", "") {
			t.Errorf("unexpected model warning for %s: %+v", file, chk.Warnings)
		}
	}
}

// An agent without a harness credential gets a warning, and an agent with one
// gets none.
func TestPreflightWarnsAboutMissingCredential(t *testing.T) {
	pf := newPrincipalFixture(t, "EXECUTOR")
	def := pf.def()
	for i := range def.Agents {
		def.Agents[i].Harness.Auth = &core.HarnessAuth{Source: "workerEnvironment"}
	}
	def.Agents[0].Harness.Auth = nil
	chk := pf.checkWith(t, def)
	if !hasIssue(chk.Warnings, agentFile(def.Agents[0].Name), "harness.auth", "has no credential") {
		t.Errorf("no credential warning for %s: %+v", def.Agents[0].Name, chk.Warnings)
	}
	for _, a := range def.Agents[1:] {
		if hasIssue(chk.Warnings, agentFile(a.Name), "harness.auth", "") {
			t.Errorf("unexpected credential warning for %s", a.Name)
		}
	}
}
