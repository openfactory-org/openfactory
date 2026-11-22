// The issue of a link_issue call (internal/orchestration/linkissue.go): a
// tracker or forge URL, or a provider with a key (or a repo and a number).
import { OfError, sourceKey, type SourceRef } from "@openfactory/core";

export interface LinkIssueInput {
  url?: string;
  provider?: string;
  key?: string;
  repo?: string;
  number?: number;
}

const TRACKER_KEY_RE = /^[A-Za-z][A-Za-z0-9_]*-[0-9]+$/;

/** Reads the issue of a tracker or forge URL, or returns undefined. */
export function parseIssueUrl(raw: string): SourceRef | undefined {
  let u: URL;
  try {
    u = new URL(raw.trim());
  } catch {
    return undefined;
  }
  if (!u.hostname) return undefined;
  const parts = u.pathname.replace(/^\/+|\/+$/g, "").split("/");
  const host = u.hostname.toLowerCase();
  const src: SourceRef = { provider: "", kind: "issue", url: raw };
  for (let i = 0; i < parts.length; i++) {
    const p = parts[i]!;
    const next = parts[i + 1] ?? "";
    if (p === "issue" && host.includes("linear") && TRACKER_KEY_RE.test(next)) {
      return { ...src, provider: "linear", external_id: next.toUpperCase() };
    }
    if (p === "browse" && TRACKER_KEY_RE.test(next)) {
      return { ...src, provider: "jira", external_id: next.toUpperCase() };
    }
    if (p === "issues" && i >= 2) {
      const n = positiveInt(next);
      if (!n) continue;
      let end = i;
      let provider = "github";
      if (parts[end - 1] === "-") {
        end--;
        provider = "gitlab";
      }
      if (end < 2) return undefined;
      return { ...src, provider, repo: parts.slice(0, end).join("/"), number: n };
    }
    if (p === "_workitems" && next === "edit" && i + 2 < parts.length && i >= 1) {
      const n = positiveInt(parts[i + 2]!);
      if (!n) continue;
      return { ...src, provider: "azure_devops", kind: "work_item", repo: parts[i - 1]!, number: n };
    }
  }
  const q = u.searchParams.get("selectedIssue") ?? "";
  if (TRACKER_KEY_RE.test(q)) return { ...src, provider: "jira", external_id: q.toUpperCase() };
  return undefined;
}

function positiveInt(s: string): number {
  if (!/^[0-9]+$/.test(s)) return 0;
  const n = Number(s);
  return n > 0 ? n : 0;
}

/** issueRef: the issue reference of a link_issue call. Throws invalid_request. */
export function issueRef(input: LinkIssueInput): SourceRef {
  const url = input.url ?? "";
  const key = (input.key ?? "").trim();
  const num = typeof input.number === "number" ? input.number : 0;
  const parsed = url ? parseIssueUrl(url) : undefined;
  if (url && !parsed && !key && !num) {
    throw new OfError(
      "invalid_request",
      `cannot read an issue from url ${JSON.stringify(url)}; give provider and key (or repo and number)`,
    );
  }
  const src: SourceRef = parsed ?? { provider: "", kind: "issue", ...(url ? { url } : {}) };
  if (input.provider) src.provider = input.provider.trim().replaceAll("-", "_").toLowerCase();
  if (key) src.external_id = key;
  if (input.repo) src.repo = input.repo;
  if (num) src.number = num;
  if (src.provider === "azure_devops" && src.number) src.kind = "work_item";
  if (!src.provider || !sourceKey(src)) {
    throw new OfError("invalid_request", "url, or provider with key (or repo and number), is required");
  }
  return src;
}
