// The GitHub webhook receiver (POST /integrations/github/webhook). Port of
// internal/forge/github/webhook.go. The Worker verifies the delivery with the
// webhook secret of the selected integration record, answers at once, and
// finishes the work (enrichment, routing, definition checks, publish to
// IngressDO) in waitUntil. Dedupe moved to IngressDO (exact and durable).
import type { Event, Integration } from "@openfactory/core";
import type { Forge, ForgeResolver } from "../forge";
import type { IntegrationSource, SecretSource } from "../registry";
import { DeliveryAmbiguousError, intakeVerified, repoOwner, DeliveryUnverifiedError } from "../util/select";
import { hasMention, splitRepo } from "@openfactory/core";
import { errorJSON, jsonResponse, readBody, verifyHmacSha256Hex } from "../util/webhook";
import { utf8 } from "../../lib/bytes";
import { EVENT_ATTR as A } from "@openfactory/core";
import { FORGE_NAME } from "./client";
import {
  ALIAS_LABEL_PREFIX,
  PROVIDER,
  Normalizer,
  checkName,
  isDefinitionPath,
  normalizeRepo,
  type GhPayload,
} from "./events";

export const WEBHOOK_PATH = "/integrations/github/webhook";

/** A factory as the receiver needs it (core.Factory JSON fields). */
export interface FactoryInfo {
  id: string;
  alias?: string;
  source?: string;
  git_repo?: string;
  git_branch?: string;
  git_path?: string;
  active_version_id?: string;
}

/** A pull request that touches a git-backed factory definition. */
export interface DefinitionPR {
  teamId: string;
  factoryId: string;
  repo: string;
  number: number;
  headSha: string;
  headRef: string;
  baseRef: string;
  checkName: string;
  changedPaths: string[];
}

export interface GitHubWebhookDeps {
  /** Teams whose records can serve a delivery (the core has one: "default"). */
  teams: () => Promise<string[]>;
  integrations: IntegrationSource;
  secrets: SecretSource;
  resolver?: ForgeResolver & { invalidate?(teamId: string): void };
  factories?: {
    byAlias(teamId: string, alias: string): Promise<FactoryInfo | null>;
    list(teamId: string): Promise<FactoryInfo[]>;
    /** github.factoryConfigChecks of the active definition ("main" by default). */
    configChecks?(teamId: string, f: FactoryInfo): Promise<string>;
  };
  /** Fills actor.user_id from a linked external identity. */
  linkActor?(teamId: string, actor: Event["actor"]): Promise<void>;
  /** Hands the events to IngressDO. */
  publish(teamId: string, events: Event[]): Promise<void>;
  onDefinitionPush?(teamId: string, repo: string, branch: string, sha: string): Promise<void>;
  onDefinitionPR?(pr: DefinitionPR): Promise<void>;
  now?: () => number;
  /** ExecutionContext.waitUntil; without it the work is awaited. */
  waitUntil?(p: Promise<unknown>): void;
}

interface Envelope {
  installation?: { id?: number } | null;
  repository?: { full_name?: string } | null;
  hook?: { app_id?: number } | null;
}

async function allRecords(deps: GitHubWebhookDeps): Promise<Integration[]> {
  const out: Integration[] = [];
  for (const t of await deps.teams()) out.push(...(await deps.integrations.list(t, PROVIDER)));
  return out.filter((x) => x.provider.toLowerCase() === PROVIDER);
}

async function verifyRecord(
  deps: GitHubWebhookDeps,
  in_: Integration,
  body: Uint8Array,
  sig: string,
): Promise<boolean> {
  const ref = in_.config?.["webhook_secret_ref"] ?? "";
  if (ref === "") return false;
  let sec: string | undefined;
  try {
    sec = await deps.secrets.get(in_.team_id, "", ref);
  } catch (e) {
    console.warn("github webhook: load webhook secret", in_.id, e);
    return false;
  }
  if (!sec) return false;
  return verifyHmacSha256Hex(utf8(sec), body, sig);
}

/**
 * Selects the record of a delivery and verifies its signature. A delivery
 * without a repository uses the orgs of the records of its installation. The
 * ping of an App (no installation, no repository) is checked with the records
 * of that app and gives no record (null).
 */
