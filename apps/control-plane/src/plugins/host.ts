// The HostApi that the control plane passes to plugin calls that need
// callbacks (replaces host.proto and internal/plugin/hostservice.go).
//
// A host object is scoped to one plugin and, for calls made for a team, to
// that team. Secrets and integration records are limited to the providers the
// plugin serves, and a secret is readable only when an integration record of
// the plugin in the team names it.

import { RpcTarget } from "cloudflare:workers";
import { OfError } from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import type { HostApi, IntegrationRecord, PluginInfo, RepoRef } from "@openfactory/plugin-api";
import { integrationToRecord } from "./mappers";

export interface HostFactory {
  id: string;
  teamId: string;
  alias: string;
  name: string;
}

/** What the host needs from the rest of the control plane. Wired in src/index.ts. */
export interface PluginHostBackend {
  listTeams(): Promise<Array<{ id: string; name: string }>>;
  /** Integration records of one team, or of every team when teamId is empty. */
  listIntegrations(teamId: string): Promise<Integration[]>;
  /** The decrypted team secret. Throws resource_not_found when there is none. */
  getSecret(teamId: string, factoryId: string, name: string): Promise<string>;
  listFactories(teamId: string): Promise<HostFactory[]>;
  getFactory(factoryId: string): Promise<HostFactory>;
  getFactoryRepos(factoryId: string): Promise<RepoRef[]>;
}

export type LogFn = (
  level: "debug" | "info" | "warn" | "error",
  message: string,
  fields: Record<string, unknown>,
) => void;

export interface PluginHostScope {
  /** The plugin the host serves. Its providers limit what it may read. */
  plugin: Pick<PluginInfo, "name" | "forgeProviders" | "responderProviders" | "integrationProviders">;
  /** The team of the call. Empty for webhooks, which have no team yet. */
  teamId?: string;
  log?: LogFn;
}

const PLACEHOLDER = /\{\{\s*([A-Za-z_][A-Za-z0-9_./-]*)\s*\}\}/g;

const defaultLog: LogFn = (level, message, fields) => {
  console[level](JSON.stringify({ message, ...fields }));
};

/** Providers (lower case) that a plugin may read integrations and secrets of. */
export function pluginProviders(p: PluginHostScope["plugin"]): Set<string> {
  const s = new Set<string>();
  for (const x of [...(p.integrationProviders ?? []), ...(p.forgeProviders ?? []), ...(p.responderProviders ?? [])]) {
    s.add(x.toLowerCase());
  }
  return s;
}

export class PluginHost extends RpcTarget implements HostApi {
  readonly #backend: PluginHostBackend;
  readonly #scope: PluginHostScope;
  readonly #providers: Set<string>;
  readonly #log: LogFn;

  constructor(backend: PluginHostBackend, scope: PluginHostScope) {
    super();
    this.#backend = backend;
    this.#scope = scope;
    this.#providers = pluginProviders(scope.plugin);
    this.#log = scope.log ?? defaultLog;
  }

  async log(
    level: "debug" | "info" | "warn" | "error",
    message: string,
    attrs?: Record<string, string>,
  ): Promise<void> {
    const lv = (["debug", "info", "warn", "error"] as const).includes(level) ? level : "info";
    // The attributes cannot replace the fixed fields.
    this.#log(lv, message, { ...(attrs ?? {}), plugin: this.#scope.plugin.name });
  }

  async listTeams(): Promise<Array<{ id: string; name: string }>> {
    const teams = await this.#backend.listTeams();
    return this.#scope.teamId ? teams.filter((t) => t.id === this.#scope.teamId) : teams;
  }

  async listIntegrations(provider?: string): Promise<IntegrationRecord[]> {
    return (await this.#integrations(this.#scope.teamId ?? "", provider)).map(integrationToRecord);
  }

  async #integrations(teamId: string, provider?: string): Promise<Integration[]> {
    const want = provider?.toLowerCase() ?? "";
    if (want !== "" && !this.#providers.has(want)) {
      throw new OfError("not_authorized", `plugin ${this.#scope.plugin.name} may not read "${want}" integrations`);
    }
    const all = await this.#backend.listIntegrations(teamId);
    return all.filter((i) => {
      const p = i.provider.toLowerCase();
      return this.#providers.has(p) && (want === "" || p === want);
    });
  }

  /** The team of a secret scope: "team:<id>" or a factory ID. */
  async #teamOfScope(scope: string): Promise<string> {
    if (scope.startsWith("team:")) return scope.slice("team:".length);
    if (scope === "") return this.#scope.teamId ?? "";
    return (await this.#backend.getFactory(scope)).teamId;
  }

  async #secret(scope: string, name: string): Promise<string> {
    const teamId = await this.#teamOfScope(scope);
    if (teamId === "" || name === "") {
      throw new OfError("not_authorized", `plugin ${this.#scope.plugin.name} may not read secret "${name}": no team`);
    }
    if (this.#scope.teamId && this.#scope.teamId !== teamId) {
      throw new OfError("not_authorized", `plugin ${this.#scope.plugin.name} may not read secrets of team ${teamId}`);
    }
    const ints = await this.#integrations(teamId);
    const allowed = ints.some(
      (i) => i.secret_ref === name || Object.entries(i.config ?? {}).some(([k, v]) => k.endsWith("_ref") && v === name),
    );
    if (!allowed) {
      throw new OfError(
        "not_authorized",
        `plugin ${this.#scope.plugin.name} may not read secret "${name}" of team ${teamId}: no ${this.#scope.plugin.name} integration references it`,
      );
    }
    const factoryId = scope.startsWith("team:") ? "" : scope;
    return this.#backend.getSecret(teamId, factoryId, name);
  }

  async getSecret(scope: string, name: string): Promise<string> {
    return this.#secret(scope, name);
  }

  async expandSecrets(scope: string, text: string): Promise<string> {
    let out = "";
    let last = 0;
    for (const m of text.matchAll(PLACEHOLDER)) {
      out += text.slice(last, m.index) + (await this.#secret(scope, m[1]!));
      last = m.index + m[0].length;
    }
    return out + text.slice(last);
  }

  async listFactories(): Promise<HostFactory[]> {
    return this.#backend.listFactories(this.#scope.teamId ?? "");
  }

  async getFactory(factoryId: string): Promise<HostFactory> {
    const f = await this.#backend.getFactory(factoryId);
    if (this.#scope.teamId && f.teamId !== this.#scope.teamId) {
      throw new OfError("resource_not_found", `factory ${factoryId} not found`);
    }
    return f;
  }

  async getFactoryRepos(factoryId: string): Promise<RepoRef[]> {
    await this.getFactory(factoryId);
    return this.#backend.getFactoryRepos(factoryId);
  }
}
