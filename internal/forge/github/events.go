package github

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// Provider is the trigger provider name.
const Provider = eventattr.ProviderGitHub

// AliasLabelPrefix marks issues and pull requests routed to a factory: "factory:<alias>".
const AliasLabelPrefix = "factory:"

type ghLabel struct {
	Name string `json:"name"`
}

type ghIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	State       string    `json:"state"`
	User        User      `json:"user"`
	Labels      []ghLabel `json:"labels"`
	Assignees   []User    `json:"assignees"`
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

type ghPR struct {
	ghIssue
	Draft              bool   `json:"draft"`
	Merged             bool   `json:"merged"`
	Head               Ref    `json:"head"`
	Base               Ref    `json:"base"`
	RequestedReviewers []User `json:"requested_reviewers"`
	// Facts for the metrics.
	Additions *int   `json:"additions"`
	Deletions *int   `json:"deletions"`
	CreatedAt string `json:"created_at"`
	MergedAt  string `json:"merged_at"`
}

type ghRepo struct {
	FullName      string `json:"full_name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
}

type ghLinkedPR struct {
	Number int `json:"number"`
	Head   Ref `json:"head"`
	Base   Ref `json:"base"`
}

type ghPayload struct {
	Action       string `json:"action"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Repository ghRepo   `json:"repository"`
	Sender     User     `json:"sender"`
	Issue      *ghIssue `json:"issue"`
	PR         *ghPR    `json:"pull_request"`
	Label      *ghLabel `json:"label"`
	Assignee   *User    `json:"assignee"`
	Comment    *struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    User   `json:"user"`
		// InReplyToID is the first comment of the review thread (review
		// comments only).
		InReplyToID int64 `json:"in_reply_to_id"`
	} `json:"comment"`
	Review *struct {
		ID      int64  `json:"id"`
		Body    string `json:"body"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
		User    User   `json:"user"`
	} `json:"review"`
	RequestedReviewer *User `json:"requested_reviewer"`
	RequestedTeam     *struct {
		Slug string `json:"slug"`
	} `json:"requested_team"`
	Changes map[string]json.RawMessage `json:"changes"`

	// push
	Ref     string `json:"ref"`
	Before  string `json:"before"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
	Compare string `json:"compare"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
	HeadCommit *struct {
		Message string `json:"message"`
	} `json:"head_commit"`

	// CI
	CheckSuite *struct {
		ID           int64        `json:"id"`
		HeadBranch   string       `json:"head_branch"`
		HeadSHA      string       `json:"head_sha"`
		Conclusion   string       `json:"conclusion"`
		PullRequests []ghLinkedPR `json:"pull_requests"`
		App          *struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"check_suite"`
	CheckRun *struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		HTMLURL    string `json:"html_url"`
		CheckSuite struct {
			HeadBranch string `json:"head_branch"`
		} `json:"check_suite"`
		PullRequests []ghLinkedPR `json:"pull_requests"`
		App          *struct {
			Slug string `json:"slug"`
		} `json:"app"`
	} `json:"check_run"`
	WorkflowRun *struct {
		ID           int64        `json:"id"`
		Name         string       `json:"name"`
		HeadBranch   string       `json:"head_branch"`
		HeadSHA      string       `json:"head_sha"`
		Conclusion   string       `json:"conclusion"`
		HTMLURL      string       `json:"html_url"`
		Actor        User         `json:"actor"`
		PullRequests []ghLinkedPR `json:"pull_requests"`
	} `json:"workflow_run"`

	// status
	SHA         string `json:"sha"`
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetURL   string `json:"target_url"`
	Branches    []struct {
		Name string `json:"name"`
	} `json:"branches"`
}

func labelNames(ls []ghLabel) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func logins(us []User) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, u.Login)
	}
	return out
}

// IsBotUser reports whether a GitHub account is a bot.
func IsBotUser(u User) bool {
	return strings.EqualFold(u.Type, "Bot") || strings.HasSuffix(strings.ToLower(u.Login), "[bot]")
}

// AliasFromLabels returns the alias of the first "factory:<alias>" label.
func AliasFromLabels(labels []string) string {
	for _, l := range labels {
		if len(l) > len(AliasLabelPrefix) && strings.EqualFold(l[:len(AliasLabelPrefix)], AliasLabelPrefix) {
			return strings.TrimSpace(l[len(AliasLabelPrefix):])
		}
	}
	return ""
}

// Normalizer converts webhook deliveries into events.
type Normalizer struct {
	// AppSlug is the GitHub App slug; its bot login is "<slug>[bot]" and "@<slug>" mentions route to factories.
	AppSlug string
	Now     func() time.Time
}