export async function intake(
  deps: GitHubWebhookDeps,
  env: Envelope,
  body: Uint8Array,
  sig: string,
): Promise<Integration | null> {
  if (sig === "") throw new DeliveryUnverifiedError("no signature");
  const all = await allRecords(deps);
  const verify = (in_: Integration) => verifyRecord(deps, in_, body, sig);
  const owners: string[] = [];
  const owner = repoOwner(PROVIDER, env.repository?.full_name ?? "");
  if (owner !== "") owners.push(owner);
  else if (env.installation?.id) {
    const want = String(env.installation.id);
    for (const in_ of all) {
      if (in_.config?.["installation_id"] !== want) continue;
      const org = (in_.config?.["org"] ?? "").trim();
      if (org !== "" && !owners.some((o) => o.toLowerCase() === org.toLowerCase())) owners.push(org);
    }
  }
  if (owners.length > 0) return intakeVerified(all, PROVIDER, owners, verify);
  if (env.hook?.app_id) {
    const app = String(env.hook.app_id);
    for (const in_ of all) if (in_.config?.["app_id"] === app && (await verify(in_))) return null;
  }
  throw new DeliveryUnverifiedError(
    `no github integration for installation/repository "${env.repository?.full_name ?? ""}"`,
  );
}

/** Events whose filters accept `paths`. */
const PATH_EVENTS = new Set([
  "pull_request_opened",
  "pull_request_ready",
  "pull_request_reopened",
  "pull_request_closed",
  "pull_request_dequeued",
  "pull_request_enqueued",
  "pull_request_labeled",
  "pull_request_merged",
  "pull_request_synchronized",
  "pull_request_assigned",
]);

/** Handles one delivery. */
export async function handleGitHubWebhook(req: Request, deps: GitHubWebhookDeps): Promise<Response> {
  let body: Uint8Array<ArrayBuffer>;
  try {
    body = await readBody(req);
  } catch (e) {
    return errorJSON(413, "payload_too_large", (e as Error).message);
  }
  const eventName = req.headers.get("x-github-event") ?? "";
  const delivery = req.headers.get("x-github-delivery") ?? "";
  const sig = req.headers.get("x-hub-signature-256") ?? "";
  if (eventName === "") return errorJSON(400, "missing_event", "X-GitHub-Event header is required");
  const text = new TextDecoder().decode(body);
  let env: Envelope;
  try {
    env = JSON.parse(text) as Envelope;
    if (typeof env !== "object" || env === null) throw new Error("not an object");
  } catch {
    return errorJSON(400, "invalid_payload", "payload is not valid JSON");
  }
  let integ: Integration | null;
  try {
    integ = await intake(deps, env, body, sig);
  } catch (e) {
    if (e instanceof DeliveryAmbiguousError) {
      console.warn("github webhook: delivery rejected", eventName, env.repository?.full_name, e.message);
      return errorJSON(409, "ambiguous_integration", e.message);
    }
    console.info("github webhook: delivery not verified", eventName, env.repository?.full_name, (e as Error).message);
    return errorJSON(401, "invalid_signature", "X-Hub-Signature-256 does not match");
  }
  if (eventName === "ping") return jsonResponse(200, { status: "pong" });
  if (eventName === "installation") {
    const n = await installationLifecycle(deps, integ, env, text, body, sig);
    return jsonResponse(200, { status: "accepted", integrations: n });
  }
  if (!integ) return jsonResponse(202, { status: "ignored", reason: "no matching integration" });
  const norm = new Normalizer({ appSlug: integ.config?.["app_slug"] ?? "", ...(deps.now ? { now: deps.now } : {}) });
  let evs: Event[];
  try {
    evs = norm.normalize(eventName, delivery, text);
  } catch (e) {
    return errorJSON(400, "invalid_payload", (e as Error).message);
  }
  const work = process(deps, integ, env.repository?.full_name ?? "", eventName, text, evs).catch((e) =>
    console.error("github webhook: process delivery", delivery, e),
  );
  if (deps.waitUntil) deps.waitUntil(work);
  else await work;
  return jsonResponse(202, { status: "accepted", events: evs.length });
}

