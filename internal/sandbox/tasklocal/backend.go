package tasklocal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

var _ core.TaskBackend = (*Backend)(nil)

const (
	defaultKillGrace = 2 * time.Minute
	destroyWait      = 5 * time.Second
	watchBuffer      = 8
)

// Options configures the backend.
type Options struct {
	// Root is the directory for task directories. It is required.
	Root string
	// Command is the argv of the runner process. It is required and has at least one element.
	Command []string
	// MaxTasks is the limit for tasks that are not exited. 0 means no limit.
	MaxTasks int
	// ExtraEnv holds variables that the backend adds to each task.
	ExtraEnv map[string]string
	// KillGrace is the extra time after MaxSessionSeconds before the hard stop. 0 means 2 minutes.
	KillGrace time.Duration
}

// Backend runs each task as a process on the host. It gives no isolation.
type Backend struct {
	opts Options

	mu    sync.Mutex
	tasks map[string]*entry
}

type watcher struct {
	ch       chan core.TaskStatus
	closed   bool
	finished chan struct{} // closed when the backend closes ch
}

type entry struct {
	status      core.TaskStatus
	dir         string
	cmd         *exec.Cmd
	done        chan struct{} // closed after the wait goroutine saw the exit
	cancelled   bool
	deadlineHit bool
	graceTimer  bool
	timer       *time.Timer
	watchers    map[*watcher]struct{}
}

// New creates a backend and the root directory.
func New(o Options) (*Backend, error) {
	if o.Root == "" {
		return nil, errors.New("tasklocal: Root is empty")
	}
	if len(o.Command) == 0 {
		return nil, errors.New("tasklocal: Command is empty")
	}
	if err := os.MkdirAll(o.Root, 0o755); err != nil {
		return nil, fmt.Errorf("tasklocal: create root: %w", err)
	}
	if o.KillGrace == 0 {
		o.KillGrace = defaultKillGrace
	}
	o.Command = append([]string(nil), o.Command...)
	return &Backend{opts: o, tasks: map[string]*entry{}}, nil
}

// Name returns "local".
func (b *Backend) Name() string { return "local" }

func dirName(taskID string) string {
	out := []rune(taskID)
	for i, r := range out {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.'
		if !ok {
			out[i] = '_'
		}
	}
	return string(out)
}

func snapshot(s core.TaskStatus) core.TaskStatus {
	if s.ExitCode != nil {
		v := *s.ExitCode
		s.ExitCode = &v
	}
	if s.StartedAt != nil {
		v := *s.StartedAt
		s.StartedAt = &v
	}
	if s.EndedAt != nil {
		v := *s.EndedAt
		s.EndedAt = &v
	}
	return s
}

// notify delivers a status to the watchers. The caller holds b.mu.
// A full buffer drops its oldest status. After an exited status the channel closes.
func (e *entry) notify() {
	st := snapshot(e.status)
	for w := range e.watchers {
		if w.closed {
			continue
		}
		for {
			select {
			case w.ch <- st:
			default:
				select {
				case <-w.ch:
				default:
				}
				continue
			}
			break
		}
		if st.State == core.TaskExited {
			closeWatcher(w)
		}
	}
	if st.State == core.TaskExited {
		e.watchers = map[*watcher]struct{}{}
	}
}

func closeWatcher(w *watcher) {
	if w.closed {
		return
	}
	w.closed = true
	close(w.ch)
	close(w.finished)
}

func (e *entry) closeWatchers() {
	for w := range e.watchers {
		closeWatcher(w)
	}
	e.watchers = map[*watcher]struct{}{}
}

