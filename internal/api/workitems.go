package api

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

var allStages = []core.Stage{
	core.StageIntake, core.StageTriage, core.StagePlanning, core.StageAwaitingApproval,
	core.StageBuilding, core.StageReviewing, core.StageHandoff, core.StageComplete, core.StageCancelled,
}

// ParseStage validates a stage name.
func ParseStage(v string) (core.Stage, error) {
	st := core.Stage(strings.ToLower(strings.TrimSpace(v)))
	if !slices.Contains(allStages, st) {
		names := make([]string, len(allStages))
		for i, s := range allStages {
			names[i] = string(s)
		}
		return "", errInvalid("unknown stage %q (expected one of %s)", v, strings.Join(names, ", "))
	}
	return st, nil
}

func (s *Server) handleListWorkItems(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	pg, err := pageFrom(r)
	if err != nil {
		return err
	}
	filter := core.WorkItemFilter{FactoryID: f.ID, Search: r.URL.Query().Get("search")}
	for _, v := range qList(r, "stage") {
		st, err := ParseStage(v)
		if err != nil {
			return err
		}
		filter.Stages = append(filter.Stages, st)
	}
	switch c := r.URL.Query().Get("creator"); c {
	case "":
	case "me":
		filter.CreatorID = p.UserID()
	default:
		filter.CreatorID = c
	}
	if filter.Since, err = qSince(r, "since"); err != nil {
		return err
	}
	filter.ListOpts = pg.listOpts()
	items, err := s.opts.Store.ListWorkItems(ctx, filter)
	if err != nil {
		return err
	}
	items, next := trimPage(items, func(wi core.WorkItem) (timeT, string) { return wi.CreatedAt, wi.ID }, pg.Limit)
	views := make([]WorkItemView, 0, len(items))
	for i := range items {
		views = append(views, s.workItemView(&items[i]))
	}
	writeJSON(w, http.StatusOK, listResponse("work_items", views, next))
	return nil
}

// WorkItemDetail is a work item with its timeline, runs and open inbox items.
type WorkItemDetail struct {
	WorkItemView
	Timeline []core.WorkItemEvent `json:"timeline"`
	Runs     []RunView            `json:"runs"`
	Inbox    []InboxItemView      `json:"inbox"`
}

// GetWorkItemDetail loads a work item visible to p with its timeline and runs.
func (s *Server) GetWorkItemDetail(ctx context.Context, p *Principal, id string) (*WorkItemDetail, error) {
	wi, _, err := s.workItemFor(ctx, p, id)
	if err != nil {
		return nil, err
	}
	events, err := s.opts.Store.ListWorkItemEvents(ctx, wi.ID)
	if err != nil {
		return nil, err
	}
	runs, err := s.opts.Store.ListRuns(ctx, core.RunFilter{TeamID: p.TeamID, WorkItemID: wi.ID, ListOpts: core.ListOpts{Limit: 500}})
	if err != nil {
		return nil, err
	}
	inbox, err := s.opts.Store.ListInboxItems(ctx, wi.FactoryID, false)
	if err != nil {
		return nil, err
	}
	d := &WorkItemDetail{WorkItemView: s.workItemView(wi), Timeline: events, Runs: []RunView{}, Inbox: []InboxItemView{}}
	if d.Timeline == nil {
		d.Timeline = []core.WorkItemEvent{}
	}
	for i := range runs {
		d.Runs = append(d.Runs, s.runView(&runs[i]))
	}
	for i := range inbox {
		if inbox[i].WorkItemID == wi.ID {
			d.Inbox = append(d.Inbox, inboxView(&inbox[i], p))
		}
	}
	return d, nil
}

func (s *Server) handleGetWorkItem(w http.ResponseWriter, r *http.Request, p *Principal) error {
	d, err := s.GetWorkItemDetail(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, d)
	return nil
}

// MessageForeman sends a message to the foreman run of a work item.
func (s *Server) MessageForeman(ctx context.Context, p *Principal, workItemID, message string) (*core.WorkItem, error) {
	wi, _, err := s.MessageForemanReceipt(ctx, p, workItemID, message)
	return wi, err
}