async function process(
  deps: GitHubWebhookDeps,
  integ: Integration,
  repo: string,
  eventName: string,
  text: string,
  evs: Event[],
): Promise<void> {
  const teamId = integ.team_id;
  let gf: Forge | null | undefined;
  const forgeOnce = async (): Promise<Forge | null> => {
    if (gf !== undefined) return gf;
    gf = null;
    if (deps.resolver && repo !== "") {
      try {
        gf = await deps.resolver.forge({
          teamId,
          ...(integ.factory_id ? { factoryId: integ.factory_id } : {}),
          codeForge: FORGE_NAME,
          repo,
        });
      } catch (e) {
        console.warn("github webhook: resolve forge", teamId, integ.id, repo, e);
      }
    }
    return gf;
  };
  let prPaths: string[] | null = null;
  let prPathsLoaded = false;
  for (const ev of evs) {
    ev.team_id = teamId;
    if (deps.linkActor) await deps.linkActor(teamId, ev.actor);
    const attrs = ev.attributes!;
    const [owner, name] = splitRepo(ev.source.repo ?? "");
    if (PATH_EVENTS.has(ev.type) && ev.source.number) {
      if (!prPathsLoaded) {
        prPathsLoaded = true;
        const f = await forgeOnce();
        if (f?.listPullRequestFiles) {
          try {
            prPaths = await f.listPullRequestFiles(owner, name, ev.source.number);
          } catch (e) {
            console.warn("github webhook: list pull request files", ev.source.repo, ev.source.number, e);
          }
        }
      }
      if (prPaths !== null) attrs[A.Paths] = prPaths;
    }
    if ((ev.type === "check_suite_completed" || ev.type === "workflow_run_completed") && attrs[A.LinkedPRCount] === 1) {
      const num = attrs[A.PRNumber];
      const f = typeof num === "number" ? await forgeOnce() : null;
      const details = f as
        | (Forge & {
            getPullRequestDetails?(
              o: string,
              r: string,
              n: number,
            ): Promise<{ labels?: { name: string }[]; user?: { login?: string } }>;
          })
        | null;
      if (details?.getPullRequestDetails && typeof num === "number") {
        try {
          const pr = await details.getPullRequestDetails(owner, name, num);
          attrs[A.Labels] = (pr.labels ?? []).map((l) => l.name);
          attrs[A.Author] = pr.user?.login ?? "";
        } catch (e) {
          console.warn("github webhook: load linked pull request", ev.source.repo, num, e);
        }
      }
    }
    await routeFactory(deps, ev, integ.config?.["app_slug"] ?? "", forgeOnce);
  }
  if (eventName === "push") await definitionPush(deps, teamId, text);
  else if (eventName === "pull_request") await definitionPR(deps, teamId, text, forgeOnce);
  if (evs.length > 0) await deps.publish(teamId, evs);
}

/** Sets factory_id from a factory:<alias> label or an @alias mention. */
async function routeFactory(
  deps: GitHubWebhookDeps,
  ev: Event,
  slug: string,
  forgeOnce: () => Promise<Forge | null>,
): Promise<void> {
  const fs = deps.factories;
  if (!fs) return;
  const attrs = ev.attributes!;
  const alias = typeof attrs[A.Alias] === "string" ? (attrs[A.Alias] as string) : "";
  if (alias !== "") {
    const f = await fs.byAlias(ev.team_id, alias);
    if (f) ev.factory_id = f.id;
    return;
  }
  const mentioned = Array.isArray(attrs[A.Mentioned]) ? (attrs[A.Mentioned] as string[]) : [];
  if (!ev.type.endsWith("_mentioned") || mentioned.length === 0) return;
  for (const m of mentioned) {
    const f = await fs.byAlias(ev.team_id, m);
    if (f) {
      ev.factory_id = f.id;
      attrs[A.Alias] = f.alias ?? "";
      return;
    }
  }
  if (slug === "" || !hasMention(ev.text ?? "", slug) || ev.actor.is_bot) return;
  const [owner, name] = splitRepo(ev.source.repo ?? "");
  const gf = await forgeOnce();
  const notice = async (msg: string) => {
    if (!gf || !ev.source.number) return;
    try {
      await gf.comment(owner, name, ev.source.number, msg);
    } catch (e) {
      console.warn("github webhook: post routing notice", ev.source.repo, e);
    }
  };
  if (!ev.actor.user_id) return; // the engine posts the onboarding notice
  let list: FactoryInfo[];
  try {
    list = await fs.list(ev.team_id);
  } catch (e) {
    console.warn("github webhook: list factories", ev.team_id, e);
    return;
  }
  if (list.length === 0) {
    await notice(
      `No factory is connected to this repository. Connect the repository to a factory or request access, then mention @${slug} again.`,
    );
  } else if (list.length === 1) {
    const f = list[0]!;
    ev.factory_id = f.id;
    attrs[A.Alias] = f.alias ?? "";
    if (gf?.addLabels && f.alias) {
      try {
        await gf.addLabels(owner, name, ev.source.number ?? 0, ALIAS_LABEL_PREFIX + f.alias);
      } catch (e) {
        console.warn("github webhook: add factory label", ev.source.repo, e);
      }
    }
  } else {
    const names = list.map((f) => "`" + ALIAS_LABEL_PREFIX + (f.alias ?? "") + "`").join(", ");
    await notice(
      `Several factories can handle this repository. Add one of these labels and mention @${slug} again: ${names}`,
    );
  }
}

function productionBranch(f: FactoryInfo): string {
  return f.git_branch || "main";
}

async function gitFactories(deps: GitHubWebhookDeps, teamId: string, repo: string): Promise<FactoryInfo[]> {
  if (!deps.factories) return [];
  let fs: FactoryInfo[];
  try {
    fs = await deps.factories.list(teamId);
  } catch (e) {
    console.warn("github webhook: list factories", teamId, e);
    return [];
  }
  const want = normalizeRepo(repo);
  return fs.filter((f) => f.source === "git" && f.git_repo && normalizeRepo(f.git_repo) === want);
}

