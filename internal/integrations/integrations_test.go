package integrations

import (
	"context"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

func TestSettingsAndInbox(t *testing.T) {
	store := forgetest.NewStore()
	store.FactoryInts["f1"] = map[string]map[string]string{"microsoft-teams": {"autoRespondToThreadReplies": "false"}}
	ctx := context.Background()
	if AutoRespondToThreadReplies(ctx, store, "f1", "microsoft_teams") {
		t.Fatal("expected false")
	}
	if !AutoRespondToThreadReplies(ctx, store, "f1", "slack") || !AutoRespondToThreadReplies(ctx, store, "", "slack") {
		t.Fatal("default should be true")
	}
	now := time.Now()
	store.WorkItems["w1"] = core.WorkItem{ID: "w1", FactoryID: "f1"}
	store.Inbox = []core.InboxItem{
		{ID: "a", FactoryID: "f1", WorkItemID: "w1", Kind: core.InboxFailure, CreatedAt: now},
		{ID: "b", FactoryID: "f1", WorkItemID: "w1", Kind: core.InboxApproval, CreatedAt: now.Add(-time.Hour)},
		{ID: "c", FactoryID: "f1", WorkItemID: "w2", Kind: core.InboxApproval, CreatedAt: now},
	}
	it, err := OpenInboxItem(ctx, store, "w1")
	if err != nil || it.ID != "b" {
		t.Fatalf("inbox = %v %v", it, err)
	}
	if u, err := LinkByEmail(ctx, store, "team_1", "slack", "U", "u", "nobody@x.com"); u != nil || err != nil {
		t.Fatalf("unexpected link: %v %v", u, err)
	}
}
