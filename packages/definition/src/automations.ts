// Automation parser. Port of internal/definition/automations.go.

import { checkEffectiveHarness, checkMCPConflicts, mergeMCP, parseMarkdown } from "./agents.ts";
import { applyHarness, dedupe, type ExecSettings, parseExec } from "./common.ts";
import { type Compiler, machineRef, sortedNames } from "./compile.ts";
import { goQuote, goTrimSpace } from "./gofmt.ts";
import { parseTrigger, type TriggerEnv } from "./triggers.ts";
import type { AgentConfig, AgentOverrides, Automation } from "./types.ts";
import { type Doc, indexPath, isNull, isSeqNode } from "./yamlnode.ts";

export const RUN_AS_FACTORY = "factory";

/** parseRunAs checks a runAs value: factory, user:<email> or serviceAccount:<name>. */
export function parseRunAs(value: string): { type: "factory" | "user" | "service_account"; name: string } | null {
  const v = goTrimSpace(value);
  if (v === "" || v === RUN_AS_FACTORY) return { type: "factory", name: "" };
  if (v.startsWith("user:")) {
    const email = goTrimSpace(v.slice("user:".length));
    if (email === "" || !email.includes("@")) return null;
    return { type: "user", name: email };
  }
  if (v.startsWith("serviceAccount:")) {
    const name = goTrimSpace(v.slice("serviceAccount:".length));
    if (name === "") return null;
    return { type: "service_account", name };
  }
  return null;
}

export function compileAutomations(c: Compiler): void {
  const env: TriggerEnv = { integrationsSet: c.def.integrationsSet, integrations: new Set(), webhooks: new Set() };
  for (const i of c.def.integrations ?? []) env.integrations.add(i.type);
  for (const name of c.t.webhooks.keys()) env.webhooks.add(name);
  for (const name of sortedNames(c.t.automations)) {
    const p = c.t.automations.get(name)!;
    const [a, ok] = parseAutomation(c, p, name, c.t.files.get(p) ?? "", env);
    if (ok) c.def.automations!.push(a);
  }
}

function parseAutomation(
  c: Compiler,
  file: string,
  name: string,
  content: string,
  env: TriggerEnv,
): [Automation, boolean] {
  const before = c.d.errs.length;
  const a: Automation = { id: "", factory_id: "", name, enabled: true, agent: "", prompt: "", triggers: null };
  const { c: dc, m, body, ok } = parseMarkdown(c.d, file, content, true);
  if (!ok || m === null) return [a, false];
  try {
    a.prompt = goTrimSpace(body);
    if (a.prompt === "") dc.errf(null, "", "the automation's Markdown body (its starting prompt) must not be empty");
    let v = m.get("enabled");
    if (v !== null && !isNull(v)) {
      const [b, good] = dc.boolean(v, "enabled");
      if (good) a.enabled = b;
    }
    v = m.get("displayName");
    if (v !== null && !isNull(v)) a.display_name = dc.str(v, "displayName")[0];
    v = m.get("runAs");
    if (v !== null && !isNull(v)) {
      const [s, good] = dc.nonEmptyStr(v, "runAs");
      if (good) {
        if (parseRunAs(s) === null) dc.errf(v, "runAs", "runAs must be factory, user:<email> or serviceAccount:<name>");
        else if (s !== RUN_AS_FACTORY) a.run_as = s;
      }
    }
    a.agent = c.foreman;
    v = m.get("agent");
    if (v !== null && !isNull(v)) {
      const [s, good] = dc.nonEmptyStr(v, "agent");
      if (good) {
        a.agent = s;
        if (!c.t.agents.has(s)) dc.errf(v, "agent", `agent ${goQuote(s)} does not match any agents/<name>/agent.md`);
      }
    }

    const tv = m.require("triggers");
    if (tv !== null) {
      const items = dc.seq(tv, "triggers") ?? [];
      if (isSeqNode(tv) && items.length === 0) dc.errf(tv, "triggers", "triggers must list at least one trigger");
      let unnamed = 0;
      const names = new Set<string>();
      items.forEach((it, i) => {
        const p = indexPath("triggers", i);
        const [t, good] = parseTrigger(dc, it, p, env);
        if (!good) return;
        if (t.schedule) {
          if ((t.schedule.name ?? "") === "") {
            unnamed++;
            if (unnamed > 1)
              dc.errf(it, p + ".schedule.name", "at most one schedule trigger in an automation can omit schedule.name");
          } else if (names.has(t.schedule.name!)) {
            dc.errf(
              it,
              p + ".schedule.name",
              `schedule name ${goQuote(t.schedule.name!)} is used more than once in this automation`,
            );
          } else {
            names.add(t.schedule.name!);
          }
        }
        (a.triggers ??= []).push(t);
      });
    }

    const e = parseExec(dc, m, false, true);
    const o = automationOverrides(c, dc, a.agent, e);
    if (o !== null) a.overrides = o;
    // Like the Go code, unknown keys reported by finish do not count here.
    return [a, c.d.errs.length === before];
  } finally {
    m.finish();
  }
}

/**
 * automationOverrides resolves an automation's execution settings against
 * the target agent. Harness, secrets and MCP servers hold complete effective
 * values (factory-wide entries included); model stays empty.
 */
function automationOverrides(c: Compiler, dc: Doc, agentName: string, e: ExecSettings): AgentOverrides | null {
  const set =
    e.model !== null ||
    e.harness !== null ||
    e.machine !== null ||
    e.envSet ||
    e.secretsSet ||
    e.mcpSet ||
    e.workerSet ||
    e.idle !== null;
  if (!set) return null;
  const o: AgentOverrides = { secrets: null, mcp_servers: null };
  const agent: AgentConfig | undefined = c.agents.get(agentName);
  const baseHarness = agent?.harness ?? { type: "", model: "" };
  let worker = agent?.worker_host ?? "";
  if (e.workerSet) {
    o.worker_host = e.worker;
    worker = e.worker;
  }
  if (e.model !== null || e.harness !== null) {
    if (agent && c.f.harnessOK) {
      const h = applyHarness(baseHarness, e.model, e.harness);
      o.harness = h;
      checkEffectiveHarness(dc, "", h, e.harness, worker, true);
    }
  } else if (agent && e.workerSet) {
    checkEffectiveHarness(dc, "", baseHarness, null, worker, false);
  }
  if (e.machine !== null) {
    machineRef(c, dc, e.machine, "machine", dc.line(e.machineNode));
    o.machine = e.machine;
  }
  if (e.envSet) o.environment_id = e.env;
  if (e.secretsSet) o.secrets = dedupe(e.secrets, c.def.secrets);
  if (e.mcpSet) {
    checkMCPConflicts(c, dc, "mcpServers", e.mcpNode, e.mcp);
    o.mcp_servers = mergeMCP(e.mcp, c.f.mcp) ?? {};
  }
  if (e.idle !== null) o.idle_timeout_minutes = e.idle;
  return o;
}