async function definitionPush(deps: GitHubWebhookDeps, teamId: string, text: string): Promise<void> {
  if (!deps.onDefinitionPush) return;
  let p: GhPayload;
  try {
    p = JSON.parse(text) as GhPayload;
  } catch {
    return;
  }
  if (p.deleted || !(p.ref ?? "").startsWith("refs/heads/")) return;
  const branch = p.ref!.slice("refs/heads/".length);
  const repo = p.repository?.full_name ?? "";
  for (const f of await gitFactories(deps, teamId, repo)) {
    if (productionBranch(f) === branch) {
      await deps.onDefinitionPush(teamId, repo, branch, p.after ?? "");
      return;
    }
  }
}

async function definitionPR(
  deps: GitHubWebhookDeps,
  teamId: string,
  text: string,
  forgeOnce: () => Promise<Forge | null>,
): Promise<void> {
  let p: GhPayload;
  try {
    p = JSON.parse(text) as GhPayload;
  } catch {
    return;
  }
  const pr = p.pull_request;
  if (!pr) return;
  switch (p.action) {
    case "opened":
    case "reopened":
    case "synchronize":
    case "ready_for_review":
      break;
    case "edited":
      if (!p.changes || !("base" in p.changes)) return;
      break;
    default:
      return;
  }
  const repo = p.repository?.full_name ?? "";
  const factories = await gitFactories(deps, teamId, repo);
  if (factories.length === 0) return;
  const [owner, name] = splitRepo(repo);
  let files: string[] = [];
  let filesKnown = false;
  const f0 = await forgeOnce();
  if (f0?.listPullRequestFiles) {
    try {
      files = await f0.listPullRequestFiles(owner, name, pr.number);
      filesKnown = true;
    } catch (e) {
      console.warn("github webhook: list pull request files for definition check", repo, e);
    }
  }
  for (const f of factories) {
    if ((pr.base?.ref ?? "") !== productionBranch(f)) {
      const mode = deps.factories?.configChecks ? await deps.factories.configChecks(teamId, f) : "main";
      if (mode !== "all") continue;
    }
    const check = checkName(f.git_path ?? "");
    const changed = files.filter((fp) => isDefinitionPath(f.git_path ?? "", fp));
    if (filesKnown && changed.length === 0) {
      const gf = await forgeOnce();
      if (gf) {
        try {
          await gf.setCommitStatus(owner, name, pr.head?.sha ?? "", {
            context: check,
            state: "success",
            description: "No factory definition changes",
          });
        } catch (e) {
          console.warn("github webhook: publish definition check", repo, e);
        }
      }
      continue;
    }
    if (deps.onDefinitionPR) {
      await deps.onDefinitionPR({
        teamId,
        factoryId: f.id,
        repo,
        number: pr.number,
        headSha: pr.head?.sha ?? "",
        headRef: pr.head?.ref ?? "",
        baseRef: pr.base?.ref ?? "",
        checkName: check,
        changedPaths: changed,
      });
    }
  }
}

/**
 * Sets the status of the records of an App installation from an installation
 * event (deleted and suspend disable, unsuspend, created and
 * new_permissions_accepted activate). A team never changes the records of
 * another team. Returns the number of changed records.
 */
async function installationLifecycle(
  deps: GitHubWebhookDeps,
  in_: Integration | null,
  env: Envelope,
  text: string,
  body: Uint8Array,
  sig: string,
): Promise<number> {
  if (!in_ || !env.installation?.id || !deps.integrations.setStatus) return 0;
  if (in_.config?.["installation_id"] !== String(env.installation.id)) {
    console.info("github webhook: installation event for another installation ignored", in_.id, env.installation.id);
    return 0;
  }
  let action = "";
  try {
    action = (JSON.parse(text) as { action?: string }).action ?? "";
  } catch {
    /* no action */
  }
  let status: string;
  let reason = "";
  switch (action) {
    case "deleted":
      status = "disabled";
      reason = "the app was removed";
      break;
    case "suspend":
      status = "disabled";
      reason = "the app was suspended";
      break;
    case "unsuspend":
    case "created":
    case "new_permissions_accepted":
      status = "active";
      break;
    default:
      return 0;
  }
  let n = 0;
  for (const r of await deps.integrations.list(in_.team_id, PROVIDER)) {
    if (r.provider.toLowerCase() !== PROVIDER || r.config?.["installation_id"] !== in_.config?.["installation_id"])
      continue;
    if (r.id !== in_.id && (r.config?.["webhook_secret_ref"] ?? "") !== "" && !(await verifyRecord(deps, r, body, sig)))
      continue;
    try {
      if (await deps.integrations.setStatus(r.id, status, reason)) n++;
    } catch (e) {
      console.warn("github webhook: installation event: set status", r.id, e);
    }
  }
  if (n > 0) deps.resolver?.invalidate?.(in_.team_id);
  return n;
}
