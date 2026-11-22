// Skills, scorers and benchmarks. Port of internal/definition/extras.go.

import { parseMarkdown } from "./agents.ts";
import {
  applyHarness,
  checkModelHarness,
  dedupe,
  type HarnessOverride,
  parseExec,
  parseHarness,
  parseMCPServers,
  secretList,
  toHarness,
  withModel,
} from "./common.ts";
import { type BenchDir, type Compiler, machineRef, type SkillDir, sortedNames } from "./compile.ts";
import { goFloat, goQuote, goTrimSpace } from "./gofmt.ts";
import type {
  BenchmarkAgentPin,
  BenchmarkConfiguration,
  BenchmarkSuite,
  BenchmarkTask,
  HarnessConfig,
  MCPServer,
  RepoAtCommit,
  Scorer,
  ScorerAgent,
  ScorerLabel,
  Skill,
} from "./types.ts";
import { Doc, indexPath, isNull, isSeqNode, Mapping, put, resolve, toValue, type YNode } from "./yamlnode.ts";

const skillNameRe = /^[a-z0-9]+(-[a-z0-9]+)*$/;

function byteLen(s: string): number {
  return new TextEncoder().encode(s).length;
}

/** parseSkill parses a skill directory. scope is "factory" or the agent name. */
function parseSkill(c: Compiler, name: string, sd: SkillDir, scope: string): [Skill, boolean] {
  const s: Skill = { name, description: "", path: sd.skillMD, content: "", scope };
  if (sd.skillMD === "") {
    c.d.add(sd.dir, 0, "", `skill directory ${sd.dir} has no SKILL.md`);
    return [s, false];
  }
  const before = c.d.errs.length;
  const content = c.t.files.get(sd.skillMD) ?? "";
  s.content = content;
  const { c: dc, m, ok } = parseMarkdown(c.d, sd.skillMD, content, true);
  if (!ok || m === null) return [s, false];
  for (const k of m.order) m.get(k);
  let v = m.require("name");
  if (v !== null) {
    const [n, good] = dc.nonEmptyStr(v, "name");
    if (good) {
      if (byteLen(n) > 64 || !skillNameRe.test(n)) {
        dc.errf(v, "name", `skill name ${goQuote(n)} must be at most 64 lowercase letters, digits and single hyphens`);
      } else if (n !== name) {
        dc.errf(v, "name", `skill name ${goQuote(n)} must match its directory name ${goQuote(name)}`);
      }
      s.name = n;
    }
  }
  v = m.require("description");
  if (v !== null) {
    const [desc, good] = dc.nonEmptyStr(v, "description");
    if (good) {
      if (byteLen(desc) > 1024) dc.errf(v, "description", "skill description must be at most 1024 characters");
      s.description = desc;
    }
  }
  if (sd.supports.size > 0) {
    s.files = {};
    for (const [rel, p] of sd.supports) put(s.files, rel, c.t.files.get(p) ?? "");
  }
  return [s, c.d.errs.length === before];
}

export function compileSkills(c: Compiler): void {
  for (const name of sortedNames(c.t.skills)) {
    const [s, ok] = parseSkill(c, name, c.t.skills.get(name)!, "factory");
    if (ok) c.def.skills!.push(s);
  }
}

/** agentSkills parses an agent's own skills. */
export function agentSkills(c: Compiler, agent: string): Skill[] {
  const dirs = c.t.agentSkills.get(agent) ?? new Map<string, SkillDir>();
  const out: Skill[] = [];
  for (const name of sortedNames(dirs)) {
    const [s, ok] = parseSkill(c, name, dirs.get(name)!, agent);
    if (ok) out.push(s);
  }
  return out;
}

// ---- scorers ----

export function compileScorers(c: Compiler): void {
  const names = new Map<string, string>();
  for (const slug of sortedNames(c.t.scorers)) {
    const p = c.t.scorers.get(slug)!;
    const [s, ok] = parseScorer(c, p, slug, c.t.files.get(p) ?? "");
    if (!ok) continue;
    const prev = names.get(s.name);
    if (prev !== undefined) {
      c.d.add(p, 0, "name", `scorer name ${goQuote(s.name)} is already used by ${prev}`);
      continue;
    }
    names.set(s.name, p);
    c.def.scorers!.push(s);
  }
}

