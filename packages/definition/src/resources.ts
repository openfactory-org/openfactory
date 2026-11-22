// Machine and webhook parsers. Port of internal/definition/resources.go.
// Machines are Linux only: platform.os macos and windows are rejected.

import { WEBHOOK_AUTH_MODES, WEBHOOK_SIGNATURE_SCHEMES } from "@openfactory/core";
import { secretName } from "./common.ts";
import { goQuote } from "./gofmt.ts";
import type { CompiledWebhook, MachineSpec } from "./types.ts";
import { type Diag, Doc, isNull, resolve, type YNode } from "./yamlnode.ts";

function isPowerOfTwo(n: number): boolean {
  if (!Number.isInteger(n) || n <= 0) return false;
  const b = BigInt(n);
  return (b & (b - 1n)) === 0n;
}

export function parseMachine(d: Diag, file: string, name: string, content: string): [MachineSpec, boolean] {
  const c = new Doc(d, file);
  const before = d.errs.length;
  const spec: MachineSpec = { name, os: "", arch: "" };
  const root = c.parse(content);
  if (root === null) return [spec, false];
  const m = c.mapping(root, "");
  if (m === null) return [spec, false];
  let pmFinish: (() => void) | null = null;
  try {
    let v = m.get("description");
    if (v !== null && !isNull(v)) spec.description = c.str(v, "description")[0];
    v = m.get("setupCommands");
    if (v !== null && !isNull(v)) {
      const [list] = c.strList(v, "setupCommands");
      if (list !== null) spec.setupCommands = list;
    }
    v = m.get("failureSessionRetentionMinutes");
    if (v !== null && !isNull(v))
      spec.failureSessionRetentionMinutes = c.intRange(v, "failureSessionRetentionMinutes", 1, 60)[0];
    let shapeSet = false;
    const shapeNode = m.get("instanceShape");
    let vcpus = 0;
    let memoryGb = 0;
    if (shapeNode !== null && !isNull(shapeNode)) {
      const sm = c.mapping(shapeNode, "instanceShape");
      if (sm !== null) {
        if (!sm.present("vcpus") || !sm.present("memoryGb"))
          c.errf(sm.node, "instanceShape", "instanceShape must set both vcpus and memoryGb");
        let sv = sm.get("vcpus");
        if (sv !== null && !isNull(sv)) vcpus = c.integer(sv, "instanceShape.vcpus")[0];
        sv = sm.get("memoryGb");
        if (sv !== null && !isNull(sv)) memoryGb = c.integer(sv, "instanceShape.memoryGb")[0];
        sm.finish();
        spec.vcpus = vcpus;
        spec.memoryGb = memoryGb;
        shapeSet = vcpus !== 0 || memoryGb !== 0;
      }
    }

    const pv = m.require("platform");
    if (pv === null) return [spec, false];
    const pm = c.mapping(pv, "platform");
    if (pm === null) return [spec, false];
    pmFinish = () => pm.finish();
    spec.os = "linux";
    spec.arch = "x86_64";
    v = pm.get("os");
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.str(v, "platform.os");
      if (ok && s !== "linux") {
        if (s === "macos" || s === "windows") {
          c.errf(v, "platform.os", `platform.os ${s} is not supported; machines run linux only`);
        } else {
          c.errf(v, "platform.os", `platform.os must be one of linux (got ${goQuote(s)})`);
        }
      }
    }
    v = pm.get("arch");
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.enum(v, "platform.arch", "x86_64", "aarch64");
      if (ok) spec.arch = s;
    }
    const linux = pm.get("linux");
    if (linux === null || isNull(linux)) {
      c.errf(pm.node, "platform.linux.image", "Linux machines require platform.linux.image");
    } else {
      const lm = c.mapping(linux, "platform.linux");
      if (lm !== null) {
        const iv = lm.require("image");
        if (iv !== null) {
          const [s, ok] = c.nonEmptyStr(iv, "platform.linux.image");
          if (ok) {
            if (/[ \t]/.test(s))
              c.errf(iv, "platform.linux.image", `platform.linux.image ${goQuote(s)} is not a valid image reference`);
            spec.image = s;
          }
        }
        const rv = lm.get("registryCredentialSecretName");
        if (rv !== null && !isNull(rv))
          spec.registryCredentialSecretName = secretName(c, rv, "platform.linux.registryCredentialSecretName")[0];
        const uv = lm.get("user");
        if (uv !== null && !isNull(uv)) spec.user = linuxUser(c, uv);
        lm.finish();
      }
    }
    if (shapeSet && (!isPowerOfTwo(vcpus) || !isPowerOfTwo(memoryGb))) {
      c.errf(
        shapeNode,
        "instanceShape",
        `${spec.os} instanceShape vcpus and memoryGb must both be positive powers of two (got ${vcpus}/${memoryGb})`,
      );
    }
    return [spec, d.errs.length === before];
  } finally {
    pmFinish?.();
    m.finish();
  }
}

