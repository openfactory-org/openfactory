// Built-in skills and the rules that pick and order the skills of a run. Port
// of internal/skills.
//
// A run gets skills from four sources. When two skills have the same name,
// the first source in this order wins: agent skills, factory skills,
// repository skills (found in the repositories by the harness), built-in
// skills (this module, from packages/core/skills/builtin).

import { compareBytes, cut } from "./gostd";
import { BUILTIN_SKILL_FILES } from "./skills-builtin.generated";
import type { AgentType, Skill } from "./types";

/** Skill scopes (Skill.scope). An agent skill has the agent name as its scope. */
export const SCOPE_FACTORY = "factory";
export const SCOPE_REPOSITORY = "repository";
export const SCOPE_BUILTIN = "builtin";

/** Built-in skill names. */
export const SKILL_GITHUB = "github";
export const SKILL_GITLAB = "gitlab";
export const SKILL_AZURE_DEVOPS = "azure-devops";
export const SKILL_CHAT = "chat";
export const SKILL_LINEAR = "linear";
export const SKILL_JIRA = "jira";
export const SKILL_FACTORY_DEFINITION = "factory-definition";

/**
 * The name and description of the YAML frontmatter of a SKILL.md. It reads
 * only simple top-level "key: value" lines.
 */
export function skillFrontmatter(content: string): { name: string; description: string } {
  const s = content.startsWith("﻿") ? content.slice(1) : content;
  if (!s.startsWith("---")) return { name: "", description: "" };
  const rest = s.slice(3);
  const end = rest.indexOf("\n---");
  if (end < 0) return { name: "", description: "" };
  let name = "";
  let description = "";
  for (const line of rest.slice(0, end).split("\n")) {
    const [k, v0, ok] = cut(line, ":");
    if (!ok || line.startsWith(" ") || line.startsWith("\t")) continue;
    const v = v0.trim().replace(/^["']+|["']+$/g, "");
    switch (k.trim()) {
      case "name":
        name = v;
        break;
      case "description":
        description = v;
        break;
    }
  }
  return { name, description };
}

const builtins: ReadonlyMap<string, Skill> = (() => {
  const out = new Map<string, Skill>();
  for (const [dir, content] of Object.entries(BUILTIN_SKILL_FILES)) {
    const fm = skillFrontmatter(content);
    const name = fm.name || dir;
    out.set(name, { name, description: fm.description, path: "", content, scope: SCOPE_BUILTIN });
  }
  return out;
})();

/** The built-in skill with the name, or undefined. */
export function builtinSkill(name: string): Skill | undefined {
  const s = builtins.get(name);
  return s ? { ...s } : undefined;
}

/** The names of the built-in skills, sorted. */
export function builtinSkillNames(): string[] {
  return [...builtins.keys()].sort(compareBytes);
}

/** What decides the built-in skills of a run. */
export interface SkillContext {
  agentType: AgentType | string;
  /** Code forges of the repositories of the run (RepoRef.codeForge; empty means GITHUB). */
  forges?: readonly string[];
  /** Providers that the factory connects (slack, microsoft_teams, linear, jira, ...). */
  integrations?: readonly string[];
  /** True for a run of a Self-improvement work item. */
  selfImprovement?: boolean;
}

/**
 * The built-in skills of a run: the skill of each forge for every agent; the
 * chat skill for the foreman when a chat integration is connected; the skill
 * of each connected tracker for the foreman, triage and spec agents; the
 * definition skill for a Self-improvement run.
 */
export function builtinSkillsForRun(c: SkillContext): Skill[] {
  const names: string[] = [];
  const add = (n: string): void => {
    if (!names.includes(n)) names.push(n);
  };
  for (const f of c.forges ?? []) {
    switch (f.trim().toUpperCase()) {
      case "":
      case "GITHUB":
        add(SKILL_GITHUB);
        break;
      case "GITLAB":
        add(SKILL_GITLAB);
        break;
      case "AZURE_DEVOPS":
      case "AZUREDEVOPS":
      case "AZURE-DEVOPS":
        add(SKILL_AZURE_DEVOPS);
        break;
    }
  }
  const talks = c.agentType === "FOREMAN";
  const tracks = talks || c.agentType === "TRIAGE" || c.agentType === "SPEC";
  for (const p of c.integrations ?? []) {
    switch (p.trim().replaceAll("-", "_").toLowerCase()) {
      case "slack":
      case "microsoft_teams":
      case "teams":
        if (talks) add(SKILL_CHAT);
        break;
      case "linear":
        if (tracks) add(SKILL_LINEAR);
        break;
      case "jira":
        if (tracks) add(SKILL_JIRA);
        break;
    }
  }
  if (c.selfImprovement) add(SKILL_FACTORY_DEFINITION);
  return names.flatMap((n) => {
    const s = builtinSkill(n);
    return s ? [s] : [];
  });
}

const skillKey = (name: string): string => name.trim().toLowerCase();

/**
 * The skills of a run in source order: agent, factory, repository, built-in.
 * A skill whose name (ignoring case) is in an earlier list is left out.
 */
export function mergeSkills(
  agent: readonly Skill[] = [],
  factory: readonly Skill[] = [],
  repository: readonly Skill[] = [],
  builtin: readonly Skill[] = [],
): Skill[] {
  const out: Skill[] = [];
  const seen = new Set<string>();
  for (const list of [agent, factory, repository, builtin]) {
    for (const s of list) {
      const k = skillKey(s.name);
      if (seen.has(k)) continue;
      seen.add(k);
      out.push(s);
    }
  }
  return out;
}

/** Separates the skills of an agent configuration into agent and factory skills by scope. */
export function splitSkills(list: readonly Skill[]): { agent: Skill[]; factory: Skill[] } {
  const agent: Skill[] = [];
  const factory: Skill[] = [];
  for (const s of list) (s.scope === SCOPE_FACTORY ? factory : agent).push(s);
  return { agent, factory };
}