function parseScorer(c: Compiler, file: string, slug: string, content: string): [Scorer, boolean] {
  const before = c.d.errs.length;
  const s: Scorer = {
    id: "",
    factory_id: "",
    slug,
    name: "",
    agents: null,
    labels: null,
    passing_score: 0,
    sampling_rate: 10,
    model: "",
    rubric: "",
    self_improvement: false,
  };
  const { c: dc, m, body, ok } = parseMarkdown(c.d, file, content, true);
  if (!ok || m === null) return [s, false];
  try {
    s.rubric = goTrimSpace(body);
    if (s.rubric === "") dc.errf(null, "", "the scorer's Markdown body (its rubric) must not be empty");
    let v = m.require("name");
    if (v !== null) s.name = dc.nonEmptyStr(v, "name")[0];
    v = m.get("description");
    if (v !== null && !isNull(v)) s.description = dc.str(v, "description")[0];
    v = m.get("output");
    if (v !== null && !isNull(v)) dc.enum(v, "output", "classification");
    let passingOK = false;
    v = m.require("passingScore");
    if (v !== null) {
      const [f, good] = dc.number(v, "passingScore");
      if (good) {
        if (f < 0 || f > 1) dc.errf(v, "passingScore", "passingScore must be between 0 and 1");
        else {
          s.passing_score = f;
          passingOK = true;
        }
      }
    }
    v = m.get("samplingRate");
    if (v !== null && !isNull(v)) {
      const [f, good] = dc.number(v, "samplingRate");
      if (good) {
        if (f < 0 || f > 100) dc.errf(v, "samplingRate", "samplingRate must be between 0 and 100");
        else s.sampling_rate = roundHundredths(resolve(v)!.value, f);
      }
    }
    let harness: HarnessOverride | null = null;
    v = m.get("harness");
    if (v !== null && !isNull(v)) harness = parseHarness(dc, v, "harness", false);
    if (harness === null) {
      v = m.require("model");
      if (v !== null) {
        const [mv, good] = dc.nonEmptyStr(v, "model");
        if (good && checkModelHarness(dc, v, "model", mv)) s.model = mv;
      }
    } else {
      v = m.get("model");
      if (v !== null && !isNull(v)) s.model = dc.nonEmptyStr(v, "model")[0];
    }
    v = m.get("selfImprovement");
    if (v !== null && !isNull(v)) s.self_improvement = dc.boolean(v, "selfImprovement")[0];
    v = m.require("agents");
    if (v !== null) {
      const items = dc.seq(v, "agents") ?? [];
      if (isSeqNode(v) && items.length === 0) dc.errf(v, "agents", "agents must list at least one agent");
      const seen = new Set<string>();
      items.forEach((it, i) => {
        const p = indexPath("agents", i);
        const sa: ScorerAgent = { name: "" };
        let nameNode: YNode | null = null;
        const r = resolve(it)!;
        if (r.kind === "scalar") {
          const [n, good] = dc.nonEmptyStr(it, p);
          if (!good) return;
          sa.name = n;
          nameNode = it;
        } else {
          const am = dc.mapping(it, p);
          if (am === null) return;
          const nv = am.require("name");
          if (nv !== null) {
            sa.name = dc.nonEmptyStr(nv, p + ".name")[0];
            nameNode = nv;
          }
          const dv = am.get("includeDescendants");
          if (dv !== null && !isNull(dv)) sa.includeDescendants = dc.boolean(dv, p + ".includeDescendants")[0];
          am.finish();
        }
        if (sa.name === "") return;
        if (!c.t.agents.has(sa.name))
          dc.errf(nameNode, p, `scorer agent ${goQuote(sa.name)} does not match any agents/<name>/agent.md`);
        if (seen.has(sa.name)) dc.errf(nameNode, p, `agent ${goQuote(sa.name)} is listed more than once`);
        seen.add(sa.name);
        (s.agents ??= []).push(sa);
      });
    }
    v = m.require("labels");
    if (v !== null) {
      const items = dc.seq(v, "labels") ?? [];
      if (isSeqNode(v) && (items.length < 1 || items.length > 20)) {
        dc.errf(v, "labels", `labels must list 1 to 20 classifications (found ${items.length})`);
      }
      const seen = new Set<string>();
      items.forEach((it, i) => {
        const p = indexPath("labels", i);
        const lm = dc.mapping(it, p);
        if (lm === null) return;
        const l: ScorerLabel = { value: "", score: 0 };
        const lv = lm.require("value");
        if (lv !== null) {
          const [val, good] = dc.nonEmptyStr(lv, p + ".value");
          if (good) {
            if (seen.has(val)) dc.errf(lv, p + ".value", `label value ${goQuote(val)} is used more than once`);
            seen.add(val);
            l.value = val;
          }
        }
        const dv = lm.get("description");
        if (dv !== null && !isNull(dv)) l.description = dc.str(dv, p + ".description")[0];
        const sv = lm.require("score");
        if (sv !== null) {
          const [f, good] = dc.number(sv, p + ".score");
          if (good) {
            if (f < 0 || f > 1) dc.errf(sv, p + ".score", `${p}.score must be between 0 and 1`);
            l.score = f;
          }
        }
        lm.finish();
        (s.labels ??= []).push(l);
      });
      const labels = s.labels ?? [];
      if (passingOK && labels.length > 0) {
        const pass = labels.some((l) => l.score >= s.passing_score);
        const fail = labels.some((l) => l.score < s.passing_score);
        if (!pass) dc.errf(v, "labels", `at least one label must meet passingScore ${goFloat(s.passing_score)}`);
        if (!fail) dc.errf(v, "labels", `at least one label must fall below passingScore ${goFloat(s.passing_score)}`);
      }
    }
    v = m.get("machine");
    if (v !== null && !isNull(v)) {
      const [r, good] = dc.nonEmptyStr(v, "machine");
      if (good) {
        machineRef(c, dc, r, "machine", dc.line(v));
        s.machine = r;
      }
    }
    v = m.get("secrets");
    if (v !== null && !isNull(v)) {
      const [list, good] = secretList(dc, v, "secrets");
      if (good && list !== null) s.secrets = list;
    }
    v = m.get("mcpServers");
    if (v !== null && !isNull(v)) {
      const [srv, good] = parseMCPServers(dc, v, "mcpServers");
      if (good && srv !== null) s.mcp_servers = srv;
    }
    resolveScorerExec(c, s, harness);
    return [s, c.d.errs.length === before];
  } finally {
    m.finish();
  }
}

