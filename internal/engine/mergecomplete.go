package engine

import (
	"context"
	"encoding/json"

	"github.com/openfactory-org/openfactory/internal/core"
)

// pendingMergeCompletion is the fallback completion of a work item after its
// last open pull request merged. The foreman gets one turn to complete
// the work item. When that turn ends and the work item is still open, the
// engine completes it.
type pendingMergeCompletion struct {
	RunID string `json:"run_id"`
	Actor string `json:"actor"`
	Note  string `json:"note"`
	// Turns is the turn counter of the root run that the merge turn must
	// pass.
	Turns int64 `json:"turns"`
	// WasDone is true when the root run was finished at the merge. A
	// finished state then does not end the merge turn; the counter does.
	WasDone bool `json:"was_done,omitempty"`
}

func mergeCompletionKey(workItemID string) string {
	return "engine:mergecomplete:" + workItemID
}

// mergeTurnRoot returns the root run that gets the merge turn, or nil when
// the work item has no root run that can take a message.
func (e *Engine) mergeTurnRoot(ctx context.Context, wi *core.WorkItem) *core.Run {
	if wi.RootRunID == "" {
		return nil
	}
	r, err := e.store.GetRun(ctx, wi.RootRunID)
	if err != nil || r.State == core.RunCancelled {
		return nil
	}
	return r
}

// armMergeCompletion records the fallback completion for the turn that the
// merge message starts.
func (e *Engine) armMergeCompletion(ctx context.Context, workItemID string, root *core.Run, actor, note string) error {
	p := pendingMergeCompletion{RunID: root.ID, Actor: actor, Note: note, Turns: root.Turns, WasDone: root.State.Terminal()}
	if !root.State.WaitsForInput() && !root.State.Terminal() {
		// The current turn ends first; its end must not count.
		p.Turns++
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return e.store.KVSet(ctx, mergeCompletionKey(workItemID), string(b))
}

// checkMergeCompletion completes the work item of a root run when the run
// passed the merge turn and the foreman did not complete the work item.
func (e *Engine) checkMergeCompletion(ctx context.Context, r *core.Run) {
	if r.ParentRunID != "" || r.WorkItemID == "" {
		return
	}
	key := mergeCompletionKey(r.WorkItemID)
	raw, err := e.store.KVGet(ctx, key)
	if err != nil || raw == "" {
		return
	}
	var p pendingMergeCompletion
	if json.Unmarshal([]byte(raw), &p) != nil || p.RunID != r.ID {
		return
	}
	ended := r.Turns > p.Turns || (r.State.Terminal() && !p.WasDone)
	if !ended {
		return // the merge turn has not ended yet
	}
	defer e.lock("mergecomplete:" + r.WorkItemID)()
	if cur, err := e.store.KVGet(ctx, key); err != nil || cur != raw {
		return // another replica handled it
	}
	if err := e.store.KVSet(ctx, key, ""); err != nil {
		e.log.Warn("clear merge completion", "work_item_id", r.WorkItemID, "err", err)
		return
	}
	wi, err := e.store.GetWorkItem(ctx, r.WorkItemID)
	if err != nil || wi.Stage.Terminal() {
		return
	}
	note := p.Note + " The foreman did not complete the work item in its turn after the merge, so the platform completed it."
	if err := e.CompleteWorkItem(ctx, wi.ID, p.Actor, note); err != nil {
		e.log.Warn("complete work item after merge", "work_item_id", wi.ID, "err", err)
	}
}
