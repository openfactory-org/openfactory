// Definition diff. Port of internal/definition/diff.go. It compares the JSON
// fields of two compiled definitions (in the Go JSON shape).

import type { CompiledDefinition, Skill } from "./types.ts";

export const CHANGE_ADDED = "added";
export const CHANGE_REMOVED = "removed";
export const CHANGE_CHANGED = "changed";

export type ChangeAction = typeof CHANGE_ADDED | typeof CHANGE_REMOVED | typeof CHANGE_CHANGED;

export const KIND_FACTORY = "factory";
export const KIND_AGENT = "agent";
export const KIND_AUTOMATION = "automation";
export const KIND_MACHINE = "machine";
export const KIND_SCORER = "scorer";
export const KIND_WEBHOOK = "webhook";
export const KIND_BENCHMARK = "benchmark";
export const KIND_SKILL = "skill";

/** Change is one added, removed or changed resource. */
export interface Change {
  kind: string;
  name: string;
  action: ChangeAction;
  /** Changed top-level fields (action "changed"). */
  fields?: string[];
}

export interface DiffResult {
  changes: Change[];
}

export function diffEmpty(r: DiffResult): boolean {
  return r.changes.length === 0;
}

export function diffCounts(r: DiffResult): { added: number; removed: number; changed: number } {
  const out = { added: 0, removed: 0, changed: 0 };
  for (const c of r.changes) {
    if (c.action === CHANGE_ADDED) out.added++;
    else if (c.action === CHANGE_REMOVED) out.removed++;
    else if (c.action === CHANGE_CHANGED) out.changed++;
  }
  return out;
}

/** diffSummary renders one change per line for pull request checks and the dashboard. */
export function diffSummary(r: DiffResult): string {
  if (diffEmpty(r)) return "No changes to the factory definition.";
  const { added, removed, changed } = diffCounts(r);
  let b = `${added} added, ${removed} removed, ${changed} changed\n`;
  const sym: Record<ChangeAction, string> = { added: "+", removed: "-", changed: "~" };
  for (const c of r.changes) {
    b += `${sym[c.action]} ${c.kind} ${c.name}`;
    if (c.fields && c.fields.length > 0) b += ` (${c.fields.join(", ")})`;
    b += "\n";
  }
  return b;
}

type Def = Partial<CompiledDefinition> & Record<string, unknown>;

/** diff compares two compiled definitions; either may be null. Changes are ordered by kind, then name. */
export function diff(
  oldDef: CompiledDefinition | null | undefined,
  newDef: CompiledDefinition | null | undefined,
): DiffResult {
  const o = (oldDef ?? {}) as Def;
  const n = (newDef ?? {}) as Def;
  const changes: Change[] = [];
  const fields = fieldDiff(factoryOnly(o), factoryOnly(n));
  if (fields.length > 0) {
    const name = (n.name ?? "") !== "" ? (n.name as string) : ((o.name as string | undefined) ?? "");
    changes.push({ kind: KIND_FACTORY, name, action: CHANGE_CHANGED, fields });
  }
  changes.push(
    ...diffSet(
      KIND_AGENT,
      indexBy(o.agents, (a) => a.name),
      indexBy(n.agents, (a) => a.name),
    ),
  );
  changes.push(
    ...diffSet(
      KIND_AUTOMATION,
      indexBy(o.automations, (a) => a.name),
      indexBy(n.automations, (a) => a.name),
    ),
  );
  changes.push(
    ...diffSet(KIND_MACHINE, new Map(Object.entries(o.machines ?? {})), new Map(Object.entries(n.machines ?? {}))),
  );
  changes.push(
    ...diffSet(
      KIND_SCORER,
      indexBy(o.scorers, (s) => s.slug),
      indexBy(n.scorers, (s) => s.slug),
    ),
  );
  changes.push(
    ...diffSet(
      KIND_WEBHOOK,
      indexBy(o.webhooks, (w) => w.name),
      indexBy(n.webhooks, (w) => w.name),
    ),
  );
  changes.push(
    ...diffSet(
      KIND_BENCHMARK,
      indexBy(o.benchmarks, (b) => b.slug),
      indexBy(n.benchmarks, (b) => b.slug),
    ),
  );
  changes.push(...diffSet(KIND_SKILL, skillIndex(o), skillIndex(n)));
  return { changes };
}

function factoryOnly(d: Def): Record<string, unknown> {
  const c: Record<string, unknown> = { ...d };
  for (const k of ["agents", "automations", "machines", "scorers", "webhooks", "benchmarks", "skills"]) delete c[k];
  return c;
}

function skillIndex(d: Def): Map<string, Skill> {
  const out = new Map<string, Skill>();
  for (const s of d.skills ?? []) out.set(s.name, s);
  for (const a of d.agents ?? []) for (const s of a.skills ?? []) out.set(a.name + "/" + s.name, s);
  return out;
}

function indexBy<T>(items: T[] | null | undefined, key: (x: T) => string): Map<string, T> {
  const out = new Map<string, T>();
  for (const it of items ?? []) out.set(key(it), it);
  return out;
}

function diffSet<T>(kind: string, oldM: Map<string, T>, newM: Map<string, T>): Change[] {
  const names = [...new Set([...oldM.keys(), ...newM.keys()])].sort();
  const out: Change[] = [];
  for (const name of names) {
    const inOld = oldM.has(name);
    const inNew = newM.has(name);
    if (!inOld) out.push({ kind, name, action: CHANGE_ADDED });
    else if (!inNew) out.push({ kind, name, action: CHANGE_REMOVED });
    else {
      const fields = fieldDiff(oldM.get(name), newM.get(name));
      if (fields.length > 0) out.push({ kind, name, action: CHANGE_CHANGED, fields });
    }
  }
  return out;
}

/** canonicalJSON stringifies with sorted object keys. */
export function canonicalJSON(v: unknown): string {
  if (v === undefined) return "null";
  if (v === null || typeof v !== "object") return JSON.stringify(v);
  if (Array.isArray(v)) return "[" + v.map((x) => canonicalJSON(x)).join(",") + "]";
  const keys = Object.keys(v as object)
    .filter((k) => (v as Record<string, unknown>)[k] !== undefined)
    .sort();
  return (
    "{" + keys.map((k) => JSON.stringify(k) + ":" + canonicalJSON((v as Record<string, unknown>)[k])).join(",") + "}"
  );
}

function toFieldMap(v: unknown): Map<string, string> {
  const out = new Map<string, string>();
  if (v === null || typeof v !== "object") return out;
  for (const [k, x] of Object.entries(v as Record<string, unknown>)) {
    if (x === undefined) continue;
    const raw = canonicalJSON(x);
    if (raw === "null" || raw === "[]" || raw === "{}" || raw === '""' || raw === "false" || raw === "0") continue;
    out.set(k, raw);
  }
  return out;
}

function fieldDiff(a: unknown, b: unknown): string[] {
  const am = toFieldMap(a);
  const bm = toFieldMap(b);
  const keys = new Set([...am.keys(), ...bm.keys()]);
  return [...keys].filter((k) => am.get(k) !== bm.get(k)).sort();
}
