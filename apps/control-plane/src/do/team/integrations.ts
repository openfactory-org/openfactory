// TeamDO module: integrations. The integration records of a team,
// the REST handlers of /integrations, and the internal ops that the Worker
// side of the chat and tracker integrations calls (store reads, account
// links, Teams service URLs). Migrations 0100-0149.
//
// Internal ops (not routes, so no role check; only the Worker calls them):
//   integrations.list      query ?provider        -> Integration[]
//   integrations.get       params id              -> Integration | null
//   integrationsForProvider                        alias of integrations.list
//   integrationsSetStatus  body {id,status,reason} -> {changed}
//   identity.find          params provider, external_id -> ExternalIdentity | null
//   identity.link          body ExternalIdentity   -> {linked}
//   identity.upsert        body ExternalIdentity   -> {}
//   identity.user          params id               -> User | null
//   identity.userByEmail   params email            -> User | null
//   factorySettings        params factory_id       -> {provider: {key: value}}
//   kv.get / kv.set        params key (, value)
import {
  INTEGRATION_ACTIVE,
  INTEGRATION_DISABLED,
  INTEGRATION_STATUSES,
  OfError,
  integrationStatus,
  integrationWorkspace,
  integrationWorkspaceKey,
  newId,
  normalizeWorkspace,
} from "@openfactory/core";
import type { ExternalIdentity, Integration, IntegrationStatus, User } from "@openfactory/core";
import { bodyObject, optString, optStringMap, str } from "../../lib/body";
import { listResponse } from "../../lib/page";
import { API_NAME_RE } from "../../secrets/validate";
import type { Migration } from "../../lib/sql/migrate";
import { all, first, run } from "../../lib/sql/query";
import { getUser } from "./auth";
import { getFactoryById, getFactoryIntegrations } from "./factories/repo";
import { created, noContent, type OpCall, type TeamKit, type TeamOp } from "./kit";

export const migrations: readonly Migration[] = [
  {
    version: 100,
    name: "integrations",
    sql: `
CREATE TABLE integrations (id TEXT PRIMARY KEY, team_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
  factory_id TEXT NOT NULL DEFAULT '', created_ms INTEGER NOT NULL, data TEXT NOT NULL);
CREATE INDEX integrations_provider ON integrations (provider);
CREATE INDEX integrations_created ON integrations (created_ms, id);
CREATE TABLE integration_kv (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_ms INTEGER NOT NULL);
`,
  },
];

export const INTEGRATION_PROVIDERS = [
  "github",
  "gitlab",
  "azure_devops",
  "slack",
  "microsoft_teams",
  "linear",
  "jira",
] as const;
const FORGE_PROVIDERS: readonly string[] = ["github", "gitlab", "azure_devops"];

// ---------- records ----------

const parse = (r: { data: string }): Integration => JSON.parse(r.data) as Integration;

/** The records, oldest first. provider "" lists all. */
export function listIntegrations(kit: TeamKit, provider = ""): Integration[] {
  const rows =
    provider === ""
      ? all<{ data: string }>(kit.sql, "SELECT data FROM integrations ORDER BY created_ms, id")
      : all<{ data: string }>(
          kit.sql,
          "SELECT data FROM integrations WHERE provider = ? ORDER BY created_ms, id",
          provider,
        );
  return rows.map(parse);
}

export function getIntegration(kit: TeamKit, id: string): Integration | undefined {
  const r = first<{ data: string }>(kit.sql, "SELECT data FROM integrations WHERE id = ?", id);
  return r ? parse(r) : undefined;
}

function putIntegration(kit: TeamKit, i: Integration): void {
  const ms = Date.parse(i.created_at) || Date.now();
  kit.sql.exec(
    `INSERT INTO integrations (id, team_id, provider, factory_id, created_ms, data) VALUES (?, ?, ?, ?, ?, ?)
     ON CONFLICT(id) DO UPDATE SET team_id = excluded.team_id, provider = excluded.provider,
       factory_id = excluded.factory_id, data = excluded.data`,
    i.id,
    i.team_id,
    i.provider,
    i.factory_id ?? "",
    ms,
    JSON.stringify(i),
  );
}

