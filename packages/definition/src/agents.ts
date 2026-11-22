// Agent parser and resolution. Port of internal/definition/agents.go.

import { CREDENTIAL_STRATEGIES, type CredentialStrategy, mergePolicies } from "@openfactory/core";
import {
  applyHarness,
  dedupe,
  emptyExec,
  type ExecSettings,
  type HarnessOverride,
  parseExec,
  parseNetworkPolicy,
} from "./common.ts";
import { type Compiler, machineRef, sortedNames } from "./compile.ts";
import { agentSkills } from "./extras.ts";
import { goQuote, goTrimSpace } from "./gofmt.ts";
import type { AgentConfig, AgentType, HarnessConfig, MCPServer, NetworkPolicy } from "./types.ts";
import {
  type Diag,
  Doc,
  indexPath,
  isNull,
  type Mapping,
  put,
  resolve,
  splitFrontmatter,
  type YNode,
} from "./yamlnode.ts";

export const agentTypes = ["CUSTOM", "FOREMAN", "MAIN", "TRIAGE", "SPEC", "IMPLEMENT", "REVIEW", "VERIFY"];

interface AgentSpec {
  name: string;
  file: string;
  c: Doc;
  node: YNode | null;
  description: string;
  typ: AgentType;
  typeNode: YNode | null;
  credential: CredentialStrategy | "";
  snapshot: boolean | null;
  spawnSet: boolean;
  spawn: string[];
  spawnNode: YNode | null;
  np: NetworkPolicy | null;
  npNode: YNode | null;
  exec: ExecSettings;
  instructions: string;
}

export interface MarkdownDoc {
  c: Doc;
  m: Mapping | null;
  body: string;
  ok: boolean;
}

/** parseMarkdown splits a Markdown resource and parses its frontmatter. */
export function parseMarkdown(d: Diag, file: string, content: string, requireFM: boolean): MarkdownDoc {
  const fm = splitFrontmatter(content);
  const c = new Doc(d, file, fm.offset);
  if (fm.error !== null) {
    d.add(file, 1, "", fm.error);
    return { c, m: null, body: "", ok: false };
  }
  if (!fm.hasFM) {
    if (requireFM) {
      d.add(file, 1, "", "file must start with YAML frontmatter delimited by --- lines");
      return { c, m: null, body: fm.body, ok: false };
    }
    return { c, m: null, body: fm.body, ok: true };
  }
  const root = c.parse(fm.fm);
  if (root === null) return { c, m: null, body: fm.body, ok: false };
  const m = c.mapping(root, "");
  if (m === null) return { c, m: null, body: fm.body, ok: false };
  return { c, m, body: fm.body, ok: true };
}

function parseAgent(d: Diag, file: string, name: string, content: string): AgentSpec | null {
  const { c, m, body, ok } = parseMarkdown(d, file, content, false);
  if (!ok) return null;
  const a: AgentSpec = {
    name,
    file,
    c,
    node: null,
    description: "",
    typ: "CUSTOM",
    typeNode: null,
    credential: "",
    snapshot: null,
    spawnSet: false,
    spawn: [],
    spawnNode: null,
    np: null,
    npNode: null,
    exec: emptyExec(c),
    instructions: goTrimSpace(body),
  };
  if (m === null) return a;
  try {
    a.node = m.node;
    let v = m.get("description");
    if (v !== null && !isNull(v)) a.description = c.str(v, "description")[0];
    v = m.get("agentType");
    if (v !== null && !isNull(v)) {
      const [s0, good] = c.enum(v, "agentType", ...agentTypes);
      let s = s0;
      if (good) {
        if (s === "MAIN") s = "FOREMAN";
        a.typ = s as AgentType;
        a.typeNode = v;
      }
    }
    v = m.get("credentialStrategy");
    if (v !== null && !isNull(v)) a.credential = c.enum(v, "credentialStrategy", ...CREDENTIAL_STRATEGIES)[0];
    v = m.get("snapshot");
    if (v !== null && !isNull(v)) {
      const [b, good] = c.boolean(v, "snapshot");
      if (good) a.snapshot = b;
    }
    if (m.has("spawnableBy")) {
      const sv = m.get("spawnableBy");
      if (!isNull(sv)) {
        const [list, good] = c.uniqueStrList(sv, "spawnableBy");
        if (good && list !== null) {
          a.spawnSet = true;
          a.spawn = list;
          a.spawnNode = sv;
        }
      }
    }
    if (m.has("networkPolicy")) {
      a.npNode = m.get("networkPolicy");
      a.np = parseNetworkPolicy(c, a.npNode, "networkPolicy")[0];
    }
    a.exec = parseExec(c, m, false, true);
    return a;
  } finally {
    m.finish();
  }
}

export function compileAgents(c: Compiler): void {
  const t = c.t;
  if (t.agents.size === 0) {
    c.d.add("agents", 0, "", "a definition must include at least one agents/<name>/agent.md");
    return;
  }
  const specs: AgentSpec[] = [];
  for (const name of sortedNames(t.agents)) {
    const p = t.agents.get(name)!;
    const a = parseAgent(c.d, p, name, t.files.get(p) ?? "");
    if (a !== null) specs.push(a);
  }
  const foremen = specs.filter((a) => a.typ === "FOREMAN");
  if (foremen.length === 0 && specs.length === t.agents.size) {
    c.d.add("agents", 0, "agentType", "exactly one agent must have agentType FOREMAN (alias MAIN); none does");
  } else if (foremen.length > 1) {
    const names = foremen.map((a) => a.name);
    for (const a of foremen.slice(1))
      a.c.errf(a.typeNode, "agentType", `exactly one agent can be the foreman; found ${names.join(", ")}`);
  }
  if (foremen.length >= 1) c.foreman = foremen[0]!.name;

  for (const a of specs) c.agents.set(a.name, resolveAgent(c, a));
  for (const a of specs) {
    if (!a.spawnSet) continue;
    const items = resolve(a.spawnNode)!.content;
    a.spawn.forEach((n, i) => {
      if (!t.agents.has(n))
        a.c.errf(items[i], indexPath("spawnableBy", i), `spawnableBy names unknown agent ${goQuote(n)}`);
    });
  }
  for (const n of sortedNames(c.agents)) c.def.agents!.push(c.agents.get(n)!);
}

