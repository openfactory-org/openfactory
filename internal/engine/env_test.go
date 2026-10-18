package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestEnv bundles an engine on a test store (SQLite in memory) and fakes,
// with one applied factory.
type TestEnv struct {
	Store   *testStore
	Bus     *FakeBus
	Engine  *Engine
	Route   *fakeRoute // the runner route of the engine: it records each message
	Secrets *FakeSecrets
	Factory *core.Factory
	Def     *core.CompiledDefinition
	Now     time.Time
}

// NewTestEnv creates the environment and applies TestDefinition (optionally modified).
func NewTestEnv(t testing.TB, modify ...func(*core.CompiledDefinition)) *TestEnv {
	t.Helper()
	ctx := context.Background()
	def := DefaultTestDefinition()
	for _, m := range modify {
		m(def)
	}
	st := newTestStore(t)
	bus := NewFakeBus()
	sec := NewFakeSecrets()
	_, _ = sec.Put(ctx, "team1", "", "ANTHROPIC_KEY", core.SecretAnthropicKey, []byte("sk-ant-test\n"))
	f := &core.Factory{ID: "fac_1", TeamID: "team1", Name: "payments", Source: core.DefinitionManaged, CreatedAt: time.Now()}
	if err := st.CreateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	route := &fakeRoute{}
	eng := New(Options{Store: st, Bus: bus, Compile: CompilerFor(def), Match: SimpleMatch, Secrets: sec, Forges: FakeForges{}, Logger: DiscardLogger(), Runner: route})
	seedPreflight(ctx, eng, st, sec, def)
	if _, err := eng.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok"}, "tester", "initial", ""); err != nil {
		t.Fatalf("apply definition: %v", err)
	}
	f, _ = st.GetFactory(ctx, f.ID)
	return &TestEnv{Store: st, Bus: bus, Engine: eng, Route: route, Secrets: sec, Factory: f, Def: def}
}

// Agent returns the materialized agent with the given name.
func (e *TestEnv) Agent(t testing.TB, name string) core.AgentConfig {
	t.Helper()
	a, err := e.Store.GetAgentByName(context.Background(), e.Factory.ID, name)
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return *a
}

// IssueEvent builds a GitHub issue event for acme/api#7 from a linked team member.
func IssueEvent(typ, text, actor string) *core.Event {
	return &core.Event{
		ID: core.NewID("evt"), TeamID: "team1", Provider: "github", Type: typ,
		Actor:      core.Actor{Provider: "github", Handle: actor, ExternalID: actor, UserID: "usr_" + actor},
		Source:     core.SourceRef{Provider: "github", Kind: "issue", Repo: "acme/api", Number: 7, Title: "Invoices omit PO number", URL: "https://github.com/acme/api/issues/7"},
		Attributes: map[string]any{"repo": "acme/api", "labels": []string{"factory-intake"}},
		Text:       text,
		ReceivedAt: time.Now(),
	}
}

// WaitFor polls cond until it holds or the timeout expires. step is invoked
// between polls.
func WaitFor(t testing.TB, timeout time.Duration, step func(), cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		if step != nil {
			step()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// DiscardLogger returns a logger that drops everything.
func DiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// seedPreflight makes the team objects that def names exist, so that the
// preflight of the first apply passes: secrets get an empty placeholder
// value (tests put the real value later), worker hosts become known hosts
// and managed MCP servers become registered servers. The preflight itself
// is tested in preflight_test.go.
func seedPreflight(ctx context.Context, eng *Engine, st *testStore, sec *FakeSecrets, def *core.CompiledDefinition) {
	for _, r := range secretRefs(def) {
		if _, err := sec.Get(ctx, "team1", "", r.name); err != nil {
			_, _ = sec.Put(ctx, "team1", "", r.name, core.SecretGeneric, []byte{})
		}
	}
	var hosts []string
	addMCP := func(m map[string]core.MCPServer) {
		for _, srv := range m {
			if srv.ManagedID != "" && !strings.HasPrefix(srv.ManagedID, core.BuiltinMCPPrefix) {
				st.registerMCP(core.RegisteredMCPServer{ID: srv.ManagedID, TeamID: "team1", Name: srv.ManagedID})
			}
		}
	}
	addMCP(def.MCPServers)
	for _, a := range def.Agents {
		if a.WorkerHost != "" {
			hosts = append(hosts, a.WorkerHost)
		}
		addMCP(a.MCPServers)
	}
	for _, au := range def.Automations {
		if au.Overrides != nil {
			if au.Overrides.WorkerHost != nil {
				hosts = append(hosts, *au.Overrides.WorkerHost)
			}
			addMCP(au.Overrides.MCPServers)
		}
	}
	eng.SetWorkerHosts(hosts)
	// Each approver is a user of the team. A test that needs other
	// attributes puts the user again under the same ID.
	for _, a := range def.Approvers {
		local, _, _ := strings.Cut(strings.ToLower(a), "@")
		st.PutUser(&core.User{ID: "usr_" + local, TeamID: "team1", Email: a, Name: local, Role: core.RoleUser})
	}
}