/** SIGNATURE_SCHEMES are the provider signature schemes a webhook can verify. */
export const SIGNATURE_SCHEMES = WEBHOOK_SIGNATURE_SCHEMES;

const credentialHeaders = new Set([
  "authorization",
  "proxy-authorization",
  "cookie",
  "set-cookie",
  "x-api-key",
  "x-hub-signature",
  "x-hub-signature-256",
  "x-pagerduty-signature",
  "sentry-hook-signature",
  "stripe-signature",
  "x-vercel-signature",
  "webhook-signature",
  "svix-signature",
  "x-gitlab-token",
  "x-slack-signature",
]);

/** MAX_WEBHOOKS is the maximum number of webhooks per factory. */
export const MAX_WEBHOOKS = 20;

function isTokenChar(c: string): boolean {
  return /^[A-Za-z0-9]$/.test(c) || "!#$%&'*+-.^_`|~".includes(c);
}

/** validDeliveryIdHeader reports whether h may be a webhook delivery ID header, and why not. */
export function validDeliveryIdHeader(h: string): { ok: boolean; why: string } {
  const bytes = new TextEncoder().encode(h);
  if (h === "" || bytes.length > 64) return { ok: false, why: "must be 1-64 characters" };
  for (const b of bytes) {
    if (b >= 0x80 || !isTokenChar(String.fromCharCode(b))) return { ok: false, why: "is not a valid HTTP header name" };
  }
  if (credentialHeaders.has(h.toLowerCase()))
    return { ok: false, why: "is a credential-bearing header and cannot identify deliveries" };
  return { ok: true, why: "" };
}

/** validURLTokenSecret reports whether a secret value can be embedded in a url_token webhook URL. */
export function validURLTokenSecret(v: string): boolean {
  return v !== "" && /^[A-Za-z0-9._~-]+$/.test(v);
}

/** canonicalMIMEHeaderKey ports textproto.CanonicalMIMEHeaderKey for token strings. */
export function canonicalMIMEHeaderKey(s: string): string {
  let upper = true;
  let out = "";
  for (const c of s) {
    if (!isTokenChar(c)) return s;
    let ch = c;
    if (upper && c >= "a" && c <= "z") ch = c.toUpperCase();
    else if (!upper && c >= "A" && c <= "Z") ch = c.toLowerCase();
    out += ch;
    upper = c === "-";
  }
  return out;
}

export function parseWebhook(d: Diag, file: string, name: string, content: string): [CompiledWebhook, boolean] {
  const c = new Doc(d, file);
  const before = d.errs.length;
  const w: CompiledWebhook = { id: "", factory_id: "", name, auth_mode: "token", secret_name: "", enabled: true };
  const root = c.parse(content);
  if (root === null) return [w, false];
  const m = c.mapping(root, "");
  if (m === null) return [w, false];
  try {
    let v = m.get("authMode");
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.enum(v, "authMode", ...WEBHOOK_AUTH_MODES);
      if (ok) w.auth_mode = s;
    }
    const sv = m.get("signatureScheme");
    if (w.auth_mode === "signature" && (sv === null || isNull(sv))) {
      c.errf(m.node, "signatureScheme", "signatureScheme is required when authMode is signature");
    } else if (w.auth_mode !== "signature" && sv !== null && !isNull(sv)) {
      c.errf(sv, "signatureScheme", "signatureScheme is only valid when authMode is signature");
    } else if (sv !== null && !isNull(sv)) {
      const [s, ok] = c.enum(sv, "signatureScheme", ...SIGNATURE_SCHEMES);
      if (ok) w.signature_scheme = s;
    }
    v = m.require("secretName");
    if (v !== null) w.secret_name = secretName(c, v, "secretName")[0];
    v = m.get("deliveryIdHeader");
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.str(v, "deliveryIdHeader");
      if (ok) {
        const r = validDeliveryIdHeader(s);
        if (!r.ok) c.errf(v, "deliveryIdHeader", `deliveryIdHeader ${goQuote(s)} ${r.why}`);
        else w.delivery_id_header = canonicalMIMEHeaderKey(s);
      }
    }
    v = m.get("enabled");
    if (v !== null && !isNull(v)) {
      const [b, ok] = c.boolean(v, "enabled");
      if (ok) w.enabled = b;
    }
    return [w, d.errs.length === before];
  } finally {
    m.finish();
  }
}

/** linuxUser reads platform.linux.user: a number of 1 or more, or "image". Root (0) is refused. */
function linuxUser(c: Doc, v: YNode): string {
  const path = "platform.linux.user";
  if (v.kind === "scalar" && v.value === "image") return "image";
  const r = resolve(v);
  if (r === null || r.kind !== "scalar" || r.tag !== "!!int") {
    c.errf(v, path, `${path} must be a numeric user ID of 1 or more, or image`);
    return "";
  }
  const [n, ok] = c.integer(v, path);
  if (!ok) return "";
  if (n < 1) {
    c.errf(v, path, `${path} must be a numeric user ID of 1 or more, or image (got ${n})`);
    return "";
  }
  return String(n);
}
