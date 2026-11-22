// The shared part of the name resolvers: find the active integrations of the team, look the names up in
// each one, and merge the IDs. A subclass reads the name lists of its provider.
import { integrationActive } from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import { NameLists, addId, sortIds, type NameMap } from "./namelist";
import type { IntegrationDeps, NameResolver } from "./types";
import { equalFold } from "./util";

export interface NameResolverSpec {
  /** The provider name in error messages, for example "Slack". */
  label: string;
  /** The provider values that resolveNames accepts. */
  providers: readonly string[];
  /** The provider of the integration records to use. */
  integrationProvider: string;
  /** The kinds that resolveNames accepts. */
  kinds: readonly string[];
}

export abstract class BaseNameResolver implements NameResolver {
  protected readonly lists: NameLists;

  constructor(
    protected readonly deps: IntegrationDeps,
    private readonly spec: NameResolverSpec,
  ) {
    this.lists = new NameLists(deps.now ? () => deps.now!().getTime() : undefined);
  }

  async resolveNames(
    teamId: string,
    provider: string,
    kind: string,
    names: string[],
    scope: Record<string, string[]> = {},
  ): Promise<Record<string, string[]>> {
    const { label } = this.spec;
    if (!this.spec.providers.some((p) => equalFold(provider, p))) {
      throw new Error(`the ${label} resolver cannot list ${provider} objects`);
    }
    if (!this.spec.kinds.includes(kind)) throw new Error(`the ${label} resolver cannot resolve "${kind}"`);
    let all: Integration[];
    try {
      all = (await this.deps.store.listIntegrations(teamId)).filter((i) =>
        equalFold(i.provider, this.spec.integrationProvider),
      );
    } catch (e) {
      throw new Error(`could not read the ${label} integrations: ${(e as Error).message}`, { cause: e });
    }
    const ints = all.filter((i) => integrationActive(i) && this.usable(i, scope));
    if (ints.length === 0) throw new Error(this.noIntegrationMessage());
    const keys = names.map((n) => this.normalize(kind, n));
    const out: NameMap = {};
    let firstErr: Error | undefined;
    let ok = 0;
    for (const in_ of ints) {
      let byName: NameMap;
      try {
        byName = await this.lookup(in_, kind, keys, scope);
      } catch (e) {
        firstErr ??= e as Error;
        continue;
      }
      ok++;
      names.forEach((n, i) => {
        for (const id of byName[keys[i]!] ?? []) addId(out, n, id);
      });
    }
    if (ok === 0) throw firstErr!;
    sortIds(out);
    return out;
  }

  /** Reads the IDs of the normalized keys from one integration. */
  protected abstract lookup(
    in_: Integration,
    kind: string,
    keys: string[],
    scope: Record<string, string[]>,
  ): Promise<NameMap>;

  /** The lookup key of a name: trimmed and lower case. */
  protected normalize(_kind: string, name: string): string {
    return name.trim().toLowerCase();
  }

  /** Whether an active integration takes part in the lookup. */
  protected usable(_in: Integration, _scope: Record<string, string[]>): boolean {
    return true;
  }

  protected noIntegrationMessage(): string {
    return `the team has no active ${this.spec.label} integration`;
  }
}
