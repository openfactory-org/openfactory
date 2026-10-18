package engine

// Fakes and the test store shared by the engine tests.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func clone[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

// testStore is the real SQLite store of a test (testdb.New) with helpers
// that put test data in through the store methods. It holds no data itself.
type testStore struct {
	*sqlstore.Store
	t testing.TB
}

// actorHandles are the handles that the tests give to event actors
// (IssueEvent links each one to the user "usr_<handle>"). newTestStore makes
// each of them an editor of team1.
var actorHandles = []string{"", "alice", "bob", "carol", "ci-bot", "contributor", "coverage[bot]", "dev", "drive-by",
	"factory[bot]", "lead", "maintainer", "outsider", "rev", "1", "dana", "human"}

// newTestStore returns a new store with the team "team1" and a user
// "usr_<handle>" (an editor of team1) for each of actorHandles.
func newTestStore(t testing.TB) *testStore {
	t.Helper()
	st := &testStore{Store: testdb.New(t), t: t}
	st.must(st.CreateTeam(context.Background(), &core.Team{ID: "team1", Name: "Team", CreatedAt: time.Now()}))
	for _, h := range actorHandles {
		st.PutUser(&core.User{ID: "usr_" + h, TeamID: "team1", Email: "actor-" + h + "@example.test", Name: h, Role: core.RoleEditor})
	}
	return st
}

func (s *testStore) must(err error) {
	s.t.Helper()
	if err != nil {
		s.t.Fatal(err)
	}
}

// PutUser creates the user, or replaces the stored user with the same ID.
func (s *testStore) PutUser(u *core.User) {
	s.t.Helper()
	ctx := context.Background()
	cp := *u
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	if _, err := s.GetUser(ctx, cp.ID); errors.Is(err, core.ErrNotFound) {
		s.must(s.CreateUser(ctx, &cp))
		return
	}
	s.must(s.UpdateUser(ctx, &cp))
}

// registerMCP registers a team MCP server.
func (s *testStore) registerMCP(m core.RegisteredMCPServer) {
	s.t.Helper()
	s.must(s.CreateMCPServer(context.Background(), &m))
}

// connectIntegration connects an integration.
func (s *testStore) connectIntegration(in core.Integration) {
	s.t.Helper()
	s.must(s.UpsertIntegration(context.Background(), &in))
}

// consumeRunInput puts a message with this ID into the queue of the run and
// moves it to the state "consumed", as a run does that took the message.
func (s *testStore) consumeRunInput(runID, id string) {
	s.t.Helper()
	ctx := context.Background()
	in := &core.RunInput{ID: id, RunID: runID, Kind: "user", Content: "answer"}
	if _, err := s.AppendRunInput(ctx, in); err != nil {
		s.t.Fatal(err)
	}
	turn := "turn_" + id
	if _, err := s.MarkRunInputsDelivered(ctx, runID, []string{id}, 1, turn); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.MarkRunInputsAccepted(ctx, runID, []string{id}); err != nil {
		s.t.Fatal(err)
	}
	if n, _, err := s.FinishRunInputsOfTurn(ctx, runID, turn); err != nil || n != 1 {
		s.t.Fatalf("consume %s: %d %v", id, n, err)
	}
}

// kvKeys returns the keys of the key-value space. The store has no list
// method, so it reads the table.
func (s *testStore) kvKeys() []string {
	s.t.Helper()
	rows, err := s.DB().QueryContext(context.Background(), "SELECT key FROM kv")
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var keys []string
	for rows.Next() {
		var k string
		s.must(rows.Scan(&k))
		keys = append(keys, k)
	}
	s.must(rows.Err())
	return keys
}

// MessagesOf returns a run's transcript.
func (s *testStore) MessagesOf(runID string) []core.RunMessage {
	s.t.Helper()
	m, err := s.ListRunMessages(context.Background(), runID, 0)
	if err != nil {
		s.t.Fatal(err)
	}
	return m
}

// ---------- bus ----------

// FakeBus is a non-blocking in-memory core.EventBus.
type FakeBus struct {
	mu   sync.Mutex
	subs map[string][]chan any
	log  []BusRecord
}

type BusRecord struct {
	Topic   string
	Payload any
}

func NewFakeBus() *FakeBus { return &FakeBus{subs: map[string][]chan any{}} }

