import { readdirSync, readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
// @ts-expect-error: a plain .mjs script without types
import { render } from "../scripts/gen-skills.mjs";
import {
  builtinSkill,
  builtinSkillNames,
  builtinSkillsForRun,
  composeSystemPrompt,
  mergeSkills,
  SCOPE_BUILTIN,
  SCOPE_FACTORY,
  SCOPE_REPOSITORY,
  skillDir,
  skillFrontmatter,
  splitSkills,
  type AgentConfig,
  type Run,
  type Skill,
  type WorkItem,
} from "../src/index";

function agent(p: Partial<AgentConfig>): AgentConfig {
  return {
    id: "",
    factory_id: "fac",
    name: "",
    description: "",
    type: "CUSTOM",
    instructions: "",
    harness: { type: "claude", model: "" },
    network_policy: { restricted: false, allowed_hosts: null, denied_hosts: null },
    spawnable_by: null,
    ...p,
  };
}

const roster = (): AgentConfig[] => [
  agent({ id: "agt_f", name: "foreman", type: "FOREMAN" }),
  agent({ id: "agt_t", name: "triage", type: "TRIAGE", description: "Investigates\nrequests" }),
  agent({ id: "agt_i", name: "implement", type: "IMPLEMENT", description: "Writes code" }),
  agent({ id: "agt_d", name: "docs", type: "CUSTOM", description: "Docs", spawnable_by: ["implement"] }),
  agent({ id: "agt_n", name: "locked", type: "CUSTOM", spawnable_by: [] }),
];

describe("composeSystemPrompt", () => {
  it("composes the foreman prompt", () => {
    const cfg = agent({
      id: "agt_f",
      name: "foreman",
      type: "FOREMAN",
      instructions: "Always be kind.",
      skills: [{ name: "github", description: "Use gh", path: "/workspace/.openfactory/skills/github/SKILL.md", content: "", scope: "" }],
    });
    const wi = {
      id: "wi_1",
      title: "PO number",
      stage: "triage",
      source: { provider: "github", kind: "issue", repo: "acme/api", number: 7, url: "https://github.com/acme/api/issues/7" },
      pull_requests: [{ url: "https://github.com/acme/api/pull/42", branch: "po", forge: "", repo: "", number: 0, state: "" }],
    } as WorkItem;
    const run = { id: "run_1", name: "foreman", metadata: { setup_profile: "backend" } } as Partial<Run> as Run;
    const out = composeSystemPrompt(cfg, wi, run, roster());
    for (const want of [
      "Role: foreman",
      "run_agents",
      "list_agents",
      "request_approval",
      "untrusted input",
      "**triage** (TRIAGE) — agent_identity_uid: `agt_t` — Investigates requests",
      "`agt_i`",
      "PO number",
      "acme/api #7",
      "https://github.com/acme/api/pull/42 (open, branch po)",
      "setup_profile = backend",
      "**github** — Use gh (`/workspace/.openfactory/skills/github/SKILL.md`)",
      "# Agent instructions\n\nAlways be kind.",
    ]) {
      expect(out).toContain(want);
    }
    for (const notWant of ["`agt_d`", "`agt_n`", "`agt_f`", "Default policy", "Never merge", "Spec approval"]) {
      expect(out).not.toContain(notWant);
    }
    expect(out.indexOf("Role: foreman")).toBeLessThan(out.indexOf("# Agent instructions"));
    expect(out.endsWith("Always be kind.\n")).toBe(true);
  });

  it("composes a stage agent prompt", () => {
    const out = composeSystemPrompt(agent({ id: "agt_i", name: "implement", type: "IMPLEMENT", instructions: "Use TDD." }), null, null, roster());
    for (const want of ["Role: implement agent (implement)", "final message is delivered to the foreman", "link_pull_request", "untrusted input", "`agt_d`", "Use TDD."]) {
      expect(out).toContain(want);
    }
    expect(out).not.toContain("`agt_t`");
    const custom = composeSystemPrompt(agent({ name: "docs", type: "CUSTOM", description: "Updates reference pages" }), null, null, roster());
    expect(custom).toContain("Your responsibility: Updates reference pages");
  });

  it("adds no workflow policy of its own", () => {
    const out = composeSystemPrompt(
      agent({ name: "foreman", type: "FOREMAN", instructions: "Dispatch implement for every request. Hand off the pull request." }),
      null,
      null,
      roster(),
    );
    const low = out.toLowerCase();
    for (const notWant of ["spec approval", "approval of the spec", "obtain explicit human approval", "never merge", "only after a merge"]) {
      expect(low).not.toContain(notWant);
    }
    expect(out).toContain("Dispatch implement for every request.");
  });

  it("builds skill directories", () => {
    expect(skillDir("github")).toBe(".openfactory/skills/github");
    expect(skillDir("my skill/..")).toBe(".openfactory/skills/my-skill");
    expect(skillDir("../../etc")).toBe(".openfactory/skills/etc");
    expect(skillDir("")).toBe(".openfactory/skills/skill");
  });
});

const names = (list: Skill[]): string => list.map((s) => s.name).join(",");

describe("skills", () => {
  it("has the built-in skills", () => {
    expect(builtinSkillNames().join(",")).toBe("azure-devops,chat,factory-definition,github,gitlab,jira,linear");
    for (const n of builtinSkillNames()) {
      const s = builtinSkill(n)!;
      expect(s.description).not.toBe("");
      expect(s.scope).toBe(SCOPE_BUILTIN);
      expect(s.content.startsWith(`---\nname: ${n}\n`)).toBe(true);
    }
    expect(builtinSkill("nope")).toBeUndefined();
  });

  it("keeps the generated module in step with the SKILL.md files", () => {
    expect(readFileSync(new URL("../src/skills-builtin.generated.ts", import.meta.url), "utf8")).toBe(render());
    expect(readdirSync(new URL("../skills/builtin", import.meta.url)).sort()).toEqual(builtinSkillNames());
  });

  it("picks the built-in skills of a run", () => {
    const ints = ["slack", "linear", "jira"];
    expect(names(builtinSkillsForRun({ agentType: "FOREMAN", forges: ["GITHUB", ""], integrations: ints }))).toBe("github,chat,linear,jira");
    expect(names(builtinSkillsForRun({ agentType: "TRIAGE", forges: ["GITLAB"], integrations: ints }))).toBe("gitlab,linear,jira");
    expect(names(builtinSkillsForRun({ agentType: "IMPLEMENT", forges: ["AZURE_DEVOPS"], integrations: ints }))).toBe("azure-devops");
    expect(names(builtinSkillsForRun({ agentType: "REVIEW", selfImprovement: true }))).toBe("factory-definition");
    expect(names(builtinSkillsForRun({ agentType: "FOREMAN", integrations: ["microsoft_teams"] }))).toBe("chat");
  });

  it("merges in source order", () => {
    const s = (name: string, scope: string): Skill => ({ name, scope, description: "", path: "", content: "" });
    const got = mergeSkills(
      [s("deploy", "implement")],
      [s("Deploy", SCOPE_FACTORY), s("checks", SCOPE_FACTORY)],
      [s("checks", SCOPE_REPOSITORY), s("github", SCOPE_REPOSITORY)],
      [s("github", SCOPE_BUILTIN), s("chat", SCOPE_BUILTIN)],
    );
    expect(got.map((x) => `${x.name}=${x.scope}`).join(",")).toBe("deploy=implement,checks=factory,github=repository,chat=builtin");
    const split = splitSkills([s("a", "implement"), s("b", SCOPE_FACTORY)]);
    expect(names(split.agent)).toBe("a");
    expect(names(split.factory)).toBe("b");
  });

  it("reads frontmatter", () => {
    expect(skillFrontmatter('﻿---\nname: x\ndescription: "Does y."\nmetadata:\n  name: nested\n---\nbody')).toEqual({
      name: "x",
      description: "Does y.",
    });
    expect(skillFrontmatter("no frontmatter").name).toBe("");
  });
});
