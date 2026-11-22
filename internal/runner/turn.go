package runner

import (
	"context"
	"sync/atomic"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/netlock"
)

// Harness runs the turns of one agent CLI or loop.
type Harness interface {
	// Capabilities returns the capabilities of the harness in the runner protocol.
	Capabilities() []runnerproto.Capability
	// Turn runs one turn. It sends events with emit. It returns when the
	// turn ends or ctx is done. If ctx is done because of an interrupt, it
	// returns the outcome interrupted.
	Turn(ctx context.Context, req TurnRequest, emit Emitter) TurnResult
}

// MidTurnInput is an optional interface of a Harness with the capability mid_turn_input.
type MidTurnInput interface {
	// Deliver gives messages to the active turn. It returns the IDs that the harness accepted.
	Deliver(ctx context.Context, items []runnerproto.InputItem) []string
}

// TurnRequest is the input of one turn.
type TurnRequest struct {
	// RunID is the ID of the run, from the start response.
	RunID   string
	TurnID  string
	Input   []runnerproto.InputItem
	Harness runnerproto.Harness // from the start response
	WorkDir string
	Env     []string // full environment, KEY=VALUE
	// SandboxID is the sandbox ID of the runner configuration.
	SandboxID string
	// Auth are the credentials of the harness. They are in memory only.
	Auth map[string]string
	// MCPEnv maps an MCP server name to extra environment values for its command. They are in memory only.
	MCPEnv map[string]map[string]string
	// MCPHeaders maps an MCP server name to its request headers. They are in memory only.
	MCPHeaders map[string]map[string]string
	// Skills are the skills that the runner wrote below .openfactory/skills.
	Skills []runnerproto.Skill
	// ResumeSessionID is the harness session to resume, from the start response.
	ResumeSessionID string
	// ControlURL is the URL of the control plane.
	ControlURL string
	// SessionToken returns the session token to send now. The token changes
	// during a session, so a caller reads it for each request.
	SessionToken func() string
	// Agent is the user that the harness processes run as, and that owns
	// the files the harness writes. Nil means the user of the runner (the
	// egress lock is off).
	Agent *netlock.Credential
}

// TurnResult is the end of one turn.
type TurnResult struct {
	Outcome     runnerproto.TurnOutcome
	Result      string
	FailureCode string
	Message     string
}

// Emitter sends one event of the active turn. data is encoded as JSON.
type Emitter func(typ runnerproto.EventType, data any)

// turn is the turn that runs now.
type turn struct {
	id          string
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{} // closed after the event turn.ended
	interrupted atomic.Bool
}

// stop cancels the turn. The harness then returns the outcome interrupted.
func (t *turn) stop() {
	t.interrupted.Store(true)
	t.cancel()
}

// startTurn emits turn.started and runs the harness in a goroutine.
func (r *runner) startTurn(data runnerproto.TurnStartData) *turn {
	ctx, cancel := context.WithCancel(context.Background())
	t := &turn{id: data.TurnID, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	r.emit(runnerproto.EventTurnStarted, t.id, nil)
	req := TurnRequest{
		RunID:   r.start.RunID,
		TurnID:  t.id,
		Input:   r.withRestoreNote(r.withConversation(context.Background(), r.fetchAttachments(ctx, data.Input))),
		Harness: r.turnHarness(),
		WorkDir: r.turnDir,
		Env:     r.env(),

		SandboxID:       r.cfg.SandboxID,
		Auth:            r.harnessAuth,
		MCPHeaders:      r.mcpHeaders,
		MCPEnv:          r.mcpEnv,
		Skills:          r.start.Workspace.Skills,
		ResumeSessionID: r.start.Harness.ResumeSessionID,
		ControlURL:      r.cfg.ControlURL,
		SessionToken:    r.client.Token,
		Agent:           r.agent,
	}
	emit := func(typ runnerproto.EventType, d any) {
		r.noteHarnessEvent(typ, d)
		r.emit(typ, t.id, d)
	}
	go func() {
		defer close(t.done)
		defer cancel()
		res := r.harness.Turn(ctx, req, emit)
		r.afterTurn(t.id)
		r.emit(runnerproto.EventTurnEnded, t.id, endOfTurn(res, t.interrupted.Load()))
		sctx, scancel := context.WithTimeout(context.Background(), snapshotTimeout)
		defer scancel()
		r.saveHarnessState(sctx)
	}()
	return t
}

// endOfTurn makes the data of turn.ended from the result of a harness.
func endOfTurn(res TurnResult, interrupted bool) runnerproto.TurnEndedData {
	if interrupted {
		return runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeInterrupted}
	}
	if !res.Outcome.Valid() {
		return runnerproto.TurnEndedData{
			Outcome:     runnerproto.OutcomeError,
			FailureCode: string(rtcore.FailureAgentFailed),
			Message:     "harness returned an invalid outcome",
		}
	}
	d := runnerproto.TurnEndedData{Outcome: res.Outcome, Result: res.Result, FailureCode: res.FailureCode, Message: res.Message}
	if d.Outcome == runnerproto.OutcomeError && d.FailureCode == "" {
		d.FailureCode = string(rtcore.FailureAgentFailed)
	}
	return d
}

// deliver gives messages to the active turn if the harness can take them.
func (r *runner) deliver(t *turn, items []runnerproto.InputItem) {
	m, ok := r.harness.(MidTurnInput)
	if !ok || !runnerproto.HasCapability(r.harness.Capabilities(), runnerproto.CapMidTurnInput) {
		return
	}
	ids := m.Deliver(t.ctx, r.fetchAttachments(t.ctx, items))
	if len(ids) > 0 {
		r.emit(runnerproto.EventInputAccepted, t.id, runnerproto.InputAcceptedData{MessageIDs: ids})
	}
}
