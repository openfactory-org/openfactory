// Package engine is the orchestration core of Open Factory: it applies factory
// definitions, routes inbound events to work items and automations, manages the
// run tree (foreman and stage agents), human decisions, and work-item stages.
// The Planner in this package computes the content of a runner session.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/store/coord"
)

// Options configures an Engine. Store and Bus are required.
type Options struct {
	Store   core.Store
	Bus     core.EventBus
	Compile core.DefinitionCompiler
	// Match reports whether an inbound event matches an automation (filters, schedules, bot policy).
	Match   func(ev *core.Event, a core.Automation) bool
	Secrets core.SecretStore
	Forges  core.ForgeResolver
	// LinkURL returns the account-linking URL for an external actor, used in
	// GitHub onboarding notices (automations.githubOnboardingNotices). Optional.
	LinkURL func(teamID string, a core.Actor) string
	// Locker serializes mutations of one entity (work item, run, factory)
	// across replicas. Default: a coord.Locker on the store's
	// core.Coordinator when it implements one, else process-local locks.
	Locker *coord.Locker
	// ReplicaID names lock holders (default coord.ReplicaID("")).
	ReplicaID string
	Clock     func() time.Time
	Logger    *slog.Logger
	// Runner routes messages, cancels and interrupts to the runner path (the
	// dispatcher). Nil means that every message, cancel and interrupt fails
	// with core.ErrUnavailable. Only tests and tools that start no run leave it
	// nil.
	Runner RunnerRoute
	// PublicURL is the external URL of the server, for run links in lifecycle messages. Optional.
	PublicURL string
	// Leader reports whether this replica is the leader. Only the leader
	// removes old dispatch records. Nil means this replica is the leader.
	Leader core.Leadership
	// EventContext are the context builders of the integrations. The
	// builders that read only the payload are always on.
	EventContext []eventctx.Builder
	// MaxOrchestrationDepth is OF_MAX_ORCHESTRATION_DEPTH. A run at
	// depth d can start child runs only if d is less than this value. 0 or
	// less means 1: only a root run starts agents.
	MaxOrchestrationDepth int
}

// RunnerRoute is the part of the dispatcher that the engine uses.
type RunnerRoute interface {
	Send(ctx context.Context, m *core.RunInput) (outcome string, err error)
	StopSession(ctx context.Context, runID string) error
	Interrupt(ctx context.Context, runID string) error
}

// noRunner is the route of an engine that has no runner. Each call fails.
type noRunner struct{}

func (noRunner) Send(context.Context, *core.RunInput) (string, error) {
	return "", fmt.Errorf("%w: no runner is configured", core.ErrUnavailable)
}

func (noRunner) StopSession(context.Context, string) error {
	return fmt.Errorf("%w: no runner is configured", core.ErrUnavailable)
}

func (noRunner) Interrupt(context.Context, string) error {
	return fmt.Errorf("%w: no runner is configured", core.ErrUnavailable)
}

// Engine implements core.Engine.
type Engine struct {
	store   core.Store
	bus     core.EventBus
	compile core.DefinitionCompiler
	match   func(ev *core.Event, a core.Automation) bool
	secrets core.SecretStore
	forges  core.ForgeResolver
	clock   func() time.Time

	linkURL atomic.Pointer[func(teamID string, a core.Actor) string]
	log     *slog.Logger

	locker *coord.Locker
	wg     sync.WaitGroup
	runner RunnerRoute

	publicURL string
	leader    core.Leadership
	evctx     atomic.Pointer[[]eventctx.Builder]

	pre preflightDeps // see preflight.go

	maxDepth int // OF_MAX_ORCHESTRATION_DEPTH, at least 1
}

var _ core.Engine = (*Engine)(nil)

// SetLinkURL sets the account-linking URL builder used in GitHub onboarding
// notices (the linker is usually created after the engine).
func (e *Engine) SetLinkURL(fn func(teamID string, a core.Actor) string) {
	e.linkURL.Store(&fn)
}

