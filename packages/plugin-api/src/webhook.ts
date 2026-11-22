// Webhook intake helpers: body limits, signatures, and the rule that picks the
// integration record of a delivery. Port of internal/forge/forgeutil/{webhook,select}.go.

import { OfError } from "@openfactory/core";
import type { HostApi, IntegrationRecord } from "./types";

/** At most this many bytes of a webhook body are read. */
export const MAX_WEBHOOK_BODY = 10 << 20;

export class BodyTooLargeError extends Error {
  override readonly name = "BodyTooLargeError";
}

export async function readBody(req: Request, max = MAX_WEBHOOK_BODY): Promise<string> {
  const declared = Number(req.headers.get("content-length") ?? "0");
  if (declared > max) throw new BodyTooLargeError("request body too large");
  const buf = new Uint8Array(await req.arrayBuffer());
  if (buf.byteLength > max) throw new BodyTooLargeError("request body too large");
  return new TextDecoder().decode(buf);
}

/** Compares two secrets in time that depends on their length only. Empty values never match. */
export function constantTimeEqual(a: string, b: string): boolean {
  if (a === "" || b === "") return false;
  const enc = new TextEncoder();
  const x = enc.encode(a);
  const y = enc.encode(b);
  let diff = x.length ^ y.length;
  const n = Math.max(x.length, y.length);
  for (let i = 0; i < n; i++) diff |= (x[i] ?? 0) ^ (y[i] ?? 0);
  return diff === 0;
}

function hex(buf: ArrayBuffer): string {
  return Array.from(new Uint8Array(buf), (b) => b.toString(16).padStart(2, "0")).join("");
}

export async function hmacSha256Hex(secret: string, data: string): Promise<string> {
  const enc = new TextEncoder();
  const key = await crypto.subtle.importKey("raw", enc.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
  ]);
  return hex(await crypto.subtle.sign("HMAC", key, enc.encode(data)));
}

/** Verifies "sha256=<hex>" (or bare hex). An empty secret never verifies. */
export async function verifyHmacSha256Hex(secret: string, body: string, sig: string): Promise<boolean> {
  if (secret === "") return false;
  const got = sig.trim().replace(/^sha256=/, "");
  if (!/^[0-9a-fA-F]+$/.test(got)) return false;
  return constantTimeEqual(await hmacSha256Hex(secret, body), got.toLowerCase());
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

/**
 * The owner (organization) a repository path belongs to: "acme/platform/api"
 * gives "acme" (GitLab, GitHub); Azure DevOps "org/project/repo" gives "org"
 * and "project/repo" gives "". "git" has no owner.
 */
export function repoOwner(provider: string, repo: string): string {
  const parts = repo
    .trim()
    .replace(/^\/+|\/+$/g, "")
    .split("/");
  switch (provider.toLowerCase()) {
    case "git":
      return "";
    case "azure_devops":
      return parts.length >= 3 ? (parts[0] ?? "") : "";
  }
  return parts.length < 2 ? "" : (parts[0] ?? "");
}

const sameOrg = (r: IntegrationRecord, owner: string) =>
  (r.config["org"] ?? "").trim().toLowerCase() === owner.toLowerCase();

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
  const tier = (fac: string) => ints.filter((r) => (r.factoryId ?? "") === fac && (owner === "" || sameOrg(r, owner)));
  const tiers = factoryId !== "" ? [tier(factoryId), tier("")] : [tier("")];
  for (const t of tiers) {
    if (t.length === 0) continue;
    if (t.length === 1) return t[0]!;
    const ids = t.map((r) => r.id).join(", ");
    if (owner === "") {
      throw noIntegration(
        `${provider}: ${t.length} records (${ids}) can serve this repository: write the repository with its organization, or keep one record`,
      );
    }
    throw noIntegration(`${provider}: ${t.length} records (${ids}) serve the owner "${owner}": keep one`);
  }
  if (owner === "") throw noIntegration(`no ${provider} integration record serves this repository`);
  throw noIntegration(
    `no ${provider} integration record for the owner "${owner}": add a record with config.org "${owner}"`,
  );
}

class NoOwnerRecord extends Error {}

function teamIntakeRecord(ints: IntegrationRecord[], provider: string, owner: string): IntegrationRecord {
  const mine = ints.filter((r) => owner === "" || sameOrg(r, owner));
  if (mine.length === 0) throw new NoOwnerRecord("no record for the owner");
  try {
    return selectIntegration(mine, provider, "", owner);
  } catch (e) {
    const fac = mine.filter((r) => (r.factoryId ?? "") !== "");
    if (fac.length === 1) return fac[0]!;
    throw e;
  }
}

/**
 * Picks the team of a webhook delivery: the record whose secret verifies the
 * delivery, whatever the order of the teams. `all` is every record of the
 * provider (all teams).
 */
export async function intakeVerified(
  all: IntegrationRecord[],
  provider: string,
  owners: string[],
  verify: (r: IntegrationRecord) => Promise<boolean>,
): Promise<IntegrationRecord> {
  const teams: string[] = [];
  const byTeam = new Map<string, IntegrationRecord[]>();
  for (const r of all) {
    if (r.provider.toLowerCase() !== provider.toLowerCase()) continue;
    if (!byTeam.has(r.teamId)) {
      teams.push(r.teamId);
      byTeam.set(r.teamId, []);
    }
    byTeam.get(r.teamId)!.push(r);
  }
  const cands: IntegrationRecord[] = [];
  const skipped: string[] = [];
  const seen = new Set<string>();
  for (const owner of owners) {
    for (const team of teams) {
      try {
        const r = teamIntakeRecord(byTeam.get(team) ?? [], provider, owner);
        if (!seen.has(r.id)) {
          seen.add(r.id);
          cands.push(r);
        }
      } catch (e) {
        if (!(e instanceof NoOwnerRecord)) skipped.push(`team ${team}: ${(e as Error).message}`);
      }
    }
  }
  const ok: IntegrationRecord[] = [];
  for (const r of cands) if (await verify(r)) ok.push(r);
  if (ok.length === 1) return ok[0]!;
  if (ok.length > 1) {
    const ids = ok.map((r) => `${r.id} (team ${r.teamId})`).join(", ");
    throw new DeliveryAmbiguousError(
      `${provider}: the records ${ids} verify the delivery: give each team its own webhook secret`,
    );
  }
  const names = owners.join(", ");
  if (cands.length === 0 && skipped.length === 0) {
    throw new DeliveryUnverifiedError(`no ${provider} integration record for the owner "${names}"`);
  }
  let msg = `${provider}: ${cands.length} records can serve the owner "${names}" and none verifies the delivery`;
  if (skipped.length > 0) msg += ": " + skipped.join("; ");
  throw new DeliveryUnverifiedError(msg);
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