function resolveAgent(c: Compiler, a: AgentSpec): AgentConfig {
  const f = c.f;
  const e = a.exec;
  const dc = a.c;
  const cfg: AgentConfig = {
    id: "",
    factory_id: "",
    name: a.name,
    description: a.description,
    type: a.typ,
    instructions: a.instructions,
    harness: { type: "", model: "" },
    network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
    spawnable_by: null,
    credential_strategy: f.def.credentialStrategy ?? "",
    idle_timeout_minutes: 0,
  };
  if (a.credential !== "") cfg.credential_strategy = a.credential;
  if (f.harnessOK) cfg.harness = applyHarness(f.defaultHarness, e.model, e.harness);
  cfg.worker_host = f.defaults.worker;
  if (e.workerSet) cfg.worker_host = e.worker;
  checkEffectiveHarness(dc, "", cfg.harness, e.harness, cfg.worker_host, e.model !== null || e.harness !== null);

  let machine = f.defaults.machine;
  let machineDoc = f.c;
  let machineNode = f.defaults.machineNode;
  let machineField = "agentDefaults.machine";
  if (e.machine !== null) {
    machine = e.machine;
    machineDoc = dc;
    machineNode = e.machineNode;
    machineField = "machine";
  }
  if (machine !== null) {
    const spec = machineRef(c, machineDoc, machine, machineField, machineDoc.line(machineNode));
    if (spec !== null) cfg.machine = spec;
  }
  cfg.environment_id = f.defaults.env;
  if (e.envSet) cfg.environment_id = e.env;
  const base = e.secretsSet ? e.secrets : f.defaults.secrets;
  const secrets = dedupe(base, f.def.secrets);
  if (secrets.length > 0) cfg.secrets = secrets;
  let mcpBase = f.defaults.mcp;
  if (e.mcpSet) {
    mcpBase = e.mcp;
    checkMCPConflicts(c, dc, "mcpServers", e.mcpNode, e.mcp);
  }
  const mcp = mergeMCP(mcpBase, f.mcp);
  if (mcp !== null) cfg.mcp_servers = mcp;
  const np = mergePolicies(f.networkPolicy, a.np);
  if (np !== null) cfg.network_policy = np;
  if (cfg.network_policy.allowed_hosts === null) cfg.network_policy.allowed_hosts = [];
  if (cfg.network_policy.denied_hosts === null) cfg.network_policy.denied_hosts = [];
  if (e.idle !== null) {
    cfg.idle_timeout_minutes = e.idle;
    cfg.idle_timeout_set = true;
  }
  if (a.snapshot !== null) cfg.snapshot = a.snapshot;
  if (a.spawnSet) cfg.spawnable_by = [...a.spawn];
  const skills = agentSkills(c, a.name);
  if (skills.length > 0) cfg.skills = skills;
  return cfg;
}

function kindOf(prefix: string): string {
  if (prefix === "") return "file";
  return prefix.endsWith(".") ? prefix.slice(0, -1) : prefix;
}

/** checkEffectiveHarness checks that workerEnvironment auth has a worker host. */
export function checkEffectiveHarness(
  dc: Doc,
  prefix: string,
  h: HarnessConfig,
  ov: HarnessOverride | null,
  workerHost: string,
  overridden: boolean,
): void {
  if (!overridden && ov === null) {
    if (h.auth && h.auth.source === "workerEnvironment" && workerHost === "") {
      dc.errf(
        null,
        prefix + "workerHost",
        `the inherited harness uses auth.source workerEnvironment, which requires a self-hosted workerHost; this ${kindOf(prefix)} clears it`,
      );
    }
    return;
  }
  let node: YNode | null = ov !== null ? ov.node : null;
  if (h.auth && h.auth.source === "workerEnvironment" && workerHost === "") {
    if (ov !== null && ov.authNode !== null) node = ov.authNode;
    dc.errf(node, prefix + "harness.auth.source", "auth.source workerEnvironment requires a self-hosted workerHost");
  }
}

export function checkMCPConflicts(
  c: Compiler,
  dc: Doc,
  field: string,
  node: YNode | null,
  m: Record<string, MCPServer> | null,
): void {
  for (const name of Object.keys(m ?? {})) {
    if (c.f.mcp !== null && Object.hasOwn(c.f.mcp, name)) {
      dc.errf(
        node,
        field + "." + name,
        `MCP server ${goQuote(name)} is already granted factory-wide by factory.yaml mcpServers`,
      );
    }
  }
}

export function mergeMCP(
  base: Record<string, MCPServer> | null,
  factory: Record<string, MCPServer> | null,
): Record<string, MCPServer> | null {
  const nb = base === null ? 0 : Object.keys(base).length;
  const nf = factory === null ? 0 : Object.keys(factory).length;
  if (nb === 0 && nf === 0) return null;
  const out: Record<string, MCPServer> = {};
  for (const [k, v] of Object.entries(base ?? {})) put(out, k, v);
  for (const [k, v] of Object.entries(factory ?? {})) if (!Object.hasOwn(out, k)) put(out, k, v);
  return out;
}
