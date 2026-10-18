package runner

import (
	"context"
	"errors"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// endInfo says how the session ends.
type endInfo struct {
	reason     runnerproto.EndReason
	code       string
	msg        string
	timeout    time.Duration // time budget for the final flush and end calls
	superseded bool          // a newer session exists: end without a call
	err        error         // fail without an end call
}

// commandLoop handles commands, the idle timer and the session deadline
// until the session must end.
func (r *runner) commandLoop(ctx context.Context, startedAt time.Time) endInfo {
	pctx, pcancel := context.WithCancel(ctx)
	defer pcancel()
	batches := make(chan []runnerproto.Command)
	pollErr := make(chan error, 1)
	go r.poll(pctx, batches, pollErr)

	var (
		active    *turn
		idleTimer *time.Timer
		idleC     <-chan time.Time
		deadlineC <-chan time.Time
		handled   int64
	)
	if s := r.start.Limits.MaxSessionSeconds; s > 0 {
		d := time.Duration(s)*time.Second - r.now().Sub(startedAt)
		if d < 0 {
			d = 0
		}
		t := time.NewTimer(d)
		defer t.Stop()
		deadlineC = t.C
	}
	// startIdle starts the idle timer. A timeout of 0 from the server ends the
	// session when a turn ends. Before the first turn it does not apply: the
	// session waits for that turn as long as a timeout that is not 0 would.
	startIdle := func(afterTurn bool) {
		secs := r.start.IdleTimeoutSeconds
		if secs < 0 || (secs == 0 && (!r.start.IdleTimeoutSet || !afterTurn)) {
			secs = defaultIdleTimeout
		}
		d := time.Duration(secs) * time.Second
		r.emit(runnerproto.EventIdle, "", runnerproto.IdleData{Deadline: r.now().Add(d).UTC()})
		idleTimer = time.NewTimer(d)
		idleC = idleTimer.C
	}
	stopIdle := func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		idleC = nil
	}
	defer stopIdle()
	// stopTurn cancels the active turn and waits for its end.
	stopTurn := func() {
		if active != nil {
			active.stop()
			<-active.done
			active = nil
		}
	}
	startIdle(false)

	for {
		var doneC <-chan struct{}
		if active != nil {
			doneC = active.done
		}
		select {
		case <-ctx.Done():
			stopTurn()
			info, _ := r.interrupted(ctx)
			return info
		case err := <-pollErr:
			stopTurn()
			return endInfo{err: err}
		case <-doneC:
			active = nil
			startIdle(true)
		case <-idleC:
			idleC = nil
			return endInfo{reason: runnerproto.EndIdle, timeout: endTimeout}
		case <-deadlineC:
			stopTurn()
			return endInfo{reason: runnerproto.EndDeadline, timeout: endTimeout}
		case cmds := <-batches:
			for _, c := range cmds {
				if c.Seq <= handled {
					continue
				}
				handled = c.Seq
				switch c.Type {
				case runnerproto.CommandTurnStart:
					var d runnerproto.TurnStartData
					if err := c.Decode(&d); err != nil || d.TurnID == "" {
						r.diagnostic("bad_command", "turn.start has no valid data")
						continue
					}
					if active != nil {
						r.diagnostic("turn_active", "a turn is active: turn "+d.TurnID+" was not started")
						continue
					}
					stopIdle()
					active = r.startTurn(d)
				case runnerproto.CommandMessageDeliver:
					var d runnerproto.MessageDeliverData
					if err := c.Decode(&d); err != nil {
						r.diagnostic("bad_command", "message.deliver has no valid data")
						continue
					}
					if active != nil {
						r.deliver(active, d.Input)
					}
				case runnerproto.CommandTurnInterrupt:
					var d runnerproto.TurnInterruptData
					if err := c.Decode(&d); err != nil {
						r.diagnostic("bad_command", "turn.interrupt has no valid data")
						continue
					}
					if active != nil && active.id == d.TurnID {
						active.stop()
					}
				case runnerproto.CommandSessionStop:
					var d runnerproto.SessionStopData
					_ = c.Decode(&d)
					stopTurn()
					reason := runnerproto.EndStopped
					if d.Reason == "deadline" {
						reason = runnerproto.EndDeadline
					}
					return endInfo{reason: reason, timeout: endTimeout}
				case runnerproto.CommandSessionCancel:
					stopTurn()
					return endInfo{reason: runnerproto.EndCancelled, timeout: endTimeout}
				}
			}
		}
	}
}

