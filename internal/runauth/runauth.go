// Package runauth checks the bearer token of the run paths of the control
// plane (/mcp/...). A path accepts the session token of the active session of
// the run. The runner sends it. A path refuses each other token.
package runauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

var (
	// ErrUnauthorized means the token is missing or not valid for the run.
	ErrUnauthorized = errors.New("runauth: unauthorized")
	// ErrSuperseded means a newer session of the run exists.
	ErrSuperseded = errors.New("runauth: session superseded")
)

// Verifier checks the bearer token of a run path.
type Verifier struct {
	Keys   runnertoken.KeySet // nil disables session tokens
	Runner core.RunnerStore   // needed with Keys
	Now    func() time.Time   // nil means time.Now
}

// Verify checks token for runID. It returns nil, ErrUnauthorized or
// ErrSuperseded. Another error is a store failure.
func (v *Verifier) Verify(ctx context.Context, runID, token string) error {
	token = strings.TrimSpace(token)
	if token == "" || runID == "" {
		return ErrUnauthorized
	}
	if strings.HasPrefix(token, runnertoken.SessionPrefix) {
		return v.verifySession(ctx, runID, token)
	}
	return ErrUnauthorized
}

func (v *Verifier) verifySession(ctx context.Context, runID, token string) error {
	if v.Keys == nil || v.Runner == nil {
		return ErrUnauthorized
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	claims, err := runnertoken.Verify(v.Keys, token, now())
	if err != nil || claims.Run != runID {
		return ErrUnauthorized
	}
	sess, err := v.Runner.GetRunSession(ctx, runID, claims.Session)
	if errors.Is(err, core.ErrNotFound) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if sess.State == core.SessionEnded {
		return ErrUnauthorized
	}
	latest, err := v.Runner.LatestRunSession(ctx, runID)
	if errors.Is(err, core.ErrNotFound) {
		return ErrUnauthorized
	}
	if err != nil {
		return err
	}
	if latest.Session > claims.Session {
		return ErrSuperseded
	}
	if err := v.Runner.TouchRunSession(ctx, runID, claims.Session, now()); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return ErrUnauthorized
		}
		return err
	}
	return nil
}

// WriteError writes the HTTP answer for an error of Verify.
func WriteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", `Bearer realm="openfactory-run"`)
		write(w, http.StatusUnauthorized, "unauthorized", "invalid or missing run token")
	case errors.Is(err, ErrSuperseded):
		write(w, http.StatusConflict, "session_superseded", "a newer session exists")
	default:
		write(w, http.StatusInternalServerError, "internal", "could not check the token")
	}
}

func write(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
