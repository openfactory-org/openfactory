package dispatch

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/networkpolicy"
	"github.com/openfactory-org/openfactory/internal/runnerapi"
	"github.com/openfactory-org/openfactory/internal/runstate"
	"github.com/openfactory-org/openfactory/internal/spend"
)

// claimQueued starts a session for each queued run. It returns the first error.
//
// The leader claims every run and applies the queue timeout. A replica that
// is not the leader claims only the runs for remote workers that are
// connected to it: a worker connection lives on one replica, and only that
// replica can give the task to the worker. The atomic session create keeps
// two replicas from claiming the same run.
func (d *Dispatcher) claimQueued(ctx context.Context, leader bool) error {
	limit := claimBatch
	if !leader {
		if d.cfg.Router == nil {
			return nil
		}
		limit = workerClaimBatch
	}
	runs, err := d.cfg.Store.ListRuns(ctx, core.RunFilter{
		States:   []core.RunState{core.RunQueued},
		ListOpts: core.ListOpts{Limit: limit},
	})
	if err != nil {
		return fmt.Errorf("list queued runs: %w", err)
	}
	// Oldest run first.
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.Before(runs[j].CreatedAt) })
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	var capa *capacity
	if len(runs) > 0 && (d.cfg.MaxSandboxes > 0 || d.cfg.FactoryLimit != nil) {
		c, err := d.newCapacity(ctx)
		if err != nil {
			// Without a count the limits cannot hold. Claim nothing, but
			// still apply the queue timeout.
			keep(fmt.Errorf("count open sessions: %w", err))
			c = &capacity{refused: true}
		}
		capa = c
	}
	spendPass := map[string]*spend.Limit{}
	for i := range runs {
		run := &runs[i]
		if !leader && !d.usesWorker(run) {
			continue
		}
		if d.cfg.Spend != nil {
			held, err := d.spendHold(ctx, run, spendPass)
			if err != nil {
				keep(fmt.Errorf("spend check of run %s: %w", run.ID, err))
				continue
			}
			if held {
				continue
			}
		}
		if leader && d.queueExpired(run) {
			if _, _, err := d.transition(ctx, run.ID, runstate.Input{Event: runstate.EventQueueTimeout}, nil); err != nil {
				keep(fmt.Errorf("queue timeout of run %s: %w", run.ID, err))
			}
			continue
		}
		if capa != nil {
			ok, err := capa.allow(ctx, d, run)
			if err != nil {
				keep(fmt.Errorf("capacity check of run %s: %w", run.ID, err))
			}
			if !ok {
				continue
			}
		}
		claimed, err := d.claimRun(ctx, run)
		if err != nil {
			keep(fmt.Errorf("claim run %s: %w", run.ID, err))
		}
		if claimed && capa != nil {
			capa.add(d, run)
		}
	}
	return first
}

// queueExpired tells that a queued run waited longer than the queue timeout.
// The wait counts from QueuedAt, or from CreatedAt when QueuedAt is not set.
func (d *Dispatcher) queueExpired(run *core.Run) bool {
	since := run.CreatedAt
	if run.QueuedAt != nil {
		since = *run.QueuedAt
	}
	return d.now().Sub(since) > d.cfg.QueueTimeout
}

