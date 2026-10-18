package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// raceWriters starts two writers at the same time. Each one writes its own
// copy of the same stored version. It returns the two errors.
func raceWriters(write func(i int) error) [2]error {
	var errs [2]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = write(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// oneStale checks that exactly one of two writers won and the other got an
// error that wraps core.ErrStale and core.ErrConflict. It returns the index
// of the winner.
func oneStale(t *testing.T, errs [2]error, what string) int {
	t.Helper()
	winner := -1
	for i, err := range errs {
		switch {
		case err == nil:
			if winner >= 0 {
				t.Fatalf("%s: both writers succeeded", what)
			}
			winner = i
		case errors.Is(err, core.ErrStale) && errors.Is(err, core.ErrConflict):
		default:
			t.Fatalf("%s: writer %d: got %v, want core.ErrStale", what, i, err)
		}
	}
	if winner < 0 {
		t.Fatalf("%s: no writer succeeded: %v", what, errs)
	}
	return winner
}

func testWorkItemVersion(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	w := &core.WorkItem{FactoryID: f.ID, Title: "versioned"}
	ok(t, s.CreateWorkItem(ctx, w))
	eq(t, w.Version, int64(1), "version after create")
	got, err := s.GetWorkItem(ctx, w.ID)
	ok(t, err)
	eq(t, got.Version, int64(1), "stored version")

	stale := *got
	got.Title = "first"
	ok(t, s.UpdateWorkItem(ctx, got))
	eq(t, got.Version, int64(2), "caller version after update")
	stale.Title = "lost"
	err = s.UpdateWorkItem(ctx, &stale)
	isErr(t, err, core.ErrStale)
	isErr(t, err, core.ErrConflict)
	eq(t, stale.Version, int64(1), "caller version is kept after a conflict")
	again, err := s.GetWorkItem(ctx, w.ID)
	ok(t, err)
	eq(t, again.Title, "first", "stale write is not stored")
	eq(t, again.Version, int64(2), "version after conflict")

	// Lists and the source lookup return the version too.
	list, err := s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID})
	ok(t, err)
	eq(t, len(list), 1, "listed")
	eq(t, list[0].Version, int64(2), "listed version")

	// The same struct can be written twice.
	got.Title = "second"
	ok(t, s.UpdateWorkItem(ctx, got))
	eq(t, got.Version, int64(3), "version after second update")

	// Two writers start from the same version: one gets ErrStale.
	cur, err := s.GetWorkItem(ctx, w.ID)
	ok(t, err)
	copies := [2]core.WorkItem{*cur, *cur}
	errs := raceWriters(func(i int) error {
		copies[i].Title = []string{"writer 0", "writer 1"}[i]
		return s.UpdateWorkItem(ctx, &copies[i])
	})
	win := oneStale(t, errs, "work item writers")
	again, err = s.GetWorkItem(ctx, w.ID)
	ok(t, err)
	eq(t, again.Title, copies[win].Title, "the winner's write is stored")
	eq(t, again.Version, int64(4), "one more version")

	isErr(t, s.UpdateWorkItem(ctx, &core.WorkItem{ID: uid("wi"), FactoryID: f.ID, Version: 1}), core.ErrNotFound)

	if tr, isTx := s.(core.Transactor); isTx {
		ok(t, tr.InTx(ctx, func(ctx context.Context) error {
			cur, err := s.GetWorkItem(ctx, w.ID)
			if err != nil {
				return err
			}
			cur.Title = "in tx"
			return s.UpdateWorkItem(ctx, cur)
		}))
		again, err = s.GetWorkItem(ctx, w.ID)
		ok(t, err)
		eq(t, again.Version, int64(5), "version after tx update")
		eq(t, again.Title, "in tx", "tx title")
	}
}

