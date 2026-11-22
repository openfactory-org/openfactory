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
import { expandWith, placeholders } from "../secrets/expand";
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

  /**
   * Authorizes the plugin to read the named secrets of a scope and returns
   * where they live. The scope's team and the plugin's integrations are read
   * once for all names, and no secret is read before every name is allowed.
   */
  async #authorize(scope: string, names: string[]): Promise<{ teamId: string; factoryId: string }> {
    const teamId = await this.#teamOfScope(scope);
    const plugin = this.#scope.plugin.name;
    const empty = names.find((n) => n === "");
    if (teamId === "" || empty !== undefined) {
      throw new OfError("not_authorized", `plugin ${plugin} may not read secret "${empty ?? names[0]}": no team`);
    }
    if (this.#scope.teamId && this.#scope.teamId !== teamId) {
      throw new OfError("not_authorized", `plugin ${plugin} may not read secrets of team ${teamId}`);
    }
    const referenced = new Set<string>();
    for (const i of await this.#integrations(teamId)) {
      if (i.secret_ref) referenced.add(i.secret_ref);
      for (const [k, v] of Object.entries(i.config ?? {})) if (k.endsWith("_ref")) referenced.add(v);
    }
    const denied = names.find((n) => !referenced.has(n));
    if (denied !== undefined) {
      throw new OfError(
        "not_authorized",
        `plugin ${plugin} may not read secret "${denied}" of team ${teamId}: no ${plugin} integration references it`,
      );
    }
    return { teamId, factoryId: scope.startsWith("team:") ? "" : scope };
  }

  async getSecret(scope: string, name: string): Promise<string> {
    const { teamId, factoryId } = await this.#authorize(scope, [name]);
    return this.#backend.getSecret(teamId, factoryId, name);
  }

  /** Expands {{NAME}} placeholders (grammar of secrets/expand.ts). Each distinct name is read once. */
  async expandSecrets(scope: string, text: string): Promise<string> {
    const names = placeholders(text);
    if (names.length === 0) return text;
    const { teamId, factoryId } = await this.#authorize(scope, names);
    return expandWith(text, async (name) => {
      try {
        return await this.#backend.getSecret(teamId, factoryId, name);
      } catch (e) {
        // A missing secret is reported by expandWith together with the other missing names.
        if (e instanceof OfError && e.code === "resource_not_found") return undefined;
        throw e;
      }
    });
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
