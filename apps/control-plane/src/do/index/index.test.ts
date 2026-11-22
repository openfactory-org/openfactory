// IndexDO reads: authorization of REST callers inside the DO, and since_ms of metricEvents.
import { env } from "cloudflare:workers";
import { newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import type { CallMeta, Principal, Role } from "../../contracts/do-messages";
import type { IndexDO } from "./index";

function fresh(): { team: string; stub: IndexDO } {
  const team = "x" + newToken("w");
  return { team, stub: env.INDEX.getByName(`team:${team}|index`) as unknown as IndexDO };
}

function meta(team: string, role: string, factoryRoles: Record<string, Role> = {}): CallMeta {
  const principal: Principal = { kind: "user", id: "u1", userId: "u1", teamId: team, role: role as Role, factoryRoles };
  return { requestId: "r1", principal };
}

const internal: CallMeta = { requestId: "internal" };

/** "ok", or the error text of a rejected RPC call. */
const outcome = (p: Promise<unknown>): Promise<string> =>
  p.then(
    () => "ok",
    (e: unknown) => String(e),
  );

async function seed(stub: IndexDO): Promise<string> {
  const id = "inb_team_" + newToken("i");
  await stub.receive({
    msgId: "m-" + id,
    kind: "index.inboxCreate",
    lane: "",
    origin: "test",
    payload: { item: { id, factory_id: "fac_a", kind: "notice", recipients: ["u1"] } },
  } as never);
  await stub.receive({
    msgId: "m-wi-" + id,
    kind: "index.project",
    lane: "",
    origin: "test",
    payload: {
      entity: "work_item",
      id: "wi_1",
      version: 1,
      summary: { id: "wi_1", factory_id: "fac_a", stage: "triage" },
    },
  } as never);
  return id;
}

describe("IndexDO authorization", () => {
  it("checks the factory role of factory-scoped lists and inbox reads", async () => {
    const { team, stub } = fresh();
    const id = await seed(stub);
    const none = meta(team, "");
    const member = meta(team, "", { fac_a: "user" });
    const cases: { name: string; call: (m: CallMeta) => Promise<unknown> }[] = [
      { name: "workItems", call: (m) => stub.list(m, "workItems", { factory_id: "fac_a" }) },
      { name: "inbox", call: (m) => stub.list(m, "inbox", { factory_id: "fac_a", scope: "team" }) },
      { name: "getInboxItem", call: (m) => stub.getInboxItem(m, id) },
      { name: "markInboxRead", call: (m) => stub.markInboxRead(m, id) },
    ];
    for (const c of cases) {
      expect(await outcome(c.call(none)), c.name).toMatch(/not_authorized/);
      expect(await outcome(c.call(member)), c.name).toBe("ok");
    }
  });

  it("refuses a principal of another team and lets internal calls through", async () => {
    const { stub } = fresh();
    const id = await seed(stub);
    expect(await outcome(stub.list(meta("other", "admin"), "workItems", { factory_id: "fac_a" }))).toMatch(
      /not_authorized/,
    );
    expect(await outcome(stub.getInboxItem(meta("other", "admin"), id))).toMatch(/not_authorized/);
    const out = (await stub.list(internal, "workItems", { factory_id: "fac_a" })) as { items: unknown[] };
    expect(out.items).toHaveLength(1);
  });

  it("needs the admin role for dead letters", async () => {
    const { team, stub } = fresh();
    expect(await outcome(stub.list(meta(team, "editor"), "deadLetters", {}))).toMatch(/not_authorized/);
    expect(await stub.list(meta(team, "admin"), "deadLetters", {})).toMatchObject({ items: [] });
  });
});

describe("IndexDO metricEvents", () => {
  it("ignores a since_ms that is not a number", async () => {
    const { stub } = fresh();
    await stub.receive({
      msgId: "me1",
      kind: "index.workItemEvent",
      lane: "",
      origin: "test",
      payload: { eventId: "ev1", wiId: "wi_1", factoryId: "fac_a", kind: "created", atMs: 5000, data: {} },
    } as never);
    const page = (q: Record<string, string>) =>
      stub.list(internal, "metricEvents", { factory_id: "fac_a", ...q }) as Promise<{ items: { id: string }[] }>;
    expect((await page({ since_ms: "abc" })).items.map((x) => x.id)).toEqual(["ev1"]);
    expect((await page({ since_ms: "6000" })).items).toEqual([]);
    expect((await page({ since_ms: "4000" })).items.map((x) => x.id)).toEqual(["ev1"]);
  });
});
