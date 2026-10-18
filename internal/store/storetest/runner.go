package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// runnerStoreOf returns the RunnerStore of s, or skips the test.
func runnerStoreOf(t *testing.T, s core.Store) core.RunnerStore {
	t.Helper()
	rs, isRunner := s.(core.RunnerStore)
	if !isRunner {
		t.Skip("store does not implement core.RunnerStore")
	}
	return rs
}

// mkSession creates a run and its session 1 with a unique bootstrap hash.
func mkSession(t *testing.T, s core.Store, rs core.RunnerStore) *core.RunSession {
	t.Helper()
	r := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, r))
	ss := &core.RunSession{RunID: r.ID, Session: 1, TaskID: uid("task"), BootstrapHash: uid("hash")}
	ok(t, rs.CreateRunSession(ctx, ss))
	return ss
}

// endSession sets the state of a session to ended.
func endSession(t *testing.T, rs core.RunnerStore, runID string, session int) {
	t.Helper()
	cur, err := rs.GetRunSession(ctx, runID, session)
	ok(t, err)
	cur.State = core.SessionEnded
	ok(t, rs.UpdateRunSession(ctx, cur))
}

func mkEvents(from, to int64) []core.RunnerEvent {
	var out []core.RunnerEvent
	for i := from; i <= to; i++ {
		out = append(out, core.RunnerEvent{Seq: i, Type: "message", At: base, Data: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))})
	}
	return out
}

func eventSeqs(evs []core.RunnerEvent) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