// MessageForemanReceipt is MessageForeman with the record of the message: its
// ID and where it went.
func (s *Server) MessageForemanReceipt(ctx context.Context, p *Principal, workItemID, message string) (*core.WorkItem, core.MessageReceipt, error) {
	if strings.TrimSpace(message) == "" {
		return nil, core.MessageReceipt{}, errInvalid("message is required")
	}
	wi, f, err := s.workItemFor(ctx, p, workItemID)
	if err != nil {
		return nil, core.MessageReceipt{}, err
	}
	if err := s.CheckFollowupAllowed(ctx, p, f, wi); err != nil {
		return nil, core.MessageReceipt{}, err
	}
	if wi.RootRunID == "" {
		return nil, core.MessageReceipt{}, Errf(CodeOperationNotSupported, "work item %s has no foreman run", wi.ID)
	}
	rec, err := s.opts.Engine.SendMessage(ctx, wi.RootRunID, message, p.ActorID(), nil)
	if err != nil {
		return nil, core.MessageReceipt{}, err
	}
	return wi, rec, nil
}

func (s *Server) handleWorkItemMessage(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req struct {
		Message string `json:"message"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	wi, rec, err := s.MessageForemanReceipt(r.Context(), p, r.PathValue("id"), req.Message)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"work_item_id": wi.ID, "run_id": wi.RootRunID, "message_id": rec.MessageID, "outcome": rec.Outcome})
	return nil
}

func (s *Server) handleCancelWorkItem(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return err
	}
	wi, _, err := s.workItemFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if wi.Stage.Terminal() {
		return Errf(CodeConflict, "work item is already %s", wi.Stage)
	}
	if wi.RootRunID != "" {
		if run, err := s.opts.Store.GetRun(ctx, wi.RootRunID); err == nil && !run.State.Terminal() {
			if err := s.opts.Engine.Cancel(ctx, run.ID, p.ActorID()); err != nil {
				return err
			}
		}
	}
	note := req.Reason
	if note == "" {
		note = "Cancelled via API"
	}
	if err := s.opts.Engine.SetStage(ctx, wi.ID, core.StageCancelled, p.ActorID(), note); err != nil {
		return err
	}
	return s.writeWorkItem(w, ctx, wi.ID)
}

// CompleteWorkItem marks a work item complete on behalf of p.
func (s *Server) CompleteWorkItem(ctx context.Context, p *Principal, id, note string) (*core.WorkItem, error) {
	wi, _, err := s.workItemFor(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if wi.Stage.Terminal() {
		return nil, Errf(CodeConflict, "work item is already %s", wi.Stage)
	}
	if err := s.opts.Engine.CompleteWorkItem(ctx, wi.ID, p.ActorID(), note); err != nil {
		return nil, err
	}
	if nwi, err := s.opts.Store.GetWorkItem(ctx, wi.ID); err == nil {
		wi = nwi
	}
	return wi, nil
}

func (s *Server) handleCompleteWorkItem(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req struct {
		Note string `json:"note"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return err
	}
	wi, err := s.CompleteWorkItem(r.Context(), p, r.PathValue("id"), req.Note)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.workItemView(wi))
	return nil
}

func (s *Server) handleSetWorkItemStage(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req struct {
		Stage string `json:"stage"`
		Note  string `json:"note"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	st, err := ParseStage(req.Stage)
	if err != nil {
		return err
	}
	wi, _, err := s.workItemFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if err := s.opts.Engine.SetStage(ctx, wi.ID, st, p.ActorID(), req.Note); err != nil {
		return err
	}
	return s.writeWorkItem(w, ctx, wi.ID)
}

func (s *Server) writeWorkItem(w http.ResponseWriter, ctx context.Context, id string) error {
	wi, err := s.opts.Store.GetWorkItem(ctx, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, s.workItemView(wi))
	return nil
}

// resolvedVia names the tool of a request for InboxItem.ResolvedVia: "api"
// for a bearer API key, "dashboard" for a browser session.
func resolvedVia(p *Principal) string {
	if p != nil && p.APIKey != nil {
		return core.InboxViaAPI
	}
	return core.InboxViaDashboard
}

// CheckFollowupAllowed applies restrictFollowupsToStarter of factory.yaml
// (REST, message_foreman and the send_work_item hand-back):
// when it is set, only the creator of the work item can send it a follow-up
// message. A work item without a creator then takes no follow-ups from the
// API; members answer its inbox items instead.
func (s *Server) CheckFollowupAllowed(ctx context.Context, p *Principal, f *core.Factory, wi *core.WorkItem) error {
	if f == nil || wi == nil {
		return nil
	}
	def, _, err := s.activeDefinition(ctx, f)
	if err != nil || def == nil || !def.RestrictFollowupsToStarter {
		return err
	}
	if wi.CreatorID != "" && wi.CreatorID == p.UserID() {
		return nil
	}
	return errForbidden("this factory sets restrictFollowupsToStarter: only the user who started work item %s can send follow-ups", wi.ID)
}
