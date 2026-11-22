// Provider signature schemes of custom webhooks. Port of internal/webhooks/signature.go.
import { b64, fromHex, timingSafeEqual, toHex, utf8 } from "../lib/bytes";

export const SCHEME_GITHUB = "github";
export const SCHEME_PAGERDUTY = "pagerduty";
export const SCHEME_SENTRY = "sentry";
export const SCHEME_STANDARD_WEBHOOKS = "standard_webhooks";
export const SCHEME_STRIPE = "stripe";
export const SCHEME_VERCEL = "vercel";

/** Maximum age (and skew) of timestamped signatures, ms. */
export const DEFAULT_SIGNATURE_TOLERANCE_MS = 5 * 60_000;

export class SignatureError extends Error {
  override readonly name = "SignatureError";
  constructor(msg: string) {
    super("signature verification failed: " + msg);
  }
}

const sigErr = (m: string) => new SignatureError(m);

type Hash = "SHA-256" | "SHA-1";

async function hmac(hash: Hash, key: Uint8Array, msg: Uint8Array | string): Promise<Uint8Array> {
  const k = await crypto.subtle.importKey("raw", key as Uint8Array<ArrayBuffer>, { name: "HMAC", hash }, false, ["sign"]);
  const m = typeof msg === "string" ? utf8(msg) : msg;
  return new Uint8Array(await crypto.subtle.sign("HMAC", k, m as Uint8Array<ArrayBuffer>));
}

function equalHex(sigHex: string, want: Uint8Array): boolean {
  let got: Uint8Array;
  try {
    got = fromHex(sigHex.trim().toLowerCase());
  } catch {
    return false;
  }
  return timingSafeEqual(got, want);
}

const cat = (a: string, body: Uint8Array): Uint8Array => {
  const p = utf8(a);
  const out = new Uint8Array(p.length + body.length);
  out.set(p);
  out.set(body, p.length);
  return out;
};

interface Scheme {
  signatureHeaders: string[];
  deliveryHeaders: string[];
  verify(secret: Uint8Array, h: Headers, body: Uint8Array, nowMs: number, tolMs: number): Promise<void>;
  sign(secret: Uint8Array, body: Uint8Array, nowMs: number, id: string): Promise<Headers>;
}

function checkTimestamp(ts: string, nowMs: number, tolMs: number): void {
  if (!/^[+-]?\d+$/.test(ts)) throw sigErr(`invalid timestamp "${ts}"`);
  const d = nowMs - Number(ts) * 1000;
  const tol = formatDuration(tolMs);
  if (d > tolMs) throw sigErr(`timestamp is older than ${tol}`);
  if (d < -tolMs) throw sigErr(`timestamp is more than ${tol} in the future`);
}

