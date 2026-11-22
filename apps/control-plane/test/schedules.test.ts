// TeamDO schedules module: sync, firing (outbox ingress.deliver, once per
// slot), pause, and personal forge tokens.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { TeamKit } from "../src/do/team/kit";
import { fireSchedule, setScheduleActiveRunCheck, syncSchedules } from "../src/do/team/schedules";
import { getUserForgeToken } from "../src/do/team/user-forge-tokens";

const automation = (cron: string, extra: Record<string, unknown> = {}) => ({
  id: "aut_1",
  factory_id: "fac_1",
  name: "nightly",
  enabled: true,
  agent: "a",
  prompt: "p",
  triggers: [{ provider: "schedule", event: "cron_fired", schedule: { name: "", cron }, ...extra }],
});

const principal = {
  kind: "user" as const,
  id: "usr_1",
  userId: "usr_1",
  teamId: "sched",
  role: "user" as const,
  factoryRoles: {},
};

describe("TeamDO schedules", () => {
  it("syncs, fires each slot once and pauses", async () => {
    const stub = env.TEAM.getByName("team:sched");
    await runInDurableObject(stub, async (instance) => {
      const kit = (instance as unknown as { kit: TeamKit }).kit;
      kit.tx(() => syncSchedules(kit, "fac_1", [automation("*/5 * * * *")]));
      const row = kit.sql.exec("SELECT key, next_ms, active FROM schedules").one() as {
        key: string;
        next_ms: number;
        active: number;
      };
      expect(row.key).toBe("aut_1:");
      expect(row.active).toBe(1);
      expect(row.next_ms % 300_000).toBe(0);
      expect(kit.sql.exec("SELECT kind FROM timers WHERE name = 'sched:aut_1:'").one().kind).toBe("schedule.fire");

      await fireSchedule(kit, "aut_1:", row.next_ms + 1000);
      const out = kit.sql
        .exec("SELECT dest, kind, msg_id, payload FROM outbox WHERE kind = 'ingress.deliver'")
        .toArray();
      expect(out.length).toBe(1);
      expect(out[0]!.dest).toBe("do:INGRESS:team:sched|ingress:schedule:aut_1");
      expect(String(out[0]!.msg_id)).toMatch(/^evt_sched_/);
      const ev = (
        JSON.parse(String(out[0]!.payload)) as { event: { attributes: Record<string, string>; type: string } }
      ).event;
      expect(ev.type).toBe("cron_fired");
      expect(ev.attributes["automation_id"]).toBe("aut_1");
      const after = kit.sql.exec("SELECT next_ms, last_fired_ms FROM schedules").one() as {
        next_ms: number;
        last_fired_ms: number;
      };
      expect(after.last_fired_ms).toBe(row.next_ms);
      expect(after.next_ms).toBe(row.next_ms + 300_000);

      // Paused: the slot advances and nothing fires.
      kit.tx(() => kit.sql.exec("UPDATE schedules SET paused = '2026-01-01T00:00:00Z usr_1'"));
      await fireSchedule(kit, "aut_1:", after.next_ms + 1000);
      expect(kit.sql.exec("SELECT COUNT(*) AS n FROM outbox WHERE kind = 'ingress.deliver'").one().n).toBe(1);

      // Changing only the cron keeps the state; disabling deactivates.
      kit.tx(() => syncSchedules(kit, "fac_1", [automation("0 * * * *")]));
      expect(
        (kit.sql.exec("SELECT last_fired_ms FROM schedules").one() as { last_fired_ms: number }).last_fired_ms,
      ).toBe(row.next_ms);
      kit.tx(() => syncSchedules(kit, "fac_1", [{ ...automation("0 * * * *"), enabled: false }]));
      expect(kit.sql.exec("SELECT active FROM schedules").one().active).toBe(0);
      expect(kit.sql.exec("SELECT COUNT(*) AS n FROM timers WHERE name = 'sched:aut_1:'").one().n).toBe(0);
    });
  });

  it("skips a concurrency skip slot and records the error when no active-run check is registered", async () => {
    const prev = setScheduleActiveRunCheck(undefined);
    try {
      await runInDurableObject(env.TEAM.getByName("team:sched-skip"), async (instance) => {
        const kit = (instance as unknown as { kit: TeamKit }).kit;
        kit.tx(() => syncSchedules(kit, "fac_1", [automation("*/5 * * * *", { concurrency: "skip" })]));
        const row = kit.sql.exec("SELECT next_ms FROM schedules").one() as { next_ms: number };
        await fireSchedule(kit, "aut_1:", row.next_ms + 1000);
        expect(kit.sql.exec("SELECT COUNT(*) AS n FROM outbox WHERE kind = 'ingress.deliver'").one().n).toBe(0);
        const after = kit.sql.exec("SELECT next_ms, last_error FROM schedules").one() as {
          next_ms: number;
          last_error: string;
        };
        expect(after.last_error).toBe("check for an active run: no active-run check is registered");
        expect(after.next_ms).toBe(row.next_ms + 300_000);
      });
    } finally {
      setScheduleActiveRunCheck(prev);
    }
  });

  it("stores personal forge tokens sealed", async () => {
    const stub = env.TEAM.getByName("team:sched");
    const meta = { requestId: "r1", principal };
    expect(await stub.api(meta, "setUserForgeToken", { forge: "github" }, { token: " ghp_x " })).toEqual({
      $status: 204,
    });
    expect(await stub.api(meta, "listUserForgeTokens", {}, undefined)).toEqual({ forges: ["GITHUB"], unreadable: [] });
    const bad = (await stub.api(meta, "setUserForgeToken", { forge: "git" }, { token: "x" })) as {
      $error: { code: string };
    };
    expect(bad.$error.code).toBe("invalid_request");
    await runInDurableObject(stub, async (instance) => {
      const kit = (instance as unknown as { kit: TeamKit }).kit;
      expect(await getUserForgeToken(kit, "usr_1", "GITHUB")).toBe("ghp_x");
      const raw = kit.sql.exec("SELECT envelope FROM user_forge_tokens").one().envelope as ArrayBuffer;
      expect(new TextDecoder().decode(raw)).not.toContain("ghp_x");
    });
    expect(await stub.api(meta, "deleteUserForgeToken", { forge: "GITHUB" }, undefined)).toEqual({ $status: 204 });
    expect(await stub.api(meta, "listUserForgeTokens", {}, undefined)).toEqual({ forges: [], unreadable: [] });
  });
});
