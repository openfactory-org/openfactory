package triggers

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

type attrs = map[string]any

func mkEvent(provider, typ string, a attrs) *core.Event {
	return &core.Event{Provider: provider, Type: typ, Attributes: a}
}

func mkTrigger(provider, event, filter string) core.Trigger {
	t := core.Trigger{Provider: provider, Event: event}
	if filter != "" {
		t.Filter = json.RawMessage(filter)
	}
	return t
}

type matchCase struct {
	name    string
	trigger core.Trigger
	event   *core.Event
	want    bool
	wantErr bool
}

func runMatchCases(t *testing.T, cases []matchCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MatchTrigger(tc.event, tc.trigger)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("MatchTrigger = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchGitHub(t *testing.T) {
	prAttrs := func(extra attrs) attrs {
		a := attrs{
			AttrRepo: "acme/api", AttrBaseBranch: "main", AttrPRNumber: 42,
			AttrLabels: []string{"bug", "Factory-Ready"}, AttrAuthor: "alex",
			AttrAssignees: []any{"sam"}, AttrPaths: []string{"src/app/main.go", "docs/readme.md"},
		}
		for k, v := range extra {
			a[k] = v
		}
		return a
	}
	pr := func(extra attrs) *core.Event { return mkEvent("github", "pull_request_opened", prAttrs(extra)) }
	bot := pr(nil)
	bot.Actor.IsBot = true
	self := pr(attrs{AttrActorIsSelf: true})

	runMatchCases(t, []matchCase{
		{"no filter", mkTrigger("github", "pull_request_opened", ""), pr(nil), true, false},
		{"wrong provider", mkTrigger("gitlab", "push", ""), pr(nil), false, false},
		{"wrong event", mkTrigger("github", "pull_request_closed", ""), pr(nil), false, false},
		{"repo match case-insensitive", mkTrigger("github", "pull_request_opened", `{"repos":["ACME/api"]}`), pr(nil), true, false},
		{"repo mismatch", mkTrigger("github", "pull_request_opened", `{"repos":["acme/web"]}`), pr(nil), false, false},
		{"AND of keys", mkTrigger("github", "pull_request_opened", `{"repos":["acme/api"],"base_branches":["develop"]}`), pr(nil), false, false},
		{"any-of list", mkTrigger("github", "pull_request_opened", `{"base_branches":["develop","main"]}`), pr(nil), true, false},
		{"base branch refs/heads normalized", mkTrigger("github", "pull_request_opened", `{"base_branches":["refs/heads/main"]}`), pr(nil), true, false},
		{"labels in matcher", mkTrigger("github", "pull_request_opened", `{"labels":{"in":["factory-ready"]}}`), pr(nil), true, false},
		{"labels not_in excludes", mkTrigger("github", "pull_request_opened", `{"labels":{"not_in":["bug"]}}`), pr(nil), false, false},
		{"labels not_in passes", mkTrigger("github", "pull_request_opened", `{"labels":{"not_in":["wip"]}}`), pr(nil), true, false},
		{"labels in + not_in", mkTrigger("github", "pull_request_opened", `{"labels":{"in":["bug"],"not_in":["wip"]}}`), pr(nil), true, false},
		{"empty list no restriction", mkTrigger("github", "pull_request_opened", `{"labels":[],"repos":null,"authors":{}}`), pr(nil), true, false},
		{"pr_numbers int", mkTrigger("github", "pull_request_opened", `{"pr_numbers":[42]}`), pr(nil), true, false},
		{"pr_numbers float attr", mkTrigger("github", "pull_request_opened", `{"pr_numbers":[42]}`), pr(attrs{AttrPRNumber: float64(42)}), true, false},
		{"pr_numbers mismatch", mkTrigger("github", "pull_request_opened", `{"pr_numbers":[7]}`), pr(nil), false, false},
		{"alias prNumbers", mkTrigger("github", "pull_request_opened", `{"prNumbers":{"not_in":[42]}}`), pr(nil), false, false},
		{"alias baseBranches", mkTrigger("github", "pull_request_opened", `{"baseBranches":["main"]}`), pr(nil), true, false},
		{"alias and native together", mkTrigger("github", "pull_request_opened", `{"baseBranches":["main"],"base_branches":["main"]}`), pr(nil), false, true},
		{"paths glob **", mkTrigger("github", "pull_request_opened", `{"paths":["src/**"]}`), pr(nil), true, false},
		{"paths glob no match", mkTrigger("github", "pull_request_opened", `{"paths":["lib/**"]}`), pr(nil), false, false},
		{"paths glob star segment", mkTrigger("github", "pull_request_opened", `{"paths":["docs/*.md"]}`), pr(nil), true, false},
		{"paths not_in excludes any", mkTrigger("github", "pull_request_opened", `{"paths":{"not_in":["docs/**"]}}`), pr(nil), false, false},
		{"authors login fold", mkTrigger("github", "pull_request_opened", `{"authors":["Alex"]}`), pr(nil), true, false},
		{"assignees []any", mkTrigger("github", "pull_request_opened", `{"assignees":["sam"]}`), pr(nil), true, false},
		{"keywords substring CI", mkTrigger("github", "pull_request_opened", `{"keywords":["RETEST"]}`), withText(pr(nil), "please retest this"), true, false},
		{"keywords no text", mkTrigger("github", "pull_request_opened", `{"keywords":["retest"]}`), pr(nil), false, false},
		{"keywords text attr fallback", mkTrigger("github", "pull_request_opened", `{"keywords":["retest"]}`), pr(attrs{AttrText: "Retest pls"}), true, false},
		{"keywords not_in", mkTrigger("github", "pull_request_opened", `{"keywords":{"not_in":["skip"]}}`), withText(pr(nil), "[SKIP] ci"), false, false},
		{"missing attr fails in", mkTrigger("github", "pull_request_opened", `{"base_branches":["main"]}`), mkEvent("github", "pull_request_opened", attrs{}), false, false},
		{"missing attr passes not_in", mkTrigger("github", "pull_request_opened", `{"base_branches":{"not_in":["main"]}}`), mkEvent("github", "pull_request_opened", nil), true, false},
		{"skip_bots default all skips bot", mkTrigger("github", "pull_request_opened", ""), bot, false, false},
		{"skip_bots default all skips attr bot", mkTrigger("github", "pull_request_opened", ""), pr(attrs{AttrActorIsBot: true}), false, false},
		{"skip_bots default skips self", mkTrigger("github", "pull_request_opened", ""), self, false, false},
		{"skip_bots self admits third-party bot", mkTrigger("github", "pull_request_opened", `{"skip_bots":"self"}`), bot, true, false},
		{"skip_bots self skips self", mkTrigger("github", "pull_request_opened", `{"skip_bots":"self"}`), self, false, false},
		{"skip_bots none admits self", mkTrigger("github", "pull_request_opened", `{"skip_bots":"none"}`), self, true, false},
		{"skip_bots invalid", mkTrigger("github", "pull_request_opened", `{"skip_bots":"some"}`), pr(nil), false, true},
		{"skip_bots list invalid", mkTrigger("github", "pull_request_opened", `{"skip_bots":["all"]}`), pr(nil), false, true},
		{"unknown key", mkTrigger("github", "pull_request_opened", `{"color":["red"]}`), pr(nil), false, true},
		{"key not for event", mkTrigger("github", "push", `{"labels":["x"]}`), mkEvent("github", "push", nil), false, true},
		{"unknown operator", mkTrigger("github", "pull_request_opened", `{"labels":{"any":["x"]}}`), pr(nil), false, true},
		{"scalar value rejected", mkTrigger("github", "pull_request_opened", `{"labels":"bug"}`), pr(nil), false, true},
		{"in and not_in conflict", mkTrigger("github", "pull_request_opened", `{"labels":{"in":["bug"],"not_in":["BUG"]}}`), pr(nil), false, true},
		{"pr_numbers string rejected", mkTrigger("github", "pull_request_opened", `{"pr_numbers":["42"]}`), pr(nil), false, true},
		{"filter not object", mkTrigger("github", "pull_request_opened", `[1]`), pr(nil), false, true},
		{"unsupported attr type fails closed", mkTrigger("github", "pull_request_opened", `{"repos":["acme/api"]}`), pr(attrs{AttrRepo: map[string]any{"x": 1}}), false, true},
	})
}

func withText(ev *core.Event, s string) *core.Event { ev.Text = s; return ev }

func TestMatchGitHubSpecialEvents(t *testing.T) {
	labeled := func(added, current []string) *core.Event {
		return mkEvent("github", "issue_labeled", attrs{AttrRepo: "acme/api", AttrAddedLabels: added, AttrLabels: current})
	}
	runMatchCases(t, []matchCase{
		{"labeled: in checks added labels", mkTrigger("github", "issue_labeled", `{"labels":["factory-ready"]}`), labeled([]string{"factory-ready"}, []string{"bug", "factory-ready"}), true, false},
		{"labeled: pre-existing label not added", mkTrigger("github", "issue_labeled", `{"labels":["factory-ready"]}`), labeled([]string{"bug"}, []string{"bug", "factory-ready"}), false, false},
		{"labeled: not_in checks current", mkTrigger("github", "issue_labeled", `{"labels":{"in":["factory-ready"],"not_in":["blocked"]}}`), labeled([]string{"factory-ready"}, []string{"blocked", "factory-ready"}), false, false},
		{"commit_status success", mkTrigger("github", "commit_status", `{"states":["FAILURE","success"]}`), mkEvent("github", "commit_status", attrs{AttrCommitState: "success", AttrContext: "ci/build"}), true, false},
		{"commit_status pending never", mkTrigger("github", "commit_status", ""), mkEvent("github", "commit_status", attrs{AttrCommitState: "pending"}), false, false},
		{"commit_status contexts fold", mkTrigger("github", "commit_status", `{"contexts":["CI/Build"]}`), mkEvent("github", "commit_status", attrs{AttrCommitState: "failure", AttrContext: "ci/build"}), true, false},
		{"commit_status invalid state value", mkTrigger("github", "commit_status", `{"states":["pending"]}`), mkEvent("github", "commit_status", attrs{AttrCommitState: "failure"}), false, true},
		{"workflow run", mkTrigger("github", "workflow_run_completed", `{"workflows":["CI"],"conclusions":["failure"],"branches":["main"]}`),
			mkEvent("github", "workflow_run_completed", attrs{AttrWorkflow: "CI", AttrConclusion: "failure", AttrBranch: "main"}), true, false},
		{"workflow run label needs one linked PR", mkTrigger("github", "workflow_run_completed", `{"labels":["x"]}`),
			mkEvent("github", "workflow_run_completed", attrs{AttrLabels: []string{"x"}, AttrLinkedPRCount: 2}), false, false},
		{"workflow run label with one linked PR", mkTrigger("github", "workflow_run_completed", `{"labels":["x"]}`),
			mkEvent("github", "workflow_run_completed", attrs{AttrLabels: []string{"x"}, AttrLinkedPRCount: 1}), true, false},
		{"check suite author not_in needs linked PR", mkTrigger("github", "check_suite_completed", `{"authors":{"not_in":["bob"]}}`),
			mkEvent("github", "check_suite_completed", attrs{}), false, false},
		{"review submitted state", mkTrigger("github", "pull_request_review_submitted", `{"review_states":["changes_requested"]}`),
			mkEvent("github", "pull_request_review_submitted", attrs{AttrReviewState: "CHANGES_REQUESTED"}), true, false},
		{"review states enum", mkTrigger("github", "pull_request_review_submitted", `{"review_states":["rejected"]}`),
			mkEvent("github", "pull_request_review_submitted", nil), false, true},
		{"review requested team", mkTrigger("github", "pull_request_review_requested", `{"reviewer_teams":["platform"]}`),
			mkEvent("github", "pull_request_review_requested", attrs{AttrReviewerTeams: []string{"Platform"}}), true, false},
		{"mentioned team slug", mkTrigger("github", "issue_mentioned", `{"mentioned":["acme/oncall"]}`),
			mkEvent("github", "issue_mentioned", attrs{AttrMentioned: []string{"alex", "acme/oncall"}}), true, false},
		{"push branches and paths", mkTrigger("github", "push", `{"branches":["main"],"paths":["**/*.go"]}`),
			mkEvent("github", "push", attrs{AttrBranch: "refs/heads/main", AttrPaths: []string{"cmd/x/main.go"}}), true, false},
	})
}

func TestMatchSlack(t *testing.T) {
	msg := func(typ string, a attrs) *core.Event { return mkEvent("slack", typ, a) }
	base := attrs{AttrChannelID: "C0123456789", AttrChannelName: "incidents", AttrUserID: "U1", AttrUserName: "alex", AttrUserEmail: "alex@example.com"}
	reaction := attrs{AttrChannelID: "C1", AttrChannelName: "engineering", AttrUserID: "U2", AttrEmoji: ":eyes::skin-tone-3:", AttrItemUserID: "U9", AttrItemUserName: "pat"}
	runMatchCases(t, []matchCase{
		{"channel id", mkTrigger("slack", "message_posted", `{"channel_ids":["C0123456789"]}`), msg("message_posted", base), true, false},
		{"channel id not_in", mkTrigger("slack", "message_posted", `{"channel_ids":{"not_in":["C0123456789"]}}`), msg("message_posted", base), false, false},
		{"channels alias with #", mkTrigger("slack", "message_posted", `{"channels":[" #incidents "]}`), msg("message_posted", base), true, false},
		{"channels alias matcher object rejected", mkTrigger("slack", "message_posted", `{"channels":{"not_in":["incidents"]}}`), msg("message_posted", base), false, true},
		{"channels with channel_ids rejected", mkTrigger("slack", "message_posted", `{"channels":["a"],"channel_ids":["C1"]}`), msg("message_posted", base), false, true},
		{"users alias by email", mkTrigger("slack", "message_posted", `{"users":["Alex@Example.com"]}`), msg("message_posted", base), true, false},
		{"users alias by name", mkTrigger("slack", "message_posted", `{"users":["alex"]}`), msg("message_posted", base), true, false},
		{"keywords", mkTrigger("slack", "message_posted", `{"keywords":["outage"]}`), withText(msg("message_posted", base), "Major OUTAGE in eu"), true, false},
		{"message_dm covers im", mkTrigger("slack", "message_dm", ""), msg("message_im", base), true, false},
		{"message_dm covers mpim", mkTrigger("slack", "message_dm", ""), msg("message_mpim", base), true, false},
		{"message_im not mpim", mkTrigger("slack", "message_im", ""), msg("message_mpim", base), false, false},
		{"self post ignored", mkTrigger("slack", "app_mention", ""), msg("app_mention", attrs{AttrActorIsSelf: true}), false, false},
		{"other bot allowed", mkTrigger("slack", "app_mention", ""), func() *core.Event { e := msg("app_mention", base); e.Actor.IsBot = true; return e }(), true, false},
		{"emoji normalized", mkTrigger("slack", "reaction_added", `{"emojis":[":eyes:"]}`), msg("reaction_added", reaction), true, false},
		{"emoji mismatch", mkTrigger("slack", "reaction_added", `{"emojis":["ticket"]}`), msg("reaction_added", reaction), false, false},
		{"item users alias", mkTrigger("slack", "reaction_added", `{"itemUsers":["pat"]}`), msg("reaction_added", reaction), true, false},
		{"item_user_ids", mkTrigger("slack", "reaction_added", `{"item_user_ids":["U9"],"user_ids":["U2"]}`), msg("reaction_added", reaction), true, false},
		{"itemUsers only on reactions", mkTrigger("slack", "message_posted", `{"itemUsers":["pat"]}`), msg("message_posted", base), false, true},
		{"emojis not on member_joined", mkTrigger("slack", "member_joined_channel", `{"emojis":["x"]}`), msg("member_joined_channel", base), false, true},
	})
}

func TestMatchTeams(t *testing.T) {
	ev := mkEvent("microsoft_teams", "app_mention", attrs{AttrTeamID: "11111111-1111-4111-8111-111111111111", AttrChannelID: "19:abc@thread.tacv2", AttrUserID: "u-1"})
	runMatchCases(t, []matchCase{
		{"scoped match (provider normalized)", mkTrigger("teams", "app_mention", `{"team_ids":["11111111-1111-4111-8111-111111111111"],"channel_ids":["19:abc@thread.tacv2"]}`), ev, true, false},
		{"other channel", mkTrigger("teams", "app_mention", `{"team_ids":["11111111-1111-4111-8111-111111111111"],"channel_ids":["19:zzz@thread.tacv2"]}`), ev, false, false},
		{"team_ids required", mkTrigger("teams", "app_mention", `{"channel_ids":["19:abc@thread.tacv2"]}`), ev, false, true},
		{"channel_ids required", mkTrigger("teams", "app_mention", `{"team_ids":["t"]}`), ev, false, true},
		{"team_ids exactly one", mkTrigger("teams", "app_mention", `{"team_ids":["a","b"],"channel_ids":["c"]}`), ev, false, true},
		{"team_ids in only", mkTrigger("teams", "app_mention", `{"team_ids":{"in":["a"],"not_in":["b"]},"channel_ids":["c"]}`), ev, false, true},
		{"channels and users aliases (names at event time)", mkTrigger("teams", "app_mention", `{"team_ids":["11111111-1111-4111-8111-111111111111"],"channels":["#Incidents"],"users":["alice adams"]}`),
			mkEvent("microsoft_teams", "app_mention", attrs{AttrTeamID: "11111111-1111-4111-8111-111111111111", AttrChannelID: "19:abc@thread.tacv2", AttrChannelName: "incidents", AttrUserID: "u-1", AttrUserName: "Alice Adams"}), true, false},
		{"channels alias other channel", mkTrigger("teams", "app_mention", `{"team_ids":["11111111-1111-4111-8111-111111111111"],"channels":["dev"]}`),
			mkEvent("microsoft_teams", "app_mention", attrs{AttrTeamID: "11111111-1111-4111-8111-111111111111", AttrChannelID: "19:abc@thread.tacv2", AttrChannelName: "incidents"}), false, false},
		{"channels alias is a channel_ids", mkTrigger("teams", "app_mention", `{"team_ids":["t"],"channels":["a"],"channel_ids":["c"]}`), ev, false, true},
		{"users alias matcher object rejected", mkTrigger("teams", "app_mention", `{"team_ids":["t"],"channels":["a"],"users":{"in":["x"]}}`), ev, false, true},
		{"keywords + users", mkTrigger("teams", "app_mention", `{"team_ids":{"in":["11111111-1111-4111-8111-111111111111"]},"channel_ids":["19:abc@thread.tacv2"],"user_ids":["u-1"],"keywords":["review"]}`), withText(ev, "please Review"), true, false},
	})
}

func TestMatchLinear(t *testing.T) {
	issue := attrs{
		AttrTeamID: "team-uuid", AttrTeamName: "Engineering", AttrProjectID: "proj-uuid", AttrProjectName: "Payments",
		AttrLabels: []string{"Factory-Ready", "bug"}, AttrStateID: "state-uuid", AttrStateName: "Todo",
		AttrIssueID: "issue-uuid", AttrIssueNumber: 42, AttrAuthor: "creator-uuid", AttrMentioned: []string{"user-uuid", "alex"},
	}
	labeled := attrs{AttrTeamName: "Engineering", AttrLabels: []string{"factory-ready", "blocked"}, AttrAddedLabels: []string{"factory-ready"}}
	runMatchCases(t, []matchCase{
		{"labels case-insensitive", mkTrigger("linear", "comment_created", `{"labels":["factory-ready"]}`), mkEvent("linear", "comment_created", issue), true, false},
		{"teams alias", mkTrigger("linear", "comment_created", `{"teams":["Engineering"],"issues":[42]}`), mkEvent("linear", "comment_created", issue), true, false},
		{"issues alias mismatch", mkTrigger("linear", "comment_created", `{"teams":["Engineering"],"issues":[43]}`), mkEvent("linear", "comment_created", issue), false, false},
		{"issues alias not on issue_created", mkTrigger("linear", "issue_created", `{"issues":[42]}`), mkEvent("linear", "issue_created", issue), false, true},
		{"issues alias positive ints", mkTrigger("linear", "comment_created", `{"issues":["ENG-42"]}`), mkEvent("linear", "comment_created", issue), false, true},
		{"projects/states aliases", mkTrigger("linear", "issue_state_changed", `{"projects":["Payments"],"states":["Todo"]}`), mkEvent("linear", "issue_state_changed", issue), true, false},
		{"native ids", mkTrigger("linear", "issue_created", `{"team_ids":["team-uuid"],"creator_ids":["creator-uuid"],"mentioned_user_ids":["alex"]}`), mkEvent("linear", "issue_created", issue), true, false},
		{"issue_labeled added", mkTrigger("linear", "issue_labeled", `{"labels":["Factory-Ready"]}`), mkEvent("linear", "issue_labeled", labeled), true, false},
		{"issue_labeled not_in current", mkTrigger("linear", "issue_labeled", `{"labels":{"in":["factory-ready"],"not_in":["blocked"]}}`), mkEvent("linear", "issue_labeled", labeled), false, false},
		{"agent session keywords", mkTrigger("linear", "agent_session_created", `{"keywords":["review"]}`), withText(mkEvent("linear", "agent_session_created", issue), "Please REVIEW this"), true, false},
		{"self activity ignored", mkTrigger("linear", "comment_created", ""), mkEvent("linear", "comment_created", attrs{AttrActorIsSelf: true}), false, false},
		{"alias matcher object rejected", mkTrigger("linear", "comment_created", `{"teams":{"in":["Engineering"]}}`), mkEvent("linear", "comment_created", issue), false, true},
	})
}

func TestMatchJiraGitLabADOFactory(t *testing.T) {
	runMatchCases(t, []matchCase{
		{"jira project + labels case-sensitive", mkTrigger("jira", "issue_created", `{"project_keys":["ENG"],"labels":["Factory-Ready"]}`),
			mkEvent("jira", "issue_created", attrs{AttrProjectKey: "ENG", AttrLabels: []string{"factory-ready"}}), false, false},
		{"jira projects and statuses aliases (names at event time)", mkTrigger("jira", "status_changed", `{"projects":["website"],"statuses":["Done"]}`),
			mkEvent("jira", "status_changed", attrs{AttrProjectKey: "WEB", AttrProjectName: "Website", AttrStatusID: "10001", AttrStatusName: "done"}), true, false},
		{"jira statuses alias other status", mkTrigger("jira", "status_changed", `{"statuses":["Done"]}`),
			mkEvent("jira", "status_changed", attrs{AttrStatusName: "In Progress"}), false, false},
		{"jira statuses only on status_changed", mkTrigger("jira", "issue_created", `{"statuses":["Done"]}`), mkEvent("jira", "issue_created", attrs{}), false, true},
		{"jira projects with native key rejected", mkTrigger("jira", "issue_created", `{"projects":["Website"],"project_keys":["WEB"]}`), mkEvent("jira", "issue_created", attrs{}), false, true},
		{"jira status ids", mkTrigger("jira", "status_changed", `{"status_ids":["10001"]}`), mkEvent("jira", "status_changed", attrs{AttrStatusID: "10001"}), true, false},
		{"jira keywords do not restrict", mkTrigger("jira", "agent_session_created", `{"project_keys":["ENG"],"keywords":["investigate"]}`),
			withText(mkEvent("jira", "agent_session_created", attrs{AttrProjectKey: "ENG"}), "nothing relevant"), true, false},
		{"gitlab merge request", mkTrigger("gitlab", "merge_request", `{"repos":["acme/platform/api"],"actions":["open"],"base_branches":["main"]}`),
			mkEvent("gitlab", "merge_request", attrs{AttrRepo: "acme/platform/api", AttrAction: "open", AttrBaseBranch: "main"}), true, false},
		{"gitlab mentioned server-managed allowed when matching", mkTrigger("gitlab", "bot_mentioned", `{"mentioned":["acme-bot"]}`),
			mkEvent("gitlab", "bot_mentioned", attrs{AttrMentioned: []string{"acme-bot"}}), true, false},
		{"gitlab self ignored", mkTrigger("gitlab", "push", ""), mkEvent("gitlab", "push", attrs{AttrActorIsSelf: true}), false, false},
		{"ado work item", mkTrigger("azure_devops", "work_item_created", `{"work_item_types":["Bug"],"labels":["factory-ready"],"authors":["ALEX@example.com"]}`),
			mkEvent("azure-devops", "work_item_created", attrs{AttrWorkItemType: "Bug", AttrLabels: []string{"factory-ready"}, AttrAuthor: "alex@example.com"}), true, false},
		{"ado pr base branch", mkTrigger("azure_devops", "pull_request_created", `{"repos":["11111111-1111-4111-8111-111111111111"],"base_branches":["main"]}`),
			mkEvent("azure_devops", "pull_request_created", attrs{AttrRepo: "11111111-1111-4111-8111-111111111111", AttrBaseBranch: "refs/heads/main"}), true, false},
		{"ado push matchable", mkTrigger("azure_devops", "push", `{"branches":["main"]}`), mkEvent("azure_devops", "push", attrs{AttrBranch: "main"}), true, false},
	})
}

func TestMatchWebhookAndSchedule(t *testing.T) {
	delivery := &core.Event{Provider: "webhook", Type: "received", Attributes: attrs{AttrWebhookID: "wh_1", AttrWebhookName: "ci-alerts"},
		Payload: json.RawMessage(`{"action":"opened","pull_request":{"draft":false},"sender":{"type":"User"}}`)}
	fire := mkEvent("schedule", "cron_fired", attrs{AttrAutomationID: "aut_1", AttrScheduleName: "nightly"})
	sched := func(name string) core.Trigger {
		return core.Trigger{Provider: "schedule", Event: "cron_fired", Schedule: &core.Schedule{Name: name, Cron: "0 2 * * *"}}
	}
	runMatchCases(t, []matchCase{
		{"webhook id + payload", mkTrigger("webhook", "received", `{"webhook_ids":["wh_1"],"payload":{"action":["opened","reopened"],"pull_request":{"draft":[false]},"sender":{"type":{"not_in":["Bot"]}}}}`), delivery, true, false},
		{"other webhook", mkTrigger("webhook", "received", `{"webhook_ids":["wh_2"]}`), delivery, false, false},
		{"webhook by name", mkTrigger("webhook", "received", `{"webhook_ids":["ci-alerts"]}`), delivery, true, false},
		{"other webhook name", mkTrigger("webhook", "received", `{"webhook_ids":["deploys"]}`), delivery, false, false},
		{"payload mismatch", mkTrigger("webhook", "received", `{"webhook_ids":["wh_1"],"payload":{"action":["closed"]}}`), delivery, false, false},
		{"webhook_ids required", mkTrigger("webhook", "received", `{"payload":{"action":["opened"]}}`), delivery, false, true},
		{"webhook_ids exactly one", mkTrigger("webhook", "received", `{"webhook_ids":["wh_1","wh_2"]}`), delivery, false, true},
		{"invalid payload pattern", mkTrigger("webhook", "received", `{"webhook_ids":["wh_1"],"payload":{"a":{"in":["x"],"not_in":["x"]}}}`), delivery, false, true},
		{"schedule name match", sched("nightly"), fire, true, false},
		{"schedule name mismatch", sched(""), fire, false, false},
	})
}

func TestMatchAutomation(t *testing.T) {
	a := core.Automation{ID: "aut_1", FactoryID: "fac_1", Enabled: true, Triggers: []core.Trigger{
		mkTrigger("github", "issue_labeled", `{"labels":"broken"}`), // invalid: fails closed, others still evaluated
		mkTrigger("github", "issue_labeled", `{"labels":["factory-ready"]}`),
		{Provider: "schedule", Event: "cron_fired", Schedule: &core.Schedule{Cron: "@daily"}},
	}}
	labeled := mkEvent("github", "issue_labeled", attrs{AttrLabels: []string{"factory-ready"}})
	if !Match(labeled, a) {
		t.Fatal("expected match via second trigger")
	}
	disabled := a
	disabled.Enabled = false
	if Match(labeled, disabled) {
		t.Fatal("disabled automation matched")
	}
	other := *labeled
	other.FactoryID = "fac_2"
	if Match(&other, a) {
		t.Fatal("event for another factory matched")
	}
	tick := mkEvent("schedule", "cron_fired", attrs{AttrAutomationID: "aut_1", AttrScheduleName: ""})
	if !Match(tick, a) {
		t.Fatal("schedule event for this automation should match")
	}
	tick.Attributes[AttrAutomationID] = "aut_2"
	if Match(tick, a) {
		t.Fatal("schedule event for another automation matched")
	}
	if got := MatchingAutomations(labeled, []core.Automation{a, disabled}); len(got) != 1 {
		t.Fatalf("MatchingAutomations = %d, want 1", len(got))
	}
	onlyBad := core.Automation{Enabled: true, Triggers: []core.Trigger{mkTrigger("github", "issue_labeled", `{"nope":[1]}`)}}
	if Match(labeled, onlyBad) {
		t.Fatal("invalid filter must fail closed")
	}
}

func TestValidateTrigger(t *testing.T) {
	cases := []struct {
		name    string
		trigger core.Trigger
		errSub  string // empty = valid
	}{
		{"valid github", mkTrigger("github", "pull_request_opened", `{"repos":["acme/api"],"labels":{"not_in":["wip"]}}`), ""},
		{"valid linear aliases", mkTrigger("linear", "issue_labeled", `{"teams":["Engineering"],"labels":["factory-ready"]}`), ""},
		{"unknown provider", mkTrigger("bitbucket", "push", ""), "unknown provider"},
		{"non-canonical teams provider", mkTrigger("microsoft-teams", "app_mention", ""), `use "teams"`},
		{"dotted event", mkTrigger("linear", "linear.issue_labeled", ""), "dotted"},
		{"unknown event", mkTrigger("github", "issue_closed", ""), "unknown event"},
		{"ado push not authorable", mkTrigger("azure_devops", "push", ""), "not available for authoring"},
		{"ado source_repos server-managed", mkTrigger("azure_devops", "pull_request_created", `{"source_repos":["x"]}`), "server-managed"},
		{"gitlab mentioned server-managed", mkTrigger("gitlab", "bot_mentioned", `{"mentioned":["x"]}`), "server-managed"},
		{"schedule_ids server-managed", core.Trigger{Provider: "schedule", Event: "cron_fired", Filter: json.RawMessage(`{"schedule_ids":["x"]}`), Schedule: &core.Schedule{Cron: "@daily"}}, "accept no filter"},
		{"schedule requires schedule", mkTrigger("schedule", "cron_fired", ""), "schedule is required"},
		{"schedule bad cron", core.Trigger{Provider: "schedule", Event: "cron_fired", Schedule: &core.Schedule{Cron: "0 9 * *"}}, "expected 5 fields"},
		{"schedule ok", core.Trigger{Provider: "schedule", Event: "cron_fired", Schedule: &core.Schedule{Cron: "0 9 * * 1-5"}}, ""},
		{"schedule on non-schedule", core.Trigger{Provider: "github", Event: "push", Schedule: &core.Schedule{Cron: "@daily"}}, "only valid for schedule"},
		{"teams missing filter", mkTrigger("teams", "message_posted", ""), "required"},
		{"webhook missing filter", mkTrigger("webhook", "received", ""), "required"},
		{"webhook valid", mkTrigger("webhook", "received", `{"webhook_ids":["wh_1"],"payload":{"status":["failed"]}}`), ""},
		{"webhook not_in on ids", mkTrigger("webhook", "received", `{"webhook_ids":{"not_in":["wh_1"]}}`), "only in"},
		{"glob malformed", mkTrigger("github", "push", `{"paths":["src/[a"]}`), "malformed glob"},
		{"glob ** inside segment", mkTrigger("github", "push", `{"paths":["src/**.go"]}`), "whole path segment"},
		{"empty value", mkTrigger("github", "push", `{"branches":[""]}`), "empty value"},
		{"bool value", mkTrigger("github", "push", `{"branches":[true]}`), "strings"},
		{"provider required", core.Trigger{Event: "push"}, "provider is required"},
		{"event required", core.Trigger{Provider: "github"}, "event is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTrigger(tc.trigger)
			if tc.errSub == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errSub) {
				t.Fatalf("err = %v, want containing %q", err, tc.errSub)
			}
			if !errors.Is(err, ErrInvalidFilter) && !errors.Is(err, ErrInvalidPattern) {
				t.Fatalf("err %v does not wrap ErrInvalidFilter/ErrInvalidPattern", err)
			}
		})
	}
}

