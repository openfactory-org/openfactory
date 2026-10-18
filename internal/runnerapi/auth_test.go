package runnerapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

func asStatus(err error, out **runnerclient.StatusError) bool { return errors.As(err, out) }

// A token of an ended session gets 401, also when a newer session exists.
func TestEndedSessionWinsOverSuperseded(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	old := c.Token()
	s := e.session(1)
	s.State = core.SessionEnded
	if err := e.mem.UpdateRunSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	e.newSession(2, time.Hour)

	// A token that was minted after the end behaves the same.
	tok, _, err := runnertoken.Mint(e.keys, runnertoken.Claims{Team: "team_1", Run: e.run.ID, Session: 1, Subject: "runner"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{old, tok} {
		c.SetToken(token)
		if _, err := c.Secrets(context.Background()); !runnerclient.IsUnauthorized(err) {
			t.Fatalf("want 401, got %v", err)
		}
	}
}

// fakeRunner returns an active session 1 and a latest session 2.
type fakeRunner struct {
	core.RunnerStore
	touched int
}

func (f *fakeRunner) GetRunSession(_ context.Context, runID string, n int) (*core.RunSession, error) {
	return &core.RunSession{RunID: runID, Session: n, State: core.SessionActive}, nil
}

func (f *fakeRunner) LatestRunSession(_ context.Context, runID string) (*core.RunSession, error) {
	return &core.RunSession{RunID: runID, Session: 2, State: core.SessionActive}, nil
}

func (f *fakeRunner) TouchRunSession(context.Context, string, int, time.Time) error {
	f.touched++
	return nil
}

func TestAuthSuperseded(t *testing.T) {
	e := newEnv(t, nil)
	fr := &fakeRunner{}
	svc, err := New(Config{Store: e.mem, Runner: fr, Keys: e.keys, Session: fakeConfigurer{}})
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := runnertoken.Mint(e.keys, runnertoken.Claims{Team: "team_1", Run: e.run.ID, Session: 1, Subject: "runner"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, runnerproto.PathSessionSecrets, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	_, _, aerr := svc.authSession(req)
	if aerr == nil || aerr.status != http.StatusConflict || aerr.code != runnerproto.CodeSessionSuperseded {
		t.Fatalf("want 409 session_superseded, got %v", aerr)
	}
	if fr.touched != 0 {
		t.Fatal("a superseded session must not be touched")
	}

	// The same through HTTP and the client.
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	c, err := runnerclient.New(runnerclient.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	c.SetToken(tok)
	if _, err := c.Secrets(context.Background()); !runnerclient.IsSuperseded(err) {
		t.Fatalf("want superseded, got %v", err)
	}
}

func TestAuthRejectsBadTokens(t *testing.T) {
	e := newEnv(t, nil)
	c := e.client()
	for _, tok := range []string{"", "garbage", "ofs_abc.def", e.boot} {
		c.SetToken(tok)
		if _, err := c.Secrets(context.Background()); !runnerclient.IsUnauthorized(err) {
			t.Errorf("token %q: want 401, got %v", tok, err)
		}
	}
}
