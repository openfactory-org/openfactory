// Jira name resolver: project names to keys, status names to IDs (port of jira/resolver.go).
import type { Integration } from "@openfactory/core";
import { ApiError, type ApiClient } from "@openfactory/core";
import { addName, type NameMap } from "../namelist";
import { BaseNameResolver } from "../resolver";
import type { IntegrationDeps } from "../types";
import { integrationSecret } from "../util";
import { PROVIDER, newApiClient } from "./jira";

const LIST_PAGE_SIZE = 50;
const MAX_LIST_PAGES = 100;

export class JiraNameResolver extends BaseNameResolver {
  constructor(deps: IntegrationDeps) {
    super(deps, {
      label: "Jira",
      providers: [PROVIDER],
      integrationProvider: PROVIDER,
      kinds: ["projects", "statuses"],
    });
  }

  protected override async lookup(in_: Integration, kind: string, keys: string[]): Promise<NameMap> {
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
