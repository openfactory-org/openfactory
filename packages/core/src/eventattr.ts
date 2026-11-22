// Names of the Event.attributes keys that forges, integrations, plugins and
// the schedule and webhook sources set, and the trigger providers. Port of
// internal/forge/eventattr and internal/triggers/attributes.go. The names are
// part of the trigger match contract: the attributes per provider and event
// are listed in apps/control-plane/src/triggers/ATTRIBUTES.md.

export const EVENT_ATTR = {
  // Repository and pull request facts (forges).
  Repo: "repo",
  SourceRepo: "source_repo",
  Labels: "labels",
  AddedLabels: "added_labels",
  Assignees: "assignees",
  Author: "author",
  Mentioned: "mentioned",
  BaseBranch: "base_branch",
  Branch: "branch",
  PRNumber: "pr_number",
  Paths: "paths",
  Reviewers: "reviewers",
  ReviewerTeams: "reviewer_teams",
  ReviewState: "review_state",
  Conclusion: "conclusion",
  Workflow: "workflow",
  CommitState: "commit_state",
  Context: "context",
  LinkedPRCount: "linked_pr_count",
  Action: "action",
  WorkItemType: "work_item_type",

  // Chat (Slack, Teams).
  ChannelID: "channel_id",
  ChannelName: "channel_name",
  UserID: "user_id",
  UserName: "user_name",
  UserEmail: "user_email",
  ItemUserID: "item_user_id",
  ItemUserName: "item_user_name",
  ItemUserEmail: "item_user_email",
  Emoji: "emoji",

  // Trackers (Linear, Jira).
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

  // Inbound webhooks and schedules.
  WebhookID: "webhook_id",
  WebhookName: "webhook_name",
  DeliveryRecordID: "delivery_record_id",
  AutomationID: "automation_id",
  ScheduleName: "schedule_name",
  ScheduledAt: "scheduled_at",

  // All providers.
  Text: "text",
  ActorIsBot: "actor_is_bot",
  ActorIsSelf: "actor_is_self",

  // Attributes that are not filter inputs but route follow-ups.
  /** Factory alias the event is addressed to (string), from a "factory:<alias>" label or an @alias mention. */
  Alias: "alias",
  /** A chat message that replies inside an existing thread (bool). */
  ThreadReply: "thread_reply",
  /** False when the factory disabled autoRespondToThreadReplies (bool). */
  AutoFollowUp: "auto_follow_up",
  /** Set on pull_request_closed (bool). */
  Merged: "merged",
  /** Draft pull requests (bool). */
  Draft: "draft",
  /** A pull request's source branch (string). */
  HeadBranch: "head_branch",
  /** The commit an event refers to (string). */
  HeadSHA: "head_sha",
  /** Slack channel type: channel, group, im, mpim (string). */
  ChannelType: "channel_type",
  /** Microsoft Entra tenant of a Teams activity (string). */
  TenantID: "tenant_id",
  /** Tracker agent-session identifier (Linear) (string). */
  SessionID: "session_id",
  /** Whether the actor has a linked account (bool). */
  Linked: "linked",
  /** Review thread of a comment (string, SourceRef.thread_ref). */
  ThreadRef: "thread_ref",

  // Pull request facts for the metrics.
  Additions: "additions",
  Deletions: "deletions",
  PRCreatedAt: "pr_created_at",
  MergedAt: "merged_at",
  PROpener: "pr_opener",
  PROpenerIsBot: "pr_opener_is_bot",
} as const;

/** The canonical trigger providers (Event.provider). */
export const EVENT_PROVIDER = {
  GitHub: "github",
  GitLab: "gitlab",
  AzureDevOps: "azure_devops",
  Slack: "slack",
  Teams: "teams",
  Linear: "linear",
  Jira: "jira",
  Webhook: "webhook",
  Schedule: "schedule",
} as const;
