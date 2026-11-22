// Wiring of the control-plane modules (integration step). src/index.ts imports
// this module for its side effects:
//
// - server routes: /webhooks/w/*, /integrations/github/webhook (WS10a);
//   /integrations/{slack/events,slack/interactions,teams/messages,linear/webhook,jira/webhook}
//   and the account linker pages /integrations/link*, /integrations/links/* (WS10b)
// - responders and tracker write-backs of Slack, Teams, Linear and Jira (notify)
// - the integration clients of the built-in MCP servers (WS11)
// - the in-process sandbox providers daytona and boatdev (credentials: Worker secrets)
// - the active-run check of schedules with concurrency "skip"
// - the git credential minter of the worker git cache
// TeamDO first: its modules import each other through do/team/index.
import "./do/team/index";
import {
  DEFAULT_TEAM,
  FINISHED_RUN_STATES,
  INBOX_VIA_CHAT,
  OfError,
  doName,
  partitionOf,
  sameIntegrationProvider,
  startedByAutomation,
  type Event,
  type Integration,
  type User,
} from "@openfactory/core";
import { createBoatdevProvider } from "@openfactory/boatdev";
import { createDaytonaProvider } from "@openfactory/daytona";
import { routeWebhookIngress } from "./api/webhooks";
import { authenticate } from "./auth/authenticate";
import { registerServerRoute } from "./builder";
import type { CallMeta, Principal } from "./contracts/do-messages";
import type { Env } from "./contracts/env";
import { registerProvider } from "./do/dispatch/providers";
import { configSnapshot, listFactories } from "./do/team/factories";
import { setScheduleActiveRunCheck } from "./do/team/schedules";
import { normalizeRepo, setGitCredentialMinter } from "./do/team/workers";
import { resolverFor } from "./forges/env";
import { reviewBuilders } from "./eventctx/index";
import { setEventContextBuilders } from "./do/ingress/intake";
import { b64url, sha256 } from "./lib/bytes";
import { adfText, markdownToAdf } from "./integrations/jira/adf";
import { newApiClient as jiraApiClient } from "./integrations/jira/jira";
import { LinearClient } from "./integrations/linear/linear";
import { LINK_CALLBACK_PATH, LINK_PATH, LINK_START_PATH, LINKS_PATH, Linker } from "./integrations/link";
import { teamProver } from "./integrations/prover";
import { createIntegrationHandlers, INBOUND_PATHS, type IntegrationHandlers } from "./integrations/registry";
import { selectTeam } from "./integrations/select";
import { SlackClient } from "./integrations/slack/client";
import type { InboxItemRef, WorkSide } from "./integrations/types";
import { integrationSecret } from "./integrations/util";
import { integrationDeps, rpcSecrets, rpcStore } from "./integrations/worker";
import { setMcpBackends } from "./mcp/index";
import type { IntegrationBackend, IntegrationRecord } from "./mcp/registry/builtin";
import { registerResponders } from "./notify/index";

const META: CallMeta = { requestId: "integrations" };

// ---------- integrations (WS10b) ----------

/** Partition of a chat or tracker event: one per source thread or issue, so events of one thread stay in order. */
export async function integrationPartition(ev: Event): Promise<string> {
  const s = ev.source;
  const key = [
    ev.provider,
    s.workspace ?? "",
    s.repo ?? "",
    s.channel ?? "",
    s.thread_id ?? "",
    s.external_id ?? "",
    s.number ?? "",
  ].join("\u0000");
  return `${ev.provider}:${b64url(await sha256(key)).slice(0, 22)}`;
}

/** The IngressDO enqueue of normalized events. */
export async function publishEvents(env: Env, events: Event[]): Promise<void> {
  for (const ev of events) {
    const teamId = ev.team_id || DEFAULT_TEAM;
    const partition = await integrationPartition(ev);
    await env.INGRESS.getByName(doName.ingress(teamId, partition)).enqueue(
      partition,
      ev as never,
      ev.delivery_id || ev.id,
    );
  }
}

