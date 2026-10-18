package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// runnerEnv returns a test environment that uses the test store as the
// runner store too.
func runnerEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t, func(o *Options) { o.Runner = o.Store.(core.RunnerStore) })
	e.engine.Sessions = e.store
	return e
}

// runnerEvent is an event of a session for addSession.
type runnerEvent struct {
	typ, turnID string
	at          time.Time
	data        string
}

// addSession stores a session through the runner store. It creates the
// session, appends the events in order (Seq 1, 2, ...), and then writes the
// state and the other fields of s. The older sessions of the run must be
// ended.
func (e *testEnv) addSession(s core.RunSession, events ...runnerEvent) {
	e.t.Helper()
	ctx := context.Background()
	created := s
	if err := e.store.CreateRunSession(ctx, &created); err != nil {
		e.t.Fatal(err)
	}
	if len(events) > 0 {
		batch := make([]core.RunnerEvent, len(events))
		for i, ev := range events {
			batch[i] = core.RunnerEvent{Seq: int64(i + 1), Type: ev.typ, TurnID: ev.turnID, At: ev.at}
			if ev.data != "" {
				batch[i].Data = []byte(ev.data)
			}
		}
		if _, err := e.store.AppendRunnerEvents(ctx, s.RunID, s.Session, batch); err != nil {
			e.t.Fatal(err)
		}
	}
	stored, err := e.store.GetRunSession(ctx, s.RunID, s.Session)
	if err != nil {
		e.t.Fatal(err)
	}
	s.Version, s.EventAck, s.CommandSeq, s.CommandAck, s.LastSeenAt = stored.Version, stored.EventAck, stored.CommandSeq, stored.CommandAck, stored.LastSeenAt
	if s.AssignedAt.IsZero() {
		s.AssignedAt = stored.AssignedAt
	}
	if err := e.store.UpdateRunSession(ctx, &s); err != nil {
		e.t.Fatal(err)
	}
}

// addInput adds a message to the queue of a run through the runner store and
// then moves it to m.State ("pending" or "consumed").
func (e *testEnv) addInput(m core.RunInput) {
	e.t.Helper()
	ctx := context.Background()
	state := m.State
	if _, err := e.store.AppendRunInput(ctx, &m); err != nil {
		e.t.Fatal(err)
	}
	switch state {
	case "", core.RunInputPending:
	case core.RunInputConsumed:
		turn := "turn_" + m.ID
		if n, err := e.store.MarkRunInputsDelivered(ctx, m.RunID, []string{m.ID}, 1, turn); err != nil || n != 1 {
			e.t.Fatalf("deliver %s: %d %v", m.ID, n, err)
		}
		if n, err := e.store.MarkRunInputsAccepted(ctx, m.RunID, []string{m.ID}); err != nil || n != 1 {
			e.t.Fatalf("accept %s: %d %v", m.ID, n, err)
		}
		if n, _, err := e.store.FinishRunInputsOfTurn(ctx, m.RunID, turn); err != nil || n != 1 {
			e.t.Fatalf("consume %s: %d %v", m.ID, n, err)
		}
	default:
		e.t.Fatalf("addInput: state %q is not supported", state)
	}
}

