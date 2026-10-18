package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/events"
)

func hasIssue(list []core.ValidationError, file, field, text string) bool {
	for _, e := range list {
		if e.File == file && (field == "" || e.Field == field) && strings.Contains(e.Message, text) {
			return true
		}
	}
	return false
}

func TestPreflightChecksTheTeam(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	_ = env.Store.CreateEnvironment(ctx, &core.Environment{ID: "env_ok", TeamID: "team1", Name: "ok"})
	_, _ = env.Secrets.Put(ctx, "team1", "", "BAD_TOKEN", core.SecretGeneric, []byte("a/b"))
	env.Store.registerMCP(core.RegisteredMCPServer{ID: "mcp_ok", TeamID: "team1", Name: "docs"})
	def := DefaultTestDefinition()
	def.Agents[1].Secrets = []string{"MISSING"}
	def.Agents[1].EnvironmentID = "env_gone"
	def.Agents[2].EnvironmentID = "env_ok"
	def.Agents[1].WorkerHost = "build-box"
	def.Agents[2].WorkerHost = "kubernetes"
	def.Agents[1].MCPServers = map[string]core.MCPServer{"gone": {ManagedID: "mcp_gone"}, "docs": {ManagedID: "docs"}}
	def.Webhooks = []core.Webhook{{Name: "deploys", AuthMode: "url_token", SecretName: "BAD_TOKEN", Enabled: true}}
	env.Engine.compile = CompilerFor(def)

	chk, err := env.Engine.CheckDefinition(ctx, env.Factory.ID, map[string]string{"factory.yaml": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if chk.Valid() {
		t.Fatal("definition with missing team objects is valid")
	}
	for _, want := range []struct{ file, field, text string }{
		{"agents/triage/agent.md", "secrets", `secret "MISSING"`},
		{"agents/triage/agent.md", "environment", `environment "env_gone"`},
		{"agents/triage/agent.md", "workerHost", `worker host "build-box"`},
		{"agents/triage/agent.md", "mcpServers.gone.managedId", `"mcp_gone"`},
		{"webhooks/deploys.yaml", "secretName", "cannot be used in a URL"},
	} {
		if !hasIssue(chk.Errors, want.file, want.field, want.text) {
			t.Errorf("missing error %+v in %+v", want, chk.Errors)
		}
	}
	if len(chk.Errors) != 5 {
		t.Errorf("errors = %+v", chk.Errors)
	}

	// A registered worker and a configured backend are known hosts.
	if err := env.Store.UpsertWorker(ctx, &core.Worker{ID: "w1", TeamID: "team1", Name: "build-box"}); err != nil {
		t.Fatal(err)
	}
	def.Agents[1].Secrets = nil
	def.Agents[1].EnvironmentID = ""
	def.Agents[1].MCPServers = nil
	def.Agents[3].WorkerHost = "gpu"
	env.Engine.SetWorkerHosts([]string{"gpu"})
	_, _ = env.Secrets.Put(ctx, "team1", "", "BAD_TOKEN", core.SecretGeneric, []byte("good-token_1"))
	chk, _ = env.Engine.CheckDefinition(ctx, env.Factory.ID, map[string]string{"factory.yaml": "ok"})
	if !chk.Valid() {
		t.Fatalf("errors = %+v", chk.Errors)
	}
}

func TestApplyRejectsAMissingSecret(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	before := env.Factory.ActiveVersionID
	def := DefaultTestDefinition()
	def.Agents[0].Secrets = []string{"NOT_THERE"}
	env.Engine.compile = CompilerFor(def)
	v, err := env.Engine.ApplyDefinition(ctx, env.Factory.ID, map[string]string{"factory.yaml": "ok"}, "a", "m", "")
	if !errors.Is(err, core.ErrInvalid) || v == nil || v.Valid || !hasIssue(v.Errors, "agents/foreman/agent.md", "secrets", "NOT_THERE") {
		t.Fatalf("apply = %+v, %v", v, err)
	}
	f, _ := env.Store.GetFactory(ctx, env.Factory.ID)
	if f.ActiveVersionID != before {
		t.Fatalf("active version changed to %s", f.ActiveVersionID)
	}
}

func TestWebhookIDAndImmutableFields(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	_, _ = env.Secrets.Put(ctx, "team1", "", "HOOK", core.SecretGeneric, []byte("tok"))
	def := DefaultTestDefinition()
	def.Webhooks = []core.Webhook{{Name: "deploys", AuthMode: "token", SecretName: "HOOK", Enabled: true}}
	env.Engine.compile = CompilerFor(def)
	apply := func() (*core.DefinitionVersion, error) {
		return env.Engine.ApplyDefinition(ctx, env.Factory.ID, map[string]string{"factory.yaml": "ok"}, "a", "m", "")
	}
	hook := func() core.Webhook {
		ws, _ := env.Store.ListWebhooks(ctx, env.Factory.ID)
		if len(ws) != 1 {
			t.Fatalf("webhooks = %+v", ws)
		}
		return ws[0]
	}
	if _, err := apply(); err != nil {
		t.Fatal(err)
	}
	first := hook()
	if first.ID == StableID("whk", env.Factory.ID, "deploys") || !strings.HasPrefix(first.ID, "whk_") || first.CreatedAt.IsZero() {
		t.Fatalf("webhook = %+v", first)
	}
	if _, err := apply(); err != nil {
		t.Fatal(err)
	}
	if again := hook(); again.ID != first.ID || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("webhook changed on re-apply: %+v -> %+v", first, again)
	}

	def.Webhooks[0].AuthMode = "signature"
	def.Webhooks[0].SignatureScheme = "standard_webhooks"
	env.Engine.compile = CompilerFor(def)
	v, err := apply()
	if !errors.Is(err, core.ErrInvalid) || !hasIssue(v.Errors, "webhooks/deploys.yaml", "authMode", "cannot change") ||
		!hasIssue(v.Errors, "webhooks/deploys.yaml", "signatureScheme", "cannot change") {
		t.Fatalf("apply = %+v, %v", v, err)
	}

	// Removed and added again: a new ID and so a new URL.
	def.Webhooks = nil
	env.Engine.compile = CompilerFor(def)
	if _, err := apply(); err != nil {
		t.Fatal(err)
	}
	def.Webhooks = []core.Webhook{{Name: "deploys", AuthMode: "signature", SignatureScheme: "standard_webhooks", SecretName: "HOOK", Enabled: true}}
	env.Engine.compile = CompilerFor(def)
	if _, err := apply(); err != nil {
		t.Fatal(err)
	}
	if again := hook(); again.ID == first.ID {
		t.Fatal("a webhook created again kept its old ID")
	}
}

type fakeResolver struct {
	names map[string]map[string][]string // kind -> name -> ids
	err   error
	scope []map[string][]string
}

func (r *fakeResolver) ResolveNames(_ context.Context, _, _, kind string, names []string, scope map[string][]string) (map[string][]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	r.scope = append(r.scope, scope)
	out := map[string][]string{}
	for _, n := range names {
		out[n] = r.names[kind][n]
	}
	return out, nil
}

func TestNameFiltersResolveToIDs(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	def := DefaultTestDefinition()
	def.Automations = append(def.Automations, core.Automation{
		Name: "linear-intake", Enabled: true, Prompt: "go",
		Triggers: []core.Trigger{{Provider: "linear", Event: "issue_created", Filter: json.RawMessage(`{"teams":["Eng"],"states":["Todo"],"labels":["bug"]}`)}},
	})
	env.Engine.compile = CompilerFor(def)
	files := map[string]string{"factory.yaml": "ok"}

	// Not connected: a warning, and the names stay as written.
	chk, _ := env.Engine.CheckDefinition(ctx, env.Factory.ID, files)
	if !chk.Valid() || !hasIssue(chk.Warnings, "automations/linear-intake/automation.md", "triggers[0].filter.teams", "not connected") {
		t.Fatalf("check = %+v", chk)
	}

	env.Store.connectIntegration(core.Integration{ID: "int1", TeamID: "team1", Provider: "linear"})
	chk, _ = env.Engine.CheckDefinition(ctx, env.Factory.ID, files)
	if !chk.Valid() || !hasIssue(chk.Warnings, "automations/linear-intake/automation.md", "triggers[0].filter.states", "cannot list") {
		t.Fatalf("check = %+v", chk)
	}

	res := &fakeResolver{err: fmt.Errorf("rate limited")}
	env.Engine.SetNameResolver(res)
	chk, _ = env.Engine.CheckDefinition(ctx, env.Factory.ID, files)
	if !chk.Valid() || !hasIssue(chk.Warnings, "automations/linear-intake/automation.md", "triggers[0].filter.teams", "rate limited") {
		t.Fatalf("check = %+v", chk)
	}

	res.err = nil
	res.names = map[string]map[string][]string{"teams": {"Eng": {"t1"}}, "states": {"Todo": {"s1", "s2"}}}
	chk, _ = env.Engine.CheckDefinition(ctx, env.Factory.ID, files)
	if chk.Valid() || !hasIssue(chk.Errors, "automations/linear-intake/automation.md", "triggers[0].filter.states", `2 linear states are named "Todo"`) {
		t.Fatalf("check = %+v", chk)
	}
	res.names["states"]["Todo"] = nil
	chk, _ = env.Engine.CheckDefinition(ctx, env.Factory.ID, files)
	if chk.Valid() || !hasIssue(chk.Errors, "automations/linear-intake/automation.md", "triggers[0].filter.states", `no linear state is named "Todo"`) {
		t.Fatalf("check = %+v", chk)
	}

	res.names["states"]["Todo"] = []string{"s1"}
	res.scope = nil
	if _, err := env.Engine.ApplyDefinition(ctx, env.Factory.ID, files, "a", "m", ""); err != nil {
		t.Fatal(err)
	}
	if len(res.scope) != 2 || len(res.scope[1]["team_ids"]) != 1 || res.scope[1]["team_ids"][0] != "t1" {
		t.Fatalf("states were resolved without the team scope: %+v", res.scope)
	}
	autos, _ := env.Store.ListAutomations(ctx, env.Factory.ID)
	var got map[string]any
	for _, a := range autos {
		if a.Name == "linear-intake" {
			_ = json.Unmarshal(a.Triggers[0].Filter, &got)
		}
	}
	if fmt.Sprint(got["team_ids"]) != "[t1]" || fmt.Sprint(got["state_ids"]) != "[s1]" || got["teams"] != nil || got["states"] != nil || fmt.Sprint(got["labels"]) != "[bug]" {
		t.Fatalf("filter = %v", got)
	}
}

// Jira and Teams name filters go through the resolver like Slack and Linear
// ones: the names become IDs, and the team of a Teams trigger is the scope of
// its channel and user names.
func TestJiraAndTeamsNameFiltersResolveToIDs(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	def := DefaultTestDefinition()
	def.Automations = append(def.Automations,
		core.Automation{Name: "jira-intake", Enabled: true, Prompt: "go",
			Triggers: []core.Trigger{{Provider: "jira", Event: "status_changed", Filter: json.RawMessage(`{"projects":["Website"],"statuses":["Done"]}`)}}},
		core.Automation{Name: "teams-intake", Enabled: true, Prompt: "go",
			Triggers: []core.Trigger{{Provider: "microsoft_teams", Event: "app_mention", Filter: json.RawMessage(`{"team_ids":["g1"],"channels":["Incidents"],"users":["Alice"]}`)}}},
	)
	env.Engine.compile = CompilerFor(def)
	files := map[string]string{"factory.yaml": "ok"}
	env.Store.connectIntegration(core.Integration{ID: "int_j", TeamID: "team1", Provider: "jira"})
	env.Store.connectIntegration(core.Integration{ID: "int_t", TeamID: "team1", Provider: "microsoft_teams"})

	res := &fakeResolver{names: map[string]map[string][]string{
		"projects": {"Website": {"WEB"}},
		"statuses": {"Done": {"10001", "10002"}},
		"channels": {"Incidents": {"19:inc@thread.tacv2"}},
		"users":    {"Alice": {"aad-alice"}},
	}}
	env.Engine.SetNameResolver(res)
	chk, _ := env.Engine.CheckDefinition(ctx, env.Factory.ID, files)
	if chk.Valid() || !hasIssue(chk.Errors, "automations/jira-intake/automation.md", "triggers[0].filter.statuses", `2 jira statuses are named "Done"`) {
		t.Fatalf("check = %+v", chk)
	}
	res.names["statuses"]["Done"] = []string{"10001"}
	res.scope = nil
	if _, err := env.Engine.ApplyDefinition(ctx, env.Factory.ID, files, "a", "m", ""); err != nil {
		t.Fatal(err)
	}
	// The Teams lookups got the team of the trigger as scope.
	scoped := 0
	for _, sc := range res.scope {
		if len(sc["team_ids"]) == 1 && sc["team_ids"][0] == "g1" {
			scoped++
		}
	}
	if scoped != 2 {
		t.Fatalf("channels and users were resolved without the team scope: %+v", res.scope)
	}
	autos, _ := env.Store.ListAutomations(ctx, env.Factory.ID)
	filters := map[string]map[string]any{}
	for _, a := range autos {
		var got map[string]any
		if len(a.Triggers) > 0 {
			_ = json.Unmarshal(a.Triggers[0].Filter, &got)
		}
		filters[a.Name] = got
	}
	j, tm := filters["jira-intake"], filters["teams-intake"]
	if fmt.Sprint(j["project_keys"]) != "[WEB]" || fmt.Sprint(j["status_ids"]) != "[10001]" || j["projects"] != nil || j["statuses"] != nil {
		t.Fatalf("jira filter = %v", j)
	}
	if fmt.Sprint(tm["channel_ids"]) != "[19:inc@thread.tacv2]" || fmt.Sprint(tm["user_ids"]) != "[aad-alice]" || fmt.Sprint(tm["team_ids"]) != "[g1]" || tm["channels"] != nil || tm["users"] != nil {
		t.Fatalf("teams filter = %v", tm)
	}
}

// failingStore fails one named write of an apply.
type failingStore struct {
	txTestStore
	failOn string
}

var errInjected = errors.New("injected write failure")

func (s *failingStore) fail(name string) error {
	if s.failOn == name {
		return fmt.Errorf("%s: %w", name, errInjected)
	}
	return nil
}

func (s *failingStore) CreateDefinitionVersion(ctx context.Context, v *core.DefinitionVersion) error {
	if err := s.fail("CreateDefinitionVersion"); err != nil {
		return err
	}
	return s.txTestStore.CreateDefinitionVersion(ctx, v)
}

func (s *failingStore) ReplaceFactoryResources(ctx context.Context, factoryID string, a []core.AgentConfig, au []core.Automation, sc []core.Scorer, wh []core.Webhook) error {
	if err := s.txTestStore.ReplaceFactoryResources(ctx, factoryID, a, au, sc, wh); err != nil {
		return err
	}
	return s.fail("ReplaceFactoryResources")
}

func (s *failingStore) KVSet(ctx context.Context, key, value string) error {
	if strings.HasSuffix(key, ":runtime") {
		if err := s.txTestStore.KVSet(ctx, key, value); err != nil {
			return err
		}
		if err := s.fail("KVSet"); err != nil {
			return err
		}
		return nil
	}
	return s.txTestStore.KVSet(ctx, key, value)
}

func (s *failingStore) SetFactoryIntegrations(ctx context.Context, factoryID string, providers []string, settings map[string]map[string]string) error {
	if err := s.txTestStore.SetFactoryIntegrations(ctx, factoryID, providers, settings); err != nil {
		return err
	}
	return s.fail("SetFactoryIntegrations")
}

func (s *failingStore) UpdateFactory(ctx context.Context, f *core.Factory) error {
	if err := s.txTestStore.UpdateFactory(ctx, f); err != nil {
		return err
	}
	return s.fail("UpdateFactory")
}

func (s *failingStore) UpdateDefinitionVersion(ctx context.Context, v *core.DefinitionVersion) error {
	if err := s.txTestStore.UpdateDefinitionVersion(ctx, v); err != nil {
		return err
	}
	return s.fail("UpdateDefinitionVersion")
}

// TestApplyIsOneTransaction fails each write of an apply in turn and checks
// that the factory stays on its old version in all tables.
func TestApplyIsOneTransaction(t *testing.T) {
	writes := []string{"CreateDefinitionVersion", "ReplaceFactoryResources", "KVSet", "SetFactoryIntegrations", "UpdateFactory", "UpdateDefinitionVersion"}
	for name, open := range txTestStores() {
		for _, w := range writes {
			t.Run(name+"/"+w, func(t *testing.T) {
				ctx := context.Background()
				fs := &failingStore{txTestStore: open(t)}
				bus := events.NewForStore(fs.txTestStore, events.DurableOptions{ReplicaID: "r"})
				defer bus.Close()
				eng, f := newTxEngine(t, fs, bus)
				f, _ = fs.GetFactory(ctx, f.ID)
				agentsBefore, _ := fs.ListAgents(ctx, f.ID)
				autosBefore, _ := fs.ListAutomations(ctx, f.ID)
				versionsBefore, _ := fs.ListDefinitionVersions(ctx, f.ID, core.ListOpts{Limit: 100})

				next := DefaultTestDefinition()
				next.Name = "renamed"
				next.Agents = append(next.Agents, core.AgentConfig{Name: "extra", Type: core.AgentCustom, Harness: next.Agents[0].Harness})
				next.Automations[0].Prompt = "changed"
				next.Integrations = []core.IntegrationDecl{{Type: "slack", Settings: map[string]string{"channel": "C2"}}}
				next.Machines["big"] = core.MachineSpec{Name: "big", OS: "linux", Arch: "x86_64", VCPUs: 16}
				eng.compile = CompilerFor(next)
				fs.failOn = w
				if _, err := eng.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok"}, "a", "m", ""); !errors.Is(err, errInjected) {
					t.Fatalf("apply error = %v", err)
				}

				after, _ := fs.GetFactory(ctx, f.ID)
				if after.ActiveVersionID != f.ActiveVersionID || after.Name != f.Name {
					t.Errorf("factory = %+v, want version %s", after, f.ActiveVersionID)
				}
				agents, _ := fs.ListAgents(ctx, f.ID)
				if len(agents) != len(agentsBefore) {
					t.Errorf("agents = %d, want %d", len(agents), len(agentsBefore))
				}
				autos, _ := fs.ListAutomations(ctx, f.ID)
				if len(autos) != 1 || autos[0].Prompt != autosBefore[0].Prompt {
					t.Errorf("automations = %+v", autos)
				}
				rt, _ := LoadFactoryRuntime(ctx, fs, f.ID)
				if rt.VersionID != f.ActiveVersionID || rt.Machines["big"].VCPUs != 8 {
					t.Errorf("runtime = %+v", rt)
				}
				integ, _ := fs.GetFactoryIntegrations(ctx, f.ID)
				if integ["slack"]["channel"] != "C1" {
					t.Errorf("integrations = %v", integ)
				}
				versions, _ := fs.ListDefinitionVersions(ctx, f.ID, core.ListOpts{Limit: 100})
				if len(versions) != len(versionsBefore) {
					t.Errorf("versions = %d, want %d", len(versions), len(versionsBefore))
				}

				// The same apply succeeds when nothing fails.
				fs.failOn = ""
				v, err := eng.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok"}, "a", "m", "")
				if err != nil {
					t.Fatal(err)
				}
				after, _ = fs.GetFactory(ctx, f.ID)
				agents, _ = fs.ListAgents(ctx, f.ID)
				if after.ActiveVersionID != v.ID || after.Name != "renamed" || len(agents) != len(agentsBefore)+1 {
					t.Errorf("after the good apply: factory %+v, %d agents", after, len(agents))
				}
			})
		}
	}
}
