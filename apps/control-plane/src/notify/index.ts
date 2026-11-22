// Routes outbound notices back to their work source: chat threads, tracker
// issues, forge issues and pull requests. Port of internal/notify.
//
// Notices leave the WorkDO outbox in order (lane "notice:<wiId>", plan rule
// 11) and arrive here one at a time: deliverNotice makes one attempt. A
// transient error throws, so the outbox retries with backoff; a permanent
// error is logged, reported through the failure hook, and not retried.
import {
  ApiError,
  OfError,
  isTrackerOperation,
  toOfError,
  type OutboundMessage,
  type Responder,
} from "@openfactory/core";
import type { OutboxEnvelope } from "../contracts/do-messages";
import type { Env } from "../contracts/env";
import { resolverFor } from "../forges/env";
import { forgeResponders } from "../forges/registry";
import { teamOfDoName } from "../lib/outbox/dest";

export type { Responder };

/** Providers whose work items are reported only on the work item itself. */
export const SILENT_PROVIDERS = new Set(["", "api", "mcp", "webhook", "schedule", "factory", "dashboard", "benchmark"]);

/** Maps provider spellings to responder keys. */
export function normalizeProvider(p: string): string {
  const s = p.trim().toLowerCase();
  switch (s) {
    case "microsoft_teams":
    case "microsoft-teams":
    case "msteams":
      return "teams";
    case "azure-devops":
    case "azuredevops":
      return "azure_devops";
  }
  return s;
}

/** Whether retrying cannot help. */
export function permanent(err: unknown): boolean {
  // toOfError also reads the code of an OfError that crossed RPC as a plain Error.
  const { error, known } = toOfError(err);
  if (known) {
    return [
      "resource_not_found",
      "invalid_request",
      "not_authorized",
      "operation_not_supported",
      "authentication_required",
    ].includes(error.code);
  }
  if (err instanceof ApiError) return !err.temporary;
  const cause = (err as { cause?: unknown } | null)?.cause;
  if (cause) return permanent(cause);
  return false;
}

export class Router {
  private readonly responders = new Map<string, Responder>();
  constructor(responders: Responder[] = []) {
    for (const r of responders) this.add(r);
  }
  add(r: Responder): void {
    this.responders.set(normalizeProvider(r.provider()), r);
  }
  providers(): string[] {
    return [...this.responders.keys()].sort();
  }
  get(provider: string): Responder | undefined {
    return this.responders.get(normalizeProvider(provider));
  }

  /**
   * One delivery attempt. Returns "delivered", "skipped" (no responder or no
   * write-back) or throws the responder error.
   */
  async deliverOnce(teamId: string, msg: OutboundMessage): Promise<"delivered" | "skipped"> {
    const provider = normalizeProvider(msg.source.provider);
    const rs = this.responders.get(provider);
    if (!rs) {
      if (!SILENT_PROVIDERS.has(provider))
        console.warn("notify: no responder for provider", provider, msg.work_item_id);
      return "skipped";
    }
    if (!teamId) throw new OfError("invalid_request", "notify: cannot resolve team for work item");
    if (!isTrackerOperation(msg.kind)) {
      await rs.reply(teamId, msg);
      return "delivered";
    }
    try {
      if (msg.kind === "issue_state") {
        if (!rs.setIssueState) return "skipped";
        await rs.setIssueState(teamId, msg.source, msg.issue_state ?? "", msg.factory_id);
      } else if (msg.kind === "attach_pull_request") {
        if (!rs.attachPullRequest) return "skipped";
        if (!msg.pull_request) throw new OfError("invalid_request", "attach_pull_request without a pull request");
        await rs.attachPullRequest(teamId, msg.source, msg.pull_request, msg.factory_id);
      }
    } catch (e) {
      // A plugin tracker's OfError crosses RPC as a plain Error: match on the code.
      if (toOfError(e).error.code === "operation_not_supported") return "skipped";
      throw e;
    }
    return "delivered";
  }
}

// ---------- registration ----------

/** Builds the responders of one provider family from the environment. */
export type ResponderFactory = (env: Env) => Responder[];

const factories: ResponderFactory[] = [(env) => forgeResponders(resolverFor(env))];
let failureHook: ((env: Env, teamId: string, msg: OutboundMessage, err: unknown) => Promise<void>) | undefined;

