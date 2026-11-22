// Factory MCP notification routes: where a factory can reach the caller.
import { inboxLink } from "@openfactory/core";
import { factoryOf, invalid, obj, s, str, tool, type Factory, type FactoryCtx, type Json } from "./common";

interface Route {
  id: string;
  kind: string;
  description: string;
}

async function routes(c: FactoryCtx, f: Factory): Promise<Route[]> {
  const out: Route[] = [
    { id: "inbox", kind: "inbox", description: `Your Open Factory inbox (${inboxLink(c.publicURL)})` },
  ];
  const ints = await c.backend.api<Json>(
    c.env,
    c.principal,
    "GET",
    `/factories/${encodeURIComponent(f.id)}/integrations`,
  );
  const raw = (ints?.integrations ?? ints) as unknown;
  const keys = (
    Array.isArray(raw)
      ? raw.map((x) => (typeof x === "string" ? x : String((x as Json).type ?? (x as Json).provider ?? "")))
      : Object.keys((raw as Json) ?? {})
  ).sort();
  for (const k of keys) {
    switch (k) {
      case "slack":
        out.push({
          id: "slack_dm",
          kind: "slack",
          description: "Slack direct message (requires a linked Slack account)",
        });
        break;
      case "microsoft-teams":
      case "microsoft_teams":
        out.push({
          id: "teams_dm",
          kind: "microsoft_teams",
          description: "Microsoft Teams chat message (requires a linked account)",
        });
        break;
      case "linear":
        out.push({
          id: "linear_issue",
          kind: "linear",
          description: "Comment on the work item's Linear issue, when it has one",
        });
        break;
      case "jira":
        out.push({
          id: "jira_issue",
          kind: "jira",
          description: "Comment on the work item's Jira issue, when it has one",
        });
        break;
    }
  }
  return out;
}

/** Fails with invalid_request when id is not a route of the factory. */
export async function checkRoute(c: FactoryCtx, f: Factory, id: string): Promise<void> {
  const rs = await routes(c, f);
  if (!rs.some((r) => r.id === id)) throw invalid(`unknown notify_route "${id}"; call list_notification_routes`);
}

export const listNotificationRoutes = tool(
  "list_notification_routes",
  "Lists the notification destinations available to you in a factory.",
  true,
  obj({ factory_id: str("factory UID") }, ["factory_id"]),
  async (c, a) => {
    const f = await factoryOf(c, s(a, "factory_id"));
    return {
      factory_id: f.id,
      note: "Delivery is best-effort; treat notifications as a convenience.",
      routes: await routes(c, f),
    };
  },
);
