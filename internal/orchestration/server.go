// Package orchestration serves the per-run MCP tool server ("factory") that every
// agent run connects to at /mcp/run/{runId}. It is how the foreman (on any
// harness) starts child agents, asks humans, posts updates and moves the work item.
package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/internal/runauth"
)

// Server is the orchestration MCP endpoint.
type Server struct {
	engine core.Engine
	store  core.Store
	cache  *mcp.SchemaCache
	log    *slog.Logger
	verify *runauth.Verifier
	turn   func(ctx context.Context, runID string) (string, error)
	// propose stores a proposed definition change. Nil hides the tool.
	propose ProposeFunc
	// redact removes the secret values of a run from the text that an agent
	// posts. Nil changes nothing.
	redact RedactFunc
	// limiter is the rate limit of send_message and follow_up_agent per
	// sender and target.
	limiter *pairLimiter
}

// RedactFunc returns a function that removes the secret values of a run from
// text (engine.Planner.RedactFor).
type RedactFunc func(ctx context.Context, run *core.Run) func(string) string

// SetRedactor makes the server remove the secret values of the run from the
// text of post_update, ask_human and request_approval.
func (s *Server) SetRedactor(fn RedactFunc) { s.redact = fn }

// redactFor returns the redaction function of a run. It never returns nil.
func (s *Server) redactFor(ctx context.Context, runID string) func(string) string {
	if s.redact == nil {
		return func(v string) string { return v }
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return func(v string) string { return v }
	}
	return s.redact(ctx, run)
}

// ProposeFunc stores a proposed version of the managed definition of the
// factory of a run (scoring.Service.ProposeDefinitionChange).
type ProposeFunc func(ctx context.Context, runID string, files map[string]string, del []string, message string) (*core.DefinitionVersion, error)

// SetProposer enables the tool propose_definition_change.
func (s *Server) SetProposer(fn ProposeFunc) { s.propose = fn }

// NewServer creates the orchestration server. It refuses every request until
// SetVerifier gives it the check of the bearer token.
func NewServer(eng core.Engine, store core.Store) *Server {
	return &Server{engine: eng, store: store, cache: mcp.NewSchemaCache(), log: slog.Default().With("component", "orchestration"), limiter: newPairLimiter(store, sendMessageLimit, sendMessageWindow)}
}

// SetLogger replaces the server's logger.
func (s *Server) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l.With("component", "orchestration")
	}
}

// SetVerifier makes the server check the bearer token with v. Without a
// verifier the server refuses every request.
func (s *Server) SetVerifier(v *runauth.Verifier) { s.verify = v }

// Register mounts the MCP endpoint at /mcp/run/{runId}.
func (s *Server) Register(mux *http.ServeMux) {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return s.serverFor(r.PathValue("runId"))
	}, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: true, // every request carries a per-run bearer token
		Logger:                     s.log,
	})
	mux.Handle("/mcp/run/{runId}", s.authenticate(h))
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("runId")
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok {
			token = ""
		}
		if s.verify == nil {
			runauth.WriteError(w, runauth.ErrUnauthorized)
			return
		}
		if err := s.verify.Verify(r.Context(), runID, token); err != nil {
			if !errors.Is(err, runauth.ErrUnauthorized) && !errors.Is(err, runauth.ErrSuperseded) {
				s.log.Error("check session token", "run", runID, "err", err)
			}
			runauth.WriteError(w, err)
			return
		}
		if _, err := s.store.GetRun(r.Context(), runID); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "run not found")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "could not load run")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- tool inputs ----------

