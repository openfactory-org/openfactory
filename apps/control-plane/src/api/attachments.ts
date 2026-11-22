// REST handlers: attachments. C0 stubs that answer 501. Owner: WS5.
// Keep the exported names: src/http/routes.ts maps each openapi operation to one of them.
import { notImplementedHandler, type Handler } from "../http/handler";

/** POST /api/v1/attachments: Upload an attachment for messages (owner DO: Worker → R2 + TeamDO KV (meta)). */
export const uploadAttachment: Handler = notImplementedHandler;
