package runnerapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

// IssueBootstrap makes a new bootstrap token for a session that the caller
// is about to create. It returns the token and its hash. The caller puts the
// hash into RunSession.BootstrapHash before CreateRunSession.
func IssueBootstrap() (token, hash string, err error) {
	return runnertoken.NewBootstrap()
}

func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	resp, aerr := s.start(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

func (s *Service) start(r *http.Request) (runnerproto.StartResponse, *apiError) {
	ctx := r.Context()
	var zero runnerproto.StartResponse
	tok, ok := bearer(r)
	if !ok {
		return zero, errUnauthorized("missing bootstrap token")
	}
	var req runnerproto.StartRequest
	if aerr := decodeBody(r, &req); aerr != nil {
		return zero, aerr
	}
	if err := req.Validate(); err != nil {
		return zero, errBad("%v", err)
	}
	hash, err := runnertoken.HashBootstrap(tok)
	if err != nil {
		return zero, errUnauthorized("invalid bootstrap token")
	}
	sess, err := s.runner.GetRunSessionByBootstrapHash(ctx, hash)
	if errors.Is(err, core.ErrNotFound) {
		return zero, errUnauthorized("unknown bootstrap token")
	}
	if err != nil {
		return zero, s.errInternal("get session by bootstrap hash", err)
	}
	run, err := s.store.GetRun(ctx, sess.RunID)
	if errors.Is(err, core.ErrNotFound) {
		return zero, errUnauthorized("unknown run")
	}
	if err != nil {
		return zero, s.errInternal("get run", err)
	}
	if run.State == core.RunCancelled {
		return zero, &apiError{status: http.StatusGone, code: runnerproto.CodeRunCancelled, msg: "the run was cancelled"}
	}
	now := s.now()
	if sess.State == core.SessionEnded {
		return zero, errUnauthorized("session ended")
	}
	if sess.State == core.SessionAssigned && !sess.BootstrapExpiresAt.IsZero() && now.After(sess.BootstrapExpiresAt) {
		return zero, errUnauthorized("bootstrap token expired")
	}

	ih := runnertoken.HashInstanceID(req.InstanceID)
	first := false
	if sess.State == core.SessionAssigned {
		upd := *sess
		upd.State = core.SessionActive
		upd.InstanceHash = ih
		upd.StartedAt = &now
		upd.SandboxID = req.SandboxID
		upd.RunnerVersion = req.RunnerVersion
		switch err := s.runner.UpdateRunSession(ctx, &upd); {
		case err == nil:
			sess, first = &upd, true
		case errors.Is(err, core.ErrConflict):
			// A parallel start won. Read the session again and check it.
			if sess, err = s.runner.GetRunSession(ctx, sess.RunID, sess.Session); err != nil {
				return zero, s.errInternal("reload session", err)
			}
		default:
			return zero, s.errInternal("update session", err)
		}
	}
	if sess.State != core.SessionActive || !runnertoken.EqualHash(sess.InstanceHash, ih) {
		return zero, errUnauthorized("bootstrap token is bound to another instance")
	}
	if first {
		s.hooks.SessionStarted(ctx, sess)
	}

	resp, err := s.session.StartConfig(ctx, run, sess)
	if err != nil {
		if isConfigError(err) {
			return zero, s.failStart(ctx, run, sess, err)
		}
		return zero, s.errInternal("start config", err)
	}
	token, exp, aerr := s.issueToken(run, sess.Session)
	if aerr != nil {
		return zero, aerr
	}
	resp.RunID = run.ID
	resp.Session = sess.Session
	resp.SessionToken = token
	resp.SessionTokenExpiresAt = exp
	return resp, nil
}

// isConfigError reports whether a StartConfig error is a fault in the
// configuration of the run (core.ErrInvalid or core.ErrNotFound). A retry
// of the session start gives the same result.
func isConfigError(err error) bool {
	return errors.Is(err, core.ErrInvalid) || errors.Is(err, core.ErrNotFound)
}

// failStart ends a session whose start configuration has a configuration
// fault. The run fails with invalid_configuration and the error text, after
// the secret values of the run are removed from it. The answer is 422 with
// the code configuration_error, which the runner does not retry.
func (s *Service) failStart(ctx context.Context, run *core.Run, sess *core.RunSession, cause error) *apiError {
	msg := cause.Error()
	if s.redact != nil {
		if red := s.redact(ctx, run); red != nil {
			msg = red(msg)
		}
	}
	s.log.WarnContext(ctx, "runnerapi: start config: configuration error", "run", run.ID, "session", sess.Session, "err", msg)
	now := s.now()
	code := core.FailureInvalidConfiguration
	err := s.updateSession(ctx, sess, func(x *core.RunSession) {
		x.State = core.SessionEnded
		x.EndedAt = &now
		x.EndReason = string(runnerproto.EndSetupFailed)
		x.FailureCode = code
		x.ActiveTurnID = ""
	})
	if err != nil {
		return s.errInternal("end session", err)
	}
	if _, err := s.runner.ResetRunInputsOfSession(ctx, sess.RunID, sess.Session); err != nil {
		s.log.WarnContext(ctx, "runnerapi: reset inputs of session", "run", sess.RunID, "session", sess.Session, "err", err)
	}
	s.hooks.SessionEnded(ctx, sess, runnerproto.EndRequest{
		Reason:       runnerproto.EndSetupFailed,
		FailureCode:  string(code),
		Message:      msg,
		LastEventSeq: sess.EventAck,
	})
	s.wake.notify(sessionKey{sess.RunID, sess.Session})
	return &apiError{status: http.StatusUnprocessableEntity, code: runnerproto.CodeConfigurationError, msg: msg}
}
