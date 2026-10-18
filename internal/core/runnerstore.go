package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file holds the persistence contract for the runner protocol
// and for the message queue of a run. The engine does not use it yet.

// ErrSeqGap is returned when a batch of runner events does not continue the
// stored sequence. The caller answers with the stored ack.
var ErrSeqGap = errors.New("sequence gap")

// SessionState is the state of a session of a run.
type SessionState string

const (
	// SessionAssigned means that a task exists and the runner did not call
	// session start yet.
	SessionAssigned SessionState = "assigned"
	// SessionActive means that the runner called session start.
	SessionActive SessionState = "active"
	// SessionEnded means that the session is closed.
	SessionEnded SessionState = "ended"
)

// Valid reports whether s is one of the three session states.
func (s SessionState) Valid() bool {
	return s == SessionAssigned || s == SessionActive || s == SessionEnded
}

// RunSession is one sandbox life of a run.
type RunSession struct {
	RunID   string       `json:"run_id"`
	Session int          `json:"session"`
	TaskID  string       `json:"task_id"`
	State   SessionState `json:"state"`

	// BootstrapHash is the hash of the bootstrap token. InstanceHash is the
	// hash of the runner instance that used the token first.
	BootstrapHash      string    `json:"bootstrap_hash,omitempty"`
	InstanceHash       string    `json:"instance_hash,omitempty"`
	BootstrapExpiresAt time.Time `json:"bootstrap_expires_at"`

	Backend       string   `json:"backend,omitempty"`
	WorkerID      string   `json:"worker_id,omitempty"`
	SandboxID     string   `json:"sandbox_id,omitempty"`
	RunnerVersion string   `json:"runner_version,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	ActiveTurnID  string   `json:"active_turn_id,omitempty"`

	AssignedAt time.Time  `json:"assigned_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	// ReadyAt is the time of the event session.ready. Before it, the runner
	// does its setup and the session gets no turn.
	ReadyAt *time.Time `json:"ready_at,omitempty"`
	EndedAt *time.Time `json:"ended_at,omitempty"`

	// Usage is the newest cumulative usage that the runner reported for
	// this session. The usage of a run is the sum over its sessions.
	Usage Usage `json:"usage"`

	// InstanceShape is the machine size of the sandbox. Nil means the
	// default shape of the backend. ComputeSeconds is the life of the
	// sandbox in seconds. The dispatcher sets it when the session ends. The
	// metrics show both next to the inference cost.
	InstanceShape  *InstanceShape `json:"instance_shape,omitempty"`
	ComputeSeconds float64        `json:"compute_seconds,omitempty"`

	EndReason     string        `json:"end_reason,omitempty"`
	FailureCode   FailureCode   `json:"failure_code,omitempty"`
	SandboxReason SandboxReason `json:"sandbox_reason,omitempty"`

	// The store owns the fields below. UpdateRunSession ignores the values
	// that the caller puts into EventAck, CommandSeq, CommandAck and
	// LastSeenAt. It checks Version and increases it.
	Version    int64     `json:"version"`
	EventAck   int64     `json:"event_ack"`
	CommandSeq int64     `json:"command_seq"`
	CommandAck int64     `json:"command_ack"`
	LastSeenAt time.Time `json:"last_seen_at"`
}

