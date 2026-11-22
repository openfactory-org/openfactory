// Port of internal/mcpregistry/builtin.go and trial.go: the built-in
// integration servers /mcp/integrations/{provider}/{runId}. They give agents
// tools for the factory's own integrations, with the integration's
// credentials (the app identity). A run may use builtin/<provider> only when
// one of its mcpServers has managedId builtin/<provider>, the factory attaches
// the integration, and the team has an integration record for the provider.
// Read tools are always there; write tools need allowWrite. In trial mode,
// write tools go to the run transcript instead of the provider.
import { BUILTIN_MCP_PREFIX, builtinMCPProvider, isTrialRun, sameIntegrationProvider } from "@openfactory/core";
import type { Env } from "../../contracts/env";
import { jsonText, textResult, type CallToolResult, type McpServer, type Tool } from "../jsonrpc";
import { runError, type McpRunBackend, type RunView } from "../run";
import { grantedTo } from "./store";

const MAX_LIST_LIMIT = 50;

/** An integration record (core.Integration as JSON). */
export interface IntegrationRecord {
  id: string;
  provider: string;
  config: Record<string, string>;
  [k: string]: unknown;
}

/** Integration clients and selection (the integration modules). Injected. */
export interface IntegrationBackend {
  /**
   * factoryIntegration: the team's record for provider, provided the run's
   * factory attaches it (the factory record wins; workspace of the work
   * item's source when it came from this provider). Throws with a message
   * for the 403 answer.
   */
  factoryIntegration(env: Env, run: RunView, provider: string, workspace: string): Promise<IntegrationRecord>;
  /** The decrypted credential of the record (IntegrationSecret). */
  credential(env: Env, run: RunView, rec: IntegrationRecord): Promise<string>;
  linearQuery(
    rec: IntegrationRecord,
    token: string,
    query: string,
    variables: Record<string, unknown>,
  ): Promise<unknown>;
  linearCreateComment(rec: IntegrationRecord, token: string, issueId: string, body: string): Promise<void>;
  jiraRequest(rec: IntegrationRecord, token: string, method: string, path: string, body?: unknown): Promise<unknown>;
  jiraText(adf: unknown): string;
  markdownToADF(md: string): unknown;
  slackCall(rec: IntegrationRecord, token: string, method: string, params: Record<string, string>): Promise<unknown>;
  slackPost(
    rec: IntegrationRecord,
    token: string,
    msg: { channel: string; text: string; thread_ts?: string; mrkdwn: boolean },
  ): Promise<string>;
  /** Null when the Microsoft Teams bot is not configured. */
  teamsPost: ((rec: IntegrationRecord, channelId: string, rootId: string, text: string) => Promise<void>) | null;
}

const notWired = async (): Promise<never> => {
  throw new Error("the integration clients are not available on this server");
};

/** Default until the integration modules are wired: every call fails. */
export const unavailableIntegrations: IntegrationBackend = {
  factoryIntegration: notWired,
  credential: notWired,
  linearQuery: notWired,
  linearCreateComment: notWired,
  jiraRequest: notWired,
  jiraText: () => "",
  markdownToADF: (md) => md,
  slackCall: notWired,
  slackPost: notWired,
  teamsPost: null,
};

interface Ctx {
  env: Env;
  run: RunView;
  rec: IntegrationRecord;
  token: string;
  backend: McpRunBackend;
  ib: IntegrationBackend;
  provider: string;
}

const toolErr = (e: unknown): CallToolResult => textResult(`Error: ${(e as Error).message ?? String(e)}`, true);
/** Go toolText("%s", raw JSON): the raw JSON text. */
const rawText = (v: unknown): CallToolResult => textResult(v === undefined ? "null" : JSON.stringify(v));

function clampLimit(n: unknown, def: number): number {
  const v = typeof n === "number" ? Math.trunc(n) : 0;
  if (v <= 0) return def;
  return v > MAX_LIST_LIMIT ? MAX_LIST_LIMIT : v;
}

const str = (a: Record<string, unknown>, k: string): string => (typeof a[k] === "string" ? (a[k] as string) : "");

