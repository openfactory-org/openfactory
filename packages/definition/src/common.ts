// Shared parsers: harness, auth, secrets, MCP servers, network policy and
// execution settings. Port of internal/definition/common.go.

import { goQuote, goTrimSpace } from "./gofmt.ts";
import { goParseURL } from "./gourl.ts";
import { harnessForModel } from "./harness-models.ts";
import { normalizeHost, resolvePolicy } from "./networkpolicy.ts";
import type { HarnessAuth, HarnessConfig, HarnessType, MCPServer, NetworkPolicy } from "./types.ts";
import { type Doc, indexPath, isNull, type Mapping, put, resolve, type YNode } from "./yamlnode.ts";

/** SCHEMA_VERSION is the current definition schema version. */
export const SCHEMA_VERSION = "v1";

/** RESERVED_MCP_SERVER_NAME is the orchestration server, injected into every run. */
export const RESERVED_MCP_SERVER_NAME = "factory";

/** BUILTIN_MCP_PREFIX is the managedId namespace of the servers that the control plane serves. */
export const BUILTIN_MCP_PREFIX = "builtin/";

/** BUILTIN_MCP_PROVIDERS are the integrations with a built-in MCP server. */
export const BUILTIN_MCP_PROVIDERS = ["jira", "linear", "slack", "teams"];

export const resourceNameRe = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
export const secretNameRe = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
export const aliasRe = /^[A-Za-z0-9 ._-]{1,60}$/;
export const reasoningRe = /^[a-z][a-z0-9_-]{0,31}$/;
export const mcpServerNameRe = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
// RE2 \s is ASCII white space.
export const emailRe = /^[^@\t\n\f\r ]+@[^@\t\n\f\r ]+\.[^@\t\n\f\r ]+$/;

/** builtinMCPProvider returns the provider of a builtin/<provider> managedId and whether it is known. */
export function builtinMCPProvider(managedId: string): { provider: string; known: boolean } | null {
  if (!managedId.startsWith(BUILTIN_MCP_PREFIX)) return null;
  const p = managedId.slice(BUILTIN_MCP_PREFIX.length);
  return { provider: p, known: BUILTIN_MCP_PROVIDERS.includes(p) };
}

function normalizeHarnessType(s: string): HarnessType | null {
  switch (s) {
    case "claude":
    case "claude-code":
      return "claude";
    case "codex":
      return "codex";
  }
  return null;
}

/** HarnessOverride is a parsed harness object: complete for agentDefaults, sparse elsewhere. */
export interface HarnessOverride {
  c: Doc;
  node: YNode;
  type: HarnessType | null;
  model: string | null;
  reasoning: string | null;
  authSet: boolean;
  /** null with authSet: clear the inherited auth. */
  auth: HarnessAuth | null;
  authNode: YNode | null;
  path: string;
}

export function parseHarness(c: Doc, n: YNode | null, path: string, full: boolean): HarnessOverride | null {
  const m = c.mapping(n, path);
  if (m === null) return null;
  try {
    const h: HarnessOverride = {
      c,
      node: m.node,
      type: null,
      model: null,
      reasoning: null,
      authSet: false,
      auth: null,
      authNode: null,
      path,
    };
    if (full) {
      m.require("type");
      m.require("model");
    }
    let v = m.get("type");
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.str(v, m.field("type"));
      if (ok) {
        const t = normalizeHarnessType(s);
        if (t !== null) h.type = t;
        else
          c.errf(
            v,
            m.field("type"),
            `${m.field("type")} must be one of claude, codex, claude-code (got ${goQuote(s)})`,
          );
      }
    }
    v = m.get("model");
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.nonEmptyStr(v, m.field("model"));
      if (ok) h.model = s;
    }
    v = m.get("reasoningLevel");
    if (v !== null && !isNull(v)) {
      const [s, ok] = c.str(v, m.field("reasoningLevel"));
      if (ok) {
        if (!reasoningRe.test(s)) {
          c.errf(
            v,
            m.field("reasoningLevel"),
            `${m.field("reasoningLevel")} ${goQuote(s)} is not a valid reasoning level (for example low, medium, high)`,
          );
        } else {
          h.reasoning = s;
        }
      }
    }
    if (m.has("auth")) {
      const av = m.get("auth");
      h.authSet = true;
      h.authNode = av;
      if (!isNull(av)) {
        h.auth = parseAuth(c, av, m.field("auth"));
        if (h.auth === null) h.authSet = false;
      }
    }
    return h;
  } finally {
    m.finish();
  }
}

