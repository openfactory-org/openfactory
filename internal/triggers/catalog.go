package triggers

import "sort"

// cmpKind selects how filter values are normalized and compared against
// event attribute values.
type cmpKind int

const (
	cmpExact           cmpKind = iota // exact, case-sensitive string equality
	cmpFold                           // case-insensitive string equality
	cmpInt                            // integer equality (filter values must be positive integers)
	cmpGlob                           // repository-relative path glob (supports **)
	cmpKeyword                        // case-insensitive substring of the event text
	cmpEmoji                          // emoji name, colons and skin-tone suffixes ignored
	cmpBranch                         // branch name, "refs/heads/" ignored
	cmpChannelName                    // channel name, surrounding whitespace and leading "#" ignored
	cmpChannelNameFold                // as cmpChannelName, and case-insensitive (Teams channel names)
	cmpNoop                           // accepted for compatibility, never restricts
)

// fieldSpec describes one filter key of a provider.
type fieldSpec struct {
	attrs         []string // event attributes compared (union of values)
	cmp           cmpKind
	addedAttr     string   // attribute used for `in` instead of attrs when present on the event
	inOnly        bool     // only `in` (plain list or {in: [...]}) is allowed
	exactlyOne    bool     // `in` must list exactly one value
	required      bool     // the key must be present with at least one `in` value
	plainListOnly bool     // name aliases: plain lists only, no matcher objects
	serverManaged bool     // rejected when authoring; honored when matching
	enum          []string // allowed (normalized) values, if restricted
	native        string   // for aliases: the canonical key this alias stands for
}

type eventSpec struct {
	keys          []string
	notAuthorable bool // present in the catalog but rejected in definition files
}

type providerSpec struct {
	fields  map[string]fieldSpec
	aliases map[string]fieldSpec // alias key -> spec (spec.native = canonical key)
	events  map[string]eventSpec
}

func events(keys []string, names ...string) map[string]eventSpec {
	m := make(map[string]eventSpec, len(names))
	for _, n := range names {
		m[n] = eventSpec{keys: keys}
	}
	return m
}

