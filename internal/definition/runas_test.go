package definition

import "testing"

// TestAutomationRunAs covers runAs of an automation.
func TestAutomationRunAs(t *testing.T) {
	const trig = "triggers:\n  - {provider: schedule, event: cron_fired, schedule: {cron: \"@daily\"}}\n"
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"runAs: factory\n", ""},
		{"runAs: \"user:dana@example.com\"\n", "user:dana@example.com"},
		{"runAs: \"serviceAccount:ci\"\n", "serviceAccount:ci"},
	} {
		def := mustCompile(t, defTree("automations/a/automation.md", automationMD(tc.in+trig)))
		if len(def.Automations) != 1 || def.Automations[0].RunAs != tc.want {
			t.Errorf("%q: automations = %+v", tc.in, def.Automations)
		}
	}
	runErrCases(t, []errCase{
		{"bad runAs", defTree("automations/a/automation.md", automationMD("runAs: \"team:x\"\n"+trig)), "automations/a/automation.md", "runAs must be factory", 2},
		{"user without email", defTree("automations/a/automation.md", automationMD("runAs: \"user:dana\"\n"+trig)), "automations/a/automation.md", "runAs must be factory", 2},
	})
}
