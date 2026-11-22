// TeamDO (key team:<id>): users, identities, API keys, service accounts,
// secrets, factories, definition versions and compiled runtime, settings
// (WS4); worker records (WS6); integrations and schedules (WS10); MCP
// registry and mcpauth state (WS11).
//
// One class composed of modules. Each module file exports:
//
//   migrations: readonly Migration[]   numbers in its range (05 §4.5):
//                                      WS4 1-99, WS10 100-199, WS11 200-299,
//                                      WS6 300-399, WS13 400-499
//   ops: Record<string, TeamOp>        handlers of TeamDO.api(meta, op, ...),
//                                      keyed by openapi operationId (or an
//                                      internal name for DO-to-DO calls)
//   setup?(kit: TeamKit): void         registers timers and outbox handlers
//
// The typed RPC methods of TeamRpc delegate to named exports of the modules.
import { OfError, notImplemented, toOfError } from "@openfactory/core";
import type { ApiKeyLookup, CallMeta, OutboxKind, Principal, Role, TeamRpc } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { ROUTES } from "../../http/routes";
import type { RouteRole } from "../../http/handler";
import { isFactoryScoped, requireRole } from "../../auth/roles";
import { BaseDO, type MessageHandler } from "../../lib/base";
import { pruneAttachments, retentionMs, type RetentionRpc } from "../../retention/sweep";
import { LiveHub, isUpgrade } from "../../lib/live/index";
import type { EnqueueInput } from "../../lib/outbox/outbox";
import type { Migration } from "../../lib/sql/migrate";
import type { AsyncTimerHandler, SyncTimerHandler, Timers } from "../../lib/timers/timers";
import * as auth from "./auth";
import * as factories from "./factories";
import * as integrations from "./integrations";
import * as mcp from "./mcp";
import * as preflight from "./preflight";
import * as schedules from "./schedules";
import * as scoring from "./scoring";
import * as secrets from "./secrets";
import * as settings from "./settings";
import * as workers from "./workers";

/** What a module sees of the TeamDO. */
export interface TeamKit {
  readonly env: Env;
  readonly sql: SqlStorage;
  readonly storage: DurableObjectStorage;
  readonly timers: Timers;
  readonly live: LiveHub;
  teamId(): string;
  doName(): string;
  /** One transactionSync; re-arms the alarm after. No await inside. */
  tx<T>(fn: () => T): T;
  /** Outbox row; call inside tx with the state change. */
  enqueue<K extends string>(input: EnqueueInput<K>): string;
  setTimer(name: string, kind: string, dueMs: number, payload?: unknown): void;
  clearTimer(name: string): boolean;
  onTimer<P = unknown>(kind: string, handler: SyncTimerHandler<P>): void;
  onTimerAsync<P = unknown>(kind: string, handler: AsyncTimerHandler<P>): void;
  onMessage<K extends OutboxKind>(kind: K, handler: MessageHandler<K>): void;
  /** Receiver-side dedupe helpers for custom flows. */
  seen(msgId: string): boolean;
  markSeen(msgId: string): void;
}

/** One call of TeamDO.api. */
export interface OpCall {
  op: string;
  meta: CallMeta;
  /** Path parameters. */
  params: Record<string, string>;
  /** Query parameters (single values). */
  query: Record<string, string>;
  body: unknown;
  /** Minimum role of the route ("" for ops that are not routes). */
  role: RouteRole | "";
  /** The principal, or throws 401. */
  principal(): Principal;
  /**
   * Checks the route role against the principal's role for this factory
   * ("" = team role). Factory-scoped routes must call it before reading
   * the resource, with "" when the factory does not exist (then 404).
   */
  authorize(factoryId: string): Principal;
}

export type TeamOp = (kit: TeamKit, call: OpCall) => unknown;

export interface TeamModule {
  migrations: readonly Migration[];
  ops: Record<string, TeamOp>;
  setup?: (kit: TeamKit) => void;
}

/** A non-200 result of an op. Plain objects cross the RPC boundary. */
export interface OpResponse {
  $status: number;
  body?: unknown;
  headers?: Record<string, string>;
}

export const created = (body: unknown, headers?: Record<string, string>): OpResponse => ({
  $status: 201,
  body,
  ...(headers ? { headers } : {}),
});
export const noContent = (): OpResponse => ({ $status: 204 });
export const withStatus = (status: number, body?: unknown): OpResponse => ({ $status: status, body });

