// The trigger catalog: filter keys per provider and event. Port of
// internal/triggers/catalog.go.
import { EVENT_ATTR, EVENT_PROVIDER } from "@openfactory/core";
import * as T from "./attributes";

export type CmpKind =
  "exact" | "fold" | "int" | "glob" | "keyword" | "emoji" | "branch" | "channelName" | "channelNameFold" | "noop";

export interface FieldSpec {
  attrs: string[];
  cmp: CmpKind;
  addedAttr?: string;
  inOnly?: boolean;
  exactlyOne?: boolean;
  required?: boolean;
  plainListOnly?: boolean;
  serverManaged?: boolean;
  enumValues?: string[];
  /** For aliases: the canonical key. */
  native?: string;
}

export interface EventSpec {
  keys: string[];
  notAuthorable?: boolean;
}

export interface ProviderSpec {
  fields: Record<string, FieldSpec>;
  aliases: Record<string, FieldSpec>;
  events: Record<string, EventSpec>;
}

export const KEY_SKIP_BOTS = "skip_bots";
export const KEY_PAYLOAD = "payload";

export const SKIP_BOTS_ALL = "all";
export const SKIP_BOTS_SELF = "self";
export const SKIP_BOTS_NONE = "none";

export const GITHUB_CI_EVENTS = new Set(["check_suite_completed", "workflow_run_completed"]);

function ev(keys: string[], ...names: string[]): Record<string, EventSpec> {
  const m: Record<string, EventSpec> = {};
  for (const n of names) m[n] = { keys };
  return m;
}

const f = (attrs: string[], cmp: CmpKind, extra: Partial<FieldSpec> = {}): FieldSpec => ({ attrs, cmp, ...extra });
const SB = KEY_SKIP_BOTS;

