// WorkDO authorization of REST callers: the caller's role for the factory of
// the run, work item or inbox item (the Go route wrapper's routeFactory), a
// principal of another team, and internal calls without a principal.
import { newRoutedId, newToken } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { CallMeta, Principal, Role } from "../../contracts/do-messages";
import type { AgentConfig, InboxItem, Run } from "../../engine/types";
import { installFakeSandbox, workStub } from "../../../test/fakes/runner";
import type { WorkDO } from "./index";

const TEAM = "default";
const internal: CallMeta = { requestId: "internal" };

function meta(role: string, factoryRoles: Record<string, Role> = {}, team = TEAM): CallMeta {
  const principal: Principal = { kind: "user", id: "u1", userId: "u1", teamId: team, role: role as Role, factoryRoles };
  return { requestId: "r1", principal };
}

/** "ok", or the error text of a rejected RPC call. */
const outcome = (p: Promise<unknown>): Promise<string> =>
  p.then(
    () => "ok",
    (e: unknown) => String(e),
  );

const agent = (factoryId: string): AgentConfig => ({
  id: "agt_" + factoryId,
  factory_id: factoryId,
  name: "foreman",
  description: "",
  type: "FOREMAN",
  instructions: "lead",
  harness: { type: "claude", model: "m" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
});

interface Seeded {
  w: WorkDO;
  run: Run;
  wiId: string;
  inboxId: string;
}

/** A work item of the factory with its root run and an open inbox item, created by internal calls. */
async function seed(factoryId: string): Promise<Seeded> {
  const token = newToken("w");
  const w = workStub(TEAM, token);
  const out = await w.runsCreate(internal, {
    dispatch: {
      factoryId,
      config: agent(factoryId),
      prompt: "work",
      actor: "usr_dana",
      title: "Work",
      workItemId: newRoutedId("wi", token),
    },
  });
  const it = (await w.api(internal, "createInboxItem", {}, { kind: "notice", title: "T", body: "B" })) as InboxItem;
  return { w, run: out.run as Run, wiId: out.workItemId!, inboxId: it.id };
}

/** The REST-facing calls of WorkDO on one seeded work item. */
function calls(s: Seeded): { name: string; call: (m: CallMeta) => Promise<unknown> }[] {
  const runId = s.run.id;
  const wi = s.wiId;
  return [
    { name: "getRun", call: (m) => s.w.api(m, "getRun", { runId }, null) },
    { name: "listRunMessages", call: (m) => s.w.api(m, "listRunMessages", { runId }, null) },
    { name: "getRunTimeline", call: (m) => s.w.api(m, "getRunTimeline", { runId }, null) },
    { name: "listRunSessions", call: (m) => s.w.api(m, "listRunSessions", { runId }, null) },
    { name: "listRunChildren", call: (m) => s.w.api(m, "listRunChildren", { runId }, null) },
    { name: "getWorkItem", call: (m) => s.w.api(m, "getWorkItem", { id: wi }, null) },
    { name: "listWorkItemInbox", call: (m) => s.w.api(m, "listWorkItemInbox", { id: wi }, null) },
    { name: "getInbox", call: (m) => s.w.api(m, "getInbox", { id: s.inboxId }, null) },
    { name: "runsMessage", call: (m) => s.w.runsMessage(m, runId, { message: "hi", actor: "u1" }) },
  ];
}

beforeEach(() => {
  ({ uninstall } = installFakeSandbox());
});
let uninstall: () => void;
afterEach(() => uninstall());

describe("WorkDO authorization", () => {
  it("checks the caller's role for the factory of the run, work item and inbox item", async () => {
    const a = await seed("fac_authz_a");
    const b = await seed("fac_authz_b");
    // No valid team role; the user role on factory A only.
    const onlyA = meta("", { fac_authz_a: "user" });
    for (const c of calls(a)) expect(await outcome(c.call(onlyA)), c.name).toBe("ok");
    for (const c of calls(b)) expect(await outcome(c.call(onlyA)), c.name).toMatch(/not_authorized/);
    expect(await outcome(b.w.api(onlyA, "resolveInbox", { id: b.inboxId }, {}))).toMatch(/not_authorized/);
    expect(await outcome(b.w.api(onlyA, "workItemCancel", { id: b.wiId }, {}))).toMatch(/not_authorized/);
    expect(await outcome(b.w.runsInterrupt(onlyA, b.run.id))).toMatch(/not_authorized/);
    expect(await outcome(b.w.runsCancel(onlyA, b.run.id, true))).toMatch(/not_authorized/);
    expect((await b.w.getRun(b.run.id)).state).not.toBe("cancelled");
    const token = newToken("w");
    const dispatchB = workStub(TEAM, token).runsCreate(onlyA, {
      dispatch: {
        factoryId: "fac_authz_b",
        config: agent("fac_authz_b"),
        prompt: "x",
        actor: "u1",
        title: "x",
        workItemId: newRoutedId("wi", token),
      },
    });
    expect(await outcome(dispatchB)).toMatch(/not_authorized/);
  });

  it("needs the editor role on the work item's factory to set its stage", async () => {
    const a = await seed("fac_authz_a");
    const b = await seed("fac_authz_b");
    const editorOfA = meta("user", { fac_authz_a: "editor" });
    const stage = (s: Seeded) => s.w.api(editorOfA, "workItemStage", { id: s.wiId }, { stage: "planning" });
    expect(await outcome(stage(a))).toBe("ok");
    const denied = await outcome(stage(b));
    expect(denied).toMatch(/not_authorized/);
    expect(denied).toContain("requires the editor role; your role for this factory is user");
  });

  it("refuses a principal of another team and lets internal calls through", async () => {
    const a = await seed("fac_authz_a");
    const other = meta("admin", {}, "other-team");
    for (const c of calls(a)) expect(await outcome(c.call(other)), c.name).toMatch(/another team/);
    expect(await outcome(a.w.runsCancel(other, a.run.id, true))).toMatch(/another team/);
    for (const c of calls(a)) expect(await outcome(c.call(internal)), c.name).toBe("ok");
  });
});
