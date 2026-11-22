// Linear: webhook receiver (Linear-Signature HMAC-SHA256; Issue, Comment and
// AgentSessionEvent payloads), account linking for agent sessions, a Responder
// (issue comments or agent activities) and the tracker write-back.
// Port of internal/integrations/linear/{linear,tracker}.go.
//
// Integration records (provider "linear"): config organization_id,
// app_user_id, token_type ("oauth" or an API key), webhook_secret_ref;
// secret_ref: access token or API key.
import {
  ISSUE_STATE_DONE,
  ISSUE_STATE_IN_PROGRESS,
  ISSUE_STATE_IN_REVIEW,
  OfError,
  integrationWorkspace,
  newId,
} from "@openfactory/core";
import type { Actor, Event, Integration, OutboundMessage, PullRequestRef, SourceRef } from "@openfactory/core";
import { A, PROVIDER_LINEAR } from "../eventattr";
import { ApiClient } from "../http";
import { intake, selectTeam } from "../select";
import { dashboardHint, member } from "../shared";
import type { Fetch, IntegrationDeps, IssueTracker, Logger, Responder, WebhookOutcome } from "../types";
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
  stringSet,
  verifyHmacSha256Hex,
} from "../util";

export const PROVIDER = PROVIDER_LINEAR;
export const WEBHOOK_PATH = "/integrations/linear/webhook";
export const DEFAULT_GRAPHQL_URL = "https://api.linear.app/graphql";
/** Bounds webhookTimestamp (replay protection). */
export const MAX_TIMESTAMP_SKEW_MS = 60_000;

// ---------- GraphQL client ----------

export class GraphQLError extends Error {
  override readonly name = "GraphQLError";
  constructor(readonly messages: string[]) {
    super("linear graphql: " + messages.join("; "));
  }
}

export class LinearClient {
  private readonly api: ApiClient;
  /** tokenType "oauth" or "bearer" sends "Bearer <token>"; otherwise the API key is sent as is. */
  constructor(endpoint: string, token: string, tokenType: string, fetchFn?: Fetch) {
    const t = tokenType.toLowerCase();
    const auth = t === "oauth" || t === "bearer" ? "Bearer " + token : token;
    this.api = new ApiClient({
      baseUrl: endpoint || DEFAULT_GRAPHQL_URL,
      fetch: fetchFn,
      userAgent: "openfactory-linear",
      authorize: (h) => h.set("Authorization", auth),
    });
  }

  async query<T = unknown>(query: string, vars: Record<string, unknown>): Promise<T | undefined> {
    const resp = await this.api.json<{ data?: T; errors?: { message: string }[] }>("POST", "", { query, variables: vars });
    if (resp?.errors && resp.errors.length > 0) throw new GraphQLError(resp.errors.map((e) => e.message));
    return resp?.data;
  }

  /** Posts a comment on an issue (UUID or identifier such as ENG-42). */
  async createComment(issueId: string, body: string): Promise<void> {
    const out = await this.query<{ commentCreate?: { success?: boolean } }>(
      "mutation CommentCreate($input: CommentCreateInput!) { commentCreate(input: $input) { success } }",
      { input: { issueId, body } },
    );
    if (!out?.commentCreate?.success) throw new Error("linear: commentCreate was not successful");
  }

  /** Posts an activity into an agent session. kind: thought, action, elicitation, response, error. */
  async createAgentActivity(sessionId: string, kind: string, body: string): Promise<void> {
    const out = await this.query<{ agentActivityCreate?: { success?: boolean } }>(
      "mutation AgentActivityCreate($input: AgentActivityCreateInput!) { agentActivityCreate(input: $input) { success } }",
      { input: { agentSessionId: sessionId, content: { type: kind, body } } },
    );
    if (!out?.agentActivityCreate?.success) throw new Error("linear: agentActivityCreate was not successful");
  }

