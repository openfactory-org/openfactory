// Azure DevOps service-hook receiver: verifies the delivery against the
// integration records of the control plane and maps it to plugin events. Port of
// plugins/azuredevops/internal/azuredevops/webhook.go.
//
// Integration config keys (provider "azure_devops"):
//   org                  organization name (matched against resourceContainers.account.baseUrl)
//   hook_username        user name of the basic-auth credentials of the service hook
//   hook_password_ref    name of the secret that holds the service hook password
//   factory_identities   "identityGUID=alias,..." factory identities; a mention of one, or a work
//                        item assigned to one, routes to its alias
//   service_identities   other identity GUIDs whose activity is ignored, comma separated

import { newId } from "@openfactory/core";
import type {
  HostApi,
  IntegrationRecord,
  PluginEvent,
  RepoRef,
  SourceRef,
  WebhookResult,
} from "@openfactory/plugin-api";
import {
  BodyTooLargeError,
  DeliveryAmbiguousError,
  EVENT_ATTR as A,
  PROVIDER_AZURE_DEVOPS,
  constantTimeEqual,
  difference,
  errorResult,
  integrationSecret,
  intakeVerified,
  jsonResult,
  readBody,
  stringSet,
  stripCode,
} from "@openfactory/plugin-api";

export const PROVIDER = PROVIDER_AZURE_DEVOPS;
export const WEBHOOK_PATH = "/plugins/azuredevops/webhook";

interface Identity {
  id?: string;
  displayName?: string;
  uniqueName?: string;
}

interface HookPR {
  pullRequestId: number;
  repository?: { id?: string; name?: string; webUrl?: string; project?: { id?: string; name?: string } };
  status?: string;
  createdBy?: Identity;
  title?: string;
  description?: string;
  sourceRefName?: string;
  targetRefName?: string;
  mergeStatus?: string;
  isDraft?: boolean;
  forkSource?: { repository?: { id?: string } } | null;
  lastMergeSourceCommit?: { commitId?: string } | null;
  labels?: Array<{ name: string }>;
}

interface HookPayload {
  id?: string;
  eventType?: string;
  resource?: unknown;
  resourceContainers?: { account?: { id?: string; baseUrl?: string }; project?: { id?: string } };
}

type Fields = Record<string, unknown>;

/** The organization of https://dev.azure.com/<org>/ or https://<org>.visualstudio.com/. */
export function orgFromBaseUrl(base: string): string {
  let u: URL;
  try {
    u = new URL(base);
  } catch {
    return "";
  }
  if (u.host === "") return "";
  if (u.host.endsWith(".visualstudio.com")) return u.host.slice(0, -".visualstudio.com".length);
  return u.pathname.replace(/^\/+|\/+$/g, "").split("/")[0] ?? "";
}

/**
 * Reports whether a factory repository lives in an Azure DevOps project.
 * Repositories are declared as owner=<project> (or <org>/<project>) and name=<repository>.
 */
export function projectCovers(r: RepoRef, project: string): boolean {
  if (r.codeForge !== "" && r.codeForge.toUpperCase() !== "AZURE_DEVOPS") return false;
  let owner = r.owner;
  const i = owner.lastIndexOf("/");
  if (i >= 0) owner = owner.slice(i + 1);
  return project !== "" && owner.toLowerCase() === project.toLowerCase();
}

const MD_MENTION = /@<([0-9a-fA-F-]{36})>/g;
const HTML_MENTION = /data-vss-mention="version:[0-9.]+,([0-9a-fA-F-]{36})"/g;

/** Identity GUIDs mentioned in markdown ("@<GUID>") or work item HTML. */
export function extractMentions(text: string): string[] {
  const out: string[] = [];
  for (const m of stripCode(text).matchAll(MD_MENTION)) out.push(m[1]!.toLowerCase());
  for (const m of text.matchAll(HTML_MENTION)) out.push(m[1]!.toLowerCase());
  return stringSet(...out);
}

function parsePairs(s: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const pair of s.split(",")) {
    const t = pair.trim();
    const i = t.indexOf("=");
    const k = (i < 0 ? t : t.slice(0, i)).trim().toLowerCase();
    if (k !== "") out.set(k, i < 0 ? "" : t.slice(i + 1).trim());
  }
  return out;
}

