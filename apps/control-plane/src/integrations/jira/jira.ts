// Jira Cloud: webhook receiver (shared-secret HMAC or Connect JWT with a
// query-string hash), issue and comment events, agent sessions, account
// linking, a Responder with REST v3 comments (ADF) and the tracker write-back.
// Port of internal/integrations/jira/{jira,tracker}.go.
//
// Integration records (provider "jira"), one per Jira Cloud site and factory:
//   config site, base_url, auth ("basic" default or "oauth"), email,
//   bot_account_id, client_key, webhook_secret_ref, shared_secret_ref;
//   secret_ref: API token or OAuth access token.
import {
  ISSUE_STATE_DONE,
  ISSUE_STATE_IN_PROGRESS,
  ISSUE_STATE_IN_REVIEW,
  OfError,
  compareBytes,
  integrationWorkspace,
  newId,
  queryEscape,
} from "@openfactory/core";
import type { Actor, Event, Integration, OutboundMessage, PullRequestRef, SourceRef } from "@openfactory/core";
import { A, PROVIDER_JIRA } from "../eventattr";
import { ApiClient } from "../http";
import { parseJwt } from "../jwt";
import { intake, selectTeam } from "../select";
import { dashboardHint, member } from "../shared";
import type { IntegrationDeps, IssueTracker, Logger, Responder, WebhookOutcome } from "../types";
import { consoleLogger } from "../types";
import {
  Dedupe,
  cfg,
  diff,
  errMessage,
  errorResponse,
  formatOutbound,
  integrationSecret,
  jsonResponse,
  readBody,
  toHex,
  verifyHmacSha256Hex,
} from "../util";
import { adfMentions, adfText, markdownToAdf } from "./adf";

export const PROVIDER = PROVIDER_JIRA;
export const WEBHOOK_PATH = "/integrations/jira/webhook";

interface JiraUser {
  accountId?: string;
  displayName?: string;
  accountType?: string;
}

interface JiraIssue {
  id?: string;
  key: string;
  self?: string;
  fields: {
    summary?: string;
    description?: unknown;
    labels?: string[];
    status?: { id: string; name: string } | null;
    project?: { id: string; key: string; name: string } | null;
    assignee?: JiraUser | null;
  };
}

interface WebhookPayload {
  webhookEvent?: string;
  user?: JiraUser | null;
  issue?: JiraIssue | null;
  changelog?: { items?: { field: string; from: string; fromString: string; to: string; toString: string }[] } | null;
  comment?: { id: string; body: unknown; author?: JiraUser | null } | null;
}

/** The Connect query-string hash of a request. */
export async function qsh(method: string, path: string, query: URLSearchParams): Promise<string> {
  const enc = (s: string): string => queryEscape(s).replaceAll("+", "%20");
  const keys = [...new Set([...query.keys()].filter((k) => k !== "jwt"))].sort(compareBytes);
  const parts = keys.map((k) => {
    const vals = query.getAll(k).sort(compareBytes).map(enc);
    return enc(k) + "=" + vals.join(",");
  });
  const canonical = method.toUpperCase() + "&" + (path === "" ? "/" : path) + "&" + parts.join("&");
  return toHex(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(canonical)));
}

/** The web URL of an issue. */
export function browseUrl(self: string, key: string): string {
  try {
    const p = new URL(self);
    if (p.host === "" || key === "") return "";
    return p.protocol + "//" + p.host + "/browse/" + key;
  } catch {
    return "";
  }
}

/** A Jira REST client for an integration record and its token. */
export function newApiClient(in_: Integration, tok: string, fetchFn?: DepsFetch): ApiClient {
  const base = cfg(in_, "base_url").replace(/\/+$/, "");
  if (base === "") throw new Error(`jira: integration ${in_.id} has no base_url`);
  const auth =
    cfg(in_, "auth").toLowerCase() === "oauth"
      ? "Bearer " + tok
      : "Basic " + btoa(String.fromCharCode(...new TextEncoder().encode(cfg(in_, "email") + ":" + tok)));
  return new ApiClient({
    baseUrl: base,
    fetch: fetchFn,
    userAgent: "openfactory-jira",
    authorize: (h) => h.set("Authorization", auth),
  });
}
type DepsFetch = NonNullable<IntegrationDeps["fetch"]>;

interface JiraTransition {
  id: string;
  name: string;
  to: { name: string; statusCategory: { key: string } };
}