// New constructs an Engine.
func New(opts Options) *Engine {
	e := &Engine{
		store:   opts.Store,
		bus:     opts.Bus,
		compile: opts.Compile,
		match:   opts.Match,
		secrets: opts.Secrets,
		forges:  opts.Forges,
		clock:   opts.Clock,

		log:       opts.Logger,
		runner:    opts.Runner,
		publicURL: opts.PublicURL,
		leader:    opts.Leader,
		maxDepth:  opts.MaxOrchestrationDepth,
	}
	if e.maxDepth < 1 {
		e.maxDepth = 1
	}
	if opts.LinkURL != nil {
		e.SetLinkURL(opts.LinkURL)
	}
	if e.runner == nil {
		e.runner = noRunner{}
	}
	if e.clock == nil {
		e.clock = time.Now
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	e.log = e.log.With("component", "engine")
	e.locker = opts.Locker
	if e.locker == nil {
		var c core.Coordinator
		if sc, ok := opts.Store.(core.Coordinator); ok {
			c = sc
		}
		e.locker = coord.NewLocker(c, coord.LockerOptions{Holder: coord.NewHolder(opts.ReplicaID), Logger: e.log})
	}
	builders := append([]eventctx.Builder(nil), opts.EventContext...)
	e.evctx.Store(&builders)
	return e
}

// AddEventContext registers more context builders. Integrations that
// are created after the engine call it.
func (e *Engine) AddEventContext(b ...eventctx.Builder) {
	cur := e.eventContextBuilders()
	next := append(append([]eventctx.Builder(nil), cur...), b...)
	e.evctx.Store(&next)
}

func (e *Engine) eventContextBuilders() []eventctx.Builder {
	if p := e.evctx.Load(); p != nil {
		return *p
	}
	return nil
}

// Start subscribes the engine to inbound events and run updates. The loops stop
// when ctx is cancelled; call Wait to block until they have exited.
func (e *Engine) Start(ctx context.Context) error {
	if e.bus == nil {
		return errors.New("engine: no event bus configured")
	}
	// React to turn ends published on this replica only: with several
	// replicas every update is also relayed to the others, and exactly one
	// engine must report each turn end.
	var runs <-chan any
	var unsubRuns func()
	if ls, ok := e.bus.(interface {
		SubscribeLocal(topic string) (<-chan any, func())
	}); ok {
		runs, unsubRuns = ls.SubscribeLocal(core.TopicRunUpdated)
	} else {
		runs, unsubRuns = e.bus.Subscribe(core.TopicRunUpdated)
	}
	if q, ok := e.bus.(core.Queue); ok {
		// Durable bus: inbound events are acknowledged only after HandleEvent
		// returns, so a crash mid-way redelivers them (at least once).
		stop := q.Consume(core.TopicInboundEvent, e.handleInboundJob)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			<-ctx.Done()
			stop()
		}()
	} else {
		e.startInboundSubscriber(ctx)
	}
	e.startDispatchSweep(ctx)
	e.startEventContextSweep(ctx)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer unsubRuns()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-runs:
				if !ok {
					return
				}
				r, ok := msg.(*core.Run)
				if !ok || r == nil {
					continue
				}
				e.onRunUpdated(ctx, r)
			}
		}
	}()
	return nil
}

// handleInboundJob is the durable consumer of core.TopicInboundEvent. Errors
// that retrying cannot fix (invalid, not found, forbidden, conflict) are
// logged and acknowledged; other errors are returned so the job is retried.
// A retry is safe: HandleEvent records each dispatch together with its work
// and skips the dispatches that a failed attempt completed.
func (e *Engine) handleInboundJob(ctx context.Context, job core.Job) error {
	ev := &core.Event{}
	if err := json.Unmarshal(job.Payload, ev); err != nil {
		e.log.Error("decode inbound event; dropping it", "job_id", job.ID, "err", err)
		return nil
	}
	if ev.ID == "" {
		ev.ID = job.ID
	}
	if _, err := e.HandleEvent(ctx, ev); err != nil {
		if permanent(err) {
			e.log.Error("handle inbound event", "event_id", ev.ID, "provider", ev.Provider, "type", ev.Type, "err", err)
			return nil
		}
		return err
	}
	return nil
}

func permanent(err error) bool {
	for _, target := range []error{core.ErrInvalid, core.ErrNotFound, core.ErrForbidden, core.ErrUnauthorized, core.ErrConflict} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func (e *Engine) startInboundSubscriber(ctx context.Context) {
	inbound, unsubIn := e.bus.Subscribe(core.TopicInboundEvent)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer unsubIn()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-inbound:
				if !ok {
					return
				}
				ev, ok := msg.(*core.Event)
				if !ok || ev == nil {
					continue
				}
				if _, err := e.HandleEvent(ctx, ev); err != nil {
					e.log.Error("handle inbound event", "event_id", ev.ID, "provider", ev.Provider, "type", ev.Type, "err", err)
				}
			}
		}
	}()
}

// Wait blocks until the loops started by Start have exited.
func (e *Engine) Wait() { e.wg.Wait() }

// lockWait bounds the wait for a distributed lock before falling back to a
// process-local one (a database outage must not wedge the engine).
const lockWait = 2 * time.Minute

// lock serializes mutations of one entity (work item, run, factory) across
// every replica sharing the database.
func (e *Engine) lock(key string) func() {
	ctx, cancel := context.WithTimeout(context.Background(), lockWait)
	defer cancel()
	unlock, err := e.locker.Lock(ctx, "engine:"+key)
	if err == nil {
		return unlock
	}
	e.log.Error("distributed lock unavailable; continuing with a process-local lock", "key", key, "err", err)
	unlock, _ = localLocker.Lock(context.Background(), "engine:"+key)
	return unlock
}

