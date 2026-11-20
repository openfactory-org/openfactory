package runnerapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// errConfigurer is a SessionConfigurer that fails with err.
type errConfigurer struct{ err error }

func (c errConfigurer) StartConfig(context.Context, *core.Run, *core.RunSession) (runnerproto.StartResponse, error) {
	return runnerproto.StartResponse{}, c.err
}

// TestStartConfigError checks the answer of session start when StartConfig
// fails. A configuration error ends the session at once, fails the run with
// invalid_configuration and gives a 422 that the runner does not retry. Any
// other error gives a 500 that the runner retries.
func TestStartConfigError(t *testing.T) {
	const secret = "fake-secret-value-0123"
	noForge := fmt.Errorf("session plan: repository acme/api: forge: team team_1, repository acme/api: "+
		"no github integration record for the owner %q: add a record with config.org %q: %w", "acme", "acme", forgeutil.ErrNoIntegration)
	tests := []struct {
		name      string
		err       error
		status    int
		code      string
		retryable bool
		wantText  string // in the message to the runner and in the end request
	}{
		{
			name:     "no code-forge integration",
			err:      noForge,
			status:   http.StatusUnprocessableEntity,
			code:     runnerproto.CodeConfigurationError,
			wantText: `no github integration record for the owner "acme"`,
		},
		{
			name:     "invalid with a secret value",
			err:      fmt.Errorf("%w: machine %q has the token %s", core.ErrInvalid, "big", secret),
			status:   http.StatusUnprocessableEntity,
			code:     runnerproto.CodeConfigurationError,
			wantText: `machine "big" has the token [REDACTED]`,
		},
		{
			name:     "not found",
			err:      fmt.Errorf("session plan: environment env_1: %w", core.ErrNotFound),
			status:   http.StatusUnprocessableEntity,
			code:     runnerproto.CodeConfigurationError,
			wantText: "environment env_1: not found",
		},
		{
			name:      "unexpected",
			err:       errors.New("database is down"),
			status:    http.StatusInternalServerError,
			code:      codeInternal,
			retryable: true,
			wantText:  "internal error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			e := newEnv(t, func(c *Config) {
				c.Session = errConfigurer{err: tt.err}
				c.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				c.Redact = func(context.Context, *core.Run) func(string) string {
					return func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }
				}
			})
			_, err := e.client().Start(context.Background(), e.boot, startReq("inst-1"))
			var se *runnerclient.StatusError
			if !errors.As(err, &se) {
				t.Fatalf("want a status error, got %v", err)
			}
			if se.Status != tt.status || se.Code != tt.code {
				t.Fatalf("status %d code %q, want %d %q", se.Status, se.Code, tt.status, tt.code)
			}
			if got := runnerclient.IsRetryable(err); got != tt.retryable {
				t.Fatalf("IsRetryable = %v, want %v", got, tt.retryable)
			}
			if !strings.Contains(se.Message, tt.wantText) || strings.Contains(se.Message, secret) {
				t.Fatalf("message %q, want it to contain %q and no secret", se.Message, tt.wantText)
			}
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("log has the secret value: %s", logs.String())
			}
			if !strings.HasPrefix(strings.TrimSpace(logs.String()), "{") {
				t.Fatalf("log is not JSON from the injected logger: %q", logs.String())
			}
			s := e.session(1)
			if !tt.retryable {
				if s.State != core.SessionEnded || s.FailureCode != core.FailureInvalidConfiguration ||
					s.EndReason != string(runnerproto.EndSetupFailed) || s.EndedAt == nil {
					t.Fatalf("bad session: %+v", s)
				}
				if len(e.hooks.ended) != 1 {
					t.Fatalf("SessionEnded called %d times", len(e.hooks.ended))
				}
				req := e.hooks.ended[0]
				if req.Reason != runnerproto.EndSetupFailed || req.FailureCode != string(core.FailureInvalidConfiguration) ||
					!strings.Contains(req.Message, tt.wantText) || strings.Contains(req.Message, secret) {
					t.Fatalf("bad end request: %+v", req)
				}
				if err := req.Validate(); err != nil {
					t.Fatalf("end request is not valid: %v", err)
				}
				// A retry of the runner finds the session ended.
				if _, err := e.client().Start(context.Background(), e.boot, startReq("inst-1")); !runnerclient.IsUnauthorized(err) {
					t.Fatalf("second start: want 401, got %v", err)
				}
				return
			}
			if s.State != core.SessionActive || len(e.hooks.ended) != 0 {
				t.Fatalf("unexpected error ended the session: %+v, ended %d", s, len(e.hooks.ended))
			}
			if !strings.Contains(logs.String(), "runnerapi: start config") || !strings.Contains(logs.String(), "database is down") {
				t.Fatalf("log misses the error: %s", logs.String())
			}
		})
	}
}
