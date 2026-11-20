package dispatch

import (
	"context"
	"encoding/json"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runstate"
)

// SessionStarted moves the run from pending to claimed.
func (d *Dispatcher) SessionStarted(ctx context.Context, s *core.RunSession) {
	if _, _, err := d.transition(ctx, s.RunID, runstate.Input{Event: runstate.EventSessionStarted}, nil); err != nil {
		d.log.Warn("dispatch: session started", "run", s.RunID, "err", err)
	}
	d.timeline(ctx, s.RunID, s.Session, "session.started", "session.started:"+s.TaskID,
		map[string]any{"runner_version": s.RunnerVersion})
}

// EventsStored reacts to the events of a runner, in order.
func (d *Dispatcher) EventsStored(ctx context.Context, s *core.RunSession, events []core.RunnerEvent) {
	wake := false
	for _, e := range events {
		d.handleEvent(ctx, s, e)
		switch runnerproto.EventType(e.Type) {
		case runnerproto.EventSessionReady, runnerproto.EventTurnEnded:
			wake = true
		}
	}
	if wake {
		d.deliver(ctx, s.RunID)
	}
}

func (d *Dispatcher) handleEvent(ctx context.Context, s *core.RunSession, e core.RunnerEvent) {
	var err error
	switch runnerproto.EventType(e.Type) {
	case runnerproto.EventMessage:
		err = d.onMessage(ctx, s.RunID, e)
	case runnerproto.EventToolCall:
		err = d.onToolCall(ctx, s.RunID, e)
	case runnerproto.EventToolResult:
		err = d.onToolResult(ctx, s.RunID, e)
	case runnerproto.EventHarnessSession:
		err = d.onHarnessSession(ctx, s.RunID, e)
	case runnerproto.EventUsage:
		err = d.onUsage(ctx, s.RunID, s.Session, e)
	case runnerproto.EventArtifact:
		err = d.onArtifact(ctx, s, e)
	case runnerproto.EventTranscriptSaved:
		err = d.onTranscriptSaved(ctx, s, e)
	case runnerproto.EventTurnStarted:
		_, _, err = d.transition(ctx, s.RunID, runstate.Input{Event: runstate.EventTurnStarted}, nil)
	case runnerproto.EventTurnEnded:
		err = d.onTurnEnded(ctx, s.RunID, e)
	}
	if err != nil {
		d.log.Warn("dispatch: handle event", "run", s.RunID, "type", e.Type, "seq", e.Seq, "err", err)
	}
}

func (d *Dispatcher) appendMessage(ctx context.Context, runID, role, content string, e core.RunnerEvent) error {
	m := &core.RunMessage{
		RunID:   runID,
		Role:    role,
		Content: content,
		Data:    append(json.RawMessage(nil), e.Data...),
	}
	if err := d.cfg.Store.AppendRunMessage(ctx, m); err != nil {
		return err
	}
	if d.cfg.OnRunMessage != nil {
		d.cfg.OnRunMessage(ctx, m)
	}
	return nil
}

func (d *Dispatcher) onMessage(ctx context.Context, runID string, e core.RunnerEvent) error {
	var data runnerproto.MessageData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	role := "assistant"
	if data.Role == "system" {
		role = "system"
	}
	return d.appendMessage(ctx, runID, role, data.Content, e)
}

func (d *Dispatcher) onToolCall(ctx context.Context, runID string, e core.RunnerEvent) error {
	var data runnerproto.ToolCallData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	return d.appendMessage(ctx, runID, "tool_call", data.Name, e)
}

func (d *Dispatcher) onToolResult(ctx context.Context, runID string, e core.RunnerEvent) error {
	var data runnerproto.ToolResultData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	return d.appendMessage(ctx, runID, "tool_result", data.Content, e)
}

func (d *Dispatcher) onHarnessSession(ctx context.Context, runID string, e core.RunnerEvent) error {
	var data runnerproto.HarnessSessionData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	_, err := d.update(ctx, runID, func(r *core.Run) { r.SessionID = data.SessionID })
	return err
}

