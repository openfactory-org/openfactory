package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

type fakeResponder struct {
	provider string
	mu       sync.Mutex
	errs     []error
	calls    []string
}

func (f *fakeResponder) Provider() string { return f.provider }

func (f *fakeResponder) Reply(_ context.Context, teamID string, msg core.OutboundMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, teamID+"|"+msg.Text)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return nil
}

func (f *fakeResponder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func teamOf(context.Context, string) (string, error) { return "team_1", nil }

func TestDeliverRetries(t *testing.T) {
	slack := &fakeResponder{provider: "slack", errs: []error{errors.New("timeout"), errors.New("timeout")}}
	r := NewRouter(forgetest.NewBus(), slack)
	r.TeamOf = teamOf
	r.BaseBackoff = time.Millisecond
	err := r.Deliver(context.Background(), core.OutboundMessage{WorkItemID: "w", Text: "hi", Source: core.SourceRef{Provider: "slack"}})
	if err != nil || slack.count() != 3 {
		t.Fatalf("err=%v calls=%d", err, slack.count())
	}
	if s := r.Stats(); s.Delivered != 1 || s.Retries != 2 || s.Failed != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestPermanentFailure(t *testing.T) {
	gh := &fakeResponder{provider: "github", errs: []error{&forgeutil.APIError{Status: 404}}}
	var failed []string
	r := NewRouter(forgetest.NewBus(), gh)
	r.TeamOf = teamOf
	r.BaseBackoff = time.Millisecond
	r.OnFailure = func(_ context.Context, msg core.OutboundMessage, err error) { failed = append(failed, msg.WorkItemID) }
	if err := r.Deliver(context.Background(), core.OutboundMessage{WorkItemID: "w1", Source: core.SourceRef{Provider: "github"}}); err == nil {
		t.Fatal("expected error")
	}
	if gh.count() != 1 || len(failed) != 1 || r.Stats().Failed != 1 {
		t.Fatalf("calls=%d failed=%v stats=%+v", gh.count(), failed, r.Stats())
	}
}

func TestMaxAttempts(t *testing.T) {
	j := &fakeResponder{provider: "jira", errs: []error{errors.New("x"), errors.New("x"), errors.New("x")}}
	r := NewRouter(forgetest.NewBus(), j)
	r.TeamOf = teamOf
	r.MaxAttempts = 2
	r.BaseBackoff = time.Millisecond
	if err := r.Deliver(context.Background(), core.OutboundMessage{Source: core.SourceRef{Provider: "jira"}}); err == nil || j.count() != 2 {
		t.Fatalf("err=%v calls=%d", err, j.count())
	}
}

func TestSubscriptionAndAliases(t *testing.T) {
	bus := forgetest.NewBus()
	teams := &fakeResponder{provider: "teams"}
	r := NewRouter(bus, teams)
	r.TeamOf = teamOf
	ctx := context.Background()
	r.Start(ctx)
	defer r.Stop()
	bus.Publish(ctx, core.TopicOutboundNotice, &core.OutboundMessage{WorkItemID: "w", Text: "a", Source: core.SourceRef{Provider: "microsoft_teams"}})
	bus.Publish(ctx, core.TopicOutboundNotice, &core.OutboundMessage{WorkItemID: "w", Text: "b", Source: core.SourceRef{Provider: "api"}})
	if !forgetest.Eventually(2*time.Second, func() bool { return teams.count() == 1 && r.Stats().Unroutable == 1 }) {
		t.Fatalf("calls=%d stats=%+v", teams.count(), r.Stats())
	}
	if got := r.Providers(); len(got) != 1 || got[0] != "teams" {
		t.Fatalf("providers = %v", got)
	}
}

func TestRoutes(t *testing.T) {
	store := forgetest.NewStore()
	store.Factories = []core.Factory{{ID: "f1", TeamID: "team_1"}}
	store.FactoryInts["f1"] = map[string]map[string]string{"slack": {"autoRespondToThreadReplies": "true"}, "microsoft-teams": {}}
	store.Integrations = []core.Integration{{TeamID: "team_1", Provider: "github"}, {TeamID: "team_1", Provider: "slack"}}
	r := NewRouter(forgetest.NewBus(), &fakeResponder{provider: "slack"}, &fakeResponder{provider: "github"})
	routes, err := r.Routes(context.Background(), store, "f1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Route{}
	for _, rt := range routes {
		got[rt.Provider] = rt
	}
	if len(routes) != 4 || !got["dashboard"].Deliverable || !got["slack"].Deliverable || got["teams"].Deliverable || got["github"].Kind != "forge" {
		t.Fatalf("routes = %+v", routes)
	}
	if got["slack"].Settings["autoRespondToThreadReplies"] != "true" {
		t.Fatal("settings missing")
	}
}

type trackerResponder struct {
	fakeResponder
	ops []string
	err error
}

func (f *trackerResponder) SetIssueState(_ context.Context, teamID string, issue core.SourceRef, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "state|"+issue.ExternalID+"|"+state)
	return f.err
}

func (f *trackerResponder) AttachPullRequest(_ context.Context, teamID string, issue core.SourceRef, pr core.PullRequestRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "attach|"+issue.ExternalID+"|"+pr.URL)
	return f.err
}

func TestTrackerOperations(t *testing.T) {
	ctx := context.Background()
	lin := &trackerResponder{fakeResponder: fakeResponder{provider: "linear"}}
	slack := &fakeResponder{provider: "slack"}
	r := NewRouter(forgetest.NewBus(), lin, slack)
	r.TeamOf = teamOf
	issue := core.SourceRef{Provider: "linear", ExternalID: "ENG-1"}
	if err := r.Deliver(ctx, core.OutboundMessage{WorkItemID: "w", Source: issue, Kind: core.OutboundIssueState, IssueState: core.IssueStateDone}); err != nil {
		t.Fatal(err)
	}
	pr := &core.PullRequestRef{URL: "https://github.com/a/b/pull/1"}
	if err := r.Deliver(ctx, core.OutboundMessage{WorkItemID: "w", Source: issue, Kind: core.OutboundAttachPullRequest, PullRequest: pr}); err != nil {
		t.Fatal(err)
	}
	if len(lin.ops) != 2 || lin.ops[0] != "state|ENG-1|done" || lin.ops[1] != "attach|ENG-1|"+pr.URL || lin.count() != 0 {
		t.Fatalf("ops = %v, replies = %d", lin.ops, lin.count())
	}
	// A responder that is not a tracker skips the operation.
	if err := r.Deliver(ctx, core.OutboundMessage{WorkItemID: "w", Source: core.SourceRef{Provider: "slack"}, Kind: core.OutboundIssueState, IssueState: "done"}); err != nil || slack.count() != 0 {
		t.Fatalf("err=%v replies=%d", err, slack.count())
	}
	// operation_not_supported is not a failure.
	lin.err = core.ErrOperationNotSupported
	if err := r.Deliver(ctx, core.OutboundMessage{WorkItemID: "w", Source: issue, Kind: core.OutboundIssueState, IssueState: "done"}); err != nil {
		t.Fatal(err)
	}
	if s := r.Stats(); s.Failed != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

// TestBenchmarkTrialNoticesStayInTheFactory shows that a notice of a
// benchmark trial reaches no responder, even when the integration of a real
// provider is connected. The trial work item has the source "benchmark".
func TestBenchmarkTrialNoticesStayInTheFactory(t *testing.T) {
	bus := forgetest.NewBus()
	slack := &fakeResponder{provider: "slack"}
	gh := &fakeResponder{provider: "github"}
	r := NewRouter(bus, slack, gh)
	r.TeamOf = teamOf
	ctx := context.Background()
	r.Start(ctx)
	defer r.Stop()
	for _, kind := range []string{"update", "handoff", "question"} {
		bus.Publish(ctx, core.TopicOutboundNotice, &core.OutboundMessage{WorkItemID: "w", Text: "trial " + kind, Kind: kind, Source: core.SourceRef{Provider: "benchmark", ExternalID: "bench_1/t1"}})
	}
	// A notice of a real source after them proves that the router works.
	bus.Publish(ctx, core.TopicOutboundNotice, &core.OutboundMessage{WorkItemID: "w", Text: "real", Source: core.SourceRef{Provider: "slack"}})
	if !forgetest.Eventually(2*time.Second, func() bool { return slack.count() == 1 }) {
		t.Fatalf("slack calls=%d stats=%+v", slack.count(), r.Stats())
	}
	if slack.count() != 1 || gh.count() != 0 {
		t.Fatalf("a trial notice was delivered: slack=%d github=%d", slack.count(), gh.count())
	}
}
