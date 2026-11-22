// TeamDO module: settings (WS4). Team environments (internal/api/platform.go)
// and run-time settings that admins change (D31: TeamDO settings; deploy-time
// settings are Worker vars and secrets, see src/config.ts). Also the
// "resource in use" helpers that other modules reuse (MCP servers).
import { OfError, newId } from "@openfactory/core";
import type { AgentConfig, Automation } from "@openfactory/definition";
import { bodyObject, optString, optStringArray, optStringMap, type JsonObject } from "../../lib/body";
import { listResponse } from "../../lib/page";
import type { Migration } from "../../lib/sql/migrate";
import { all, first } from "../../lib/sql/query";
import { listAgents, listAutomations, listFactories } from "./factories";
import { created, noContent, type TeamKit, type TeamOp } from "./kit";

export const migrations: readonly Migration[] = [
  {
    version: 4,
    name: "settings",
    sql: `
CREATE TABLE environments (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX environments_name ON environments (name);
CREATE TABLE settings (k TEXT PRIMARY KEY, v TEXT NOT NULL, updated_ms INTEGER NOT NULL);
`,
  },
];

// ---------- settings ----------

export function getSetting<T>(kit: TeamKit, k: string): T | undefined {
  const r = first<{ v: string }>(kit.sql, "SELECT v FROM settings WHERE k = ?", k);
  return r ? (JSON.parse(r.v) as T) : undefined;
}

/** Writes a setting; call inside tx. */
export function setSetting(kit: TeamKit, k: string, v: unknown): void {
  if (v === undefined || v === null) {
    kit.sql.exec("DELETE FROM settings WHERE k = ?", k);
    return;
  }
  kit.sql.exec(
    "INSERT INTO settings (k, v, updated_ms) VALUES (?, ?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v, updated_ms = excluded.updated_ms",
    k,
    JSON.stringify(v),
    Date.now(),
  );
}

// ---------- in-use helpers ----------

/**
 * The parts of an active definition that reference team resources
 * (definitionForReferences in internal/api/platform.go): the stored agents,
 * automations and scorers, and the factory-wide mcpServers and scorerDefaults
 * of the compiled runtime.
 */
export interface ReferenceDefinition {
  agents: AgentConfig[];
  automations: Automation[];
  scorers: { slug: string; mcp_servers?: Record<string, { managedId?: string }> | null }[];
  mcpServers: Record<string, { managedId?: string }>;
  scorerDefaults?: { environment_id?: string | null; mcp_servers?: Record<string, { managedId?: string }> | null };
}

/** Places of the active definitions of the team that `uses` reports, as "<place> of factory <name>". */
export function teamResourceUses(kit: TeamKit, uses: (def: ReferenceDefinition) => string[]): string[] {
  const out: string[] = [];
  for (const f of listFactories(kit)) {
    if (!f.active_version_id) continue;
    const rt = first<{ data: string }>(kit.sql, "SELECT data FROM factory_runtime WHERE factory_id = ?", f.id);
    const runtime = rt
      ? (JSON.parse(rt.data) as {
          mcp_servers?: ReferenceDefinition["mcpServers"] | null;
          scorer_defaults?: ReferenceDefinition["scorerDefaults"] | null;
        })
      : {};
    const def: ReferenceDefinition = {
      agents: listAgents(kit, f.id),
      automations: listAutomations(kit, f.id),
      scorers: all<{ data: string }>(kit.sql, "SELECT data FROM scorers WHERE factory_id = ? ORDER BY ord", f.id).map(
        (r) => JSON.parse(r.data) as ReferenceDefinition["scorers"][number],
      ),
      mcpServers: runtime.mcp_servers ?? {},
      ...(runtime.scorer_defaults ? { scorerDefaults: runtime.scorer_defaults } : {}),
    };
    for (const u of uses(def)) out.push(`${u} of factory ${f.name}`);
  }
  return out;
}

/** environmentUses (platform.go): the agents, automations and scorerDefaults that select environment id. */
export function environmentUses(def: ReferenceDefinition, id: string): string[] {
  const out: string[] = [];
  for (const a of def.agents) if (a.environment_id === id) out.push("agent " + a.name);
  for (const au of def.automations) {
    const ov = au.overrides as { environmentId?: string | null; environment_id?: string | null } | undefined | null;
    if (ov?.environment_id === id || ov?.environmentId === id) out.push("automation " + au.name);
  }
  if (def.scorerDefaults?.environment_id === id) out.push("scorerDefaults");
  return out;
}

