// Definition compiler entry point and tree classification. Port of
// internal/definition/compile.go.

import { compileAgents } from "./agents.ts";
import { compileAutomations } from "./automations.ts";
import { resourceNameRe } from "./common.ts";
import { encodeDefinition } from "./encode.ts";
import { compileBenchmarks, compileScorers, compileSkills, scorerBaseHarness } from "./extras.ts";
import { FACTORY_FILE, type FactorySpec, parseFactory } from "./factory.ts";
import { goQuote } from "./gofmt.ts";
import { MAX_WEBHOOKS, parseMachine, parseWebhook } from "./resources.ts";
import type { AgentConfig, CompiledDefinition, CompileResult, MachineSpec } from "./types.ts";
import { Diag, type Doc } from "./yamlnode.ts";

export interface SkillDir {
  dir: string;
  /** Path of SKILL.md; "" when missing. */
  skillMD: string;
  supports: Map<string, string>;
}

export interface BenchDir {
  /** Path of suite.yaml; "" when missing. */
  suite: string;
  tasks: Map<string, string>;
}

/** Tree is a definition's files classified by resource kind. */
export interface Tree {
  factory: string | null;
  agents: Map<string, string>;
  agentSkills: Map<string, Map<string, SkillDir>>;
  skills: Map<string, SkillDir>;
  automations: Map<string, string>;
  machines: Map<string, string>;
  webhooks: Map<string, string>;
  scorers: Map<string, string>;
  benchmarks: Map<string, BenchDir>;
  files: Map<string, string>;
}

/** Compiler holds the state of one compile call. */
export interface Compiler {
  d: Diag;
  t: Tree;
  f: FactorySpec;
  def: CompiledDefinition;
  agents: Map<string, AgentConfig>;
  foreman: string;
}

/** goPathClean ports Go's path.Clean. */
export function goPathClean(p: string): string {
  if (p === "") return ".";
  const rooted = p.startsWith("/");
  const parts = p.split("/");
  const out: string[] = [];
  for (const part of parts) {
    if (part === "" || part === ".") continue;
    if (part === "..") {
      if (out.length > 0 && out[out.length - 1] !== "..") out.pop();
      else if (!rooted) out.push("..");
      continue;
    }
    out.push(part);
  }
  const s = out.join("/");
  if (rooted) return "/" + s;
  return s === "" ? "." : s;
}

/** normalizePath cleans a definition path to its slash-separated relative form; "" for invalid paths. */
export function normalizePath(p: string): string {
  let s = p.replaceAll("\\", "/");
  s = goPathClean("/" + s);
  s = s.startsWith("/") ? s.slice(1) : s;
  if (s === "." || s === "") return "";
  return s;
}

export function sortedNames<V>(m: Map<string, V>): string[] {
  return [...m.keys()].sort();
}

