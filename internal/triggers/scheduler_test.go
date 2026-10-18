package triggers

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// fakeStore implements the parts of core.Store the scheduler uses.
type fakeStore struct {
	core.Store
	mu    sync.Mutex
	autos []core.Automation
	kv    map[string]string
}

func newFakeStore() *fakeStore { return &fakeStore{kv: map[string]string{}} }

func (f *fakeStore) ListAllEnabledAutomations(context.Context) ([]core.Automation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Automation(nil), f.autos...), nil
}

func (f *fakeStore) GetFactory(_ context.Context, id string) (*core.Factory, error) {
	return &core.Factory{ID: id, TeamID: "team_" + id}, nil
}

func (f *fakeStore) KVGet(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.kv[k]
	if !ok {
		return "", core.ErrNotFound
	}
	return v, nil
}

func (f *fakeStore) KVSet(_ context.Context, k, v string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[k] = v
	return nil
}

type fakeBus struct {
	mu     sync.Mutex
	events []*core.Event
	subs   map[string][]chan any
}

func (b *fakeBus) Publish(_ context.Context, topic string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ev, ok := payload.(*core.Event); ok && topic == core.TopicInboundEvent {
		b.events = append(b.events, ev)
	}
	for _, ch := range b.subs[topic] {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (b *fakeBus) Subscribe(topic string) (<-chan any, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs == nil {
		b.subs = map[string][]chan any{}
	}
	ch := make(chan any, 8)
	b.subs[topic] = append(b.subs[topic], ch)
	return ch, func() {}
}

func (b *fakeBus) published() []*core.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*core.Event(nil), b.events...)
}

func schedAuto(id, name, cron string) core.Automation {
	return core.Automation{ID: id, FactoryID: "fac_1", Name: id, Enabled: true, Triggers: []core.Trigger{
		{Provider: "schedule", Event: "cron_fired", Schedule: &core.Schedule{Name: name, Cron: cron}},
	}}
}

func TestValidateCron(t *testing.T) {
	valid := []string{"0 9 * * 1-5", "*/15 * * * *", "@daily", "@hourly", "@weekly", "@monthly", "@yearly", "@midnight", "@every 1h", "@every 90m"}
	for _, c := range valid {
		if err := ValidateCron(c); err != nil {
			t.Errorf("ValidateCron(%q) = %v", c, err)
		}
	}
	invalid := []string{"", "0 9 * *", "0 0 9 * * *", "61 * * * *", "@sometimes", "@every 10s", "@every nope", "CRON_TZ=Europe/Paris 0 9 * * *", "TZ=UTC 0 9 * * *"}
	for _, c := range invalid {
		if err := ValidateCron(c); err == nil {
			t.Errorf("ValidateCron(%q) = nil, want error", c)
		}
	}
}

