// TeamDO module: custom webhook records. Migrations 0150-0199 (shared with
// schedules.ts and user-forge-tokens.ts).
//
// The records follow the active definition: definitionApply calls
// syncWebhooks inside its transaction. The REST routes edit
// webhooks/<name>.yaml of a managed definition and apply it; the secret of a
// webhook is a team secret, restored when the apply fails.
import { OfError, type Webhook } from "@openfactory/core";
import { bodyObject, optBool, optString, str } from "../../lib/body";
import { listResponse } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import {
  ingressURL,
  generateSecret,
  AUTH_SIGNATURE,
  AUTH_TOKEN,
  AUTH_URL_TOKEN,
  MAX_WEBHOOKS_PER_FACTORY,
  validURLToken,
} from "../../webhooks/config";
import { signatureSchemes } from "../../webhooks/signature";
import { created, noContent, type OpCall, type TeamKit, type TeamOp } from "./kit";
import { applyFiles as applyFactoryFiles } from "./factories/definitions";
import { activeFiles, factoryFor, getFactoryById, webhookFile, type FactoryRecord } from "./factories/repo";
import { editYamlDocument } from "./factories/yaml-edit";
import * as secrets from "./secrets";

export const migrations: readonly Migration[] = [
  {
    version: 151,
    name: "webhooks",
    sql: `
CREATE TABLE webhooks (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, name TEXT NOT NULL, secret_name TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX webhooks_factory ON webhooks (factory_id);
`,
  },
];

const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");

interface WebhookRow {
  data: string;
}

/** Replaces the webhook records of a factory (sync; inside the apply transaction). */
export function syncWebhooks(kit: TeamKit, factoryId: string, webhooks: unknown[]): void {
  const now = Date.now();
  const keep = new Set<string>();
  for (const w0 of webhooks as Webhook[]) {
    if (!w0?.id) continue;
    keep.add(w0.id);
    const old = first<{ created_ms: number; data: string }>(
      kit.sql,
      "SELECT created_ms, data FROM webhooks WHERE id = ?",
      w0.id,
    );
    const createdMs = old?.created_ms ?? now;
    const w: Webhook = {
      ...w0,
      factory_id: factoryId,
      created_at: old ? (JSON.parse(old.data) as Webhook).created_at : iso(createdMs),
    };
    kit.sql.exec(
      `INSERT INTO webhooks (id, factory_id, name, secret_name, created_ms, data) VALUES (?, ?, ?, ?, ?, ?)
       ON CONFLICT(id) DO UPDATE SET factory_id = excluded.factory_id, name = excluded.name, secret_name = excluded.secret_name, data = excluded.data`,
      w.id,
      factoryId,
      w.name,
      w.secret_name,
      createdMs,
      JSON.stringify(w),
    );
  }
  for (const r of all<{ id: string }>(kit.sql, "SELECT id FROM webhooks WHERE factory_id = ?", factoryId)) {
    if (!keep.has(r.id)) kit.sql.exec("DELETE FROM webhooks WHERE id = ?", r.id);
  }
}

export function getWebhook(kit: TeamKit, id: string): Webhook | undefined {
  const r = first<WebhookRow & Record<string, SqlStorageValue>>(kit.sql, "SELECT data FROM webhooks WHERE id = ?", id);
  return r ? (JSON.parse(r.data) as Webhook) : undefined;
}

export function listWebhooks(kit: TeamKit, factoryId: string): Webhook[] {
  return all<{ data: string }>(
    kit.sql,
    "SELECT data FROM webhooks WHERE factory_id = ? ORDER BY created_ms, id",
    factoryId,
  ).map((r) => JSON.parse(r.data) as Webhook);
}

function webhookView(kit: TeamKit, w: Webhook): Webhook & { ingress_url: string } {
  return { ...w, ingress_url: ingressURL(kit.env.OF_PUBLIC_URL ?? "", w) };
}

function webhookFor(kit: TeamKit, c: OpCall): { wh: Webhook; f: FactoryRecord } {
  const id = c.params["id"] ?? "";
  const wh = getWebhook(kit, id);
  const f = wh ? getFactoryById(kit, wh.factory_id) : undefined;
  c.authorize(f?.id ?? "");
  if (!wh || !f) throw new OfError("resource_not_found", `webhook ${id} not found`);
  return { wh, f };
}

