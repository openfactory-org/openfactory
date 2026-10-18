package conversation

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func blocks(conv runnerproto.Conversation) []runnerproto.Block {
	var out []runnerproto.Block
	for _, st := range conv.Steps {
		for _, m := range st.Messages {
			out = append(out, m.Blocks...)
		}
	}
	return out
}

// The runner path stores call_id and arguments, with the tool name as the
// content.
func TestBuildRunnerPathToolData(t *testing.T) {
	conv := Build(&core.Run{ID: "run_1"}, []core.RunMessage{
		{ID: "m1", Role: "user", Content: "go"},
		{ID: "m2", Role: "tool_call", Content: "bash", Data: []byte(`{"call_id":"c1","arguments":{"command":"ls"}}`)},
		{ID: "m3", Role: "tool_result", Content: "a.txt", Data: []byte(`{"call_id":"c1"}`)},
	})
	b := blocks(conv)
	if len(b) != 3 || b[1].Name != "bash" || b[1].CallID != "c1" || string(b[1].Input) != `{"command":"ls"}` || b[2].CallID != "c1" {
		t.Fatalf("blocks = %+v", b)
	}
}

func TestBuildToolMessageWithoutData(t *testing.T) {
	conv := Build(&core.Run{ID: "run_1"}, []core.RunMessage{
		{ID: "m1", Role: "tool_call", Content: "bash"},
		{ID: "m2", Role: "tool_result", Content: "ok", Data: []byte(`not json`)},
	})
	b := blocks(conv)
	if len(b) != 2 || b[0].Name != "bash" || b[0].CallID != "" {
		t.Fatalf("blocks = %+v", b)
	}
}
