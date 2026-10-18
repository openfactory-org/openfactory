package core

import (
	"context"
	"time"
)

// Webhook delivery retention.
const (
	// WebhookKeepNewest is how many deliveries of one webhook keep their body.
	WebhookKeepNewest = 50
	// WebhookIdentityRetention is how long the identity of a delivery (its
	// delivery ID, status and run IDs) stays after its body is removed. A
	// retry inside this time is still found as a duplicate.
	WebhookIdentityRetention = 7 * 24 * time.Hour
)

// WebhookPruneResult counts what PruneWebhookDeliveries changed.
type WebhookPruneResult struct {
	// Stripped is the number of rows that lost their body and headers.
	Stripped int
	// Deleted is the number of rows that were removed.
	Deleted int
}

// WebhookDeliveryPruner is an optional Store capability: retention of the
// table of webhook deliveries.
type WebhookDeliveryPruner interface {
	// PruneWebhookDeliveries applies the retention to each webhook. In the
	// order newest first, the first keepNewest rows of a webhook stay as they
	// are. Each older row loses its body and headers but keeps its identity;
	// when it was received before `before`, the row is deleted. A row that
	// lost its body is not changed again.
	PruneWebhookDeliveries(ctx context.Context, keepNewest int, before time.Time) (WebhookPruneResult, error)
}

// IdentityLinker is an optional Store capability: an atomic link of an
// external account. UpsertExternalIdentity replaces a row, so a check before
// it is a race.
type IdentityLinker interface {
	// LinkExternalIdentity binds x to x.UserID in one step. It writes the row
	// when no row exists for (Provider, ExternalID), when the row has no user
	// (an unlinked account), or when the row has the same user (the handle is
	// updated when x.Handle is not empty). It never changes a row of another
	// user. It returns the row as stored after the call, and linked, which
	// is true when that row belongs to x.UserID.
	LinkExternalIdentity(ctx context.Context, x *ExternalIdentity) (cur *ExternalIdentity, linked bool, err error)
}
