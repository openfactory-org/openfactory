package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// testDispatchLedger checks core.DispatchLedger.
func testDispatchLedger(t *testing.T, s core.Store) {
	l, has := s.(core.DispatchLedger)
	if !has {
		t.Skip("store has no dispatch ledger")
	}
	key := uid("gh")
	d := &core.DispatchRecord{ActionKey: key, Target: "auto:a1", Ref: "run_1", CreatedAt: base}
	inserted, err := l.InsertDispatch(ctx, d)
	ok(t, err)
	eq(t, inserted, true, "first insert")

	// The same pair does not insert again and does not change the record.
	inserted, err = l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: key, Target: "auto:a1", Ref: "run_other", CreatedAt: base.Add(time.Hour)})
	ok(t, err)
	eq(t, inserted, false, "second insert of one pair")
	got, err := l.GetDispatch(ctx, key, "auto:a1")
	ok(t, err)
	eq(t, got.Ref, "run_1", "ref of the first insert")
	timeEq(t, got.CreatedAt, base, "created_at of the first insert")

	// Another target or another key is another record.
	inserted, err = l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: key, Target: "wi:w1", CreatedAt: base})
	ok(t, err)
	eq(t, inserted, true, "other target")
	inserted, err = l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: uid("gh"), Target: "auto:a1", CreatedAt: base})
	ok(t, err)
	eq(t, inserted, true, "other key")

	_, err = l.GetDispatch(ctx, key, "auto:none")
	isErr(t, err, core.ErrNotFound)
	_, err = l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: "", Target: "auto:a1"})
	isErr(t, err, core.ErrInvalid)

	// An empty time becomes the current time.
	k2 := uid("gh")
	_, err = l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: k2, Target: "auto:a1"})
	ok(t, err)
	got, err = l.GetDispatch(ctx, k2, "auto:a1")
	ok(t, err)
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at was not set")
	}

	// Purge removes the records created before the time, and no others.
	old := uid("old")
	for i := 0; i < 3; i++ {
		_, err = l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: old, Target: "t" + string(rune('a'+i)), CreatedAt: base.Add(-48 * time.Hour)})
		ok(t, err)
	}
	n, err := l.PurgeDispatches(ctx, base.Add(-24*time.Hour))
	ok(t, err)
	eq(t, n, 3, "purged")
	_, err = l.GetDispatch(ctx, old, "ta")
	isErr(t, err, core.ErrNotFound)
	_, err = l.GetDispatch(ctx, key, "auto:a1")
	ok(t, err)
	n, err = l.PurgeDispatches(ctx, base.Add(-24*time.Hour))
	ok(t, err)
	eq(t, n, 0, "second purge")
	// After the purge the pair can be dispatched again.
	inserted, err = l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: old, Target: "ta", CreatedAt: base})
	ok(t, err)
	eq(t, inserted, true, "insert after purge")
}

// testDispatchLedgerRace checks that two inserts of one pair at the same
// moment give one winner.
func testDispatchLedgerRace(t *testing.T, s core.Store) {
	l, has := s.(core.DispatchLedger)
	if !has {
		t.Skip("store has no dispatch ledger")
	}
	for round := 0; round < 5; round++ {
		key := uid("race")
		var mu sync.Mutex
		wins := 0
		errs := raceWriters(func(int) error {
			inserted, err := l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: key, Target: "auto:a1", CreatedAt: base})
			if inserted {
				mu.Lock()
				wins++
				mu.Unlock()
			}
			return err
		})
		ok(t, errs[0])
		ok(t, errs[1])
		eq(t, wins, 1, "winners of one dispatch")
	}
}