export function isOpResponse(v: unknown): v is OpResponse {
  return typeof v === "object" && v !== null && typeof (v as OpResponse).$status === "number";
}

/** Error result of api(): OfError fields, so that detail and extra survive RPC. */
export interface OpError {
  $error: {
    code: string;
    detail: string;
    reason?: string;
    provider?: string;
    authUrl?: string;
    setupUrl?: string;
    extra?: Record<string, unknown>;
  };
}

export function isOpError(v: unknown): v is OpError {
  return typeof v === "object" && v !== null && typeof (v as OpError).$error === "object";
}

export const QUERY_PREFIX = "?";

const MODULES: Record<string, TeamModule> = {
  auth,
  secrets,
  factories,
  settings,
  workers,
  integrations,
  schedules,
  mcp,
  scoring,
  preflight,
};

function allMigrations(): Migration[] {
  const list = Object.entries(MODULES).flatMap(([name, m]) =>
    m.migrations.map((x) => ({ ...x, name: `${name}/${x.name}` })),
  );
  list.sort((a, b) => a.version - b.version);
  for (let i = 1; i < list.length; i++) {
    if (list[i]!.version === list[i - 1]!.version) {
      throw new Error(`TeamDO: migration ${list[i]!.version} defined twice (${list[i - 1]!.name}, ${list[i]!.name})`);
    }
  }
  return list;
}

function allOps(): Map<string, TeamOp> {
  const ops = new Map<string, TeamOp>();
  for (const [name, m] of Object.entries(MODULES)) {
    for (const [op, fn] of Object.entries(m.ops)) {
      if (ops.has(op)) throw new Error(`TeamDO: op ${op} defined twice (module ${name})`);
      ops.set(op, fn);
    }
  }
  return ops;
}

const ROUTE_BY_OP = new Map(ROUTES.map((r) => [r.operationId, r]));
// Computed on first use: the modules import ./index, so a module-level call
// could run while one of them is still initializing (import cycle).
let opsCache: Map<string, TeamOp> | undefined;
function opsMap(): Map<string, TeamOp> {
  opsCache ??= allOps();
  return opsCache;
}