var localLocker = coord.NewLocker(nil, coord.LockerOptions{})

func (e *Engine) now() time.Time { return e.clock().UTC() }

// publish publishes on the bus. Inside a transaction (Engine.Atomic) a bus
// that does not join transactions is only called after the commit.
func (e *Engine) publish(ctx context.Context, topic string, payload any) {
	if e.bus == nil {
		return
	}
	if _, ok := e.bus.(txAware); ok || !core.InTransaction(ctx) {
		e.bus.Publish(ctx, topic, payload)
		return
	}
	core.AfterCommit(ctx, func() { e.bus.Publish(context.WithoutCancel(ctx), topic, payload) })
}

func (e *Engine) publishRun(ctx context.Context, r *core.Run) {
	cp := *r
	e.publish(ctx, core.TopicRunUpdated, &cp)
}

func (e *Engine) publishWorkItem(ctx context.Context, w *core.WorkItem) {
	cp := *w
	e.publish(ctx, core.TopicWorkItem, &cp)
}

// timeline appends a work item event; failures are logged, not returned.
func (e *Engine) timeline(ctx context.Context, workItemID, kind, runID, actor string, data any) {
	if workItemID == "" {
		return
	}
	ev := &core.WorkItemEvent{
		ID:         core.NewID("wie"),
		WorkItemID: workItemID,
		Kind:       kind,
		RunID:      runID,
		Actor:      actor,
		CreatedAt:  e.now(),
	}
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			ev.Data = b
		}
	}
	if err := e.store.AppendWorkItemEvent(ctx, ev); err != nil {
		e.log.Warn("append work item event", "work_item_id", workItemID, "kind", kind, "err", err)
	}
}

// notify publishes an outbound message to the work item's source.
func (e *Engine) notify(ctx context.Context, wi *core.WorkItem, kind, text string, options []string) {
	e.notifyInbox(ctx, wi, kind, text, options, "")
}

// notifyInbox is notify for messages tied to an inbox item (approvals, questions).
// A reply goes into the sub-thread of the last text action of the work
// item, for example a review thread.
func (e *Engine) notifyInbox(ctx context.Context, wi *core.WorkItem, kind, text string, options []string, inboxID string) {
	e.notifyTo(ctx, wi, kind, text, options, inboxID, true)
}

// notifyTop is notify without the sub-thread: the message is a top-level
// reply on the source (for example the completion notice).
func (e *Engine) notifyTop(ctx context.Context, wi *core.WorkItem, kind, text string) {
	e.notifyTo(ctx, wi, kind, text, nil, "", false)
}

func (e *Engine) notifyTo(ctx context.Context, wi *core.WorkItem, kind, text string, options []string, inboxID string, inThread bool) {
	if wi == nil || wi.Source.Provider == "" || text == "" {
		return
	}
	src := wi.Source
	if inThread {
		if t, ok := e.replyThread(ctx, wi); ok {
			src = t
		}
	} else {
		src.ThreadRef = ""
	}
	e.publish(ctx, core.TopicOutboundNotice, &core.OutboundMessage{
		WorkItemID: wi.ID,
		FactoryID:  wi.FactoryID,
		Source:     src,
		Text:       text,
		Kind:       kind,
		Options:    options,
		InboxID:    inboxID,
		ThreadRef:  src.ThreadRef,
	})
}

func isNotFound(err error) bool { return errors.Is(err, core.ErrNotFound) }

// foremanOf returns the factory's foreman agent.
func (e *Engine) foremanOf(ctx context.Context, factoryID string) (*core.AgentConfig, error) {
	agents, err := e.store.ListAgents(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	for i := range agents {
		if agents[i].Type == core.AgentForeman {
			return &agents[i], nil
		}
	}
	return nil, fmt.Errorf("%w: factory %s has no foreman agent", core.ErrNotFound, factoryID)
}

// resolveAgent finds an agent by name (empty = foreman, "MAIN" alias accepted).
func (e *Engine) resolveAgent(ctx context.Context, factoryID, name string) (*core.AgentConfig, error) {
	if name == "" || name == "MAIN" || name == string(core.AgentForeman) {
		if name != "" {
			if a, err := e.store.GetAgentByName(ctx, factoryID, name); err == nil {
				return a, nil
			}
		}
		return e.foremanOf(ctx, factoryID)
	}
	a, err := e.store.GetAgentByName(ctx, factoryID, name)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: agent %q not found in factory", core.ErrNotFound, name)
		}
		return nil, err
	}
	return a, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…[truncated %d bytes]", len(s)-cut)
}
