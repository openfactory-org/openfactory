package triggers

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Schedule controls outside the definition. The pause and the
// last error of a schedule live in the store's KV, not in the definition
// files, so an operator can pause a schedule of a git-backed factory without
// a commit. Every replica reads the same rows, so the list and the pause do
// not depend on the replica that runs the scheduler.

const (
	// DefaultLastFiredKVPrefix is the default SchedulerOptions.KVPrefix.
	DefaultLastFiredKVPrefix = "triggers/schedule/last_fired/"
	// PausedKVPrefix prefixes the KV keys that pause a schedule. The value
	// is the pause time (RFC 3339) and the actor; "" means not paused.
	PausedKVPrefix = "triggers/schedule/paused/"
	// ErrorKVPrefix prefixes the KV keys that hold the last error of a
	// schedule; "" means none.
	ErrorKVPrefix = "triggers/schedule/last_error/"
)

// kvStore is the part of core.Store that holds the schedule state.
type kvStore interface {
	KVGet(ctx context.Context, key string) (string, error)
	KVSet(ctx context.Context, key, value string) error
}

// ScheduleKey is the key of one schedule: automation ID and schedule name.
func ScheduleKey(automationID, name string) string { return scheduleKey(automationID, name) }

// SchedulePaused reports whether the schedule is paused.
func SchedulePaused(ctx context.Context, store kvStore, automationID, name string) (bool, error) {
	v, err := store.KVGet(ctx, PausedKVPrefix+scheduleKey(automationID, name))
	if errors.Is(err, core.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v != "", nil
}

// SetSchedulePaused pauses or resumes a schedule. by names the actor.
func SetSchedulePaused(ctx context.Context, store kvStore, automationID, name string, paused bool, by string, now time.Time) error {
	v := ""
	if paused {
		v = now.UTC().Format(time.RFC3339) + " " + by
	}
	return store.KVSet(ctx, PausedKVPrefix+scheduleKey(automationID, name), v)
}

// activeRunStates are the states of a run that is not finished.
var activeRunStates = []core.RunState{
	core.RunQueued, core.RunPending, core.RunStarting, core.RunClaimed,
	core.RunRunning, core.RunBlocked, core.RunIdle,
}

// activeAutomationRun returns the ID of a run started by the automation that
// is not finished, or "".
func activeAutomationRun(ctx context.Context, store core.Store, factoryID, automationID string) (string, error) {
	runs, err := store.ListRuns(ctx, core.RunFilter{FactoryID: factoryID, States: activeRunStates})
	if err != nil {
		return "", err
	}
	for _, r := range runs {
		if core.StartedByAutomation(r.TriggeredBy, automationID) {
			return r.ID, nil
		}
	}
	return "", nil
}

// ScheduleStatus describes one schedule trigger of a factory
// (GET /factories/{uid}/schedules).
type ScheduleStatus struct {
	AutomationID   string `json:"automation_id"`
	AutomationName string `json:"automation_name"`
	// ID is the schedule ID used by the pause and resume calls: the
	// automation name, plus ":" and the schedule name when it has one.
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	Concurrency string     `json:"concurrency"`
	Enabled     bool       `json:"enabled"`
	Paused      bool       `json:"paused"`
	PausedAt    *time.Time `json:"paused_at,omitempty"`
	PausedBy    string     `json:"paused_by,omitempty"`
	LastRunAt   *time.Time `json:"last_run_at"`
	// NextRunAt is the next slot; it is null when the automation is
	// disabled, the schedule is paused or the cron expression is invalid.
	NextRunAt *time.Time `json:"next_run_at"`
	LastError string     `json:"last_error,omitempty"`
}

// ScheduleID returns the ID of a schedule in the API: the automation name,
// plus ":" and the schedule name when it has one.
func ScheduleID(automationName, scheduleName string) string {
	if scheduleName == "" {
		return automationName
	}
	return automationName + ":" + scheduleName
}

// ListSchedules returns the schedule triggers of the factory's automations,
// sorted by schedule ID, with their state at now.
func ListSchedules(ctx context.Context, store core.Store, factoryID string, now time.Time) ([]ScheduleStatus, error) {
	autos, err := store.ListAutomations(ctx, factoryID)
	if err != nil {
		return nil, fmt.Errorf("list automations: %w", err)
	}
	out := []ScheduleStatus{}
	for _, a := range autos {
		for _, t := range a.Triggers {
			if canonicalProvider(t.Provider) != ProviderSchedule || t.Event != EventCronFired || t.Schedule == nil {
				continue
			}
			st, err := scheduleStatus(ctx, store, a, t, now)
			if err != nil {
				return nil, err
			}
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// FindSchedule returns the automation and the schedule trigger with the API
// schedule ID id (see ScheduleID), or core.ErrNotFound.
func FindSchedule(ctx context.Context, store core.Store, factoryID, id string) (*core.Automation, *core.Trigger, error) {
	autos, err := store.ListAutomations(ctx, factoryID)
	if err != nil {
		return nil, nil, fmt.Errorf("list automations: %w", err)
	}
	for i := range autos {
		a := &autos[i]
		if a.Name != id && !strings.HasPrefix(id, a.Name+":") {
			continue
		}
		for j := range a.Triggers {
			t := &a.Triggers[j]
			if canonicalProvider(t.Provider) == ProviderSchedule && t.Schedule != nil && ScheduleID(a.Name, t.Schedule.Name) == id {
				return a, t, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("%w: schedule %q", core.ErrNotFound, id)
}

func scheduleStatus(ctx context.Context, store core.Store, a core.Automation, t core.Trigger, now time.Time) (ScheduleStatus, error) {
	key := scheduleKey(a.ID, t.Schedule.Name)
	st := ScheduleStatus{
		AutomationID: a.ID, AutomationName: a.Name,
		ID: ScheduleID(a.Name, t.Schedule.Name), Name: t.Schedule.Name, Cron: t.Schedule.Cron,
		Concurrency: t.Concurrency, Enabled: a.Enabled,
	}
	if st.Concurrency == "" {
		st.Concurrency = core.ScheduleConcurrencyAllow
	}
	get := func(k string) (string, error) {
		v, err := store.KVGet(ctx, k)
		if errors.Is(err, core.ErrNotFound) {
			return "", nil
		}
		return v, err
	}
	last, err := get(DefaultLastFiredKVPrefix + key)
	if err != nil {
		return st, err
	}
	base := now.UTC()
	if v, err := time.Parse(time.RFC3339Nano, last); err == nil {
		st.LastRunAt = &v
		if v.After(base) {
			base = v
		}
	}
	paused, err := get(PausedKVPrefix + key)
	if err != nil {
		return st, err
	}
	if paused != "" {
		st.Paused = true
		at, by, _ := strings.Cut(paused, " ")
		if v, err := time.Parse(time.RFC3339, at); err == nil {
			st.PausedAt = &v
		}
		st.PausedBy = by
	}
	if st.LastError, err = get(ErrorKVPrefix + key); err != nil {
		return st, err
	}
	sched, err := parseCron(t.Schedule.Cron)
	if err != nil {
		st.LastError = "invalid cron: " + err.Error()
		return st, nil //nolint:nilerr // an invalid cron is reported in the status (LastError)
	}
	if a.Enabled && !st.Paused {
		next := sched.Next(base)
		st.NextRunAt = &next
	}
	return st, nil
}

// ManualEvent returns the event of a manual start of automation a by actor
// (POST /automations/{id}/run). It is addressed to the automation only. The
// caller dispatches the automation itself; the event is the run's context.
func ManualEvent(a core.Automation, teamID string, actor core.Actor, now time.Time) *core.Event {
	id := core.NewID("evt")
	return &core.Event{
		ID:         id,
		TeamID:     teamID,
		FactoryID:  a.FactoryID,
		Provider:   "factory",
		Type:       "manual_run",
		DeliveryID: "manual:" + a.ID + "@" + strconv.FormatInt(now.UnixNano(), 10),
		Actor:      actor,
		Source:     core.SourceRef{Provider: "factory", Kind: "manual", Title: a.Name, ExternalID: id},
		Attributes: map[string]any{AttrAutomationID: a.ID},
		ReceivedAt: now.UTC(),
	}
}