func testRunnerSessions(t *testing.T, s core.Store) {
	rs := runnerStoreOf(t, s)

	// 1. Missing run.
	isErr(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: uid("run"), Session: 1}), core.ErrNotFound)

	// 2. Wrong first number.
	r := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, r))
	isErr(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: r.ID, Session: 2}), core.ErrConflict)

	// 3. First session.
	hash := uid("hash")
	ss := &core.RunSession{RunID: r.ID, Session: 1, TaskID: uid("task"), BootstrapHash: hash, Capabilities: []string{"resume"}}
	ok(t, rs.CreateRunSession(ctx, ss))
	eq(t, ss.State, core.SessionAssigned, "state")
	eq(t, ss.Version, int64(1), "version")
	eq(t, ss.EventAck, int64(0), "event ack")
	eq(t, ss.CommandSeq, int64(0), "command seq")
	eq(t, ss.CommandAck, int64(0), "command ack")
	if ss.AssignedAt.IsZero() || ss.LastSeenAt.IsZero() {
		t.Fatalf("AssignedAt and LastSeenAt must be set: %+v", ss)
	}

	// 4. Duplicate.
	isErr(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: r.ID, Session: 1}), core.ErrConflict)
	// 5. Next session while the old one is not ended.
	isErr(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: r.ID, Session: 2}), core.ErrConflict)

	// 6. Get.
	got, err := rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	eq(t, got.RunID, ss.RunID, "run id")
	eq(t, got.Session, 1, "session")
	eq(t, got.TaskID, ss.TaskID, "task id")
	eq(t, got.State, ss.State, "state")
	eq(t, got.BootstrapHash, hash, "bootstrap hash")
	eq(t, got.Version, ss.Version, "version")
	eq(t, got.EventAck, int64(0), "event ack")
	eq(t, len(got.Capabilities), 1, "capabilities")
	eq(t, got.Capabilities[0], "resume", "capability")
	timeEq(t, got.AssignedAt, ss.AssignedAt, "assigned_at")
	timeEq(t, got.LastSeenAt, ss.LastSeenAt, "last_seen_at")
	timeEq(t, got.BootstrapExpiresAt, ss.BootstrapExpiresAt, "bootstrap_expires_at")
	_, err = rs.GetRunSession(ctx, r.ID, 9)
	isErr(t, err, core.ErrNotFound)

	// 7. By bootstrap hash.
	byHash, err := rs.GetRunSessionByBootstrapHash(ctx, hash)
	ok(t, err)
	eq(t, byHash.RunID, r.ID, "by hash run")
	eq(t, byHash.Session, 1, "by hash session")
	_, err = rs.GetRunSessionByBootstrapHash(ctx, uid("unknown"))
	isErr(t, err, core.ErrNotFound)
	_, err = rs.GetRunSessionByBootstrapHash(ctx, "")
	isErr(t, err, core.ErrNotFound)

	// 8. Update.
	before, err := rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	cur, err := rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	startedAt := base.Add(time.Minute)
	cur.State = core.SessionActive
	cur.InstanceHash = "i1"
	cur.StartedAt = &startedAt
	ok(t, rs.UpdateRunSession(ctx, cur))
	eq(t, cur.Version, int64(2), "version after update")
	got, err = rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	eq(t, got.State, core.SessionActive, "state after update")
	eq(t, got.InstanceHash, "i1", "instance hash")
	if got.StartedAt == nil {
		t.Fatal("started_at lost")
	}
	timeEq(t, *got.StartedAt, startedAt, "started_at")
	eq(t, got.Version, int64(2), "stored version")

	// 9. Stale update.
	stale := *before
	stale.SandboxID = "stale"
	isErr(t, rs.UpdateRunSession(ctx, &stale), core.ErrConflict)
	got, err = rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	eq(t, got.SandboxID, "", "sandbox id after stale update")
	eq(t, got.Version, int64(2), "version after stale update")

	// 10. Missing session.
	isErr(t, rs.UpdateRunSession(ctx, &core.RunSession{RunID: r.ID, Session: 9, Version: 1}), core.ErrNotFound)

	// 11. Counters are not changed by an update.
	n, err := rs.AppendRunnerEvents(ctx, r.ID, 1, mkEvents(1, 3))
	ok(t, err)
	eq(t, n, int64(3), "ack")
	fresh, err := rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	eq(t, fresh.EventAck, int64(3), "stored event ack")
	fresh.EventAck = 0
	fresh.CommandSeq = 77
	fresh.SandboxID = "sb1"
	ok(t, rs.UpdateRunSession(ctx, fresh))
	got, err = rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	eq(t, got.EventAck, int64(3), "event ack after update")
	eq(t, got.CommandSeq, int64(0), "command seq after update")
	eq(t, got.SandboxID, "sb1", "sandbox id after update")

	// 12. Touch.
	seen := got.LastSeenAt
	ok(t, rs.TouchRunSession(ctx, r.ID, 1, seen.Add(time.Hour)))
	got, err = rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	timeEq(t, got.LastSeenAt, seen.Add(time.Hour), "last_seen_at after touch")
	version := got.Version
	ok(t, rs.TouchRunSession(ctx, r.ID, 1, seen.Add(-2*time.Hour)))
	got, err = rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	timeEq(t, got.LastSeenAt, seen.Add(time.Hour), "last_seen_at after older touch")
	eq(t, got.Version, version, "version after touch")
	isErr(t, rs.TouchRunSession(ctx, r.ID, 9, seen), core.ErrNotFound)

	// 13. End session 1, create session 2.
	cur, err = rs.GetRunSession(ctx, r.ID, 1)
	ok(t, err)
	cur.State = core.SessionEnded
	ok(t, rs.UpdateRunSession(ctx, cur))
	ok(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: r.ID, Session: 2, TaskID: uid("task")}))
	latest, err := rs.LatestRunSession(ctx, r.ID)
	ok(t, err)
	eq(t, latest.Session, 2, "latest session")
	list, err := rs.ListRunSessions(ctx, r.ID)
	ok(t, err)
	eq(t, len(list), 2, "sessions")
	eq(t, list[0].Session, 1, "first listed")
	eq(t, list[1].Session, 2, "second listed")

	// 14. Run without a session.
	empty := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, empty))
	_, err = rs.LatestRunSession(ctx, empty.ID)
	isErr(t, err, core.ErrNotFound)
	list, err = rs.ListRunSessions(ctx, empty.ID)
	ok(t, err)
	eq(t, len(list), 0, "sessions of empty run")

	// 14b. Open sessions: session 2 of r is open, session 1 is ended.
	open, err := rs.ListOpenRunSessions(ctx, 0)
	ok(t, err)
	found := 0
	for _, o := range open {
		if o.State != core.SessionAssigned && o.State != core.SessionActive {
			t.Fatalf("open list has state %q", o.State)
		}
		if o.RunID == r.ID {
			found++
			eq(t, o.Session, 2, "open session of run")
		}
	}
	eq(t, found, 1, "open sessions of run")
	for i := 1; i < len(open); i++ {
		if open[i].LastSeenAt.Before(open[i-1].LastSeenAt) {
			t.Fatalf("open sessions not in LastSeenAt order at %d", i)
		}
	}
	if len(open) > 0 {
		one, err := rs.ListOpenRunSessions(ctx, 1)
		ok(t, err)
		eq(t, len(one), 1, "open sessions with limit")
	}
	readyAt := base.Add(2 * time.Minute)
	cur, err = rs.GetRunSession(ctx, r.ID, 2)
	ok(t, err)
	cur.ReadyAt = &readyAt
	ok(t, rs.UpdateRunSession(ctx, cur))
	got, err = rs.GetRunSession(ctx, r.ID, 2)
	ok(t, err)
	if got.ReadyAt == nil {
		t.Fatal("ready_at lost")
	}
	timeEq(t, *got.ReadyAt, readyAt, "ready_at")

	// 15. Bootstrap hash is unique when it is not empty.
	other := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, other))
	isErr(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: other.ID, Session: 1, BootstrapHash: hash}), core.ErrConflict)
	ok(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: other.ID, Session: 1}))
	endSession(t, rs, other.ID, 1)
	ok(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: other.ID, Session: 2}))
}

