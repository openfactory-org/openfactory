// GitLab webhook receiver: verifies the delivery against the integration
// records of the control plane and maps it to plugin events. Port of
// plugins/gitlab/internal/gitlab/webhook.go.
//
// Integration config keys (provider "gitlab"):
//   org                top-level group the record covers (deliveries are matched by project path)
//   bot_username       the team-wide bot account (a mention of it emits bot_mentioned)
//   factory_bots       per-factory bot accounts, "username=alias,username2=alias2"
//   service_accounts   other service accounts whose activity is ignored, comma separated
//   webhook_secret_ref name of the secret that holds the X-Gitlab-Token value

import { newId } from "@openfactory/core";
import type { HostApi, IntegrationRecord, PluginEvent, SourceRef, WebhookResult } from "@openfactory/plugin-api";
import {
  BodyTooLargeError,
  DeliveryAmbiguousError,
  EVENT_ATTR as A,
  PROVIDER_GITLAB,
  constantTimeEqual,
  errorResult,
  extractMentions,
  integrationSecret,
  intakeVerified,
  jsonResult,
  readBody,
  repoOwner,
  stringSet,
} from "@openfactory/plugin-api";

export const PROVIDER = PROVIDER_GITLAB;
export const WEBHOOK_PATH = "/plugins/gitlab/webhook";

interface GlUser {
  id?: number;
  username?: string;
  name?: string;
}

interface GlMR {
  iid: number;
  title?: string;
  description?: string;
  url?: string;
  action?: string;
  state?: string;
  source_branch?: string;
  target_branch?: string;
  draft?: boolean;
  last_commit?: { id?: string };
}

interface GlPayload {
  object_kind?: string;
  user?: GlUser;
  project?: { id?: number; path_with_namespace?: string; web_url?: string };
  object_attributes?: Record<string, unknown>;
  labels?: Array<{ title: string }>;
  merge_request?: GlMR;
  issue?: { iid: number; title?: string; description?: string; url?: string };
  ref?: string;
  after?: string;
  user_id?: number;
  user_username?: string;
  commits?: Array<{ message?: string; added?: string[]; modified?: string[]; removed?: string[] }>;
}

interface Bots {
  teamBot: string;
  factoryBots: Map<string, string>; // username (lower) -> alias
  service: Set<string>;
}

export function parseBots(rec: IntegrationRecord): Bots {
  const b: Bots = {
    teamBot: (rec.config["bot_username"] ?? "").replace(/^@/, "").toLowerCase(),
    factoryBots: new Map(),
    service: new Set(),
  };
  for (const pair of (rec.config["factory_bots"] ?? "").split(",")) {
    const t = pair.trim();
    const i = t.indexOf("=");
    if (i < 0) continue;
    const u = t.slice(0, i).replace(/^@/, "").toLowerCase();
    if (u !== "") b.factoryBots.set(u, t.slice(i + 1).trim());
  }
  for (const s of (rec.config["service_accounts"] ?? "").split(",")) {
    const v = s.trim().replace(/^@/, "").toLowerCase();
    if (v !== "") b.service.add(v);
  }
  return b;
}

function isSelf(b: Bots, username: string): boolean {
  const u = username.toLowerCase();
  if (u === "") return false;
  return b.factoryBots.has(u) || b.service.has(u) || (b.teamBot !== "" && u === b.teamBot);
}

function joinText(title: string, body: string): string {
  return body === "" ? title : `${title}\n\n${body}`;
}

export interface NormalizeOptions {
  now?: () => Date;
}