function schema(props: Record<string, [string, string]>, required: string[]): Record<string, unknown> {
  const properties: Record<string, unknown> = {};
  for (const [k, [type, description]] of Object.entries(props)) properties[k] = { type, description };
  return { type: "object", properties, required, additionalProperties: false };
}

function audit(c: Ctx, tool: string): void {
  console.log(
    JSON.stringify({
      msg: "built-in mcp tool call",
      provider: c.rec.provider,
      tool,
      run: c.run.id,
      factory: c.run.factoryId,
    }),
  );
}

/** trialOr: in trial mode, a write tool records the call in the run and answers success. */
function trialOr(c: Ctx, tool: string, h: Tool<Ctx>["handler"]): Tool<Ctx>["handler"] {
  if (!isTrialRun({ metadata: c.run.metadata })) return h;
  return async (args) => {
    audit(c, tool + " (trial mode)");
    const json = JSON.stringify(args);
    try {
      await c.backend.appendRunLog(
        c.env,
        c.run,
        `Trial mode: ${tool} was recorded and not sent to ${c.rec.provider}. Arguments: ${json}`,
        args,
      );
    } catch (e) {
      return toolErr(new Error(`record trial write: ${(e as Error).message}`));
    }
    return textResult(`Done (trial mode: recorded in the run, not sent to ${c.rec.provider}).`);
  };
}

function linearTools(c: Ctx, allowWrite: boolean): Tool<Ctx>[] {
  const tools: Tool<Ctx>[] = [
    {
      name: "linear_get_issue",
      description:
        "Read a Linear issue (UUID or identifier such as ENG-42): title, state, assignee, labels, description and recent comments.",
      inputSchema: schema({ id: ["string", "Issue UUID or identifier, e.g. ENG-42"] }, ["id"]),
      handler: async (a) => {
        audit(c, "linear_get_issue");
        if (str(a, "id").trim() === "") return toolErr(new Error("id is required"));
        try {
          const out = (await c.ib.linearQuery(
            c.rec,
            c.token,
            `query Issue($id: String!) { issue(id: $id) { identifier title url description priorityLabel
		state { name } assignee { name } team { key name } project { name } labels { nodes { name } }
		comments(first: 20) { nodes { body createdAt user { name } } } } }`,
            { id: str(a, "id") },
          )) as { issue?: unknown };
          return rawText(out?.issue ?? null);
        } catch (e) {
          return toolErr(e);
        }
      },
    },
    {
      name: "linear_search_issues",
      description: "Search Linear issues by text.",
      inputSchema: schema(
        {
          query: ["string", "Text to search for in issue titles and descriptions"],
          limit: ["integer", "Maximum results (default 10, at most 50)"],
        },
        ["query"],
      ),
      handler: async (a) => {
        audit(c, "linear_search_issues");
        if (str(a, "query").trim() === "") return toolErr(new Error("query is required"));
        try {
          const out = (await c.ib.linearQuery(
            c.rec,
            c.token,
            `query Search($term: String!, $first: Int) { searchIssues(term: $term, first: $first) {
		nodes { identifier title url state { name } assignee { name } } } }`,
            { term: str(a, "query"), first: clampLimit(a.limit, 10) },
          )) as { searchIssues?: { nodes?: unknown } };
          return rawText(out?.searchIssues?.nodes ?? null);
        } catch (e) {
          return toolErr(e);
        }
      },
    },
  ];
  if (allowWrite) {
    tools.push({
      name: "linear_create_comment",
      description: "Post a Markdown comment on a Linear issue.",
      inputSchema: schema(
        { issue_id: ["string", "Issue UUID or identifier, e.g. ENG-42"], body: ["string", "Markdown comment"] },
        ["issue_id", "body"],
      ),
      handler: trialOr(c, "linear_create_comment", async (a) => {
        audit(c, "linear_create_comment");
        if (str(a, "issue_id").trim() === "" || str(a, "body").trim() === "")
          return toolErr(new Error("issue_id and body are required"));
        try {
          await c.ib.linearCreateComment(c.rec, c.token, str(a, "issue_id"), str(a, "body"));
          return textResult(`Commented on ${str(a, "issue_id")}.`);
        } catch (e) {
          return toolErr(e);
        }
      }),
    });
  }
  return tools;
}

