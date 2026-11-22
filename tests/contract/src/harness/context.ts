// Shared fixtures: the admin client, definition trees and factories.
import { readdirSync, readFileSync } from "node:fs";
import { join, relative } from "node:path";
import { inject } from "vitest";
import { Api, uniq } from "./api.ts";
import type { AdminContext } from "./global-setup.ts";
import { repoPath } from "./paths.ts";

export function adminContext(): AdminContext {
  const a = inject("admin");
  if (!a)
    throw new Error("the target was not bootstrapped (see the contract setup output): POST /api/v1/bootstrap failed");
  return a;
}

/** The API client with the admin key of the bootstrap. */
export function adminApi(): Api {
  return new Api(adminContext().key);
}

/** Reads a definition tree (all files except README.md) as {relative path: content}. */
export function readTree(rel: string): Record<string, string> {
  const root = repoPath(rel);
  const files: Record<string, string> = {};
  const walk = (dir: string): void => {
    for (const e of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, e.name);
      if (e.isDirectory()) walk(p);
      else if (e.name !== "README.md") files[relative(root, p).split("\\").join("/")] = readFileSync(p, "utf8");
    }
  };
  walk(root);
  return files;
}

/** The example definition directories (examples/NN-*). */
export function exampleDirs(): string[] {
  return readdirSync(repoPath("examples"), { withFileTypes: true })
    .filter((e) => e.isDirectory() && /^\d\d-/.test(e.name))
    .map((e) => e.name)
    .sort();
}

/** A minimal managed definition: one foreman, optional extra agents and factory.yaml lines. */
export function minimalDefinition(opts: {
  name: string;
  agents?: Record<string, { type: string; description?: string; extra?: string }>;
  factoryExtra?: string;
}): Record<string, string> {
  const files: Record<string, string> = {
    "factory.yaml":
      `schemaVersion: v1\nname: ${opts.name}\nalias: ${opts.name}\ndescription: Contract test factory.\n` +
      `repositories: []\nagentDefaults:\n  model: claude-sonnet-5-5\n${opts.factoryExtra ?? ""}`,
    "agents/foreman/agent.md":
      "---\ndescription: Foreman of the contract tests\nagentType: FOREMAN\n---\n\nDo exactly what the prompt says.\n",
  };
  for (const [name, a] of Object.entries(opts.agents ?? {})) {
    files[`agents/${name}/agent.md`] =
      `---\ndescription: ${a.description ?? `${name} agent`}\nagentType: ${a.type}\n${a.extra ?? ""}---\n\nDo exactly what the prompt says.\n`;
  }
  return files;
}

export interface CreatedFactory {
  uid: string;
  name: string;
  activeVersionId?: string;
}

/** Creates a managed factory from definition files and returns its UID. */
export async function createFactory(api: Api, files: Record<string, string>): Promise<CreatedFactory> {
  const body = await api.post<{ factory: { uid: string; name: string; active_version_id?: string } }>(
    "/factories",
    { files },
    201,
  );
  return { uid: body.factory.uid, name: body.factory.name, activeVersionId: body.factory.active_version_id };
}

/** Creates a factory with the minimal definition and the given extra agents. */
export async function createTestFactory(
  api: Api,
  agents?: Record<string, { type: string; description?: string; extra?: string }>,
  factoryExtra?: string,
): Promise<CreatedFactory> {
  return createFactory(api, minimalDefinition({ name: uniq("ct"), agents, factoryExtra }));
}

/** Secrets that the example definitions reference. Values are clearly fake. */
export const EXAMPLE_SECRETS: { name: string; type: string }[] = [
  { name: "ANTHROPIC_API_KEY", type: "anthropic_api_key" },
  { name: "OPENAI_API_KEY", type: "openai_api_key" },
  { name: "NPM_TOKEN", type: "generic" },
  { name: "DATADOG_API_KEY", type: "generic" },
  { name: "DATADOG_APP_KEY", type: "generic" },
  { name: "PAGERDUTY_WEBHOOK_SECRET", type: "generic" },
  { name: "SENTRY_WEBHOOK_SECRET", type: "generic" },
];

/** Creates the team secrets of EXAMPLE_SECRETS unless they exist. */
export async function ensureExampleSecrets(api: Api): Promise<void> {
  for (const s of EXAMPLE_SECRETS) {
    const res = await api.request("POST", "/secrets", {
      body: { name: s.name, type: s.type, value: `contract-test-fake-${s.name.toLowerCase()}` },
    });
    if (res.status !== 201 && res.status !== 409) {
      throw new Error(`create secret ${s.name}: status ${res.status}: ${res.text.slice(0, 300)}`);
    }
  }
}