/** The transition for a generic issue state, or "". */
export function pickTransition(ts: JiraTransition[], state: string): string {
  const byCategory = (cat: string, ...prefer: string[]): string => {
    let first = "";
    for (const t of ts) {
      if (t.to.statusCategory.key !== cat) continue;
      for (const p of prefer) {
        if (t.to.name.toLowerCase().includes(p) || t.name.toLowerCase().includes(p)) return t.id;
      }
      if (first === "") first = t.id;
    }
    return first;
  };
  switch (state) {
    case ISSUE_STATE_DONE:
      return byCategory("done", "done", "closed", "resolved");
    case ISSUE_STATE_IN_PROGRESS:
      return byCategory("indeterminate", "progress");
    case ISSUE_STATE_IN_REVIEW:
      for (const t of ts) {
        if (t.to.statusCategory.key === "indeterminate" && t.to.name.toLowerCase().includes("review")) return t.id;
      }
  }
  return "";
}

const splitLabels = (s: string): string[] => s.split(/\s+/).filter((x) => x !== "");

export class JiraHandler implements Responder, IssueTracker {
  private readonly log: Logger;
  private readonly dedupe = new Dedupe(3600_000);
  constructor(readonly deps: IntegrationDeps) {
    this.log = deps.log ?? consoleLogger;
  }

  private now(): Date {
    return this.deps.now ? this.deps.now() : new Date();
  }

  provider(): string {
    return PROVIDER;
  }

  async handleWebhook(req: Request): Promise<WebhookOutcome> {
    let body: string;
    try {
      body = await readBody(req);
    } catch (e) {
      return { response: errorResponse(413, "payload_too_large", errMessage(e)) };
    }
    let p: WebhookPayload;
    try {
      p = JSON.parse(body) as WebhookPayload;
    } catch {
      return { response: errorResponse(400, "invalid_payload", "payload is not valid JSON") };
    }
    let jwtIss = "";
    const auth = req.headers.get("Authorization") ?? "";
    if (auth.startsWith("JWT ")) {
      try {
        jwtIss = parseJwt(auth.slice(4)).claimString("iss");
      } catch {
        /* a bad token fails verification below */
      }
    }
    const site = p.issue?.self ?? "";
    const found = await this.findIntegration(site, jwtIss);
    if (!(await this.verify(found.integration, req, body))) {
      return { response: errorResponse(401, "invalid_signature", "webhook authentication failed") };
    }
    if (found.error || !found.integration) {
      this.log.info("delivery ignored", { site, err: found.error?.message });
      return { response: jsonResponse(202, { status: "ignored", reason: "no matching integration" }) };
    }
    const delivery = req.headers.get("X-Atlassian-Webhook-Identifier") ?? "";
    if (this.dedupe.isSeen(delivery)) return { response: jsonResponse(200, { status: "duplicate" }) };
    const in_ = found.integration;
    return {
      response: jsonResponse(202, { status: "accepted" }),
      background: async () => {
        const evs = await this.normalize(in_, delivery, body);
        if (evs.length > 0) await this.deps.publish(evs);
      },
    };
  }

  private async verify(in_: Integration | null, req: Request, body: string): Promise<boolean> {
    const sig = req.headers.get("X-Hub-Signature") ?? "";
    if (sig !== "") {
      if (in_ && cfg(in_, "webhook_secret_ref") !== "") {
        try {
          const sec = await integrationSecret(this.deps.secrets, in_, "webhook_secret_ref");
          return await verifyHmacSha256Hex(sec, body, sig);
        } catch (e) {
          this.log.warn("load webhook secret", { integration: in_.id, err: errMessage(e) });
        }
      }
      return false;
    }
    const auth = req.headers.get("Authorization") ?? "";
    if (!auth.startsWith("JWT ") || !in_ || cfg(in_, "shared_secret_ref") === "") return false;
    let sec: string;
    try {
      sec = await integrationSecret(this.deps.secrets, in_, "shared_secret_ref");
    } catch (e) {
      this.log.warn("load connect shared secret", { integration: in_.id, err: errMessage(e) });
      return false;
    }
    try {
      const j = parseJwt(auth.slice(4));
      await j.verifyHS256(sec);
      j.verifyTime(this.now().getTime(), 60_000);
      const ck = cfg(in_, "client_key");
      if (ck !== "" && j.claimString("iss") !== ck) return false;
      const u = new URL(req.url);
      let path = u.pathname;
      try {
        path = decodeURIComponent(path);
      } catch {
        /* keep the raw path */
      }
      return j.claimString("qsh") === (await qsh(req.method, path, u.searchParams));
    } catch {
      return false;
    }
  }

  /** The record of a delivery: records whose site is the site of the issue; a Connect app only reaches its own records. */
  private findIntegration(issueSelf: string, clientKey: string) {
    return intake(this.deps.store, PROVIDER, issueSelf, (i) => {
      const ck = cfg(i, "client_key");
      return clientKey === "" || ck === "" || ck === clientKey;
    });
  }