func testInboxVersion(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	it := &core.InboxItem{FactoryID: f.ID, Kind: core.InboxApproval, Title: "Approve?", Options: []string{"approve", "reject"}}
	ok(t, s.CreateInboxItem(ctx, it))
	eq(t, it.Version, int64(1), "version after create")
	got, err := s.GetInboxItem(ctx, it.ID)
	ok(t, err)
	eq(t, got.Version, int64(1), "stored version")

	// Two people resolve the same item at the same time: one wins.
	copies := [2]core.InboxItem{*got, *got}
	errs := raceWriters(func(i int) error {
		at := base.Add(time.Duration(i) * time.Second)
		copies[i].Resolved, copies[i].Resolution, copies[i].ResolvedAt = true, copies[i].Options[i], &at
		return s.UpdateInboxItem(ctx, &copies[i])
	})
	win := oneStale(t, errs, "inbox writers")
	eq(t, copies[win].Version, int64(2), "winner version")
	eq(t, copies[1-win].Version, int64(1), "loser keeps its version")
	again, err := s.GetInboxItem(ctx, it.ID)
	ok(t, err)
	eq(t, again.Resolution, copies[win].Resolution, "the winner's resolution is stored")
	eq(t, again.Version, int64(2), "stored version")

	list, err := s.ListInboxItems(ctx, f.ID, true)
	ok(t, err)
	eq(t, len(list), 1, "listed")
	eq(t, list[0].Version, int64(2), "listed version")

	isErr(t, s.UpdateInboxItem(ctx, &core.InboxItem{ID: uid("inbox"), Version: 1}), core.ErrNotFound)
}

func testRunVersionRace(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	r := &core.Run{FactoryID: anyFactory, TeamID: tm.ID, Name: "raced"}
	ok(t, s.CreateRun(ctx, r))
	copies := [2]core.Run{*r, *r}
	errs := raceWriters(func(i int) error {
		copies[i].Result = []string{"writer 0", "writer 1"}[i]
		return s.UpdateRun(ctx, &copies[i])
	})
	win := oneStale(t, errs, "run writers")
	got, err := s.GetRun(ctx, r.ID)
	ok(t, err)
	eq(t, got.Result, copies[win].Result, "the winner's write is stored")
	eq(t, got.Version, int64(2), "stored version")
}

// testLateLockHolder pauses the holder of an entity lock for longer than the
// lock TTL. A second holder takes the lock and writes. The late write of the
// first holder fails with core.ErrStale, so it cannot replace newer data.
func testLateLockHolder(t *testing.T, s core.Store, c core.Coordinator) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	w := &core.WorkItem{FactoryID: f.ID, Title: "locked", Stage: core.StageBuilding}
	ok(t, s.CreateWorkItem(ctx, w))
	key := "wi:" + w.ID
	const ttl = 30 * time.Second

	got, err := c.TryLock(ctx, key, "replica-a", base, base.Add(ttl))
	ok(t, err)
	eq(t, got, true, "a locks")
	aCopy, err := s.GetWorkItem(ctx, w.ID)
	ok(t, err)

	// Replica a pauses (for example a long garbage collection) past the TTL.
	later := base.Add(ttl + time.Second)
	got, err = c.TryLock(ctx, key, "replica-b", later, later.Add(ttl))
	ok(t, err)
	eq(t, got, true, "b takes the expired lock")
	bCopy, err := s.GetWorkItem(ctx, w.ID)
	ok(t, err)
	bCopy.Stage = core.StageComplete
	ok(t, s.UpdateWorkItem(ctx, bCopy))
	ok(t, c.Unlock(ctx, key, "replica-b"))

	// Replica a wakes up and writes what it read before the pause.
	aCopy.Stage = core.StagePlanning
	err = s.UpdateWorkItem(ctx, aCopy)
	isErr(t, err, core.ErrStale)
	isErr(t, err, core.ErrConflict)
	stored, err := s.GetWorkItem(ctx, w.ID)
	ok(t, err)
	eq(t, stored.Stage, core.StageComplete, "the late write did not replace newer data")
}
