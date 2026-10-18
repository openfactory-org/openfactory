package triggers

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	busevents "github.com/openfactory-org/openfactory/internal/events"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// schedEnv is a SQLite in-memory store with one factory and its automations, and a
// durable bus on the store.
type schedEnv struct {
	store *sqlstore.Store
	bus   *busevents.Durable
	fac   *core.Factory
	now   time.Time
	s     *Scheduler
}

func newSchedEnv(t *testing.T, autos ...core.Automation) *schedEnv {
	t.Helper()
	ctx := context.Background()
	st := testdb.New(t)
	if err := st.CreateTeam(ctx, &core.Team{ID: "team1", Name: "team"}); err != nil {
		t.Fatal(err)
	}
	fac := &core.Factory{ID: "fac_1", TeamID: "team1", Name: "f", Alias: "f", Source: core.DefinitionGit}
	if err := st.CreateFactory(ctx, fac); err != nil {
		t.Fatal(err)
	}
	for i := range autos {
		autos[i].FactoryID = fac.ID
	}
	if err := st.ReplaceFactoryResources(ctx, fac.ID, nil, autos, nil, nil); err != nil {
		t.Fatal(err)
	}
	bus := busevents.NewForStore(st, busevents.DurableOptions{ReplicaID: "r", PollInterval: time.Hour})
	t.Cleanup(func() { _ = bus.Close() })
	e := &schedEnv{store: st, bus: bus, fac: fac, now: time.Date(2026, 3, 1, 1, 59, 30, 0, time.UTC)}
	e.s = NewScheduler(st, bus, SchedulerOptions{Now: func() time.Time { return e.now }, DisableAutoReload: true})
	if err := e.s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	return e
}

