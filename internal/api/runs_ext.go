package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/conversation"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// This file holds the run endpoints for messages,
// inputs, cancel, timeline, diagnostics, sessions and conversation.

const (
	// maxRunnerEventsPerSession bounds the events that one request reads
	// for one session.
	maxRunnerEventsPerSession = 20000
	runnerEventPage           = 1000
)

// Outcomes of a message.
const (
	OutcomeLiveSession = core.MessageLiveSession
	OutcomeNewSession  = core.MessageNewSession
	OutcomeQueued      = core.MessageQueued
)

// requireRunner returns the runner store or the error 501.
func (s *Server) requireRunner() (core.RunnerStore, error) {
	if s.opts.Runner == nil {
		return nil, featureUnavailable("the runner store")
	}
	return s.opts.Runner, nil
}

// messageRequest is the body of a message to a run.
type messageRequest struct {
	Message     string   `json:"message"`
	Attachments []string `json:"attachments"`
}

// sendRunMessage checks a message and gives it to the engine. It returns the
// run and the record of the message that the engine returns: the ID of the
// stored message and where it went (live_session, new_session or queued).
func (s *Server) sendRunMessage(ctx context.Context, p *Principal, runID string, req messageRequest) (*core.Run, core.MessageReceipt, error) {
	if strings.TrimSpace(req.Message) == "" {
		return nil, core.MessageReceipt{}, errInvalid("message is required")
	}
	run, err := s.runFor(ctx, p, runID)
	if err != nil {
		return nil, core.MessageReceipt{}, err
	}
	// On the runner path a message continues a cancelled run in a new session.
	if run.State == core.RunCancelled && s.opts.Runner == nil {
		return nil, core.MessageReceipt{}, Errf(CodeOperationNotSupported, "run %s was cancelled and cannot receive messages", run.ID)
	}
	if run.WorkItemID != "" {
		wi, f, err := s.workItemFor(ctx, p, run.WorkItemID)
		if err == nil {
			if err := s.CheckFollowupAllowed(ctx, p, f, wi); err != nil {
				return nil, core.MessageReceipt{}, err
			}
		}
	}
	if err := s.checkAttachments(ctx, run.TeamID, req.Attachments); err != nil {
		return nil, core.MessageReceipt{}, err
	}
	rec, err := s.opts.Engine.SendMessage(ctx, run.ID, req.Message, p.ActorID(), req.Attachments)
	if err != nil {
		return nil, core.MessageReceipt{}, err
	}
	if nr, err := s.opts.Store.GetRun(ctx, run.ID); err == nil {
		run = nr
	}
	return run, rec, nil
}

