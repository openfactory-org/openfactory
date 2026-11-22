// GitHub webhook deliveries → core events. Port of internal/forge/github/events.go.
import { newId, type Event, type SourceRef } from "@openfactory/core";
import { EVENT_ATTR as A, EVENT_PROVIDER } from "@openfactory/core";
import { extractMentions, stringSet } from "@openfactory/core";

export const PROVIDER = EVENT_PROVIDER.GitHub;
/** Marks issues and pull requests routed to a factory: "factory:<alias>". */
export const ALIAS_LABEL_PREFIX = "factory:";

interface GhUser {
  id?: number;
  login?: string;
  type?: string;
}
interface GhLabel {
  name: string;
}
interface GhIssue {
  number: number;
  title?: string;
  body?: string | null;
  html_url?: string;
  state?: string;
  user?: GhUser;
  labels?: GhLabel[];
  assignees?: GhUser[];
  pull_request?: { url?: string } | null;
}
interface GhRef {
  ref?: string;
  sha?: string;
  repo?: { full_name?: string };
}
interface GhPR extends GhIssue {
  draft?: boolean;
  merged?: boolean;
  head?: GhRef;
  base?: GhRef;
  requested_reviewers?: GhUser[];
  additions?: number | null;
  deletions?: number | null;
  created_at?: string | null;
  merged_at?: string | null;
}
interface GhLinkedPR {
  number: number;
  head?: GhRef;
  base?: GhRef;
}

/** The fields of a GitHub delivery that the normalizer reads. */
export interface GhPayload {
  action?: string;
  installation?: { id?: number } | null;
  repository?: { full_name?: string; html_url?: string; default_branch?: string };
  sender?: GhUser;
  issue?: GhIssue | null;
  pull_request?: GhPR | null;
  label?: GhLabel | null;
  assignee?: GhUser | null;
  comment?: { id: number; body?: string; html_url?: string; user?: GhUser; in_reply_to_id?: number } | null;
  review?: { id: number; body?: string | null; state?: string; html_url?: string; user?: GhUser } | null;
  requested_reviewer?: GhUser | null;
  requested_team?: { slug?: string } | null;
  changes?: Record<string, unknown> | null;
  ref?: string;
  before?: string;
  after?: string;
  deleted?: boolean;
  compare?: string;
  commits?: { added?: string[]; modified?: string[]; removed?: string[] }[] | null;
  head_commit?: { message?: string } | null;
  check_suite?: {
    id: number;
    head_branch?: string;
    head_sha?: string;
    conclusion?: string | null;
    pull_requests?: GhLinkedPR[];
    app?: { slug?: string } | null;
  } | null;
  check_run?: {
    id: number;
    name?: string;
    head_sha?: string;
    html_url?: string;
    check_suite?: { head_branch?: string };
    pull_requests?: GhLinkedPR[];
    app?: { slug?: string } | null;
  } | null;
  workflow_run?: {
    id: number;
    name?: string;
    head_branch?: string;
    head_sha?: string;
    conclusion?: string | null;
    html_url?: string;
    pull_requests?: GhLinkedPR[];
  } | null;
  sha?: string;
  state?: string;
  context?: string;
  description?: string | null;
  target_url?: string | null;
  branches?: { name: string }[] | null;
}

const labelNames = (ls: GhLabel[] | undefined): string[] => (ls ?? []).map((l) => l.name);
const logins = (us: GhUser[] | undefined): string[] => (us ?? []).map((u) => u.login ?? "");

/** Whether a GitHub account is a bot. */
export function isBotUser(u: GhUser | undefined): boolean {
  if (!u) return false;
  return (u.type ?? "").toLowerCase() === "bot" || (u.login ?? "").toLowerCase().endsWith("[bot]");
}