/** joinList: "a", "a and b", "a, b and c". */
export function joinList(items: string[]): string {
  if (items.length <= 1) return items[0] ?? "";
  return items.slice(0, -1).join(", ") + " and " + items[items.length - 1];
}

/** inUseError: conflict naming at most 5 places. */
export function inUseError(kind: string, name: string, uses: string[]): OfError | undefined {
  if (uses.length === 0) return undefined;
  const listed = uses.length > 5 ? [...uses.slice(0, 5), `${uses.length - 5} more`] : uses;
  const ref = uses.length > 1 ? "the references" : "the reference";
  return new OfError("conflict", `${kind} ${name} is used by ${joinList(listed)}. Remove ${ref} first.`);
}

// ---------- environments ----------

export interface Environment {
  id: string;
  team_id: string;
  name: string;
  description?: string;
  repos: string[];
  image?: string;
  setup_commands?: string[];
  env_vars?: Record<string, string>;
}

/** validateRepo: [forge:]owner/name or an http(s) repository URL. */
function validRepo(r: string): boolean {
  let ref = r;
  const se = r.indexOf("://");
  if (se >= 0) {
    try {
      const u = new URL(r);
      const scheme = r.slice(0, se);
      if ((scheme !== "https" && scheme !== "http") || !u.host) return false;
      ref = u.pathname.replace(/^\/+|\/+$/g, "").replace(/\.git$/, "");
    } catch {
      return false;
    }
  } else {
    const i = r.indexOf(":");
    if (i >= 0) {
      if (i === 0 || r.slice(0, i).includes("/")) return false;
      ref = r.slice(i + 1);
    }
  }
  const i = ref.lastIndexOf("/");
  return !(i <= 0 || i === ref.length - 1 || /[ \t]/.test(ref));
}

const IMAGE_REF_RE = new RegExp(
  "^" +
    "(?:(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\\[[0-9a-fA-F:]+\\])(?::[0-9]+)?/)?" +
    "[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*" +
    "(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?" +
    "(?:@[A-Za-z][A-Za-z0-9]*(?:[-_+.][A-Za-z][A-Za-z0-9]*)*:[0-9a-fA-F]{32,})?$",
);
const ENV_VAR_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

function validateFields(b: JsonObject): {
  repos?: string[];
  image?: string;
  env?: Record<string, string>;
  setup?: string[];
} {
  const repos = optStringArray(b, "repos");
  for (const r of repos ?? []) {
    if (!validRepo(r.trim()))
      throw new OfError(
        "invalid_request",
        `repo ${JSON.stringify(r)} must be [forge:]owner/name or an https:// repository URL`,
      );
  }
  let image = optString(b, "image");
  if (image !== undefined) {
    image = image.trim();
    if (image !== "" && !(image.length <= 512 && IMAGE_REF_RE.test(image))) {
      throw new OfError(
        "invalid_request",
        `image ${JSON.stringify(image)} is not a valid image reference (for example node:22-bookworm or registry.example.com/team/tools:1.2)`,
      );
    }
  }
  const env = optStringMap(b, "env_vars");
  for (const k of Object.keys(env ?? {}).sort()) {
    if (!ENV_VAR_RE.test(k)) {
      throw new OfError(
        "invalid_request",
        `env_vars: ${JSON.stringify(k)} is not a valid variable name (letters, digits and _, not starting with a digit)`,
      );
    }
    if (k.toUpperCase().startsWith("OF_")) {
      throw new OfError(
        "invalid_request",
        `env_vars: ${JSON.stringify(k)} starts with OF_, which is reserved for variables that the platform sets`,
      );
    }
  }
  const setup = optStringArray(b, "setup_commands");
  return {
    ...(repos ? { repos } : {}),
    ...(image !== undefined ? { image } : {}),
    ...(env ? { env } : {}),
    ...(setup ? { setup } : {}),
  };
}

export function listEnvironments(kit: TeamKit): Environment[] {
  return all<{ data: string }>(kit.sql, "SELECT data FROM environments ORDER BY created_ms, id").map(
    (r) => JSON.parse(r.data) as Environment,
  );
}

export function getEnvironment(kit: TeamKit, id: string): Environment | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM environments WHERE id = ?", id);
  return r ? (JSON.parse(r.data) as Environment) : undefined;
}