interface JiraIssue {
  key: string;
  fields: {
    summary: string;
    description?: unknown;
    status?: { name?: string };
    assignee?: { displayName?: string } | null;
    issuetype?: { name?: string };
    labels?: string[] | null;
    comment?: { comments?: { author?: { displayName?: string }; body?: unknown; created?: string }[] };
  };
}

function jiraTools(c: Ctx, allowWrite: boolean): Tool<Ctx>[] {
  const tools: Tool<Ctx>[] = [
    {
      name: "jira_get_issue",
      description:
        "Read a Jira issue by key (e.g. OPS-12): summary, status, assignee, description and recent comments.",
      inputSchema: schema({ key: ["string", "Issue key, e.g. OPS-12"] }, ["key"]),
      handler: async (a) => {
        audit(c, "jira_get_issue");
        if (str(a, "key").trim() === "") return toolErr(new Error("key is required"));
        try {
          const raw = (await c.ib.jiraRequest(
            c.rec,
            c.token,
            "GET",
            `/rest/api/3/issue/${encodeURIComponent(str(a, "key"))}?fields=summary,description,status,assignee,issuetype,labels,comment`,
          )) as JiraIssue;
          const f = raw.fields ?? ({} as JiraIssue["fields"]);
          const out: Record<string, unknown> = {
            description: c.ib.jiraText(f.description),
            key: raw.key,
            labels: f.labels ?? null,
            status: f.status?.name ?? "",
            summary: f.summary ?? "",
            type: f.issuetype?.name ?? "",
          };
          if (f.assignee) out.assignee = f.assignee.displayName ?? "";
          let cs = f.comment?.comments ?? [];
          if (cs.length > 20) cs = cs.slice(cs.length - 20);
          out.comments = cs.map((cm) => ({
            Author: cm.author?.displayName ?? "",
            Created: cm.created ?? "",
            Body: c.ib.jiraText(cm.body),
          }));
          // Go marshals a map with sorted keys.
          return jsonText(Object.fromEntries(Object.entries(out).sort(([x], [y]) => (x < y ? -1 : 1))));
        } catch (e) {
          return toolErr(e);
        }
      },
    },
    {
      name: "jira_search",
      description: "Search Jira issues with JQL.",
      inputSchema: schema(
        {
          jql: ["string", "JQL query, e.g. project = OPS AND status != Done"],
          limit: ["integer", "Maximum results (default 10, at most 50)"],
        },
        ["jql"],
      ),
      handler: async (a) => {
        audit(c, "jira_search");
        if (str(a, "jql").trim() === "") return toolErr(new Error("jql is required"));
        try {
          const raw = (await c.ib.jiraRequest(c.rec, c.token, "POST", "/rest/api/3/search/jql", {
            jql: str(a, "jql"),
            maxResults: clampLimit(a.limit, 10),
            fields: ["summary", "status", "assignee"],
          })) as { issues?: JiraIssue[] };
          const out = (raw.issues ?? []).map((is) => ({
            key: is.key,
            summary: is.fields?.summary ?? "",
            status: is.fields?.status?.name ?? "",
            ...(is.fields?.assignee?.displayName ? { assignee: is.fields.assignee.displayName } : {}),
          }));
          return jsonText(out);
        } catch (e) {
          return toolErr(e);
        }
      },
    },
  ];
  if (allowWrite) {
    tools.push({
      name: "jira_add_comment",
      description: "Add a Markdown comment to a Jira issue.",
      inputSchema: schema({ key: ["string", "Issue key, e.g. OPS-12"], body: ["string", "Markdown comment"] }, [
        "key",
        "body",
      ]),
      handler: trialOr(c, "jira_add_comment", async (a) => {
        audit(c, "jira_add_comment");
        if (str(a, "key").trim() === "" || str(a, "body").trim() === "")
          return toolErr(new Error("key and body are required"));
        try {
          await c.ib.jiraRequest(
            c.rec,
            c.token,
            "POST",
            `/rest/api/3/issue/${encodeURIComponent(str(a, "key"))}/comment`,
            {
              body: c.ib.markdownToADF(str(a, "body")),
            },
          );
          return textResult(`Commented on ${str(a, "key")}.`);
        } catch (e) {
          return toolErr(e);
        }
      }),
    });
  }
  return tools;
}