function workStub(env: Env, teamId: string, id: string) {
  return env.WORK.getByName(doName.work(teamId, partitionOf(id)));
}

/** What the integrations need from WorkDO and IndexDO. */
export function workSide(env: Env, teamId = DEFAULT_TEAM): WorkSide {
  return {
    async factoryOf(workItemId) {
      const wi = (await workStub(env, teamId, workItemId).api(META, "getWorkItem", { id: workItemId }, null)) as {
        factory_id?: string;
      };
      return wi.factory_id ?? "";
    },
    async openInboxItem(workItemId): Promise<InboxItemRef | null> {
      // Newest unresolved item of the work item, approvals and questions first (integrations.OpenInboxItem).
      const out = (await env.INDEX.getByName(doName.index(teamId)).list(META, "inbox", {
        work_item_id: workItemId,
        resolved: "false",
        scope: "team",
        limit: "200",
      })) as { items?: { id: string; kind?: string; created_at?: string }[] };
      let best: { id: string; kind?: string; created_at?: string } | undefined;
      const pref = (k?: string) => k === "approval" || k === "question";
      for (const it of out.items ?? []) {
        if (
          !best ||
          (pref(it.kind) && !pref(best.kind)) ||
          (pref(it.kind) === pref(best.kind) && (it.created_at ?? "") > (best.created_at ?? ""))
        )
          best = it;
      }
      return best ? { id: best.id } : null;
    },
    async onAction(inboxId, option, actor) {
      const principal: Principal = { kind: "user", id: actor, userId: actor, teamId, role: "user", factoryRoles: {} };
      await workStub(env, teamId, inboxId).api(
        { requestId: "chat-action", principal },
        "resolveInbox",
        { id: inboxId },
        { resolution: option, via: INBOX_VIA_CHAT },
      );
    },
  };
}

/** The account linker (signed link pages, provider sign-in, unlink). */
export function linkerFor(env: Env, teamId = DEFAULT_TEAM): Linker {
  const store = rpcStore(env, teamId);
  const secrets = rpcSecrets(env, teamId);
  return new Linker({
    store,
    key: env.OF_SIGNING_KEY,
    publicUrl: env.OF_PUBLIC_URL,
    currentUser: async (req): Promise<User | null> => {
      const p = await authenticate(req, env);
      if (!p || p.kind !== "user" || !p.userId) return null;
      return store.getUser(p.userId);
    },
    loginUrl: (next) => "/login?return_to=" + encodeURIComponent(next),
    prover: (t, provider) => teamProver(store, secrets, t, provider),
    audit: (rec) => {
      const line = { audit: true, ...rec };
      if (rec.outcome === "failure") console.warn("audit: " + rec.action, line);
      else console.log("audit: " + rec.action, line);
    },
  });
}

let cached: { env: Env; handlers: IntegrationHandlers; linker: Linker } | undefined;

/** The integration handlers of the default team, built once per env. */
export function integrationsFor(env: Env): { handlers: IntegrationHandlers; linker: Linker } {
  if (cached && cached.env === env) return cached;
  const linker = linkerFor(env);
  const handlers = createIntegrationHandlers(
    integrationDeps(env, { publish: (evs) => publishEvents(env, evs), linker, work: workSide(env) }),
  );
  cached = { env, handlers, linker };
  return cached;
}

const INBOUND = new Set<string>(INBOUND_PATHS);

function isLinkerPath(path: string): boolean {
  return (
    path.startsWith(LINK_PATH) ||
    path.startsWith(LINK_START_PATH) ||
    path === LINK_CALLBACK_PATH ||
    path.startsWith(LINKS_PATH)
  );
}

registerServerRoute("/webhooks/w/", (request, env, ctx) => routeWebhookIngress(request, env, ctx));
registerServerRoute("/integrations/github/webhook", (request, env, ctx) => routeWebhookIngress(request, env, ctx));
registerServerRoute("/integrations/", async (request, env, ctx, url) => {
  const path = url.pathname;
  if (INBOUND.has(path)) {
    const out = await integrationsFor(env).handlers.handleRequest(request);
    if (!out) return new Response("Method Not Allowed\n", { status: 405, headers: { allow: "POST" } });
    if (out.background) {
      const bg = out.background;
      ctx.waitUntil(bg().catch((e: unknown) => console.error("integrations: background step failed", path, e)));
    }
    return out.response;
  }
  if (isLinkerPath(path)) return integrationsFor(env).linker.handle(request);
  return null;
});

