import { describe, expect, it } from "vitest";
import { OfError } from "@openfactory/core";
import { NameLists, addId, addName, sortIds } from "./namelist";
import { NoIntegrationError, intake, select, selectTeam } from "./select";
import { FakeStore, integration } from "./testkit";
import { autoRespondToThreadReplies, factorySetting, member } from "./shared";
import { user } from "./testkit";

const slack = (id: string, ws: string, over = {}) =>
  integration({ id, provider: "slack", config: { team_id: ws }, ...over });

describe("select", () => {
  it("prefers the factory record, then the team record", () => {
    const ints = [slack("t", "T1"), slack("f", "T1", { factory_id: "fac_1" }), slack("o", "T1", { factory_id: "fac_2" })];
    expect(select(ints, "slack", "fac_1", "T1").id).toBe("f");
    expect(select(ints, "slack", "fac_3", "t1").id).toBe("t");
    expect(select(ints, "slack", "", "T1").id).toBe("t");
  });

  it("never uses another factory's record or the first record", () => {
    const ints = [slack("o", "T1", { factory_id: "fac_2" })];
    expect(() => select(ints, "slack", "fac_1", "T1")).toThrow(NoIntegrationError);
    expect(() => select([slack("a", "T1"), slack("b", "T1")], "slack", "", "T1")).toThrow(/keep one/);
    expect(() => select([slack("a", "T1"), slack("b", "T2")], "slack", "", "")).toThrow(/does not name its workspace/);
    expect(select([slack("a", "T1")], "slack", "", "").id).toBe("a");
  });

  it("names the key to add when no record matches", () => {
    expect(() => select([], "jira", "", "https://Acme.atlassian.net/x")).toThrow(/acme\.atlassian\.net.*config\.site/);
  });

  it("selectTeam rejects a record that is not active", async () => {
    const s = new FakeStore();
    s.integrations = [slack("a", "T1", { status: "disabled", status_reason: "by admin" })];
    const err = await selectTeam(s, "team_1", "slack", "", "T1").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(OfError);
    expect((err as OfError).code).toBe("integration_disabled");
    expect((err as OfError).message).toContain("by admin");
  });
});

describe("intake", () => {
  it("returns the team record, or the one factory record", async () => {
    const s = new FakeStore();
    s.integrations = [slack("t", "T1"), slack("f", "T1", { factory_id: "fac_1" })];
    expect((await intake(s, "slack", "T1")).integration?.id).toBe("t");
    s.integrations = [slack("f", "T1", { factory_id: "fac_1" })];
    expect((await intake(s, "slack", "T1")).integration?.id).toBe("f");
    s.integrations = [slack("f", "T1", { factory_id: "fac_1" }), slack("g", "T1", { factory_id: "fac_2" })];
    const r = await intake(s, "slack", "T1");
    expect(r.integration).toBeNull();
    expect(r.error?.message).toMatch(/no team record/);
  });

  it("refuses records of two teams and an empty workspace", async () => {
    const s = new FakeStore();
    s.integrations = [slack("a", "T1"), slack("b", "T1", { team_id: "team_2" })];
    expect((await intake(s, "slack", "T1")).error?.message).toMatch(/records of 2 teams/);
    expect((await intake(s, "slack", "")).error).toBeInstanceOf(NoIntegrationError);
  });

  it("returns a disabled record with its error, and applies match", async () => {
    const s = new FakeStore();
    s.integrations = [slack("a", "T1", { status: "auth_failed" }), slack("b", "T1", { config: { team_id: "T1", api_app_id: "A2" } })];
    const r = await intake(s, "slack", "T1", (i) => i.config?.["api_app_id"] !== "A2");
    expect(r.integration?.id).toBe("a");
    expect((r.error as OfError).code).toBe("integration_disabled");
  });
});

describe("name lists", () => {
  it("caches, refreshes a missing name at most every 30 s, backs off after an error and serves stale", async () => {
    let now = 0;
    let reads = 0;
    let fail = false;
    const l = new NameLists(() => now);
    const fetch = async () => {
      reads++;
      if (fail) throw new Error("boom");
      const m = {};
      addName(m, "General", "C1");
      return m;
    };
    expect(await l.lookup("k", ["general"], fetch)).toEqual({ general: ["C1"] });
    await l.lookup("k", ["general"], fetch);
    expect(reads).toBe(1);
    await l.lookup("k", ["missing"], fetch); // list age 0 < 30 s: no new read
    expect(reads).toBe(1);
    now = 31_000;
    await l.lookup("k", ["missing"], fetch);
    expect(reads).toBe(2);
    fail = true;
    now = 6 * 60_000;
    expect(await l.get("k", 300_000, fetch)).toEqual({ general: ["C1"] }); // stale list
    expect(reads).toBe(3);
    now += 10_000;
    await l.get("k", 300_000, fetch); // back-off: no call
    expect(reads).toBe(3);
    now += 2 * 3600_000;
    await expect(l.get("k", 300_000, fetch)).rejects.toThrow("boom");
  });

  it("adds, dedupes and sorts ids", () => {
    const m: Record<string, string[]> = {};
    addName(m, " Foo ", "b");
    addName(m, "foo", "a");
    addName(m, "foo", "a");
    addName(m, "", "x");
    addId(m, "bar", "1");
    sortIds(m);
    expect(m).toEqual({ foo: ["a", "b"], bar: ["1"] });
  });
});

describe("shared", () => {
  it("member needs a link and the same team", async () => {
    const s = new FakeStore();
    s.users = [user({ id: "usr_1" }), user({ id: "usr_2", team_id: "team_2" })];
    s.identities.set("slack|U1", { user_id: "usr_1", provider: "slack", external_id: "U1", handle: "" });
    s.identities.set("slack|U2", { user_id: "usr_2", provider: "slack", external_id: "U2", handle: "" });
    expect((await member(s, "team_1", "slack", "U1"))?.id).toBe("usr_1");
    expect(await member(s, "team_1", "slack", "U2")).toBeNull();
    expect(await member(s, "team_1", "slack", "U9")).toBeNull();
  });

  it("reads factory settings with either dash form", async () => {
    const s = new FakeStore();
    s.settings["fac_1"] = { microsoft_teams: { autoRespondToThreadReplies: "No" } };
    expect(await factorySetting(s, "fac_1", "microsoft-teams", "autoRespondToThreadReplies")).toBe("No");
    expect(await autoRespondToThreadReplies(s, "fac_1", "microsoft-teams")).toBe(false);
    expect(await autoRespondToThreadReplies(s, "fac_2", "slack")).toBe(true);
  });
});
