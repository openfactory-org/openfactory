package definition

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiff(t *testing.T) {
	old := mustCompile(t, defTree(
		"machines/linux.yaml", linuxMachine,
		"agents/reviewer/agent.md", agentMD("agentType: REVIEW\nmodel: claude-sonnet-5-5\n", ""),
		"agents/gone/agent.md", agentMD("", ""),
		"automations/a/automation.md", automationMD("triggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n"),
		"skills/s/SKILL.md", "---\nname: s\ndescription: d\n---\nx\n",
	))
	new := mustCompile(t, defTree(
		"factory.yaml", factoryWith("alias: newalias\n"),
		"machines/linux.yaml", linuxMachine,
		"agents/reviewer/agent.md", agentMD("agentType: REVIEW\nmodel: claude-haiku-5-5\n", "New instructions."),
		"agents/added/agent.md", agentMD("", ""),
		"automations/a/automation.md", automationMD("triggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@hourly\"}}\n"),
		"webhooks/w.yaml", "secretName: S\n",
	))
	d := Diff(old, new)
	got := map[string]Change{}
	for _, c := range d.Changes {
		got[c.Kind+"/"+c.Name] = c
	}
	expect := map[string]string{
		"factory/test":   ChangeChanged,
		"agent/reviewer": ChangeChanged,
		"agent/added":    ChangeAdded,
		"agent/gone":     ChangeRemoved,
		"automation/a":   ChangeChanged,
		"webhook/w":      ChangeAdded,
		"skill/s":        ChangeRemoved,
	}
	for k, action := range expect {
		if got[k].Action != action {
			t.Errorf("%s: action %q, want %q (changes %+v)", k, got[k].Action, action, d.Changes)
		}
	}
	if _, ok := got["agent/foreman"]; ok {
		t.Error("unchanged foreman reported")
	}
	if _, ok := got["machine/linux"]; ok {
		t.Error("unchanged machine reported")
	}
	if f := got["agent/reviewer"].Fields; strings.Join(f, ",") != "harness,instructions" {
		t.Errorf("reviewer fields = %v", f)
	}
	if f := got["factory/test"].Fields; strings.Join(f, ",") != "alias" {
		t.Errorf("factory fields = %v", f)
	}
	a, r, c := d.Counts()
	if a != 2 || r != 2 || c != 3 {
		t.Errorf("counts = %d %d %d", a, r, c)
	}
	sum := d.Summary()
	for _, want := range []string{"2 added, 2 removed, 3 changed", "+ agent added", "- agent gone", "~ agent reviewer (harness, instructions)"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q:\n%s", want, sum)
		}
	}
	if !Diff(old, old).Empty() || Diff(old, old).Summary() != "No changes to the factory definition." {
		t.Error("self diff not empty")
	}
	if n := len(Diff(nil, old).Changes); n == 0 {
		t.Error("diff from nil should add everything")
	}
	if b, err := json.Marshal(d); err != nil || !strings.Contains(string(b), `"action":"added"`) {
		t.Errorf("json = %s %v", b, err)
	}
}

func TestSchemasCatalog(t *testing.T) {
	all := Schemas()
	if len(all) != len(SchemaNames) {
		t.Fatalf("Schemas() = %d documents", len(all))
	}
	for name, raw := range all {
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" || doc["$id"] != SchemaBaseURL+name {
			t.Errorf("%s: $schema/$id = %v %v", name, doc["$schema"], doc["$id"])
		}
	}
	for _, n := range []string{"factory", "factory.schema.json", "v1/factory.schema.json"} {
		if _, ok := Schema(n); !ok {
			t.Errorf("Schema(%q) not found", n)
		}
	}
	if _, ok := Schema("nope"); ok {
		t.Error("unknown schema found")
	}
	if v := SchemaVersions(); v.Current != "v1" || len(v.Versions) != 1 {
		t.Errorf("versions = %+v", v)
	}
	if _, err := compiledSchemas(); err != nil {
		t.Fatal(err)
	}
}

func TestSchemaForPath(t *testing.T) {
	for p, want := range map[string]string{
		"factory.yaml":                "factory",
		"agents/a/agent.md":           "agent",
		"automations/a/automation.md": "automation",
		"scorers/s/scorer.md":         "scorer",
		"machines/r.yaml":             "machine",
		"routers/r.yaml":              "",
		"webhooks/w.yaml":             "webhook",
		"benchmarks/b/suite.yaml":     "benchmark_suite",
		"benchmarks/b/tasks/t.yaml":   "benchmark_suite_task",
		"skills/s/SKILL.md":           "",
		"README.md":                   "",
		"agents/a/skills/s/SKILL.md":  "",
	} {
		if got := SchemaForPath(p); got != want {
			t.Errorf("SchemaForPath(%q) = %q, want %q", p, got, want)
		}
	}
}