func merge(ms ...map[string]eventSpec) map[string]eventSpec {
	out := map[string]eventSpec{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func keys(k ...string) []string { return k }

// Special keys handled outside the generic matcher.
const (
	keySkipBots = "skip_bots"
	keyPayload  = "payload"
)

// Skip-bots policies for GitHub triggers.
const (
	SkipBotsAll  = "all"
	SkipBotsSelf = "self"
	SkipBotsNone = "none"
)

// GitHubCIEvents are GitHub events whose label/author constraints refer to the
// single linked pull request.
var githubCIEvents = map[string]bool{"check_suite_completed": true, "workflow_run_completed": true}

var catalog = map[string]providerSpec{
	ProviderGitHub: {
		fields: map[string]fieldSpec{
			"repos":          {attrs: keys(AttrRepo), cmp: cmpFold},
			"labels":         {attrs: keys(AttrLabels), addedAttr: AttrAddedLabels, cmp: cmpFold},
			"assignees":      {attrs: keys(AttrAssignees), cmp: cmpFold},
			"authors":        {attrs: keys(AttrAuthor), cmp: cmpFold},
			"keywords":       {cmp: cmpKeyword},
			"mentioned":      {attrs: keys(AttrMentioned), cmp: cmpFold},
			"base_branches":  {attrs: keys(AttrBaseBranch), cmp: cmpBranch},
			"pr_numbers":     {attrs: keys(AttrPRNumber), cmp: cmpInt},
			"paths":          {attrs: keys(AttrPaths), cmp: cmpGlob},
			"reviewers":      {attrs: keys(AttrReviewers), cmp: cmpFold},
			"reviewer_teams": {attrs: keys(AttrReviewerTeams), cmp: cmpFold},
			"review_states":  {attrs: keys(AttrReviewState), cmp: cmpFold, enum: []string{"approved", "changes_requested", "commented"}},
			"branches":       {attrs: keys(AttrBranch), cmp: cmpBranch},
			"conclusions":    {attrs: keys(AttrConclusion), cmp: cmpFold},
			"workflows":      {attrs: keys(AttrWorkflow), cmp: cmpExact},
			"states":         {attrs: keys(AttrCommitState), cmp: cmpFold, enum: []string{"success", "failure", "error"}},
			"contexts":       {attrs: keys(AttrContext), cmp: cmpFold},
		},
		aliases: map[string]fieldSpec{
			"baseBranches": {attrs: keys(AttrBaseBranch), cmp: cmpBranch, native: "base_branches"},
			"prNumbers":    {attrs: keys(AttrPRNumber), cmp: cmpInt, native: "pr_numbers"},
		},
		events: merge(
			events(keys("repos", "labels", "assignees", "authors", "keywords", keySkipBots), "issue_created"),
			events(keys("repos", "labels", "assignees", "authors", keySkipBots), "issue_labeled", "issue_assigned", "pull_request_assigned"),
			events(keys("repos", "labels", "authors", "keywords", keySkipBots), "issue_commented", "pull_request_commented"),
			events(keys("repos", "mentioned", "labels", "authors", "keywords", keySkipBots), "issue_mentioned", "pull_request_mentioned"),
			events(keys("repos", "base_branches", "pr_numbers", "paths", "assignees", "authors", "labels", "keywords", keySkipBots),
				"pull_request_opened", "pull_request_ready", "pull_request_reopened"),
			events(keys("repos", "base_branches", "pr_numbers", "paths", "assignees", "authors", "labels", keySkipBots),
				"pull_request_closed", "pull_request_dequeued", "pull_request_enqueued", "pull_request_labeled",
				"pull_request_merged", "pull_request_synchronized"),
			events(keys("repos", "reviewers", "reviewer_teams", "authors", keySkipBots), "pull_request_review_requested"),
			events(keys("repos", "mentioned", "labels", "review_states", "authors", "keywords", keySkipBots), "pull_request_review_submitted"),
			events(keys("repos", "branches", "paths", keySkipBots), "push"),
			events(keys("repos", "conclusions", "branches", "labels", "authors", keySkipBots), "check_suite_completed"),
			events(keys("repos", "conclusions", "branches", "workflows", "labels", "authors", keySkipBots), "workflow_run_completed"),
			events(keys("repos", "states", "contexts", keySkipBots), "commit_status"),
			events(keys("repos", keySkipBots), "check_run_rerequested", "check_suite_rerequested"),
		),
	},
	ProviderGitLab: {
		fields: map[string]fieldSpec{
			"repos":         {attrs: keys(AttrRepo), cmp: cmpFold},
			"actions":       {attrs: keys(AttrAction), cmp: cmpFold},
			"base_branches": {attrs: keys(AttrBaseBranch), cmp: cmpBranch},
			"branches":      {attrs: keys(AttrBranch), cmp: cmpBranch},
			"mentioned":     {attrs: keys(AttrMentioned), cmp: cmpFold, serverManaged: true},
		},
		events: merge(
			events(keys("repos", "mentioned"), "bot_mentioned"),
			events(keys("repos", "actions", "base_branches"), "merge_request"),
			events(keys("repos", "branches"), "push"),
		),
	},
	ProviderAzureDevOps: {
		fields: map[string]fieldSpec{
			"repos":           {attrs: keys(AttrRepo), cmp: cmpFold},
			"base_branches":   {attrs: keys(AttrBaseBranch), cmp: cmpBranch},
			"branches":        {attrs: keys(AttrBranch), cmp: cmpBranch},
			"source_repos":    {attrs: keys(AttrSourceRepo), cmp: cmpFold, serverManaged: true},
			"mentioned":       {attrs: keys(AttrMentioned), cmp: cmpFold},
			"work_item_types": {attrs: keys(AttrWorkItemType), cmp: cmpExact},
			"labels":          {attrs: keys(AttrLabels), addedAttr: AttrAddedLabels, cmp: cmpFold},
			"assignees":       {attrs: keys(AttrAssignees), cmp: cmpFold},
			"authors":         {attrs: keys(AttrAuthor), cmp: cmpFold},
		},
		events: merge(
			events(keys("repos", "base_branches", "source_repos"),
				"pull_request_created", "pull_request_updated", "pull_request_commented", "pull_request_closed", "pull_request_merged"),
			events(keys("repos", "mentioned"), "pull_request_mentioned"),
			events(keys("work_item_types", "labels", "assignees", "authors"), "work_item_created", "work_item_assigned", "work_item_labeled"),
			events(keys("mentioned"), "work_item_mentioned"),
			map[string]eventSpec{"push": {keys: keys("repos", "branches"), notAuthorable: true}},
		),
	},
	ProviderJira: {
		fields: map[string]fieldSpec{
			"project_keys": {attrs: keys(AttrProjectKey), cmp: cmpExact},
			"labels":       {attrs: keys(AttrLabels), addedAttr: AttrAddedLabels, cmp: cmpExact},
			"status_ids":   {attrs: keys(AttrStatusID), cmp: cmpExact},
			// Accepted for compatibility on agent sessions; never restricts.
			"keywords": {cmp: cmpNoop},
		},
		aliases: map[string]fieldSpec{
			"projects": {attrs: keys(AttrProjectName), cmp: cmpFold, plainListOnly: true, native: "project_keys"},
			"statuses": {attrs: keys(AttrStatusName), cmp: cmpFold, plainListOnly: true, native: "status_ids"},
		},
		events: merge(
			events(keys("project_keys", "labels"), "issue_created", "issue_labeled"),
			events(keys("project_keys", "status_ids"), "status_changed"),
			events(keys("project_keys", "labels", "keywords"), "agent_session_created"),
		),
	},
	ProviderLinear: {
		fields: map[string]fieldSpec{
			"team_ids":           {attrs: keys(AttrTeamID), cmp: cmpExact},
			"project_ids":        {attrs: keys(AttrProjectID), cmp: cmpExact},
			"labels":             {attrs: keys(AttrLabels), addedAttr: AttrAddedLabels, cmp: cmpFold},
			"state_ids":          {attrs: keys(AttrStateID), cmp: cmpExact},
			"assignee_ids":       {attrs: keys(AttrAssignees), cmp: cmpExact},
			"mentioned_user_ids": {attrs: keys(AttrMentioned), cmp: cmpExact},
			"creator_ids":        {attrs: keys(AttrAuthor), cmp: cmpExact},
			"issue_ids":          {attrs: keys(AttrIssueID), cmp: cmpExact},
			"keywords":           {cmp: cmpKeyword},
		},
		aliases: map[string]fieldSpec{
			"teams":    {attrs: keys(AttrTeamName), cmp: cmpExact, plainListOnly: true, native: "team_ids"},
			"projects": {attrs: keys(AttrProjectName), cmp: cmpExact, plainListOnly: true, native: "project_ids"},
			"states":   {attrs: keys(AttrStateName), cmp: cmpExact, plainListOnly: true, native: "state_ids"},
			"issues":   {attrs: keys(AttrIssueNumber), cmp: cmpInt, plainListOnly: true, native: "issue_ids"},
		},
		events: merge(
			events(keys("team_ids", "project_ids", "labels", "state_ids", "assignee_ids", "mentioned_user_ids", "creator_ids"),
				"issue_labeled", "issue_created", "issue_assigned", "issue_state_changed"),
			events(keys("team_ids", "project_ids", "labels", "state_ids", "issue_ids", "mentioned_user_ids", "creator_ids"), "comment_created"),
			events(keys("team_ids", "project_ids", "labels", "creator_ids", "keywords"), "agent_session_created"),
		),
	},
	ProviderSlack: {
		fields: map[string]fieldSpec{
			"channel_ids":   {attrs: keys(AttrChannelID), cmp: cmpExact},
			"user_ids":      {attrs: keys(AttrUserID), cmp: cmpExact},
			"keywords":      {cmp: cmpKeyword},
			"emojis":        {attrs: keys(AttrEmoji), cmp: cmpEmoji},
			"item_user_ids": {attrs: keys(AttrItemUserID), cmp: cmpExact},
		},
		aliases: map[string]fieldSpec{
			"channels":  {attrs: keys(AttrChannelName), cmp: cmpChannelName, plainListOnly: true, native: "channel_ids"},
			"users":     {attrs: keys(AttrUserName, AttrUserEmail), cmp: cmpFold, plainListOnly: true, native: "user_ids"},
			"itemUsers": {attrs: keys(AttrItemUserName, AttrItemUserEmail), cmp: cmpFold, plainListOnly: true, native: "item_user_ids"},
		},
		events: merge(
			events(keys("channel_ids", "user_ids", "keywords"), "app_mention", "message_dm", "message_im", "message_mpim", "message_posted"),
			events(keys("channel_ids", "user_ids", "keywords", "emojis", "item_user_ids"), "reaction_added"),
			events(keys("channel_ids", "user_ids"), "member_joined_channel"),
		),
	},
	ProviderTeams: {
		fields: map[string]fieldSpec{
			"team_ids":    {attrs: keys(AttrTeamID), cmp: cmpFold, inOnly: true, exactlyOne: true, required: true},
			"channel_ids": {attrs: keys(AttrChannelID), cmp: cmpExact, inOnly: true, required: true},
			"user_ids":    {attrs: keys(AttrUserID), cmp: cmpFold},
			"keywords":    {cmp: cmpKeyword},
		},
		aliases: map[string]fieldSpec{
			"channels": {attrs: keys(AttrChannelName), cmp: cmpChannelNameFold, plainListOnly: true, native: "channel_ids"},
			"users":    {attrs: keys(AttrUserName), cmp: cmpFold, plainListOnly: true, native: "user_ids"},
		},
		events: events(keys("team_ids", "channel_ids", "user_ids", "keywords"), "app_mention", "message_posted"),
	},
	ProviderWebhook: {
		fields: map[string]fieldSpec{
			"webhook_ids": {attrs: keys(AttrWebhookID, AttrWebhookName), cmp: cmpExact, inOnly: true, exactlyOne: true, required: true},
		},
		events: events(keys("webhook_ids", keyPayload), EventWebhookReceived),
	},
	ProviderSchedule: {
		fields: map[string]fieldSpec{
			"schedule_ids": {attrs: keys(AttrAutomationID), cmp: cmpExact, serverManaged: true},
		},
		events: events(keys("schedule_ids"), EventCronFired),
	},
}

// selfIgnoredProviders never match events authored by Open Factory itself.
var selfIgnoredProviders = map[string]bool{
	ProviderSlack: true, ProviderTeams: true, ProviderLinear: true,
	ProviderJira: true, ProviderGitLab: true, ProviderAzureDevOps: true,
}

// Providers returns the canonical trigger provider names, sorted.
func Providers() []string {
	out := make([]string, 0, len(catalog))
	for p := range catalog {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// EventInfo describes one provider event for catalog endpoints.
type EventInfo struct {
	Event        string   `json:"event"`
	FilterFields []string `json:"filter_fields"`
	Aliases      []string `json:"aliases,omitempty"`
	Authorable   bool     `json:"authorable"`
}

// Events returns the event catalog for a provider (sorted by event name), or
// nil for an unknown provider. Server-managed keys are omitted.
func Events(provider string) []EventInfo {
	ps, ok := catalog[canonicalProvider(provider)]
	if !ok {
		return nil
	}
	out := make([]EventInfo, 0, len(ps.events))
	for name, es := range ps.events {
		info := EventInfo{Event: name, Authorable: !es.notAuthorable}
		for _, k := range es.keys {
			if f, ok := ps.fields[k]; ok && f.serverManaged {
				continue
			}
			info.FilterFields = append(info.FilterFields, k)
		}
		for alias, as := range ps.aliases {
			if contains(es.keys, as.native) {
				info.Aliases = append(info.Aliases, alias)
			}
		}
		sort.Strings(info.Aliases)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Event < out[j].Event })
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
