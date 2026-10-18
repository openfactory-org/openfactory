package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/links"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// ---------- inputs ----------

type listFactoriesIn struct {
	Search string `json:"search,omitempty" jsonschema:"optional case-insensitive filter on factory name or alias"`
}

type schemaIn struct {
	Document string `json:"document,omitempty" jsonschema:"schema document name such as factory.schema.json or agent.schema.json; omit for the catalog"`
	Version  string `json:"version,omitempty" jsonschema:"schema version; defaults to the current version"`
}

type validateIn struct {
	Files map[string]string `json:"files" jsonschema:"the complete definition tree: path relative to the factory root (factory.yaml, agents/<name>/agent.md, ...) mapped to file content"`
}

type emptyIn struct{}

type createTeamIn struct {
	Name      string `json:"name" jsonschema:"the team name the user confirmed"`
	Confirmed bool   `json:"confirmed" jsonschema:"must be true once the user explicitly confirmed the name"`
}

type joinTeamIn struct {
	TeamID string `json:"team_id" jsonschema:"ID of a team from list_teams joinable choices"`
}

type reposIn struct {
	TeamID    string `json:"team_id,omitempty" jsonschema:"team ID; defaults to your team"`
	CodeForge string `json:"code_forge,omitempty" jsonschema:"GITHUB (default) or GITLAB"`
	Query     string `json:"query,omitempty" jsonschema:"optional filter on owner/name"`
}

type trackerIn struct {
	TeamID   string `json:"team_id,omitempty" jsonschema:"team ID; defaults to your team"`
	Provider string `json:"provider" jsonschema:"linear or jira"`
}

type connectionIn struct {
	Provider string `json:"provider" jsonschema:"github, gitlab, slack, linear or jira"`
}

type connectionStatusIn struct {
	ConnectionID string `json:"connection_id,omitempty" jsonschema:"connection_id returned by start_connection"`
	Provider     string `json:"provider,omitempty" jsonschema:"alternatively, the provider to check"`
}

type repoIn struct {
	CodeForge string `json:"codeForge,omitempty" jsonschema:"GITHUB, GITLAB or AZURE_DEVOPS"`
	Owner     string `json:"owner"`
	Name      string `json:"name"`
}

type createFactoryIn struct {
	TeamID       string   `json:"team_id,omitempty" jsonschema:"team ID; defaults to your team"`
	Name         string   `json:"name" jsonschema:"factory name"`
	Alias        string   `json:"alias,omitempty" jsonschema:"foreman handle used for @-mentions (up to 60 letters, digits, spaces, '.', '_', '-')"`
	Description  string   `json:"description,omitempty"`
	Repositories []repoIn `json:"repositories,omitempty" jsonschema:"repositories the factory works on"`
	Integrations []string `json:"integrations,omitempty" jsonschema:"integrations to attach: slack, microsoft-teams, linear or jira (linear and jira are mutually exclusive)"`
	Agents       []string `json:"agents,omitempty" jsonschema:"default agents besides the foreman: triage, spec, implement, review; omit for all"`
	Model        string   `json:"model,omitempty" jsonschema:"default model for agents; a Claude model selects the claude harness and an OpenAI model selects codex; defaults to claude-opus-5-5"`
}

type listWorkItemsIn struct {
	FactoryID string `json:"factory_id" jsonschema:"factory UID"`
	Creator   string `json:"creator,omitempty" jsonschema:"'me' or a user ID"`
	Stage     string `json:"stage,omitempty" jsonschema:"comma-separated stages: intake, triage, planning, awaiting_approval, building, reviewing, handoff, complete, cancelled"`
	Since     string `json:"since,omitempty" jsonschema:"RFC 3339 time or YYYY-MM-DD"`
	Limit     int    `json:"limit,omitempty" jsonschema:"page size (default 20, max 100)"`
	Cursor    string `json:"cursor,omitempty" jsonschema:"next_cursor from a previous call"`
}

type searchWorkItemsIn struct {
	Query string `json:"query" jsonschema:"text to search for in work item titles"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum results (default 20)"`
}

type getWorkItemIn struct {
	WorkItem     string `json:"work_item" jsonschema:"a work item ID or a reference: work item or run URL, pull request or issue URL, owner/repo#123, Slack permalink, Linear or Jira issue key or URL, or a branch name"`
	StartWorking bool   `json:"start_working,omitempty" jsonschema:"also return local setup guidance for continuing the work item in a local checkout"`
}