// seedRun stores a run of the test team.
func (e *testEnv) seedRun(id, parent string, state core.RunState) *core.Run {
	e.t.Helper()
	r := &core.Run{ID: id, TeamID: e.team.ID, FactoryID: e.runFactory(), ParentRunID: parent, Name: id, State: state, Prompt: "task " + id,
		CreatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := e.store.CreateRun(context.Background(), r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func idsOf(r resp, key string) []string {
	var out []string
	for _, x := range list(r.Body, key) {
		m, _ := x.(map[string]any)
		out = append(out, str(m, "run_id"))
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]bool{}
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[w] {
			return false
		}
	}
	return true
}

func TestExternalRunStateMapping(t *testing.T) {
	for st, want := range map[core.RunState]string{
		core.RunQueued: "QUEUED", core.RunPending: "PENDING", core.RunStarting: "PENDING", core.RunClaimed: "CLAIMED",
		core.RunRunning: "RUNNING", core.RunBlocked: "BLOCKED", core.RunIdle: "SUCCEEDED", core.RunSucceeded: "SUCCEEDED",
		core.RunFailed: "FAILED", core.RunError: "ERROR", core.RunCancelled: "CANCELLED",
	} {
		if got := ExternalRunState(st); got != want {
			t.Errorf("ExternalRunState(%s) = %s, want %s", st, got, want)
		}
	}
	filters := map[string][]core.RunState{
		"RUNNING":   {core.RunRunning},
		"PENDING":   {core.RunPending, core.RunStarting},
		"SUCCEEDED": {core.RunSucceeded, core.RunIdle},
		"ERROR":     {core.RunError},
		"BLOCKED":   {core.RunBlocked},
		"CLAIMED":   {core.RunClaimed},
		"CANCELED":  {core.RunCancelled},
	}
	for v, want := range filters {
		got, ok := internalRunStates(v)
		if !ok || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("internalRunStates(%s) = %v, %v; want %v", v, got, ok, want)
		}
	}
	for _, v := range []string{"nope", "INPROGRESS", "IN_PROGRESS", "IDLE"} {
		if _, ok := internalRunStates(v); ok {
			t.Errorf("state %s accepted", v)
		}
	}
}

func TestRunViewFields(t *testing.T) {
	e := runnerEnv(t)
	ctx := context.Background()
	root := e.seedRun("run_root", "", core.RunRunning)
	child := e.seedRun("run_child", root.ID, core.RunRunning)
	e.seedRun("run_grand", child.ID, core.RunQueued)
	e.seedRun("run_done", root.ID, core.RunSucceeded)
	e.seedRun("run_idle", root.ID, core.RunIdle)
	e.addSession(core.RunSession{RunID: root.ID, Session: 1, State: core.SessionEnded})
	e.addSession(core.RunSession{RunID: root.ID, Session: 2, State: core.SessionActive})

	g := e.must(e.do("GET", "/runs/"+root.ID, e.viewer.Key, nil), 200)
	b := g.Body
	if b["state"] != "RUNNING" || b["phase"] != "running" || b["root_run_id"] != root.ID || b["depth"] != float64(0) {
		t.Fatalf("root view = %s", g.Raw)
	}
	// run_child and run_grand are not finished. run_idle waits for input and counts as finished.
	if b["active_child_runs"] != float64(2) || b["session"] != float64(2) || b["sandbox_running"] != true {
		t.Fatalf("root view = %s", g.Raw)
	}
	if b["url"] != "https://factory.example.test/runs/"+root.ID || b["session_link"] != nil || b["triggered_by"] != nil || b["source"] != "API" || b["url"] != b["run_url"] {
		t.Fatalf("urls = %s", g.Raw)
	}
	if str(b, "usage", "coverage") != "known" {
		t.Fatalf("usage = %v", b["usage"])
	}

	gc := e.must(e.do("GET", "/runs/run_grand", e.viewer.Key, nil), 200)
	if gc.Body["root_run_id"] != root.ID || gc.Body["depth"] != float64(2) || gc.Body["parent_run_id"] != child.ID ||
		gc.Body["session"] != float64(0) || gc.Body["sandbox_running"] != false {
		t.Fatalf("grandchild view = %s", gc.Raw)
	}

	// A finished run: the idle row shows as SUCCEEDED.
	if i := e.must(e.do("GET", "/runs/run_idle", e.viewer.Key, nil), 200); i.Body["state"] != "SUCCEEDED" || i.Body["phase"] != "idle" {
		t.Fatalf("idle view = %s", i.Raw)
	}

	// Failure: the status object has the message, the code and the retry flag.
	f := e.seedRun("run_fail", "", core.RunError)
	f.Error, f.FailureCode = "sandbox disappeared", core.FailureSandboxLost
	_ = e.store.UpdateRun(ctx, f)
	fv := e.must(e.do("GET", "/runs/run_fail", e.viewer.Key, nil), 200)
	if fv.Body["state"] != "ERROR" || fv.Body["error"] != "sandbox disappeared" ||
		str(fv.Body, "status_message", "message") != "sandbox disappeared" ||
		str(fv.Body, "status_message", "failure_code") != "sandbox_lost" ||
		fv.Body["status_message"].(map[string]any)["retryable"] != true ||
		!strings.Contains(str(fv.Body, "status_message", "detail"), "platform") {
		t.Fatalf("failed view = %s", fv.Raw)
	}
	nr := e.seedRun("run_nr", "", core.RunFailed)
	nr.Error, nr.FailureCode = "bad", core.FailureAgentFailed
	_ = e.store.UpdateRun(ctx, nr)
	if v := e.must(e.do("GET", "/runs/run_nr", e.viewer.Key, nil), 200); v.Body["status_message"].(map[string]any)["retryable"] != false {
		t.Fatalf("view = %s", v.Raw)
	}

	// Blocked: blocked_reason is set.
	bl := e.seedRun("run_blocked", "", core.RunBlocked)
	bl.BlockedReason = "human_request"
	_ = e.store.UpdateRun(ctx, bl)
	bv := e.must(e.do("GET", "/runs/run_blocked", e.viewer.Key, nil), 200)
	if bv.Body["state"] != "BLOCKED" || bv.Body["blocked_reason"] != "human_request" ||
		!strings.Contains(str(bv.Body, "status_message", "detail"), "human_request") {
		t.Fatalf("blocked view = %s", bv.Raw)
	}

	// A run without any status text has no status_message.
	if q := e.must(e.do("GET", "/runs/run_done", e.viewer.Key, nil), 200); q.Body["status_message"] != nil {
		t.Fatalf("status_message = %v", q.Body["status_message"])
	}
}

func TestRunViewWithoutRunnerStore(t *testing.T) {
	e := newTestEnv(t)
	e.seedRun("run_a", "", core.RunRunning)
	g := e.must(e.do("GET", "/runs/run_a", e.viewer.Key, nil), 200)
	if g.Body["sandbox_running"] != false || g.Body["session"] != float64(0) {
		t.Fatalf("view = %s", g.Raw)
	}
	for _, p := range []string{"inputs", "timeline", "diagnostics", "sessions"} {
		r := e.must(e.do("GET", "/runs/run_a/"+p, e.viewer.Key, nil), 501)
		if errCode(r) != CodeFeatureNotAvailable {
			t.Errorf("%s: code = %s", p, errCode(r))
		}
	}
	// The conversation needs no runner store.
	e.must(e.do("GET", "/runs/run_a/conversation", e.viewer.Key, nil), 200)
}

func TestRunViewDepthIsBounded(t *testing.T) {
	e := newTestEnv(t)
	parent := ""
	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("run_d%02d", i)
		e.seedRun(id, parent, core.RunRunning)
		parent = id
	}
	g := e.must(e.do("GET", "/runs/run_d14", e.viewer.Key, nil), 200)
	if g.Body["depth"] != float64(maxRunTreeDepth) || g.Body["root_run_id"] != "run_d04" {
		t.Fatalf("view = %s", g.Raw)
	}
	top := e.must(e.do("GET", "/runs/run_d00", e.viewer.Key, nil), 200)
	if top.Body["active_child_runs"] != float64(maxRunTreeDepth) {
		t.Fatalf("active_child_runs = %v", top.Body["active_child_runs"])
	}
}