  async issue(id: string): Promise<IssueContext> {
    const out = await this.query<{ issue: IssueContext }>(
      `query Issue($id: String!) { issue(id: $id) { id identifier number title url
		team { id name key } project { id name } state { id name } labels { nodes { id name } } } }`,
      { id },
    );
    if (!out) throw new Error("linear: empty answer");
    return out.issue;
  }
}

interface Named {
  id: string;
  name: string;
  key?: string;
}

export interface IssueContext {
  id: string;
  identifier: string;
  number: number;
  title: string;
  url: string;
  team?: Named | null;
  project?: Named | null;
  state?: Named | null;
  labels?: { nodes?: Named[] };
}

// ---------- webhook payloads ----------

interface ActorRef {
  id: string;
  type?: string;
  name?: string;
}

interface IssueData {
  id?: string;
  identifier?: string;
  number?: number;
  title?: string;
  description?: string;
  url?: string;
  teamId?: string;
  team?: Named | null;
  projectId?: string;
  project?: Named | null;
  stateId?: string;
  state?: Named | null;
  assigneeId?: string;
  creatorId?: string;
  labelIds?: string[];
  labels?: Named[];
}

interface Payload {
  action?: string;
  type?: string;
  data?: unknown;
  updatedFrom?: Record<string, unknown>;
  url?: string;
  organizationId?: string;
  webhookTimestamp?: number;
  actor?: ActorRef | null;
  agentSession?: {
    id: string;
    issueId?: string;
    issue?: IssueData | null;
    creatorId?: string;
    creator?: ActorRef | null;
    comment?: { id: string; body: string } | null;
  } | null;
  agentActivity?: { content?: { type?: string; body?: string }; userId?: string } | null;
  promptContext?: string;
}

const UUID = "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
const PROFILE_URL_RE = /https?:\/\/linear\.app\/[^/\s)]+\/profiles\/([A-Za-z0-9._-]+)/g;
const MENTION_UUID_RE = new RegExp(`(?:mention[^>\\]]*?|@\\[[^\\]]*\\]\\()(${UUID})`, "gi");

/** User UUIDs from mention markup and profile slugs from profile URLs. */
export function extractMentions(text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(MENTION_UUID_RE)) out.push(m[1]!.toLowerCase());
  for (const m of text.matchAll(PROFILE_URL_RE)) out.push(m[1]!);
  return stringSet(...out);
}

const labelNames = (ls: Named[] | undefined): string[] => (ls ?? []).map((l) => l.name);

// ---------- tracker state ----------

export interface WorkflowState {
  id: string;
  name: string;
  /** triage, backlog, unstarted, started, completed, canceled */
  type: string;
  position: number;
}

/** The workflow state for a generic issue state. "" when no state fits or the issue is already in one. */
export function pickState(states: WorkflowState[], current: WorkflowState, state: string): string {
  const sorted = [...states].sort((a, b) => a.position - b.position);
  const find = (typ: string, prefer: string): string => {
    let first = "";
    for (const s of sorted) {
      if (s.type !== typ) continue;
      if (prefer !== "" && s.name.toLowerCase().includes(prefer)) return s.id;
      if (first === "") first = s.id;
    }
    return first;
  };
  let want = "";
  switch (state) {
    case ISSUE_STATE_DONE:
      if (current.type === "completed") return "";
      want = find("completed", "done");
      break;
    case ISSUE_STATE_IN_PROGRESS:
      if (current.type === "started" || current.type === "completed") return "";
      want = find("started", "progress");
      break;
    case ISSUE_STATE_IN_REVIEW:
      if (current.type === "completed" || current.name.toLowerCase().includes("review")) return "";
      for (const s of sorted) {
        if (s.type === "started" && s.name.toLowerCase().includes("review")) {
          want = s.id;
          break;
        }
      }
      if (want === "" && current.type !== "started") want = find("started", "progress");
  }
  return want === current.id ? "" : want;
}

