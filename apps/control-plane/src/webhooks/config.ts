// Custom webhook configuration rules, secrets, header redaction and the
// filter test. Port of internal/webhooks/config.go.
import { OfError, pathEscape, type Webhook, type WebhookDelivery } from "@openfactory/core";
import { b64, b64url, randomBytes } from "../lib/bytes";
import { matchPayload } from "../triggers/payload";
import { matchTrigger, validateTrigger } from "../triggers/match";
import { filterValue } from "../triggers/filter";
import { isObj } from "../triggers/payload";
import { AttrWebhookID, EventWebhookReceived, ProviderWebhook } from "../triggers/attributes";
import { SCHEME_STANDARD_WEBHOOKS, schemeKnown, signatureHeaders, signatureSchemes } from "./signature";

export const AUTH_TOKEN = "token";
export const AUTH_URL_TOKEN = "url_token";
export const AUTH_SIGNATURE = "signature";

export const STATUS_ACCEPTED = "accepted";
export const STATUS_REJECTED = "rejected";
export const STATUS_DEDUPLICATED = "deduplicated";

export const REASON_AUTH = "auth";
export const REASON_INVALID_JSON = "invalid_json";
export const REASON_TOO_LARGE = "too_large";
export const REASON_RATE_LIMITED = "rate_limited";
export const REASON_DUPLICATE = "duplicate";
export const REASON_SECRET = "secret_unavailable";

export const MAX_WEBHOOKS_PER_FACTORY = 20;
export const MAX_DELIVERY_ID_HEADER_LEN = 64;
export const DEFAULT_MAX_BODY_BYTES = 256 << 10;
export const DEFAULT_RATE_LIMIT = 60;
export const DEFAULT_AUTH_FAILURE_LIMIT = 60;
/** Generic delivery ID header a sender may set. */
export const SENDER_DELIVERY_ID_HEADER = "X-OF-Delivery-Id";

/** The URL senders post to; a url_token webhook without a token shows "{token}". */
export function ingressURL(publicURL: string, w: Pick<Webhook, "id" | "auth_mode">, urlToken = ""): string {
  const u = publicURL.replace(/\/+$/, "") + "/webhooks/w/" + pathEscape(w.id);
  if (w.auth_mode === AUTH_URL_TOKEN) return u + "/" + (urlToken === "" ? "{token}" : pathEscape(urlToken));
  return u;
}

/** Only letters, digits, "-", ".", "_" and "~". */
export function validURLToken(s: string): boolean {
  return /^[A-Za-z0-9._~-]+$/.test(s);
}