// testDispatchLedgerTx checks that a dispatch record commits and rolls back
// with the transaction of the work that it belongs to.
func testDispatchLedgerTx(t *testing.T, s core.Store) {
	l, has := s.(core.DispatchLedger)
	tx, isTx := s.(core.Transactor)
	if !has || !isTx {
		t.Skip("store has no dispatch ledger or no transactions")
	}
	key := uid("tx")
	boom := errors.New("boom")
	err := tx.InTx(ctx, func(ctx2 context.Context) error {
		_, err := l.InsertDispatch(ctx2, &core.DispatchRecord{ActionKey: key, Target: "auto:a1", CreatedAt: base})
		ok(t, err)
		runErr := s.CreateRun(ctx2, &core.Run{FactoryID: anyFactory, ID: uid("run")})
		ok(t, runErr)
		return boom
	})
	isErr(t, err, boom)
	_, err = l.GetDispatch(ctx, key, "auto:a1")
	isErr(t, err, core.ErrNotFound)

	// A record inside a committed transaction stays. A second transaction
	// with the same pair sees that the record exists.
	err = tx.InTx(ctx, func(ctx2 context.Context) error {
		inserted, err := l.InsertDispatch(ctx2, &core.DispatchRecord{ActionKey: key, Target: "auto:a1", Ref: "run_1", CreatedAt: base})
		ok(t, err)
		eq(t, inserted, true, "insert in a transaction")
		// The record is visible inside the transaction.
		_, err = l.GetDispatch(ctx2, key, "auto:a1")
		ok(t, err)
		return nil
	})
	ok(t, err)
	err = tx.InTx(ctx, func(ctx2 context.Context) error {
		inserted, err := l.InsertDispatch(ctx2, &core.DispatchRecord{ActionKey: key, Target: "auto:a1", CreatedAt: base})
		ok(t, err)
		eq(t, inserted, false, "insert of an existing pair in a transaction")
		return nil
	})
	ok(t, err)
	got, err := l.GetDispatch(ctx, key, "auto:a1")
	ok(t, err)
	eq(t, got.Ref, "run_1", "ref after commit")
}

