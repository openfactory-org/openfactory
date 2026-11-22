// TeamDO module: factories. The factory routes (create, update, delete, list)
// and the composition of the module:
//
//   repo.ts            factory records, versions, runtime and resource tables
//   definitions.ts     definition check and apply, versions routes, proposals
//   dashboard-edit.ts  agent spec, automation files, settings, integrations
//   yaml-edit.ts       YAML and Markdown frontmatter editing
//
// The built-in preflight checks (alias, secrets, environments) are in
// ../preflight.ts; other modules declare their checks in their `preflights`
// export and TeamDO hands them over in kit.preflights.
import { OfError, newId } from "@openfactory/core";
import { compile, defaultDefinition, DEFAULT_AGENT_NAMES, type AgentConfig } from "@openfactory/definition";
import { actorId, roleRank } from "../../../auth/roles";
import { bodyObject, optString, str, type JsonObject } from "../../../lib/body";
import { listResponse, msOf, pageFrom, paginate } from "../../../lib/page";
import { first } from "../../../lib/sql/query";
import { syncSchedules } from "../schedules";
import { syncWebhooks } from "../webhooks";
import { created, noContent, type TeamOp } from "../kit";
import { dashboardOps, INTEGRATION_TYPES } from "./dashboard-edit";
import {
  applyDefinition,
  applyFiles,
  definitionOps,
  gitSource,
  validationError,
  type ApplyResult,
  type CompileResult,
} from "./definitions";
import {
  activeFiles,
  activeRuntime,
  agentOut,
  aliasFromName,
  checkAliasFree,
  deleteFactoryRows,
  factoryFor,
  factoryView,
  findFactory,
  getFactoryById,
  getFactoryIntegrations,
  listAgents,
  listAutomations,
  listFactories,
  putFactory,
  validAlias,
  type DefinitionVersion,
  type FactoryRecord,
} from "./repo";
import { editYamlDocument } from "./yaml-edit";

export { migrations } from "./repo";
// Names that files outside do/team import from this path.
export { activeFiles, getFactoryById, getVersion, listFactories, type FactoryRecord } from "./repo";
export {
  applyDefinition,
  checkDefinition,
  configSnapshot,
  setDefSyncRunner,
  setGitSourceChecker,
  type DefSyncRunner,
  type GitSourceChecker,
} from "./definitions";

type RepoForge = NonNullable<NonNullable<Parameters<typeof defaultDefinition>[0]["repositories"]>[number]["codeForge"]>;

const CODE_FORGES = ["GITHUB", "GITLAB", "AZURE_DEVOPS"];

