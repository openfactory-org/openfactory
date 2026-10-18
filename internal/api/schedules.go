package api

import (
	"errors"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/triggers"
)

// Schedule controls outside the definition. The pause lives in the store,
// so it works the same for managed
// and git-backed factories and changes no definition file.

// handleListSchedules serves GET /factories/{uid}/schedules.
func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request, p *Principal) error {
	f, err := s.factoryFor(r.Context(), p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	list, err := triggers.ListSchedules(r.Context(), s.opts.Store, f.ID, s.now())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listResponse("schedules", list, ""))
	return nil
}

// handlePauseSchedule serves POST /factories/{uid}/schedules/{id}/pause.
func (s *Server) handlePauseSchedule(w http.ResponseWriter, r *http.Request, p *Principal) error {
	return s.setSchedulePaused(w, r, p, true)
}

// handleResumeSchedule serves POST /factories/{uid}/schedules/{id}/resume.
func (s *Server) handleResumeSchedule(w http.ResponseWriter, r *http.Request, p *Principal) error {
	return s.setSchedulePaused(w, r, p, false)
}

func (s *Server) setSchedulePaused(w http.ResponseWriter, r *http.Request, p *Principal, paused bool) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	a, t, err := triggers.FindSchedule(ctx, s.opts.Store, f.ID, id)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return errNotFound("schedule " + id)
		}
		return err
	}
	if err := triggers.SetSchedulePaused(ctx, s.opts.Store, a.ID, t.Schedule.Name, paused, p.ActorID(), s.now()); err != nil {
		return err
	}
	list, err := triggers.ListSchedules(ctx, s.opts.Store, f.ID, s.now())
	if err != nil {
		return err
	}
	for _, st := range list {
		if st.ID == id {
			writeJSON(w, http.StatusOK, st)
			return nil
		}
	}
	return errNotFound("schedule " + id)
}

// handleRunAutomation serves POST /automations/{id}/run: a manual start of
// the automation with its prompt, agent and overrides. A disabled
// automation can be started this way too.
func (s *Server) handleRunAutomation(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	id := r.PathValue("id")
	fs, err := s.opts.Store.ListFactories(ctx, p.TeamID, "")
	if err != nil {
		return err
	}
	var (
		auto *core.Automation
		fac  *core.Factory
	)
	for i := range fs {
		as, err := s.opts.Store.ListAutomations(ctx, fs[i].ID)
		if err != nil {
			return err
		}
		for j := range as {
			if as[j].ID == id {
				auto, fac = &as[j], &fs[i]
			}
		}
		if auto != nil {
			break
		}
	}
	if auto == nil {
		return errNotFound("automation " + id)
	}
	if !p.CanOn(fac.ID, core.RoleEditor) {
		return errForbidden("this operation requires the %s role; your role for this factory is %s", core.RoleEditor, p.RoleFor(fac.ID))
	}
	actor := core.Actor{Provider: "factory", UserID: p.UserID(), Handle: p.ActorID()}
	ev := triggers.ManualEvent(*auto, fac.TeamID, actor, s.now())
	wi, run, err := s.opts.Engine.Dispatch(ctx, core.DispatchRequest{
		FactoryID:    fac.ID,
		Prompt:       auto.Prompt,
		Source:       ev.Source,
		Event:        ev,
		AutomationID: auto.ID,
		AgentName:    auto.Agent,
		CreatorID:    p.UserID(),
		Overrides:    auto.Overrides,
		TriggeredBy:  "automation:" + auto.ID,
	})
	if err != nil {
		return err
	}
	if run == nil {
		return Errf(CodeInternalError, "engine returned no run")
	}
	resp := DispatchResponse{RunView: s.runView(run)}
	if wi != nil {
		resp.WorkItemURL = s.WorkItemURL(fac.ID, wi.ID)
		if resp.WorkItemID == "" {
			resp.WorkItemID = wi.ID
		}
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}
