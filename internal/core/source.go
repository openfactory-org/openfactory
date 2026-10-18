package core

import "strings"

// RunSource is the closed list of run sources of the API. Metrics and
// filters group by it, so it never holds a free
// string.
type RunSource string

const (
	SourceAPI             RunSource = "API"
	SourceMCP             RunSource = "MCP"
	SourceDashboard       RunSource = "DASHBOARD"
	SourceCLI             RunSource = "CLI"
	SourceAutomation      RunSource = "AUTOMATION"
	SourceSchedule        RunSource = "SCHEDULE"
	SourceWebhook         RunSource = "WEBHOOK"
	SourceMention         RunSource = "MENTION"
	SourceOrchestration   RunSource = "ORCHESTRATION"
	SourceScorer          RunSource = "SCORER"
	SourceBenchmark       RunSource = "BENCHMARK"
	SourceSelfImprovement RunSource = "SELF_IMPROVEMENT"
)

// RunSources lists each RunSource in the order of the API.
var RunSources = []RunSource{
	SourceAPI, SourceMCP, SourceDashboard, SourceCLI, SourceAutomation, SourceSchedule,
	SourceWebhook, SourceMention, SourceOrchestration, SourceScorer, SourceBenchmark, SourceSelfImprovement,
}

// RunSourceName validates a source name of a filter. The match ignores
// case. It returns false for a name that is not in RunSources.
func RunSourceName(v string) (RunSource, bool) {
	s := RunSource(strings.ToUpper(strings.TrimSpace(v)))
	for _, k := range RunSources {
		if k == s {
			return s, true
		}
	}
	return "", false
}

// RunSourceOf maps the stored Run.TriggeredBy value to the API source and
// the source reference (the ID of the automation, schedule, webhook, parent
// run, scorer or benchmark). The stored values are "api", "mcp",
// "dashboard", "cli", "mention", "follow_up", "self_improvement",
// "automation:<id>", "schedule:<id>", "webhook:<id>", "run:<id>",
// "scorer:<id>" and "benchmark:<id>". An empty or unknown value is API.
func RunSourceOf(triggeredBy string) (RunSource, string) {
	kind, ref, _ := strings.Cut(strings.TrimSpace(triggeredBy), ":")
	switch strings.ToLower(kind) {
	case "mcp":
		return SourceMCP, ""
	case "dashboard":
		return SourceDashboard, ""
	case "cli":
		return SourceCLI, ""
	case "mention", "follow_up":
		return SourceMention, ""
	case "self_improvement":
		return SourceSelfImprovement, ref
	case "automation":
		return SourceAutomation, ref
	case "schedule":
		return SourceSchedule, ref
	case "webhook":
		return SourceWebhook, ref
	case "run":
		return SourceOrchestration, ref
	case "scorer":
		return SourceScorer, ref
	case "benchmark":
		return SourceBenchmark, ref
	}
	return SourceAPI, ""
}

// SourceAndRef returns the source of the run and its source reference. It
// reads the stored Source and SourceRef. When Source is empty (a run from
// before the stored field), it derives both from TriggeredBy.
func (r *Run) SourceAndRef() (RunSource, string) {
	if r.Source != "" {
		return r.Source, r.SourceRef
	}
	return RunSourceOf(r.TriggeredBy)
}

// TriggeredByFor returns the Run.TriggeredBy value of a run that an
// automation starts for event provider. A schedule event gives
// "schedule:<id>", a custom webhook event gives "webhook:<id>", and each
// other event gives "automation:<id>".
func TriggeredByFor(provider, automationID string) string {
	switch provider {
	case "schedule":
		return "schedule:" + automationID
	case "webhook":
		return "webhook:" + automationID
	}
	return "automation:" + automationID
}

// StartedByAutomation reports whether a TriggeredBy value names the
// automation. The prefix depends on the event provider (TriggeredByFor), so
// the check accepts each of the three prefixes.
func StartedByAutomation(triggeredBy, automationID string) bool {
	if automationID == "" {
		return false
	}
	for _, p := range []string{"automation:", "schedule:", "webhook:"} {
		if triggeredBy == p+automationID {
			return true
		}
	}
	return false
}
