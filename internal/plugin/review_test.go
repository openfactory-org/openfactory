package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
	"github.com/openfactory-org/openfactory/pkg/plugin/sdk"
)

// reviewForge adds the review operations to fakeForge. MarkReady is not
// supported.
type reviewForge struct {
	*fakeForge
}

func (f reviewForge) ReplyInThread(_ context.Context, _, _ string, n int, thread, body string) error {
	f.record(fmt.Sprintf("reply %d %s %s", n, thread, body))
	return nil
}
func (f reviewForge) ResolveThread(_ context.Context, _, _ string, n int, thread string) error {
	f.record(fmt.Sprintf("resolve %d %s", n, thread))
	return nil
}
func (f reviewForge) SubmitReview(_ context.Context, in core.ReviewInput) error {
	f.record(fmt.Sprintf("review %d %s %s:%d", in.Number, in.Event, in.Comments[0].Path, in.Comments[0].Line))
	return nil
}
func (f reviewForge) MarkReady(context.Context, string, string, int) error {
	return core.ErrOperationNotSupported
}
func (f reviewForge) SetWorkItemState(_ context.Context, project string, id int, state string) error {
	f.record(fmt.Sprintf("wistate %s/%d %s", project, id, state))
	return nil
}
func (f reviewForge) AttachPullRequestToWorkItem(_ context.Context, project string, id int, pr core.PullRequestRef) error {
	f.record(fmt.Sprintf("wiattach %s/%d %s#%d", project, id, pr.Repo, pr.Number))
	return nil
}

type trackerChat struct {
	fakeResponder
	mu  sync.Mutex
	ops []string
}

func (r *trackerChat) SetIssueState(_ context.Context, _ string, issue core.SourceRef, state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, "state "+issue.ExternalID+" "+state)
	return nil
}

func (r *trackerChat) AttachPullRequest(_ context.Context, _ string, issue core.SourceRef, pr core.PullRequestRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, fmt.Sprintf("attach %s %s#%d", issue.ExternalID, pr.Repo, pr.Number))
	return nil
}

// TestPluginReviewAndTrackerOperations checks the review operations of the
// forge service and the tracker operations of the responder service over
// the plugin protocol.
func TestPluginReviewAndTrackerOperations(t *testing.T) {
	env := newTestEnv(t, nil)
	tracker := &trackerChat{}
	ctx := context.Background()
	base := env.fakePlugin(nil)
	plug := func() sdk.Plugin {
		p := base()
		p.Forge = func(context.Context, sdk.ForgeTarget) (core.Forge, error) { return reviewForge{env.forge}, nil }
		p.Responders = []core.Responder{tracker}
		return p
	}
	if err := env.host.launch(ctx, "fake-plugin", testLauncher(t, plug, &env.launches, &env.kills)); err != nil {
		t.Fatal(err)
	}
	f, err := env.host.ForgeFor(ctx, forge.ForgeTarget{Provider: "fake", Token: "tok", Config: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ReplyInThread(ctx, "acme", "api", 4, "9001", "fixed"); err != nil {
		t.Fatal(err)
	}
	if err := f.ResolveThread(ctx, "acme", "api", 4, "9001"); err != nil {
		t.Fatal(err)
	}
	if err := f.SubmitReview(ctx, core.ReviewInput{Owner: "acme", Repo: "api", Number: 4, Event: "comment",
		Comments: []core.ReviewComment{{Path: "a.go", Line: 3, Body: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.MarkReady(ctx, "acme", "api", 4); !errors.Is(err, core.ErrOperationNotSupported) {
		t.Fatalf("mark ready: %v", err)
	}
	want := "reply 4 9001 fixed|resolve 4 9001|review 4 comment a.go:3"
	if got := fmt.Sprint(env.forge.calls); got != "["+joinCalls(want)+"]" {
		t.Fatalf("calls = %s", got)
	}

	// The work item write-back of a forge with work items.
	wt, ok := f.(forge.WorkItemTracker)
	if !ok {
		t.Fatal("plugin forge is not a work item tracker")
	}
	if err := wt.SetWorkItemState(ctx, "proj", 7, core.IssueStateDone); err != nil {
		t.Fatal(err)
	}
	if err := wt.AttachPullRequestToWorkItem(ctx, "proj", 7, core.PullRequestRef{Repo: "proj/web", Number: 11}); err != nil {
		t.Fatal(err)
	}
	if got := env.forge.calls[len(env.forge.calls)-2:]; fmt.Sprint(got) != "[wistate proj/7 done wiattach proj/7 proj/web#11]" {
		t.Fatalf("work item calls = %v", got)
	}

	rs := env.host.Responders()
	if len(rs) != 1 {
		t.Fatalf("responders = %v", rs)
	}
	it, ok := rs[0].(core.IssueTracker)
	if !ok {
		t.Fatal("plugin responder is not an issue tracker")
	}
	issue := core.SourceRef{Provider: "fakechat", ExternalID: "ENG-1"}
	if err := it.SetIssueState(ctx, "team_1", issue, core.IssueStateDone); err != nil {
		t.Fatal(err)
	}
	if err := it.AttachPullRequest(ctx, "team_1", issue, core.PullRequestRef{Repo: "acme/api", Number: 4}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(tracker.ops) != "[state ENG-1 done attach ENG-1 acme/api#4]" {
		t.Fatalf("ops = %v", tracker.ops)
	}
	// The thread of a reply crosses the protocol.
	msg := core.OutboundMessage{Text: "hi", ThreadRef: "9001", Source: core.SourceRef{Provider: "fakechat", ThreadRef: "9001"}}
	if err := rs[0].Reply(ctx, "team_1", msg); err != nil {
		t.Fatal(err)
	}
	if got := tracker.msgs[len(tracker.msgs)-1]; got.ThreadRef != "9001" || got.Source.ThreadRef != "9001" {
		t.Fatalf("reply = %+v", got)
	}
}

func joinCalls(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] == '|' {
			out[i] = ' '
		}
	}
	return string(out)
}
