// Package apitest provides fakes of the core interfaces used by the api and
// mcpserver tests: Engine, SecretStore, EventBus and a minimal
// DefinitionCompiler. The tests use a real store from internal/store/testdb.
package apitest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ---------- event bus ----------

// Bus is an in-memory core.EventBus.
type Bus struct {
	mu   sync.Mutex
	subs map[string][]chan any
}

// NewBus returns an empty bus.
func NewBus() *Bus { return &Bus{subs: map[string][]chan any{}} }

// Publish implements core.EventBus (non-blocking; drops on full buffers).
func (b *Bus) Publish(_ context.Context, topic string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs[topic] {
		select {
		case ch <- payload:
		default:
		}
	}
}

// Subscribe implements core.EventBus.
func (b *Bus) Subscribe(topic string) (<-chan any, func()) {
	ch := make(chan any, 256)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			list := b.subs[topic]
			for i, c := range list {
				if c == ch {
					b.subs[topic] = append(list[:i], list[i+1:]...)
					break
				}
			}
		})
	}
}

// ---------- secrets ----------

// Secrets is a core.SecretStore that keeps plaintext in a core.Store.
type Secrets struct{ Store core.Store }

func (s *Secrets) Put(ctx context.Context, teamID, factoryID, name string, typ core.SecretType, value []byte) (*core.Secret, error) {
	t := now()
	sec := &core.Secret{ID: core.NewID("sec"), TeamID: teamID, FactoryID: factoryID, Name: name, Type: typ, CreatedAt: t, UpdatedAt: t}
	if err := s.Store.PutSecret(ctx, sec, value); err != nil {
		return nil, err
	}
	return sec, nil
}
func (s *Secrets) Get(ctx context.Context, teamID, factoryID, name string) ([]byte, error) {
	_, v, err := s.Store.GetSecret(ctx, teamID, factoryID, name)
	return v, err
}
func (s *Secrets) List(ctx context.Context, teamID, factoryID string) ([]core.Secret, error) {
	return s.Store.ListSecrets(ctx, teamID, factoryID)
}
func (s *Secrets) Delete(ctx context.Context, id string) error { return s.Store.DeleteSecret(ctx, id) }
func (s *Secrets) Expand(ctx context.Context, teamID, factoryID, in string) (string, error) {
	return in, nil
}

var _ core.SecretStore = (*Secrets)(nil)

// ---------- compiler ----------

