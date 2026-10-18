package runnerapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	defaultCloudTokenTTL = 3600
	minCloudTokenTTL     = 60
	maxCloudTokenTTL     = 3600 // at most 1 hour
)

// CloudTokenSource makes the cloud identity token of a run. It
// returns an error that wraps core.ErrForbidden when the
// audience is not allowed, and core.ErrInvalid for a bad request.
type CloudTokenSource interface {
	CloudToken(ctx context.Context, run *core.Run, session *core.RunSession, audience string, ttl time.Duration) (string, time.Time, error)
}

func (s *Service) handleCloudToken(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	if s.cloudTokens == nil {
		writeError(w, &apiError{status: http.StatusNotImplemented, code: codeNotImpl, msg: "not implemented"})
		return
	}
	var req runnerproto.CloudTokenRequest
	if aerr := decodeBody(r, &req); aerr != nil {
		writeError(w, aerr)
		return
	}
	if req.Audience == "" {
		writeError(w, errBad("audience is required"))
		return
	}
	if req.TTLSeconds == 0 {
		req.TTLSeconds = defaultCloudTokenTTL
	}
	if req.TTLSeconds < minCloudTokenTTL || req.TTLSeconds > maxCloudTokenTTL {
		writeError(w, errBad("ttl_seconds must be from %d to %d", minCloudTokenTTL, maxCloudTokenTTL))
		return
	}
	run, err := s.store.GetRun(r.Context(), sess.RunID)
	if err != nil {
		writeError(w, errInternal("get run", err))
		return
	}
	tok, exp, err := s.cloudTokens.CloudToken(r.Context(), run, sess, req.Audience, time.Duration(req.TTLSeconds)*time.Second)
	switch {
	case errors.Is(err, core.ErrForbidden):
		writeError(w, &apiError{status: http.StatusForbidden, code: codeForbidden, msg: "no cloud token for this audience"})
	case errors.Is(err, core.ErrInvalid):
		writeError(w, errBad("invalid cloud token request"))
	case err != nil:
		writeError(w, errInternal("cloud token", err))
	default:
		writeJSON(w, http.StatusOK, runnerproto.CloudTokenResponse{Token: tok, ExpiresAt: exp})
	}
}