export function parseAuth(c: Doc, n: YNode | null, path: string): HarnessAuth | null {
  const m = c.mapping(n, path);
  if (m === null) return null;
  try {
    const [src, ok] = c.enum(m.require("source"), m.field("source"), "managedSecret", "workerEnvironment");
    if (!ok) {
      m.get("secretName");
      return null;
    }
    const a: HarnessAuth = { source: src };
    const sn = m.get("secretName");
    if (src === "managedSecret") {
      if (sn === null || isNull(sn)) {
        c.errf(m.node, m.field("secretName"), `${m.field("secretName")} is required when auth.source is managedSecret`);
        return null;
      }
      const [s, good] = secretName(c, sn, m.field("secretName"));
      if (!good) return null;
      a.secretName = s;
    } else if (sn !== null && !isNull(sn)) {
      c.errf(
        sn,
        m.field("secretName"),
        `${m.field("secretName")} is not allowed when auth.source is workerEnvironment`,
      );
      return null;
    }
    return a;
  } finally {
    m.finish();
  }
}

export function secretName(c: Doc, n: YNode | null, path: string): [string, boolean] {
  const [s, ok] = c.nonEmptyStr(n, path);
  if (!ok) return ["", false];
  if (!secretNameRe.test(s)) {
    c.errf(
      n,
      path,
      `${path} ${goQuote(s)} is not a valid secret name (letters, digits and _, not starting with a digit)`,
    );
    return ["", false];
  }
  return [s, true];
}

export function secretList(c: Doc, n: YNode | null, path: string): [string[] | null, boolean] {
  const [list, ok0] = c.uniqueStrList(n, path);
  if (!ok0 || list === null) return [list, false];
  let ok = true;
  const items = resolve(n)!.content;
  list.forEach((s, i) => {
    if (!secretNameRe.test(s)) {
      c.errf(
        items[i],
        indexPath(path, i),
        `${goQuote(s)} is not a valid secret name (letters, digits and _, not starting with a digit)`,
      );
      ok = false;
    }
  });
  return [list, ok];
}

/** parseMCPServers decodes an mcpServers map. */
export function parseMCPServers(c: Doc, n: YNode | null, path: string): [Record<string, MCPServer> | null, boolean] {
  const m = c.mapping(n, path);
  if (m === null) return [null, false];
  const out: Record<string, MCPServer> = {};
  let ok = true;
  for (const name of m.order) {
    const v = m.get(name);
    const f = m.field(name);
    if (!mcpServerNameRe.test(name)) {
      c.errf(m.keyNode(name), f, `MCP server name ${goQuote(name)} must be 1-64 letters, digits, '.', '_' or '-'`);
      ok = false;
      continue;
    }
    if (name === RESERVED_MCP_SERVER_NAME) {
      c.errf(m.keyNode(name), f, `MCP server name ${goQuote(name)} is reserved for the orchestration server`);
      ok = false;
      continue;
    }
    const [srv, good] = parseMCPServer(c, v, f);
    if (!good) {
      ok = false;
      continue;
    }
    put(out, name, srv);
  }
  return [out, ok];
}

