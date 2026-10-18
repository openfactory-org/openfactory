package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func trackerNotices(env *TestEnv) []core.OutboundMessage {
	var out []core.OutboundMessage
	for _, m := range env.Bus.Notices() {
		if core.IsTrackerOperation(m.Kind) {
			out = append(out, m)
		}
	}
	return out
}

// TestLinkIssueWriteBack checks the tracker write-back: a linked issue
// gets the pull requests of the work item and moves to in review, and to
// done when the work item completes. Events on the issue route to the work
// item.
func TestLinkIssueWriteBack(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	issue := core.SourceRef{Provider: "linear", Kind: "issue", ExternalID: "ENG-12", URL: "https://linear.app/acme/issue/ENG-12"}
	if err := env.Engine.LinkIssue(ctx, root.WorkItemID, issue); err != nil {
		t.Fatal(err)
	}
	// A second link of the same issue changes nothing.
	if err := env.Engine.LinkIssue(ctx, root.WorkItemID, issue); err != nil {
		t.Fatal(err)
	}
	ops := trackerNotices(env)
	if len(ops) != 1 || ops[0].Kind != core.OutboundIssueState || ops[0].IssueState != core.IssueStateInProgress || ops[0].Source.ExternalID != "ENG-12" {
		t.Fatalf("ops after link = %+v", ops)
	}
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if !wi.HasRef("linear:ENG-12") {
		t.Fatalf("refs = %+v", wi.Refs())
	}

	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 42, URL: "https://github.com/acme/api/pull/42"}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
		t.Fatal(err)
	}
	ops = trackerNotices(env)[1:]
	if len(ops) != 2 || ops[0].Kind != core.OutboundAttachPullRequest || ops[0].PullRequest == nil || ops[0].PullRequest.Number != 42 ||
		ops[1].IssueState != core.IssueStateInReview {
		t.Fatalf("ops after pull request = %+v", ops)
	}
	wi, _ = env.Store.GetWorkItem(ctx, root.WorkItemID)
	if !wi.HasRef("github:acme/api#42") {
		t.Fatalf("pull request is not a reference: %+v", wi.Refs())
	}

	// A comment on the linked issue routes to the work item.
	ev := &core.Event{ID: "lc", TeamID: "team1", Provider: "linear", Type: "issue_commented", Text: "Also cover refunds",
		Actor: core.Actor{Handle: "dana"}, Source: issue}
	found, err := env.Engine.findActiveWorkItem(ctx, env.Factory.ID, ev.Source.Key())
	if err != nil || found == nil || found.ID != root.WorkItemID {
		t.Fatalf("issue does not route to the work item: %v %v", found, err)
	}

	// The merge completes the work item: the issue moves to done.
	if err := env.Engine.CompleteWorkItem(ctx, root.WorkItemID, "system", "merged"); err != nil {
		t.Fatal(err)
	}
	ops = trackerNotices(env)
	last := ops[len(ops)-1]
	if last.Kind != core.OutboundIssueState || last.IssueState != core.IssueStateDone || last.Source.ExternalID != "ENG-12" {
		t.Fatalf("last op = %+v", last)
	}
	// A completed work item cannot get a new issue.
	if err := env.Engine.LinkIssue(ctx, root.WorkItemID, core.SourceRef{Provider: "jira", ExternalID: "OPS-1"}); err == nil {
		t.Fatal("link to a completed work item succeeded")
	}
	if err := env.Engine.LinkIssue(ctx, root.WorkItemID, core.SourceRef{Provider: "jira"}); err == nil {
		t.Fatal("link without a key succeeded")
	}
}

