package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// UnresolveWindow is the time after a resolve in which a user can undo it.
const UnresolveWindow = 10 * time.Minute

// recipientsOf returns the default recipients of an inbox item of a work
// item: the creator of the work item. A work item without a creator gives
// no recipients, and the item is then for the editors and admins of the
// factory.
func recipientsOf(wi *core.WorkItem) []string {
	if wi == nil || wi.CreatorID == "" {
		return nil
	}
	return []string{wi.CreatorID}
}

// defaultQuestions builds the questions of a question item. A question from
// core.WithQuestion in ctx wins. Else the item has one question: the title,
// with the options of the request.
func defaultQuestions(ctx context.Context, title string, options []string) []core.Question {
	q, ok := core.QuestionFrom(ctx)
	if !ok {
		q = core.Question{Text: title, Options: options}
	}
	if q.ID == "" {
		q.ID = "q1"
	}
	if q.Text == "" {
		q.Text = title
	}
	if len(q.Options) == 0 {
		q.Options = options
	}
	switch q.Type {
	case core.QuestionSingleSelect, core.QuestionMultiSelect, core.QuestionText:
	default:
		q.Type = core.QuestionText
		if len(q.Options) > 0 {
			q.Type = core.QuestionSingleSelect
		}
	}
	if q.Type == core.QuestionText {
		q.Options = nil
	}
	if q.RecommendedOption != "" && !slices.Contains(q.Options, q.RecommendedOption) {
		q.RecommendedOption = ""
	}
	return []core.Question{q}
}

// openItemFor returns an open item of the work item that is for one of the
// recipients, or nil. A blocked or failure item is created only when no
// such item exists.
func (e *Engine) openItemFor(ctx context.Context, wi *core.WorkItem, recipients []string) *core.InboxItem {
	open, err := e.openInboxForWorkItem(ctx, wi)
	if err != nil {
		return nil
	}
	for i := range open {
		got := open[i].RecipientIDs()
		if len(recipients) == 0 && len(got) == 0 {
			return &open[i]
		}
		for _, r := range recipients {
			if slices.Contains(got, r) {
				return &open[i]
			}
		}
	}
	return nil
}

// reportBlocked raises a blocked inbox item for a root run that stopped in
// blocked with the reason harness_needs_input, unless the work item has an
// open item for the same people.
func (e *Engine) reportBlocked(ctx context.Context, r *core.Run) {
	wi, err := e.store.GetWorkItem(ctx, r.WorkItemID)
	if err != nil || wi.Stage.Terminal() {
		return
	}
	rcpt := recipientsOf(wi)
	if e.openItemFor(ctx, wi, rcpt) != nil {
		return
	}
	body := strings.TrimSpace(r.Result)
	if body == "" {
		body = "The agent stopped and waits for input. Answer to continue the run."
	}
	it := &core.InboxItem{
		ID:         core.NewID("inb"),
		FactoryID:  r.FactoryID,
		WorkItemID: wi.ID,
		RunID:      r.ID,
		Kind:       core.InboxBlocked,
		Title:      "Run waits for input: " + wi.Title,
		Body:       truncate(body, 4000),
		Recipients: rcpt,
		CreatedAt:  e.now(),
	}
	if err := e.store.CreateInboxItem(ctx, it); err == nil {
		cp := *it
		e.publish(ctx, core.TopicInbox, &cp)
	}
}

// closeHandoffs resolves the open handoff items of a work item when a review
// is submitted at the forge or a pull request merges. The resolution does not
// go to the run: the forge event reaches it on its own path.
func (e *Engine) closeHandoffs(ctx context.Context, wi *core.WorkItem, ev *core.Event, actor string) {
	open, err := e.openInboxForWorkItem(ctx, wi)
	if err != nil {
		return
	}
	resolution := "review submitted"
	if isMergeEvent(ev) {
		resolution = "pull request merged"
	}
	if actor != "" {
		resolution += " by " + actor
	}
	ctx = core.WithResolver(ctx, core.Resolver{UserID: ev.Actor.UserID, Via: core.InboxViaForge})
	for i := range open {
		it := &open[i]
		if it.Kind != core.InboxHandoff {
			continue
		}
		if err := e.resolveInboxItem(ctx, it, resolution, actor); err != nil && !errors.Is(err, core.ErrConflict) {
			e.log.Warn("close handoff item", "inbox_id", it.ID, "err", err)
			continue
		}
		e.timeline(ctx, wi.ID, "note", it.RunID, actor, map[string]any{"inbox_id": it.ID, "text": "handoff closed: " + resolution})
	}
}

