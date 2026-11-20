package runnerapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// GitCredentialProvider returns the git credential for a repository of a run.
// It returns an error that wraps core.ErrForbidden when the run has no such
// repository.
type GitCredentialProvider interface {
	GitCredential(ctx context.Context, run *core.Run, s *core.RunSession, req runnerproto.GitCredentialRequest) (runnerproto.GitCredentialResponse, error)
}

func (s *Service) handleGitCredential(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if s.gitCreds == nil {
		s.writeError(w, &apiError{status: http.StatusNotImplemented, code: codeNotImpl, msg: "not implemented"})
		return
	}
	var req runnerproto.GitCredentialRequest
	if aerr := decodeBody(r, &req); aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if req.Protocol != "https" {
		s.writeError(w, errBad("protocol must be https"))
		return
	}
	if req.Host == "" || req.Path == "" {
		s.writeError(w, errBad("host and path are required"))
		return
	}
	run, err := s.store.GetRun(r.Context(), sess.RunID)
	if err != nil {
		s.writeError(w, s.errInternal("get run", err))
		return
	}
	resp, err := s.gitCreds.GitCredential(r.Context(), run, sess, req)
	switch {
	case errors.Is(err, core.ErrForbidden):
		s.writeError(w, &apiError{status: http.StatusForbidden, code: codeForbidden, msg: "no credential for this repository"})
	case err != nil:
		s.writeError(w, s.errInternal("git credential", err))
	default:
		s.writeJSON(w, http.StatusOK, resp)
	}
}
