package mcpregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
	"github.com/openfactory-org/openfactory/internal/integrations/jira"
	"github.com/openfactory-org/openfactory/internal/integrations/linear"
	"github.com/openfactory-org/openfactory/internal/integrations/slack"
)

// Built-in integration servers give agents tools for the factory's own
// integrations, with the integration's credentials (the bot or app identity,
// not the requester's). A run may use builtin/<provider> only when:
//
//   - one of its mcpServers entries has managedId builtin/<provider>;
//   - the factory attaches that integration (factory.yaml integrations);
//   - the team has an integration record for the provider.
//
// Read tools are always available; write tools (comments, messages) need
// allowWrite: true on the agent's mcpServers entry.

const maxListLimit = 50

type integrationKey struct{}

type integrationCtx struct {
	run        *core.Run
	in         core.Integration
	token      []byte
	allowWrite bool
}

func (s *Service) integrationHandler() http.Handler {
	sdk := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		ic, _ := r.Context().Value(integrationKey{}).(*integrationCtx)
		if ic == nil {
			return nil
		}
		return s.integrationServer(ic)
	}, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: true, // every request carries a per-run bearer token
		Logger:                     s.log,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		run := runFrom(ctx)
		provider := r.PathValue("provider")
		id := core.BuiltinMCPPrefix + provider
		if _, known := core.BuiltinMCPProvider(id); !known {
			writeError(w, http.StatusNotFound, "not_found", "unknown built-in MCP server "+id)
			return
		}
		granted, allowWrite := grantedTo(run, id)
		if !granted {
			writeError(w, http.StatusForbidden, "forbidden", "this run was not granted "+id)
			return
		}
		in, err := s.factoryIntegration(ctx, run, provider)
		if err != nil {
			writeError(w, http.StatusForbidden, "integration", err.Error())
			return
		}
		var tok []byte
		// The Teams handler reads the bot credentials of the record
		// itself, so the secret is not read here.
		if provider != "teams" {
			tok, err = forgeutil.IntegrationSecret(ctx, s.opts.Secrets, *in, "")
			if err != nil {
				s.log.Warn("built-in mcp: integration credential", "provider", provider, "run", run.ID, "err", err)
				writeError(w, http.StatusBadGateway, "integration", "the "+provider+" integration has no usable credential")
				return
			}
		}
		ic := &integrationCtx{run: run, in: *in, token: tok, allowWrite: allowWrite}
		sdk.ServeHTTP(w, r.WithContext(context.WithValue(ctx, integrationKey{}, ic)))
	})
}

// factoryIntegration returns the team's integration record for provider,
// provided the run's factory attaches the integration. It uses the selection
// rule: the record of the run's factory wins over the
// team record.
func (s *Service) factoryIntegration(ctx context.Context, run *core.Run, provider string) (*core.Integration, error) {
	attached, err := s.opts.Store.GetFactoryIntegrations(ctx, run.FactoryID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return nil, err
	}
	isAttached := false
	for k := range attached {
		if core.SameIntegrationProvider(k, provider) {
			isAttached = true
			break
		}
	}
	if !isAttached {
		return nil, fmt.Errorf("the factory does not attach the %s integration (add it to factory.yaml integrations)", provider)
	}
	// The workspace of the work item's source, when it came from this
	// provider; otherwise the factory, or else the team, needs one record.
	workspace := ""
	if run.WorkItemID != "" {
		if wi, err := s.opts.Store.GetWorkItem(ctx, run.WorkItemID); err == nil && core.SameIntegrationProvider(wi.Source.Provider, provider) {
			workspace = wi.Source.Workspace
		}
	}
	in, err := integrations.SelectTeam(ctx, s.opts.Store, run.TeamID, provider, run.FactoryID, workspace)
	if err != nil {
		return nil, err
	}
	return &in, nil
}