// recordAnswer stores the ID of the run message that carried the resolution
// of an item. UnresolveInbox reads it.
func (e *Engine) recordAnswer(ctx context.Context, itemID, runID, text string) {
	msgs, err := e.store.ListRunMessages(ctx, runID, 0)
	if err != nil {
		return
	}
	var id string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && msgs[i].Content == text {
			id = msgs[i].ID
			break
		}
	}
	if id == "" {
		return
	}
	_ = retryStale(ctx, func() error {
		it, err := e.store.GetInboxItem(ctx, itemID)
		if err != nil {
			return err
		}
		it.AnswerMessageID = id
		return e.store.UpdateInboxItem(ctx, it)
	})
}

// runInputGetter is the part of core.RunnerStore that answerConsumed needs.
type runInputGetter interface {
	GetRunInput(ctx context.Context, id string) (*core.RunInput, error)
}

// answerConsumed reports whether the run consumed the message that carried
// the answer of an item. The queue of the run tells: a message that is not
// pending was taken by the run. A message that is not in the queue was not
// consumed.
func (e *Engine) answerConsumed(ctx context.Context, it *core.InboxItem) (bool, error) {
	if it.RunID == "" || it.AnswerMessageID == "" {
		return false, nil
	}
	q, ok := e.store.(runInputGetter)
	if !ok {
		return false, nil
	}
	in, err := q.GetRunInput(ctx, it.AnswerMessageID)
	if errors.Is(err, core.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return in.State != core.RunInputPending, nil
}

// UnresolveInbox undoes a resolve. It works for
// UnresolveWindow after the resolve, and only while the run did not consume
// the answer. The run gets a message that withdraws the answer, in the same
// turn as the answer.
func (e *Engine) UnresolveInbox(ctx context.Context, inboxID, actor string) (*core.InboxItem, error) {
	defer e.lock("inbox:" + inboxID)()
	it, err := e.store.GetInboxItem(ctx, inboxID)
	if err != nil {
		return nil, err
	}
	if !it.Resolved {
		return nil, fmt.Errorf("%w: inbox item is not resolved", core.ErrConflict)
	}
	if it.ResolvedAt == nil || e.now().Sub(*it.ResolvedAt) > UnresolveWindow {
		return nil, fmt.Errorf("%w: the resolve is older than %s and cannot be undone", core.ErrConflict, UnresolveWindow)
	}
	if it.ResolvedVia == core.InboxViaForge || (it.ResolvedBy == "" && it.ResolvedVia == "") {
		return nil, fmt.Errorf("%w: the platform closed this item and it cannot be undone", core.ErrConflict)
	}
	consumed, err := e.answerConsumed(ctx, it)
	if err != nil {
		return nil, err
	}
	if consumed {
		return nil, fmt.Errorf("%w: the run already read the answer", core.ErrConflict)
	}
	old := it.Resolution
	err = retryStale(ctx, func() error {
		cur, err := e.store.GetInboxItem(ctx, inboxID)
		if err != nil {
			return err
		}
		cur.Resolved, cur.Resolution, cur.ResolvedBy, cur.ResolvedVia, cur.ResolvedAt = false, "", "", "", nil
		cur.AnswerMessageID = ""
		if err := e.store.UpdateInboxItem(ctx, cur); err != nil {
			return err
		}
		it = cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	cp := *it
	e.publish(ctx, core.TopicInbox, &cp)
	if it.WorkItemID != "" {
		e.timeline(ctx, it.WorkItemID, "note", it.RunID, actor, map[string]any{"inbox_id": it.ID, "text": "resolve undone"})
	}
	if it.RunID != "" && resumesRun(it.Kind) {
		who := e.actorLabel(ctx, actor)
		msg := fmt.Sprintf("[human correction] %s withdrew the answer to %q (it was: %s). Ignore that answer. A new answer will arrive as a follow-up message.", who, it.Title, truncate(old, 500))
		if err := e.FollowUp(ctx, it.RunID, msg, actor); err != nil {
			e.log.Warn("deliver inbox unresolve", "inbox_id", it.ID, "run_id", it.RunID, "err", err)
		}
	}
	return it, nil
}
