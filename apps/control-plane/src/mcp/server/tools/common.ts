// Shared parts of the Factory MCP tools: the request context, argument
// readers, input schema builders, the tool wrapper, the REST wire types and
// the REST reads that more than one tool module uses.
import {
  OfError,
  factoryLink,
  toOfError,
  workItemLink,
  type Factory as CoreFactory,
  type InboxItem,
  type Run,
  type SourceRef,
  type Stage,
  type WorkItem as CoreWorkItem,
} from "@openfactory/core";
import type { Principal } from "../../../contracts/do-messages";
import { roleRank } from "../../../auth/roles";
import type { Env } from "../../../contracts/env";
import { textResult, type CallToolResult, type JsonSchema, type Tool } from "../../jsonrpc";
import type { FactoryMcpBackend } from "../backend";
import type { PullRequestRef } from "../refs";

/** Per-request context of the Factory MCP tools. */
export interface FactoryCtx {
  env: Env;
  principal: Principal;
  backend: FactoryMcpBackend;
  publicURL: string;
}

export type Args = Record<string, unknown>;
export type Json = Record<string, unknown>;

export const invalid = (msg: string) => new OfError("invalid_request", msg);
export const notFound = (msg: string) => new OfError("resource_not_found", msg);

export const s = (a: Args, k: string): string => (typeof a[k] === "string" ? (a[k] as string) : "");
export const n = (a: Args, k: string): number => (typeof a[k] === "number" ? Math.trunc(a[k] as number) : 0);
export const b = (a: Args, k: string): boolean => a[k] === true;

export const userIdOf = (p: Principal) => p.userId ?? "";
export const actorOf = (p: Principal) => (p.userId ? "user:" + p.userId : "api");
export const can = (p: Principal, role: "user" | "editor" | "admin") => roleRank(p.role) >= roleRank(role);

export function checkTeam(p: Principal, teamId: string): void {
  if (teamId !== "" && teamId !== p.teamId) throw notFound(`team ${teamId} not found`);
}

// ---------- input schemas (hand-written; go-sdk inferred them from struct tags) ----------

export type Prop = {
  type: string;
  description?: string;
  items?: unknown;
  additionalProperties?: unknown;
  properties?: unknown;
  required?: string[];
};

export function obj(props: Record<string, Prop>, required: string[] = []): JsonSchema {
  return { type: "object", properties: props, required, additionalProperties: false };
}
export const str = (description?: string): Prop => ({ type: "string", ...(description ? { description } : {}) });
export const int = (description: string): Prop => ({ type: "integer", description });
export const bool = (description: string): Prop => ({ type: "boolean", description });

// ---------- the tool wrapper ----------

/** errorResult: a tool-level failure the model can act on. */
function errorResult(e: unknown): CallToolResult {
  const { error, known } = toOfError(e);
  if (!known) console.error("mcp tool failed", e);
  return textResult(JSON.stringify({ error: { code: error.code, message: error.detail || error.code } }), true);
}

function jsonResult(v: unknown): CallToolResult {
  return textResult(JSON.stringify(v, null, 2));
}

type Handler = (c: FactoryCtx, a: Args) => Promise<unknown>;

export function tool(
  name: string,
  description: string,
  readOnly: boolean,
  inputSchema: JsonSchema,
  h: Handler,
): Tool<FactoryCtx> {
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

// ---------- REST wire types ----------

/** A factory as the REST API returns it. */
export type Factory = Pick<CoreFactory, "id" | "name"> &
  Partial<Omit<CoreFactory, "id" | "name">> & { uid?: string; url?: string };

/** A work item as the REST API returns it. */
export type WorkItem = Pick<CoreWorkItem, "id" | "factory_id" | "title" | "stage" | "created_at" | "updated_at"> &
  Partial<Pick<CoreWorkItem, "source_key" | "creator_id" | "local_owner" | "root_run_id" | "labels">> & {
    source?: Partial<SourceRef>;
    pull_requests?: PullRequestRef[];
    url?: string;
  };

/** A run as the REST API returns it (a work item detail names it by run_id). */
export type RunJson = Pick<Run, "state" | "created_at"> &
  Partial<
    Pick<Run, "id" | "agent_name" | "name" | "parent_run_id" | "result" | "error" | "finished_at" | "work_item_id">
  > & { run_id?: string; url?: string; run_url?: string };

/** An inbox item of a work item detail. Recipients may be user IDs or user objects. */
export type InboxJson = Pick<InboxItem, "id" | "kind" | "title"> &
  Partial<Pick<InboxItem, "options" | "questions" | "resolved" | "factory_id">> & { recipients?: unknown[] };

export type WorkItemDetail = WorkItem & { runs?: RunJson[]; inbox?: InboxJson[] };

interface Page<T> {
  items?: T[];
  page_info?: { next_cursor?: string };
}

export const TERMINAL_STAGES = new Set<Stage>(["complete", "cancelled"]);

// ---------- REST reads ----------

export async function factoryOf(c: FactoryCtx, id: string): Promise<Factory> {
  try {
    return await c.backend.api<Factory>(c.env, c.principal, "GET", `/factories/${encodeURIComponent(id)}`);
  } catch (e) {
    const { error } = toOfError(e);
    if (error.code === "resource_not_found" || error.code === "not_authorized")
      throw notFound(`factory ${id} not found`);
    throw error;
  }
}

/** Every factory the caller can read: all pages of GET /factories. */
export async function listFactoriesAll(c: FactoryCtx, search = ""): Promise<Factory[]> {
  const out: Factory[] = [];
  const seen = new Set<string>();
  let cursor = "";
  for (;;) {
    const q = new URLSearchParams({ limit: "200" });
    if (search) q.set("search", search);
    if (cursor) q.set("cursor", cursor);
    const page = await c.backend.api<Page<Factory>>(c.env, c.principal, "GET", `/factories?${q}`);
    out.push(...(page.items ?? []));
    cursor = page.page_info?.next_cursor ?? "";
    // A cursor seen before would loop forever.
    if (!cursor || seen.has(cursor)) return out;
    seen.add(cursor);
  }
}

export async function workItemDetail(c: FactoryCtx, id: string): Promise<WorkItemDetail> {
  return c.backend.api<WorkItemDetail>(c.env, c.principal, "GET", `/work-items/${encodeURIComponent(id)}`);
}

/** Up to max work items of one factory, newest first, over as many pages as needed. */
export async function listWorkItemsOf(
  c: FactoryCtx,
  factoryId: string,
  max: number,
  query: Record<string, string> = {},
): Promise<WorkItem[]> {
  const out: WorkItem[] = [];
  let cursor = "";
  while (out.length < max) {
    const q = new URLSearchParams({ ...query, limit: String(Math.min(200, max - out.length)) });
    if (cursor) q.set("cursor", cursor);
    const page = await c.backend.api<Page<WorkItem>>(
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

// ---------- views ----------

export const factoryURL = (c: FactoryCtx, f: Factory) => f.url || factoryLink(c.publicURL, f.id);
export const workItemURL = (c: FactoryCtx, fid: string, id: string) => workItemLink(c.publicURL, fid, id);

export function workItemView(c: FactoryCtx, w: WorkItem): Json {
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