func TestValidateTriggers(t *testing.T) {
	s := func(name string) core.Trigger {
		return core.Trigger{Provider: "schedule", Event: "cron_fired", Schedule: &core.Schedule{Name: name, Cron: "@hourly"}}
	}
	if err := ValidateTriggers([]core.Trigger{s(""), s("a"), s("b")}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if err := ValidateTriggers([]core.Trigger{s(""), s("")}); err == nil || !strings.Contains(err.Error(), "omit its name") {
		t.Fatalf("want unnamed duplicate error, got %v", err)
	}
	if err := ValidateTriggers([]core.Trigger{s("a"), s("a")}); err == nil || !strings.Contains(err.Error(), "duplicate schedule name") {
		t.Fatalf("want duplicate name error, got %v", err)
	}
	if err := ValidateTriggers(nil); err == nil {
		t.Fatal("want error for no triggers")
	}
}

func TestEventsCatalogue(t *testing.T) {
	evs := Events("linear")
	if len(evs) != 6 {
		t.Fatalf("linear events = %d, want 6", len(evs))
	}
	for _, e := range evs {
		if e.Event == "comment_created" && !contains(e.Aliases, "issues") {
			t.Fatalf("comment_created aliases = %v, want issues", e.Aliases)
		}
		if e.Event == "issue_created" && contains(e.Aliases, "issues") {
			t.Fatal("issue_created must not offer the issues alias")
		}
	}
	for _, e := range Events("gitlab") {
		if contains(e.FilterFields, "mentioned") {
			t.Fatal("server-managed key listed")
		}
	}
	if Events("nope") != nil {
		t.Fatal("unknown provider should return nil")
	}
	if len(Providers()) != 9 {
		t.Fatalf("providers = %v", Providers())
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"src/**", "src/a/b/c.go", true},
		{"src/**", "src", true},
		{"src/**", "lib/src/a.go", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "a/b/main.go", true},
		{"**/*.go", "a/b/main.ts", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/sub/a.md", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"a/**/**/b", "a/x/b", true},
		{"/src/*.go", "src/x.go", true},
		{"README.md", "README.md", true},
		{"README.md", "docs/README.md", false},
		{"src/?.go", "src/a.go", true},
	}
	for _, c := range cases {
		if got := matchGlob(c.pat, c.name); got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

func TestNormalizeEmoji(t *testing.T) {
	for in, want := range map[string]string{
		":eyes:": "eyes", "eyes": "eyes", ":thumbsup::skin-tone-3:": "thumbsup", "wave:skin-tone-2": "wave", " :Ticket: ": "ticket",
	} {
		if got := normalizeEmoji(in); got != want {
			t.Errorf("normalizeEmoji(%q) = %q, want %q", in, got, want)
		}
	}
}
