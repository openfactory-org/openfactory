package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
)

type threadBuilder struct{}

func (threadBuilder) Provider() string { return "slack" }
func (threadBuilder) EventContext(_ context.Context, ev *core.Event) ([]eventctx.File, []eventctx.Omitted, error) {
	return []eventctx.File{{Path: "thread.json", Data: []byte(`[{"ts":"1.0","user":"U1","text":"The staging deploy broke at 10:02."}]`)}},
		[]eventctx.Omitted{{Name: "screenshot.png", Reason: "larger than 1024 KiB"}}, nil
}

// TestWebhookRunStoresPayload checks that a run from
// a custom webhook gets the delivery body and the envelope in the event
// context of its first message. The dispatcher writes them to the workspace
// (internal/dispatch/eventfiles_test.go).
func TestWebhookRunStoresPayload(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	ev := &core.Event{ID: "evt_w", TeamID: "team1", FactoryID: env.Factory.ID, Provider: "webhook", Type: "received", DeliveryID: "dlv-9",
		Actor:      core.Actor{Provider: "webhook", ExternalID: "wh_1", Handle: "ci-alerts"},
		Source:     core.SourceRef{Provider: "webhook", Kind: "delivery", ExternalID: "dlv-9", Title: "ci-alerts"},
		Attributes: map[string]any{"webhook_id": "wh_1", "webhook_name": "ci-alerts"},
		Payload:    json.RawMessage(`{"build":{"status":"failed","id":42}}`)}
	_, run, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "Look at the failed build.", Source: ev.Source, Event: ev})
	if err != nil {
		t.Fatal(err)
	}
	msgID := env.Store.MessagesOf(run.ID)[0].ID
	raw, err := env.Store.KVGet(ctx, eventctx.KVKey(msgID))
	if err != nil {
		t.Fatal(err)
	}
	b, err := eventctx.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range b.Files {
		files[f.Path] = string(f.Data)
	}
	if !strings.Contains(files[".openfactory/events/"+msgID+"/payload.json"], `"status":"failed"`) {
		t.Fatalf("payload file missing in %v", files)
	}
	if !strings.Contains(files[".openfactory/events/"+msgID+"/envelope.json"], `"delivery": "dlv-9"`) {
		t.Fatalf("envelope missing in %v", files)
	}
	if !strings.Contains(b.PromptSection(), "Webhook: ci-alerts, delivery dlv-9") {
		t.Errorf("prompt section = %q", b.PromptSection())
	}
}

// TestSlackRunGetsThread checks that a run that
// starts in a chat thread gets the earlier messages of the thread, and a
// later event message has the envelope and the payload, not the context.
func TestSlackRunGetsThread(t *testing.T) {
	env := NewTestEnv(t)
	env.Engine.AddEventContext(threadBuilder{})
	ctx := context.Background()
	ev := &core.Event{ID: "evt_s", TeamID: "team1", FactoryID: env.Factory.ID, Provider: "slack", Type: "app_mention",
		Actor:  core.Actor{Provider: "slack", ExternalID: "U2", Handle: "dana"},
		Source: core.SourceRef{Provider: "slack", Kind: "thread", Channel: "C1", ThreadID: "1.0"}, Text: "@factory fix the deploy"}
	_, run, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Source: ev.Source, Event: ev})
	if err != nil {
		t.Fatal(err)
	}
	msgID := env.Store.MessagesOf(run.ID)[0].ID
	raw, _ := env.Store.KVGet(ctx, eventctx.KVKey(msgID))
	first, err := eventctx.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	var thread string
	for _, f := range first.Files {
		if f.Path == ".openfactory/events/"+msgID+"/context/thread.json" {
			thread = string(f.Data)
		}
	}
	if !strings.Contains(thread, "The staging deploy broke at 10:02.") {
		t.Fatalf("thread = %q", thread)
	}
	prompt := first.PromptSection()
	if !strings.Contains(prompt, "context/thread.json") || !strings.Contains(prompt, "Left out: screenshot.png") {
		t.Fatalf("prompt:\n%s", prompt)
	}

	if err := env.Engine.followUpEvent(ctx, run.ID, "[follow-up] more", "dana", ev); err != nil {
		t.Fatal(err)
	}
	msgs := env.Store.MessagesOf(run.ID)
	second := msgs[len(msgs)-1].ID
	raw, _ = env.Store.KVGet(ctx, eventctx.KVKey(second))
	b, err := eventctx.Decode(raw)
	if err != nil || len(b.Envelope.ContextFiles) != 0 {
		t.Fatalf("second bundle = %+v, %v", b, err)
	}
	var hasEnvelope bool
	for _, f := range b.Files {
		if f.Path == ".openfactory/events/"+second+"/envelope.json" {
			hasEnvelope = true
		}
	}
	if !hasEnvelope {
		t.Fatalf("second envelope missing: %+v", b.Files)
	}
}

func webhookEvent() *core.Event {
	return &core.Event{ID: "evt_x", TeamID: "team1", Provider: "webhook", Type: "received", DeliveryID: "dlv-1",
		Actor:   core.Actor{Provider: "webhook", ExternalID: "wh_1", Handle: "ci"},
		Source:  core.SourceRef{Provider: "webhook", Kind: "delivery", ExternalID: "dlv-1", Title: "ci"},
		Payload: json.RawMessage(`{"ok":false}`)}
}

func hasKey(t *testing.T, st core.Store, key string) bool {
	t.Helper()
	_, err := st.KVGet(context.Background(), key)
	return err == nil
}

func TestEventContextReleasedOnCancel(t *testing.T) {
	f := NewTestEnv(t)
	ctx := context.Background()
	ev := webhookEvent()
	ev.FactoryID = f.Factory.ID
	_, run, err := f.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: f.Factory.ID, Prompt: "Look at it.", Source: ev.Source, Event: ev})
	if err != nil {
		t.Fatal(err)
	}
	msgID := f.Store.MessagesOf(run.ID)[0].ID
	if !hasKey(t, f.Store, eventctx.KVKey(msgID)) {
		t.Fatal("event context key is missing before the cancel")
	}
	if err := f.Engine.Cancel(ctx, run.ID, "usr"); err != nil {
		t.Fatal(err)
	}
	if hasKey(t, f.Store, eventctx.KVKey(msgID)) || hasKey(t, f.Store, eventctx.PromptKVKey(run.ID)) {
		t.Fatal("event context keys stay after the cancel")
	}
}

// TestSweepEventContext checks the sweep of the leader: it removes the
// event context that is older than the retention time and keeps the rest.
func TestSweepEventContext(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	_ = env.Store.KVSet(ctx, eventctx.KVKey("msg_old"), "{}")
	_ = env.Store.KVSet(ctx, eventctx.PromptKVKey("run_old"), "msg_old")
	_ = env.Store.KVSet(ctx, "attachment:att_1", "{}")
	if n, err := env.Engine.SweepEventContext(ctx); err != nil || n != 0 {
		t.Fatalf("fresh sweep = %d, %v", n, err)
	}
	env.Engine.clock = func() time.Time { return time.Now().Add(eventctx.Retention + time.Hour) }
	if n, err := env.Engine.SweepEventContext(ctx); err != nil || n != 2 {
		t.Fatalf("old sweep = %d, %v", n, err)
	}
	if hasKey(t, env.Store, eventctx.KVKey("msg_old")) || !hasKey(t, env.Store, "attachment:att_1") {
		t.Fatal("the sweep removed the wrong keys")
	}
}
