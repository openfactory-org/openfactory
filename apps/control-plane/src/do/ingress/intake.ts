// Event intake: port of internal/engine.HandleEvent (events.go) in the
// IngressDO alarm (03 §7.1). One action at a time per partition. Each step is
// idempotent: RouterDO.resolveOrReserve gives a retried action the same work
// item token (plan rule 4), and the WorkDO ledger (actionKey, target)
// deduplicates the dispatch, so a retry creates no second run.
import { doName, newId, OfError, sourceKey, toOfError, type Automation, type Event } from "@openfactory/core";
import type { CallMeta, CoreEvent, Principal } from "../../contracts/do-messages";
import type { Env } from "../../contracts/env";
import { unwrapOp } from "../../lib/rpc";
import {
  actionKey,
  continuationKind,
  CONTINUE_USER,
  factoryCovers,
  isMention,
  isSelfEvent,
  linkRequiredFor,
  normalizeProvider,
  permanentCode,
  triggeredByFor,
  trustOfMessage,
} from "../../engine/intake";
import { buildDispatchPrompt, workItemTitle } from "../../engine/text";
import type { AgentConfig, Principal as RunPrincipal } from "../../engine/types";
import { dispatchPrincipals, factoryPrincipal, isApprover as approverMatch } from "../../engine/principal";
import { prFactsKey } from "../../metrics/build";
import { applyOverrides } from "../../scoring/overrides";
import { matchingAutomations } from "../../triggers/match";
import type { IntakeHandler, IntakeResult } from "./index";
import { build as buildBundle, promptSection, storeBundle, type Builder } from "../../eventctx/index";
import { resolverFor } from "../../forges/env";
import { githubActorEligible, postOnboardingNotice, type OnboardingReason } from "./onboarding";

/** The event context of one message: the R2 bundle, its message ID and its prompt section. */
export interface BundleRef {
  ref: string;
  messageId: string;
  promptSection: string;
}

/** Provider context builders of the event context (integrations, forge review threads). */
export type BuildersFor = (env: Env) => Builder[];
let buildersFor: BuildersFor | undefined;

/** Sets the builders (bootstrapControlPlane in src/wiring.ts). Returns the ones it replaces. */
export function setEventContextBuilders(fn: BuildersFor | undefined): BuildersFor | undefined {
  const prev = buildersFor;
  buildersFor = fn;
  return prev;
}

function contextBuilders(env: Env): Builder[] {
  if (!buildersFor) throw new Error("no event context builders are registered: call bootstrapControlPlane()");
  return buildersFor(env);
}

/**
 * eventBundle + storeEventBundle (engine/eventfiles.go): builds the event
 * context of a message in the IngressDO (the builders do network I/O) and
 * stores it in R2 by hash. withContext runs the provider builders (the first
 * message of a run). A failure gives no event files; intake goes on.
 */
export async function storeEventBundle(env: Env, ev: CoreEvent, withContext: boolean): Promise<BundleRef | undefined> {
  try {
    const messageId = newId("msg");
    const b = await buildBundle(messageId, ev as unknown as Event, withContext, contextBuilders(env));
    if (!b) return undefined;
    return { ref: await storeBundle(env.DATA, b), messageId, promptSection: promptSection(b) };
  } catch (e) {
    console.warn("intake: event context", ev.provider, ev.type, e instanceof Error ? e.message : String(e));
    return undefined;
  }
}

/** The reopen window of a completed work item (references.go ReopenWindow). */
export const REOPEN_WINDOW_MS = 7 * 24 * 3_600_000;

interface Snapshot {
  factory: { id: string; alias?: string; name?: string };
  runtime: {
    repositories?: { codeForge?: string; owner: string; name: string }[] | null;
    require_linked_actor?: boolean;
    restrict_followups_to_starter?: boolean;
    github_onboarding_notices?: boolean;
    approvers?: string[];
    machines?: Record<string, unknown>;
  };
  agents: AgentConfig[];
  automations: Automation[];
  integrations?: Record<string, unknown>;
}

interface User {
  id: string;
  team_id?: string;
  email?: string;
  name?: string;
  kind?: string;
}