const issueIdOf = (src: SourceRef): string => src.channel || src.external_id || "";

export class LinearHandler implements Responder, IssueTracker {
  private readonly log: Logger;
  private readonly dedupe = new Dedupe(3600_000);
  constructor(
    readonly deps: IntegrationDeps,
    private readonly graphqlUrl = "",
  ) {
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
    let p: Payload;
    try {
      p = JSON.parse(body) as Payload;
    } catch {
      return { response: errorResponse(400, "invalid_payload", "payload is not valid JSON") };
    }
    const found = await intake(this.deps.store, PROVIDER, p.organizationId ?? "");
    if (!(await this.verify(found.integration, body, req.headers.get("Linear-Signature") ?? ""))) {
      return { response: errorResponse(401, "invalid_signature", "Linear-Signature does not match") };
    }
    if (p.webhookTimestamp) {
      const skew = this.now().getTime() - p.webhookTimestamp;
      if (skew > MAX_TIMESTAMP_SKEW_MS || skew < -MAX_TIMESTAMP_SKEW_MS) {
        return { response: errorResponse(401, "stale_webhook", "webhookTimestamp is outside the allowed window") };
      }
    }
    if (found.error || !found.integration) {
      this.log.info("delivery ignored", { org: p.organizationId, err: found.error?.message });
      return { response: jsonResponse(202, { status: "ignored", reason: "no matching integration" }) };
    }
    const delivery = req.headers.get("Linear-Delivery") ?? "";
    if (this.dedupe.isSeen(delivery)) return { response: jsonResponse(200, { status: "duplicate" }) };
    const in_ = found.integration;
    return {
      response: jsonResponse(202, { status: "accepted" }),
      background: async () => {
        let evs: Event[];
        try {
          evs = await this.normalize(in_, delivery, body);
        } catch (e) {
          this.log.warn("normalize linear webhook", { err: errMessage(e) });
          return;
        }
        if (evs.length > 0) await this.deps.publish(evs);
      },
    };
  }