function environmentOf(kit: TeamKit, id: string): Environment {
  const e = getEnvironment(kit, id);
  if (!e) throw new OfError("resource_not_found", `environment ${id} not found`);
  return e;
}

function checkNameFree(kit: TeamKit, name: string, exceptId: string): void {
  const e = listEnvironments(kit).find((x) => x.id !== exceptId && x.name.toLowerCase() === name.toLowerCase());
  if (e) throw new OfError("conflict", `an environment named ${JSON.stringify(e.name)} already exists`);
}

function putEnvironment(kit: TeamKit, e: Environment, createdMs = Date.now()): void {
  kit.sql.exec(
    "INSERT INTO environments (id, name, created_ms, data) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, data = excluded.data",
    e.id,
    e.name,
    createdMs,
    JSON.stringify(e),
  );
}

function clean(e: Environment): Environment {
  const out: Environment = { id: e.id, team_id: e.team_id, name: e.name, repos: e.repos ?? [] };
  if (e.description) out.description = e.description;
  if (e.image) out.image = e.image;
  if (e.setup_commands?.length) out.setup_commands = e.setup_commands;
  if (e.env_vars && Object.keys(e.env_vars).length) out.env_vars = e.env_vars;
  // Go field order: id, team_id, name, description, repos, image, setup_commands, env_vars.
  return {
    id: out.id,
    team_id: out.team_id,
    name: out.name,
    ...(out.description ? { description: out.description } : {}),
    repos: out.repos,
    ...(out.image ? { image: out.image } : {}),
    ...(out.setup_commands ? { setup_commands: out.setup_commands } : {}),
    ...(out.env_vars ? { env_vars: out.env_vars } : {}),
  };
}

export const ops: Record<string, TeamOp> = {
  listEnvironments: (kit) => listResponse(listEnvironments(kit), ""),

  createEnvironment: (kit, c) => {
    const b = bodyObject(c.body);
    const name = optString(b, "name")?.trim() ?? "";
    if (name === "") throw new OfError("invalid_request", "name is required");
    const v = validateFields(b);
    return kit.tx(() => {
      checkNameFree(kit, name, "");
      const desc = optString(b, "description");
      const e = clean({
        id: newId("env"),
        team_id: kit.teamId(),
        name,
        ...(desc ? { description: desc } : {}),
        repos: v.repos ?? [],
        ...(v.image ? { image: v.image } : {}),
        ...(v.setup ? { setup_commands: v.setup } : {}),
        ...(v.env ? { env_vars: v.env } : {}),
      });
      putEnvironment(kit, e);
      return created(e);
    });
  },

  getEnvironment: (kit, c) => environmentOf(kit, c.params["id"]!),

  updateEnvironment: (kit, c) => {
    const b = bodyObject(c.body);
    const v = validateFields(b);
    return kit.tx(() => {
      const e = environmentOf(kit, c.params["id"]!);
      const name = optString(b, "name");
      if (name !== undefined) {
        if (name.trim() === "") throw new OfError("invalid_request", "name cannot be empty");
        checkNameFree(kit, name.trim(), e.id);
        e.name = name.trim();
      }
      const desc = optString(b, "description");
      if (desc !== undefined) e.description = desc;
      if (v.repos) e.repos = v.repos;
      if (v.image !== undefined) e.image = v.image;
      if (v.setup) e.setup_commands = v.setup;
      if (v.env) e.env_vars = v.env;
      const out = clean(e);
      const cur = first<{ created_ms: number }>(kit.sql, "SELECT created_ms FROM environments WHERE id = ?", e.id);
      putEnvironment(kit, out, cur?.created_ms);
      return out;
    });
  },

  deleteEnvironment: (kit, c) =>
    kit.tx(() => {
      const e = environmentOf(kit, c.params["id"]!);
      const uses = teamResourceUses(kit, (def) => environmentUses(def, e.id));
      const err = inUseError("Environment", e.name, uses);
      if (err) throw err;
      kit.sql.exec("DELETE FROM environments WHERE id = ?", e.id);
      return noContent();
    }),

  /** Internal: read a team setting. */
  settingsGet: (kit, c) => ({ value: getSetting(kit, String(bodyObject(c.body)["key"] ?? "")) ?? null }),
};

export function setup(_kit: TeamKit): void {}
