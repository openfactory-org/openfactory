package dispatch

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// newReplica makes another dispatcher over the store, the backend and the
// clock of e. It is another replica of the control plane.
func newReplica(e *env, id string) *Dispatcher {
	e.t.Helper()
	d, err := New(Config{
		Store:      e.mem,
		Runner:     e.mem,
		Backend:    e.backend,
		ControlURL: "http://control.test",
		Image:      "img",
		ReplicaID:  id,
		Now:        func() time.Time { return *e.clock },
	})
	if err != nil {
		e.t.Fatal(err)
	}
	d.SetCommander(e.cmd)
	d.cfg.Router = e.d.cfg.Router
	return d
}

// accept gives events to a replica like the runner API does: the store keeps
// the events that are new, and the replica reacts to those only. A runner
// that sends again after a reconnect therefore repeats nothing.
func (e *env) accept(d *Dispatcher, runID string, session int, evs ...core.RunnerEvent) {
	e.t.Helper()
	s := e.session(runID, session)
	before := s.EventAck
	ack, err := e.mem.AppendRunnerEvents(e.ctx, runID, session, evs)
	if err != nil {
		e.t.Fatalf("append events: %v", err)
	}
	var fresh []core.RunnerEvent
	for _, ev := range evs {
		if ev.Seq > before && ev.Seq <= ack {
			fresh = append(fresh, ev)
		}
	}
	d.EventsStored(e.ctx, s, fresh)
}