export const CATALOG: Record<string, ProviderSpec> = {
  [EVENT_PROVIDER.GitHub]: {
    fields: {
      repos: f([EVENT_ATTR.Repo], "fold"),
      labels: f([EVENT_ATTR.Labels], "fold", { addedAttr: EVENT_ATTR.AddedLabels }),
      assignees: f([EVENT_ATTR.Assignees], "fold"),
      authors: f([EVENT_ATTR.Author], "fold"),
      keywords: f([], "keyword"),
      mentioned: f([EVENT_ATTR.Mentioned], "fold"),
      base_branches: f([EVENT_ATTR.BaseBranch], "branch"),
      pr_numbers: f([EVENT_ATTR.PRNumber], "int"),
      paths: f([EVENT_ATTR.Paths], "glob"),
      reviewers: f([EVENT_ATTR.Reviewers], "fold"),
      reviewer_teams: f([EVENT_ATTR.ReviewerTeams], "fold"),
      review_states: f([EVENT_ATTR.ReviewState], "fold", {
        enumValues: ["approved", "changes_requested", "commented"],
      }),
      branches: f([EVENT_ATTR.Branch], "branch"),
      conclusions: f([EVENT_ATTR.Conclusion], "fold"),
      workflows: f([EVENT_ATTR.Workflow], "exact"),
      states: f([EVENT_ATTR.CommitState], "fold", { enumValues: ["success", "failure", "error"] }),
      contexts: f([EVENT_ATTR.Context], "fold"),
    },
    aliases: {
      baseBranches: f([EVENT_ATTR.BaseBranch], "branch", { native: "base_branches" }),
      prNumbers: f([EVENT_ATTR.PRNumber], "int", { native: "pr_numbers" }),
    },
    events: {
      ...ev(["repos", "labels", "assignees", "authors", "keywords", SB], "issue_created"),
      ...ev(
        ["repos", "labels", "assignees", "authors", SB],
        "issue_labeled",
        "issue_assigned",
        "pull_request_assigned",
      ),
      ...ev(["repos", "labels", "authors", "keywords", SB], "issue_commented", "pull_request_commented"),
      ...ev(["repos", "mentioned", "labels", "authors", "keywords", SB], "issue_mentioned", "pull_request_mentioned"),
      ...ev(
        ["repos", "base_branches", "pr_numbers", "paths", "assignees", "authors", "labels", "keywords", SB],
        "pull_request_opened",
        "pull_request_ready",
        "pull_request_reopened",
      ),
      ...ev(
        ["repos", "base_branches", "pr_numbers", "paths", "assignees", "authors", "labels", SB],
        "pull_request_closed",
        "pull_request_dequeued",
        "pull_request_enqueued",
        "pull_request_labeled",
        "pull_request_merged",
        "pull_request_synchronized",
      ),
      ...ev(["repos", "reviewers", "reviewer_teams", "authors", SB], "pull_request_review_requested"),
      ...ev(
        ["repos", "mentioned", "labels", "review_states", "authors", "keywords", SB],
        "pull_request_review_submitted",
      ),
      ...ev(["repos", "branches", "paths", SB], "push"),
      ...ev(["repos", "conclusions", "branches", "labels", "authors", SB], "check_suite_completed"),
      ...ev(["repos", "conclusions", "branches", "workflows", "labels", "authors", SB], "workflow_run_completed"),
      ...ev(["repos", "states", "contexts", SB], "commit_status"),
      ...ev(["repos", SB], "check_run_rerequested", "check_suite_rerequested"),
    },
  },
  [EVENT_PROVIDER.GitLab]: {
    fields: {
      repos: f([EVENT_ATTR.Repo], "fold"),
      actions: f([EVENT_ATTR.Action], "fold"),
      base_branches: f([EVENT_ATTR.BaseBranch], "branch"),
      branches: f([EVENT_ATTR.Branch], "branch"),
      mentioned: f([EVENT_ATTR.Mentioned], "fold", { serverManaged: true }),
    },
    aliases: {},
    events: {
      ...ev(["repos", "mentioned"], "bot_mentioned"),
      ...ev(["repos", "actions", "base_branches"], "merge_request"),
      ...ev(["repos", "branches"], "push"),
    },
  },
  [EVENT_PROVIDER.AzureDevOps]: {
    fields: {
      repos: f([EVENT_ATTR.Repo], "fold"),
      base_branches: f([EVENT_ATTR.BaseBranch], "branch"),
      branches: f([EVENT_ATTR.Branch], "branch"),
      source_repos: f([EVENT_ATTR.SourceRepo], "fold", { serverManaged: true }),
      mentioned: f([EVENT_ATTR.Mentioned], "fold"),
      work_item_types: f([EVENT_ATTR.WorkItemType], "exact"),
      labels: f([EVENT_ATTR.Labels], "fold", { addedAttr: EVENT_ATTR.AddedLabels }),
      assignees: f([EVENT_ATTR.Assignees], "fold"),
      authors: f([EVENT_ATTR.Author], "fold"),
    },
    aliases: {},
    events: {
      ...ev(
        ["repos", "base_branches", "source_repos"],
        "pull_request_created",
        "pull_request_updated",
        "pull_request_commented",
        "pull_request_closed",
        "pull_request_merged",
      ),
      ...ev(["repos", "mentioned"], "pull_request_mentioned"),
      ...ev(
        ["work_item_types", "labels", "assignees", "authors"],
        "work_item_created",
        "work_item_assigned",
        "work_item_labeled",
      ),
      ...ev(["mentioned"], "work_item_mentioned"),
      push: { keys: ["repos", "branches"], notAuthorable: true },
    },
  },
  [EVENT_PROVIDER.Jira]: {
    fields: {
      project_keys: f([EVENT_ATTR.ProjectKey], "exact"),
      labels: f([EVENT_ATTR.Labels], "exact", { addedAttr: EVENT_ATTR.AddedLabels }),
      status_ids: f([EVENT_ATTR.StatusID], "exact"),
      keywords: f([], "noop"),
    },
    aliases: {
      projects: f([EVENT_ATTR.ProjectName], "fold", { plainListOnly: true, native: "project_keys" }),
      statuses: f([EVENT_ATTR.StatusName], "fold", { plainListOnly: true, native: "status_ids" }),
    },
    events: {
      ...ev(["project_keys", "labels"], "issue_created", "issue_labeled"),
      ...ev(["project_keys", "status_ids"], "status_changed"),
      ...ev(["project_keys", "labels", "keywords"], "agent_session_created"),
    },
  },
  [EVENT_PROVIDER.Linear]: {
    fields: {
      team_ids: f([EVENT_ATTR.TeamID], "exact"),
      project_ids: f([EVENT_ATTR.ProjectID], "exact"),
      labels: f([EVENT_ATTR.Labels], "fold", { addedAttr: EVENT_ATTR.AddedLabels }),
      state_ids: f([EVENT_ATTR.StateID], "exact"),
      assignee_ids: f([EVENT_ATTR.Assignees], "exact"),
      mentioned_user_ids: f([EVENT_ATTR.Mentioned], "exact"),
      creator_ids: f([EVENT_ATTR.Author], "exact"),
      issue_ids: f([EVENT_ATTR.IssueID], "exact"),
      keywords: f([], "keyword"),
    },
    aliases: {
      teams: f([EVENT_ATTR.TeamName], "exact", { plainListOnly: true, native: "team_ids" }),
      projects: f([EVENT_ATTR.ProjectName], "exact", { plainListOnly: true, native: "project_ids" }),
      states: f([EVENT_ATTR.StateName], "exact", { plainListOnly: true, native: "state_ids" }),
      issues: f([EVENT_ATTR.IssueNumber], "int", { plainListOnly: true, native: "issue_ids" }),
    },
    events: {
      ...ev(
        ["team_ids", "project_ids", "labels", "state_ids", "assignee_ids", "mentioned_user_ids", "creator_ids"],
        "issue_labeled",
        "issue_created",
        "issue_assigned",
        "issue_state_changed",
      ),
      ...ev(
        ["team_ids", "project_ids", "labels", "state_ids", "issue_ids", "mentioned_user_ids", "creator_ids"],
        "comment_created",
      ),
      ...ev(["team_ids", "project_ids", "labels", "creator_ids", "keywords"], "agent_session_created"),
    },
  },
  [EVENT_PROVIDER.Slack]: {
    fields: {
      channel_ids: f([EVENT_ATTR.ChannelID], "exact"),
      user_ids: f([EVENT_ATTR.UserID], "exact"),
      keywords: f([], "keyword"),
      emojis: f([EVENT_ATTR.Emoji], "emoji"),
      item_user_ids: f([EVENT_ATTR.ItemUserID], "exact"),
    },
    aliases: {
      channels: f([EVENT_ATTR.ChannelName], "channelName", { plainListOnly: true, native: "channel_ids" }),
      users: f([EVENT_ATTR.UserName, EVENT_ATTR.UserEmail], "fold", { plainListOnly: true, native: "user_ids" }),
      itemUsers: f([EVENT_ATTR.ItemUserName, EVENT_ATTR.ItemUserEmail], "fold", {
        plainListOnly: true,
        native: "item_user_ids",
      }),
    },
    events: {
      ...ev(
        ["channel_ids", "user_ids", "keywords"],
        "app_mention",
        "message_dm",
        "message_im",
        "message_mpim",
        "message_posted",
      ),
      ...ev(["channel_ids", "user_ids", "keywords", "emojis", "item_user_ids"], "reaction_added"),
      ...ev(["channel_ids", "user_ids"], "member_joined_channel"),
    },
  },
  [EVENT_PROVIDER.Teams]: {
    fields: {
      team_ids: f([EVENT_ATTR.TeamID], "fold", { inOnly: true, exactlyOne: true, required: true }),
      channel_ids: f([EVENT_ATTR.ChannelID], "exact", { inOnly: true, required: true }),
      user_ids: f([EVENT_ATTR.UserID], "fold"),
      keywords: f([], "keyword"),
    },
    aliases: {
      channels: f([EVENT_ATTR.ChannelName], "channelNameFold", { plainListOnly: true, native: "channel_ids" }),
      users: f([EVENT_ATTR.UserName], "fold", { plainListOnly: true, native: "user_ids" }),
    },
    events: ev(["team_ids", "channel_ids", "user_ids", "keywords"], "app_mention", "message_posted"),
  },
  [EVENT_PROVIDER.Webhook]: {
    fields: {
      webhook_ids: f([EVENT_ATTR.WebhookID, EVENT_ATTR.WebhookName], "exact", {
        inOnly: true,
        exactlyOne: true,
        required: true,
      }),
    },
    aliases: {},
    events: ev(["webhook_ids", KEY_PAYLOAD], T.EventWebhookReceived),
  },
  [EVENT_PROVIDER.Schedule]: {
    fields: { schedule_ids: f([EVENT_ATTR.AutomationID], "exact", { serverManaged: true }) },
    aliases: {},
    events: ev(["schedule_ids"], T.EventCronFired),
  },
};

