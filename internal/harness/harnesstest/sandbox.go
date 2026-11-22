// Package harnesstest provides fake rtcore.Sandbox implementations for harness tests.
package harnesstest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// Sandbox is an in-memory sandbox. Files written via WriteFile are kept in Files.
// `mkdir -p` execs succeed; other execs use ExecFunc (default: exit 0).
// Start uses StartFunc (default: a process with empty output).
type Sandbox struct {
	Dir       string
	ExecFunc  func(ctx context.Context, req rtcore.ExecRequest) (*rtcore.ExecResult, error)
	StartFunc func(ctx context.Context, req rtcore.ExecRequest) (rtcore.Process, error)

	mu     sync.Mutex
	Files  map[string][]byte
	Execs  []rtcore.ExecRequest
	Starts []rtcore.ExecRequest
}

// New returns an in-memory sandbox rooted at dir.
func New(dir string) *Sandbox { return &Sandbox{Dir: dir, Files: map[string][]byte{}} }

func (s *Sandbox) ID() string      { return "sbx_fake" }
func (s *Sandbox) WorkDir() string { return s.Dir }

func (s *Sandbox) Exec(ctx context.Context, req rtcore.ExecRequest) (*rtcore.ExecResult, error) {
	s.mu.Lock()
	s.Execs = append(s.Execs, req)
	s.mu.Unlock()
	if len(req.Cmd) > 0 && req.Cmd[0] == "mkdir" {
		return &rtcore.ExecResult{}, nil
	}
	if s.ExecFunc != nil {
		return s.ExecFunc(ctx, req)
	}
	return &rtcore.ExecResult{}, nil
}

func (s *Sandbox) Start(ctx context.Context, req rtcore.ExecRequest) (rtcore.Process, error) {
	s.mu.Lock()
	s.Starts = append(s.Starts, req)
	s.mu.Unlock()
	if s.StartFunc != nil {
		return s.StartFunc(ctx, req)
	}
	return NewProcess("", "", 0), nil
}

func (s *Sandbox) WriteFile(_ context.Context, path string, data []byte, _ uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Files[path] = append([]byte(nil), data...)
	return nil
}

func (s *Sandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Files[path]
	if !ok {
		return nil, rtcore.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (s *Sandbox) Destroy(context.Context) error { return nil }

// File returns a written file's content ("" if missing).
func (s *Sandbox) File(path string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.Files[path])
}

// StartRequests returns a copy of the recorded Start requests.
func (s *Sandbox) StartRequests() []rtcore.ExecRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rtcore.ExecRequest(nil), s.Starts...)
}

// Process is a canned process.
type Process struct {
	stdout, stderr io.Reader
	StdinBuf       *WriteBuffer
	Code           int
	killed         chan struct{}
	once           sync.Once
}

// NewProcess returns a process that emits stdout/stderr then exits with code.
// Like a real CLI that reads its prompt from stdin, Wait returns only after stdin
// is closed (or the process is killed), so a test sees the full stdin once the
// harness has finished.
func NewProcess(stdout, stderr string, code int) *Process {
	return &Process{stdout: strings.NewReader(stdout), stderr: strings.NewReader(stderr), StdinBuf: &WriteBuffer{}, Code: code, killed: make(chan struct{})}
}

func (p *Process) Stdout() io.Reader     { return p.stdout }
func (p *Process) Stderr() io.Reader     { return p.stderr }
func (p *Process) Stdin() io.WriteCloser { return p.StdinBuf }
func (p *Process) Wait() (int, error) {
	select {
	case <-p.StdinBuf.Closed():
	case <-p.killed:
	}
	return p.Code, nil
}
func (p *Process) Kill() error { p.once.Do(func() { close(p.killed) }); return nil }

// WriteBuffer is a concurrency-safe WriteCloser that records writes.
type WriteBuffer struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
	done   chan struct{}
}

// Closed returns a channel that is closed when Close is called.
func (w *WriteBuffer) Closed() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done == nil {
		w.done = make(chan struct{})
	}
	return w.done
}

func (w *WriteBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *WriteBuffer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done == nil {
		w.done = make(chan struct{})
	}
	if !w.closed {
		close(w.done)
	}
	w.closed = true
	return nil
}

func (w *WriteBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// Local is a sandbox backed by a real directory and os/exec, for tests that need a shell.
type Local struct {
	Dir string
}

// NewLocal returns a Local sandbox rooted at dir.
func NewLocal(dir string) *Local { return &Local{Dir: dir} }

func (l *Local) ID() string      { return "sbx_local_test" }
func (l *Local) WorkDir() string { return l.Dir }

func (l *Local) cmd(ctx context.Context, req rtcore.ExecRequest) (*exec.Cmd, context.CancelFunc) {
	cancel := context.CancelFunc(func() {})
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	c := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	c.Dir = req.Dir
	if c.Dir == "" {
		c.Dir = l.Dir
	}
	c.Env = os.Environ()
	for k, v := range req.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdin = req.Stdin
	return c, cancel
}

func (l *Local) Exec(ctx context.Context, req rtcore.ExecRequest) (*rtcore.ExecResult, error) {
	c, cancel := l.cmd(ctx, req)
	defer cancel()
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	res := &rtcore.ExecResult{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

type localProc struct {
	c              *exec.Cmd
	cancel         context.CancelFunc
	stdout, stderr io.Reader
	stdin          io.WriteCloser
}

func (p *localProc) Stdout() io.Reader     { return p.stdout }
func (p *localProc) Stderr() io.Reader     { return p.stderr }
func (p *localProc) Stdin() io.WriteCloser { return p.stdin }
func (p *localProc) Kill() error           { return p.c.Process.Kill() }
func (p *localProc) Wait() (int, error) {
	err := p.c.Wait()
	p.cancel()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (l *Local) Start(ctx context.Context, req rtcore.ExecRequest) (rtcore.Process, error) {
	c, cancel := l.cmd(ctx, req)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		cancel()
		return nil, err
	}
	return &localProc{c: c, cancel: cancel, stdout: stdout, stderr: stderr, stdin: stdin}, nil
}

func (l *Local) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(l.Dir, p)
}

func (l *Local) WriteFile(_ context.Context, path string, data []byte, mode uint32) error {
	p := l.abs(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, os.FileMode(mode))
}

func (l *Local) ReadFile(_ context.Context, path string) ([]byte, error) {
	b, err := os.ReadFile(l.abs(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, rtcore.ErrNotFound
	}
	return b, err
}

func (l *Local) Destroy(context.Context) error { return nil }
