// Factory MCP tools over factories: list and create.
import { factoryLink } from "@openfactory/core";
import {
  checkTeam,
  factoryURL,
  listFactoriesAll,
  obj,
  s,
  str,
  tool,
  type Factory,
  type Json,
  type Prop,
} from "./common";

const REPO_ITEM: Prop = {
  type: "object",
  properties: { codeForge: str("GITHUB, GITLAB or AZURE_DEVOPS"), owner: str(), name: str() },
  required: ["owner", "name"],
  additionalProperties: false,
};

export const listFactories = tool(
  "list_factories",
  "Lists the factories you can access.",
  true,
  obj({ search: str("optional case-insensitive filter on factory name or alias") }),
  async (c, a) => {
    const q = s(a, "search").toLowerCase();
    const out = (await listFactoriesAll(c, s(a, "search")))
      .filter((f) => q === "" || f.name.toLowerCase().includes(q) || (f.alias ?? "").toLowerCase().includes(q))
      .map((f) => ({
        uid: f.id,
        name: f.name,
        alias: f.alias ?? "",
        ...(f.description ? { description: f.description } : {}),
        source: f.source ?? "",
        url: factoryURL(c, f),
      }))
      .sort((x, y) => (x.name < y.name ? -1 : x.name > y.name ? 1 : 0));
    return { factories: out };
  },
);

export const createFactory = tool(
  "create_factory",
  "Creates a factory with the selected repositories, integrations, and optional factory agents.",
  false,
  obj(
    {
      team_id: str("team ID; defaults to your team"),
      name: str("factory name"),
      alias: str("foreman handle used for @-mentions (up to 60 letters, digits, spaces, '.', '_', '-')"),
      description: str(),
      repositories: { type: "array", description: "repositories the factory works on", items: REPO_ITEM },
      integrations: {
        type: "array",
        description:
          "integrations to attach: slack, microsoft-teams, linear or jira (linear and jira are mutually exclusive)",
        items: { type: "string" },
      },
      agents: {
        type: "array",
        description: "default agents besides the foreman: triage, spec, implement, review; omit for all",
        items: { type: "string" },
      },
      model: str(
        "default model for agents; a Claude model selects the claude harness and an OpenAI model selects codex; defaults to claude-opus-5-5",
      ),
    },
    ["name"],
  ),
  async (c, a) => {
    checkTeam(c.principal, s(a, "team_id"));
    const req: Json = { name: s(a, "name"), definition: "defaults" };
    for (const k of ["alias", "description", "model"]) if (s(a, k)) req[k] = s(a, k);
    if (Array.isArray(a.repositories)) {
      req.repositories = (a.repositories as Json[]).map((r) => ({
        codeForge: String(r.codeForge ?? "").toUpperCase(),
        owner: String(r.owner ?? ""),
        name: String(r.name ?? ""),
      }));
    }
    if (Array.isArray(a.integrations)) req.integrations = (a.integrations as string[]).map((t) => ({ type: t }));
    if (Array.isArray(a.agents)) req.agents = a.agents;
    const res = await c.backend.api<{ factory: Factory }>(c.env, c.principal, "POST", "/factories", req);
    return {
      dashboard_url: res.factory.url ?? factoryLink(c.publicURL, res.factory.id),
      factory: res.factory,
      next_steps:
        "Share the dashboard link with the user. If the web app is open on its setup screen, refresh it to pick up the new factory. " +
        "Send the first work with send_work_item.",
    };
  },
);