function validHeaderName(s: string): boolean {
  return /^[A-Za-z0-9!#$%&'*+\-.^_`|~]+$/.test(s);
}

const CREDENTIAL_HEADERS = new Set([
  "authorization",
  "proxy-authorization",
  "cookie",
  "set-cookie",
  "x-api-key",
  "api-key",
  ...signatureHeaders().map((h) => h.toLowerCase()),
]);

export function isCredentialHeader(name: string): boolean {
  const l = name.toLowerCase();
  if (CREDENTIAL_HEADERS.has(l)) return true;
  return ["signature", "secret", "token", "password", "api-key", "apikey", "credential", "authorization"].some((f) =>
    l.includes(f),
  );
}

/** Go http.CanonicalHeaderKey. */
export function canonicalHeaderKey(s: string): string {
  if (!validHeaderName(s)) return s;
  return s
    .toLowerCase()
    .split("-")
    .map((p) => (p ? p[0]!.toUpperCase() + p.slice(1) : p))
    .join("-");
}

/** Flattens request headers for the delivery log without credential headers. */
export function redactHeaders(h: Headers): Record<string, string> {
  const out: Record<string, string> = {};
  h.forEach((v, k) => {
    if (isCredentialHeader(k)) return;
    out[canonicalHeaderKey(k)] = v;
  });
  return out;
}

/** Checks auth mode, signature scheme, secret name and delivery ID header. Throws invalid_request. */
export function validateConfig(
  w: Pick<Webhook, "auth_mode" | "signature_scheme" | "secret_name" | "delivery_id_header">,
): void {
  const errs: string[] = [];
  const mode = w.auth_mode || AUTH_TOKEN;
  const scheme = w.signature_scheme ?? "";
  if (mode === AUTH_TOKEN || mode === AUTH_URL_TOKEN) {
    if (scheme !== "") errs.push("signatureScheme is only valid with authMode signature");
  } else if (mode === AUTH_SIGNATURE) {
    if (scheme === "") errs.push("signatureScheme is required for authMode signature");
    else if (!schemeKnown(scheme))
      errs.push(`unknown signatureScheme "${scheme}" (allowed: ${signatureSchemes().join(", ")})`);
  } else {
    errs.push(`unknown authMode "${w.auth_mode}" (allowed: token, url_token, signature)`);
  }
  if ((w.secret_name ?? "").trim() === "") errs.push("secretName is required");
  const h = w.delivery_id_header ?? "";
  if (h !== "") {
    if (h.length > MAX_DELIVERY_ID_HEADER_LEN)
      errs.push(`deliveryIdHeader must be at most ${MAX_DELIVERY_ID_HEADER_LEN} characters`);
    else if (!validHeaderName(h)) errs.push(`deliveryIdHeader "${h}" is not a valid HTTP header name`);
    else if (isCredentialHeader(h)) errs.push(`deliveryIdHeader "${h}" carries credentials and cannot be used`);
  }
  if (errs.length > 0) throw new OfError("invalid_request", errs.join("; "));
}

/**
 * A secret for a new webhook: "whsec_" + base64url(32 bytes) for token
 * modes, "whsec_" + base64(24 bytes) for Standard Webhooks. Other schemes use
 * the secret their provider issues.
 */
export function generateSecret(authMode: string, scheme: string): string {
  const b = randomBytes(32);
  if (authMode === "" || authMode === AUTH_TOKEN || authMode === AUTH_URL_TOKEN) return "whsec_" + b64url(b);
  if (authMode === AUTH_SIGNATURE) {
    if (scheme === SCHEME_STANDARD_WEBHOOKS) return "whsec_" + b64(b.subarray(0, 24));
    throw new Error(`the ${scheme} provider issues its own signing secret`);
  }
  throw new Error(`unknown auth mode "${authMode}"`);
}

export interface FilterTestResult {
  would_trigger: boolean;
  webhook_matched: boolean;
  payload_matched: boolean;
  reason?: string;
}

/**
 * Evaluates a webhook trigger filter ({"webhook_ids": [...], "payload": {...}})
 * against a stored delivery. The delivery body may be raw JSON text (exact
 * numbers). Throws for an invalid filter.
 */
export function testFilter(
  filter: unknown,
  d: Pick<WebhookDelivery, "webhook_id" | "status" | "body"> & { bodyText?: string },
): FilterTestResult {
  validateTrigger({ provider: ProviderWebhook, event: EventWebhookReceived, filter });
  const v = filterValue(filter);
  const parts: Record<string, unknown> = isObj(v) ? { ...v } : {};
  const pattern = parts["payload"];
  delete parts["payload"];
  const res: FilterTestResult = { would_trigger: false, webhook_matched: false, payload_matched: false };
  const ev = {
    provider: ProviderWebhook,
    type: EventWebhookReceived,
    actor: { provider: "webhook", external_id: "", handle: "", is_bot: false },
    attributes: { [AttrWebhookID]: d.webhook_id },
  };
  res.webhook_matched = matchTrigger(ev, { provider: ProviderWebhook, event: EventWebhookReceived, filter: parts });
  const reasons: string[] = [];
  if (!res.webhook_matched) reasons.push("the delivery came from a different webhook");
  const body = d.bodyText ?? (d.body === undefined || d.body === null ? "" : d.body);
  if (d.status !== STATUS_ACCEPTED || body === "") {
    reasons.push("the delivery was not accepted, so it has no stored payload");
  } else {
    try {
      const pm = matchPayload(pattern, body);
      if (!pm) reasons.push("the payload does not match the filter");
      res.payload_matched = pm;
    } catch (e) {
      reasons.push("the payload filter could not be evaluated: " + (e as Error).message);
    }
  }
  res.would_trigger = res.webhook_matched && res.payload_matched;
  reasons.sort();
  if (reasons.length > 0) res.reason = reasons.join("; ");
  return res;
}