/** The platform calls of intake. Tests replace them. */
export interface IntakeDeps {
  listFactoryIds(): Promise<string[]>;
  snapshot(factoryId: string): Promise<Snapshot>;
  factoryByAlias(alias: string): Promise<string | undefined>;
  user(id: string): Promise<User | undefined>;
  prFacts(ev: CoreEvent): Promise<void>;
  router: {
    resolve(factoryId: string, key: string): Promise<{ wiId: string; token: string; status: string } | null>;
    resolveOrReserve(i: {
      factoryId: string;
      sourceKeys: string[];
      actionKey: string;
      target: string;
    }): Promise<{ wiId: string; token: string; created: boolean }>;
    reopen(i: { factoryId: string; sourceKey: string; wiId: string }): Promise<boolean>;
  };
  work(token: string): {
    applyAction(i: unknown): Promise<{
      ok: true;
      served: boolean;
      terminal: boolean;
      workItemId?: string;
      rootRunId?: string;
      rootAgent?: string;
    }>;
    intakeState(): Promise<{
      workItemId?: string;
      stage?: string;
      rootRunId?: string;
      rootAgent?: string;
      creatorId?: string;
    }>;
    allowBotAction(actionKey: string): Promise<boolean>;
    reopen(i: { actionKey: string; event: CoreEvent }): Promise<unknown>;
  };
  /**
   * The principal that the runAs of an automation names (engine.resolveRunAs).
   * Throws invalid_request when it names no principal of the team. Without it
   * automation runs act as the factory.
   */
  resolveRunAs?(factoryId: string, runAs: string): Promise<RunPrincipal>;
  /** The event context of a message (optional: without it runs get no event files). */
  bundle?(ev: CoreEvent, withContext: boolean): Promise<BundleRef | undefined>;
  /** The GitHub onboarding comment of an ineligible account (optional). */
  onboardingNotice?(ev: CoreEvent, reason: OnboardingReason, factoryId: string): Promise<void>;
}

/** One bundle per (event, withContext) within one HandleEvent. */
function bundler(d: IntakeDeps, ev: CoreEvent): (withContext: boolean) => Promise<{ bundle?: BundleRef }> {
  const memo = new Map<boolean, Promise<BundleRef | undefined>>();
  return async (withContext) => {
    if (!d.bundle) return {};
    let p = memo.get(withContext);
    if (!p) {
      p = d.bundle(ev, withContext);
      memo.set(withContext, p);
    }
    const b = await p;
    return b ? { bundle: b } : {};
  };
}

export function platformIntakeDeps(env: Env, teamId: string): IntakeDeps {
  const team = env.TEAM.getByName(doName.team(teamId));
  const principal: Principal = { kind: "service_account", id: "intake", teamId, role: "admin", factoryRoles: {} };
  const meta = (): CallMeta => ({ requestId: newId("req"), principal });
  const router = env.ROUTER.getByName(doName.router(teamId));
  return {
    listFactoryIds: async () =>
      (
        unwrapOp<{ items?: { id: string }[] }>(
          await team.api(meta(), "listFactories", { "?limit": "500" }, null),
          "listFactories",
        ).items ?? []
      ).map((f) => f.id),
    snapshot: async (fid) => (await team.configSnapshot(fid)) as Snapshot,
    factoryByAlias: async (alias) => {
      try {
        return unwrapOp<{ id?: string }>(await team.api(meta(), "getFactory", { uid: alias }, null), "getFactory").id;
      } catch {
        return undefined;
      }
    },
    user: async (id) => {
      try {
        return unwrapOp<User>(await team.api(meta(), "getUser", { id }, null), "getUser");
      } catch {
        return undefined;
      }
    },
    prFacts: async (ev) => {
      const src = ev.source as { repo?: string; number?: number; kind?: string };
      if (!src?.repo || !src.number) return;
      const p = normalizeProvider(ev.provider);
      if (!["github", "gitlab", "azure_devops"].includes(p)) return;
      const prKey = prFactsKey({ forge: p.toUpperCase(), repo: src.repo, number: src.number });
      await env.INDEX.getByName(doName.index(teamId)).receive({
        msgId: `prf:${ev.id}`,
        kind: "index.prFacts",
        lane: `prfacts:${prKey}`,
        origin: doName.ingress(teamId, "intake"),
        payload: { prKey, event: ev },
      });
    },
    router: {
      resolve: (f, k) => router.resolve(f, k),
      resolveOrReserve: (i) => router.resolveOrReserve(i),
      reopen: (i) => router.reopen(i),
    },
    work: (token) => env.WORK.getByName(doName.work(teamId, token)) as unknown as ReturnType<IntakeDeps["work"]>,
    resolveRunAs: async (factoryId, runAs) =>
      unwrapOp<RunPrincipal>(
        await team.api(meta(), "principals.resolveRunAs", {}, { factoryId, runAs }),
        "principals.resolveRunAs",
      ),
    bundle: (ev, withContext) => storeEventBundle(env, ev, withContext),
    onboardingNotice: async (ev, reason, factoryId) => {
      await postOnboardingNotice(env, resolverFor(env), teamId, ev, reason, factoryId);
    },
  };
}

