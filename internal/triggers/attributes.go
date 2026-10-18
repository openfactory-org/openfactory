// Package triggers implements automation matching: provider/event/filter
// evaluation for every trigger provider, the webhook payload pattern matcher,
// and the cron scheduler that emits schedule events.
//
// Integrations normalize inbound activity into core.Event values and populate
// core.Event.Attributes with the keys defined in this file. The full contract,
// per provider and event, is documented in ATTRIBUTES.md next to this file.
package triggers

// Normalized core.Event.Attributes keys. Values are strings, string lists
// ([]string or []any of strings), integers, or booleans as documented on each
// constant. Integrations must populate only the attributes they actually know;
// a missing attribute fails any inclusion (`in`) matcher on that key and passes
// any exclusion (`not_in`) matcher.
const (
	// AttrRepo is the repository the event belongs to (string).
	// GitHub: "owner/name". GitLab: full project path including subgroups
	// ("acme/platform/api"). Azure DevOps: repository UUID.
	AttrRepo = "repo"
	// AttrSourceRepo is the repository holding a pull request's source branch
	// (Azure DevOps, string UUID).
	AttrSourceRepo = "source_repo"
	// AttrLabels is the current label set of the issue / pull request / work
	// item ([]string, label names).
	AttrLabels = "labels"
	// AttrAddedLabels lists labels newly added by this event ([]string). On
	// *_labeled events an `in` labels matcher checks this set (when present)
	// while `not_in` checks AttrLabels. Include labels present at creation.
	AttrAddedLabels = "added_labels"
	// AttrAssignees lists assignee identities ([]string). GitHub logins, Linear
	// user UUIDs, Azure DevOps unique names (e-mail). On assignment events,
	// the newly assigned identities.
	AttrAssignees = "assignees"
	// AttrAuthor is the author/creator identity (string). GitHub login of the
	// issue/PR author (for CI events: the linked PR's author), Linear creator
	// UUID (issue creator, comment author or session creator by event),
	// Azure DevOps unique name.
	AttrAuthor = "author"
	// AttrMentioned lists identities mentioned by the new content ([]string).
	// GitHub logins, org/team slugs or app slugs; GitLab bot usernames; Azure
	// DevOps identity UUIDs; Linear user UUIDs or profile slugs.
	AttrMentioned = "mentioned"
	// AttrBaseBranch is the branch a pull/merge request targets (string, no
	// "refs/heads/" prefix).
	AttrBaseBranch = "base_branch"
	// AttrBranch is the pushed branch or a CI run's head branch (string).
	AttrBranch = "branch"
	// AttrPRNumber is the pull request number (integer).
	AttrPRNumber = "pr_number"
	// AttrPaths lists repository-relative paths touched by the change
	// ([]string, forward slashes, no leading "/").
	AttrPaths = "paths"
	// AttrReviewers lists logins review was requested from ([]string).
	AttrReviewers = "reviewers"
	// AttrReviewerTeams lists team slugs review was requested from ([]string).
	AttrReviewerTeams = "reviewer_teams"
	// AttrReviewState is the submitted review state: approved,
	// changes_requested or commented (string).
	AttrReviewState = "review_state"
	// AttrConclusion is a check suite / workflow run conclusion (string).
	AttrConclusion = "conclusion"
	// AttrWorkflow is the GitHub Actions workflow name (string).
	AttrWorkflow = "workflow"
	// AttrCommitState is a commit status state: success, failure, error or
	// pending (string).
	AttrCommitState = "commit_state"
	// AttrContext is a commit status context name, e.g. "ci/build" (string).
	AttrContext = "context"
	// AttrLinkedPRCount is the number of pull requests linked to a CI event
	// (integer). Label/author constraints on CI events require exactly one.
	AttrLinkedPRCount = "linked_pr_count"
	// AttrAction is the GitLab merge request action: open, update, close,
	// reopen, merge, approved... (string).
	AttrAction = "action"
	// AttrWorkItemType is the Azure DevOps work item type, e.g. "Bug" (string).
	AttrWorkItemType = "work_item_type"

	// AttrChannelID is a chat conversation ID (string): Slack conversation ID
	// (C…, D…, G…), Teams channel conversation ID ("19:…@thread.tacv2").
	AttrChannelID = "channel_id"
	// AttrChannelName is the Slack or Teams channel name without the leading "#".
	AttrChannelName = "channel_name"
	// AttrUserID is the acting chat user's ID (string): Slack user ID, or the
	// Microsoft Entra object UUID for Teams. For reactions, the reacting user.
	AttrUserID = "user_id"
	// AttrUserName is the acting Slack user's username, or the Teams display
	// name (string).
	AttrUserName = "user_name"
	// AttrUserEmail is the acting Slack user's e-mail address (string).
	AttrUserEmail = "user_email"
	// AttrItemUserID is the Slack user ID of the reacted-to message's author.
	AttrItemUserID = "item_user_id"
	// AttrItemUserName is the username of the reacted-to message's author.
	AttrItemUserName = "item_user_name"
	// AttrItemUserEmail is the e-mail of the reacted-to message's author.
	AttrItemUserEmail = "item_user_email"
	// AttrEmoji is the reaction emoji name (string, colons optional).
	AttrEmoji = "emoji"

	// AttrTeamID is a tracker/chat team ID (string): Linear team UUID or
	// Microsoft Graph team UUID.
	AttrTeamID = "team_id"
	// AttrTeamName is the Linear team name (string).
	AttrTeamName = "team_name"
	// AttrProjectID is the Linear project UUID (string).
	AttrProjectID = "project_id"
	// AttrProjectName is the Linear or Jira project name (string).
	AttrProjectName = "project_name"
	// AttrProjectKey is the Jira project key, e.g. "ENG" (string).
	AttrProjectKey = "project_key"
	// AttrStateID is the Linear workflow state UUID (string). On
	// issue_state_changed, the state transitioned into.
	AttrStateID = "state_id"
	// AttrStateName is the Linear workflow state name (string).
	AttrStateName = "state_name"
	// AttrStatusID is the Jira status ID (string).
	AttrStatusID = "status_id"
	// AttrStatusName is the Jira status name (string). On status_changed, the
	// new status.
	AttrStatusName = "status_name"
	// AttrIssueID is the Linear issue UUID (string).
	AttrIssueID = "issue_id"
	// AttrIssueNumber is the Linear issue number within its team (integer).
	AttrIssueNumber = "issue_number"

	// AttrWebhookID is the custom webhook UID a delivery arrived on (string).
	AttrWebhookID = "webhook_id"
	// AttrWebhookName is the custom webhook's name (string). A webhook_ids
	// filter entry matches either the webhook UID or its name.
	AttrWebhookName = "webhook_name"
	// AttrDeliveryRecordID is the WebhookDelivery record ID (string).
	AttrDeliveryRecordID = "delivery_record_id"

	// AttrAutomationID is the automation a schedule event belongs to (string).
	AttrAutomationID = "automation_id"
	// AttrScheduleName is the name of the schedule that fired (string; empty
	// for the unnamed schedule).
	AttrScheduleName = "schedule_name"
	// AttrScheduledAt is the RFC 3339 UTC time the schedule was due (string).
	AttrScheduledAt = "scheduled_at"

	// AttrText optionally carries the searchable text for keyword filters
	// when core.Event.Text is empty (string).
	AttrText = "text"
	// AttrActorIsBot marks bot-authored events (bool). Equivalent to
	// core.Actor.IsBot; either one marks the event as bot-authored.
	AttrActorIsBot = "actor_is_bot"
	// AttrActorIsSelf marks events authored by Open Factory's own bot or
	// service accounts (bool). Such events never match Slack, Teams, Linear,
	// Jira, GitLab or Azure DevOps triggers; for GitHub they are governed by
	// skip_bots (skipped by "all" and "self").
	AttrActorIsSelf = "actor_is_self"
)

// Canonical provider names (core.Trigger.Provider / core.Event.Provider).
const (
	ProviderGitHub      = "github"
	ProviderGitLab      = "gitlab"
	ProviderAzureDevOps = "azure_devops"
	ProviderSlack       = "slack"
	ProviderTeams       = "teams"
	ProviderLinear      = "linear"
	ProviderJira        = "jira"
	ProviderWebhook     = "webhook"
	ProviderSchedule    = "schedule"
)

// Event types emitted by the scheduler and webhook ingress.
const (
	EventCronFired       = "cron_fired"
	EventWebhookReceived = "received"
)
