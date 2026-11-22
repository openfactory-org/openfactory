// ApplyOverrides and the judge/dispatch configuration: port of
// internal/engine/overrides.go and the config part of internal/engine/judge.go.
// WS5 owns the engine; this copy exists so that benchmark trials and judge
// runs can be started through WorkDO.runsCreate until the engine exports its
// own dispatch preparation (see the report).
import { JUDGE_SKILL_NAME, OfError, type AgentConfig, type MachineSpec, type MCPServer } from "@openfactory/core";
import type { JudgeRunRequest, Overrides } from "./types";

/** The part of a factory's runtime that dispatch needs (TeamRpc.configSnapshot). */
export interface FactoryRuntimeLike {
  agents: AgentConfig[];
  machines?: Record<string, MachineSpec> | null;
  /** Factory-wide secrets and MCP servers. They apply to every run. */
  secrets?: string[] | null;
  mcp_servers?: Record<string, MCPServer> | null;
}

export function cloneAgentConfig(c: AgentConfig): AgentConfig {
  return structuredClone(c);
}

/** Applies sparse execution overrides to an agent configuration. */
export function applyOverrides(
  cfg: AgentConfig,
  ov: Overrides | null | undefined,
  machines: Record<string, MachineSpec> | null | undefined,
): AgentConfig {
  const out = cloneAgentConfig(cfg);
  if (!ov) return out;
  if (ov.harness && ov.model) throw new OfError("invalid_request", "set at most one of model and harness");
  const h = ov.harness;
  if (h && h.type) {
    // A harness with a type is complete: it replaces the harness of the agent as a whole.
    out.harness = structuredClone(h);
  } else if (h) {
    if (h.model) out.harness.model = h.model;
    if (h.reasoningLevel) out.harness.reasoningLevel = h.reasoningLevel;
    if (h.auth) out.harness.auth = { ...h.auth };
  }
  if (ov.model) out.harness.model = ov.model;
  if (ov.machine) {
    const spec = machines?.[ov.machine];
    if (!spec) throw new OfError("invalid_request", `unknown machine ${JSON.stringify(ov.machine)}`);
    const m: MachineSpec = structuredClone(spec);
    if (!m.name) m.name = ov.machine;
    out.machine = m;
  }
  if (ov.environment_id !== undefined && ov.environment_id !== null) out.environment_id = ov.environment_id;
  // Secrets and MCP servers add to the lists of the agent.
  for (const name of ov.secrets ?? []) {
    out.secrets ??= [];
    if (!out.secrets.includes(name)) out.secrets.push(name);
  }
  if (ov.mcp_servers && Object.keys(ov.mcp_servers).length > 0) {
    out.mcp_servers ??= {};
    for (const [k, v] of Object.entries(ov.mcp_servers)) out.mcp_servers[k] = structuredClone(v);
  }
  if (ov.worker_host !== undefined && ov.worker_host !== null) out.worker_host = ov.worker_host;
  if (ov.idle_timeout_minutes !== undefined && ov.idle_timeout_minutes !== null) {
    if (ov.idle_timeout_minutes < 0 || ov.idle_timeout_minutes > 60)
      throw new OfError("invalid_request", "idle_timeout_minutes must be between 0 and 60");
    out.idle_timeout_minutes = ov.idle_timeout_minutes;
    out.idle_timeout_set = true;
  }
  return out;
}

/** Adds the factory-wide secrets and MCP servers to cfg. */
export function withFactoryWide(
  cfg: AgentConfig,
  rt: Pick<FactoryRuntimeLike, "secrets" | "mcp_servers"> | null | undefined,
): AgentConfig {
  if (!rt) return cfg;
  for (const s of rt.secrets ?? []) {
    cfg.secrets ??= [];
    if (!cfg.secrets.includes(s)) cfg.secrets.push(s);
  }
  for (const [k, v] of Object.entries(rt.mcp_servers ?? {})) {
    if (cfg.mcp_servers && k in cfg.mcp_servers) continue;
    cfg.mcp_servers ??= {};
    cfg.mcp_servers[k] = v;
  }
  return cfg;
}

