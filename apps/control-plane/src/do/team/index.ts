// TeamDO (key team:<id>): users, identities, API keys, service accounts,
// secrets, factories, definition versions and compiled runtime, settings,
// worker records, integrations and schedules, MCP registry and mcpauth state.
//
// One class composed of modules. Each module file exports a TeamModule
// (./kit.ts):
//
//   migrations: readonly Migration[]   numbers in its range: auth, secrets,
//                                      factories, settings 1-99; integrations,
//                                      schedules, webhooks, user forge tokens
//                                      100-199; mcp 200-299;
//                                      workers 300-399; scoring 400-499
//   ops: Record<string, TeamOp>        handlers of TeamDO.api(meta, op, ...),
//                                      keyed by openapi operationId (or an
//                                      internal name for DO-to-DO calls)
//   setup?(kit: TeamKit): void         registers timers and outbox handlers
//
// Modules import their types from ./kit, never from this file. The typed RPC
// methods of TeamRpc delegate to named exports of the modules.
import { OfError, notImplemented, toOfError } from "@openfactory/core";
import type { ApiKeyLookup, CallMeta, Principal, Role, TeamRpc } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { ROUTE_META, type RouteMeta, type RouteRole } from "../../contracts/routes";
import { isFactoryScoped, requireRole } from "../../auth/roles";
import { BaseDO } from "../../lib/base";
import { pruneAttachments, retentionMs, type RetentionRpc } from "../../retention/sweep";
import { LiveHub, isUpgrade } from "../../lib/live/index";
import type { OpError } from "../../lib/rpc";
import type { Migration } from "../../lib/sql/migrate";
import * as auth from "./auth";
import * as factories from "./factories";
import * as definitions from "./factories/definitions";
import * as integrations from "./integrations";
import * as mcp from "./mcp";
import * as preflight from "./preflight";
import * as schedules from "./schedules";
import * as scoring from "./scoring";
import * as secrets from "./secrets";
import * as settings from "./settings";
import * as userForgeTokens from "./user-forge-tokens";
import * as webhooks from "./webhooks";
import * as workers from "./workers";
import type { OpCall, TeamKit, TeamModule, TeamOp } from "./kit";

/** Prefix of query parameters in the params of api() ("?limit"). */
const QUERY_PREFIX = "?";

const MODULES: Record<string, TeamModule> = {
  auth,
  secrets,
  factories,
  settings,
  workers,
  integrations,
  schedules,
  webhooks,
  userForgeTokens,
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

const ROUTE_BY_OP = new Map<string, RouteMeta>(ROUTE_META.map((r) => [r.operationId, r]));

/**
 * Minimum role of internal ops (no REST route) that authorize a caller.
 * Without an entry, authorize() only requires a principal.
 */
const INTERNAL_OP_ROLE: Record<string, RouteRole> = {
  automationForRun: "editor",
  scoringFactory: "user",
  webhookForRoute: "user",
  scoringBenchmarkPlan: "editor",
};
const OPS = allOps();
const MIGRATIONS = allMigrations();
const PREFLIGHTS = Object.values(MODULES).flatMap((m) => m.preflights ?? []);
const REWRAP_SOURCES = Object.values(MODULES).flatMap((m) => m.rewrapSources ?? []);

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
      preflights: PREFLIGHTS,
      rewrapSources: REWRAP_SOURCES,
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
    return MIGRATIONS;
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

  // ---------- REST ----------

  /**
   * REST handlers of TeamDO-owned routes. `op` is the openapi operationId.
   * Query parameters arrive in `params` with the "?" prefix ("?limit").
   * Returns the body (200), an OpResponse, or an OpError (never throws an OfError).
   */
  async api(meta: CallMeta, op: string, params: Record<string, string>, body: unknown): Promise<unknown> {
    try {
      const fn = OPS.get(op);
      if (!fn) throw notImplemented(`${op} is not implemented yet`);
      const route = ROUTE_BY_OP.get(op);
      const role: RouteRole | "" = route?.role ?? INTERNAL_OP_ROLE[op] ?? "";
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
    return this.rpc(() => definitions.definitionApply(this.kit, meta, factoryId, input));
  }

  secretsResolve(
    meta: CallMeta,
    input: { factoryId: string; runId?: string; names: string[] },
  ): Promise<Record<string, string>> {
    return this.rpc(() => secrets.resolve(this.kit, meta, input));
  }

  configSnapshot(factoryId: string): Promise<unknown> {
    return this.rpc(() => definitions.configSnapshot(this.kit, factoryId));
  }

  proposeDefinition(meta: CallMeta, input: definitions.ProposeDefinitionInput): Promise<unknown> {
    return this.rpc(() => definitions.proposeDefinition(this.kit, meta, input));
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
