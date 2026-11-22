// Port of internal/mcpserver/tools.go and resolve.go: the Factory MCP tools.
// Each tool calls the same REST operations as the dashboard (backend.api).
import {
  OfError,
  factoryLink,
  inboxLink,
  integrationsLink,
  runLink,
  toOfError,
  workItemLink,
  type ErrorCode,
} from "@openfactory/core";
import type { Principal } from "../../contracts/do-messages";
import { roleRank } from "../../auth/roles";
import type { Env } from "../../contracts/env";
import { textResult, type CallToolResult, type JsonSchema, type Tool } from "../jsonrpc";
import { teamOp } from "../team";
import type { Connection, FactoryMcpBackend } from "./backend";
import {
  firstLine,
  matchesWorkItem,
  parsePullRequestURL,
  planReference,
  repoSetupFor,
  shortId,
  slug,
  splitRepo,
  truncate,
  workRefs,
  type PullRequestRef,
  type RepoSetup,
  type WorkItemLike,
} from "./refs";

/** Per-request context of the Factory MCP tools. */
export interface FactoryCtx {
  env: Env;
  principal: Principal;
  backend: FactoryMcpBackend;
  publicURL: string;
}

type Args = Record<string, unknown>;
type Json = Record<string, unknown>;

const invalid = (msg: string) => new OfError("invalid_request", msg);
const notFound = (msg: string) => new OfError("resource_not_found", msg);

const s = (a: Args, k: string): string => (typeof a[k] === "string" ? (a[k] as string) : "");
const n = (a: Args, k: string): number => (typeof a[k] === "number" ? Math.trunc(a[k] as number) : 0);
const b = (a: Args, k: string): boolean => a[k] === true;

const userIdOf = (p: Principal) => p.userId ?? "";
const actorOf = (p: Principal) => (p.userId ? "user:" + p.userId : "api");
const can = (p: Principal, role: "user" | "editor" | "admin") => roleRank(p.role) >= roleRank(role);

/** errorResult: a tool-level failure the model can act on. */
function errorResult(e: unknown): CallToolResult {
  const { error, known } = toOfError(e);
  if (!known) console.error("mcp tool failed", e);
  return textResult(JSON.stringify({ error: { code: error.code, message: error.detail || error.code } }), true);
}

function jsonResult(v: unknown): CallToolResult {
  return textResult(JSON.stringify(v, null, 2));
}

// ---------- input schemas (hand-written; go-sdk inferred them from struct tags) ----------

type Prop = { type: string; description?: string; items?: unknown; additionalProperties?: unknown; properties?: unknown; required?: string[] };

function obj(props: Record<string, Prop>, required: string[] = []): JsonSchema {
  return { type: "object", properties: props, required, additionalProperties: false };
}
const str = (description?: string): Prop => ({ type: "string", ...(description ? { description } : {}) });
const int = (description: string): Prop => ({ type: "integer", description });
const bool = (description: string): Prop => ({ type: "boolean", description });

// ---------- helpers over REST ----------

interface Factory {
  id: string;
  uid?: string;
  team_id?: string;
  name: string;
  alias?: string;
  description?: string;
  source?: string;
  url?: string;
  active_version_id?: string;
}

interface WorkItem extends WorkItemLike {
  id: string;
  factory_id: string;
  title: string;
  stage: string;
  source?: Json & WorkItemLike["source"];
  creator_id?: string;
  local_owner?: string;
  root_run_id?: string;
  labels?: Record<string, string>;
  created_at: string;
  updated_at: string;
  url?: string;
}

interface RunJson {
  id?: string;
  run_id?: string;
  agent_name?: string;
  name?: string;
  state: string;
  parent_run_id?: string;
  result?: string;
  error?: string;
  url?: string;
  run_url?: string;
  created_at: string;
  finished_at?: string;
  work_item_id?: string;
}

interface WorkItemDetail extends WorkItem {
  runs?: RunJson[];
  inbox?: (Json & { id: string; kind: string; title: string; options?: unknown; questions?: unknown[]; resolved?: boolean; factory_id?: string; recipients?: unknown[] })[];
}

const TERMINAL_STAGES = new Set(["complete", "cancelled"]);
const TERMINAL_RUN_STATES = new Set(["completed", "failed", "cancelled", "succeeded", "error"]);
const STAGES = ["intake", "triage", "planning", "awaiting_approval", "building", "reviewing", "handoff", "complete", "cancelled"];

async function factoryOf(c: FactoryCtx, id: string): Promise<Factory> {
  try {
    return await c.backend.api<Factory>(c.env, c.principal, "GET", `/factories/${encodeURIComponent(id)}`);
  } catch (e) {
    const { error } = toOfError(e);
    if (error.code === "resource_not_found" || error.code === "not_authorized") throw notFound(`factory ${id} not found`);
    throw error;
  }
}

async function listFactoriesAll(c: FactoryCtx, search = ""): Promise<Factory[]> {
  const q = new URLSearchParams({ limit: "200" });
  if (search) q.set("search", search);
  const out = await c.backend.api<{ items: Factory[] }>(c.env, c.principal, "GET", `/factories?${q}`);
  return out.items ?? [];
}

