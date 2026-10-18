package runner

import (
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func testConv(texts ...string) runnerproto.Conversation {
	var msgs []runnerproto.ConversationMessage
	for _, t := range texts {
		msgs = append(msgs, runnerproto.ConversationMessage{Role: "user", Blocks: []runnerproto.Block{{Type: runnerproto.BlockText, Text: t}}})
	}
	return runnerproto.Conversation{Steps: []runnerproto.ConversationStep{{Messages: msgs}}}
}

func TestRenderConversation(t *testing.T) {
	conv := runnerproto.Conversation{Steps: []runnerproto.ConversationStep{{Messages: []runnerproto.ConversationMessage{
		{Role: "user", Blocks: []runnerproto.Block{{Type: "text", Text: "fix it\nplease"}}},
		{Role: "assistant", Blocks: []runnerproto.Block{
			{Type: "text", Text: "ok"},
			{Type: "action", Name: "bash", CallID: "c1"},
			{Type: "action_result", CallID: "c1", Text: "done"},
			{Type: "event", Text: "ignored"},
		}},
	}}}}
	want := "user: fix it please\nassistant: ok\nassistant called bash\nresult: done"
	if got := renderConversation(conv, 1<<10); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := renderConversation(runnerproto.Conversation{}, 100); got != "" {
		t.Fatalf("empty conversation gave %q", got)
	}
}

func TestRenderConversationKeepsNewest(t *testing.T) {
	conv := testConv("aaaaaaaa", "bbbbbbbb", "cccccccc") // each line is "user: " + 8 = 14 bytes + newline
	got := renderConversation(conv, 31)
	want := conversationOmitted + "\nuser: bbbbbbbb\nuser: cccccccc"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	got = renderConversation(conv, 1<<10)
	if strings.Contains(got, conversationOmitted) {
		t.Fatalf("nothing was cut: %q", got)
	}
	// One line longer than the limit keeps its end.
	got = renderConversation(testConv(strings.Repeat("x", 100)), 10)
	if got != conversationOmitted+"\n"+strings.Repeat("x", 10) {
		t.Fatalf("got %q", got)
	}
}
