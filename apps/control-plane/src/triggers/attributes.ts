// Normalized core.Event attribute keys for trigger matching. Port of
// internal/triggers/attributes.go; the contract per provider and event is in
// ATTRIBUTES.md next to this file.

export const AttrRepo = "repo";
export const AttrSourceRepo = "source_repo";
export const AttrLabels = "labels";
export const AttrAddedLabels = "added_labels";
export const AttrAssignees = "assignees";
export const AttrAuthor = "author";
export const AttrMentioned = "mentioned";
export const AttrBaseBranch = "base_branch";
export const AttrBranch = "branch";
export const AttrPRNumber = "pr_number";
export const AttrPaths = "paths";
export const AttrReviewers = "reviewers";
export const AttrReviewerTeams = "reviewer_teams";
export const AttrReviewState = "review_state";
export const AttrConclusion = "conclusion";
export const AttrWorkflow = "workflow";
export const AttrCommitState = "commit_state";
export const AttrContext = "context";
export const AttrLinkedPRCount = "linked_pr_count";
export const AttrAction = "action";
export const AttrWorkItemType = "work_item_type";
export const AttrChannelID = "channel_id";
export const AttrChannelName = "channel_name";
export const AttrUserID = "user_id";
export const AttrUserName = "user_name";
export const AttrUserEmail = "user_email";
export const AttrItemUserID = "item_user_id";
export const AttrItemUserName = "item_user_name";
export const AttrItemUserEmail = "item_user_email";
export const AttrEmoji = "emoji";
export const AttrTeamID = "team_id";
export const AttrTeamName = "team_name";
export const AttrProjectID = "project_id";
export const AttrProjectName = "project_name";
export const AttrProjectKey = "project_key";
export const AttrStateID = "state_id";
export const AttrStateName = "state_name";
export const AttrStatusID = "status_id";
export const AttrStatusName = "status_name";
export const AttrIssueID = "issue_id";
export const AttrIssueNumber = "issue_number";
export const AttrWebhookID = "webhook_id";
export const AttrWebhookName = "webhook_name";
export const AttrDeliveryRecordID = "delivery_record_id";
export const AttrAutomationID = "automation_id";
export const AttrScheduleName = "schedule_name";
export const AttrScheduledAt = "scheduled_at";
export const AttrText = "text";
export const AttrActorIsBot = "actor_is_bot";
export const AttrActorIsSelf = "actor_is_self";

export const ProviderGitHub = "github";
export const ProviderGitLab = "gitlab";
export const ProviderAzureDevOps = "azure_devops";
export const ProviderSlack = "slack";
export const ProviderTeams = "teams";
export const ProviderLinear = "linear";
export const ProviderJira = "jira";
export const ProviderWebhook = "webhook";
export const ProviderSchedule = "schedule";

export const EventCronFired = "cron_fired";
export const EventWebhookReceived = "received";