// Compile is a minimal DefinitionCompiler: it checks factory.yaml (schemaVersion
// v1, name), requires exactly one foreman, and materializes agents,
// webhooks, machines and benchmark suites.
func Compile(files map[string]string) (*core.CompiledDefinition, []core.ValidationError) {
	var errs []core.ValidationError
	root := map[string]any{}
	doc, ok := files["factory.yaml"]
	if !ok {
		return nil, []core.ValidationError{{File: "factory.yaml", Message: "factory.yaml is required"}}
	}
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		return nil, []core.ValidationError{{File: "factory.yaml", Message: err.Error()}}
	}
	if root["schemaVersion"] != "v1" {
		errs = append(errs, core.ValidationError{File: "factory.yaml", Field: "schemaVersion", Message: "must be v1"})
	}
	name, _ := root["name"].(string)
	if name == "" {
		errs = append(errs, core.ValidationError{File: "factory.yaml", Field: "name", Message: "is required"})
	}
	def := &core.CompiledDefinition{SchemaVersion: "v1", Name: name, Machines: map[string]core.MachineSpec{}}
	def.Alias, _ = root["alias"].(string)
	def.Description, _ = root["description"].(string)
	def.RestrictFollowupsToStarter, _ = root["restrictFollowupsToStarter"].(bool)
	if list, ok := root["approvers"].([]any); ok {
		for _, a := range list {
			if s, ok := a.(string); ok {
				def.Approvers = append(def.Approvers, s)
			}
		}
	}
	if repos, ok := root["repositories"].([]any); ok {
		for _, r := range repos {
			if m, ok := r.(map[string]any); ok {
				ref := core.RepoRef{}
				ref.CodeForge, _ = m["codeForge"].(string)
				ref.Owner, _ = m["owner"].(string)
				ref.Name, _ = m["name"].(string)
				def.Repositories = append(def.Repositories, ref)
			}
		}
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	foremen := 0
	for _, p := range paths {
		parts := strings.Split(p, "/")
		switch {
		case len(parts) == 3 && parts[0] == "agents" && parts[2] == "agent.md":
			fm, body := frontmatter(files[p])
			t := core.AgentCustom
			if v, ok := fm["agentType"].(string); ok && v != "" {
				t = core.AgentType(strings.ToUpper(v))
				if t == "MAIN" {
					t = core.AgentForeman
				}
			}
			if t == core.AgentForeman {
				foremen++
			}
			desc, _ := fm["description"].(string)
			def.Agents = append(def.Agents, core.AgentConfig{Name: parts[1], Type: t, Description: desc, Instructions: body,
				Harness: core.HarnessConfig{Type: core.HarnessClaude, Model: "claude-opus-5-5"}})
		case len(parts) == 2 && parts[0] == "webhooks" && strings.HasSuffix(parts[1], ".yaml"):
			m := map[string]any{}
			if err := yaml.Unmarshal([]byte(files[p]), &m); err != nil {
				errs = append(errs, core.ValidationError{File: p, Message: err.Error()})
				continue
			}
			wh := core.Webhook{Name: strings.TrimSuffix(parts[1], ".yaml"), AuthMode: "token", Enabled: true}
			if v, ok := m["authMode"].(string); ok {
				wh.AuthMode = v
			}
			wh.SignatureScheme, _ = m["signatureScheme"].(string)
			wh.SecretName, _ = m["secretName"].(string)
			wh.DeliveryIDHeader, _ = m["deliveryIdHeader"].(string)
			if v, ok := m["enabled"].(bool); ok {
				wh.Enabled = v
			}
			if wh.SecretName == "" {
				errs = append(errs, core.ValidationError{File: p, Field: "secretName", Message: "is required"})
			}
			def.Webhooks = append(def.Webhooks, wh)
		case len(parts) == 2 && parts[0] == "machines" && strings.HasSuffix(parts[1], ".yaml"):
			var spec core.MachineSpec
			m := map[string]any{}
			_ = yaml.Unmarshal([]byte(files[p]), &m)
			spec.Name = strings.TrimSuffix(parts[1], ".yaml")
			spec.OS, _ = m["os"].(string)
			spec.Arch, _ = m["arch"].(string)
			def.Machines[spec.Name] = spec
		case len(parts) == 3 && parts[0] == "benchmarks" && parts[2] == "suite.yaml":
			m := map[string]any{}
			_ = yaml.Unmarshal([]byte(files[p]), &m)
			s := core.BenchmarkSuite{Slug: parts[1]}
			s.Name, _ = m["name"].(string)
			s.Agent, _ = m["agent"].(string)
			def.Benchmarks = append(def.Benchmarks, s)
		}
	}
	if foremen != 1 {
		errs = append(errs, core.ValidationError{File: "agents", Message: fmt.Sprintf("exactly one foreman agent is required, found %d", foremen)})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return def, nil
}

func frontmatter(doc string) (map[string]any, string) {
	m := map[string]any{}
	rest, ok := strings.CutPrefix(doc, "---\n")
	if !ok {
		return m, doc
	}
	fm, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return m, doc
	}
	_ = yaml.Unmarshal([]byte(fm), &m)
	return m, strings.TrimSpace(strings.TrimPrefix(body, "\n"))
}

// ---------- engine ----------

// Call records one engine invocation.
type Call struct {
	Method string
	Args   []any
}

// Engine is a fake core.Engine backed by a core.Store.
type Engine struct {
	Store   core.Store
	Bus     core.EventBus
	Compile core.DefinitionCompiler

	mu    sync.Mutex
	Calls []Call
	// Err, when set, is returned by every mutating method.
	Err error
	// InterruptErr is returned by Interrupt.
	InterruptErr error
	// Hook, when set, runs at the end of each successful SendMessage with the run ID.
	Hook func(runID string)
	// Sessions, when set, tells SendMessage whether a run has a live session.
	Sessions core.RunnerStore
	// Receipt, when set, changes the record that SendMessage returns.
	Receipt func(core.MessageReceipt) core.MessageReceipt
}

// NewEngine returns an engine over store using the fake compiler.
func NewEngine(store core.Store, bus core.EventBus) *Engine {
	return &Engine{Store: store, Bus: bus, Compile: Compile}
}