// outboxEvents returns the IDs of the inbound events in the outbox.
func (e *schedEnv) outboxEvents(t *testing.T) []string {
	t.Helper()
	msgs, err := e.store.OutboxList(context.Background(), core.OutboxPending, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range msgs {
		if m.Topic == core.TopicInboundEvent {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

func TestScheduleSlotAndEventInOneTransaction(t *testing.T) {
	e := newSchedEnv(t, schedAuto("aut_1", "nightly", "0 2 * * *"))
	ctx := context.Background()
	slot := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	e.s.tick(ctx, slot.Add(time.Second))
	ids := e.outboxEvents(t)
	if len(ids) != 1 || ids[0] != ScheduleEventID(scheduleKey("aut_1", "nightly"), slot) {
		t.Fatalf("outbox events %v", ids)
	}
	if last, ok := e.s.lastFired(ctx, scheduleKey("aut_1", "nightly")); !ok || !last.Equal(slot) {
		t.Fatalf("last fired = %v %v", last, ok)
	}

	// When the event cannot be written, the slot is not stored either.
	e2 := newSchedEnv(t, schedAuto("aut_1", "nightly", "0 2 * * *"))
	e2.s.bus = failingBus{}
	e2.s.tick(ctx, slot.Add(time.Second))
	if _, ok := e2.s.lastFired(ctx, scheduleKey("aut_1", "nightly")); ok {
		t.Fatal("slot stored although its event was rolled back")
	}
	list, err := ListSchedules(ctx, e2.store, e2.fac.ID, slot.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].LastError == "" {
		t.Fatalf("schedules %+v, want a last error", list)
	}
}

// failingBus dooms the transaction of every publish, like the durable bus
// when the outbox write fails.
type failingBus struct{}

func (failingBus) Publish(ctx context.Context, _ string, _ any) {
	core.FailTx(ctx, errors.New("outbox unavailable"))
}
func (failingBus) Subscribe(string) (<-chan any, func()) { return make(chan any), func() {} }

func TestSchedulePauseWithoutDefinitionChange(t *testing.T) {
	e := newSchedEnv(t, schedAuto("aut_1", "nightly", "0 2 * * *"))
	ctx := context.Background()
	if err := SetSchedulePaused(ctx, e.store, "aut_1", "nightly", true, "alice", e.now); err != nil {
		t.Fatal(err)
	}
	list, err := ListSchedules(ctx, e.store, e.fac.ID, e.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Paused || list[0].PausedBy != "alice" || list[0].NextRunAt != nil || list[0].ID != "aut_1:nightly" {
		t.Fatalf("paused schedule %+v", list)
	}
	e.s.tick(ctx, time.Date(2026, 3, 1, 2, 0, 1, 0, time.UTC))
	if ids := e.outboxEvents(t); len(ids) != 0 {
		t.Fatalf("paused schedule fired: %v", ids)
	}

	if err := SetSchedulePaused(ctx, e.store, "aut_1", "nightly", false, "alice", e.now); err != nil {
		t.Fatal(err)
	}
	list, _ = ListSchedules(ctx, e.store, e.fac.ID, time.Date(2026, 3, 1, 3, 0, 0, 0, time.UTC))
	if list[0].Paused || list[0].NextRunAt == nil || !list[0].NextRunAt.Equal(time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("resumed schedule %+v", list[0])
	}
	e.s.tick(ctx, time.Date(2026, 3, 2, 2, 0, 1, 0, time.UTC))
	if ids := e.outboxEvents(t); len(ids) != 1 {
		t.Fatalf("resumed schedule events %v", ids)
	}
	list, _ = ListSchedules(ctx, e.store, e.fac.ID, time.Date(2026, 3, 2, 2, 0, 1, 0, time.UTC))
	if list[0].LastRunAt == nil || !list[0].LastRunAt.Equal(time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("last run %+v", list[0].LastRunAt)
	}

	a, tr, err := FindSchedule(ctx, e.store, e.fac.ID, "aut_1:nightly")
	if err != nil || a.ID != "aut_1" || tr.Schedule.Name != "nightly" {
		t.Fatalf("find = %+v %+v %v", a, tr, err)
	}
	if _, _, err := FindSchedule(ctx, e.store, e.fac.ID, "aut_1:other"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("find unknown = %v", err)
	}
}

func TestScheduleConcurrencySkip(t *testing.T) {
	skip := schedAuto("aut_1", "", "@every 1h")
	skip.Triggers[0].Concurrency = core.ScheduleConcurrencySkip
	allow := schedAuto("aut_2", "", "@every 1h")
	e := newSchedEnv(t, skip, allow)
	ctx := context.Background()
	for _, id := range []string{"aut_1", "aut_2"} {
		r := &core.Run{TeamID: "team1", FactoryID: e.fac.ID, State: core.RunRunning, TriggeredBy: "automation:" + id}
		if err := e.store.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var slot2 time.Time
	for _, info := range e.s.Schedules() {
		if info.AutomationID == "aut_2" {
			slot2 = info.Next
		}
	}
	first := e.now.Add(time.Hour)
	e.s.tick(ctx, first)
	if ids := e.outboxEvents(t); len(ids) != 1 || ids[0] != ScheduleEventID(scheduleKey("aut_2", ""), slot2) {
		t.Fatalf("events with the skip schedule busy: %v", ids)
	}
	list, _ := ListSchedules(ctx, e.store, e.fac.ID, first)
	for _, s := range list {
		if s.AutomationID == "aut_1" && (s.Concurrency != "skip" || s.LastRunAt == nil) {
			t.Fatalf("skipped schedule %+v: the slot must count as handled", s)
		}
		if s.AutomationID == "aut_2" && s.Concurrency != "allow" {
			t.Fatalf("default concurrency %q", s.Concurrency)
		}
	}

	// The run finishes: the next slot fires.
	runs, _ := e.store.ListRuns(ctx, core.RunFilter{FactoryID: e.fac.ID})
	for i := range runs {
		runs[i].State = core.RunSucceeded
		if err := e.store.UpdateRun(ctx, &runs[i]); err != nil {
			t.Fatal(err)
		}
	}
	e.s.tick(ctx, first.Add(time.Hour))
	if ids := e.outboxEvents(t); len(ids) != 3 {
		t.Fatalf("events after the run finished: %v", ids)
	}
}