func TestListRunsStateAndTreeFilters(t *testing.T) {
	e := newTestEnv(t)
	root := e.seedRun("run_root", "", core.RunRunning)
	c1 := e.seedRun("run_c1", root.ID, core.RunRunning)
	e.seedRun("run_c2", root.ID, core.RunIdle)
	e.seedRun("run_g1", c1.ID, core.RunFailed)
	e.seedRun("run_other", "", core.RunPending)
	e.seedRun("run_starting", "", core.RunStarting)

	get := func(q string) resp { return e.must(e.do("GET", "/runs"+q, e.viewer.Key, nil), 200) }
	if got := idsOf(get("?root_run_id=run_root"), "runs"); !sameSet(got, "run_root", "run_c1", "run_c2", "run_g1") {
		t.Errorf("root_run_id = %v", got)
	}
	if got := idsOf(get("?ancestor_run_id=run_root"), "runs"); !sameSet(got, "run_c1", "run_c2", "run_g1") {
		t.Errorf("ancestor_run_id = %v", got)
	}
	if got := idsOf(get("?ancestor_run_id=run_root&state=FAILED"), "runs"); !sameSet(got, "run_g1") {
		t.Errorf("ancestor and state = %v", got)
	}
	if got := idsOf(get("?root_run_id=run_c1"), "runs"); !sameSet(got, "run_c1", "run_g1") {
		t.Errorf("root c1 = %v", got)
	}
	if got := idsOf(get("?state=RUNNING"), "runs"); !sameSet(got, "run_root", "run_c1") {
		t.Errorf("RUNNING = %v", got)
	}
	if got := idsOf(get("?state=PENDING"), "runs"); !sameSet(got, "run_other", "run_starting") {
		t.Errorf("PENDING = %v", got)
	}
	if got := idsOf(get("?state=SUCCEEDED"), "runs"); !sameSet(got, "run_c2") {
		t.Errorf("SUCCEEDED = %v", got)
	}
	// Paging inside a tree.
	p1 := get("?root_run_id=run_root&limit=3")
	if len(list(p1.Body, "runs")) != 3 || nextCursor(p1.Body) == nil {
		t.Fatalf("page 1 = %s", p1.Raw)
	}
	p2 := get("?root_run_id=run_root&limit=3&cursor=" + nextCursor(p1.Body).(string))
	if len(list(p2.Body, "runs")) != 1 || nextCursor(p2.Body) != nil {
		t.Fatalf("page 2 = %s", p2.Raw)
	}
	// The list view has the new fields too.
	for _, x := range list(get("?root_run_id=run_root").Body, "runs") {
		m := x.(map[string]any)
		if m["run_id"] == "run_g1" && (m["depth"] != float64(2) || m["root_run_id"] != "run_root") {
			t.Errorf("list view = %v", m)
		}
		if m["run_id"] == "run_root" && m["active_child_runs"] != float64(1) {
			t.Errorf("root list view = %v", m)
		}
	}
	e.must(e.do("GET", "/runs?state=NOPE", e.viewer.Key, nil), 400)
	e.must(e.do("GET", "/runs?root_run_id=run_nope", e.viewer.Key, nil), 404)
	e.must(e.do("GET", "/runs?root_run_id=run_root", e.other.Key, nil), 404)
	e.must(e.do("GET", "/runs?root_run_id=run_root&ancestor_run_id=run_c1", e.viewer.Key, nil), 400)
}