/** scorerBaseHarness returns the judge harness before one scorer's fields apply. */
export function scorerBaseHarness(c: Compiler): [HarnessConfig, boolean] {
  let base = c.f.defaultHarness;
  let ok = c.f.harnessOK;
  const sd = c.f.scorerDefaults;
  if (sd === null) return [base, ok];
  if (sd.harness !== null) {
    if (!ok) {
      const h = toHarness(sd.harness, null);
      if (h === null) return [base, false];
      base = h;
      ok = true;
    } else {
      base = applyHarness(base, null, sd.harness);
    }
  }
  if (sd.model !== null && ok) base = withModel(base, sd.model);
  return [base, ok];
}

function resolveScorerExec(c: Compiler, s: Scorer, harness: HarnessOverride | null): void {
  let [base, ok] = scorerBaseHarness(c);
  if (harness !== null) {
    if (ok) base = applyHarness(base, null, harness);
    else {
      const h = toHarness(harness, null);
      if (h !== null) {
        base = h;
        ok = true;
      }
    }
  }
  if (ok) {
    if (s.model !== "" && harness === null) base = withModel(base, s.model);
    else if (s.model !== "") base = { ...base, model: s.model };
    const h: HarnessConfig = { ...base };
    if (base.auth) h.auth = { ...base.auth };
    s.harness = h;
    if (s.model === "") s.model = h.model;
  }
  const sd = c.f.scorerDefaults;
  if (sd === null) return;
  if ((s.machine ?? "") === "") s.machine = sd.machine;
  if ((sd.secrets ?? []).length > 0) s.secrets = dedupe(sd.secrets, s.secrets);
  if (sd.mcp !== null && Object.keys(sd.mcp).length > 0) {
    const merged: Record<string, MCPServer> = {};
    for (const [k, v] of Object.entries(sd.mcp)) put(merged, k, v);
    for (const [k, v] of Object.entries(s.mcp_servers ?? {})) put(merged, k, v);
    s.mcp_servers = merged;
  }
}

/**
 * roundHundredths rounds a decimal literal to two decimal places, half away
 * from zero, with exact decimal arithmetic (12.345 rounds to 12.35).
 */