function classify(d: Diag, files: Record<string, string>): Tree {
  const t: Tree = {
    factory: null,
    agents: new Map(),
    agentSkills: new Map(),
    skills: new Map(),
    automations: new Map(),
    machines: new Map(),
    webhooks: new Map(),
    scorers: new Map(),
    benchmarks: new Map(),
    files: new Map(),
  };
  const paths: string[] = [];
  for (const [p, content] of Object.entries(files)) {
    const np = normalizePath(p);
    if (np === "") {
      d.add(p, 0, "", "invalid path");
      continue;
    }
    if (t.files.has(np)) {
      d.add(np, 0, "", "path is listed more than once");
      continue;
    }
    t.files.set(np, content);
    paths.push(np);
  }
  paths.sort();

  const checkName = (p: string, kind: string, name: string): boolean => {
    if (!resourceNameRe.test(name)) {
      d.add(
        p,
        0,
        "",
        `${kind} name ${goQuote(name)} must be 1-64 letters, digits, '.', '_' or '-' and start with a letter or digit`,
      );
      return false;
    }
    return true;
  };
  const yamlFile = (p: string, kind: string, dir: string, file: string, into: Map<string, string>): void => {
    if (file.endsWith(".yaml")) {
      const name = file.slice(0, -".yaml".length);
      if (checkName(p, kind, name)) into.set(name, p);
    } else if (file.endsWith(".yml")) {
      d.add(p, 0, "", `${kind} files must use the .yaml extension (${dir}/<name>.yaml)`);
    }
  };
  const addSkillFile = (m: Map<string, SkillDir>, dir: string, name: string, rel: string, p: string): void => {
    let sd = m.get(name);
    if (!sd) {
      sd = { dir, skillMD: "", supports: new Map() };
      m.set(name, sd);
    }
    if (rel === "SKILL.md") sd.skillMD = p;
    else sd.supports.set(rel, p);
  };
  const bench = (slug: string): BenchDir => {
    let b = t.benchmarks.get(slug);
    if (!b) {
      b = { suite: "", tasks: new Map() };
      t.benchmarks.set(slug, b);
    }
    return b;
  };

  for (const p of paths) {
    const seg = p.split("/");
    const s0 = seg[0];
    const s1 = seg[1] ?? "";
    if (p === FACTORY_FILE) {
      t.factory = t.files.get(p) ?? "";
    } else if (p === "factory.yml") {
      d.add(p, 0, "", "the root document must be named factory.yaml");
    } else if (s0 === "agents" && seg.length === 3 && seg[2] === "agent.md") {
      if (checkName(p, "agent", s1)) t.agents.set(s1, p);
    } else if (s0 === "agents" && seg.length === 2 && s1.endsWith(".md")) {
      d.add(p, 0, "", "agent files must be agents/<name>/agent.md");
    } else if (s0 === "agents" && seg.length >= 5 && seg[2] === "skills") {
      if (checkName(p, "skill", seg[3]!)) {
        let m = t.agentSkills.get(s1);
        if (!m) {
          m = new Map();
          t.agentSkills.set(s1, m);
        }
        addSkillFile(m, seg.slice(0, 4).join("/"), seg[3]!, seg.slice(4).join("/"), p);
      }
    } else if (s0 === "skills" && seg.length >= 3) {
      if (checkName(p, "skill", s1)) addSkillFile(t.skills, seg.slice(0, 2).join("/"), s1, seg.slice(2).join("/"), p);
    } else if (s0 === "automations" && seg.length === 3 && seg[2] === "automation.md") {
      if (checkName(p, "automation", s1)) t.automations.set(s1, p);
    } else if (s0 === "automations" && seg.length === 2 && s1.endsWith(".md")) {
      d.add(p, 0, "", "automation files must be automations/<name>/automation.md");
    } else if (s0 === "machines" && seg.length === 2) {
      yamlFile(p, "machine", "machines", s1, t.machines);
    } else if (s0 === "webhooks" && seg.length === 2) {
      yamlFile(p, "webhook", "webhooks", s1, t.webhooks);
    } else if (s0 === "scorers" && seg.length === 3 && seg[2] === "scorer.md") {
      if (checkName(p, "scorer", s1)) t.scorers.set(s1, p);
    } else if (s0 === "scorers" && seg.length === 2 && s1.endsWith(".md")) {
      d.add(p, 0, "", "scorer files must be scorers/<slug>/scorer.md");
    } else if (s0 === "benchmarks" && seg.length === 3 && seg[2] === "suite.yaml") {
      if (checkName(p, "benchmark suite", s1)) bench(s1).suite = p;
    } else if (s0 === "benchmarks" && seg.length === 4 && seg[2] === "tasks") {
      if (checkName(p, "benchmark suite", s1)) {
        const tasks = new Map<string, string>();
        yamlFile(p, "benchmark task", "benchmarks/<suite>/tasks", seg[3]!, tasks);
        for (const [k, v] of tasks) bench(s1).tasks.set(k, v);
      }
    }
  }
  for (const [agent, skills] of t.agentSkills) {
    if (!t.agents.has(agent)) {
      for (const sd of skills.values()) {
        d.add(
          sd.dir,
          0,
          "",
          `skills directory belongs to agent ${goQuote(agent)}, which has no agents/${agent}/agent.md`,
        );
      }
    }
  }
  for (const [slug, b] of t.benchmarks) {
    if (b.suite === "") d.add("benchmarks/" + slug, 0, "", "benchmark directory has task files but no suite.yaml");
  }
  return t;
}