/** Adds responders (Slack, Teams, Linear, Jira from src/integrations). Later ones win. */
export function registerResponders(f: ResponderFactory): void {
  factories.push(f);
}

/** Called after a permanent failure (for example to write an inbox failure). */
export function setNoticeFailureHook(fn: typeof failureHook): void {
  failureHook = fn;
}

let routerCache: { env: Env; router: Router } | undefined;

export function routerFor(env: Env): Router {
  if (routerCache && routerCache.env === env) return routerCache.router;
  const r = new Router();
  for (const f of factories) for (const rs of f(env)) r.add(rs);
  routerCache = { env, router: r };
  return r;
}

/** The team of an outbox origin ("team:<id>|..."): the DO name of the sender. */

type NoticeKind = "notice.forge" | "notice.responder" | "notice.tracker";

/**
 * Delivers one notice of the WorkDO outbox. Throws for a transient error (the
 * outbox retries); returns for success, skip, and permanent failure.
 */
export async function deliverNotice(env: Env, envelope: OutboxEnvelope<NoticeKind> | OutboxEnvelope): Promise<void> {
  const payload = envelope.payload as { message?: OutboundMessage };
  const msg = payload.message;
  if (!msg) {
    console.warn("notify: notice without a message", envelope.msgId);
    return;
  }
  const teamId = teamOfDoName(envelope.origin);
  try {
    await routerFor(env).deliverOnce(teamId, msg);
  } catch (e) {
    if (!permanent(e)) throw e;
    console.error(
      "notify: outbound delivery failed",
      msg.source.provider,
      msg.work_item_id,
      msg.kind,
      (e as Error).message,
    );
    if (failureHook) await failureHook(env, teamId, msg, e).catch((x) => console.warn("notify: failure hook", x));
  }
}

// ---------- notification routes ----------

export interface Route {
  provider: string;
  /** inbox, chat, tracker, forge */
  kind: string;
  destination: string;
  deliverable: boolean;
  settings?: Record<string, string>;
}

const ROUTE_INFO: Record<string, { kind: string; dest: string }> = {
  slack: {
    kind: "chat",
    dest: "Replies in the Slack thread or DM where the work started (approval options as buttons)",
  },
  teams: { kind: "chat", dest: "Replies in the Microsoft Teams channel thread where the work started" },
  linear: { kind: "tracker", dest: "Comments on the Linear issue, or activities in its agent session" },
  jira: { kind: "tracker", dest: "Comments on the Jira work item" },
  github: { kind: "forge", dest: "Comments on the originating GitHub issue or pull request" },
  gitlab: { kind: "forge", dest: "Notes on the originating GitLab merge request or issue" },
  azure_devops: { kind: "forge", dest: "Comment threads on the originating Azure DevOps pull request or work item" },
};

/**
 * The notification destinations of a factory: the dashboard inbox, the chat
 * and tracker integrations attached in its definition, and the team's forge
 * connections. available lists providers with a responder (null = all).
 */
export function routes(
  attached: Record<string, Record<string, string> | null> | null,
  teamIntegrationProviders: string[],
  available: string[] | null,
): Route[] {
  const avail = new Set((available ?? []).map(normalizeProvider));
  const deliverable = (p: string) => available === null || avail.has(p);
  const out: Route[] = [
    {
      provider: "dashboard",
      kind: "inbox",
      deliverable: true,
      destination: "Factory inbox (approvals, questions, handoffs, failures) and the work item timeline",
    },
  ];
  const seen = new Set<string>();
  const add = (p0: string, settings?: Record<string, string> | null) => {
    const p = normalizeProvider(p0);
    const info = ROUTE_INFO[p];
    if (!info || seen.has(p)) return;
    seen.add(p);
    const r: Route = { provider: p, kind: info.kind, destination: info.dest, deliverable: deliverable(p) };
    if (settings) r.settings = settings;
    out.push(r);
  };
  for (const p of Object.keys(attached ?? {}).sort()) add(p, attached![p]);
  for (const p of [...teamIntegrationProviders].sort()) {
    const n = normalizeProvider(p);
    if (n === "github" || n === "gitlab" || n === "azure_devops") add(p);
  }
  return out;
}