registerResponders((env) => Object.values(integrationsFor(env).handlers.responders));

// Event context builders of intake: chat threads (slack, teams) and forge review threads.
setEventContextBuilders((env) => [
  ...reviewBuilders(resolverFor(env)),
  ...Object.entries(integrationsFor(env).handlers.contextBuilders).map(([provider, b]) => ({
    provider: () => provider,
    eventContext: (ev: Event) => b.eventContext(ev),
  })),
]);

// ---------- built-in MCP integration servers (WS11) ----------

function asIntegration(rec: IntegrationRecord): Integration {
  return rec as unknown as Integration;
}

/** The integration clients of /mcp/integrations/{provider}/{runId}. */
export function integrationBackend(env: Env): IntegrationBackend {
  return {
    async factoryIntegration(_env, run, provider, workspace) {
      const store = rpcStore(env, run.teamId || DEFAULT_TEAM);
      const attached = await store.getFactoryIntegrations(run.factoryId).catch((e: unknown) => {
        if (e instanceof OfError && e.code === "resource_not_found") return {};
        throw e;
      });
      if (!Object.keys(attached).some((k) => sameIntegrationProvider(k, provider))) {
        throw new Error(
          `the factory does not attach the ${provider} integration (add it to factory.yaml integrations)`,
        );
      }
      return (await selectTeam(
        store,
        run.teamId || DEFAULT_TEAM,
        provider,
        run.factoryId,
        workspace,
      )) as unknown as IntegrationRecord;
    },
    credential: (_env, run, rec) =>
      integrationSecret(rpcSecrets(env, run.teamId || DEFAULT_TEAM), asIntegration(rec), ""),
    linearQuery: (rec, token, query, variables) =>
      new LinearClient("", token, rec.config["token_type"] ?? "").query(query, variables),
    linearCreateComment: (rec, token, issueId, body) =>
      new LinearClient("", token, rec.config["token_type"] ?? "").createComment(issueId, body),
    jiraRequest: (rec, token, method, path, body) =>
      jiraApiClient(asIntegration(rec), token).json(method, path, body as never),
    jiraText: (adf) => adfText(adf),
    markdownToADF: (md) => markdownToAdf(md),
    slackCall: (_rec, token, method, params) => new SlackClient("", token).call(method, new URLSearchParams(params)),
    slackPost: (_rec, token, msg) => new SlackClient("", token).postMessage(msg),
    teamsPost: (rec, channelId, rootId, text) =>
      integrationsFor(env).handlers.teams.handler.postMessage(asIntegration(rec), channelId, rootId, text),
  };
}

const backends = new WeakMap<Env, IntegrationBackend>();

/** Points the MCP surface at the integration clients of env (called per request; cached per env). */
export function bindMcpBackends(env: Env): void {
  let b = backends.get(env);
  if (!b) {
    b = integrationBackend(env);
    backends.set(env, b);
  }
  setMcpBackends({ integrations: b });
}

// ---------- sandbox providers ----------
//
// daytona and boatdev run in-process. Their credentials are Worker secrets
// (OF_DAYTONA_API_KEY, OF_BOATDEV_API_KEY), with OF_DAYTONA_TIER and
// OF_DAYTONA_TARGET as vars. The install ID that labels the sandboxes is the
// host of OF_PUBLIC_URL. local is registered by src/do/workerhub/providers.ts;
// cloudflare-sandbox is the service binding (src/do/dispatch/providers.ts).

function installId(env: Env): string {
  try {
    return new URL(env.OF_PUBLIC_URL).host || "openfactory";
  } catch {
    return "openfactory";
  }
}

