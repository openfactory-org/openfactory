// TeamDO module (WS10a): schedules, custom webhook records and personal forge
// tokens. Migrations 0150-0199.
//
// Schedules: definitionApply calls syncSchedules inside its transaction. Each
// active schedule has one TeamDO timer ("sched:<key>") at its next slot; the
// 1/min Cron safety net re-arms the alarm. A slot writes last_fired_ms and an
// ingress.deliver outbox row (partition "schedule:<automationId>", msgId =
// deterministic event ID) in one transaction, so a slot fires at most once.
// Pause and the last error live here, outside the definition (Go KV keys
// triggers/schedule/{paused,last_error,last_fired}/).
import { OfError, type Automation, type Trigger, type Webhook } from "@openfactory/core";
import { actorId } from "../../auth/roles";
import { timingSafeEqual } from "../../lib/bytes";
import { all, first } from "../../lib/sql/query";
import { dest } from "../../lib/outbox/dest";
import { listResponse } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { canonicalProvider } from "../../triggers/catalog";
import { parseCron } from "../../triggers/cron";
import { EVENT_PROVIDER } from "@openfactory/core";
import { EventCronFired } from "../../triggers/attributes";
import { scheduleEvent, scheduleId, scheduleKey } from "../../triggers/schedules";
import { USER_TOKEN_FORGES, integrationProvider, supportsUserTokens } from "../../forges/registry";
import {
  ingressURL,
  generateSecret,
  AUTH_SIGNATURE,
  AUTH_URL_TOKEN,
  MAX_WEBHOOKS_PER_FACTORY,
  validURLToken,
} from "../../webhooks/config";
import { signatureSchemes } from "../../webhooks/signature";
import { created, noContent, type OpCall, type TeamKit, type TeamOp } from "./index";
import * as factories from "./factories";
import * as secrets from "./secrets";
import { setNoticeDeliverer } from "../work/index";
import { deliverNotice } from "../../notify/index";

// Notices of the WorkDO outbox (dest "notice:") are delivered by src/notify.
setNoticeDeliverer((env, envelope) => deliverNotice(env, envelope));

export const migrations: readonly Migration[] = [
  {
    version: 150,
    name: "schedules",
    sql: `
CREATE TABLE schedules (key TEXT PRIMARY KEY, factory_id TEXT NOT NULL, automation_id TEXT NOT NULL,
  automation_name TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', cron TEXT NOT NULL,
  concurrency TEXT NOT NULL DEFAULT '', active INTEGER NOT NULL DEFAULT 1, next_ms INTEGER NOT NULL DEFAULT 0,
  last_fired_ms INTEGER NOT NULL DEFAULT 0, paused TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '');
CREATE INDEX schedules_factory ON schedules (factory_id);
`,
  },
  {
    version: 151,
    name: "webhooks",
    sql: `
CREATE TABLE webhooks (id TEXT PRIMARY KEY, factory_id TEXT NOT NULL, name TEXT NOT NULL, secret_name TEXT NOT NULL DEFAULT '',
  created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX webhooks_factory ON webhooks (factory_id);
`,
  },
  {
    version: 152,
    name: "user_forge_tokens",
    sql: `
CREATE TABLE user_forge_tokens (user_id TEXT NOT NULL, provider TEXT NOT NULL, envelope BLOB NOT NULL, updated_ms INTEGER NOT NULL,
  PRIMARY KEY (user_id, provider));
`,
  },
];

const TIMER_KIND = "schedule.fire";
const timerName = (key: string) => "sched:" + key;

interface ScheduleRow {
  key: string;
  factory_id: string;
  automation_id: string;
  automation_name: string;
  name: string;
  cron: string;
  concurrency: string;
  active: number;
  next_ms: number;
  last_fired_ms: number;
  paused: string;
  last_error: string;
}

const getRow = (kit: TeamKit, key: string) =>
  first<ScheduleRow & Record<string, SqlStorageValue>>(kit.sql, "SELECT * FROM schedules WHERE key = ?", key);

