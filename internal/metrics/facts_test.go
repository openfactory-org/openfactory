package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func prEvent(typ string, actor core.Actor, at time.Time, attrs map[string]any) *core.Event {
	if attrs == nil {
		attrs = map[string]any{}
	}
	return &core.Event{TeamID: "team1", Provider: "github", Type: typ, Actor: actor, ReceivedAt: at, Attributes: attrs,
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/app", Number: 1, URL: "https://x/1"}}
}

var (
	agent  = core.Actor{Handle: "of[bot]", IsBot: true, IsSelf: true}
	person = core.Actor{Handle: "dana"}
	ci     = core.Actor{Handle: "ci[bot]", IsBot: true}
)

func TestRecordEvent(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	t0 := at(0, 1)
	steps := []*core.Event{
		prEvent("pull_request_opened", agent, t0, map[string]any{"head_branch": "fix", "additions": 30, "deletions": 5}),
		// A push by a bot to the head branch, reported twice (push and synchronize).
		{TeamID: "team1", Provider: "github", Type: "push", Actor: ci, ReceivedAt: t0.Add(time.Hour),
			Source: core.SourceRef{Provider: "github", Kind: "push", Repo: "acme/app"}, Attributes: map[string]any{"branch": "fix", "head_sha": "s1"}},
		prEvent("pull_request_synchronized", ci, t0.Add(time.Hour), map[string]any{"head_sha": "s1"}),
		// The opener's own review does not count; a reviewer's does.
		prEvent("pull_request_review_submitted", agent, t0.Add(2*time.Hour), nil),
		prEvent("pull_request_review_submitted", person, t0.Add(3*time.Hour), nil),
		prEvent("pull_request_review_submitted", person, t0.Add(4*time.Hour), nil),
		prEvent("pull_request_merged", person, t0.Add(5*time.Hour), map[string]any{"merged_at": t0.Add(5 * time.Hour).Format(time.RFC3339)}),
		// A push after the merge is not added: the PR is not open.
		{TeamID: "team1", Provider: "github", Type: "push", Actor: person, ReceivedAt: t0.Add(6 * time.Hour),
			Source: core.SourceRef{Provider: "github", Kind: "push", Repo: "acme/app"}, Attributes: map[string]any{"branch": "fix", "head_sha": "s2"}},
		// Another provider, and an event without a team, are skipped.
		{TeamID: "team1", Provider: "slack", Type: "message", Source: core.SourceRef{Provider: "slack", Kind: "pull_request", Repo: "x", Number: 9}},
	}
	for _, ev := range steps {
		if err := RecordEvent(ctx, st, ev, ev.ReceivedAt); err != nil {
			t.Fatal(err)
		}
	}
	f, err := st.GetPullRequestFacts(ctx, core.PullRequestFactsID("team1", "github:acme/app#1"))
	if err != nil {
		t.Fatal(err)
	}
	if f.OpenerKind != core.PushByAgent || f.OpenedAt == nil || !f.OpenedAt.Equal(t0) || f.HeadBranch != "fix" {
		t.Fatalf("opener = %+v", f)
	}
	if len(f.Pushes) != 1 || f.Pushes[0].Kind != core.PushByBot || f.Pushes[0].SHA != "s1" {
		t.Fatalf("pushes = %+v", f.Pushes)
	}
	if f.FirstReviewAt == nil || !f.FirstReviewAt.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("first review = %v", f.FirstReviewAt)
	}
	if f.MergedAt == nil || !f.MergedAt.Equal(t0.Add(5*time.Hour)) || f.Open() {
		t.Fatalf("merged = %v", f.MergedAt)
	}
	if n, ok := f.ChangedLines(); !ok || n != 35 {
		t.Fatalf("lines = %d %v", n, ok)
	}
	if f.PersonPushedBefore(*f.MergedAt) {
		t.Fatal("a person pushed")
	}
	all, _ := st.ListPullRequestFacts(ctx, core.PullRequestFactFilter{})
	if len(all) != 1 {
		t.Fatalf("facts = %d", len(all))
	}
}

