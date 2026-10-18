package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

var _ core.RunnerStore = (*Store)(nil)

// errInputDedupe tells AppendRunInput that another writer stored the same
// dedupe key first.
var errInputDedupe = errors.New("run input dedupe race")

const sessionCols = "data, state, version, event_ack, command_seq, command_ack, last_seen_ns"

// scanSession reads one session. The columns win over the JSON document.
func (s *Store) scanSession(r interface{ Scan(...any) error }) (*core.RunSession, error) {
	var (
		data                              []byte
		state                             string
		version, eventAck, cmdSeq, cmdAck int64
		seen                              int64
	)
	if err := r.Scan(&data, &state, &version, &eventAck, &cmdSeq, &cmdAck, &seen); err != nil {
		return nil, s.mapErr(err)
	}
	v := new(core.RunSession)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("%s: decode record: %w", s.d.Name, err)
	}
	v.State = core.SessionState(state)
	v.Version, v.EventAck, v.CommandSeq, v.CommandAck = version, eventAck, cmdSeq, cmdAck
	v.LastSeenAt = fromNS(seen)
	return v, nil
}

func (s *Store) getSession(ctx context.Context, q querier, query string, args ...any) (*core.RunSession, error) {
	return s.scanSession(q.QueryRowContext(ctx, s.rebind(query), args...))
}

func (s *Store) sessionNotFound(runID string, session int) error {
	return fmt.Errorf("%w: session %d of run %s", core.ErrNotFound, session, runID)
}

// sessionExists reports whether the session is stored.
func (s *Store) sessionExists(ctx context.Context, q querier, runID string, session int) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, s.rebind("SELECT 1 FROM run_sessions WHERE run_id=? AND session=?"), runID, session).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, s.mapErr(err)
}

// CreateRunSession implements core.RunnerStore.
func (s *Store) CreateRunSession(ctx context.Context, in *core.RunSession) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, s.rebind("SELECT 1 FROM runs WHERE id=?"), in.RunID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: run %s", core.ErrNotFound, in.RunID)
		}
		if err != nil {
			return s.mapErr(err)
		}
		var highest, open int
		err = tx.QueryRowContext(ctx, s.rebind(
			"SELECT COALESCE(MAX(session),0), COALESCE(SUM(CASE WHEN state<>'ended' THEN 1 ELSE 0 END),0) FROM run_sessions WHERE run_id=?"),
			in.RunID).Scan(&highest, &open)
		if err != nil {
			return s.mapErr(err)
		}
		if open > 0 {
			return fmt.Errorf("%w: run %s has a session that is not ended", core.ErrConflict, in.RunID)
		}
		if in.Session != highest+1 {
			return fmt.Errorf("%w: session %d of run %s: next session is %d", core.ErrConflict, in.Session, in.RunID, highest+1)
		}
		v := *in
		v.State = core.SessionAssigned
		v.Version = 1
		v.EventAck, v.CommandSeq, v.CommandAck = 0, 0, 0
		defaultTime(&v.AssignedAt)
		defaultTime(&v.LastSeenAt)
		data, err := marshal(&v)
		if err != nil {
			return err
		}
		r := &row{}
		r.set("run_id", v.RunID).set("session", v.Session).set("state", string(v.State)).
			set("bootstrap_hash", v.BootstrapHash).set("version", v.Version).
			set("last_seen_ns", ns(v.LastSeenAt)).set("data", data)
		if err := s.insert(ctx, tx, "run_sessions", r); err != nil {
			return err
		}
		// Read the stored time back, so the argument matches later reads.
		v.LastSeenAt = fromNS(ns(v.LastSeenAt))
		*in = v
		return nil
	})
}

// GetRunSession implements core.RunnerStore.
func (s *Store) GetRunSession(ctx context.Context, runID string, session int) (*core.RunSession, error) {
	v, err := s.getSession(ctx, s.conn(ctx), "SELECT "+sessionCols+" FROM run_sessions WHERE run_id=? AND session=?", runID, session)
	if errors.Is(err, core.ErrNotFound) {
		return nil, s.sessionNotFound(runID, session)
	}
	return v, err
}

// GetRunSessionByBootstrapHash implements core.RunnerStore.
func (s *Store) GetRunSessionByBootstrapHash(ctx context.Context, hash string) (*core.RunSession, error) {
	notFound := fmt.Errorf("%w: session with this bootstrap hash", core.ErrNotFound)
	if hash == "" {
		return nil, notFound
	}
	v, err := s.getSession(ctx, s.conn(ctx), "SELECT "+sessionCols+" FROM run_sessions WHERE bootstrap_hash=?", hash)
	if errors.Is(err, core.ErrNotFound) {
		return nil, notFound
	}
	return v, err
}