func (e *Engine) record(method string, args ...any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Calls = append(e.Calls, Call{Method: method, Args: args})
}

// CallsTo returns the recorded calls of one method.
func (e *Engine) CallsTo(method string) []Call {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Call
	for _, c := range e.Calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (e *Engine) publish(ctx context.Context, topic string, v any) {
	if e.Bus != nil {
		e.Bus.Publish(ctx, topic, v)
	}
}

func (e *Engine) HandleEvent(ctx context.Context, ev *core.Event) ([]*core.Run, error) {
	e.record("HandleEvent", ev)
	return nil, e.Err
}

func (e *Engine) Dispatch(ctx context.Context, req core.DispatchRequest) (*core.WorkItem, *core.Run, error) {
	e.record("Dispatch", req)
	if e.Err != nil {
		return nil, nil, e.Err
	}
	f, err := e.Store.GetFactory(ctx, req.FactoryID)
	if err != nil {
		return nil, nil, err
	}
	var wi *core.WorkItem
	if key := req.Source.Key(); key != "" {
		wi, _ = e.Store.FindWorkItemBySource(ctx, f.ID, key)
	}
	if wi == nil {
		t := now()
		wi = &core.WorkItem{ID: core.NewID("wi"), FactoryID: f.ID, Title: req.Title, Stage: core.StageIntake, Source: req.Source,
			SourceKey: req.Source.Key(), CreatorID: req.CreatorID, CreatedAt: t, UpdatedAt: t}
		if err := e.Store.CreateWorkItem(ctx, wi); err != nil {
			return nil, nil, err
		}
	}
	agent := req.AgentName
	if agent == "" {
		agent = "foreman"
	}
	run := &core.Run{ID: core.NewID("run"), TeamID: f.TeamID, FactoryID: f.ID, WorkItemID: wi.ID, AgentName: agent, Name: agent,
		State: core.RunQueued, Prompt: req.Prompt, Metadata: req.Metadata, TriggeredBy: req.TriggeredBy, CreatedAt: now()}
	if u, err := e.Store.GetUser(ctx, req.CreatorID); err == nil {
		run.Creator, run.RunAs = u.AsPrincipal(), u.AsPrincipal()
	} else {
		run.RunAs = core.FactoryPrincipal(f)
	}
	if err := e.Store.CreateRun(ctx, run); err != nil {
		return nil, nil, err
	}
	if wi.RootRunID == "" {
		wi.RootRunID = run.ID
		_ = e.Store.UpdateWorkItem(ctx, wi)
	}
	_ = e.Store.AppendRunMessage(ctx, &core.RunMessage{RunID: run.ID, Role: "user", Content: req.Prompt})
	e.publish(ctx, core.TopicRunUpdated, run)
	return wi, run, nil
}

func (e *Engine) RunAgents(ctx context.Context, req core.RunAgentsRequest) ([]*core.Run, error) {
	e.record("RunAgents", req)
	return nil, e.Err
}

func (e *Engine) FollowUp(ctx context.Context, runID, message, actor string) error {
	_, err := e.SendMessage(ctx, runID, message, actor, nil)
	return err
}

// SendMessage records the call as "FollowUp" (with the attachment IDs as a
// fifth argument when there are any) and appends the message.
func (e *Engine) SendMessage(ctx context.Context, runID, message, actor string, attachments []string) (core.MessageReceipt, error) {
	if len(attachments) > 0 {
		e.record("FollowUp", runID, message, actor, attachments)
	} else {
		e.record("FollowUp", runID, message, actor)
	}
	if e.Err != nil {
		return core.MessageReceipt{}, e.Err
	}
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return core.MessageReceipt{}, err
	}
	outcome := e.outcomeFor(ctx, run)
	m := &core.RunMessage{RunID: runID, Role: "user", Content: message}
	if err := e.Store.AppendRunMessage(ctx, m); err != nil {
		return core.MessageReceipt{}, err
	}
	run.State = core.RunQueued
	_ = e.Store.UpdateRun(ctx, run)
	e.publish(ctx, core.TopicRunMessage, m)
	e.publish(ctx, core.TopicRunUpdated, run)
	if e.Hook != nil {
		e.Hook(runID)
	}
	rec := core.MessageReceipt{MessageID: m.ID, Outcome: outcome}
	if e.Receipt != nil {
		rec = e.Receipt(rec)
	}
	return rec, nil
}