func TestCronRunsInUTC(t *testing.T) {
	s, err := parseCron("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	loc := time.FixedZone("X", 5*3600)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, loc) // 2025-12-31 19:00 UTC
	next := s.Next(from).UTC()
	if want := time.Date(2025, 12, 31, 9, 0, 0, 0, time.UTC).Add(24 * time.Hour); !next.Equal(want) {
		t.Fatalf("next = %v, want %v", next, want)
	}
}

func TestSchedulerFiresAndDedupes(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.autos = []core.Automation{schedAuto("aut_1", "nightly", "0 2 * * *"), {ID: "aut_x", Enabled: true, Triggers: []core.Trigger{{Provider: "github", Event: "push"}}}}
	bus := &fakeBus{}
	now := time.Date(2026, 3, 1, 1, 59, 30, 0, time.UTC)
	s := NewScheduler(store, bus, SchedulerOptions{Now: func() time.Time { return now }, DisableAutoReload: true})
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	infos := s.Schedules()
	if len(infos) != 1 || infos[0].Name != "nightly" || !infos[0].Next.Equal(time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("schedules = %+v", infos)
	}

	s.tick(ctx, time.Date(2026, 3, 1, 1, 59, 59, 0, time.UTC))
	if n := len(bus.published()); n != 0 {
		t.Fatalf("fired early: %d", n)
	}
	fireAt := time.Date(2026, 3, 1, 2, 0, 0, 100, time.UTC)
	s.tick(ctx, fireAt)
	evs := bus.published()
	if len(evs) != 1 {
		t.Fatalf("published %d events, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Provider != "schedule" || ev.Type != "cron_fired" || ev.FactoryID != "fac_1" || ev.TeamID != "team_fac_1" {
		t.Fatalf("bad event: %+v", ev)
	}
	if ev.Attributes[AttrAutomationID] != "aut_1" || ev.Attributes[AttrScheduleName] != "nightly" {
		t.Fatalf("bad attributes: %+v", ev.Attributes)
	}
	if ev.Source.Provider != "schedule" || ev.Source.Kind != "schedule" || ev.Source.Key() != "" {
		t.Fatalf("bad source: %+v", ev.Source)
	}
	if !Match(ev, store.autos[0]) {
		t.Fatal("fired event does not match its automation")
	}
	if s.Schedules()[0].Next.Before(time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("next not advanced: %v", s.Schedules()[0].Next)
	}

	// Simulated restart within the same minute: the persisted last-fired
	// time prevents a second fire for the same slot.
	s2 := NewScheduler(store, bus, SchedulerOptions{Now: func() time.Time { return fireAt.Add(-time.Second) }, DisableAutoReload: true})
	if err := s2.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s2.Schedules()[0].Next; !got.Equal(time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("restarted next = %v", got)
	}
	// Even if a stale entry is due for the already-fired slot, fire() skips it.
	s2.mu.Lock()
	for _, e := range s2.entries {
		e.next = time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	}
	s2.mu.Unlock()
	s2.tick(ctx, fireAt.Add(10*time.Second))
	if n := len(bus.published()); n != 1 {
		t.Fatalf("double fire after restart: %d events", n)
	}
}

func TestSchedulerReloadSemantics(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	bus := &fakeBus{}
	now := time.Date(2026, 3, 1, 10, 0, 30, 0, time.UTC)
	s := NewScheduler(store, bus, SchedulerOptions{Now: func() time.Time { return now }, DisableAutoReload: true})

	store.autos = []core.Automation{schedAuto("aut_1", "a", "0 12 * * *")}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	key := scheduleKey("aut_1", "a")
	first := s.entries[key]

	// Same cron: entry kept as is.
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if s.entries[key] != first {
		t.Fatal("unchanged schedule was replaced")
	}

	// Changing only the cron updates the same key.
	store.autos = []core.Automation{schedAuto("aut_1", "a", "30 10 * * *")}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.entries) != 1 || s.entries[key] == nil || s.entries[key].spec != "30 10 * * *" {
		t.Fatalf("cron update failed: %+v", s.Schedules())
	}
	if want := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC); !s.entries[key].next.Equal(want) {
		t.Fatalf("next = %v, want %v", s.entries[key].next, want)
	}

	// Changing the name replaces it.
	store.autos = []core.Automation{schedAuto("aut_1", "b", "30 10 * * *")}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.entries[key]; ok || s.entries[scheduleKey("aut_1", "b")] == nil || len(s.entries) != 1 {
		t.Fatalf("rename did not replace: %+v", s.Schedules())
	}

	// Disabled automations and invalid crons are dropped.
	dis := schedAuto("aut_1", "b", "30 10 * * *")
	dis.Enabled = false
	store.autos = []core.Automation{dis, schedAuto("aut_2", "", "bogus"), schedAuto("aut_3", "", "@every 1h")}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	infos := s.Schedules()
	if len(infos) != 1 || infos[0].AutomationID != "aut_3" || !infos[0].Next.Equal(now.Add(time.Hour)) {
		t.Fatalf("schedules = %+v", infos)
	}
}

func TestSchedulerStartLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newFakeStore()
	store.autos = []core.Automation{schedAuto("aut_1", "", "* * * * *")}
	bus := &fakeBus{}
	// Pretend "now" is just before a minute boundary so the real-time loop
	// fires after ~50ms.
	base := time.Date(2026, 3, 1, 10, 0, 59, 950_000_000, time.UTC)
	start := time.Now()
	clock := func() time.Time { return base.Add(time.Since(start)) }
	s := NewScheduler(store, bus, SchedulerOptions{Now: clock})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("second Start = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(bus.published()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(bus.published()) != 1 {
		t.Fatalf("loop published %d events", len(bus.published()))
	}

	// Definition applied -> automatic reload picks up a new schedule.
	store.mu.Lock()
	store.autos = append(store.autos, schedAuto("aut_2", "x", "@daily"))
	store.mu.Unlock()
	bus.Publish(ctx, core.TopicDefinition, &core.Factory{ID: "fac_1"})
	deadline = time.Now().Add(3 * time.Second)
	for len(s.Schedules()) != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(s.Schedules()) != 2 {
		t.Fatalf("auto reload: %+v", s.Schedules())
	}
}

type flagLeader struct{ v atomic.Bool }

func (l *flagLeader) IsLeader() bool { return l.v.Load() }

