package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

var (
	_ core.DispatchLedger        = (*Store)(nil)
	_ core.WebhookDeliveryPruner = (*Store)(nil)
	_ core.IdentityLinker        = (*Store)(nil)
)

// ---------- Dispatch ledger (core.DispatchLedger) ----------

// InsertDispatch implements core.DispatchLedger. It joins the transaction of
// ctx, so the record commits with the run or the message that it belongs to.
// On PostgreSQL a second transaction with the same key waits for the first one
// and then inserts nothing.
func (s *Store) InsertDispatch(ctx context.Context, d *core.DispatchRecord) (bool, error) {
	if d.ActionKey == "" || d.Target == "" {
		return false, fmt.Errorf("%w: action key and target are required", core.ErrInvalid)
	}
	defaultTime(&d.CreatedAt)
	res, err := s.exec(ctx, s.conn(ctx),
		"INSERT INTO dispatches (action_key, target, ref, created_ns) VALUES (?,?,?,?) ON CONFLICT (action_key, target) DO NOTHING",
		d.ActionKey, d.Target, d.Ref, d.CreatedAt.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := s.rowsAffected(res)
	return n > 0, err
}

// GetDispatch implements core.DispatchLedger.
func (s *Store) GetDispatch(ctx context.Context, actionKey, target string) (*core.DispatchRecord, error) {
	d := &core.DispatchRecord{ActionKey: actionKey, Target: target}
	var ns int64
	err := s.conn(ctx).QueryRowContext(ctx, s.rebind("SELECT ref, created_ns FROM dispatches WHERE action_key=? AND target=?"),
		actionKey, target).Scan(&d.Ref, &ns)
	if err != nil {
		return nil, s.mapErr(err)
	}
	d.CreatedAt = time.Unix(0, ns).UTC()
	return d, nil
}

// PurgeDispatches implements core.DispatchLedger.
func (s *Store) PurgeDispatches(ctx context.Context, before time.Time) (int, error) {
	res, err := s.exec(ctx, s.conn(ctx), "DELETE FROM dispatches WHERE created_ns < ?", before.UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := s.rowsAffected(res)
	return int(n), err
}

// ---------- Webhook delivery retention (core.WebhookDeliveryPruner) ----------

// webhookStripBatch is how many rows one step of the strip loop reads.
const webhookStripBatch = 200

// PruneWebhookDeliveries implements core.WebhookDeliveryPruner.
func (s *Store) PruneWebhookDeliveries(ctx context.Context, keepNewest int, before time.Time) (core.WebhookPruneResult, error) {
	var out core.WebhookPruneResult
	if keepNewest < 0 {
		return out, fmt.Errorf("%w: keepNewest must not be negative", core.ErrInvalid)
	}
	q := s.conn(ctx)
	rows, err := q.QueryContext(ctx, s.rebind("SELECT DISTINCT webhook_id FROM webhook_deliveries"))
	if err != nil {
		return out, s.mapErr(err)
	}
	var hooks []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return out, s.mapErr(err)
		}
		hooks = append(hooks, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return out, s.mapErr(err)
	}
	_ = rows.Close()

	for _, hook := range hooks {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		stripped, deleted, err := s.pruneWebhook(ctx, q, hook, keepNewest, before)
		out.Stripped += stripped
		out.Deleted += deleted
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// pruneWebhook applies the retention to one webhook. The rows after the first
// keepNewest (newest first) are "old": they are deleted when received before
// `before`, and otherwise they lose body and headers.
func (s *Store) pruneWebhook(ctx context.Context, q querier, hook string, keepNewest int, before time.Time) (stripped, deleted int, err error) {
	var bns int64
	var bid string
	err = q.QueryRowContext(ctx, s.rebind("SELECT received_ns, id FROM webhook_deliveries WHERE webhook_id=? ORDER BY received_ns DESC, id DESC LIMIT 1 OFFSET ?"),
		hook, keepNewest).Scan(&bns, &bid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil // not more than keepNewest rows
	}
	if err != nil {
		return 0, 0, s.mapErr(err)
	}
	// The boundary row is the first old row. A row is old when it is not
	// newer than the boundary in the order (received_ns, id).
	const old = "(received_ns < ? OR (received_ns = ? AND id <= ?))"
	res, err := s.exec(ctx, q, "DELETE FROM webhook_deliveries WHERE webhook_id=? AND received_ns < ? AND "+old,
		hook, before.UnixNano(), bns, bns, bid)
	if err != nil {
		return 0, 0, err
	}
	n, err := s.rowsAffected(res)
	if err != nil {
		return 0, 0, err
	}
	deleted = int(n)

	for {
		type item struct {
			id   string
			data []byte
		}
		rows, err := q.QueryContext(ctx, s.rebind(fmt.Sprintf(
			"SELECT id, data FROM webhook_deliveries WHERE webhook_id=? AND stripped=0 AND %s ORDER BY received_ns, id LIMIT %d", old, webhookStripBatch)),
			hook, bns, bns, bid)
		if err != nil {
			return stripped, deleted, s.mapErr(err)
		}
		var batch []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.data); err != nil {
				_ = rows.Close()
				return stripped, deleted, s.mapErr(err)
			}
			batch = append(batch, it)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return stripped, deleted, s.mapErr(err)
		}
		_ = rows.Close()
		for _, it := range batch {
			var d core.WebhookDelivery
			if err := json.Unmarshal(it.data, &d); err != nil {
				return stripped, deleted, fmt.Errorf("%s: decode webhook delivery %s: %w", s.d.Name, it.id, err)
			}
			d.Body, d.Headers = nil, nil
			data, err := marshal(&d)
			if err != nil {
				return stripped, deleted, err
			}
			r := (&row{}).set("id", it.id).set("data", data).set("stripped", 1)
			if err := s.update(ctx, q, "webhook_deliveries", r); err != nil && !errors.Is(err, core.ErrNotFound) {
				return stripped, deleted, err
			}
			stripped++
		}
		if len(batch) < webhookStripBatch {
			return stripped, deleted, nil
		}
	}
}

// ---------- Atomic account link (core.IdentityLinker) ----------

// LinkExternalIdentity implements core.IdentityLinker with one statement: an
// insert that, on a conflict, updates the row only when it has no user or has
// the same user. A row of another user is never changed.
func (s *Store) LinkExternalIdentity(ctx context.Context, x *core.ExternalIdentity) (*core.ExternalIdentity, bool, error) {
	if x.Provider == "" || x.ExternalID == "" || x.UserID == "" {
		return nil, false, fmt.Errorf("%w: provider, external id and user are required", core.ErrInvalid)
	}
	if x.Handle == "" {
		// A link of the same user is kept as it is.
		cur, err := s.FindExternalIdentity(ctx, x.Provider, x.ExternalID)
		if err == nil && cur.UserID == x.UserID {
			return cur, true, nil
		}
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return nil, false, err
		}
	}
	data, err := marshal(x)
	if err != nil {
		return nil, false, err
	}
	r := s.prep((&row{}).set("provider", x.Provider).set("external_id", x.ExternalID).set("user_id", x.UserID).set("data", data))
	res, err := s.exec(ctx, s.conn(ctx),
		"INSERT INTO external_identities (provider, external_id, user_id, data) VALUES (?,?,?,?)"+
			" ON CONFLICT (provider, external_id) DO UPDATE SET user_id=excluded.user_id, data=excluded.data"+
			" WHERE external_identities.user_id = '' OR external_identities.user_id = excluded.user_id",
		r.args...)
	if err != nil {
		return nil, false, err
	}
	n, err := s.rowsAffected(res)
	if err != nil {
		return nil, false, err
	}
	if n > 0 {
		cp := *x
		return &cp, true, nil
	}
	cur, err := s.FindExternalIdentity(ctx, x.Provider, x.ExternalID)
	if err != nil {
		return nil, false, err
	}
	return cur, cur.UserID == x.UserID, nil
}
