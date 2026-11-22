package harness

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// RuleFileNames are the project rule files that a run reads from the root of
// each repository, in this order. The `open` harness reads all of them.
// The `claude` and `codex` harnesses add the files that their CLI does not
// read itself.
var RuleFileNames = []string{"AGENTS.md", "AGENT.md", "CLAUDE.md"}

// RepoSkillDirs are the skill directories that a run finds in the root of
// each repository. Each skill is <dir>/<name>/SKILL.md.
var RepoSkillDirs = []string{".agents/skills", ".claude/skills"}

// Limits of repository discovery.
const (
	maxRuleFileBytes   = 64 << 10
	maxRepoSkillBytes  = 64 << 10
	maxRepoSkills      = 50
	repoDiscoveryLimit = 30 * time.Second
	discoveryMarker    = "of-repo-scan-ok"
)

// RuleFile is a project rule file of a repository.
type RuleFile struct {
	// Path is relative to the work directory, for example "AGENTS.md" or
	// "api/AGENTS.md".
	Path    string
	Content string
	// Truncated is true when the file was longer than the limit.
	Truncated bool
}

// RepoContext is what a run finds in its repositories.
type RepoContext struct {
	Rules []RuleFile
	// Skills have the scope "repository". Path is the SKILL.md path relative
	// to the work directory.
	Skills []rtcore.Skill
}

// repoScanScript lists the repository roots (the work directory and each
// direct subdirectory with a .git entry), their rule files and their skill
// files. A skill directory with a ".gitignore" that ignores everything was
// written by a harness, not by the repository, and is skipped.
func repoScanScript() string {
	var b strings.Builder
	b.WriteString(`for root in . */; do
  root=${root%/}
  [ -d "$root" ] || continue
  if [ "$root" != "." ] && [ ! -e "$root/.git" ]; then continue; fi
`)
	for _, n := range RuleFileNames {
		fmt.Fprintf(&b, "  [ -f \"$root/%s\" ] && printf 'rule\\t%%s\\n' \"$root/%s\"\n", n, n)
	}
	for _, d := range RepoSkillDirs {
		fmt.Fprintf(&b, `  for f in "$root"/%s/*/SKILL.md; do
    [ -f "$f" ] || continue
    g="${f%%/SKILL.md}/.gitignore"
    if [ -f "$g" ] && [ "$(cat "$g")" = "*" ]; then continue; fi
    printf 'skill\t%%s\n' "$f"
  done
`, d)
	}
	b.WriteString("done\necho " + discoveryMarker + "\n")
	return b.String()
}

// DiscoverRepo finds the rule files and the skills of the repositories in the
// work directory. A failure to read a file leaves that file out; discovery
// never fails a turn.
func DiscoverRepo(ctx context.Context, sb rtcore.Sandbox, workDir string) RepoContext {
	var rc RepoContext
	var rules, skillFiles []string
	scanned := false
	cctx, cancel := context.WithTimeout(ctx, repoDiscoveryLimit)
	res, err := sb.Exec(cctx, rtcore.ExecRequest{Cmd: []string{"sh", "-c", repoScanScript()}, Dir: workDir, Timeout: repoDiscoveryLimit})
	cancel()
	if err == nil && res != nil && res.ExitCode == 0 && strings.Contains(res.Stdout, discoveryMarker) {
		scanned = true
		for _, line := range strings.Split(res.Stdout, "\n") {
			kind, p, ok := strings.Cut(strings.TrimSpace(line), "\t")
			if !ok {
				continue
			}
			p = path.Clean(strings.TrimPrefix(p, "./"))
			switch kind {
			case "rule":
				rules = append(rules, p)
			case "skill":
				skillFiles = append(skillFiles, p)
			}
		}
	}
	if !scanned {
		// The sandbox cannot run the scan: look at the work directory only.
		rules = append(rules, RuleFileNames...)
	}
	for _, p := range rules {
		b, err := sb.ReadFile(ctx, ResolvePath(workDir, p))
		if err != nil {
			continue
		}
		rf := RuleFile{Path: p, Content: string(b)}
		if len(b) > maxRuleFileBytes {
			rf.Content, rf.Truncated = cutUTF8(string(b), maxRuleFileBytes), true
		}
		if strings.TrimSpace(rf.Content) == "" {
			continue
		}
		rc.Rules = append(rc.Rules, rf)
	}
	seen := map[string]bool{}
	for _, p := range skillFiles {
		if len(rc.Skills) >= maxRepoSkills {
			break
		}
		b, err := sb.ReadFile(ctx, ResolvePath(workDir, p))
		if err != nil || len(b) > maxRepoSkillBytes {
			continue
		}
		name, desc := skillFrontmatter(string(b))
		if name == "" {
			name = path.Base(path.Dir(p))
		}
		if seen[SkillDirName(name)] {
			continue // the first repository and the first directory win
		}
		seen[SkillDirName(name)] = true
		rc.Skills = append(rc.Skills, rtcore.Skill{Name: name, Description: desc, Path: p, Content: string(b), Scope: "repository"})
	}
	return rc
}

// MergeRepoSkills puts the repository skills into the skills of a request in
// this order: agent and factory skills first, then repository skills,
// then built-in skills (scope "builtin"). A skill with a name that an earlier
// source has is left out.
func MergeRepoSkills(req, repo []rtcore.Skill) []rtcore.Skill {
	var first, builtin []rtcore.Skill
	for _, s := range req {
		if s.Scope == "builtin" {
			builtin = append(builtin, s)
		} else {
			first = append(first, s)
		}
	}
	out := make([]rtcore.Skill, 0, len(req)+len(repo))
	seen := map[string]bool{}
	for _, list := range [][]rtcore.Skill{first, repo, builtin} {
		for _, s := range list {
			k := SkillDirName(s.Name)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, s)
		}
	}
	return out
}

// RepoInstructions renders the rule files for a system prompt. skip reports
// the files that the harness CLI reads itself; they are left out.
func RepoInstructions(rules []RuleFile, skip func(RuleFile) bool) string {
	var b strings.Builder
	for _, r := range rules {
		if skip != nil && skip(r) {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("# Repository instructions\n\nThese files come from the repositories of this run. Follow them for work in that repository, unless they conflict with the factory or agent instructions above.\n")
		}
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", r.Path, strings.TrimSpace(r.Content))
		if r.Truncated {
			fmt.Fprintf(&b, "\n(The file is longer than %d KiB and was cut. Read the full file in the workspace.)\n", maxRuleFileBytes>>10)
		}
	}
	return b.String()
}

// RootFile reports whether a rule file is the named file in the root of the
// work directory.
func RootFile(name string) func(RuleFile) bool {
	return func(r RuleFile) bool { return r.Path == name }
}

func skillFrontmatter(content string) (name, description string) {
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
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
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

func cutUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}