export function roundHundredths(literal: string, fallback: number): number {
  const r = parseRat(literal);
  if (r === null) {
    const x = fallback * 100;
    return (x < 0 ? -Math.round(-x) : Math.round(x)) / 100;
  }
  const num = r.num * 100n;
  const den = r.den;
  let q = num / den;
  let rem = num % den;
  if (rem < 0n) rem = -rem;
  if (rem * 2n >= den) q += num < 0n ? -1n : 1n;
  return Number(q) / 100;
}

function parseRat(s: string): { num: bigint; den: bigint } | null {
  let m = /^([+-]?)([0-9]*)(?:\.([0-9]*))?(?:[eE]([+-]?[0-9]+))?$/.exec(s);
  if (m && ((m[2] ?? "") !== "" || (m[3] ?? "") !== "")) {
    const sign = m[1] === "-" ? -1n : 1n;
    const intPart = m[2] ?? "";
    const frac = m[3] ?? "";
    const exp = Number(m[4] ?? "0");
    let num = BigInt((intPart + frac).replace(/^0+(?=.)/, "") || "0") * sign;
    const scale = frac.length - exp;
    let den = 1n;
    if (scale > 0) den = 10n ** BigInt(scale);
    else if (scale < 0) num *= 10n ** BigInt(-scale);
    return { num, den };
  }
  m = /^([+-]?)0([xXbBoO])([0-9a-fA-F]+)$/.exec(s);
  if (m) {
    const base = { x: 16, b: 2, o: 8 }[m[2]!.toLowerCase() as "x" | "b" | "o"];
    let n = 0n;
    for (const ch of m[3]!) {
      const d = parseInt(ch, 16);
      if (d >= base) return null;
      n = n * BigInt(base) + BigInt(d);
    }
    return { num: m[1] === "-" ? -n : n, den: 1n };
  }
  m = /^([+-]?)([0-9]+)\/([0-9]+)$/.exec(s);
  if (m) {
    const den = BigInt(m[3]!);
    if (den === 0n) return null;
    const n = BigInt(m[2]!);
    return { num: m[1] === "-" ? -n : n, den };
  }
  return null;
}

// ---- benchmarks ----

const shaRe = /^[0-9a-f]{40}$/;

export function compileBenchmarks(c: Compiler): void {
  const names = new Map<string, string>();
  for (const slug of sortedNames(c.t.benchmarks)) {
    const b = c.t.benchmarks.get(slug)!;
    if (b.suite === "") continue;
    const [suite, ok] = parseSuite(c, slug, b);
    if (!ok) continue;
    const prev = names.get(suite.name);
    if (prev !== undefined) {
      c.d.add(b.suite, 0, "name", `benchmark suite name ${goQuote(suite.name)} is already used by ${prev}`);
      continue;
    }
    names.set(suite.name, b.suite);
    c.def.benchmarks!.push(suite);
  }
}

function parseSuite(c: Compiler, slug: string, b: BenchDir): [BenchmarkSuite, boolean] {
  const before = c.d.errs.length;
  const s: BenchmarkSuite = { slug, name: "", agent: "" };
  const dc = new Doc(c.d, b.suite);
  const root = dc.parse(c.t.files.get(b.suite) ?? "");
  if (root === null) return [s, false];
  const m = dc.mapping(root, "");
  if (m === null) return [s, false];
  try {
    let v = m.require("name");
    if (v !== null) s.name = dc.nonEmptyStr(v, "name")[0];
    v = m.get("description");
    if (v !== null && !isNull(v)) s.description = dc.str(v, "description")[0];
    v = m.require("agent");
    if (v !== null) {
      const [a, good] = dc.nonEmptyStr(v, "agent");
      if (good) {
        if (!c.t.agents.has(a)) dc.errf(v, "agent", `agent ${goQuote(a)} does not match any agents/<name>/agent.md`);
        s.agent = a;
      }
    }
    v = m.get("configurations");
    if (v !== null && !isNull(v)) {
      const items = dc.seq(v, "configurations") ?? [];
      if (isSeqNode(v) && (items.length < 1 || items.length > 6)) {
        dc.errf(v, "configurations", `configurations must list 1 to 6 configurations (found ${items.length})`);
      }
      const seen = new Set<string>();
      items.forEach((it, i) => {
        const p = indexPath("configurations", i);
        const [cfg, good] = parseBenchConfig(c, dc, it, p);
        if (!good) return;
        if (seen.has(cfg.name))
          dc.errf(it, p + ".name", `configuration name ${goQuote(cfg.name)} is used more than once`);
        seen.add(cfg.name);
        (s.configurations ??= []).push(cfg);
      });
    }
    const listed = new Set<string>();
    v = m.get("tasks");
    if (v !== null && !isNull(v)) {
      const [list] = dc.uniqueStrList(v, "tasks");
      const items = resolve(v)!.content;
      (list ?? []).forEach((taskSlug, i) => {
        listed.add(taskSlug);
        const p = b.tasks.get(taskSlug);
        if (p === undefined) {
          dc.errf(
            items[i],
            indexPath("tasks", i),
            `task ${goQuote(taskSlug)} has no file benchmarks/${s.slug}/tasks/${taskSlug}.yaml`,
          );
          return;
        }
        const [task, good] = parseTask(c, p, taskSlug);
        if (good) (s.tasks ??= []).push(task);
      });
    }
    const unlisted = [...b.tasks.keys()].filter((k) => !listed.has(k)).sort();
    for (const t of unlisted)
      c.d.add(b.tasks.get(t)!, 0, "", `task file is not listed in benchmarks/${s.slug}/suite.yaml tasks`);
    return [s, c.d.errs.length === before];
  } finally {
    m.finish();
  }
}

