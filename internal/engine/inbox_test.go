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

func TestResolveInboxFollowUpUsesNameAndSkipsBookkeeping(t *testing.T) {
	tests := []struct {
		name    string
		kind    core.InboxKind
		actor   string
		options []string
		resumes bool
		want    string
	}{
		{name: "question by user with name", kind: core.InboxQuestion, actor: "user:usr_dana", resumes: true, want: "[human answer] Dana Lee answered"},
		{name: "question by user without name", kind: core.InboxQuestion, actor: "user:usr_mail", resumes: true, want: "[human answer] mail@example.com answered"},
		{name: "question by unknown user", kind: core.InboxQuestion, actor: "user:usr_gone", resumes: true, want: "[human answer] a team member answered"},
		{name: "question by automation", kind: core.InboxQuestion, actor: "webhook:aut_1", resumes: true, want: "[human answer] an automation answered"},
		{name: "approval", kind: core.InboxApproval, actor: "user:usr_dana", resumes: true, want: "resolved by Dana Lee: ok"},
		{name: "blocked", kind: core.InboxBlocked, actor: "user:usr_dana", resumes: true, want: "[human response] Dana Lee responded"},
		{name: "handoff", kind: core.InboxHandoff, actor: "user:usr_dana", resumes: false},
		{name: "answered", kind: core.InboxAnswered, actor: "user:usr_dana", resumes: false},
		// A failure notice is bookkeeping: retrying is a separate follow-up.
		{name: "failure", kind: core.InboxFailure, actor: "user:usr_dana", resumes: false},
		// The review of a self-improvement pull request has no options.
		{name: "pull request review", kind: core.InboxReview, actor: "user:usr_dana", resumes: false},
		// The review of a definition change: the author run needs the decision.
		{name: "definition change review", kind: core.InboxReview, options: []string{"approve", "reject"}, actor: "user:usr_dana", resumes: true, want: "[human response] Dana Lee responded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := NewTestEnv(t)
			ctx := context.Background()
			for _, u := range []*core.User{
				{ID: "usr_dana", TeamID: "team1", Name: "Dana Lee", Email: "dana@example.com", Role: core.RoleUser},
				{ID: "usr_mail", TeamID: "team1", Email: "mail@example.com", Role: core.RoleUser},
			} {
				env.Store.PutUser(u)
			}
			root := creatorRun(t, env)
			it, err := env.Engine.RequestHuman(ctx, root.ID, tt.kind, "Title", "Body", tt.options)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := env.Store.ListRunMessages(ctx, root.ID, 0)
			rctx := core.WithResolver(ctx, core.Resolver{UserID: "usr_dana", Via: core.InboxViaDashboard})
			if err := env.Engine.ResolveInbox(rctx, it.ID, "ok", tt.actor); err != nil {
				t.Fatal(err)
			}
			after, _ := env.Store.ListRunMessages(ctx, root.ID, 0)
			if !tt.resumes {
				if len(after) != len(before) {
					t.Fatalf("a %s resolve must not message the run: %+v", tt.kind, after[len(before):])
				}
				// Undo sends no correction either: nothing was sent.
				if _, err := env.Engine.UnresolveInbox(ctx, it.ID, tt.actor); err != nil {
					t.Fatal(err)
				}
				if again, _ := env.Store.ListRunMessages(ctx, root.ID, 0); len(again) != len(before) {
					t.Fatalf("undo of a %s resolve must not message the run", tt.kind)
				}
				return
			}
			if len(after) != len(before)+1 {
				t.Fatalf("messages = %d, want %d", len(after), len(before)+1)
			}
			got := after[len(after)-1].Content
			if !strings.Contains(got, tt.want) || strings.Contains(got, "usr_") {
				t.Fatalf("follow-up = %q, want %q and no raw user ID", got, tt.want)
			}
		})
	}
}
