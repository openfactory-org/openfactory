package rtcore

import (
	"context"
	"io"
	"time"
)

// ExecRequest is one process to run in a sandbox.
type ExecRequest struct {
	Cmd     []string
	Dir     string
	Env     map[string]string
	Stdin   io.Reader
	Timeout time.Duration
}

// ExecResult is the result of Sandbox.Exec.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Sandbox is a workspace that a harness uses: it runs processes and reads and
// writes files. The runner implements it on the host of the task.
type Sandbox interface {
	ID() string
	WorkDir() string
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	// Start launches a long-running process and returns its stdout/stderr streams.
	Start(ctx context.Context, req ExecRequest) (Process, error)
	WriteFile(ctx context.Context, path string, data []byte, mode uint32) error
	ReadFile(ctx context.Context, path string) ([]byte, error)
	Destroy(ctx context.Context) error
}

// Process is a long-running process that Sandbox.Start launched.
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader
	Stdin() io.WriteCloser
	Wait() (int, error)
	Kill() error
}