interface SlackMessage {
  user?: string;
  bot_id?: string;
  text?: string;
  ts?: string;
  thread_ts?: string;
}

const slackMessages = (out: unknown): unknown => {
  const ms = (out as { messages?: SlackMessage[] } | null)?.messages;
  if (!ms) return null;
  return ms.map((m) => ({
    ...(m.user ? { user: m.user } : {}),
    ...(m.bot_id ? { bot_id: m.bot_id } : {}),
    text: m.text ?? "",
    ts: m.ts ?? "",
    ...(m.thread_ts ? { thread_ts: m.thread_ts } : {}),
  }));
};

function slackTools(c: Ctx, allowWrite: boolean): Tool<Ctx>[] {
  const tools: Tool<Ctx>[] = [
    {
      name: "slack_read_thread",
      description: "Read the messages of a Slack thread (channel ID and the parent message ts).",
      inputSchema: schema(
        {
          channel: ["string", "Channel ID, e.g. C0123456"],
          ts: ["string", "ts of the thread's parent message"],
          limit: ["integer", "Maximum messages (default 50, at most 50)"],
        },
        ["channel", "ts"],
      ),
      handler: async (a) => {
        audit(c, "slack_read_thread");
        if (str(a, "channel") === "" || str(a, "ts") === "") return toolErr(new Error("channel and ts are required"));
        try {
          const out = await c.ib.slackCall(c.rec, c.token, "conversations.replies", {
            channel: str(a, "channel"),
            ts: str(a, "ts"),
            limit: String(clampLimit(a.limit, MAX_LIST_LIMIT)),
          });
          return jsonText(slackMessages(out));
        } catch (e) {
          return toolErr(e);
        }
      },
    },
    {
      name: "slack_channel_history",
      description: "Read recent messages of a Slack channel the app is a member of.",
      inputSchema: schema(
        {
          channel: ["string", "Channel ID, e.g. C0123456"],
          limit: ["integer", "Maximum messages (default 20, at most 50)"],
        },
        ["channel"],
      ),
      handler: async (a) => {
        audit(c, "slack_channel_history");
        if (str(a, "channel") === "") return toolErr(new Error("channel is required"));
        try {
          const out = await c.ib.slackCall(c.rec, c.token, "conversations.history", {
            channel: str(a, "channel"),
            limit: String(clampLimit(a.limit, 20)),
          });
          return jsonText(slackMessages(out));
        } catch (e) {
          return toolErr(e);
        }
      },
    },
  ];
  if (allowWrite) {
    tools.push({
      name: "slack_post_message",
      description: "Post a message to a Slack channel, optionally as a thread reply.",
      inputSchema: schema(
        {
          channel: ["string", "Channel ID, e.g. C0123456"],
          text: ["string", "Message text (Slack mrkdwn)"],
          thread_ts: ["string", "Reply in this thread (parent message ts)"],
        },
        ["channel", "text"],
      ),
      handler: trialOr(c, "slack_post_message", async (a) => {
        audit(c, "slack_post_message");
        if (str(a, "channel") === "" || str(a, "text").trim() === "")
          return toolErr(new Error("channel and text are required"));
        try {
          const ts = await c.ib.slackPost(c.rec, c.token, {
            channel: str(a, "channel"),
            text: str(a, "text"),
            ...(str(a, "thread_ts") ? { thread_ts: str(a, "thread_ts") } : {}),
            mrkdwn: true,
          });
          return textResult(`Posted message ${ts} in ${str(a, "channel")}.`);
        } catch (e) {
          return toolErr(e);
        }
      }),
    });
  }
  return tools;
}

