// Agents open pull requests and people merge them. The guard reads the MCP
// requests a run sends through the control plane (the MCP proxy and the
// built-in integration servers) and refuses each tools/call of a merge tool,
// unless the factory sets allowAgentMerge. Port of internal/mergeguard.
//
// The guard is a control on the MCP path only; it does not see what an agent
// does with a forge credential outside MCP.

/** Largest MCP request body the guard reads. */
export const MAX_BODY = 16 << 20;

export const REFUSAL_TEXT =
  "Refused: agents in this factory cannot merge pull requests or merge requests. " +
  "Open the pull request and hand it off; a person merges it. " +
  "To allow merges by agents, set allowAgentMerge: true in factory.yaml.";

export interface Policy {
  /** Whether the run of the request can merge. Undefined means never. */
  allow?: (req: Request) => boolean | Promise<boolean>;
  /** Called for each merge call with the decision. */
  record?: (req: Request, tool: string, allowed: boolean) => void;
}

interface RpcRequest {
  id?: unknown;
  hasId: boolean;
  method: string;
  name: string;
}

function toCall(v: unknown): RpcRequest {
  const o = (typeof v === "object" && v !== null && !Array.isArray(v) ? v : {}) as Record<string, unknown>;
  const params = (typeof o["params"] === "object" && o["params"] !== null ? o["params"] : {}) as Record<
    string,
    unknown
  >;
  return {
    id: o["id"],
    hasId: "id" in o,
    method: typeof o["method"] === "string" ? o["method"] : "",
    name: typeof params["name"] === "string" ? params["name"] : "",
  };
}

/** Reads one JSON-RPC message or a batch. A body that is not JSON-RPC gives no calls. */
export function parse(body: string): { calls: RpcRequest[]; batch: boolean } {
  const t = body.trim();
  if (t === "") return { calls: [], batch: false };
  const batch = t[0] === "[";
  let v: unknown;
  try {
    v = JSON.parse(t);
  } catch {
    return { calls: [], batch };
  }
  if (batch) return { calls: Array.isArray(v) ? v.map(toCall) : [], batch };
  if (typeof v !== "object" || v === null || Array.isArray(v)) return { calls: [], batch: false };
  return { calls: [toCall(v)], batch: false };
}

/**
 * Wraps an MCP handler: merge tool calls are refused, every other request goes
 * to next with the same body.
 */
export function wrap(next: (req: Request) => Promise<Response>, p: Policy): (req: Request) => Promise<Response> {
  return async (req) => {
    if (req.method !== "POST" || req.body === null) return next(req);
    if (Number(req.headers.get("content-length") ?? "0") > MAX_BODY)
      return new Response("the MCP request is too large\n", { status: 413 });
    let text: string;
    try {
      text = await req.text();
    } catch {
      return new Response("could not read the request\n", { status: 400 });
    }
    if (new TextEncoder().encode(text).length > MAX_BODY)
      return new Response("the MCP request is too large\n", { status: 413 });
    const fresh = new Request(req, { body: text });
    const { calls, batch } = parse(text);
    const merges = calls.filter((c) => c.method === "tools/call" && isMergeTool(c.name));
    if (merges.length === 0) return next(fresh);
    const allowed = p.allow ? await p.allow(fresh) : false;
    for (const c of merges) p.record?.(fresh, c.name, allowed);
    if (allowed) return next(fresh);
    return refuse(calls, batch);
  };
}

/** The refusal: a tool result with isError for each merge call; other calls of a batch get a JSON-RPC error. */
export function refuse(calls: RpcRequest[], batch: boolean): Response {
  const out: unknown[] = [];
  for (const c of calls) {
    if (!c.hasId) continue;
    if (c.method === "tools/call" && isMergeTool(c.name)) {
      out.push({
        jsonrpc: "2.0",
        id: c.id,
        result: { content: [{ type: "text", text: REFUSAL_TEXT }], isError: true },
      });
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
  const headers = { "content-type": "application/json", "x-of-merge-refused": "true" };
  if (out.length === 0) return new Response(null, { status: 202, headers });
  return new Response(JSON.stringify(batch ? out : out[0]) + "\n", { status: 200, headers });
}

/** Splits a tool name into lower-case words at non-letters, non-digits and lower-to-upper changes. */
export function words(name: string): string[] {
  const out: string[] = [];
  let cur = "";
  const rs = Array.from(name);
  const isAlnum = (r: string) => /^[\p{L}\p{N}]$/u.test(r);
  const isUpper = (r: string) => /^\p{Lu}$/u.test(r);
  const isLower = (r: string) => /^\p{Ll}$/u.test(r);
  const isDigit = (r: string) => /^\p{Nd}$/u.test(r);
  for (let i = 0; i < rs.length; i++) {
    const r = rs[i]!;
    if (isAlnum(r)) {
      if (isUpper(r) && i > 0 && (isLower(rs[i - 1]!) || isDigit(rs[i - 1]!)) && cur !== "") {
        out.push(cur);
        cur = "";
      }
      cur += r.toLowerCase();
    } else if (cur !== "") {
      out.push(cur);
      cur = "";
    }
  }
  if (cur !== "") out.push(cur);
  return out;
}

/**
 * Whether a tool name names a merge of a pull or merge request, or a change
 * that makes the forge merge later (auto-merge, a merge queue).
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
          // the noun "merge request"
        } else if (is(i + 1, "queue")) {
          if (is(i - 1, "add", "enqueue") || (is(i - 1, "to") && is(i - 2, "add")) || is(i + 2, "add", "enqueue"))
            return true;
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
