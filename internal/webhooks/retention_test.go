package webhooks

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func seedDeliveries(t *testing.T, st core.Store, hook string, n int, newest time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		d := &core.WebhookDelivery{
			WebhookID: hook, DeliveryID: hook + "-" + strconv.Itoa(i), Status: StatusAccepted,
			Body: json.RawMessage(`{"n":1}`), Headers: map[string]string{"X-A": "b"},
			ReceivedAt: newest.Add(-time.Duration(i) * time.Minute),
		}
		if err := st.CreateWebhookDelivery(context.Background(), d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSweepDeliveriesKeepsIdentity(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	seedDeliveries(t, st, "wh1", 80, now)                       // 30 rows lose the body
	seedDeliveries(t, st, "wh2", 10, now.Add(-30*24*time.Hour)) // old, but within the newest 50

	res, err := SweepDeliveries(ctx, st, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stripped != 30 || res.Deleted != 0 {
		t.Fatalf("result = %+v, want 30 stripped and none deleted", res)
	}
	list, _ := st.ListWebhookDeliveries(ctx, "wh1", 200)
	withBody := 0
	for _, d := range list {
		if len(d.Body) > 0 {
			withBody++
		}
	}
	if len(list) != 80 || withBody != core.WebhookKeepNewest {
		t.Fatalf("rows = %d, with body = %d", len(list), withBody)
	}
	// A retry of a stripped delivery is still a duplicate.
	if err := st.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: "wh1", DeliveryID: "wh1-79", Status: StatusAccepted}); err == nil {
		t.Fatal("a retry of a stripped delivery was accepted as new")
	}
	// Eight days later the stripped rows are deleted.
	res, err = SweepDeliveries(ctx, st, now.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 30 {
		t.Fatalf("deleted = %d, want 30", res.Deleted)
	}
}

func TestRetentionLoopOnlyLeaderSweeps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := testdb.New(t)
	seedDeliveries(t, st, "wh1", 60, time.Now())

	leader := make(chan bool, 1)
	var isLeader = func() bool {
		select {
		case v := <-leader:
			leader <- v
			return v
		default:
			return false
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RetentionLoop(ctx, st, isLeader, 5*time.Millisecond, nil, nil)
	}()
	count := func() int {
		list, _ := st.ListWebhookDeliveries(ctx, "wh1", 200)
		n := 0
		for _, d := range list {
			if len(d.Body) > 0 {
				n++
			}
		}
		return n
	}
	time.Sleep(60 * time.Millisecond)
	if n := count(); n != 60 {
		t.Fatalf("a follower stripped rows: %d with body", n)
	}
	leader <- true
	deadline := time.Now().Add(2 * time.Second)
	for count() != core.WebhookKeepNewest && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := count(); n != core.WebhookKeepNewest {
		t.Fatalf("the leader left %d rows with a body, want %d", n, core.WebhookKeepNewest)
	}
	cancel()
	<-done
}