// handleFollowUp serves POST /runs/{runId}/followups. It answers with the run.
func (s *Server) handleFollowUp(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req messageRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	run, _, err := s.sendRunMessage(r.Context(), p, r.PathValue("runId"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, s.runViewCtx(r.Context(), run))
	return nil
}

// handlePostRunMessage serves POST /runs/{runId}/messages.
func (s *Server) handlePostRunMessage(w http.ResponseWriter, r *http.Request, p *Principal) error {
	var req messageRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	run, rec, err := s.sendRunMessage(r.Context(), p, r.PathValue("runId"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": run.ID, "message_id": rec.MessageID, "outcome": rec.Outcome})
	return nil
}

// handleCancelRun serves POST /runs/{runId}/cancel.
//
// The body is optional: {"cascade": true|false}. The default is true, which
// also cancels the runs under the run. A cancel is accepted in each state:
// a finished run keeps its state, and its live session ends.
func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req struct {
		Cascade *bool `json:"cascade"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return err
	}
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	cancel := s.opts.Engine.Cancel
	if req.Cascade != nil && !*req.Cascade {
		cancel = s.opts.Engine.CancelOne
	}
	if err := cancel(ctx, run.ID, p.ActorID()); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID})
	return nil
}

// InputView is one message of the queue of a run.
type InputView struct {
	MessageID   string              `json:"message_id"`
	Seq         int64               `json:"seq"`
	Kind        string              `json:"kind"`
	Sender      core.RunInputSender `json:"sender"`
	State       core.RunInputState  `json:"state"`
	Content     string              `json:"content"`
	Data        json.RawMessage     `json:"data,omitempty"`
	Attachments []string            `json:"attachments,omitempty"`
	Untrusted   bool                `json:"untrusted"`
	DedupeKey   string              `json:"dedupe_key,omitempty"`
	Session     int                 `json:"session,omitempty"`
	TurnID      string              `json:"turn_id,omitempty"`
	Accepted    bool                `json:"accepted"`
	CreatedAt   time.Time           `json:"created_at"`
}

// handleRunInputs serves GET /runs/{runId}/inputs: the queue of the
// run. A later API version moves the queue to GET /messages. Today /messages keeps
// the transcript.
func (s *Server) handleRunInputs(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	rs, err := s.requireRunner()
	if err != nil {
		return err
	}
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	after, err := qInt64(r, "after")
	if err != nil {
		return err
	}
	var states []core.RunInputState
	for _, v := range qList(r, "state") {
		st := core.RunInputState(strings.ToLower(v))
		if st != core.RunInputPending && st != core.RunInputDelivered && st != core.RunInputConsumed {
			return errInvalid("unknown input state %q (expected pending, delivered or consumed)", v)
		}
		states = append(states, st)
	}
	ins, err := rs.ListRunInputs(ctx, run.ID, after, states...)
	if err != nil {
		return err
	}
	views := make([]InputView, 0, len(ins))
	for _, in := range ins {
		views = append(views, InputView{MessageID: in.ID, Seq: in.Seq, Kind: in.Kind, Sender: in.Sender, State: in.State,
			Content: in.Content, Data: in.Data, Attachments: in.Attachments, Untrusted: in.Untrusted,
			DedupeKey: in.DedupeKey, Session: in.Session, TurnID: in.TurnID,
			Accepted: in.Accepted, CreatedAt: in.CreatedAt})
	}
	writeJSON(w, http.StatusOK, listResponse("inputs", views, ""))
	return nil
}

// runnerEvents reads the events of one session in pages.
func runnerEvents(ctx context.Context, rs core.RunnerStore, runID string, session int) ([]core.RunnerEvent, error) {
	var out []core.RunnerEvent
	var after int64
	for len(out) < maxRunnerEventsPerSession {
		page, err := rs.ListRunnerEvents(ctx, runID, session, after, runnerEventPage)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < runnerEventPage {
			break
		}
		after = page[len(page)-1].Seq
	}
	return out, nil
}

// TimelineEvent is one row of the timeline of a run.
type TimelineEvent struct {
	EventID string          `json:"event_id"`
	RunID   string          `json:"run_id"`
	Session int             `json:"session"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data"`
}

// timelineRunnerTypes are the runner events that become timeline rows.
var timelineRunnerTypes = map[string]bool{
	string(runnerproto.EventSetupStep):        true,
	string(runnerproto.EventSnapshotRestored): true,
	string(runnerproto.EventSessionReady):     true,
	string(runnerproto.EventTurnStarted):      true,
	string(runnerproto.EventTurnEnded):        true,
	string(runnerproto.EventSnapshotSaved):    true,
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// eventData returns the data of a runner event as an object. A turn event
// gets its turn_id from the event when the data has none.
func eventData(ev core.RunnerEvent) json.RawMessage {
	m := map[string]any{}
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &m) // data that is not an object gives an empty object
	}
	if m == nil { // the data was JSON null, which sets the map to nil
		m = map[string]any{}
	}
	if ev.TurnID != "" {
		if _, ok := m["turn_id"]; !ok {
			m["turn_id"] = ev.TurnID
		}
	}
	return mustJSON(m)
}

// handleRunTimeline serves GET /runs/{runId}/timeline.
//
// The rows come from four places: the control-plane events of the run
// timeline table, the runner events of the types setup.step,
// snapshot.restored, session.ready, turn.started, turn.ended and
// snapshot.saved, run.created from the run row when no stored row has it, and
// run.cancelled from the run row when the run is cancelled and no stored row
// has it (the engine cancels a run without a timeline write). The rows are
// sorted by time, then by event ID.
func (s *Server) handleRunTimeline(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	rs, err := s.requireRunner()
	if err != nil {
		return err
	}
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	stored, err := rs.ListTimelineEvents(ctx, run.ID, time.Time{}, 0)
	if err != nil {
		return err
	}
	events := make([]TimelineEvent, 0, len(stored)+2)
	has := map[string]bool{}
	for _, ev := range stored {
		has[ev.Type] = true
		data := ev.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		events = append(events, TimelineEvent{EventID: ev.EventID, RunID: run.ID, Session: ev.Session, Type: ev.Type, At: ev.At, Data: data})
	}
	if !has["run.created"] {
		events = append(events, TimelineEvent{EventID: "evt_" + run.ID + "_created", RunID: run.ID, Type: "run.created", At: run.CreatedAt,
			Data: mustJSON(runCreatedData(run))})
	}
	if run.State == core.RunCancelled && !has["run.cancelled"] {
		at := run.CreatedAt
		if run.FinishedAt != nil {
			at = *run.FinishedAt
		}
		events = append(events, TimelineEvent{EventID: "evt_" + run.ID + "_cancelled", RunID: run.ID,
			Type: "run.cancelled", At: at, Data: mustJSON(map[string]any{})})
	}
	sessions, err := rs.ListRunSessions(ctx, run.ID)
	if err != nil {
		return err
	}
	for _, ss := range sessions {
		base := "evt_" + run.ID + "_" + strconv.Itoa(ss.Session)
		evs, err := runnerEvents(ctx, rs, run.ID, ss.Session)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if !timelineRunnerTypes[ev.Type] {
				continue
			}
			events = append(events, TimelineEvent{EventID: base + "_e" + strconv.FormatInt(ev.Seq, 10), RunID: run.ID, Session: ss.Session,
				Type: ev.Type, At: ev.At, Data: eventData(ev)})
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if !events[i].At.Equal(events[j].At) {
			return events[i].At.Before(events[j].At)
		}
		return events[i].EventID < events[j].EventID
	})
	writeJSON(w, http.StatusOK, listResponse("events", events, ""))
	return nil
}

// DiagnosticView is one diagnostic of a run.
type DiagnosticView struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	At      time.Time       `json:"at"`
	Session int             `json:"session"`
}