func TestPostRunMessage(t *testing.T) {
	e := runnerEnv(t)
	ctx := context.Background()
	post := func(id, msg string) resp {
		return e.do("POST", "/runs/"+id+"/messages", e.viewer.Key, map[string]any{"message": msg})
	}

	// Live session.
	e.seedRun("run_live", "", core.RunRunning)
	e.addSession(core.RunSession{RunID: "run_live", Session: 1, State: core.SessionActive})
	r := e.must(post("run_live", "hello live"), 202)
	if r.Body["outcome"] != OutcomeLiveSession || !strings.HasPrefix(str(r.Body, "message_id"), "msg_") {
		t.Fatalf("live = %s", r.Raw)
	}
	msgs, _ := e.store.ListRunMessages(ctx, "run_live", 0)
	if len(msgs) != 1 || msgs[0].ID != r.Body["message_id"] {
		t.Fatalf("message id %v does not match %+v", r.Body["message_id"], msgs)
	}

	// The record is the one that the engine returns: the API does not guess.
	e.seedRun("run_q", "", core.RunQueued)
	e.engine.Receipt = func(core.MessageReceipt) core.MessageReceipt {
		return core.MessageReceipt{MessageID: "msg_from_engine", Outcome: core.MessageQueued}
	}
	r = e.must(post("run_q", "hello queued"), 202)
	e.engine.Receipt = nil
	if r.Body["outcome"] != OutcomeQueued || r.Body["message_id"] != "msg_from_engine" || r.Body["run_id"] != "run_q" {
		t.Fatalf("queued = %s", r.Raw)
	}

	// Finished run without a session: a new session.
	e.seedRun("run_done", "", core.RunSucceeded)
	e.addSession(core.RunSession{RunID: "run_done", Session: 1, State: core.SessionEnded})
	if r = e.must(post("run_done", "again"), 202); r.Body["outcome"] != OutcomeNewSession {
		t.Fatalf("done = %s", r.Raw)
	}
	// Idle without a session.
	e.seedRun("run_idle", "", core.RunIdle)
	if r = e.must(post("run_idle", "again"), 202); r.Body["outcome"] != OutcomeNewSession {
		t.Fatalf("idle = %s", r.Raw)
	}
	// The other queue states.
	for i, st := range []core.RunState{core.RunPending, core.RunClaimed, core.RunRunning} {
		id := fmt.Sprintf("run_w%d", i)
		e.seedRun(id, "", st)
		if r = e.must(post(id, "x"), 202); r.Body["outcome"] != OutcomeQueued {
			t.Fatalf("%s = %s", st, r.Raw)
		}
	}

	// Attachments go to the engine; errors map as for followups.
	e.must(post("run_done", " "), 400)
	e.must(post("run_nope", "x"), 404)
	e.must(e.do("POST", "/runs/run_done/messages", e.other.Key, map[string]any{"message": "x"}), 404)
	e.seedRun("run_cancelled", "", core.RunCancelled)
	// With the runner store, a message continues a cancelled run.
	if r = e.must(post("run_cancelled", "x"), 202); r.Body["outcome"] != OutcomeNewSession {
		t.Fatalf("cancelled = %s", r.Raw)
	}
	at := e.must(e.do("POST", "/runs/run_done/messages", e.viewer.Key, map[string]any{"message": "x", "attachments": []string{"att_missing"}}), 400)
	_ = at

	// The followups route is unchanged: it answers with the run.
	f := e.must(e.do("POST", "/runs/run_done/followups", e.viewer.Key, map[string]any{"message": "y"}), 202)
	if f.Body["run_id"] != "run_done" || f.Body["state"] == nil {
		t.Fatalf("followup = %s", f.Raw)
	}
}

