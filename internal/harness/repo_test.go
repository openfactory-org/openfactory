package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/harness/claude"
	"github.com/openfactory-org/openfactory/internal/harness/harnesstest"
)

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const deploySkill = "---\nname: deploy\ndescription: Deploys the service.\n---\n\nRun make deploy.\n"

func TestDiscoverRepo(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Use tabs.")
	writeFile(t, filepath.Join(dir, ".agents/skills/deploy/SKILL.md"), deploySkill)
	// A skill that a harness wrote has a .gitignore with "*": it is skipped.
	writeFile(t, filepath.Join(dir, ".claude/skills/injected/SKILL.md"), "---\nname: injected\n---\n")
	writeFile(t, filepath.Join(dir, ".claude/skills/injected/.gitignore"), "*\n")
	// A second repository in a subdirectory.
	writeFile(t, filepath.Join(dir, "web/.git/HEAD"), "ref: refs/heads/main\n")
	writeFile(t, filepath.Join(dir, "web/CLAUDE.md"), "Run npm test.")
	writeFile(t, filepath.Join(dir, "web/.claude/skills/lint/SKILL.md"), "---\nname: lint\ndescription: Lints.\n---\n")
	// A directory that is not a repository is not read.
	writeFile(t, filepath.Join(dir, "notes/AGENTS.md"), "ignore me")

	rc := harness.DiscoverRepo(context.Background(), harnesstest.NewLocal(dir), dir)
	var rules []string
	for _, r := range rc.Rules {
		rules = append(rules, r.Path+"="+r.Content)
	}
	if got := strings.Join(rules, ","); got != "AGENTS.md=Use tabs.,web/CLAUDE.md=Run npm test." {
		t.Fatalf("rules = %s", got)
	}
	var skills []string
	for _, s := range rc.Skills {
		skills = append(skills, s.Name+"@"+s.Path+":"+s.Scope)
	}
	if got := strings.Join(skills, ","); got != "deploy@.agents/skills/deploy/SKILL.md:repository,lint@web/.claude/skills/lint/SKILL.md:repository" {
		t.Fatalf("skills = %s", got)
	}
}

func TestDiscoverRepoWithoutExec(t *testing.T) {
	sb := harnesstest.New("/w")
	sb.Files["/w/AGENTS.md"] = []byte("Rule.")
	rc := harness.DiscoverRepo(context.Background(), sb, "/w")
	if len(rc.Rules) != 1 || rc.Rules[0].Content != "Rule." {
		t.Fatalf("rules = %+v", rc.Rules)
	}
}

func TestMergeRepoSkillsOrder(t *testing.T) {
	req := []core.Skill{
		{Name: "github", Scope: "builtin", Content: "builtin"},
		{Name: "checks", Scope: "factory", Content: "factory"},
		{Name: "chat", Scope: "builtin"},
	}
	repo := []core.Skill{{Name: "github", Scope: "repository", Content: "repo"}, {Name: "checks", Scope: "repository", Content: "repo"}}
	got := harness.MergeRepoSkills(req, repo)
	var names []string
	for _, s := range got {
		names = append(names, s.Name+"="+s.Scope)
	}
	if strings.Join(names, ",") != "checks=factory,github=repository,chat=builtin" {
		t.Fatalf("merged = %v", names)
	}
}

// capture wraps a local sandbox and records the processes that a harness
// starts instead of running them.
type capture struct {
	*harnesstest.Local
	mu     sync.Mutex
	starts []core.ExecRequest
}

func (c *capture) Start(_ context.Context, req core.ExecRequest) (core.Process, error) {
	c.mu.Lock()
	c.starts = append(c.starts, req)
	c.mu.Unlock()
	return harnesstest.NewProcess(`{"type":"result","subtype":"success","result":"ok","session_id":"s"}`+"\n", "", 0), nil
}

func drain(t *testing.T, ch <-chan core.HarnessEvent) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-timeout:
			t.Fatal("timeout")
		}
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestSameRepositoryInstructions checks that a repository
// with one rule file and one skill gives its instructions to a `claude` run
// one time.
func TestSameRepositoryInstructions(t *testing.T) {
	for _, rule := range []string{"AGENTS.md", "CLAUDE.md"} {
		t.Run(rule, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, rule), "Always run make check before a commit.")
			writeFile(t, filepath.Join(dir, ".agents/skills/deploy/SKILL.md"), deploySkill)

			// claude: the CLI reads CLAUDE.md and .claude/skills itself; the
			// harness adds every other rule file and skill.
			sb := &capture{Local: harnesstest.NewLocal(dir)}
			ch, err := claude.New(claude.Options{}).Run(context.Background(), sb, core.HarnessRequest{
				Run: &core.Run{ID: "run_c"}, Prompt: "go", SystemPrompt: "Factory prompt.", WorkDir: dir,
				Auth: map[string]string{"ANTHROPIC_API_KEY": "k"},
			})
			if err != nil {
				t.Fatal(err)
			}
			drain(t, ch)
			if len(sb.starts) != 1 {
				t.Fatalf("starts = %d", len(sb.starts))
			}
			appended := argAfter(sb.starts[0].Cmd, "--append-system-prompt")
			native := ""
			if b, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); err == nil {
				native = string(b)
			}
			if !strings.Contains(appended+native, "Always run make check before a commit.") {
				t.Fatalf("claude gets no rule: appended=%q native=%q", appended, native)
			}
			if strings.Count(appended+native, "Always run make check") != 1 {
				t.Fatalf("claude gets the rule twice: appended=%q", appended)
			}
			if b, err := os.ReadFile(filepath.Join(dir, ".claude/skills/deploy/SKILL.md")); err != nil || string(b) != deploySkill {
				t.Fatalf("claude skill = %q, %v", b, err)
			}
		})
	}
}