/** The agent of a dispatch: the named agent, else the foreman. */
export function resolveAgent(rt: FactoryRuntimeLike, name: string | undefined): AgentConfig {
  const agents = rt.agents ?? [];
  const a = name ? agents.find((x) => x.name === name) : agents.find((x) => x.type === "FOREMAN");
  if (!a)
    throw new OfError(
      "invalid_request",
      name ? `agent ${JSON.stringify(name)} does not exist` : "the factory has no foreman agent",
    );
  return a;
}

async function stableId(prefix: string, factoryId: string, name: string): Promise<string> {
  const d = new Uint8Array(
    await crypto.subtle.digest("SHA-256", new TextEncoder().encode(`${factoryId}\x00${prefix}\x00${name}`)),
  );
  return `${prefix}_${[...d]
    .map((x) => x.toString(16).padStart(2, "0"))
    .join("")
    .slice(0, 20)}`;
}

function judgeSkillContent(files: Record<string, string>): string {
  let b = `---\nname: ${JUDGE_SKILL_NAME}\ndescription: The evidence of the run that you score.\n---\n\n`;
  b += "This skill holds the evidence of the run that you score. Read the files before you decide.\n\n";
  for (const p of Object.keys(files)) b += `- \`${p}\`\n`;
  return b;
}

/**
 * The agent configuration of a judge run (StartJudgeRun): harness, machine,
 * secrets and MCP servers of the request, the files as a skill, and the
 * network policy, environment and worker host of the foreman.
 */
export async function judgeConfig(rt: FactoryRuntimeLike, req: JudgeRunRequest): Promise<AgentConfig> {
  if (!req.factoryId) throw new OfError("invalid_request", "factory id is required");
  if (req.prompt.trim() === "") throw new OfError("invalid_request", "prompt is required");
  if (!req.harness.type || !req.harness.model) {
    throw new OfError(
      "invalid_request",
      "the judge needs a harness with a type and a model (set harness or model on the scorer, scorerDefaults or agentDefaults)",
    );
  }
  const name = req.name || "scorer";
  const cfg = {
    id: await stableId("judge", req.factoryId, name),
    factory_id: req.factoryId,
    name,
    description: "The judge of a scorer.",
    type: "CUSTOM",
    instructions: req.instructions,
    harness: structuredClone(req.harness),
    secrets: [...(req.secrets ?? [])],
    idle_timeout_minutes: 0,
    idle_timeout_set: true,
    spawnable_by: [],
  } as unknown as AgentConfig;
  if (req.mcpServers && Object.keys(req.mcpServers).length > 0) cfg.mcp_servers = structuredClone(req.mcpServers);
  const foreman = (rt.agents ?? []).find((a) => a.type === "FOREMAN");
  if (foreman) {
    cfg.network_policy = structuredClone(foreman.network_policy);
    if (foreman.environment_id) cfg.environment_id = foreman.environment_id;
    if (foreman.worker_host) cfg.worker_host = foreman.worker_host;
    if (foreman.machine) cfg.machine = structuredClone(foreman.machine);
    if (foreman.credential_strategy) cfg.credential_strategy = foreman.credential_strategy;
  }
  if (req.machine) {
    const spec = rt.machines?.[req.machine];
    if (!spec)
      throw new OfError("invalid_request", `machine ${JSON.stringify(req.machine)} is not defined in the factory`);
    const m: MachineSpec = structuredClone(spec);
    if (!m.name) m.name = req.machine;
    cfg.machine = m;
  }
  if (Object.keys(req.files).length > 0) {
    cfg.skills = [
      {
        name: JUDGE_SKILL_NAME,
        description: "The evidence of the run that you score.",
        path: `skills/${JUDGE_SKILL_NAME}/SKILL.md`,
        content: judgeSkillContent(req.files),
        files: { ...req.files },
        scope: name,
      } as unknown as NonNullable<AgentConfig["skills"]>[number],
    ];
  }
  return withFactoryWide(cfg, rt);
}