func TestRunInputs(t *testing.T) {
	e := runnerEnv(t)
	e.seedRun("run_a", "", core.RunRunning)
	e.addInput(core.RunInput{RunID: "run_a", Kind: "user", Sender: core.RunInputSender{Type: "user", Name: "Dana"}, Content: "first", State: core.RunInputConsumed})
	e.addInput(core.RunInput{RunID: "run_a", Kind: "child_result", Sender: core.RunInputSender{Type: "run", ID: "run_b"}, Content: "second",
		Data: []byte(`{"event":"succeeded"}`), Untrusted: true, DedupeKey: "lifecycle:run_b:succeeded:1"})

	g := e.must(e.do("GET", "/runs/run_a/inputs", e.viewer.Key, nil), 200)
	in := list(g.Body, "inputs")
	if len(in) != 2 {
		t.Fatalf("inputs = %s", g.Raw)
	}
	first := in[0].(map[string]any)
	if first["kind"] != "user" || first["state"] != "consumed" || str(first, "sender", "name") != "Dana" || first["seq"] != float64(1) {
		t.Fatalf("first = %v", first)
	}
	// The record has the untrusted flag, the dedupe key and the event data.
	second := in[1].(map[string]any)
	if second["untrusted"] != true || second["dedupe_key"] != "lifecycle:run_b:succeeded:1" || str(second, "data", "event") != "succeeded" || first["untrusted"] != false {
		t.Fatalf("second = %v", second)
	}
	if p := e.must(e.do("GET", "/runs/run_a/inputs?state=pending", e.viewer.Key, nil), 200); len(list(p.Body, "inputs")) != 1 {
		t.Fatalf("pending = %s", p.Raw)
	}
	if p := e.must(e.do("GET", "/runs/run_a/inputs?after=1", e.viewer.Key, nil), 200); len(list(p.Body, "inputs")) != 1 {
		t.Fatalf("after = %s", p.Raw)
	}
	e.must(e.do("GET", "/runs/run_a/inputs?state=bogus", e.viewer.Key, nil), 400)
	e.must(e.do("GET", "/runs/run_a/inputs", e.other.Key, nil), 404)
	// The transcript list stays at /messages.
	if m := e.must(e.do("GET", "/runs/run_a/messages", e.viewer.Key, nil), 200); m.Body["messages"] == nil {
		t.Fatalf("messages = %s", m.Raw)
	}
}