/** IsApprover (workitems.go): by email, or by name for a service account. */
export function isApprover(approvers: string[], u: User | undefined): boolean {
  return approverMatch(approvers, u);
}

function resolveAgent(snap: Snapshot, name: string): AgentConfig | undefined {
  if (!name) return snap.agents.find((a) => a.type === "FOREMAN") ?? snap.agents.find((a) => a.name === "foreman");
  return snap.agents.find((a) => a.name === name || a.id === name);
}

/** senderTrust: platform for schedules, external for webhooks, member for a linked user of the team. */
async function senderTrust(d: IntakeDeps, ev: CoreEvent, teamId: string): Promise<User | undefined> {
  const p = normalizeProvider(ev.provider);
  if (p === "schedule" || p === "system") {
    ev.actor.trust = "platform";
    return undefined;
  }
  ev.actor.trust = "external";
  if (p === "webhook" || !ev.actor.user_id) return undefined;
  const u = await d.user(ev.actor.user_id);
  if (u && (u.team_id ?? teamId) === (ev.team_id || teamId)) ev.actor.trust = "member";
  return u;
}

interface Errs {
  list: unknown[];
}

function retryErrorOf(errs: unknown[]): unknown {
  const transient = errs.find((e) => !permanentCode(toOfError(e).error.code));
  return transient ?? errs[0];
}

/** HandleEvent. Returns done, or a retry for a transient failure. */
export async function handleEvent(d: IntakeDeps, teamId: string, ev: CoreEvent): Promise<IntakeResult> {
  if (!ev) throw new OfError("invalid_request", "nil event");
  if (!ev.actor) ev.actor = { provider: ev.provider, external_id: "", handle: "", is_bot: false };
  ev.attributes ??= {};
  ev.source ??= {};
  const akey = actionKey(ev);
  if (akey) ev.action_key = akey;
  try {
    await d.prFacts(ev);
  } catch (e) {
    console.warn("intake: record pull request facts", ev.provider, ev.type, e);
  }
  if (isSelfEvent(ev)) return { done: true };

  // 2. Factories.
  const snaps: Snapshot[] = [];
  const ids = ev.factory_id ? [ev.factory_id] : await d.listFactoryIds();
  for (const id of ids) {
    const snap = await d.snapshot(id);
    if (ev.factory_id || factoryCovers(ev, snap.runtime.repositories, Object.keys(snap.integrations ?? {})))
      snaps.push(snap);
  }
  // 3. Sender.
  const user = await senderTrust(d, ev, teamId);

  const errs: Errs = { list: [] };
  const served = new Set<string>();
  const bundles = bundler(d, ev);
  for (const snap of snaps) {
    try {
      if (await handleForFactory(d, snap, ev, akey, user, errs, bundles)) served.add(snap.factory.id);
    } catch (e) {
      errs.list.push(e);
    }
  }

  // 7. A mention of a factory alias that nothing served goes to its foreman.
  if (isMention(ev) && !ev.actor.is_bot) {
    try {
      const alias = String(ev.attributes.alias ?? "")
        .trim()
        .replace(/^@/, "");
      const fid = ev.factory_id || (alias ? await d.factoryByAlias(alias) : undefined);
      const snap = snaps.find((s) => s.factory.id === fid);
      if (fid && snap && !served.has(fid)) await dispatchMention(d, snap, ev, akey, bundles, user);
    } catch (e) {
      errs.list.push(e);
    }
  }
  if (errs.list.length === 0) return { done: true };
  const err = retryErrorOf(errs.list);
  const { error } = toOfError(err);
  if (permanentCode(error.code)) {
    console.warn("intake: event dropped", ev.id, error.detail);
    return { done: true };
  }
  return { retry: true, error: err instanceof Error ? err.message : String(err) };
}