// TestTrackerSourceWriteBack checks that a work item that started on a
// tracker issue writes back to that issue, and that a GitHub issue source
// does not (the forge closes it from the pull request).
func TestTrackerSourceWriteBack(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	_, run, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "work",
		Source: core.SourceRef{Provider: "jira", Kind: "issue", ExternalID: "OPS-7"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.CompleteWorkItem(ctx, run.WorkItemID, "system", ""); err != nil {
		t.Fatal(err)
	}
	ops := trackerNotices(env)
	if len(ops) != 1 || ops[0].Source.ExternalID != "OPS-7" || ops[0].IssueState != core.IssueStateDone {
		t.Fatalf("ops = %+v", ops)
	}

	root := foremanRun(t, env) // GitHub issue source
	if err := env.Engine.CompleteWorkItem(ctx, root.WorkItemID, "system", ""); err != nil {
		t.Fatal(err)
	}
	if n := len(trackerNotices(env)); n != 1 {
		t.Fatalf("GitHub issue source got tracker operations: %d", n)
	}
}

// TestReplyInReviewThread checks that a reply to a review comment goes into
// its review thread, and that the completion notice is a top-level comment.
func TestReplyInReviewThread(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 42, URL: "https://github.com/acme/api/pull/42"}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
		t.Fatal(err)
	}
	ev := &core.Event{ID: "rc", TeamID: "team1", Provider: "github", Type: "pull_request_commented", Text: "Rename this variable",
		Actor:  core.Actor{Handle: "rev", UserID: "usr_rev"},
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 42, ThreadRef: "9001"}}
	if _, err := env.Engine.HandleEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.PostUpdate(ctx, root.ID, "Renamed it.", "update"); err != nil {
		t.Fatal(err)
	}
	notices := env.Bus.Notices()
	reply := notices[len(notices)-1]
	if reply.ThreadRef != "9001" || reply.Source.Number != 42 || reply.Source.Kind != "pull_request" {
		t.Fatalf("reply = %+v", reply)
	}
	if err := env.Engine.CompleteWorkItem(ctx, root.WorkItemID, "system", ""); err != nil {
		t.Fatal(err)
	}
	notices = env.Bus.Notices()
	done := notices[len(notices)-1]
	if done.ThreadRef != "" || done.Source.Number != 7 {
		t.Fatalf("completion notice = %+v", done)
	}
}

// TestReopenCompletedWorkItem checks that text for a reference of a recently
// completed work item opens it again, and that an old one stays closed.
func TestReopenCompletedWorkItem(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	env.Engine.clock = func() time.Time { return now }
	root := foremanRun(t, env)
	if err := env.Engine.CompleteWorkItem(ctx, root.WorkItemID, "system", ""); err != nil {
		t.Fatal(err)
	}
	// A reply after completion, within the window.
	now = now.Add(48 * time.Hour)
	runs, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "This still fails on refunds", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageTriage || wi.CompletedAt != nil {
		t.Fatalf("work item = %+v", wi)
	}
	if len(runs) != 1 || runs[0].ID != root.ID {
		t.Fatalf("runs = %v", runs)
	}
	evs, _ := env.Store.ListWorkItemEvents(ctx, wi.ID)
	reopened := false
	for _, e := range evs {
		reopened = reopened || e.Kind == "reopened"
	}
	if !reopened {
		t.Fatal("no reopened timeline entry")
	}

	// Complete again, and reply after the window: no reopen.
	if err := env.Engine.CompleteWorkItem(ctx, root.WorkItemID, "system", ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(ReopenWindow + time.Hour)
	if _, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "One more thing", "alice")); err != nil {
		t.Fatal(err)
	}
	wi, _ = env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageComplete {
		t.Fatalf("old work item reopened: %s", wi.Stage)
	}
}

// TestHandleEventRecordsPullRequestFacts checks that the engine records the
// facts of a pull request that its own bot opened, before it drops the event.
func TestHandleEventRecordsPullRequestFacts(t *testing.T) {
	st := newTestStore(t)
	eng := New(Options{Store: st, Bus: NewFakeBus(), Logger: DiscardLogger()})
	ev := &core.Event{TeamID: "team1", Provider: "github", Type: "pull_request_opened", ReceivedAt: time.Now(),
		Actor:  core.Actor{Handle: "of[bot]", IsBot: true, IsSelf: true},
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 9}}
	if _, err := eng.HandleEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	f, err := st.GetPullRequestFacts(context.Background(), core.PullRequestFactsID("team1", "github:acme/api#9"))
	if err != nil || f.OpenerKind != core.PushByAgent || f.OpenedAt == nil {
		t.Fatalf("facts = %+v, %v", f, err)
	}
}