// LatestRunSession implements core.RunnerStore.
func (s *Store) LatestRunSession(ctx context.Context, runID string) (*core.RunSession, error) {
	v, err := s.getSession(ctx, s.conn(ctx), "SELECT "+sessionCols+" FROM run_sessions WHERE run_id=? ORDER BY session DESC LIMIT 1", runID)
	if errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("%w: no session for run %s", core.ErrNotFound, runID)
	}
	return v, err
}

// ListRunSessions implements core.RunnerStore.
func (s *Store) ListRunSessions(ctx context.Context, runID string) ([]core.RunSession, error) {
	rows, err := s.conn(ctx).QueryContext(ctx, s.rebind("SELECT "+sessionCols+" FROM run_sessions WHERE run_id=? ORDER BY session"), runID)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := []core.RunSession{}
	for rows.Next() {
		v, err := s.scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, s.mapErr(rows.Err())
}

// ListOpenRunSessions implements core.RunnerStore.
func (s *Store) ListOpenRunSessions(ctx context.Context, limit int) ([]core.RunSession, error) {
	query := "SELECT " + sessionCols + " FROM run_sessions WHERE state IN (?,?) ORDER BY last_seen_ns, run_id, session"
	args := []any{string(core.SessionAssigned), string(core.SessionActive)}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.conn(ctx).QueryContext(ctx, s.rebind(query), args...)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := []core.RunSession{}
	for rows.Next() {
		v, err := s.scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, s.mapErr(rows.Err())
}

// UpdateRunSession implements core.RunnerStore.
func (s *Store) UpdateRunSession(ctx context.Context, in *core.RunSession) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		next := *in
		next.Version = in.Version + 1
		data, err := marshal(&next)
		if err != nil {
			return err
		}
		if s.d.SanitizeJSON != nil {
			data = s.d.SanitizeJSON(data)
		}
		res, err := s.exec(ctx, tx,
			"UPDATE run_sessions SET state=?, bootstrap_hash=?, data=?, version=version+1 WHERE run_id=? AND session=? AND version=?",
			string(in.State), in.BootstrapHash, data, in.RunID, in.Session, in.Version)
		if err != nil {
			return err
		}
		n, err := s.rowsAffected(res)
		if err != nil {
			return err
		}
		if n == 0 {
			ok, err := s.sessionExists(ctx, tx, in.RunID, in.Session)
			if err != nil {
				return err
			}
			if !ok {
				return s.sessionNotFound(in.RunID, in.Session)
			}
			return fmt.Errorf("%w: session %d of run %s: version %d is not the stored version", core.ErrConflict, in.Session, in.RunID, in.Version)
		}
		v, err := s.getSession(ctx, tx, "SELECT "+sessionCols+" FROM run_sessions WHERE run_id=? AND session=?", in.RunID, in.Session)
		if err != nil {
			return err
		}
		*in = *v
		return nil
	})
}

// TouchRunSession implements core.RunnerStore.
func (s *Store) TouchRunSession(ctx context.Context, runID string, session int, at time.Time) error {
	q := s.conn(ctx)
	res, err := s.exec(ctx, q, "UPDATE run_sessions SET last_seen_ns=? WHERE run_id=? AND session=? AND last_seen_ns<?",
		ns(at), runID, session, ns(at))
	if err != nil {
		return err
	}
	n, err := s.rowsAffected(res)
	if err != nil || n > 0 {
		return err
	}
	ok, err := s.sessionExists(ctx, q, runID, session)
	if err != nil {
		return err
	}
	if !ok {
		return s.sessionNotFound(runID, session)
	}
	return nil
}

