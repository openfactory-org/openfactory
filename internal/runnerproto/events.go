package runnerproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EventType is the type of an event.
type EventType string

// The event types.
const (
	// EventSetupStep reports one step of the setup.
	EventSetupStep EventType = "setup.step"
	// EventSnapshotRestored reports a snapshot restore.
	EventSnapshotRestored EventType = "snapshot.restored"
	// EventSessionReady reports that the setup is complete.
	EventSessionReady EventType = "session.ready"
	// EventTurnStarted reports the start of a turn.
	EventTurnStarted EventType = "turn.started"
	// EventInputAccepted reports messages that the harness accepted.
	EventInputAccepted EventType = "input.accepted"
	// EventMessage carries a message of the conversation.
	EventMessage EventType = "message"
	// EventToolCall carries a tool call.
	EventToolCall EventType = "tool_call"
	// EventToolResult carries the result of a tool call.
	EventToolResult EventType = "tool_result"
	// EventHarnessSession carries the ID of the harness session.
	EventHarnessSession EventType = "harness.session"
	// EventUsage carries token usage.
	EventUsage EventType = "usage"
	// EventArtifact reports an uploaded artifact.
	EventArtifact EventType = "artifact"
	// EventTranscriptSaved reports an uploaded transcript.
	EventTranscriptSaved EventType = "transcript.saved"
	// EventDiagnostic carries a diagnostic.
	EventDiagnostic EventType = "diagnostic"
	// EventTurnEnded reports the end of a turn.
	EventTurnEnded EventType = "turn.ended"
	// EventIdle reports the idle deadline.
	EventIdle EventType = "idle"
	// EventSnapshotSaved reports an uploaded snapshot part.
	EventSnapshotSaved EventType = "snapshot.saved"
)

// Valid reports whether t is a known event type.
func (t EventType) Valid() bool {
	switch t {
	case EventSetupStep, EventSnapshotRestored, EventSessionReady, EventTurnStarted,
		EventInputAccepted, EventMessage, EventToolCall, EventToolResult,
		EventHarnessSession, EventUsage, EventArtifact, EventTranscriptSaved,
		EventDiagnostic, EventTurnEnded, EventIdle, EventSnapshotSaved:
		return true
	}
	return false
}

// Event is one event that the runner reports.
type Event struct {
	// Seq is the sequence number of the event in the session, from 1.
	Seq int64 `json:"seq"`
	// Type is the event type.
	Type EventType `json:"type"`
	// TurnID is the turn of the event, if any.
	TurnID string `json:"turn_id,omitempty"`
	// At is the time of the event.
	At time.Time `json:"at"`
	// Data is the data of the event. Its shape depends on Type.
	Data json.RawMessage `json:"data,omitempty"`
}

// EventBatch is the body of the events request.
type EventBatch struct {
	// Events are the events in sequence order.
	Events []Event `json:"events"`
}

// EventAck is the body of the response to the events request.
type EventAck struct {
	// Ack is the newest sequence number that the control plane has.
	Ack int64 `json:"ack"`
}

// NewEvent makes an event and encodes data into its Data field. A nil data
// gives an empty Data field.
func NewEvent(seq int64, typ EventType, turnID string, at time.Time, data any) (Event, error) {
	e := Event{Seq: seq, Type: typ, TurnID: turnID, At: at}
	if data == nil {
		return e, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("encode data of event %q: %w", typ, err)
	}
	e.Data = raw
	return e, nil
}