func TestSchemasRejectInvalidDocuments(t *testing.T) {
	cases := []struct {
		path, content string
	}{
		{"factory.yaml", baseFactory + "bogus: 1\n"},
		{"factory.yaml", "schemaVersion: v2\nname: x\nrepositories: []\nagentDefaults: {model: m}\n"},
		{"factory.yaml", baseFactoryHead + "agentDefaults: {model: a, harness: {type: codex, model: b}}\n"},
		{"factory.yaml", baseFactoryHead + "agentDefaults: {harness: {type: open, model: b, reasoningLevel: high}}\n"},
		{"factory.yaml", baseFactory + "mcpServers:\n  s: {command: x, url: \"https://a\"}\n"},
		{"factory.yaml", baseFactory + "mcpServers:\n  factory: {command: x}\n"},
		{"factory.yaml", baseFactory + "networkPolicy:\n  allowedHosts: [\"https://x.com\"]\n"},
		{"factory.yaml", baseFactory + "selfImprovement: {reviewerType: custom}\n"},
		{"agents/a/agent.md", agentMD("agentType: BOSS\n", "")},
		{"agents/a/agent.md", agentMD("unknown: 1\n", "")},
		{"agents/a/agent.md", agentMD("model: a\nharness: {model: b}\n", "")},
		{"agents/a/agent.md", agentMD("idleTimeoutMinutes: 61\n", "")},
		{"automations/a/automation.md", automationMD("triggers: []\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: github, event: push, filter: {labels: [x]}}\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: github, event: nope}\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: schedule, event: cron_fired}\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}, filter: {}}\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: webhook, event: received, filter: {webhook_ids: [a, b]}}\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: teams, event: app_mention}\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: linear, event: issue_labeled, filter: {teams: [a], team_ids: [\"2f1c1a5e-8f0a-4c55-9e1c-3d2b7a9e4f10\"]}}\n")},
		{"automations/a/automation.md", automationMD("triggers:\n  - {provider: github, event: push, filter: {skip_bots: some}}\n")},
		{"machines/r.yaml", "platform: {os: linux}\n"},
		{"machines/r.yaml", "platform: {os: macos, arch: x86_64}\n"},
		{"machines/r.yaml", "platform: {os: macos, mac: {version: \"13\"}}\n"},
		{"machines/r.yaml", "instanceShape: {vcpus: 4}\n" + linuxMachine},
		{"webhooks/w.yaml", "authMode: signature\nsecretName: S\n"},
		{"webhooks/w.yaml", "signatureScheme: github\nsecretName: S\n"},
		{"webhooks/w.yaml", "secretName: S\ndeliveryIdHeader: \"bad header\"\n"},
		{"scorers/s/scorer.md", scorerMD(scorerBase + "samplingRate: 200\n")},
		{"scorers/s/scorer.md", scorerMD(strings.Replace(scorerBase, "name: s\n", "", 1))},
		{"benchmarks/b/suite.yaml", "name: S\nagent: a\nconfigurations: []\n"},
		{"benchmarks/b/suite.yaml", "name: S\nagent: a\nconfigurations: [{name: c, agents: [{agent: a}]}]\n"},
		{"benchmarks/b/tasks/t.yaml", "title: t\nprompt: p\n"},
		{"benchmarks/b/tasks/t.yaml", taskBase + "startingRepoRefs: [\"github.com:a/b@main\"]\n"},
	}
	for _, tc := range cases {
		if err := ValidateSchema(tc.path, tc.content); err == nil {
			t.Errorf("schema accepted invalid %s:\n%s", tc.path, tc.content)
		}
	}
	// Valid documents pass, including Markdown without frontmatter.
	valid := map[string]string{
		"factory.yaml":      baseFactory,
		"agents/a/agent.md": "Just instructions.\n",
		"skills/s/SKILL.md": "anything",
	}
	for p, c := range valid {
		if err := ValidateSchema(p, c); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := ValidateSchema("factory.yaml", "a: [\n"); err == nil {
		t.Error("invalid YAML accepted")
	}
}
