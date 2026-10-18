package engine

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Atomic runs fn in one store transaction when the store implements
// core.Transactor: the state changes fn makes and the events it publishes
// with the context it receives commit together (transactional outbox), and
// in-process deliveries happen only after the commit. Without a Transactor fn
// runs directly and every write commits on its own (the previous behavior).
//
// fn must use only the context it receives for store calls and must not take
// engine locks (Engine.lock); take them before calling Atomic.
func (e *Engine) Atomic(ctx context.Context, fn func(ctx context.Context) error) error {
	return core.RunInTx(ctx, e.store, fn)
}

// txAware is implemented by buses whose Publish joins a store transaction
// carried by the context (events.Bus, events.Durable).
type txAware interface{ TxAware() bool }
