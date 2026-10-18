package server

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/auth"
	"github.com/openfactory-org/openfactory/internal/core"
)

type auditCapture struct {
	mu     sync.Mutex
	events []auth.AuditEvent
}

func (a *auditCapture) Record(_ context.Context, e auth.AuditEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
}

// TestMergeAudit checks that each merge call of an agent is an audit event:
// a refused call has the outcome failure, an allowed one success.
func TestMergeAudit(t *testing.T) {
	sink := &auditCapture{}
	rec := mergeAudit(sink, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run := &core.Run{ID: "run_1", TeamID: "team_1", FactoryID: "fac_1", AgentName: "implement"}
	rec(context.Background(), run, "forge", "merge_pull_request", false)
	rec(context.Background(), run, "forge", "merge_pull_request", true)
	if len(sink.events) != 2 {
		t.Fatalf("events = %d", len(sink.events))
	}
	refused, allowed := sink.events[0], sink.events[1]
	if refused.Action != "agent.merge" || refused.Outcome != auth.OutcomeFailure || refused.ActorID != "run:run_1" || refused.TeamID != "team_1" {
		t.Fatalf("refused = %+v", refused)
	}
	if refused.Detail["tool"] != "merge_pull_request" || refused.Detail["mcp_server"] != "forge" || refused.Detail["factory_id"] != "fac_1" {
		t.Fatalf("detail = %+v", refused.Detail)
	}
	if allowed.Outcome != auth.OutcomeSuccess {
		t.Fatalf("allowed = %+v", allowed)
	}
}
