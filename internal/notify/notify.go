// Package notify routes outbound factory messages (core.TopicOutboundNotice)
// back to the work source they came from: Slack and Teams threads, Linear and
// Jira issues, and GitHub / GitLab / Azure DevOps issue and pull request
// threads. Each source provider is served by a core.Responder; deliveries are
// retried with exponential backoff and failures are logged and counted.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Defaults.
const (
	DefaultMaxAttempts = 5
	DefaultBaseBackoff = time.Second
	DefaultMaxBackoff  = 30 * time.Second
	DefaultConcurrency = 8
)

// ErrNoTeam is returned when the team of a work item cannot be determined.
var ErrNoTeam = errors.New("notify: cannot resolve team for work item")

// Providers whose work items are reported only on the work item itself (dashboard / API / MCP).
var silentProviders = map[string]bool{
	"": true, "api": true, "mcp": true, "webhook": true, "schedule": true, "factory": true, "dashboard": true,
	// A benchmark trial has no real source: its notices stay in the run (trial mode).
	"benchmark": true,
}

// Stats are delivery counters.
type Stats struct {
	Received   int64 `json:"received"`
	Delivered  int64 `json:"delivered"`
	Failed     int64 `json:"failed"`
	Retries    int64 `json:"retries"`
	Unroutable int64 `json:"unroutable"`
}

// Router subscribes to outbound notices and delivers them through responders.
type Router struct {
	// TeamOf resolves the team owning a work item. Required.
	TeamOf func(ctx context.Context, workItemID string) (string, error)
	// OnFailure is called after the last failed attempt (optional; e.g. to record an inbox failure).
	OnFailure   func(ctx context.Context, msg core.OutboundMessage, err error)
	Logger      *slog.Logger
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Concurrency int

	bus        core.EventBus
	responders map[string]core.Responder

	received, delivered, failed, retries, unroutable atomic.Int64

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	wg     sync.WaitGroup
}

// NewRouter creates a router. Responders are keyed by Provider(); later ones win.
func NewRouter(bus core.EventBus, responders ...core.Responder) *Router {
	r := &Router{bus: bus, responders: map[string]core.Responder{}}
	for _, rs := range responders {
		r.Add(rs)
	}
	return r
}

// Add registers (or replaces) a responder.
func (r *Router) Add(rs core.Responder) {
	if rs == nil {
		return
	}
	r.mu.Lock()
	r.responders[NormalizeProvider(rs.Provider())] = rs
	r.mu.Unlock()
}

// Providers lists providers with a responder, sorted.
func (r *Router) Providers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.responders))
	for p := range r.responders {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// NormalizeProvider maps provider spellings to responder keys.
func NormalizeProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	switch p {
	case "microsoft_teams", "microsoft-teams", "msteams":
		return "teams"
	case "azure-devops", "azuredevops":
		return "azure_devops"
	}
	return p
}

func (r *Router) log() *slog.Logger {
	if r.Logger == nil {
		return slog.Default().With("component", "notify")
	}
	return r.Logger
}

// Start subscribes to core.TopicOutboundNotice and delivers until ctx is cancelled or Stop is called.
func (r *Router) Start(ctx context.Context) {
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	r.mu.Unlock()

	ch, unsub := r.bus.Subscribe(core.TopicOutboundNotice)
	conc := r.Concurrency
	if conc <= 0 {
		conc = DefaultConcurrency
	}
	sem := make(chan struct{}, conc)
	go func() {
		defer close(r.done)
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				r.wg.Wait()
				return
			case v, ok := <-ch:
				if !ok {
					r.wg.Wait()
					return
				}
				msg, ok := asMessage(v)
				if !ok {
					r.log().Warn("ignoring outbound notice with unexpected payload", "type", fmt.Sprintf("%T", v))
					continue
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					r.wg.Wait()
					return
				}
				r.wg.Add(1)
				go func() {
					defer r.wg.Done()
					defer func() { <-sem }()
					_ = r.Deliver(ctx, msg)
				}()
			}
		}
	}()
}

