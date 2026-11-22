// REST handlers: webhooks. C0 stubs that answer 501. Owner: WS10.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** GET /api/v1/factories/{uid}/webhooks: List webhooks (owner DO: TeamDO). */
export const listWebhooks: Handler = notImplementedHandler;

/** POST /api/v1/factories/{uid}/webhooks: Create a webhook (managed factories) (owner DO: TeamDO). */
export const createWebhook: Handler = notImplementedHandler;

/** GET /api/v1/webhooks/{id}: Get a webhook (owner DO: TeamDO). */
export const getWebhook: Handler = notImplementedHandler;

/** PATCH /api/v1/webhooks/{id}: Update a webhook (enabled, delivery ID header, provider secret) (owner DO: TeamDO). */
export const updateWebhook: Handler = notImplementedHandler;

/** DELETE /api/v1/webhooks/{id}: Delete a webhook (owner DO: TeamDO). */
export const deleteWebhook: Handler = notImplementedHandler;

/** POST /api/v1/webhooks/{id}/rotate-secret: Rotate a webhook's secret (owner DO: TeamDO). */
export const rotateWebhookSecret: Handler = notImplementedHandler;

/** GET /api/v1/webhooks/{id}/deliveries: Recent deliveries (newest first) (owner DO: IngressDO (partition = webhook)). */
export const listDeliveries: Handler = notImplementedHandler;

/** GET /api/v1/webhooks/{id}/deliveries/{deliveryId}: Get a delivery with its payload (owner DO: IngressDO (partition = webhook)). */
export const getDelivery: Handler = notImplementedHandler;

/** POST /api/v1/webhooks/{id}/test-filter: Evaluate a trigger filter against a stored delivery (owner DO: Worker (pure) + IngressDO read of the delivery). */
export const testWebhookFilter: Handler = notImplementedHandler;
