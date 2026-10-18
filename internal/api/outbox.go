package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/coord"
)

// OutboxMessageView is a dead-lettered outbox message, as the dashboard shows
// it. The payload is not included.
type OutboxMessageView struct {
	ID           string    `json:"id"`
	Topic        string    `json:"topic"`
	PartitionKey string    `json:"partition_key,omitempty"`
	State        string    `json:"state"`
	Attempts     int       `json:"attempts"`
	LastError    string    `json:"last_error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	// Summary names the event or notice, for example "issue_labeled github:acme/api#7".
	Summary    string `json:"summary,omitempty"`
	FactoryID  string `json:"factory_id,omitempty"`
	WorkItemID string `json:"work_item_id,omitempty"`
}

// outboxCoordinator returns the coordinator of the store, the same one the
// event bus uses (coord.From), or nil.
func (s *Server) outboxCoordinator() core.Coordinator {
	return coord.From(s.opts.Store)
}

// outboxView decodes the team and a summary of a message. It reports false
// when the message does not belong to teamID.
func (s *Server) outboxView(ctx context.Context, m core.OutboxMessage, teamID string, teams map[string]string) (OutboxMessageView, bool) {
	v := OutboxMessageView{ID: m.ID, Topic: m.Topic, PartitionKey: m.PartitionKey, State: string(m.State),
		Attempts: m.Attempts, LastError: m.LastError, CreatedAt: m.CreatedAt}
	factoryTeam := func(id string) string {
		if id == "" {
			return ""
		}
		if t, ok := teams[id]; ok {
			return t
		}
		t := ""
		if f, err := s.opts.Store.GetFactory(ctx, id); err == nil {
			t = f.TeamID
		}
		teams[id] = t
		return t
	}
	team := ""
	switch m.Topic {
	case core.TopicInboundEvent:
		var e core.Event
		if json.Unmarshal(m.Payload, &e) != nil {
			return v, false
		}
		team, v.FactoryID = e.TeamID, e.FactoryID
		v.Summary = e.Type
		if k := e.Source.Key(); k != "" {
			v.Summary += " " + k
		}
	case core.TopicOutboundNotice:
		var n core.OutboundMessage
		if json.Unmarshal(m.Payload, &n) != nil {
			return v, false
		}
		v.FactoryID, v.WorkItemID = n.FactoryID, n.WorkItemID
		if v.FactoryID == "" && n.WorkItemID != "" {
			if wi, err := s.opts.Store.GetWorkItem(ctx, n.WorkItemID); err == nil {
				v.FactoryID = wi.FactoryID
			}
		}
		team = factoryTeam(v.FactoryID)
		v.Summary = n.Kind
		if k := n.Source.Key(); k != "" {
			v.Summary += " " + k
		}
	}
	return v, team != "" && team == teamID
}

// deadOutbox lists the dead messages of the caller's team.
func (s *Server) deadOutbox(ctx context.Context, c core.Coordinator, teamID string) ([]OutboxMessageView, map[string]bool, error) {
	msgs, err := c.OutboxList(ctx, core.OutboxDead, 500)
	if err != nil {
		return nil, nil, err
	}
	teams := map[string]string{}
	out := []OutboxMessageView{}
	ids := map[string]bool{}
	for _, m := range msgs {
		if v, mine := s.outboxView(ctx, m, teamID, teams); mine {
			out = append(out, v)
			ids[m.ID] = true
		}
	}
	return out, ids, nil
}

// handleListDeadOutbox lists dead-lettered outbox messages of the team,
// oldest first. A dead message does not hold back later messages of its
// partition key.
func (s *Server) handleListDeadOutbox(w http.ResponseWriter, r *http.Request, p *Principal) error {
	c := s.outboxCoordinator()
	if c == nil {
		writeJSON(w, http.StatusOK, listResponse("messages", []OutboxMessageView{}, ""))
		return nil
	}
	out, _, err := s.deadOutbox(r.Context(), c, p.TeamID)
	if err != nil {
		return err
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n < len(out) {
		out = out[:n]
	}
	writeJSON(w, http.StatusOK, listResponse("messages", out, ""))
	return nil
}

// handleRedriveOutbox moves a dead message of the team back to pending. It
// takes its old place in the order of its partition key.
func (s *Server) handleRedriveOutbox(w http.ResponseWriter, r *http.Request, p *Principal) error {
	id := r.PathValue("id")
	c := s.outboxCoordinator()
	if c == nil {
		return errNotFound("outbox message " + id)
	}
	_, ids, err := s.deadOutbox(r.Context(), c, p.TeamID)
	if err != nil {
		return err
	}
	if !ids[id] {
		return errNotFound("dead outbox message " + id)
	}
	if err := c.OutboxRedrive(r.Context(), id, s.now()); err != nil {
		if isNotFound(err) {
			return errNotFound("dead outbox message " + id)
		}
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