function parseMCPServer(c: Doc, n: YNode | null, path: string): [MCPServer, boolean] {
  const s: MCPServer = {};
  const m = c.mapping(n, path);
  if (m === null) return [s, false];
  try {
    let ok = true;
    let transports = 0;
    for (const k of ["managedId", "command", "url"]) if (m.present(k)) transports++;
    if (transports !== 1) {
      c.errf(m.node, path, `${path} must set exactly one of managedId, command or url`);
      ok = false;
    }
    let managedId = "";
    let v = m.get("managedId");
    if (v !== null && !isNull(v)) {
      managedId = c.nonEmptyStr(v, m.field("managedId"))[0];
      s.managedId = managedId;
      const b = builtinMCPProvider(managedId);
      if (b !== null && !b.known) {
        c.errf(
          v,
          m.field("managedId"),
          `${m.field("managedId")}: unknown built-in MCP server ${goQuote(managedId)} (known: builtin/${BUILTIN_MCP_PROVIDERS.join(", builtin/")})`,
        );
        ok = false;
      }
    }
    v = m.get("allowWrite");
    if (v !== null && !isNull(v)) {
      const [b, good] = c.boolean(v, m.field("allowWrite"));
      if (good) s.allowWrite = b;
      else ok = false;
      if (!managedId.startsWith(BUILTIN_MCP_PREFIX)) {
        c.errf(
          v,
          m.field("allowWrite"),
          `${m.field("allowWrite")} is only valid with a built-in managedId (builtin/<provider>)`,
        );
        ok = false;
      }
    }
    let command = "";
    v = m.get("command");
    if (v !== null && !isNull(v)) {
      command = c.nonEmptyStr(v, m.field("command"))[0];
      s.command = command;
    }
    v = m.get("url");
    if (v !== null && !isNull(v)) {
      const [raw, good] = c.nonEmptyStr(v, m.field("url"));
      if (good) {
        const u = goParseURL(raw);
        if (u === null || (u.scheme !== "http" && u.scheme !== "https") || u.host === "") {
          c.errf(v, m.field("url"), `${m.field("url")} must be an absolute http or https URL`);
          ok = false;
        } else {
          s.url = raw;
        }
      }
    }
    v = m.get("args");
    if (v !== null && !isNull(v)) {
      if (command === "" && !m.present("command")) {
        c.errf(v, m.field("args"), `${m.field("args")} is only valid with command`);
        ok = false;
      }
      const items = c.seq(v, m.field("args")) ?? [];
      items.forEach((it, i) => {
        const [a, good] = c.str(it, indexPath(m.field("args"), i));
        if (good) (s.args ??= []).push(a);
        else ok = false;
      });
    }
    v = m.get("env");
    if (v !== null && !isNull(v)) {
      if (!m.present("command")) {
        c.errf(v, m.field("env"), `${m.field("env")} is only valid with command`);
        ok = false;
      }
      const [env, good] = c.strMap(v, m.field("env"));
      ok = ok && good;
      if (env !== null) s.env = env;
    }
    v = m.get("headers");
    if (v !== null && !isNull(v)) {
      if (!m.present("url")) {
        c.errf(v, m.field("headers"), `${m.field("headers")} is only valid with url`);
        ok = false;
      }
      const [h, good] = c.strMap(v, m.field("headers"));
      ok = ok && good;
      if (h !== null) s.headers = h;
    }
    return [s, ok];
  } finally {
    m.finish();
  }
}

