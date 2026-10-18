// Package dispatch is the part of the control plane that starts a sandbox
// task for each queued run, moves the run through its states from the reports
// of the runner, and delivers the messages of a run to its runner.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/runnerapi"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runstate"
	"github.com/openfactory-org/openfactory/internal/spend"
)

// Commander sends a command to the runner of a session. *runnerapi.Service implements it.
type Commander interface {
	EnqueueCommand(ctx context.Context, runID string, session int, typ runnerproto.CommandType, data any) (core.RunnerCommand, error)
}

// Config holds what the dispatcher needs.
type Config struct {
	Store      core.Store
	Runner     core.RunnerStore
	Backend    core.TaskBackend
	ControlURL string // OF_CONTROL_URL of each task
	// Router gives the backends of remote workers. Nil means no remote workers.
	Router BackendRouter
	// PublicControlURL is OF_CONTROL_URL of a task on a remote worker. Empty means ControlURL.
	PublicControlURL string
	// ReplicaID names this replica as the holder of the leader lease. Empty means a random ID.
	ReplicaID string
	Image     string                     // core.Task.Image; empty means "local"
	ImageFor  func(run *core.Run) string // image of one task; nil or an empty result means Image
	// ToolImagesFor returns the tool images of one task: the images that hold
	// the harness CLI of the run. nil, or an empty result, means no tool image.
	ToolImagesFor func(run *core.Run) []core.TaskImage
	Now           func() time.Time // nil means time.Now
	Logger        *slog.Logger     // nil means slog.Default()
	// BootstrapTTL is the start timeout: the time a runner has to call session
	// start after the assignment. 0 means 30 minutes.
	BootstrapTTL time.Duration
	// LostAfter is the silence after which the dispatcher asks the backend about a session. 0 means 90 seconds.
	LostAfter time.Duration
	// MaxSessionSeconds goes into the task. It is also the maximum session
	// time: after it the dispatcher sends session.stop. 0 means 8 hours.
	MaxSessionSeconds int
	// QueueTimeout is the longest time of a run in queued. 0 means 30 minutes.
	QueueTimeout time.Duration
	// StopGrace is the time that a runner has for its snapshot after a stop
	// or a cancel. 0 means 2 minutes.
	StopGrace time.Duration
	// MaxSandboxes is the most open sessions. 0 means no limit.
	MaxSandboxes int
	// FactoryLimit returns the most open sessions of a factory. A result of
	// 0 means no limit. A nil function means no limit for all factories.
	FactoryLimit func(ctx context.Context, factoryID string) (int, error)
	// SweepEvery is the time between two orphan sweeps. 0 means 1 minute.
	SweepEvery time.Duration
	// OnRunChanged is called after each run write. It may be nil.
	OnRunChanged func(ctx context.Context, r *core.Run)
	// OnRunMessage is called after each conversation write. It may be nil.
	OnRunMessage func(ctx context.Context, m *core.RunMessage)
	// OnUpload stores an upload that an artifact or transcript.saved event reports. It may be nil.
	OnUpload func(ctx context.Context, runID string, session int, kind, name, uploadID string) error
	// RequiredHosts returns the hosts that a restricted run needs besides its own lists. It may be nil.
	RequiredHosts func(run *core.Run) []string
	// Preflight checks a queued run before it gets a task. An error with a
	// failure code (core.FailureCodeError) of the user class fails the run
	// with that code, and no task starts. Other errors leave the run queued.
	// It may be nil.
	Preflight func(ctx context.Context, run *core.Run) error
	// Spend checks the spend limits. A run over a limit waits in
	// queued with the reason spend_limit, and a turn end over a limit stops
	// the run tree with spend_limit_exceeded. Nil means no spend limits.
	Spend *spend.Tracker
}

const (
	defaultBootstrapTTL      = 30 * time.Minute
	defaultLostAfter         = 90 * time.Second
	defaultMaxSessionSeconds = 8 * 60 * 60
	defaultImage             = "local"
	claimBatch               = 50
	workerClaimBatch         = 500 // a replica that is not the leader looks further for worker runs
	openSessionBatch         = 200
	sessionUpdateRetries     = 3
	defaultQueueTimeout      = 30 * time.Minute
	defaultStopGrace         = 2 * time.Minute
	defaultSweepEvery        = time.Minute
	// capacityBatch is the most open sessions that a capacity check reads.
	capacityBatch = 10000
)

// Dispatcher starts tasks, tracks runs and delivers messages.
type Dispatcher struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu               sync.RWMutex
	commander        Commander
	attachmentLookup AttachmentLookup // guarded by mu, see attachments.go

	sweepMu   sync.Mutex
	lastSweep time.Time
}

var _ runnerapi.Hooks = (*Dispatcher)(nil)