// nextSession returns the number of the next session of the run. The bool is
// false when the latest session is not ended.
func (d *Dispatcher) nextSession(ctx context.Context, runID string) (int, bool, error) {
	latest, err := d.cfg.Runner.LatestRunSession(ctx, runID)
	if errors.Is(err, core.ErrNotFound) {
		return 1, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	if latest.State != core.SessionEnded {
		return 0, false, nil
	}
	return latest.Session + 1, true, nil
}

// claimRun starts a session for one run. The bool is true when the run got a
// task that the backend accepted.
func (d *Dispatcher) claimRun(ctx context.Context, run *core.Run) (bool, error) {
	n, ok, err := d.nextSession(ctx, run.ID)
	if err != nil || !ok {
		return false, err
	}
	if d.cfg.Preflight != nil {
		if err := d.cfg.Preflight(ctx, run); err != nil {
			var fe *core.FailureCodeError
			if !errors.As(err, &fe) {
				return false, fmt.Errorf("preflight: %w", err)
			}
			d.log.Info("dispatch: preflight failed", "run", run.ID, "code", fe.Code, "err", err)
			in := runstate.Input{Event: runstate.EventConfigFault, FailureCode: fe.Code}
			if _, _, terr := d.transition(ctx, run.ID, in, nil); terr != nil {
				return false, fmt.Errorf("preflight fault of run %s: %w", run.ID, terr)
			}
			return false, nil
		}
	}
	backend, remote := d.pickBackend(run)
	if backend == nil {
		return false, nil // no worker can take the task now: the run stays queued
	}
	token, hash, err := runnerapi.IssueBootstrap()
	if err != nil {
		return false, fmt.Errorf("issue bootstrap token: %w", err)
	}
	now := d.now()
	sess := &core.RunSession{
		RunID:              run.ID,
		Session:            n,
		TaskID:             core.TaskID(run.ID, n),
		BootstrapHash:      hash,
		BootstrapExpiresAt: now.Add(d.cfg.BootstrapTTL),
		Backend:            backend.Name(),
		InstanceShape:      core.InstanceShapeOf(run.Config.Machine),
		AssignedAt:         now,
		LastSeenAt:         now,
	}
	if err := d.cfg.Runner.CreateRunSession(ctx, sess); err != nil {
		if errors.Is(err, core.ErrConflict) {
			return false, nil // another replica won
		}
		return false, fmt.Errorf("create session: %w", err)
	}
	if err := d.ensurePrompt(ctx, run); err != nil {
		d.abandon(ctx, run.ID, n)
		return false, fmt.Errorf("queue prompt: %w", err)
	}
	if n == 1 {
		// The engine queues a new run without the dispatcher. Record it here.
		at := run.CreatedAt
		if run.QueuedAt != nil {
			at = *run.QueuedAt
		}
		d.timelineAt(ctx, run.ID, 0, "run.queued", queuedEventID(run), at, map[string]any{"reason": "new"})
	}
	_, changed, err := d.transition(ctx, run.ID, runstate.Input{Event: runstate.EventAssigned}, nil)
	if err != nil {
		d.abandon(ctx, run.ID, n)
		return false, fmt.Errorf("assign run: %w", err)
	}
	if !changed {
		// The run left the queue (for example it was cancelled). No task.
		d.abandon(ctx, run.ID, n)
		return false, nil
	}
	task := d.buildTask(run, n, token)
	if remote && d.cfg.PublicControlURL != "" {
		task.Env[core.TaskEnvControlURL] = d.cfg.PublicControlURL
	}
	if err := d.startTask(ctx, backend, task); err != nil {
		d.startFailed(ctx, run.ID, n, err)
		return false, nil
	}
	assigned := map[string]any{"backend": backend.Name()}
	if remote {
		assigned["worker_id"] = run.Config.WorkerHost
	}
	d.timelineAt(ctx, run.ID, n, "sandbox.assigned", "sandbox.assigned:"+task.TaskID, now, assigned)
	return true, nil
}

// ensurePrompt puts the first prompt in the queue. The dedupe key makes it safe to repeat.
func (d *Dispatcher) ensurePrompt(ctx context.Context, run *core.Run) error {
	if run.Prompt == "" {
		return nil
	}
	_, err := d.cfg.Runner.AppendRunInput(ctx, &core.RunInput{
		RunID:     run.ID,
		Kind:      "user",
		Content:   run.Prompt,
		Sender:    core.RunInputSender{Type: "platform"},
		DedupeKey: "prompt",
	})
	return err
}

func (d *Dispatcher) buildTask(run *core.Run, n int, token string) core.Task {
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	image := d.cfg.Image
	if d.cfg.ImageFor != nil {
		if v := strings.TrimSpace(d.cfg.ImageFor(run)); v != "" {
			image = v
		}
	}
	user := ""
	if run.Config.Machine != nil {
		user = run.Config.Machine.User // platform.linux.user
	}
	// A restricted policy goes to the backend. An unrestricted run has none.
	var policy *core.TaskNetworkPolicy
	if np := run.Config.NetworkPolicy; np.Restricted {
		// Required hosts are allowed, and deniedHosts cannot block them.
		if d.cfg.RequiredHosts != nil {
			np = networkpolicy.WithRequired(np, d.cfg.RequiredHosts(run)...)
		}
		policy = &core.TaskNetworkPolicy{
			Mode:         "restricted",
			AllowedHosts: sortedUnique(append([]string(nil), np.AllowedHosts...)),
			DeniedHosts:  append([]string(nil), np.DeniedHosts...),
		}
	}
	var toolImages []core.TaskImage
	if d.cfg.ToolImagesFor != nil {
		toolImages = append(toolImages, d.cfg.ToolImagesFor(run)...)
	}
	return core.Task{
		TaskID:        core.TaskID(run.ID, n),
		RunID:         run.ID,
		Session:       n,
		TeamID:        run.TeamID,
		FactoryID:     run.FactoryID,
		Image:         image,
		ToolImages:    toolImages,
		NetworkPolicy: policy,
		Platform:      core.TaskPlatform{OS: "linux", Arch: arch, User: user},
		Env: map[string]string{
			core.TaskEnvControlURL:     d.cfg.ControlURL,
			core.TaskEnvRunID:          run.ID,
			core.TaskEnvSession:        strconv.Itoa(n),
			core.TaskEnvBootstrapToken: token,
		},
		MaxSessionSeconds: d.cfg.MaxSessionSeconds,
	}
}

func (d *Dispatcher) startTask(ctx context.Context, backend core.TaskBackend, task core.Task) error {
	if err := task.Validate(); err != nil {
		return fmt.Errorf("invalid task: %w", err)
	}
	_, err := backend.Start(ctx, task)
	return err
}

// startFailed ends the session that never started and applies the lost event.
func (d *Dispatcher) startFailed(ctx context.Context, runID string, n int, startErr error) {
	if _, _, err := d.endSession(ctx, runID, n, "not_started", ""); err != nil {
		d.log.Warn("dispatch: end session", "run", runID, "session", n, "err", err)
	}
	code := core.FailureSandboxCreateFailed
	prior := d.priorRetries(ctx, runID, n)
	if errors.Is(startErr, core.ErrAtCapacity) {
		code = core.FailureResourceUnavailable
		prior = 0
	} else if errors.Is(startErr, core.ErrInvalid) {
		// The backend refused the task, for example a restricted policy that
		// it cannot enforce. A retry gives the same result.
		code = core.FailureInvalidConfiguration
		prior = 0
		d.log.Warn("dispatch: start task", "run", runID, "session", n, "err", startErr)
	} else {
		d.log.Warn("dispatch: start task", "run", runID, "session", n, "err", startErr)
	}
	in := runstate.Input{Event: runstate.EventSessionLost, FailureCode: code, PriorRetries: prior}
	if _, _, err := d.transition(ctx, runID, in, nil); err != nil {
		d.log.Warn("dispatch: lost event", "run", runID, "err", err)
	}
}

// abandon ends a session that got no task.
func (d *Dispatcher) abandon(ctx context.Context, runID string, n int) {
	if _, _, err := d.endSession(ctx, runID, n, "not_started", ""); err != nil {
		d.log.Warn("dispatch: end session", "run", runID, "session", n, "err", err)
	}
}

// sortedUnique returns the sorted list of the distinct non-empty values.
func sortedUnique(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