/** Providers that never match events authored by the platform itself. */
export const SELF_IGNORED_PROVIDERS = new Set<string>([
  EVENT_PROVIDER.Slack,
  EVENT_PROVIDER.Teams,
  EVENT_PROVIDER.Linear,
  EVENT_PROVIDER.Jira,
  EVENT_PROVIDER.GitLab,
  EVENT_PROVIDER.AzureDevOps,
]);

/** Canonical trigger provider names, sorted. */
export function providers(): string[] {
  return Object.keys(CATALOG).sort();
}

export interface EventInfo {
  event: string;
  filter_fields: string[];
  aliases?: string[];
  authorable: boolean;
}

/** Maps integration identifiers to trigger provider names. */
export function canonicalProvider(p: string): string {
  const c = p.trim().toLowerCase().replace(/-/g, "_");
  switch (c) {
    case "microsoft_teams":
    case "msteams":
      return EVENT_PROVIDER.Teams;
    case "azuredevops":
    case "ado":
      return EVENT_PROVIDER.AzureDevOps;
  }
  return c;
}

/** The event catalog of a provider (sorted), or null. Server-managed keys are left out. */
export function events(provider: string): EventInfo[] | null {
  const ps = CATALOG[canonicalProvider(provider)];
  if (!ps) return null;
  const out: EventInfo[] = [];
  for (const [name, es] of Object.entries(ps.events)) {
    const info: EventInfo = { event: name, filter_fields: [], authorable: !es.notAuthorable };
    for (const k of es.keys) {
      if (ps.fields[k]?.serverManaged) continue;
      info.filter_fields.push(k);
    }
    const aliases = Object.entries(ps.aliases)
      .filter(([, a]) => es.keys.includes(a.native!))
      .map(([n]) => n)
      .sort();
    if (aliases.length > 0) info.aliases = aliases;
    out.push(info);
  }
  return out.sort((a, b) => (a.event < b.event ? -1 : a.event > b.event ? 1 : 0));
}
