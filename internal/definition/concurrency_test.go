package definition

import (
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestScheduleTriggerConcurrency(t *testing.T) {
	trig := func(trigger string) map[string]string {
		return defTree("automations/a/automation.md", automationMD("triggers:\n"+indent(trigger, "  ")))
	}
	def := mustCompile(t, trig("- provider: schedule\n  event: cron_fired\n  concurrency: skip\n  schedule: {name: nightly, cron: \"@daily\"}\n- provider: schedule\n  event: cron_fired\n  schedule: {name: hourly, cron: \"@hourly\"}\n"))
	var a *core.Automation
	for i := range def.Automations {
		if def.Automations[i].Name == "a" {
			a = &def.Automations[i]
		}
	}
	if a == nil || len(a.Triggers) != 2 {
		t.Fatalf("automation %+v", a)
	}
	if a.Triggers[0].Concurrency != core.ScheduleConcurrencySkip || a.Triggers[1].Concurrency != "" {
		t.Fatalf("concurrency = %q, %q", a.Triggers[0].Concurrency, a.Triggers[1].Concurrency)
	}

	for name, c := range map[string]struct{ trigger, want string }{
		"bad value":   {"- provider: schedule\n  event: cron_fired\n  concurrency: queue\n  schedule: {cron: \"@daily\"}\n", "must be allow or skip"},
		"not on push": {"- provider: github\n  event: push\n  concurrency: skip\n", "only valid for schedule/cron_fired"},
	} {
		t.Run(name, func(t *testing.T) {
			_, errs := Compile(trig(c.trigger))
			found := false
			for _, e := range errs {
				found = found || strings.Contains(e.Message, c.want)
			}
			if !found {
				t.Fatalf("errors %+v, want %q", errs, c.want)
			}
		})
	}
}
