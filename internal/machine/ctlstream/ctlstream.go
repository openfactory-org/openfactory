// Package ctlstream is the control stream between the worker agent on the
// host and vm-init in the guest. It runs over Firecracker's hybrid vsock: the
// guest connects to CID 2, port Port, which reaches the host unix socket
// <uds_path>_<Port>. Frames are newline-delimited JSON, at most MaxFrame
// bytes each. Unknown frame types are ignored by both sides.
package ctlstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Port is the vsock port of the control stream.
const Port = 1024

// HostCID is the CID of the host.
const HostCID = 2

// MaxFrame is the longest frame, newline included.
const MaxFrame = 64 << 10

// Version is the version of the control stream.
const Version = 1

// Frame types.
const (
	// TypeHello: guest → host. vm-init is ready (phase boot, template or
	// restored), or it reconnected to a new agent with a running runner
	// (phase resume).
	TypeHello = "hello"
	// TypeGo: host → guest. MMDS holds the task document.
	TypeGo = "go"
	// TypeMMDSRead: guest → host. The host may clear MMDS.
	TypeMMDSRead = "mmds_read"
	// TypeRunnerStarted: guest → host. The runner process exists.
	TypeRunnerStarted = "runner_started"
	// TypeCancel: host → guest. SIGTERM now, SIGKILL after the grace.
	TypeCancel = "cancel"
	// TypeExit: guest → host. The final report.
	TypeExit = "exit"
	// TypeExitAck: host → guest. vm-init may reboot.
	TypeExitAck = "exit_ack"
)

// Hello phases.
const (
	// PhaseBoot: a cold boot, of a session VM or of a snapshot template.
	PhaseBoot = "boot"
	// PhaseTemplate is reserved; vm-init cannot know before go.
	PhaseTemplate = "template"
	// PhaseRestored: the stream broke before go because the VM was
	// restored from a template.
	PhaseRestored = "restored"
	// PhaseResume: the stream broke (an agent restart) and vm-init
	// reconnected. The runner keeps running. StreamKey proves that the
	// frame comes from vm-init.
	PhaseResume = "resume"
)

// Exit phases.
const (
	ExitPhaseInit   = "init"
	ExitPhaseRunner = "runner"
)

// Frame is one frame of the control stream. Each type uses a subset of the fields.
type Frame struct {
	Type string `json:"type"`
	// hello
	V             int    `json:"v,omitempty"`
	VMInitVersion string `json:"vm_init_version,omitempty"`
	Phase         string `json:"phase,omitempty"`
	// StreamKey: in go, a random key of this VM (host → guest); in a
	// resume hello, the same key (guest → host). Agent code in the guest
	// cannot read the memory of vm-init, so it cannot answer a resume.
	StreamKey string `json:"stream_key,omitempty"`
	// Running: in a resume hello, the runner still runs.
	Running bool `json:"running,omitempty"`
	// runner_started
	PID int `json:"pid,omitempty"`
	// cancel
	GraceSeconds int `json:"grace_seconds,omitempty"`
	// exit
	ExitCode   *int   `json:"exit_code,omitempty"`
	Signal     int    `json:"signal,omitempty"`
	OOMKills   int    `json:"oom_kills,omitempty"`
	Error      string `json:"error,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
}

// ErrFrameTooLarge is returned for a frame above MaxFrame.
var ErrFrameTooLarge = errors.New("control frame too large")

// Conn reads and writes frames on one stream. Write is safe for concurrent use.
type Conn struct {
	rw  io.ReadWriteCloser
	sc  *bufio.Scanner
	wmu sync.Mutex
}

// New wraps a stream.
func New(rw io.ReadWriteCloser) *Conn {
	sc := bufio.NewScanner(rw)
	sc.Buffer(make([]byte, 4096), MaxFrame)
	return &Conn{rw: rw, sc: sc}
}

// Write sends one frame.
func (c *Conn) Write(f Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxFrame {
		return ErrFrameTooLarge
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(append(data, '\n'))
	return err
}

// Read returns the next frame with a known type. Lines that are not JSON
// objects are skipped. It returns io.EOF at the end of the stream and
// ErrFrameTooLarge for a line above MaxFrame.
func (c *Conn) Read() (Frame, error) {
	for c.sc.Scan() {
		var f Frame
		if json.Unmarshal(c.sc.Bytes(), &f) != nil || !Known(f.Type) {
			continue
		}
		return f, nil
	}
	if err := c.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Frame{}, ErrFrameTooLarge
		}
		return Frame{}, err
	}
	return Frame{}, io.EOF
}

// Close closes the stream.
func (c *Conn) Close() error { return c.rw.Close() }

// Known reports whether t is a frame type of this version.
func Known(t string) bool {
	switch t {
	case TypeHello, TypeGo, TypeMMDSRead, TypeRunnerStarted, TypeCancel, TypeExit, TypeExitAck:
		return true
	}
	return false
}

// Expect reads frames until one of type t arrives. Frames of other types are
// passed to other, which may be nil.
func (c *Conn) Expect(t string, other func(Frame)) (Frame, error) {
	for {
		f, err := c.Read()
		if err != nil {
			return f, fmt.Errorf("waiting for %s: %w", t, err)
		}
		if f.Type == t {
			return f, nil
		}
		if other != nil {
			other(f)
		}
	}
}

// Tail keeps the last N bytes written to it. vm-init keeps the stderr of
// the runner in it for the exit report.
type Tail struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

// NewTail returns a Tail of n bytes.
func NewTail(n int) *Tail { return &Tail{n: n} }

// Write keeps the end of p.
func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.n:]...)
	}
	return len(p), nil
}

// String returns the kept bytes.
func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
