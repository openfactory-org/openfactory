package runnerproto

import "encoding/json"

// Conversation is the normalized conversation of a run.
type Conversation struct {
	// ConversationID is the ID of the conversation.
	ConversationID string `json:"conversation_id"`
	// Steps are the steps in order.
	Steps []ConversationStep `json:"steps"`
}

// ConversationStep is one step of the conversation. A step starts with a
// message of a user.
type ConversationStep struct {
	// StepID is the ID of the step.
	StepID string `json:"step_id"`
	// TurnID is the turn of the step, if it is known.
	TurnID string `json:"turn_id,omitempty"`
	// Session is the session of the step. 0 means that it is not known.
	Session int `json:"session"`
	// Messages are the messages of the step.
	Messages []ConversationMessage `json:"messages"`
}

// ConversationMessage is one message of a step.
type ConversationMessage struct {
	// MessageID is the ID of the message.
	MessageID string `json:"message_id,omitempty"`
	// Role is user, assistant or system.
	Role string `json:"role"`
	// Kind is the kind of a message from the queue of the run.
	Kind string `json:"kind,omitempty"`
	// Sender is the sender of the message, if it is known.
	Sender *Sender `json:"sender,omitempty"`
	// Blocks are the content blocks.
	Blocks []Block `json:"blocks"`
}

// Block types.
const (
	BlockText         = "text"
	BlockAction       = "action"
	BlockActionResult = "action_result"
	BlockEvent        = "event"
)

// Block is one content block of a message.
type Block struct {
	// Type is text, action, action_result or event.
	Type string `json:"type"`
	// Text is the text of a text block or of a result.
	Text string `json:"text,omitempty"`
	// CallID links an action and its result.
	CallID string `json:"call_id,omitempty"`
	// Name is the name of an action.
	Name string `json:"name,omitempty"`
	// Category is the category of an action.
	Category string `json:"category,omitempty"`
	// State is running, completed, failed or denied.
	State string `json:"state,omitempty"`
	// Input is the input of an action as JSON.
	Input json.RawMessage `json:"input,omitempty"`
	// IsError is true for the result of a failed action.
	IsError bool `json:"is_error,omitempty"`
}
