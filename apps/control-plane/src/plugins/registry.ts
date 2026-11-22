// Discovers the plugin Workers bound to the control plane and checks their
// version. A plugin is any service binding named PLUGIN_<NAME> whose entrypoint
// answers info() (wrangler.jsonc "services", entrypoint "Plugin").

import type { ForgePlugin, IngressPlugin, PluginInfo, PluginRole, ResponderPlugin, SandboxPlugin } from "@openfactory/plugin-api";
import { PLUGIN_API_VERSION, checkPluginInfo, isNotSupported } from "@openfactory/plugin-api";

/** The binding name prefix of plugin Workers. */
export const PLUGIN_BINDING_PREFIX = "PLUGIN_";

/** What a plugin binding can be called as. Which parts exist depends on info().roles. */
export type PluginStub = Partial<ForgePlugin & ResponderPlugin & IngressPlugin & SandboxPlugin> & {
  info(): Promise<PluginInfo>;
  ping(): Promise<void>;
};

export interface LoadedPlugin {
  /** Binding name, for example PLUGIN_GITLAB. */
  binding: string;
  info: PluginInfo;
  stub: PluginStub;
}

export interface RejectedPlugin {
  binding: string;
  /** Why the plugin is not used: unreachable, a version mismatch, a duplicate name. */
  reason: string;
}

export interface DiscoverOptions {
  /** Cache lifetime. Default 60 s; 0 turns the cache off. */
  ttlMs?: number;
  now?: () => number;
  /** Time limit of one info() call. Default 5 s. */
  timeoutMs?: number;
  log?: (message: string, fields: Record<string, unknown>) => void;
}

const DEFAULT_TTL_MS = 60_000;
const DEFAULT_TIMEOUT_MS = 5_000;

/** The result of discovery: the usable plugins and the rejected bindings. */
export class PluginRegistry {
  constructor(
    readonly plugins: readonly LoadedPlugin[],
    readonly rejected: readonly RejectedPlugin[],
  ) {}

  /** A plugin by its name (info().name). */
  get(name: string): LoadedPlugin | undefined {
    return this.plugins.find((p) => p.info.name === name);
  }

  withRole(role: PluginRole): LoadedPlugin[] {
    return this.plugins.filter((p) => p.info.roles.includes(role));
  }

  /** The plugin that serves a forge provider (for example "gitlab"). */
  forProvider(provider: string): LoadedPlugin | undefined {
    const want = provider.toLowerCase();
    return this.withRole("forge").find((p) => (p.info.forgeProviders ?? []).some((x) => x.toLowerCase() === want));
  }

  /**
   * The forge plugins in the shape of the forge resolver option `plugins`: keyed by
   * plugin name and by each forge provider they serve.
   */
  forgePlugins(): Record<string, ForgePlugin> {
    const out: Record<string, ForgePlugin> = {};
    for (const p of this.withRole("forge")) {
      const stub = p.stub as ForgePlugin;
      out[p.info.name] = stub;
      for (const prov of p.info.forgeProviders ?? []) {
        const k = prov.toLowerCase();
        if (!(k in out)) out[k] = stub;
      }
    }
    return out;
  }
}

function bindingPlugins(env: object): Array<[string, PluginStub]> {
  const out: Array<[string, PluginStub]> = [];
  for (const [k, v] of Object.entries(env)) {
    // Plain vars (strings) with the same prefix are not plugins.
    if (k.startsWith(PLUGIN_BINDING_PREFIX) && v !== null && (typeof v === "object" || typeof v === "function")) {
      out.push([k, v as PluginStub]);
    }
  }
  return out.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

function withTimeout<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const t = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what} timed out after ${ms} ms`)), ms);
  });
  return Promise.race([p, t]).finally(() => {
    if (timer !== undefined) clearTimeout(timer);
  });
}

/** Calls info() on each plugin binding and checks it against PLUGIN_API_VERSION. No cache. */
export async function loadPlugins(env: object, opts: DiscoverOptions = {}): Promise<PluginRegistry> {
  const log = opts.log ?? ((message, fields) => console.warn(JSON.stringify({ message, ...fields })));
  const timeout = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  const bindings = bindingPlugins(env);
  const infos = await Promise.all(
    bindings.map(async ([binding, stub]) => {
      try {
        return { binding, stub, info: await withTimeout(Promise.resolve(stub.info()), timeout, `${binding}.info()`) };
      } catch (e) {
        return { binding, stub, error: e instanceof Error ? e.message : String(e) };
      }
    }),
  );
  const plugins: LoadedPlugin[] = [];
  const rejected: RejectedPlugin[] = [];
  const names = new Set<string>();
  for (const r of infos) {
    if ("error" in r) {
      rejected.push({ binding: r.binding, reason: `unreachable: ${r.error}` });
      log("plugin binding unreachable", { binding: r.binding, error: r.error });
      continue;
    }
    const info: PluginInfo = {
      ...r.info,
      roles: Array.isArray(r.info.roles) ? [...r.info.roles] : [],
    };
    const check = checkPluginInfo(info, PLUGIN_API_VERSION);
    if (!check.ok) {
      rejected.push({ binding: r.binding, reason: check.reason });
      log("plugin rejected", { binding: r.binding, reason: check.reason });
      continue;
    }
    if (names.has(info.name)) {
      const reason = `a plugin named ${info.name} is already bound`;
      rejected.push({ binding: r.binding, reason });
      log("plugin rejected", { binding: r.binding, reason });
      continue;
    }
    names.add(info.name);
    plugins.push({ binding: r.binding, info, stub: r.stub });
  }
  return new PluginRegistry(plugins, rejected);
}

interface CacheEntry {
  at: number;
  value: Promise<PluginRegistry>;
}

const cache = new WeakMap<object, CacheEntry>();

/**
 * loadPlugins with a per-isolate cache keyed by the env object. A failed load is
 * not kept. Call forgetPlugins(env) after a deploy that changes bindings (tests).
 */
export function discoverPlugins(env: object, opts: DiscoverOptions = {}): Promise<PluginRegistry> {
  const now = (opts.now ?? Date.now)();
  const ttl = opts.ttlMs ?? DEFAULT_TTL_MS;
  const hit = cache.get(env);
  if (hit && ttl > 0 && now - hit.at < ttl) return hit.value;
  const value = loadPlugins(env, opts);
  cache.set(env, { at: now, value });
  value.catch(() => cache.delete(env));
  return value;
}

export function forgetPlugins(env: object): void {
  cache.delete(env);
}

/** True when an RPC failure means the plugin does not implement the optional method. */
export { isNotSupported };
