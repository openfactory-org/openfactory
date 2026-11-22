// The MCP servers of a run: managed servers resolved, {{SECRET}} references
// expanded, and the orchestration server of the control plane added.
import { invalid } from "@openfactory/core";
import type { Run } from "../../engine/types";
import { expandWith } from "../../secrets/expand";
import { wrap, type PlannerDeps } from "./deps";

export const ORCHESTRATION_SERVER = "factory";

export interface McpServer {
  managedId?: string;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  headers?: Record<string, string>;
}

async function expand(
  d: PlannerDeps,
  r: Run,
  m: Record<string, string> | undefined,
): Promise<Record<string, string> | undefined> {
  if (!m) return undefined;
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(m)) {
    out[k] = v.includes("{{")
      ? await expandWith(v, async (n) => (await d.secret(r.factory_id, n)) ?? (await d.secret("", n)))
      : v;
  }
  return out;
}

const withoutAuthorization = (h: Record<string, string> | undefined): Record<string, string> =>
  Object.fromEntries(Object.entries(h ?? {}).filter(([k]) => k.toLowerCase() !== "authorization"));

/** mcpServers: managedId resolved, {{SECRET}} expanded, the orchestration server injected. */
export async function mcpServers(d: PlannerDeps, r: Run): Promise<Record<string, McpServer>> {
  const out: Record<string, McpServer> = {};
  const base = d.sandboxUrl;
  for (const [name, s0] of Object.entries(r.config.mcp_servers ?? {})) {
    if (name === ORCHESTRATION_SERVER) continue;
    let s: McpServer = { ...s0 };
    if (s.managedId) {
      let got: McpServer;
      try {
        got = await d.resolveManagedMcp(r, s.managedId);
      } catch (e) {
        throw wrap(e, `mcp server ${JSON.stringify(name)}: managedId ${JSON.stringify(s.managedId)}`);
      }
      if (!got.url && !got.command)
        throw invalid(
          `mcp server ${JSON.stringify(name)}: managedId ${JSON.stringify(s.managedId)} has no url or command`,
        );
      let agentHeaders = s.headers;
      if (base && got.url?.startsWith(base + "/mcp/")) agentHeaders = withoutAuthorization(s.headers);
      const env = { ...(got.env ?? {}), ...(s.env ?? {}) };
      const headers = { ...(got.headers ?? {}), ...(agentHeaders ?? {}) };
      s = {
        ...got,
        ...(Object.keys(env).length ? { env } : {}),
        ...(Object.keys(headers).length ? { headers } : {}),
      };
      delete s.managedId;
      if (!Object.keys(env).length) delete s.env;
      if (!Object.keys(headers).length) delete s.headers;
    }
    if (s.args) s.args = [...s.args];
    try {
      const env = await expand(d, r, s.env);
      if (env) s.env = env;
    } catch (e) {
      throw wrap(e, `mcp server ${JSON.stringify(name)} env`);
    }
    try {
      const headers = await expand(d, r, s.headers);
      if (headers) s.headers = headers;
    } catch (e) {
      throw wrap(e, `mcp server ${JSON.stringify(name)} headers`);
    }
    out[name] = s;
  }
  if (base) out[ORCHESTRATION_SERVER] = { url: `${base}/mcp/run/${r.id}` };
  return out;
}

/** sessionTokenServers: control-plane servers lose their Authorization header. */
export function sessionTokenServers(base: string, mcp: Record<string, McpServer>): Set<string> {
  const out = new Set<string>();
  if (!base) return out;
  for (const [name, srv] of Object.entries(mcp)) {
    if (!srv.url?.startsWith(base + "/mcp/")) continue;
    srv.headers = withoutAuthorization(srv.headers);
    out.add(name);
  }
  return out;
}