type messageIn struct {
	WorkItemID string `json:"work_item_id" jsonschema:"work item ID"`
	Message    string `json:"message" jsonschema:"progress, a question, or a blocker for the foreman"`
}

type conversationIn struct {
	WorkItemID string `json:"work_item_id" jsonschema:"work item ID"`
	After      int64  `json:"after,omitempty" jsonschema:"only return messages with a sequence number greater than this"`
}

type sendWorkItemIn struct {
	FactoryID      string `json:"factory_id,omitempty" jsonschema:"target factory UID (required for a new work item)"`
	WorkItemID     string `json:"work_item_id,omitempty" jsonschema:"existing work item to hand work back to"`
	Title          string `json:"title,omitempty" jsonschema:"title of a new work item"`
	Note           string `json:"note" jsonschema:"what the foreman starts from: goal, context, constraints and work already done; when handing back: what changed, what was validated, what remains"`
	Branch         string `json:"branch,omitempty" jsonschema:"pushed branch holding the work"`
	PullRequestURL string `json:"pull_request_url,omitempty" jsonschema:"pull request holding the work"`
	NotifyRoute    string `json:"notify_route,omitempty" jsonschema:"route ID from list_notification_routes to be notified when the work item needs you or finishes"`
}

type routesIn struct {
	FactoryID string `json:"factory_id" jsonschema:"factory UID"`
}

type completeIn struct {
	WorkItemID string `json:"work_item_id" jsonschema:"work item ID"`
	Note       string `json:"note,omitempty" jsonschema:"closing note"`
}

// ---------- registration ----------

func (s *Server) registerTools() {
	tool(s, "list_factories", "Lists the factories you can access.", true, s.listFactories)
	tool(s, "get_factory_file_schema", "Returns the JSON Schema documents for factory definition files, as a catalog or one document at a time.", true, s.getSchema)
	tool(s, "validate_factory_files", "Validates a complete factory definition tree without saving or applying it.", true, s.validateFiles)
	tool(s, "list_teams", "Lists current memberships and first-time joinable team choices.", true, s.listTeams)
	tool(s, "create_team", "Creates the authenticated user's first team with a confirmed name.", false, s.createTeam)
	tool(s, "join_team", "Joins a team selected from the first-time discovery choices.", false, s.joinTeam)
	tool(s, "list_forge_repositories", "Lists repositories available through a team's connected GitHub or GitLab account.", true, s.listRepositories)
	tool(s, "list_tracker_scopes", "Lists the Linear teams or Jira projects that can be scoped to route issues to a new factory.", true, s.listTrackerScopes)
	tool(s, "start_connection", "Starts or checks setup for GitHub, GitLab, Slack, Linear, or Jira.", false, s.startConnection)
	tool(s, "get_connection_status", "Checks whether a browser authorization flow completed.", true, s.connectionStatus)
	tool(s, "create_factory", "Creates a factory with the selected repositories, integrations, and optional factory agents.", false, s.createFactory)
	tool(s, "list_work_items", "Lists the work items in one factory, with filters such as creator, stage, and date.", true, s.listWorkItems)
	tool(s, "search_work_items", "Searches work item titles across all factories you can access.", true, s.searchWorkItems)
	tool(s, "get_work_item", "Reads a work item's status, run history, and outputs. Accepts a work item ID or a reference such as a URL, issue, pull request, or branch. With start_working=true, also returns local setup guidance.", false, s.getWorkItem)
	tool(s, "message_foreman", "Sends a message to a work item's foreman.", false, s.messageForeman)
	tool(s, "get_conversation", "Reads a work item's foreman conversation.", true, s.getConversation)
	tool(s, "send_work_item", "Creates a new work item, or hands work back to an existing one.", false, s.sendWorkItem)
	tool(s, "list_notification_routes", "Lists the notification destinations available to you in a factory.", true, s.listNotificationRoutes)
	tool(s, "complete_work_item", "Marks a work item complete.", false, s.completeWorkItem)
}

// ---------- factories & schemas ----------