// outcomeFor is the outcome of a message to run: live_session when the newest
// session (from Sessions) is assigned or active, queued for a run that waits
// or works, new_session otherwise.
func (e *Engine) outcomeFor(ctx context.Context, run *core.Run) string {
	if e.Sessions != nil {
		if ss, err := e.Sessions.LatestRunSession(ctx, run.ID); err == nil && (ss.State == core.SessionAssigned || ss.State == core.SessionActive) {
			return core.MessageLiveSession
		}
	}
	switch run.State {
	case core.RunQueued, core.RunPending, core.RunClaimed, core.RunStarting, core.RunRunning:
		return core.MessageQueued
	}
	return core.MessageNewSession
}

func (e *Engine) Cancel(ctx context.Context, runID, actor string) error {
	e.record("Cancel", runID, actor)
	if e.Err != nil {
		return e.Err
	}
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	run.State = core.RunCancelled
	t := now()
	run.FinishedAt = &t
	_ = e.Store.UpdateRun(ctx, run)
	e.publish(ctx, core.TopicRunUpdated, run)
	return nil
}

func (e *Engine) CancelOne(ctx context.Context, runID, actor string) error {
	e.record("CancelOne", runID, actor)
	if e.Err != nil {
		return e.Err
	}
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if !run.State.Terminal() {
		run.State = core.RunCancelled
		t := now()
		run.FinishedAt = &t
		_ = e.Store.UpdateRun(ctx, run)
		e.publish(ctx, core.TopicRunUpdated, run)
	}
	return nil
}

func (e *Engine) Interrupt(ctx context.Context, runID string) error {
	e.record("Interrupt", runID)
	return e.InterruptErr
}

func (e *Engine) SetStage(ctx context.Context, id string, stage core.Stage, actor, note string) error {
	e.record("SetStage", id, stage, actor, note)
	if e.Err != nil {
		return e.Err
	}
	wi, err := e.Store.GetWorkItem(ctx, id)
	if err != nil {
		return err
	}
	wi.Stage = stage
	wi.UpdatedAt = now()
	_ = e.Store.AppendWorkItemEvent(ctx, &core.WorkItemEvent{WorkItemID: id, Kind: "stage_changed", Actor: actor})
	return e.Store.UpdateWorkItem(ctx, wi)
}

func (e *Engine) RequestHuman(ctx context.Context, runID string, kind core.InboxKind, title, body string, options []string) (*core.InboxItem, error) {
	e.record("RequestHuman", runID, kind, title)
	return nil, e.Err
}

func (e *Engine) ResolveInbox(ctx context.Context, id, resolution, actor string) error {
	e.record("ResolveInbox", id, resolution, actor)
	if e.Err != nil {
		return e.Err
	}
	it, err := e.Store.GetInboxItem(ctx, id)
	if err != nil {
		return err
	}
	t := now()
	it.Resolved, it.Resolution, it.ResolvedBy, it.ResolvedAt = true, resolution, actor, &t
	if res, ok := core.ResolverFrom(ctx); ok {
		it.ResolvedVia = res.Via
	}
	return e.Store.UpdateInboxItem(ctx, it)
}

// UnresolveInbox reopens a resolved item. It has no time window and no
// consumed check; the engine tests cover those.
func (e *Engine) UnresolveInbox(ctx context.Context, id, actor string) (*core.InboxItem, error) {
	e.record("UnresolveInbox", id, actor)
	if e.Err != nil {
		return nil, e.Err
	}
	it, err := e.Store.GetInboxItem(ctx, id)
	if err != nil {
		return nil, err
	}
	if !it.Resolved {
		return nil, fmt.Errorf("%w: inbox item is not resolved", core.ErrConflict)
	}
	it.Resolved, it.Resolution, it.ResolvedBy, it.ResolvedVia, it.ResolvedAt = false, "", "", "", nil
	return it, e.Store.UpdateInboxItem(ctx, it)
}

func (e *Engine) PostUpdate(ctx context.Context, runID, text, kind string) error {
	e.record("PostUpdate", runID, text, kind)
	return e.Err
}

func (e *Engine) LinkPullRequest(ctx context.Context, id string, pr core.PullRequestRef) error {
	e.record("LinkPullRequest", id, pr)
	if e.Err != nil {
		return e.Err
	}
	wi, err := e.Store.GetWorkItem(ctx, id)
	if err != nil {
		return err
	}
	wi.PullRequests = append(wi.PullRequests, pr)
	return e.Store.UpdateWorkItem(ctx, wi)
}

