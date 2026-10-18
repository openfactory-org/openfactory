package engine

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/skills"
)

// skillSpecForges are the forges that ResolveSkill tries, in order.
var skillSpecForges = []string{"GITHUB", "GITLAB", "AZURE_DEVOPS"}

// skillSpecDirs are the directories where a bare skill name is looked up.
var skillSpecDirs = []string{".agents/skills", ".claude/skills", "skills"}

// maxSkillSpecFiles is the most files that one resolved skill can have.
const maxSkillSpecFiles = 50

// ParseSkillSpec splits a skill spec "owner/repo:path". path is a skill
// directory ("skills/deploy"), its SKILL.md ("skills/deploy/SKILL.md") or a
// bare skill name ("deploy").
func ParseSkillSpec(spec string) (owner, repo, p string, err error) {
	spec = strings.TrimSpace(spec)
	full, rest, ok := strings.Cut(spec, ":")
	owner, repo, ok2 := strings.Cut(full, "/")
	rest = strings.Trim(strings.TrimSpace(rest), "/")
	if !ok || !ok2 || owner == "" || repo == "" || strings.Contains(repo, "/") || rest == "" {
		return "", "", "", fmt.Errorf("%w: skill spec %q must be owner/repo:path", core.ErrInvalid, spec)
	}
	clean := path.Clean(rest)
	if clean == "." || strings.HasPrefix(clean, "..") || path.IsAbs(clean) {
		return "", "", "", fmt.Errorf("%w: skill spec %q has a bad path", core.ErrInvalid, spec)
	}
	clean = strings.TrimSuffix(strings.TrimSuffix(clean, "/SKILL.md"), "SKILL.md")
	return owner, repo, strings.Trim(clean, "/"), nil
}

// ResolveSkill reads the skill of a skill spec (owner/repo:path) from a
// repository of a forge that the team connected. It reads the default
// branch. The skill has the scope "agent".
func (e *Engine) ResolveSkill(ctx context.Context, teamID, spec string) (*core.Skill, error) {
	owner, repo, p, err := ParseSkillSpec(spec)
	if err != nil {
		return nil, err
	}
	if e.forges == nil {
		return nil, fmt.Errorf("%w: no forge is configured to read skill %q", core.ErrInvalid, spec)
	}
	dirs := []string{p}
	if !strings.Contains(p, "/") {
		for _, d := range skillSpecDirs {
			dirs = append(dirs, d+"/"+p)
		}
	}
	var lastErr error
	for _, cf := range skillSpecForges {
		// The record of the owner of the repository reads it.
		f, err := e.forges.Forge(ctx, core.ForgeLookup{TeamID: teamID, CodeForge: cf, Repo: owner + "/" + repo})
		if err != nil || f == nil {
			continue
		}
		for _, dir := range dirs {
			files, _, err := f.GetFiles(ctx, owner, repo, "", dir)
			if err != nil {
				lastErr = err
				continue
			}
			s, ok := skillFromFiles(dir, files)
			if !ok {
				continue
			}
			return s, nil
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("%w: skill %q: %w", core.ErrInvalid, spec, lastErr)
	}
	return nil, fmt.Errorf("%w: skill %q: no SKILL.md found", core.ErrNotFound, spec)
}

// skillFromFiles makes a skill from the files under dir. Keys can be relative
// to dir or to the repository root.
func skillFromFiles(dir string, files map[string]string) (*core.Skill, bool) {
	rel := map[string]string{}
	for k, v := range files {
		k = strings.TrimPrefix(strings.TrimPrefix(k, "/"), dir+"/")
		rel[k] = v
	}
	content, ok := rel["SKILL.md"]
	if !ok {
		return nil, false
	}
	name, desc := skills.Frontmatter(content)
	if name == "" {
		name = path.Base(dir)
	}
	s := &core.Skill{Name: name, Description: desc, Content: content, Scope: "agent"}
	for k, v := range rel {
		if k == "SKILL.md" || len(s.Files) >= maxSkillSpecFiles {
			continue
		}
		if s.Files == nil {
			s.Files = map[string]string{}
		}
		s.Files[k] = v
	}
	return s, true
}

// childSkills resolves the skills that run_agents attaches to a child: a
// factory skill name, a skill name of another agent of the factory, a
// built-in skill name, or a skill spec (owner/repo:path).
func (e *Engine) childSkills(ctx context.Context, parent *core.Run, rt *FactoryRuntime, names []string) ([]core.Skill, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var agents []core.AgentConfig
	out := make([]core.Skill, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if strings.Contains(n, ":") {
			s, err := e.ResolveSkill(ctx, parent.TeamID, n)
			if err != nil {
				return nil, err
			}
			out = append(out, *s)
			continue
		}
		if s, ok := findSkill(rt.Skills, n); ok {
			out = append(out, s)
			continue
		}
		if agents == nil {
			var err error
			if agents, err = e.store.ListAgents(ctx, parent.FactoryID); err != nil {
				return nil, err
			}
		}
		found := false
		for _, a := range agents {
			if s, ok := findSkill(a.Skills, n); ok {
				out = append(out, s)
				found = true
				break
			}
		}
		if found {
			continue
		}
		if s, ok := skills.Builtin(n); ok {
			out = append(out, s)
			continue
		}
		return nil, fmt.Errorf("%w: skill %q is not a factory, agent or built-in skill", core.ErrInvalid, n)
	}
	return out, nil
}

func findSkill(list []core.Skill, name string) (core.Skill, bool) {
	for _, s := range list {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return core.Skill{}, false
}