func testRunnerEvents(t *testing.T, s core.Store) {
	rs := runnerStoreOf(t, s)
	ss := mkSession(t, s, rs)
	run := ss.RunID
	count := func(session int, want int) []core.RunnerEvent {
		t.Helper()
		evs, err := rs.ListRunnerEvents(ctx, run, session, 0, 0)
		ok(t, err)
		eq(t, len(evs), want, "events")
		return evs
	}

	// 1. First batch.
	before := time.Now().Add(-time.Second)
	ack, err := rs.AppendRunnerEvents(ctx, run, 1, mkEvents(1, 3))
	ok(t, err)
	eq(t, ack, int64(3), "ack")
	evs := count(1, 3)
	for i, e := range evs {
		eq(t, e.RunID, run, "run id")
		eq(t, e.Session, 1, "session")
		eq(t, e.Seq, int64(i+1), "seq")
		eq(t, e.Type, "message", "type")
		jsonEq(t, e.Data, json.RawMessage(fmt.Sprintf(`{"n":%d}`, i+1)), "data")
	}

	// 9. LastSeenAt moved.
	got, err := rs.GetRunSession(ctx, run, 1)
	ok(t, err)
	if got.LastSeenAt.Before(before) {
		t.Fatalf("last_seen_at %v is before %v", got.LastSeenAt, before)
	}
	eq(t, got.EventAck, int64(3), "session event ack")

	// 2. Overlap.
	ack, err = rs.AppendRunnerEvents(ctx, run, 1, mkEvents(2, 4))
	ok(t, err)
	eq(t, ack, int64(4), "ack after overlap")
	evs = count(1, 4)
	for i, e := range evs {
		eq(t, e.Seq, int64(i+1), "seq after overlap")
	}

	// 3. Full repeat.
	ack, err = rs.AppendRunnerEvents(ctx, run, 1, mkEvents(1, 4))
	ok(t, err)
	eq(t, ack, int64(4), "ack after repeat")
	count(1, 4)

	// 4. Gap.
	ack, err = rs.AppendRunnerEvents(ctx, run, 1, mkEvents(7, 8))
	isErr(t, err, core.ErrSeqGap)
	eq(t, ack, int64(4), "ack after gap")
	count(1, 4)

	// 5. Bad batches.
	bad := append(mkEvents(5, 5), mkEvents(7, 7)...)
	_, err = rs.AppendRunnerEvents(ctx, run, 1, bad)
	isErr(t, err, core.ErrInvalid)
	count(1, 4)
	_, err = rs.AppendRunnerEvents(ctx, run, 1, mkEvents(0, 1))
	isErr(t, err, core.ErrInvalid)
	count(1, 4)

	// 6. Empty batch.
	ack, err = rs.AppendRunnerEvents(ctx, run, 1, nil)
	ok(t, err)
	eq(t, ack, int64(4), "ack of empty batch")

	// 7. Listing.
	part, err := rs.ListRunnerEvents(ctx, run, 1, 1, 2)
	ok(t, err)
	eq(t, fmt.Sprint(eventSeqs(part)), "[2 3]", "limited list")
	part, err = rs.ListRunnerEvents(ctx, run, 1, 4, 0)
	ok(t, err)
	eq(t, len(part), 0, "list after the last event")

	// 8. Missing session.
	_, err = rs.AppendRunnerEvents(ctx, run, 9, mkEvents(1, 1))
	isErr(t, err, core.ErrNotFound)

	// 10. Ended session, next session.
	endSession(t, rs, run, 1)
	ok(t, rs.CreateRunSession(ctx, &core.RunSession{RunID: run, Session: 2}))
	ack, err = rs.AppendRunnerEvents(ctx, run, 2, mkEvents(1, 2))
	ok(t, err)
	eq(t, ack, int64(2), "ack of session 2")
	count(1, 4)
	count(2, 2)
	_, err = rs.AppendRunnerEvents(ctx, run, 1, mkEvents(5, 5))
	isErr(t, err, core.ErrConflict)

	// 11. Concurrency.
	cs := mkSession(t, s, rs)
	const workers = 8
	var wg sync.WaitGroup
	type result struct {
		ack int64
		err error
	}
	results := make(chan result, workers)
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := rs.AppendRunnerEvents(ctx, cs.RunID, 1, mkEvents(1, 50))
			results <- result{a, err}
		}()
	}
	wg.Wait()
	close(results)
	for r := range results {
		ok(t, r.err)
		eq(t, r.ack, int64(50), "concurrent ack")
	}
	all, err := rs.ListRunnerEvents(ctx, cs.RunID, 1, 0, 0)
	ok(t, err)
	eq(t, len(all), 50, "concurrent events")
}

