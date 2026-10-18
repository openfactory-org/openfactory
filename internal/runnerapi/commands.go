package runnerapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// sessionKey names a session in the wake registry.
type sessionKey struct {
	run     string
	session int
}

// wakeRegistry lets a long poll wait for a signal of its session.
type wakeRegistry struct {
	mu   sync.Mutex
	subs map[sessionKey][]chan struct{}
}

func newWakeRegistry() *wakeRegistry {
	return &wakeRegistry{subs: make(map[sessionKey][]chan struct{})}
}

// subscribe returns a channel that is closed at the next wake of the session,
// and a function that removes the subscription.
func (w *wakeRegistry) subscribe(k sessionKey) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	w.mu.Lock()
	w.subs[k] = append(w.subs[k], ch)
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		list := w.subs[k]
		for i, c := range list {
			if c == ch {
				list = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(list) == 0 {
			delete(w.subs, k)
		} else {
			w.subs[k] = list
		}
	}
}

// notify closes and removes the channels of the session.
func (w *wakeRegistry) notify(k sessionKey) {
	w.mu.Lock()
	list := w.subs[k]
	delete(w.subs, k)
	w.mu.Unlock()
	for _, ch := range list {
		close(ch)
	}
}

// EnqueueCommand appends a command for a session and wakes the long polls
// of this process. Other replicas see it at their next poll interval.
func (s *Service) EnqueueCommand(ctx context.Context, runID string, session int, typ runnerproto.CommandType, data any) (core.RunnerCommand, error) {
	if !typ.Valid() {
		return core.RunnerCommand{}, errors.Join(core.ErrInvalid, errors.New("unknown command type "+string(typ)))
	}
	enc, err := runnerproto.NewCommand(0, typ, data)
	if err != nil {
		return core.RunnerCommand{}, errors.Join(core.ErrInvalid, err)
	}
	c := core.RunnerCommand{RunID: runID, Session: session, Type: string(typ), Data: enc.Data}
	if err := s.runner.AppendRunnerCommand(ctx, &c); err != nil {
		return core.RunnerCommand{}, err
	}
	s.wake.notify(sessionKey{runID, session})
	return c, nil
}

func (s *Service) handleCommands(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	after, wait, aerr := parseCommandQuery(r)
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	ctx := r.Context()
	if after > 0 {
		if err := s.runner.AckRunnerCommands(ctx, sess.RunID, sess.Session, after); err != nil {
			writeError(w, errInternal("ack commands", err))
			return
		}
	}
	cmds, aerr := s.pollCommands(ctx, sess, after, wait)
	if ctx.Err() != nil {
		return
	}
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	writeJSON(w, http.StatusOK, runnerproto.CommandsResponse{Commands: cmds})
}

func parseCommandQuery(r *http.Request) (after int64, wait time.Duration, aerr *apiError) {
	q := r.URL.Query()
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, errBad("invalid after %q", v)
		}
		after = n
	}
	secs := 0
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, 0, errBad("invalid wait %q", v)
		}
		secs = n
	}
	return after, time.Duration(runnerproto.ClampWait(secs)) * time.Second, nil
}

// pollCommands returns the commands after a sequence number. It waits up to
// wait for a command. It returns an empty, non-nil slice if the wait ends.
// If the request context ends, it returns with a nil slice and no error.
func (s *Service) pollCommands(ctx context.Context, sess *core.RunSession, after int64, wait time.Duration) ([]runnerproto.Command, *apiError) {
	key := sessionKey{sess.RunID, sess.Session}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		// Subscribe before the read, so that no wake is lost between them.
		woken, unsubscribe := s.wake.subscribe(key)
		cmds, err := s.runner.ListRunnerCommands(ctx, sess.RunID, sess.Session, after)
		if err != nil {
			unsubscribe()
			if ctx.Err() != nil {
				return nil, nil
			}
			return nil, errInternal("list commands", err)
		}
		if len(cmds) > 0 {
			unsubscribe()
			return toProtoCommands(cmds), nil
		}
		if wait <= 0 {
			unsubscribe()
			return []runnerproto.Command{}, nil
		}
		tick := time.NewTimer(s.pollInterval)
		select {
		case <-woken:
		case <-tick.C:
			if _, aerr := s.activeSession(ctx, sess.RunID, sess.Session); aerr != nil {
				unsubscribe()
				if ctx.Err() != nil {
					return nil, nil
				}
				return nil, aerr
			}
		case <-deadline.C:
			tick.Stop()
			unsubscribe()
			return []runnerproto.Command{}, nil
		case <-ctx.Done():
			tick.Stop()
			unsubscribe()
			return nil, nil
		}
		tick.Stop()
		unsubscribe()
	}
}

func toProtoCommands(in []core.RunnerCommand) []runnerproto.Command {
	out := make([]runnerproto.Command, len(in))
	for i, c := range in {
		out[i] = runnerproto.Command{Seq: c.Seq, Type: runnerproto.CommandType(c.Type), Data: c.Data}
	}
	return out
}