// AppendRunnerEvents implements core.RunnerStore.
func (s *Store) AppendRunnerEvents(ctx context.Context, runID string, session int, events []core.RunnerEvent) (int64, error) {
	for i, e := range events {
		if e.Seq < 1 {
			return 0, fmt.Errorf("%w: event seq %d must be 1 or more", core.ErrInvalid, e.Seq)
		}
		if i > 0 && e.Seq != events[i-1].Seq+1 {
			return 0, fmt.Errorf("%w: event seq %d does not follow %d", core.ErrInvalid, e.Seq, events[i-1].Seq)
		}
	}
	var ack int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// The guard update locks the row for the rest of the transaction.
		var state string
		err := tx.QueryRowContext(ctx, s.rebind(
			"UPDATE run_sessions SET event_ack=event_ack WHERE run_id=? AND session=? RETURNING event_ack, state"),
			runID, session).Scan(&ack, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return s.sessionNotFound(runID, session)
		}
		if err != nil {
			return s.mapErr(err)
		}
		if core.SessionState(state) == core.SessionEnded {
			return fmt.Errorf("%w: session %d of run %s is ended", core.ErrConflict, session, runID)
		}
		rest := events
		for len(rest) > 0 && rest[0].Seq <= ack {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			return nil
		}
		if rest[0].Seq != ack+1 {
			return fmt.Errorf("%w: got seq %d, want %d", core.ErrSeqGap, rest[0].Seq, ack+1)
		}
		for _, e := range rest {
			e.RunID, e.Session = runID, session
			data, err := marshal(&e)
			if err != nil {
				return err
			}
			r := &row{}
			r.set("run_id", runID).set("session", session).set("seq", e.Seq).set("data", data)
			if err := s.insert(ctx, tx, "runner_events", r); err != nil {
				return err
			}
		}
		last := rest[len(rest)-1].Seq
		if _, err := s.exec(ctx, tx, "UPDATE run_sessions SET event_ack=?, last_seen_ns=? WHERE run_id=? AND session=?",
			last, ns(time.Now().UTC()), runID, session); err != nil {
			return err
		}
		ack = last
		return nil
	})
	return ack, err
}

// ListRunnerEvents implements core.RunnerStore.
func (s *Store) ListRunnerEvents(ctx context.Context, runID string, session int, afterSeq int64, limit int) ([]core.RunnerEvent, error) {
	query := "SELECT data FROM runner_events WHERE run_id=? AND session=? AND seq>? ORDER BY seq"
	args := []any{runID, session, afterSeq}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	return listDocs[core.RunnerEvent](ctx, s, s.conn(ctx), query, args...)
}

// AppendRunnerCommand implements core.RunnerStore.
func (s *Store) AppendRunnerCommand(ctx context.Context, c *core.RunnerCommand) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var seq int64
		err := tx.QueryRowContext(ctx, s.rebind(
			"UPDATE run_sessions SET command_seq=command_seq+1 WHERE run_id=? AND session=? AND state<>'ended' RETURNING command_seq"),
			c.RunID, c.Session).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			ok, err := s.sessionExists(ctx, tx, c.RunID, c.Session)
			if err != nil {
				return err
			}
			if !ok {
				return s.sessionNotFound(c.RunID, c.Session)
			}
			return fmt.Errorf("%w: session %d of run %s is ended", core.ErrConflict, c.Session, c.RunID)
		}
		if err != nil {
			return s.mapErr(err)
		}
		v := *c
		v.Seq = seq
		defaultTime(&v.CreatedAt)
		data, err := marshal(&v)
		if err != nil {
			return err
		}
		r := &row{}
		r.set("run_id", v.RunID).set("session", v.Session).set("seq", seq).set("data", data)
		if err := s.insert(ctx, tx, "runner_commands", r); err != nil {
			return err
		}
		*c = v
		return nil
	})
}

// ListRunnerCommands implements core.RunnerStore.
func (s *Store) ListRunnerCommands(ctx context.Context, runID string, session int, afterSeq int64) ([]core.RunnerCommand, error) {
	return listDocs[core.RunnerCommand](ctx, s, s.conn(ctx),
		"SELECT data FROM runner_commands WHERE run_id=? AND session=? AND seq>? ORDER BY seq", runID, session, afterSeq)
}

// AckRunnerCommands implements core.RunnerStore.
func (s *Store) AckRunnerCommands(ctx context.Context, runID string, session int, ack int64) error {
	q := s.conn(ctx)
	// The CASE form is the smaller of ack and command_seq on both dialects.
	const least = "CASE WHEN ? < command_seq THEN ? ELSE command_seq END"
	res, err := s.exec(ctx, q,
		"UPDATE run_sessions SET command_ack = "+least+" WHERE run_id=? AND session=? AND command_ack < "+least,
		ack, ack, runID, session, ack, ack)
	if err != nil {
		return err
	}
	n, err := s.rowsAffected(res)
	if err != nil || n > 0 {
		return err
	}
	ok, err := s.sessionExists(ctx, q, runID, session)
	if err != nil {
		return err
	}
	if !ok {
		return s.sessionNotFound(runID, session)
	}
	return nil
}

