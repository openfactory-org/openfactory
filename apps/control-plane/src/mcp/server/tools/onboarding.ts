// Factory MCP onboarding tools: teams, forge and tracker listings, and the
// browser connection flow of team integrations.
import { OfError, integrationsLink } from "@openfactory/core";
import { teamOp } from "../../team";
import type { Connection } from "../backend";
import { b, bool, can, checkTeam, invalid, notFound, obj, s, str, tool, type FactoryCtx } from "./common";

const FORGE_PROVIDERS: Record<string, string> = { GITHUB: "github", GITLAB: "gitlab" };
const CONNECTION_PROVIDERS = ["github", "gitlab", "slack", "linear", "jira"];

async function existingIntegration(c: FactoryCtx, provider: string): Promise<{ id: string } | null> {
  const out = await c.backend.api<{ items?: { id: string; provider: string }[] }>(
    c.env,
    c.principal,
    "GET",
    "/integrations",
  );
  return (out.items ?? []).find((x) => x.provider === provider) ?? null;
}

export const listTeams = tool(
  "list_teams",
  "Lists current memberships and first-time joinable team choices.",
  true,
  obj({}),
  async (c) => {
    const team = await c.backend.api<{ id: string; name: string }>(
      c.env,
      c.principal,
      "GET",
      `/teams/${encodeURIComponent(c.principal.teamId)}`,
    );
    return {
      joinable: [],
      memberships: [{ name: team.name, role: c.principal.role, team_id: team.id }],
      note: "Self-hosted API keys belong to one team; ask an admin to add you to another team.",
    };
  },
);

export const createTeam = tool(
  "create_team",
  "Creates the authenticated user's first team with a confirmed name.",
  false,
  obj(
    {
      name: str("the team name the user confirmed"),
      confirmed: bool("must be true once the user explicitly confirmed the name"),
    },
    ["name", "confirmed"],
  ),
  async (c, a) => {
    if (s(a, "name").trim() === "") throw invalid("name is required");
    if (!b(a, "confirmed")) throw invalid("confirm the team name with the user, then call again with confirmed=true");
    const team = await c.backend.api<{ id: string; name: string }>(
      c.env,
      c.principal,
      "GET",
      `/teams/${encodeURIComponent(c.principal.teamId)}`,
    );
    throw new OfError(
      "conflict",
      `you are already a member of team "${team.name}" (${team.id}); a first team can only be created once`,
    );
  },
);

export const joinTeam = tool(
  "join_team",
  "Joins a team selected from the first-time discovery choices.",
  false,
  obj({ team_id: str("ID of a team from list_teams joinable choices") }, ["team_id"]),
  async (c, a) => {
    if (s(a, "team_id") === c.principal.teamId) return { status: "already_member", team_id: c.principal.teamId };
    throw new OfError(
      "operation_not_supported",
      `team "${s(a, "team_id")}" is not among your joinable teams; ask one of its admins to add you`,
    );
  },
);

export const listForgeRepositories = tool(
  "list_forge_repositories",
  "Lists repositories available through a team's connected GitHub or GitLab account.",
  true,
  obj({
    team_id: str("team ID; defaults to your team"),
    code_forge: str("GITHUB (default) or GITLAB"),
    query: str("optional filter on owner/name"),
  }),
  async (c, a) => {
    checkTeam(c.principal, s(a, "team_id"));
    const forge = (s(a, "code_forge") || "GITHUB").toUpperCase();
    if (!(forge in FORGE_PROVIDERS)) throw invalid("code_forge must be GITHUB or GITLAB");
    if (!c.backend.listRepositories) {
      throw new OfError(
        "integration_not_configured",
        `repository listing is not available; call start_connection with provider "${FORGE_PROVIDERS[forge]}"`,
      );
    }
    const repos = await c.backend.listRepositories(c.env, c.principal.teamId, forge, s(a, "query"));
    return { code_forge: forge, repositories: repos ?? [] };
  },
);

export const listTrackerScopes = tool(
  "list_tracker_scopes",
  "Lists the Linear teams or Jira projects that can be scoped to route issues to a new factory.",
  true,
  obj({ team_id: str("team ID; defaults to your team"), provider: str("linear or jira") }, ["provider"]),
  async (c, a) => {
    checkTeam(c.principal, s(a, "team_id"));
    const prov = s(a, "provider").toLowerCase();
    if (prov !== "linear" && prov !== "jira") throw invalid("provider must be linear or jira");
    if (!c.backend.listTrackerScopes) {
      throw new OfError(
        "integration_not_configured",
        `${prov} is not connected; call start_connection with provider "${prov}"`,
      );
    }
    const scopes = await c.backend.listTrackerScopes(c.env, c.principal.teamId, prov);
    return { provider: prov, scopes: scopes ?? [] };
  },
);

export const startConnection = tool(
  "start_connection",
  "Starts or checks setup for GitHub, GitLab, Slack, Linear, or Jira.",
  false,
  obj({ provider: str("github, gitlab, slack, linear or jira") }, ["provider"]),
  async (c, a) => {
    const prov = s(a, "provider").toLowerCase();
    if (!CONNECTION_PROVIDERS.includes(prov))
      throw invalid(`provider must be one of ${CONNECTION_PROVIDERS.join(", ")}`);
    if (c.backend.startConnection) return c.backend.startConnection(c.env, c.principal.teamId, prov);
    const ex = await existingIntegration(c, prov);
    if (ex) return { provider: prov, status: "connected", integration_id: ex.id } satisfies Connection;
    if (!can(c.principal, "admin")) {
      throw new OfError(
        "not_authorized",
        `${prov} is not connected and connecting team integrations requires an admin`,
      );
    }
    const { id } = await teamOp<{ id: string }>(
      c.env,
      "mcp.connection.start",
      { provider: prov },
      { teamId: c.principal.teamId },
    );
    return {
      connection_id: id,
      provider: prov,
      status: "pending",
      authorize_url: integrationsLink(c.publicURL, prov, id),
      detail:
        "Open authorize_url in a browser, connect the integration on the Integrations page, then call get_connection_status.",
    } satisfies Connection;
  },
);

export const getConnectionStatus = tool(
  "get_connection_status",
  "Checks whether a browser authorization flow completed.",
  true,
  obj({
    connection_id: str("connection_id returned by start_connection"),
    provider: str("alternatively, the provider to check"),
  }),
  async (c, a) => {
    const cid = s(a, "connection_id");
    if (cid === "" && s(a, "provider") === "") throw invalid("connection_id or provider is required");
    if (cid !== "" && c.backend.connectionStatus) return c.backend.connectionStatus(c.env, c.principal.teamId, cid);
    let prov = s(a, "provider").toLowerCase();
    if (cid !== "") {
      const row = await teamOp<{ provider: string } | null>(
        c.env,
        "mcp.connection.get",
        { id: cid },
        { teamId: c.principal.teamId },
      );
      if (!row) throw notFound(`connection ${cid} not found`);
      prov = row.provider;
    }
    const ex = await existingIntegration(c, prov);
    if (ex)
      return { ...(cid ? { connection_id: cid } : {}), provider: prov, status: "connected", integration_id: ex.id };
    return {
      ...(cid ? { connection_id: cid } : {}),
      provider: prov,
      status: "pending",
      detail: "authorization has not completed yet",
    };
  },
);
