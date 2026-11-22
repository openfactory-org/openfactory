// WorkDO.api metricsSessions: the sessions of runs for the metrics, only of
// the factory that the body names, authorized on that factory, and capped.
import { newRoutedId, newToken } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { CallMeta, Principal, Role } from "../../contracts/do-messages";
import type { AgentConfig, Run } from "../../engine/types";
import { installFakeSandbox, pump, workStub } from "../../../test/fakes/runner";
import { MAX_METRICS_RUNS } from "./api-handlers";

const TEAM = "default";
const internal: CallMeta = { requestId: "internal" };

function meta(factoryRoles: Record<string, Role>): CallMeta {
  const principal: Principal = { kind: "user", id: "u1", userId: "u1", teamId: TEAM, role: "" as Role, factoryRoles };
  return { requestId: "r1", principal };
}

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

/** "ok", or the error text of a rejected RPC call. */
const outcome = (p: Promise<unknown>): Promise<string> =>
  p.then(
    () => "ok",
    (e: unknown) => String(e),
  );

let uninstall: () => void;
beforeEach(() => {
  ({ uninstall } = installFakeSandbox());
});
afterEach(() => uninstall());

describe("WorkDO metricsSessions", () => {
  it("returns the sessions of the named factory's runs only, and caps the run IDs", async () => {
    const token = newToken("w");
    const w = workStub(TEAM, token);
    const create = async (factoryId: string): Promise<Run> =>
      (
        await w.runsCreate(internal, {
          dispatch: {
            factoryId,
            config: agent(factoryId),
            prompt: "work",
            actor: "usr_dana",
            title: "Work",
            workItemId: newRoutedId("wi", token),
          },
        })
      ).run as Run;
    const a = await create("fac_ms_a");
    const b = await create("fac_ms_b");
    await pump(TEAM, token);

    const call = (m: CallMeta, body: unknown) => w.api(m, "metricsSessions", {}, body);
    const all = (await call(internal, { factoryId: "fac_ms_b", runIds: [b.id] })) as {
      sessions: Record<string, unknown[]>;
    };
    expect(all.sessions[b.id]?.length ?? 0).toBeGreaterThan(0);

    const onlyA = meta({ fac_ms_a: "user" });
    const got = (await call(onlyA, { factoryId: "fac_ms_a", runIds: [a.id, b.id, "run_missing"] })) as {
      sessions: Record<string, { assigned_at?: string }[]>;
    };
    expect(Object.keys(got.sessions)).toEqual([a.id]);
    expect(got.sessions[a.id]![0]!.assigned_at).toBeTruthy();

    // The factory of the body authorizes the call.
    expect(await outcome(call(onlyA, { factoryId: "fac_ms_b", runIds: [b.id] }))).toMatch(/not_authorized/);
    expect(await outcome(call(onlyA, { runIds: [a.id] }))).not.toBe("ok");
    const tooMany = Array.from({ length: MAX_METRICS_RUNS + 1 }, () => a.id);
    expect(await outcome(call(onlyA, { factoryId: "fac_ms_a", runIds: tooMany }))).toMatch(/invalid_request/);
    const atCap = Array.from({ length: MAX_METRICS_RUNS }, () => a.id);
    expect(Object.keys(((await call(onlyA, { factoryId: "fac_ms_a", runIds: atCap })) as typeof got).sessions)).toEqual(
      [a.id],
    );
  });
});