type factorySummary struct {
	UID         string `json:"uid"`
	Name        string `json:"name"`
	Alias       string `json:"alias"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	URL         string `json:"url"`
}

func (s *Server) factorySummary(f *core.Factory) factorySummary {
	return factorySummary{UID: f.ID, Name: f.Name, Alias: f.Alias, Description: f.Description, Source: string(f.Source), URL: s.api.FactoryURL(f.ID)}
}

func (s *Server) listFactories(ctx context.Context, p *api.Principal, in listFactoriesIn) (any, error) {
	fs, err := s.opts.Store.ListFactories(ctx, p.TeamID, in.Search)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(in.Search)
	out := []factorySummary{}
	for i := range fs {
		if q != "" && !strings.Contains(strings.ToLower(fs[i].Name), q) && !strings.Contains(strings.ToLower(fs[i].Alias), q) {
			continue
		}
		out = append(out, s.factorySummary(&fs[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return map[string]any{"factories": out}, nil
}

func (s *Server) getSchema(_ context.Context, _ *api.Principal, in schemaIn) (any, error) {
	version := in.Version
	if version == "" {
		version = s.api.SchemaVersion()
	}
	if in.Document != "" {
		doc, err := s.api.SchemaDocument(version, in.Document)
		if err != nil {
			return nil, err
		}
		return map[string]any{"version": version, "document": in.Document, "schema": doc}, nil
	}
	docs, err := s.api.SchemaDocuments(version)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(docs))
	for n := range docs {
		names = append(names, n)
	}
	sort.Strings(names)
	return map[string]any{
		"version":   version,
		"documents": names,
		"url":       s.opts.PublicURL + "/api/v1/factory-files/schemas/" + version + "/{document}",
		"applies_to": map[string]string{
			"factory.schema.json": "factory.yaml", "agent.schema.json": "agents/<name>/agent.md frontmatter",
			"automation.schema.json": "automations/<name>/automation.md frontmatter", "scorer.schema.json": "scorers/<name>/scorer.md frontmatter",
			"machine.schema.json": "machines/<name>.yaml", "webhook.schema.json": "webhooks/<name>.yaml",
			"benchmark_suite.schema.json": "benchmarks/<suite>/suite.yaml", "benchmark_suite_task.schema.json": "benchmarks/<suite>/tasks/<task>.yaml",
			"common.schema.json": "shared definitions",
		},
	}, nil
}

func (s *Server) validateFiles(_ context.Context, _ *api.Principal, in validateIn) (any, error) {
	return s.api.ValidateFiles(in.Files)
}

// ---------- teams & onboarding ----------

func (s *Server) listTeams(ctx context.Context, p *api.Principal, _ emptyIn) (any, error) {
	team, err := s.opts.Store.GetTeam(ctx, p.TeamID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"memberships": []map[string]any{{"team_id": team.ID, "name": team.Name, "role": p.Role}},
		"joinable":    []any{},
		"note":        "Self-hosted API keys belong to one team; ask an admin to add you to another team.",
	}, nil
}

func (s *Server) createTeam(ctx context.Context, p *api.Principal, in createTeamIn) (any, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, invalid("name is required")
	}
	if !in.Confirmed {
		return nil, invalid("confirm the team name with the user, then call again with confirmed=true")
	}
	team, err := s.opts.Store.GetTeam(ctx, p.TeamID)
	if err != nil {
		return nil, err
	}
	return nil, api.Errf(api.CodeConflict, "you are already a member of team %q (%s); a first team can only be created once", team.Name, team.ID)
}

func (s *Server) joinTeam(ctx context.Context, p *api.Principal, in joinTeamIn) (any, error) {
	if in.TeamID == p.TeamID {
		return map[string]any{"team_id": p.TeamID, "status": "already_member"}, nil
	}
	return nil, api.Errf(api.CodeOperationNotSupported, "team %q is not among your joinable teams; ask one of its admins to add you", in.TeamID)
}

func (s *Server) checkTeam(p *api.Principal, teamID string) error {
	if teamID != "" && teamID != p.TeamID {
		return api.Errf(api.CodeResourceNotFound, "team %s not found", teamID)
	}
	return nil
}

var forgeProviders = map[string]string{"GITHUB": "github", "GITLAB": "gitlab"}

func (s *Server) listRepositories(ctx context.Context, p *api.Principal, in reposIn) (any, error) {
	if err := s.checkTeam(p, in.TeamID); err != nil {
		return nil, err
	}
	forge := strings.ToUpper(in.CodeForge)
	if forge == "" {
		forge = "GITHUB"
	}
	if _, ok := forgeProviders[forge]; !ok {
		return nil, invalid("code_forge must be GITHUB or GITLAB")
	}
	if s.opts.ListRepositories == nil {
		return nil, api.Errf(api.CodeIntegrationNotConfigured, "repository listing is not available; call start_connection with provider %q", forgeProviders[forge])
	}
	repos, err := s.opts.ListRepositories(ctx, p.TeamID, forge, in.Query)
	if err != nil {
		return nil, err
	}
	if repos == nil {
		repos = []Repository{}
	}
	return map[string]any{"code_forge": forge, "repositories": repos}, nil
}

func (s *Server) listTrackerScopes(ctx context.Context, p *api.Principal, in trackerIn) (any, error) {
	if err := s.checkTeam(p, in.TeamID); err != nil {
		return nil, err
	}
	prov := strings.ToLower(in.Provider)
	if prov != "linear" && prov != "jira" {
		return nil, invalid("provider must be linear or jira")
	}
	if s.opts.ListTrackerScopes == nil {
		return nil, api.Errf(api.CodeIntegrationNotConfigured, "%s is not connected; call start_connection with provider %q", prov, prov)
	}
	scopes, err := s.opts.ListTrackerScopes(ctx, p.TeamID, prov)
	if err != nil {
		return nil, err
	}
	if scopes == nil {
		scopes = []TrackerScope{}
	}
	return map[string]any{"provider": prov, "scopes": scopes}, nil
}

var connectionProviders = []string{"github", "gitlab", "slack", "linear", "jira"}

func (s *Server) existingIntegration(ctx context.Context, teamID, provider string) (*core.Integration, error) {
	ins, err := s.opts.Store.ListIntegrations(ctx, teamID)
	if err != nil {
		return nil, err
	}
	for i := range ins {
		if ins[i].Provider == provider {
			return &ins[i], nil
		}
	}
	return nil, nil
}

func (s *Server) startConnection(ctx context.Context, p *api.Principal, in connectionIn) (any, error) {
	prov := strings.ToLower(in.Provider)
	if !slices.Contains(connectionProviders, prov) {
		return nil, invalid("provider must be one of %s", strings.Join(connectionProviders, ", "))
	}
	if s.opts.StartConnection != nil {
		return s.opts.StartConnection(ctx, p.TeamID, prov)
	}
	if in, err := s.existingIntegration(ctx, p.TeamID, prov); err != nil {
		return nil, err
	} else if in != nil {
		return &Connection{Provider: prov, Status: "connected", Integration: in.ID}, nil
	}
	if !p.Can(core.RoleAdmin) {
		return nil, api.Errf(api.CodeNotAuthorized, "%s is not connected and connecting team integrations requires an admin", prov)
	}
	id := core.NewID("conn")
	if err := s.opts.Store.KVSet(ctx, "mcp:connection:"+id, p.TeamID+"|"+prov); err != nil {
		return nil, err
	}
	return &Connection{ID: id, Provider: prov, Status: "pending",
		AuthorizeURL: links.Integrations(s.opts.PublicURL, prov, id),
		Detail:       "Open authorize_url in a browser, connect the integration on the Integrations page, then call get_connection_status."}, nil
}

func (s *Server) connectionStatus(ctx context.Context, p *api.Principal, in connectionStatusIn) (any, error) {
	if in.ConnectionID == "" && in.Provider == "" {
		return nil, invalid("connection_id or provider is required")
	}
	if in.ConnectionID != "" && s.opts.ConnectionStatus != nil {
		return s.opts.ConnectionStatus(ctx, p.TeamID, in.ConnectionID)
	}
	prov := strings.ToLower(in.Provider)
	if in.ConnectionID != "" {
		v, err := s.opts.Store.KVGet(ctx, "mcp:connection:"+in.ConnectionID)
		if err != nil {
			return nil, api.Errf(api.CodeResourceNotFound, "connection %s not found", in.ConnectionID)
		}
		team, provider, _ := strings.Cut(v, "|")
		if team != p.TeamID {
			return nil, api.Errf(api.CodeResourceNotFound, "connection %s not found", in.ConnectionID)
		}
		prov = provider
	}
	in2, err := s.existingIntegration(ctx, p.TeamID, prov)
	if err != nil {
		return nil, err
	}
	if in2 != nil {
		return &Connection{ID: in.ConnectionID, Provider: prov, Status: "connected", Integration: in2.ID}, nil
	}
	return &Connection{ID: in.ConnectionID, Provider: prov, Status: "pending", Detail: "authorization has not completed yet"}, nil
}

func (s *Server) createFactory(ctx context.Context, p *api.Principal, in createFactoryIn) (any, error) {
	if err := s.checkTeam(p, in.TeamID); err != nil {
		return nil, err
	}
	req := api.CreateFactoryRequest{Name: in.Name, Alias: in.Alias, Description: in.Description, Model: in.Model,
		Definition: []byte(`"defaults"`)}
	for _, r := range in.Repositories {
		req.Repositories = append(req.Repositories, core.RepoRef{CodeForge: strings.ToUpper(r.CodeForge), Owner: r.Owner, Name: r.Name})
	}
	for _, t := range in.Integrations {
		req.Integrations = append(req.Integrations, core.IntegrationDecl{Type: t})
	}
	if in.Agents != nil {
		agents := in.Agents
		req.Agents = &agents
	}
	res, err := s.api.CreateFactory(ctx, p, req)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"factory":       res.Factory,
		"dashboard_url": res.Factory.URL,
		"next_steps": "Share the dashboard link with the user. If the web app is open on its setup screen, refresh it to pick up the new factory. " +
			"Send the first work with send_work_item.",
	}, nil
}

// ---------- work items ----------

type runSummary struct {
	RunID       string     `json:"run_id"`
	AgentName   string     `json:"agent_name,omitempty"`
	Name        string     `json:"name,omitempty"`
	State       string     `json:"state"`
	ParentRunID string     `json:"parent_run_id,omitempty"`
	Result      string     `json:"result,omitempty"`
	Error       string     `json:"error,omitempty"`
	URL         string     `json:"url"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

type workItemView struct {
	ID           string                `json:"id"`
	FactoryID    string                `json:"factory_id"`
	Title        string                `json:"title"`
	Stage        core.Stage            `json:"stage"`
	URL          string                `json:"url"`
	Source       core.SourceRef        `json:"source"`
	CreatorID    string                `json:"creator_id,omitempty"`
	LocalOwner   string                `json:"local_owner,omitempty"`
	PullRequests []core.PullRequestRef `json:"pull_requests,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
}

func (s *Server) workItemView(w *core.WorkItem) workItemView {
	return workItemView{ID: w.ID, FactoryID: w.FactoryID, Title: w.Title, Stage: w.Stage, URL: s.api.WorkItemURL(w.FactoryID, w.ID),
		Source: w.Source, CreatorID: w.CreatorID, LocalOwner: w.LocalOwner, PullRequests: w.PullRequests, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
}

func (s *Server) factoryOf(ctx context.Context, p *api.Principal, id string) (*core.Factory, error) {
	f, err := s.opts.Store.GetFactory(ctx, id)
	if err != nil || f.TeamID != p.TeamID {
		return nil, api.Errf(api.CodeResourceNotFound, "factory %s not found", id)
	}
	return f, nil
}

func (s *Server) listWorkItems(ctx context.Context, p *api.Principal, in listWorkItemsIn) (any, error) {
	f, err := s.factoryOf(ctx, p, in.FactoryID)
	if err != nil {
		return nil, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	flt := core.WorkItemFilter{FactoryID: f.ID, ListOpts: core.ListOpts{Limit: limit + 1, Cursor: in.Cursor}}
	switch in.Creator {
	case "":
	case "me":
		flt.CreatorID = p.UserID()
	default:
		flt.CreatorID = in.Creator
	}
	if in.Stage != "" {
		for _, st := range strings.Split(in.Stage, ",") {
			stage, err := api.ParseStage(st)
			if err != nil {
				return nil, err
			}
			flt.Stages = append(flt.Stages, stage)
		}
	}
	if in.Since != "" {
		t, err := parseTime(in.Since)
		if err != nil {
			return nil, err
		}
		flt.Since = &t
	}
	items, err := s.opts.Store.ListWorkItems(ctx, flt)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		out["next_cursor"] = encodeCursor(last.CreatedAt, last.ID)
	}
	views := make([]workItemView, 0, len(items))
	for i := range items {
		views = append(views, s.workItemView(&items[i]))
	}
	out["work_items"] = views
	return out, nil
}

func parseTime(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	return time.Time{}, invalid("invalid time %q: use RFC 3339 or YYYY-MM-DD", v)
}

func (s *Server) searchWorkItems(ctx context.Context, p *api.Principal, in searchWorkItemsIn) (any, error) {
	q := strings.TrimSpace(in.Query)
	if q == "" {
		return nil, invalid("query is required")
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	fs, err := s.opts.Store.ListFactories(ctx, p.TeamID, "")
	if err != nil {
		return nil, err
	}
	var all []core.WorkItem
	for _, f := range fs {
		items, err := s.opts.Store.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, Search: q, ListOpts: core.ListOpts{Limit: limit}})
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if strings.Contains(strings.ToLower(it.Title), strings.ToLower(q)) {
				all = append(all, it)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt.After(all[j].UpdatedAt) })
	if len(all) > limit {
		all = all[:limit]
	}
	views := make([]workItemView, 0, len(all))
	for i := range all {
		views = append(views, s.workItemView(&all[i]))
	}
	return map[string]any{"work_items": views}, nil
}

func (s *Server) getWorkItem(ctx context.Context, p *api.Principal, in getWorkItemIn) (any, error) {
	wi, err := s.resolveWorkItem(ctx, p, in.WorkItem)
	if err != nil {
		return nil, err
	}
	d, err := s.api.GetWorkItemDetail(ctx, p, wi.ID)
	if err != nil {
		return nil, err
	}
	runs := make([]runSummary, 0, len(d.Runs))
	active := 0
	var output string
	for _, r := range d.Runs {
		if !r.Run.State.Terminal() {
			active++
		}
		runs = append(runs, runSummary{RunID: r.RunID, AgentName: r.AgentName, Name: r.Name, State: r.State, ParentRunID: r.ParentRunID,
			Result: truncate(r.Result, 2000), Error: r.Error, URL: r.RunURL, CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt})
		if r.RunID == d.RootRunID {
			output = r.Result
		}
	}
	pending := []map[string]any{}
	for _, it := range d.Inbox {
		// The personal queue: only the items that need the caller.
		if !api.InboxForCaller(&it.InboxItem, p) {
			continue
		}
		item := map[string]any{"id": it.ID, "kind": it.Kind, "title": it.Title, "options": it.Options}
		if len(it.Questions) > 0 {
			item["questions"] = it.Questions
		}
		pending = append(pending, item)
	}
	out := map[string]any{
		"work_item":      s.workItemView(&d.WorkItem),
		"runs":           runs,
		"active_runs":    active,
		"output":         truncate(output, 8000),
		"needs_decision": pending,
	}
	if in.StartWorking {
		g, err := s.startWorking(ctx, p, &d.WorkItem, active)
		if err != nil {
			return nil, err
		}
		out["local_setup"] = g
		out["work_item"] = s.workItemView(&d.WorkItem)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (truncated)"
}

func (s *Server) messageForeman(ctx context.Context, p *api.Principal, in messageIn) (any, error) {
	wi, err := s.api.MessageForeman(ctx, p, in.WorkItemID, in.Message)
	if err != nil {
		return nil, err
	}
	return map[string]any{"work_item_id": wi.ID, "run_id": wi.RootRunID, "status": "sent",
		"note": "Messaging keeps the factory informed but does not move or hand back the work item. Read replies with get_conversation."}, nil
}

type conversationMessage struct {
	Seq       int64     `json:"seq"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Server) getConversation(ctx context.Context, p *api.Principal, in conversationIn) (any, error) {
	d, err := s.api.GetWorkItemDetail(ctx, p, in.WorkItemID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"work_item_id": d.ID, "stage": d.Stage, "messages": []conversationMessage{}}
	if d.RootRunID == "" {
		return out, nil
	}
	msgs, err := s.opts.Store.ListRunMessages(ctx, d.RootRunID, in.After)
	if err != nil {
		return nil, err
	}
	conv := []conversationMessage{}
	var last int64
	for _, m := range msgs {
		last = m.Seq
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		conv = append(conv, conversationMessage{Seq: m.Seq, Role: m.Role, Content: m.Content, CreatedAt: m.CreatedAt})
	}
	out["run_id"] = d.RootRunID
	out["messages"] = conv
	if last > 0 {
		out["last_seq"] = last
	}
	for _, r := range d.Runs {
		if r.RunID == d.RootRunID {
			out["foreman_state"] = r.State
		}
	}
	return out, nil
}

func (s *Server) sendWorkItem(ctx context.Context, p *api.Principal, in sendWorkItemIn) (any, error) {
	if strings.TrimSpace(in.Note) == "" {
		return nil, invalid("note is required")
	}
	var pr *core.PullRequestRef
	if in.PullRequestURL != "" {
		ref, ok := parsePullRequestURL(in.PullRequestURL)
		if !ok {
			return nil, invalid("pull_request_url is not a recognized pull or merge request URL")
		}
		ref.Branch = in.Branch
		pr = &ref
	}
	if in.WorkItemID != "" {
		return s.handBack(ctx, p, in, pr)
	}
	if in.FactoryID == "" {
		return nil, invalid("factory_id is required for a new work item (or pass work_item_id to hand work back)")
	}
	f, err := s.factoryOf(ctx, p, in.FactoryID)
	if err != nil {
		return nil, err
	}
	if in.NotifyRoute != "" {
		if err := s.checkRoute(ctx, f, in.NotifyRoute); err != nil {
			return nil, err
		}
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = firstLine(in.Note)
	}
	prompt := in.Note
	if refs := workRefs(in.Branch, in.PullRequestURL); refs != "" {
		prompt += "\n\n" + refs
	}
	wi, run, err := s.opts.Engine.Dispatch(ctx, core.DispatchRequest{
		FactoryID: f.ID, Prompt: prompt, Title: title, CreatorID: p.UserID(), TriggeredBy: "mcp",
		Source: core.SourceRef{Provider: "mcp", Kind: "work_item", Title: title},
	})
	if err != nil {
		return nil, err
	}
	if pr != nil {
		if err := s.opts.Engine.LinkPullRequest(ctx, wi.ID, *pr); err != nil {
			return nil, err
		}
	}
	if in.NotifyRoute != "" {
		if err := s.setLabel(ctx, wi.ID, "notify_route", in.NotifyRoute); err != nil {
			return nil, err
		}
	}
	return map[string]any{"work_item_id": wi.ID, "run_id": run.ID, "url": s.api.WorkItemURL(f.ID, wi.ID), "run_url": s.api.RunURL(run.ID),
		"status": "created", "note": "The foreman takes it from here and reports progress on the work item's conversation."}, nil
}

func (s *Server) handBack(ctx context.Context, p *api.Principal, in sendWorkItemIn, pr *core.PullRequestRef) (any, error) {
	d, err := s.api.GetWorkItemDetail(ctx, p, in.WorkItemID)
	if err != nil {
		return nil, err
	}
	wi := &d.WorkItem
	if wi.Stage.Terminal() {
		return nil, api.Errf(api.CodeConflict, "work item %s is already %s", wi.ID, wi.Stage)
	}
	f, err := s.factoryOf(ctx, p, wi.FactoryID)
	if err != nil {
		return nil, err
	}
	// A hand-back is a follow-up (or a new run) for the work item: the
	// same restrictFollowupsToStarter check as message_foreman.
	if err := s.api.CheckFollowupAllowed(ctx, p, f, wi); err != nil {
		return nil, err
	}
	if in.NotifyRoute != "" {
		if err := s.checkRoute(ctx, f, in.NotifyRoute); err != nil {
			return nil, err
		}
	}
	if pr != nil && !slices.ContainsFunc(wi.PullRequests, func(x core.PullRequestRef) bool { return x.URL == pr.URL }) {
		if err := s.opts.Engine.LinkPullRequest(ctx, wi.ID, *pr); err != nil {
			return nil, err
		}
	}
	who := p.UserID()
	if p.User != nil && p.User.Name != "" {
		who = p.User.Name
	}
	msg := "## Work handed back from a local session (" + who + ")\n\n" + in.Note
	if refs := workRefs(in.Branch, in.PullRequestURL); refs != "" {
		msg += "\n\n" + refs
	}
	msg += "\n\nDecide the next step for this work item."
	runID := wi.RootRunID
	if runID != "" {
		if err := s.opts.Engine.FollowUp(ctx, runID, msg, p.ActorID()); err != nil {
			return nil, err
		}
	} else {
		_, run, err := s.opts.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: wi.FactoryID, Prompt: msg, Source: wi.Source,
			Title: wi.Title, CreatorID: p.UserID(), TriggeredBy: "mcp"})
		if err != nil {
			return nil, err
		}
		runID = run.ID
	}
	// Clear the local owner and record the hand-back on the timeline.
	if _, err := storeutil.UpdateWorkItem(ctx, s.opts.Store, wi.ID, func(cur *core.WorkItem) error {
		cur.LocalOwner = ""
		if in.NotifyRoute != "" {
			if cur.Labels == nil {
				cur.Labels = map[string]string{}
			}
			cur.Labels["notify_route"] = in.NotifyRoute
		}
		cur.UpdatedAt = time.Now().UTC()
		return nil
	}); err != nil {
		return nil, err
	}
	_ = s.opts.Store.AppendWorkItemEvent(ctx, &core.WorkItemEvent{ID: core.NewID("wie"), WorkItemID: wi.ID, Kind: "note",
		Actor: p.ActorID(), Data: mustJSON(map[string]any{"handed_back": true, "branch": in.Branch, "pull_request_url": in.PullRequestURL}),
		CreatedAt: time.Now().UTC()})
	return map[string]any{"work_item_id": wi.ID, "run_id": runID, "url": s.api.WorkItemURL(wi.FactoryID, wi.ID), "status": "handed_back",
		"note": "The work returned to the same work item; the foreman decides the next step. Handing back does not complete the work item."}, nil
}

