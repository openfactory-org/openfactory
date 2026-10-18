package core

import (
	"context"
	"time"
)

// DispatchRecord is one dispatch decision: one action given to one target.
// A target is an automation
// ("auto:<id>"), a work item ("wi:<id>") or a mention route ("mention:<factory id>").
type DispatchRecord struct {
	ActionKey string    `json:"action_key"`
	Target    string    `json:"target"`
	Ref       string    `json:"ref,omitempty"` // the run or message that the dispatch created
	CreatedAt time.Time `json:"created_at"`
}

// DispatchRetention is how long dispatch records are kept. The leader removes older records.
const DispatchRetention = 7 * 24 * time.Hour

// DispatchLedger is an optional Store capability: the table of dispatch
// records, with a unique key on (ActionKey, Target). Inside a transaction
// (Transactor.InTx) the methods join it, so a dispatch record commits together
// with the run or the message that the dispatch creates. A store without it
// gets a fallback in the key-value space that has no expiry.
type DispatchLedger interface {
	// InsertDispatch records d. It reports false when a record with the same
	// action key and target exists (and changes nothing).
	InsertDispatch(ctx context.Context, d *DispatchRecord) (bool, error)
	// GetDispatch returns the record of (actionKey, target), or ErrNotFound.
	GetDispatch(ctx context.Context, actionKey, target string) (*DispatchRecord, error)
	// PurgeDispatches deletes the records created before t and returns how many it deleted.
	PurgeDispatches(ctx context.Context, before time.Time) (int, error)
}