/** Sets the status of a record. Returns true when it changed. An unknown record is not an error. */
export function setIntegrationStatus(kit: TeamKit, id: string, status: IntegrationStatus, reason: string): boolean {
  return kit.tx(() => {
    const i = getIntegration(kit, id);
    if (!i) return false;
    if (integrationStatus(i) === status && (i.status_reason ?? "") === reason) return false;
    i.status = status;
    i.status_reason = reason;
    i.status_updated_at = new Date().toISOString();
    putIntegration(kit, i);
    return true;
  });
}

// ---------- REST ----------

interface IntegrationRequest {
  provider?: string;
  name?: string;
  config?: Record<string, string>;
  secret_ref?: string;
  factory_id?: string;
  status?: string;
}

function request(body: unknown): IntegrationRequest {
  const b = bodyObject(body);
  return {
    provider: optString(b, "provider"),
    name: optString(b, "name"),
    config: optStringMap(b, "config"),
    secret_ref: optString(b, "secret_ref"),
    factory_id: optString(b, "factory_id"),
    status: optString(b, "status"),
  };
}

/** A status value of a request: one of INTEGRATION_STATUSES, else 400. */
function statusOf(v: string): IntegrationStatus {
  if (!(INTEGRATION_STATUSES as readonly string[]).includes(v))
    throw new OfError("invalid_request", `status must be one of ${INTEGRATION_STATUSES.join(", ")}`);
  return v as IntegrationStatus;
}

/** Applies the status that an admin asks for: active or disabled. auth_failed is set only by the server. */
function applyStatus(i: Integration, status: string | undefined): void {
  if (status === undefined || status === null) return;
  if (status !== INTEGRATION_ACTIVE && status !== INTEGRATION_DISABLED)
    throw new OfError("invalid_request", "status must be active or disabled");
  if (integrationStatus(i) === status) return;
  i.status = status;
  i.status_reason = status === INTEGRATION_DISABLED ? "disabled by an admin" : "";
  i.status_updated_at = new Date().toISOString();
}

/** A forge record needs config.org and a credential; a chat or tracker record goes to checkWorkspace. */
function checkForgeRecord(i: Integration): void {
  if (!FORGE_PROVIDERS.includes(i.provider)) return checkWorkspace(i);
  const cfg = i.config ?? (i.config = {});
  const org = (cfg["org"] ?? "").trim();
  if (org === "") {
    throw new OfError(
      "invalid_request",
      `config.org is necessary for a ${i.provider} record: set it to the owner that the record serves (an organization, a user name or, for GitLab, the top-level group)`,
    );
  }
  if (org.includes("/"))
    throw new OfError(
      "invalid_request",
      `config.org "${org}" must be one name without a slash (for GitLab, the top-level group)`,
    );
  cfg["org"] = org;
  if (i.provider === "github" && (cfg["app_id"] ?? "") !== "") {
    if (!i.secret_ref && (cfg["private_key_ref"] ?? "") === "") {
      throw new OfError(
        "invalid_request",
        "a github app record needs its private key: set private_key_ref (or secret_ref) to the name of a team secret",
      );
    }
    return;
  }
  if (i.secret_ref) return;
  throw new OfError(
    "invalid_request",
    `a ${i.provider} record needs a credential: set secret_ref to the name of a team secret that holds a token`,
  );
}

const WORKSPACE_WHAT: Record<string, string> = {
  slack: "the Slack workspace ID (T…)",
  microsoft_teams: "the Microsoft Graph team ID",
  linear: "the Linear organization ID",
  jira: "the host name of the Jira site (acme.atlassian.net) or its cloud ID",
};

/** A chat or tracker record needs the key that names its workspace. A Jira site is kept as its host name. */
function checkWorkspace(i: Integration): void {
  const key = integrationWorkspaceKey(i.provider);
  if (key === "") return;
  const cfg = i.config ?? (i.config = {});
  const v = normalizeWorkspace(i.provider, cfg[key] ?? "");
  if (v === "") {
    throw new OfError(
      "invalid_request",
      `config.${key} is necessary for a ${i.provider} record: set it to ${WORKSPACE_WHAT[i.provider] ?? "the workspace"} that the record serves`,
    );
  }
  cfg[key] = v;
}

