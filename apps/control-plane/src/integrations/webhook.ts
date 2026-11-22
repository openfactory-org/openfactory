// The shared steps of an inbound integration webhook: read the body, parse it, select the integration
// record, verify the request, then let the handler answer.
import type { Logger, WebhookOutcome } from "./types";
import type { IntakeResult } from "./select";
import { type Dedupe, errMessage, errorResponse, jsonResponse, readBody } from "./util";

export interface WebhookPipeline<P> {
  /** Parses the body. A throw gives 400 invalid_payload with invalidPayload as the message. */
  parse(body: string): P;
  invalidPayload: string;
  /** Selects the record of the delivery. Without it, no record is selected. */
  select?(payload: P, req: Request): Promise<IntakeResult>;
  /** Checks the request; false gives 401 with the unauthorized code and message. */
  verify(req: Request, body: string, payload: P, found: IntakeResult): Promise<boolean>;
  unauthorized: { code: string; message: string };
  /** Answers a verified request. */
  accept(payload: P, found: IntakeResult, body: string): WebhookOutcome | Promise<WebhookOutcome>;
}

const NONE: IntakeResult = { integration: null, error: null };

export async function handleIntegrationWebhook<P>(req: Request, p: WebhookPipeline<P>): Promise<WebhookOutcome> {
  let body: string;
  try {
    body = await readBody(req);
  } catch (e) {
    return { response: errorResponse(413, "payload_too_large", errMessage(e)) };
  }
  let payload: P;
  try {
    payload = p.parse(body);
  } catch {
    return { response: errorResponse(400, "invalid_payload", p.invalidPayload) };
  }
  const found = p.select ? await p.select(payload, req) : NONE;
  if (!(await p.verify(req, body, payload, found))) {
    return { response: errorResponse(401, p.unauthorized.code, p.unauthorized.message) };
  }
  return p.accept(payload, found, body);
}

/**
 * The answer of a tracker webhook (Jira, Linear) after verification: 202 "ignored" without a usable
 * record, 200 "duplicate" for a delivery ID seen before, else 202 "accepted" with the background work.
 */
export function acceptDelivery(
  found: IntakeResult,
  opts: {
    log: Logger;
    logFields: Record<string, unknown>;
    dedupe: Dedupe;
    delivery: string;
    background: (integration: NonNullable<IntakeResult["integration"]>) => Promise<void>;
  },
): WebhookOutcome {
  if (found.error || !found.integration) {
    opts.log.info("delivery ignored", { ...opts.logFields, err: found.error?.message });
    return { response: jsonResponse(202, { status: "ignored", reason: "no matching integration" }) };
  }
  if (opts.dedupe.isSeen(opts.delivery)) return { response: jsonResponse(200, { status: "duplicate" }) };
  const in_ = found.integration;
  return { response: jsonResponse(202, { status: "accepted" }), background: () => opts.background(in_) };
}
