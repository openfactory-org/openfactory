package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// References of a work item, the tracker write-back and the reopen of a
// completed work item.

// ReopenWindow is how long after completion an event for a reference of a
// work item opens that work item again.
const ReopenWindow = 7 * 24 * time.Hour

var _ core.IssueLinker = (*Engine)(nil)

// completedRefKey is the key-value entry that maps a reference of a
// completed work item to the work item. A reopen reads it.
func completedRefKey(factoryID, key string) string {
	return "engine:completedref:" + factoryID + ":" + key
}

// replyThreadKey holds the source (with ThreadRef) of the last text action
// that a work item got from a sub-thread. Replies of the work item go into
// that thread until an action arrives from outside a thread.
func replyThreadKey(workItemID string) string { return "engine:replythread:" + workItemID }

// LinkIssue implements core.IssueLinker. The issue becomes a reference of the
// work item: its events route to the work item, the pull requests of the
// work item are attached to it, and its state follows the work item.
func (e *Engine) LinkIssue(ctx context.Context, workItemID string, issue core.SourceRef) error {
	issue.Provider = normalizeProvider(issue.Provider)
	if issue.Kind == "" {
		issue.Kind = "issue"
	}
	issue.ThreadRef = ""
	key := issue.Key()
	if issue.Provider == "" || key == "" {
		return fmt.Errorf("%w: an issue needs a provider and a key (tracker key, or repo and number)", core.ErrInvalid)
	}
	var wi *core.WorkItem
	added := false
	unlock := e.lock("wi:" + workItemID)
	err := e.atomicRetry(ctx, func(ctx context.Context) error {
		var err error
		if wi, err = e.store.GetWorkItem(ctx, workItemID); err != nil {
			return err
		}
		if wi.Stage.Terminal() {
			return fmt.Errorf("%w: work item is %s", core.ErrConflict, wi.Stage)
		}
		added = false
		if wi.HasRef(key) {
			return nil
		}
		wi.AddRef(core.WorkItemRef{Kind: core.RefIssue, Key: key, Source: issue, AddedAt: e.now()})
		wi.UpdatedAt = e.now()
		if err := e.store.UpdateWorkItem(ctx, wi); err != nil {
			return err
		}
		added = true
		if err := e.store.KVSet(ctx, sourceAliasKey(wi.FactoryID, key), wi.ID); err != nil {
			return err
		}
		e.timeline(ctx, wi.ID, "issue_linked", "", "system", map[string]any{"issue": issue, "key": key})
		// The write-back commits with the link (outbox).
		state := core.IssueStateInProgress
		for _, pr := range wi.PullRequests {
			e.trackerOp(ctx, wi, issue, core.OutboundAttachPullRequest, "", pr)
			if pr.State == "open" || pr.State == "draft" || pr.State == "" {
				state = core.IssueStateInReview
			}
		}
		e.trackerOp(ctx, wi, issue, core.OutboundIssueState, state, core.PullRequestRef{})
		e.publishWorkItem(ctx, wi)
		return nil
	})
	unlock()
	if err != nil {
		return err
	}
	if added {
		e.log.Info("linked issue to work item", "work_item_id", workItemID, "issue", key)
	}
	return nil
}

// trackerIssues returns the tracker issues of a work item: the linked issues,
// and the source when it is a tracker issue.
func trackerIssues(wi *core.WorkItem) []core.SourceRef {
	var out []core.SourceRef
	for _, r := range wi.Refs() {
		switch {
		case r.Kind == core.RefIssue:
			out = append(out, r.Source)
		case r.Kind == core.RefSource && r.Source.IsTrackerIssue():
			out = append(out, r.Source)
		}
	}
	return out
}

// trackerOp publishes one write-back to a tracker. It goes through the
// outbox with the other outbound messages, so it commits with the change
// that caused it.
func (e *Engine) trackerOp(ctx context.Context, wi *core.WorkItem, issue core.SourceRef, kind, state string, pr core.PullRequestRef) {
	msg := &core.OutboundMessage{
		WorkItemID: wi.ID,
		FactoryID:  wi.FactoryID,
		Source:     issue,
		Kind:       kind,
		IssueState: state,
	}
	if kind == core.OutboundAttachPullRequest {
		p := pr
		msg.PullRequest = &p
	}
	e.publish(ctx, core.TopicOutboundNotice, msg)
}

// writeBackPullRequest attaches a newly linked pull request to the tracker
// issues of a work item and moves them to in review.
func (e *Engine) writeBackPullRequest(ctx context.Context, wi *core.WorkItem, pr core.PullRequestRef) {
	for _, issue := range trackerIssues(wi) {
		e.trackerOp(ctx, wi, issue, core.OutboundAttachPullRequest, "", pr)
		if pr.State != "merged" && pr.State != "closed" {
			e.trackerOp(ctx, wi, issue, core.OutboundIssueState, core.IssueStateInReview, core.PullRequestRef{})
		}
	}
}