func (d *Dispatcher) onUsage(ctx context.Context, runID string, session int, e core.RunnerEvent) error {
	var data runnerproto.UsageData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	if !data.Cumulative {
		return nil
	}
	// A cumulative event replaces the usage of its session only. The run
	// gets the sum over all its sessions, and is partial when one session
	// is partial. A runner older than duration_ms sends no duration.
	_, _, err := d.updateSession(ctx, runID, session, func(s *core.RunSession) bool {
		s.Usage = core.Usage{
			InputTokens:  data.InputTokens,
			OutputTokens: data.OutputTokens,
			CostUSD:      data.CostUSD,
			DurationMS:   data.DurationMS,
		}
		if data.Coverage == core.UsagePartial {
			s.Usage.Coverage = core.UsagePartial
		}
		return true
	})
	if err != nil {
		return err
	}
	sessions, err := d.cfg.Runner.ListRunSessions(ctx, runID)
	if err != nil {
		return err
	}
	var total core.Usage
	for _, s := range sessions {
		total.InputTokens += s.Usage.InputTokens
		total.OutputTokens += s.Usage.OutputTokens
		total.CostUSD += s.Usage.CostUSD
		total.DurationMS += s.Usage.DurationMS
		if s.Usage.Coverage == core.UsagePartial {
			total.Coverage = core.UsagePartial
		}
	}
	_, err = d.update(ctx, runID, func(r *core.Run) {
		r.Usage = total
	})
	return err
}

func (d *Dispatcher) onTurnEnded(ctx context.Context, runID string, e core.RunnerEvent) error {
	var data runnerproto.TurnEndedData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	open, err := d.hasOpenRequest(ctx, runID)
	if err != nil {
		d.log.Warn("dispatch: check open requests", "run", runID, "err", err)
		open = false
	}
	in := runstate.Input{
		Event:          runstate.EventTurnEnded,
		Outcome:        data.Outcome,
		FailureCode:    core.FailureCode(data.FailureCode),
		HasOpenRequest: open,
	}
	r, _, err := d.transition(ctx, runID, in, func(r *core.Run) {
		r.Turns++
		if data.Result != "" {
			r.Result = data.Result
		}
		if data.Outcome == runnerproto.OutcomeError {
			setFailureMessage(r, data.Message)
		}
	})
	if err == nil && r != nil {
		d.spendAtTurnEnd(ctx, r)
	}
	return err
}

// SessionEnded reacts to the end request of a runner. The service already
// reset the message queue of the session.
func (d *Dispatcher) SessionEnded(ctx context.Context, s *core.RunSession, req runnerproto.EndRequest) {
	if err := d.applySessionEnd(ctx, s, req); err != nil {
		d.log.Warn("dispatch: session ended", "run", s.RunID, "reason", req.Reason, "err", err)
	}
	d.timelineSessionEnded(ctx, s.RunID, s.Session, string(req.Reason), "")
	d.destroy(ctx, s)
}

func (d *Dispatcher) applySessionEnd(ctx context.Context, s *core.RunSession, req runnerproto.EndRequest) error {
	lost := func(code core.FailureCode) error {
		in := runstate.Input{
			Event:        runstate.EventSessionLost,
			FailureCode:  code,
			PriorRetries: d.priorRetries(ctx, s.RunID, s.Session),
		}
		_, _, err := d.transition(ctx, s.RunID, in, nil)
		return err
	}
	switch req.Reason {
	case runnerproto.EndSetupFailed:
		in := runstate.Input{Event: runstate.EventSetupFailed, FailureCode: core.FailureCode(req.FailureCode)}
		_, _, err := d.transition(ctx, s.RunID, in, func(r *core.Run) { setFailureMessage(r, req.Message) })
		return err
	case runnerproto.EndFatal:
		return lost(core.FailureAgentProcessFailed)
	case runnerproto.EndIdle, runnerproto.EndStopped, runnerproto.EndDeadline:
		r, err := d.cfg.Store.GetRun(ctx, s.RunID)
		if err != nil {
			return err
		}
		// A turn did not end, or no turn ran yet.
		if r.State == core.RunRunning || r.State == core.RunClaimed {
			return lost(core.FailureRunnerLost)
		}
	}
	return nil
}
