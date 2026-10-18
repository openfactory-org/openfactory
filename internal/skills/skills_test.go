package skills

import (
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func names(list []core.Skill) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func TestBuiltins(t *testing.T) {
	want := "azure-devops,chat,factory-definition,github,gitlab,jira,linear"
	if got := strings.Join(BuiltinNames(), ","); got != want {
		t.Fatalf("built-in skills = %s", got)
	}
	for _, n := range BuiltinNames() {
		s, _ := Builtin(n)
		if s.Description == "" || s.Scope != ScopeBuiltin || !strings.HasPrefix(s.Content, "---\nname: "+n+"\n") {
			t.Errorf("skill %s: %+v", n, s)
		}
	}
}

func TestForRun(t *testing.T) {
	ints := []string{"slack", "linear", "jira"}
	cases := []struct {
		c    Context
		want string
	}{
		{Context{AgentType: core.AgentForeman, Forges: []string{"GITHUB", ""}, Integrations: ints}, "github,chat,linear,jira"},
		{Context{AgentType: core.AgentTriage, Forges: []string{"GITLAB"}, Integrations: ints}, "gitlab,linear,jira"},
		{Context{AgentType: core.AgentImplement, Forges: []string{"AZURE_DEVOPS"}, Integrations: ints}, "azure-devops"},
		{Context{AgentType: core.AgentReview, SelfImprovement: true}, "factory-definition"},
		{Context{AgentType: core.AgentForeman, Integrations: []string{"microsoft_teams"}}, "chat"},
	}
	for _, tc := range cases {
		if got := names(ForRun(tc.c)); got != tc.want {
			t.Errorf("%+v: got %s, want %s", tc.c, got, tc.want)
		}
	}
}

func TestMergeOrder(t *testing.T) {
	agent := []core.Skill{{Name: "deploy", Scope: "implement"}}
	factory := []core.Skill{{Name: "Deploy", Scope: ScopeFactory}, {Name: "checks", Scope: ScopeFactory}}
	repo := []core.Skill{{Name: "checks", Scope: ScopeRepository}, {Name: "github", Scope: ScopeRepository}}
	builtin := []core.Skill{{Name: "github", Scope: ScopeBuiltin}, {Name: "chat", Scope: ScopeBuiltin}}
	got := Merge(agent, factory, repo, builtin)
	var out []string
	for _, s := range got {
		out = append(out, s.Name+"="+s.Scope)
	}
	if s := strings.Join(out, ","); s != "deploy=implement,checks=factory,github=repository,chat=builtin" {
		t.Fatalf("merge = %s", s)
	}
}

func TestFrontmatter(t *testing.T) {
	n, d := Frontmatter("\ufeff---\nname: x\ndescription: \"Does y.\"\nmetadata:\n  name: nested\n---\nbody")
	if n != "x" || d != "Does y." {
		t.Fatalf("name=%q desc=%q", n, d)
	}
	if n, _ := Frontmatter("no frontmatter"); n != "" {
		t.Fatalf("name = %q", n)
	}
}
