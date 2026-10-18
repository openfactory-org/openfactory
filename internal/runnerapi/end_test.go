package runnerapi

import (
	"context"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestEndGapThenOK(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	ids := e.deliver(1, "t1")
	send(t, c, msgs(t, 1, 2)...)

	err := c.End(context.Background(), runnerproto.EndRequest{Reason: runnerproto.EndIdle, LastEventSeq: 5})
	var se *runnerclient.StatusError
	if !errors.Is(err, runnerclient.ErrGap) || !errors.As(err, &se) || se.Ack == nil || *se.Ack != 2 {
		t.Fatalf("want gap with ack 2, got %v", err)
	}
	if e.session(1).State != core.SessionActive {
		t.Fatal("session must stay active after a gap")
	}

	req := runnerproto.EndRequest{Reason: runnerproto.EndFatal, LastEventSeq: 2, FailureCode: "harness_crash"}
	if err := c.End(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	s := e.session(1)
	if s.State != core.SessionEnded || s.EndedAt == nil || s.EndReason != "fatal" ||
		s.FailureCode != core.FailureAgentFailed || s.ActiveTurnID != "" {
		t.Fatalf("session %+v", s)
	}
	if m := e.input(ids[0]); m.State != core.RunInputPending {
		t.Fatalf("input %+v", m)
	}
	if len(e.hooks.ended) != 1 || e.hooks.ended[0].Reason != runnerproto.EndFatal {
		t.Fatalf("hook %+v", e.hooks.ended)
	}
	if _, err := c.Secrets(context.Background()); !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401 after end, got %v", err)
	}
	if err := c.End(context.Background(), req); !runnerclient.IsUnauthorized(err) {
		t.Fatalf("second end: want 401, got %v", err)
	}
}
