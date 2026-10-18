package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
)

// RunConfig holds optional overrides for the agent of a run.
type RunConfig struct {
	Harness            *core.HarnessConfig       `json:"harness,omitempty"`
	ModelID            string                    `json:"model_id,omitempty"`
	EnvironmentID      string                    `json:"environment_id,omitempty"`
	Machine            json.RawMessage           `json:"machine,omitempty"` // machine name or MachineSpec object
	WorkerHost         string                    `json:"worker_host,omitempty"`
	MCPServers         map[string]core.MCPServer `json:"mcp_servers,omitempty"`
	Secrets            []string                  `json:"secrets,omitempty"`
	IdleTimeoutMinutes *int                      `json:"idle_timeout_minutes,omitempty"`
}

// CreateRunRequest is the body of POST /runs. Each run belongs to a factory:
// the request names an agent of a factory with AgentIdentityUID, or with
// FactoryID and AgentName.
type CreateRunRequest struct {
	Prompt           string            `json:"prompt"`
	Title            string            `json:"title,omitempty"`
	AgentIdentityUID string            `json:"agent_identity_uid,omitempty"`
	FactoryID        string            `json:"factory_id,omitempty"`
	AgentName        string            `json:"agent_name,omitempty"`
	Config           *RunConfig        `json:"config,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// normalizeHarness validates a harness configuration in place.
func normalizeHarness(h *core.HarnessConfig, workerHost string) error {
	t := strings.ToLower(string(h.Type))
	switch t {
	case "", "claude", "claude-code":
		h.Type = core.HarnessClaude
	case "codex":
		h.Type = core.HarnessCodex
	default:
		return errInvalid("harness.type must be one of claude, claude-code, codex")
	}
	if h.Model == "" {
		h.Model = harness.DefaultModel(h.Type)
	}
	if a := h.Auth; a != nil {
		switch a.Source {
		case "managedSecret":
			if a.SecretName == "" {
				return errInvalid("harness.auth.secretName is required with source managedSecret")
			}
		case "workerEnvironment":
			if a.SecretName != "" {
				return errInvalid("harness.auth.secretName is not allowed with source workerEnvironment")
			}
			if workerHost == "" || workerHost == "server" {
				return errInvalid("harness.auth.source workerEnvironment requires a self-hosted worker_host")
			}
		default:
			return errInvalid("harness.auth.source must be managedSecret or workerEnvironment")
		}
	}
	return nil
}

// ValidateMCPServers checks that each server selects exactly one transport.
func ValidateMCPServers(m map[string]core.MCPServer) error {
	for name, srv := range m {
		if strings.TrimSpace(name) == "" {
			return errInvalid("mcp_servers keys must be non-empty")
		}
		n := 0
		for _, set := range []bool{srv.ManagedID != "", srv.Command != "", srv.URL != ""} {
			if set {
				n++
			}
		}
		if n != 1 {
			return errInvalid("mcp_servers.%s must set exactly one of managedId, command or url", name)
		}
		if srv.URL != "" {
			u, err := url.Parse(srv.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return errInvalid("mcp_servers.%s.url must be an absolute http(s) URL", name)
			}
			if len(srv.Args) > 0 || len(srv.Env) > 0 {
				return errInvalid("mcp_servers.%s: args and env apply only to command servers", name)
			}
		}
		if srv.Command != "" && len(srv.Headers) > 0 {
			return errInvalid("mcp_servers.%s: headers apply only to url servers", name)
		}
		if strings.HasPrefix(srv.ManagedID, core.BuiltinMCPPrefix) {
			if _, known := core.BuiltinMCPProvider(srv.ManagedID); !known {
				return errInvalid("mcp_servers.%s: unknown built-in MCP server %q", name, srv.ManagedID)
			}
		} else if srv.AllowWrite {
			return errInvalid("mcp_servers.%s: allowWrite is only valid with a built-in managedId", name)
		}
	}
	return nil
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req CreateRunRequest
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	run, err := s.CreateRun(withRequestSource(ctx, r, p), p, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, s.runViewCtx(ctx, run))
	return nil
}

// CreateRun starts a run of an agent of a factory on behalf of p.
func (s *Server) CreateRun(ctx context.Context, p *Principal, req CreateRunRequest) (*core.Run, error) {
	cfgIn := req.Config
	if cfgIn == nil {
		cfgIn = &RunConfig{}
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errInvalid("prompt is required")
	}
	if err := ValidateMetadata(req.Metadata); err != nil {
		return nil, err
	}
	workerHost := strings.TrimSpace(cfgIn.WorkerHost)
	if workerHost == "managed" || workerHost == "default" {
		workerHost = ""
	}
	var harness *core.HarnessConfig
	if cfgIn.Harness != nil {
		h := *cfgIn.Harness
		if h.Model == "" {
			h.Model = cfgIn.ModelID
		}
		if err := normalizeHarness(&h, workerHost); err != nil {
			return nil, err
		}
		harness = &h
	}
	if err := ValidateMCPServers(cfgIn.MCPServers); err != nil {
		return nil, err
	}
	for _, sn := range cfgIn.Secrets {
		if !secretNameRe.MatchString(sn) {
			return nil, errInvalid("invalid secret name %q", sn)
		}
	}
	if t := cfgIn.IdleTimeoutMinutes; t != nil && (*t < 0 || *t > 60) {
		return nil, errInvalid("idle_timeout_minutes must be between 0 and 60")
	}
	if cfgIn.EnvironmentID != "" {
		env, err := s.opts.Store.GetEnvironment(ctx, cfgIn.EnvironmentID)
		if err != nil || env.TeamID != p.TeamID {
			if err == nil || isNotFound(err) {
				return nil, errInvalid("environment %s does not exist", cfgIn.EnvironmentID)
			}
			return nil, err
		}
	}
	var machine *core.MachineSpec
	if raw := bytes.TrimSpace(cfgIn.Machine); len(raw) > 0 && string(raw) != "null" {
		var name string
		if err := json.Unmarshal(raw, &name); err == nil {
			machine = &core.MachineSpec{Name: name}
		} else {
			var spec core.MachineSpec
			if err := json.Unmarshal(raw, &spec); err != nil {
				return nil, errInvalid("config.machine must be a machine name or a machine object")
			}
			machine = &spec
		}
	}

	// Each run belongs to a factory.
	var factoryID, agentName string
	switch {
	case req.AgentIdentityUID != "":
		agent, err := s.opts.Store.GetAgent(ctx, req.AgentIdentityUID)
		if err != nil {
			if isNotFound(err) {
				return nil, errNotFound("agent " + req.AgentIdentityUID)
			}
			return nil, err
		}
		if _, err := s.factoryFor(ctx, p, agent.FactoryID); err != nil {
			return nil, errNotFound("agent " + req.AgentIdentityUID)
		}
		factoryID, agentName = agent.FactoryID, agent.Name
	case req.FactoryID != "" && strings.TrimSpace(req.AgentName) != "":
		f, err := s.factoryFor(ctx, p, req.FactoryID)
		if err != nil {
			return nil, err
		}
		factoryID, agentName = f.ID, strings.TrimSpace(req.AgentName)
	default:
		return nil, errInvalid("agent_identity_uid, or factory_id and agent_name, is required")
	}
	ov := &core.AgentOverrides{Harness: harness, Secrets: cfgIn.Secrets, MCPServers: cfgIn.MCPServers,
		IdleTimeoutMinutes: cfgIn.IdleTimeoutMinutes}
	if harness == nil {
		ov.Model = cfgIn.ModelID
	}
	if cfgIn.EnvironmentID != "" {
		ov.EnvironmentID = &cfgIn.EnvironmentID
	}
	if workerHost != "" {
		ov.WorkerHost = &workerHost
	}
	if machine != nil {
		ov.Machine = machine.Name
	}
	title := req.Title
	if title == "" {
		title = deriveTitle(req.Prompt)
	}
	_, run, err := s.opts.Engine.Dispatch(ctx, core.DispatchRequest{
		FactoryID: factoryID, Prompt: req.Prompt, AgentName: agentName,
		Source:    core.SourceRef{Provider: "api", Kind: "task", Title: title},
		CreatorID: p.UserID(), Title: title, Overrides: ov, Metadata: req.Metadata, TriggeredBy: requestSource(ctx),
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	pg, err := pageFrom(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := core.RunFilter{
		TeamID:      p.TeamID,
		WorkItemID:  q.Get("work_item_id"),
		ParentRunID: q.Get("parent_run_id"),
		AgentName:   q.Get("agent_name"),
		Name:        firstNonEmpty(q.Get("name"), q.Get("config_name")),
	}
	if f.CreatorID, err = creatorParam(q.Get("creator"), p); err != nil {
		return err
	}
	if fid := firstNonEmpty(q.Get("factory_id"), q.Get("factory")); fid != "" {
		fac, err := s.factoryFor(ctx, p, fid)
		if err != nil {
			return err
		}
		f.FactoryID = fac.ID
	}
	for _, v := range qList(r, "state") {
		sts, ok := internalRunStates(v)
		if !ok {
			return errInvalid("unknown state %q (expected QUEUED, PENDING, CLAIMED, RUNNING, BLOCKED, SUCCEEDED, FAILED, ERROR or CANCELLED)", v)
		}
		f.States = append(f.States, sts...)
	}
	since := firstNonEmpty(q.Get("since"), q.Get("created_after"))
	if f.Since, err = parseSince(since, s.now()); err != nil {
		return err
	}
	lookup := s.newRunLookup(p.TeamID)
	extra, err := runExtraFilterFrom(r)
	if err != nil {
		return err
	}
	var runs []core.Run
	var next string
	if rootID, ancID := q.Get("root_run_id"), q.Get("ancestor_run_id"); rootID != "" || ancID != "" {
		runs, next, err = s.listRunTree(ctx, p, lookup, f, pg, rootID, ancID, extra)
		if err != nil {
			return err
		}
	} else if extra.empty() {
		f.ListOpts = pg.listOpts()
		runs, err = s.opts.Store.ListRuns(ctx, f)
		if err != nil {
			return err
		}
		runs, next = trimPage(runs, func(r core.Run) (timeT, string) { return r.CreatedAt, r.ID }, pg.Limit)
	} else {
		runs, next, err = scanFiltered(pg, func(o core.ListOpts) ([]core.Run, error) {
			ff := f
			ff.ListOpts = o
			return s.opts.Store.ListRuns(ctx, ff)
		}, func(r core.Run) bool { return extra.keep(&r) },
			func(r core.Run) (timeT, string) { return r.CreatedAt, r.ID })
		if err != nil {
			return err
		}
	}
	views := make([]RunView, 0, len(runs))
	for i := range runs {
		views = append(views, s.runViewWith(ctx, &runs[i], lookup))
	}
	writeJSON(w, http.StatusOK, listResponse("runs", views, next))
	return nil
}

// listRunTree serves the filters root_run_id and ancestor_run_id. It walks
// down from the named run with parent_run_id lists (at most 1000 runs and
// 10 levels), then applies the other filters in memory. root_run_id returns
// the run itself and its descendants. ancestor_run_id returns the descendants.
func (s *Server) listRunTree(ctx context.Context, p *Principal, l *runLookup, f core.RunFilter, pg pageParams, rootID, ancID string, extra runExtraFilter) ([]core.Run, string, error) {
	if rootID != "" && ancID != "" && rootID != ancID {
		return nil, "", errInvalid("root_run_id and ancestor_run_id name different runs: use one of them")
	}
	id, includeSelf := rootID, true
	if id == "" {
		id, includeSelf = ancID, false
	}
	top, err := s.runFor(ctx, p, id)
	if err != nil {
		return nil, "", err
	}
	l.runs[top.ID] = top
	var set []core.Run
	if includeSelf {
		set = append(set, *top)
	}
	set = append(set, l.descendants(ctx, top.ID)...)
	states := map[core.RunState]bool{}
	for _, st := range f.States {
		states[st] = true
	}
	out := set[:0:0]
	for _, run := range set {
		switch {
		case f.FactoryID != "" && run.FactoryID != f.FactoryID,
			f.WorkItemID != "" && run.WorkItemID != f.WorkItemID,
			f.ParentRunID != "" && run.ParentRunID != f.ParentRunID,
			f.AgentName != "" && run.AgentName != f.AgentName,
			f.Name != "" && run.Name != f.Name,
			f.CreatorID != "" && run.CreatorID() != f.CreatorID,
			f.Since != nil && run.CreatedAt.Before(*f.Since),
			len(states) > 0 && !states[run.State],
			!extra.keep(&run):
			continue
		}
		out = append(out, run)
	}
	page, next := paginate(out, func(r core.Run) (timeT, string) { return r.CreatedAt, r.ID }, pg)
	return page, next, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	run, err := s.runFor(r.Context(), p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	v := s.runViewCtx(r.Context(), run)
	v.JudgeUsage = s.judgeUsageOf(r.Context(), run)
	writeJSON(w, http.StatusOK, v)
	return nil
}

// judgeUsageOf sums the usage of the judge runs of the scores of a run. It
// returns nil when the run has no scored judge run.
func (s *Server) judgeUsageOf(ctx context.Context, run *core.Run) *core.Usage {
	scorers, err := s.opts.Store.ListScorers(ctx, run.FactoryID)
	if err != nil {
		return nil
	}
	var sum core.Usage
	found := false
	for _, sc := range scorers {
		score, err := s.opts.Store.GetScore(ctx, sc.ID, run.ID)
		if err != nil || score.JudgeUsage == nil {
			continue
		}
		found = true
		sum.InputTokens += score.JudgeUsage.InputTokens
		sum.OutputTokens += score.JudgeUsage.OutputTokens
		sum.CostUSD += score.JudgeUsage.CostUSD
		sum.DurationMS += score.JudgeUsage.DurationMS
	}
	if !found {
		return nil
	}
	return &sum
}

func (s *Server) handleRunMessages(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	after, err := qInt64(r, "after")
	if err != nil {
		return err
	}
	limit, err := qInt(r, "limit", 200)
	if err != nil {
		return err
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	msgs, err := s.runMessagesAfter(ctx, run.ID, after)
	if err != nil {
		return err
	}
	if roles := qList(r, "role"); len(roles) > 0 {
		filtered := msgs[:0]
		for _, m := range msgs {
			for _, role := range roles {
				if m.Role == role {
					filtered = append(filtered, m)
					break
				}
			}
		}
		msgs = filtered
	}
	resp := map[string]any{"run_id": run.ID, "state": ExternalRunState(run.State)}
	if len(msgs) > limit {
		msgs = msgs[:limit]
		resp["next_after"] = msgs[len(msgs)-1].Seq
		resp["has_more"] = true
	} else {
		resp["has_more"] = false
	}
	if msgs == nil {
		msgs = []core.RunMessage{}
	}
	resp["messages"] = msgs
	writeJSON(w, http.StatusOK, resp)
	return nil
}

func qInt64(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, errInvalid("query parameter %q must be a non-negative integer", name)
	}
	return n, nil
}

// handleRunEvents streams a run's messages and state changes as Server-Sent Events.
//
// Events:
//
//	event: run      data: RunView        (initial state and every state change)
//	event: message  data: RunMessage     (id: message seq; resume with Last-Event-ID or ?after=)
//	event: done     data: RunView        (the run reached a terminal state; stream ends)
//
// Query: after=<seq>, stop_on_idle=true to end the stream when the run goes idle.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	after, err := qInt64(r, "after")
	if err != nil {
		return err
	}
	if lei := r.Header.Get("Last-Event-ID"); lei != "" {
		if n, err := strconv.ParseInt(lei, 10, 64); err == nil && n > after {
			after = n
		}
	}
	stopOnIdle, err := qBool(r, "stop_on_idle")
	if err != nil {
		return err
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Errf(CodeFeatureNotAvailable, "streaming is not supported by this connection")
	}

	var msgCh, runCh <-chan any
	if s.opts.Bus != nil {
		var unsubM, unsubR func()
		msgCh, unsubM = s.opts.Bus.Subscribe(core.TopicRunMessage)
		runCh, unsubR = s.opts.Bus.Subscribe(core.TopicRunUpdated)
		defer unsubM()
		defer unsubR()
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	st := &sseStream{w: w, f: flusher, lastSeq: after, lastState: run.State}
	st.send("run", "", s.runViewCtx(ctx, run))
	if err := s.replayMessages(ctx, st, run.ID); err != nil {
		st.send("error", "", map[string]string{"message": err.Error()})
		return nil
	}
	finished := func(rn *core.Run) bool {
		return rn.State.Terminal() || (stopOnIdle && rn.State.WaitsForInput())
	}
	if finished(run) {
		st.send("done", "", s.runViewCtx(ctx, run))
		return nil
	}

	heartbeat := time.NewTicker(s.opts.SSEHeartbeat)
	defer heartbeat.Stop()
	poll := time.NewTicker(s.pollInterval())
	defer poll.Stop()

	onRun := func(rn *core.Run) bool {
		if rn.State != st.lastState {
			st.lastState = rn.State
			st.send("run", "", s.runViewCtx(ctx, rn))
		}
		if finished(rn) {
			_ = s.replayMessages(ctx, st, run.ID) // flush trailing messages
			st.send("done", "", s.runViewCtx(ctx, rn))
			return true
		}
		return false
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			st.comment("ping")
		case v, ok := <-msgCh:
			if !ok {
				msgCh = nil
				continue
			}
			if m, ok := v.(*core.RunMessage); ok && m.RunID == run.ID {
				if m.Seq > st.lastSeq+1 {
					// A gap (dropped bus message): catch up from the store.
					_ = s.replayMessages(ctx, st, run.ID)
				} else if m.Seq > st.lastSeq {
					st.lastSeq = m.Seq
					st.send("message", strconv.FormatInt(m.Seq, 10), m)
				}
			}
		case v, ok := <-runCh:
			if !ok {
				runCh = nil
				continue
			}
			if rn, ok := v.(*core.Run); ok && rn.ID == run.ID {
				if onRun(rn) {
					return nil
				}
			}
		case <-poll.C:
			if err := s.replayMessages(ctx, st, run.ID); err != nil {
				return nil
			}
			rn, err := s.opts.Store.GetRun(ctx, run.ID)
			if err != nil {
				return nil
			}
			if onRun(rn) {
				return nil
			}
		}
	}
}

func (s *Server) pollInterval() time.Duration {
	if s.opts.SSEHeartbeat < 2*time.Second {
		return s.opts.SSEHeartbeat
	}
	return 2 * time.Second
}

func (s *Server) replayMessages(ctx context.Context, st *sseStream, runID string) error {
	msgs, err := s.runMessagesAfter(ctx, runID, st.lastSeq)
	if err != nil {
		return err
	}
	for i := range msgs {
		if msgs[i].Seq <= st.lastSeq {
			continue
		}
		st.lastSeq = msgs[i].Seq
		st.send("message", strconv.FormatInt(msgs[i].Seq, 10), &msgs[i])
	}
	return nil
}

type sseStream struct {
	w         http.ResponseWriter
	f         http.Flusher
	lastSeq   int64
	lastState core.RunState
}

func (s *sseStream) send(event, id string, v any) {
	data, _ := json.Marshal(v)
	var b strings.Builder
	if id != "" {
		b.WriteString("id: " + id + "\n")
	}
	b.WriteString("event: " + event + "\n")
	b.WriteString("data: ")
	b.Write(data)
	b.WriteString("\n\n")
	_, _ = s.w.Write([]byte(b.String()))
	s.f.Flush()
}

func (s *sseStream) comment(text string) {
	_, _ = s.w.Write([]byte(": " + text + "\n\n"))
	s.f.Flush()
}

// handleInterruptRun stops the active turn of a run and keeps its session.
func (s *Server) handleInterruptRun(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	run, err := s.runFor(ctx, p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	if err := s.opts.Engine.Interrupt(ctx, run.ID); err != nil {
		var ie *core.InterruptError
		if errors.As(err, &ie) {
			return &Error{Code: CodeConflict, Detail: ie.Error(), Extra: map[string]any{"reason": ie.Reason}}
		}
		return err
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "interrupting"})
	return nil
}
