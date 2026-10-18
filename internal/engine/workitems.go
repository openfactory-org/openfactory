package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// labelExplicitStage marks that the stage was set explicitly; the next stage
// inference from run_agents is skipped (and clears the mark).
const labelExplicitStage = "of.stage_explicit"

var validStages = map[core.Stage]bool{
	core.StageIntake: true, core.StageTriage: true, core.StagePlanning: true,
	core.StageAwaitingApproval: true, core.StageBuilding: true, core.StageReviewing: true,
	core.StageHandoff: true, core.StageComplete: true, core.StageCancelled: true,
}

// ValidStage reports whether s is a known stage.
func ValidStage(s core.Stage) bool { return validStages[s] }

// SetStage moves a work item to a stage. Any non-terminal stage may move to any
// other stage (the foreman may skip stages or go back); terminal stages are final.
func (e *Engine) SetStage(ctx context.Context, workItemID string, stage core.Stage, actor, note string) error {
	if !ValidStage(stage) {
		return fmt.Errorf("%w: unknown stage %q", core.ErrInvalid, stage)
	}
	switch stage {
	case core.StageComplete:
		return e.CompleteWorkItem(ctx, workItemID, actor, note)
	case core.StageCancelled:
		wi, err := e.store.GetWorkItem(ctx, workItemID)
		if err != nil {
			return err
		}
		if wi.Stage == core.StageCancelled {
			return nil
		}
		if wi.Stage.Terminal() {
			return fmt.Errorf("%w: work item is %s", core.ErrConflict, wi.Stage)
		}
		if wi.RootRunID != "" {
			return e.Cancel(ctx, wi.RootRunID, actor)
		}
	}
	return e.setStage(ctx, workItemID, stage, actor, note, true)
}

func (e *Engine) setStage(ctx context.Context, workItemID string, stage core.Stage, actor, note string, explicit bool) error {
	defer e.lock("wi:" + workItemID)()
	return e.atomicRetry(ctx, func(ctx context.Context) error {
		wi, err := e.store.GetWorkItem(ctx, workItemID)
		if err != nil {
			return err
		}
		if wi.Stage == stage {
			return nil
		}
		if wi.Stage.Terminal() {
			return fmt.Errorf("%w: work item is already %s", core.ErrConflict, wi.Stage)
		}
		from := wi.Stage
		now := e.now()
		wi.Stage = stage
		wi.UpdatedAt = now
		if stage.Terminal() {
			wi.CompletedAt = &now
		}
		if explicit {
			if wi.Labels == nil {
				wi.Labels = map[string]string{}
			}
			wi.Labels[labelExplicitStage] = "1"
		}
		if err := e.store.UpdateWorkItem(ctx, wi); err != nil {
			return err
		}
		data := map[string]any{"from": from, "to": stage}
		if note != "" {
			data["note"] = note
		}
		if !explicit {
			data["inferred"] = true
		}
		e.timeline(ctx, wi.ID, "stage_changed", "", actor, data)
		e.publishWorkItem(ctx, wi)
		return nil
	})
}

// inferStage moves the work item to the stage implied by a dispatched stage
// agent, unless the stage was just set explicitly.
func (e *Engine) inferStage(ctx context.Context, workItemID string, stage core.Stage, actor string) {
	unlock := e.lock("wi:" + workItemID)
	var wi *core.WorkItem
	cleared := false
	err := retryStale(ctx, func() error {
		cleared = false
		var err error
		if wi, err = e.store.GetWorkItem(ctx, workItemID); err != nil {
			return err
		}
		if wi.Stage.Terminal() || wi.Labels[labelExplicitStage] == "" {
			return nil
		}
		delete(wi.Labels, labelExplicitStage)
		wi.UpdatedAt = e.now()
		cleared = true
		return e.store.UpdateWorkItem(ctx, wi)
	})
	unlock()
	if err != nil || cleared || wi.Stage.Terminal() {
		return
	}
	if wi.Stage == stage {
		return
	}
	if err := e.setStage(ctx, workItemID, stage, actor, "inferred from dispatched agent", false); err != nil {
		e.log.Warn("infer stage", "work_item_id", workItemID, "stage", stage, "err", err)
	}
}

func (e *Engine) openInboxForWorkItem(ctx context.Context, wi *core.WorkItem) ([]core.InboxItem, error) {
	items, err := e.store.ListInboxItems(ctx, wi.FactoryID, false)
	if err != nil {
		return nil, err
	}
	var out []core.InboxItem
	for _, it := range items {
		if !it.Resolved && it.WorkItemID == wi.ID {
			out = append(out, it)
		}
	}
	return out, nil
}