/** Decodes System.AssignedTo / System.CreatedBy (an object or a "Name <email>" string). */
function identityField(raw: unknown): Identity {
  if (raw === undefined || raw === null) return {};
  if (typeof raw === "string") {
    const i = raw.lastIndexOf("<");
    if (i >= 0 && raw.endsWith(">")) return { displayName: raw.slice(0, i).trim(), uniqueName: raw.slice(i + 1, -1) };
    return { uniqueName: raw, displayName: raw };
  }
  if (typeof raw === "object") return raw as Identity;
  return {};
}

const str = (v: unknown): string => (typeof v === "string" ? v : "");

function splitTags(s: string): string[] {
  return s
    .split(";")
    .map((t) => t.trim())
    .filter((t) => t !== "");
}

function stripHtml(s: string): string {
  const t = s.replaceAll("<br>", "\n").replaceAll("<br/>", "\n").replaceAll("</p>", "\n").replaceAll("</div>", "\n");
  return t
    .replace(/<[^>]+>/g, "")
    .replaceAll("&nbsp;", " ")
    .replaceAll("&amp;", "&")
    .replaceAll("&lt;", "<")
    .replaceAll("&gt;", ">")
    .replaceAll("&quot;", '"')
    .replaceAll("&#39;", "'")
    .trim();
}

export interface NormalizeOptions {
  now?: () => Date;
}

