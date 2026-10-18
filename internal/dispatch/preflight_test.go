package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestPreflightFailsRunBeforeTask checks that a preflight error with a
// failure code fails the run before a task starts, and that another error
// leaves the run queued.
func TestPreflightFailsRunBeforeTask(t *testing.T) {
	e := newEnv(t)
	var result error
	d, err := New(Config{
		Store: e.mem, Runner: e.mem, Backend: e.backend, ControlURL: "http://control.test", Image: "img",
		Now:       func() time.Time { return *e.clock },
		Preflight: func(context.Context, *core.Run) error { return result },
	})
	if err != nil {
		t.Fatal(err)
	}
	d.SetCommander(e.cmd)
	e.d = d

	result = errors.New("store unavailable")
	r := e.newRun(core.RunQueued, "go")
	_ = e.d.Tick(e.ctx)
	if got := e.run(r.ID); got.State != core.RunQueued || len(e.backend.started) != 0 {
		t.Fatalf("after a plain error: state %s, tasks %d", got.State, len(e.backend.started))
	}

	result = &core.FailureCodeError{Code: core.FailureIntegrationDisabled, Err: core.ErrIntegrationDisabled}
	e.tick()
	got := e.run(r.ID)
	if got.State != core.RunFailed || got.FailureCode != core.FailureIntegrationDisabled {
		t.Fatalf("state %s, code %q", got.State, got.FailureCode)
	}
	if len(e.backend.started) != 0 {
		t.Fatal("a task started")
	}
}
