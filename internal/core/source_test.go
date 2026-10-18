package core

import "testing"

func TestRunSourceAndRef(t *testing.T) {
	// A stored source wins over TriggeredBy.
	r := &Run{TriggeredBy: "automation:a1", Source: SourceSchedule, SourceRef: "a1"}
	if s, ref := r.SourceAndRef(); s != SourceSchedule || ref != "a1" {
		t.Fatalf("stored = %s %s", s, ref)
	}
	// An old row has no stored source: it falls back to TriggeredBy.
	for triggered, want := range map[string]RunSource{
		"automation:a1": SourceAutomation, "schedule:a1": SourceSchedule, "webhook:a1": SourceWebhook,
		"run:r1": SourceOrchestration, "mention": SourceMention, "": SourceAPI,
	} {
		old := &Run{TriggeredBy: triggered}
		if s, _ := old.SourceAndRef(); s != want {
			t.Errorf("fallback %q = %s, want %s", triggered, s, want)
		}
	}
}

func TestTriggeredByFor(t *testing.T) {
	for provider, want := range map[string]string{
		"schedule": "schedule:a1", "webhook": "webhook:a1", "github": "automation:a1", "": "automation:a1",
	} {
		if got := TriggeredByFor(provider, "a1"); got != want {
			t.Errorf("TriggeredByFor(%q) = %q, want %q", provider, got, want)
		}
	}
	for _, by := range []string{"automation:a1", "schedule:a1", "webhook:a1"} {
		if !StartedByAutomation(by, "a1") || StartedByAutomation(by, "a2") {
			t.Errorf("StartedByAutomation(%q)", by)
		}
	}
	if StartedByAutomation("api", "a1") || StartedByAutomation("automation:", "") {
		t.Error("StartedByAutomation matched a non-automation value")
	}
}