const WEBHOOK_NAME_RE = /^[a-z0-9][a-z0-9_-]{0,62}$/;
const LATE_SECRET_SCHEMES = ["vercel", "stripe", "pagerduty"];

async function managedFiles(kit: TeamKit, f: FactoryRecord): Promise<Record<string, string>> {
  if (f.source && f.source !== "managed") {
    throw new OfError(
      "operation_not_supported",
      `this factory's definition lives in ${f.git_repo ?? "git"}: change webhooks/<name>.yaml there`,
    );
  }
  if (!f.active_version_id) throw new OfError("operation_not_supported", "factory has no applied definition yet");
  return { ...(await activeFiles(kit, f)) };
}

/** applyFactoryFiles with the error text of the Go webhook routes. */
async function applyFiles(
  kit: TeamKit,
  c: OpCall,
  f: FactoryRecord,
  files: Record<string, string>,
  message: string,
): Promise<void> {
  await applyFactoryFiles(kit, c, f, files, message, "definition could not be applied");
}

/**
 * Stores value as the team secret secretName, then runs apply. When apply
 * fails, restores the previous value (or deletes the new secret) so that the
 * stored secret keeps matching the active definition. An empty value skips the
 * secret and only runs apply.
 */
async function applyWithSecret(
  kit: TeamKit,
  secretName: string,
  value: string,
  apply: () => Promise<void>,
): Promise<void> {
  if (!value) return apply();
  const old = await secrets.getSecretValue(kit, "", secretName);
  const oldType = old !== undefined ? secrets.typeOf(kit, "", secretName) : "generic";
  await secrets.putSecret(kit, "", secretName, "generic", value);
  try {
    await apply();
  } catch (e) {
    try {
      if (old !== undefined) await secrets.putSecret(kit, "", secretName, oldType, old);
      else kit.tx(() => kit.sql.exec("DELETE FROM secrets WHERE factory_id = '' AND name = ?", secretName));
    } catch (x) {
      console.warn("restore webhook secret after a failed apply", secretName, x);
    }
    throw e;
  }
}

function newWebhookSecret(scheme: string): string {
  return scheme === "standard_webhooks" ? generateSecret(AUTH_SIGNATURE, scheme) : generateSecret("token", "");
}

function defaultSecretName(name: string): string {
  return name.replace(/[-.]/g, "_").toUpperCase() + "_WEBHOOK_SECRET";
}

function secretInUse(kit: TeamKit, name: string): boolean {
  return !!first(kit.sql, "SELECT 1 AS x FROM webhooks WHERE secret_name = ? LIMIT 1", name);
}