/** At most one record for each provider, factory and (forge) config.org or (chat, tracker) workspace key. */
function checkUnique(kit: TeamKit, i: Integration): void {
  const isForge = FORGE_PROVIDERS.includes(i.provider);
  const wsKey = integrationWorkspaceKey(i.provider);
  const fac = i.factory_id ?? "";
  if (!isForge && wsKey === "" && fac === "") return;
  for (const x of listIntegrations(kit)) {
    if (x.id === i.id || x.provider !== i.provider || (x.factory_id ?? "") !== fac) continue;
    const where = fac !== "" ? `factory ${fac}` : "the team";
    if (wsKey !== "") {
      const want = i.config?.[wsKey] ?? "";
      if (integrationWorkspace(x).toLowerCase() !== want.toLowerCase()) continue;
      throw new OfError(
        "conflict",
        `${where} already has a ${i.provider} record for config.${wsKey} "${want}" (${x.id})`,
      );
    }
    if (!isForge) throw new OfError("conflict", `factory ${fac} already has a ${i.provider} integration (${x.id})`);
    if ((x.config?.["org"] ?? "").trim().toLowerCase() !== (i.config?.["org"] ?? "").toLowerCase()) continue;
    throw new OfError(
      "conflict",
      `${where} already has a ${i.provider} record for the owner "${i.config?.["org"]}" (${x.id})`,
    );
  }
}

function integrationFor(kit: TeamKit, id: string): Integration {
  const i = getIntegration(kit, id);
  if (!i) throw new OfError("resource_not_found", `integration ${id} not found`);
  return i;
}

function checkSecretRef(v: string | undefined): void {
  if (v !== undefined && v !== "" && !API_NAME_RE.test(v))
    throw new OfError("invalid_request", `invalid secret_ref "${v}"`);
}

const sortedByCreated = (a: Integration, b: Integration): number =>
  a.created_at < b.created_at ? -1 : a.created_at > b.created_at ? 1 : a.id < b.id ? -1 : 1;

// ---------- store ops for the Worker side ----------

const jsonOf = <T>(r: { data: string } | undefined): T | null => (r ? (JSON.parse(r.data) as T) : null);

function findIdentity(kit: TeamKit, provider: string, externalId: string): ExternalIdentity | null {
  return jsonOf<ExternalIdentity>(
    first<{ data: string }>(
      kit.sql,
      "SELECT data FROM external_identities WHERE provider = ? AND external_id = ?",
      provider,
      externalId,
    ),
  );
}

function identityBody(c: OpCall): ExternalIdentity {
  const b = bodyObject(c.body);
  const x = {
    user_id: str(b, "user_id"),
    provider: str(b, "provider"),
    external_id: str(b, "external_id"),
    handle: str(b, "handle"),
  };
  if (!x.provider || !x.external_id) throw new OfError("invalid_request", "identity needs provider and external_id");
  return x;
}

function writeIdentity(kit: TeamKit, x: ExternalIdentity): void {
  kit.sql.exec(
    `INSERT INTO external_identities (provider, external_id, user_id, data) VALUES (?, ?, ?, ?)
     ON CONFLICT(provider, external_id) DO UPDATE SET user_id = excluded.user_id, data = excluded.data`,
    x.provider,
    x.external_id,
    x.user_id,
    JSON.stringify(x),
  );
}

