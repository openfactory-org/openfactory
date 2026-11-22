// Names of the Event.attributes keys that the integrations set (port of
// internal/forge/eventattr). Keep in step with the trigger matcher.
export const A = {
  Labels: "labels",
  AddedLabels: "added_labels",
  Assignees: "assignees",
  Author: "author",
  Mentioned: "mentioned",
  ChannelID: "channel_id",
  ChannelName: "channel_name",
  UserID: "user_id",
  UserName: "user_name",
  UserEmail: "user_email",
  ItemUserID: "item_user_id",
  ItemUserName: "item_user_name",
  ItemUserEmail: "item_user_email",
  Emoji: "emoji",
  TeamID: "team_id",
  TeamName: "team_name",
  ProjectID: "project_id",
  ProjectName: "project_name",
  ProjectKey: "project_key",
  StateID: "state_id",
  StateName: "state_name",
  StatusID: "status_id",
  StatusName: "status_name",
  IssueID: "issue_id",
  IssueNumber: "issue_number",
  Text: "text",
  ActorIsBot: "actor_is_bot",
  ActorIsSelf: "actor_is_self",
  ThreadReply: "thread_reply",
  AutoFollowUp: "auto_follow_up",
  ChannelType: "channel_type",
  TenantID: "tenant_id",
  SessionID: "session_id",
  Linked: "linked",
} as const;

export const PROVIDER_SLACK = "slack";
export const PROVIDER_TEAMS = "teams";
export const PROVIDER_LINEAR = "linear";
export const PROVIDER_JIRA = "jira";
export const INTEGRATION_TEAMS = "microsoft_teams";