  private async verify(in_: Integration | null, body: string, sig: string): Promise<boolean> {
    if (sig === "") return false;
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

  async client(in_: Integration): Promise<LinearClient> {
    const tok = await integrationSecret(this.deps.secrets, in_);
    return new LinearClient(this.graphqlUrl, tok, cfg(in_, "token_type"), this.deps.fetch);
  }

  /** The client for the Linear record of a team that serves a source: the organization of the source, the record of the factory first, then the team record. */
  private async teamClient(teamId: string, factoryId: string, src: SourceRef): Promise<LinearClient> {
    const in_ = await selectTeam(this.deps.store, teamId, PROVIDER, factoryId, src.workspace ?? "");
    return this.client(in_);
  }

  private issueAttrs(is: IssueData, labels: string[]): Record<string, unknown> {
    const a: Record<string, unknown> = {
      [A.IssueID]: is.id ?? "",
      [A.IssueNumber]: is.number ?? 0,
      [A.Labels]: labels,
      [A.Author]: is.creatorId ?? "",
    };
    const setNamed = (idKey: string, nameKey: string, id: string, n: Named | null | undefined): void => {
      if (n && n.id) id = n.id;
      if (id !== "") a[idKey] = id;
      if (n && n.name) a[nameKey] = n.name;
    };
    setNamed(A.TeamID, A.TeamName, is.teamId ?? "", is.team);
    setNamed(A.ProjectID, A.ProjectName, is.projectId ?? "", is.project);
    setNamed(A.StateID, A.StateName, is.stateId ?? "", is.state);
    a[A.Assignees] = is.assigneeId ? [is.assigneeId] : [];
    return a;
  }

  private async enrich(c: LinearClient | null, issueId: string, is: IssueData): Promise<string[]> {
    if (!c || issueId === "") return labelNames(is.labels);
    try {
      const ic = await c.issue(issueId);
      is.id = ic.id;
      is.identifier = ic.identifier;
      is.number = ic.number;
      is.url = ic.url;
      if (!is.title) is.title = ic.title;
      is.team = ic.team;
      is.project = ic.project;
      is.state = ic.state;
      return labelNames(ic.labels?.nodes);
    } catch (e) {
      this.log.debug("load linear issue", { issue: issueId, err: errMessage(e) });
      return labelNames(is.labels);
    }
  }

  /** Converts a verified webhook payload into events, enriching through GraphQL when a token is configured. */
  async normalize(in_: Integration, deliveryId: string, body: string): Promise<Event[]> {
    let p: Payload;
    try {
      p = JSON.parse(body) as Payload;
    } catch (e) {
      throw new Error(`linear: decode payload: ${errMessage(e)}`, { cause: e });
    }
    const appUser = cfg(in_, "app_user_id");
    let c: LinearClient | null = null;
    try {
      c = await this.client(in_);
    } catch {
      c = null;
    }
    const factoryId = in_.factory_id ?? "";
    const workspace = integrationWorkspace(in_);
    const out: Event[] = [];
    const ownApps = new Set<string>();
    if (appUser !== "") ownApps.add(appUser);
    try {
      for (const o of await this.deps.store.listIntegrations("")) {
        const u = cfg(o, "app_user_id");
        if (o.provider.toLowerCase() === PROVIDER && u !== "") ownApps.add(u);
      }
    } catch {
      /* only the own app is known */
    }
    const payload = JSON.parse(body) as unknown;
    const emit = (typ: string, actor: Actor, src: SourceRef, text: string, attrs: Record<string, unknown>): void => {
      const self = actor.external_id !== "" && ownApps.has(actor.external_id);
      const a: Actor = { ...actor, is_bot: actor.is_bot || self };
      attrs[A.ActorIsSelf] = self;
      attrs[A.ActorIsBot] = a.is_bot;
      if (text !== "") attrs[A.Text] = text;
      out.push({
        id: newId("evt"),
        team_id: in_.team_id,
        factory_id: factoryId,
        provider: PROVIDER,
        type: typ,
        delivery_id: deliveryId !== "" ? deliveryId + ":" + typ : "",
        actor: a,
        source: src,
        attributes: attrs,
        payload,
        text,
        received_at: this.now().toISOString(),
      });
    };
    const actorOf = async (id: string, name: string, typ: string): Promise<Actor> => {
      const a: Actor = { provider: PROVIDER, external_id: id, handle: name, is_bot: typ !== "" && typ !== "user" };
      if (id !== "") {
        try {
          const x = await this.deps.store.findExternalIdentity(PROVIDER, id);
          if (x) a.user_id = x.user_id;
        } catch (e) {
          this.log.warn("lookup external identity", { err: errMessage(e) });
        }
      }
      return a;
    };
    const issueSource = (is: IssueData): SourceRef => ({
      provider: PROVIDER,
      kind: "issue",
      url: is.url ?? "",
      channel: is.id ?? "",
      external_id: is.identifier ?? "",
      title: is.title ?? "",
      workspace,
    });
    let pActor: Actor = { provider: "", external_id: "", handle: "", is_bot: false };
    if (p.actor) pActor = await actorOf(p.actor.id, p.actor.name ?? "", p.actor.type ?? "");
    if (appUser !== "" && pActor.external_id === appUser) return []; // the app's own activity never starts work

    switch (p.type) {
      case "Issue": {
        const is = (p.data ?? {}) as IssueData;
        if (!is.url) is.url = p.url ?? "";
        const labels = labelNames(is.labels);
        const base = this.issueAttrs(is, labels);
        base[A.Mentioned] = extractMentions(is.description ?? "");
        const src = issueSource(is);
        const text = ((is.title ?? "") + "\n\n" + (is.description ?? "")).trim();
        if (pActor.external_id === "") pActor = await actorOf(is.creatorId ?? "", "", "user");
        const cp = (): Record<string, unknown> => ({ ...base });
        const uf = p.updatedFrom ?? {};
        if (p.action === "create") {
          emit("issue_created", pActor, src, text, cp());
          if (labels.length > 0) {
            const a = cp();
            a[A.AddedLabels] = labels;
            emit("issue_labeled", pActor, src, text, a);
          }
          if (is.assigneeId) emit("issue_assigned", pActor, src, text, cp());
        } else if (p.action === "update") {
          if ("labelIds" in uf) {
            const prev = Array.isArray(uf["labelIds"]) ? (uf["labelIds"] as string[]) : [];
            const addedIds = diff(is.labelIds ?? [], prev);
            const added = (is.labels ?? []).flatMap((l) => addedIds.filter((id) => id === l.id).map(() => l.name));
            if (added.length > 0) {
              const a = cp();
              a[A.AddedLabels] = added;
              emit("issue_labeled", pActor, src, text, a);
            }
          }
          if ("assigneeId" in uf && is.assigneeId) emit("issue_assigned", pActor, src, text, cp());
          if ("stateId" in uf) emit("issue_state_changed", pActor, src, text, cp());
        }
        break;
      }
      case "Comment": {
        if (p.action !== "create") return [];
        const cm = (p.data ?? {}) as {
          id?: string;
          body?: string;
          issueId?: string;
          issue?: IssueData | null;
          userId?: string;
          user?: ActorRef | null;
        };
        if (appUser !== "" && cm.userId === appUser) return [];
        let is: IssueData = { id: cm.issueId ?? "" };
        if (cm.issue) {
          is = { ...cm.issue };
          if (!is.id) is.id = cm.issueId ?? "";
        }
        const labels = await this.enrich(c, is.id ?? "", is);
        const a = this.issueAttrs(is, labels);
        delete a[A.Assignees];
        a[A.Author] = cm.userId ?? "";
        a[A.Mentioned] = extractMentions(cm.body ?? "");
        a["comment_id"] = cm.id ?? "";
        const actor = pActor.external_id === "" ? await actorOf(cm.userId ?? "", cm.user?.name ?? "", "user") : pActor;
        emit("comment_created", actor, issueSource(is), cm.body ?? "", a);
        break;
      }
      case "AgentSessionEvent": {
        const s = p.agentSession;
        if (!s) return [];
        let is: IssueData = { id: s.issueId ?? "" };
        if (s.issue) is = { ...s.issue };
        const labels = await this.enrich(c, is.id ?? "", is);
        const a = this.issueAttrs(is, labels);
        delete a[A.Assignees];
        a[A.Author] = s.creatorId ?? "";
        a[A.SessionID] = s.id;
        let actor = await actorOf(s.creatorId ?? "", s.creator?.name ?? "", "user");
        const src: SourceRef = {
          provider: PROVIDER,
          kind: "agent_session",
          url: is.url ?? "",
          channel: is.id ?? "",
          thread_id: s.id,
          external_id: is.identifier ?? "",
          title: is.title ?? "",
          workspace,
        };
        let text = "";
        let typ = "";
        switch (p.action) {
          case "created":
            typ = "agent_session_created";
            text = s.comment?.body ?? "";
            if (text === "") text = p.promptContext ?? "";
            break;
          case "prompted":
            typ = "agent_session_prompted"; // a follow-up in an existing session
            if (p.agentActivity) {
              text = p.agentActivity.content?.body ?? "";
              if (p.agentActivity.userId) actor = await actorOf(p.agentActivity.userId, "", "user");
            }
            break;
          default:
            return [];
        }
        // Agent sessions need a linked account of an active team member.
        let mem = null;
        try {
          mem = await member(this.deps.store, in_.team_id, PROVIDER, actor.external_id);
        } catch (e) {
          this.log.warn("membership lookup", { err: errMessage(e) });
        }
        if (!mem) {
          if (c) {
            // The agent session is visible to the workspace, so the prompt has no link URL.
            const msg =
              "Link your Linear account to an active member of this factory's Open Factory team, then retry the request. " +
              dashboardHint(this.deps.linker, PROVIDER);
            try {
              await c.createAgentActivity(s.id, "elicitation", msg);
            } catch (e) {
              this.log.warn("post authentication prompt", { err: errMessage(e) });
            }
          }
          return [];
        }
        actor.user_id = mem.id;
        a[A.Linked] = true;
        emit(typ, actor, src, text, a);
        break;
      }
    }
    return out;
  }

  /** Agent-session sources receive agent activities; issues receive comments. */
  async reply(teamId: string, msg: OutboundMessage): Promise<void> {
    const c = await this.teamClient(teamId, msg.factory_id ?? "", msg.source);
    const body = formatOutbound(msg);
    if (msg.source.kind === "agent_session" && msg.source.thread_id) {
      let kind = "response";
      switch (msg.kind) {
        case "question":
        case "approval_request":
          kind = "elicitation";
          break;
        case "error":
          kind = "error";
          break;
        case "update":
          kind = "thought";
      }
      await c.createAgentActivity(msg.source.thread_id, kind, body);
      return;
    }
    const issue = msg.source.channel || msg.source.external_id || "";
    if (issue === "") throw new Error("linear: outbound message has no issue");
    await c.createComment(issue, body);
  }

  /** Moves the issue to the first state of the matching type of its team. */
  async setIssueState(teamId: string, issue: SourceRef, state: string, factoryId = ""): Promise<void> {
    const id = issueIdOf(issue);
    if (id === "") throw new Error("linear: issue has no ID");
    const c = await this.teamClient(teamId, factoryId, issue);
    let out: { issue: { id: string; state: WorkflowState; team: { states: { nodes: WorkflowState[] } } } } | undefined;
    try {
      out = await c.query(
        "query IssueStates($id: String!) { issue(id: $id) { id state { id name type position } team { states { nodes { id name type position } } } } }",
        { id },
      );
    } catch (e) {
      throw new Error(`linear: read states of ${id}: ${errMessage(e)}`, { cause: e });
    }
    if (!out) throw new Error(`linear: read states of ${id}: empty answer`);
    const stateId = pickState(out.issue.team.states.nodes, out.issue.state, state);
    if (stateId === "") return;
    let res: { issueUpdate?: { success?: boolean } } | undefined;
    try {
      res = await c.query(
        "mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }",
        { id: out.issue.id, input: { stateId } },
      );
    } catch (e) {
      throw new Error(`linear: set state of ${id}: ${errMessage(e)}`, { cause: e });
    }
    if (!res?.issueUpdate?.success) throw new Error("linear: issueUpdate was not successful");
  }

  /** Attaches a pull request. Linear keeps one attachment for each URL, so a second call updates it. */
  async attachPullRequest(teamId: string, issue: SourceRef, pr: PullRequestRef, factoryId = ""): Promise<void> {
    const id = issueIdOf(issue);
    if (id === "") throw new Error("linear: issue has no ID");
    if (pr.url === "") throw new OfError("invalid_request", "linear: the pull request has no URL");
    const c = await this.teamClient(teamId, factoryId, issue);
    const input: Record<string, unknown> = { issueId: id, url: pr.url, title: `Pull request ${pr.repo}#${pr.number}` };
    if (pr.state !== "") input["subtitle"] = pr.state;
    let res: { attachmentCreate?: { success?: boolean } } | undefined;
    try {
      res = await c.query(
        "mutation AttachmentCreate($input: AttachmentCreateInput!) { attachmentCreate(input: $input) { success } }",
        { input },
      );
    } catch (e) {
      throw new Error(`linear: attach pull request to ${id}: ${errMessage(e)}`, { cause: e });
    }
    if (!res?.attachmentCreate?.success) throw new Error("linear: attachmentCreate was not successful");
  }
}
