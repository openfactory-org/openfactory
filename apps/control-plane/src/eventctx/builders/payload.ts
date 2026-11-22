// Builders that read context the payload already has: the issue of a Linear
// or Jira event and the review comment of a GitHub or GitLab event.
import type { Event } from "@openfactory/core";
import { adfText } from "../../integrations/jira/adf";
import { utf8 } from "../../lib/bytes";
import { indentJSON, type Builder, type File } from "../types";

interface Issue {
  key?: string;
  title?: string;
  description?: string;
  state?: string;
  labels?: string[];
  assignee?: string;
  project?: string;
  url?: string;
}

function issueFile(is: Issue): File[] {
  if (!is.key && !is.title && !is.description) return [];
  const clean: Issue = {};
  for (const [k, v] of Object.entries(is) as [keyof Issue, unknown][]) {
    if (v === undefined || v === "" || (Array.isArray(v) && v.length === 0)) continue;
    (clean as Record<string, unknown>)[k] = v;
  }
  return [{ path: "issue.json", data: indentJSON(clean) }];
}

type Named = { name?: string; displayName?: string } | null | undefined;
const named = (n: Named): string => (n ? n.name || n.displayName || "" : "");

/** The payload of an event as an object; null when it is not a JSON object. */
export function payloadObj(ev: Event): Record<string, unknown> | null {
  const p = ev.payload;
  if (p === undefined || p === null) return null;
  if (typeof p === "string") {
    try {
      const v = JSON.parse(p) as unknown;
      return typeof v === "object" && v !== null ? (v as Record<string, unknown>) : null;
    } catch {
      return null;
    }
  }
  return typeof p === "object" ? (p as Record<string, unknown>) : null;
}

function linearIssue(ev: Event): File[] {
  const p = payloadObj(ev) as { data?: Record<string, unknown> } | null;
  if (!p) return [];
  const d = (p.data ?? {}) as {
    identifier?: string;
    title?: string;
    description?: string;
    url?: string;
    state?: Named;
    assignee?: Named;
    project?: Named;
    labels?: { name?: string }[];
    issue?: { identifier?: string; title?: string; url?: string };
  };
  const is: Issue = {
    key: d.identifier ?? "",
    title: d.title ?? "",
    description: d.description ?? "",
    url: d.url ?? "",
    state: named(d.state),
    assignee: named(d.assignee),
    project: named(d.project),
  };
  if (d.issue && !is.key) {
    is.key = d.issue.identifier ?? "";
    is.title = d.issue.title ?? "";
    is.url = d.issue.url ?? "";
  }
  is.labels = (d.labels ?? []).map((l) => l.name ?? "");
  return issueFile(is);
}

function jiraIssue(ev: Event): File[] {
  const p = payloadObj(ev) as { issue?: { key?: string; fields?: Record<string, unknown> } | null } | null;
  if (!p?.issue) return [];
  const f = (p.issue.fields ?? {}) as {
    summary?: string;
    description?: unknown;
    status?: Named;
    assignee?: Named;
    project?: Named;
    labels?: string[];
  };
  const is: Issue = {
    key: p.issue.key ?? "",
    title: f.summary ?? "",
    state: named(f.status),
    assignee: named(f.assignee),
    project: named(f.project),
    labels: f.labels ?? [],
    url: ev.source.url ?? "",
  };
  if (typeof f.description === "string") is.description = f.description;
  else if (f.description !== undefined && f.description !== null) is.description = adfText(f.description);
  return issueFile(is);
}

interface ReviewComment {
  author?: string;
  body?: string;
  path?: string;
  line?: number;
  commit_id?: string;
  in_reply_to_id?: number;
  url?: string;
}

function reviewFiles(rc: ReviewComment, hunk: string): File[] {
  const clean: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(rc)) if (v !== undefined && v !== "" && v !== 0) clean[k] = v;
  const out: File[] = [{ path: "review_comment.json", data: indentJSON(clean) }];
  if (hunk.trim() !== "")
    out.push({ path: "diff.patch", data: utf8(`--- a/${rc.path ?? ""}\n+++ b/${rc.path ?? ""}\n${hunk}\n`) });
  return out;
}

function githubReviewComment(ev: Event): File[] {
  const p = payloadObj(ev) as { comment?: Record<string, unknown> | null } | null;
  const c = p?.comment as
    | {
        body?: string;
        path?: string;
        diff_hunk?: string;
        line?: number | null;
        commit_id?: string;
        in_reply_to_id?: number;
        html_url?: string;
        user?: { login?: string };
      }
    | null
    | undefined;
  if (!c || (!c.path && !c.diff_hunk)) return [];
  return reviewFiles(
    {
      author: c.user?.login ?? "",
      body: c.body ?? "",
      path: c.path ?? "",
      line: c.line ?? 0,
      commit_id: c.commit_id ?? "",
      in_reply_to_id: c.in_reply_to_id ?? 0,
      url: c.html_url ?? "",
    },
    c.diff_hunk ?? "",
  );
}

function gitlabNote(ev: Event): File[] {
  const p = payloadObj(ev) as {
    user?: { username?: string };
    object_attributes?: {
      note?: string;
      url?: string;
      commit_id?: string;
      position?: { new_path?: string; new_line?: number } | null;
    } | null;
  } | null;
  const a = p?.object_attributes;
  if (!a?.position) return [];
  return reviewFiles(
    {
      author: p?.user?.username ?? "",
      body: a.note ?? "",
      path: a.position.new_path ?? "",
      line: a.position.new_line ?? 0,
      commit_id: a.commit_id ?? "",
      url: a.url ?? "",
    },
    "",
  );
}

function payloadBuilder(provider: string, fn: (ev: Event) => File[]): Builder {
  return {
    provider: () => provider,
    eventContext: async (ev) => ({ files: ev.payload === undefined || ev.payload === null ? [] : fn(ev) }),
  };
}

/** Builders that read context the payload already has. */
export function defaultBuilders(): Builder[] {
  return [
    payloadBuilder("linear", linearIssue),
    payloadBuilder("jira", jiraIssue),
    payloadBuilder("github", githubReviewComment),
    payloadBuilder("gitlab", gitlabNote),
  ];
}
