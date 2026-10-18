package runnerapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// processEventsTimeout bounds the work on one batch of stored events.
const processEventsTimeout = 30 * time.Second

func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	var batch runnerproto.EventBatch
	if aerr := decodeBody(r, &batch); aerr != nil {
		writeError(w, aerr)
		return
	}
	events := make([]core.RunnerEvent, len(batch.Events))
	for i, e := range batch.Events {
		events[i] = core.RunnerEvent{Seq: e.Seq, Type: string(e.Type), TurnID: e.TurnID, At: e.At, Data: e.Data}
	}
	before := sess.EventAck
	ack, err := s.runner.AppendRunnerEvents(r.Context(), sess.RunID, sess.Session, events)
	switch {
	case errors.Is(err, core.ErrSeqGap):
		writeError(w, errGap(ack))
		return
	case errors.Is(err, core.ErrInvalid):
		writeError(w, errBad("%v", err))
		return
	case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrNotFound):
		writeError(w, errUnauthorized("session ended"))
		return
	case err != nil:
		writeError(w, errInternal("append events", err))
		return
	}
	var fresh []core.RunnerEvent
	for _, e := range events {
		if e.Seq > before && e.Seq <= ack {
			fresh = append(fresh, e)
		}
	}
	// The events are stored and acknowledged, so their effects must not depend
	// on the request: a runner that ends its session right after its last
	// batch (idle timeout 0) closes the connection and cancels r.Context(),
	// and a lost turn.ended would start a new session.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), processEventsTimeout)
	defer cancel()
	s.processEvents(pctx, sess, fresh)
	writeJSON(w, http.StatusOK, runnerproto.EventAck{Ack: ack})
}

// processEvents applies the effect of new events. The events are stored
// already, so an error is logged and does not change the answer.
func (s *Service) processEvents(ctx context.Context, sess *core.RunSession, events []core.RunnerEvent) {
	if len(events) == 0 {
		return
	}
	for _, e := range events {
		if err := s.processEvent(ctx, sess, e); err != nil {
			slog.Warn("runnerapi: process event", "run", sess.RunID, "session", sess.Session,
				"seq", e.Seq, "type", e.Type, "err", err)
		}
	}
	s.hooks.EventsStored(ctx, sess, events)
}

func (s *Service) processEvent(ctx context.Context, sess *core.RunSession, e core.RunnerEvent) error {
	run := sess.RunID
	switch runnerproto.EventType(e.Type) {
	case runnerproto.EventSessionReady:
		var d runnerproto.SessionReadyData
		if err := decodeData(e.Data, &d); err != nil {
			return err
		}
		caps := make([]string, len(d.Capabilities))
		for i, c := range d.Capabilities {
			caps[i] = string(c)
		}
		now := s.now()
		err := s.updateSession(ctx, sess, func(x *core.RunSession) {
			x.Capabilities = caps
			if x.ReadyAt == nil {
				x.ReadyAt = &now
			}
		})
		if len(d.Repositories) > 0 && s.store != nil {
			err = errors.Join(err, s.resolveStartCommits(ctx, run, d.Repositories))
		}
		return err

	case runnerproto.EventTurnStarted:
		if e.TurnID == "" {
			return errors.New("turn.started without turn_id")
		}
		err := s.updateSession(ctx, sess, func(x *core.RunSession) { x.ActiveTurnID = e.TurnID })
		if _, merr := s.runner.MarkRunInputsAcceptedByTurn(ctx, run, e.TurnID); merr != nil {
			err = errors.Join(err, merr)
		}
		return err

	case runnerproto.EventInputAccepted:
		var d runnerproto.InputAcceptedData
		if err := decodeData(e.Data, &d); err != nil {
			return err
		}
		_, err := s.runner.MarkRunInputsAccepted(ctx, run, d.MessageIDs)
		return err

	case runnerproto.EventTurnEnded:
		if e.TurnID == "" {
			return errors.New("turn.ended without turn_id")
		}
		_, _, err := s.runner.FinishRunInputsOfTurn(ctx, run, e.TurnID)
		if sess.ActiveTurnID == e.TurnID {
			uerr := s.updateSession(ctx, sess, func(x *core.RunSession) {
				if x.ActiveTurnID == e.TurnID {
					x.ActiveTurnID = ""
				}
			})
			err = errors.Join(err, uerr)
		}
		return err
	}
	return nil
}

// decodeData decodes the data of an event. Empty data is not an error.
func decodeData(raw []byte, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// resolveStartCommits replaces the refs of the start commits of a run with
// the commits that the runner checked out. An entry that is resolved already
// stays as it is, so a later session does not change the record.
func (s *Service) resolveStartCommits(ctx context.Context, runID string, repos []runnerproto.RepositoryCommit) error {
	list, err := core.LoadStartCommits(ctx, s.store, runID)
	if err != nil || len(list) == 0 {
		return err
	}
	changed := false
	for i := range list {
		if list[i].Resolved {
			continue
		}
		for _, rc := range repos {
			if rc.Path == list[i].Path && rc.Commit != "" {
				list[i].Ref, list[i].Resolved, changed = rc.Commit, true, true
			}
		}
	}
	if !changed {
		return nil
	}
	return core.SaveStartCommits(ctx, s.store, runID, list)
}