// writeBackComplete moves the tracker issues of a completed work item to the
// done state, and records its references for a reopen.
func (e *Engine) writeBackComplete(ctx context.Context, wi *core.WorkItem) {
	for _, issue := range trackerIssues(wi) {
		e.trackerOp(ctx, wi, issue, core.OutboundIssueState, core.IssueStateDone, core.PullRequestRef{})
	}
	for _, r := range wi.Refs() {
		if err := e.store.KVSet(ctx, completedRefKey(wi.FactoryID, r.Key), wi.ID); err != nil {
			e.log.Warn("record reference of completed work item", "work_item_id", wi.ID, "key", r.Key, "err", err)
		}
	}
}

// reopenable returns the completed work item of a source key when it
// completed less than ReopenWindow ago, else nil.
func (e *Engine) reopenable(ctx context.Context, factoryID, key string) *core.WorkItem {
	if key == "" {
		return nil
	}
	id, err := e.store.KVGet(ctx, completedRefKey(factoryID, key))
	if err != nil || id == "" {
		return nil
	}
	wi, err := e.store.GetWorkItem(ctx, id)
	if err != nil || wi.Stage != core.StageComplete || wi.CompletedAt == nil {
		return nil
	}
	if e.now().Sub(*wi.CompletedAt) > ReopenWindow {
		return nil
	}
	return wi
}

// reopenWorkItem opens a completed work item again. The stage goes to triage.
// It returns the reopened work item, or nil when another source key already
// belongs to an active work item or the item changed.
func (e *Engine) reopenWorkItem(ctx context.Context, wi *core.WorkItem, ev *core.Event) *core.WorkItem {
	var out *core.WorkItem
	unlock := e.lock("wi:" + wi.ID)
	err := e.atomicRetry(ctx, func(ctx context.Context) error {
		cur, err := e.store.GetWorkItem(ctx, wi.ID)
		if err != nil {
			return err
		}
		if cur.Stage != core.StageComplete {
			if !cur.Stage.Terminal() {
				out = cur
			}
			return nil
		}
		// One source key belongs to at most one active work item.
		if other, err := e.store.FindWorkItemBySource(ctx, cur.FactoryID, cur.SourceKey); err == nil && other != nil && other.ID != cur.ID && !other.Stage.Terminal() {
			return nil
		}
		from := cur.Stage
		completedAt := cur.CompletedAt
		cur.Stage = core.StageTriage
		cur.CompletedAt = nil
		cur.UpdatedAt = e.now()
		if err := e.store.UpdateWorkItem(ctx, cur); err != nil {
			return err
		}
		data := map[string]any{"from": from, "to": core.StageTriage, "reason": "event for a reference of the completed work item"}
		if ev != nil {
			data["provider"] = ev.Provider
			data["event"] = ev.Type
			data["key"] = ev.Source.Key()
		}
		if completedAt != nil {
			data["completed_at"] = completedAt
		}
		e.timeline(ctx, cur.ID, "reopened", "", "system", data)
		e.publishWorkItem(ctx, cur)
		out = cur
		return nil
	})
	unlock()
	if err != nil {
		e.log.Warn("reopen work item", "work_item_id", wi.ID, "err", err)
		return nil
	}
	if out != nil {
		e.log.Info("reopened completed work item", "work_item_id", out.ID)
	}
	return out
}

// rememberReplyThread records the sub-thread of a text action that a work
// item got. A later reply of the work item goes into that thread. An action
// from outside a thread clears it.
func (e *Engine) rememberReplyThread(ctx context.Context, wi *core.WorkItem, src core.SourceRef) {
	if src.ThreadRef == "" || !wi.HasRef(src.Key()) {
		cur, _ := e.store.KVGet(ctx, replyThreadKey(wi.ID))
		if cur != "" {
			_ = e.store.KVSet(ctx, replyThreadKey(wi.ID), "")
		}
		return
	}
	b, err := json.Marshal(src)
	if err != nil {
		return
	}
	if err := e.store.KVSet(ctx, replyThreadKey(wi.ID), string(b)); err != nil {
		e.log.Warn("record reply thread", "work_item_id", wi.ID, "err", err)
	}
}

// replyThread returns the sub-thread that replies of a work item go into.
func (e *Engine) replyThread(ctx context.Context, wi *core.WorkItem) (core.SourceRef, bool) {
	if wi.Source.ThreadRef != "" {
		return wi.Source, true
	}
	v, err := e.store.KVGet(ctx, replyThreadKey(wi.ID))
	if err != nil || strings.TrimSpace(v) == "" {
		return core.SourceRef{}, false
	}
	var src core.SourceRef
	if json.Unmarshal([]byte(v), &src) != nil || src.ThreadRef == "" {
		return core.SourceRef{}, false
	}
	return src, true
}
