// The actor that WorkDO records for a REST caller: "user:<id>" for a person,
// as Go's Principal.ActorID did (the dashboard names "user:" actors).
import { newRoutedId, newToken } from "@openfactory/core";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { CallMeta, Principal } from "../../contracts/do-messages";
import type { AgentConfig, Run } from "../../engine/types";
import { installFakeSandbox, workStub } from "../../../test/fakes/runner";
import { actorOfMeta } from "./api-handlers";

const TEAM = "default";
const internal: CallMeta = { requestId: "internal" };
const user: Principal = {
  kind: "user",
  id: "usr_ada",
  userId: "usr_ada",
  teamId: TEAM,
  role: "admin",
  factoryRoles: {},
};

const agent: AgentConfig = {
  id: "agt_act",
  factory_id: "fac_act",
  name: "foreman",
  description: "",
  type: "FOREMAN",
  instructions: "lead",
  harness: { type: "claude", model: "m" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
};

let uninstall: () => void;
beforeEach(() => {
  ({ uninstall } = installFakeSandbox());
});
afterEach(() => uninstall());

describe("WorkDO actors", () => {
  it.each([
    { name: "a person", p: user, want: "user:usr_ada" },
    {
      name: "a service account",
      p: { ...user, kind: "service_account", id: "usr_ci", userId: "usr_ci" } as Principal,
      want: "user:usr_ci",
    },
    {
      name: "an internal caller",
      p: { kind: "service_account", id: "system:scoring", teamId: TEAM, role: "admin", factoryRoles: {} } as Principal,
      want: "system:scoring",
    },
    { name: "no principal", p: undefined, want: "api" },
  ])("records $want for $name", ({ p, want }) => {
    expect(actorOfMeta(p ? { principal: p } : undefined)).toBe(want);
  });

  it("records user:<id> as the canceller of a run", async () => {
    const token = newToken("w");
    const w = workStub(TEAM, token);
    const out = await w.runsCreate(internal, {
      dispatch: {
        factoryId: "fac_act",
        config: agent,
        prompt: "work",
        actor: "dashboard",
        title: "Work",
        workItemId: newRoutedId("wi", token),
      },
    });
    const run = out.run as Run;
    await w.runsCancel({ requestId: "r1", principal: user }, run.id, true);
    expect((await w.getRun(run.id)).cancelled_by).toBe("user:usr_ada");
  });
});
