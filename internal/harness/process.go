package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// Event kinds (rtcore.HarnessEvent.Kind).
const (
	EventMessage    = "message"
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventLog        = "log"
	EventUsage      = "usage"
	EventSession    = "session"
	EventDone       = "done"
	EventError      = "error"
)

// WriteFile creates parent directories (via `mkdir -p`) and writes data into the sandbox.
func WriteFile(ctx context.Context, sb rtcore.Sandbox, p string, data []byte, mode uint32) error {
	dir := path.Dir(p)
	if dir != "." && dir != "/" {
		res, err := sb.Exec(ctx, rtcore.ExecRequest{Cmd: []string{"mkdir", "-p", dir}, Timeout: 30 * time.Second})
		if err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
		if res != nil && res.ExitCode != 0 {
			return fmt.Errorf("mkdir %s: exit %d: %s", dir, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
	}
	return sb.WriteFile(ctx, p, data, mode)
}

// MergeEnv merges maps left to right (later maps win).
func MergeEnv(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// WorkDir returns req.WorkDir, falling back to the sandbox work dir.
func WorkDir(sb rtcore.Sandbox, req rtcore.HarnessRequest) string {
	if req.WorkDir != "" {
		return req.WorkDir
	}
	return sb.WorkDir()
}

// StreamResult is the outcome of StreamLines.
type StreamResult struct {
	ExitCode int
	Stderr   string // last ~16 KiB of stderr
}

const stderrTail = 16 << 10

// StreamLines starts req in the sandbox, writes stdin (then closes it), and calls onLine for
// every stdout line (without the trailing newline). The process is killed when ctx is done.
func StreamLines(ctx context.Context, sb rtcore.Sandbox, req rtcore.ExecRequest, stdin string, onLine func([]byte)) (StreamResult, error) {
	req.Stdin = nil
	proc, err := sb.Start(ctx, req)
	if err != nil {
		return StreamResult{ExitCode: -1}, err
	}
	if w := proc.Stdin(); w != nil {
		go func() {
			if stdin != "" {
				_, _ = io.WriteString(w, stdin)
			}
			_ = w.Close()
		}()
	} else if stdin != "" {
		_ = proc.Kill()
		return StreamResult{ExitCode: -1}, errors.New("sandbox process has no stdin")
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = proc.Kill()
		case <-done:
		}
	}()

	var (
		wg     sync.WaitGroup
		errBuf tailBuffer
	)
	if r := proc.Stderr(); r != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = io.Copy(&errBuf, r)
		}()
	}
	if r := proc.Stdout(); r != nil {
		br := bufio.NewReaderSize(r, 64<<10)
		for {
			line, rerr := br.ReadBytes('\n')
			line = bytes.TrimRight(line, "\r\n")
			if len(bytes.TrimSpace(line)) > 0 {
				onLine(line)
			}
			if rerr != nil {
				break
			}
		}
	}
	wg.Wait()
	code, werr := proc.Wait()
	res := StreamResult{ExitCode: code, Stderr: errBuf.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	return res, werr
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTail {
		t.buf = t.buf[len(t.buf)-stderrTail:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// Emitter sends events on a channel without blocking forever once ctx is done.
type Emitter struct {
	ctx context.Context
	ch  chan rtcore.HarnessEvent
}

// NewEmitter returns an emitter and the receive side of its channel (buffer 256).
func NewEmitter(ctx context.Context) (*Emitter, <-chan rtcore.HarnessEvent) {
	ch := make(chan rtcore.HarnessEvent, 256)
	return &Emitter{ctx: ctx, ch: ch}, ch
}

// Emit sends ev; it returns false when ctx is done and the event could not be delivered.
func (e *Emitter) Emit(ev rtcore.HarnessEvent) bool {
	select {
	case e.ch <- ev:
		return true
	default:
	}
	select {
	case e.ch <- ev:
		return true
	case <-e.ctx.Done():
		return false
	}
}

// Log emits a log event.
func (e *Emitter) Log(format string, args ...any) {
	e.Emit(rtcore.HarnessEvent{Kind: EventLog, Role: "log", Content: fmt.Sprintf(format, args...)})
}

// Close closes the channel. Call exactly once, after the final done/error event.
func (e *Emitter) Close() { close(e.ch) }

// Truncate shortens s to at most n bytes keeping the head and tail.
func Truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	head := n * 2 / 3
	tail := n - head
	omitted := len(s) - head - tail
	return s[:head] + fmt.Sprintf("\n\n[... %d bytes truncated ...]\n\n", omitted) + s[len(s)-tail:]
}

// LastLines returns the last n non-empty lines of s.
func LastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
