package runner

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	maxStderrTail  = 2 << 10
	maxContentSize = 256 << 10
	killWaitDelay  = 2 * time.Second
)

// ExecHarness runs one command for each turn. The text of all input items
// goes to stdin, separated by blank lines. Each stdout line is not an event:
// the full stdout, trimmed, is one assistant message and the result.
// Exit 0 gives the outcome done. Another exit gives the outcome error with
// the failure code agent_failed and the last 2 KiB of stderr as the message.
// It has the capability interrupt (ctx done kills the process group).
type ExecHarness struct {
	Command []string // for example {"sh", "-c", "cat"}
}

// Capabilities returns the capabilities of the harness.
func (h ExecHarness) Capabilities() []runnerproto.Capability {
	return []runnerproto.Capability{runnerproto.CapInterrupt}
}

// Turn runs the command once.
func (h ExecHarness) Turn(ctx context.Context, req TurnRequest, emit Emitter) TurnResult {
	if len(h.Command) == 0 {
		return agentError("exec harness has no command")
	}
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: maxStderrTail}
	cmd := exec.CommandContext(ctx, h.Command[0], h.Command[1:]...)
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killWaitDelay
	cmd.Dir = req.WorkDir
	cmd.Env = req.Env
	if req.Agent != nil {
		req.Agent.Apply(cmd)
	}
	cmd.Stdin = strings.NewReader(RenderInput(req.Input))
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return TurnResult{Outcome: runnerproto.OutcomeInterrupted}
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return agentError(msg)
	}
	out := strings.TrimSpace(stdout.String())
	out, cut := cutContent(out)
	if out != "" {
		emit(runnerproto.EventMessage, runnerproto.MessageData{Role: "assistant", Content: out, Truncated: cut})
	}
	return TurnResult{Outcome: runnerproto.OutcomeDone, Result: out}
}

func agentError(msg string) TurnResult {
	return TurnResult{
		Outcome:     runnerproto.OutcomeError,
		FailureCode: string(rtcore.FailureAgentFailed),
		Message:     msg,
	}
}

// RenderInput makes the text of input items for a harness that reads text.
// A user message is plain text. Another kind is shown in a marked section
// with its kind and sender. Items are separated by blank lines.
func RenderInput(items []runnerproto.InputItem) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		text := it.Content
		if it.Kind != runnerproto.KindUser && it.Kind != "" {
			head := "[" + string(it.Kind)
			if name := senderName(it.Sender); name != "" {
				head += " from " + name
			}
			text = head + "]\n" + it.Content
		}
		if lines := attachmentLines(it.Attachments); len(lines) > 0 {
			text += "\n\n" + strings.Join(lines, "\n")
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n\n")
}

func senderName(s runnerproto.Sender) string {
	switch {
	case s.Name != "":
		return s.Name
	case s.ID != "":
		return s.ID
	}
	return s.Type
}

// cutContent cuts s to the size limit of one event content.
func cutContent(s string) (string, bool) {
	if len(s) <= maxContentSize {
		return s, false
	}
	return strings.ToValidUTF8(s[:maxContentSize], ""), true
}

// tailBuffer keeps the last max bytes that were written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(string(b.buf), "")
}
