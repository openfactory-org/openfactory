package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

// The session token of a runner gets 401 on each path that takes it
// when its session ended, and a session that started later does not accept the
// token of an earlier one.
func TestSessionTokenOnTheMCPPaths(t *testing.T) {
	ctx := t.Context()
	srv, err := New(ctx, testConfig(t), WithLogger(quietLogger()), WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	// The server is not started: the dispatcher must not touch the sessions
	// that the test makes by hand.
	defer srv.Close()

	rs := srv.Store.(core.RunnerStore)
	run := &core.Run{ID: "run_sess", TeamID: "team1", FactoryID: "fac_test", State: core.RunRunning, CreatedAt: time.Now()}
	if err := srv.Store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	newSession := func(n int) {
		t.Helper()
		s := &core.RunSession{RunID: run.ID, Session: n, TaskID: "task_" + run.ID, State: core.SessionActive, AssignedAt: time.Now()}
		if err := rs.CreateRunSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	endSession := func(n int) {
		t.Helper()
		s, err := rs.GetRunSession(ctx, run.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		s.State = core.SessionEnded
		if err := rs.UpdateRunSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	token := func(n int) string {
		t.Helper()
		tok, _, err := runnertoken.Mint(srv.runner.keys, runnertoken.Claims{Team: "team1", Run: run.ID, Session: n, Subject: "runner"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	paths := []string{
		"/mcp/run/" + run.ID,
		"/mcp/proxy/" + run.ID + "/srv_1",
		"/mcp/integrations/github/" + run.ID,
	}
	status := func(path, tok string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	want := func(what, tok string, ok bool) {
		t.Helper()
		for _, p := range paths {
			got := status(p, tok)
			if refused := got == http.StatusUnauthorized || got == http.StatusConflict; refused == ok {
				t.Errorf("%s on %s: status %d, accepted=%v", what, p, got, !refused)
			}
		}
	}

	newSession(1)
	tok1 := token(1)
	want("the token of an active session", tok1, true)

	// The session ends: its token gets 401 on each path, also while the run is not finished.
	endSession(1)
	for _, p := range paths {
		if got := status(p, tok1); got != http.StatusUnauthorized {
			t.Errorf("token of an ended session on %s: status %d, want 401", p, got)
		}
	}

	// A follow-up starts session 2: the token of session 1 stays refused, the new token works.
	newSession(2)
	want("the token of the new session", token(2), true)
	for _, p := range paths {
		if got := status(p, tok1); got != http.StatusUnauthorized {
			t.Errorf("token of the old session on %s: status %d, want 401", p, got)
		}
	}
}