func (s *Service) integrationServer(ic *integrationCtx) *mcp.Server {
	provider := strings.ToLower(ic.in.Provider)
	if core.SameIntegrationProvider(provider, "teams") {
		provider = "teams"
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "openfactory-" + provider, Version: "1.0.0"}, &mcp.ServerOptions{
		SchemaCache:  s.cache,
		Instructions: fmt.Sprintf("Tools for the factory's %s integration. They act as the integration's app identity.", provider),
	})
	switch provider {
	case "linear":
		t := &linearTools{s: s, ic: ic}
		mcp.AddTool(srv, &mcp.Tool{Name: "linear_get_issue", Description: "Read a Linear issue (UUID or identifier such as ENG-42): title, state, assignee, labels, description and recent comments."}, t.getIssue)
		mcp.AddTool(srv, &mcp.Tool{Name: "linear_search_issues", Description: "Search Linear issues by text."}, t.search)
		if ic.allowWrite {
			mcp.AddTool(srv, &mcp.Tool{Name: "linear_create_comment", Description: "Post a Markdown comment on a Linear issue."}, trialOr(s, ic, "linear_create_comment", t.comment))
		}
	case "jira":
		t := &jiraTools{s: s, ic: ic}
		mcp.AddTool(srv, &mcp.Tool{Name: "jira_get_issue", Description: "Read a Jira issue by key (e.g. OPS-12): summary, status, assignee, description and recent comments."}, t.getIssue)
		mcp.AddTool(srv, &mcp.Tool{Name: "jira_search", Description: "Search Jira issues with JQL."}, t.search)
		if ic.allowWrite {
			mcp.AddTool(srv, &mcp.Tool{Name: "jira_add_comment", Description: "Add a Markdown comment to a Jira issue."}, trialOr(s, ic, "jira_add_comment", t.comment))
		}
	case "slack":
		t := &slackTools{s: s, ic: ic}
		mcp.AddTool(srv, &mcp.Tool{Name: "slack_read_thread", Description: "Read the messages of a Slack thread (channel ID and the parent message ts)."}, t.readThread)
		mcp.AddTool(srv, &mcp.Tool{Name: "slack_channel_history", Description: "Read recent messages of a Slack channel the app is a member of."}, t.history)
		if ic.allowWrite {
			mcp.AddTool(srv, &mcp.Tool{Name: "slack_post_message", Description: "Post a message to a Slack channel, optionally as a thread reply."}, trialOr(s, ic, "slack_post_message", t.post))
		}
	case "teams":
		t := &teamsTools{s: s, ic: ic}
		mcp.AddTool(srv, &mcp.Tool{Name: "teams_list_channels", Description: "List the Microsoft Teams channels that the integration is limited to. An empty list means that the bot can post in each channel where it was mentioned."}, t.listChannels)
		if ic.allowWrite {
			mcp.AddTool(srv, &mcp.Tool{Name: "teams_post_message", Description: "Post a message to a Microsoft Teams channel, optionally as a reply in a thread. The bot cannot read messages."}, trialOr(s, ic, "teams_post_message", t.post))
		}
	}
	return srv
}

func (s *Service) audit(ic *integrationCtx, tool string) {
	s.log.Info("built-in mcp tool call", "provider", ic.in.Provider, "tool", tool, "run", ic.run.ID, "factory", ic.run.FactoryID)
}

func toolText(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func toolErr(err error) *mcp.CallToolResult {
	r := toolText("Error: %v", err)
	r.IsError = true
	return r
}

func toolJSON(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return toolErr(err)
	}
	return toolText("%s", b)
}