export const ops: Record<string, TeamOp> = {
  listIntegrations: (kit, c) => {
    const prov = c.query["provider"] ?? "";
    const items = listIntegrations(kit, prov).sort(sortedByCreated);
    return listResponse(items, "");
  },

  createIntegration: (kit, c) =>
    kit.tx(() => {
      const req = request(c.body);
      if (!req.provider || !(INTEGRATION_PROVIDERS as readonly string[]).includes(req.provider)) {
        throw new OfError("invalid_request", `provider must be one of ${INTEGRATION_PROVIDERS.join(", ")}`);
      }
      const name = (req.name ?? "").trim();
      const i: Integration = {
        id: newId("int"),
        team_id: kit.teamId(),
        provider: req.provider,
        name: name !== "" ? name : req.provider,
        config: { ...(req.config ?? {}) },
        created_at: new Date().toISOString(),
      };
      if (req.secret_ref !== undefined) {
        checkSecretRef(req.secret_ref);
        if (req.secret_ref !== "") i.secret_ref = req.secret_ref;
      }
      const fac = (req.factory_id ?? "").trim();
      if (fac !== "") {
        if (!getFactoryById(kit, fac)) {
          throw new OfError("invalid_request", `factory_id "${fac}" is not a factory of this team`);
        }
        i.factory_id = fac;
      }
      checkForgeRecord(i);
      checkUnique(kit, i);
      applyStatus(i, req.status);
      putIntegration(kit, i);
      return created(i);
    }),

  getIntegration: (kit, c) => integrationFor(kit, c.params["id"]!),

  updateIntegration: (kit, c) =>
    kit.tx(() => {
      const i = integrationFor(kit, c.params["id"]!);
      const req = request(c.body);
      if (req.provider !== undefined && req.provider !== i.provider)
        throw new OfError("invalid_request", "provider cannot change");
      if (req.factory_id !== undefined && req.factory_id.trim() !== (i.factory_id ?? "")) {
        throw new OfError("invalid_request", "factory_id cannot change");
      }
      applyStatus(i, req.status);
      if (req.name !== undefined && req.name.trim() !== "") i.name = req.name.trim();
      if (req.config) i.config = { ...req.config };
      i.config ??= {};
      if (req.secret_ref !== undefined) {
        checkSecretRef(req.secret_ref);
        if (req.secret_ref === "") delete i.secret_ref;
        else i.secret_ref = req.secret_ref;
      }
      checkForgeRecord(i);
      checkUnique(kit, i);
      putIntegration(kit, i);
      return i;
    }),

  deleteIntegration: (kit, c) =>
    kit.tx(() => {
      const i = integrationFor(kit, c.params["id"]!);
      run(kit.sql, "DELETE FROM integrations WHERE id = ?", i.id);
      return noContent();
    }),

  "integrations.list": (kit, c) => listIntegrations(kit, c.query["provider"] ?? ""),
  integrationsForProvider: (kit, c) => ({ items: listIntegrations(kit, c.query["provider"] ?? "") }),
  "integrations.get": (kit, c) => getIntegration(kit, c.params["id"]!) ?? null,
  integrationsSetStatus: (kit, c) => {
    const b = bodyObject(c.body, true);
    const id = str(b, "id");
    const status = str(b, "status");
    if (!id || !status) throw new OfError("invalid_request", "id and status are required");
    return { changed: setIntegrationStatus(kit, id, statusOf(status), str(b, "reason")) };
  },

  "identity.find": (kit, c) => findIdentity(kit, c.params["provider"]!, c.params["external_id"]!),
  "identity.link": (kit, c) =>
    kit.tx(() => {
      const x = identityBody(c);
      const cur = findIdentity(kit, x.provider, x.external_id);
      if (cur && cur.user_id !== "" && cur.user_id !== x.user_id) return { linked: false };
      // A link of the same user is kept as it is, unless the handle is new.
      if (!(cur && cur.user_id === x.user_id && (x.handle === "" || cur.handle === x.handle))) writeIdentity(kit, x);
      return { linked: true };
    }),
  "identity.upsert": (kit, c) =>
    kit.tx(() => {
      writeIdentity(kit, identityBody(c));
      return {};
    }),
  "identity.user": (kit, c): User | null => (getUser(kit, c.params["id"]!) as User | undefined) ?? null,
  "identity.userByEmail": (kit, c): User | null => {
    const e = (c.params["email"] ?? "").trim().toLowerCase();
    if (e === "") return null;
    return jsonOf<User>(first<{ data: string }>(kit.sql, "SELECT data FROM users WHERE email = ?", e));
  },
  factorySettings: (kit, c) => getFactoryIntegrations(kit, c.params["factory_id"]!),
  "kv.get": (kit, c) =>
    first<{ value: string }>(kit.sql, "SELECT value FROM integration_kv WHERE key = ?", c.params["key"]!)?.value ??
    null,
  "kv.set": (kit, c) =>
    kit.tx(() => {
      kit.sql.exec(
        `INSERT INTO integration_kv (key, value, updated_ms) VALUES (?, ?, ?)
         ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_ms = excluded.updated_ms`,
        c.params["key"]!,
        c.params["value"] ?? "",
        Date.now(),
      );
      return {};
    }),
};