func (b *FakeBus) Publish(_ context.Context, topic string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = append(b.log, BusRecord{topic, payload})
	for _, ch := range b.subs[topic] {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (b *FakeBus) Subscribe(topic string) (<-chan any, func()) {
	ch := make(chan any, 4096)
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], ch)
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subs[topic]
		for i, c := range list {
			if c == ch {
				b.subs[topic] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
}

// Records returns published payloads for a topic.
func (b *FakeBus) Records(topic string) []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []any
	for _, r := range b.log {
		if r.Topic == topic {
			out = append(out, r.Payload)
		}
	}
	return out
}

// Notices returns outbound notices published so far.
func (b *FakeBus) Notices() []core.OutboundMessage {
	var out []core.OutboundMessage
	for _, p := range b.Records(core.TopicOutboundNotice) {
		out = append(out, *p.(*core.OutboundMessage))
	}
	return out
}

// ---------- secrets & forges ----------

type FakeSecrets struct {
	mu     sync.Mutex
	Values map[string][]byte
	Types  map[string]core.SecretType
}

func NewFakeSecrets() *FakeSecrets {
	return &FakeSecrets{Values: map[string][]byte{}, Types: map[string]core.SecretType{}}
}

func (f *FakeSecrets) Put(_ context.Context, teamID, factoryID, name string, typ core.SecretType, value []byte) (*core.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Values[name] = value
	f.Types[name] = typ
	return &core.Secret{ID: "sec_" + name, TeamID: teamID, FactoryID: factoryID, Name: name, Type: typ}, nil
}
func (f *FakeSecrets) Get(_ context.Context, _, _, name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.Values[name]
	if !ok {
		return nil, core.ErrNotFound
	}
	return v, nil
}
func (f *FakeSecrets) List(_ context.Context, teamID, _ string) ([]core.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Secret
	for n, t := range f.Types {
		out = append(out, core.Secret{Name: n, Type: t, TeamID: teamID})
	}
	return out, nil
}
func (f *FakeSecrets) Delete(context.Context, string) error { return nil }

var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+)\s*\}\}`)

func (f *FakeSecrets) Expand(ctx context.Context, teamID, factoryID, s string) (string, error) {
	var err error
	out := placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		name := placeholderRe.FindStringSubmatch(m)[1]
		v, gerr := f.Get(ctx, teamID, factoryID, name)
		if gerr != nil {
			err = fmt.Errorf("secret %s: %w", name, gerr)
			return m
		}
		return string(v)
	})
	return out, err
}

type fakeForge struct {
	core.Forge
	name string
}

func (f fakeForge) Name() string { return f.name }
func (f fakeForge) CloneURL(_ context.Context, owner, repo string) (string, error) {
	return "https://x-access-token:tok@github.com/" + owner + "/" + repo + ".git", nil
}

type FakeForges struct{}

func (FakeForges) Forge(_ context.Context, l core.ForgeLookup) (core.Forge, error) {
	if l.CodeForge != "GITHUB" {
		return nil, fmt.Errorf("forge %s not connected", l.CodeForge)
	}
	return fakeForge{name: l.CodeForge}, nil
}

// ---------- definitions ----------

// DefaultTestDefinition returns a compiled definition with the default agent team and a
// GitHub issue_labeled intake automation.
func DefaultTestDefinition() *core.CompiledDefinition {
	agent := func(name string, typ core.AgentType, desc string) core.AgentConfig {
		return core.AgentConfig{
			Name: name, Type: typ, Description: desc, Instructions: "Instructions for " + name + ".",
			Harness: core.HarnessConfig{Type: core.HarnessClaude, Model: "claude-sonnet-4-5", Auth: &core.HarnessAuth{Source: "managedSecret", SecretName: "ANTHROPIC_KEY"}},
		}
	}
	return &core.CompiledDefinition{
		SchemaVersion:   "v1",
		Name:            "payments",
		Description:     "Payments factory",
		Repositories:    []core.RepoRef{{CodeForge: "GITHUB", Owner: "acme", Name: "api"}},
		Integrations:    []core.IntegrationDecl{{Type: "slack", Settings: map[string]string{"channel": "C1"}}},
		IntegrationsSet: true,
		Agents: []core.AgentConfig{
			agent("foreman", core.AgentForeman, "Coordinates the work"),
			agent("triage", core.AgentTriage, "Investigates requests"),
			agent("spec", core.AgentSpec, "Writes specs"),
			agent("implement", core.AgentImplement, "Writes code"),
			agent("review", core.AgentReview, "Reviews changes"),
		},
		Automations: []core.Automation{{
			Name: "issue-intake", Enabled: true, Prompt: "Coordinate this issue through human handoff.",
			Triggers: []core.Trigger{{Provider: "github", Event: "issue_labeled"}},
		}},
		Machines: map[string]core.MachineSpec{"big": {Name: "big", OS: "linux", Arch: "x86_64", VCPUs: 8}},
		Skills:   []core.Skill{{Name: "conventions", Description: "Repo conventions", Content: "# Conventions", Files: map[string]string{"ref.md": "ref"}}},
	}
}

// CompilerFor returns a DefinitionCompiler that yields def unless files["factory.yaml"] == "invalid".
func CompilerFor(def *core.CompiledDefinition) core.DefinitionCompiler {
	return func(files map[string]string) (*core.CompiledDefinition, []core.ValidationError) {
		if files["factory.yaml"] == "invalid" {
			return nil, []core.ValidationError{{File: "factory.yaml", Line: 1, Message: "schemaVersion is required"}}
		}
		return clone(def), nil
	}
}

// SimpleMatch matches an automation when any trigger's provider and event equal the event's.
func SimpleMatch(ev *core.Event, a core.Automation) bool {
	for _, t := range a.Triggers {
		if t.Provider == ev.Provider && t.Event == ev.Type {
			return !ev.Actor.IsBot
		}
	}
	return false
}