func clampLimit(n, def int) int {
	if n <= 0 {
		return def
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

// ---------- Linear ----------

type linearTools struct {
	s  *Service
	ic *integrationCtx
}

func (t *linearTools) client() *linear.Client {
	return linear.NewClient(t.s.opts.LinearGraphQLURL, string(t.ic.token), t.ic.in.Config["token_type"], t.s.http)
}

type linearIssueInput struct {
	ID string `json:"id" jsonschema:"Issue UUID or identifier, e.g. ENG-42"`
}

func (t *linearTools) getIssue(ctx context.Context, _ *mcp.CallToolRequest, in linearIssueInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "linear_get_issue")
	if strings.TrimSpace(in.ID) == "" {
		return toolErr(errors.New("id is required")), nil, nil
	}
	var out struct {
		Issue json.RawMessage `json:"issue"`
	}
	err := t.client().Query(ctx, `query Issue($id: String!) { issue(id: $id) { identifier title url description priorityLabel
		state { name } assignee { name } team { key name } project { name } labels { nodes { name } }
		comments(first: 20) { nodes { body createdAt user { name } } } } }`, map[string]any{"id": in.ID}, &out)
	if err != nil {
		return toolErr(err), nil, nil
	}
	return toolText("%s", out.Issue), nil, nil
}

type linearSearchInput struct {
	Query string `json:"query" jsonschema:"Text to search for in issue titles and descriptions"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results (default 10, at most 50)"`
}

func (t *linearTools) search(ctx context.Context, _ *mcp.CallToolRequest, in linearSearchInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "linear_search_issues")
	if strings.TrimSpace(in.Query) == "" {
		return toolErr(errors.New("query is required")), nil, nil
	}
	var out struct {
		SearchIssues struct {
			Nodes json.RawMessage `json:"nodes"`
		} `json:"searchIssues"`
	}
	err := t.client().Query(ctx, `query Search($term: String!, $first: Int) { searchIssues(term: $term, first: $first) {
		nodes { identifier title url state { name } assignee { name } } } }`,
		map[string]any{"term": in.Query, "first": clampLimit(in.Limit, 10)}, &out)
	if err != nil {
		return toolErr(err), nil, nil
	}
	return toolText("%s", out.SearchIssues.Nodes), nil, nil
}

type linearCommentInput struct {
	IssueID string `json:"issue_id" jsonschema:"Issue UUID or identifier, e.g. ENG-42"`
	Body    string `json:"body" jsonschema:"Markdown comment"`
}

func (t *linearTools) comment(ctx context.Context, _ *mcp.CallToolRequest, in linearCommentInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "linear_create_comment")
	if strings.TrimSpace(in.IssueID) == "" || strings.TrimSpace(in.Body) == "" {
		return toolErr(errors.New("issue_id and body are required")), nil, nil
	}
	if err := t.client().CreateComment(ctx, in.IssueID, in.Body); err != nil {
		return toolErr(err), nil, nil
	}
	return toolText("Commented on %s.", in.IssueID), nil, nil
}

// ---------- Jira ----------

type jiraTools struct {
	s  *Service
	ic *integrationCtx
}

func (t *jiraTools) client() (*forgeutil.Client, error) {
	return jira.NewAPIClient(t.ic.in, t.ic.token, t.s.http)
}

type jiraIssueInput struct {
	Key string `json:"key" jsonschema:"Issue key, e.g. OPS-12"`
}

func (t *jiraTools) getIssue(ctx context.Context, _ *mcp.CallToolRequest, in jiraIssueInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "jira_get_issue")
	if strings.TrimSpace(in.Key) == "" {
		return toolErr(errors.New("key is required")), nil, nil
	}
	c, err := t.client()
	if err != nil {
		return toolErr(err), nil, nil
	}
	var raw struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string          `json:"summary"`
			Description json.RawMessage `json:"description"`
			Status      struct {
				Name string `json:"name"`
			} `json:"status"`
			Assignee *struct {
				DisplayName string `json:"displayName"`
			} `json:"assignee"`
			IssueType struct {
				Name string `json:"name"`
			} `json:"issuetype"`
			Labels  []string `json:"labels"`
			Comment struct {
				Comments []struct {
					Author struct {
						DisplayName string `json:"displayName"`
					} `json:"author"`
					Body    json.RawMessage `json:"body"`
					Created string          `json:"created"`
				} `json:"comments"`
			} `json:"comment"`
		} `json:"fields"`
	}
	path := "/rest/api/3/issue/" + url.PathEscape(in.Key) + "?fields=summary,description,status,assignee,issuetype,labels,comment"
	if err := c.Do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return toolErr(err), nil, nil
	}
	type comment struct {
		Author, Created, Body string
	}
	out := map[string]any{
		"key": raw.Key, "summary": raw.Fields.Summary, "status": raw.Fields.Status.Name, "type": raw.Fields.IssueType.Name,
		"labels": raw.Fields.Labels, "description": jira.Text(raw.Fields.Description),
	}
	if raw.Fields.Assignee != nil {
		out["assignee"] = raw.Fields.Assignee.DisplayName
	}
	cs := raw.Fields.Comment.Comments
	if len(cs) > 20 {
		cs = cs[len(cs)-20:]
	}
	comments := make([]comment, 0, len(cs))
	for _, cm := range cs {
		comments = append(comments, comment{Author: cm.Author.DisplayName, Created: cm.Created, Body: jira.Text(cm.Body)})
	}
	out["comments"] = comments
	return toolJSON(out), nil, nil
}