// testWebhookRetention checks core.WebhookDeliveryPruner.
func testWebhookRetention(t *testing.T, s core.Store) {
	p, has := s.(core.WebhookDeliveryPruner)
	if !has {
		t.Skip("store has no webhook delivery retention")
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-core.WebhookIdentityRetention)
	body := json.RawMessage(`{"payload":"x"}`)
	hdr := map[string]string{"X-Event": "push"}

	busy, quiet := uid("wh"), uid("wh")
	mk := func(hook, id string, at time.Time, withBody bool) *core.WebhookDelivery {
		d := &core.WebhookDelivery{WebhookID: hook, DeliveryID: id, Status: "accepted", RunIDs: []string{"run_" + id}, ReceivedAt: at}
		if withBody {
			d.Body, d.Headers = body, hdr
		}
		ok(t, s.CreateWebhookDelivery(ctx, d))
		return d
	}
	// busy: 5 recent rows, 4 within 7 days but not recent, 3 older than 7 days.
	var recent, mid, old []*core.WebhookDelivery
	for i := 0; i < 5; i++ {
		recent = append(recent, mk(busy, "r"+strconv.Itoa(i), now.Add(-time.Duration(i)*time.Minute), true))
	}
	for i := 0; i < 4; i++ {
		mid = append(mid, mk(busy, "m"+strconv.Itoa(i), now.Add(-24*time.Hour-time.Duration(i)*time.Hour), true))
	}
	for i := 0; i < 3; i++ {
		old = append(old, mk(busy, "o"+strconv.Itoa(i), cutoff.Add(-time.Duration(i+1)*time.Hour), true))
	}
	// A rejected row keeps no delivery ID, and an old row that is one of the newest.
	rejected := &core.WebhookDelivery{WebhookID: busy, Status: "rejected", Reason: "bad signature", Headers: hdr, ReceivedAt: now.Add(-48 * time.Hour)}
	ok(t, s.CreateWebhookDelivery(ctx, rejected))
	// quiet: two old rows. They are among the newest 5, so they stay whole.
	q1 := mk(quiet, "q1", cutoff.Add(-72*time.Hour), true)
	q2 := mk(quiet, "q2", cutoff.Add(-96*time.Hour), true)

	// Keep the newest 5 rows of each webhook.
	res, err := p.PruneWebhookDeliveries(ctx, 5, cutoff)
	ok(t, err)
	// busy has 13 rows. Rows 6..13 are old: 4 mid and the rejected row are
	// inside 7 days (stripped), 3 are older (deleted). The mid rows and the
	// rejected row are 5 rows: m0..m3 and rejected.
	// The suite may share the database with earlier cases, so the counts of the
	// sweep are lower bounds. The checks of the rows below are exact.
	atLeast(t, res.Deleted, 3, "deleted")
	atLeast(t, res.Stripped, 5, "stripped")

	for _, d := range recent {
		got, err := s.GetWebhookDelivery(ctx, d.ID)
		ok(t, err)
		jsonEq(t, got.Body, body, "body of a newest row")
		eq(t, got.Headers["X-Event"], "push", "headers of a newest row")
	}
	for _, d := range mid {
		got, err := s.GetWebhookDelivery(ctx, d.ID)
		ok(t, err)
		eq(t, len(got.Body), 0, "body of a stripped row")
		eq(t, len(got.Headers), 0, "headers of a stripped row")
		eq(t, got.DeliveryID, d.DeliveryID, "identity of a stripped row")
		eq(t, got.Status, "accepted", "status of a stripped row")
		eq(t, len(got.RunIDs), 1, "run ids of a stripped row")
		// A retry is still found as a duplicate.
		f, err := s.FindWebhookDelivery(ctx, busy, d.DeliveryID)
		ok(t, err)
		eq(t, f.ID, d.ID, "find stripped row by identity")
		isErr(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: busy, DeliveryID: d.DeliveryID, Status: "accepted", ReceivedAt: now}), core.ErrConflict)
	}
	got, err := s.GetWebhookDelivery(ctx, rejected.ID)
	ok(t, err)
	eq(t, len(got.Headers), 0, "headers of a stripped rejected row")
	eq(t, got.Reason, "bad signature", "reason of a stripped rejected row")
	for _, d := range old {
		_, err := s.GetWebhookDelivery(ctx, d.ID)
		isErr(t, err, core.ErrNotFound)
		_, err = s.FindWebhookDelivery(ctx, busy, d.DeliveryID)
		isErr(t, err, core.ErrNotFound)
	}
	for _, d := range []*core.WebhookDelivery{q1, q2} {
		got, err := s.GetWebhookDelivery(ctx, d.ID)
		ok(t, err)
		jsonEq(t, got.Body, body, "body of a quiet webhook")
	}
	list, err := s.ListWebhookDeliveries(ctx, busy, 100)
	ok(t, err)
	eq(t, len(list), 10, "rows left of the busy webhook")

	// The sweep is stable: a second run changes nothing in these webhooks.
	_, err = p.PruneWebhookDeliveries(ctx, 5, cutoff)
	ok(t, err)
	list, err = s.ListWebhookDeliveries(ctx, busy, 100)
	ok(t, err)
	eq(t, len(list), 10, "rows left of the busy webhook after a second sweep")
	for i, d := range list {
		eq(t, len(d.Body) > 0, i < 5, "body after a second sweep")
	}

	// Later the stripped rows pass 7 days and the sweep deletes them.
	res, err = p.PruneWebhookDeliveries(ctx, 5, now.Add(-12*time.Hour))
	ok(t, err)
	atLeast(t, res.Deleted, 5, "deleted after the identity time")
	list, err = s.ListWebhookDeliveries(ctx, busy, 100)
	ok(t, err)
	eq(t, len(list), 5, "rows left after the identity time")

	// The default number: fewer rows than the limit are never touched.
	_, err = p.PruneWebhookDeliveries(ctx, core.WebhookKeepNewest, now.Add(time.Hour))
	ok(t, err)
	list, err = s.ListWebhookDeliveries(ctx, busy, 100)
	ok(t, err)
	eq(t, len(list), 5, "rows of the busy webhook after the sweep with the default number")
	for _, d := range list {
		eq(t, len(d.Body) > 0, true, "body after the sweep with the default number")
	}
	_, err = p.PruneWebhookDeliveries(ctx, -1, now)
	isErr(t, err, core.ErrInvalid)
}

// testWebhookRetentionManyRows checks the batches of the strip loop.
func testWebhookRetentionManyRows(t *testing.T, s core.Store) {
	p, has := s.(core.WebhookDeliveryPruner)
	if !has {
		t.Skip("store has no webhook delivery retention")
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	hook := uid("wh")
	total := core.WebhookKeepNewest + 450
	for i := 0; i < total; i++ {
		ok(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{
			WebhookID: hook, DeliveryID: "d" + strconv.Itoa(i), Status: "accepted",
			Body: json.RawMessage(`{"n":1}`), ReceivedAt: now.Add(-time.Duration(i) * time.Second),
		}))
	}
	res, err := p.PruneWebhookDeliveries(ctx, core.WebhookKeepNewest, now.Add(-core.WebhookIdentityRetention))
	ok(t, err)
	atLeast(t, res.Stripped, 450, "stripped")
	list, err := s.ListWebhookDeliveries(ctx, hook, 1000)
	ok(t, err)
	eq(t, len(list), total, "rows")
	withBody := 0
	for _, d := range list {
		if len(d.Body) > 0 {
			withBody++
		}
	}
	eq(t, withBody, core.WebhookKeepNewest, "rows with a body")
}