function parseBenchConfig(c: Compiler, dc: Doc, n: YNode, p: string): [BenchmarkConfiguration, boolean] {
  const cfg: BenchmarkConfiguration = { name: "" };
  const m = dc.mapping(n, p);
  if (m === null) return [cfg, false];
  try {
    let v = m.require("name");
    if (v !== null) cfg.name = dc.nonEmptyStr(v, p + ".name")[0];
    v = m.get("machine");
    if (v !== null && !isNull(v)) {
      const [r, good] = dc.nonEmptyStr(v, p + ".machine");
      if (good) {
        machineRef(c, dc, r, p + ".machine", dc.line(v));
        cfg.machine = r;
      }
    }
    v = m.get("agents");
    if (v !== null && !isNull(v)) {
      const seen = new Set<string>();
      (dc.seq(v, p + ".agents") ?? []).forEach((it, i) => {
        const ap = indexPath(p + ".agents", i);
        const am = dc.mapping(it, ap);
        if (am === null) return;
        const pin: BenchmarkAgentPin = { agent: "" };
        const av = am.require("agent");
        if (av !== null) {
          const [name, good] = dc.nonEmptyStr(av, ap + ".agent");
          if (good) {
            if (!c.t.agents.has(name))
              dc.errf(av, ap + ".agent", `agent ${goQuote(name)} does not match any agents/<name>/agent.md`);
            if (seen.has(name))
              dc.errf(av, ap + ".agent", `agent ${goQuote(name)} is listed more than once in this configuration`);
            seen.add(name);
            pin.agent = name;
          }
        }
        if (am.present("model") === am.present("harness"))
          dc.errf(am.node, ap, `${ap} must set exactly one of model or harness`);
        const sub = new Mapping(dc, am.node, ap);
        for (const k of ["model", "harness"]) {
          const pv = am.get(k);
          if (pv !== null) sub.vals.set(k, pv);
        }
        sub.keys = am.keys;
        const e = parseExec(dc, sub, true, false);
        if (e.model !== null) pin.model = e.model;
        const h = toHarness(e.harness, null);
        if (h !== null && e.harness !== null) {
          pin.harness = h;
          const a = c.agents.get(pin.agent);
          if (a && h.auth && h.auth.source === "workerEnvironment" && (a.worker_host ?? "") === "") {
            dc.errf(
              e.harness.authNode,
              ap + ".harness.auth.source",
              `auth.source workerEnvironment requires agent ${goQuote(pin.agent)} to use a self-hosted workerHost`,
            );
          }
        }
        am.finish();
        (cfg.agents ??= []).push(pin);
      });
    }
    return [cfg, true];
  } finally {
    m.finish();
  }
}

