// Test helpers for the run MCP tools: a WorkDO with runs in any state and
// relation, built through the engine inside one transaction.
import { env } from "cloudflare:workers";
import { runInDurableObject } from "cloudflare:test";
import { doName, newToken } from "@openfactory/core";
import { createRun, createWorkItem, type Fx } from "../do/work/engine";
import type { WorkDO } from "../do/work/index";
import type { AgentConfig, Run, RunState, Stage } from "../engine/types";

export const TEAM = "default";
export const FACTORY = "fac_o";

export const agent = (name: string, type = "IMPLEMENT", extra: Partial<AgentConfig> = {}): AgentConfig => ({
  id: `agt_${name}`,
  factory_id: FACTORY,
  name,
  description: `${name} agent`,
  type,
  instructions: "",
  harness: { type: "claude" },
  network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
  spawnable_by: null,
  ...extra,
});

export function workStub(token: string): WorkDO {
  return env.WORK.getByName(doName.work(TEAM, token)) as unknown as WorkDO;
}

const built: string[] = [];

/** Delivers the outbox rows of the built WorkDOs, so that no delivery is in flight when a test ends. */
export async function settle(): Promise<void> {
  for (const token of built.splice(0)) {
    for (let i = 0; i < 3; i++) {
      await workStub(token).pump();
      await (env.DISPATCH.getByName(doName.dispatch(TEAM)) as unknown as { pump(): Promise<void> }).pump();
    }
  }
}

/** Runs fn with an engine Fx of the WorkDO of token, in one storage transaction. */
export async function withFx<T>(token: string, fn: (fx: Fx, sql: SqlStorage) => T): Promise<T> {
  return runInDurableObject(workStub(token) as unknown as DurableObjectStub, (inst: unknown, state) => {
    const make = (inst as { makeFx(w: { cmd: Set<string>; run: Set<string> }): Fx }).makeFx.bind(inst);
    const fx = make({ cmd: new Set(), run: new Set() });
    return state.storage.transactionSync(() => fn(fx, state.storage.sql));
  });
}

export interface RunSpec {
  agent?: string;
  type?: string;
  workItem?: string;
  parent?: string;
  state: RunState;
}

export interface WorkItemSpec {
  stage: Stage;
  root?: string;
}

/**
 * Builds work items and runs in one WorkDO. Names map to the created IDs.
 * Runs are created in the order of the record, so a parent comes first.
 */
export async function buildRuns(
  items: Record<string, WorkItemSpec>,
  runs: Record<string, RunSpec>,
): Promise<{ token: string; ids: Record<string, string>; name: (id: string) => string }> {
  const token = newToken("w");
  built.push(token);
  const ids = await withFx(token, (fx) => {
    const out: Record<string, string> = {};
    for (const [name, spec] of Object.entries(items)) {
      const wi = createWorkItem(fx, { factoryId: FACTORY, title: name }, "usr_1");
      wi.stage = spec.stage;
      fx.repo.putWorkItem(wi);
      out[name] = wi.id;
    }
    for (const [name, spec] of Object.entries(runs)) {
      const r = createRun(
        fx,
        {
          factoryId: FACTORY,
          ...(spec.workItem ? { workItemId: out[spec.workItem]! } : {}),
          ...(spec.parent ? { parentRunId: out[spec.parent]! } : {}),
          config: agent(spec.agent ?? name, spec.type),
          name,
          prompt: "go",
        },
        "usr_1",
      );
      out[name] = r.id;
      if (spec.state !== "queued") fx.repo.putRun(Object.assign(fx.repo.mustRun(r.id), { state: spec.state }));
    }
    for (const [name, spec] of Object.entries(items)) {
      if (!spec.root) continue;
      const wi = fx.repo.getWorkItem(out[name]!)!;
      wi.root_run_id = out[spec.root]!;
      fx.repo.putWorkItem(wi);
    }
    return out;
  });
  const back = new Map(Object.entries(ids).map(([k, v]) => [v, k]));
  return { token, ids, name: (id) => back.get(id) ?? id };
}

/** The text of the first content block of a tool result. */
export const resultText = (r: { content: { text: string }[] }): string => r.content[0]?.text ?? "";

export type { Run };