/** Maps a service-hook payload to events for the team of the integration record. */
export function normalize(rec: IntegrationRecord, body: string, opts: NormalizeOptions = {}): PluginEvent[] {
  const parsed: unknown = JSON.parse(body);
  const p = (parsed ?? {}) as HookPayload;
  const now = opts.now ?? (() => new Date());
  const factories = parsePairs(rec.config["factory_identities"] ?? "");
  const service = new Set(
    (rec.config["service_identities"] ?? "")
      .split(",")
      .map((s) => s.trim().toLowerCase())
      .filter((s) => s !== ""),
  );
  const isSelf = (id: string): boolean => {
    const x = id.toLowerCase();
    return x !== "" && (factories.has(x) || service.has(x));
  };
  const out: PluginEvent[] = [];
  const pid = p.id ?? "";

  const emit = (
    type: string,
    actor: Identity,
    source: SourceRef,
    text: string,
    attrs: Fields,
    dedupeKey = "",
  ): void => {
    const self = isSelf(actor.id ?? "");
    const a: Fields = { [A.ActorIsBot]: self, [A.ActorIsSelf]: self, ...attrs };
    if (text !== "") a[A.Text] = text;
    let did = `${pid}:${type}`;
    let akey = "";
    // The events of one delivery share the action key.
    if (pid !== "") akey = `${PROVIDER}:${pid}`;
    if (dedupeKey !== "") {
      // The key names the fact (a merge), which several deliveries can report, so it is also the action key.
      did = dedupeKey;
      akey = `${PROVIDER}:${dedupeKey}`;
    }
    const ev: PluginEvent = {
      id: newId("evt"),
      teamId: rec.teamId,
      provider: PROVIDER,
      type,
      deliveryId: did,
      actor: {
        provider: PROVIDER,
        externalId: (actor.id ?? "").toLowerCase(),
        handle: actor.uniqueName ?? "",
        isBot: self,
      },
      source,
      attributes: a,
      payload: parsed,
      text,
      receivedAt: now().toISOString(),
    };
    if (akey !== "") ev.actionKey = akey;
    out.push(ev);
  };

  const mentionAttrs = (base: Fields, mentioned: string[]): Fields => {
    const a: Fields = { ...base, [A.Mentioned]: mentioned };
    for (const m of mentioned) {
      const alias = factories.get(m);
      if (alias) {
        a[A.Alias] = alias;
        break;
      }
    }
    return a;
  };

  const prSource = (pr: HookPR): { src: SourceRef; attrs: Fields } => {
    const web = pr.repository?.webUrl ?? "";
    const src: SourceRef = {
      provider: PROVIDER,
      kind: "pull_request",
      url: web !== "" ? `${web}/pullrequest/${pr.pullRequestId}` : "",
      repo: `${pr.repository?.project?.name ?? ""}/${pr.repository?.name ?? ""}`,
      number: pr.pullRequestId,
      title: pr.title ?? "",
    };
    let sourceRepo = pr.repository?.id ?? "";
    if (pr.forkSource?.repository?.id) sourceRepo = pr.forkSource.repository.id;
    const attrs: Fields = {
      [A.Repo]: (pr.repository?.id ?? "").toLowerCase(),
      [A.SourceRepo]: sourceRepo.toLowerCase(),
      [A.BaseBranch]: (pr.targetRefName ?? "").replace(/^refs\/heads\//, ""),
      [A.HeadBranch]: (pr.sourceRefName ?? "").replace(/^refs\/heads\//, ""),
      [A.PRNumber]: pr.pullRequestId,
      [A.Author]: pr.createdBy?.uniqueName ?? "",
      [A.Draft]: pr.isDraft ?? false,
      [A.Labels]: (pr.labels ?? []).map((l) => l.name),
    };
    if (pr.lastMergeSourceCommit?.commitId !== undefined) attrs[A.HeadSHA] = pr.lastMergeSourceCommit.commitId;
    return { src, attrs };
  };

  const workItemComment = (actor: Identity, src: SourceRef, base: Fields, html: string): void => {
    if (html === "") return;
    const text = stripHtml(html);
    emit("work_item_commented", actor, src, text, base);
    const m = extractMentions(html);
    if (m.length > 0 && !isSelf(actor.id ?? "")) emit("work_item_mentioned", actor, src, text, mentionAttrs({}, m));
  };

  const resource = (p.resource ?? {}) as Record<string, unknown>;
  switch (p.eventType) {
    case "git.pullrequest.created":
    case "git.pullrequest.updated":
    case "git.pullrequest.merged": {
      const pr = resource as unknown as HookPR;
      const { src, attrs } = prSource(pr);
      const creator = pr.createdBy ?? {};
      let text = pr.title ?? "";
      if (pr.description) text += "\n\n" + pr.description;
      const mergedKey = `${(pr.repository?.id ?? "").toLowerCase()}/${pr.pullRequestId}:merged`;
      if (p.eventType === "git.pullrequest.created") {
        emit("pull_request_created", creator, src, text, attrs);
        const m = extractMentions(pr.description ?? "");
        if (m.length > 0) emit("pull_request_mentioned", creator, src, text, mentionAttrs(attrs, m));
      } else if (pr.status === "completed") {
        emit("pull_request_merged", creator, src, text, attrs, mergedKey);
      } else if (p.eventType === "git.pullrequest.merged") {
        // A merge was tried but the pull request is not completed yet.
      } else if (pr.status === "abandoned") {
        emit("pull_request_closed", creator, src, text, attrs);
      } else {
        emit("pull_request_updated", creator, src, text, attrs);
      }
      break;
    }
    case "ms.vss-code.git-pullrequest-comment-event": {
      const res = resource as {
        comment?: { id?: number; content?: string; author?: Identity; _links?: { threads?: { href?: string } } };
        pullRequest?: HookPR;
      };
      const { src, attrs } = prSource(res.pullRequest ?? { pullRequestId: 0 });
      // A reply goes into the comment thread.
      const href = (res.comment?._links?.threads?.href ?? "").replace(/\/+$/, "");
      const last = href.slice(href.lastIndexOf("/") + 1);
      if (href !== "" && /^[+-]?\d+$/.test(last)) {
        src.threadRef = last;
        attrs[A.ThreadRef] = last;
      }
      const author = res.comment?.author ?? {};
      const content = res.comment?.content ?? "";
      emit("pull_request_commented", author, src, content, attrs);
      const m = extractMentions(content);
      if (m.length > 0 && !isSelf(author.id ?? "")) emit("pull_request_mentioned", author, src, content, mentionAttrs(attrs, m));
      break;
    }
    case "workitem.created":
    case "workitem.updated":
    case "workitem.commented": {
      const res = resource as {
        id?: number;
        workItemId?: number;
        fields?: Fields;
        revisedBy?: Identity;
        revision?: { id?: number; fields?: Fields };
        _links?: { html?: { href?: string } };
      };
      let id = res.id ?? 0;
      let fields: Fields = res.fields ?? {};
      let changes: Fields = {};
      if (p.eventType === "workitem.updated") {
        id = res.workItemId ?? 0;
        changes = res.fields ?? {};
        fields = res.revision?.fields ?? {};
      }
      const project = str(fields["System.TeamProject"]);
      const title = str(fields["System.Title"]);
      const src: SourceRef = {
        provider: PROVIDER,
        kind: "work_item",
        url: res._links?.html?.href ?? "",
        repo: project,
        number: id,
        externalId: String(id),
        title,
      };
      const assignee = identityField(fields["System.AssignedTo"]);
      const creator = identityField(fields["System.CreatedBy"]);
      const tags = splitTags(str(fields["System.Tags"]));
      const a: Fields = {
        [A.WorkItemType]: str(fields["System.WorkItemType"]),
        [A.Labels]: tags,
        [A.Assignees]: stringSet(assignee.uniqueName ?? ""),
        [A.Author]: creator.uniqueName ?? "",
      };
      const desc = str(fields["System.Description"]);
      const text = (title + "\n\n" + stripHtml(desc)).trim();
      if (p.eventType === "workitem.created") {
        emit("work_item_created", creator, src, text, { ...a, [A.AddedLabels]: tags });
        const m = extractMentions(desc);
        if (m.length > 0) emit("work_item_mentioned", creator, src, text, mentionAttrs({}, m));
      } else if (p.eventType === "workitem.updated") {
        const actor = res.revisedBy ?? {};
        const asg = changes["System.AssignedTo"] as { newValue?: unknown } | undefined;
        if (asg !== undefined) {
          const nv = identityField(asg.newValue);
          if ((nv.uniqueName ?? "") !== "") {
            const aa: Fields = { ...a, [A.Assignees]: [nv.uniqueName] };
            // Assigning a factory identity routes the work item to its factory.
            const alias = factories.get((nv.id ?? "").toLowerCase());
            if (alias) aa[A.Alias] = alias;
            emit("work_item_assigned", actor, src, text, aa);
          }
        }
        const tg = changes["System.Tags"] as { oldValue?: unknown; newValue?: unknown } | undefined;
        if (tg !== undefined) {
          const added = difference(splitTags(str(tg.newValue)), splitTags(str(tg.oldValue)));
          if (added.length > 0) {
            emit("work_item_labeled", actor, src, text, { ...a, [A.AddedLabels]: added, [A.Labels]: splitTags(str(tg.newValue)) });
          }
        }
        const hist = changes["System.History"] as { newValue?: unknown } | undefined;
        if (hist !== undefined) workItemComment(actor, src, a, str(hist.newValue));
      } else {
        workItemComment(res.revisedBy ?? {}, src, a, str(fields["System.History"]));
      }
      break;
    }
    case "git.push": {
      const res = resource as {
        pushedBy?: Identity;
        repository?: { id?: string; name?: string; project?: { name?: string } };
        refUpdates?: Array<{ name?: string; newObjectId?: string }>;
      };
      for (const ru of res.refUpdates ?? []) {
        const name = ru.name ?? "";
        const oid = ru.newObjectId ?? "";
        if (!name.startsWith("refs/heads/") || /^0*$/.test(oid)) continue;
        const branch = name.slice("refs/heads/".length);
        const rid = res.repository?.id ?? "";
        emit(
          "push",
          res.pushedBy ?? {},
          {
            provider: PROVIDER,
            kind: "push",
            repo: `${res.repository?.project?.name ?? ""}/${res.repository?.name ?? ""}`,
            externalId: `${rid}@${oid}`,
            title: branch,
          },
          "",
          { [A.Repo]: rid.toLowerCase(), [A.Branch]: branch, [A.HeadSHA]: oid },
          `${pid}:push:${branch}`,
        );
      }
      break;
    }
    default:
  }
  return out;
}

/** Parses "Authorization: Basic ..." into user and password. */
export function basicAuth(req: Request): { user: string; pass: string } | undefined {
  const h = req.headers.get("Authorization") ?? "";
  if (!/^basic /i.test(h)) return undefined;
  let decoded: string;
  try {
    const bin = atob(h.slice(6).trim());
    decoded = new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
  } catch {
    return undefined;
  }
  const i = decoded.indexOf(":");
  if (i < 0) return undefined;
  return { user: decoded.slice(0, i), pass: decoded.slice(i + 1) };
}

/**
 * Sets PluginEvent.factoryId:
 *  1. a factory identity alias (mention, or work item assignment) routes to that factory;
 *  2. otherwise a work item event goes to the factory of the integration record when it has one;
 *  3. otherwise to the only team factory whose repositories are in the project of the work
 *     item. With several such factories factoryId stays empty and the engine offers the event
 *     to each of them.
 * Pull request and push events carry a repository and the engine routes them.
 */
export async function routeFactories(host: HostApi, integ: IntegrationRecord, events: PluginEvent[]): Promise<void> {
  const needs = events.some(
    (ev) => (typeof ev.attributes?.[A.Alias] === "string" && ev.attributes[A.Alias] !== "") || ev.source?.kind === "work_item",
  );
  if (!needs) return;
  const factories = await host.listFactories();
  const matchesByProject = new Map<string, string[]>();
  for (const ev of events) {
    const team = factories.filter((f) => f.teamId === ev.teamId);
    const alias = ev.attributes?.[A.Alias];
    if (typeof alias === "string" && alias !== "") {
      const f = team.find((x) => x.alias.toLowerCase() === alias.toLowerCase());
      if (f) {
        ev.factoryId = f.id;
        continue;
      }
    }
    if (ev.source?.kind !== "work_item") continue;
    if (integ.factoryId) {
      const f = team.find((x) => x.id === integ.factoryId);
      if (f) {
        ev.factoryId = f.id;
        continue;
      }
    }
    const project = ev.source.repo ?? "";
    if (project === "") continue;
    let match = matchesByProject.get(project);
    if (!match) {
      match = [];
      for (const f of team) {
        let repos: RepoRef[];
        try {
          repos = await host.getFactoryRepos(f.id);
        } catch {
          continue;
        }
        if (repos.some((r) => projectCovers(r, project))) match.push(f.id);
      }
      matchesByProject.set(project, match);
    }
    if (match.length === 1) ev.factoryId = match[0]!;
  }
}

/** POST /plugins/azuredevops/webhook. The sender gets `status`, the events go to IngressDO. */
export async function handleWebhook(req: Request, host: HostApi, opts: NormalizeOptions = {}): Promise<WebhookResult> {
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
  let p: HookPayload;
  try {
    p = (JSON.parse(body) ?? {}) as HookPayload;
  } catch {
    return reject(400, "invalid_payload", "payload is not valid JSON");
  }
  const org = orgFromBaseUrl(p.resourceContainers?.account?.baseUrl ?? "");
  const creds = basicAuth(req);
  let integ: IntegrationRecord;
  try {
    if (!creds) throw new Error("no service hook credentials");
    if (org === "") throw new Error("the delivery names no azure devops organization");
    const all = await host.listIntegrations(PROVIDER);
    integ = await intakeVerified(all, PROVIDER, [org], async (r) => {
      if ((r.config["hook_password_ref"] ?? "") === "") return false;
      let sec: string;
      try {
        sec = await integrationSecret(host, r, "hook_password_ref");
      } catch (e) {
        await host.log("warn", "load service hook password", { integration: r.id, err: (e as Error).message });
        return false;
      }
      const wantUser = r.config["hook_username"] ?? "";
      return constantTimeEqual(sec, creds.pass) && (wantUser === "" || constantTimeEqual(wantUser, creds.user));
    });
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    if (e instanceof DeliveryAmbiguousError) {
      await host.log("warn", "delivery rejected", { org, err: msg });
      return reject(409, "ambiguous_integration", msg);
    }
    await host.log("info", "delivery not verified", { org, err: msg });
    return reject(401, "unauthorized", "invalid service hook credentials", {
      "WWW-Authenticate": 'Basic realm="openfactory"',
    });
  }
  let events: PluginEvent[];
  try {
    events = normalize(integ, body, opts);
  } catch (e) {
    return reject(400, "invalid_payload", (e as Error).message);
  }
  await routeFactories(host, integ, events);
  return { ...jsonResult(202, { status: "accepted", events: events.length }), events };
}
