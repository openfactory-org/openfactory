package linear

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestPickState(t *testing.T) {
	states := []WorkflowState{
		{ID: "b", Name: "Backlog", Type: "backlog", Position: 0},
		{ID: "t", Name: "Todo", Type: "unstarted", Position: 1},
		{ID: "p", Name: "In Progress", Type: "started", Position: 2},
		{ID: "r", Name: "In Review", Type: "started", Position: 3},
		{ID: "d", Name: "Done", Type: "completed", Position: 4},
		{ID: "x", Name: "Canceled", Type: "canceled", Position: 5},
	}
	todo, prog, review, done := states[1], states[2], states[3], states[4]
	cases := []struct {
		cur   WorkflowState
		state string
		want  string
	}{
		{todo, core.IssueStateInProgress, "p"},
		{prog, core.IssueStateInProgress, ""},
		{todo, core.IssueStateInReview, "r"},
		{review, core.IssueStateInReview, ""},
		{prog, core.IssueStateDone, "d"},
		{done, core.IssueStateDone, ""},
		{done, core.IssueStateInReview, ""},
	}
	for _, c := range cases {
		if got := PickState(states, c.cur, c.state); got != c.want {
			t.Errorf("%s -> %s: got %q, want %q", c.cur.Name, c.state, got, c.want)
		}
	}
	// A team without a review state stays in progress.
	noReview := []WorkflowState{todo, prog, done}
	if got := PickState(noReview, prog, core.IssueStateInReview); got != "" {
		t.Fatalf("got %q", got)
	}
	if got := PickState(noReview, todo, core.IssueStateInReview); got != "p" {
		t.Fatalf("got %q", got)
	}
}
