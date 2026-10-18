package harness

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Directories (relative to the run's work dir) used for injected state.
const (
	StateDir         = ".openfactory"
	SkillsDir        = ".openfactory/skills"
	SessionsDir      = ".openfactory/sessions"
	ClaudeSkillsDir  = ".claude/skills"
	stateGitignore   = "*\n"
	skillFileName    = "SKILL.md"
	maxSkillNameSize = 64
)

var skillNameRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// SkillDirName returns a filesystem-safe directory name for a skill.
func SkillDirName(name string) string {
	n := skillNameRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	n = strings.Trim(n, "-.")
	if n == "" {
		n = "skill"
	}
	if len(n) > maxSkillNameSize {
		n = n[:maxSkillNameSize]
	}
	return n
}

// DedupeSkills returns skills unique by directory name; agent-scoped skills override
// factory-scoped skills with the same name. Output is sorted by name.
func DedupeSkills(skills []core.Skill) []core.Skill {
	byName := map[string]core.Skill{}
	for _, s := range skills {
		k := SkillDirName(s.Name)
		prev, ok := byName[k]
		if !ok || (prev.Scope == "factory" && s.Scope != "factory") {
			byName[k] = s
		}
	}
	out := make([]core.Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return SkillDirName(out[i].Name) < SkillDirName(out[j].Name) })
	return out
}

// ResolvePath joins p onto workDir unless p is absolute.
func ResolvePath(workDir, p string) string {
	if path.IsAbs(p) {
		return path.Clean(p)
	}
	if workDir == "" {
		workDir = "."
	}
	return path.Join(workDir, p)
}

// EnsureStateDir creates <workDir>/.openfactory with a .gitignore so injected files
// never show up in the repository's git status.
func EnsureStateDir(ctx context.Context, sb core.Sandbox, workDir string) error {
	return WriteFile(ctx, sb, ResolvePath(workDir, StateDir+"/.gitignore"), []byte(stateGitignore), 0o644)
}

// SkillWriteOptions controls WriteSkills.
type SkillWriteOptions struct {
	// Dirs are skill roots relative to workDir (or absolute). Default: SkillsDir.
	Dirs []string
	// SkipExisting leaves a SKILL.md that already exists untouched (used for roots inside the
	// repository, such as .claude/skills, so a repository's own skill is never overwritten).
	SkipExisting bool
	// IgnoreInGit writes a ".gitignore" containing "*" into each written skill directory.
	IgnoreInGit bool
}

// WriteSkills writes each skill's SKILL.md and supporting files to <root>/<name>/ for every root.
// It returns the directory (relative to workDir when the root is relative) of each written skill
// in the first root, keyed by skill name.
func WriteSkills(ctx context.Context, sb core.Sandbox, workDir string, skills []core.Skill, opts SkillWriteOptions) (map[string]string, error) {
	dirs := opts.Dirs
	if len(dirs) == 0 {
		dirs = []string{SkillsDir}
	}
	written := map[string]string{}
	for i, root := range dirs {
		for _, s := range DedupeSkills(skills) {
			rel := path.Join(root, SkillDirName(s.Name))
			dir := ResolvePath(workDir, rel)
			skillPath := path.Join(dir, skillFileName)
			if opts.SkipExisting {
				if _, err := sb.ReadFile(ctx, skillPath); err == nil {
					continue
				}
			}
			if err := WriteFile(ctx, sb, skillPath, []byte(s.Content), 0o644); err != nil {
				return nil, fmt.Errorf("write skill %s: %w", s.Name, err)
			}
			for name, content := range s.Files {
				clean := path.Clean("/" + name)[1:] // strips any ../ escapes
				if clean == "" || clean == skillFileName {
					continue
				}
				if err := WriteFile(ctx, sb, path.Join(dir, clean), []byte(content), 0o644); err != nil {
					return nil, fmt.Errorf("write skill %s file %s: %w", s.Name, name, err)
				}
			}
			if opts.IgnoreInGit {
				if err := WriteFile(ctx, sb, path.Join(dir, ".gitignore"), []byte(stateGitignore), 0o644); err != nil {
					return nil, err
				}
			}
			if i == 0 {
				written[s.Name] = rel
			}
		}
	}
	return written, nil
}

// SkillsIndex renders a markdown list of skills for inclusion in a system prompt.
// dir is the skills root shown to the agent (e.g. ".openfactory/skills").
func SkillsIndex(skills []core.Skill, dir string) string {
	skills = DedupeSkills(skills)
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Skills\n\nThe following skills are available. Read a skill's SKILL.md before following it")
	if dir != "" {
		b.WriteString(" (files live under `" + dir + "/<name>/`)")
	}
	b.WriteString(".\n\n")
	for _, s := range skills {
		desc := strings.TrimSpace(s.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "- **%s**: %s", s.Name, oneLine(desc))
		if dir != "" {
			fmt.Fprintf(&b, " — `%s/%s/SKILL.md`", dir, SkillDirName(s.Name))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:297] + "..."
	}
	return s
}