/** Maps a delivery to events for the team of the integration record. */
export function normalize(
  rec: IntegrationRecord,
  eventHeader: string,
  deliveryId: string,
  body: string,
  opts: NormalizeOptions = {},
): PluginEvent[] {
  const parsed: unknown = JSON.parse(body);
  const p = (parsed ?? {}) as GlPayload;
  const now = opts.now ?? (() => new Date());
  const bots = parseBots(rec);
  const repo = p.project?.path_with_namespace ?? "";
  let user: GlUser = p.user ?? {};
  if (!user.username && p.user_username) user = { id: p.user_id ?? 0, username: p.user_username };
  const self = isSelf(bots, user.username ?? "");
  const out: PluginEvent[] = [];

  const emit = (type: string, source: SourceRef, text: string, attrs: Record<string, unknown>): void => {
    const a: Record<string, unknown> = { [A.Repo]: repo, [A.ActorIsBot]: self, [A.ActorIsSelf]: self, ...attrs };
    if (text !== "") a[A.Text] = text;
    const ev: PluginEvent = {
      id: newId("evt"),
      teamId: rec.teamId,
      provider: PROVIDER,
      type,
      actor: { provider: PROVIDER, externalId: String(user.id ?? 0), handle: user.username ?? "", isBot: self },
      source,
      attributes: a,
      payload: parsed,
      text,
      receivedAt: now().toISOString(),
    };
    if (deliveryId !== "") {
      // One delivery can give several events: they share the action key, and the engine routes them once.
      ev.deliveryId = `${deliveryId}:${type}`;
      ev.actionKey = `${PROVIDER}:${deliveryId}`;
    }
    out.push(ev);
  };

  const labels = (p.labels ?? []).map((l) => l.title);
  let kind = p.object_kind ?? "";
  if (kind === "") kind = eventHeader.replaceAll(" ", "_").replace(/_Hook$/, "").toLowerCase();
  const oa = p.object_attributes ?? {};

  switch (kind) {
    case "merge_request": {
      const mr = oa as unknown as GlMR;
      if (typeof mr.iid !== "number") throw new Error("gitlab: decode merge request: iid is not a number");
      const src: SourceRef = {
        provider: PROVIDER,
        kind: "pull_request",
        url: mr.url ?? "",
        repo,
        number: mr.iid,
        title: mr.title ?? "",
      };
      emit("merge_request", src, joinText(mr.title ?? "", mr.description ?? ""), {
        [A.Action]: mr.action ?? "",
        [A.BaseBranch]: mr.target_branch ?? "",
        [A.HeadBranch]: mr.source_branch ?? "",
        [A.HeadSHA]: mr.last_commit?.id ?? "",
        [A.PRNumber]: mr.iid,
        [A.Labels]: labels,
        [A.Draft]: mr.draft ?? false,
      });
      break;
    }
    case "note": {
      const note = oa as {
        note?: string;
        noteable_type?: string;
        url?: string;
        discussion_id?: string;
        action?: string;
        type?: string | null;
      };
      if (note.action === "update") return out; // only new comments count
      let src: SourceRef;
      if (note.noteable_type === "MergeRequest" && p.merge_request) {
        src = {
          provider: PROVIDER,
          kind: "pull_request",
          url: p.merge_request.url ?? "",
          repo,
          number: p.merge_request.iid,
          title: p.merge_request.title ?? "",
        };
      } else if (note.noteable_type === "Issue" && p.issue) {
        src = {
          provider: PROVIDER,
          kind: "issue",
          url: p.issue.url ?? "",
          repo,
          number: p.issue.iid,
          title: p.issue.title ?? "",
        };
      } else {
        return out;
      }
      const discussion = note.discussion_id ?? "";
      const attrs: Record<string, unknown> = { discussion_id: discussion, comment_url: note.url ?? "" };
      if (src.kind === "pull_request") {
        attrs[A.PRNumber] = src.number;
        // A note in a discussion (a review thread) gets its reply in that discussion.
        if (discussion !== "" && (note.type === "DiffNote" || note.type === "DiscussionNote")) {
          src.threadRef = discussion;
          attrs[A.ThreadRef] = discussion;
        }
      }
      const text = note.note ?? "";
      // A comment continues an existing work item on the same merge request or issue.
      emit(`${src.kind}_commented`, src, text, attrs);
      const botHits: string[] = [];
      let alias = "";
      for (const m of extractMentions(text)) {
        const lm = m.toLowerCase();
        const a = bots.factoryBots.get(lm);
        if (a !== undefined) {
          botHits.push(m);
          if (alias === "") alias = a;
        } else if (bots.teamBot !== "" && lm === bots.teamBot) {
          botHits.push(m);
        }
      }
      if (botHits.length > 0 && !self) {
        const ma: Record<string, unknown> = { [A.Mentioned]: botHits, discussion_id: discussion };
        if (alias !== "") ma[A.Alias] = alias;
        emit("bot_mentioned", src, text, ma);
      }
      break;
    }
    case "push": {
      const ref = p.ref ?? "";
      const after = p.after ?? "";
      if (!ref.startsWith("refs/heads/") || /^0*$/.test(after)) return out;
      const branch = ref.slice("refs/heads/".length);
      const paths: string[] = [];
      let text = "";
      for (const c of p.commits ?? []) {
        paths.push(...(c.added ?? []), ...(c.modified ?? []), ...(c.removed ?? []));
        text = c.message ?? "";
      }
      emit("push", { provider: PROVIDER, kind: "push", repo, externalId: `${repo}@${after}`, title: branch }, text, {
        [A.Branch]: branch,
        [A.Paths]: stringSet(...paths),
        [A.HeadSHA]: after,
      });
      break;
    }
    case "issue": {
      const is = oa as { iid?: number; title?: string; description?: string; url?: string; action?: string };
      emit(
        "issue",
        { provider: PROVIDER, kind: "issue", url: is.url ?? "", repo, number: is.iid ?? 0, title: is.title ?? "" },
        joinText(is.title ?? "", is.description ?? ""),
        { [A.Action]: is.action ?? "", [A.Labels]: labels },
      );
      break;
    }
    case "pipeline": {
      const pl = oa as { id?: number; ref?: string; sha?: string; status?: string; url?: string };
      const attrs: Record<string, unknown> = {
        [A.Branch]: pl.ref ?? "",
        [A.HeadSHA]: pl.sha ?? "",
        [A.Conclusion]: pl.status ?? "",
      };
      if (p.merge_request) attrs[A.PRNumber] = p.merge_request.iid;
      emit(
        "pipeline",
        {
          provider: PROVIDER,
          kind: "pipeline",
          url: pl.url ?? "",
          repo,
          externalId: `${repo}/pipeline/${pl.id ?? 0}`,
          title: pl.ref ?? "",
        },
        "",
        attrs,
      );
      break;
    }
    default:
  }
  return out;
}