/** parseNetworkPolicy decodes a networkPolicy object; null gives null. */
export function parseNetworkPolicy(c: Doc, n: YNode | null, path: string): [NetworkPolicy | null, boolean] {
  if (isNull(n)) return [null, true];
  const m = c.mapping(n, path);
  if (m === null) return [null, false];
  try {
    const p: NetworkPolicy = {};
    let ok = true;
    const v = m.get("extends");
    if (v !== null && !isNull(v)) {
      let refs: string[] = [];
      let nodes: YNode[] = [];
      const rv = resolve(v)!;
      if (rv.kind === "scalar") {
        const [s, good] = c.nonEmptyStr(v, m.field("extends"));
        if (good) {
          refs = [s];
          nodes = [v];
        } else ok = false;
      } else {
        const [list, good] = c.uniqueStrList(v, m.field("extends"));
        ok = ok && good;
        refs = list ?? [];
        nodes = resolve(v)!.content;
      }
      refs.forEach((ref, i) => {
        const r = resolvePolicy(ref);
        if (r.error !== undefined) {
          c.errf(nodes[i], m.field("extends"), r.error);
          ok = false;
          return;
        }
        (p.extends ??= []).push(ref);
      });
    }
    const hosts = (key: string): string[] | undefined => {
      const hv = m.get(key);
      if (hv === null || isNull(hv)) return undefined;
      const [list, good] = c.strList(hv, m.field(key));
      if (!good || list === null) {
        ok = false;
        return undefined;
      }
      const items = resolve(hv)!.content;
      const out: string[] = [];
      const seen = new Set<string>();
      list.forEach((h, i) => {
        const r = normalizeHost(h);
        if (r.error !== undefined) {
          c.errf(items[i], indexPath(m.field(key), i), r.error);
          ok = false;
          return;
        }
        if (!seen.has(r.host)) {
          seen.add(r.host);
          out.push(r.host);
        }
      });
      return out;
    };
    const allowed = hosts("allowedHosts");
    if (allowed !== undefined) p.allowedHosts = allowed;
    const denied = hosts("deniedHosts");
    if (denied !== undefined) p.deniedHosts = denied;
    return [p, ok];
  } finally {
    m.finish();
  }
}

/** ExecSettings are the execution settings of agentDefaults, agents and automations. */
export interface ExecSettings {
  c: Doc;
  node: YNode | null;
  model: string | null;
  modelNode: YNode | null;
  harness: HarnessOverride | null;
  machine: string | null;
  machineNode: YNode | null;
  envSet: boolean;
  env: string;
  secretsSet: boolean;
  secrets: string[] | null;
  mcpSet: boolean;
  mcp: Record<string, MCPServer> | null;
  mcpNode: YNode | null;
  workerSet: boolean;
  worker: string;
  idle: number | null;
}

export function emptyExec(c: Doc, node: YNode | null = null): ExecSettings {
  return {
    c,
    node,
    model: null,
    modelNode: null,
    harness: null,
    machine: null,
    machineNode: null,
    envSet: false,
    env: "",
    secretsSet: false,
    secrets: null,
    mcpSet: false,
    mcp: null,
    mcpNode: null,
    workerSet: false,
    worker: "",
    idle: null,
  };
}

/** parseExec consumes the execution-setting keys of m. */
export function parseExec(c: Doc, m: Mapping, fullHarness: boolean, idle: boolean): ExecSettings {
  const e = emptyExec(c, m.node);
  if (m.present("model") && m.present("harness")) {
    c.errf(m.keyNode("harness"), m.field("harness"), "set at most one of model or harness");
  }
  let v = m.get("model");
  if (v !== null && !isNull(v)) {
    const [s, ok] = c.nonEmptyStr(v, m.field("model"));
    if (ok && checkModelHarness(c, v, m.field("model"), s)) {
      e.model = s;
      e.modelNode = v;
    }
  }
  v = m.get("harness");
  if (v !== null && !isNull(v)) e.harness = parseHarness(c, v, m.field("harness"), fullHarness);
  v = m.get("machine");
  if (v !== null && !isNull(v)) {
    const [s, ok] = c.nonEmptyStr(v, m.field("machine"));
    if (ok) {
      e.machine = s;
      e.machineNode = v;
    }
  }
  if (m.has("environmentId")) {
    const ev = m.get("environmentId");
    e.envSet = true;
    if (!isNull(ev)) e.env = c.str(ev, m.field("environmentId"))[0];
  }
  v = m.get("secrets");
  if (v !== null && !isNull(v)) {
    const [list, ok] = secretList(c, v, m.field("secrets"));
    if (ok) {
      e.secretsSet = true;
      e.secrets = list;
    }
  }
  v = m.get("mcpServers");
  if (v !== null && !isNull(v)) {
    const [srv, ok] = parseMCPServers(c, v, m.field("mcpServers"));
    if (ok) {
      e.mcpSet = true;
      e.mcp = srv;
      e.mcpNode = v;
    }
  }
  if (m.has("workerHost")) {
    const wv = m.get("workerHost");
    e.workerSet = true;
    if (!isNull(wv)) e.worker = goTrimSpace(c.str(wv, m.field("workerHost"))[0]);
  }
  if (idle) {
    v = m.get("idleTimeoutMinutes");
    if (v !== null && !isNull(v)) {
      const [i, ok] = c.intRange(v, m.field("idleTimeoutMinutes"), 0, 60);
      if (ok) e.idle = i;
    }
  }
  return e;
}

