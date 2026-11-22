// Port of internal/mergeguard: the rule that agents open pull requests and
// people merge them. It reads the MCP requests that a run sends through the
// control plane (the MCP proxy and the built-in integration servers) and
// refuses each tools/call of a merge tool, unless the factory sets
// allowAgentMerge. The guard is a control on the MCP path only.
import { MAX_MCP_BODY } from "./jsonrpc";

/** The text of the tool result that an agent gets for a refused merge. */
export const REFUSAL_TEXT =
  "Refused: agents in this factory cannot merge pull requests or merge requests. " +
  "Open the pull request and hand it off; a person merges it. " +
  "To allow merges by agents, set allowAgentMerge: true in factory.yaml.";

export const MERGE_REFUSED_HEADER = "X-OF-Merge-Refused";

interface RpcCall {
  id?: unknown;
  method?: unknown;
  params?: { name?: unknown };
}

/** Decides about merge calls of one request. */
export interface MergePolicy {
  /** Whether the run of the request can merge. Undefined means never. */
  allow?: () => boolean | Promise<boolean>;
  /** Called for each merge call, with the decision. */
  record?: (tool: string, allowed: boolean) => void | Promise<void>;
}

/** One JSON-RPC message or a batch. A body that is not JSON-RPC gives no calls. */
function parse(text: string): { calls: RpcCall[]; batch: boolean } {
  const t = text.trim();
  if (t === "") return { calls: [], batch: false };
  try {
    const v = JSON.parse(t) as unknown;
    if (Array.isArray(v)) return { calls: v.filter((x) => typeof x === "object" && x !== null) as RpcCall[], batch: true };
    if (typeof v === "object" && v !== null) return { calls: [v as RpcCall], batch: false };
  } catch {
    /* not JSON */
  }
  return { calls: [], batch: t.startsWith("[") };
}

const toolName = (c: RpcCall): string => (typeof c.params?.name === "string" ? c.params.name : "");
const isMergeCall = (c: RpcCall): boolean => c.method === "tools/call" && isMergeTool(toolName(c));

/**
 * Checks one POST body. Returns null when the request may pass (the caller
 * forwards `body`), or the refusal Response.
 */
export async function guardMerge(text: string, p: MergePolicy): Promise<Response | null> {
  const { calls, batch } = parse(text);
  const merges = calls.filter(isMergeCall);
  if (merges.length === 0) return null;
  const allowed = p.allow ? await p.allow() : false;
  for (const c of merges) await p.record?.(toolName(c), allowed);
  if (allowed) return null;
  return refuse(calls, batch);
}

/**
 * Reads the body of `request` once and runs the guard. Returns the body
 * text for the next handler, or the refusal / size error Response.
 */
export async function readGuarded(request: Request, p: MergePolicy): Promise<{ text: string } | { response: Response }> {
  const text = await request.text();
  if (text.length > MAX_MCP_BODY) {
    return { response: new Response("the MCP request is too large\n", { status: 413 }) };
  }
  if (request.method !== "POST") return { text };
  const refused = await guardMerge(text, p);
  return refused ? { response: refused } : { text };
}

/**
 * Answers without calling the server. A merge call gets a tool result with
 * isError. In a batch, the other requests get a JSON-RPC error: the guard
 * does not split a batch.
 */
function refuse(calls: RpcCall[], batch: boolean): Response {
  const out: unknown[] = [];
  for (const c of calls) {
    if (c.id === undefined || c.id === null) continue; // a notification has no response
    if (isMergeCall(c)) {
      out.push({ jsonrpc: "2.0", id: c.id, result: { content: [{ type: "text", text: REFUSAL_TEXT }], isError: true } });
    } else {
      out.push({
        jsonrpc: "2.0",
        id: c.id,
        error: {
          code: -32600,
          message: "this batch has a merge call that the platform refused; send the other requests again without it",
        },
      });
    }
  }
  const headers = { "content-type": "application/json", [MERGE_REFUSED_HEADER]: "true" };
  if (out.length === 0) return new Response(null, { status: 202, headers });
  return new Response(JSON.stringify(batch ? out : out[0]) + "\n", { status: 200, headers });
}

/**
 * Whether an MCP tool name names a merge of a pull request or a merge
 * request, or a change that makes the forge merge later (auto-merge, a merge
 * queue). Names are compared as words: merge_pull_request, mergePullRequest,
 * forge.merge-pr and accept_merge_request match; get_merge_request does not.
 */
export function isMergeTool(name: string): boolean {
  const toks = words(name);
  const n = toks.length;
  if (n === 0) return false;
  const is = (i: number, ...set: string[]) => i >= 0 && i < n && set.includes(toks[i]!);
  const target = (i: number) =>
    is(i, "pr", "prs", "mr", "mrs", "pullrequest", "pullrequests", "branch", "branches", "into", "changes", "change") ||
    (is(i, "pull") && is(i + 1, "request", "requests")) ||
    (is(i, "merge") && is(i + 1, "request", "requests"));
  for (let i = 0; i < n; i++) {
    switch (toks[i]) {
      case "automerge":
        return true;
      case "auto":
        if (is(i + 1, "merge")) return true;
        break;
      case "enqueue":
        if (target(i + 1) || is(i + 1, "merge")) return true;
        break;
      case "merge":
        if (is(i + 1, "request", "requests")) {
          // The noun "merge request".
        } else if (is(i + 1, "queue")) {
          if (is(i - 1, "add", "enqueue") || (is(i - 1, "to") && is(i - 2, "add")) || is(i + 2, "add", "enqueue")) return true;
        } else if (i === n - 1) {
          return true;
        } else if (target(i + 1)) {
          return true;
        } else if (is(i + 1, "and", "squash", "rebase") || is(i - 1, "squash", "rebase", "approve", "and")) {
          return true;
        }
        break;
      case "accept":
      case "complete":
        if (target(i + 1)) return true;
        break;
    }
  }
  return false;
}

const LETTER_OR_DIGIT = /[\p{L}\p{N}]/u;
const UPPER = /\p{Lu}/u;
const LOWER = /\p{Ll}/u;
const DIGIT = /\p{N}/u;

/** Splits a tool name into lower-case words at non-letters, non-digits and lower-to-upper case changes. */
export function words(name: string): string[] {
  const out: string[] = [];
  let cur = "";
  const rs = [...name];
  rs.forEach((r, i) => {
    if (LETTER_OR_DIGIT.test(r)) {
      if (UPPER.test(r) && i > 0 && (LOWER.test(rs[i - 1]!) || DIGIT.test(rs[i - 1]!))) {
        if (cur) out.push(cur);
        cur = "";
      }
      cur += r.toLowerCase();
    } else {
      if (cur) out.push(cur);
      cur = "";
    }
  });
  if (cur) out.push(cur);
  return out;
}