func (n Normalizer) isSelf(u User) bool {
	if n.AppSlug == "" {
		return false
	}
	l := strings.ToLower(u.Login)
	s := strings.ToLower(n.AppSlug)
	return l == s+"[bot]" || l == s
}

// Normalize maps one webhook delivery (X-GitHub-Event name + JSON body) to zero or more events.
// TeamID / FactoryID / Actor.UserID are filled in by the webhook handler.
func (n Normalizer) Normalize(eventName, deliveryID string, body []byte) ([]*core.Event, error) {
	var p ghPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("github: decode %s payload: %w", eventName, err)
	}
	now := time.Now
	if n.Now != nil {
		now = n.Now
	}
	repo := p.Repository.FullName
	var out []*core.Event
	emit := func(typ string, src core.SourceRef, text string, attrs map[string]any) {
		a := map[string]any{
			eventattr.Repo:        repo,
			eventattr.ActorIsBot:  IsBotUser(p.Sender),
			eventattr.ActorIsSelf: n.isSelf(p.Sender),
		}
		for k, v := range attrs {
			a[k] = v
		}
		if text != "" {
			a[eventattr.Text] = text
		}
		did, akey := deliveryID, ""
		if did != "" {
			// One delivery gives several events (a comment and a mention):
			// they share the action key, and the engine routes them one time.
			did = deliveryID + ":" + typ
			akey = Provider + ":" + deliveryID
		}
		out = append(out, &core.Event{
			ID:         core.NewID("evt"),
			Provider:   Provider,
			Type:       typ,
			DeliveryID: did,
			ActionKey:  akey,
			Actor: core.Actor{
				Provider: Provider, ExternalID: fmt.Sprint(p.Sender.ID), Handle: p.Sender.Login,
				IsBot: IsBotUser(p.Sender), IsSelf: n.isSelf(p.Sender),
			},
			Source:     src,
			Attributes: a,
			Payload:    json.RawMessage(body),
			Text:       text,
			ReceivedAt: now().UTC(),
		})
	}

	issueSource := func(is *ghIssue) core.SourceRef {
		kind := "issue"
		if is.PullRequest != nil {
			kind = "pull_request"
		}
		return core.SourceRef{Provider: Provider, Kind: kind, URL: is.HTMLURL, Repo: repo, Number: is.Number, Title: is.Title}
	}
	issueAttrs := func(is *ghIssue) map[string]any {
		labels := labelNames(is.Labels)
		a := map[string]any{
			eventattr.Labels:    labels,
			eventattr.Assignees: logins(is.Assignees),
			eventattr.Author:    is.User.Login,
		}
		if alias := AliasFromLabels(labels); alias != "" {
			a[eventattr.Alias] = alias
		}
		return a
	}
	prAttrs := func(pr *ghPR) map[string]any {
		a := issueAttrs(&pr.ghIssue)
		a[eventattr.BaseBranch] = pr.Base.Ref
		a[eventattr.HeadBranch] = pr.Head.Ref
		a[eventattr.HeadSHA] = pr.Head.SHA
		a[eventattr.PRNumber] = pr.Number
		a[eventattr.Draft] = pr.Draft
		// Facts for the metrics.
		if pr.Additions != nil {
			a[eventattr.Additions] = *pr.Additions
		}
		if pr.Deletions != nil {
			a[eventattr.Deletions] = *pr.Deletions
		}
		if pr.CreatedAt != "" {
			a[eventattr.PRCreatedAt] = pr.CreatedAt
		}
		if pr.MergedAt != "" {
			a[eventattr.MergedAt] = pr.MergedAt
		}
		if pr.User.Login != "" {
			a[eventattr.PROpener] = pr.User.Login
			a[eventattr.PROpenerIsBot] = IsBotUser(pr.User)
		}
		return a
	}
	prSource := func(pr *ghPR) core.SourceRef {
		return core.SourceRef{Provider: Provider, Kind: "pull_request", URL: pr.HTMLURL, Repo: repo, Number: pr.Number, Title: pr.Title}
	}
	mentionEvent := func(typ string, src core.SourceRef, text string, base map[string]any) {
		mentioned := forgeutil.ExtractMentions(text)
		if len(mentioned) == 0 {
			return
		}
		a := copyAttrs(base)
		a[eventattr.Mentioned] = mentioned
		emit(typ, src, text, a)
	}

	switch eventName {
	case "issues":
		if p.Issue == nil {
			return nil, nil
		}
		src := issueSource(p.Issue)
		text := joinText(p.Issue.Title, p.Issue.Body)
		a := issueAttrs(p.Issue)
		switch p.Action {
		case "opened":
			a[eventattr.AddedLabels] = labelNames(p.Issue.Labels)
			emit("issue_created", src, text, a)
			if len(p.Issue.Labels) > 0 {
				emit("issue_labeled", src, text, copyAttrs(a))
			}
			mentionEvent("issue_mentioned", src, p.Issue.Body, a)
		case "labeled":
			if p.Label != nil {
				a[eventattr.AddedLabels] = []string{p.Label.Name}
			}
			emit("issue_labeled", src, text, a)
		case "assigned":
			if p.Assignee != nil {
				a[eventattr.Assignees] = []string{p.Assignee.Login}
			}
			emit("issue_assigned", src, text, a)
		}
	case "issue_comment":
		if p.Issue == nil || p.Comment == nil || p.Action != "created" {
			return nil, nil
		}
		src := issueSource(p.Issue)
		a := issueAttrs(p.Issue)
		a["comment_url"] = p.Comment.HTMLURL
		a[eventattr.Author] = p.Comment.User.Login // comment events: the commenter
		kind := "issue"
		if p.Issue.PullRequest != nil {
			kind = "pull_request"
			a[eventattr.PRNumber] = p.Issue.Number
		}
		emit(kind+"_commented", src, p.Comment.Body, a)
		mentionEvent(kind+"_mentioned", src, p.Comment.Body, a)
	case "pull_request":
		if p.PR == nil {
			return nil, nil
		}
		pr := p.PR
		src := prSource(pr)
		text := joinText(pr.Title, pr.Body)
		a := prAttrs(pr)
		switch p.Action {
		case "opened":
			a[eventattr.AddedLabels] = labelNames(pr.Labels)
			emit("pull_request_opened", src, text, a)
			if !pr.Draft {
				emit("pull_request_ready", src, text, copyAttrs(a))
			}
			mentionEvent("pull_request_mentioned", src, pr.Body, a)
		case "ready_for_review":
			emit("pull_request_ready", src, text, a)
		case "reopened":
			emit("pull_request_reopened", src, text, a)
		case "synchronize":
			emit("pull_request_synchronized", src, text, a)
		case "labeled":
			if p.Label != nil {
				a[eventattr.AddedLabels] = []string{p.Label.Name}
			}
			emit("pull_request_labeled", src, text, a)
		case "assigned":
			if p.Assignee != nil {
				a[eventattr.Assignees] = []string{p.Assignee.Login}
			}
			emit("pull_request_assigned", src, text, a)
		case "closed":
			a[eventattr.Merged] = pr.Merged
			emit("pull_request_closed", src, text, a)
			if pr.Merged {
				emit("pull_request_merged", src, text, copyAttrs(a))
			}
		case "enqueued":
			emit("pull_request_enqueued", src, text, a)
		case "dequeued":
			emit("pull_request_dequeued", src, text, a)
		case "review_requested":
			ra := copyAttrs(a)
			if p.RequestedReviewer != nil {
				ra[eventattr.Reviewers] = []string{p.RequestedReviewer.Login}
			} else {
				ra[eventattr.Reviewers] = []string{}
			}
			if p.RequestedTeam != nil {
				ra[eventattr.ReviewerTeams] = []string{p.RequestedTeam.Slug}
			} else {
				ra[eventattr.ReviewerTeams] = []string{}
			}
			emit("pull_request_review_requested", src, text, ra)
		}
	case "pull_request_review":
		if p.PR == nil || p.Review == nil || p.Action != "submitted" {
			return nil, nil
		}
		src := prSource(p.PR)
		a := prAttrs(p.PR)
		a[eventattr.ReviewState] = strings.ToLower(p.Review.State)
		a[eventattr.Author] = p.Review.User.Login // review events: the reviewer
		a[eventattr.Mentioned] = forgeutil.ExtractMentions(p.Review.Body)
		emit("pull_request_review_submitted", src, p.Review.Body, a)
		mentionEvent("pull_request_mentioned", src, p.Review.Body, a)
	case "pull_request_review_comment":
		if p.PR == nil || p.Comment == nil || p.Action != "created" {
			return nil, nil
		}
		src := prSource(p.PR)
		// The reply goes into the review thread: GitHub names a thread by
		// its first comment.
		thread := p.Comment.InReplyToID
		if thread == 0 {
			thread = p.Comment.ID
		}
		src.ThreadRef = fmt.Sprint(thread)
		a := prAttrs(p.PR)
		a["comment_url"] = p.Comment.HTMLURL
		a[eventattr.Author] = p.Comment.User.Login
		a[eventattr.ThreadRef] = src.ThreadRef
		emit("pull_request_commented", src, p.Comment.Body, a)
		mentionEvent("pull_request_mentioned", src, p.Comment.Body, a)
	case "push":
		if p.Deleted || !strings.HasPrefix(p.Ref, "refs/heads/") || strings.Trim(p.After, "0") == "" {
			return nil, nil
		}
		branch := strings.TrimPrefix(p.Ref, "refs/heads/")
		var paths []string
		for _, c := range p.Commits {
			paths = append(paths, c.Added...)
			paths = append(paths, c.Modified...)
			paths = append(paths, c.Removed...)
		}
		text := ""
		if p.HeadCommit != nil {
			text = p.HeadCommit.Message
		}
		src := core.SourceRef{Provider: Provider, Kind: "push", URL: p.Compare, Repo: repo, ExternalID: repo + "@" + p.After, Title: branch}
		emit("push", src, text, map[string]any{
			eventattr.Branch:  branch,
			eventattr.Paths:   forgeutil.StringSet(paths...),
			eventattr.HeadSHA: p.After,
		})
	case "check_suite":
		cs := p.CheckSuite
		if cs == nil {
			return nil, nil
		}
		src := core.SourceRef{Provider: Provider, Kind: "check_suite", Repo: repo, ExternalID: fmt.Sprintf("%s/check_suite/%d", repo, cs.ID), Title: cs.HeadBranch}
		a := ciAttrs(cs.HeadBranch, cs.HeadSHA, cs.PullRequests)
		switch p.Action {
		case "completed":
			a[eventattr.Conclusion] = cs.Conclusion
			emit("check_suite_completed", src, "", a)
		case "rerequested":
			if n.ownsApp(cs.App) {
				emit("check_suite_rerequested", src, "", a)
			}
		}
	case "check_run":
		cr := p.CheckRun
		if cr == nil || p.Action != "rerequested" || !n.ownsApp(cr.App) {
			return nil, nil
		}
		src := core.SourceRef{Provider: Provider, Kind: "check_run", URL: cr.HTMLURL, Repo: repo, ExternalID: fmt.Sprintf("%s/check_run/%d", repo, cr.ID), Title: cr.Name}
		a := ciAttrs(cr.CheckSuite.HeadBranch, cr.HeadSHA, cr.PullRequests)
		a["check_name"] = cr.Name
		emit("check_run_rerequested", src, "", a)
	case "workflow_run":
		wr := p.WorkflowRun
		if wr == nil || p.Action != "completed" {
			return nil, nil
		}
		src := core.SourceRef{Provider: Provider, Kind: "workflow_run", URL: wr.HTMLURL, Repo: repo, ExternalID: fmt.Sprintf("%s/workflow_run/%d", repo, wr.ID), Title: wr.Name}
		a := ciAttrs(wr.HeadBranch, wr.HeadSHA, wr.PullRequests)
		a[eventattr.Conclusion] = wr.Conclusion
		a[eventattr.Workflow] = wr.Name
		emit("workflow_run_completed", src, "", a)
	case "status":
		state := strings.ToLower(p.State)
		if state != "success" && state != "failure" && state != "error" {
			return nil, nil
		}
		src := core.SourceRef{Provider: Provider, Kind: "commit", URL: p.TargetURL, Repo: repo, ExternalID: repo + "@" + p.SHA + "/" + p.Context, Title: p.Context}
		var branches []string
		for _, b := range p.Branches {
			branches = append(branches, b.Name)
		}
		emit("commit_status", src, p.Description, map[string]any{
			eventattr.CommitState: state,
			eventattr.Context:     p.Context,
			eventattr.HeadSHA:     p.SHA,
			"branches":            branches,
		})
	}
	return out, nil
}

func (n Normalizer) ownsApp(app *struct {
	Slug string `json:"slug"`
}) bool {
	if n.AppSlug == "" || app == nil {
		return n.AppSlug == ""
	}
	return strings.EqualFold(app.Slug, n.AppSlug)
}

func ciAttrs(branch, sha string, prs []ghLinkedPR) map[string]any {
	a := map[string]any{
		eventattr.Branch:        branch,
		eventattr.HeadSHA:       sha,
		eventattr.LinkedPRCount: len(prs),
	}
	if len(prs) == 1 {
		a[eventattr.PRNumber] = prs[0].Number
		a[eventattr.BaseBranch] = prs[0].Base.Ref
	}
	return a
}

func copyAttrs(a map[string]any) map[string]any {
	out := make(map[string]any, len(a))
	for k, v := range a {
		out[k] = v
	}
	return out
}

func joinText(title, body string) string {
	if body == "" {
		return title
	}
	return title + "\n\n" + body
}
