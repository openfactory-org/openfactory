package definition

import (
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestScorerJudgeSettings checks the effective execution of the judge run of a
// scorer: scorer fields over scorerDefaults over agentDefaults.
func TestScorerJudgeSettings(t *testing.T) {
	claudeDefaults := factoryDefaults("harness:\n  type: claude\n  model: team-model\n  auth: {source: managedSecret, secretName: TEAM_KEY}\n")

	// No scorer harness: the harness of agentDefaults with the scorer model.
	def := mustCompile(t, defTree("factory.yaml", claudeDefaults, "scorers/s/scorer.md", scorerMD(scorerBase)))
	h := def.Scorers[0].Harness
	if h == nil || h.Type != core.HarnessClaude || h.Model != "claude-haiku-5-5" || h.Auth == nil || h.Auth.SecretName != "TEAM_KEY" {
		t.Fatalf("judge harness = %+v", h)
	}
	if d := def.ScorerDefaults; d == nil || d.Harness == nil || d.Harness.Model != "team-model" {
		t.Fatalf("resolved scorerDefaults = %+v", d)
	}

	// scorerDefaults sets a harness, a model, a machine, secrets and MCP servers.
	f := claudeDefaults + "scorerDefaults:\n  harness: {type: codex, model: codex-model}\n  machine: linux\n  secrets: [A]\n  mcpServers:\n    docs: {url: 'https://docs.example.com/mcp'}\n"
	noModel := strings.Replace(scorerBase, "model: claude-haiku-5-5\n", "", 1)
	def = mustCompile(t, defTree("factory.yaml", f, "machines/linux.yaml", linuxMachine,
		"scorers/s/scorer.md", scorerMD(noModel+"harness: {type: codex, model: x}\n"),
		"scorers/t/scorer.md", scorerMD(strings.Replace(scorerBase, "name: s", "name: t", 1)+"secrets: [B]\nmcpServers:\n  docs: {url: 'https://other.example.com/mcp'}\nmachine: big\n"),
		"machines/big.yaml", linuxMachine,
	))
	s, tt := def.Scorers[0], def.Scorers[1]
	if s.Harness == nil || s.Harness.Type != core.HarnessCodex || s.Harness.Model != "x" || s.Model != "x" || s.Machine != "linux" {
		t.Fatalf("scorer s = %+v harness %+v", s, s.Harness)
	}
	// The scorer model claude-haiku-5-5 selects the claude harness of the judge.
	if tt.Harness == nil || tt.Harness.Type != core.HarnessClaude || tt.Harness.Model != "claude-haiku-5-5" || tt.Harness.Auth != nil || tt.Machine != "big" {
		t.Fatalf("scorer t = %+v harness %+v", tt, tt.Harness)
	}
	if strings.Join(tt.Secrets, ",") != "A,B" || tt.MCPServers["docs"].URL != "https://other.example.com/mcp" {
		t.Fatalf("scorer t secrets %v mcp %v", tt.Secrets, tt.MCPServers)
	}
	if def.ScorerDefaults.Harness.Type != core.HarnessCodex || def.ScorerDefaults.Machine != "linux" {
		t.Fatalf("scorerDefaults = %+v", def.ScorerDefaults)
	}

	// scorerDefaults.model replaces the model of agentDefaults.
	def = mustCompile(t, defTree("factory.yaml", claudeDefaults+"scorerDefaults:\n  model: claude-sonnet-5-5\n"))
	if h := def.ScorerDefaults.Harness; h.Type != core.HarnessClaude || h.Model != "claude-sonnet-5-5" || h.Auth == nil || def.ScorerDefaults.Model != "claude-sonnet-5-5" {
		t.Fatalf("scorerDefaults.model = %+v", def.ScorerDefaults)
	}

	// A scorer needs model or harness.
	runErrCases(t, []errCase{
		{"no model and no harness", defTree("scorers/s/scorer.md", scorerMD(noModel)), "scorers/s/scorer.md", "model is required", 0},
		{"bad harness type", defTree("scorers/s/scorer.md", scorerMD(scorerBase+"harness: {type: nope, model: m}\n")), "scorers/s/scorer.md", "harness.type must be one of", 0},
	})
}

func TestBenchmarkMachineAndTrackerSeed(t *testing.T) {
	files := defTree("machines/linux.yaml", linuxMachine,
		"benchmarks/s/suite.yaml", suiteBase+"configurations:\n  - name: a\n    machine: linux\n",
		"benchmarks/s/tasks/t1.yaml", taskBase+"trackerSeed: {issues: [{title: x, state: Todo}]}\n")
	def := mustCompile(t, files)
	s := def.Benchmarks[0]
	if s.Configurations[0].Machine != "linux" || s.Tasks[0].TrackerSeed == nil {
		t.Fatalf("suite = %+v", s)
	}
	runErrCases(t, []errCase{
		{"unknown machine", defTree("benchmarks/s/suite.yaml", suiteBase+"configurations:\n  - name: a\n    machine: ghost\n", "benchmarks/s/tasks/t1.yaml", taskBase), "benchmarks/s/suite.yaml", `machine "ghost" does not match`, 0},
		{"both seeds", defTree("benchmarks/s/suite.yaml", suiteBase, "benchmarks/s/tasks/t1.yaml", taskBase+"trackerSeed: {a: 1}\nlinearSeed: {a: 1}\n"), "benchmarks/s/tasks/t1.yaml", "not both", 0},
	})
}

func TestSelfImprovementAnalysisModel(t *testing.T) {
	def := mustCompile(t, defTree("factory.yaml", factoryWith("selfImprovement:\n  analysisModel: deep-model\n")))
	if def.SelfImprovement == nil || def.SelfImprovement.AnalysisModel != "deep-model" {
		t.Fatalf("selfImprovement = %+v", def.SelfImprovement)
	}
}