/**
 * compile validates and compiles a definition tree (path → content). On any
 * validation error it returns a null definition and the errors sorted by file
 * and line. Agent, automation and factory IDs are left empty.
 */
export function compile(files: Record<string, string>): CompileResult {
  const d = new Diag();
  const t = classify(d, files);
  if (t.factory === null) {
    d.add(FACTORY_FILE, 0, "", "factory.yaml is required at the root of the definition");
    return { definition: null, errors: d.sorted() };
  }
  const f = parseFactory(d, t.factory);
  if (f === null) return { definition: null, errors: d.sorted() };
  const c: Compiler = { d, t, f, def: f.def, agents: new Map(), foreman: "" };
  const def = c.def;
  const machines: Record<string, MachineSpec> = {};
  def.machines = machines;
  def.agents = [];
  def.automations = [];
  def.scorers = [];
  def.webhooks = [];
  def.benchmarks = [];
  def.skills = [];

  for (const name of sortedNames(t.machines)) {
    const p = t.machines.get(name)!;
    const [spec, ok] = parseMachine(d, p, name, t.files.get(p) ?? "");
    if (ok) machines[name] = spec;
  }
  if (t.webhooks.size > MAX_WEBHOOKS) {
    d.add("webhooks", 0, "", `a factory can define at most ${MAX_WEBHOOKS} webhooks (found ${t.webhooks.size})`);
  }
  for (const name of sortedNames(t.webhooks)) {
    const p = t.webhooks.get(name)!;
    const [w, ok] = parseWebhook(d, p, name, t.files.get(p) ?? "");
    if (ok) def.webhooks.push(w);
  }
  checkDefaultMachine(c);
  checkScorerDefaults(c);

  compileAgents(c);
  compileAutomations(c);
  compileSkills(c);
  compileScorers(c);
  compileBenchmarks(c);

  if (d.errs.length > 0) return { definition: null, errors: d.sorted() };
  return { definition: encodeDefinition(def), errors: [] };
}

/** machineRef validates a machine reference and returns its spec. */
export function machineRef(c: Compiler, dc: Doc, name: string, field: string, line: number): MachineSpec | null {
  if (!c.t.machines.has(name)) {
    c.d.add(dc.file, line, field, `${field} ${goQuote(name)} does not match any machines/<name>.yaml file`);
    return null;
  }
  const spec = c.def.machines?.[name];
  if (!spec) return null;
  return { ...spec };
}

function checkDefaultMachine(c: Compiler): void {
  const e = c.f.defaults;
  if (e.machine === null) return;
  const spec = machineRef(c, c.f.c, e.machine, "agentDefaults.machine", c.f.c.line(e.machineNode));
  if (spec !== null && spec.os !== "linux") {
    c.f.c.errf(
      e.machineNode,
      "agentDefaults.machine",
      `agentDefaults.machine must be a Linux machine because it supplies the factory-managed environment (${e.machine} is ${spec.os})`,
    );
  }
}

function checkScorerDefaults(c: Compiler): void {
  const sd = c.f.scorerDefaults;
  if (sd !== null && sd.machine !== "")
    machineRef(c, c.f.c, sd.machine, "scorerDefaults.machine", c.f.c.line(sd.machineNode));
  const [h, ok] = scorerBaseHarness(c);
  if (ok) {
    c.def.scorerDefaults ??= { secrets: null, mcp_servers: null };
    c.def.scorerDefaults.harness = h;
  }
}