func testRunnerCommands(t *testing.T, s core.Store) {
	rs := runnerStoreOf(t, s)
	ss := mkSession(t, s, rs)
	run := ss.RunID

	// 1. Append.
	types := []string{"turn.start", "message.deliver", "turn.interrupt"}
	for i, ty := range types {
		c := &core.RunnerCommand{RunID: run, Session: 1, Type: ty, Data: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))}
		ok(t, rs.AppendRunnerCommand(ctx, c))
		eq(t, c.Seq, int64(i+1), "seq")
		if c.CreatedAt.IsZero() {
			t.Fatal("CreatedAt must be set")
		}
	}
	got, err := rs.GetRunSession(ctx, run, 1)
	ok(t, err)
	eq(t, got.CommandSeq, int64(3), "command seq")

	// 2. List.
	list, err := rs.ListRunnerCommands(ctx, run, 1, 1)
	ok(t, err)
	eq(t, len(list), 2, "commands after 1")
	for i, c := range list {
		eq(t, c.Seq, int64(i+2), "listed seq")
		eq(t, c.Type, types[i+1], "listed type")
		eq(t, c.RunID, run, "listed run id")
		eq(t, c.Session, 1, "listed session")
		jsonEq(t, c.Data, json.RawMessage(fmt.Sprintf(`{"i":%d}`, i+1)), "listed data")
	}
	list, err = rs.ListRunnerCommands(ctx, run, 1, 3)
	ok(t, err)
	eq(t, len(list), 0, "commands after 3")

	// 3. Ack.
	ackOf := func() int64 {
		t.Helper()
		g, err := rs.GetRunSession(ctx, run, 1)
		ok(t, err)
		return g.CommandAck
	}
	ok(t, rs.AckRunnerCommands(ctx, run, 1, 2))
	eq(t, ackOf(), int64(2), "ack 2")
	ok(t, rs.AckRunnerCommands(ctx, run, 1, 1))
	eq(t, ackOf(), int64(2), "ack stays 2")
	ok(t, rs.AckRunnerCommands(ctx, run, 1, 99))
	eq(t, ackOf(), int64(3), "ack capped to command seq")
	isErr(t, rs.AckRunnerCommands(ctx, run, 9, 1), core.ErrNotFound)

	// 4. Missing and ended sessions.
	isErr(t, rs.AppendRunnerCommand(ctx, &core.RunnerCommand{RunID: run, Session: 9, Type: "x"}), core.ErrNotFound)
	endSession(t, rs, run, 1)
	isErr(t, rs.AppendRunnerCommand(ctx, &core.RunnerCommand{RunID: run, Session: 1, Type: "x"}), core.ErrConflict)

	// 5. Concurrency.
	cs := mkSession(t, s, rs)
	const workers, per = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*per)
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := rs.AppendRunnerCommand(ctx, &core.RunnerCommand{RunID: cs.RunID, Session: 1, Type: "turn.start"}); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	all, err := rs.ListRunnerCommands(ctx, cs.RunID, 1, 0)
	ok(t, err)
	eq(t, len(all), workers*per, "commands")
	seqs := make([]int, len(all))
	for i, c := range all {
		seqs[i] = int(c.Seq)
	}
	sort.Ints(seqs)
	for i, v := range seqs {
		eq(t, v, i+1, "gapless seq")
	}
}