// New checks the config and makes a Dispatcher.
func New(cfg Config) (*Dispatcher, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("dispatch: Store is nil")
	case cfg.Runner == nil:
		return nil, errors.New("dispatch: Runner is nil")
	case cfg.Backend == nil:
		return nil, errors.New("dispatch: Backend is nil")
	case cfg.ControlURL == "":
		return nil, errors.New("dispatch: ControlURL is empty")
	}
	if cfg.BootstrapTTL <= 0 {
		cfg.BootstrapTTL = defaultBootstrapTTL
	}
	if cfg.LostAfter <= 0 {
		cfg.LostAfter = defaultLostAfter
	}
	if cfg.MaxSessionSeconds <= 0 {
		cfg.MaxSessionSeconds = defaultMaxSessionSeconds
	}
	if cfg.QueueTimeout <= 0 {
		cfg.QueueTimeout = defaultQueueTimeout
	}
	if cfg.StopGrace <= 0 {
		cfg.StopGrace = defaultStopGrace
	}
	if cfg.SweepEvery <= 0 {
		cfg.SweepEvery = defaultSweepEvery
	}
	if cfg.MaxSandboxes < 0 {
		cfg.MaxSandboxes = 0
	}
	if cfg.Image == "" {
		cfg.Image = defaultImage
	}
	if cfg.ReplicaID == "" {
		cfg.ReplicaID = core.NewID("replica")
	}
	d := &Dispatcher{cfg: cfg, log: cfg.Logger, now: cfg.Now}
	if d.log == nil {
		d.log = slog.Default()
	}
	if d.now == nil {
		d.now = time.Now
	}
	return d, nil
}

// SetCommander sets the commander. The server calls it after it made the
// runnerapi service, because that service needs the dispatcher as its Hooks.
func (d *Dispatcher) SetCommander(c Commander) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.commander = c
}

func (d *Dispatcher) getCommander() Commander {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.commander
}

// mutateRun reads a run, calls apply, and writes the run when apply says so.
// The read and the write are in one transaction if the store has them.
// It returns the run and whether it was written.
func (d *Dispatcher) mutateRun(ctx context.Context, runID string, apply func(r *core.Run) bool) (*core.Run, bool, error) {
	var out *core.Run
	wrote := false
	body := func(ctx context.Context) error {
		wrote = false
		r, err := d.cfg.Store.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		out = r
		if !apply(r) {
			return nil
		}
		if err := d.cfg.Store.UpdateRun(ctx, r); err != nil {
			return err
		}
		wrote = true
		return nil
	}
	// A conflict means another writer changed the run after the read. Read
	// again and apply again.
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		if tx, ok := d.cfg.Store.(core.Transactor); ok {
			err = tx.InTx(ctx, body)
		} else {
			err = body(ctx)
		}
		if !errors.Is(err, core.ErrConflict) {
			break
		}
	}
	if err != nil {
		return nil, false, err
	}
	if wrote && out.State.Terminal() {
		// The run ended: its event context is not needed any more. A
		// failure is not fatal. The leader sweep removes the keys later.
		if _, err := eventctx.Release(context.WithoutCancel(ctx), d.cfg.Store, out.ID); err != nil {
			d.log.Warn("release event context", "run", out.ID, "err", err)
		}
	}
	if wrote && d.cfg.OnRunChanged != nil {
		d.cfg.OnRunChanged(ctx, out)
	}
	return out, wrote, nil
}

// transition applies one run event with runstate and writes the run. The bool
// is true when the state changed. An invalid transition is logged and nothing
// is written. The mutate function runs also when the state did not change.
func (d *Dispatcher) transition(ctx context.Context, runID string, in runstate.Input, mutate func(*core.Run)) (*core.Run, bool, error) {
	changed := false
	var decision runstate.Result
	r, _, err := d.mutateRun(ctx, runID, func(r *core.Run) bool {
		changed = false
		res, err := runstate.Next(r.State, in)
		decision = res
		if err != nil {
			d.log.Warn("dispatch: run event refused", "run", runID, "event", in.Event, "state", r.State, "err", err)
			return false
		}
		if res.Changed {
			changed = true
			d.applyResult(r, res)
		}
		if mutate != nil {
			mutate(r)
			return true
		}
		return changed
	})
	if err == nil && changed {
		d.timelineTransition(ctx, r, in, decision)
	}
	return r, changed, err
}

// update changes fields of a run without a run event.
func (d *Dispatcher) update(ctx context.Context, runID string, mutate func(*core.Run)) (*core.Run, error) {
	r, _, err := d.mutateRun(ctx, runID, func(r *core.Run) bool {
		mutate(r)
		return true
	})
	return r, err
}

func (d *Dispatcher) applyResult(r *core.Run, res runstate.Result) {
	now := d.now()
	r.State = res.State
	if res.State != core.RunBlocked {
		r.BlockedReason = ""
	}
	if res.State != core.RunQueued {
		r.QueuedReason = ""
	}
	switch res.State {
	case core.RunFailed, core.RunError:
		r.FailureCode = res.FailureCode
		r.Error = string(res.FailureCode)
		r.FinishedAt = &now
	case core.RunBlocked:
		r.BlockedReason = res.BlockedReason
	case core.RunSucceeded, core.RunCancelled:
		r.FinishedAt = &now
	case core.RunRunning:
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
		r.Error = ""
		r.FailureCode = ""
	case core.RunQueued:
		r.QueuedAt = &now
		r.FinishedAt = nil
		r.Error = ""
		r.FailureCode = ""
	}
}