// Stop ends the subscription and waits for in-flight deliveries.
func (r *Router) Stop() {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func asMessage(v any) (core.OutboundMessage, bool) {
	switch m := v.(type) {
	case *core.OutboundMessage:
		if m == nil {
			return core.OutboundMessage{}, false
		}
		return *m, true
	case core.OutboundMessage:
		return m, true
	}
	return core.OutboundMessage{}, false
}

// Stats returns delivery counters.
func (r *Router) Stats() Stats {
	return Stats{
		Received: r.received.Load(), Delivered: r.delivered.Load(), Failed: r.failed.Load(),
		Retries: r.retries.Load(), Unroutable: r.unroutable.Load(),
	}
}

// permanent reports whether retrying cannot help.
func permanent(err error) bool {
	if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrInvalid) || errors.Is(err, core.ErrForbidden) ||
		errors.Is(err, core.ErrOperationNotSupported) ||
		errors.Is(err, core.ErrUnauthorized) || errors.Is(err, context.Canceled) {
		return true
	}
	var t interface{ Temporary() bool }
	if errors.As(err, &t) {
		return !t.Temporary()
	}
	return false
}

func (r *Router) backoff(attempt int) time.Duration {
	base, maxB := r.BaseBackoff, r.MaxBackoff
	if base <= 0 {
		base = DefaultBaseBackoff
	}
	if maxB <= 0 {
		maxB = DefaultMaxBackoff
	}
	d := base << (attempt - 1)
	if d > maxB || d <= 0 {
		d = maxB
	}
	// ±20% jitter
	j := time.Duration(float64(d) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // G404: retry jitter needs no cryptographic randomness
	return j
}

// Deliver routes one message synchronously, retrying transient failures.
func (r *Router) Deliver(ctx context.Context, msg core.OutboundMessage) error {
	r.received.Add(1)
	// A tracker write-back has no factory argument: it reads the factory
	// from the context to select the integration record.
	ctx = core.WithFactory(ctx, msg.FactoryID)
	provider := NormalizeProvider(msg.Source.Provider)
	r.mu.Lock()
	rs, ok := r.responders[provider]
	r.mu.Unlock()
	if !ok {
		r.unroutable.Add(1)
		if !silentProviders[provider] {
			r.log().Warn("no responder for provider", "provider", provider, "work_item", msg.WorkItemID)
		}
		return nil
	}
	if r.TeamOf == nil {
		r.failed.Add(1)
		return fmt.Errorf("%w: no TeamOf configured", ErrNoTeam)
	}
	teamID, err := r.TeamOf(ctx, msg.WorkItemID)
	if err != nil || teamID == "" {
		r.failed.Add(1)
		if err == nil {
			err = ErrNoTeam
		}
		r.log().Error("resolve team for outbound message", "work_item", msg.WorkItemID, "err", err)
		r.fail(ctx, msg, err)
		return err
	}
	send := func() error { return rs.Reply(ctx, teamID, msg) }
	if core.IsTrackerOperation(msg.Kind) {
		// A write-back to a tracker. A responder that
		// is not a tracker skips it.
		t, ok := rs.(core.IssueTracker)
		if !ok {
			r.log().Debug("responder is not an issue tracker; write-back skipped", "provider", provider, "kind", msg.Kind)
			return nil
		}
		send = func() error {
			var err error
			switch msg.Kind {
			case core.OutboundIssueState:
				err = t.SetIssueState(ctx, teamID, msg.Source, msg.IssueState)
			case core.OutboundAttachPullRequest:
				if msg.PullRequest == nil {
					return fmt.Errorf("%w: attach_pull_request without a pull request", core.ErrInvalid)
				}
				err = t.AttachPullRequest(ctx, teamID, msg.Source, *msg.PullRequest)
			}
			if errors.Is(err, core.ErrOperationNotSupported) {
				r.log().Debug("tracker does not support the write-back", "provider", provider, "kind", msg.Kind)
				return nil
			}
			return err
		}
	}
	attempts := r.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}
	for attempt := 1; ; attempt++ {
		err = send()
		if err == nil {
			r.delivered.Add(1)
			return nil
		}
		if attempt >= attempts || permanent(err) || ctx.Err() != nil {
			break
		}
		r.retries.Add(1)
		wait := r.backoff(attempt)
		r.log().Warn("outbound delivery failed; retrying", "provider", provider, "work_item", msg.WorkItemID, "attempt", attempt, "retry_in", wait, "err", err)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			err = ctx.Err()
		case <-t.C:
			continue
		}
		break
	}
	r.failed.Add(1)
	r.log().Error("outbound delivery failed", "provider", provider, "work_item", msg.WorkItemID, "kind", msg.Kind, "err", err)
	r.fail(ctx, msg, err)
	return err
}