// Start starts one process for the task. A second call with the same task ID
// returns the existing status.
func (b *Backend) Start(ctx context.Context, task core.Task) (core.TaskStatus, error) {
	if err := task.Validate(); err != nil {
		return core.TaskStatus{}, fmt.Errorf("tasklocal: invalid task: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if e, ok := b.tasks[task.TaskID]; ok {
		return snapshot(e.status), nil
	}
	if b.opts.MaxTasks > 0 {
		active := 0
		for _, e := range b.tasks {
			if e.status.State != core.TaskExited {
				active++
			}
		}
		if active >= b.opts.MaxTasks {
			return core.TaskStatus{}, core.ErrAtCapacity
		}
	}

	dir := b.opts.Root + string(os.PathSeparator) + dirName(task.TaskID)
	workspace := dir + string(os.PathSeparator) + "workspace"
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return core.TaskStatus{}, fmt.Errorf("tasklocal: create task directory: %w", err)
	}
	logFile, err := os.OpenFile(dir+string(os.PathSeparator)+"runner.log", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return core.TaskStatus{}, fmt.Errorf("tasklocal: create log file: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(b.opts.Command[0], b.opts.Command[1:]...) //nolint:noctx // the task process outlives the Start call; Cancel and the session timer end it
	cmd.Dir = workspace
	env := os.Environ()
	for k, v := range b.opts.ExtraEnv {
		env = append(env, k+"="+v)
	}
	for k, v := range task.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setProcessGroup(cmd)

	e := &entry{
		dir:      dir,
		cmd:      cmd,
		done:     make(chan struct{}),
		watchers: map[*watcher]struct{}{},
	}
	b.tasks[task.TaskID] = e

	if err := cmd.Start(); err != nil {
		now := time.Now()
		e.cmd = nil
		e.status = core.TaskStatus{
			TaskID:  task.TaskID,
			State:   core.TaskExited,
			Reason:  core.SandboxInvalidSpec,
			Message: err.Error(),
			EndedAt: &now,
		}
		close(e.done)
		return snapshot(e.status), nil
	}

	now := time.Now()
	e.status = core.TaskStatus{
		TaskID:    task.TaskID,
		State:     core.TaskRunning,
		SandboxID: "pid-" + strconv.Itoa(cmd.Process.Pid),
		StartedAt: &now,
	}
	d := time.Duration(task.MaxSessionSeconds)*time.Second + b.opts.KillGrace
	e.timer = time.AfterFunc(d, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if e.status.State == core.TaskExited {
			return
		}
		e.deadlineHit = true
		_ = killGroup(e.cmd)
	})
	go b.wait(e)
	return snapshot(e.status), nil
}

// wait waits for the process and records the exit.
func (b *Backend) wait(e *entry) {
	err := e.cmd.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	defer close(e.done)
	if e.timer != nil {
		e.timer.Stop()
	}
	code := -1
	if e.cmd.ProcessState != nil {
		code = e.cmd.ProcessState.ExitCode()
	}
	now := time.Now()
	e.status.State = core.TaskExited
	e.status.EndedAt = &now
	e.status.ExitCode = &code
	switch {
	case e.cancelled:
		e.status.Reason = core.SandboxCancelled
	case e.deadlineHit:
		e.status.Reason = core.SandboxDeadlineExceeded
	case code == 0:
		e.status.Reason = core.SandboxCompleted
	default:
		e.status.Reason = core.SandboxProcessFailed
	}
	if err != nil && code != 0 {
		e.status.Message = err.Error()
	}
	e.notify()
}

// Status returns the current status of the task.
func (b *Backend) Status(ctx context.Context, taskID string) (core.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.tasks[taskID]
	if !ok {
		return core.TaskStatus{}, fmt.Errorf("tasklocal: task %q: %w", taskID, core.ErrNotFound)
	}
	return snapshot(e.status), nil
}

// Watch returns a channel that gets the current status and each later change.
// The channel closes after the exited status or when ctx ends.
func (b *Backend) Watch(ctx context.Context, taskID string) (<-chan core.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("tasklocal: task %q: %w", taskID, core.ErrNotFound)
	}
	w := &watcher{ch: make(chan core.TaskStatus, watchBuffer), finished: make(chan struct{})}
	w.ch <- snapshot(e.status)
	if e.status.State == core.TaskExited {
		closeWatcher(w)
		return w.ch, nil
	}
	e.watchers[w] = struct{}{}
	go func() {
		select {
		case <-ctx.Done():
			b.mu.Lock()
			defer b.mu.Unlock()
			closeWatcher(w)
			delete(e.watchers, w)
		case <-w.finished:
		}
	}()
	return w.ch, nil
}

// Cancel sends SIGTERM to the process group and returns at once. After
// graceSeconds it sends SIGKILL if the task has not exited.
func (b *Backend) Cancel(ctx context.Context, taskID string, graceSeconds int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.tasks[taskID]
	if !ok {
		return fmt.Errorf("tasklocal: task %q: %w", taskID, core.ErrNotFound)
	}
	if e.status.State == core.TaskExited {
		return nil
	}
	e.cancelled = true
	_ = terminateGroup(e.cmd)
	if e.graceTimer {
		return nil
	}
	e.graceTimer = true
	if graceSeconds < 0 {
		graceSeconds = 0
	}
	go func() {
		t := time.NewTimer(time.Duration(graceSeconds) * time.Second)
		defer t.Stop()
		select {
		case <-e.done:
		case <-t.C:
			b.mu.Lock()
			defer b.mu.Unlock()
			if e.status.State != core.TaskExited {
				_ = killGroup(e.cmd)
			}
		}
	}()
	return nil
}

// Destroy kills the process if it is alive, removes the task directory and
// forgets the task. It is safe to call two times.
func (b *Backend) Destroy(ctx context.Context, taskID string) error {
	b.mu.Lock()
	e, ok := b.tasks[taskID]
	if !ok {
		b.mu.Unlock()
		return nil
	}
	if e.status.State != core.TaskExited {
		e.cancelled = true
		_ = killGroup(e.cmd)
	}
	done := e.done
	b.mu.Unlock()

	t := time.NewTimer(destroyWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if e.timer != nil {
		e.timer.Stop()
	}
	err := os.RemoveAll(e.dir)
	e.closeWatchers()
	if b.tasks[taskID] == e {
		delete(b.tasks, taskID)
	}
	if err != nil {
		return fmt.Errorf("tasklocal: remove task directory: %w", err)
	}
	return nil
}

// List returns the status of each known task, sorted by task ID.
func (b *Backend) List(ctx context.Context) ([]core.TaskStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]core.TaskStatus, 0, len(b.tasks))
	for _, e := range b.tasks {
		out = append(out, snapshot(e.status))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out, nil
}

// Preflight checks that the runner program exists and that the root is writable.
func (b *Backend) Preflight(ctx context.Context) error {
	if _, err := exec.LookPath(b.opts.Command[0]); err != nil {
		return fmt.Errorf("tasklocal: runner program: %w", err)
	}
	f, err := os.CreateTemp(b.opts.Root, ".preflight-*")
	if err != nil {
		return fmt.Errorf("tasklocal: root is not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("tasklocal: remove preflight file: %w", err)
	}
	return nil
}