function scheduleTriggers(a: Automation): Trigger[] {
  return (a.triggers ?? []).filter(
    (t) => canonicalProvider(t.provider) === EVENT_PROVIDER.Schedule && t.event === EventCronFired && t.schedule,
  );
}

function nextSlot(cron: string, baseMs: number): number {
  try {
    return parseCron(cron).next(baseMs);
  } catch {
    return 0;
  }
}

/**
 * Reconciles the schedules of a factory with its automations. Sync, no I/O:
 * call inside the definition apply transaction. Changing only the cron keeps
 * the last-fired state; a new name is a new schedule.
 */
export function syncSchedules(kit: TeamKit, factoryId: string, automations: unknown[]): void {
  const now = Date.now();
  const want = new Map<string, { a: Automation; t: Trigger }>();
  for (const a of automations as Automation[]) {
    if (!a?.enabled) continue;
    for (const t of scheduleTriggers(a)) want.set(scheduleKey(a.id, t.schedule!.name ?? ""), { a, t });
  }
  for (const r of all<ScheduleRow & Record<string, SqlStorageValue>>(
    kit.sql,
    "SELECT * FROM schedules WHERE factory_id = ? AND active = 1",
    factoryId,
  )) {
    if (want.has(r.key)) continue;
    kit.sql.exec("UPDATE schedules SET active = 0, next_ms = 0 WHERE key = ?", r.key);
    kit.clearTimer(timerName(r.key));
  }
  for (const [key, { a, t }] of want) {
    const cron = t.schedule!.cron;
    const cur = getRow(kit, key);
    let next = cur?.next_ms ?? 0;
    if (!cur || cur.cron !== cron || !cur.active || next === 0) {
      next = nextSlot(cron, Math.max(now, cur?.last_fired_ms ?? 0));
      if (next === 0) console.warn("schedule: invalid cron; skipping", a.id, t.schedule!.name, cron);
    }
    kit.sql.exec(
      `INSERT INTO schedules (key, factory_id, automation_id, automation_name, name, cron, concurrency, active, next_ms)
       VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
       ON CONFLICT(key) DO UPDATE SET factory_id = excluded.factory_id, automation_name = excluded.automation_name, cron = excluded.cron,
         concurrency = excluded.concurrency, active = excluded.active, next_ms = excluded.next_ms`,
      key,
      factoryId,
      a.id,
      a.name,
      t.schedule!.name ?? "",
      cron,
      t.concurrency ?? "",
      next === 0 ? 0 : 1,
      next,
    );
    if (next === 0) kit.clearTimer(timerName(key));
    else kit.setTimer(timerName(key), TIMER_KIND, next, { key });
  }
}

/** Reports whether a run started by the automation is not finished ("" = none). Registered by the engine. */
export type ActiveRunCheck = (kit: TeamKit, factoryId: string, automationId: string) => Promise<string>;
let activeRunCheck: ActiveRunCheck | undefined;

export function setScheduleActiveRunCheck(fn: ActiveRunCheck): void {
  activeRunCheck = fn;
}

function recordError(kit: TeamKit, key: string, msg: string): void {
  kit.tx(() => kit.sql.exec("UPDATE schedules SET last_error = ? WHERE key = ? AND last_error != ?", msg, key, msg));
}