func inputSeqs(list []core.RunInput) string {
	out := make([]int64, len(list))
	for i, m := range list {
		out[i] = m.Seq
	}
	return fmt.Sprint(out)
}

func testRunInputs(t *testing.T, s core.Store) {
	rs := runnerStoreOf(t, s)

	// 1. Missing run.
	created, err := rs.AppendRunInput(ctx, &core.RunInput{RunID: uid("run"), Kind: "user"})
	isErr(t, err, core.ErrNotFound)
	eq(t, created, false, "created for a missing run")

	r := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, r))
	run := r.ID

	// 2. Three appends.
	var msgs []*core.RunInput
	for i := 1; i <= 3; i++ {
		m := &core.RunInput{
			RunID: run, Kind: "user", Content: fmt.Sprintf("hello %d", i),
			Sender:      core.RunInputSender{Type: "user", ID: "u1", Name: "Ann"},
			Data:        json.RawMessage(`{"a":1}`),
			Attachments: []string{"art_1", "art_2"},
			Untrusted:   true,
		}
		if i == 1 {
			m.State, m.Session, m.TurnID, m.Accepted = core.RunInputConsumed, 5, "x", true
		}
		created, err := rs.AppendRunInput(ctx, m)
		ok(t, err)
		eq(t, created, true, "created")
		eq(t, m.Seq, int64(i), "seq")
		if len(m.ID) < 4 || m.ID[:4] != "msg_" {
			t.Fatalf("ID %q must start with msg_", m.ID)
		}
		eq(t, m.State, core.RunInputPending, "state")
		eq(t, m.Session, 0, "session")
		eq(t, m.TurnID, "", "turn id")
		eq(t, m.Accepted, false, "accepted")
		if m.CreatedAt.IsZero() {
			t.Fatal("CreatedAt must be set")
		}
		msgs = append(msgs, m)
	}
	id1, id2, id3 := msgs[0].ID, msgs[1].ID, msgs[2].ID

	// 3. Round trip.
	got, err := rs.GetRunInput(ctx, id2)
	ok(t, err)
	eq(t, got.RunID, run, "run id")
	eq(t, got.Kind, "user", "kind")
	eq(t, got.Sender, core.RunInputSender{Type: "user", ID: "u1", Name: "Ann"}, "sender")
	eq(t, got.Content, "hello 2", "content")
	jsonEq(t, got.Data, json.RawMessage(`{"a":1}`), "data")
	eq(t, fmt.Sprint(got.Attachments), "[art_1 art_2]", "attachments")
	eq(t, got.Untrusted, true, "untrusted")
	_, err = rs.GetRunInput(ctx, uid("msg"))
	isErr(t, err, core.ErrNotFound)

	// 4. Dedupe.
	first := &core.RunInput{RunID: run, Kind: "user", Content: "first", DedupeKey: "k1"}
	created, err = rs.AppendRunInput(ctx, first)
	ok(t, err)
	eq(t, created, true, "created with key")
	eq(t, first.Seq, int64(4), "seq with key")
	dup := &core.RunInput{RunID: run, Kind: "user", Content: "second", DedupeKey: "k1"}
	created, err = rs.AppendRunInput(ctx, dup)
	ok(t, err)
	eq(t, created, false, "created again")
	eq(t, dup.ID, first.ID, "deduped id")
	eq(t, dup.Seq, int64(4), "deduped seq")
	eq(t, dup.Content, "first", "deduped content")
	all, err := rs.ListRunInputs(ctx, run, 0)
	ok(t, err)
	eq(t, len(all), 4, "messages after dedupe")
	r2 := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, r2))
	created, err = rs.AppendRunInput(ctx, &core.RunInput{RunID: r2.ID, Kind: "user", Content: "x", DedupeKey: "k1"})
	ok(t, err)
	eq(t, created, true, "same key in another run")

	// 5. Listing.
	eq(t, inputSeqs(all), "[1 2 3 4]", "all seqs")
	part, err := rs.ListRunInputs(ctx, run, 2)
	ok(t, err)
	eq(t, inputSeqs(part), "[3 4]", "seqs after 2")
	part, err = rs.ListRunInputs(ctx, run, 0, core.RunInputDelivered)
	ok(t, err)
	eq(t, len(part), 0, "delivered messages")

	// 6. Delivered.
	n, err := rs.MarkRunInputsDelivered(ctx, run, []string{id1, id2}, 1, "turn_a")
	ok(t, err)
	eq(t, n, 2, "delivered")
	n, err = rs.MarkRunInputsDelivered(ctx, run, []string{id1, id2}, 1, "turn_a")
	ok(t, err)
	eq(t, n, 0, "delivered again")
	got, err = rs.GetRunInput(ctx, id1)
	ok(t, err)
	eq(t, got.State, core.RunInputDelivered, "state")
	eq(t, got.Session, 1, "session")
	eq(t, got.TurnID, "turn_a", "turn id")
	eq(t, got.Accepted, false, "accepted")

	// 7. Accepted.
	n, err = rs.MarkRunInputsAccepted(ctx, run, []string{id1})
	ok(t, err)
	eq(t, n, 1, "accepted")
	n, err = rs.MarkRunInputsAccepted(ctx, run, []string{id3})
	ok(t, err)
	eq(t, n, 0, "accepted pending")

	// 8. Finish turn.
	consumed, returned, err := rs.FinishRunInputsOfTurn(ctx, run, "turn_a")
	ok(t, err)
	eq(t, consumed, 1, "consumed")
	eq(t, returned, 1, "returned")
	got, err = rs.GetRunInput(ctx, id1)
	ok(t, err)
	eq(t, got.State, core.RunInputConsumed, "id1 state")
	eq(t, got.Session, 1, "id1 session")
	eq(t, got.TurnID, "turn_a", "id1 turn id")
	got, err = rs.GetRunInput(ctx, id2)
	ok(t, err)
	eq(t, got.State, core.RunInputPending, "id2 state")
	eq(t, got.Session, 0, "id2 session")
	eq(t, got.TurnID, "", "id2 turn id")
	eq(t, got.Accepted, false, "id2 accepted")

	// 9. Accept by turn, reset by session.
	n, err = rs.MarkRunInputsDelivered(ctx, run, []string{id2, id3}, 2, "turn_b")
	ok(t, err)
	eq(t, n, 2, "delivered in turn b")
	n, err = rs.MarkRunInputsAcceptedByTurn(ctx, run, "turn_b")
	ok(t, err)
	eq(t, n, 2, "accepted by turn")
	n, err = rs.ResetRunInputsOfSession(ctx, run, 2)
	ok(t, err)
	eq(t, n, 2, "reset")
	for _, id := range []string{id2, id3} {
		got, err = rs.GetRunInput(ctx, id)
		ok(t, err)
		eq(t, got.State, core.RunInputPending, "reset state")
		eq(t, got.Session, 0, "reset session")
		eq(t, got.TurnID, "", "reset turn id")
		eq(t, got.Accepted, false, "reset accepted")
	}
	got, err = rs.GetRunInput(ctx, id1)
	ok(t, err)
	eq(t, got.State, core.RunInputConsumed, "id1 still consumed")

	// 10. Another run changes nothing.
	n, err = rs.MarkRunInputsDelivered(ctx, r2.ID, []string{id3}, 1, "t")
	ok(t, err)
	eq(t, n, 0, "delivered through another run")
	got, err = rs.GetRunInput(ctx, id3)
	ok(t, err)
	eq(t, got.State, core.RunInputPending, "id3 state")

	// 11. Empty ID lists.
	n, err = rs.MarkRunInputsDelivered(ctx, run, nil, 1, "t")
	ok(t, err)
	eq(t, n, 0, "empty delivered")
	n, err = rs.MarkRunInputsAccepted(ctx, run, []string{})
	ok(t, err)
	eq(t, n, 0, "empty accepted")

	// 12. Several states.
	both, err := rs.ListRunInputs(ctx, run, 0, core.RunInputPending, core.RunInputConsumed)
	ok(t, err)
	eq(t, len(both), 4, "pending and consumed")

	// 13. Concurrency.
	cr := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, cr))
	const workers, per = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*per)
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := rs.AppendRunInput(ctx, &core.RunInput{RunID: cr.ID, Kind: "user", Content: fmt.Sprintf("%d-%d", g, i)}); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	list, err := rs.ListRunInputs(ctx, cr.ID, 0)
	ok(t, err)
	eq(t, len(list), workers*per, "messages")
	seqs := make([]int, len(list))
	for i, m := range list {
		seqs[i] = int(m.Seq)
	}
	sort.Ints(seqs)
	for i, v := range seqs {
		eq(t, v, i+1, "gapless seq")
	}
}

