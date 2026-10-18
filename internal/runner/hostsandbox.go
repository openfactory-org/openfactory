package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/openfactory-org/openfactory/internal/core"
)

// maxCapturedOutput is the size limit of each output stream of Exec.
const maxCapturedOutput = 4 << 20

// hostSandbox is a core.Sandbox that runs processes on the local machine,
// in the workspace. Inside the runner, the sandbox is the machine itself.
type hostSandbox struct {
	id  string
	dir string   // the workspace, an absolute path
	env []string // base environment, KEY=VALUE
}

var _ core.Sandbox = (*hostSandbox)(nil)

// newHostSandbox makes a sandbox for a workspace. An empty id means "local".
func newHostSandbox(id, dir string, env []string) *hostSandbox {
	if id == "" {
		id = "local"
	}
	return &hostSandbox{id: id, dir: dir, env: env}
}

// ID returns the sandbox ID.
func (s *hostSandbox) ID() string { return s.id }

// WorkDir returns the workspace directory.
func (s *hostSandbox) WorkDir() string { return s.dir }

// abs makes a path absolute. A relative path is relative to the workspace.
func (s *hostSandbox) abs(p string) string {
	if p == "" {
		return s.dir
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(s.dir, filepath.FromSlash(p))
}

// command makes the command of a request. The environment is the base
// environment and then the variables of the request.
func (s *hostSandbox) command(ctx context.Context, req core.ExecRequest) (*exec.Cmd, error) {
	if len(req.Cmd) == 0 {
		return nil, errors.New("hostsandbox: empty command")
	}
	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = s.abs(req.Dir)
	env := append([]string(nil), s.env...)
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+req.Env[k])
	}
	cmd.Env = env
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killWaitDelay
	return cmd, nil
}

// Exec runs a command and waits for it. A non-zero exit is not an error.
func (s *hostSandbox) Exec(ctx context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd, err := s.command(ctx, req)
	if err != nil {
		return nil, err
	}
	stdout := &tailBuffer{max: maxCapturedOutput}
	stderr := &tailBuffer{max: maxCapturedOutput}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if req.Stdin != nil {
		cmd.Stdin = req.Stdin
	}
	err = cmd.Run()
	res := &core.ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		res.ExitCode = -1
		return res, ctxErr
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) && !errors.Is(err, exec.ErrWaitDelay) {
		return res, err
	}
	return res, nil
}

// Start launches a process. Its output streams are pipes. The process is the
// leader of its own process group. Kill ends the whole group.
func (s *hostSandbox) Start(ctx context.Context, req core.ExecRequest) (core.Process, error) {
	cmd, err := s.command(ctx, req)
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	var stdin io.WriteCloser
	if req.Stdin != nil {
		cmd.Stdin = req.Stdin
	} else if stdin, err = cmd.StdinPipe(); err != nil {
		closeAll(outR, outW, errR, errW)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW)
		return nil, fmt.Errorf("start %s: %w", req.Cmd[0], err)
	}
	// The child has its own copy of the write ends.
	outW.Close()
	errW.Close()
	return newHostProcess(cmd, outR, errR, stdin), nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// WriteFile writes a file. A relative path is relative to the workspace.
// Missing parent directories are created. Mode 0 means 0644.
func (s *hostSandbox) WriteFile(_ context.Context, path string, data []byte, mode uint32) error {
	p := s.abs(path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(mode) & os.ModePerm
	if mode == 0 {
		perm = 0o644
	}
	if err := os.WriteFile(p, data, perm); err != nil {
		return err
	}
	// The umask can remove bits at creation, and an old file keeps its mode.
	return os.Chmod(p, perm)
}

// ReadFile reads a file. A relative path is relative to the workspace.
func (s *hostSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	return os.ReadFile(s.abs(path))
}

// Destroy does nothing. The sandbox outlives the turn.
func (s *hostSandbox) Destroy(context.Context) error { return nil }

// hostProcess is a process that hostSandbox started.
type hostProcess struct {
	cmd    *exec.Cmd
	stdout *os.File
	stderr *os.File
	stdin  io.WriteCloser

	done chan struct{} // closed after the process was waited for
	code int
	err  error
}

func newHostProcess(cmd *exec.Cmd, stdout, stderr *os.File, stdin io.WriteCloser) *hostProcess {
	p := &hostProcess{cmd: cmd, stdout: stdout, stderr: stderr, stdin: stdin, done: make(chan struct{})}
	go p.wait()
	return p
}

// wait reaps the process, so that it never stays a zombie.
func (p *hostProcess) wait() {
	defer close(p.done)
	err := p.cmd.Wait()
	p.code = -1
	if p.cmd.ProcessState != nil {
		p.code = p.cmd.ProcessState.ExitCode()
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) && !errors.Is(err, exec.ErrWaitDelay) {
		p.err = err
	}
}

func (p *hostProcess) Stdout() io.Reader { return p.stdout }
func (p *hostProcess) Stderr() io.Reader { return p.stderr }

// Stdin returns the input pipe, or nil if the request had its own stdin.
func (p *hostProcess) Stdin() io.WriteCloser { return p.stdin }

// Wait waits for the exit and returns the exit code. A non-zero exit is not
// an error. It can be called more than once.
func (p *hostProcess) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

// Kill kills the process group. It does nothing after the exit.
func (p *hostProcess) Kill() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return killProcessGroup(p.cmd)
}
