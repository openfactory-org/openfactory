package runauth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

var testNow = time.Unix(1794400000, 0)

type fakeRunner struct {
	core.RunnerStore
	state    core.SessionState
	latest   int
	touched  int
	getErr   error
	latestEr error
}

func (f *fakeRunner) GetRunSession(_ context.Context, runID string, n int) (*core.RunSession, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return &core.RunSession{RunID: runID, Session: n, State: f.state}, nil
}

func (f *fakeRunner) LatestRunSession(_ context.Context, runID string) (*core.RunSession, error) {
	if f.latestEr != nil {
		return nil, f.latestEr
	}
	return &core.RunSession{RunID: runID, Session: f.latest}, nil
}

func (f *fakeRunner) TouchRunSession(context.Context, string, int, time.Time) error {
	f.touched++
	return nil
}

func keys(t *testing.T) *runnertoken.StaticKeys {
	t.Helper()
	k, err := runnertoken.NewStaticKeys("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mint(t *testing.T, k runnertoken.KeySet, run string, session int, at time.Time) string {
	t.Helper()
	tok, _, err := runnertoken.Mint(k, runnertoken.Claims{Team: "t", Run: run, Session: session, Subject: "runner"}, at)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func newVerifier(t *testing.T, fr *fakeRunner) *Verifier {
	return &Verifier{
		Keys:   keys(t),
		Runner: fr,
		Now:    func() time.Time { return testNow },
	}
}

// TestOtherTokensRefused checks that a token that is not a session token is
// refused, for example an HMAC of the run ID or a token with another prefix.
func TestOtherTokensRefused(t *testing.T) {
	v := newVerifier(t, &fakeRunner{})
	ctx := context.Background()
	m := hmac.New(sha256.New, []byte("server-key"))
	m.Write([]byte("run:run_1"))
	old := hex.EncodeToString(m.Sum(nil))
	if err := v.Verify(ctx, "run_1", old); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("hmac token: %v", err)
	}
	if err := v.Verify(ctx, "run_1", "ofr_eyJ2IjoxfQ.c2ln"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("prefixed token: %v", err)
	}
	if err := v.Verify(ctx, "run_1", "junk"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("junk: %v", err)
	}
	if err := v.Verify(ctx, "run_1", ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty: %v", err)
	}
}

func TestSessionToken(t *testing.T) {
	fr := &fakeRunner{state: core.SessionActive, latest: 1}
	v := newVerifier(t, fr)
	tok := mint(t, v.Keys, "run_1", 1, testNow)
	if err := v.Verify(context.Background(), "run_1", tok); err != nil {
		t.Fatalf("good: %v", err)
	}
	if fr.touched != 1 {
		t.Fatalf("touched = %d", fr.touched)
	}
}

func TestSessionTokenWrongRun(t *testing.T) {
	v := newVerifier(t, &fakeRunner{state: core.SessionActive, latest: 1})
	tok := mint(t, v.Keys, "run_1", 1, testNow)
	if err := v.Verify(context.Background(), "run_2", tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionTokenExpired(t *testing.T) {
	v := newVerifier(t, &fakeRunner{state: core.SessionActive, latest: 1})
	tok := mint(t, v.Keys, "run_1", 1, testNow.Add(-2*time.Hour))
	if err := v.Verify(context.Background(), "run_1", tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionTokenEnded(t *testing.T) {
	v := newVerifier(t, &fakeRunner{state: core.SessionEnded, latest: 1})
	tok := mint(t, v.Keys, "run_1", 1, testNow)
	if err := v.Verify(context.Background(), "run_1", tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionTokenUnknownSession(t *testing.T) {
	v := newVerifier(t, &fakeRunner{getErr: core.ErrNotFound})
	tok := mint(t, v.Keys, "run_1", 1, testNow)
	if err := v.Verify(context.Background(), "run_1", tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionTokenSuperseded(t *testing.T) {
	v := newVerifier(t, &fakeRunner{state: core.SessionActive, latest: 2})
	tok := mint(t, v.Keys, "run_1", 1, testNow)
	if err := v.Verify(context.Background(), "run_1", tok); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("got %v", err)
	}
}

func TestSessionTokenStoreFailure(t *testing.T) {
	boom := errors.New("boom")
	v := newVerifier(t, &fakeRunner{getErr: boom})
	tok := mint(t, v.Keys, "run_1", 1, testNow)
	err := v.Verify(context.Background(), "run_1", tok)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	rec := httptest.NewRecorder()
	WriteError(rec, err)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestKeysNilRefusesSession(t *testing.T) {
	v := newVerifier(t, &fakeRunner{state: core.SessionActive, latest: 1})
	tok := mint(t, v.Keys, "run_1", 1, testNow)
	v.Keys = nil
	if err := v.Verify(context.Background(), "run_1", tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("got %v", err)
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, ErrUnauthorized)
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("401: %d %v", rec.Code, rec.Header())
	}
	rec = httptest.NewRecorder()
	WriteError(rec, ErrSuperseded)
	var body struct{ Error struct{ Code string } }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 409 || body.Error.Code != "session_superseded" {
		t.Fatalf("409: %d %s", rec.Code, rec.Body)
	}
}
