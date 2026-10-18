package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func creatorRun(t *testing.T, env *TestEnv) *core.Run {
	t.Helper()
	_, run, err := env.Engine.Dispatch(context.Background(), core.DispatchRequest{
		FactoryID: env.Factory.ID, Prompt: "work", Source: IssueEvent("x", "", "").Source, CreatorID: "usr_dana",
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestInboxRecipientsAndQuestions(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := creatorRun(t, env)
	q := core.Question{Type: core.QuestionMultiSelect, Options: []string{"a", "b"}, RecommendedOption: "b"}
	it, err := env.Engine.RequestHuman(core.WithQuestion(ctx, q), root.ID, core.InboxQuestion, "Pick", "Pick some", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(it.Recipients) != 1 || it.Recipients[0] != "usr_dana" {
		t.Fatalf("recipients = %v", it.Recipients)
	}
	if len(it.Questions) != 1 || it.Questions[0].ID != "q1" || it.Questions[0].Type != core.QuestionMultiSelect || it.Questions[0].RecommendedOption != "b" || it.Questions[0].Text != "Pick" {
		t.Fatalf("questions = %+v", it.Questions)
	}
	// Without a question in ctx, options give one single_select question.
	it2, _ := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Which?", "", []string{"x"})
	if it2.Questions[0].Type != core.QuestionSingleSelect {
		t.Fatalf("default type = %+v", it2.Questions)
	}
	// An answered item is information only: a resolve sends nothing to the run.
	info, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxAnswered, "", "FYI", nil)
	if err != nil || info.Kind != core.InboxAnswered {
		t.Fatalf("answered = %+v, %v", info, err)
	}
	before, _ := env.Store.ListRunMessages(ctx, root.ID, 0)
	if err := env.Engine.ResolveInbox(ctx, info.ID, "seen", "user:usr_dana"); err != nil {
		t.Fatal(err)
	}
	after, _ := env.Store.ListRunMessages(ctx, root.ID, 0)
	if len(after) != len(before) {
		t.Fatal("an answered item must not message the run")
	}
	// A blocked item is not created when the person has an open item.
	if b, _ := env.Engine.RequestHuman(ctx, root.ID, core.InboxBlocked, "", "waits", nil); b == nil || b.ID != it.ID && b.ID != it2.ID {
		t.Fatalf("blocked with an open item = %+v", b)
	}
}

func TestBlockedRunCreatesInboxItem(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := creatorRun(t, env)
	r, _ := env.Store.GetRun(ctx, root.ID)
	r.State, r.BlockedReason, r.Result = core.RunBlocked, "harness_needs_input", "Which branch?"
	_ = env.Store.UpdateRun(ctx, r)
	env.Engine.onRunUpdated(ctx, r)
	items, _ := env.Store.ListInboxItems(ctx, env.Factory.ID, false)
	if len(items) != 1 || items[0].Kind != core.InboxBlocked || items[0].Body != "Which branch?" || items[0].Recipients[0] != "usr_dana" {
		t.Fatalf("inbox = %+v", items)
	}
}

func TestHandoffClosesOnReviewAndMerge(t *testing.T) {
	for _, typ := range []string{"pull_request_review_submitted", "pull_request_merged"} {
		t.Run(typ, func(t *testing.T) {
			env := NewTestEnv(t)
			ctx := context.Background()
			root := creatorRun(t, env)
			pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/api", Number: 42, URL: "https://github.com/acme/api/pull/42"}
			if err := env.Engine.LinkPullRequest(ctx, root.WorkItemID, pr); err != nil {
				t.Fatal(err)
			}
			h, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxHandoff, "", "PR ready", nil)
			if err != nil {
				t.Fatal(err)
			}
			ev := &core.Event{ID: "e-" + typ, TeamID: "team1", Provider: "github", Type: typ, Actor: core.Actor{Handle: "rev"},
				Source: core.SourceRef{Provider: "github", Kind: "pull_request", Repo: "acme/api", Number: 42, URL: pr.URL}}
			if _, err := env.Engine.HandleEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
			got, _ := env.Store.GetInboxItem(ctx, h.ID)
			if !got.Resolved || got.ResolvedVia != core.InboxViaForge || !strings.Contains(got.Resolution, "rev") {
				t.Fatalf("handoff = %+v", got)
			}
			// The platform closed it, so it cannot be undone.
			if _, err := env.Engine.UnresolveInbox(ctx, h.ID, "user:usr_dana"); !errors.Is(err, core.ErrConflict) {
				t.Fatalf("unresolve forge close = %v", err)
			}
		})
	}
}

func TestUnresolveInbox(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := creatorRun(t, env)
	r, _ := env.Store.GetRun(ctx, root.ID)
	r.State = core.RunIdle
	_ = env.Store.UpdateRun(ctx, r)
	it, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Which DB?", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rctx := core.WithResolver(ctx, core.Resolver{UserID: "usr_dana", Via: core.InboxViaDashboard})
	if err := env.Engine.ResolveInbox(rctx, it.ID, "Postgres", "user:usr_dana"); err != nil {
		t.Fatal(err)
	}
	got, _ := env.Store.GetInboxItem(ctx, it.ID)
	if got.AnswerMessageID == "" {
		t.Fatalf("answer message not recorded: %+v", got)
	}
	un, err := env.Engine.UnresolveInbox(ctx, it.ID, "user:usr_dana")
	if err != nil || un.Resolved || un.Resolution != "" {
		t.Fatalf("unresolve = %+v, %v", un, err)
	}
	msgs, _ := env.Store.ListRunMessages(ctx, root.ID, 0)
	if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, "withdrew the answer") {
		t.Fatalf("last message = %q", last.Content)
	}
	if _, err := env.Engine.UnresolveInbox(ctx, it.ID, "user:usr_dana"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("second unresolve = %v", err)
	}

	// The run consumed the answer: no undo.
	if err := env.Engine.ResolveInbox(rctx, it.ID, "SQLite", "user:usr_dana"); err != nil {
		t.Fatal(err)
	}
	answered, _ := env.Store.GetInboxItem(ctx, it.ID)
	env.Store.consumeRunInput(root.ID, answered.AnswerMessageID)
	if _, err := env.Engine.UnresolveInbox(ctx, it.ID, "user:usr_dana"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("unresolve after consume = %v", err)
	}

	// Older than the window: no undo.
	it3, _ := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Late?", "", nil)
	_ = env.Engine.ResolveInbox(rctx, it3.ID, "yes", "user:usr_dana")
	old, _ := env.Store.GetInboxItem(ctx, it3.ID)
	past := time.Now().Add(-UnresolveWindow - time.Minute)
	old.ResolvedAt = &past
	_ = env.Store.UpdateInboxItem(ctx, old)
	if _, err := env.Engine.UnresolveInbox(ctx, it3.ID, "user:usr_dana"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("unresolve after window = %v", err)
	}
}