async function createWebhook(kit: TeamKit, c: OpCall): Promise<unknown> {
  const f = factoryFor(kit, c);
  const b = bodyObject(c.body, true);
  const req = {
    auth_mode: optString(b, "auth_mode"),
    signature_scheme: optString(b, "signature_scheme"),
    secret_name: optString(b, "secret_name"),
    secret: optString(b, "secret"),
    delivery_id_header: optString(b, "delivery_id_header"),
    enabled: optBool(b, "enabled"),
  };
  const name = str(b, "name").trim();
  if (!WEBHOOK_NAME_RE.test(name)) {
    throw new OfError(
      "invalid_request",
      "name must be 1-63 lowercase letters, digits, '-' or '_' (it becomes webhooks/<name>.yaml)",
    );
  }
  const mode = req.auth_mode || "token";
  const scheme = req.signature_scheme ?? "";
  if (mode === "token" || mode === AUTH_URL_TOKEN) {
    if (scheme) throw new OfError("invalid_request", "signature_scheme is only valid with auth_mode signature");
    if (req.secret) throw new OfError("invalid_request", `secret is generated for ${mode} webhooks; omit it`);
  } else if (mode === AUTH_SIGNATURE) {
    if (!signatureSchemes().includes(scheme))
      throw new OfError("invalid_request", `signature_scheme must be one of ${signatureSchemes().join(", ")}`);
  } else {
    throw new OfError("invalid_request", "auth_mode must be token, url_token or signature");
  }
  const files = await managedFiles(kit, f);
  if (webhookFile(name) in files) throw new OfError("conflict", `webhook "${name}" already exists`);
  if (
    Object.keys(files).filter((p) => p.startsWith("webhooks/") && p.endsWith(".yaml")).length >=
    MAX_WEBHOOKS_PER_FACTORY
  ) {
    throw new OfError("conflict", `a factory can have at most ${MAX_WEBHOOKS_PER_FACTORY} webhooks`);
  }
  const explicit = (req.secret_name ?? "").trim() !== "";
  const secretName = explicit ? req.secret_name!.trim() : defaultSecretName(name);
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(secretName))
    throw new OfError("invalid_request", `invalid secret_name "${secretName}"`);
  let enabled = req.enabled === undefined || req.enabled;
  const old = await secrets.getSecretValue(kit, "", secretName);
  const existing = old !== undefined;
  const generated = mode !== AUTH_SIGNATURE || scheme === "standard_webhooks";
  let reuse = false;
  if (existing && !req.secret) reuse = !generated ? true : explicit || secretInUse(kit, secretName);
  let store = "";
  let reveal = "";
  let awaiting = false;
  if (req.secret) store = req.secret;
  else if (reuse) {
    if (mode === AUTH_URL_TOKEN && !validURLToken(old!)) {
      throw new OfError("invalid_request", "a url_token secret accepts only letters, digits, '-', '.', '_' and '~'");
    }
  } else if (mode === AUTH_SIGNATURE && LATE_SECRET_SCHEMES.includes(scheme)) {
    store = newWebhookSecret("");
    enabled = false;
    awaiting = true;
  } else if (mode === AUTH_SIGNATURE && scheme !== "standard_webhooks") {
    throw new OfError("invalid_request", `secret (the provider's signing secret) is required for the ${scheme} scheme`);
  } else {
    store = reveal = newWebhookSecret(scheme);
  }
  const doc = editYamlDocument("", {
    authMode: mode,
    ...(scheme ? { signatureScheme: scheme } : {}),
    secretName,
    ...(req.delivery_id_header ? { deliveryIdHeader: req.delivery_id_header } : {}),
    enabled,
  });
  files[webhookFile(name)] = doc;
  await applyWithSecret(kit, secretName, store, () => applyFiles(kit, c, f, files, "Add webhook " + name));
  const wh = listWebhooks(kit, f.id).find((w) => w.name === name);
  if (!wh) throw new OfError("resource_not_found", `webhook ${name}`);
  let url = ingressURL(kit.env.OF_PUBLIC_URL ?? "", { id: wh.id, auth_mode: AUTH_TOKEN });
  if (mode === AUTH_URL_TOKEN && reveal) url += "/" + reveal;
  else if (mode === AUTH_URL_TOKEN) url += "/{token}";
  return created({
    webhook: webhookView(kit, wh),
    ingress_url: url,
    ...(reveal ? { secret: reveal } : {}),
    ...(awaiting ? { awaiting_secret: true } : {}),
    ...(reuse ? { secret_reused: true } : {}),
  });
}

