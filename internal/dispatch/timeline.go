package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runstate"
)

// timeline writes one control-plane event to the timeline of a run.
// The event ID must be deterministic, so
// that a retried write does not add a second row. An error is logged and
// never returned: a lost timeline row must not fail a run.
func (d *Dispatcher) timeline(ctx context.Context, runID string, session int, typ, eventID string, data any) {
	d.timelineAt(ctx, runID, session, typ, eventID, d.now(), data)
}

// timelineAt is timeline with the time of the event chosen by the caller.
func (d *Dispatcher) timelineAt(ctx context.Context, runID string, session int, typ, eventID string, at time.Time, data any) {
	ev := &core.TimelineEvent{EventID: eventID, RunID: runID, Session: session, Type: typ, At: at.UTC()}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			d.log.Warn("dispatch: encode timeline data", "run", runID, "type", typ, "err", err)
			return
		}
		ev.Data = b
	}
	if _, err := d.cfg.Runner.AppendTimelineEvent(ctx, ev); err != nil {
		d.log.Warn("dispatch: write timeline", "run", runID, "type", typ, "err", err)
	}
}

// latestSessionNumber returns the highest session number of the run, or 0.
func (d *Dispatcher) latestSessionNumber(ctx context.Context, runID string) int {
	s, err := d.cfg.Runner.LatestRunSession(ctx, runID)
	if err != nil {
		return 0
	}
	return s.Session
}

func nanos(t *time.Time, fallback time.Time) int64 {
	if t != nil {
		return t.UnixNano()
	}
	return fallback.UnixNano()
}

// queuedEventID is the ID of the run.queued event for the queue entry of r.
func queuedEventID(r *core.Run) string {
	return fmt.Sprintf("run.queued:%s:%d", r.ID, nanos(r.QueuedAt, r.CreatedAt))
}

// timelineTransition writes the events of a run state change.
func (d *Dispatcher) timelineTransition(ctx context.Context, r *core.Run, in runstate.Input, res runstate.Result) {
	session := d.latestSessionNumber(ctx, r.ID)
	switch r.State {
	case core.RunQueued:
		reason := "new"
		switch {
		case in.Event == runstate.EventSessionLost || res.Retry:
			reason = "retry"
			code := in.FailureCode
			if res.FailureCode != "" {
				code = res.FailureCode
			}
			d.timeline(ctx, r.ID, session, "run.retry_scheduled",
				fmt.Sprintf("run.retry_scheduled:%s:%d", core.TaskID(r.ID, session), nanos(r.QueuedAt, r.CreatedAt)),
				map[string]any{"failure_code": code, "attempt": in.PriorRetries + 1})
		case in.Event == runstate.EventMessage:
			reason = "message"
		}
		at := d.now()
		if r.QueuedAt != nil {
			at = *r.QueuedAt
		}
		d.timelineAt(ctx, r.ID, session, "run.queued", queuedEventID(r), at, map[string]any{"reason": reason})
	case core.RunBlocked:
		d.timeline(ctx, r.ID, session, "run.blocked",
			fmt.Sprintf("run.blocked:%s:%d:%s", r.ID, r.Turns, r.BlockedReason),
			map[string]any{"blocked_reason": r.BlockedReason})
	case core.RunSucceeded, core.RunFailed, core.RunError, core.RunCancelled:
		if r.State == core.RunCancelled && in.Event == runstate.EventCancel {
			// The engine cancels runs too and does not write the timeline.
			// The timeline route builds run.cancelled from the run row.
			return
		}
		at := d.now()
		if r.FinishedAt != nil {
			at = *r.FinishedAt
		}
		var data any
		if r.FailureCode != "" {
			data = map[string]any{"failure_code": r.FailureCode}
		}
		d.timelineAt(ctx, r.ID, session, "run."+string(r.State),
			fmt.Sprintf("run.%s:%s:%d:%d", r.State, r.ID, r.Turns, nanos(r.FinishedAt, at)), at, data)
	}
}

// timelineSessionEnded writes session.ended.
func (d *Dispatcher) timelineSessionEnded(ctx context.Context, runID string, session int, reason string, sandbox core.SandboxReason) {
	data := map[string]any{"reason": reason}
	if sandbox != "" {
		data["sandbox_reason"] = sandbox
	}
	d.timeline(ctx, runID, session, "session.ended", fmt.Sprintf("session.ended:%s", core.TaskID(runID, session)), data)
}