  /** Converts a verified webhook payload into events. */
  async normalize(in_: Integration, deliveryId: string, body: string): Promise<Event[]> {
    let p: WebhookPayload;
    try {
      p = JSON.parse(body) as WebhookPayload;
    } catch {
      return [];
    }
    if (!p.issue) return [];
    const is = p.issue;
    const bot = cfg(in_, "bot_account_id");
    const actorUser = p.comment?.author ?? p.user ?? null;
    const actor: Actor = { provider: PROVIDER, external_id: "", handle: "", is_bot: false };
    if (actorUser) {
      actor.external_id = actorUser.accountId ?? "";
      actor.handle = actorUser.displayName ?? "";
      actor.is_bot = actorUser.accountType === "app";
    }
    if (bot !== "" && actor.external_id === bot) return []; // the factory's own activity never starts work
    // Activity by another factory's Jira account on this server is marked actor_is_self.
    let ownAccount = false;
    if (actor.external_id !== "") {
      try {
        for (const o of await this.deps.store.listIntegrations("")) {
          if (o.provider.toLowerCase() === PROVIDER && cfg(o, "bot_account_id") === actor.external_id) {
            ownAccount = true;
            actor.is_bot = true;
          }
        }
      } catch {
        /* the marker stays false */
      }
    }
    await this.linkActor(actor);
    const projectKey = is.fields.project?.key ?? "";
    const projectName = is.fields.project?.name ?? "";
    const labels = is.fields.labels ?? [];
    const src: SourceRef = {
      provider: PROVIDER,
      kind: "issue",
      url: browseUrl(is.self ?? "", is.key),
      external_id: is.key,
      title: is.fields.summary ?? "",
      workspace: integrationWorkspace(in_),
    };
    const desc = adfText(is.fields.description);
    const issueText = ((is.fields.summary ?? "") + "\n\n" + desc).trim();
    const base = (): Record<string, unknown> => {
      const a: Record<string, unknown> = {
        [A.ProjectKey]: projectKey,
        [A.ProjectName]: projectName,
        [A.Labels]: [...labels],
        [A.ActorIsBot]: actor.is_bot,
        [A.ActorIsSelf]: ownAccount,
        issue_key: is.key,
      };
      if (is.fields.status) {
        a[A.StatusID] = is.fields.status.id;
        a[A.StatusName] = is.fields.status.name;
      }
      return a;
    };
    const out: Event[] = [];
    const emit = (typ: string, text: string, a: Record<string, unknown>, a2: Actor = actor): void => {
      if (text !== "") a[A.Text] = text;
      out.push({
        id: newId("evt"),
        team_id: in_.team_id,
        factory_id: in_.factory_id ?? "",
        provider: PROVIDER,
        type: typ,
        delivery_id: deliveryId !== "" ? deliveryId + ":" + typ : "",
        actor: { ...a2 },
        source: src,
        attributes: a,
        payload: JSON.parse(body) as unknown,
        text,
        received_at: this.now().toISOString(),
      });
    };
    let sessionText = "";
    let startSession = false;
    switch (p.webhookEvent) {
      case "jira:issue_created": {
        const a = base();
        a[A.AddedLabels] = [...labels];
        emit("issue_created", issueText, a);
        if (labels.length > 0) {
          const la = base();
          la[A.AddedLabels] = [...labels];
          emit("issue_labeled", issueText, la);
        }
        if (bot !== "" && is.fields.assignee?.accountId === bot) {
          startSession = true;
          sessionText = issueText;
        }
        for (const m of adfMentions(is.fields.description)) {
          if (bot !== "" && m === bot) {
            startSession = true;
            sessionText = issueText;
          }
        }
        break;
      }
      case "jira:issue_updated": {
        if (!p.changelog) break;
        for (const it of p.changelog.items ?? []) {
          switch (it.field.toLowerCase()) {
            case "labels": {
              const added = diff(splitLabels(it.toString), splitLabels(it.fromString ?? ""));
              if (added.length > 0) {
                const a = base();
                a[A.Labels] = splitLabels(it.toString);
                a[A.AddedLabels] = added;
                emit("issue_labeled", issueText, a);
              }
              break;
            }
            case "status": {
              const a = base();
              a[A.StatusID] = it.to;
              a[A.StatusName] = it.toString;
              emit("status_changed", issueText, a);
              break;
            }
            case "assignee":
              if (bot !== "" && it.to === bot) {
                startSession = true;
                sessionText = issueText;
              }
          }
        }
        break;
      }
      case "comment_created": {
        if (!p.comment) break;
        const text = adfText(p.comment.body);
        const a = base();
        a["comment_id"] = p.comment.id;
        emit("comment_created", text, a);
        for (const m of adfMentions(p.comment.body)) {
          if (bot !== "" && m === bot) {
            startSession = true;
            sessionText = text;
          }
        }
        break;
      }
    }
    if (startSession) {
      let mem = null;
      try {
        mem = await member(this.deps.store, in_.team_id, PROVIDER, actor.external_id);
      } catch (e) {
        this.log.warn("membership lookup", { err: errMessage(e) });
      }
      if (!mem) {
        // A comment is public, so the prompt has no link URL.
        const msg =
          "To start factory work from Jira, connect your Jira account to an active member of this factory's Open Factory team, then assign or mention the factory again. " +
          dashboardHint(this.deps.linker, PROVIDER);
        try {
          await this.comment(in_, is.key, msg);
        } catch (e) {
          this.log.warn("post connect prompt", { err: errMessage(e) });
        }
      } else {
        actor.user_id = mem.id;
        const a = base();
        a[A.Linked] = true;
        emit("agent_session_created", sessionText, a);
      }
    }
    return out;
  }