type childRunInput struct {
	Name             string            `json:"name" jsonschema:"Label for the child run (does not select the agent)"`
	AgentIdentityUID string            `json:"agent_identity_uid" jsonschema:"UID of the configured agent to run, from list_agents"`
	Prompt           string            `json:"prompt" jsonschema:"Task for this child, appended to base_prompt"`
	Metadata         map[string]string `json:"metadata,omitempty" jsonschema:"Per-child run metadata (string keys and values), exposed as OF_METADATA_* environment variables; overrides batch and parent values"`
	Model            string            `json:"model,omitempty" jsonschema:"Optional model override for this child"`
	Machine          string            `json:"machine,omitempty" jsonschema:"Optional machine name override for this child"`
	EnvironmentID    string            `json:"environment_id,omitempty" jsonschema:"Optional environment override for this child"`
	WorkerHost       string            `json:"worker_host,omitempty" jsonschema:"Optional worker host override for this child"`
	Skills           []string          `json:"skills,omitempty" jsonschema:"Optional skills to attach to this child: the name of a factory skill, of a skill of another agent, of a built-in skill, or a skill spec owner/repo:path"`
}

type runAgentsInput struct {
	Summary         string            `json:"summary" jsonschema:"Short description of why these agents are being started"`
	BasePrompt      string            `json:"base_prompt,omitempty" jsonschema:"Instructions shared by every child; each child's prompt is appended"`
	Remote          map[string]any    `json:"remote,omitempty" jsonschema:"Run the children in the cloud (always the case here); may carry environment_id, worker_host, machine or model for all children"`
	Metadata        map[string]string `json:"metadata,omitempty" jsonschema:"Batch run metadata applied to every child (child values win)"`
	AgentRunConfigs []childRunInput   `json:"agent_run_configs" jsonschema:"One entry per child run"`
}

type followUpInput struct {
	RunID   string `json:"run_id" jsonschema:"ID of one of your child runs"`
	Message string `json:"message" jsonschema:"Message delivered to the child as its next user message"`
}

type askHumanInput struct {
	Question string   `json:"question" jsonschema:"The question for the requester"`
	Context  string   `json:"context,omitempty" jsonschema:"Background needed to answer"`
	Options  []string `json:"options,omitempty" jsonschema:"Optional suggested answers"`
	// MultiSelect and RecommendedOption shape the question in the inbox.
	MultiSelect       bool   `json:"multi_select,omitempty" jsonschema:"The person can pick more than one option"`
	RecommendedOption string `json:"recommended_option,omitempty" jsonschema:"The option you recommend. It must be one of options"`
}

type requestApprovalInput struct {
	Title   string   `json:"title" jsonschema:"What needs approval, e.g. 'Spec: purchase-order field'"`
	Summary string   `json:"summary" jsonschema:"What is being approved and why"`
	Links   []string `json:"links,omitempty" jsonschema:"Links to review (spec pull request, documents)"`
	Options []string `json:"options,omitempty" jsonschema:"Decision options (default approve/reject)"`
}

type postUpdateInput struct {
	Text string `json:"text" jsonschema:"Markdown update posted where the request started"`
	Kind string `json:"kind,omitempty" jsonschema:"update (default), handoff (finished result ready for the team) or error"`
}

type setStageInput struct {
	Stage string `json:"stage" jsonschema:"One of intake, triage, planning, awaiting_approval, building, reviewing, handoff, complete, cancelled"`
	Note  string `json:"note,omitempty" jsonschema:"Why the stage changed"`
}

type linkPRInput struct {
	URL    string `json:"url" jsonschema:"Pull request (merge request) URL"`
	Repo   string `json:"repo,omitempty" jsonschema:"owner/name; derived from the URL when omitted"`
	Number int    `json:"number,omitempty" jsonschema:"Pull request number; derived from the URL when omitted"`
	Branch string `json:"branch,omitempty" jsonschema:"Head branch"`
	Forge  string `json:"forge,omitempty" jsonschema:"GITHUB, GITLAB or AZURE_DEVOPS; derived from the URL when omitted"`
	State  string `json:"state,omitempty" jsonschema:"open (default), draft, merged or closed"`
}