const inputCols = "data, state, session, turn_id, accepted"

// scanInput reads one message. The columns win over the JSON document.
func (s *Store) scanInput(r interface{ Scan(...any) error }) (*core.RunInput, error) {
	var (
		data     []byte
		state    string
		session  int
		turnID   string
		accepted bool
	)
	if err := r.Scan(&data, &state, &session, &turnID, &accepted); err != nil {
		return nil, s.mapErr(err)
	}
	v := new(core.RunInput)
	if err := json.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("%s: decode record: %w", s.d.Name, err)
	}
	v.State = core.RunInputState(state)
	v.Session, v.TurnID, v.Accepted = session, turnID, accepted
	return v, nil
}

// AppendRunInput implements core.RunnerStore.
func (s *Store) AppendRunInput(ctx context.Context, m *core.RunInput) (bool, error) {
	byKey := func(q querier) (*core.RunInput, error) {
		return s.scanInput(q.QueryRowContext(ctx,
			s.rebind("SELECT "+inputCols+" FROM run_inputs WHERE run_id=? AND dedupe_key=?"), m.RunID, m.DedupeKey))
	}
	var existing *core.RunInput
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if m.DedupeKey != "" {
			v, err := byKey(tx)
			if err == nil {
				existing = v
				return nil
			}
			if !errors.Is(err, core.ErrNotFound) {
				return err
			}
		}
		var seq int64
		err := tx.QueryRowContext(ctx, s.rebind("UPDATE runs SET input_seq=input_seq+1 WHERE id=? RETURNING input_seq"), m.RunID).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: run %s", core.ErrNotFound, m.RunID)
		}
		if err != nil {
			return s.mapErr(err)
		}
		v := *m
		v.Seq = seq
		v.State = core.RunInputPending
		v.Session, v.TurnID, v.Accepted = 0, "", false
		defaultID(&v.ID, "msg")
		defaultTime(&v.CreatedAt)
		data, err := marshal(&v)
		if err != nil {
			return err
		}
		r := &row{}
		r.set("id", v.ID).set("run_id", v.RunID).set("seq", seq).set("state", string(v.State)).
			set("session", 0).set("turn_id", "").set("accepted", false).
			set("dedupe_key", v.DedupeKey).set("data", data)
		if err := s.insert(ctx, tx, "run_inputs", r); err != nil {
			if errors.Is(err, core.ErrConflict) && v.DedupeKey != "" {
				return errInputDedupe
			}
			return err
		}
		*m = v
		return nil
	})
	switch {
	case errors.Is(err, errInputDedupe):
		v, err := byKey(s.conn(ctx))
		if err != nil {
			return false, err
		}
		*m = *v
		return false, nil
	case err != nil:
		return false, err
	case existing != nil:
		*m = *existing
		return false, nil
	}
	return true, nil
}

// GetRunInput implements core.RunnerStore.
func (s *Store) GetRunInput(ctx context.Context, id string) (*core.RunInput, error) {
	v, err := s.scanInput(s.conn(ctx).QueryRowContext(ctx, s.rebind("SELECT "+inputCols+" FROM run_inputs WHERE id=?"), id))
	if errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("%w: message %s", core.ErrNotFound, id)
	}
	return v, err
}

