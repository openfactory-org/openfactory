package jira

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func transition(id, name, to, cat string) jiraTransition {
	var t jiraTransition
	t.ID, t.Name, t.To.Name, t.To.StatusCategory.Key = id, name, to, cat
	return t
}

func TestPickTransition(t *testing.T) {
	ts := []jiraTransition{
		transition("11", "Start", "In Progress", "indeterminate"),
		transition("21", "Review", "Code Review", "indeterminate"),
		transition("31", "Close", "Done", "done"),
	}
	if got := pickTransition(ts, core.IssueStateInProgress); got != "11" {
		t.Errorf("in progress = %q", got)
	}
	if got := pickTransition(ts, core.IssueStateInReview); got != "21" {
		t.Errorf("in review = %q", got)
	}
	if got := pickTransition(ts, core.IssueStateDone); got != "31" {
		t.Errorf("done = %q", got)
	}
	if got := pickTransition(ts[:1], core.IssueStateInReview); got != "" {
		t.Errorf("review without a review status = %q", got)
	}
}