/** Fires one due slot (timer handler). Exported for tests. */
export async function fireSchedule(kit: TeamKit, key: string, nowMs = Date.now()): Promise<void> {
  const r = getRow(kit, key);
  if (!r || !r.active || r.next_ms === 0) return;
  if (r.next_ms > nowMs) {
    kit.tx(() => kit.setTimer(timerName(key), TIMER_KIND, r.next_ms, { key }));
    return;
  }
  const slot = r.next_ms;
  const next = nextSlot(r.cron, nowMs);
  const advance = () => {
    kit.sql.exec("UPDATE schedules SET next_ms = ? WHERE key = ?", next, key);
    if (next > 0) kit.setTimer(timerName(key), TIMER_KIND, next, { key });
  };
  if (r.paused !== "") {
    kit.tx(advance);
    return;
  }
  if (r.concurrency === "skip" && activeRunCheck) {
    let active: string;
    try {
      active = await activeRunCheck(kit, r.factory_id, r.automation_id);
    } catch (e) {
      recordError(kit, key, "check for an active run: " + (e as Error).message);
      kit.tx(advance);
      return;
    }
    if (active !== "") {
      kit.tx(() => {
        kit.sql.exec("UPDATE schedules SET last_fired_ms = MAX(last_fired_ms, ?) WHERE key = ?", slot, key);
        advance();
      });
      return;
    }
  }
  const ev = await scheduleEvent({
    key,
    automationId: r.automation_id,
    automationName: r.automation_name,
    factoryId: r.factory_id,
    teamId: kit.teamId(),
    name: r.name,
    slotMs: slot,
    nowMs,
  });
  kit.tx(() => {
    const cur = getRow(kit, key);
    if (!cur || cur.last_fired_ms >= slot) {
      advance();
      return;
    }
    const partition = "schedule:" + r.automation_id;
    kit.enqueue({
      dest: dest.ingress(kit.teamId(), partition),
      kind: "ingress.deliver",
      msgId: ev.id,
      payload: { partition, event: ev as never, dedupeKey: ev.id },
    });
    kit.sql.exec("UPDATE schedules SET last_fired_ms = ?, last_error = '' WHERE key = ?", slot, key);
    advance();
  });
}

// ---------- schedule status (GET /factories/{uid}/schedules) ----------

export interface ScheduleStatus {
  automation_id: string;
  automation_name: string;
  id: string;
  name: string;
  cron: string;
  concurrency: string;
  enabled: boolean;
  paused: boolean;
  paused_at?: string;
  paused_by?: string;
  last_run_at: string | null;
  next_run_at: string | null;
  last_error?: string;
}

const iso = (ms: number) => new Date(ms).toISOString().replace(".000Z", "Z");

/** The schedule triggers of the factory's automations, sorted by schedule ID. */
export function listScheduleStatus(kit: TeamKit, factoryId: string, nowMs = Date.now()): ScheduleStatus[] {
  const out: ScheduleStatus[] = [];
  for (const a of factories.listAutomations(kit, factoryId)) {
    for (const t of scheduleTriggers(a)) {
      const name = t.schedule!.name ?? "";
      const r = getRow(kit, scheduleKey(a.id, name));
      const st: ScheduleStatus = {
        automation_id: a.id,
        automation_name: a.name,
        id: scheduleId(a.name, name),
        name,
        cron: t.schedule!.cron,
        concurrency: t.concurrency || "allow",
        enabled: a.enabled,
        paused: false,
        last_run_at: null,
        next_run_at: null,
      };
      let base = nowMs;
      if (r?.last_fired_ms) {
        st.last_run_at = iso(r.last_fired_ms);
        base = Math.max(base, r.last_fired_ms);
      }
      if (r?.paused) {
        st.paused = true;
        const i = r.paused.indexOf(" ");
        const at = i < 0 ? r.paused : r.paused.slice(0, i);
        if (!Number.isNaN(Date.parse(at))) st.paused_at = at;
        const by = i < 0 ? "" : r.paused.slice(i + 1);
        if (by) st.paused_by = by;
      }
      if (r?.last_error) st.last_error = r.last_error;
      try {
        const s = parseCron(t.schedule!.cron);
        if (a.enabled && !st.paused) {
          const n = s.next(base);
          if (n > 0) st.next_run_at = iso(n);
        }
      } catch (e) {
        st.last_error = "invalid cron: " + (e as Error).message;
      }
      out.push(st);
    }
  }
  return out.sort((x, y) => (x.id < y.id ? -1 : x.id > y.id ? 1 : 0));
}