async function updateWebhook(kit: TeamKit, c: OpCall): Promise<unknown> {
  const { wh, f } = webhookFor(kit, c);
  const b = bodyObject(c.body, true);
  const req = {
    name: optString(b, "name"),
    auth_mode: optString(b, "auth_mode"),
    signature_scheme: optString(b, "signature_scheme"),
    enabled: optBool(b, "enabled"),
    delivery_id_header: optString(b, "delivery_id_header"),
    secret: optString(b, "secret"),
  };
  if (req.name !== undefined && req.name !== wh.name)
    throw new OfError("operation_not_supported", "webhooks cannot be renamed; create a new webhook instead");
  if (
    (req.auth_mode !== undefined && req.auth_mode !== wh.auth_mode) ||
    (req.signature_scheme !== undefined && req.signature_scheme !== (wh.signature_scheme ?? ""))
  ) {
    throw new OfError(
      "operation_not_supported",
      "auth_mode and signature_scheme cannot change; delete and recreate the webhook",
    );
  }
  const files = await managedFiles(kit, f);
  const path = webhookFile(wh.name);
  const doc = files[path];
  if (doc === undefined) throw new OfError("resource_not_found", "webhook file " + path);
  const edits: Record<string, unknown> = {};
  let store = "";
  if (req.secret !== undefined) {
    if (wh.auth_mode !== AUTH_SIGNATURE)
      throw new OfError(
        "invalid_request",
        "secret can only be supplied for signature webhooks; use rotate-secret instead",
      );
    if (req.secret.trim() === "") throw new OfError("invalid_request", "secret cannot be empty");
    store = req.secret;
    if (req.enabled === undefined) edits["enabled"] = true;
  }
  if (req.enabled !== undefined) edits["enabled"] = req.enabled;
  if (req.delivery_id_header !== undefined)
    edits["deliveryIdHeader"] = req.delivery_id_header === "" ? null : req.delivery_id_header;
  files[path] = editYamlDocument(doc, edits);
  await applyWithSecret(kit, wh.secret_name, store, () => applyFiles(kit, c, f, files, "Update webhook " + wh.name));
  const nwh = listWebhooks(kit, f.id).find((w) => w.name === wh.name);
  if (!nwh) throw new OfError("resource_not_found", `webhook ${wh.name}`);
  return webhookView(kit, nwh);
}

async function rotateSecret(kit: TeamKit, c: OpCall): Promise<unknown> {
  const { wh, f } = webhookFor(kit, c);
  const req = { secret: optString(bodyObject(c.body, true), "secret") };
  const files = await managedFiles(kit, f);
  let reveal = "";
  let store: string;
  if (req.secret) {
    if (wh.auth_mode !== AUTH_SIGNATURE)
      throw new OfError("invalid_request", "secret can only be supplied for signature webhooks");
    store = req.secret;
  } else if (wh.auth_mode === AUTH_SIGNATURE && wh.signature_scheme !== "standard_webhooks") {
    throw new OfError("invalid_request", 'supply the provider\'s new signing secret in "secret"');
  } else {
    store = reveal = newWebhookSecret(wh.signature_scheme ?? "");
  }
  await applyWithSecret(kit, wh.secret_name, store, () =>
    applyFiles(kit, c, f, files, "Rotate webhook secret " + wh.name),
  );
  const base = ingressURL(kit.env.OF_PUBLIC_URL ?? "", { id: wh.id, auth_mode: AUTH_TOKEN });
  const resp: Record<string, unknown> = { webhook: webhookView(kit, wh), ingress_url: base };
  if (reveal) {
    resp["secret"] = reveal;
    if (wh.auth_mode === AUTH_URL_TOKEN) resp["ingress_url"] = base + "/" + reveal;
  }
  return resp;
}

// ---------- ops ----------

export const ops: Record<string, TeamOp> = {
  listWebhooks: (kit, c) => {
    const f = factoryFor(kit, c);
    return listResponse(
      listWebhooks(kit, f.id).map((w) => webhookView(kit, w)),
      "",
    );
  },
  getWebhook: (kit, c) => webhookView(kit, webhookFor(kit, c).wh),
  createWebhook,
  updateWebhook,
  rotateWebhookSecret: rotateSecret,
  deleteWebhook: async (kit, c) => {
    const { wh, f } = webhookFor(kit, c);
    const files = await managedFiles(kit, f);
    delete files[webhookFile(wh.name)];
    await applyFiles(kit, c, f, files, "Delete webhook " + wh.name);
    return noContent();
  },
  /** Internal (webhook ingress, deliveries API): {webhook, team_id} or null. No principal needed. */
  webhooksResolve: (kit, c) => {
    const id = c.query["id"] ?? c.params["id"] ?? "";
    const wh = getWebhook(kit, id);
    return wh ? { webhook: wh, team_id: kit.teamId() } : null;
  },
  /** Internal (webhook API on IngressDO-owned routes): the webhook, authorized for the route role. */
  webhookForRoute: (kit, c) => {
    const { wh } = webhookFor(kit, c);
    return webhookView(kit, wh);
  },
};
