package triggers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

// SchedulerOptions configures a Scheduler.
type SchedulerOptions struct {
	Logger *slog.Logger
	// Now returns the current time (default time.Now). Times are used in UTC.
	Now func() time.Time
	// KVPrefix prefixes the KV keys holding last-fired times (default
	// DefaultLastFiredKVPrefix; ListSchedules reads the default prefix).
	KVPrefix string
	// DisableAutoReload stops the scheduler from reloading when a definition
	// is applied (core.TopicDefinition).
	DisableAutoReload bool
	// Leader gates firing to the replica holding the leader lease.
	// Nil means always leader (single process).
	Leader core.Leadership
	// CatchUpWindow is how late a slot may still fire, e.g. right after this
	// replica took over leadership from a dead leader (default 5m). Older
	// slots are skipped.
	CatchUpWindow time.Duration
}

// Scheduler fires schedule/cron_fired triggers of enabled automations and
// publishes core.Event values on core.TopicInboundEvent.
//
// Schedules are keyed by automation ID + schedule name: changing only the cron
// expression updates the schedule in place (its last-fired state is kept),
// while changing the name replaces it. The last fired time of each schedule is
// persisted in the store's KV so a restart never fires the same slot twice.
type Scheduler struct {
	store  core.Store
	bus    core.EventBus
	log    *slog.Logger
	now    func() time.Time
	prefix string
	auto   bool
	leader core.Leadership
	window time.Duration

	mu      sync.Mutex
	entries map[string]*schedEntry
	wake    chan struct{}
	started bool
}

type schedEntry struct {
	key            string
	automationID   string
	automationName string
	factoryID      string
	teamID         string
	name           string
	spec           string
	concurrency    string
	sched          cron.Schedule
	next           time.Time
}

// ScheduleInfo describes one active schedule.
type ScheduleInfo struct {
	AutomationID string    `json:"automation_id"`
	FactoryID    string    `json:"factory_id"`
	Name         string    `json:"name"`
	Cron         string    `json:"cron"`
	Next         time.Time `json:"next"`
}

// NewScheduler returns a scheduler; call Start to run it.
func NewScheduler(store core.Store, bus core.EventBus, opts SchedulerOptions) *Scheduler {
	s := &Scheduler{
		store:   store,
		bus:     bus,
		log:     opts.Logger,
		now:     opts.Now,
		prefix:  opts.KVPrefix,
		auto:    !opts.DisableAutoReload,
		leader:  opts.Leader,
		window:  opts.CatchUpWindow,
		entries: map[string]*schedEntry{},
		wake:    make(chan struct{}, 1),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.prefix == "" {
		s.prefix = DefaultLastFiredKVPrefix
	}
	if s.window <= 0 {
		s.window = 5 * time.Minute
	}
	return s
}

// Start loads schedules and runs the scheduling loop until ctx is cancelled.
// It returns after the initial load; a load error is returned but the loop
// still starts so a later Reload can recover.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("scheduler already started")
	}
	s.started = true
	s.mu.Unlock()

	err := s.Reload(ctx)
	if s.auto && s.bus != nil {
		ch, unsub := s.bus.Subscribe(core.TopicDefinition)
		go func() {
			defer unsub()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-ch:
					if !ok {
						return
					}
					if err := s.Reload(ctx); err != nil {
						s.log.Error("schedule reload failed", "err", err)
					}
				}
			}
		}()
	}
	go s.loop(ctx)
	return err
}

func (s *Scheduler) loop(ctx context.Context) {
	for {
		wait := time.Hour
		if next, ok := s.earliest(); ok {
			wait = next.Sub(s.now())
			if wait < 0 {
				wait = 0
			}
		}
		if !s.isLeader() && wait < time.Second {
			// Due slots wait for leadership; re-check it periodically.
			wait = time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
			s.tick(ctx, s.now())
		}
	}
}

func (s *Scheduler) earliest() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best time.Time
	for _, e := range s.entries {
		if best.IsZero() || e.next.Before(best) {
			best = e.next
		}
	}
	return best, !best.IsZero()
}

func scheduleKey(automationID, name string) string { return automationID + ":" + name }

