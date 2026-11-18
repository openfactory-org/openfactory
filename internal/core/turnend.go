package core

import (
	"context"
	"strconv"
)

// TopicRunTurnEnded is the durable queue topic of turn ends (payload:
// RunTurnEnded). A writer that moves a run to a state that ends a turn
// (RunState.WaitsForInput or RunState.Terminal) enqueues a job in the same
// transaction (EnqueueTurnEnded). The engine consumes it and reports the turn
// end: the lifecycle message to the parent run, the timeline entry and the
// failure inbox item. So a crash of the replica after the state change, or a
// failed send, does not lose the report. The live topic TopicRunUpdated is
// lossy and only makes the report faster.
const TopicRunTurnEnded = "run.turn_ended"

// RunTurnEnded is the payload of a TopicRunTurnEnded job.
type RunTurnEnded struct {
	RunID string `json:"run_id"`
}

// OutboxPartitionKey keeps the turn ends of one run in order.
func (m RunTurnEnded) OutboxPartitionKey() string { return "run:" + m.RunID }

// TurnMarker identifies one turn end of r. An idle run that later expires to
// succeeded keeps the same ConsumedSeq and Turns, so its turn end is reported
// once. The cancel of a run that waits is reported once more.
func TurnMarker(r *Run) string {
	m := strconv.FormatInt(r.ConsumedSeq, 10) + ":" + strconv.FormatInt(r.Turns, 10)
	if r.State == RunCancelled {
		m += ":cancelled"
	}
	return m
}

// EnqueueTurnEnded enqueues the turn-end job of r on q. Call it in the
// transaction that writes the new state of r. It does nothing when q is nil or
// when the state of r does not end a turn. The job ID contains the turn
// marker, so a second write of the same turn end enqueues no second job.
func EnqueueTurnEnded(ctx context.Context, q Queue, r *Run) error {
	if q == nil || r == nil || (!r.State.WaitsForInput() && !r.State.Terminal()) {
		return nil
	}
	return q.Enqueue(ctx, TopicRunTurnEnded, "turn:"+r.ID+":"+TurnMarker(r), RunTurnEnded{RunID: r.ID})
}