func (e *Engine) CompleteWorkItem(ctx context.Context, id, actor, note string) error {
	e.record("CompleteWorkItem", id, actor, note)
	if e.Err != nil {
		return e.Err
	}
	wi, err := e.Store.GetWorkItem(ctx, id)
	if err != nil {
		return err
	}
	t := now()
	wi.Stage, wi.CompletedAt = core.StageComplete, &t
	return e.Store.UpdateWorkItem(ctx, wi)
}

func (e *Engine) ApplyDefinition(ctx context.Context, factoryID string, files map[string]string, author, message, sha string) (*core.DefinitionVersion, error) {
	e.record("ApplyDefinition", factoryID, files, author, message)
	if e.Err != nil {
		return nil, e.Err
	}
	f, err := e.Store.GetFactory(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	v := &core.DefinitionVersion{ID: core.NewID("defv"), FactoryID: factoryID, Files: files, Author: author, Message: message, CreatedAt: now()}
	def, verrs := e.Compile(files)
	v.Valid, v.Errors = len(verrs) == 0, verrs
	if v.Valid {
		t := now()
		v.AppliedAt = &t
	}
	if err := e.Store.CreateDefinitionVersion(ctx, v); err != nil {
		return nil, err
	}
	if !v.Valid {
		return v, nil
	}
	old, _ := e.Store.ListWebhooks(ctx, factoryID)
	ids := map[string]string{}
	created := map[string]time.Time{}
	for _, w := range old {
		ids[w.Name], created[w.Name] = w.ID, w.CreatedAt
	}
	var agents []core.AgentConfig
	for _, a := range def.Agents {
		a.ID = "agt_" + factoryID + "_" + a.Name
		a.FactoryID = factoryID
		agents = append(agents, a)
	}
	var whs []core.Webhook
	for _, w := range def.Webhooks {
		w.FactoryID = factoryID
		if id, ok := ids[w.Name]; ok {
			w.ID, w.CreatedAt = id, created[w.Name]
		} else {
			w.ID, w.CreatedAt = core.NewID("wh"), now()
		}
		whs = append(whs, w)
	}
	if err := e.Store.ReplaceFactoryResources(ctx, factoryID, agents, def.Automations, def.Scorers, whs); err != nil {
		return nil, err
	}
	f.ActiveVersionID = v.ID
	if def.Name != "" {
		f.Name = def.Name
	}
	if def.Alias != "" {
		f.Alias = def.Alias
	}
	f.UpdatedAt = now()
	if err := e.Store.UpdateFactory(ctx, f); err != nil {
		return nil, err
	}
	e.publish(ctx, core.TopicDefinition, f)
	return v, nil
}

var _ core.Engine = (*Engine)(nil)

// ---------- seeding ----------

// Identity is a seeded user with a plaintext API key.
type Identity struct {
	User *core.User
	Key  string
}

// SeedUser creates a user in team with role and an API key built by mint
// (normally api.NewAPIKey; injected to avoid an import cycle).
func SeedUser(ctx context.Context, s core.Store, teamID string, role core.Role,
	mint func(teamID, userID, name string) (*core.APIKey, string, error)) (Identity, error) {
	u := &core.User{ID: core.NewID("usr"), TeamID: teamID, Email: string(role) + "-" + core.NewID("") + "@example.test",
		Name: string(role), Role: role, CreatedAt: now()}
	if err := s.CreateUser(ctx, u); err != nil {
		return Identity{}, err
	}
	k, plain, err := mint(teamID, u.ID, "test")
	if err != nil {
		return Identity{}, err
	}
	if err := s.CreateAPIKey(ctx, k); err != nil {
		return Identity{}, err
	}
	return Identity{User: u, Key: plain}, nil
}

// MinimalFiles returns a valid definition tree for the fake compiler.
func MinimalFiles(name string) map[string]string {
	return map[string]string{
		"factory.yaml":            "schemaVersion: v1\nname: " + name + "\nrepositories: []\nagentDefaults:\n  model: auto\n",
		"agents/foreman/agent.md": "---\nagentType: FOREMAN\n---\nCoordinate.\n",
	}
}

func now() time.Time { return time.Now().UTC() }