var defaultInboxTitles = map[core.InboxKind]string{
	core.InboxApproval: "Approval requested",
	core.InboxQuestion: "Question from the factory",
	core.InboxHandoff:  "Ready for your review",
	core.InboxFailure:  "Run needs attention",
	core.InboxReview:   "Change ready for review",
	core.InboxBlocked:  "Run waits for input",
	core.InboxAnswered: "For your information",
}

// RequestHuman creates an inbox item for a run, notifies the work source, and
// moves the work item to awaiting_approval (approvals) or handoff (handoffs).
// The run is expected to end its turn; the decision arrives as a follow-up.
func (e *Engine) RequestHuman(ctx context.Context, runID string, kind core.InboxKind, title, body string, options []string) (*core.InboxItem, error) {
	defaultTitle, ok := defaultInboxTitles[kind]
	if !ok {
		return nil, fmt.Errorf("%w: unknown inbox kind %q", core.ErrInvalid, kind)
	}
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.State == core.RunCancelled {
		return nil, fmt.Errorf("%w: run was cancelled", core.ErrConflict)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = defaultTitle
	}
	if kind == core.InboxApproval && len(options) == 0 {
		options = []string{"approve", "reject"}
	}
	var wi *core.WorkItem
	if r.WorkItemID != "" {
		if wi, err = e.store.GetWorkItem(ctx, r.WorkItemID); err != nil {
			return nil, err
		}
	}
	it := &core.InboxItem{
		ID:        core.NewID("inb"),
		FactoryID: r.FactoryID,
		RunID:     r.ID,
		Kind:      kind,
		Title:     title,
		Body:      body,
		Options:   options,
		CreatedAt: e.now(),
	}
	if wi != nil {
		it.WorkItemID = wi.ID
		it.Recipients = recipientsOf(wi)
		if kind == core.InboxBlocked || kind == core.InboxFailure {
			// One open notice for each person is enough.
			if open := e.openItemFor(ctx, wi, it.Recipients); open != nil {
				return open, nil
			}
		}
	}
	if kind == core.InboxQuestion {
		it.Questions = defaultQuestions(ctx, title, options)
	}
	actor := "run:" + r.ID
	// The inbox item, its timeline entry and the notice to the work source
	// commit together.
	err = e.Atomic(ctx, func(ctx context.Context) error {
		if err := e.store.CreateInboxItem(ctx, it); err != nil {
			return err
		}
		cp := *it
		e.publish(ctx, core.TopicInbox, &cp)
		if wi == nil {
			return nil
		}

		timelineKind := map[core.InboxKind]string{
			core.InboxApproval: "approval_requested",
			core.InboxQuestion: "question",
		}[kind]
		if timelineKind == "" {
			timelineKind = "note"
		}
		e.timeline(ctx, wi.ID, timelineKind, r.ID, actor, map[string]any{"inbox_id": it.ID, "kind": kind, "title": title, "body": truncate(body, 4000)})

		var text strings.Builder
		noticeKind := "update"
		switch kind {
		case core.InboxApproval:
			noticeKind = "approval_request"
			fmt.Fprintf(&text, "**Approval needed: %s**\n\n%s\n\nReply `approve` to continue, or reply with the changes you want.", title, body)
		case core.InboxQuestion:
			noticeKind = "question"
			fmt.Fprintf(&text, "**Question: %s**\n\n%s\n\nReply here with your answer.", title, body)
		case core.InboxHandoff:
			noticeKind = "handoff"
			fmt.Fprintf(&text, "**%s**\n\n%s", title, body)
		case core.InboxFailure:
			noticeKind = "error"
			fmt.Fprintf(&text, "**%s**\n\n%s", title, body)
		default:
			fmt.Fprintf(&text, "**%s**\n\n%s", title, body)
		}
		e.notifyInbox(ctx, wi, noticeKind, strings.TrimSpace(text.String()), options, it.ID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if wi == nil {
		return it, nil
	}

	switch kind {
	case core.InboxApproval:
		if err := e.setStage(ctx, wi.ID, core.StageAwaitingApproval, actor, title, false); err != nil {
			e.log.Warn("set awaiting_approval", "work_item_id", wi.ID, "err", err)
		}
	case core.InboxHandoff:
		if err := e.setStage(ctx, wi.ID, core.StageHandoff, actor, title, false); err != nil {
			e.log.Warn("set handoff", "work_item_id", wi.ID, "err", err)
		}
	}
	return it, nil
}

// CanApprove reports whether a team member can resolve an approval of a
// factory. If the factory has no approvers list in factory.yaml, every
// caller can. If it has one, the user must exist and their email must be in
// the list (case-insensitive). An empty userID (a sender without a linked
// account) cannot approve a factory with an approvers list.
func (e *Engine) CanApprove(ctx context.Context, factoryID, userID string) (bool, error) {
	rt, err := LoadFactoryRuntime(ctx, e.store, factoryID)
	if err != nil {
		return false, err
	}
	if len(rt.Approvers) == 0 {
		return true, nil
	}
	if userID == "" {
		return false, nil
	}
	u, err := e.store.GetUser(ctx, userID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return IsApprover(rt.Approvers, u), nil
}

// IsApprover reports whether u matches an entry of an approvers list. An
// entry matches the email of the user, case-insensitive. For a service
// account, an entry also matches its name.
func IsApprover(approvers []string, u *core.User) bool {
	if u == nil {
		return false
	}
	email := strings.TrimSpace(u.Email)
	for _, a := range approvers {
		a = strings.TrimSpace(a)
		if email != "" && strings.EqualFold(a, email) {
			return true
		}
		if u.IsServiceAccount() && u.Name != "" && a == u.Name {
			return true
		}
	}
	return false
}

// ResolveInbox records a human decision and delivers it to the requesting run
// as a follow-up. The core.Resolver in ctx (core.WithResolver) names the team
// member and the tool. It is recorded on the item. For an approval, the
// member must pass CanApprove.
func (e *Engine) ResolveInbox(ctx context.Context, inboxID, resolution, actor string) error {
	resolution = strings.TrimSpace(resolution)
	if resolution == "" {
		return fmt.Errorf("%w: resolution is required", core.ErrInvalid)
	}
	unlock := e.lock("inbox:" + inboxID)
	it, err := e.store.GetInboxItem(ctx, inboxID)
	if err != nil {
		unlock()
		return err
	}
	if it.Resolved {
		unlock()
		return fmt.Errorf("%w: inbox item already resolved", core.ErrConflict)
	}
	if it.Kind == core.InboxApproval {
		res, _ := core.ResolverFrom(ctx)
		ok, err := e.CanApprove(ctx, it.FactoryID, res.UserID)
		if err != nil {
			unlock()
			return err
		}
		if !ok {
			unlock()
			return fmt.Errorf("%w: only the approvers listed in factory.yaml can resolve this approval", core.ErrForbidden)
		}
	}
	err = e.Atomic(ctx, func(ctx context.Context) error {
		if err := e.resolveInboxItem(ctx, it, resolution, actor); err != nil {
			return err
		}
		if it.WorkItemID != "" {
			kind := "answer"
			if it.Kind == core.InboxApproval {
				kind = "approval_decided"
			}
			e.timeline(ctx, it.WorkItemID, kind, it.RunID, actor, map[string]any{"inbox_id": it.ID, "resolution": truncate(resolution, 4000)})
		}
		return nil
	})
	unlock()
	if err != nil {
		return err
	}
	if it.RunID == "" || it.Kind == core.InboxAnswered {
		// An answered item is information only: the run needs no reply.
		return nil
	}
	who := actor
	if who == "" {
		who = "a team member"
	}
	var msg string
	switch it.Kind {
	case core.InboxApproval:
		msg = fmt.Sprintf("[human decision] Approval request %q was resolved by %s: %s\n\nIf approved, continue with the next stage. If rejected or changes were requested, revise accordingly and ask again before implementation.", it.Title, who, resolution)
	case core.InboxQuestion:
		msg = fmt.Sprintf("[human answer] %s answered your question %q:\n\n%s", who, it.Title, resolution)
	default:
		msg = fmt.Sprintf("[human response] %s responded to %q (%s):\n\n%s", who, it.Title, it.Kind, resolution)
	}
	if err := e.FollowUp(ctx, it.RunID, msg, actor); err != nil {
		e.log.Warn("deliver inbox resolution", "inbox_id", it.ID, "run_id", it.RunID, "err", err)
	} else {
		e.recordAnswer(ctx, it.ID, it.RunID, msg)
	}
	return nil
}

// PostUpdate posts a progress update from a run back to its work source. An
// update of kind "handoff" also records a handoff inbox item and moves the work item to handoff.
func (e *Engine) PostUpdate(ctx context.Context, runID, text, kind string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("%w: text is required", core.ErrInvalid)
	}
	if kind == "" {
		kind = "update"
	}
	if kind == "handoff" {
		_, err := e.RequestHuman(ctx, runID, core.InboxHandoff, "", text, nil)
		return err
	}
	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if r.WorkItemID == "" {
		return nil
	}
	wi, err := e.store.GetWorkItem(ctx, r.WorkItemID)
	if err != nil {
		return err
	}
	return e.Atomic(ctx, func(ctx context.Context) error {
		e.timeline(ctx, wi.ID, "message_out", r.ID, "run:"+r.ID, map[string]any{"kind": kind, "text": truncate(text, 8000)})
		e.notify(ctx, wi, kind, text, nil)
		return nil
	})
}

// forgeProvider maps a forge name (GITHUB, GITLAB, AZURE_DEVOPS) to an event provider name.
func forgeProvider(forge string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(forge), "-", "_"))
}

// LinkPullRequest attaches (or updates) a pull request on a work item. Events on
// the pull request are routed to the work item from then on.
func (e *Engine) LinkPullRequest(ctx context.Context, workItemID string, pr core.PullRequestRef) error {
	if pr.URL == "" && (pr.Repo == "" || pr.Number == 0) {
		return fmt.Errorf("%w: pull request url or repo+number is required", core.ErrInvalid)
	}
	if pr.State == "" {
		pr.State = "open"
	}
	defer e.lock("wi:" + workItemID)()
	return e.atomicRetry(ctx, func(ctx context.Context) error {
		wi, err := e.store.GetWorkItem(ctx, workItemID)
		if err != nil {
			return err
		}
		replaced := false
		for i, p := range wi.PullRequests {
			if (pr.URL != "" && p.URL == pr.URL) || (pr.Repo != "" && pr.Number != 0 && strings.EqualFold(p.Repo, pr.Repo) && p.Number == pr.Number) {
				merged := p
				if pr.Forge != "" {
					merged.Forge = pr.Forge
				}
				if pr.Repo != "" {
					merged.Repo = pr.Repo
				}
				if pr.Number != 0 {
					merged.Number = pr.Number
				}
				if pr.URL != "" {
					merged.URL = pr.URL
				}
				if pr.Branch != "" {
					merged.Branch = pr.Branch
				}
				merged.State = pr.State
				wi.PullRequests[i] = merged
				replaced = true
				break
			}
		}
		if !replaced {
			wi.PullRequests = append(wi.PullRequests, pr)
			if pr.Repo != "" && pr.Number != 0 {
				wi.AddRef(core.WorkItemRef{Kind: core.RefPullRequest, Source: core.PullRequestSource(pr), AddedAt: e.now()})
			}
		}
		wi.UpdatedAt = e.now()
		if err := e.store.UpdateWorkItem(ctx, wi); err != nil {
			return err
		}
		if pr.Repo != "" && pr.Number != 0 {
			provider := forgeProvider(pr.Forge)
			if provider == "" {
				provider = "github"
			}
			key := core.SourceRef{Provider: provider, Repo: pr.Repo, Number: pr.Number}.Key()
			if key != wi.SourceKey {
				if err := e.store.KVSet(ctx, sourceAliasKey(wi.FactoryID, key), wi.ID); err != nil {
					e.log.Warn("alias pull request to work item", "work_item_id", wi.ID, "err", err)
				}
			}
		}
		if !replaced {
			e.timeline(ctx, wi.ID, "pr_linked", "", "system", pr)
			// Attach the pull request to the tracker issues.
			e.writeBackPullRequest(ctx, wi, pr)
		}
		e.publishWorkItem(ctx, wi)
		return nil
	})
}

// CompleteWorkItem marks a work item complete, stops its remaining child runs and
// clears open inbox items.
func (e *Engine) CompleteWorkItem(ctx context.Context, workItemID, actor, note string) error {
	wi, err := e.store.GetWorkItem(ctx, workItemID)
	if err != nil {
		return err
	}
	if wi.Stage == core.StageComplete {
		return nil
	}
	if wi.Stage == core.StageCancelled {
		return fmt.Errorf("%w: work item was cancelled", core.ErrConflict)
	}
	if err := e.setStage(ctx, wi.ID, core.StageComplete, actor, note, true); err != nil {
		return err
	}
	if cur, err := e.store.GetWorkItem(ctx, wi.ID); err == nil {
		// Move the tracker issues to done.
		e.writeBackComplete(ctx, cur)
	}
	if wi.RootRunID != "" {
		// Stop child runs that are still active; the root run winds down on its own.
		if _, err := e.cancelTree(ctx, wi.RootRunID, wi.RootRunID, actor); err != nil {
			e.log.Warn("stop child runs on completion", "work_item_id", wi.ID, "err", err)
		}
	}
	if open, err := e.openInboxForWorkItem(ctx, wi); err == nil {
		for i := range open {
			e.markInboxResolved(ctx, &open[i], "work item completed", actor)
		}
	}
	text := "This work item is complete."
	if note != "" {
		text += " " + note
	}
	if cur, err := e.store.GetWorkItem(ctx, wi.ID); err == nil {
		e.notifyTop(ctx, cur, "update", text)
	}
	return nil
}
