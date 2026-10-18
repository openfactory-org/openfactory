package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// This file holds the parts of event intake that make it safe to retry and
// that route one provider action one time:
// action keys, the dispatch ledger, the sender trust and the bot limit.

// ActionKey returns the action key of an event: the
// events of one provider action share it. An integration sets
// Event.ActionKey. Otherwise the key comes from the delivery ID, without the
// ":<event type>" suffix that integrations add for each event of one delivery.
// For a chat message it is the team, the channel and the message timestamp.
// It is empty when the event has no delivery identity.
func ActionKey(ev *core.Event) string {
	if ev == nil {
		return ""
	}
	if ev.ActionKey != "" {
		return ev.ActionKey
	}
	p := normalizeProvider(ev.Provider)
	if p == "slack" {
		if k := slackMessageKey(ev); k != "" {
			return k
		}
	}
	if ev.DeliveryID == "" {
		return ""
	}
	did := strings.TrimSuffix(ev.DeliveryID, ":"+ev.Type)
	return p + ":" + did
}

// slackMessageKey is team:channel:ts for a message event. A mention and the
// channel message of the same post then share one key.
func slackMessageKey(ev *core.Event) string {
	if len(ev.Payload) == 0 {
		return ""
	}
	var m struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
		Team    string `json:"team"`
	}
	if json.Unmarshal(ev.Payload, &m) != nil || m.Channel == "" || m.TS == "" {
		return ""
	}
	team := m.Team
	if team == "" {
		team = ev.TeamID
	}
	return "slack:" + team + ":" + m.Channel + ":" + m.TS
}

// ---------- Dispatch ledger ----------

// errAlreadyDispatched rolls back a transaction whose dispatch record exists:
// another delivery of the same action did the work.
var errAlreadyDispatched = errors.New("engine: action already dispatched to this target")

// ledger returns the store's dispatch table, or the key-value fallback.
func (e *Engine) ledger() core.DispatchLedger {
	if l, ok := e.store.(core.DispatchLedger); ok {
		return l
	}
	return kvLedger{st: e.store}
}

// kvLedger keeps dispatch records in the key-value space. It is used when the
// store has no dispatch table. It cannot delete records.
type kvLedger struct{ st core.Store }

func kvDispatchKey(actionKey, target string) string {
	return "engine:dispatch:" + actionKey + "|" + target
}