func TestBuildWithFacts(t *testing.T) {
	h := int64(time.Hour / time.Millisecond)
	fx := fixture()
	adds, dels := 5, 2
	big := 400
	fx.Facts = map[string]core.PullRequestFacts{
		// pr1: opened by the agent, a bot push, reviewed and merged: autonomous.
		PRFactsKey(core.PullRequestRef{Forge: "GITHUB", Repo: "acme/app", Number: 1}): {
			Key: "github:acme/app#1", OpenerKind: core.PushByAgent, OpenedAt: ptr(at(0, 4)), FirstReviewAt: ptr(at(0, 6)), MergedAt: ptr(at(0, 9)),
			Pushes: []core.PullRequestPush{{At: at(0, 5), Kind: core.PushByBot}}, Additions: &adds, Deletions: &dels,
		},
		// pr2: a person pushed before the merge: not autonomous.
		PRFactsKey(core.PullRequestRef{Forge: "GITHUB", Repo: "Acme/App", Number: 2}): {
			Key: "github:acme/app#2", OpenerKind: core.PushByAgent, OpenedAt: ptr(at(1, 8)), MergedAt: ptr(at(1, 18)),
			Pushes: []core.PullRequestPush{{At: at(1, 10), Kind: core.PushByPerson}}, Additions: &big,
		},
	}
	d := Build("fac", fx, since, until)
	a := d.Autonomy
	if a.MergedPRs != 2 || a.AutonomousMergedPRs != 1 || a.PRRate != 0.5 || a.MergedPRsWithoutFacts != 0 {
		t.Fatalf("autonomy = %+v", a)
	}
	if a.FactsSince == nil || !a.FactsSince.Equal(at(0, 4)) {
		t.Fatalf("facts since = %v", a.FactsSince)
	}
	// The work-item figure is unchanged.
	if a.CompletedWorkItems != 5 || a.AutonomousWorkItems != 3 {
		t.Fatalf("work item autonomy = %+v", a)
	}
	pc := d.CycleTime.PullRequests
	// pr1: start 0h, PR 4h, review 6h, merge 9h. pr2: start 24h, PR 32h, merge 42h.
	if pc.Count != 2 || pc.Reviewed != 1 || pc.StartToPRMS != 6*h || pc.PRToFirstReviewMS != 2*h || pc.FirstReviewToMergeMS != 3*h || pc.StartToMergeMS != (9*h+18*h)/2 {
		t.Fatalf("pr cycle = %+v", pc)
	}
	sizes := map[string]PRSizeCost{}
	for _, b := range d.PullRequests.BySize {
		sizes[b.Size] = b
	}
	// pr1 is xs, pr2 is l, pr3 and pr4 have no facts.
	if sizes["xs"].Count != 1 || sizes["l"].Count != 1 || sizes["unknown"].Count != 2 || sizes["xl"].Lines != "1000+" || sizes["s"].Lines != "10-49" {
		t.Fatalf("sizes = %+v", d.PullRequests.BySize)
	}
	if !approxEq(sizes["xs"].TotalCostUSD, 3) {
		t.Fatalf("xs cost = %v", sizes["xs"].TotalCostUSD)
	}
}

func TestComputeLoadsFacts(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	if err := st.CreateFactory(ctx, &core.Factory{ID: "fac", TeamID: "team1", Name: "f", CreatedAt: at(0, 0)}); err != nil {
		t.Fatal(err)
	}
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/app", Number: 1, State: "merged"}
	if err := st.CreateWorkItem(ctx, &core.WorkItem{ID: "w1", FactoryID: "fac", Stage: core.StageComplete, CreatedAt: at(0, 0), UpdatedAt: at(0, 9),
		CompletedAt: ptr(at(0, 9)), PullRequests: []core.PullRequestRef{pr}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutPullRequestFacts(ctx, &core.PullRequestFacts{TeamID: "team1", Key: "github:acme/app#1", OpenerKind: core.PushByAgent,
		OpenedAt: ptr(at(0, 2)), MergedAt: ptr(at(0, 8))}); err != nil {
		t.Fatal(err)
	}
	d, err := Compute(ctx, st, "fac", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if d.Autonomy.MergedPRs != 1 || d.Autonomy.AutonomousMergedPRs != 1 || d.CycleTime.PullRequests.StartToMergeMS != int64(8*time.Hour/time.Millisecond) {
		t.Fatalf("autonomy = %+v cycle = %+v", d.Autonomy, d.CycleTime.PullRequests)
	}
}
