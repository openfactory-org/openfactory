package events

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// notifyCounter counts the live notifications about run messages.
type notifyCounter struct {
	core.Coordinator
	runMessages atomic.Int64
}

func (c *notifyCounter) Notify(ctx context.Context, payload string) error {
	if strings.Contains(payload, `"t":"`+core.TopicRunMessage+`"`) {
		c.runMessages.Add(1)
	}
	return c.Coordinator.Notify(ctx, payload)
}

func TestDurablePublishRunMessagesOneNotificationPerBatch(t *testing.T) {
	st := testdb.New(t)
	counter := &notifyCounter{Coordinator: st}
	a := NewDurable(counter, st, fastOpts("a"))
	b := NewDurable(st, st, fastOpts("b"))
	defer a.Close()
	defer b.Close()
	ctx := context.Background()
	run := &core.Run{FactoryID: "fac_test", TeamID: "t", State: core.RunRunning}
	if err := st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	aMsgs, unsubA := a.Subscribe(core.TopicRunMessage)
	defer unsubA()
	bMsgs, unsubB := b.Subscribe(core.TopicRunMessage)
	defer unsubB()

	const n = 20
	var batch []*core.RunMessage
	err := st.InTx(ctx, func(ctx context.Context) error {
		for i := 0; i < n; i++ {
			m := &core.RunMessage{RunID: run.ID, Role: "tool_call", Content: fmt.Sprintf("call %d", i)}
			if err := st.AppendRunMessage(ctx, m); err != nil {
				return err
			}
			batch = append(batch, m)
		}
		a.PublishRunMessages(ctx, batch)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if got := recv(t, aMsgs).(*core.RunMessage); got != batch[i] {
			t.Fatalf("local message %d = %#v", i, got)
		}
		got := recv(t, bMsgs).(*core.RunMessage)
		if got.Seq != batch[i].Seq || got.Content != fmt.Sprintf("call %d", i) {
			t.Fatalf("remote message %d = seq %d %q, want seq %d", i, got.Seq, got.Content, batch[i].Seq)
		}
	}
	if c := counter.runMessages.Load(); c != 1 {
		t.Fatalf("notifications for one batch = %d, want 1", c)
	}
	select {
	case v := <-bMsgs:
		t.Fatalf("extra remote message: %#v", v)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDurablePublishRunMessagesNothingOnRollback(t *testing.T) {
	st := testdb.New(t)
	a := NewForStore(st, fastOpts("a"))
	defer a.Close()
	ctx := context.Background()
	ch, unsub := a.Subscribe(core.TopicRunMessage)
	defer unsub()
	_ = st.InTx(ctx, func(ctx context.Context) error {
		a.PublishRunMessages(ctx, []*core.RunMessage{{RunID: "run_x", Seq: 1}, {RunID: "run_x", Seq: 2}})
		return fmt.Errorf("roll back")
	})
	select {
	case v := <-ch:
		t.Fatalf("message published for a rolled-back transaction: %#v", v)
	case <-time.After(50 * time.Millisecond):
	}
}
