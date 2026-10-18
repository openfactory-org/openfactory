package definition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

const examplesDir = "../../examples"

func exampleDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(examplesDir, e.Name(), "factory.yaml")); err != nil {
			continue // not a definition tree (e.g. the custom-binary Go module)
		}
		out = append(out, filepath.Join(examplesDir, e.Name()))
	}
	want := []string{"00-default-agents", "01-single-repo-quickstart", "02-sdlc-issue-to-pr", "03-multi-harness",
		"04-code-review-only", "05-incident-response", "06-common-automations", "07-self-hosted-worker"}
	for _, w := range want {
		found := false
		for _, d := range out {
			found = found || filepath.Base(d) == w
		}
		if !found {
			t.Errorf("missing example %s", w)
		}
	}
	return out
}

func TestExamplesCompile(t *testing.T) {
	for _, dir := range exampleDirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			files, err := LoadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := files["README.md"]; !ok {
				t.Error("example has no README.md")
			}
			def, errs := Compile(files)
			for _, e := range errs {
				t.Errorf("%s:%d %s: %s", e.File, e.Line, e.Field, e.Message)
			}
			if def == nil {
				return
			}
			foremen := 0
			for _, a := range def.Agents {
				if a.Type == core.AgentForeman {
					foremen++
				}
				// Each agent runs a harness CLI: claude or codex.
				if a.Harness.Type != core.HarnessClaude && a.Harness.Type != core.HarnessCodex {
					t.Errorf("agent %s: harness %q, want claude or codex", a.Name, a.Harness.Type)
				}
			}
			for _, sc := range def.Scorers {
				if sc.Harness == nil || (sc.Harness.Type != core.HarnessClaude && sc.Harness.Type != core.HarnessCodex) {
					t.Errorf("scorer %s: harness %+v, want claude or codex", sc.Slug, sc.Harness)
				}
			}
			if foremen != 1 {
				t.Errorf("%d foremen", foremen)
			}
		})
	}
}

func TestExamplesValidateAgainstSchemas(t *testing.T) {
	for _, dir := range exampleDirs(t) {
		files, err := LoadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for p, content := range files {
			if SchemaForPath(p) == "" {
				continue
			}
			if err := ValidateSchema(p, content); err != nil {
				t.Errorf("%s/%s: %v", filepath.Base(dir), p, err)
			}
		}
	}
}

func TestExamplesHaveNoVendorReferences(t *testing.T) {
	// Vendor names from the reference material, stored rot13-encoded so the
	// repository itself never spells them.
	var banned []string
	for _, enc := range []string{"jnec", "bm-", "bm "} {
		banned = append(banned, rot13(enc))
	}
	for _, dir := range exampleDirs(t) {
		files, _ := LoadDir(dir)
		for p, content := range files {
			lc := strings.ToLower(content)
			for _, b := range banned {
				if strings.Contains(lc, b) {
					t.Errorf("%s/%s contains %q", filepath.Base(dir), p, b)
				}
			}
		}
	}
}

func TestDefaultAgentsMatchExample(t *testing.T) {
	files, err := LoadDir(filepath.Join(examplesDir, "00-default-agents"))
	if err != nil {
		t.Fatal(err)
	}
	defaults := DefaultAgents()
	if len(defaults) != len(DefaultAgentNames) {
		t.Fatalf("DefaultAgents returned %d files", len(defaults))
	}
	for p, content := range defaults {
		if files[p] != content {
			t.Errorf("examples/00-default-agents/%s differs from the embedded default", p)
		}
	}
}

func TestDefaultDefinition(t *testing.T) {
	files, err := DefaultDefinition(DefaultDefinitionOptions{
		Name: "payments", Alias: "payments",
		Repositories: []core.RepoRef{{Owner: "acme", Name: "api"}, {Owner: "acme", Name: "web"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	def, errs := Compile(files)
	if len(errs) > 0 {
		t.Fatalf("default definition invalid: %v\n%s", errs, files["factory.yaml"])
	}
	if len(def.Agents) != 5 || def.Alias != "payments" || len(def.Repositories) != 2 {
		t.Errorf("unexpected definition: %d agents, alias %q", len(def.Agents), def.Alias)
	}
	var impl, rev string
	for _, a := range def.Agents {
		switch a.Type {
		case core.AgentImplement:
			impl = a.Harness.Model
		case core.AgentReview:
			rev = a.Harness.Model
		}
	}
	if impl == "" || impl == rev {
		t.Errorf("review model %q must differ from implement model %q", rev, impl)
	}

	// Subset, multiple forges.
	files, err = DefaultDefinition(DefaultDefinitionOptions{
		Name: "mixed", Agents: []string{"implement"},
		Repositories: []core.RepoRef{{CodeForge: "GITHUB", Owner: "acme", Name: "api"}, {CodeForge: "GITLAB", Owner: "acme/platform", Name: "infra"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	def, errs = Compile(files)
	if len(errs) > 0 {
		t.Fatalf("invalid: %v\n%s", errs, files["factory.yaml"])
	}
	if len(def.Agents) != 2 || def.Repositories[1].CodeForge != "GITLAB" {
		t.Errorf("unexpected: %+v", def.Repositories)
	}
	if _, err := DefaultDefinition(DefaultDefinitionOptions{}); err == nil {
		t.Error("expected error for missing name")
	}
	if _, err := DefaultDefinition(DefaultDefinitionOptions{Name: "x", Agents: []string{"nope"}}); err == nil {
		t.Error("expected error for unknown agent")
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	must := func(p, c string) {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("factory.yaml", "x")
	must("agents/a/agent.md", "y")
	must(".git/config", "z")
	must(".hidden", "z")
	files, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files["agents/a/agent.md"] != "y" {
		t.Errorf("got %v", files)
	}
	if _, err := LoadDir(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected error")
	}
	if _, err := LoadDir(filepath.Join(dir, "factory.yaml")); err == nil {
		t.Error("expected error for file")
	}
}

func rot13(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return 'a' + (r-'a'+13)%26
		case r >= 'A' && r <= 'Z':
			return 'A' + (r-'A'+13)%26
		}
		return r
	}, s)
}
