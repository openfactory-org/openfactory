package runnerapi

import (
	"log/slog"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func (s *Service) handleEnd(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	var req runnerproto.EndRequest
	if aerr := decodeBody(r, &req); aerr != nil {
		writeError(w, aerr)
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, errBad("%v", err))
		return
	}
	if req.LastEventSeq > sess.EventAck {
		writeError(w, errGap(sess.EventAck))
		return
	}
	ctx := r.Context()
	now := s.now()
	// The sandbox is not trusted. Only the codes that a runner can report
	// are kept.
	var code core.FailureCode
	if req.FailureCode != "" {
		code = core.RunnerFailureCode(req.FailureCode)
	}
	err := s.updateSession(ctx, sess, func(x *core.RunSession) {
		x.State = core.SessionEnded
		x.EndedAt = &now
		x.EndReason = string(req.Reason)
		x.FailureCode = code
		x.ActiveTurnID = ""
	})
	if err != nil {
		writeError(w, errInternal("end session", err))
		return
	}
	if _, err := s.runner.ResetRunInputsOfSession(ctx, sess.RunID, sess.Session); err != nil {
		slog.Warn("runnerapi: reset inputs of session", "run", sess.RunID, "session", sess.Session, "err", err)
	}
	s.hooks.SessionEnded(ctx, sess, req)
	s.wake.notify(sessionKey{sess.RunID, sess.Session})
	writeJSON(w, http.StatusOK, struct{}{})
}
