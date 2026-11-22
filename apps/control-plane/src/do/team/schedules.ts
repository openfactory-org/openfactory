// TeamDO module: schedules. Migrations 0150-0199 (shared with webhooks.ts and
// user-forge-tokens.ts).
//
// definitionApply calls syncSchedules inside its transaction. Each
// active schedule has one TeamDO timer ("sched:<key>") at its next slot; the
// 1/min Cron safety net re-arms the alarm. A slot writes last_fired_ms and an
// ingress.deliver outbox row (partition "schedule:<automationId>", msgId =
// deterministic event ID) in one transaction, so a slot fires at most once.
// Pause and the last error live here, outside the definition (Go KV keys
// triggers/schedule/{paused,last_error,last_fired}/).
import { OfError, type Automation, type Trigger } from "@openfactory/core";
import { actorId } from "../../auth/roles";
import { all, first } from "../../lib/sql/query";
import { dest } from "../../lib/outbox/dest";
import { listResponse } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { canonicalProvider } from "../../triggers/catalog";
import { parseCron } from "../../triggers/cron";
import { EVENT_PROVIDER } from "@openfactory/core";
import { EventCronFired } from "../../triggers/attributes";
import { scheduleEvent, scheduleId, scheduleKey } from "../../triggers/schedules";
import type { OpCall, TeamKit, TeamOp } from "./kit";
import { factoryFor, getFactoryById, listAutomations } from "./factories/repo";

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

/**
 * Reports whether a run started by the automation is not finished ("" = none).
 * wiring.ts registers it. Without it a "skip" schedule does not fire: the slot
 * is skipped and last_error says why.
 */
export type ActiveRunCheck = (kit: TeamKit, factoryId: string, automationId: string) => Promise<string>;
let activeRunCheck: ActiveRunCheck | undefined;

/** Sets the check; returns the one it replaces (tests restore it). */
export function setScheduleActiveRunCheck(fn: ActiveRunCheck | undefined): ActiveRunCheck | undefined {
  const prev = activeRunCheck;
  activeRunCheck = fn;
  return prev;
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
  if (r.concurrency === "skip") {
    let active: string;
    try {
      if (!activeRunCheck) throw new Error("no active-run check is registered");
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
  for (const a of listAutomations(kit, factoryId)) {
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
  for (const a of listAutomations(kit, factoryId)) {
    if (a.name !== id && !id.startsWith(a.name + ":")) continue;
    for (const t of scheduleTriggers(a)) if (scheduleId(a.name, t.schedule!.name ?? "") === id) return { a, t };
  }
  throw new OfError("resource_not_found", `schedule ${id}`);
}

function setPaused(kit: TeamKit, c: OpCall, paused: boolean): ScheduleStatus {
  const f = factoryFor(kit, c);
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

// ---------- ops ----------

export const ops: Record<string, TeamOp> = {
  listSchedules: (kit, c) => {
    const f = factoryFor(kit, c);
    return listResponse(listScheduleStatus(kit, f.id), "");
  },
  pauseSchedule: (kit, c) => setPaused(kit, c, true),
  resumeSchedule: (kit, c) => setPaused(kit, c, false),
  /** Internal (Worker runAutomation): the automation and its factory, authorized for editor. */
  automationForRun: (kit, c) => {
    const id = c.params["id"] ?? c.query["id"] ?? "";
    const r = first<{ factory_id: string; data: string }>(
      kit.sql,
      "SELECT factory_id, data FROM automations WHERE id = ?",
      id,
    );
    const f = r ? getFactoryById(kit, r.factory_id) : undefined;
    if (!r || !f) {
      c.authorize("");
      throw new OfError("resource_not_found", `automation ${id}`);
    }
    const p = c.authorize(f.id);
    return { automation: JSON.parse(r.data) as Automation, factory: f, team_id: kit.teamId(), principal: p };
  },
};

export function setup(kit: TeamKit): void {
  kit.onTimerAsync<{ key: string }>(TIMER_KIND, async (t) => {
    const key = t.payload?.key ?? t.name.slice("sched:".length);
    await fireSchedule(kit, key);
  });
}