export class TeamDO extends BaseDO implements TeamRpc, RetentionRpc {
  readonly live: LiveHub;
  private readonly kit: TeamKit;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.live = new LiveHub(ctx);
    // eslint-disable-next-line @typescript-eslint/no-this-alias
    const self = this;
    this.kit = {
      env,
      sql: this.sql,
      storage: ctx.storage,
      timers: this.timers,
      live: this.live,
      teamId: () => self.teamId(),
      doName: () => self.doName(),
      tx: (fn) => self.tx(fn),
      enqueue: (input) => self.enqueue(input),
      setTimer: (n, k, d, p) => self.setTimer(n, k, d, p),
      clearTimer: (n) => self.clearTimer(n),
      onTimer: (k, h) => self.onTimer(k, h),
      onTimerAsync: (k, h) => self.onTimerAsync(k, h),
      onMessage: (k, h) => self.onMessage(k, h),
      seen: (id) => self.seen(id),
      markSeen: (id) => self.markSeen(id),
    };
    for (const m of Object.values(MODULES)) m.setup?.(this.kit);
  }

  protected migrations(): readonly Migration[] {
    return allMigrations();
  }

  /** Liveness probe for tests and the readiness check. */
  async ping(): Promise<string> {
    return "TeamDO";
  }

  override async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    if (url.pathname === "/subscribe/definitions" && isUpgrade(request)) {
      return this.live.accept(["definitions"], { factoryId: url.searchParams.get("factory_id") ?? "" });
    }
    return new Response("not found", { status: 404 });
  }

  override async webSocketMessage(): Promise<void> {
    // Subscribers never send.
  }

  override async webSocketClose(ws: WebSocket, code: number): Promise<void> {
    try {
      ws.close(code === 1005 ? 1000 : code, "bye");
    } catch {
      /* already closed */
    }
  }

  // ---------- REST ----------

  /**
   * REST handlers of TeamDO-owned routes. `op` is the openapi operationId.
   * Query parameters arrive in `params` with the "?" prefix ("?limit").
   * Returns the body (200), an OpResponse, or an OpError (never throws an OfError).
   */
  async api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown> {
    try {
      const fn = opsMap().get(op);
      if (!fn) throw notImplemented(`${op} is not implemented yet`);
      const route = ROUTE_BY_OP.get(op);
      const role: RouteRole | "" = route?.role ?? "";
      const path: Record<string, string> = {};
      const query: Record<string, string> = {};
      for (const [k, v] of Object.entries(params)) {
        if (k.startsWith(QUERY_PREFIX)) query[k.slice(QUERY_PREFIX.length)] = v;
        else path[k] = v;
      }
      const scoped = route ? isFactoryScoped(route) : false;
      let authorized = !route || role === "public";
      const call: OpCall = {
        op,
        meta,
        params: path,
        query,
        body,
        role,
        principal: () => requireRole(meta.principal, "", "public"),
        authorize: (factoryId) => {
          const p = requireRole(meta.principal, factoryId, role === "" ? "public" : role);
          authorized = true;
          return p;
        },
      };
      if (route && !scoped && role !== "public") call.authorize("");
      const out = await fn(this.kit, call);
      if (!authorized) {
        console.error(`TeamDO: op ${op} returned without authorize()`);
        throw new OfError("internal_error", "authorization was not checked");
      }
      return out;
    } catch (e) {
      const { error, known } = toOfError(e);
      if (!known) console.error(`TeamDO: op ${op} failed`, e);
      return {
        $error: {
          code: error.code,
          detail: error.detail,
          ...(error.reason ? { reason: error.reason } : {}),
          ...(error.provider ? { provider: error.provider } : {}),
          ...(error.authUrl ? { authUrl: error.authUrl } : {}),
          ...(error.setupUrl ? { setupUrl: error.setupUrl } : {}),
          ...(error.extra ? { extra: error.extra } : {}),
        },
      } satisfies OpError;
    }
  }

  // ---------- typed RPC (TeamRpc) ----------

  authLookupApiKey(keyHash: string): Promise<ApiKeyLookup | null> {
    return this.rpc(() => auth.lookupApiKey(this.kit, keyHash));
  }

  authSession(sessionId: string): Promise<Principal | null> {
    return this.rpc(() => auth.sessionPrincipal(this.kit, sessionId));
  }

  definitionApply(meta: CallMeta, factoryId: string, input: unknown): Promise<unknown> {
    return this.rpc(() => factories.definitionApply(this.kit, meta, factoryId, input));
  }

  secretsResolve(
    meta: CallMeta,
    input: { factoryId: string; runId?: string; names: string[] },
  ): Promise<Record<string, string>> {
    return this.rpc(() => secrets.resolve(this.kit, meta, input));
  }

  configSnapshot(factoryId: string): Promise<unknown> {
    return this.rpc(() => factories.configSnapshot(this.kit, factoryId));
  }

  proposeDefinition(meta: CallMeta, input: factories.ProposeDefinitionInput): Promise<unknown> {
    return this.rpc(() => factories.proposeDefinition(this.kit, meta, input));
  }

  workersEnroll(input: Parameters<TeamRpc["workersEnroll"]>[0]): Promise<{ workerId: string }> {
    return this.rpc(() => workers.workersEnroll(this.kit, input));
  }

  workersUpdate(meta: CallMeta, workerId: string, patch: unknown): Promise<unknown> {
    return this.rpc(() => workers.workersUpdate(this.kit, meta, workerId, patch));
  }

  workersRevoke(meta: CallMeta, workerId: string): Promise<unknown> {
    return this.rpc(() => workers.workersRevoke(this.kit, meta, workerId));
  }

  /**
   * One retention step (retention.sweep job): deletes uploaded attachments in
   * R2 older than OF_OBJECT_STORE_RETENTION, one list page per call. The list
   * cursor is kept between steps.
   */
  async retentionSweep(): Promise<{ more: boolean }> {
    return this.rpc(async () => {
      const keep = retentionMs(this.env);
      if (keep <= 0) return { more: false };
      const cursor = await this.ctx.storage.get<string>("retention:attachments-cursor");
      const out = await pruneAttachments(this.env.DATA, Date.now(), keep, cursor);
      if (out.more && out.cursor) await this.ctx.storage.put("retention:attachments-cursor", out.cursor);
      else await this.ctx.storage.delete("retention:attachments-cursor");
      return { more: out.more };
    });
  }

  /** POST /secrets/rewrap: rewraps every envelope under the current key. */
  secretsRewrap(meta: CallMeta): Promise<unknown> {
    return this.rpc(() => secrets.rewrapAll(this.kit, meta));
  }
}

export type { Role };
