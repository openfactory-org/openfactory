// The trigger catalog: filter keys per provider and event. Port of
// internal/triggers/catalog.go.
import * as T from "./attributes";

export type CmpKind = "exact" | "fold" | "int" | "glob" | "keyword" | "emoji" | "branch" | "channelName" | "channelNameFold" | "noop";

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
  [T.ProviderGitHub]: {
    fields: {
      repos: f([T.AttrRepo], "fold"),
      labels: f([T.AttrLabels], "fold", { addedAttr: T.AttrAddedLabels }),
      assignees: f([T.AttrAssignees], "fold"),
      authors: f([T.AttrAuthor], "fold"),
      keywords: f([], "keyword"),
      mentioned: f([T.AttrMentioned], "fold"),
      base_branches: f([T.AttrBaseBranch], "branch"),
      pr_numbers: f([T.AttrPRNumber], "int"),
      paths: f([T.AttrPaths], "glob"),
      reviewers: f([T.AttrReviewers], "fold"),
      reviewer_teams: f([T.AttrReviewerTeams], "fold"),
      review_states: f([T.AttrReviewState], "fold", { enumValues: ["approved", "changes_requested", "commented"] }),
      branches: f([T.AttrBranch], "branch"),
      conclusions: f([T.AttrConclusion], "fold"),
      workflows: f([T.AttrWorkflow], "exact"),
      states: f([T.AttrCommitState], "fold", { enumValues: ["success", "failure", "error"] }),
      contexts: f([T.AttrContext], "fold"),
    },
    aliases: {
      baseBranches: f([T.AttrBaseBranch], "branch", { native: "base_branches" }),
      prNumbers: f([T.AttrPRNumber], "int", { native: "pr_numbers" }),
    },
    events: {
      ...ev(["repos", "labels", "assignees", "authors", "keywords", SB], "issue_created"),
      ...ev(["repos", "labels", "assignees", "authors", SB], "issue_labeled", "issue_assigned", "pull_request_assigned"),
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
      ...ev(["repos", "mentioned", "labels", "review_states", "authors", "keywords", SB], "pull_request_review_submitted"),
      ...ev(["repos", "branches", "paths", SB], "push"),
      ...ev(["repos", "conclusions", "branches", "labels", "authors", SB], "check_suite_completed"),
      ...ev(["repos", "conclusions", "branches", "workflows", "labels", "authors", SB], "workflow_run_completed"),
      ...ev(["repos", "states", "contexts", SB], "commit_status"),
      ...ev(["repos", SB], "check_run_rerequested", "check_suite_rerequested"),
    },
  },
  [T.ProviderGitLab]: {
    fields: {
      repos: f([T.AttrRepo], "fold"),
      actions: f([T.AttrAction], "fold"),
      base_branches: f([T.AttrBaseBranch], "branch"),
      branches: f([T.AttrBranch], "branch"),
      mentioned: f([T.AttrMentioned], "fold", { serverManaged: true }),
    },
    aliases: {},
    events: {
      ...ev(["repos", "mentioned"], "bot_mentioned"),
      ...ev(["repos", "actions", "base_branches"], "merge_request"),
      ...ev(["repos", "branches"], "push"),
    },
  },
  [T.ProviderAzureDevOps]: {
    fields: {
      repos: f([T.AttrRepo], "fold"),
      base_branches: f([T.AttrBaseBranch], "branch"),
      branches: f([T.AttrBranch], "branch"),
      source_repos: f([T.AttrSourceRepo], "fold", { serverManaged: true }),
      mentioned: f([T.AttrMentioned], "fold"),
      work_item_types: f([T.AttrWorkItemType], "exact"),
      labels: f([T.AttrLabels], "fold", { addedAttr: T.AttrAddedLabels }),
      assignees: f([T.AttrAssignees], "fold"),
      authors: f([T.AttrAuthor], "fold"),
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
      ...ev(["work_item_types", "labels", "assignees", "authors"], "work_item_created", "work_item_assigned", "work_item_labeled"),
      ...ev(["mentioned"], "work_item_mentioned"),
      push: { keys: ["repos", "branches"], notAuthorable: true },
    },
  },
  [T.ProviderJira]: {
    fields: {
      project_keys: f([T.AttrProjectKey], "exact"),
      labels: f([T.AttrLabels], "exact", { addedAttr: T.AttrAddedLabels }),
      status_ids: f([T.AttrStatusID], "exact"),
      keywords: f([], "noop"),
    },
    aliases: {
      projects: f([T.AttrProjectName], "fold", { plainListOnly: true, native: "project_keys" }),
      statuses: f([T.AttrStatusName], "fold", { plainListOnly: true, native: "status_ids" }),
    },
    events: {
      ...ev(["project_keys", "labels"], "issue_created", "issue_labeled"),
      ...ev(["project_keys", "status_ids"], "status_changed"),
      ...ev(["project_keys", "labels", "keywords"], "agent_session_created"),
    },
  },
  [T.ProviderLinear]: {
    fields: {
      team_ids: f([T.AttrTeamID], "exact"),
      project_ids: f([T.AttrProjectID], "exact"),
      labels: f([T.AttrLabels], "fold", { addedAttr: T.AttrAddedLabels }),
      state_ids: f([T.AttrStateID], "exact"),
      assignee_ids: f([T.AttrAssignees], "exact"),
      mentioned_user_ids: f([T.AttrMentioned], "exact"),
      creator_ids: f([T.AttrAuthor], "exact"),
      issue_ids: f([T.AttrIssueID], "exact"),
      keywords: f([], "keyword"),
    },
    aliases: {
      teams: f([T.AttrTeamName], "exact", { plainListOnly: true, native: "team_ids" }),
      projects: f([T.AttrProjectName], "exact", { plainListOnly: true, native: "project_ids" }),
      states: f([T.AttrStateName], "exact", { plainListOnly: true, native: "state_ids" }),
      issues: f([T.AttrIssueNumber], "int", { plainListOnly: true, native: "issue_ids" }),
    },
    events: {
      ...ev(
        ["team_ids", "project_ids", "labels", "state_ids", "assignee_ids", "mentioned_user_ids", "creator_ids"],
        "issue_labeled",
        "issue_created",
        "issue_assigned",
        "issue_state_changed",
      ),
      ...ev(["team_ids", "project_ids", "labels", "state_ids", "issue_ids", "mentioned_user_ids", "creator_ids"], "comment_created"),
      ...ev(["team_ids", "project_ids", "labels", "creator_ids", "keywords"], "agent_session_created"),
    },
  },
  [T.ProviderSlack]: {
    fields: {
      channel_ids: f([T.AttrChannelID], "exact"),
      user_ids: f([T.AttrUserID], "exact"),
      keywords: f([], "keyword"),
      emojis: f([T.AttrEmoji], "emoji"),
      item_user_ids: f([T.AttrItemUserID], "exact"),
    },
    aliases: {
      channels: f([T.AttrChannelName], "channelName", { plainListOnly: true, native: "channel_ids" }),
      users: f([T.AttrUserName, T.AttrUserEmail], "fold", { plainListOnly: true, native: "user_ids" }),
      itemUsers: f([T.AttrItemUserName, T.AttrItemUserEmail], "fold", { plainListOnly: true, native: "item_user_ids" }),
    },
    events: {
      ...ev(["channel_ids", "user_ids", "keywords"], "app_mention", "message_dm", "message_im", "message_mpim", "message_posted"),
      ...ev(["channel_ids", "user_ids", "keywords", "emojis", "item_user_ids"], "reaction_added"),
      ...ev(["channel_ids", "user_ids"], "member_joined_channel"),
    },
  },
  [T.ProviderTeams]: {
    fields: {
      team_ids: f([T.AttrTeamID], "fold", { inOnly: true, exactlyOne: true, required: true }),
      channel_ids: f([T.AttrChannelID], "exact", { inOnly: true, required: true }),
      user_ids: f([T.AttrUserID], "fold"),
      keywords: f([], "keyword"),
    },
    aliases: {
      channels: f([T.AttrChannelName], "channelNameFold", { plainListOnly: true, native: "channel_ids" }),
      users: f([T.AttrUserName], "fold", { plainListOnly: true, native: "user_ids" }),
    },
    events: ev(["team_ids", "channel_ids", "user_ids", "keywords"], "app_mention", "message_posted"),
  },
  [T.ProviderWebhook]: {
    fields: {
      webhook_ids: f([T.AttrWebhookID, T.AttrWebhookName], "exact", { inOnly: true, exactlyOne: true, required: true }),
    },
    aliases: {},
    events: ev(["webhook_ids", KEY_PAYLOAD], T.EventWebhookReceived),
  },
  [T.ProviderSchedule]: {
    fields: { schedule_ids: f([T.AttrAutomationID], "exact", { serverManaged: true }) },
    aliases: {},
    events: ev(["schedule_ids"], T.EventCronFired),
  },
};

/** Providers that never match events authored by the platform itself. */
export const SELF_IGNORED_PROVIDERS = new Set([
  T.ProviderSlack,
  T.ProviderTeams,
  T.ProviderLinear,
  T.ProviderJira,
  T.ProviderGitLab,
  T.ProviderAzureDevOps,
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
      return T.ProviderTeams;
    case "azuredevops":
    case "ado":
      return T.ProviderAzureDevOps;
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
