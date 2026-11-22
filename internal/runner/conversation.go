package runner

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	// maxConversationBytes is the most text of the conversation that the
	// runner gives to a harness that cannot resume its session.
	maxConversationBytes = 64 << 10
	conversationTimeout  = 30 * time.Second
	conversationOmitted  = "[older messages omitted]"
)

// withConversation puts the conversation of the run in front of the input of
// the first turn, if the harness cannot resume its session. It
// does this once per session. If the request fails, it
// reports a diagnostic and returns the input as it is.
func (r *runner) withConversation(ctx context.Context, input []runnerproto.InputItem) []runnerproto.InputItem {
	if r.conversationDone {
		return input
	}
	if r.start.Session <= 1 || r.harnessStateRestored {
		return input
	}
	r.conversationDone = true
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, conversationTimeout)
	defer cancel()
	conv, err := r.client.Conversation(ctx)
	if err != nil {
		r.log.Warn("read the conversation", "error", scrub(err))
		r.diagnostic("conversation/unavailable", "the conversation of the run could not be read: "+scrub(err))
		return input
	}
	text := renderConversation(conv, maxConversationBytes)
	if text == "" {
		return input
	}
	item := runnerproto.InputItem{
		MessageID:   fmt.Sprintf("conversation-%d", r.start.Session),
		Kind:        runnerproto.KindSystem,
		Sender:      runnerproto.Sender{Type: "platform"},
		Content:     "The harness could not resume its session. This is the conversation so far:\n\n" + text,
		Attachments: []runnerproto.Attachment{},
	}
	return append([]runnerproto.InputItem{item}, input...)
}

// renderConversation renders the steps as plain text with one line for each
// text block, action and result. It keeps the newest max bytes of lines.
func renderConversation(conv runnerproto.Conversation, max int) string {
	var lines []string
	for _, st := range conv.Steps {
		for _, m := range st.Messages {
			for _, b := range m.Blocks {
				switch b.Type {
				case runnerproto.BlockText:
					lines = append(lines, m.Role+": "+oneLine(b.Text))
				case runnerproto.BlockAction:
					lines = append(lines, m.Role+" called "+b.Name)
				case runnerproto.BlockActionResult:
					lines = append(lines, "result: "+oneLine(b.Text))
				}
			}
		}
	}
	size, from := 0, len(lines)
	for from > 0 && size+len(lines[from-1])+1 <= max {
		from--
		size += len(lines[from]) + 1
	}
	out := strings.Join(lines[from:], "\n")
	if from > 0 {
		if from == len(lines) && len(lines) > 0 {
			// The newest line alone is too long: keep its end.
			l := lines[len(lines)-1]
			out = strings.ToValidUTF8(l[len(l)-max:], "")
		}
		out = conversationOmitted + "\n" + out
	}
	return out
}

// oneLine replaces line breaks, so that each block is one line.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", " ")
}