function resolveCreateFiles(
  b: JsonObject,
  source: string,
  name: string,
  alias: string,
  description: string,
): Record<string, string> | null {
  const files = b["files"];
  if (files && typeof files === "object" && Object.keys(files).length > 0) return files as Record<string, string>;
  let useDefaults = source === "managed";
  const d = b["definition"];
  if (d !== undefined && d !== null) {
    if (typeof d === "string") {
      if (d !== "defaults")
        throw new OfError("invalid_request", `definition must be "defaults" or an object with "files"`);
      useDefaults = true;
    } else {
      const fs = (d as { files?: unknown }).files;
      if (typeof d !== "object" || !fs || typeof fs !== "object" || Object.keys(fs).length === 0) {
        throw new OfError("invalid_request", `definition must be "defaults" or an object with a non-empty "files" map`);
      }
      return fs as Record<string, string>;
    }
  }
  if (!useDefaults) return null;
  let agents: string[] | undefined;
  const ra = b["agents"];
  if (Array.isArray(ra)) {
    agents = [];
    for (const a0 of ra) {
      const a = String(a0).trim().toLowerCase();
      if (a === "foreman" || a === "main") continue;
      if (!(DEFAULT_AGENT_NAMES as readonly string[]).includes(a)) {
        throw new OfError(
          "invalid_request",
          `unknown default agent ${JSON.stringify(a)} (choose from ${DEFAULT_AGENT_NAMES.join(", ")})`,
        );
      }
      if (!agents.includes(a)) agents.push(a);
    }
  }
  const integrations = Array.isArray(b["integrations"]) ? (b["integrations"] as { type?: string }[]) : [];
  let trackers = 0;
  for (const it of integrations) {
    let t = String(it?.type ?? "")
      .trim()
      .toLowerCase();
    if (t === "teams" || t === "microsoft_teams") t = "microsoft-teams";
    if (!INTEGRATION_TYPES.some((x) => x.type === t)) {
      throw new OfError(
        "invalid_request",
        `unknown integration type ${JSON.stringify(it?.type ?? "")} (choose from ${INTEGRATION_TYPES.map((x) => x.type).join(", ")})`,
      );
    }
    if (t === "linear" || t === "jira") trackers++;
  }
  if (trackers > 1)
    throw new OfError("invalid_request", "linear and jira are mutually exclusive: declare at most one issue tracker");
  const repos = Array.isArray(b["repositories"])
    ? (b["repositories"] as { owner?: string; name?: string; codeForge?: string }[])
    : [];
  repos.forEach((r, i) => {
    if (!String(r?.owner ?? "").trim() || !String(r?.name ?? "").trim()) {
      throw new OfError("invalid_request", `repositories[${i}]: owner and name are required`);
    }
    if (r.codeForge && !CODE_FORGES.includes(r.codeForge.toUpperCase())) {
      throw new OfError("invalid_request", `repositories[${i}]: codeForge must be one of ${CODE_FORGES.join(", ")}`);
    }
  });
  if (!name) throw new OfError("invalid_request", "name is required");
  return defaultDefinition({
    name,
    alias: alias || aliasFromName(name),
    ...(description ? { description } : {}),
    // An empty codeForge is the same as none (GITHUB).
    repositories: repos.map((r) => ({
      owner: String(r.owner),
      name: String(r.name),
      ...(r.codeForge ? { codeForge: r.codeForge as RepoForge } : {}),
    })),
    ...(str(b, "model") ? { model: str(b, "model") } : {}),
    ...(agents ? { agents } : {}),
  });
}

// ---------- ops ----------