// ListRunInputs implements core.RunnerStore.
func (s *Store) ListRunInputs(ctx context.Context, runID string, afterSeq int64, states ...core.RunInputState) ([]core.RunInput, error) {
	w := &where{}
	w.add("run_id=?", runID)
	w.add("seq>?", afterSeq)
	names := make([]string, len(states))
	for i, st := range states {
		names[i] = string(st)
	}
	w.in("state", names)
	rows, err := s.conn(ctx).QueryContext(ctx, s.rebind("SELECT "+inputCols+" FROM run_inputs"+w.sql()+" ORDER BY seq"), w.args...)
	if err != nil {
		return nil, s.mapErr(err)
	}
	defer rows.Close()
	out := []core.RunInput{}
	for rows.Next() {
		v, err := s.scanInput(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, s.mapErr(rows.Err())
}

// updateInputs runs one UPDATE on run_inputs and returns the row count.
func (s *Store) updateInputs(ctx context.Context, q querier, set string, w *where, setArgs ...any) (int, error) {
	res, err := s.exec(ctx, q, "UPDATE run_inputs SET "+set+w.sql(), append(setArgs, w.args...)...)
	if err != nil {
		return 0, err
	}
	n, err := s.rowsAffected(res)
	return int(n), err
}

const putBackSet = "state='pending', session=0, turn_id='', accepted=?"

// MarkRunInputsDelivered implements core.RunnerStore.
func (s *Store) MarkRunInputsDelivered(ctx context.Context, runID string, ids []string, session int, turnID string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	w := &where{}
	w.add("run_id=?", runID)
	w.add("state='pending'")
	w.in("id", ids)
	return s.updateInputs(ctx, s.conn(ctx), "state='delivered', session=?, turn_id=?, accepted=?", w, session, turnID, false)
}

// MarkRunInputsAccepted implements core.RunnerStore.
func (s *Store) MarkRunInputsAccepted(ctx context.Context, runID string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	w := &where{}
	w.add("run_id=?", runID)
	w.add("state='delivered'")
	w.add("accepted=?", false)
	w.in("id", ids)
	return s.updateInputs(ctx, s.conn(ctx), "accepted=?", w, true)
}

// MarkRunInputsAcceptedByTurn implements core.RunnerStore.
func (s *Store) MarkRunInputsAcceptedByTurn(ctx context.Context, runID string, turnID string) (int, error) {
	w := &where{}
	w.add("run_id=?", runID)
	w.add("turn_id=?", turnID)
	w.add("state='delivered'")
	w.add("accepted=?", false)
	return s.updateInputs(ctx, s.conn(ctx), "accepted=?", w, true)
}

// FinishRunInputsOfTurn implements core.RunnerStore.
func (s *Store) FinishRunInputsOfTurn(ctx context.Context, runID string, turnID string) (int, int, error) {
	var consumed, returned int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		w := &where{}
		w.add("run_id=?", runID)
		w.add("turn_id=?", turnID)
		w.add("state='delivered'")
		w.add("accepted=?", true)
		var err error
		if consumed, err = s.updateInputs(ctx, tx, "state='consumed'", w); err != nil {
			return err
		}
		w = &where{}
		w.add("run_id=?", runID)
		w.add("turn_id=?", turnID)
		w.add("state='delivered'")
		returned, err = s.updateInputs(ctx, tx, putBackSet, w, false)
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return consumed, returned, nil
}

// ResetRunInputsOfSession implements core.RunnerStore.
func (s *Store) ResetRunInputsOfSession(ctx context.Context, runID string, session int) (int, error) {
	w := &where{}
	w.add("run_id=?", runID)
	w.add("session=?", session)
	w.add("state='delivered'")
	return s.updateInputs(ctx, s.conn(ctx), putBackSet, w, false)
}

// AppendTimelineEvent implements core.RunnerStore.
func (s *Store) AppendTimelineEvent(ctx context.Context, ev *core.TimelineEvent) (bool, error) {
	if ev.EventID == "" {
		return false, fmt.Errorf("%w: timeline event needs an event_id", core.ErrInvalid)
	}
	defaultTime(&ev.At)
	v := *ev
	v.At = fromNS(ns(v.At))
	data, err := marshal(&v)
	if err != nil {
		return false, err
	}
	r := s.prep((&row{}).set("run_id", v.RunID).set("event_id", v.EventID).set("session", v.Session).
		set("type", v.Type).set("at_ns", ns(v.At)).set("data", data))
	res, err := s.exec(ctx, s.conn(ctx),
		"INSERT INTO run_timeline (run_id, event_id, session, type, at_ns, data) VALUES (?,?,?,?,?,?) ON CONFLICT (run_id, event_id) DO NOTHING",
		r.args...)
	if err != nil {
		return false, err
	}
	n, err := s.rowsAffected(res)
	if err != nil {
		return false, err
	}
	if n > 0 {
		*ev = v
	}
	return n > 0, nil
}

// ListTimelineEvents implements core.RunnerStore.
func (s *Store) ListTimelineEvents(ctx context.Context, runID string, afterAt time.Time, limit int) ([]core.TimelineEvent, error) {
	query := "SELECT data FROM run_timeline WHERE run_id=? AND at_ns>? ORDER BY at_ns, event_id"
	args := []any{runID, ns(afterAt)}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	return listDocs[core.TimelineEvent](ctx, s, s.conn(ctx), query, args...)
}