func testRunnerTx(t *testing.T, s core.Store) {
	rs := runnerStoreOf(t, s)
	tx, isTx := s.(core.Transactor)
	if !isTx {
		t.Skip("store does not implement core.Transactor")
	}
	writes := func(ctx context.Context, runID string) error {
		if err := rs.CreateRunSession(ctx, &core.RunSession{RunID: runID, Session: 1, TaskID: uid("task")}); err != nil {
			return err
		}
		_, err := rs.AppendRunInput(ctx, &core.RunInput{RunID: runID, Kind: "user", Content: "hi"})
		return err
	}

	// 1. Rollback.
	rolled := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, rolled))
	boom := errors.New("boom")
	err := tx.InTx(ctx, func(ctx context.Context) error {
		if err := writes(ctx, rolled.ID); err != nil {
			return err
		}
		return boom
	})
	isErr(t, err, boom)
	_, err = rs.GetRunSession(ctx, rolled.ID, 1)
	isErr(t, err, core.ErrNotFound)
	list, err := rs.ListRunInputs(ctx, rolled.ID, 0)
	ok(t, err)
	eq(t, len(list), 0, "inputs after rollback")

	// 2. Commit.
	kept := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, kept))
	ok(t, tx.InTx(ctx, func(ctx context.Context) error { return writes(ctx, kept.ID) }))
	_, err = rs.GetRunSession(ctx, kept.ID, 1)
	ok(t, err)
	list, err = rs.ListRunInputs(ctx, kept.ID, 0)
	ok(t, err)
	eq(t, len(list), 1, "inputs after commit")
}