func workRefs(branch, prURL string) string {
	var parts []string
	if branch != "" {
		parts = append(parts, "Branch: `"+branch+"`")
	}
	if prURL != "" {
		parts = append(parts, "Pull request: "+prURL)
	}
	return strings.Join(parts, "\n")
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(strings.TrimLeft(l, "# ")); l != "" {
			if len(l) > 80 {
				l = l[:79] + "…"
			}
			return l
		}
	}
	return "Work item from a coding agent"
}

func (s *Server) setLabel(ctx context.Context, id, k, v string) error {
	_, err := storeutil.UpdateWorkItem(ctx, s.opts.Store, id, func(wi *core.WorkItem) error {
		if wi.Labels == nil {
			wi.Labels = map[string]string{}
		}
		wi.Labels[k] = v
		return nil
	})
	return err
}

// ---------- notification routes ----------

type route struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

func (s *Server) routes(ctx context.Context, f *core.Factory) ([]route, error) {
	out := []route{{ID: "inbox", Kind: "inbox", Description: "Your Open Factory inbox (" + links.Inbox(s.opts.PublicURL) + ")"}}
	ints, err := s.opts.Store.GetFactoryIntegrations(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(ints))
	for k := range ints {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "slack":
			out = append(out, route{ID: "slack_dm", Kind: "slack", Description: "Slack direct message (requires a linked Slack account)"})
		case "microsoft-teams", "microsoft_teams":
			out = append(out, route{ID: "teams_dm", Kind: "microsoft_teams", Description: "Microsoft Teams chat message (requires a linked account)"})
		case "linear":
			out = append(out, route{ID: "linear_issue", Kind: "linear", Description: "Comment on the work item's Linear issue, when it has one"})
		case "jira":
			out = append(out, route{ID: "jira_issue", Kind: "jira", Description: "Comment on the work item's Jira issue, when it has one"})
		}
	}
	return out, nil
}

