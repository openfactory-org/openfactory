// Names of the core.Event attributes set by the forge and integration code.
// Port of internal/forge/eventattr. The values mirror the attribute contract
// of src/triggers (attributes.ts, ATTRIBUTES.md); keep the two in step.

export const Repo = "repo";
export const SourceRepo = "source_repo";
export const Labels = "labels";
export const AddedLabels = "added_labels";
export const Assignees = "assignees";
export const Author = "author";
export const Mentioned = "mentioned";
export const BaseBranch = "base_branch";
export const Branch = "branch";
export const PRNumber = "pr_number";
export const Paths = "paths";
export const Reviewers = "reviewers";
export const ReviewerTeams = "reviewer_teams";
export const ReviewState = "review_state";
export const Conclusion = "conclusion";
export const Workflow = "workflow";
export const CommitState = "commit_state";
export const Context = "context";
export const LinkedPRCount = "linked_pr_count";
export const Action = "action";
export const WorkItemType = "work_item_type";

export const ChannelID = "channel_id";
export const ChannelName = "channel_name";
export const UserID = "user_id";
export const UserName = "user_name";
export const UserEmail = "user_email";
export const ItemUserID = "item_user_id";
export const ItemUserName = "item_user_name";
export const ItemUserEmail = "item_user_email";
export const Emoji = "emoji";

export const TeamID = "team_id";
export const TeamName = "team_name";
export const ProjectID = "project_id";
export const ProjectName = "project_name";
export const ProjectKey = "project_key";
export const StateID = "state_id";
export const StateName = "state_name";
export const StatusID = "status_id";
export const StatusName = "status_name";
export const IssueID = "issue_id";
export const IssueNumber = "issue_number";

export const Text = "text";
export const ActorIsBot = "actor_is_bot";
export const ActorIsSelf = "actor_is_self";

// Attributes that are not filter inputs but route follow-ups.

/** Factory alias the event is addressed to (string), from a "factory:<alias>" label or an @alias mention. */
export const Alias = "alias";
/** A chat message that replies inside an existing thread (bool). */
export const ThreadReply = "thread_reply";
/** False when the factory disabled autoRespondToThreadReplies (bool). */
export const AutoFollowUp = "auto_follow_up";
/** Set on pull_request_closed (bool). */
export const Merged = "merged";
/** Draft pull requests (bool). */
export const Draft = "draft";
/** A pull request's source branch (string). */
export const HeadBranch = "head_branch";
/** The commit an event refers to (string). */
export const HeadSHA = "head_sha";
/** Slack channel type: channel, group, im, mpim (string). */
export const ChannelType = "channel_type";
/** Microsoft Entra tenant of a Teams activity (string). */
export const TenantID = "tenant_id";
/** Tracker agent-session identifier (Linear) (string). */
export const SessionID = "session_id";
/** Whether the actor has a linked account (bool). */
export const Linked = "linked";
/** Review thread of a comment (string, SourceRef.thread_ref). */
export const ThreadRef = "thread_ref";

// Pull request facts for the metrics.
export const Additions = "additions";
export const Deletions = "deletions";
export const PRCreatedAt = "pr_created_at";
export const MergedAt = "merged_at";
export const PROpener = "pr_opener";
export const PROpenerIsBot = "pr_opener_is_bot";

// Canonical trigger providers.
export const ProviderGitHub = "github";
export const ProviderGitLab = "gitlab";
export const ProviderAzureDevOps = "azure_devops";
export const ProviderSlack = "slack";
export const ProviderTeams = "teams";
export const ProviderLinear = "linear";
export const ProviderJira = "jira";

/** Integration provider identifier of Teams in core.Integration.provider. */
export const IntegrationTeams = "microsoft_teams";