async function workItemDetail(c: FactoryCtx, id: string): Promise<WorkItemDetail> {
  return c.backend.api<WorkItemDetail>(c.env, c.principal, "GET", `/work-items/${encodeURIComponent(id)}`);
}

const factoryURL = (c: FactoryCtx, f: Factory) => f.url || factoryLink(c.publicURL, f.id);
const workItemURL = (c: FactoryCtx, fid: string, id: string) => workItemLink(c.publicURL, fid, id);

function workItemView(c: FactoryCtx, w: WorkItem): Json {
  const v: Json = {
    id: w.id,
    factory_id: w.factory_id,
    title: w.title,
    stage: w.stage,
    url: w.url || workItemURL(c, w.factory_id, w.id),
    source: w.source ?? {},
  };
  if (w.creator_id) v.creator_id = w.creator_id;
  if (w.local_owner) v.local_owner = w.local_owner;
  if (w.pull_requests && w.pull_requests.length > 0) v.pull_requests = w.pull_requests;
  v.created_at = w.created_at;
  v.updated_at = w.updated_at;
  return v;
}

/** InboxForCaller: the items that need the caller. */
function forCaller(it: { recipients?: unknown[] }, p: Principal): boolean {
  const rc = (it.recipients ?? [])
    .map((r) => (typeof r === "string" ? r : ((r as Json)?.user_id ?? (r as Json)?.id) as string | undefined))
    .filter((x): x is string => !!x);
  if (rc.length === 0) return true;
  return userIdOf(p) !== "" && rc.includes(userIdOf(p));
}

// ---------- tools ----------

const instructions = `Factory MCP connects you to the user's Open Factory. A factory is a team of agents bound to repositories;
work sent to it becomes a work item coordinated by the factory's foreman agent.

- send_work_item creates a work item (state the goal, context, constraints and work already done in the note), or hands work back to an
  existing work item with its ID, the pushed branch or pull request URL and a note covering what changed, what was validated and what remains.
- To continue a work item locally: find it (list_work_items, search_work_items, or get_work_item with any reference), call get_work_item with start_working=true,
  follow the returned Git setup, keep the foreman informed with message_foreman / get_conversation, push, then hand back with send_work_item.
- Picking up a work item does not claim, lock or pause it. complete_work_item closes a work item when nothing remains.
- Factory MCP never modifies local files; run the suggested commands yourself.`;

export const FACTORY_MCP_INSTRUCTIONS = instructions;

type Handler = (c: FactoryCtx, a: Args) => Promise<unknown>;

function tool(name: string, description: string, readOnly: boolean, inputSchema: JsonSchema, h: Handler): Tool<FactoryCtx> {
  return {
    name,
    description,
    inputSchema,
    annotations: { readOnlyHint: readOnly },
    handler: async (a, c) => {
      try {
        return jsonResult(await h(c, a));
      } catch (e) {
        return errorResult(e);
      }
    },
  };
}

const APPLIES_TO: Record<string, string> = {
  "agent.schema.json": "agents/<name>/agent.md frontmatter",
  "automation.schema.json": "automations/<name>/automation.md frontmatter",
  "benchmark_suite.schema.json": "benchmarks/<suite>/suite.yaml",
  "benchmark_suite_task.schema.json": "benchmarks/<suite>/tasks/<task>.yaml",
  "common.schema.json": "shared definitions",
  "factory.schema.json": "factory.yaml",
  "machine.schema.json": "machines/<name>.yaml",
  "scorer.schema.json": "scorers/<name>/scorer.md frontmatter",
  "webhook.schema.json": "webhooks/<name>.yaml",
};

const FORGE_PROVIDERS: Record<string, string> = { GITHUB: "github", GITLAB: "gitlab" };
const CONNECTION_PROVIDERS = ["github", "gitlab", "slack", "linear", "jira"];

async function existingIntegration(c: FactoryCtx, provider: string): Promise<{ id: string } | null> {
  const out = await c.backend.api<{ items?: { id: string; provider: string }[] }>(c.env, c.principal, "GET", "/integrations");
  return (out.items ?? []).find((x) => x.provider === provider) ?? null;
}

function checkTeam(p: Principal, teamId: string): void {
  if (teamId !== "" && teamId !== p.teamId) throw notFound(`team ${teamId} not found`);
}

