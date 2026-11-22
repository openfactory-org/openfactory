// Default agents and the default definition of a new factory. Port of
// internal/definition/defaults.go (LoadDir is in node.ts).

import { SCHEMA_VERSION } from "./common.ts";
import { DEFAULT_AGENT_FILES } from "./default-agents.generated.ts";
import { invalid } from "./errors.ts";
import { codeForgeValues, FACTORY_FILE } from "./factory.ts";
import { goQuote, goTrimSpace } from "./gofmt.ts";
import { yamlString } from "./yamlemit.ts";

/** DEFAULT_AGENT_NAMES lists the default agents in pipeline order. */
export const DEFAULT_AGENT_NAMES = ["foreman", "triage", "spec", "implement", "review"];

/** DEFAULT_MODEL is the factory-wide default model of a new factory. */
export const DEFAULT_MODEL = "claude-opus-5-5";

/** defaultAgents returns the default agent files keyed by definition path (agents/<name>/agent.md). */
export function defaultAgents(): Record<string, string> {
  return { ...DEFAULT_AGENT_FILES };
}

export interface DefaultDefinitionOptions {
  name: string;
  description?: string;
  alias?: string;
  /** The repositories as the caller gave them. An empty codeForge means GITHUB; the compiler validates the rest. */
  repositories?: { codeForge?: string; owner?: string; name?: string }[];
  /** Default DEFAULT_MODEL. */
  model?: string;
  /** Subset of DEFAULT_AGENT_NAMES; foreman is always included. Empty: all. */
  agents?: string[];
}

/**
 * defaultDefinition returns a complete, valid definition tree for a new
 * factory: factory.yaml and the selected default agents. It throws an
 * OfError (invalid) for a missing name or an unknown agent.
 */
export function defaultDefinition(opts: DefaultDefinitionOptions): Record<string, string> {
  if (goTrimSpace(opts.name) === "") throw invalid("factory name is required");
  const model = opts.model || DEFAULT_MODEL;
  const repos = opts.repositories ?? [];
  const forges = new Set(repos.map((r) => r.codeForge ?? ""));

  let y = "schemaVersion: " + yamlString(SCHEMA_VERSION, 4);
  y += "name: " + yamlString(opts.name, 4);
  if (opts.description) y += "description: " + yamlString(opts.description, 4);
  if (opts.alias) y += "alias: " + yamlString(opts.alias, 4);
  if (repos.length === 0) {
    y += "repositories: []\n";
  } else {
    y += "repositories:\n";
    for (const r of repos) {
      const cf = r.codeForge ?? "";
      let first = true;
      const line = (k: string, v: string): void => {
        y += (first ? "    - " : "      ") + k + ": " + yamlString(v, 10);
        first = false;
      };
      if (forges.size > 1 || (cf !== "" && cf !== "GITHUB")) line("codeForge", cf);
      line("owner", r.owner ?? "");
      line("name", r.name ?? "");
    }
  }
  y += "agentDefaults:\n    model: " + yamlString(model, 8);
  const files: Record<string, string> = { [FACTORY_FILE]: y };
  if (forges.size > 1 || (forges.size === 1 && !forges.has("GITHUB") && !forges.has(""))) {
    const list = codeForgeValues.filter((f) => forges.has(f));
    if (list.length > 0) files[FACTORY_FILE] += "codeForges:\n" + list.map((f) => "  - " + f + "\n").join("");
  }
  const want = new Set(["foreman"]);
  if ((opts.agents ?? []).length === 0) for (const n of DEFAULT_AGENT_NAMES) want.add(n);
  for (const n of opts.agents ?? []) {
    if (!DEFAULT_AGENT_NAMES.includes(n)) throw invalid(`unknown default agent ${goQuote(n)}`);
    want.add(n);
  }
  for (const [p, content] of Object.entries(DEFAULT_AGENT_FILES)) {
    const name = p.split("/")[1]!;
    if (want.has(name)) files[p] = content;
  }
  return files;
}
