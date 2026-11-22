// Webhook intake helpers: body limits, signatures, and the rule that picks the
// integration record of a delivery. Port of internal/forge/forgeutil/{webhook,select}.go.

import {
  OfError,
  intakeVerified as intakeRecord,
  ownerRule,
  readTextCapped,
  selectRecord,
  type RecordRule,
} from "@openfactory/core";
import type { HostApi, IntegrationRecord } from "./types";

export {
  constantTimeEqual,
  hmacSha256,
  hmacSha256Hex,
  repoOwner,
  sha256Hex,
  timingSafeEqual,
  toHex,
  verifyHmacSha256Hex,
} from "@openfactory/core";

/** At most this many bytes of a webhook body are read. */
export const MAX_WEBHOOK_BODY = 10 << 20;

export class BodyTooLargeError extends Error {
  override readonly name = "BodyTooLargeError";
}

/**
 * Reads a webhook body as text. It stops reading at max bytes and throws
 * BodyTooLargeError when the body is longer, so a large or endless body is
 * never buffered in full.
 */
export async function readBody(req: Request, max = MAX_WEBHOOK_BODY): Promise<string> {
  try {
    return await readTextCapped(req, max, "request body");
  } catch (e) {
    if (e instanceof OfError && e.code === "invalid_request") throw new BodyTooLargeError("request body too large");
    throw e;
  }
}

/** A JSON response for a webhook sender. */
export function jsonResult(status: number, value: unknown, headers: Record<string, string> = {}) {
  return { status, headers: { "content-type": "application/json", ...headers }, body: JSON.stringify(value) };
}

export function errorResult(status: number, code: string, message: string, headers: Record<string, string> = {}) {
  return jsonResult(status, { error: { code, message } }, headers);
}

// ---------- Integration selection ----------

/** The reason of an invalid_request OfError: no record (or several) can serve the repository. */
export const REASON_NO_INTEGRATION = "no_integration";

function noIntegration(msg: string): OfError {
  return new OfError("invalid_request", msg, { reason: REASON_NO_INTEGRATION });
}

export function isNoIntegration(e: unknown): boolean {
  return e instanceof OfError && e.code === "invalid_request" && e.reason === REASON_NO_INTEGRATION;
}

/** No integration record verifies the delivery. */
export class DeliveryUnverifiedError extends Error {
  override readonly name = "DeliveryUnverifiedError";
}

/** Records of several teams verify the delivery. */
export class DeliveryAmbiguousError extends Error {
  override readonly name = "DeliveryAmbiguousError";
}

const rule = (provider: string): RecordRule<IntegrationRecord> =>
  ownerRule(
    provider,
    { id: (r) => r.id, team: (r) => r.teamId, factory: (r) => r.factoryId ?? "", org: (r) => r.config["org"] ?? "" },
    noIntegration,
  );

/**
 * Picks the record for a repository. A record of the factory wins over a team
 * record. Several records in the first non-empty tier are an error.
 */
export function selectIntegration(
  ints: IntegrationRecord[],
  provider: string,
  factoryId: string,
  owner: string,
): IntegrationRecord {
  return selectRecord(ints, rule(provider), factoryId, owner);
}

/**
 * Picks the team of a webhook delivery: the record whose secret verifies the
 * delivery, whatever the order of the teams. `all` is every record of the
 * provider (all teams).
 */
export function intakeVerified(
  all: IntegrationRecord[],
  provider: string,
  owners: string[],
  verify: (r: IntegrationRecord) => Promise<boolean>,
): Promise<IntegrationRecord> {
  const recs = all.filter((r) => r.provider.toLowerCase() === provider.toLowerCase());
  return intakeRecord(recs, rule(provider), provider, owners, verify, {
    ambiguous: (msg) => new DeliveryAmbiguousError(msg),
    unverified: (msg) => new DeliveryUnverifiedError(msg),
  });
}

/**
 * The secret scope that names the team of an integration record. The host
 * resolves "team:<teamId>" to the team-level secret; a record of a factory
 * uses the factory ID. HostApi.getSecret has no team argument, so webhooks
 * (which have no call team) carry the team in the scope.
 */
export function secretScope(rec: IntegrationRecord): string {
  return `team:${rec.teamId}`;
}

/** Reads the secret that config[configKey] of a record names. Throws resource_not_found without one. */
export async function integrationSecret(host: HostApi, rec: IntegrationRecord, configKey: string): Promise<string> {
  const name = configKey !== "" ? (rec.config[configKey] ?? "") : (rec.secretRef ?? "");
  if (name === "") {
    throw new OfError("resource_not_found", `integration ${rec.id} (${rec.provider}): no secret configured`);
  }
  return host.getSecret(secretScope(rec), name);
}
