// Extension points of the control plane. A custom build imports
// @openfactory/control-plane and adds to these registries before it exports
// the Worker.
//
// Server routes outside /api/v1 (/runner/v1, /mcp, /webhooks, /integrations,
// /plugins, ...) match here by path prefix. bootstrapControlPlane()
// (src/wiring.ts) registers the built-in routes and hooks; a custom build adds
// its own. This module has no other imports, so the builder bundle stays small.
import type { Env } from "./contracts/env";

export type ServerRouteHandler = (
  request: Request,
  env: Env,
  ctx: ExecutionContext,
  url: URL,
  requestId: string,
) => Promise<Response | null>;

interface ServerRoute {
  prefix: string;
  handler: ServerRouteHandler;
}

const serverRoutes: ServerRoute[] = [];

/** Registers a handler for paths that start with prefix. Longer prefixes win. Return null to pass. */
export function registerServerRoute(prefix: string, handler: ServerRouteHandler): void {
  serverRoutes.push({ prefix, handler });
  serverRoutes.sort((a, b) => b.prefix.length - a.prefix.length);
}

export function matchServerRoutes(path: string): ServerRouteHandler[] {
  return serverRoutes.filter((r) => path === r.prefix || path.startsWith(r.prefix)).map((r) => r.handler);
}

export type CronHook = (env: Env, ctx: ExecutionContext) => Promise<void>;
const cronHooks: CronHook[] = [];

/** Registers work for the 1/min Cron safety net. */
export function registerCronHook(h: CronHook): void {
  cronHooks.push(h);
}

export function cronHooksList(): readonly CronHook[] {
  return cronHooks;
}