// maxFailureMessage is the longest text that a runner adds to the error of
// a failed run.
const maxFailureMessage = 8000

// setFailureMessage adds the text that a runner gave for a failure to the
// error of the run, after the failure code: "code: text". The failure code
// stays in failure_code and at the start of the error. The text is the step
// and the output of a failed setup, or the error of a failed turn. It does
// nothing when the run did not fail, or when another text is already there
// (for example the text of a spend limit).
func setFailureMessage(r *core.Run, msg string) {
	msg = strings.ToValidUTF8(strings.TrimSpace(msg), "")
	if msg == "" || r.FailureCode == "" || (r.State != core.RunFailed && r.State != core.RunError) {
		return
	}
	if r.Error != string(r.FailureCode) {
		return
	}
	if len(msg) > maxFailureMessage {
		msg = strings.ToValidUTF8(msg[:maxFailureMessage], "") + "..."
	}
	r.Error = string(r.FailureCode) + ": " + msg
}

// updateSession changes a session with a version check. It retries on a
// conflict with a fresh read. The change function returns false to stop
// without a write. The bool result is true when the session was written.
func (d *Dispatcher) updateSession(ctx context.Context, runID string, n int, change func(*core.RunSession) bool) (*core.RunSession, bool, error) {
	for attempt := 0; ; attempt++ {
		s, err := d.cfg.Runner.GetRunSession(ctx, runID, n)
		if err != nil {
			return nil, false, err
		}
		if !change(s) {
			return s, false, nil
		}
		err = d.cfg.Runner.UpdateRunSession(ctx, s)
		if err == nil {
			return s, true, nil
		}
		if !errors.Is(err, core.ErrConflict) || attempt >= sessionUpdateRetries {
			return nil, false, err
		}
	}
}

// endSession closes a session. It returns false if the session was already
// ended, so that only one caller does the follow-up work.
func (d *Dispatcher) endSession(ctx context.Context, runID string, n int, reason string, sandbox core.SandboxReason) (*core.RunSession, bool, error) {
	now := d.now()
	s, wrote, err := d.updateSession(ctx, runID, n, func(s *core.RunSession) bool {
		if s.State == core.SessionEnded {
			return false
		}
		s.State = core.SessionEnded
		s.EndReason = reason
		s.SandboxReason = sandbox
		s.EndedAt = &now
		s.ComputeSeconds = computeSeconds(s, now)
		s.ActiveTurnID = ""
		return true
	})
	if err == nil && wrote {
		d.timelineSessionEnded(ctx, runID, n, reason, sandbox)
	}
	return s, wrote, err
}

// computeSeconds is the life of the sandbox of a session until end: from the
// start of the session, or from its assignment if it never started.
func computeSeconds(s *core.RunSession, end time.Time) float64 {
	from := s.AssignedAt
	if s.StartedAt != nil {
		from = *s.StartedAt
	}
	if from.IsZero() || end.Before(from) {
		return 0
	}
	return end.Sub(from).Seconds()
}

// priorRetries counts the ended sessions before the session before, newest
// first, that ended with "lost" or "not_started". It stops at the first other one.
func (d *Dispatcher) priorRetries(ctx context.Context, runID string, before int) int {
	list, err := d.cfg.Runner.ListRunSessions(ctx, runID)
	if err != nil {
		d.log.Warn("dispatch: list sessions", "run", runID, "err", err)
		return 0
	}
	count := 0
	for i := len(list) - 1; i >= 0; i-- {
		s := list[i]
		if s.Session >= before {
			continue
		}
		if s.State != core.SessionEnded || (s.EndReason != "lost" && s.EndReason != "not_started") {
			break
		}
		count++
	}
	return count
}

func (d *Dispatcher) destroy(ctx context.Context, s *core.RunSession) {
	if err := d.backendOf(s).Destroy(ctx, s.TaskID); err != nil {
		d.log.Warn("dispatch: destroy task", "task", s.TaskID, "err", err)
	}
}

// Tick does one pass of the work: claim queued runs, deliver pending
// messages, check silent sessions. It returns the first error after it tried
// all the work. Each replica calls it in a loop.
func (d *Dispatcher) Tick(ctx context.Context) error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	// Only the leader claims runs of the built-in backend, stops sessions at
	// their deadline and sweeps orphan tasks. One
	// claimer makes the capacity limits exact across replicas. Each replica
	// claims the runs of the remote workers that are connected to it.
	leader := d.isLeader(ctx)
	keep(d.claimQueued(ctx, leader))
	open, err := d.cfg.Runner.ListOpenRunSessions(ctx, openSessionBatch)
	if err != nil {
		keep(err)
		return first
	}
	d.deliverAll(ctx, open)
	if leader {
		keep(d.checkDeadlines(ctx, open))
	}
	keep(d.checkSilent(ctx, open))
	if leader {
		keep(d.sweepIfDue(ctx))
	}
	return first
}