function teamsTools(c: Ctx, allowWrite: boolean): Tool<Ctx>[] {
  const tools: Tool<Ctx>[] = [
    {
      name: "teams_list_channels",
      description:
        "List the Microsoft Teams channels that the integration is limited to. An empty list means that the bot can post in each channel where it was mentioned.",
      inputSchema: { type: "object", properties: {}, additionalProperties: false },
      handler: async () => {
        audit(c, "teams_list_channels");
        const ids = (c.rec.config?.channel_ids ?? "")
          .split(",")
          .map((x) => x.trim())
          .filter(Boolean);
        return jsonText({ channel_ids: ids, limited: ids.length > 0 });
      },
    },
  ];
  if (allowWrite) {
    tools.push({
      name: "teams_post_message",
      description:
        "Post a message to a Microsoft Teams channel, optionally as a reply in a thread. The bot cannot read messages.",
      inputSchema: schema(
        {
          channel: ["string", "Channel conversation ID, e.g. 19:abc@thread.tacv2"],
          text: ["string", "Message text (Markdown)"],
          thread: ["string", "Reply in this thread (ID of the first message of the thread)"],
        },
        ["channel", "text"],
      ),
      handler: trialOr(c, "teams_post_message", async (a) => {
        audit(c, "teams_post_message");
        if (str(a, "channel") === "" || str(a, "text").trim() === "")
          return toolErr(new Error("channel and text are required"));
        if (!c.ib.teamsPost) return toolErr(new Error("the Microsoft Teams bot is not configured on this server"));
        try {
          await c.ib.teamsPost(c.rec, str(a, "channel"), str(a, "thread"), str(a, "text"));
          return textResult(`Posted message in ${str(a, "channel")}.`);
        } catch (e) {
          return toolErr(e);
        }
      }),
    });
  }
  return tools;
}

/** integrationServer: the tools of one provider for one run. */
export function integrationServer(c: Ctx, allowWrite: boolean): McpServer<Ctx> {
  let provider = c.rec.provider.toLowerCase();
  if (sameIntegrationProvider(provider, "teams")) provider = "teams";
  const tools =
    provider === "linear"
      ? linearTools(c, allowWrite)
      : provider === "jira"
        ? jiraTools(c, allowWrite)
        : provider === "slack"
          ? slackTools(c, allowWrite)
          : provider === "teams"
            ? teamsTools(c, allowWrite)
            : [];
  return {
    info: { name: "openfactory-" + provider, version: "1.0.0" },
    instructions: `Tools for the factory's ${provider} integration. They act as the integration's app identity.`,
    tools,
  };
}

/**
 * Checks the grant and the integration of a /mcp/integrations request.
 * Returns the server and its context, or the error Response.
 */
export async function integrationContext(
  env: Env,
  backend: McpRunBackend,
  ib: IntegrationBackend,
  run: RunView,
  provider: string,
): Promise<{ server: McpServer<Ctx>; ctx: Ctx } | { response: Response }> {
  const id = BUILTIN_MCP_PREFIX + provider;
  if (!builtinMCPProvider(id).known)
    return { response: runError(404, "not_found", "unknown built-in MCP server " + id) };
  const { granted, allowWrite } = grantedTo(run.mcpServers, id);
  if (!granted) return { response: runError(403, "forbidden", "this run was not granted " + id) };
  let rec: IntegrationRecord;
  try {
    const src = await backend.workItemSource(env, run);
    const workspace = src && sameIntegrationProvider(src.provider, provider) ? src.workspace : "";
    rec = await ib.factoryIntegration(env, run, provider, workspace);
  } catch (e) {
    return { response: runError(403, "integration", (e as Error).message) };
  }
  let token = "";
  // The Teams handler reads the bot credentials of the record itself.
  if (provider !== "teams") {
    try {
      token = await ib.credential(env, run, rec);
    } catch (e) {
      console.warn("built-in mcp: integration credential", provider, run.id, (e as Error).message);
      return { response: runError(502, "integration", `the ${provider} integration has no usable credential`) };
    }
  }
  const ctx: Ctx = { env, run, rec, token, backend, ib, provider };
  return { server: integrationServer(ctx, allowWrite), ctx };
}
