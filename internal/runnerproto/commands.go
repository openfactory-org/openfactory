package runnerproto

import (
	"encoding/json"
	"fmt"
)

// CommandType is the type of a command.
type CommandType string

// The command types.
const (
	// CommandTurnStart starts a turn.
	CommandTurnStart CommandType = "turn.start"
	// CommandMessageDeliver gives messages to an active turn.
	CommandMessageDeliver CommandType = "message.deliver"
	// CommandTurnInterrupt stops the active turn.
	CommandTurnInterrupt CommandType = "turn.interrupt"
	// CommandSessionStop ends the session.
	CommandSessionStop CommandType = "session.stop"
	// CommandSessionCancel ends the session because the run was cancelled.
	CommandSessionCancel CommandType = "session.cancel"
)

// Valid reports whether t is a known command type.
func (t CommandType) Valid() bool {
	switch t {
	case CommandTurnStart, CommandMessageDeliver, CommandTurnInterrupt,
		CommandSessionStop, CommandSessionCancel:
		return true
	}
	return false
}

// Command is one command from the control plane to the runner.
type Command struct {
	// Seq is the sequence number of the command in the session.
	Seq int64 `json:"seq"`
	// Type is the command type.
	Type CommandType `json:"type"`
	// Data is the data of the command. Its shape depends on Type.
	Data json.RawMessage `json:"data,omitempty"`
}

// CommandsResponse is the body of the response to the commands request.
type CommandsResponse struct {
	// Commands are the commands in sequence order.
	Commands []Command `json:"commands"`
}

// NewCommand makes a command and encodes data into its Data field. A nil data
// gives an empty Data field.
func NewCommand(seq int64, typ CommandType, data any) (Command, error) {
	c := Command{Seq: seq, Type: typ}
	if data == nil {
		return c, nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return Command{}, fmt.Errorf("encode data of command %q: %w", typ, err)
	}
	c.Data = raw
	return c, nil
}

// Decode decodes the Data field into v. An empty Data field is not an error
// and leaves v unchanged.
func (c Command) Decode(v any) error {
	if len(c.Data) == 0 {
		return nil
	}
	return json.Unmarshal(c.Data, v)
}

// TurnStartData is the data of a turn.start command.
type TurnStartData struct {
	// TurnID is the ID of the new turn.
	TurnID string `json:"turn_id"`
	// Input are the messages for the harness.
	Input []InputItem `json:"input"`
}

// MessageDeliverData is the data of a message.deliver command.
type MessageDeliverData struct {
	// Input are the messages for the active turn.
	Input []InputItem `json:"input"`
}

// TurnInterruptData is the data of a turn.interrupt command.
type TurnInterruptData struct {
	// TurnID is the ID of the turn to stop.
	TurnID string `json:"turn_id"`
}

// SessionStopData is the data of a session.stop command.
type SessionStopData struct {
	// Reason is the reason for the stop.
	Reason string `json:"reason"`
	// Snapshot says that the runner takes a snapshot before it ends.
	Snapshot bool `json:"snapshot"`
}

// MessageKind is the kind of a message in the queue of a run.
type MessageKind string

// The message kinds.
const (
	// KindUser is a message from a person or a service account.
	KindUser MessageKind = "user"
	// KindAgent is a message from another run.
	KindAgent MessageKind = "agent"
	// KindLifecycle is a message about a change of state of another run.
	KindLifecycle MessageKind = "lifecycle"
	// KindEvent is a message about an external fact.
	KindEvent MessageKind = "event"
	// KindSystem is a message from the platform.
	KindSystem MessageKind = "system"
)

// Valid reports whether k is a known message kind.
func (k MessageKind) Valid() bool {
	switch k {
	case KindUser, KindAgent, KindLifecycle, KindEvent, KindSystem:
		return true
	}
	return false
}

// InputItem is one message that the runner gives to the harness.
type InputItem struct {
	// MessageID is the ID of the message.
	MessageID string `json:"message_id"`
	// Kind is the kind of the message.
	Kind MessageKind `json:"kind"`
	// Sender is the sender of the message.
	Sender Sender `json:"sender"`
	// Content is the text of the message.
	Content string `json:"content"`
	// Data depends on the kind.
	Data json.RawMessage `json:"data,omitempty"`
	// Untrusted is true for content from a sender that is not a team member.
	Untrusted bool `json:"untrusted,omitempty"`
	// Attachments are the files of the message.
	Attachments []Attachment `json:"attachments"`
	// Files are small files that the control plane sends inline, for
	// example the event context of the message. The
	// runner writes each file to its path in the work directory, applies
	// the redactor, and does not give the files to the harness.
	Files []InputFile `json:"files,omitempty"`
}

// InputFile is a file of an input item. Path is relative to the work
// directory. Data is base64 in JSON.
type InputFile struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

// Sender is the sender of a message.
type Sender struct {
	// Type is the kind of sender, for example user or run.
	Type string `json:"type"`
	// ID is the ID of the sender.
	ID string `json:"id,omitempty"`
	// Name is the display name of the sender.
	Name string `json:"name,omitempty"`
}

// Attachment is a file that belongs to a message.
type Attachment struct {
	// ID is the ID of the attachment.
	ID string `json:"id"`
	// Name is the file name.
	Name string `json:"name"`
	// ContentType is the media type.
	ContentType string `json:"content_type"`
	// Bytes is the size of the file.
	Bytes int64 `json:"bytes"`
	// Path is the path in the workspace, if the message sets one.
	Path string `json:"path,omitempty"`
}

// ClampWait limits the wait of a commands request to the range from 0 to
// MaxCommandWaitSeconds.
func ClampWait(seconds int) int {
	if seconds < 0 {
		return 0
	}
	if seconds > MaxCommandWaitSeconds {
		return MaxCommandWaitSeconds
	}
	return seconds
}
