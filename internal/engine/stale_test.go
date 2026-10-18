package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// staleStore makes the next writes of a kind fail with core.ErrStale, like
// a versioned store after another writer changed the row. Before it fails,
// it runs the "other writer" hook on the inner store. It embeds only
// core.Store, so the engine sees no transactions and the other writer can
// write between the read and the write of the engine.
type staleStore struct {
	core.Store
	mu        sync.Mutex
	staleWI   int
	staleRun  int
	staleIbx  int
	otherWI   func()
	otherIbx  func()
	wiWrites  int
	runWrites int
}

func (s *staleStore) take(n *int, hook func()) bool {
	s.mu.Lock()
	fail := *n > 0
	if fail {
		*n--
	}
	s.mu.Unlock()
	if fail && hook != nil {
		hook()
	}
	return fail
}

func (s *staleStore) UpdateWorkItem(ctx context.Context, w *core.WorkItem) error {
	if s.take(&s.staleWI, s.otherWI) {
		return fmt.Errorf("%w: work item %s changed", core.ErrStale, w.ID)
	}
	s.mu.Lock()
	s.wiWrites++
	s.mu.Unlock()
	return s.Store.UpdateWorkItem(ctx, w)
}

func (s *staleStore) UpdateRun(ctx context.Context, r *core.Run) error {
	if s.take(&s.staleRun, nil) {
		return fmt.Errorf("%w: run %s changed", core.ErrStale, r.ID)
	}
	s.mu.Lock()
	s.runWrites++
	s.mu.Unlock()
	return s.Store.UpdateRun(ctx, r)
}

func (s *staleStore) UpdateInboxItem(ctx context.Context, i *core.InboxItem) error {
	if s.take(&s.staleIbx, s.otherIbx) {
		return fmt.Errorf("%w: inbox item %s changed", core.ErrStale, i.ID)
	}
	return s.Store.UpdateInboxItem(ctx, i)
}

func newStaleEngine(t *testing.T) (*Engine, *staleStore, *core.WorkItem) {
	t.Helper()
	ctx := context.Background()
	fs := newTestStore(t)
	st := &staleStore{Store: fs}
	eng := New(Options{Store: st, Bus: NewFakeBus(), Compile: CompilerFor(DefaultTestDefinition()), Match: SimpleMatch,
		Forges: FakeForges{}, Logger: DiscardLogger()})
	wi := &core.WorkItem{ID: core.NewID("wi"), FactoryID: "fac_1", Title: "t", Stage: core.StageIntake, CreatedAt: time.Now()}
	if err := fs.CreateWorkItem(ctx, wi); err != nil {
		t.Fatal(err)
	}
	return eng, st, wi
}

// A stage change that meets a newer version reads the work item again
// and keeps the change of the other writer.
func TestSetStageRetriesStaleWrite(t *testing.T) {
	ctx := context.Background()
	eng, st, wi := newStaleEngine(t)
	st.staleWI = 2
	st.otherWI = func() {
		cur, _ := st.GetWorkItem(ctx, wi.ID)
		cur.Title = "renamed by another writer"
		_ = st.Store.UpdateWorkItem(ctx, cur)
	}
	if err := eng.SetStage(ctx, wi.ID, core.StageBuilding, "tester", "go"); err != nil {
		t.Fatalf("set stage: %v", err)
	}
	got, _ := st.GetWorkItem(ctx, wi.ID)
	if got.Stage != core.StageBuilding {
		t.Fatalf("stage = %s, want building", got.Stage)
	}
	if got.Title != "renamed by another writer" {
		t.Fatalf("title = %q: the retry did not read the newer version", got.Title)
	}
	if st.wiWrites != 1 {
		t.Fatalf("work item writes = %d, want 1", st.wiWrites)
	}
}