  private async linkActor(a: Actor): Promise<void> {
    if (a.external_id === "") return;
    try {
      const x = await this.deps.store.findExternalIdentity(a.provider, a.external_id);
      if (x) a.user_id = x.user_id;
    } catch (e) {
      this.log.warn("lookup external identity", { provider: a.provider, err: errMessage(e) });
    }
  }

  private async apiClient(in_: Integration): Promise<ApiClient> {
    const tok = await integrationSecret(this.deps.secrets, in_);
    return newApiClient(in_, tok, this.deps.fetch);
  }

  private async comment(in_: Integration, key: string, markdown: string): Promise<void> {
    const c = await this.apiClient(in_);
    try {
      await c.send("POST", `/rest/api/3/issue/${encodeURIComponent(key)}/comment`, { body: markdownToAdf(markdown) });
    } catch (e) {
      throw new Error(`jira: comment on ${key}: ${errMessage(e)}`, { cause: e });
    }
  }

  /** The Jira record of a team for a source: the site of the source (workspace, else the host of its URL). */
  private integrationFor(teamId: string, factoryId: string, src: SourceRef): Promise<Integration> {
    const site = src.workspace || src.url || "";
    return selectTeam(this.deps.store, teamId, PROVIDER, factoryId, site);
  }

  async reply(teamId: string, msg: OutboundMessage): Promise<void> {
    if (!msg.source.external_id) throw new Error("jira: outbound message has no issue key");
    const in_ = await this.integrationFor(teamId, msg.factory_id ?? "", msg.source);
    await this.comment(in_, msg.source.external_id, formatOutbound(msg));
  }

  /** Runs the first transition that leads to a status of the matching category. A workflow without a review status leaves the issue as it is. */
  async setIssueState(teamId: string, issue: SourceRef, state: string, factoryId = ""): Promise<void> {
    const key = issue.external_id ?? "";
    if (key === "") throw new Error("jira: issue has no key");
    const in_ = await this.integrationFor(teamId, factoryId, issue);
    const c = await this.apiClient(in_);
    const path = `/rest/api/3/issue/${encodeURIComponent(key)}/transitions`;
    let out: { transitions?: JiraTransition[] } | undefined;
    try {
      out = await c.json("GET", path);
    } catch (e) {
      throw new Error(`jira: list transitions of ${key}: ${errMessage(e)}`, { cause: e });
    }
    const id = pickTransition(out?.transitions ?? [], state);
    if (id === "") return;
    try {
      await c.send("POST", path, { transition: { id } });
    } catch (e) {
      throw new Error(`jira: transition ${key} to ${state}: ${errMessage(e)}`, { cause: e });
    }
  }

  /** Attaches a pull request with a remote link. */
  async attachPullRequest(teamId: string, issue: SourceRef, pr: PullRequestRef, factoryId = ""): Promise<void> {
    const key = issue.external_id ?? "";
    if (key === "") throw new Error("jira: issue has no key");
    if (pr.url === "") throw new OfError("invalid_request", "jira: the pull request has no URL");
    const in_ = await this.integrationFor(teamId, factoryId, issue);
    const c = await this.apiClient(in_);
    const body = {
      // The global ID makes a second call update the link and not add one.
      globalId: "openfactory:pr:" + pr.url,
      object: { url: pr.url, title: `Pull request ${pr.repo}#${pr.number}` },
    };
    try {
      await c.send("POST", `/rest/api/3/issue/${encodeURIComponent(key)}/remotelink`, body);
    } catch (e) {
      throw new Error(`jira: link pull request to ${key}: ${errMessage(e)}`, { cause: e });
    }
  }
}
