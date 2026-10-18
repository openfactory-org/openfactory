package eventctx_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func TestReleaseRemovesTheKeysOfARun(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	run := &core.Run{FactoryID: "fac_test", TeamID: "t1", Prompt: "x", State: core.RunSucceeded}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	other := &core.Run{FactoryID: "fac_test", TeamID: "t1", Prompt: "y", State: core.RunQueued}
	if err := st.CreateRun(ctx, other); err != nil {
		t.Fatal(err)
	}
	// The first prompt has a queue ID that is not the ID of a stored
	// message. The prompt key names it.
	add := func(runID, role string) string {
		m := &core.RunMessage{ID: core.NewID("msg"), RunID: runID, Role: role, Content: "c"}
		if err := st.AppendRunMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	userMsg := add(run.ID, "user")
	assistant := add(run.ID, "assistant")
	otherMsg := add(other.ID, "user")
	for _, k := range []string{eventctx.KVKey(userMsg), eventctx.KVKey(assistant), eventctx.KVKey("msg_first"), eventctx.KVKey(otherMsg)} {
		_ = st.KVSet(ctx, k, "{}")
	}
	_ = st.KVSet(ctx, eventctx.PromptKVKey(run.ID), "msg_first")
	_ = st.KVSet(ctx, eventctx.PromptKVKey(other.ID), otherMsg)

	if _, err := eventctx.Release(ctx, st, run.ID); err != nil {
		t.Fatal(err)
	}
	gone := func(k string) bool { _, err := st.KVGet(ctx, k); return errors.Is(err, core.ErrNotFound) }
	for _, k := range []string{eventctx.KVKey(userMsg), eventctx.KVKey("msg_first"), eventctx.PromptKVKey(run.ID)} {
		if !gone(k) {
			t.Errorf("key %s stays", k)
		}
	}
	// A key of an assistant message was never written by the engine, but it
	// is not a user message: Release leaves it. Keys of another run stay.
	for _, k := range []string{eventctx.KVKey(assistant), eventctx.KVKey(otherMsg), eventctx.PromptKVKey(other.ID)} {
		if gone(k) {
			t.Errorf("key %s was removed", k)
		}
	}
	// A second call, and a run without keys, are not errors.
	if _, err := eventctx.Release(ctx, st, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := eventctx.Release(ctx, st, "run_unknown"); err != nil {
		t.Fatal(err)
	}
}

func TestExpireRemovesOldKeysOnly(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	_ = st.KVSet(ctx, eventctx.KVKey("msg_1"), "{}")
	_ = st.KVSet(ctx, eventctx.PromptKVKey("run_1"), "msg_1")
	_ = st.KVSet(ctx, "attachment:att_1", "{}")
	if n, err := eventctx.Expire(ctx, st, time.Now()); err != nil || n != 0 {
		t.Fatalf("fresh keys: removed %d, err %v", n, err)
	}
	if n, err := eventctx.Expire(ctx, st, time.Now().Add(eventctx.Retention+time.Minute)); err != nil || n != 2 {
		t.Fatalf("old keys: removed %d, err %v", n, err)
	}
	if _, err := st.KVGet(ctx, "attachment:att_1"); err != nil {
		t.Fatalf("an unrelated key was removed: %v", err)
	}
}