async function dispatchMention(
  d: IntakeDeps,
  snap: Snapshot,
  ev: CoreEvent,
  akey: string,
  bundles = bundler(d, ev),
  user?: User,
): Promise<void> {
  if (ev.actor.trust !== "member") {
    if (normalizeProvider(ev.provider) === "github") await githubActorEligible(d, ev.team_id, ev, [snap]);
    console.info("intake: mention from a sender without a link ignored", ev.provider, ev.actor.handle);
    return;
  }
  const agent = resolveAgent(snap, "");
  if (!agent) throw new OfError("invalid_request", `factory ${snap.factory.id} has no foreman`);
  const target = `mention:${snap.factory.id}`;
  const key = sourceKey(ev.source as never);
  const r = await d.router.resolveOrReserve({
    factoryId: snap.factory.id,
    sourceKeys: key ? [key] : [],
    actionKey: akey || ev.id,
    target,
  });
  await d.work(r.token).applyAction({
    actionKey: akey,
    target,
    kind: "mention",
    event: ev,
    ...(await bundles(true)),
    dispatchRequest: dispatchPlan(snap, agent, ev, {
      prompt: "",
      triggeredBy: "mention",
      workItemId: r.wiId,
      sourceKey: key,
      principals: dispatchPrincipals({
        factoryId: snap.factory.id,
        triggeredBy: "mention",
        teamId: "",
        ...(memberOf(ev, user) ? { creator: memberOf(ev, user)! } : {}),
      }),
    }),
  });
}

/** The user record of a linked sender of the team (trust member), as a principal source. */
function memberOf(ev: CoreEvent, user: User | undefined): User | undefined {
  if (ev.actor.trust !== "member" || !ev.actor.user_id) return undefined;
  return user && user.id === ev.actor.user_id ? user : { id: ev.actor.user_id };
}

/** The principals of a run of automation a (engine.dispatchPrincipals). Throws when its runAs names nobody. */
async function automationPrincipals(
  d: IntakeDeps,
  factoryId: string,
  a: Automation,
  creator: User | undefined,
  triggeredBy: string,
): Promise<{ creator?: RunPrincipal; runAs?: RunPrincipal }> {
  let runAs: RunPrincipal = factoryPrincipal(factoryId);
  const v = (a.run_as ?? "").trim();
  if (v !== "" && v !== "factory" && d.resolveRunAs) {
    try {
      runAs = await d.resolveRunAs(factoryId, v);
    } catch (e) {
      const { error } = toOfError(e);
      throw new OfError(error.code as never, `automation ${a.name}: ${error.detail}`);
    }
  }
  return dispatchPrincipals({
    factoryId,
    triggeredBy,
    teamId: "",
    automation: true,
    automationRunAs: runAs,
    ...(creator ? { creator } : {}),
  });
}

function dispatchPlan(
  snap: Snapshot,
  agent: AgentConfig,
  ev: CoreEvent,
  o: {
    prompt: string;
    triggeredBy: string;
    workItemId?: string;
    sourceKey?: string;
    automationId?: string;
    principals?: { creator?: RunPrincipal; runAs?: RunPrincipal };
  },
): Record<string, unknown> {
  const prompt = buildDispatchPrompt({ prompt: o.prompt, event: ev });
  return {
    factoryId: snap.factory.id,
    config: agent,
    prompt,
    actor: ev.actor.user_id || ev.actor.handle || "system",
    title: workItemTitle({ prompt: o.prompt, source: ev.source as never, event: ev }),
    source: ev.source,
    ...(o.sourceKey ? { sourceKey: o.sourceKey } : {}),
    ...(o.automationId ? { automationId: o.automationId } : {}),
    ...(ev.actor.user_id ? { creatorId: ev.actor.user_id } : {}),
    ...(o.principals?.creator ? { creator: o.principals.creator } : {}),
    ...(o.principals?.runAs ? { runAs: o.principals.runAs } : {}),
    triggeredBy: o.triggeredBy,
    eventText: ev.text ?? "",
    ...(o.workItemId ? { workItemId: o.workItemId } : {}),
  };
}