// Two replicas share one store: only the leader fires, a new leader fires a
// slot the dead leader missed (within the catch-up window), and a slot never
// fires twice.
func TestSchedulerFiresOnlyOnLeader(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.autos = []core.Automation{schedAuto("aut_1", "hourly", "0 * * * *")}
	bus := &fakeBus{}
	start := time.Date(2026, 3, 1, 1, 59, 0, 0, time.UTC)
	la, lb := &flagLeader{}, &flagLeader{}
	la.v.Store(true)
	a := NewScheduler(store, bus, SchedulerOptions{Now: func() time.Time { return start }, DisableAutoReload: true, Leader: la})
	b := NewScheduler(store, bus, SchedulerOptions{Now: func() time.Time { return start }, DisableAutoReload: true, Leader: lb})
	for _, s := range []*Scheduler{a, b} {
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	slot1 := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	b.tick(ctx, slot1)
	a.tick(ctx, slot1)
	if evs := bus.published(); len(evs) != 1 {
		t.Fatalf("published %d events, want 1 (leader only)", len(evs))
	}
	if got, want := bus.published()[0].ID, ScheduleEventID(scheduleKey("aut_1", "hourly"), slot1); got != want {
		t.Fatalf("event id = %q, want deterministic %q", got, want)
	}
	// b kept slot1 pending; becoming leader it must not fire it again.
	lb.v.Store(true)
	b.tick(ctx, slot1.Add(time.Second))
	if n := len(bus.published()); n != 1 {
		t.Fatalf("slot fired twice: %d events", n)
	}
	// Leader a dies before slot2; b (non-leader at slot2) takes over 30s later.
	la.v.Store(false)
	lb.v.Store(false)
	slot2 := slot1.Add(time.Hour)
	b.tick(ctx, slot2)
	if n := len(bus.published()); n != 1 {
		t.Fatalf("non-leader fired: %d events", n)
	}
	lb.v.Store(true)
	b.tick(ctx, slot2.Add(30*time.Second))
	evs := bus.published()
	if len(evs) != 2 || evs[1].Attributes[AttrScheduledAt] != slot2.Format(time.RFC3339) {
		t.Fatalf("missed slot not caught up: %+v", evs)
	}
	// Slots older than the catch-up window are skipped by non-leaders.
	lb.v.Store(false)
	slot3 := slot2.Add(time.Hour)
	b.tick(ctx, slot3.Add(10*time.Minute))
	lb.v.Store(true)
	b.tick(ctx, slot3.Add(11*time.Minute))
	if n := len(bus.published()); n != 2 {
		t.Fatalf("stale slot fired: %d events", n)
	}
}

// A replica that is not the leader must follow the clock. With an interval
// shorter than the catch-up window, the old leader fires several slots while
// the follower waits. When the leader dies, the follower must fire the slot
// that fell due during the gap, not skip it because it still held an old slot
// that the dead leader had already fired.
func TestSchedulerFollowerCatchesUpAfterShortIntervals(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.autos = []core.Automation{schedAuto("aut_1", "tick", "* * * * *")}
	bus := &fakeBus{}
	start := time.Date(2026, 3, 1, 1, 59, 30, 0, time.UTC)
	la, lb := &flagLeader{}, &flagLeader{}
	la.v.Store(true)
	a := NewScheduler(store, bus, SchedulerOptions{Now: func() time.Time { return start }, DisableAutoReload: true, Leader: la})
	b := NewScheduler(store, bus, SchedulerOptions{Now: func() time.Time { return start }, DisableAutoReload: true, Leader: lb})
	for _, s := range []*Scheduler{a, b} {
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	slot := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ { // slots 02:00 .. 02:03: the leader fires, the follower waits
		now := slot.Add(time.Duration(i) * time.Minute)
		b.tick(ctx, now)
		a.tick(ctx, now)
	}
	if n := len(bus.published()); n != 4 {
		t.Fatalf("published %d events, want 4", n)
	}
	for _, s := range b.Schedules() {
		if want := slot.Add(3 * time.Minute); !s.Next.Equal(want) {
			t.Fatalf("follower next = %v, want the latest due slot %v", s.Next, want)
		}
	}
	// The leader dies. Slot 02:04 falls due during the gap, and the follower
	// takes over six seconds later.
	la.v.Store(false)
	gap := slot.Add(4 * time.Minute)
	b.tick(ctx, gap)
	lb.v.Store(true)
	b.tick(ctx, gap.Add(6*time.Second))
	evs := bus.published()
	if len(evs) != 5 || evs[4].Attributes[AttrScheduledAt] != gap.Format(time.RFC3339) {
		t.Fatalf("slot of the gap not fired: %d events", len(evs))
	}
	// The next slot fires on time, once.
	b.tick(ctx, gap.Add(time.Minute))
	if n := len(bus.published()); n != 6 {
		t.Fatalf("published %d events, want 6", n)
	}
}