const factoryOps: Record<string, TeamOp> = {
  listFactories: (kit, c) => {
    const page = pageFrom(c.query);
    const q = (c.query["search"] ?? "").trim().toLowerCase();
    let fs = listFactories(kit);
    if (q) fs = fs.filter((f) => f.name.toLowerCase().includes(q) || f.alias.toLowerCase().includes(q));
    const p = paginate(fs, (f) => ({ ms: msOf(f.created_at), id: f.id }), page);
    return listResponse(
      p.items.map((f) => factoryView(kit, f)),
      p.next,
    );
  },

  getFactory: (kit, c) => factoryView(kit, factoryFor(kit, c)),

  createFactory: async (kit, c) => {
    const p = c.principal();
    if (roleRank(p.role) < roleRank("editor"))
      throw new OfError("not_authorized", "creating a factory requires the team-wide editor role");
    const b = bodyObject(c.body);
    let name = str(b, "name").trim();
    let alias = str(b, "alias").trim();
    let description = str(b, "description");
    const source = str(b, "source") || "managed";
    if (source !== "managed" && source !== "git")
      throw new OfError("invalid_request", `source must be "managed" or "git"`);
    if (source === "git" && str(b, "git_repo").trim() === "")
      throw new OfError("invalid_request", "git_repo is required for a git-backed definition");
    const files = resolveCreateFiles(b, source, name, alias, description);
    // Compiled once here; applyDefinition reuses the result.
    let compiled: CompileResult | undefined;
    if (files && Object.keys(files).length > 0) {
      const res = (compiled = compile(files));
      if (res.errors.length > 0) throw validationError("definition is invalid", res.errors);
      const def = res.definition;
      if (def) {
        name ||= def.name;
        alias ||= def.alias ?? "";
        description ||= def.description ?? "";
      }
    }
    if (name === "") throw new OfError("invalid_request", "name is required");
    if ([...name].length > 100) throw new OfError("invalid_request", "name must be at most 100 characters");
    if (alias === "") alias = aliasFromName(name);
    if (!validAlias(alias))
      throw new OfError("invalid_request", "alias accepts up to 60 letters, numbers, spaces, '.', '_' and '-'");
    const now = new Date().toISOString();
    const f: FactoryRecord = {
      id: newId("fac"),
      team_id: kit.teamId(),
      name,
      alias,
      ...(description ? { description } : {}),
      source,
      created_at: now,
      updated_at: now,
    };
    if (source === "git") {
      f.git_repo = str(b, "git_repo").trim();
      f.git_branch = str(b, "git_branch").trim() || "main";
      const gp = str(b, "git_path")
        .trim()
        .replace(/^\/+|\/+$/g, "");
      if (gp) f.git_path = gp;
    }
    kit.tx(() => {
      checkAliasFree(kit, alias, "");
      putFactory(kit, f);
    });
    // The branch of a git source must compile and pass the preflight, as for
    // a change of the source. The check needs the factory record (its team),
    // so a refusal removes the record again.
    const checkGit = gitSource();
    if (f.source === "git" && checkGit) {
      try {
        await checkGit(kit, f, "the factory was not created");
      } catch (e) {
        kit.tx(() => deleteFactoryRows(kit, f.id));
        throw e;
      }
    }
    let version: DefinitionVersion | undefined;
    if (files && Object.keys(files).length > 0) {
      let r: ApplyResult;
      try {
        r = await applyDefinition(kit, f.id, {
          files,
          author: actorId(p),
          message: "Create factory",
          ...(compiled ? { compiled } : {}),
        });
      } catch (e) {
        kit.tx(() => deleteFactoryRows(kit, f.id));
        throw e;
      }
      if (r.invalid) {
        kit.tx(() => deleteFactoryRows(kit, f.id));
        throw validationError("definition could not be applied", r.version.errors);
      }
      version = r.version;
    }
    const nf = getFactoryById(kit, f.id) ?? f;
    return created({ factory: factoryView(kit, nf), ...(version ? { version } : {}) });
  },

  updateFactory: async (kit, c) => {
    const f = factoryFor(kit, c);
    const wasSource = f.source;
    const b = bodyObject(c.body);
    const yamlEdits: Record<string, unknown> = {};
    const name = optString(b, "name");
    if (name !== undefined) {
      const n = name.trim();
      if (n === "" || [...n].length > 100) throw new OfError("invalid_request", "name must be 1-100 characters");
      f.name = n;
      yamlEdits["name"] = n;
    }
    const alias = optString(b, "alias");
    if (alias !== undefined) {
      const a = alias.trim();
      if (!validAlias(a))
        throw new OfError("invalid_request", "alias accepts up to 60 letters, numbers, spaces, '.', '_' and '-'");
      checkAliasFree(kit, a, f.id);
      f.alias = a;
      yamlEdits["alias"] = a;
    }
    const desc = optString(b, "description");
    if (desc !== undefined) {
      const d = desc.trim();
      if (d) f.description = d;
      else delete f.description;
      yamlEdits["description"] = d || null;
    }
    const cs0 = optString(b, "credential_strategy");
    if (cs0 !== undefined) {
      const cs = cs0.trim().toUpperCase();
      if (cs !== "" && cs !== "EXECUTOR" && cs !== "CREATOR")
        throw new OfError("invalid_request", "credential_strategy must be EXECUTOR or CREATOR");
      yamlEdits["credentialStrategy"] = cs || null;
      if (f.source !== "managed")
        throw new OfError(
          "operation_not_supported",
          "credentialStrategy of a git-backed factory is set in its factory.yaml",
        );
    }
    const source = optString(b, "source");
    const gitRepo = optString(b, "git_repo");
    const gitBranch = optString(b, "git_branch");
    const gitPath = optString(b, "git_path");
    if (source !== undefined) {
      if (source === "managed") f.source = "managed";
      else if (source === "git") {
        const repo = gitRepo !== undefined ? gitRepo.trim() : (f.git_repo ?? "");
        if (!repo) throw new OfError("invalid_request", "git_repo is required to switch the definition source to git");
        f.source = "git";
        f.git_branch ||= "main";
      } else throw new OfError("invalid_request", "source must be managed or git");
    }
    if (gitPath !== undefined) {
      const gp = gitPath.trim().replace(/^\/+|\/+$/g, "");
      if (gp.includes(".."))
        throw new OfError("invalid_request", "git_path must be a relative directory inside the repository");
    }
    if (gitRepo !== undefined || gitBranch !== undefined || gitPath !== undefined) {
      if (f.source !== "git")
        throw new OfError("operation_not_supported", "git settings apply only to git-backed factories");
      if (gitRepo !== undefined) {
        if (gitRepo.trim() === "") throw new OfError("invalid_request", "git_repo cannot be empty");
        f.git_repo = gitRepo.trim();
      }
      if (gitBranch !== undefined) f.git_branch = gitBranch.trim() || "main";
      if (gitPath !== undefined) f.git_path = gitPath.trim().replace(/^\/+|\/+$/g, "");
    }
    // Before the source becomes git (or its location changes), the branch
    // must compile and pass the preflight.
    const checkGit = gitSource();
    if (
      f.source === "git" &&
      checkGit &&
      (wasSource !== "git" || gitRepo !== undefined || gitBranch !== undefined || gitPath !== undefined)
    )
      await checkGit(kit, f, "the source did not change");
    let out = f;
    if (f.source === "managed" && f.active_version_id && Object.keys(yamlEdits).length > 0) {
      const files = await activeFiles(kit, f);
      files["factory.yaml"] = editYamlDocument(files["factory.yaml"] ?? "", yamlEdits);
      await applyFiles(kit, c, f, files, "Update factory settings");
      const nf = getFactoryById(kit, f.id)!;
      out = { ...nf, name: f.name, alias: f.alias, source: f.source };
      if (f.description) out.description = f.description;
      else delete out.description;
      for (const k of ["git_repo", "git_branch", "git_path"] as const) {
        if (f[k]) out[k] = f[k];
      }
    }
    out.updated_at = new Date().toISOString();
    kit.tx(() => putFactory(kit, out));
    return factoryView(kit, out);
  },

  deleteFactory: (kit, c) => {
    const f = factoryFor(kit, c);
    kit.tx(() => {
      deleteFactoryRows(kit, f.id);
      syncSchedules(kit, f.id, []);
      syncWebhooks(kit, f.id, []);
    });
    return noContent();
  },

  listAgents: (kit, c) => {
    const f = factoryFor(kit, c);
    let as = listAgents(kit, f.id);
    const t = c.query["type"];
    if (t) as = as.filter((a) => a.type.toLowerCase() === t.toLowerCase());
    return listResponse(as, "");
  },

  getAgent: (kit, c) => {
    const f = factoryFor(kit, c);
    const name = c.params["name"]!;
    const r =
      first<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? AND name = ?", f.id, name) ??
      first<{ data: string }>(kit.sql, "SELECT data FROM agents WHERE factory_id = ? AND id = ?", f.id, name);
    if (!r) throw new OfError("resource_not_found", `agent ${name} not found`);
    return agentOut(JSON.parse(r.data) as AgentConfig);
  },

  listAutomations: (kit, c) => listResponse(listAutomations(kit, factoryFor(kit, c).id), ""),

  getFactoryIntegrations: (kit, c) => {
    const f = factoryFor(kit, c);
    return { factory_id: f.id, integrations: getFactoryIntegrations(kit, f.id) };
  },

  /** Internal (GitHub webhook routing): the factories of the team (core.Factory JSON). */
  forgeFactories: (kit) => ({ items: listFactories(kit) }),

  /** Internal: factories of the team (plugin host). */
  "factories.list": (kit) =>
    listFactories(kit).map((f) => ({ id: f.id, teamId: f.team_id, alias: f.alias, name: f.name })),

  /** Internal: one factory and its repositories (plugin host). Body {factory_id}. */
  "factories.get": (kit, c) => {
    const ref = str(bodyObject(c.body), "factory_id");
    const f = findFactory(kit, ref);
    if (!f) throw new OfError("resource_not_found", `factory ${ref} not found`);
    const repos = activeRuntime(kit, f.id)?.repositories ?? [];
    return { id: f.id, teamId: f.team_id, alias: f.alias, name: f.name, repositories: repos };
  },
};

export const ops: Record<string, TeamOp> = { ...factoryOps, ...definitionOps, ...dashboardOps };
