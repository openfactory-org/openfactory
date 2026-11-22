// Shared helpers of the integrations (port of forgeutil webhook.go and text.go).
import { OfError } from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import { readTextCapped } from "../lib/body";
import type { SecretReader } from "./types";

// The signature helpers live in @openfactory/core.
export {
  constantTimeEqual,
  hmacSha256,
  hmacSha256Hex,
  sha256Hex,
  timingSafeEqual,
  toHex,
  verifyHmacSha256Hex,
} from "../lib/bytes";

/** Most bytes of an inbound webhook body. */
export const MAX_WEBHOOK_BODY = 10 << 20;

/**
 * Reads the request body as text. It buffers at most MAX_WEBHOOK_BODY bytes
 * and throws an invalid_request OfError when the body is longer.
 */
export async function readBody(req: Request): Promise<string> {
  return readTextCapped(req, MAX_WEBHOOK_BODY, "request body");
}

export function jsonResponse(status: number, v: unknown): Response {
  return new Response(JSON.stringify(v), { status, headers: { "Content-Type": "application/json" } });
}

export function errorResponse(status: number, code: string, message: string): Response {
  return jsonResponse(status, { error: { code, message } });
}

/**
 * Remembers recently seen delivery IDs to drop provider retries in this
 * isolate. IngressDO dedupes published events durably by delivery_id; this
 * best-effort check also keeps a retry from repeating the replies that the
 * Slack and Teams handlers post before (or instead of) publishing.
 */
export class Dedupe {
  private readonly seen = new Map<string, number>();
  constructor(
    private readonly ttlMs: number,
    private readonly now: () => number = Date.now,
  ) {}

  /** Records id and reports whether it was recorded within the TTL. */
  isSeen(id: string): boolean {
    if (id === "") return false;
    const now = this.now();
    if (this.seen.size > 10000) {
      for (const [k, t] of this.seen) if (now - t > this.ttlMs) this.seen.delete(k);
    }
    const t = this.seen.get(id);
    if (t !== undefined && now - t <= this.ttlMs) return true;
    this.seen.set(id, now);
    return false;
  }
}

/** The secret named by in.secret_ref, or by in.config[configKey] when configKey is set. */
export async function integrationSecret(
  secrets: SecretReader | undefined,
  integration: Integration,
  configKey = "",
): Promise<string> {
  const name = configKey ? (integration.config?.[configKey] ?? "") : (integration.secret_ref ?? "");
  if (name === "") {
    throw new OfError(
      "resource_not_found",
      `integration ${integration.id} (${integration.provider}): no secret configured`,
    );
  }
  if (!secrets) throw new Error(`integration ${integration.id}: no secret store configured`);
  try {
    return await secrets.get(integration.team_id, name);
  } catch (e) {
    throw new Error(`integration ${integration.id}: secret ${name}: ${(e as Error).message}`, { cause: e });
  }
}

export function cfg(integration: Pick<Integration, "config">, key: string): string {
  return integration.config?.[key] ?? "";
}

export function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** Go strings.EqualFold (simple case folding); it lives in @openfactory/core. */
export { equalFold } from "@openfactory/core";

/** Default size cap of the per-handler lookup caches (users, channels, tokens). */
export const CACHE_MAX_ENTRIES = 1000;

/**
 * Sets a key in a cache map and drops the oldest entries (insertion order)
 * so the map never holds more than max entries.
 */
export function setCapped<K, V>(m: Map<K, V>, key: K, value: V, max = CACHE_MAX_ENTRIES): void {
  m.delete(key);
  while (m.size >= max) {
    const oldest = m.keys().next();
    if (oldest.done) break;
    m.delete(oldest.value);
  }
  m.set(key, value);
}