type jiraSearchInput struct {
	JQL   string `json:"jql" jsonschema:"JQL query, e.g. project = OPS AND status != Done"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results (default 10, at most 50)"`
}

func (t *jiraTools) search(ctx context.Context, _ *mcp.CallToolRequest, in jiraSearchInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "jira_search")
	if strings.TrimSpace(in.JQL) == "" {
		return toolErr(errors.New("jql is required")), nil, nil
	}
	c, err := t.client()
	if err != nil {
		return toolErr(err), nil, nil
	}
	var raw struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
				Status  struct {
					Name string `json:"name"`
				} `json:"status"`
				Assignee *struct {
					DisplayName string `json:"displayName"`
				} `json:"assignee"`
			} `json:"fields"`
		} `json:"issues"`
	}
	body := map[string]any{"jql": in.JQL, "maxResults": clampLimit(in.Limit, 10), "fields": []string{"summary", "status", "assignee"}}
	if err := c.Do(ctx, http.MethodPost, "/rest/api/3/search/jql", body, &raw); err != nil {
		return toolErr(err), nil, nil
	}
	type hit struct {
		Key      string `json:"key"`
		Summary  string `json:"summary"`
		Status   string `json:"status"`
		Assignee string `json:"assignee,omitempty"`
	}
	out := make([]hit, 0, len(raw.Issues))
	for _, is := range raw.Issues {
		h := hit{Key: is.Key, Summary: is.Fields.Summary, Status: is.Fields.Status.Name}
		if is.Fields.Assignee != nil {
			h.Assignee = is.Fields.Assignee.DisplayName
		}
		out = append(out, h)
	}
	return toolJSON(out), nil, nil
}

type jiraCommentInput struct {
	Key  string `json:"key" jsonschema:"Issue key, e.g. OPS-12"`
	Body string `json:"body" jsonschema:"Markdown comment"`
}

func (t *jiraTools) comment(ctx context.Context, _ *mcp.CallToolRequest, in jiraCommentInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "jira_add_comment")
	if strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Body) == "" {
		return toolErr(errors.New("key and body are required")), nil, nil
	}
	c, err := t.client()
	if err != nil {
		return toolErr(err), nil, nil
	}
	if err := c.Do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(in.Key)+"/comment",
		map[string]any{"body": jira.MarkdownToADF(in.Body)}, nil); err != nil {
		return toolErr(err), nil, nil
	}
	return toolText("Commented on %s.", in.Key), nil, nil
}

// ---------- Slack ----------

type slackTools struct {
	s  *Service
	ic *integrationCtx
}

func (t *slackTools) client() *slack.Client {
	return slack.NewClient(t.s.opts.SlackAPIBaseURL, string(t.ic.token), t.s.http)
}

type slackMessage struct {
	User     string `json:"user,omitempty"`
	BotID    string `json:"bot_id,omitempty"`
	Text     string `json:"text"`
	TS       string `json:"ts"`
	ThreadTS string `json:"thread_ts,omitempty"`
}

type slackThreadInput struct {
	Channel string `json:"channel" jsonschema:"Channel ID, e.g. C0123456"`
	TS      string `json:"ts" jsonschema:"ts of the thread's parent message"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum messages (default 50, at most 50)"`
}

func (t *slackTools) readThread(ctx context.Context, _ *mcp.CallToolRequest, in slackThreadInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "slack_read_thread")
	if in.Channel == "" || in.TS == "" {
		return toolErr(errors.New("channel and ts are required")), nil, nil
	}
	var out struct {
		Messages []slackMessage `json:"messages"`
	}
	q := url.Values{"channel": {in.Channel}, "ts": {in.TS}, "limit": {strconv.Itoa(clampLimit(in.Limit, maxListLimit))}}
	if err := t.client().Call(ctx, "conversations.replies", q, &out); err != nil {
		return toolErr(err), nil, nil
	}
	return toolJSON(out.Messages), nil, nil
}