func (l kvLedger) InsertDispatch(ctx context.Context, d *core.DispatchRecord) (bool, error) {
	key := kvDispatchKey(d.ActionKey, d.Target)
	if v, err := l.st.KVGet(ctx, key); err == nil && v != "" {
		return false, nil
	} else if err != nil && !isNotFound(err) {
		return false, err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	return true, l.st.KVSet(ctx, key, string(b))
}

func (l kvLedger) GetDispatch(ctx context.Context, actionKey, target string) (*core.DispatchRecord, error) {
	v, err := l.st.KVGet(ctx, kvDispatchKey(actionKey, target))
	if err != nil {
		return nil, err
	}
	if v == "" {
		return nil, core.ErrNotFound
	}
	d := &core.DispatchRecord{}
	if err := json.Unmarshal([]byte(v), d); err != nil {
		return nil, fmt.Errorf("decode dispatch record: %w", err)
	}
	return d, nil
}

func (l kvLedger) PurgeDispatches(context.Context, time.Time) (int, error) { return 0, nil }

// dispatched reports whether the action was given to target already.
func (e *Engine) dispatched(ctx context.Context, actionKey, target string) (bool, error) {
	if actionKey == "" {
		return false, nil
	}
	_, err := e.ledger().GetDispatch(ctx, actionKey, target)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// markDispatched records a dispatch outside of the transaction of its work,
// for work that is idempotent on its own (an inbox resolution).
func (e *Engine) markDispatched(ctx context.Context, actionKey, target, ref string) {
	if actionKey == "" {
		return
	}
	if _, err := e.ledger().InsertDispatch(ctx, &core.DispatchRecord{ActionKey: actionKey, Target: target, Ref: ref, CreatedAt: e.now()}); err != nil {
		e.log.Warn("record dispatch", "action_key", actionKey, "target", target, "err", err)
	}
}

// dispatchClaim travels in the context of one dispatch. The transaction that
// creates the run or appends the message records it (recordClaim), so the
// dispatch record and its work commit together.
type dispatchClaim struct {
	actionKey string
	targets   []string
	recorded  bool
}

type dispatchClaimKey struct{}

func withClaim(ctx context.Context, actionKey string, targets ...string) context.Context {
	if actionKey == "" {
		return ctx
	}
	return context.WithValue(ctx, dispatchClaimKey{}, &dispatchClaim{actionKey: actionKey, targets: targets})
}

func claimFrom(ctx context.Context) *dispatchClaim {
	c, _ := ctx.Value(dispatchClaimKey{}).(*dispatchClaim)
	return c
}

// addClaimTarget adds a target to the claim of ctx, if any.
func addClaimTarget(ctx context.Context, target string) {
	if c := claimFrom(ctx); c != nil && !c.recorded {
		for _, t := range c.targets {
			if t == target {
				return
			}
		}
		c.targets = append(c.targets, target)
	}
}

// recordClaim inserts the dispatch records of the claim in ctx. It is called
// inside the transaction that creates ref. It returns errAlreadyDispatched
// when a record exists, which rolls the transaction back.
func (e *Engine) recordClaim(ctx context.Context, ref string) error {
	c := claimFrom(ctx)
	if c == nil || c.recorded {
		return nil
	}
	l := e.ledger()
	for _, t := range c.targets {
		ok, err := l.InsertDispatch(ctx, &core.DispatchRecord{ActionKey: c.actionKey, Target: t, Ref: ref, CreatedAt: e.now()})
		if err != nil {
			return err
		}
		if !ok {
			return errAlreadyDispatched
		}
	}
	c.recorded = true
	return nil
}

// unrecordClaim is called when the transaction of recordClaim failed.
func unrecordClaim(ctx context.Context) {
	if c := claimFrom(ctx); c != nil {
		c.recorded = false
	}
}

// SweepDispatches removes dispatch records older than core.DispatchRetention.
// The leader calls it every hour.
func (e *Engine) SweepDispatches(ctx context.Context) (int, error) {
	return e.ledger().PurgeDispatches(ctx, e.now().Add(-core.DispatchRetention))
}

// dispatchSweepEvery is the interval of the dispatch sweep.
const dispatchSweepEvery = time.Hour

func (e *Engine) startDispatchSweep(ctx context.Context) {
	if _, ok := e.store.(core.DispatchLedger); !ok {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(dispatchSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if e.leader != nil && !e.leader.IsLeader() {
				continue
			}
			n, err := e.SweepDispatches(ctx)
			if err != nil && ctx.Err() == nil {
				e.log.Warn("dispatch sweep", "err", err)
				continue
			}
			if n > 0 {
				e.log.Info("dispatch sweep removed old records", "records", n)
			}
		}
	}()
}

// ---------- Sender trust ----------

// isSelfEvent reports whether the product's own bot or app sent the event.
func isSelfEvent(ev *core.Event) bool {
	if ev.Actor.IsSelf {
		return true
	}
	b, _ := ev.Attributes["actor_is_self"].(bool)
	return b
}

// senderTrust sets ev.Actor.Trust. A sender is a member
// when it has a link to a user of the team of one of the factories. The
// integration resolves the link and sets Actor.UserID; the engine checks that
// the user exists and is in the team.
func (e *Engine) senderTrust(ctx context.Context, ev *core.Event, factories []core.Factory) string {
	p := normalizeProvider(ev.Provider)
	switch p {
	case "schedule", "system":
		ev.Actor.Trust = core.TrustPlatform
		return ev.Actor.Trust
	case "webhook":
		ev.Actor.Trust = core.TrustExternal
		return ev.Actor.Trust
	}
	ev.Actor.Trust = core.TrustExternal
	if ev.Actor.UserID == "" {
		return ev.Actor.Trust
	}
	u, err := e.store.GetUser(ctx, ev.Actor.UserID)
	if err != nil {
		if !isNotFound(err) {
			e.log.Warn("look up linked user", "user_id", ev.Actor.UserID, "err", err)
		}
		return ev.Actor.Trust
	}
	if (ev.TeamID != "" && u.TeamID == ev.TeamID) || factoryInTeam(factories, u.TeamID) {
		ev.Actor.Trust = core.TrustMember
	}
	return ev.Actor.Trust
}

// linkRequiredFor reports whether requireLinkedActor applies to the provider:
// forges, chats and trackers. It does not apply to custom webhooks, schedules,
// the API or MCP.
func linkRequiredFor(provider string) bool {
	switch normalizeProvider(provider) {
	case "webhook", "schedule", "system", "api", "mcp", "":
		return false
	}
	return true
}

// messageTrust is the trust of a user message to a work item:
// trusted reports whether the text goes to the agent as trusted
// input, canApprove and canAnswer whether it can resolve an open approval or
// question.
type messageTrust struct {
	trusted    bool
	canApprove bool
	canAnswer  bool
	reason     string
}

func (e *Engine) trustOfMessage(ctx context.Context, wi *core.WorkItem, ev *core.Event, rt *FactoryRuntime) messageTrust {
	if ev.Actor.Trust != core.TrustMember || ev.Actor.IsBot {
		return messageTrust{reason: "sender is not a linked team member"}
	}
	if rt.RestrictFollowupsToStarter && (wi.CreatorID == "" || wi.CreatorID != ev.Actor.UserID) {
		return messageTrust{reason: "restrictFollowupsToStarter: sender did not start the work item"}
	}
	mt := messageTrust{trusted: true, canAnswer: true, canApprove: true, reason: "linked team member"}
	if len(rt.Approvers) > 0 {
		u, err := e.store.GetUser(ctx, ev.Actor.UserID)
		if err != nil || !IsApprover(rt.Approvers, u) {
			mt.canApprove = false
			mt.reason = "linked team member, not in approvers"
		}
	}
	return mt
}

// ---------- Bot limit ----------

// Bot actions accepted for one work item in botWindow.
const (
	maxBotActions = 20
	botWindow     = time.Hour
)

type botActions struct {
	Times []time.Time `json:"times"`
	Noted time.Time   `json:"noted,omitempty"`
}

func botActionsKey(workItemID string) string { return "engine:botactions:" + workItemID }

// allowBotAction counts one bot action for a work item. It reports false when
// the work item had maxBotActions bot actions in the last hour. The first
// drop in a window writes a timeline entry. An action is counted one time.
func (e *Engine) allowBotAction(ctx context.Context, wi *core.WorkItem, ev *core.Event, actionKey string) bool {
	if actionKey != "" {
		if done, _ := e.dispatched(ctx, actionKey, "bot:"+wi.ID); done {
			return true
		}
	}
	key := botActionsKey(wi.ID)
	defer e.lock(key)()
	var st botActions
	if _, err := kvGetJSON(ctx, e.store, key, &st); err != nil {
		e.log.Warn("read bot action count", "work_item_id", wi.ID, "err", err)
	}
	now := e.now()
	kept := st.Times[:0]
	for _, t := range st.Times {
		if now.Sub(t) < botWindow {
			kept = append(kept, t)
		}
	}
	st.Times = kept
	if len(st.Times) >= maxBotActions {
		if now.Sub(st.Noted) >= botWindow {
			st.Noted = now
			e.timeline(ctx, wi.ID, "bot_limit", "", nonEmpty(ev.Actor.Handle, ev.Actor.ExternalID), map[string]any{
				"text": fmt.Sprintf("More than %d bot actions in one hour. Bot actions are dropped until the count falls.", maxBotActions),
			})
			_ = kvPutJSON(ctx, e.store, key, st)
		}
		return false
	}
	st.Times = append(st.Times, now)
	if err := kvPutJSON(ctx, e.store, key, st); err != nil {
		e.log.Warn("write bot action count", "work_item_id", wi.ID, "err", err)
	}
	e.markDispatched(ctx, actionKey, "bot:"+wi.ID, "")
	return true
}