function parseTask(c: Compiler, file: string, slug: string): [BenchmarkTask, boolean] {
  const before = c.d.errs.length;
  const t: BenchmarkTask = { slug, title: "", prompt: "", successCriteria: "" };
  const dc = new Doc(c.d, file);
  const root = dc.parse(c.t.files.get(file) ?? "");
  if (root === null) return [t, false];
  const m = dc.mapping(root, "");
  if (m === null) return [t, false];
  try {
    let v = m.require("title");
    if (v !== null) t.title = dc.nonEmptyStr(v, "title")[0];
    v = m.require("prompt");
    if (v !== null) t.prompt = dc.nonEmptyStr(v, "prompt")[0];
    v = m.require("successCriteria");
    if (v !== null) t.successCriteria = dc.nonEmptyStr(v, "successCriteria")[0];
    v = m.get("sourceRunId");
    if (v !== null && !isNull(v)) t.sourceRunId = dc.nonEmptyStr(v, "sourceRunId")[0];
    v = m.get("tags");
    if (v !== null && !isNull(v)) {
      const [list] = dc.strList(v, "tags");
      if (list !== null) t.tags = list;
    }
    v = m.get("trackerSeed");
    if (v !== null && !isNull(v)) t.trackerSeed = toValue(v);
    v = m.get("linearSeed");
    if (v !== null && !isNull(v)) {
      if (t.trackerSeed !== undefined && t.trackerSeed !== null) {
        dc.errf(v, "linearSeed", "set trackerSeed or linearSeed, not both (linearSeed is the older name)");
      } else {
        t.trackerSeed = toValue(v);
      }
    }
    v = m.get("startingRepoRefs");
    if (v !== null && !isNull(v)) {
      (dc.seq(v, "startingRepoRefs") ?? []).forEach((it, i) => {
        const [ref, good] = parseRepoAtCommitNode(dc, it, indexPath("startingRepoRefs", i));
        if (good) (t.startingRepoRefs ??= []).push(ref);
      });
    }
    return [t, c.d.errs.length === before];
  } finally {
    m.finish();
  }
}

/** parseRepoAtCommit parses "github.com:OWNER/REPO@SHA" or "gitlab.com:OWNER/REPO@SHA". */
export function parseRepoAtCommit(s: string): RepoAtCommit | null {
  const colon = s.indexOf(":");
  if (colon < 0) return null;
  const host = s.slice(0, colon);
  const rest = s.slice(colon + 1);
  let forge: string;
  if (host === "github.com") forge = "GITHUB";
  else if (host === "gitlab.com") forge = "GITLAB";
  else return null;
  const at = rest.indexOf("@");
  if (at < 0) return null;
  const repoPath = rest.slice(0, at);
  const sha = rest.slice(at + 1);
  if (!shaRe.test(sha)) return null;
  const i = repoPath.lastIndexOf("/");
  if (i <= 0 || i === repoPath.length - 1) return null;
  const owner = repoPath.slice(0, i);
  const repo = repoPath.slice(i + 1);
  if (forge === "GITHUB" && owner.includes("/")) return null;
  return { codeForge: forge, owner, repo, ref: sha };
}

const emptyRef = (): RepoAtCommit => ({ codeForge: "", owner: "", repo: "", ref: "" });

function parseRepoAtCommitNode(dc: Doc, n: YNode, p: string): [RepoAtCommit, boolean] {
  const r = resolve(n);
  if (r !== null && r.kind === "scalar") {
    const [s, ok] = dc.nonEmptyStr(n, p);
    if (!ok) return [emptyRef(), false];
    const ref = parseRepoAtCommit(s);
    if (ref === null) {
      dc.errf(
        n,
        p,
        `${p} ${goQuote(s)} must have the form github.com:OWNER/REPO@COMMIT_SHA or gitlab.com:OWNER/REPO@COMMIT_SHA with a full 40-character commit SHA`,
      );
      return [emptyRef(), false];
    }
    return [ref, true];
  }
  const m = dc.mapping(n, p);
  if (m === null) return [emptyRef(), false];
  try {
    const ref = emptyRef();
    let ok = true;
    let v = m.require("codeForge");
    if (v !== null) ref.codeForge = dc.enum(v, p + ".codeForge", "GITHUB", "GITLAB")[0];
    else ok = false;
    v = m.require("owner");
    if (v !== null) ref.owner = dc.nonEmptyStr(v, p + ".owner")[0];
    else ok = false;
    v = m.require("repo");
    if (v !== null) ref.repo = dc.nonEmptyStr(v, p + ".repo")[0];
    else ok = false;
    v = m.require("ref");
    if (v !== null) {
      const [s, good] = dc.nonEmptyStr(v, p + ".ref");
      if (good) {
        if (!shaRe.test(s)) {
          dc.errf(v, p + ".ref", `${p}.ref must be a full 40-character lowercase commit SHA, not a branch or tag`);
          ok = false;
        }
        ref.ref = s;
      }
    } else ok = false;
    return [ref, ok];
  } finally {
    m.finish();
  }
}
