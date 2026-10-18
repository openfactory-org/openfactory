package events

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

type txStore interface {
	core.Store
	core.Transactor
	core.Coordinator
}

func txStores(t *testing.T) map[string]func(t *testing.T) txStore {
	return map[string]func(t *testing.T) txStore{
		"sqlite": func(t *testing.T) txStore {
			s, err := sqlite.OpenStore(context.Background(), filepath.Join(t.TempDir(), "of.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
}

func noRecv(t *testing.T, ch <-chan any, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s: unexpected delivery %#v", what, v)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestDurablePublishInTxCommits(t *testing.T) {
	for name, open := range txStores(t) {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			d := NewForStore(st, fastOpts("a"))
			defer d.Close()
			inbound, unsub := d.Subscribe(core.TopicInboundEvent)
			defer unsub()
			live, unsubLive := d.Subscribe(core.TopicRunUpdated)
			defer unsubLive()
			local, unsubLocal := d.SubscribeLocal(core.TopicRunUpdated)
			defer unsubLocal()

			ctx := context.Background()
			run := &core.Run{FactoryID: "fac_test", ID: core.NewID("run")}
			err := st.InTx(ctx, func(ctx context.Context) error {
				if err := st.CreateRun(ctx, run); err != nil {
					return err
				}
				d.Publish(ctx, core.TopicRunUpdated, run)
				d.Publish(ctx, core.TopicInboundEvent, &core.Event{ID: "evt_tx", Provider: "x"})
				// Nothing is visible to subscribers before the commit.
				select {
				case v := <-live:
					t.Errorf("live update delivered before commit: %#v", v)
				case v := <-local:
					t.Errorf("local update delivered before commit: %#v", v)
				case <-time.After(50 * time.Millisecond):
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := recv(t, live).(*core.Run); got.ID != run.ID {
				t.Fatalf("live: got %s", got.ID)
			}
			if got := recv(t, local).(*core.Run); got.ID != run.ID {
				t.Fatalf("local: got %s", got.ID)
			}
			if got := recv(t, inbound).(*core.Event); got.ID != "evt_tx" {
				t.Fatalf("inbound: got %s", got.ID)
			}
		})
	}
}

func TestDurablePublishInTxRollback(t *testing.T) {
	for name, open := range txStores(t) {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			d := NewForStore(st, fastOpts("a"))
			defer d.Close()
			inbound, unsub := d.Subscribe(core.TopicInboundEvent)
			defer unsub()
			live, unsubLive := d.Subscribe(core.TopicRunUpdated)
			defer unsubLive()

			ctx := context.Background()
			run := &core.Run{FactoryID: "fac_test", ID: core.NewID("run")}
			boom := errors.New("state change failed")
			err := st.InTx(ctx, func(ctx context.Context) error {
				d.Publish(ctx, core.TopicInboundEvent, &core.Event{ID: "evt_rb", Provider: "x"})
				d.Publish(ctx, core.TopicRunUpdated, run)
				return boom
			})
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			noRecv(t, inbound, "durable after rollback")
			noRecv(t, live, "live after rollback")
			pending, err := st.OutboxList(ctx, core.OutboxPending, 10)
			if err != nil || len(pending) != 0 {
				t.Fatalf("outbox after rollback: %v %v", pending, err)
			}
		})
	}
}

// A failed outbox write inside a transaction rolls the transaction back
// instead of delivering in process.
func TestDurableOutboxFailureFailsTx(t *testing.T) {
	for name, open := range txStores(t) {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			d := NewForStore(st, fastOpts("a"))
			defer d.Close()
			notices, unsub := d.Subscribe(core.TopicOutboundNotice)
			defer unsub()

			ctx := context.Background()
			run := &core.Run{FactoryID: "fac_test", ID: core.NewID("run")}
			err := st.InTx(ctx, func(ctx context.Context) error {
				if err := st.CreateRun(ctx, run); err != nil {
					return err
				}
				d.Publish(ctx, core.TopicOutboundNotice, make(chan int)) // cannot be encoded
				return nil
			})
			if !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if _, err := st.GetRun(ctx, run.ID); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("state change survived the failed outbox write: %v", err)
			}
			noRecv(t, notices, "no in-process fallback inside a transaction")
			if fb := d.DurableStats().Fallbacks; fb != 0 {
				t.Fatalf("fallbacks = %d", fb)
			}
		})
	}
}

func TestBusPublishInTxDefersDelivery(t *testing.T) {
	st := testdb.New(t)
	b := New()
	defer b.Close()
	ch, unsub := b.Subscribe("topic")
	defer unsub()
	ctx := context.Background()
	_ = st.InTx(ctx, func(ctx context.Context) error {
		b.Publish(ctx, "topic", "rolled back")
		return errors.New("rollback")
	})
	noRecv(t, ch, "bus after rollback")
	if err := st.InTx(ctx, func(ctx context.Context) error {
		b.Publish(ctx, "topic", "committed")
		if len(ch) != 0 {
			t.Error("delivered before commit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, ch); got != "committed" {
		t.Fatalf("got %v", got)
	}
}