registerProvider("daytona", (env) => {
  const apiKey = env.OF_DAYTONA_API_KEY ?? "";
  if (apiKey === "")
    throw new OfError(
      "invalid_request",
      "sandbox provider daytona is not configured: set the OF_DAYTONA_API_KEY secret",
    );
  return createDaytonaProvider({
    apiKey,
    installId: installId(env),
    tier: Number(env.OF_DAYTONA_TIER || "3"),
    ...(env.OF_DAYTONA_TARGET ? { target: env.OF_DAYTONA_TARGET } : {}),
    allowUnenforcedNetworkPolicy: env.OF_ALLOW_UNENFORCED_NETWORK_POLICY,
  });
});

registerProvider("boatdev", (env) => {
  const apiKey = env.OF_BOATDEV_API_KEY ?? "";
  if (apiKey === "")
    throw new OfError(
      "invalid_request",
      "sandbox provider boatdev is not configured: set the OF_BOATDEV_API_KEY secret",
    );
  return createBoatdevProvider({ apiKey, installId: installId(env) });
});

// ---------- schedules: concurrency "skip" ----------

const FINISHED = new Set<string>(FINISHED_RUN_STATES);

setScheduleActiveRunCheck(async (kit, factoryId, automationId) => {
  const idx = kit.env.INDEX.getByName(doName.index(kit.teamId()));
  let cursor = "";
  for (let page = 0; page < 50; page++) {
    const out = (await idx.list(META, "runs", {
      factory_id: factoryId,
      limit: "200",
      ...(cursor ? { cursor } : {}),
    })) as {
      items?: { id: string; state?: string; triggered_by?: string }[];
      page_info?: { next_cursor?: string };
    };
    for (const r of out.items ?? []) {
      if (!FINISHED.has((r.state ?? "").toLowerCase()) && startedByAutomation(r.triggered_by ?? "", automationId))
        return r.id;
    }
    cursor = out.page_info?.next_cursor ?? "";
    if (!cursor) break;
  }
  return "";
});

// ---------- worker git cache: read-only credential of one repository ----------

/** Life reported for a minted git credential (engine runnerGitCredentialTTL). */
const GIT_CREDENTIAL_TTL_MS = 50 * 60_000;

function withoutCredentials(u: string): string {
  try {
    const x = new URL(u);
    x.username = "";
    x.password = "";
    return x.toString();
  } catch {
    return u;
  }
}

/**
 * Finds the repository among the definition repositories of the worker's
 * factories (all factories when the worker has no list) and returns the
 * credential of the forge clone URL. The integration of that factory decides
 * the credential, the same as for a run.
 */
setGitCredentialMinter(async (kit, input) => {
  const factoryIds = input.factories.length > 0 ? input.factories : listFactories(kit).map((f) => f.id);
  const resolver = resolverFor(kit.env);
  for (const factoryId of factoryIds) {
    let snap: { runtime?: { repositories?: { codeForge?: string; owner?: string; name?: string }[] | null } };
    try {
      snap = (await configSnapshot(kit, factoryId)) as typeof snap;
    } catch {
      continue;
    }
    for (const r of snap.runtime?.repositories ?? []) {
      if (!r.owner || !r.name) continue;
      const codeForge = (r.codeForge ?? "github").toUpperCase();
      const forge = await resolver
        .forge({ teamId: kit.teamId(), factoryId, codeForge, repo: `${r.owner}/${r.name}` })
        .catch(() => null);
      if (!forge) continue;
      const url = await forge.cloneURL(r.owner, r.name).catch(() => "");
      if (url === "" || normalizeRepo(withoutCredentials(url)) !== input.repo) continue;
      const u = new URL(url);
      const password = decodeURIComponent(u.password);
      if (password === "") throw new OfError("not_authorized", `the forge gave no credential for ${input.repo}`);
      return {
        username: decodeURIComponent(u.username),
        password,
        expiresAt: new Date(Date.now() + GIT_CREDENTIAL_TTL_MS).toISOString(),
      };
    }
  }
  throw new OfError("not_authorized", `${input.repo} is not a repository of the worker's factories`);
});