/** resolveWorkItem: a work item ID or a reference (URL, issue, pull request, branch). */
async function resolveWorkItem(c: FactoryCtx, ref0: string): Promise<WorkItem> {
  const ref = ref0.trim();
  if (ref === "") throw invalid("work item is required");
  const nf = notFound(`no work item matches "${ref}"`);
  const owned = async (id: string): Promise<WorkItem | null> => {
    try {
      return await workItemDetail(c, id);
    } catch {
      return null;
    }
  };
  const ofRun = async (runId: string): Promise<WorkItem | null> => {
    try {
      const r = await c.backend.api<RunJson>(c.env, c.principal, "GET", `/runs/${encodeURIComponent(runId)}`);
      return r.work_item_id ? await owned(r.work_item_id) : null;
    } catch {
      return null;
    }
  };
  if (/^[A-Za-z0-9_.-]+$/.test(ref)) {
    const wi = (await owned(ref)) ?? (await ofRun(ref));
    if (wi) return wi;
  }
  const plan = planReference(ref, c.publicURL);
  if (plan.kind === "notFound") throw nf;
  if (plan.kind === "dashboard") {
    for (const id of plan.workItems) {
      const wi = await owned(id);
      if (wi) return wi;
    }
    for (const id of plan.runs) {
      const wi = await ofRun(id);
      if (wi) return wi;
    }
    throw nf;
  }
  const fs = await listFactoriesAll(c);
  // Fast path: active work item by source key.
  for (const key of plan.keys) {
    for (const f of fs) {
      const id = await c.backend.resolveSourceKey(c.env, c.principal, f.id, key).catch(() => null);
      if (id) {
        const wi = await owned(id);
        if (wi) return wi;
      }
    }
  }
  // Slow path: scan recent work items (terminal ones too), matching source
  // keys, linked pull requests and branches.
  let best: WorkItem | null = null;
  for (const f of fs) {
    for (const wi of await listWorkItemsOf(c, f.id, 500)) {
      if (matchesWorkItem(wi, plan.keys, plan.prURL, plan.branch) && (!best || Date.parse(wi.updated_at) > Date.parse(best.updated_at))) {
        best = wi;
      }
    }
  }
  if (!best) throw nf;
  return best;
}

async function listWorkItemsOf(c: FactoryCtx, factoryId: string, max: number, query: Record<string, string> = {}): Promise<WorkItem[]> {
  const out: WorkItem[] = [];
  let cursor = "";
  while (out.length < max) {
    const q = new URLSearchParams({ ...query, limit: String(Math.min(200, max - out.length)) });
    if (cursor) q.set("cursor", cursor);
    const page = await c.backend.api<{ items: WorkItem[]; page_info?: { next_cursor?: string } }>(
      c.env,
      c.principal,
      "GET",
      `/factories/${encodeURIComponent(factoryId)}/work-items?${q}`,
    );
    out.push(...(page.items ?? []));
    cursor = page.page_info?.next_cursor ?? "";
    if (!cursor) break;
  }
  return out;
}

function parseTime(v: string): string {
  if (/^\d{4}-\d{2}-\d{2}$/.test(v) && !Number.isNaN(Date.parse(v + "T00:00:00Z"))) return v + "T00:00:00Z";
  if (/^\d{4}-\d{2}-\d{2}T/.test(v) && !Number.isNaN(Date.parse(v))) return v;
  throw invalid(`invalid time "${v}": use RFC 3339 or YYYY-MM-DD`);
}

interface Route {
  id: string;
  kind: string;
  description: string;
}

async function routes(c: FactoryCtx, f: Factory): Promise<Route[]> {
  const out: Route[] = [{ id: "inbox", kind: "inbox", description: `Your Open Factory inbox (${inboxLink(c.publicURL)})` }];
  const ints = await c.backend.api<Json>(c.env, c.principal, "GET", `/factories/${encodeURIComponent(f.id)}/integrations`);
  const raw = (ints?.integrations ?? ints) as unknown;
  const keys = (Array.isArray(raw) ? raw.map((x) => (typeof x === "string" ? x : String((x as Json).type ?? (x as Json).provider ?? ""))) : Object.keys((raw as Json) ?? {})).sort();
  for (const k of keys) {
    switch (k) {
      case "slack":
        out.push({ id: "slack_dm", kind: "slack", description: "Slack direct message (requires a linked Slack account)" });
        break;
      case "microsoft-teams":
      case "microsoft_teams":
        out.push({ id: "teams_dm", kind: "microsoft_teams", description: "Microsoft Teams chat message (requires a linked account)" });
        break;
      case "linear":
        out.push({ id: "linear_issue", kind: "linear", description: "Comment on the work item's Linear issue, when it has one" });
        break;
      case "jira":
        out.push({ id: "jira_issue", kind: "jira", description: "Comment on the work item's Jira issue, when it has one" });
        break;
    }
  }
  return out;
}

async function checkRoute(c: FactoryCtx, f: Factory, id: string): Promise<void> {
  const rs = await routes(c, f);
  if (!rs.some((r) => r.id === id)) throw invalid(`unknown notify_route "${id}"; call list_notification_routes`);
}

async function factoryRepos(c: FactoryCtx, factoryId: string): Promise<{ codeForge: string; owner: string; name: string }[]> {
  try {
    const d = await c.backend.api<Json>(c.env, c.principal, "GET", `/factories/${encodeURIComponent(factoryId)}/definition`);
    const compiled = (d?.compiled ?? d?.definition ?? d) as Json;
    const repos = (compiled?.repositories ?? []) as { codeForge?: string; owner?: string; name?: string }[];
    return repos.map((r) => ({ codeForge: r.codeForge ?? "GITHUB", owner: r.owner ?? "", name: r.name ?? "" }));
  } catch {
    return [];
  }
}