/** Steps 4-6 for one factory. Returns whether the action was served there. */
async function handleForFactory(
  d: IntakeDeps,
  snap: Snapshot,
  ev: CoreEvent,
  akey: string,
  user: User | undefined,
  errs: Errs,
  bundles = bundler(d, ev),
): Promise<boolean> {
  const fid = snap.factory.id;
  const rt = snap.runtime;
  if (rt.require_linked_actor && ev.actor.trust === "external" && linkRequiredFor(ev.provider)) {
    console.info("intake: event from a sender without a link dropped (requireLinkedActor)", fid, ev.provider, ev.type);
    return true;
  }
  // 4. Work item.
  const key = sourceKey(ev.source as never);
  let wi: { wiId: string; token: string } | undefined;
  if (key) {
    const r = await d.router.resolve(fid, key);
    if (r && (r.status === "active" || r.status === "reserved")) wi = r;
    else if (r && r.status === "completed" && !ev.actor.is_bot && continuationKind(ev) === CONTINUE_USER) {
      if (await d.router.reopen({ factoryId: fid, sourceKey: key, wiId: r.wiId })) {
        await d.work(r.token).reopen({ actionKey: akey || ev.id, event: ev });
        wi = r;
      }
    }
  }
  if (wi && ev.actor.is_bot && !(await d.work(wi.token).allowBotAction(akey))) {
    console.info("intake: bot action dropped: bot limit of the work item", wi.wiId, ev.actor.handle);
    return true;
  }

  let served = false;
  let rootServed = false;
  let state = wi ? await d.work(wi.token).intakeState() : {};
  // 5. Continuation.
  if (wi) {
    const kind = continuationKind(ev);
    if (kind) {
      const trust = trustOfMessage(ev, state.creatorId ?? "", rt, isApprover(rt.approvers ?? [], user));
      const res = await d.work(wi.token).applyAction({
        actionKey: akey,
        target: `wi:${wi.wiId}`,
        kind: "continue",
        event: ev,
        ...(await bundles(false)),
        continuation: { kind, trust, alias: snap.factory.alias ?? "" },
      });
      served = true;
      rootServed = true;
      if (res.terminal) return served;
      state = await d.work(wi.token).intakeState();
    }
  }

  // 6. Automations, one dispatch for each match, in definition order.
  const matches = matchingAutomations(ev as unknown as Event, snap.automations ?? []);
  const local: unknown[] = [];
  for (const a of matches) {
    const target = `auto:${a.id}`;
    const base = resolveAgent(snap, a.agent);
    if (!base) {
      local.push(
        new OfError("invalid_request", `automation ${a.name}: agent ${JSON.stringify(a.agent)} does not exist`),
      );
      continue;
    }
    let agent: AgentConfig;
    try {
      agent = a.overrides
        ? (applyOverrides(
            base as never,
            a.overrides as never,
            (rt.machines ?? null) as never,
          ) as unknown as AgentConfig)
        : base;
    } catch (e) {
      local.push(e);
      continue;
    }
    const triggeredBy = triggeredByFor(ev.provider, a.id);
    let principals: { creator?: RunPrincipal; runAs?: RunPrincipal };
    try {
      principals = await automationPrincipals(d, fid, a, memberOf(ev, user), triggeredBy);
    } catch (e) {
      local.push(e);
      continue;
    }
    if (!wi || !state.rootRunId) {
      // The first match creates the work item and its root run.
      try {
        const r = wi
          ? { wiId: wi.wiId, token: wi.token }
          : await d.router.resolveOrReserve({
              factoryId: fid,
              sourceKeys: key ? [key] : [],
              actionKey: akey || ev.id,
              target,
            });
        const res = await d.work(r.token).applyAction({
          actionKey: akey,
          target,
          kind: "createRoot",
          event: ev,
          ...(await bundles(true)),
          dispatchRequest: dispatchPlan(snap, agent, ev, {
            prompt: a.prompt,
            triggeredBy,
            workItemId: r.wiId,
            sourceKey: key,
            automationId: a.id,
            principals,
          }),
        });
        served = true;
        wi = { wiId: res.workItemId ?? r.wiId, token: r.token };
        state = {
          ...(res.rootRunId ? { rootRunId: res.rootRunId } : {}),
          ...(res.rootAgent !== undefined ? { rootAgent: res.rootAgent } : {}),
        };
        rootServed = true;
      } catch (e) {
        // Stop here: the root run comes from the first match. A retry does the rest.
        errs.list.push(...local, e);
        return served;
      }
    } else if (agent.name === state.rootAgent) {
      try {
        if (rootServed) {
          // The root run already has the action.
          await d.work(wi.token).applyAction({
            actionKey: akey,
            target,
            kind: "deliverToRoot",
            event: ev,
            dispatchRequest: {
              ...dispatchPlan(snap, agent, ev, { prompt: a.prompt, triggeredBy, principals }),
              alreadyServed: true,
            },
          });
        } else {
          await d.work(wi.token).applyAction({
            actionKey: akey,
            target,
            kind: "deliverToRoot",
            event: ev,
            ...(await bundles(false)),
            dispatchRequest: dispatchPlan(snap, agent, ev, { prompt: a.prompt, triggeredBy, principals }),
          });
        }
        served = true;
        rootServed = true;
      } catch (e) {
        local.push(e);
      }
    } else {
      try {
        await d.work(wi.token).applyAction({
          actionKey: akey,
          target,
          kind: "sideRun",
          event: ev,
          ...(await bundles(true)),
          dispatchRequest: dispatchPlan(snap, agent, ev, { prompt: a.prompt, triggeredBy, principals }),
        });
        served = true;
      } catch (e) {
        local.push(e);
      }
    }
  }
  errs.list.push(...local);
  return served;
}

