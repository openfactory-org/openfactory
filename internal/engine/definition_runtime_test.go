package engine

import (
	"context"
	"reflect"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
)

// TestApplyStoresTheSenderSettingsOnTheRuntime applies a definition tree that
// the real compiler reads, and checks the runtime that the apply stores. The
// settings of the sender checks must come from the files.
func TestApplyStoresTheSenderSettingsOnTheRuntime(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	st.PutUser(&core.User{ID: "usr_ada", TeamID: "team1", Email: "ada@example.test", Role: core.RoleUser})
	st.PutUser(&core.User{ID: "sa_ci", TeamID: "team1", Name: "ci", Role: core.RoleEditor, Kind: core.UserKindServiceAccount})
	f := &core.Factory{ID: "fac_1", TeamID: "team1", Name: "payments", Source: core.DefinitionManaged}
	if err := st.CreateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	eng := New(Options{Store: st, Bus: NewFakeBus(), Compile: definition.Compile, Secrets: NewFakeSecrets(), Forges: FakeForges{}, Logger: DiscardLogger()})

	files, err := definition.DefaultDefinition(definition.DefaultDefinitionOptions{Name: "payments", Agents: []string{"foreman"}})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(extra string) (*FactoryRuntime, error) {
		fs := map[string]string{}
		for k, v := range files {
			fs[k] = v
		}
		fs["factory.yaml"] += extra
		if _, err := eng.ApplyDefinition(ctx, f.ID, fs, "tester", "test", ""); err != nil {
			return nil, err
		}
		return LoadFactoryRuntime(ctx, st, f.ID)
	}

	rt, err := apply("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Approvers) != 0 || rt.RestrictFollowupsToStarter || rt.RequireLinkedActor {
		t.Fatalf("defaults: %+v", rt)
	}

	rt, err = apply("approvers:\n  - ada@example.test\n  - ci\nrestrictFollowupsToStarter: true\nrequireLinkedActor: true\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rt.Approvers, []string{"ada@example.test", "ci"}) || !rt.RestrictFollowupsToStarter || !rt.RequireLinkedActor {
		t.Fatalf("runtime = approvers %v, restrict %v, linked %v", rt.Approvers, rt.RestrictFollowupsToStarter, rt.RequireLinkedActor)
	}

	// The settings follow the active version: an apply without them clears them.
	rt, err = apply("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Approvers) != 0 || rt.RestrictFollowupsToStarter || rt.RequireLinkedActor {
		t.Fatalf("after clearing: %+v", rt)
	}

	// An approver who is not in the team stops the apply. The runtime stays.
	rt, err = apply("approvers:\n  - ada@example.test\nrequireLinkedActor: true\n")
	if err != nil || !rt.RequireLinkedActor {
		t.Fatalf("setup: %v %+v", err, rt)
	}
	if _, err := apply("approvers:\n  - nobody@example.test\n"); err == nil {
		t.Fatal("apply with an unknown approver succeeded")
	}
	rt, _ = LoadFactoryRuntime(ctx, st, f.ID)
	if !reflect.DeepEqual(rt.Approvers, []string{"ada@example.test"}) || !rt.RequireLinkedActor {
		t.Fatalf("runtime changed by a failed apply: %+v", rt)
	}
}