/** startWorking: marks the work item as taken over locally and returns Git setup guidance. */
async function startWorking(c: FactoryCtx, wi: WorkItem, activeRuns: number): Promise<Json> {
  const p = c.principal;
  await c.backend.patchWorkItem(c.env, p, wi.id, {
    local_owner: userIdOf(p),
    event: { kind: "note", actor: actorOf(p), data: { local_owner: userIdOf(p), picked_up_locally: true } },
  });
  wi.local_owner = userIdOf(p);
  wi.updated_at = new Date().toISOString();
  const sid = shortId(wi.id);
  let newBranch = "factory/" + sid;
  const sl = slug(wi.title);
  if (sl) newBranch += "-" + sl;
  const setups: RepoSetup[] = [];
  const seen = new Set<string>();
  for (const pr of wi.pull_requests ?? []) {
    if (!pr.branch || seen.has(pr.repo) || pr.state === "merged" || pr.state === "closed") continue;
    seen.add(pr.repo);
    const [owner, name] = splitRepo(pr.repo);
    setups.push(repoSetupFor(pr.forge, owner, name, pr.branch, "", sid, true));
  }
  if (setups.length === 0) {
    let repos = await factoryRepos(c, wi.factory_id);
    const srcRepo = (wi.source?.repo as string | undefined) ?? "";
    if (srcRepo) {
      const [owner, name] = splitRepo(srcRepo);
      repos = [{ codeForge: wi.source?.provider === "gitlab" ? "GITLAB" : "GITHUB", owner, name }, ...repos];
    }
    for (const r of repos) {
      const full = `${r.owner}/${r.name}`;
      if (seen.has(full)) continue;
      seen.add(full);
      setups.push(repoSetupFor(r.codeForge, r.owner, r.name, newBranch, "main", sid, false));
    }
  }
  const warnings = ["Picking up a work item does not claim, lock, or pause it: the factory may keep running its own work on it."];
  if (activeRuns > 0) {
    warnings.push(`${activeRuns} run(s) are still active on this work item; coordinate with the foreman to avoid duplicate changes.`);
  }
  if (setups.length === 0) {
    warnings.push("No repository is linked to this work item or its factory; ask the foreman which repository to use.");
  }
  return {
    local_owner: userIdOf(p),
    next_steps: [
      "Run the setup commands yourself; Factory MCP never modifies your files.",
      "Tell the foreman you are picking the work item up with message_foreman, and read replies with get_conversation.",
      "Validate your change, commit and push the branch: the factory cannot see uncommitted or unpushed work.",
      "Hand back with send_work_item using work_item_id, the pushed branch or pull_request_url, and a note covering what changed, what was validated, and what remains.",
      "When nothing remains for the factory to do, call complete_work_item.",
    ],
    repositories: setups,
    warnings,
  };
}

const REPO_ITEM: Prop = {
  type: "object",
  properties: { codeForge: str("GITHUB, GITLAB or AZURE_DEVOPS"), owner: str(), name: str() },
  required: ["owner", "name"],
  additionalProperties: false,
};

