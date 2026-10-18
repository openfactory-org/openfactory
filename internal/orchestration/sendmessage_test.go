package orchestration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func newMessageFixture(t *testing.T) (*fakeEngine, *Server, *httptest.Server) {
	t.Helper()
	st := &fakeStore{
		runs: map[string]*core.Run{
			"run_root":  {ID: "run_root", FactoryID: "fac", WorkItemID: "wi_1", AgentName: "foreman", State: core.RunSucceeded},
			"run_a":     {ID: "run_a", FactoryID: "fac", WorkItemID: "wi_1", ParentRunID: "run_root", AgentName: "implement", State: core.RunRunning},
			"run_b":     {ID: "run_b", FactoryID: "fac", WorkItemID: "wi_1", ParentRunID: "run_root", AgentName: "review", State: core.RunBlocked},
			"run_side":  {ID: "run_side", FactoryID: "fac", WorkItemID: "wi_1", AgentName: "watch", State: core.RunRunning},
			"run_dead":  {ID: "run_dead", FactoryID: "fac", WorkItemID: "wi_1", ParentRunID: "run_root", State: core.RunCancelled},
			"run_other": {ID: "run_other", FactoryID: "fac", WorkItemID: "wi_2", AgentName: "triage", State: core.RunRunning},
			"run_solo":  {ID: "run_solo", FactoryID: "fac", State: core.RunSucceeded},
			"run_kid":   {ID: "run_kid", FactoryID: "fac", ParentRunID: "run_solo", State: core.RunRunning},
			"run_lone":  {ID: "run_lone", FactoryID: "fac", State: core.RunRunning},
		},
		wi: &core.WorkItem{ID: "wi_1", FactoryID: "fac", Stage: core.StageBuilding, RootRunID: "run_root"},
	}
	eng := &fakeEngine{store: st}
	srv := newServer(eng, st)
	eng.now = func() time.Time { return srv.limiter.now() }
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return eng, srv, ts
}

func TestSendMessageAllowed(t *testing.T) {
	eng, _, ts := newMessageFixture(t)
	tests := []struct{ from, to, rel string }{
		{"run_a", "run_root", "parent"},
		{"run_a", "run_b", "sibling"},
		{"run_root", "run_a", "child"},
		{"run_side", "run_root", "work_item"},
		{"run_b", "run_side", "work_item"},
		{"run_kid", "run_solo", "parent"},
		{"run_solo", "run_kid", "child"},
	}
	for _, tt := range tests {
		out, isErr := callText(t, connect(t, ts, tt.from), "send_message", map[string]any{"run_id": tt.to, "text": "hello from " + tt.from})
		if isErr || !strings.Contains(out, tt.rel) {
			t.Fatalf("%s -> %s: %v %s", tt.from, tt.to, isErr, out)
		}
		c := eng.last()
		if c.Method != "FollowUp" || c.Args[0] != tt.to || c.Args[1] != "hello from "+tt.from || c.Args[2] != "run:"+tt.from {
			t.Fatalf("%s -> %s: call = %+v", tt.from, tt.to, c)
		}
	}
}

func TestSendMessageRefused(t *testing.T) {
	eng, _, ts := newMessageFixture(t)
	tests := []struct{ from, to, want string }{
		{"run_a", "run_other", "not in your work item"},
		{"run_other", "run_a", "not in your work item"},
		{"run_a", "run_dead", "not active"},
		{"run_a", "run_a", "own run"},
		{"run_a", "run_nope", "not found"},
		{"run_lone", "run_solo", "not in your work item"},
	}
	for _, tt := range tests {
		out, isErr := callText(t, connect(t, ts, tt.from), "send_message", map[string]any{"run_id": tt.to, "text": "hi"})
		if !isErr || !strings.Contains(out, tt.want) {
			t.Fatalf("%s -> %s: %v %s", tt.from, tt.to, isErr, out)
		}
	}
	if len(eng.calls) != 0 {
		t.Fatalf("calls = %+v", eng.calls)
	}
}

