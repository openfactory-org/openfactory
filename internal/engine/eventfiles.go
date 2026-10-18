package engine

import (
	"context"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
)

// eventBundle builds the event context of a message.
// withContext runs the context builders of the provider; only
// the first message of a run gets them. It returns nil without an event.
func (e *Engine) eventBundle(ctx context.Context, msgID string, ev *core.Event, withContext bool) *eventctx.Bundle {
	if ev == nil {
		return nil
	}
	return eventctx.Build(ctx, msgID, ev, withContext, e.eventContextBuilders())
}

// storeEventBundle stores the event context of a message in the key/value
// store, in the transaction of the message. The dispatcher
// reads it when the message reaches the run. first marks the first prompt of
// a run: the queue gives that prompt its own ID.
func (e *Engine) storeEventBundle(ctx context.Context, runID string, first bool, b *eventctx.Bundle) error {
	if b == nil {
		return nil
	}
	raw, err := b.Encode()
	if err != nil {
		return err
	}
	if err := e.store.KVSet(ctx, eventctx.KVKey(b.MessageID), raw); err != nil {
		return err
	}
	if first {
		return e.store.KVSet(ctx, eventctx.PromptKVKey(runID), b.MessageID)
	}
	return nil
}

// followUpEvent sends a message that an event caused, with the event
// context (envelope and payload) of the event.
func (e *Engine) followUpEvent(ctx context.Context, runID, message, actor string, ev *core.Event) error {
	msgID := core.NewID("msg")
	b := e.eventBundle(ctx, msgID, ev, false)
	return e.sendMessageWith(ctx, runID, message+b.PromptSection(), actor, nil, msgID, b)
}

// releaseEventContext removes the event context of a run that ended.
// A failure is logged: the sweep of the leader removes the keys later.
func (e *Engine) releaseEventContext(ctx context.Context, runID string) {
	if _, err := eventctx.Release(context.WithoutCancel(ctx), e.store, runID); err != nil {
		e.log.Warn("release event context", "run_id", runID, "err", err)
	}
}

// SweepEventContext removes the event context that is older than
// eventctx.Retention. The leader calls it every hour.
func (e *Engine) SweepEventContext(ctx context.Context) (int, error) {
	return eventctx.Expire(ctx, e.store, e.now())
}

// eventContextSweepEvery is the interval of the event context sweep.
const eventContextSweepEvery = time.Hour

// startEventContextSweep removes old event context on the leader, like the
// dispatch sweep. A store without core.KVExpirer needs no sweep.
func (e *Engine) startEventContextSweep(ctx context.Context) {
	if _, ok := e.store.(core.KVExpirer); !ok {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(eventContextSweepEvery)
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
			n, err := e.SweepEventContext(ctx)
			if err != nil && ctx.Err() == nil {
				e.log.Warn("event context sweep", "err", err)
				continue
			}
			if n > 0 {
				e.log.Info("event context sweep removed old keys", "keys", n)
			}
		}
	}()
}