type slackHistoryInput struct {
	Channel string `json:"channel" jsonschema:"Channel ID, e.g. C0123456"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum messages (default 20, at most 50)"`
}

func (t *slackTools) history(ctx context.Context, _ *mcp.CallToolRequest, in slackHistoryInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "slack_channel_history")
	if in.Channel == "" {
		return toolErr(errors.New("channel is required")), nil, nil
	}
	var out struct {
		Messages []slackMessage `json:"messages"`
	}
	q := url.Values{"channel": {in.Channel}, "limit": {strconv.Itoa(clampLimit(in.Limit, 20))}}
	if err := t.client().Call(ctx, "conversations.history", q, &out); err != nil {
		return toolErr(err), nil, nil
	}
	return toolJSON(out.Messages), nil, nil
}

type slackPostInput struct {
	Channel  string `json:"channel" jsonschema:"Channel ID, e.g. C0123456"`
	Text     string `json:"text" jsonschema:"Message text (Slack mrkdwn)"`
	ThreadTS string `json:"thread_ts,omitempty" jsonschema:"Reply in this thread (parent message ts)"`
}

func (t *slackTools) post(ctx context.Context, _ *mcp.CallToolRequest, in slackPostInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "slack_post_message")
	if in.Channel == "" || strings.TrimSpace(in.Text) == "" {
		return toolErr(errors.New("channel and text are required")), nil, nil
	}
	ts, err := t.client().PostMessage(ctx, slack.Message{Channel: in.Channel, Text: in.Text, ThreadTS: in.ThreadTS, Mrkdwn: true})
	if err != nil {
		return toolErr(err), nil, nil
	}
	return toolText("Posted message %s in %s.", ts, in.Channel), nil, nil
}

// ---------- Microsoft Teams ----------

// TeamsPoster posts a message to a Microsoft Teams channel as the bot of an
// integration record. internal/integrations/teams.Handler implements it.
type TeamsPoster interface {
	PostMessage(ctx context.Context, in core.Integration, channelID, rootID, text string) error
}

// SetTeamsPoster sets the poster for builtin/teams. Without one, the write
// tool answers with an error. Call it before the service starts to serve.
func (s *Service) SetTeamsPoster(p TeamsPoster) { s.teams = p }

type teamsTools struct {
	s  *Service
	ic *integrationCtx
}

type teamsNoInput struct{}

func (t *teamsTools) listChannels(_ context.Context, _ *mcp.CallToolRequest, _ teamsNoInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "teams_list_channels")
	var ids []string
	for _, id := range strings.Split(t.ic.in.Config["channel_ids"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if ids == nil {
		ids = []string{}
	}
	return toolJSON(map[string]any{"channel_ids": ids, "limited": len(ids) > 0}), nil, nil
}

type teamsPostInput struct {
	Channel string `json:"channel" jsonschema:"Channel conversation ID, e.g. 19:abc@thread.tacv2"`
	Text    string `json:"text" jsonschema:"Message text (Markdown)"`
	Thread  string `json:"thread,omitempty" jsonschema:"Reply in this thread (ID of the first message of the thread)"`
}

func (t *teamsTools) post(ctx context.Context, _ *mcp.CallToolRequest, in teamsPostInput) (*mcp.CallToolResult, any, error) {
	t.s.audit(t.ic, "teams_post_message")
	if in.Channel == "" || strings.TrimSpace(in.Text) == "" {
		return toolErr(errors.New("channel and text are required")), nil, nil
	}
	if t.s.teams == nil {
		return toolErr(errors.New("the Microsoft Teams bot is not configured on this server")), nil, nil
	}
	if err := t.s.teams.PostMessage(ctx, t.ic.in, in.Channel, in.Thread, in.Text); err != nil {
		return toolErr(err), nil, nil
	}
	return toolText("Posted message in %s.", in.Channel), nil, nil
}