/** Go Duration.String for whole seconds. */
function formatDuration(ms: number): string {
  const s = Math.round(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  if (h > 0) return `${h}h${m}m${sec}s`;
  if (m > 0) return `${m}m${sec}s`;
  return `${sec}s`;
}

/** Decodes a "whsec_<base64>" key; a secret that is not base64 is used raw. */
export function standardWebhooksKey(secret: Uint8Array): Uint8Array {
  let s = new TextDecoder().decode(secret).trim();
  if (s.startsWith("whsec_")) s = s.slice(6);
  const tries: [RegExp, boolean, boolean][] = [
    [/^[A-Za-z0-9+/]*={0,2}$/, true, false],
    [/^[A-Za-z0-9+/]*$/, false, false],
    [/^[A-Za-z0-9_-]*={0,2}$/, true, true],
    [/^[A-Za-z0-9_-]*$/, false, true],
  ];
  for (const [re, padded, url] of tries) {
    if (!re.test(s)) continue;
    if (padded && s.length % 4 !== 0) continue;
    if (!padded && (s.length % 4 === 1)) continue;
    let t = url ? s.replace(/-/g, "+").replace(/_/g, "/") : s;
    if (!padded) t += "=".repeat((4 - (t.length % 4)) % 4);
    try {
      const raw = atob(t);
      if (raw.length === 0) continue;
      return Uint8Array.from(raw, (c) => c.charCodeAt(0));
    } catch {
      /* next */
    }
  }
  return utf8(s);
}

const SCHEMES: Record<string, Scheme> = {
  [SCHEME_GITHUB]: {
    signatureHeaders: ["X-Hub-Signature-256", "X-Hub-Signature"],
    deliveryHeaders: ["X-GitHub-Delivery"],
    async verify(secret, h, body) {
      const v = (h.get("X-Hub-Signature-256") ?? "").trim();
      if (v === "") throw sigErr("missing X-Hub-Signature-256 header");
      if (!v.startsWith("sha256=")) throw sigErr("X-Hub-Signature-256 must start with sha256=");
      if (!equalHex(v.slice(7), await hmac("SHA-256", secret, body))) throw sigErr("signature mismatch");
    },
    async sign(secret, body, _now, id) {
      const h = new Headers({ "X-Hub-Signature-256": "sha256=" + toHex(await hmac("SHA-256", secret, body)) });
      if (id) h.set("X-GitHub-Delivery", id);
      return h;
    },
  },
  [SCHEME_PAGERDUTY]: {
    signatureHeaders: ["X-PagerDuty-Signature"],
    deliveryHeaders: ["X-Webhook-Id"],
    async verify(secret, h, body) {
      const v = (h.get("X-PagerDuty-Signature") ?? "").trim();
      if (v === "") throw sigErr("missing X-PagerDuty-Signature header");
      const want = await hmac("SHA-256", secret, body);
      let found = false;
      for (const part of v.split(",")) {
        const p = part.trim();
        if (!p.startsWith("v1=")) continue;
        found = true;
        if (equalHex(p.slice(3), want)) return;
      }
      if (!found) throw sigErr("X-PagerDuty-Signature has no v1 signature");
      throw sigErr("signature mismatch");
    },
    async sign(secret, body, _now, id) {
      const h = new Headers({ "X-PagerDuty-Signature": "v1=" + toHex(await hmac("SHA-256", secret, body)) });
      if (id) h.set("X-Webhook-Id", id);
      return h;
    },
  },
  [SCHEME_SENTRY]: {
    signatureHeaders: ["Sentry-Hook-Signature"],
    deliveryHeaders: ["Request-ID"],
    async verify(secret, h, body) {
      const v = (h.get("Sentry-Hook-Signature") ?? "").trim();
      if (v === "") throw sigErr("missing Sentry-Hook-Signature header");
      if (!equalHex(v, await hmac("SHA-256", secret, body))) throw sigErr("signature mismatch");
    },
    async sign(secret, body, _now, id) {
      const h = new Headers({ "Sentry-Hook-Signature": toHex(await hmac("SHA-256", secret, body)) });
      if (id) h.set("Request-ID", id);
      return h;
    },
  },
  [SCHEME_STANDARD_WEBHOOKS]: {
    signatureHeaders: ["Webhook-Signature", "Svix-Signature"],
    deliveryHeaders: ["Webhook-Id", "Svix-Id"],
    async verify(secret, h, body, now, tol) {
      const get = (n: string) => (h.get("Webhook-" + n) ?? "").trim() || (h.get("Svix-" + n) ?? "").trim();
      const id = get("Id");
      const ts = get("Timestamp");
      const sigs = get("Signature");
      if (!id || !ts || !sigs) throw sigErr("missing webhook-id, webhook-timestamp or webhook-signature header");
      checkTimestamp(ts, now, tol);
      const want = await hmac("SHA-256", standardWebhooksKey(secret), cat(`${id}.${ts}.`, body));
      let found = false;
      for (const part of sigs.split(/\s+/).filter((x) => x)) {
        if (!part.startsWith("v1,")) continue;
        found = true;
        let got: Uint8Array;
        try {
          got = Uint8Array.from(atob(part.slice(3)), (c) => c.charCodeAt(0));
        } catch {
          continue;
        }
        if (timingSafeEqual(got, want)) return;
      }
      if (!found) throw sigErr("webhook-signature has no v1 signature");
      throw sigErr("signature mismatch");
    },
    async sign(secret, body, now, id0) {
      const id = id0 || "msg_" + (BigInt(now) * 1_000_000n).toString(36);
      const ts = String(Math.floor(now / 1000));
      const sig = await hmac("SHA-256", standardWebhooksKey(secret), cat(`${id}.${ts}.`, body));
      return new Headers({ "Webhook-Id": id, "Webhook-Timestamp": ts, "Webhook-Signature": "v1," + b64(sig) });
    },
  },
  [SCHEME_STRIPE]: {
    signatureHeaders: ["Stripe-Signature"],
    deliveryHeaders: [],
    async verify(secret, h, body, now, tol) {
      const v = (h.get("Stripe-Signature") ?? "").trim();
      if (v === "") throw sigErr("missing Stripe-Signature header");
      let ts = "";
      const sigs: string[] = [];
      for (const part of v.split(",")) {
        const p = part.trim();
        const i = p.indexOf("=");
        if (i < 0) continue;
        const k = p.slice(0, i);
        if (k === "t") ts = p.slice(i + 1);
        else if (k === "v1") sigs.push(p.slice(i + 1));
      }
      if (ts === "") throw sigErr("Stripe-Signature has no timestamp");
      if (sigs.length === 0) throw sigErr("Stripe-Signature has no v1 signature");
      checkTimestamp(ts, now, tol);
      const want = await hmac("SHA-256", secret, cat(ts + ".", body));
      if (sigs.some((s) => equalHex(s, want))) return;
      throw sigErr("signature mismatch");
    },
    async sign(secret, body, now) {
      const ts = String(Math.floor(now / 1000));
      return new Headers({ "Stripe-Signature": `t=${ts},v1=` + toHex(await hmac("SHA-256", secret, cat(ts + ".", body))) });
    },
  },
  [SCHEME_VERCEL]: {
    signatureHeaders: ["X-Vercel-Signature"],
    deliveryHeaders: [],
    async verify(secret, h, body) {
      const v = (h.get("X-Vercel-Signature") ?? "").trim();
      if (v === "") throw sigErr("missing x-vercel-signature header");
      if (!equalHex(v, await hmac("SHA-1", secret, body))) throw sigErr("signature mismatch");
    },
    async sign(secret, body) {
      return new Headers({ "X-Vercel-Signature": toHex(await hmac("SHA-1", secret, body)) });
    },
  },
};

/** Supported scheme names, sorted. */
export function signatureSchemes(): string[] {
  return Object.keys(SCHEMES).sort();
}

export function schemeKnown(name: string): boolean {
  return Object.prototype.hasOwnProperty.call(SCHEMES, name);
}

/** Header names that carry signatures (redacted from the delivery log). */
export function signatureHeaders(): string[] {
  return Object.values(SCHEMES).flatMap((s) => s.signatureHeaders);
}

/** Delivery ID headers of a scheme (first non-empty wins). */
export function deliveryHeaders(name: string): string[] {
  return SCHEMES[name]?.deliveryHeaders ?? [];
}

/** Checks a delivery's signature. Throws SignatureError. */
export async function verify(scheme: string, secret: Uint8Array, h: Headers, body: Uint8Array, nowMs: number, tolMs = 0): Promise<void> {
  const sc = SCHEMES[scheme];
  if (!sc) throw sigErr(`unknown signature scheme "${scheme}"`);
  if (secret.length === 0) throw sigErr("no signing secret configured");
  await sc.verify(secret, h, body, nowMs, tolMs > 0 ? tolMs : DEFAULT_SIGNATURE_TOLERANCE_MS);
}

/** The headers a sender with the scheme attaches (for test deliveries). */
export async function sign(scheme: string, secret: Uint8Array, body: Uint8Array, nowMs: number, id = ""): Promise<Headers> {
  const sc = SCHEMES[scheme];
  if (!sc) throw new Error(`unknown signature scheme "${scheme}"`);
  return sc.sign(secret, body, nowMs, id);
}