// handleRunDiagnostics serves GET /runs/{runId}/diagnostics.
func (s *Server) handleRunDiagnostics(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	rs, err := s.requireRunner()
	if err != nil {
		return err
	}
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	sessions, err := rs.ListRunSessions(ctx, run.ID)
	if err != nil {
		return err
	}
	out := []DiagnosticView{}
	for _, ss := range sessions {
		evs, err := runnerEvents(ctx, rs, run.ID, ss.Session)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.Type != string(runnerproto.EventDiagnostic) {
				continue
			}
			var d runnerproto.DiagnosticData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue // a diagnostic with bad data has no code to show
			}
			out = append(out, DiagnosticView{Code: d.Code, Message: d.Message, Data: d.Data, At: ev.At, Session: ss.Session})
		}
	}
	writeJSON(w, http.StatusOK, listResponse("diagnostics", out, ""))
	return nil
}

// SnapshotView tells which snapshot parts a session saved.
type SnapshotView struct {
	Workspace    bool `json:"workspace"`
	HarnessState bool `json:"harness_state"`
}

// SessionView is one session of a run.
type SessionView struct {
	Session       int                `json:"session"`
	Number        int                `json:"number"`
	State         core.SessionState  `json:"state"`
	AssignedAt    time.Time          `json:"assigned_at"`
	StartedAt     *time.Time         `json:"started_at,omitempty"`
	ReadyAt       *time.Time         `json:"ready_at,omitempty"`
	EndedAt       *time.Time         `json:"ended_at,omitempty"`
	EndReason     string             `json:"end_reason,omitempty"`
	FailureCode   core.FailureCode   `json:"failure_code,omitempty"`
	SandboxReason core.SandboxReason `json:"sandbox_reason,omitempty"`
	Usage         UsageView          `json:"usage"`
	Snapshot      SnapshotView       `json:"snapshot"`
}

// handleRunSessions serves GET /runs/{runId}/sessions.
func (s *Server) handleRunSessions(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	rs, err := s.requireRunner()
	if err != nil {
		return err
	}
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	sessions, err := rs.ListRunSessions(ctx, run.ID)
	if err != nil {
		return err
	}
	out := make([]SessionView, 0, len(sessions))
	for _, ss := range sessions {
		v := SessionView{Session: ss.Session, Number: ss.Session, State: ss.State, AssignedAt: ss.AssignedAt,
			StartedAt: ss.StartedAt, ReadyAt: ss.ReadyAt, EndedAt: ss.EndedAt, EndReason: ss.EndReason,
			FailureCode: ss.FailureCode, SandboxReason: ss.SandboxReason,
			Usage: UsageView{Usage: ss.Usage, Coverage: "known"}}
		evs, err := runnerEvents(ctx, rs, run.ID, ss.Session)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.Type != string(runnerproto.EventSnapshotSaved) {
				continue
			}
			var d runnerproto.SnapshotSavedData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				continue // a snapshot event with bad data names no part
			}
			switch d.Part {
			case "workspace":
				v.Snapshot.Workspace = true
			case "harness_state":
				v.Snapshot.HarnessState = true
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, listResponse("sessions", out, ""))
	return nil
}

// handleRunConversation serves GET /runs/{runId}/conversation. The
// query scope=final_message returns only the last assistant message.
func (s *Server) handleRunConversation(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	scope := r.URL.Query().Get("scope")
	if scope != "" && scope != "full" && scope != "final_message" {
		return errInvalid("scope must be full or final_message")
	}
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	msgs, err := s.runMessagesAfter(ctx, run.ID, 0)
	if err != nil {
		return err
	}
	conv := conversation.Build(run, msgs)
	if scope != "final_message" {
		writeJSON(w, http.StatusOK, conv)
		return nil
	}
	// The result has the format of the full conversation with one step that
	// holds one message, or no step when the run has no assistant message.
	final := conversation.Build(run, nil)
	for i := len(conv.Steps) - 1; i >= 0 && len(final.Steps) == 0; i-- {
		step := conv.Steps[i]
		for j := len(step.Messages) - 1; j >= 0; j-- {
			if m := step.Messages[j]; m.Role == "assistant" {
				step.Messages = step.Messages[j : j+1]
				final.Steps = append(final.Steps, step)
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, final)
	return nil
}

// runCreatedData is the data of the run.created timeline event: the source
// and the creator.
func runCreatedData(run *core.Run) map[string]any {
	src, ref := run.SourceAndRef()
	d := map[string]any{"source": src}
	if ref != "" {
		d["source_ref"] = ref
	}
	if run.Creator != nil {
		d["creator"] = run.Creator
	}
	return d
}