/** The IngressDO intake handler over the platform bindings (bootstrapControlPlane registers it). */
export const platformIntakeHandler: IntakeHandler = (env, ctx, event) =>
  handleEvent(platformIntakeDeps(env, ctx.teamId), ctx.teamId, structuredClone(event));

/**
 * A manual run of an automation (POST .../schedules/{id}/run): one work item
 * and root run for the event, idempotent on the event ID. Returns the root run ID.
 */
export async function dispatchAutomation(
  d: IntakeDeps,
  factoryId: string,
  a: Automation,
  ev: CoreEvent,
): Promise<{ runId?: string; workItemId?: string }> {
  const snap = await d.snapshot(factoryId);
  const base = resolveAgent(snap, a.agent);
  if (!base)
    throw new OfError("invalid_request", `automation ${a.name}: agent ${JSON.stringify(a.agent)} does not exist`);
  const agent = a.overrides
    ? (applyOverrides(
        base as never,
        a.overrides as never,
        (snap.runtime.machines ?? null) as never,
      ) as unknown as AgentConfig)
    : base;
  const target = `auto:${a.id}`;
  const akey = actionKey(ev) || ev.id;
  const triggeredBy = triggeredByFor(ev.provider, a.id);
  // The person or service account who started the manual run is its creator.
  const starter = ev.actor?.user_id ? ((await d.user(ev.actor.user_id)) ?? { id: ev.actor.user_id }) : undefined;
  const principals = await automationPrincipals(d, factoryId, a, starter, triggeredBy);
  const r = await d.router.resolveOrReserve({ factoryId, sourceKeys: [], actionKey: akey, target });
  const res = await d.work(r.token).applyAction({
    actionKey: akey,
    target,
    kind: "createRoot",
    event: ev,
    ...(await bundler(d, ev)(true)),
    dispatchRequest: dispatchPlan(snap, agent, ev, {
      prompt: a.prompt,
      triggeredBy,
      workItemId: r.wiId,
      automationId: a.id,
      principals,
    }),
  });
  return {
    ...(res.rootRunId ? { runId: res.rootRunId } : {}),
    ...(res.workItemId ? { workItemId: res.workItemId } : {}),
  };
}
