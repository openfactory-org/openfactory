package dispatch

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// TestAddEventFiles checks that the event context of a message reaches its
// item, also for the first prompt, which has its own queue ID.
func TestAddEventFiles(t *testing.T) {
	e := newEnv(t)
	ev := &core.Event{Provider: "webhook", Type: "received", Payload: json.RawMessage(`{"a":1}`)}
	first := eventctx.Build(e.ctx, "msg_first", ev, true, nil)
	later := eventctx.Build(e.ctx, "msg_later", ev, false, nil)
	for _, b := range []*eventctx.Bundle{first, later} {
		raw, err := b.Encode()
		if err != nil {
			t.Fatal(err)
		}
		_ = e.mem.KVSet(e.ctx, eventctx.KVKey(b.MessageID), raw)
	}
	_ = e.mem.KVSet(e.ctx, eventctx.PromptKVKey("run_1"), "msg_first")
	msgs := []core.RunInput{
		{ID: "in_1", RunID: "run_1", DedupeKey: "prompt"},
		{ID: "msg_later", RunID: "run_1"},
		{ID: "msg_plain", RunID: "run_1"},
	}
	items := make([]runnerproto.InputItem, len(msgs))
	e.d.addEventFiles(e.ctx, msgs, items)
	if len(items[0].Files) != 2 || items[0].Files[0].Path != ".openfactory/events/msg_first/envelope.json" {
		t.Fatalf("first = %+v", items[0].Files)
	}
	if len(items[1].Files) != 2 || items[1].Files[1].Path != ".openfactory/events/msg_later/payload.json" || string(items[1].Files[1].Data) != `{"a":1}` {
		t.Fatalf("later = %+v", items[1].Files)
	}
	if len(items[2].Files) != 0 {
		t.Fatalf("plain = %+v", items[2].Files)
	}
}

// TestEventContextReleasedWhenRunEnds checks that a turn end that finishes
// the run removes its event context keys.
func TestEventContextReleasedWhenRunEnds(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	s := e.session(run.ID, 1)
	first := &core.RunMessage{ID: "msg_first", RunID: run.ID, Role: "user", Content: "go"}
	if err := e.mem.AppendRunMessage(e.ctx, first); err != nil {
		t.Fatal(err)
	}
	_ = e.mem.KVSet(e.ctx, eventctx.KVKey(first.ID), "{}")
	_ = e.mem.KVSet(e.ctx, eventctx.PromptKVKey(run.ID), first.ID)
	e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil))
	for _, k := range []string{eventctx.KVKey(first.ID), eventctx.PromptKVKey(run.ID)} {
		if _, err := e.mem.KVGet(e.ctx, k); err != nil {
			t.Fatalf("key %s was removed while the run is active: %v", k, err)
		}
	}
	e.events(s, event(t, 2, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone, Result: "ok"}))
	if r := e.run(run.ID); r.State != core.RunSucceeded {
		t.Fatalf("run state = %s", r.State)
	}
	for _, k := range []string{eventctx.KVKey(first.ID), eventctx.PromptKVKey(run.ID)} {
		if _, err := e.mem.KVGet(e.ctx, k); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("key %s stays after the run ended: %v", k, err)
		}
	}
}