// InstanceShape is the machine size of a session: the shape of the machine of
// the agent (MachineSpec).
type InstanceShape struct {
	Name     string `json:"name,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	VCPUs    int    `json:"vcpus,omitempty"`
	MemoryGB int    `json:"memory_gb,omitempty"`
}

// InstanceShapeOf returns the shape of a machine spec, or nil for a nil spec.
func InstanceShapeOf(r *MachineSpec) *InstanceShape {
	if r == nil {
		return nil
	}
	return &InstanceShape{Name: r.Name, OS: r.OS, Arch: r.Arch, VCPUs: r.VCPUs, MemoryGB: r.MemoryGB}
}

// Label returns a short text for the shape, for example
// "linux/x86_64 4 vCPU 8 GB". A nil or empty shape is "default".
func (s *InstanceShape) Label() string {
	if s == nil || (s.OS == "" && s.Arch == "" && s.VCPUs == 0 && s.MemoryGB == 0) {
		return "default"
	}
	var parts []string
	switch {
	case s.OS != "" && s.Arch != "":
		parts = append(parts, s.OS+"/"+s.Arch)
	case s.OS != "":
		parts = append(parts, s.OS)
	case s.Arch != "":
		parts = append(parts, s.Arch)
	}
	if s.VCPUs > 0 {
		parts = append(parts, fmt.Sprintf("%d vCPU", s.VCPUs))
	}
	if s.MemoryGB > 0 {
		parts = append(parts, fmt.Sprintf("%d GB", s.MemoryGB))
	}
	return strings.Join(parts, " ")
}

// RunnerEvent is one event that a runner sent for a session.
type RunnerEvent struct {
	RunID   string          `json:"run_id"`
	Session int             `json:"session"`
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	TurnID  string          `json:"turn_id,omitempty"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// RunnerCommand is one command from the control plane to the runner of a
// session.
type RunnerCommand struct {
	RunID     string          `json:"run_id"`
	Session   int             `json:"session"`
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// RunInputState is the delivery state of a message in the queue of a run.
type RunInputState string

const (
	// RunInputPending means that no runner got the message yet.
	RunInputPending RunInputState = "pending"
	// RunInputDelivered means that a command with the message went to a runner.
	RunInputDelivered RunInputState = "delivered"
	// RunInputConsumed means that the turn that got the message ended.
	RunInputConsumed RunInputState = "consumed"
)

// RunInputSender says who wrote a message.
type RunInputSender struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// RunInput is one message in the queue of a run.
type RunInput struct {
	ID          string          `json:"message_id"`
	RunID       string          `json:"run_id"`
	Seq         int64           `json:"seq"`
	Kind        string          `json:"kind"`
	Sender      RunInputSender  `json:"sender"`
	Content     string          `json:"content"`
	Data        json.RawMessage `json:"data,omitempty"`
	Attachments []string        `json:"attachments,omitempty"`
	Untrusted   bool            `json:"untrusted,omitempty"`
	DedupeKey   string          `json:"dedupe_key,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`

	// The store changes the fields below with the Mark, Finish and Reset
	// methods.
	State    RunInputState `json:"state"`
	Session  int           `json:"session,omitempty"`
	TurnID   string        `json:"turn_id,omitempty"`
	Accepted bool          `json:"accepted,omitempty"`
}

// TimelineEvent is one row of the timeline of a run. EventID is set by the
// writer. A second write with the same
// EventID for the run is ignored.
type TimelineEvent struct {
	EventID string          `json:"event_id"`
	RunID   string          `json:"run_id"`
	Session int             `json:"session"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// RunnerStore persists sessions, runner events, runner commands and the
// message queue of a run. A Store can implement it. Each method is atomic.
// Inside Transactor.InTx, a method joins the transaction of the context.
type RunnerStore interface {
	// CreateRunSession inserts a session in the state "assigned".
	// The run must exist (ErrNotFound). s.Session must be the highest session
	// number of the run plus 1, or 1 for the first session (ErrConflict if
	// not). Each older session of the run must be ended (ErrConflict if not).
	// The store sets Version to 1, the three counters to 0, and AssignedAt
	// and LastSeenAt to the current time if they are zero. It writes these
	// values back into s.
	CreateRunSession(ctx context.Context, s *RunSession) error

	// GetRunSession returns one session (ErrNotFound if missing).
	GetRunSession(ctx context.Context, runID string, session int) (*RunSession, error)

	// GetRunSessionByBootstrapHash returns the session with this bootstrap
	// hash (ErrNotFound if missing). An empty hash gives ErrNotFound.
	GetRunSessionByBootstrapHash(ctx context.Context, hash string) (*RunSession, error)

	// LatestRunSession returns the session with the highest number
	// (ErrNotFound if the run has no session).
	LatestRunSession(ctx context.Context, runID string) (*RunSession, error)

	// ListRunSessions returns the sessions of a run, lowest number first.
	ListRunSessions(ctx context.Context, runID string) ([]RunSession, error)

	// ListOpenRunSessions returns the sessions of all runs that are in the
	// state "assigned" or "active", the oldest LastSeenAt first, then by run
	// ID and session number. A limit of 0 or less means no limit.
	ListOpenRunSessions(ctx context.Context, limit int) ([]RunSession, error)

	// UpdateRunSession replaces the session if s.Version equals the stored
	// version. It then increases the version by 1 and writes it into s.
	// A different stored version gives ErrConflict. A missing session gives
	// ErrNotFound. It never changes EventAck, CommandSeq, CommandAck or
	// LastSeenAt.
	UpdateRunSession(ctx context.Context, s *RunSession) error

	// TouchRunSession sets LastSeenAt if at is later than the stored value.
	// It does not change the version. A missing session gives ErrNotFound.
	TouchRunSession(ctx context.Context, runID string, session int, at time.Time) error

	// AppendRunnerEvents stores a batch of events of one session and returns
	// the new EventAck. The events must be in increasing Seq order with a
	// step of 1, and each Seq must be 1 or more (ErrInvalid if not, and
	// nothing is stored). An empty batch returns the stored EventAck.
	// An event whose Seq is not higher than the stored EventAck is
	// ignored. If the first event that is not ignored does not have the Seq
	// EventAck+1, nothing is stored, and the result is the stored EventAck
	// and ErrSeqGap. A missing session gives ErrNotFound. A session in the
	// state "ended" gives ErrConflict. The store sets RunID and Session of
	// each stored event from the arguments. It also sets LastSeenAt to the
	// current time.
	AppendRunnerEvents(ctx context.Context, runID string, session int, events []RunnerEvent) (ack int64, err error)

	// ListRunnerEvents returns the events of a session with a Seq higher than
	// afterSeq, in Seq order. A limit of 0 or less means no limit.
	ListRunnerEvents(ctx context.Context, runID string, session int, afterSeq int64, limit int) ([]RunnerEvent, error)

	// AppendRunnerCommand stores a command and gives it the next sequence
	// number of the session (the first is 1). It writes Seq and CreatedAt
	// into c. A missing session gives ErrNotFound. A session in the state
	// "ended" gives ErrConflict.
	AppendRunnerCommand(ctx context.Context, c *RunnerCommand) error

	// ListRunnerCommands returns the commands of a session with a Seq higher
	// than afterSeq, in Seq order.
	ListRunnerCommands(ctx context.Context, runID string, session int, afterSeq int64) ([]RunnerCommand, error)

	// AckRunnerCommands sets CommandAck to ack, if ack is higher than the
	// stored value. An ack higher than CommandSeq counts as CommandSeq.
	// A missing session gives ErrNotFound.
	AckRunnerCommands(ctx context.Context, runID string, session int, ack int64) error

	// AppendRunInput adds a message to the queue of a run and gives it the
	// next sequence number of the run (the first is 1). The run must exist
	// (ErrNotFound). The store sets the state to "pending", clears Session,
	// TurnID and Accepted, and sets ID (prefix "msg") and CreatedAt if they
	// are empty. If DedupeKey is not empty and the run already has a message
	// with that key, nothing is stored: the store copies the existing message
	// into m and returns created == false.
	AppendRunInput(ctx context.Context, m *RunInput) (created bool, err error)

	// GetRunInput returns one message by its ID (ErrNotFound if missing).
	GetRunInput(ctx context.Context, id string) (*RunInput, error)

	// ListRunInputs returns the messages of a run with a Seq higher than
	// afterSeq, in Seq order. If states is not empty, only messages in one of
	// these states are returned.
	ListRunInputs(ctx context.Context, runID string, afterSeq int64, states ...RunInputState) ([]RunInput, error)

	// MarkRunInputsDelivered moves the named messages of the run from
	// "pending" to "delivered" and sets their Session and TurnID. Accepted
	// becomes false. Messages in a different state do not change. It returns
	// the number of messages that changed.
	MarkRunInputsDelivered(ctx context.Context, runID string, ids []string, session int, turnID string) (int, error)

	// MarkRunInputsAccepted sets Accepted for the named messages of the run
	// that are "delivered". It returns the number of messages that changed.
	MarkRunInputsAccepted(ctx context.Context, runID string, ids []string) (int, error)

	// MarkRunInputsAcceptedByTurn sets Accepted for each "delivered" message
	// of the run with this TurnID. It returns the number that changed.
	MarkRunInputsAcceptedByTurn(ctx context.Context, runID string, turnID string) (int, error)

	// FinishRunInputsOfTurn closes a turn. Each "delivered" message of the
	// run with this TurnID that is accepted becomes "consumed". Each one that
	// is not accepted goes back to "pending", with Session 0, an empty TurnID
	// and Accepted false. It returns the two counts.
	FinishRunInputsOfTurn(ctx context.Context, runID string, turnID string) (consumed int, returned int, err error)

	// ResetRunInputsOfSession puts each "delivered" message of the run with
	// this Session back to "pending", with Session 0, an empty TurnID and
	// Accepted false. It returns the number that changed. The engine calls it
	// when a session is lost.
	ResetRunInputsOfSession(ctx context.Context, runID string, session int) (int, error)

	// AppendTimelineEvent stores one timeline event. The run must exist
	// (ErrNotFound). EventID must not be empty (ErrInvalid). If the run
	// already has an event with this EventID, nothing is stored and created
	// is false. If At is zero, the store uses the current time and writes it
	// into ev.
	AppendTimelineEvent(ctx context.Context, ev *TimelineEvent) (created bool, err error)

	// ListTimelineEvents returns the timeline events of a run with an At
	// later than afterAt, ordered by At and then by EventID. A zero afterAt
	// means no lower bound. A limit of 0 or less means no limit.
	ListTimelineEvents(ctx context.Context, runID string, afterAt time.Time, limit int) ([]TimelineEvent, error)
}
