// Schedule helpers shared by the TeamDO schedules module and the API: schedule
// keys and IDs, the deterministic event of a slot, the manual-run event and
// the follower slot rule. Port of internal/triggers/{schedules,scheduler}.go.
import { newId, sha256Hex, type Actor, type Automation, type Event } from "@openfactory/core";
import { EVENT_ATTR, EVENT_PROVIDER } from "@openfactory/core";
import * as T from "./attributes";
import type { CronSchedule } from "./cron";

/** Key of one schedule: automation ID and schedule name. */
export function scheduleKey(automationId: string, name: string): string {
  return automationId + ":" + name;
}

/** The schedule ID of the API: the automation name, plus ":" and the schedule name. */
export function scheduleId(automationName: string, scheduleName: string): string {
  return scheduleName === "" ? automationName : automationName + ":" + scheduleName;
}

const rfc3339 = (ms: number): string => new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");

/** The deterministic event ID of one slot, so that a slot fired twice deduplicates. */
export async function scheduleEventId(key: string, slotMs: number): Promise<string> {
  const sum = await sha256Hex(`${key}@${Math.floor(slotMs / 1000)}`);
  return "evt_sched_" + sum.slice(0, 24);
}

export interface SlotInput {
  key: string;
  automationId: string;
  automationName: string;
  factoryId: string;
  teamId: string;
  name: string;
  slotMs: number;
  nowMs: number;
}

/** The event of one schedule slot (schedule/cron_fired). */
export async function scheduleEvent(s: SlotInput): Promise<Event> {
  return {
    id: await scheduleEventId(s.key, s.slotMs),
    team_id: s.teamId,
    factory_id: s.factoryId,
    provider: EVENT_PROVIDER.Schedule,
    type: T.EventCronFired,
    delivery_id: `${s.key}@${Math.floor(s.slotMs / 1000)}`,
    actor: { provider: EVENT_PROVIDER.Schedule, external_id: "", handle: "schedule", is_bot: false },
    source: { provider: EVENT_PROVIDER.Schedule, kind: "schedule", title: s.automationName },
    attributes: {
      [EVENT_ATTR.AutomationID]: s.automationId,
      [EVENT_ATTR.ScheduleName]: s.name,
      [EVENT_ATTR.ScheduledAt]: rfc3339(s.slotMs),
    },
    received_at: new Date(s.nowMs).toISOString(),
  };
}

/** The event of a manual start of an automation (POST /automations/{id}/run). */
export function manualEvent(
  a: Pick<Automation, "id" | "name" | "factory_id">,
  teamId: string,
  actor: Actor,
  nowMs: number,
): Event {
  const id = newId("evt");
  return {
    id,
    team_id: teamId,
    factory_id: a.factory_id,
    provider: "factory",
    type: "manual_run",
    delivery_id: `manual:${a.id}@${BigInt(nowMs) * 1_000_000n}`,
    actor,
    source: { provider: "factory", kind: "manual", title: a.name, external_id: id },
    attributes: { [EVENT_ATTR.AutomationID]: a.id },
    received_at: new Date(nowMs).toISOString(),
  };
}

/**
 * The slot a replica that missed due slots keeps: the latest slot at or before
 * now that is not older than window, or the first slot after now.
 */
export function pendingSlot(sched: CronSchedule, next: number, now: number, windowMs: number): number {
  let latest = next;
  if (now - latest > windowMs) {
    latest = sched.next(now - windowMs);
    if (latest > now) return latest;
  }
  for (;;) {
    const n = sched.next(latest);
    if (n === 0 || n > now) return latest;
    latest = n;
  }
}