/** checkModelHarness reports an error when a shorthand model selects no harness. */
export function checkModelHarness(c: Doc, n: YNode, path: string, model: string): boolean {
  if (harnessForModel(model) !== "") return true;
  c.errf(
    n,
    path,
    `${path} ${goQuote(model)} is not a Claude or an OpenAI model name, so it selects no harness; set harness with a type (claude or codex) and a model`,
  );
  return false;
}

function cloneHarness(h: HarnessConfig): HarnessConfig {
  const out: HarnessConfig = { type: h.type, model: h.model };
  if (h.reasoningLevel !== undefined) out.reasoningLevel = h.reasoningLevel;
  if (h.auth) out.auth = { ...h.auth };
  return out;
}

/** withModel applies the shorthand `model: X` on top of base. */
export function withModel(base: HarnessConfig, model: string): HarnessConfig {
  let t = harnessForModel(model);
  if (t === "") t = base.type;
  if (t !== base.type) return { type: t, model };
  const out = cloneHarness(base);
  out.model = model;
  return out;
}

/** applyHarness applies a model shorthand or a sparse harness override on top of base. */
export function applyHarness(base: HarnessConfig, model: string | null, h: HarnessOverride | null): HarnessConfig {
  if (model !== null) return withModel(base, model);
  if (h === null) return base;
  let out = cloneHarness(base);
  if (h.type !== null && h.type !== base.type) {
    out = { type: h.type, model: "" };
    if (h.model === null) {
      h.c.errf(
        h.node,
        h.path + ".model",
        `${h.path}.model is required when changing the harness type from ${base.type} to ${h.type}`,
      );
      out.model = base.model;
    }
  }
  if (h.model !== null) out.model = h.model;
  if (h.reasoning !== null) out.reasoningLevel = h.reasoning;
  if (h.authSet) {
    if (h.auth) out.auth = { ...h.auth };
    else delete out.auth;
  }
  return out;
}

export function toHarness(h: HarnessOverride | null, model: string | null): HarnessConfig | null {
  if (model !== null) return { type: harnessForModel(model), model };
  if (h === null || h.type === null || h.model === null) return null;
  const out: HarnessConfig = { type: h.type, model: h.model };
  if (h.reasoning !== null) out.reasoningLevel = h.reasoning;
  if (h.auth) out.auth = h.auth;
  return out;
}

export function dedupe(...lists: (string[] | null | undefined)[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const l of lists) {
    for (const s of l ?? []) {
      if (!seen.has(s)) {
        seen.add(s);
        out.push(s);
      }
    }
  }
  return out;
}

export function copyMCP(m: Record<string, MCPServer> | null | undefined): Record<string, MCPServer> | null {
  if (m === null || m === undefined) return null;
  const out: Record<string, MCPServer> = {};
  for (const [k, v] of Object.entries(m)) put(out, k, v);
  return out;
}
