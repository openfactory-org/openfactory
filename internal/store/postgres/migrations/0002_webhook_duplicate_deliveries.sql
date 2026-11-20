-- A delivery log entry for a repeated delivery has the delivery ID of the
-- first delivery. Only the first (accepted) entry is unique per webhook.
ALTER TABLE webhook_deliveries ADD COLUMN duplicate INTEGER NOT NULL DEFAULT 0;
DROP INDEX webhook_deliveries_dedupe;
CREATE UNIQUE INDEX webhook_deliveries_dedupe ON webhook_deliveries (webhook_id, delivery_id) WHERE delivery_id <> '' AND duplicate = 0;