// testLinkExternalIdentity checks core.IdentityLinker.
func testLinkExternalIdentity(t *testing.T, s core.Store) {
	l, has := s.(core.IdentityLinker)
	if !has {
		t.Skip("store has no atomic account link")
	}
	ext := uid("U")
	cur, linked, err := l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "u1", Provider: "slack", ExternalID: ext, Handle: "ann"})
	ok(t, err)
	eq(t, linked, true, "first link")
	eq(t, cur.UserID, "u1", "first link user")
	got, err := s.FindExternalIdentity(ctx, "slack", ext)
	ok(t, err)
	eq(t, got.UserID, "u1", "stored user")
	eq(t, got.Handle, "ann", "stored handle")

	// Another user does not replace the link, and nothing changes.
	cur, linked, err = l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "u2", Provider: "slack", ExternalID: ext, Handle: "mallory"})
	ok(t, err)
	eq(t, linked, false, "link of another user")
	eq(t, cur.UserID, "u1", "owner after a refused link")
	got, err = s.FindExternalIdentity(ctx, "slack", ext)
	ok(t, err)
	eq(t, got.UserID, "u1", "user after a refused link")
	eq(t, got.Handle, "ann", "handle after a refused link")

	// The same user again: the handle is updated when it is given, and kept when it is empty.
	_, linked, err = l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "u1", Provider: "slack", ExternalID: ext, Handle: "annie"})
	ok(t, err)
	eq(t, linked, true, "same user")
	got, _ = s.FindExternalIdentity(ctx, "slack", ext)
	eq(t, got.Handle, "annie", "updated handle")
	cur, linked, err = l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "u1", Provider: "slack", ExternalID: ext})
	ok(t, err)
	eq(t, linked, true, "same user, no handle")
	eq(t, cur.Handle, "annie", "handle kept")
	got, _ = s.FindExternalIdentity(ctx, "slack", ext)
	eq(t, got.Handle, "annie", "stored handle kept")

	// The same external ID at another provider is another account.
	_, linked, err = l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "u2", Provider: "github", ExternalID: ext, Handle: "x"})
	ok(t, err)
	eq(t, linked, true, "other provider")

	// An unlinked row (no user) can be linked by another user.
	ok(t, s.UpsertExternalIdentity(ctx, &core.ExternalIdentity{UserID: "", Provider: "slack", ExternalID: ext, Handle: "annie"}))
	cur, linked, err = l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "u3", Provider: "slack", ExternalID: ext, Handle: "carl"})
	ok(t, err)
	eq(t, linked, true, "link after unlink")
	eq(t, cur.UserID, "u3", "user after unlink")

	_, _, err = l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "", Provider: "slack", ExternalID: ext})
	isErr(t, err, core.ErrInvalid)
	_, _, err = l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: "u1", Provider: "", ExternalID: ext})
	isErr(t, err, core.ErrInvalid)
}

// testLinkExternalIdentityRace checks that two links of one external account
// by two users at the same moment give one owner.
func testLinkExternalIdentityRace(t *testing.T, s core.Store) {
	l, has := s.(core.IdentityLinker)
	if !has {
		t.Skip("store has no atomic account link")
	}
	for round := 0; round < 5; round++ {
		ext := uid("U")
		users := [2]string{"u_a", "u_b"}
		var mu sync.Mutex
		var winners []string
		errs := raceWriters(func(i int) error {
			_, linked, err := l.LinkExternalIdentity(ctx, &core.ExternalIdentity{UserID: users[i], Provider: "slack", ExternalID: ext, Handle: users[i]})
			if linked {
				mu.Lock()
				winners = append(winners, users[i])
				mu.Unlock()
			}
			return err
		})
		ok(t, errs[0])
		ok(t, errs[1])
		eq(t, len(winners), 1, "owners of one account")
		got, err := s.FindExternalIdentity(ctx, "slack", ext)
		ok(t, err)
		eq(t, got.UserID, winners[0], "stored owner")
	}
}

func atLeast(t *testing.T, got, want int, what string) {
	t.Helper()
	if got < want {
		t.Fatalf("%s: got %d, want at least %d", what, got, want)
	}
}