type proposeInput struct {
	Files   map[string]string `json:"files,omitempty" jsonschema:"Changed or new files of the factory definition: path relative to the definition root (for example agents/implement/agent.md) mapped to the full new content"`
	Delete  []string          `json:"delete,omitempty" jsonschema:"Paths of definition files to remove"`
	Message string            `json:"message" jsonschema:"What the change does and why, for the reviewer"`
}

type completeInput struct {
	Note string `json:"note,omitempty" jsonschema:"Closing note, e.g. which pull request merged"`
}

type emptyInput struct{}

// ---------- server ----------

func (s *Server) serverFor(runID string) *mcp.Server {
	if runID == "" {
		return nil
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "openfactory-orchestration", Version: "1.0.0"}, &mcp.ServerOptions{
		SchemaCache:  s.cache,
		Instructions: "Factory orchestration tools for this run: start agents, ask humans, post updates and manage the work item.",
	})
	t := &tools{s: s, runID: runID}
	mcp.AddTool(srv, &mcp.Tool{Name: "run_agents", Description: "Start one or more configured factory agents as child runs. Each child reports back to you as a follow-up message when it finishes a turn."}, withIdempotency(t, "run_agents", t.runAgents))
	mcp.AddTool(srv, &mcp.Tool{Name: "list_agents", Description: "List the factory agents you may start, with their UIDs, types and descriptions. active_runs lists the active runs you can send a message to with send_message."}, t.listAgents)
	mcp.AddTool(srv, &mcp.Tool{Name: "follow_up_agent", Description: "Send a follow-up message to one of your child runs so it continues its existing conversation (revisions, answers)."}, withIdempotency(t, "follow_up_agent", t.followUp))
	mcp.AddTool(srv, &mcp.Tool{Name: "send_message", Description: "Send a message to an active run of your work item: your parent, a child, a sibling or a side run. The target gets it as an agent message from your run. Find targets in active_runs of list_agents."}, withIdempotency(t, "send_message", t.sendMessage))
	mcp.AddTool(srv, &mcp.Tool{Name: "ask_human", Description: "Ask the requester a question. End your turn afterwards; the answer arrives as a follow-up message."}, withIdempotency(t, "ask_human", t.askHuman))
	mcp.AddTool(srv, &mcp.Tool{Name: "request_approval", Description: "Request an explicit human approval (e.g. of a spec) before continuing. Moves the work item to awaiting_approval. End your turn afterwards; the decision arrives as a follow-up message."}, withIdempotency(t, "request_approval", t.requestApproval))
	mcp.AddTool(srv, &mcp.Tool{Name: "post_update", Description: "Post a progress update (or, with kind=handoff, the finished result) to where the work item started."}, withIdempotency(t, "post_update", t.postUpdate))
	mcp.AddTool(srv, &mcp.Tool{Name: "set_stage", Description: "Set the work item's stage. Stages may be skipped or revisited."}, withIdempotency(t, "set_stage", t.setStage))
	mcp.AddTool(srv, &mcp.Tool{Name: "link_pull_request", Description: "Attach a pull request to the work item so its reviews and merge events are routed back."}, withIdempotency(t, "link_pull_request", t.linkPR))
	mcp.AddTool(srv, &mcp.Tool{Name: "link_issue", Description: "Attach a tracker issue to the work item. Its events are routed back, its pull requests are attached to it, and it moves to done when the work item completes."}, withIdempotency(t, "link_issue", t.linkIssue))
	mcp.AddTool(srv, &mcp.Tool{Name: "get_work_item", Description: "Read the work item: stage, source, pull requests, open human requests, child runs and recent timeline."}, t.getWorkItem)
	if s.propose != nil {
		mcp.AddTool(srv, &mcp.Tool{Name: "propose_definition_change", Description: "Propose a change to the managed factory definition for human review. The files are checked with the compiler and the preflight; errors come back so you can fix them. A valid proposal is stored and a reviewer sees its diff in the inbox. An approval applies it; a rejection comes back to you as a message. For a definition in git, open a pull request instead."}, withIdempotency(t, "propose_definition_change", t.proposeDefinitionChange))
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "complete_work_item", Description: "Mark the work item complete. By default only after a merge notification or a human's confirmation that the change landed."}, withIdempotency(t, "complete_work_item", t.complete))
	return srv
}