func TestCancelCascade(t *testing.T) {
	e := newTestEnv(t)
	e.seedRun("run_a", "", core.RunRunning)
	e.seedRun("run_b", "", core.RunRunning)
	e.seedRun("run_c", "", core.RunRunning)
	e.must(e.do("POST", "/runs/run_a/cancel", e.viewer.Key, map[string]any{"cascade": false}), 200)
	if n := len(e.engine.CallsTo("CancelOne")); n != 1 {
		t.Fatalf("CancelOne calls = %d, want 1", n)
	}
	e.must(e.do("POST", "/runs/run_b/cancel", e.viewer.Key, map[string]any{"cascade": true}), 200)
	e.must(e.do("POST", "/runs/run_c/cancel", e.viewer.Key, nil), 200)
	// A cancel is accepted in each state.
	e.must(e.do("POST", "/runs/run_c/cancel", e.viewer.Key, "{}"), 200)
	if n := len(e.engine.CallsTo("Cancel")); n != 3 {
		t.Fatalf("Cancel calls = %d, want 3", n)
	}
}

func TestRunTimelineDiagnosticsSessions(t *testing.T) {
	e := runnerEnv(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	run := e.seedRun("run_a", "", core.RunSucceeded)
	run.CreatedAt = t0
	fin := at(100)
	run.FinishedAt = &fin
	_ = e.store.UpdateRun(ctx, run)

	st1, rd1, en1 := at(5), at(10), at(50)
	e.addSession(core.RunSession{RunID: "run_a", Session: 1, State: core.SessionEnded, AssignedAt: at(1), StartedAt: &st1, ReadyAt: &rd1, EndedAt: &en1,
		EndReason: "idle_timeout", RunnerVersion: "1.2.3", Usage: core.Usage{InputTokens: 7}},
		runnerEvent{"setup.step", "", at(6), `{"step":"clone","status":"ok","duration_ms":12}`},
		runnerEvent{"snapshot.restored", "", at(7), `{"parts":["workspace"]}`},
		runnerEvent{"session.ready", "", at(10), `{"capabilities":["turn"]}`},
		runnerEvent{"turn.started", "turn_1", at(11), ""},
		runnerEvent{"usage", "", at(12), `{"input_tokens":1}`},
		runnerEvent{"diagnostic", "", at(13), `{"code":"network/egress_blocked","message":"blocked","data":{"blocked_host":"x.test"}}`},
		runnerEvent{"turn.ended", "turn_1", at(20), `{"outcome":"done"}`},
		runnerEvent{"snapshot.saved", "", at(21), `{"part":"workspace","bytes":10}`},
		runnerEvent{"snapshot.saved", "", at(22), `{"part":"harness_state","bytes":5}`})
	st2 := at(60)
	e.addSession(core.RunSession{RunID: "run_a", Session: 2, State: core.SessionActive, AssignedAt: at(55), StartedAt: &st2, SandboxReason: core.SandboxCompleted},
		runnerEvent{"snapshot.saved", "", at(70), `{"part":"workspace","bytes":10}`})

	// Control-plane events. The run.queued row has the same time as run.created.
	tlAdd := func(id string, session int, typ string, s int, data string) {
		ev := core.TimelineEvent{EventID: id, RunID: "run_a", Session: session, Type: typ, At: at(s)}
		if data != "" {
			ev.Data = []byte(data)
		}
		if _, err := e.store.AppendTimelineEvent(ctx, &ev); err != nil {
			t.Fatal(err)
		}
	}
	tlAdd("q", 0, "run.queued", 1, `{"reason":"new"}`)
	tlAdd("a1", 1, "sandbox.assigned", 2, `{"backend":"local"}`)
	tlAdd("s1", 1, "session.started", 5, `{"runner_version":"1.2.3"}`)
	tlAdd("sd1", 1, "secrets.delivered", 5, `{"names":["TOKEN"]}`)
	tlAdd("e1", 1, "session.ended", 50, `{"reason":"idle_timeout"}`)
	tlAdd("s2", 2, "session.started", 60, `{"runner_version":"1.2.4"}`)
	tlAdd("ok", 0, "run.succeeded", 100, "")
	tlAdd("ok", 0, "run.failed", 101, "") // same ID: ignored

	tl := e.must(e.do("GET", "/runs/run_a/timeline", e.viewer.Key, nil), 200)
	var types []string
	for _, x := range list(tl.Body, "events") {
		m := x.(map[string]any)
		types = append(types, fmt.Sprintf("%s/%v", m["type"], m["session"]))
		if m["event_id"] == "" || m["run_id"] != "run_a" || m["at"] == nil || m["data"] == nil {
			t.Errorf("event = %v", m)
		}
	}
	want := "run.created/0 run.queued/0 sandbox.assigned/1 session.started/1 secrets.delivered/1 setup.step/1 snapshot.restored/1 session.ready/1 turn.started/1 turn.ended/1 snapshot.saved/1 snapshot.saved/1 session.ended/1 session.started/2 snapshot.saved/2 run.succeeded/0"
	if strings.Join(types, " ") != want {
		t.Fatalf("timeline types:\n got %s\nwant %s", strings.Join(types, " "), want)
	}
	for _, x := range list(tl.Body, "events") {
		m := x.(map[string]any)
		switch m["type"] {
		case "turn.started":
			if str(m, "data", "turn_id") != "turn_1" {
				t.Errorf("turn.started data = %v", m["data"])
			}
		case "session.ended":
			if str(m, "data", "reason") != "idle_timeout" {
				t.Errorf("session.ended data = %v", m["data"])
			}
		case "session.started":
			if m["session"] == float64(1) && str(m, "data", "runner_version") != "1.2.3" {
				t.Errorf("session.started data = %v", m["data"])
			}
		}
	}

	dg := e.must(e.do("GET", "/runs/run_a/diagnostics", e.viewer.Key, nil), 200)
	ds := list(dg.Body, "diagnostics")
	if len(ds) != 1 {
		t.Fatalf("diagnostics = %s", dg.Raw)
	}
	d0 := ds[0].(map[string]any)
	if d0["code"] != "network/egress_blocked" || d0["message"] != "blocked" || str(d0, "data", "blocked_host") != "x.test" || d0["at"] == nil {
		t.Fatalf("diagnostic = %v", d0)
	}

	ss := e.must(e.do("GET", "/runs/run_a/sessions", e.viewer.Key, nil), 200)
	sl := list(ss.Body, "sessions")
	if len(sl) != 2 {
		t.Fatalf("sessions = %s", ss.Raw)
	}
	s1, s2 := sl[0].(map[string]any), sl[1].(map[string]any)
	snap1, snap2 := s1["snapshot"].(map[string]any), s2["snapshot"].(map[string]any)
	if s1["number"] != float64(1) || s1["state"] != "ended" || s1["end_reason"] != "idle_timeout" || s1["ready_at"] == nil ||
		s1["ended_at"] == nil || s1["assigned_at"] == nil || s1["started_at"] == nil ||
		snap1["workspace"] != true || snap1["harness_state"] != true || str(s1, "usage", "coverage") != "known" ||
		s1["usage"].(map[string]any)["input_tokens"] != float64(7) {
		t.Fatalf("session 1 = %v", s1)
	}
	if s2["number"] != float64(2) || s2["state"] != "active" || s2["sandbox_reason"] != string(core.SandboxCompleted) ||
		snap2["workspace"] != true || snap2["harness_state"] != false {
		t.Fatalf("session 2 = %v", s2)
	}

	for _, p := range []string{"timeline", "diagnostics", "sessions", "conversation", "inputs"} {
		e.must(e.do("GET", "/runs/run_a/"+p, e.other.Key, nil), 404)
		e.must(e.do("GET", "/runs/run_nope/"+p, e.viewer.Key, nil), 404)
	}
}

func TestRunTimelineCancelledFromRunRow(t *testing.T) {
	e := runnerEnv(t)
	ctx := context.Background()
	run := e.seedRun("run_c", "", core.RunCancelled)
	fin := run.CreatedAt.Add(time.Minute)
	run.FinishedAt = &fin
	_ = e.store.UpdateRun(ctx, run)
	tl := e.must(e.do("GET", "/runs/run_c/timeline", e.viewer.Key, nil), 200)
	var types []string
	for _, x := range list(tl.Body, "events") {
		types = append(types, x.(map[string]any)["type"].(string))
	}
	if strings.Join(types, " ") != "run.created run.cancelled" {
		t.Fatalf("types = %v", types)
	}
}

func TestRunConversation(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.seedRun("run_a", "", core.RunSucceeded)
	add := func(role, content, data string) {
		m := &core.RunMessage{RunID: "run_a", Role: role, Content: content}
		if data != "" {
			m.Data = []byte(data)
		}
		_ = e.store.AppendRunMessage(ctx, m)
	}
	add("user", "do it", "")
	add("assistant", "working", "")
	add("tool_call", "run_command", `{"call_id":"c1","category":"command","arguments":{"cmd":"ls"}}`)
	add("tool_result", "a.txt", `{"call_id":"c1"}`)
	add("assistant", "all done", "")

	c := e.must(e.do("GET", "/runs/run_a/conversation", e.viewer.Key, nil), 200)
	steps := list(c.Body, "steps")
	if c.Body["conversation_id"] != "run_a" || len(steps) != 1 {
		t.Fatalf("conversation = %s", c.Raw)
	}
	msgs := steps[0].(map[string]any)["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "user" || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Fatalf("messages = %v", msgs)
	}
	blocks := msgs[1].(map[string]any)["blocks"].([]any)
	if len(blocks) != 4 || blocks[1].(map[string]any)["type"] != "action" || blocks[2].(map[string]any)["type"] != "action_result" {
		t.Fatalf("blocks = %v", blocks)
	}

	f := e.must(e.do("GET", "/runs/run_a/conversation?scope=final_message", e.viewer.Key, nil), 200)
	fsteps := list(f.Body, "steps")
	if len(fsteps) != 1 {
		t.Fatalf("final = %s", f.Raw)
	}
	fm := fsteps[0].(map[string]any)["messages"].([]any)
	if len(fm) != 1 {
		t.Fatalf("final messages = %v", fm)
	}
	fb := fm[0].(map[string]any)["blocks"].([]any)
	if fm[0].(map[string]any)["role"] != "assistant" || len(fb) != 4 {
		t.Fatalf("final message = %v", fm[0])
	}

	// A run with no assistant message has no step in the final scope.
	e.seedRun("run_b", "", core.RunQueued)
	_ = e.store.AppendRunMessage(ctx, &core.RunMessage{RunID: "run_b", Role: "user", Content: "hi"})
	g := e.must(e.do("GET", "/runs/run_b/conversation?scope=final_message", e.viewer.Key, nil), 200)
	if len(list(g.Body, "steps")) != 0 {
		t.Fatalf("final of run_b = %s", g.Raw)
	}
	e.must(e.do("GET", "/runs/run_a/conversation?scope=bogus", e.viewer.Key, nil), 400)
}