// Reload re-reads every enabled automation with schedule triggers and
// reconciles the active schedules.
func (s *Scheduler) Reload(ctx context.Context) error {
	autos, err := s.store.ListAllEnabledAutomations(ctx)
	if err != nil {
		return fmt.Errorf("list automations: %w", err)
	}
	teams := map[string]string{}
	teamOf := func(factoryID string) string {
		if t, ok := teams[factoryID]; ok {
			return t
		}
		f, err := s.store.GetFactory(ctx, factoryID)
		if err != nil {
			s.log.Warn("schedule: factory lookup failed", "factory_id", factoryID, "err", err)
			teams[factoryID] = ""
			return ""
		}
		teams[factoryID] = f.TeamID
		return f.TeamID
	}

	type desired struct {
		a           core.Automation
		name        string
		spec        string
		concurrency string
	}
	want := map[string]desired{}
	for _, a := range autos {
		if !a.Enabled {
			continue
		}
		for _, t := range a.Triggers {
			if canonicalProvider(t.Provider) != ProviderSchedule || t.Event != EventCronFired || t.Schedule == nil {
				continue
			}
			want[scheduleKey(a.ID, t.Schedule.Name)] = desired{a: a, name: t.Schedule.Name, spec: t.Schedule.Cron, concurrency: t.Concurrency}
		}
	}

	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.entries {
		if _, ok := want[key]; !ok {
			delete(s.entries, key)
		}
	}
	for key, d := range want {
		e, exists := s.entries[key]
		if exists && e.spec == d.spec {
			e.automationName, e.factoryID, e.concurrency = d.a.Name, d.a.FactoryID, d.concurrency
			e.teamID = teamOf(d.a.FactoryID)
			continue
		}
		sched, err := parseCron(d.spec)
		if err != nil {
			s.log.Warn("schedule: invalid cron; skipping", "automation_id", d.a.ID, "schedule", d.name, "err", err)
			delete(s.entries, key)
			continue
		}
		base := now
		if last, ok := s.lastFired(ctx, key); ok && last.After(base) {
			base = last
		}
		s.entries[key] = &schedEntry{
			key:            key,
			automationID:   d.a.ID,
			automationName: d.a.Name,
			factoryID:      d.a.FactoryID,
			teamID:         teamOf(d.a.FactoryID),
			name:           d.name,
			spec:           d.spec,
			concurrency:    d.concurrency,
			sched:          sched,
			next:           sched.Next(base),
		}
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return nil
}

// Schedules lists active schedules sorted by next fire time.
func (s *Scheduler) Schedules() []ScheduleInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ScheduleInfo, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, ScheduleInfo{AutomationID: e.automationID, FactoryID: e.factoryID, Name: e.name, Cron: e.spec, Next: e.next})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Next.Equal(out[j].Next) {
			return out[i].Next.Before(out[j].Next)
		}
		return scheduleKey(out[i].AutomationID, out[i].Name) < scheduleKey(out[j].AutomationID, out[j].Name)
	})
	return out
}

func (s *Scheduler) isLeader() bool { return s.leader == nil || s.leader.IsLeader() }

// tick fires every schedule due at or before now. Missed slots are not
// replayed: each due schedule fires once and advances past now. A replica
// that is not the leader fires nothing; it keeps due slots pending for up to
// CatchUpWindow so a newly elected leader still fires a slot the previous
// leader missed (the last-fired KV and the deterministic event ID make a
// slot fire at most once).
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	now = now.UTC()
	type due struct {
		e         schedEntry
		scheduled time.Time
	}
	leader := s.isLeader()
	var fire []due
	s.mu.Lock()
	for _, e := range s.entries {
		if e.next.IsZero() || e.next.After(now) {
			continue
		}
		if !leader {
			e.next = e.pendingSlot(now, s.window)
			continue
		}
		fire = append(fire, due{e: *e, scheduled: e.next})
		e.next = e.sched.Next(now)
	}
	s.mu.Unlock()
	sort.Slice(fire, func(i, j int) bool { return fire[i].e.key < fire[j].e.key })
	for _, d := range fire {
		s.fire(ctx, d.e, d.scheduled, now)
	}
}

// pendingSlot is the slot a replica that is not the leader keeps pending: the
// latest slot at or before now that is not older than window, or the first
// slot after now when there is none. The leader fires slots as they fall due,
// so a follower must follow the clock. If it stayed on its oldest due slot, a
// slot of a short interval would be lost when it took over: it would see an
// old slot that the dead leader had already fired, and skip to the next one.
func (e *schedEntry) pendingSlot(now time.Time, window time.Duration) time.Time {
	latest := e.next
	if now.Sub(latest) > window {
		latest = e.sched.Next(now.Add(-window))
		if latest.After(now) {
			return latest
		}
	}
	for {
		n := e.sched.Next(latest)
		if n.After(now) {
			return latest
		}
		latest = n
	}
}