// Engine.updateRun reads the run again after a stale write and applies
// fn to the new version.
func TestUpdateRunRetriesStaleWrite(t *testing.T) {
	ctx := context.Background()
	fs := newTestStore(t)
	st := &staleStore{Store: fs, staleRun: 2}
	if err := fs.CreateFactory(ctx, &core.Factory{ID: "fac_1", TeamID: "team1", Name: "payments", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := &core.Run{ID: core.NewID("run"), TeamID: "team1", FactoryID: "fac_1", State: core.RunRunning, CreatedAt: time.Now()}
	if err := fs.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	eng := New(Options{Store: st, Bus: NewFakeBus(), Logger: DiscardLogger()})
	calls := 0
	got, _, err := eng.updateRun(ctx, r.ID, func(r *core.Run) bool {
		calls++
		r.Result = "done"
		return true
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if calls != 3 || st.runWrites != 1 || got.Result != "done" {
		t.Fatalf("calls = %d, writes = %d, result = %q; want 3, 1, done", calls, st.runWrites, got.Result)
	}
}

// An inbox item that another writer resolved meanwhile keeps that
// resolution, and the late resolution gets a conflict.
func TestResolveInboxItemStale(t *testing.T) {
	ctx := context.Background()
	eng, st, wi := newStaleEngine(t)
	it := &core.InboxItem{ID: core.NewID("inbox"), FactoryID: "fac_1", WorkItemID: wi.ID, Kind: core.InboxQuestion, Title: "?"}
	if err := st.CreateInboxItem(ctx, it); err != nil {
		t.Fatal(err)
	}

	// The other writer changes the item but does not resolve it: the retry wins.
	st.staleIbx = 1
	st.otherIbx = func() {
		cur, _ := st.GetInboxItem(ctx, it.ID)
		cur.Body = "edited"
		_ = st.Store.UpdateInboxItem(ctx, cur)
	}
	mine := *it
	if err := eng.resolveInboxItem(ctx, &mine, "yes", "alice"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, _ := st.GetInboxItem(ctx, it.ID)
	if !got.Resolved || got.Resolution != "yes" || got.Body != "edited" {
		t.Fatalf("got %+v, want resolved yes with the edited body", got)
	}

	// The other writer resolves the item first: the late write is refused.
	it2 := &core.InboxItem{ID: core.NewID("inbox"), FactoryID: "fac_1", Kind: core.InboxQuestion, Title: "?"}
	if err := st.CreateInboxItem(ctx, it2); err != nil {
		t.Fatal(err)
	}
	st.staleIbx = 1
	st.otherIbx = func() {
		cur, _ := st.GetInboxItem(ctx, it2.ID)
		cur.Resolved, cur.Resolution = true, "no"
		_ = st.Store.UpdateInboxItem(ctx, cur)
	}
	late := *it2
	err := eng.resolveInboxItem(ctx, &late, "yes", "bob")
	if !errors.Is(err, core.ErrConflict) || errors.Is(err, core.ErrStale) {
		t.Fatalf("late resolve err = %v, want a plain conflict", err)
	}
	got, _ = st.GetInboxItem(ctx, it2.ID)
	if got.Resolution != "no" {
		t.Fatalf("resolution = %q, want the first one (no)", got.Resolution)
	}
}

func TestRetryStaleLimits(t *testing.T) {
	ctx := context.Background()
	calls := 0
	err := retryStale(ctx, func() error {
		calls++
		return fmt.Errorf("%w: busy", core.ErrStale)
	})
	if !errors.Is(err, core.ErrStale) || calls != storeutil.StaleAttempts {
		t.Fatalf("err = %v after %d calls, want ErrStale after %d", err, calls, storeutil.StaleAttempts)
	}
	calls = 0
	err = retryStale(ctx, func() error {
		calls++
		return fmt.Errorf("%w: work item is complete", core.ErrConflict)
	})
	if calls != 1 || !errors.Is(err, core.ErrConflict) {
		t.Fatalf("a plain conflict is not retried: %d calls, err %v", calls, err)
	}
}