func TestSendMessageRateLimit(t *testing.T) {
	eng, srv, ts := newMessageFixture(t)
	now := time.Unix(1000, 0)
	srv.limiter.now = func() time.Time { return now }
	sess := connect(t, ts, "run_a")
	for i := range sendMessageLimit {
		if out, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_b", "text": fmt.Sprintf("ping %d", i)}); isErr {
			t.Fatalf("message %d: %s", i, out)
		}
	}
	out, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_b", "text": "one more"})
	if !isErr || !strings.Contains(out, "rate limit") {
		t.Fatalf("over the limit: %v %s", isErr, out)
	}
	// An other pair has its own count.
	if out, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_root", "text": "to parent"}); isErr {
		t.Fatalf("other pair: %s", out)
	}
	// After the window, the pair can send again.
	now = now.Add(sendMessageWindow + time.Second)
	if out, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_b", "text": "later"}); isErr {
		t.Fatalf("after window: %s", out)
	}
	if len(eng.calls) != sendMessageLimit+2 {
		t.Fatalf("calls = %d", len(eng.calls))
	}
}

// Two replicas of the server share one store. A caller whose requests go to
// both replicas still sends at most the limit to one target in one window.
func TestSendMessageRateLimitAcrossReplicas(t *testing.T) {
	eng, srvA, tsA := newMessageFixture(t)
	st := srvA.store
	srvB := newServer(eng, st)
	mux := http.NewServeMux()
	srvB.Register(mux)
	tsB := httptest.NewServer(mux)
	t.Cleanup(tsB.Close)
	now := time.Unix(1000, 0)
	srvA.limiter.now = func() time.Time { return now }
	srvB.limiter.now = func() time.Time { return now }
	sessA, sessB := connect(t, tsA, "run_a"), connect(t, tsB, "run_a")
	sent := 0
	for i := range 2 * sendMessageLimit {
		sess := sessA
		if i%2 == 1 {
			sess = sessB
		}
		if _, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_b", "text": fmt.Sprintf("ping %d", i)}); !isErr {
			sent++
		}
	}
	if sent > sendMessageLimit {
		t.Fatalf("run_a -> run_b delivered %d messages in one window, limit is %d", sent, sendMessageLimit)
	}
}

// A parent that is refused by the send_message limit cannot send more
// messages to the same child with follow_up_agent: the two share the count.
func TestFollowUpSharesRateLimit(t *testing.T) {
	eng, srv, ts := newMessageFixture(t)
	now := time.Unix(1000, 0)
	srv.limiter.now = func() time.Time { return now }
	sess := connect(t, ts, "run_root")
	for i := range sendMessageLimit {
		if out, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_a", "text": fmt.Sprintf("ping %d", i)}); isErr {
			t.Fatalf("message %d: %s", i, out)
		}
	}
	if out, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_a", "text": "over"}); !isErr {
		t.Fatalf("send_message over the limit was not refused: %s", out)
	}
	extra := 0
	for i := range 3 * sendMessageLimit {
		if _, isErr := callText(t, sess, "follow_up_agent", map[string]any{"run_id": "run_a", "message": fmt.Sprintf("more %d", i)}); !isErr {
			extra++
		}
	}
	if extra > 0 {
		t.Fatalf("after the limit, follow_up_agent delivered %d more messages run_root -> run_a in the same window (engine calls %d)", extra, len(eng.calls))
	}
}

// Failed deliveries do not use up the limit: after 10 engine errors nothing
// was delivered, so the pair can still send.
func TestFailedSendDoesNotUseLimit(t *testing.T) {
	eng, srv, ts := newMessageFixture(t)
	now := time.Unix(1000, 0)
	srv.limiter.now = func() time.Time { return now }
	sess := connect(t, ts, "run_a")
	eng.err = fmt.Errorf("runner unavailable")
	for i := range sendMessageLimit {
		callText(t, sess, "send_message", map[string]any{"run_id": "run_b", "text": fmt.Sprintf("ping %d", i)})
	}
	eng.err = nil
	if out, isErr := callText(t, sess, "send_message", map[string]any{"run_id": "run_b", "text": "now it works"}); isErr {
		t.Fatalf("0 messages delivered, but the next send is refused: %s", out)
	}
}

func TestListAgentsShowsActiveRuns(t *testing.T) {
	_, _, ts := newMessageFixture(t)
	out, isErr := callText(t, connect(t, ts, "run_a"), "list_agents", nil)
	if isErr {
		t.Fatal(out)
	}
	for _, want := range []string{`"run_root"`, `"run_b"`, `"run_side"`, `"parent"`, `"sibling"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	for _, bad := range []string{`"run_dead"`, `"run_other"`, `"run_id": "run_a"`} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %s in %s", bad, out)
		}
	}
}
