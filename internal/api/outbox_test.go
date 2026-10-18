package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestDeadOutboxMessages(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	// The SQL store is also the coordinator that holds the outbox.
	f := &core.Factory{ID: core.NewID("fac"), TeamID: e.team.ID, Name: "f"}
	if err := e.store.CreateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	dead := func(id, topic string, payload any) {
		b, _ := json.Marshal(payload)
		m := &core.OutboxMessage{ID: id, Topic: topic, Payload: b, PartitionKey: core.OutboxPartitionKey(topic, payload)}
		if _, err := e.store.OutboxEnqueue(ctx, m); err != nil {
			t.Fatal(err)
		}
		got, err := e.store.OutboxClaim(ctx, []string{topic}, "h", time.Now().Add(time.Second), time.Minute, 10)
		if err != nil || len(got) != 1 {
			t.Fatalf("claim %s: %v %v", id, got, err)
		}
		if err := e.store.OutboxDeadLetter(ctx, id, "h", "handler failed"); err != nil {
			t.Fatal(err)
		}
	}
	src := core.SourceRef{Provider: "github", Repo: "acme/api", Number: 7}
	dead("evt_mine", core.TopicInboundEvent, &core.Event{ID: "evt_mine", TeamID: e.team.ID, Type: "issue_comment", Source: src})
	dead("evt_other", core.TopicInboundEvent, &core.Event{ID: "evt_other", TeamID: core.NewID("team"), Type: "issue_comment", Source: src})
	dead("note_mine", core.TopicOutboundNotice, &core.OutboundMessage{FactoryID: f.ID, Kind: "update", Source: src})

	// Only admins see the list, and only the messages of their team.
	e.must(e.do("GET", "/outbox/dead", e.editor.Key, nil), 403)
	r := e.must(e.do("GET", "/outbox/dead", e.admin.Key, nil), 200)
	items := list(r.Body, "messages")
	if len(items) != 2 {
		t.Fatalf("messages = %v, want the 2 of this team", items)
	}
	first := items[0].(map[string]any)
	if first["id"] != "evt_mine" || first["summary"] != "issue_comment github:acme/api#7" || first["last_error"] != "handler failed" {
		t.Fatalf("first = %v", first)
	}
	if first["partition_key"] == "" || first["payload"] != nil {
		t.Fatalf("first = %v: want a partition key and no payload", first)
	}
	r = e.must(e.do("GET", "/outbox/dead", e.other.Key, nil), 200)
	if n := len(list(r.Body, "messages")); n != 0 {
		t.Fatalf("other team sees %d messages", n)
	}

	// Redrive: another team cannot, the owner can, and the message is pending again.
	e.must(e.do("POST", "/outbox/evt_mine/redrive", e.other.Key, nil), 404)
	e.must(e.do("POST", "/outbox/evt_other/redrive", e.admin.Key, nil), 404)
	e.must(e.do("POST", "/outbox/evt_mine/redrive", e.admin.Key, nil), 204)
	e.must(e.do("POST", "/outbox/evt_mine/redrive", e.admin.Key, nil), 404)
	pending, _ := e.store.OutboxList(ctx, core.OutboxPending, 10)
	if len(pending) != 1 || pending[0].ID != "evt_mine" || pending[0].Attempts != 0 {
		t.Fatalf("pending = %v", pending)
	}
}