func (e *env) assistantTranscript(runID string) string {
	e.t.Helper()
	msgs, err := e.mem.ListRunMessages(e.ctx, runID, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var parts []string
	for _, m := range msgs {
		parts = append(parts, m.Role+":"+m.Content)
	}
	return strings.Join(parts, ";")
}

// TestReplicaStopsDuringTurn stops the replica that watches a turn. The turn
// continues. The transcript has no repeated events and no repeated tool calls.
func TestReplicaStopsDuringTurn(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e) // replica A starts the task and delivers the turn
	e.backend.setStatus(core.TaskStatus{TaskID: run.ID + ".1", State: core.TaskRunning})

	first := []core.RunnerEvent{
		event(t, 1, runnerproto.EventTurnStarted, turn, nil),
		event(t, 2, runnerproto.EventMessage, turn, runnerproto.MessageData{Role: "assistant", Content: "looking"}),
		event(t, 3, runnerproto.EventToolCall, turn, runnerproto.ToolCallData{CallID: "c1", Name: "run_agents"}),
	}
	e.accept(e.d, run.ID, 1, first...)
	if len(e.commands(run.ID, 1)) != 1 {
		t.Fatalf("commands before the stop = %d", len(e.commands(run.ID, 1)))
	}

	// Replica A stops. It never runs again. Replica B takes over the queue.
	b := newReplica(e, "replica-b")
	for range 4 {
		e.advance(20 * time.Second)
		if err := b.Tick(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.run(run.ID).State; got != core.RunRunning {
		t.Fatalf("state after the stop = %s, want running", got)
	}
	if s := e.session(run.ID, 1); s.State != core.SessionActive {
		t.Fatalf("session = %+v", s)
	}
	sessions, _ := e.mem.ListRunSessions(e.ctx, run.ID)
	if len(sessions) != 1 || len(e.backend.startedTasks()) != 1 {
		t.Fatalf("sessions = %d, tasks = %d: the turn started again", len(sessions), len(e.backend.startedTasks()))
	}
	if len(e.backend.destroyedIDs()) != 0 || len(e.backend.cancelled) != 0 {
		t.Fatalf("destroyed %v, cancelled %v", e.backend.destroyedIDs(), e.backend.cancelled)
	}
	if len(e.commands(run.ID, 1)) != 1 {
		t.Fatalf("commands after the stop = %d, want the one turn.start", len(e.commands(run.ID, 1)))
	}

	// The runner connects to replica B and sends its events again from its
	// cursor: the two events that it is not sure about, and the new ones.
	e.accept(b, run.ID, 1, append(slices.Clone(first[1:]),
		event(t, 4, runnerproto.EventToolResult, turn, runnerproto.ToolResultData{CallID: "c1", Content: "started 1 child"}),
		event(t, 5, runnerproto.EventMessage, turn, runnerproto.MessageData{Role: "assistant", Content: "done"}),
		event(t, 6, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone, Result: "done"}),
	)...)
	if r := e.run(run.ID); r.State != core.RunSucceeded || r.Result != "done" {
		t.Fatalf("run = %s %q", r.State, r.Result)
	}
	want := "assistant:looking;tool_call:run_agents;tool_result:started 1 child;assistant:done"
	if got := e.assistantTranscript(run.ID); got != want {
		t.Fatalf("transcript = %s\nwant       %s", got, want)
	}
}

// TestWorkerRestartsDuringTurn restarts the remote worker during a turn. The turn
// continues. The runner talks to the control plane directly, so the turn does
// not need the connection of the worker.
func TestWorkerRestartsDuringTurn(t *testing.T) {
	e, r := routedEnv(t)
	run := e.workerRun()
	e.tick()
	e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
	e.makeActive(run.ID, 1)
	e.tick() // delivers the prompt
	turn := e.session(run.ID, 1).ActiveTurnID
	task := run.ID + ".1"
	r.backend.setStatus(core.TaskStatus{TaskID: task, State: core.TaskRunning})
	e.accept(e.d, run.ID, 1,
		event(t, 1, runnerproto.EventTurnStarted, turn, nil),
		event(t, 2, runnerproto.EventToolCall, turn, runnerproto.ToolCallData{CallID: "c1", Name: "bash"}))

	// The worker restarts: the hub has no connection for a while. The runner
	// is silent for longer than the first wait, so the dispatcher asks the
	// backend. It gets no answer, and the session stays.
	r.backend.statusEr = fmt.Errorf("worker is not connected: %w", core.ErrUnavailable)
	r.ready = false
	e.advance(e.d.cfg.LostAfter + time.Second)
	e.tick()
	if s := e.session(run.ID, 1); s.State != core.SessionActive {
		t.Fatalf("session while the worker restarts = %+v", s)
	}
	if got := e.run(run.ID).State; got != core.RunRunning {
		t.Fatalf("state while the worker restarts = %s", got)
	}

	// The worker is back and reports the task as running. The runner sends its
	// events again and ends the turn.
	r.backend.statusEr = nil
	r.ready = true
	e.tick()
	e.accept(e.d, run.ID, 1,
		event(t, 2, runnerproto.EventToolCall, turn, runnerproto.ToolCallData{CallID: "c1", Name: "bash"}),
		event(t, 3, runnerproto.EventToolResult, turn, runnerproto.ToolResultData{CallID: "c1", Content: "ok"}),
		event(t, 4, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone, Result: "ok"}))
	if got := e.run(run.ID); got.State != core.RunSucceeded || got.Result != "ok" {
		t.Fatalf("run = %s %q", got.State, got.Result)
	}
	if n := len(r.backend.startedTasks()); n != 1 {
		t.Fatalf("tasks started = %d: the turn started again", n)
	}
	if len(e.commands(run.ID, 1)) != 1 || len(r.backend.destroyedIDs()) != 0 || len(r.backend.cancelled) != 0 {
		t.Fatalf("commands %d, destroyed %v, cancelled %v", len(e.commands(run.ID, 1)), r.backend.destroyedIDs(), r.backend.cancelled)
	}
	if got, want := e.assistantTranscript(run.ID), "tool_call:bash;tool_result:ok"; got != want {
		t.Fatalf("transcript = %s, want %s", got, want)
	}
}