func (r *runner) diagnostic(code, msg string) {
	r.emit(runnerproto.EventDiagnostic, "", runnerproto.DiagnosticData{Code: code, Message: msg})
}

// poll asks for commands in a loop and sends each batch to out. It refreshes
// the session token before it expires.
func (r *runner) poll(ctx context.Context, out chan<- []runnerproto.Command, errc chan<- error) {
	var after int64
	delay := minDelay
	expires := r.start.SessionTokenExpiresAt
	fail := func(err error) bool {
		switch {
		case ctx.Err() != nil:
		case runnerclient.IsSuperseded(err):
			r.markSuperseded()
		default:
			errc <- err
		}
		return true
	}
	for ctx.Err() == nil {
		if expires.Sub(r.now()) < tokenRefreshBefore {
			resp, err := r.client.RefreshToken(ctx)
			switch {
			case err == nil:
				expires = resp.SessionTokenExpiresAt
				r.keepSession()
			case runnerclient.IsRetryable(err):
				r.log.Warn("refresh token", "error", err)
			default:
				fail(err)
				return
			}
		}
		cmds, err := r.client.Commands(ctx, after, r.cfg.PollWait)
		if err != nil {
			if ctx.Err() != nil || !runnerclient.IsRetryable(err) {
				fail(err)
				return
			}
			r.log.Warn("get commands", "error", err, "retry_in", delay)
			if sleepCtx(ctx, delay) != nil {
				return
			}
			delay = nextDelay(delay)
			continue
		}
		delay = minDelay
		if len(cmds) == 0 {
			continue
		}
		select {
		case out <- cmds:
		case <-ctx.Done():
			return
		}
		for _, c := range cmds {
			after = max(after, c.Seq)
		}
	}
}

// finish flushes the events and ends the session. It returns nil if the end
// call succeeded or a newer session exists.
func (r *runner) finish(info endInfo) error {
	if info.superseded {
		return nil
	}
	if info.err != nil {
		if runnerclient.IsSuperseded(info.err) {
			return nil
		}
		return info.err
	}
	// A last read, so that a block shortly before the end is not lost.
	if r.ready {
		r.reportEgress(context.Background())
	}
	if r.ready && r.start.Workspace.Snapshot.Enabled && snapshotReason(info.reason) {
		r.captureSnapshot()
	}
	r.stopFlusher()
	// The message goes to the run: the redactor runs over it as over events.
	info.msg = redactString(info.msg, redactTerms(r.redact))
	ctx, cancel := context.WithTimeout(context.Background(), info.timeout)
	defer cancel()
	gaps := 0
	delay := minDelay
	for {
		if err := r.flushAll(ctx); err != nil {
			return endResult(err)
		}
		err := r.client.End(ctx, runnerproto.EndRequest{
			Reason:       info.reason,
			LastEventSeq: r.events.LastSeq(),
			FailureCode:  info.code,
			Message:      info.msg,
		})
		var se *runnerclient.StatusError
		switch {
		case err == nil:
			return nil
		case errors.Is(err, runnerclient.ErrGap):
			if gaps++; gaps >= maxEndGapTries {
				return err
			}
			if errors.As(err, &se) && se.Ack != nil {
				r.sender.SetAcked(*se.Ack)
			}
		case runnerclient.IsRetryable(err):
			if serr := sleepCtx(ctx, delay); serr != nil {
				return err
			}
			delay = nextDelay(delay)
		default:
			return endResult(err)
		}
	}
}

// flushAll sends events until the control plane has all of them. It retries
// with a growing delay until ctx is done.
func (r *runner) flushAll(ctx context.Context) error {
	delay := minDelay
	for {
		err := r.sender.Flush(ctx)
		if err == nil && r.sender.Acked() >= r.events.LastSeq() {
			return nil
		}
		if err != nil && (runnerclient.IsSuperseded(err) || runnerclient.IsUnauthorized(err) || ctx.Err() != nil) {
			return err
		}
		if serr := sleepCtx(ctx, delay); serr != nil {
			return serr
		}
		delay = nextDelay(delay)
	}
}

// endResult turns the error of an end step into the result of Run.
func endResult(err error) error {
	if runnerclient.IsSuperseded(err) {
		return nil
	}
	return err
}
