// Webhook helpers: body limits and JSON error bodies. Port of
// internal/forge/forgeutil/webhook.go. The signature helpers live in
// @openfactory/core. Dedupe moved to IngressDO (exact and durable), so the
// in-memory Dedupe of Go is not ported.
import { readBytesCapped } from "../../lib/body";

export { hmacSha256Hex, verifyHmacSha256Hex } from "../../lib/bytes";

/** Largest inbound forge webhook body. */
export const MAX_WEBHOOK_BODY = 10 << 20;

/**
 * Reads at most max bytes of a request body. It throws an invalid_request
 * OfError when the body is larger, and never buffers more than max bytes.
 */
export async function readBody(req: Request, max = MAX_WEBHOOK_BODY): Promise<Uint8Array<ArrayBuffer>> {
  return readBytesCapped(req, max, "request body");
}

/** The standard JSON error envelope {"error":{"code","message"}}. */
export function errorJSON(status: number, code: string, message: string, headers?: Record<string, string>): Response {
  return jsonResponse(status, { error: { code, message } }, headers);
}

export function jsonResponse(status: number, v: unknown, headers?: Record<string, string>): Response {
  return new Response(JSON.stringify(v) + "\n", {
    status,
    headers: { "content-type": "application/json", ...(headers ?? {}) },
  });
}