// fire starts one slot. A paused schedule fires nothing. With concurrency
// skip, the slot is dropped while a run of the automation is not finished.
// Otherwise the last-fired time and the event are written in one store
// transaction: the event goes to the outbox in that transaction (when the bus
// is the durable bus on this store), so a stop between the two writes can no
// longer lose the slot.
func (s *Scheduler) fire(ctx context.Context, e schedEntry, scheduled, now time.Time) {
	log := s.log.With("automation_id", e.automationID, "schedule", e.name, "slot", scheduled)
	if paused, err := SchedulePaused(ctx, s.store, e.automationID, e.name); err != nil {
		log.Warn("schedule: reading the pause failed", "err", err)
	} else if paused {
		log.Info("schedule paused; slot not fired")
		return
	}
	if e.concurrency == core.ScheduleConcurrencySkip {
		active, err := activeAutomationRun(ctx, s.store, e.factoryID, e.automationID)
		if err != nil {
			s.recordError(ctx, e.key, "check for an active run: "+err.Error())
			log.Warn("schedule: checking for an active run failed; slot not fired", "err", err)
			return
		}
		if active != "" {
			log.Info("schedule skipped: the last run is not finished", "run_id", active)
			if err := s.store.KVSet(ctx, s.prefix+e.key, scheduled.UTC().Format(time.RFC3339Nano)); err != nil {
				log.Warn("schedule: persisting last fired time failed", "err", err)
			}
			return
		}
	}
	ev := &core.Event{
		ID:         ScheduleEventID(e.key, scheduled),
		TeamID:     e.teamID,
		FactoryID:  e.factoryID,
		Provider:   ProviderSchedule,
		Type:       EventCronFired,
		DeliveryID: fmt.Sprintf("%s@%d", e.key, scheduled.Unix()),
		Actor:      core.Actor{Provider: ProviderSchedule, Handle: "schedule"},
		Source:     core.SourceRef{Provider: ProviderSchedule, Kind: "schedule", Title: e.automationName},
		Attributes: map[string]any{
			AttrAutomationID: e.automationID,
			AttrScheduleName: e.name,
			AttrScheduledAt:  scheduled.UTC().Format(time.RFC3339),
		},
		ReceivedAt: now,
	}
	already := false
	err := core.RunInTx(ctx, s.store, func(ctx context.Context) error {
		if last, ok := s.lastFired(ctx, e.key); ok && !scheduled.After(last) {
			already = true
			return nil
		}
		if err := s.store.KVSet(ctx, s.prefix+e.key, scheduled.UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("persist last fired time: %w", err)
		}
		s.bus.Publish(ctx, core.TopicInboundEvent, ev)
		return nil
	})
	switch {
	case err != nil:
		s.recordError(ctx, e.key, err.Error())
		log.Warn("schedule: firing the slot failed", "err", err)
	case already:
		log.Debug("schedule slot already fired")
	default:
		s.recordError(ctx, e.key, "")
		log.Info("schedule fired")
	}
}

// recordError stores the last error of a schedule ("" clears it).
func (s *Scheduler) recordError(ctx context.Context, key, msg string) {
	if old, _ := s.store.KVGet(ctx, ErrorKVPrefix+key); old == msg {
		return
	}
	if err := s.store.KVSet(ctx, ErrorKVPrefix+key, msg); err != nil {
		s.log.Debug("schedule: recording the last error failed", "key", key, "err", err)
	}
}

// ScheduleEventID is the deterministic event ID of one schedule slot, so the
// outbox deduplicates a slot fired twice (e.g. during a leader handover).
func ScheduleEventID(key string, slot time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s@%d", key, slot.UTC().Unix())))
	return "evt_sched_" + hex.EncodeToString(sum[:12])
}

func (s *Scheduler) lastFired(ctx context.Context, key string) (time.Time, bool) {
	v, err := s.store.KVGet(ctx, s.prefix+key)
	if err != nil || v == "" {
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			s.log.Warn("schedule: reading last fired time failed", "key", key, "err", err)
		}
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
