package scoring

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestImprovementPullRequestReview shows the review of a Self-improvement
// pull request: no item at the start, one item and one review request
// when the run has linked an open pull request, and a note when the forge
// refuses the request.
func TestImprovementPullRequestReview(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.st.factories[testFactory].Source = core.DefinitionGit
	e.st.factories[testFactory].GitRepo = "github.com/acme/def"
	e.addScorer(core.Scorer{ID: "sc_a", Slug: "a", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100, SelfImprovement: true})
	var asked []string
	var askErr error
	e.svc.opts.RequestReview = func(_ context.Context, teamID string, pr core.PullRequestRef, userID string) error {
		asked = append(asked, userID+"@"+pr.Repo)
		return askErr
	}
	if err := e.svc.HandleRunUpdate(ctx, e.finishedRun("run_1", "implement", core.RunSucceeded, "nope")); err != nil {
		t.Fatal(err)
	}
	runs, err := e.svc.TriggerSelfImprovement(ctx, testFactory, "")
	if err != nil || len(runs) != 1 {
		t.Fatalf("trigger: %v %v", runs, err)
	}
	imp := runs[0]
	imp.State = core.RunIdle // the agent ended its turn
	e.st.putRun(imp)
	if len(e.st.inbox) != 0 || len(asked) != 0 {
		t.Fatalf("at the start: inbox %d, requests %v", len(e.st.inbox), asked)
	}
	// A run update without a pull request creates nothing.
	if err := e.svc.HandleRunUpdate(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if len(e.st.inbox) != 0 {
		t.Fatalf("inbox without a pull request: %d", len(e.st.inbox))
	}
	// A merged pull request is not reviewed. An open one is.
	wi := &core.WorkItem{ID: imp.WorkItemID, FactoryID: testFactory, PullRequests: []core.PullRequestRef{
		{Forge: "GITHUB", Repo: "acme/def", Number: 6, URL: "https://github.com/acme/def/pull/6", State: "merged"},
		{Forge: "GITHUB", Repo: "acme/def", Number: 7, URL: "https://github.com/acme/def/pull/7", State: "open"},
	}}
	e.st.putWorkItem(wi)
	if err := e.svc.HandleRunUpdate(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0] != "u_admin@acme/def" || len(e.st.inbox) != 1 {
		t.Fatalf("requests %v, inbox %d", asked, len(e.st.inbox))
	}
	if !strings.Contains(e.st.inbox[0].Body, "A review is requested from the reviewer") {
		t.Fatalf("body: %s", e.st.inbox[0].Body)
	}
	// A second pull request whose review request fails still gets its item.
	askErr = errors.New("user has no linked account")
	wi.PullRequests = append(wi.PullRequests, core.PullRequestRef{Forge: "GITHUB", Repo: "acme/app", Number: 3, URL: "https://github.com/acme/app/pull/3", State: "draft"})
	e.st.putWorkItem(wi)
	if err := e.svc.HandleRunUpdate(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if len(e.st.inbox) != 2 || !strings.Contains(e.st.inbox[1].Body, "could not be requested") || !strings.Contains(e.st.inbox[1].Body, "no linked account") {
		t.Fatalf("inbox %+v", e.st.inbox)
	}
	if len(asked) != 2 {
		t.Fatalf("requests %v", asked)
	}
}