/** The alias of the first "factory:<alias>" label. */
export function aliasFromLabels(labels: string[]): string {
  for (const l of labels) {
    if (
      l.length > ALIAS_LABEL_PREFIX.length &&
      l.slice(0, ALIAS_LABEL_PREFIX.length).toLowerCase() === ALIAS_LABEL_PREFIX
    ) {
      return l.slice(ALIAS_LABEL_PREFIX.length).trim();
    }
  }
  return "";
}

function joinText(title: string, body: string): string {
  return body === "" ? title : title + "\n\n" + body;
}

function ciAttrs(branch: string, sha: string, prs: GhLinkedPR[] | undefined): Record<string, unknown> {
  const list = prs ?? [];
  const a: Record<string, unknown> = { [A.Branch]: branch, [A.HeadSHA]: sha, [A.LinkedPRCount]: list.length };
  if (list.length === 1) {
    a[A.PRNumber] = list[0]!.number;
    a[A.BaseBranch] = list[0]!.base?.ref ?? "";
  }
  return a;
}

export interface NormalizerOptions {
  /** The App slug: its bot login is "<slug>[bot]", "@<slug>" mentions route to factories. */
  appSlug?: string;
  now?: () => number;
}

/** Converts webhook deliveries into events. team_id, factory_id and actor.user_id are filled in later. */
export class Normalizer {
  constructor(private readonly opts: NormalizerOptions = {}) {}

  private isSelf(u: GhUser | undefined): boolean {
    const s = (this.opts.appSlug ?? "").toLowerCase();
    if (s === "" || !u) return false;
    const l = (u.login ?? "").toLowerCase();
    return l === s + "[bot]" || l === s;
  }

  private ownsApp(app: { slug?: string } | null | undefined): boolean {
    const s = this.opts.appSlug ?? "";
    if (s === "" || !app) return s === "";
    return (app.slug ?? "").toLowerCase() === s.toLowerCase();
  }

