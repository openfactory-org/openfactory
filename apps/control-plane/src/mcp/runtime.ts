// The MCP runtime of this isolate. Tests replace it with setMcpRuntime to
// route outbound fetches to fakes and to move the clock.
import type { Env } from "../contracts/env";
import { defaultRuntime, type McpRuntime } from "./team";

let override: ((env: Env) => McpRuntime) | undefined;
const cache = new WeakMap<object, McpRuntime>();

export function mcpRuntime(env: Env): McpRuntime {
  if (override) return override(env);
  let rt = cache.get(env);
  if (!rt) {
    rt = defaultRuntime(env);
    cache.set(env, rt);
  }
  return rt;
}

/** Test hook: replaces the runtime (undefined restores the default). */
export function setMcpRuntime(fn: ((env: Env) => McpRuntime) | undefined): void {
  override = fn;
}
