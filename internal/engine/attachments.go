package engine

import (
	"context"
	"encoding/json"

	"github.com/openfactory-org/openfactory/internal/core"
)

// SendMessage appends a user message with attachments (IDs of
// core.Attachment) to a run. Without attachments it is FollowUp. It returns
// the record of the message: the ID and the outcome.
// The outcome is the one that the dispatcher reports for the queue entry.
func (e *Engine) SendMessage(ctx context.Context, runID, message, actor string, attachments []string) (core.MessageReceipt, error) {
	return e.sendMessageReceipt(ctx, runID, message, actor, attachments, "", nil)
}

// MessageActor returns the actor of a run message from its Data: the actor
// string, or the field "actor" of an object (messageData). It returns ""
// when Data has no actor.
func MessageActor(data json.RawMessage) string {
	var actor string
	if json.Unmarshal(data, &actor) == nil {
		return actor
	}
	var obj struct {
		Actor string `json:"actor"`
	}
	if json.Unmarshal(data, &obj) == nil {
		return obj.Actor
	}
	return ""
}

// messageData is the Data of a run message. Without attachments it is the
// actor alone, as before. With attachments it is an object.
func messageData(actor string, attachments []string) []byte {
	if len(attachments) == 0 {
		return actorData(actor)
	}
	b, err := json.Marshal(map[string]any{"actor": actor, "attachments": attachments})
	if err != nil {
		return actorData(actor)
	}
	return b
}
