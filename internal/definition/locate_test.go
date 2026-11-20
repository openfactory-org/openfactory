package definition

import "testing"

func TestKeyLine(t *testing.T) {
	agent := "---\ndescription: x\nenvironmentId: env_x\nmcpServers:\n  docs:\n    managedId: m\n---\nBody\n"
	auto := "---\ntriggers:\n  - provider: webhook\n    event: received\n    filter:\n      webhook_ids: [a]\n  - provider: webhook\n    event: received\n    filter:\n      webhook_ids:\n        in:\n          - b\n---\nGo.\n"
	yml := "agentDefaults:\n  environmentId: env_y\nsecrets: [A, B]\n"
	for _, tc := range []struct {
		name, file, content, path string
		want                      int
	}{
		{"frontmatter key", "agents/a/agent.md", agent, "environmentId", 3},
		{"nested key", "agents/a/agent.md", agent, "mcpServers.docs.managedId", 6},
		{"list index", "automations/x/automation.md", auto, "triggers[0].filter.webhook_ids[0]", 6},
		{"in list item", "automations/x/automation.md", auto, "triggers[1].filter.webhook_ids.in[0]", 12},
		{"yaml file", "factory.yaml", yml, "agentDefaults.environmentId", 2},
		{"yaml list key", "factory.yaml", yml, "secrets", 3},
		{"missing key", "factory.yaml", yml, "nope", 0},
		{"index out of range", "factory.yaml", yml, "secrets[5]", 0},
		{"bad path", "factory.yaml", yml, "secrets[x]", 0},
		{"empty path", "factory.yaml", yml, "", 0},
		{"no frontmatter", "agents/a/agent.md", "just text\n", "environmentId", 0},
		{"bad yaml", "factory.yaml", "a: [\n", "a", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := KeyLine(tc.file, tc.content, tc.path); got != tc.want {
				t.Errorf("KeyLine(%q) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}