export interface WebhookOptions extends NormalizeOptions {}

/** POST /plugins/gitlab/webhook. The sender gets `status`, the events go to IngressDO. */
export async function handleWebhook(req: Request, host: HostApi, opts: WebhookOptions = {}): Promise<WebhookResult> {
  const reject = (status: number, code: string, message: string, headers?: Record<string, string>): WebhookResult => ({
    ...errorResult(status, code, message, headers),
    events: [],
  });
  let body: string;
  try {
    body = await readBody(req);
  } catch (e) {
    if (e instanceof BodyTooLargeError) return reject(413, "payload_too_large", e.message);
    throw e;
  }
  let p: GlPayload;
  try {
    p = (JSON.parse(body) ?? {}) as GlPayload;
  } catch {
    return reject(400, "invalid_payload", "payload is not valid JSON");
  }
  const project = p.project?.path_with_namespace ?? "";
  const token = req.headers.get("X-Gitlab-Token") ?? "";
  const owner = repoOwner(PROVIDER, project);
  let integ: IntegrationRecord;
  try {
    if (owner === "") {
      throw new Error(`no gitlab integration for project "${project}"`);
    }
    const all = await host.listIntegrations(PROVIDER);
    integ = await intakeVerified(all, PROVIDER, [owner], async (r) => {
      if (token === "" || (r.config["webhook_secret_ref"] ?? "") === "") return false;
      try {
        return constantTimeEqual(await integrationSecret(host, r, "webhook_secret_ref"), token);
      } catch (e) {
        await host.log("warn", "load webhook secret", { integration: r.id, err: (e as Error).message });
        return false;
      }
    });
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    if (e instanceof DeliveryAmbiguousError) {
      await host.log("warn", "delivery rejected", { project, err: msg });
      return reject(409, "ambiguous_integration", msg);
    }
    await host.log("info", "delivery not verified", { project, err: msg });
    return reject(401, "invalid_token", "X-Gitlab-Token does not match");
  }
  const delivery = req.headers.get("X-Gitlab-Event-UUID") || req.headers.get("X-Gitlab-Webhook-UUID") || "";
  let events: PluginEvent[];
  try {
    events = normalize(integ, req.headers.get("X-Gitlab-Event") ?? "", delivery, body, opts);
  } catch (e) {
    return reject(400, "invalid_payload", (e as Error).message);
  }
  const aliases = events.some((ev) => typeof ev.attributes?.[A.Alias] === "string" && ev.attributes[A.Alias] !== "");
  if (aliases) {
    const factories = await host.listFactories();
    for (const ev of events) {
      const alias = ev.attributes?.[A.Alias];
      if (typeof alias !== "string" || alias === "") continue;
      const f = factories.find((x) => x.teamId === ev.teamId && x.alias.toLowerCase() === alias.toLowerCase());
      if (f) ev.factoryId = f.id;
    }
  }
  return { ...jsonResult(202, { status: "accepted", events: events.length }), events };
}