function findSchedule(kit: TeamKit, factoryId: string, id: string): { a: Automation; t: Trigger } {
  for (const a of factories.listAutomations(kit, factoryId)) {
    if (a.name !== id && !id.startsWith(a.name + ":")) continue;
    for (const t of scheduleTriggers(a)) if (scheduleId(a.name, t.schedule!.name ?? "") === id) return { a, t };
  }
  throw new OfError("resource_not_found", `schedule ${id}`);
}

function setPaused(kit: TeamKit, c: OpCall, paused: boolean): ScheduleStatus {
  const f = factories.factoryFor(kit, c);
  const id = c.params["id"] ?? "";
  const { a, t } = findSchedule(kit, f.id, id);
  const key = scheduleKey(a.id, t.schedule!.name ?? "");
  const v = paused ? `${iso(Math.floor(Date.now() / 1000) * 1000)} ${actorId(c.principal())}` : "";
  kit.tx(() => {
    kit.sql.exec(
      `INSERT INTO schedules (key, factory_id, automation_id, automation_name, name, cron, concurrency, active, next_ms, paused)
       VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?) ON CONFLICT(key) DO UPDATE SET paused = excluded.paused`,
      key,
      f.id,
      a.id,
      a.name,
      t.schedule!.name ?? "",
      t.schedule!.cron,
      t.concurrency ?? "",
      v,
    );
  });
  const st = listScheduleStatus(kit, f.id).find((s) => s.id === id);
  if (!st) throw new OfError("resource_not_found", `schedule ${id}`);
  return st;
}

// ---------- webhooks ----------

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

function webhookFor(kit: TeamKit, c: OpCall): { wh: Webhook; f: factories.FactoryRecord } {
  const id = c.params["id"] ?? "";
  const wh = getWebhook(kit, id);
  const f = wh ? factories.getFactoryById(kit, wh.factory_id) : undefined;
  c.authorize(f?.id ?? "");
  if (!wh || !f) throw new OfError("resource_not_found", `webhook ${id} not found`);
  return { wh, f };
}

const WEBHOOK_NAME_RE = /^[a-z0-9][a-z0-9_-]{0,62}$/;
const LATE_SECRET_SCHEMES = ["vercel", "stripe", "pagerduty"];

async function managedFiles(kit: TeamKit, f: factories.FactoryRecord): Promise<Record<string, string>> {
  if (f.source && f.source !== "managed") {
    throw new OfError(
      "operation_not_supported",
      `this factory's definition lives in ${f.git_repo ?? "git"}: change webhooks/<name>.yaml there`,
    );
  }
  if (!f.active_version_id) throw new OfError("operation_not_supported", "factory has no applied definition yet");
  return { ...(await factories.activeFiles(kit, f)) };
}