  /** Maps one delivery (X-GitHub-Event and the body) to zero or more events. */
  normalize(eventName: string, deliveryId: string, body: string): Event[] {
    let p: GhPayload;
    try {
      p = JSON.parse(body) as GhPayload;
    } catch (e) {
      throw new Error(`github: decode ${eventName} payload: ${(e as Error).message}`, { cause: e });
    }
    if (typeof p !== "object" || p === null) throw new Error(`github: decode ${eventName} payload: not an object`);
    const now = this.opts.now ?? Date.now;
    const repo = p.repository?.full_name ?? "";
    const sender = p.sender ?? {};
    const out: Event[] = [];
    const rawPayload = safeParse(body);
    const emit = (typ: string, src: SourceRef, text: string, attrs: Record<string, unknown>): void => {
      const a: Record<string, unknown> = {
        [A.Repo]: repo,
        [A.ActorIsBot]: isBotUser(sender),
        [A.ActorIsSelf]: this.isSelf(sender),
        ...attrs,
      };
      if (text !== "") a[A.Text] = text;
      let did = deliveryId;
      let akey = "";
      if (did !== "") {
        did = deliveryId + ":" + typ;
        akey = PROVIDER + ":" + deliveryId;
      }
      const ev: Event = {
        id: newId("evt"),
        team_id: "",
        provider: PROVIDER,
        type: typ,
        actor: {
          provider: PROVIDER,
          external_id: String(sender.id ?? 0),
          handle: sender.login ?? "",
          is_bot: isBotUser(sender),
          is_self: this.isSelf(sender),
        },
        source: src,
        attributes: a,
        payload: rawPayload,
        received_at: new Date(now()).toISOString(),
      };
      if (did) ev.delivery_id = did;
      if (akey) ev.action_key = akey;
      if (text) ev.text = text;
      out.push(ev);
    };
    const issueSource = (is: GhIssue): SourceRef => ({
      provider: PROVIDER,
      kind: is.pull_request ? "pull_request" : "issue",
      url: is.html_url ?? "",
      repo,
      number: is.number,
      title: is.title ?? "",
    });
    const issueAttrs = (is: GhIssue): Record<string, unknown> => {
      const labels = labelNames(is.labels);
      const a: Record<string, unknown> = {
        [A.Labels]: labels,
        [A.Assignees]: logins(is.assignees),
        [A.Author]: is.user?.login ?? "",
      };
      const alias = aliasFromLabels(labels);
      if (alias) a[A.Alias] = alias;
      return a;
    };
    const prAttrs = (pr: GhPR): Record<string, unknown> => {
      const a = issueAttrs(pr);
      a[A.BaseBranch] = pr.base?.ref ?? "";
      a[A.HeadBranch] = pr.head?.ref ?? "";
      a[A.HeadSHA] = pr.head?.sha ?? "";
      a[A.PRNumber] = pr.number;
      a[A.Draft] = !!pr.draft;
      if (typeof pr.additions === "number") a[A.Additions] = pr.additions;
      if (typeof pr.deletions === "number") a[A.Deletions] = pr.deletions;
      if (pr.created_at) a[A.PRCreatedAt] = pr.created_at;
      if (pr.merged_at) a[A.MergedAt] = pr.merged_at;
      if (pr.user?.login) {
        a[A.PROpener] = pr.user.login;
        a[A.PROpenerIsBot] = isBotUser(pr.user);
      }
      return a;
    };
    const prSource = (pr: GhPR): SourceRef => ({
      provider: PROVIDER,
      kind: "pull_request",
      url: pr.html_url ?? "",
      repo,
      number: pr.number,
      title: pr.title ?? "",
    });
    const mentionEvent = (typ: string, src: SourceRef, text: string, base: Record<string, unknown>): void => {
      const mentioned = extractMentions(text);
      if (mentioned.length === 0) return;
      emit(typ, src, text, { ...base, [A.Mentioned]: mentioned });
    };

    switch (eventName) {
      case "issues": {
        const is = p.issue;
        if (!is) return [];
        const src = issueSource(is);
        const text = joinText(is.title ?? "", is.body ?? "");
        const a = issueAttrs(is);
        switch (p.action) {
          case "opened":
            a[A.AddedLabels] = labelNames(is.labels);
            emit("issue_created", src, text, a);
            if ((is.labels ?? []).length > 0) emit("issue_labeled", src, text, { ...a });
            mentionEvent("issue_mentioned", src, is.body ?? "", a);
            break;
          case "labeled":
            if (p.label) a[A.AddedLabels] = [p.label.name];
            emit("issue_labeled", src, text, a);
            break;
          case "assigned":
            if (p.assignee) a[A.Assignees] = [p.assignee.login ?? ""];
            emit("issue_assigned", src, text, a);
            break;
        }
        break;
      }
      case "issue_comment": {
        const is = p.issue;
        const c = p.comment;
        if (!is || !c || p.action !== "created") return [];
        const src = issueSource(is);
        const a = issueAttrs(is);
        a["comment_url"] = c.html_url ?? "";
        a[A.Author] = c.user?.login ?? "";
        let kind = "issue";
        if (is.pull_request) {
          kind = "pull_request";
          a[A.PRNumber] = is.number;
        }
        emit(kind + "_commented", src, c.body ?? "", a);
        mentionEvent(kind + "_mentioned", src, c.body ?? "", a);
        break;
      }
      case "pull_request": {
        const pr = p.pull_request;
        if (!pr) return [];
        const src = prSource(pr);
        const text = joinText(pr.title ?? "", pr.body ?? "");
        const a = prAttrs(pr);
        switch (p.action) {
          case "opened":
            a[A.AddedLabels] = labelNames(pr.labels);
            emit("pull_request_opened", src, text, a);
            if (!pr.draft) emit("pull_request_ready", src, text, { ...a });
            mentionEvent("pull_request_mentioned", src, pr.body ?? "", a);
            break;
          case "ready_for_review":
            emit("pull_request_ready", src, text, a);
            break;
          case "reopened":
            emit("pull_request_reopened", src, text, a);
            break;
          case "synchronize":
            emit("pull_request_synchronized", src, text, a);
            break;
          case "labeled":
            if (p.label) a[A.AddedLabels] = [p.label.name];
            emit("pull_request_labeled", src, text, a);
            break;
          case "assigned":
            if (p.assignee) a[A.Assignees] = [p.assignee.login ?? ""];
            emit("pull_request_assigned", src, text, a);
            break;
          case "closed":
            a[A.Merged] = !!pr.merged;
            emit("pull_request_closed", src, text, a);
            if (pr.merged) emit("pull_request_merged", src, text, { ...a });
            break;
          case "enqueued":
            emit("pull_request_enqueued", src, text, a);
            break;
          case "dequeued":
            emit("pull_request_dequeued", src, text, a);
            break;
          case "review_requested":
            emit("pull_request_review_requested", src, text, {
              ...a,
              [A.Reviewers]: p.requested_reviewer ? [p.requested_reviewer.login ?? ""] : [],
              [A.ReviewerTeams]: p.requested_team ? [p.requested_team.slug ?? ""] : [],
            });
            break;
        }
        break;
      }
      case "pull_request_review": {
        const pr = p.pull_request;
        const r = p.review;
        if (!pr || !r || p.action !== "submitted") return [];
        const src = prSource(pr);
        const a = prAttrs(pr);
        a[A.ReviewState] = (r.state ?? "").toLowerCase();
        a[A.Author] = r.user?.login ?? "";
        a[A.Mentioned] = extractMentions(r.body ?? "");
        emit("pull_request_review_submitted", src, r.body ?? "", a);
        mentionEvent("pull_request_mentioned", src, r.body ?? "", a);
        break;
      }
      case "pull_request_review_comment": {
        const pr = p.pull_request;
        const c = p.comment;
        if (!pr || !c || p.action !== "created") return [];
        const src = prSource(pr);
        src.thread_ref = String(c.in_reply_to_id || c.id);
        const a = prAttrs(pr);
        a["comment_url"] = c.html_url ?? "";
        a[A.Author] = c.user?.login ?? "";
        a[A.ThreadRef] = src.thread_ref;
        emit("pull_request_commented", src, c.body ?? "", a);
        mentionEvent("pull_request_mentioned", src, c.body ?? "", a);
        break;
      }
      case "push": {
        const after = p.after ?? "";
        if (p.deleted || !(p.ref ?? "").startsWith("refs/heads/") || after.replace(/0/g, "") === "") return [];
        const branch = p.ref!.slice("refs/heads/".length);
        const paths: string[] = [];
        for (const c of p.commits ?? []) paths.push(...(c.added ?? []), ...(c.modified ?? []), ...(c.removed ?? []));
        const src: SourceRef = {
          provider: PROVIDER,
          kind: "push",
          url: p.compare ?? "",
          repo,
          external_id: repo + "@" + after,
          title: branch,
        };
        emit("push", src, p.head_commit?.message ?? "", {
          [A.Branch]: branch,
          [A.Paths]: stringSet(...paths),
          [A.HeadSHA]: after,
        });
        break;
      }
      case "check_suite": {
        const cs = p.check_suite;
        if (!cs) return [];
        const src: SourceRef = {
          provider: PROVIDER,
          kind: "check_suite",
          repo,
          external_id: `${repo}/check_suite/${cs.id}`,
          title: cs.head_branch ?? "",
        };
        const a = ciAttrs(cs.head_branch ?? "", cs.head_sha ?? "", cs.pull_requests);
        if (p.action === "completed") {
          a[A.Conclusion] = cs.conclusion ?? "";
          emit("check_suite_completed", src, "", a);
        } else if (p.action === "rerequested" && this.ownsApp(cs.app)) {
          emit("check_suite_rerequested", src, "", a);
        }
        break;
      }
      case "check_run": {
        const cr = p.check_run;
        if (!cr || p.action !== "rerequested" || !this.ownsApp(cr.app)) return [];
        const src: SourceRef = {
          provider: PROVIDER,
          kind: "check_run",
          url: cr.html_url ?? "",
          repo,
          external_id: `${repo}/check_run/${cr.id}`,
          title: cr.name ?? "",
        };
        const a = ciAttrs(cr.check_suite?.head_branch ?? "", cr.head_sha ?? "", cr.pull_requests);
        a["check_name"] = cr.name ?? "";
        emit("check_run_rerequested", src, "", a);
        break;
      }
      case "workflow_run": {
        const wr = p.workflow_run;
        if (!wr || p.action !== "completed") return [];
        const src: SourceRef = {
          provider: PROVIDER,
          kind: "workflow_run",
          url: wr.html_url ?? "",
          repo,
          external_id: `${repo}/workflow_run/${wr.id}`,
          title: wr.name ?? "",
        };
        const a = ciAttrs(wr.head_branch ?? "", wr.head_sha ?? "", wr.pull_requests);
        a[A.Conclusion] = wr.conclusion ?? "";
        a[A.Workflow] = wr.name ?? "";
        emit("workflow_run_completed", src, "", a);
        break;
      }
      case "status": {
        const state = (p.state ?? "").toLowerCase();
        if (state !== "success" && state !== "failure" && state !== "error") return [];
        const ctx = p.context ?? "";
        const src: SourceRef = {
          provider: PROVIDER,
          kind: "commit",
          url: p.target_url ?? "",
          repo,
          external_id: `${repo}@${p.sha ?? ""}/${ctx}`,
          title: ctx,
        };
        const branches = (p.branches ?? []).map((b) => b.name);
        emit("commit_status", src, p.description ?? "", {
          [A.CommitState]: state,
          [A.Context]: ctx,
          [A.HeadSHA]: p.sha ?? "",
          branches: branches.length > 0 ? branches : null,
        });
        break;
      }
    }
    return out;
  }
}

