// Package conversation builds the normalized conversation of a run
// from the stored run messages.
package conversation

import (
	"encoding/json"
	"strconv"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// Build turns the messages of a run into the normalized
// conversation. A user message starts a new step.
func Build(run *core.Run, msgs []core.RunMessage) runnerproto.Conversation {
	conv := runnerproto.Conversation{ConversationID: run.ID, Steps: []runnerproto.ConversationStep{}}
	var step *runnerproto.ConversationStep
	newStep := func() {
		conv.Steps = append(conv.Steps, runnerproto.ConversationStep{
			StepID:   "step_" + strconv.Itoa(len(conv.Steps)+1),
			Messages: []runnerproto.ConversationMessage{},
		})
		step = &conv.Steps[len(conv.Steps)-1]
	}
	// assistant returns the assistant message at the end of the step.
	assistant := func(id string) *runnerproto.ConversationMessage {
		if step == nil {
			newStep()
		}
		if n := len(step.Messages); n > 0 && step.Messages[n-1].Role == "assistant" {
			return &step.Messages[n-1]
		}
		step.Messages = append(step.Messages, runnerproto.ConversationMessage{MessageID: id, Role: "assistant"})
		return &step.Messages[len(step.Messages)-1]
	}
	for _, m := range msgs {
		id := m.ID
		var data struct {
			CallID    string          `json:"call_id"`
			Category  string          `json:"category"`
			Arguments json.RawMessage `json:"arguments"`
			IsError   bool            `json:"is_error"`
		}
		if len(m.Data) > 0 {
			_ = json.Unmarshal(m.Data, &data) // a bad Data field only loses the extras
		}
		callID, args := data.CallID, data.Arguments
		text := runnerproto.Block{Type: runnerproto.BlockText, Text: m.Content}
		switch m.Role {
		case "user":
			newStep()
			step.Messages = append(step.Messages, runnerproto.ConversationMessage{
				MessageID: id, Role: "user", Kind: "user", Blocks: []runnerproto.Block{text},
			})
		case "assistant":
			am := assistant(id)
			am.Blocks = append(am.Blocks, text)
		case "tool_call":
			am := assistant(id) // the content is the name of the tool
			am.Blocks = append(am.Blocks, runnerproto.Block{
				Type: runnerproto.BlockAction, Name: m.Content, CallID: callID,
				Category: data.Category, State: "completed", Input: args,
			})
		case "tool_result":
			am := assistant(id)
			am.Blocks = append(am.Blocks, runnerproto.Block{
				Type: runnerproto.BlockActionResult, CallID: callID, IsError: data.IsError, Text: m.Content,
			})
		case "system":
			if step == nil {
				newStep()
			}
			step.Messages = append(step.Messages, runnerproto.ConversationMessage{
				MessageID: id, Role: "system", Kind: "system", Blocks: []runnerproto.Block{text},
			})
		}
	}
	return conv
}
