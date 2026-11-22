// Builds the chat and tracker integrations from one set of dependencies and
// routes their inbound paths. WS10a registers the responders, trackers, name
// resolvers and context builders in its notify and eventctx registries.
import { JiraHandler, WEBHOOK_PATH as JIRA_PATH } from "./jira/jira";
import { JiraNameResolver } from "./jira/resolver";
import { LinearHandler, WEBHOOK_PATH as LINEAR_PATH } from "./linear/linear";
import { LinearNameResolver } from "./linear/resolver";
import { SlackHandler, EVENTS_PATH, INTERACTIONS_PATH } from "./slack/handler";
import { SlackResponder } from "./slack/responder";
import { SlackNameResolver } from "./slack/resolver";
import { TeamsHandler, MESSAGES_PATH } from "./teams/handler";
import { TeamsNameResolver } from "./teams/resolver";
import type {
  EventContextBuilder,
  IntegrationDeps,
  IssueTracker,
  NameResolver,
  Responder,
  WebhookOutcome,
} from "./types";

export interface IntegrationOptions {
  slackApiBaseUrl?: string;
  linearGraphqlUrl?: string;
  teamsOpenIdMetadataUrl?: string;
  teamsLoginUrl?: string;
  teamsGraphUrl?: string;
}

export interface IntegrationHandlers {
  slack: { handler: SlackHandler; responder: SlackResponder; resolver: SlackNameResolver };
  teams: { handler: TeamsHandler; resolver: TeamsNameResolver };
  linear: { handler: LinearHandler; resolver: LinearNameResolver };
  jira: { handler: JiraHandler; resolver: JiraNameResolver };
  /** Responders by trigger provider name (slack, teams, linear, jira). */
  responders: Record<string, Responder>;
  /** Tracker write-backs by provider (linear, jira). */
  trackers: Record<string, IssueTracker>;
  /** Name resolvers by provider (slack, teams, microsoft_teams, linear, jira). */
  nameResolvers: Record<string, NameResolver>;
  /** Event context builders by provider (slack, teams). */
  contextBuilders: Record<string, EventContextBuilder>;
  /** Serves an inbound path. Returns null for a path that is not an integration endpoint. */
  handleRequest(req: Request): Promise<WebhookOutcome | null>;
}

export const INBOUND_PATHS = [EVENTS_PATH, INTERACTIONS_PATH, MESSAGES_PATH, LINEAR_PATH, JIRA_PATH] as const;

export function createIntegrationHandlers(deps: IntegrationDeps, opts: IntegrationOptions = {}): IntegrationHandlers {
  const slackHandler = new SlackHandler(deps, { apiBaseUrl: opts.slackApiBaseUrl });
  const slackResponder = new SlackResponder(deps, opts.slackApiBaseUrl);
  const slackResolver = new SlackNameResolver(deps, opts.slackApiBaseUrl);
  const teamsHandler = new TeamsHandler(deps, {
    openIdMetadataUrl: opts.teamsOpenIdMetadataUrl,
    loginUrl: opts.teamsLoginUrl,
    graphUrl: opts.teamsGraphUrl,
  });
  const teamsResolver = new TeamsNameResolver(teamsHandler);
  const linearHandler = new LinearHandler(deps, opts.linearGraphqlUrl);
  const linearResolver = new LinearNameResolver(deps, opts.linearGraphqlUrl);
  const jiraHandler = new JiraHandler(deps);
  const jiraResolver = new JiraNameResolver(deps);
  return {
    slack: { handler: slackHandler, responder: slackResponder, resolver: slackResolver },
    teams: { handler: teamsHandler, resolver: teamsResolver },
    linear: { handler: linearHandler, resolver: linearResolver },
    jira: { handler: jiraHandler, resolver: jiraResolver },
    responders: { slack: slackResponder, teams: teamsHandler, linear: linearHandler, jira: jiraHandler },
    trackers: { linear: linearHandler, jira: jiraHandler },
    nameResolvers: {
      slack: slackResolver,
      teams: teamsResolver,
      microsoft_teams: teamsResolver,
      linear: linearResolver,
      jira: jiraResolver,
    },
    contextBuilders: { slack: slackResponder, teams: teamsHandler },
    async handleRequest(req: Request): Promise<WebhookOutcome | null> {
      if (req.method !== "POST") return null;
      switch (new URL(req.url).pathname) {
        case EVENTS_PATH:
          return slackHandler.handleEvents(req);
        case INTERACTIONS_PATH:
          return slackHandler.handleInteractions(req);
        case MESSAGES_PATH:
          return teamsHandler.handleMessages(req);
        case LINEAR_PATH:
          return linearHandler.handleWebhook(req);
        case JIRA_PATH:
          return jiraHandler.handleWebhook(req);
      }
      return null;
    },
  };
}