// Decode decodes the Data field into v. An empty Data field is not an error
// and leaves v unchanged.
func (e Event) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// Validate checks a batch that the runner sends after the sequence number
// afterSeq. A batch can repeat events that the control plane already has, but
// it must not leave a gap.
func (b EventBatch) Validate(afterSeq int64) error {
	if len(b.Events) == 0 {
		return errors.New("event batch is empty")
	}
	for i, e := range b.Events {
		if !e.Type.Valid() {
			return fmt.Errorf("event %d: invalid type %q", i, e.Type)
		}
		if e.Seq < 1 {
			return fmt.Errorf("event %d: seq %d is less than 1", i, e.Seq)
		}
		if i == 0 {
			if e.Seq > afterSeq+1 {
				return fmt.Errorf("gap at start: first seq is %d, expected %d or less", e.Seq, afterSeq+1)
			}
			continue
		}
		if prev := b.Events[i-1].Seq; e.Seq != prev+1 {
			return fmt.Errorf("gap in batch: seq %d follows seq %d", e.Seq, prev)
		}
	}
	return nil
}

// TurnOutcome is the outcome of a turn.
type TurnOutcome string

// The outcomes of a turn.
const (
	// OutcomeDone means that the turn finished.
	OutcomeDone TurnOutcome = "done"
	// OutcomeError means that the turn failed.
	OutcomeError TurnOutcome = "error"
	// OutcomeInterrupted means that the turn was interrupted.
	OutcomeInterrupted TurnOutcome = "interrupted"
	// OutcomeNeedsInput means that the harness waits for input.
	OutcomeNeedsInput TurnOutcome = "needs_input"
)

// Valid reports whether o is a known outcome.
func (o TurnOutcome) Valid() bool {
	switch o {
	case OutcomeDone, OutcomeError, OutcomeInterrupted, OutcomeNeedsInput:
		return true
	}
	return false
}

// Capability is a capability of a harness.
type Capability string

// The capabilities of a harness.
const (
	// CapResume means that the harness continues a stored session.
	CapResume Capability = "resume"
	// CapMidTurnInput means that the harness accepts a message during a turn.
	CapMidTurnInput Capability = "mid_turn_input"
	// CapInterrupt means that the harness stops a turn and keeps its session.
	CapInterrupt Capability = "interrupt"
	// CapNeedsInputSignal means that the harness reports that it waits for input.
	CapNeedsInputSignal Capability = "needs_input_signal"
)

// Valid reports whether c is a known capability.
func (c Capability) Valid() bool {
	switch c {
	case CapResume, CapMidTurnInput, CapInterrupt, CapNeedsInputSignal:
		return true
	}
	return false
}

// HasCapability reports whether list contains c.
func HasCapability(list []Capability, c Capability) bool {
	for _, x := range list {
		if x == c {
			return true
		}
	}
	return false
}

// SetupStepData is the data of a setup.step event.
type SetupStepData struct {
	// Step is the name of the step.
	Step string `json:"step"`
	// Status is the status of the step.
	Status string `json:"status"`
	// DurationMS is the duration in milliseconds.
	DurationMS int64 `json:"duration_ms"`
	// Output is the output of the step, at most MaxSetupOutputBytes.
	Output string `json:"output,omitempty"`
}

// SnapshotRestoredData is the data of a snapshot.restored event.
type SnapshotRestoredData struct {
	// Parts are the snapshot parts that were restored.
	Parts []string `json:"parts"`
	// FailedPaths are the paths that the runner could not restore.
	FailedPaths []string `json:"failed_paths"`
}

// SessionReadyData is the data of a session.ready event.
type SessionReadyData struct {
	// Capabilities are the capabilities of the harness.
	Capabilities []Capability `json:"capabilities"`
	// Repositories are the clones of the workspace with the commit that
	// the runner checked out. Optional.
	Repositories []RepositoryCommit `json:"repositories,omitempty"`
}

// RepositoryCommit is the checked-out commit of one repository.
type RepositoryCommit struct {
	// Path is the directory of the clone, as in Repository.Path.
	Path string `json:"path"`
	// Commit is the commit SHA.
	Commit string `json:"commit"`
}

// InputAcceptedData is the data of an input.accepted event.
type InputAcceptedData struct {
	// MessageIDs are the IDs of the messages that the harness accepted.
	MessageIDs []string `json:"message_ids"`
}

