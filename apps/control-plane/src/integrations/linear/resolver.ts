// Linear name resolver: team, project, state names and issue numbers to IDs (port of linear/resolver.go).
import { ApiError } from "@openfactory/core";
import type { Integration } from "@openfactory/core";
import { addId, addName, type NameMap } from "../namelist";
import { BaseNameResolver } from "../resolver";
import type { IntegrationDeps } from "../types";
import { cfg, integrationSecret } from "../util";
import { GraphQLError, LinearClient, PROVIDER } from "./linear";

const LIST_PAGE_SIZE = 100;
const MAX_LIST_PAGES = 50;
/** The most issues that one issue number can give (the same number exists in each team). */
const ISSUE_MATCHES = 50;

interface PageInfo {
  hasNextPage: boolean;
  endCursor: string;
}
interface Nodes<T> {
  nodes: T[];
  pageInfo: PageInfo;
}

const inTeams = (teams: string[], id: string): boolean => teams.length === 0 || teams.includes(id);

export class LinearNameResolver extends BaseNameResolver {
  constructor(
    deps: IntegrationDeps,
    private readonly graphqlUrl = "",
  ) {
    super(deps, {
      label: "Linear",
      providers: [PROVIDER],
      integrationProvider: PROVIDER,
      kinds: ["teams", "projects", "states", "issues"],
    });
  }

  /** A name ignores a leading "#", so "#123" is issue number 123. */
  protected override normalize(_kind: string, name: string): string {
    return name.trim().replace(/^#/, "").trim().toLowerCase();
  }

  private async client(in_: Integration): Promise<LinearClient> {
    let tok: string;
    try {
      tok = await integrationSecret(this.deps.secrets, in_);
    } catch (e) {
      throw new Error(`Linear integration credential: ${(e as Error).message}`, { cause: e });
    }
    return new LinearClient(this.graphqlUrl, tok, cfg(in_, "token_type"), this.deps.fetch);
  }

  /** scope["team_ids"] limits the lookup to these Linear teams. */
  protected override async lookup(
    in_: Integration,
    kind: string,
    keys: string[],
    scope: Record<string, string[]>,
  ): Promise<NameMap> {
    const teams = [...(scope["team_ids"] ?? [])].sort();
    const base = `${in_.id}/${kind}/${teams.join(",")}`;
    const c = await this.client(in_);
    try {
      if (kind !== "issues") return await this.lists.lookup(base, keys, () => listKind(c, kind, teams));
      const out: NameMap = {};
      for (const k of keys) {
        const n = /^\d+$/.test(k) ? Number(k) : 0;
        if (n <= 0) continue; // not an issue number: no match
        const byName = await this.lists.lookup(`${base}/${k}`, [k], () => findIssues(c, n, teams));
        for (const [key, ids] of Object.entries(byName)) for (const id of ids) addId(out, key, id);
      }
      return out;
    } catch (e) {
      throw describe(kind, e);
    }
  }
}

function describe(kind: string, e: unknown): unknown {
  const msg = `Linear refused the credential of the integration, so the ${kind} cannot be listed`;
  if (e instanceof ApiError && (e.status === 401 || e.status === 403))
    return new Error(`${msg}: ${e.message}`, { cause: e });
  if (e instanceof GraphQLError && e.message.toLowerCase().includes("authenticat"))
    return new Error(`${msg}: ${e.message}`, { cause: e });
  return e;
}

async function listKind(c: LinearClient, kind: string, teams: string[]): Promise<NameMap> {
  const out: NameMap = {};
  let after: string | null = null;
  for (let page = 0; page < MAX_LIST_PAGES; page++) {
    const vars = { first: LIST_PAGE_SIZE, after };
    let more: PageInfo;
    switch (kind) {
      case "teams": {
        const d = await c.query<{ teams: Nodes<{ id: string; name: string }> }>(
          "query($first: Int, $after: String) { teams(first: $first, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } }",
          vars,
        );
        for (const n of d?.teams.nodes ?? []) if (inTeams(teams, n.id)) addName(out, n.name, n.id);
        more = d?.teams.pageInfo ?? { hasNextPage: false, endCursor: "" };
        break;
      }
      case "projects": {
        const d = await c.query<{ projects: Nodes<{ id: string; name: string; teams: { nodes: { id: string }[] } }> }>(
          "query($first: Int, $after: String) { projects(first: $first, after: $after) { nodes { id name teams(first: 50) { nodes { id } } } pageInfo { hasNextPage endCursor } } }",
          vars,
        );
        for (const n of d?.projects.nodes ?? []) {
          if (n.teams.nodes.some((t) => inTeams(teams, t.id))) addName(out, n.name, n.id);
        }
        more = d?.projects.pageInfo ?? { hasNextPage: false, endCursor: "" };
        break;
      }
      default: {
        const d = await c.query<{ workflowStates: Nodes<{ id: string; name: string; team: { id: string } }> }>(
          "query($first: Int, $after: String) { workflowStates(first: $first, after: $after) { nodes { id name team { id } } pageInfo { hasNextPage endCursor } } }",
          vars,
        );
        for (const n of d?.workflowStates.nodes ?? []) if (inTeams(teams, n.team.id)) addName(out, n.name, n.id);
        more = d?.workflowStates.pageInfo ?? { hasNextPage: false, endCursor: "" };
      }
    }
    if (!more.hasNextPage || more.endCursor === "") return out;
    after = more.endCursor;
  }
  return out;
}

/** The issues with a number, in the teams (all teams when there are none). */
async function findIssues(c: LinearClient, number: number, teams: string[]): Promise<NameMap> {
  const d = await c.query<{ issues: { nodes: { id: string; team: { id: string } }[] } }>(
    "query($n: Float!, $first: Int) { issues(first: $first, filter: { number: { eq: $n } }) { nodes { id team { id } } } }",
    { n: number, first: ISSUE_MATCHES },
  );
  const out: NameMap = {};
  for (const n of d?.issues.nodes ?? []) if (inTeams(teams, n.team.id)) addId(out, String(number), n.id);
  return out;
}