export const FACTORY_TOOLS: readonly Tool<FactoryCtx>[] = [
  tool("list_factories", "Lists the factories you can access.", true, obj({ search: str("optional case-insensitive filter on factory name or alias") }), async (c, a) => {
    const q = s(a, "search").toLowerCase();
    const out = (await listFactoriesAll(c, s(a, "search")))
      .filter((f) => q === "" || f.name.toLowerCase().includes(q) || (f.alias ?? "").toLowerCase().includes(q))
      .map((f) => ({
        uid: f.id,
        name: f.name,
        alias: f.alias ?? "",
        ...(f.description ? { description: f.description } : {}),
        source: f.source ?? "",
        url: factoryURL(c, f),
      }))
      .sort((x, y) => (x.name < y.name ? -1 : x.name > y.name ? 1 : 0));
    return { factories: out };
  }),

  tool(
    "get_factory_file_schema",
    "Returns the JSON Schema documents for factory definition files, as a catalog or one document at a time.",
    true,
    obj({
      document: str("schema document name such as factory.schema.json or agent.schema.json; omit for the catalog"),
      version: str("schema version; defaults to the current version"),
    }),
    async (c, a) => {
      let version = s(a, "version");
      if (version === "") {
        const v = await c.backend.api<{ current_version: string }>(c.env, c.principal, "GET", "/factory-files/schemas");
        version = v.current_version;
      }
      const doc = s(a, "document");
      if (doc !== "") {
        const schema = await c.backend.api(c.env, c.principal, "GET", `/factory-files/schemas/${encodeURIComponent(version)}/${encodeURIComponent(doc)}`);
        return { version, document: doc, schema };
      }
      const docs = await c.backend.api<Json>(c.env, c.principal, "GET", `/factory-files/schemas/${encodeURIComponent(version)}`);
      return {
        applies_to: APPLIES_TO,
        documents: Object.keys(docs ?? {}).sort(),
        url: `${c.publicURL}/api/v1/factory-files/schemas/${version}/{document}`,
        version,
      };
    },
  ),

  tool(
    "validate_factory_files",
    "Validates a complete factory definition tree without saving or applying it.",
    true,
    obj(
      {
        files: {
          type: "object",
          description:
            "the complete definition tree: path relative to the factory root (factory.yaml, agents/<name>/agent.md, ...) mapped to file content",
          additionalProperties: { type: "string" },
        },
      },
      ["files"],
    ),
    async (c, a) => {
      const files = (typeof a.files === "object" && a.files !== null ? a.files : {}) as Record<string, string>;
      if (Object.keys(files).length === 0) throw invalid("files is required");
      return c.backend.validateFiles(c.env, files);
    },
  ),

  tool("list_teams", "Lists current memberships and first-time joinable team choices.", true, obj({}), async (c) => {
    const team = await c.backend.api<{ id: string; name: string }>(c.env, c.principal, "GET", `/teams/${encodeURIComponent(c.principal.teamId)}`);
    return {
      joinable: [],
      memberships: [{ name: team.name, role: c.principal.role, team_id: team.id }],
      note: "Self-hosted API keys belong to one team; ask an admin to add you to another team.",
    };
  }),

  tool(
    "create_team",
    "Creates the authenticated user's first team with a confirmed name.",
    false,
    obj({ name: str("the team name the user confirmed"), confirmed: bool("must be true once the user explicitly confirmed the name") }, ["name", "confirmed"]),
    async (c, a) => {
      if (s(a, "name").trim() === "") throw invalid("name is required");
      if (!b(a, "confirmed")) throw invalid("confirm the team name with the user, then call again with confirmed=true");
      const team = await c.backend.api<{ id: string; name: string }>(c.env, c.principal, "GET", `/teams/${encodeURIComponent(c.principal.teamId)}`);
      throw new OfError("conflict", `you are already a member of team "${team.name}" (${team.id}); a first team can only be created once`);
    },
  ),

  tool("join_team", "Joins a team selected from the first-time discovery choices.", false, obj({ team_id: str("ID of a team from list_teams joinable choices") }, ["team_id"]), async (c, a) => {
    if (s(a, "team_id") === c.principal.teamId) return { status: "already_member", team_id: c.principal.teamId };
    throw new OfError("operation_not_supported", `team "${s(a, "team_id")}" is not among your joinable teams; ask one of its admins to add you`);
  }),

  tool(
    "list_forge_repositories",
    "Lists repositories available through a team's connected GitHub or GitLab account.",
    true,
    obj({ team_id: str("team ID; defaults to your team"), code_forge: str("GITHUB (default) or GITLAB"), query: str("optional filter on owner/name") }),
    async (c, a) => {
      checkTeam(c.principal, s(a, "team_id"));
      const forge = (s(a, "code_forge") || "GITHUB").toUpperCase();
      if (!(forge in FORGE_PROVIDERS)) throw invalid("code_forge must be GITHUB or GITLAB");
      if (!c.backend.listRepositories) {
        throw new OfError("integration_not_configured", `repository listing is not available; call start_connection with provider "${FORGE_PROVIDERS[forge]}"`);
      }
      const repos = await c.backend.listRepositories(c.env, c.principal.teamId, forge, s(a, "query"));
      return { code_forge: forge, repositories: repos ?? [] };
    },
  ),

  tool(
    "list_tracker_scopes",
    "Lists the Linear teams or Jira projects that can be scoped to route issues to a new factory.",
    true,
    obj({ team_id: str("team ID; defaults to your team"), provider: str("linear or jira") }, ["provider"]),
    async (c, a) => {
      checkTeam(c.principal, s(a, "team_id"));
      const prov = s(a, "provider").toLowerCase();
      if (prov !== "linear" && prov !== "jira") throw invalid("provider must be linear or jira");
      if (!c.backend.listTrackerScopes) {
        throw new OfError("integration_not_configured", `${prov} is not connected; call start_connection with provider "${prov}"`);
      }
      const scopes = await c.backend.listTrackerScopes(c.env, c.principal.teamId, prov);
      return { provider: prov, scopes: scopes ?? [] };
    },
  ),

  tool("start_connection", "Starts or checks setup for GitHub, GitLab, Slack, Linear, or Jira.", false, obj({ provider: str("github, gitlab, slack, linear or jira") }, ["provider"]), async (c, a) => {
    const prov = s(a, "provider").toLowerCase();
    if (!CONNECTION_PROVIDERS.includes(prov)) throw invalid(`provider must be one of ${CONNECTION_PROVIDERS.join(", ")}`);
    if (c.backend.startConnection) return c.backend.startConnection(c.env, c.principal.teamId, prov);
    const ex = await existingIntegration(c, prov);
    if (ex) return { provider: prov, status: "connected", integration_id: ex.id } satisfies Connection;
    if (!can(c.principal, "admin")) {
      throw new OfError("not_authorized", `${prov} is not connected and connecting team integrations requires an admin`);
    }
    const { id } = await teamOp<{ id: string }>(c.env, "mcp.connection.start", { provider: prov }, { teamId: c.principal.teamId });
    return {
      connection_id: id,
      provider: prov,
      status: "pending",
      authorize_url: integrationsLink(c.publicURL, prov, id),
      detail: "Open authorize_url in a browser, connect the integration on the Integrations page, then call get_connection_status.",
    } satisfies Connection;
  }),

  tool(
    "get_connection_status",
    "Checks whether a browser authorization flow completed.",
    true,
    obj({ connection_id: str("connection_id returned by start_connection"), provider: str("alternatively, the provider to check") }),
    async (c, a) => {
      const cid = s(a, "connection_id");
      if (cid === "" && s(a, "provider") === "") throw invalid("connection_id or provider is required");
      if (cid !== "" && c.backend.connectionStatus) return c.backend.connectionStatus(c.env, c.principal.teamId, cid);
      let prov = s(a, "provider").toLowerCase();
      if (cid !== "") {
        const row = await teamOp<{ provider: string } | null>(c.env, "mcp.connection.get", { id: cid }, { teamId: c.principal.teamId });
        if (!row) throw notFound(`connection ${cid} not found`);
        prov = row.provider;
      }
      const ex = await existingIntegration(c, prov);
      if (ex) return { ...(cid ? { connection_id: cid } : {}), provider: prov, status: "connected", integration_id: ex.id };
      return { ...(cid ? { connection_id: cid } : {}), provider: prov, status: "pending", detail: "authorization has not completed yet" };
    },
  ),

  tool(
    "create_factory",
    "Creates a factory with the selected repositories, integrations, and optional factory agents.",
    false,
    obj(
      {
        team_id: str("team ID; defaults to your team"),
        name: str("factory name"),
        alias: str("foreman handle used for @-mentions (up to 60 letters, digits, spaces, '.', '_', '-')"),
        description: str(),
        repositories: { type: "array", description: "repositories the factory works on", items: REPO_ITEM },
        integrations: {
          type: "array",
          description: "integrations to attach: slack, microsoft-teams, linear or jira (linear and jira are mutually exclusive)",
          items: { type: "string" },
        },
        agents: { type: "array", description: "default agents besides the foreman: triage, spec, implement, review; omit for all", items: { type: "string" } },
        model: str(
          "default model for agents; a Claude model selects the claude harness and an OpenAI model selects codex; defaults to claude-opus-5-5",
        ),
      },
      ["name"],
    ),
    async (c, a) => {
      checkTeam(c.principal, s(a, "team_id"));
      const req: Json = { name: s(a, "name"), definition: "defaults" };
      for (const k of ["alias", "description", "model"]) if (s(a, k)) req[k] = s(a, k);
      if (Array.isArray(a.repositories)) {
        req.repositories = (a.repositories as Json[]).map((r) => ({
          codeForge: String(r.codeForge ?? "").toUpperCase(),
          owner: String(r.owner ?? ""),
          name: String(r.name ?? ""),
        }));
      }
      if (Array.isArray(a.integrations)) req.integrations = (a.integrations as string[]).map((t) => ({ type: t }));
      if (Array.isArray(a.agents)) req.agents = a.agents;
      const res = await c.backend.api<{ factory: Factory }>(c.env, c.principal, "POST", "/factories", req);
      return {
        dashboard_url: res.factory.url ?? factoryLink(c.publicURL, res.factory.id),
        factory: res.factory,
        next_steps:
          "Share the dashboard link with the user. If the web app is open on its setup screen, refresh it to pick up the new factory. " +
          "Send the first work with send_work_item.",
      };
    },
  ),

  tool(
    "list_work_items",
    "Lists the work items in one factory, with filters such as creator, stage, and date.",
    true,
    obj(
      {
        factory_id: str("factory UID"),
        creator: str("'me' or a user ID"),
        stage: str("comma-separated stages: intake, triage, planning, awaiting_approval, building, reviewing, handoff, complete, cancelled"),
        since: str("RFC 3339 time or YYYY-MM-DD"),
        limit: int("page size (default 20, max 100)"),
        cursor: str("next_cursor from a previous call"),
      },
      ["factory_id"],
    ),
    async (c, a) => {
      const f = await factoryOf(c, s(a, "factory_id"));
      let limit = n(a, "limit");
      if (limit <= 0) limit = 20;
      if (limit > 100) limit = 100;
      const q = new URLSearchParams({ limit: String(limit) });
      if (s(a, "cursor")) q.set("cursor", s(a, "cursor"));
      const creator = s(a, "creator");
      if (creator === "me") q.set("creator", userIdOf(c.principal));
      else if (creator !== "") q.set("creator", creator);
      if (s(a, "stage") !== "") {
        const stages = s(a, "stage").split(",").map((x) => x.trim().toLowerCase());
        for (const st of stages) if (!STAGES.includes(st)) throw invalid(`unknown stage "${st}"`);
        q.set("stage", stages.join(","));
      }
      if (s(a, "since") !== "") q.set("since", parseTime(s(a, "since")));
      const page = await c.backend.api<{ items: WorkItem[]; page_info?: { next_cursor?: string } }>(
        c.env,
        c.principal,
        "GET",
        `/factories/${encodeURIComponent(f.id)}/work-items?${q}`,
      );
      const out: Json = {};
      if (page.page_info?.next_cursor) out.next_cursor = page.page_info.next_cursor;
      out.work_items = (page.items ?? []).map((w) => workItemView(c, w));
      return out;
    },
  ),

  tool(
    "search_work_items",
    "Searches work item titles across all factories you can access.",
    true,
    obj({ query: str("text to search for in work item titles"), limit: int("maximum results (default 20)") }, ["query"]),
    async (c, a) => {
      const q = s(a, "query").trim();
      if (q === "") throw invalid("query is required");
      let limit = n(a, "limit");
      if (limit <= 0 || limit > 100) limit = 20;
      const all: WorkItem[] = [];
      for (const f of await listFactoriesAll(c)) {
        const items = await listWorkItemsOf(c, f.id, limit, { search: q });
        for (const it of items) if (it.title.toLowerCase().includes(q.toLowerCase())) all.push(it);
      }
      all.sort((x, y) => Date.parse(y.updated_at) - Date.parse(x.updated_at));
      return { work_items: all.slice(0, limit).map((w) => workItemView(c, w)) };
    },
  ),

  tool(
    "get_work_item",
    "Reads a work item's status, run history, and outputs. Accepts a work item ID or a reference such as a URL, issue, pull request, or branch. With start_working=true, also returns local setup guidance.",
    false,
    obj(
      {
        work_item: str(
          "a work item ID or a reference: work item or run URL, pull request or issue URL, owner/repo#123, Slack permalink, Linear or Jira issue key or URL, or a branch name",
        ),
        start_working: bool("also return local setup guidance for continuing the work item in a local checkout"),
      },
      ["work_item"],
    ),
    async (c, a) => {
      const wi = await resolveWorkItem(c, s(a, "work_item"));
      const d = await workItemDetail(c, wi.id);
      let active = 0;
      let output = "";
      const runs = (d.runs ?? []).map((r) => {
        if (!TERMINAL_RUN_STATES.has(r.state)) active++;
        const id = r.run_id ?? r.id ?? "";
        if (id === d.root_run_id) output = r.result ?? "";
        return {
          run_id: id,
          ...(r.agent_name ? { agent_name: r.agent_name } : {}),
          ...(r.name ? { name: r.name } : {}),
          state: r.state,
          ...(r.parent_run_id ? { parent_run_id: r.parent_run_id } : {}),
          ...(r.result ? { result: truncate(r.result, 2000) } : {}),
          ...(r.error ? { error: r.error } : {}),
          url: r.run_url ?? r.url ?? runLink(c.publicURL, id),
          created_at: r.created_at,
          ...(r.finished_at ? { finished_at: r.finished_at } : {}),
        };
      });
      const pending = (d.inbox ?? [])
        .filter((it) => !it.resolved && forCaller(it, c.principal))
        .map((it) => ({
          id: it.id,
          kind: it.kind,
          options: it.options ?? null,
          ...(it.questions && it.questions.length > 0 ? { questions: it.questions } : {}),
          title: it.title,
        }));
      const out: Json = {
        active_runs: active,
        needs_decision: pending,
        output: truncate(output, 8000),
        runs,
        work_item: workItemView(c, d),
      };
      if (b(a, "start_working")) {
        out.local_setup = await startWorking(c, d, active);
        out.work_item = workItemView(c, d);
      }
      return out;
    },
  ),

  tool(
    "message_foreman",
    "Sends a message to a work item's foreman.",
    false,
    obj({ work_item_id: str("work item ID"), message: str("progress, a question, or a blocker for the foreman") }, ["work_item_id", "message"]),
    async (c, a) => {
      const res = await c.backend.api<{ work_item_id?: string; run_id?: string }>(
        c.env,
        c.principal,
        "POST",
        `/work-items/${encodeURIComponent(s(a, "work_item_id"))}/messages`,
        { message: s(a, "message") },
      );
      return {
        note: "Messaging keeps the factory informed but does not move or hand back the work item. Read replies with get_conversation.",
        run_id: res.run_id ?? "",
        status: "sent",
        work_item_id: res.work_item_id ?? s(a, "work_item_id"),
      };
    },
  ),

  tool(
    "get_conversation",
    "Reads a work item's foreman conversation.",
    true,
    obj({ work_item_id: str("work item ID"), after: int("only return messages with a sequence number greater than this") }, ["work_item_id"]),
    async (c, a) => {
      const d = await workItemDetail(c, s(a, "work_item_id"));
      const out: Json = { messages: [], stage: d.stage, work_item_id: d.id };
      if (!d.root_run_id) return out;
      const q = new URLSearchParams({ after: String(n(a, "after")) });
      const msgs = await c.backend.api<{ messages?: { seq: number; role: string; content: string; created_at: string }[] }>(
        c.env,
        c.principal,
        "GET",
        `/runs/${encodeURIComponent(d.root_run_id)}/messages?${q}`,
      );
      let last = 0;
      const conv: Json[] = [];
      for (const m of msgs.messages ?? []) {
        last = m.seq;
        if (m.role !== "user" && m.role !== "assistant") continue;
        conv.push({ seq: m.seq, role: m.role, content: m.content, created_at: m.created_at });
      }
      out.run_id = d.root_run_id;
      out.messages = conv;
      if (last > 0) out.last_seq = last;
      for (const r of d.runs ?? []) if ((r.run_id ?? r.id) === d.root_run_id) out.foreman_state = r.state;
      return out;
    },
  ),

  tool(
    "send_work_item",
    "Creates a new work item, or hands work back to an existing one.",
    false,
    obj(
      {
        factory_id: str("target factory UID (required for a new work item)"),
        work_item_id: str("existing work item to hand work back to"),
        title: str("title of a new work item"),
        note: str(
          "what the foreman starts from: goal, context, constraints and work already done; when handing back: what changed, what was validated, what remains",
        ),
        branch: str("pushed branch holding the work"),
        pull_request_url: str("pull request holding the work"),
        notify_route: str("route ID from list_notification_routes to be notified when the work item needs you or finishes"),
      },
      ["note"],
    ),
    async (c, a) => {
      if (s(a, "note").trim() === "") throw invalid("note is required");
      let pr: PullRequestRef | null = null;
      if (s(a, "pull_request_url") !== "") {
        pr = parsePullRequestURL(s(a, "pull_request_url"));
        if (!pr) throw invalid("pull_request_url is not a recognized pull or merge request URL");
        if (s(a, "branch")) pr.branch = s(a, "branch");
      }
      if (s(a, "work_item_id") !== "") return handBack(c, a, pr);
      if (s(a, "factory_id") === "") throw invalid("factory_id is required for a new work item (or pass work_item_id to hand work back)");
      const f = await factoryOf(c, s(a, "factory_id"));
      if (s(a, "notify_route")) await checkRoute(c, f, s(a, "notify_route"));
      const title = s(a, "title").trim() || firstLine(s(a, "note"));
      let prompt = s(a, "note");
      const refs = workRefs(s(a, "branch"), s(a, "pull_request_url"));
      if (refs) prompt += "\n\n" + refs;
      const run = await c.backend.api<RunJson & { work_item_id: string }>(c.env, c.principal, "POST", `/factories/${encodeURIComponent(f.id)}/runs`, {
        prompt,
        title,
        source: { provider: "mcp", kind: "work_item", title },
        metadata: { triggered_by: "mcp" },
      });
      const wiId = run.work_item_id;
      const runId = run.run_id ?? run.id ?? "";
      if (pr) await c.backend.linkPullRequest(c.env, c.principal, wiId, pr);
      if (s(a, "notify_route")) await c.backend.patchWorkItem(c.env, c.principal, wiId, { labels: { notify_route: s(a, "notify_route") } });
      return {
        note: "The foreman takes it from here and reports progress on the work item's conversation.",
        run_id: runId,
        run_url: runLink(c.publicURL, runId),
        status: "created",
        url: workItemURL(c, f.id, wiId),
        work_item_id: wiId,
      };
    },
  ),

  tool(
    "list_notification_routes",
    "Lists the notification destinations available to you in a factory.",
    true,
    obj({ factory_id: str("factory UID") }, ["factory_id"]),
    async (c, a) => {
      const f = await factoryOf(c, s(a, "factory_id"));
      return { factory_id: f.id, note: "Delivery is best-effort; treat notifications as a convenience.", routes: await routes(c, f) };
    },
  ),

  tool(
    "complete_work_item",
    "Marks a work item complete.",
    false,
    obj({ work_item_id: str("work item ID"), note: str("closing note") }, ["work_item_id"]),
    async (c, a) => {
      const wi = await c.backend.api<WorkItem>(c.env, c.principal, "POST", `/work-items/${encodeURIComponent(s(a, "work_item_id"))}/complete`, {
        ...(s(a, "note") ? { note: s(a, "note") } : {}),
      });
      return { status: "completed", work_item: workItemView(c, wi) };
    },
  ),
];