func testRunTimeline(t *testing.T, s core.Store) {
	rs := runnerStoreOf(t, s)
	r := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, r))

	// Missing run and missing event ID.
	_, err := rs.AppendTimelineEvent(ctx, &core.TimelineEvent{EventID: "e", RunID: uid("run"), Type: "run.queued", At: base})
	isErr(t, err, core.ErrNotFound)
	_, err = rs.AppendTimelineEvent(ctx, &core.TimelineEvent{RunID: r.ID, Type: "run.queued", At: base})
	isErr(t, err, core.ErrInvalid)

	// Append, then the same event ID again.
	ev := &core.TimelineEvent{EventID: "b", RunID: r.ID, Session: 1, Type: "session.started", At: base.Add(time.Second), Data: json.RawMessage(`{"runner_version":"1"}`)}
	created, err := rs.AppendTimelineEvent(ctx, ev)
	ok(t, err)
	eq(t, created, true, "created")
	again := *ev
	again.Type = "other"
	created, err = rs.AppendTimelineEvent(ctx, &again)
	ok(t, err)
	eq(t, created, false, "second write is ignored")

	// Order is At, then EventID.
	for _, e := range []core.TimelineEvent{
		{EventID: "z", Type: "run.queued", At: base},
		{EventID: "d", Type: "sandbox.assigned", At: base.Add(time.Second)},
		{EventID: "a", Type: "run.succeeded", At: base.Add(2 * time.Second)},
	} {
		e.RunID = r.ID
		created, err := rs.AppendTimelineEvent(ctx, &e)
		ok(t, err)
		eq(t, created, true, "created "+e.EventID)
	}
	list, err := rs.ListTimelineEvents(ctx, r.ID, time.Time{}, 0)
	ok(t, err)
	var ids []string
	for _, e := range list {
		ids = append(ids, e.EventID)
	}
	eq(t, fmt.Sprint(ids), "[z b d a]", "order")
	eq(t, list[1].Type, "session.started", "first write kept")
	eq(t, list[1].Session, 1, "session")
	var got struct {
		V string `json:"runner_version"`
	}
	ok(t, json.Unmarshal(list[1].Data, &got))
	eq(t, got.V, "1", "data")

	// Limit and afterAt.
	list, err = rs.ListTimelineEvents(ctx, r.ID, time.Time{}, 2)
	ok(t, err)
	eq(t, len(list), 2, "limit")
	eq(t, list[1].EventID, "b", "limit keeps the oldest")
	list, err = rs.ListTimelineEvents(ctx, r.ID, base.Add(time.Second), 0)
	ok(t, err)
	eq(t, len(list), 1, "after")
	eq(t, list[0].EventID, "a", "after")

	// Another run has its own events and the same event ID is allowed.
	o := &core.Run{FactoryID: anyFactory, ID: uid("run")}
	ok(t, s.CreateRun(ctx, o))
	created, err = rs.AppendTimelineEvent(ctx, &core.TimelineEvent{EventID: "a", RunID: o.ID, Type: "run.failed", At: base})
	ok(t, err)
	eq(t, created, true, "same ID in another run")
	list, err = rs.ListTimelineEvents(ctx, uid("none"), time.Time{}, 0)
	ok(t, err)
	eq(t, len(list), 0, "unknown run")
}