// TestMergedPullRequestMovesIssueToDone checks that the
// merge of the last pull request completes the work item, and the linked
// tracker issue moves to done.
func TestMergedPullRequestMovesIssueToDone(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	issue := core.SourceRef{Provider: "jira", Kind: "issue", ExternalID: "OPS-12", Channel: "OPS"}
	if err := env.Engine.LinkIssue(ctx, root.WorkItemID, issue); err != nil {
		t.Fatal(err)
	}
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 51, URL: "https://github.com/acme/api/pull/51"}
	if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
		t.Fatal(err)
	}
	r, _ := env.Store.GetRun(ctx, root.ID)
	r.State, r.Turns = core.RunSucceeded, 1
	_ = env.Store.UpdateRun(ctx, r)
	merged := &core.Event{ID: "mm", TeamID: "team1", Provider: "github", Type: "pull_request_merged", Actor: core.Actor{Handle: "dana"},
		Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 51, URL: pr.URL}}
	if _, err := env.Engine.HandleEvent(ctx, merged); err != nil {
		t.Fatal(err)
	}
	for _, op := range trackerNotices(env) {
		if op.IssueState == core.IssueStateDone {
			t.Fatalf("issue is done before the foreman turn ended: %+v", op)
		}
	}
	// The turn that the merge started ends without a completion: the engine
	// completes the work item.
	r, _ = env.Store.GetRun(ctx, root.ID)
	r.State, r.Turns = core.RunSucceeded, 2
	_ = env.Store.UpdateRun(ctx, r)
	env.Engine.onRunUpdated(ctx, r)
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if wi.Stage != core.StageComplete {
		t.Fatalf("stage = %s", wi.Stage)
	}
	ops := trackerNotices(env)
	last := ops[len(ops)-1]
	if last.Kind != core.OutboundIssueState || last.IssueState != core.IssueStateDone || last.Source.ExternalID != "OPS-12" {
		t.Fatalf("last tracker operation = %+v (all: %+v)", last, ops)
	}
}

// kvFailStore fails each KVGet with a store error that is not "not found".
type kvFailStore struct{ *testStore }

func (kvFailStore) KVGet(context.Context, string) (string, error) {
	return "", errors.New("store unavailable")
}

// A store error on the alias lookup is returned, not read as "no work item";
// an alias to a work item that is gone is "no work item".
func TestFindActiveWorkItemStoreErrors(t *testing.T) {
	ctx := context.Background()
	fs := newTestStore(t)
	eng := New(Options{Store: kvFailStore{fs}, Bus: NewFakeBus(), Compile: CompilerFor(DefaultTestDefinition()), Match: SimpleMatch,
		Forges: FakeForges{}, Logger: DiscardLogger()})
	if wi, err := eng.findActiveWorkItem(ctx, "fac_1", "linear:ENG-1"); err == nil || wi != nil {
		t.Fatalf("store error: got %v, %v; want an error", wi, err)
	}

	eng = New(Options{Store: fs, Bus: NewFakeBus(), Compile: CompilerFor(DefaultTestDefinition()), Match: SimpleMatch,
		Forges: FakeForges{}, Logger: DiscardLogger()})
	if err := fs.KVSet(ctx, sourceAliasKey("fac_1", "linear:ENG-1"), "wi_gone"); err != nil {
		t.Fatal(err)
	}
	if wi, err := eng.findActiveWorkItem(ctx, "fac_1", "linear:ENG-1"); err != nil || wi != nil {
		t.Fatalf("alias to a missing work item: got %v, %v; want nil, nil", wi, err)
	}
}