type tools struct {
	s     *Server
	runID string
}

// redactAll applies rd to each string of list. It returns nil for an empty list.
func redactAll(rd func(string) string, list []string) []string {
	if len(list) == 0 {
		return list
	}
	out := make([]string, len(list))
	for i, v := range list {
		out[i] = rd(v)
	}
	return out
}

func textResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func errResult(err error) *mcp.CallToolResult {
	r := textResult("Error: %v", err)
	r.IsError = true
	return r
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult(err)
	}
	return textResult("%s", b)
}

func (t *tools) run(ctx context.Context) (*core.Run, error) {
	r, err := t.s.store.GetRun(ctx, t.runID)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (t *tools) activeRun(ctx context.Context) (*core.Run, error) {
	r, err := t.run(ctx)
	if err != nil {
		return nil, err
	}
	if r.State == core.RunCancelled {
		return nil, errors.New("this run was cancelled; stop working")
	}
	return r, nil
}

func (t *tools) workItemRun(ctx context.Context) (*core.Run, error) {
	r, err := t.activeRun(ctx)
	if err != nil {
		return nil, err
	}
	if r.WorkItemID == "" {
		return nil, errors.New("this run is not attached to a work item")
	}
	return r, nil
}

func (t *tools) actor() string { return "run:" + t.runID }

func (t *tools) runAgents(ctx context.Context, _ *mcp.CallToolRequest, in runAgentsInput) (*mcp.CallToolResult, any, error) {
	if _, err := t.activeRun(ctx); err != nil {
		return errResult(err), nil, nil
	}
	req := core.RunAgentsRequest{
		ParentRunID: t.runID,
		Summary:     in.Summary,
		BasePrompt:  in.BasePrompt,
		Remote:      in.Remote,
		Metadata:    in.Metadata,
	}
	for _, c := range in.AgentRunConfigs {
		cc := core.ChildRunConfig{Name: c.Name, AgentIdentityUID: c.AgentIdentityUID, Prompt: c.Prompt, Metadata: c.Metadata, Skills: c.Skills}
		if c.Model != "" || c.Machine != "" || c.EnvironmentID != "" || c.WorkerHost != "" {
			ov := &core.AgentOverrides{Model: c.Model, Machine: c.Machine}
			if c.EnvironmentID != "" {
				v := c.EnvironmentID
				ov.EnvironmentID = &v
			}
			if c.WorkerHost != "" {
				v := c.WorkerHost
				ov.WorkerHost = &v
			}
			cc.Overrides = ov
		}
		req.AgentRunConfigs = append(req.AgentRunConfigs, cc)
	}
	runs, err := t.s.engine.RunAgents(ctx, req)
	if err != nil {
		return errResult(err), nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Started %d child run(s):\n", len(runs))
	for _, r := range runs {
		fmt.Fprintf(&b, "- %s: run_id=%s agent=%s state=%s\n", r.Name, r.ID, r.AgentName, r.State)
	}
	b.WriteString("Each child reports back as a follow-up message when it finishes. End your turn now if you have nothing else to do.")
	return textResult("%s", b.String()), nil, nil
}

type agentInfo struct {
	UID         string `json:"agent_identity_uid"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	Harness     string `json:"harness,omitempty"`
	Model       string `json:"model,omitempty"`
}

func (t *tools) listAgents(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
	r, err := t.run(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	agents, err := t.s.store.ListAgents(ctx, r.FactoryID)
	if err != nil {
		return errResult(err), nil, nil
	}
	caller := r.AgentName
	if caller == "" {
		caller = r.Config.Name
	}
	out := []agentInfo{}
	for _, a := range agents {
		if !engine.CanSpawn(a, caller, r.Config.Type) {
			continue
		}
		out = append(out, agentInfo{UID: a.ID, Name: a.Name, Type: string(a.Type), Description: a.Description, Harness: string(a.Harness.Type), Model: a.Harness.Model})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return jsonResult(map[string]any{"agents": out, "active_runs": t.activeTargets(ctx, r)}), nil, nil
}

// isDescendant reports whether run id descends from ancestor.
func (t *tools) isDescendant(ctx context.Context, id, ancestor string) bool {
	for range 32 {
		r, err := t.s.store.GetRun(ctx, id)
		if err != nil || r.ParentRunID == "" {
			return false
		}
		if r.ParentRunID == ancestor {
			return true
		}
		id = r.ParentRunID
	}
	return false
}

func (t *tools) followUp(ctx context.Context, _ *mcp.CallToolRequest, in followUpInput) (*mcp.CallToolResult, any, error) {
	if _, err := t.activeRun(ctx); err != nil {
		return errResult(err), nil, nil
	}
	if !t.isDescendant(ctx, in.RunID, t.runID) {
		return errResult(fmt.Errorf("run %q is not one of your child runs", in.RunID)), nil, nil
	}
	if res := t.checkLimit(ctx, t.runID, in.RunID); res != nil {
		return res, nil, nil
	}
	if err := t.s.engine.FollowUp(ctx, in.RunID, in.Message, t.actor()); err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Follow-up delivered to %s. It will report back when it finishes.", in.RunID), nil, nil
}

func (t *tools) askHuman(ctx context.Context, _ *mcp.CallToolRequest, in askHumanInput) (*mcp.CallToolResult, any, error) {
	if _, err := t.activeRun(ctx); err != nil {
		return errResult(err), nil, nil
	}
	rd := t.s.redactFor(ctx, t.runID)
	in.Question, in.Context = rd(in.Question), rd(in.Context)
	in.Options = redactAll(rd, in.Options)
	in.RecommendedOption = rd(in.RecommendedOption)
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return errResult(errors.New("question is required")), nil, nil
	}
	body := q
	if c := strings.TrimSpace(in.Context); c != "" {
		body = q + "\n\n" + c
	}
	title := q
	if len(title) > 100 {
		title = title[:97] + "..."
	}
	spec := core.Question{Text: title, Options: in.Options, RecommendedOption: strings.TrimSpace(in.RecommendedOption)}
	switch {
	case len(in.Options) == 0:
		spec.Type = core.QuestionText
	case in.MultiSelect:
		spec.Type = core.QuestionMultiSelect
	default:
		spec.Type = core.QuestionSingleSelect
	}
	it, err := t.s.engine.RequestHuman(core.WithQuestion(ctx, spec), t.runID, core.InboxQuestion, title, body, in.Options)
	if err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Question posted (inbox item %s). End your turn now; the answer will arrive as a follow-up message.", it.ID), nil, nil
}

func (t *tools) requestApproval(ctx context.Context, _ *mcp.CallToolRequest, in requestApprovalInput) (*mcp.CallToolResult, any, error) {
	if _, err := t.activeRun(ctx); err != nil {
		return errResult(err), nil, nil
	}
	rd := t.s.redactFor(ctx, t.runID)
	in.Title, in.Summary = rd(in.Title), rd(in.Summary)
	in.Links, in.Options = redactAll(rd, in.Links), redactAll(rd, in.Options)
	body := strings.TrimSpace(in.Summary)
	if len(in.Links) > 0 {
		body += "\n\nReview:\n"
		for _, l := range in.Links {
			body += "- " + l + "\n"
		}
	}
	it, err := t.s.engine.RequestHuman(ctx, t.runID, core.InboxApproval, in.Title, strings.TrimSpace(body), in.Options)
	if err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Approval requested (inbox item %s). The work item is awaiting approval. End your turn now; the decision will arrive as a follow-up message.", it.ID), nil, nil
}

func (t *tools) postUpdate(ctx context.Context, _ *mcp.CallToolRequest, in postUpdateInput) (*mcp.CallToolResult, any, error) {
	if _, err := t.activeRun(ctx); err != nil {
		return errResult(err), nil, nil
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	switch kind {
	case "", "update", "handoff", "error":
	default:
		return errResult(fmt.Errorf("unknown kind %q (want update, handoff or error)", in.Kind)), nil, nil
	}
	in.Text = t.s.redactFor(ctx, t.runID)(in.Text)
	if err := t.s.engine.PostUpdate(ctx, t.runID, in.Text, kind); err != nil {
		return errResult(err), nil, nil
	}
	if kind == "handoff" {
		return textResult("Handoff posted; the work item is in handoff. Wait for a merge notification or confirmation before completing it."), nil, nil
	}
	return textResult("Update posted."), nil, nil
}

func (t *tools) proposeDefinitionChange(ctx context.Context, _ *mcp.CallToolRequest, in proposeInput) (*mcp.CallToolResult, any, error) {
	if _, err := t.activeRun(ctx); err != nil {
		return errResult(err), nil, nil
	}
	v, err := t.s.propose(ctx, t.runID, in.Files, in.Delete, in.Message)
	if err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Proposed version %s is stored and waits for review (base version %s). End your turn; the decision arrives as a message.", v.ID, v.BaseVersionID), nil, nil
}

func (t *tools) setStage(ctx context.Context, _ *mcp.CallToolRequest, in setStageInput) (*mcp.CallToolResult, any, error) {
	r, err := t.workItemRun(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	// A side run does not change the stage.
	if sr, ok := t.s.engine.(interface {
		IsSideRun(context.Context, *core.Run) bool
	}); ok && sr.IsSideRun(ctx, r) {
		return errResult(errors.New("a side run cannot set the stage of the work item; the root run owns the stage")), nil, nil
	}
	stage := core.Stage(strings.ToLower(strings.TrimSpace(in.Stage)))
	if !engine.ValidStage(stage) {
		return errResult(fmt.Errorf("unknown stage %q", in.Stage)), nil, nil
	}
	if err := t.s.engine.SetStage(ctx, r.WorkItemID, stage, t.actor(), in.Note); err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Work item stage set to %s.", stage), nil, nil
}

// ParsePullRequestURL extracts forge, repo and number from a GitHub, GitLab or Azure DevOps PR URL.
func ParsePullRequestURL(raw string) (forge, repo string, number int, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", "", 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(parts) - 2; i >= 1; i-- {
		n, err := strconv.Atoi(parts[i+1])
		if err != nil {
			continue
		}
		switch parts[i] {
		case "pull", "pulls":
			return "GITHUB", strings.Join(parts[:i], "/"), n, i >= 2
		case "merge_requests":
			end := i
			if end > 0 && parts[end-1] == "-" {
				end--
			}
			return "GITLAB", strings.Join(parts[:end], "/"), n, end >= 2
		case "pullrequest":
			// {org}/{project}/_git/{repo}/pullrequest/{n}
			if i >= 2 && parts[i-2] == "_git" {
				return "AZURE_DEVOPS", strings.Join(append(append([]string{}, parts[:i-2]...), parts[i-1]), "/"), n, true
			}
			return "AZURE_DEVOPS", strings.Join(parts[:i], "/"), n, i >= 1
		}
	}
	return "", "", 0, false
}

func (t *tools) linkPR(ctx context.Context, _ *mcp.CallToolRequest, in linkPRInput) (*mcp.CallToolResult, any, error) {
	r, err := t.workItemRun(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	pr := core.PullRequestRef{Forge: strings.ToUpper(in.Forge), Repo: in.Repo, Number: in.Number, URL: in.URL, Branch: in.Branch, State: strings.ToLower(in.State)}
	if forge, repo, n, ok := ParsePullRequestURL(in.URL); ok {
		if pr.Forge == "" {
			pr.Forge = forge
		}
		if pr.Repo == "" {
			pr.Repo = repo
		}
		if pr.Number == 0 {
			pr.Number = n
		}
	}
	switch pr.State {
	case "", "open", "draft", "merged", "closed":
	default:
		return errResult(fmt.Errorf("unknown state %q", in.State)), nil, nil
	}
	if pr.URL == "" && (pr.Repo == "" || pr.Number == 0) {
		return errResult(errors.New("url (or repo and number) is required")), nil, nil
	}
	if err := t.s.engine.LinkPullRequest(ctx, r.WorkItemID, pr); err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Linked pull request %s to the work item.", nonEmpty(pr.URL, fmt.Sprintf("%s#%d", pr.Repo, pr.Number))), nil, nil
}

func nonEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (t *tools) complete(ctx context.Context, _ *mcp.CallToolRequest, in completeInput) (*mcp.CallToolResult, any, error) {
	r, err := t.workItemRun(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	if err := t.s.engine.CompleteWorkItem(ctx, r.WorkItemID, t.actor(), in.Note); err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Work item marked complete."), nil, nil
}

type childView struct {
	RunID  string `json:"run_id"`
	Name   string `json:"name"`
	Agent  string `json:"agent"`
	State  string `json:"state"`
	Parent string `json:"parent_run_id,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func (t *tools) getWorkItem(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, any, error) {
	r, err := t.run(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	if r.WorkItemID == "" {
		return errResult(errors.New("this run is not attached to a work item")), nil, nil
	}
	wi, err := t.s.store.GetWorkItem(ctx, r.WorkItemID)
	if err != nil {
		return errResult(err), nil, nil
	}
	out := map[string]any{
		"id":            wi.ID,
		"title":         wi.Title,
		"stage":         wi.Stage,
		"source":        wi.Source,
		"pull_requests": wi.PullRequests,
		"root_run_id":   wi.RootRunID,
		"created_at":    wi.CreatedAt,
	}
	if runs, err := t.s.store.ListRuns(ctx, core.RunFilter{WorkItemID: wi.ID}); err == nil {
		views := make([]childView, 0, len(runs))
		for _, cr := range runs {
			if cr.ID == r.ID {
				continue
			}
			views = append(views, childView{RunID: cr.ID, Name: cr.Name, Agent: cr.AgentName, State: string(cr.State), Parent: cr.ParentRunID, Result: truncate(cr.Result, 1500), Error: truncate(cr.Error, 500)})
		}
		out["runs"] = views
	}
	if items, err := t.s.store.ListInboxItems(ctx, wi.FactoryID, false); err == nil {
		var open []map[string]any
		for _, it := range items {
			if it.WorkItemID == wi.ID && !it.Resolved {
				open = append(open, map[string]any{"id": it.ID, "kind": it.Kind, "title": it.Title})
			}
		}
		out["open_human_requests"] = open
	}
	if evs, err := t.s.store.ListWorkItemEvents(ctx, wi.ID); err == nil {
		if len(evs) > 20 {
			evs = evs[len(evs)-20:]
		}
		tl := make([]map[string]any, 0, len(evs))
		for _, ev := range evs {
			item := map[string]any{"kind": ev.Kind, "at": ev.CreatedAt}
			if ev.Actor != "" {
				item["actor"] = ev.Actor
			}
			if len(ev.Data) > 0 && len(ev.Data) < 2000 {
				item["data"] = ev.Data
			}
			tl = append(tl, item)
		}
		out["timeline"] = tl
	}
	return jsonResult(out), nil, nil
}