/** factories.applyFiles with the error text of the Go webhook routes. */
async function applyFiles(
  kit: TeamKit,
  c: OpCall,
  f: factories.FactoryRecord,
  files: Record<string, string>,
  message: string,
): Promise<void> {
  await factories.applyFiles(kit, c, f, files, message, "definition could not be applied");
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
  const f = factories.factoryFor(kit, c);
  const req = (c.body ?? {}) as {
    name?: string;
    auth_mode?: string;
    signature_scheme?: string;
    secret_name?: string;
    secret?: string;
    delivery_id_header?: string;
    enabled?: boolean;
  };
  const name = (req.name ?? "").trim();
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
  if (factories.webhookFile(name) in files) throw new OfError("conflict", `webhook "${name}" already exists`);
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
  const doc = factories.editYamlDocument("", {
    authMode: mode,
    ...(scheme ? { signatureScheme: scheme } : {}),
    secretName,
    ...(req.delivery_id_header ? { deliveryIdHeader: req.delivery_id_header } : {}),
    enabled,
  });
  files[factories.webhookFile(name)] = doc;
  await applyWithSecret(kit, secretName, store, () => applyFiles(kit, c, f, files, "Add webhook " + name));
  const wh = listWebhooks(kit, f.id).find((w) => w.name === name);
  if (!wh) throw new OfError("resource_not_found", `webhook ${name}`);
  let url = ingressURL(kit.env.OF_PUBLIC_URL ?? "", { id: wh.id, auth_mode: "" });
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
  const req = (c.body ?? {}) as {
    name?: string;
    auth_mode?: string;
    signature_scheme?: string;
    enabled?: boolean;
    delivery_id_header?: string;
    secret?: string;
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
  const path = factories.webhookFile(wh.name);
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
  files[path] = factories.editYamlDocument(doc, edits);
  await applyWithSecret(kit, wh.secret_name, store, () => applyFiles(kit, c, f, files, "Update webhook " + wh.name));
  const nwh = listWebhooks(kit, f.id).find((w) => w.name === wh.name);
  if (!nwh) throw new OfError("resource_not_found", `webhook ${wh.name}`);
  return webhookView(kit, nwh);
}

async function rotateSecret(kit: TeamKit, c: OpCall): Promise<unknown> {
  const { wh, f } = webhookFor(kit, c);
  const req = (c.body ?? {}) as { secret?: string };
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
  const base = ingressURL(kit.env.OF_PUBLIC_URL ?? "", { id: wh.id, auth_mode: "" });
  const resp: Record<string, unknown> = { webhook: webhookView(kit, wh), ingress_url: base };
  if (reveal) {
    resp["secret"] = reveal;
    if (wh.auth_mode === AUTH_URL_TOKEN) resp["ingress_url"] = base + "/" + reveal;
  }
  return resp;
}

// ---------- personal forge tokens ----------

const tokenName = (userId: string, provider: string) => `user-forge-token/${userId}/${provider}`;

function userOf(c: OpCall): string {
  const p = c.principal();
  const uid = p.userId ?? (p.kind === "user" ? p.id : "");
  if (!uid) throw new OfError("not_authorized", "personal forge tokens require an API key that belongs to a user");
  return uid;
}

/** The decrypted personal token of a user for codeForge, or undefined. */
export async function getUserForgeToken(kit: TeamKit, userId: string, codeForge: string): Promise<string | undefined> {
  if (!userId) return undefined;
  const p = integrationProvider(codeForge);
  const r = first<{ envelope: ArrayBuffer }>(
    kit.sql,
    "SELECT envelope FROM user_forge_tokens WHERE user_id = ? AND provider = ?",
    userId,
    p,
  );
  if (!r) return undefined;
  const v = await secrets.openValue(
    kit,
    { teamId: kit.teamId(), factoryId: "", name: tokenName(userId, p), type: "generic" },
    new Uint8Array(r.envelope),
  );
  return new TextDecoder().decode(v);
}

async function setUserForgeToken(kit: TeamKit, c: OpCall): Promise<unknown> {
  const uid = userOf(c);
  const forge = (c.params["forge"] ?? "").toUpperCase();
  const tok = ((c.body ?? {}) as { token?: string }).token?.trim() ?? "";
  if (tok === "") throw new OfError("invalid_request", "token is required");
  if (!supportsUserTokens(forge)) {
    throw new OfError(
      "invalid_request",
      `${forge} does not support per-user tokens (supported: ${USER_TOKEN_FORGES.join(", ")})`,
    );
  }
  const p = integrationProvider(forge);
  const env = await secrets.sealValue(
    kit,
    { teamId: kit.teamId(), factoryId: "", name: tokenName(uid, p), type: "generic" },
    tok,
  );
  kit.tx(() =>
    kit.sql.exec(
      `INSERT INTO user_forge_tokens (user_id, provider, envelope, updated_ms) VALUES (?, ?, ?, ?)
       ON CONFLICT(user_id, provider) DO UPDATE SET envelope = excluded.envelope, updated_ms = excluded.updated_ms`,
      uid,
      p,
      env,
      Date.now(),
    ),
  );
  return noContent();
}

async function listUserForgeTokens(kit: TeamKit, c: OpCall): Promise<unknown> {
  const uid = userOf(c);
  const forges: string[] = [];
  const unreadable: string[] = [];
  for (const f of USER_TOKEN_FORGES) {
    const p = integrationProvider(f);
    if (!first(kit.sql, "SELECT 1 AS x FROM user_forge_tokens WHERE user_id = ? AND provider = ?", uid, p)) continue;
    forges.push(f);
    try {
      await getUserForgeToken(kit, uid, f);
    } catch {
      unreadable.push(f);
    }
  }
  return { forges, unreadable };
}

// ---------- ops ----------

export const ops: Record<string, TeamOp> = {
  listSchedules: (kit, c) => {
    const f = factories.factoryFor(kit, c);
    return listResponse(listScheduleStatus(kit, f.id), "");
  },
  pauseSchedule: (kit, c) => setPaused(kit, c, true),
  resumeSchedule: (kit, c) => setPaused(kit, c, false),
  /** Internal (Worker runAutomation): the automation and its factory, authorized for editor. */
  automationForRun: (kit, c) => {
    const id = c.params["id"] ?? c.query["id"] ?? "";
    for (const f of factories.listFactories(kit)) {
      const a = factories.listAutomations(kit, f.id).find((x) => x.id === id);
      if (!a) continue;
      const p = c.authorize(f.id);
      return { automation: a, factory: f, team_id: kit.teamId(), principal: p };
    }
    c.authorize("");
    throw new OfError("resource_not_found", `automation ${id}`);
  },

  listWebhooks: (kit, c) => {
    const f = factories.factoryFor(kit, c);
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
    delete files[factories.webhookFile(wh.name)];
    await applyFiles(kit, c, f, files, "Delete webhook " + wh.name);
    return noContent();
  },
  /** Internal (GitHub webhook routing): the factories of the team (core.Factory JSON). */
  forgeFactories: (kit) => ({ items: factories.listFactories(kit) }),
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

  listUserForgeTokens,
  setUserForgeToken,
  deleteUserForgeToken: (kit, c) => {
    const uid = userOf(c);
    const p = integrationProvider((c.params["forge"] ?? "").toUpperCase());
    kit.tx(() => kit.sql.exec("DELETE FROM user_forge_tokens WHERE user_id = ? AND provider = ?", uid, p));
    return noContent();
  },
  /** Internal (forge resolver): {token} of body {user_id, forge}. */
  userForgeTokenGet: async (kit, c) => {
    const b = (c.body ?? {}) as { user_id?: string; forge?: string };
    const token = await getUserForgeToken(kit, b.user_id ?? "", b.forge ?? "");
    return { token: token ?? "" };
  },
};

export function setup(kit: TeamKit): void {
  kit.onTimerAsync<{ key: string }>(TIMER_KIND, async (t) => {
    const key = t.payload?.key ?? t.name.slice("sched:".length);
    await fireSchedule(kit, key);
  });
  secrets.registerRewrapSource({
    name: "user_forge_tokens",
    list: (k) =>
      all<{ user_id: string; provider: string; envelope: ArrayBuffer }>(
        k.sql,
        "SELECT user_id, provider, envelope FROM user_forge_tokens",
      ).map((r) => ({
        key: `${r.user_id}\u0000${r.provider}`,
        id: { teamId: k.teamId(), factoryId: "", name: tokenName(r.user_id, r.provider), type: "generic" },
        envelope: new Uint8Array(r.envelope),
      })),
    replace: (k, key, old, next) => {
      const [uid, p] = key.split("\u0000") as [string, string];
      const cur = first<{ envelope: ArrayBuffer }>(
        k.sql,
        "SELECT envelope FROM user_forge_tokens WHERE user_id = ? AND provider = ?",
        uid,
        p,
      );
      if (!cur) return false;
      if (!timingSafeEqual(new Uint8Array(cur.envelope), old)) return false;
      k.sql.exec("UPDATE user_forge_tokens SET envelope = ? WHERE user_id = ? AND provider = ?", next, uid, p);
      return true;
    },
  });
}
