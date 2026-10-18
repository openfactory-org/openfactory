// Package eventattr names the core.Event.Attributes keys populated by the
// forge and integration packages.
//
// The values mirror the normalized attribute contract owned by
// internal/triggers (attributes.go / ATTRIBUTES.md). They are duplicated here
// so that ingress packages do not depend on the matcher package; keep the two
// in sync.
package eventattr

const (
	Repo          = "repo"           // string: GitHub "owner/name", GitLab project path, Azure DevOps repo UUID
	SourceRepo    = "source_repo"    // string: Azure DevOps source repository UUID
	Labels        = "labels"         // []string: current labels
	AddedLabels   = "added_labels"   // []string: labels added by this event
	Assignees     = "assignees"      // []string
	Author        = "author"         // string
	Mentioned     = "mentioned"      // []string
	BaseBranch    = "base_branch"    // string
	Branch        = "branch"         // string
	PRNumber      = "pr_number"      // int
	Paths         = "paths"          // []string
	Reviewers     = "reviewers"      // []string
	ReviewerTeams = "reviewer_teams" // []string
	ReviewState   = "review_state"   // string
	Conclusion    = "conclusion"     // string
	Workflow      = "workflow"       // string
	CommitState   = "commit_state"   // string
	Context       = "context"        // string
	LinkedPRCount = "linked_pr_count"
	Action        = "action"         // string: GitLab merge request action
	WorkItemType  = "work_item_type" // string

	ChannelID     = "channel_id"
	ChannelName   = "channel_name"
	UserID        = "user_id"
	UserName      = "user_name"
	UserEmail     = "user_email"
	ItemUserID    = "item_user_id"
	ItemUserName  = "item_user_name"
	ItemUserEmail = "item_user_email"
	Emoji         = "emoji"

	TeamID      = "team_id"
	TeamName    = "team_name"
	ProjectID   = "project_id"
	ProjectName = "project_name"
	ProjectKey  = "project_key"
	StateID     = "state_id"
	StateName   = "state_name"
	StatusID    = "status_id"
	StatusName  = "status_name"
	IssueID     = "issue_id"
	IssueNumber = "issue_number"

	Text        = "text"
	ActorIsBot  = "actor_is_bot"
	ActorIsSelf = "actor_is_self"
)

// Additional attributes set by ingress packages that are not filter inputs but
// help the engine route follow-ups.
const (
	// Alias is the factory alias the event is addressed to (string), derived
	// from a "factory:<alias>" label or an @alias mention.
	Alias = "alias"
	// ThreadReply marks a chat message that replies inside an existing thread (bool).
	ThreadReply = "thread_reply"
	// AutoFollowUp is false when the factory disabled autoRespondToThreadReplies:
	// a plain thread reply must then not continue the work item (bool).
	AutoFollowUp = "auto_follow_up"
	// Merged is set on pull_request_closed (bool).
	Merged = "merged"
	// Draft marks draft pull requests (bool).
	Draft = "draft"
	// HeadBranch is a pull request's source branch (string).
	HeadBranch = "head_branch"
	// HeadSHA is the commit an event refers to (string).
	HeadSHA = "head_sha"
	// ChannelType is the Slack channel type: channel, group, im, mpim (string).
	ChannelType = "channel_type"
	// TenantID is the Microsoft Entra tenant of a Teams activity (string).
	TenantID = "tenant_id"
	// SessionID is a tracker agent-session identifier (Linear) (string).
	SessionID = "session_id"
	// Linked reports whether the actor has a linked Open Factory account (bool).
	Linked = "linked"
	// ThreadRef is the review thread of a comment (string, SourceRef.ThreadRef).
	ThreadRef = "thread_ref"

	// Pull request facts for the metrics. They are set on pull request
	// events when the provider sends them.

	// Additions and Deletions are the changed lines of a pull request (int).
	Additions = "additions"
	Deletions = "deletions"
	// PRCreatedAt is when the pull request was opened (RFC 3339 string).
	PRCreatedAt = "pr_created_at"
	// MergedAt is when the pull request merged (RFC 3339 string).
	MergedAt = "merged_at"
	// PROpener is the account that opened the pull request (string).
	PROpener = "pr_opener"
	// PROpenerIsBot reports whether that account is a bot (bool).
	PROpenerIsBot = "pr_opener_is_bot"
)

// Canonical trigger providers.
const (
	ProviderGitHub      = "github"
	ProviderGitLab      = "gitlab"
	ProviderAzureDevOps = "azure_devops"
	ProviderSlack       = "slack"
	ProviderTeams       = "teams"
	ProviderLinear      = "linear"
	ProviderJira        = "jira"
)

// Integration provider identifiers used in core.Integration.Provider.
const (
	IntegrationTeams = "microsoft_teams"
)