// MessageData is the data of a message event.
type MessageData struct {
	// Role is assistant or system.
	Role string `json:"role"`
	// Content is the text of the message.
	Content string `json:"content"`
	// Truncated is true if the runner cut the content.
	Truncated bool `json:"truncated,omitempty"`
}

// ToolCallData is the data of a tool_call event.
type ToolCallData struct {
	// CallID is the ID of the call.
	CallID string `json:"call_id"`
	// Name is the name of the tool.
	Name string `json:"name"`
	// Arguments are the arguments of the call as JSON.
	Arguments json.RawMessage `json:"arguments"`
	// Category is the category of the tool.
	Category string `json:"category"`
}

// ToolResultData is the data of a tool_result event.
type ToolResultData struct {
	// CallID is the ID of the call.
	CallID string `json:"call_id"`
	// Content is the result text.
	Content string `json:"content"`
	// IsError is true if the call failed.
	IsError bool `json:"is_error"`
	// Truncated is true if the runner cut the content.
	Truncated bool `json:"truncated,omitempty"`
}

// HarnessSessionData is the data of a harness.session event.
type HarnessSessionData struct {
	// SessionID is the ID that the harness uses to resume.
	SessionID string `json:"session_id"`
}

// UsageData is the data of a usage event.
type UsageData struct {
	// InputTokens is the number of input tokens.
	InputTokens int64 `json:"input_tokens"`
	// OutputTokens is the number of output tokens.
	OutputTokens int64 `json:"output_tokens"`
	// CacheReadTokens is the number of tokens read from the cache.
	CacheReadTokens int64 `json:"cache_read_tokens,omitempty"`
	// CacheWriteTokens is the number of tokens written to the cache.
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// CostUSD is the cost in US dollars.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// DurationMS is the agent time in milliseconds: for each turn, the
	// duration that the harness reports, else the wall time of the turn in
	// the runner. Older runners do not send it.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Cumulative is true if the counts are totals for the session.
	Cumulative bool `json:"cumulative"`
	// Coverage tells how much of the usage the counts cover: "known", or
	// "partial" when a turn ended before the harness reported its totals
	// (the counts are then an estimate). Empty means known.
	Coverage string `json:"coverage,omitempty"`
}

// ArtifactData is the data of an artifact event.
type ArtifactData struct {
	// Name is the name of the artifact.
	Name string `json:"name"`
	// Type is the type of the artifact.
	Type string `json:"type"`
	// UploadID is the ID of the upload of the file.
	UploadID string `json:"upload_id"`
}

// TranscriptSavedData is the data of a transcript.saved event.
type TranscriptSavedData struct {
	// UploadID is the ID of the upload of the transcript.
	UploadID string `json:"upload_id"`
}

// DiagnosticData is the data of a diagnostic event.
type DiagnosticData struct {
	// Code is the diagnostic code.
	Code string `json:"code"`
	// Message is a text for people.
	Message string `json:"message"`
	// Data is extra data as JSON.
	Data json.RawMessage `json:"data,omitempty"`
}

// TurnEndedData is the data of a turn.ended event.
type TurnEndedData struct {
	// Outcome is the outcome of the turn.
	Outcome TurnOutcome `json:"outcome"`
	// Result is the result text.
	Result string `json:"result,omitempty"`
	// FailureCode is the failure code, if the turn failed.
	FailureCode string `json:"failure_code,omitempty"`
	// Message is a text for people.
	Message string `json:"message,omitempty"`
}

// IdleData is the data of an idle event.
type IdleData struct {
	// Deadline is the time when the runner ends the session.
	Deadline time.Time `json:"deadline"`
}

// SnapshotSavedData is the data of a snapshot.saved event.
type SnapshotSavedData struct {
	// Part is the snapshot part.
	Part string `json:"part"`
	// UploadIDs are the IDs of the uploads of the part.
	UploadIDs []string `json:"upload_ids"`
	// Bytes is the size of the part.
	Bytes int64 `json:"bytes"`
}