func (r *Router) fail(ctx context.Context, msg core.OutboundMessage, err error) {
	if r.OnFailure != nil {
		r.OnFailure(context.WithoutCancel(ctx), msg, err)
	}
}

// ---------- Notification routes ----------

// Route describes one place a factory's notifications can be delivered.
type Route struct {
	Provider    string            `json:"provider"`
	Kind        string            `json:"kind"` // inbox, chat, tracker, forge
	Destination string            `json:"destination"`
	Deliverable bool              `json:"deliverable"` // a responder is registered for the provider
	Settings    map[string]string `json:"settings,omitempty"`
}

var routeInfo = map[string]struct{ kind, dest string }{
	"slack":        {"chat", "Replies in the Slack thread or DM where the work started (approval options as buttons)"},
	"teams":        {"chat", "Replies in the Microsoft Teams channel thread where the work started"},
	"linear":       {"tracker", "Comments on the Linear issue, or activities in its agent session"},
	"jira":         {"tracker", "Comments on the Jira work item"},
	"github":       {"forge", "Comments on the originating GitHub issue or pull request"},
	"gitlab":       {"forge", "Notes on the originating GitLab merge request or issue"},
	"azure_devops": {"forge", "Comment threads on the originating Azure DevOps pull request or work item"},
}

// Routes lists the notification destinations of a factory: the dashboard inbox, the chat and
// tracker integrations attached in its definition, and the team's code-forge connections.
// available lists providers with a registered responder (nil = assume all are deliverable).
func Routes(ctx context.Context, store core.Store, factoryID string, available []string) ([]Route, error) {
	f, err := store.GetFactory(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	avail := map[string]bool{}
	for _, p := range available {
		avail[NormalizeProvider(p)] = true
	}
	deliverable := func(p string) bool { return available == nil || avail[p] }
	routes := []Route{{
		Provider: "dashboard", Kind: "inbox", Deliverable: true,
		Destination: "Factory inbox (approvals, questions, handoffs, failures) and the work item timeline",
	}}
	seen := map[string]bool{}
	add := func(p string, settings map[string]string) {
		p = NormalizeProvider(p)
		info, ok := routeInfo[p]
		if !ok || seen[p] {
			return
		}
		seen[p] = true
		routes = append(routes, Route{Provider: p, Kind: info.kind, Destination: info.dest, Deliverable: deliverable(p), Settings: settings})
	}
	attached, err := store.GetFactoryIntegrations(ctx, factoryID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	names := make([]string, 0, len(attached))
	for p := range attached {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		add(p, attached[p])
	}
	ints, err := store.ListIntegrations(ctx, f.TeamID)
	if err != nil {
		return nil, err
	}
	sort.Slice(ints, func(i, j int) bool { return ints[i].Provider < ints[j].Provider })
	for _, in := range ints {
		switch NormalizeProvider(in.Provider) {
		case "github", "gitlab", "azure_devops":
			add(in.Provider, nil)
		}
	}
	return routes, nil
}

// Routes is Routes with Deliverable reflecting this router's responders.
func (r *Router) Routes(ctx context.Context, store core.Store, factoryID string) ([]Route, error) {
	return Routes(ctx, store, factoryID, r.Providers())
}