// A rolling upgrade with active runs fails no run and repeats
// no turn. Each replica stops in turn and a new one starts. The runs are in the
// middle of a turn the whole time.
func TestRollingUpgradeWithActiveRuns(t *testing.T) {
	e := newEnv(t)
	const n = 3
	type live struct {
		run  *core.Run
		turn string
	}
	var runs []live
	for i := range n {
		run := e.newRun(core.RunQueued, fmt.Sprintf("prompt %d", i))
		runs = append(runs, live{run: run})
	}
	e.tick() // the first replica assigns a task to each run
	for i := range runs {
		run := runs[i].run
		e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
		e.makeActive(run.ID, 1)
	}
	e.tick() // delivers each prompt
	for i := range runs {
		run := runs[i].run
		runs[i].turn = e.session(run.ID, 1).ActiveTurnID
		e.backend.setStatus(core.TaskStatus{TaskID: run.ID + ".1", State: core.TaskRunning})
		e.accept(e.d, run.ID, 1,
			event(t, 1, runnerproto.EventTurnStarted, runs[i].turn, nil),
			event(t, 2, runnerproto.EventMessage, runs[i].turn, runnerproto.MessageData{Role: "assistant", Content: "working " + run.ID}))
	}

	// The backend lists the tasks, so the orphan sweep of each new replica
	// looks at them. They are older than the sweep age. Each has an open
	// session, so the sweep must keep them.
	started := *e.clock
	var listed []core.TaskStatus
	for _, l := range runs {
		listed = append(listed, core.TaskStatus{TaskID: l.run.ID + ".1", State: core.TaskRunning, StartedAt: &started})
	}
	e.backend.mu.Lock()
	e.backend.listed = listed
	e.backend.mu.Unlock()

	// Three replicas stop one after the other. A new replica starts after each
	// stop and does its work: the queue tick and the orphan sweep.
	current := e.d
	for step := range 3 {
		e.advance(40 * time.Second) // the grace period of a pod
		current = newReplica(e, fmt.Sprintf("replica-%d", step+2))
		e.advance(30 * time.Second)
		if err := current.Tick(e.ctx); err != nil {
			t.Fatal(err)
		}
		for _, l := range runs {
			// The runner keeps its connection to the control plane and sends events.
			e.mem.TouchRunSession(e.ctx, l.run.ID, 1, *e.clock)
		}
	}
	e.advance(5 * time.Minute) // longer than the orphan age: the sweep must keep the live tasks
	if err := current.Tick(e.ctx); err != nil {
		t.Fatal(err)
	}

	for _, l := range runs {
		if got := e.run(l.run.ID).State; got != core.RunRunning {
			t.Fatalf("run %s is %s after the rolling upgrade, want running", l.run.ID, got)
		}
		if got := len(e.commands(l.run.ID, 1)); got != 1 {
			t.Fatalf("run %s got %d commands, want the one turn.start", l.run.ID, got)
		}
		if sessions, _ := e.mem.ListRunSessions(e.ctx, l.run.ID); len(sessions) != 1 {
			t.Fatalf("run %s has %d sessions", l.run.ID, len(sessions))
		}
	}
	if got := len(e.backend.startedTasks()); got != n {
		t.Fatalf("tasks started = %d, want %d", got, n)
	}
	if len(e.backend.destroyedIDs()) != 0 || len(e.backend.cancelled) != 0 {
		t.Fatalf("destroyed %v, cancelled %v", e.backend.destroyedIDs(), e.backend.cancelled)
	}

	// The turns end through the last replica. No run failed, and each
	// transcript has its message once.
	for _, l := range runs {
		e.accept(current, l.run.ID, 1,
			event(t, 2, runnerproto.EventMessage, l.turn, runnerproto.MessageData{Role: "assistant", Content: "working " + l.run.ID}),
			event(t, 3, runnerproto.EventTurnEnded, l.turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone, Result: "done"}))
		if got := e.run(l.run.ID).State; got != core.RunSucceeded {
			t.Fatalf("run %s is %s after its turn ended", l.run.ID, got)
		}
		if got, want := e.assistantTranscript(l.run.ID), "assistant:working "+l.run.ID; got != want {
			t.Fatalf("transcript = %s, want %s", got, want)
		}
	}
}

// A replica that stops cleanly gives the dispatch lease up. The next replica
// is the leader at its next tick, and does not wait for the lease to end.
func TestReleaseLeadershipLetsAnotherReplicaLead(t *testing.T) {
	e := newEnv(t)
	other := newReplica(e, "replica-b")
	if !e.d.isLeader(e.ctx) {
		t.Fatal("the first replica is not the leader")
	}
	if other.isLeader(e.ctx) {
		t.Fatal("the second replica is a leader while the first holds the lease")
	}
	e.advance(time.Second) // far less than leaderTTL
	other.ReleaseLeadership(e.ctx)
	if other.isLeader(e.ctx) {
		t.Fatal("a replica that does not hold the lease released it")
	}
	e.d.ReleaseLeadership(e.ctx)
	if !other.isLeader(e.ctx) {
		t.Fatal("the second replica is not the leader after the release")
	}
}