async function handBack(c: FactoryCtx, a: Args, pr: PullRequestRef | null): Promise<Json> {
  const p = c.principal;
  const wi = await workItemDetail(c, s(a, "work_item_id"));
  if (TERMINAL_STAGES.has(wi.stage)) throw new OfError("conflict", `work item ${wi.id} is already ${wi.stage}`);
  const f = await factoryOf(c, wi.factory_id);
  if (s(a, "notify_route")) await checkRoute(c, f, s(a, "notify_route"));
  if (pr && !(wi.pull_requests ?? []).some((x) => x.url === pr.url)) await c.backend.linkPullRequest(c.env, p, wi.id, pr);
  const who = userIdOf(p);
  let msg = `## Work handed back from a local session (${who})\n\n${s(a, "note")}`;
  const refs = workRefs(s(a, "branch"), s(a, "pull_request_url"));
  if (refs) msg += "\n\n" + refs;
  msg += "\n\nDecide the next step for this work item.";
  let runId = wi.root_run_id ?? "";
  if (runId) {
    // The followups route applies restrictFollowupsToStarter, as message_foreman.
    await c.backend.api(c.env, p, "POST", `/runs/${encodeURIComponent(runId)}/followups`, { message: msg });
  } else {
    const run = await c.backend.api<RunJson>(c.env, p, "POST", `/factories/${encodeURIComponent(wi.factory_id)}/runs`, {
      prompt: msg,
      title: wi.title,
      source: wi.source ?? {},
      metadata: { triggered_by: "mcp" },
    });
    runId = run.run_id ?? run.id ?? "";
  }
  // Clear the local owner and record the hand-back on the timeline.
  await c.backend.patchWorkItem(c.env, p, wi.id, {
    local_owner: "",
    ...(s(a, "notify_route") ? { labels: { notify_route: s(a, "notify_route") } } : {}),
    event: {
      kind: "note",
      actor: actorOf(p),
      data: { handed_back: true, branch: s(a, "branch"), pull_request_url: s(a, "pull_request_url") },
    },
  });
  return {
    note: "The work returned to the same work item; the foreman decides the next step. Handing back does not complete the work item.",
    run_id: runId,
    status: "handed_back",
    url: workItemURL(c, wi.factory_id, wi.id),
    work_item_id: wi.id,
  };
}

export type { ErrorCode };