function safeParse(body: string): unknown {
  try {
    return JSON.parse(body) as unknown;
  } catch {
    return undefined;
  }
}

/** Reduces "https://github.com/acme/x.git", "github.com/acme/x" or "acme/x" to "acme/x". */
export function normalizeRepo(s: string): string {
  let t = s.trim().replace(/\.git$/, "");
  for (const p of ["https://", "http://", "ssh://", "git@"]) if (t.startsWith(p)) t = t.slice(p.length);
  t = t.replace(":", "/");
  let parts = t.replace(/^\/+|\/+$/g, "").split("/");
  if (parts.length >= 3 && parts[0]!.includes(".")) parts = parts.slice(1);
  if (parts.length >= 2) return (parts[parts.length - 2] + "/" + parts[parts.length - 1]).toLowerCase();
  return t.toLowerCase();
}

/** Base name of the factory-definition check. */
export const DEFINITION_CHECK_NAME = "openfactory/definition";

/** The definition check name for a definition stored at gitPath. */
export function checkName(gitPath: string): string {
  const p = gitPath.replace(/^\/+|\/+$/g, "");
  if (p === "" || p === ".") return DEFINITION_CHECK_NAME;
  return `${DEFINITION_CHECK_NAME} (${p})`;
}

const DEFINITION_ROOTS = [
  "factory.yaml",
  "factory.yml",
  "agents",
  "automations",
  "runners",
  "routers",
  "benchmarks",
  "scorers",
  "webhooks",
  "skills",
];

/** Whether a repository path belongs to a definition stored at gitPath. */
export function isDefinitionPath(gitPath: string, p: string): boolean {
  let dir = gitPath.replace(/^\/+|\/+$/g, "");
  if (dir === ".") dir = "";
  let rest = p;
  if (dir !== "") {
    if (!rest.startsWith(dir + "/")) return false;
    rest = rest.slice(dir.length + 1);
  }
  return DEFINITION_ROOTS.includes(rest.split("/")[0]!);
}
