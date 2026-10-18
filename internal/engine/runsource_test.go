package engine

import (
	"context"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestStoredRunSource checks that each run writer stores the source:
// a schedule automation gives SCHEDULE, a webhook automation gives WEBHOOK,
// another automation gives AUTOMATION, a mention gives MENTION.
func TestStoredRunSource(t *testing.T) {
	ctx := context.Background()
	env := NewTestEnv(t, func(d *core.CompiledDefinition) {
		d.Automations = append(d.Automations,
			core.Automation{Name: "nightly", Enabled: true, Prompt: "Run the nightly check.",
				Triggers: []core.Trigger{{Provider: "schedule", Event: "cron_fired"}}},
			core.Automation{Name: "ci-hook", Enabled: true, Prompt: "Look at the delivery.",
				Triggers: []core.Trigger{{Provider: "webhook", Event: "webhook_received"}}},
		)
	})
	autoID := func(name string) string {
		autos, err := env.Store.ListAutomations(ctx, env.Factory.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range autos {
			if a.Name == name {
				return a.ID
			}
		}
		t.Fatalf("automation %s not found", name)
		return ""
	}
	event := func(provider, typ string) *core.Event {
		return &core.Event{
			ID: core.NewID("evt"), TeamID: "team1", Provider: provider, Type: typ, DeliveryID: provider + "-1",
			Actor:      core.Actor{Provider: provider, Handle: provider},
			Source:     core.SourceRef{Provider: provider, Kind: provider, ExternalID: provider + "-1", Title: provider},
			ReceivedAt: time.Now(),
		}
	}
	tests := []struct {
		name    string
		ev      *core.Event
		auto    string
		wantSrc core.RunSource
		wantBy  string
	}{
		{"schedule", event("schedule", "cron_fired"), "nightly", core.SourceSchedule, "schedule:"},
		{"webhook", event("webhook", "webhook_received"), "ci-hook", core.SourceWebhook, "webhook:"},
		{"issue automation", IssueEvent("issue_labeled", "bug", "maintainer"), "issue-intake", core.SourceAutomation, "automation:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runs, err := env.Engine.HandleEvent(ctx, tc.ev)
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs = %v, err = %v", runs, err)
			}
			got, err := env.Store.GetRun(ctx, runs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			id := autoID(tc.auto)
			if got.Source != tc.wantSrc || got.SourceRef != id || got.TriggeredBy != tc.wantBy+id {
				t.Fatalf("source = %q ref = %q triggered_by = %q, want %q / %q / %q", got.Source, got.SourceRef, got.TriggeredBy, tc.wantSrc, id, tc.wantBy+id)
			}
		})
	}

	// A mention and an API dispatch store their source too.
	menv := NewTestEnv(t)
	runs, err := menv.Engine.HandleEvent(ctx, mentionEvent("maintainer"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("mention runs = %v, err = %v", runs, err)
	}
	if got, _ := menv.Store.GetRun(ctx, runs[0].ID); got.Source != core.SourceMention || got.SourceRef != "" {
		t.Fatalf("mention source = %q ref = %q", got.Source, got.SourceRef)
	}
	for by, want := range map[string]core.RunSource{"cli": core.SourceCLI, "dashboard": core.SourceDashboard, "mcp": core.SourceMCP, "": core.SourceAPI} {
		_, run, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "Do it " + by, TriggeredBy: by})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := env.Store.GetRun(ctx, run.ID); got.Source != want {
			t.Fatalf("dispatch by %q: source = %q, want %q", by, got.Source, want)
		}
	}
}
