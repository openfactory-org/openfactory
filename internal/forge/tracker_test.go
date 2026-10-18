package forge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// workItemForge is a forge with work items.
type workItemForge struct {
	core.Forge
	mu  sync.Mutex
	ops []string
}

func (f *workItemForge) Name() string { return "AZURE_DEVOPS" }

func (f *workItemForge) SetWorkItemState(_ context.Context, project string, id int, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("state %s/%d %s", project, id, state))
	return nil
}

func (f *workItemForge) AttachPullRequestToWorkItem(_ context.Context, project string, id int, pr core.PullRequestRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("attach %s/%d %s#%d", project, id, pr.Repo, pr.Number))
	return nil
}

func TestResponderTracksWorkItems(t *testing.T) {
	ctx := context.Background()
	f := &workItemForge{}
	rs := NewResponder(staticResolver{f}, "azure_devops")
	var _ core.IssueTracker = rs
	issue := core.SourceRef{Provider: "azure_devops", Kind: "work_item", Repo: "proj", Number: 7}
	if err := rs.SetIssueState(ctx, "t", issue, core.IssueStateInReview); err != nil {
		t.Fatal(err)
	}
	if err := rs.AttachPullRequest(ctx, "t", issue, core.PullRequestRef{Repo: "proj/web", Number: 11}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(f.ops) != "[state proj/7 in_review attach proj/7 proj/web#11]" {
		t.Fatalf("ops = %v", f.ops)
	}
	// A pull request or an issue of a code host is not a tracker issue. A
	// forge without work items does not support the operation.
	pr := core.SourceRef{Provider: "azure_devops", Kind: "pull_request", Repo: "proj/web", Number: 11}
	if err := rs.SetIssueState(ctx, "t", pr, core.IssueStateDone); !errors.Is(err, core.ErrOperationNotSupported) {
		t.Fatalf("pull request source: %v", err)
	}
	if err := rs.SetIssueState(ctx, "t", issue, core.IssueStateDone); err != nil {
		t.Fatal(err)
	}
	gh := NewResponder(staticResolver{&threadForge{}}, "github")
	gi := core.SourceRef{Provider: "github", Kind: "work_item", Repo: "acme/api", Number: 3}
	if err := gh.AttachPullRequest(ctx, "t", gi, core.PullRequestRef{}); !errors.Is(err, core.ErrOperationNotSupported) {
		t.Fatalf("forge without work items: %v", err)
	}
}
