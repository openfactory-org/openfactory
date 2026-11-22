// Jira name resolver: project names to keys, status names to IDs (port of jira/resolver.go).

import type { Integration } from "@openfactory/core";
import { ApiError } from "../http";
import { NameLists, addId, addName, sortIds, type NameMap } from "../namelist";
import type { IntegrationDeps, NameResolver } from "../types";
import { equalFold, integrationSecret } from "../util";
import type { ApiClient } from "../http";
import { PROVIDER, newApiClient } from "./jira";
import { integrationActive } from "@openfactory/core";

const LIST_PAGE_SIZE = 50;
const MAX_LIST_PAGES = 100;

export class JiraNameResolver implements NameResolver {
  private readonly lists: NameLists;
  constructor(private readonly deps: IntegrationDeps) {
    this.lists = new NameLists(deps.now ? () => deps.now!().getTime() : undefined);
  }

  async resolveNames(
    teamId: string,
    provider: string,
    kind: string,
    names: string[],
  ): Promise<Record<string, string[]>> {
    if (!equalFold(provider, PROVIDER)) throw new Error(`the Jira resolver cannot list ${provider} objects`);
    if (kind !== "projects" && kind !== "statuses") throw new Error(`the Jira resolver cannot resolve "${kind}"`);
    let all: Integration[];
    try {
      all = (await this.deps.store.listIntegrations(teamId)).filter((i) => equalFold(i.provider, PROVIDER));
    } catch (e) {
      throw new Error(`could not read the Jira integrations: ${(e as Error).message}`, { cause: e });
    }
    const ints = all.filter(integrationActive);
    if (ints.length === 0) throw new Error("the team has no active Jira integration");
    const keys = names.map((n) => n.trim().toLowerCase());
    const out: NameMap = {};
    let firstErr: Error | undefined;
    let ok = 0;
    for (const in_ of ints) {
      let byName: NameMap;
      try {
        byName = await this.lookup(in_, kind, keys);
      } catch (e) {
        firstErr ??= e as Error;
        continue;
      }
      ok++;
      names.forEach((n, i) => {
        for (const v of byName[keys[i]!] ?? []) addId(out, n, v);
      });
    }
    if (ok === 0) throw firstErr!;
    sortIds(out);
    return out;
  }

  private async lookup(in_: Integration, kind: string, keys: string[]): Promise<NameMap> {
    let api: ApiClient;
    try {
      api = newApiClient(in_, await integrationSecret(this.deps.secrets, in_), this.deps.fetch);
    } catch (e) {
      throw new Error(`Jira integration credential: ${(e as Error).message}`, { cause: e });
    }
    try {
      return await this.lists.lookup(`${in_.id}/${kind}`, keys, () =>
        kind === "projects" ? listProjects(api) : listStatuses(api),
      );
    } catch (e) {
      if (e instanceof ApiError && (e.status === 401 || e.status === 403)) {
        throw new Error(
          `Jira refused the credential of the integration, so the ${kind} cannot be listed: ${e.message}`,
          { cause: e },
        );
      }
      throw e;
    }
  }
}

async function listProjects(api: ApiClient): Promise<NameMap> {
  const out: NameMap = {};
  let start = 0;
  for (let page = 0; page < MAX_LIST_PAGES; page++) {
    const resp = await api.json<{ values?: { key: string; name: string }[]; isLast?: boolean }>(
      "GET",
      `/rest/api/3/project/search?startAt=${start}&maxResults=${LIST_PAGE_SIZE}`,
    );
    const values = resp?.values ?? [];
    for (const p of values) addName(out, p.name, p.key);
    if (resp?.isLast || values.length === 0) break;
    start += values.length;
  }
  return out;
}

async function listStatuses(api: ApiClient): Promise<NameMap> {
  const resp = (await api.json<{ id: string; name: string }[]>("GET", "/rest/api/3/status")) ?? [];
  const out: NameMap = {};
  for (const s of resp) addName(out, s.name, s.id);
  return out;
}
