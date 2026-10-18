// Package skills holds the built-in skills of the binary and the rules that
// pick and order the skills of a run.
//
// A run gets skills from four sources. When two skills have the same name,
// the first source in this order wins:
//
//  1. agent skills (agents/<name>/skills/ in the definition, or attached by
//     run_agents),
//  2. factory skills (skills/ in the definition),
//  3. repository skills (found in the repositories of the run by the
//     harness),
//  4. built-in skills (this package).
package skills

import (
	"embed"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Skill scopes (core.Skill.Scope). An agent skill has the agent name as its
// scope.
const (
	ScopeFactory    = "factory"
	ScopeRepository = "repository"
	ScopeBuiltin    = "builtin"
)

// Built-in skill names.
const (
	GitHub            = "github"
	GitLab            = "gitlab"
	AzureDevOps       = "azure-devops"
	Chat              = "chat"
	Linear            = "linear"
	Jira              = "jira"
	FactoryDefinition = "factory-definition"
)

//go:embed builtin
var builtinFS embed.FS

var builtins = loadBuiltins()

func loadBuiltins() map[string]core.Skill {
	out := map[string]core.Skill{}
	err := fs.WalkDir(builtinFS, "builtin", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Base(p) != "SKILL.md" {
			return err
		}
		b, err := builtinFS.ReadFile(p)
		if err != nil {
			return err
		}
		dir := path.Base(path.Dir(p))
		name, desc := Frontmatter(string(b))
		if name == "" {
			name = dir
		}
		out[name] = core.Skill{Name: name, Description: desc, Content: string(b), Scope: ScopeBuiltin}
		return nil
	})
	if err != nil {
		panic("skills: reading built-in skills: " + err.Error())
	}
	return out
}

// Builtin returns the built-in skill with the name.
func Builtin(name string) (core.Skill, bool) {
	s, ok := builtins[name]
	return s, ok
}

// BuiltinNames returns the names of the built-in skills, sorted.
func BuiltinNames() []string {
	out := make([]string, 0, len(builtins))
	for n := range builtins {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Frontmatter returns the name and the description of the YAML frontmatter
// of a SKILL.md. It reads only simple "key: value" lines.
func Frontmatter(content string) (name, description string) {
	s := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(s, "---") {
		return "", ""
	}
	rest := s[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			name = v
		case "description":
			description = v
		}
	}
	return name, description
}

// Context is what decides the built-in skills of a run.
type Context struct {
	// AgentType is the type of the agent.
	AgentType core.AgentType
	// Forges are the code forges of the repositories of the run
	// (core.RepoRef.CodeForge; empty means GITHUB).
	Forges []string
	// Integrations are the providers that the factory connects (slack,
	// microsoft_teams, linear, jira, ...).
	Integrations []string
	// SelfImprovement is true for a run of a Self-improvement work item.
	SelfImprovement bool
}

// ForRun returns the built-in skills that a run gets:
//
//   - every agent gets the skill of each forge of its repositories;
//   - the foreman, the only agent that talks to people, gets the chat skill
//     when a chat integration is connected;
//   - the foreman and the triage and spec agents get the skill of each
//     connected tracker;
//   - a Self-improvement run gets the definition skill.
func ForRun(c Context) []core.Skill {
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, f := range c.Forges {
		switch strings.ToUpper(strings.TrimSpace(f)) {
		case "", "GITHUB":
			add(GitHub)
		case "GITLAB":
			add(GitLab)
		case "AZURE_DEVOPS", "AZUREDEVOPS", "AZURE-DEVOPS":
			add(AzureDevOps)
		}
	}
	talks := c.AgentType == core.AgentForeman
	tracks := talks || c.AgentType == core.AgentTriage || c.AgentType == core.AgentSpec
	for _, p := range c.Integrations {
		switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "-", "_")) {
		case "slack", "microsoft_teams", "teams":
			if talks {
				add(Chat)
			}
		case "linear":
			if tracks {
				add(Linear)
			}
		case "jira":
			if tracks {
				add(Jira)
			}
		}
	}
	if c.SelfImprovement {
		add(FactoryDefinition)
	}
	out := make([]core.Skill, 0, len(names))
	for _, n := range names {
		if s, ok := builtins[n]; ok {
			out = append(out, s)
		}
	}
	return out
}

// Merge returns the skills of a run in the order of the sources: agent,
// factory, repository, built-in. A skill whose name is already in an earlier
// list is left out. The order inside each list does not change.
func Merge(agent, factory, repository, builtin []core.Skill) []core.Skill {
	out := make([]core.Skill, 0, len(agent)+len(factory)+len(repository)+len(builtin))
	seen := map[string]bool{}
	for _, list := range [][]core.Skill{agent, factory, repository, builtin} {
		for _, s := range list {
			k := key(s.Name)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// Split separates the skills of an agent configuration into agent skills and
// factory skills by their scope.
func Split(list []core.Skill) (agent, factory []core.Skill) {
	for _, s := range list {
		if s.Scope == ScopeFactory {
			factory = append(factory, s)
		} else {
			agent = append(agent, s)
		}
	}
	return agent, factory
}

func key(name string) string { return strings.ToLower(strings.TrimSpace(name)) }
