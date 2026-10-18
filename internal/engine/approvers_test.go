package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestApproversList(t *testing.T) {
	env := NewTestEnv(t, func(d *core.CompiledDefinition) { d.Approvers = []string{"Ada@Example.test"} })
	ctx := context.Background()
	ada := &core.User{ID: "usr_ada", TeamID: "team1", Email: "ada@example.test", Role: core.RoleUser}
	bob := &core.User{ID: "usr_bob", TeamID: "team1", Email: "bob@example.test", Role: core.RoleAdmin}
	env.Store.PutUser(ada)
	env.Store.PutUser(bob)
	rt, err := LoadFactoryRuntime(ctx, env.Store, env.Factory.ID)
	if err != nil || len(rt.Approvers) != 1 {
		t.Fatalf("runtime approvers = %v, %v", rt.Approvers, err)
	}

	root := foremanRun(t, env)
	it, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxApproval, "Spec", "Review the spec", nil)
	if err != nil {
		t.Fatal(err)
	}

	// A member who is not in the list cannot approve, even an admin.
	asBob := core.WithResolver(ctx, core.Resolver{UserID: bob.ID, Via: core.InboxViaDashboard})
	if err := env.Engine.ResolveInbox(asBob, it.ID, "approved", "user:"+bob.ID); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("bob: err = %v, want forbidden", err)
	}
	// A sender without a linked account cannot approve.
	if err := env.Engine.ResolveInbox(ctx, it.ID, "approved", "stranger"); !errors.Is(err, core.ErrForbidden) {
		t.Fatalf("no resolver: err = %v, want forbidden", err)
	}
	if got, _ := env.Store.GetInboxItem(ctx, it.ID); got.Resolved {
		t.Fatalf("item resolved by a non-approver: %+v", got)
	}

	// A listed approver can, from any tool. The tool is recorded.
	asAda := core.WithResolver(ctx, core.Resolver{UserID: ada.ID, Via: core.InboxViaChat})
	if err := env.Engine.ResolveInbox(asAda, it.ID, "approved", ada.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := env.Store.GetInboxItem(ctx, it.ID)
	if !got.Resolved || got.ResolvedBy != ada.ID || got.ResolvedVia != core.InboxViaChat {
		t.Fatalf("inbox = %+v", got)
	}

	// The list applies to approvals only: any member answers a question.
	q, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Required?", "Is the field required?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.Engine.ResolveInbox(asBob, q.ID, "No", "user:"+bob.ID); err != nil {
		t.Fatalf("question: %v", err)
	}
}

func TestCanApproveWithoutList(t *testing.T) {
	env := NewTestEnv(t)
	ok, err := env.Engine.CanApprove(context.Background(), env.Factory.ID, "")
	if err != nil || !ok {
		t.Fatalf("CanApprove without a list = %v, %v", ok, err)
	}
	if IsApprover([]string{"a@example.test"}, nil) || IsApprover([]string{"a@example.test"}, &core.User{}) {
		t.Fatal("IsApprover matched a user without an email")
	}
}