func (s *Server) checkRoute(ctx context.Context, f *core.Factory, id string) error {
	rs, err := s.routes(ctx, f)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(rs, func(r route) bool { return r.ID == id }) {
		return invalid("unknown notify_route %q; call list_notification_routes", id)
	}
	return nil
}

func (s *Server) listNotificationRoutes(ctx context.Context, p *api.Principal, in routesIn) (any, error) {
	f, err := s.factoryOf(ctx, p, in.FactoryID)
	if err != nil {
		return nil, err
	}
	rs, err := s.routes(ctx, f)
	if err != nil {
		return nil, err
	}
	return map[string]any{"factory_id": f.ID, "routes": rs, "note": "Delivery is best-effort; treat notifications as a convenience."}, nil
}

func (s *Server) completeWorkItem(ctx context.Context, p *api.Principal, in completeIn) (any, error) {
	wi, err := s.api.CompleteWorkItem(ctx, p, in.WorkItemID, in.Note)
	if err != nil {
		return nil, err
	}
	return map[string]any{"work_item": s.workItemView(wi), "status": "completed"}, nil
}

func mustJSON(v any) []byte {
	b, err := jsonMarshal(v)
	if err != nil {
		panic(fmt.Sprintf("mcpserver: marshal: %v", err))
	}
	return b
}
