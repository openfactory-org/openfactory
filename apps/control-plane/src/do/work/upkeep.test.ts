// WorkDO data upkeep (WS13 hooks): compaction of a finished run's messages to
// R2, message reads that merge the compacted object, retention without a
// retention period, and the agent pins of a trial.
import { env } from "cloudflare:workers";
import { doName, newToken } from "@openfactory/core";
import { describe, expect, it } from "vitest";
import { compactedKey } from "../../compaction/compact";
import { readRunMessages } from "../../api/runs";
import type { AgentConfig, Run } from "../../engine/types";
import type { WorkDO } from "./index";

const TEAM = "default";
const meta = { requestId: "t" };

const foreman: AgentConfig = {
  id: "agt_up",
  factory_id: "fac_up",
  name: "foreman",
  description: "",
  type: "FOREMAN",
  instructions: "lead",
  harness: { type: "claude", model: "m" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
};

function work(token: string) {
  return env.WORK.getByName(doName.work(TEAM, token)) as unknown as WorkDO;
}

describe("WorkDO upkeep", () => {
  it("compactRun moves the messages of a finished run to R2; reads merge them", async () => {
    const token = newToken("w");
    const w = work(token);
    const { run } = await w.runsCreate(meta, {
      run: { factoryId: "fac_up", config: foreman, prompt: "first", actor: "api" },
    } as never);
    await w.runsMessage(meta, run.id, { message: "second", actor: "usr_a" });
    // A live run is not compacted.
    expect(await w.compactRun(run.id)).toEqual({ more: false });
    expect(await env.DATA.head(compactedKey(run.id))).toBeNull();
    await w.runsCancel(meta, run.id, true);
    const before = await readRunMessages(env, w, meta, run.id, 0);
    expect(before.length).toBeGreaterThan(0);

    expect(await w.compactRun(run.id)).toEqual({ more: false });
    expect(await env.DATA.head(compactedKey(run.id))).not.toBeNull();
    const rows = (await w.api(meta, "listRunMessages", { runId: run.id }, null)) as {
      messages: unknown[];
      compacted_upto?: number;
    };
    expect(rows.messages).toHaveLength(0);
    expect(rows.compacted_upto).toBe(before.at(-1)!.seq);
    const after = await readRunMessages(env, w, meta, run.id, 0);
    expect(after.map((m) => [m.seq, m.content])).toEqual(before.map((m) => [m.seq, m.content]));
    expect((await readRunMessages(env, w, meta, run.id, before[0]!.seq)).map((m) => m.seq)).toEqual(
      before.slice(1).map((m) => m.seq),
    );
    // Idempotent.
    expect(await w.compactRun(run.id)).toEqual({ more: false });
  });

  it("retentionSweep keeps everything without OF_OBJECT_STORE_RETENTION", async () => {
    expect(await work(newToken("w")).retentionSweep()).toEqual({ more: false });
  });

  it("stores the agent pins of a dispatch for run_agents", async () => {
    const token = newToken("w");
    const w = work(token);
    const out = (await w.runsCreate(meta, {
      dispatch: {
        factoryId: "fac_up",
        config: foreman,
        prompt: "trial",
        actor: "bench",
        title: "trial",
        agentPins: { coder: { model: "pinned" } },
      },
    } as never)) as { run: Run; workItemId: string };
    const pins = (await w.api(meta, "getAgentPins", { id: out.workItemId }, null)) as { pins: Record<string, unknown> };
    expect(pins.pins).toEqual({ coder: { model: "pinned" } });
  });
});
