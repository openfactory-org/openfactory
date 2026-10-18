package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// linkForges is a forge resolver with per-user tokens: a user in linked
// acts with a personal token; another user gets ErrExternalAuthRequired.
type linkForges struct {
	FakeForges
	linked map[string]bool
}

func (l linkForges) ForgeForUser(ctx context.Context, lk core.ForgeLookup, userID string) (core.Forge, bool, error) {
	if userID == "" {
		f, err := l.Forge(ctx, lk)
		return f, false, err
	}
	if !l.linked[userID] {
		return nil, false, fmt.Errorf("%w: user %s has no linked account", core.ErrExternalAuthRequired, userID)
	}
	f, err := l.Forge(ctx, lk)
	return f, true, err
}

type principalFixture struct {
	eng   *Engine
	st    *sqlstore.Store
	f     *core.Factory
	dana  *core.User // a person with a linked forge account
	erin  *core.User // a person without a link
	ci    *core.User // a service account
	autos map[string]string
}

// newPrincipalFixture applies a definition with the strategy creator and
// three automations: one without runAs, one that runs as dana and one that
// runs as the service account ci.
func newPrincipalFixture(t *testing.T, strategy string) *principalFixture {
	t.Helper()
	ctx := context.Background()
	st := testdb.New(t)
	_ = st.CreateTeam(ctx, &core.Team{ID: "team1", Name: "team"})
	pf := &principalFixture{st: st, autos: map[string]string{}}
	pf.dana = &core.User{ID: "usr_dana", TeamID: "team1", Email: "dana@example.com", Name: "Dana", Role: core.RoleEditor}
	pf.erin = &core.User{ID: "usr_erin", TeamID: "team1", Email: "erin@example.com", Name: "Erin", Role: core.RoleEditor}
	pf.ci = &core.User{ID: "sa_ci", TeamID: "team1", Name: "ci", Role: core.RoleEditor, Kind: core.UserKindServiceAccount}
	for _, u := range []*core.User{pf.dana, pf.erin, pf.ci} {
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	sec := NewFakeSecrets()
	_, _ = sec.Put(ctx, "team1", "", "ANTHROPIC_KEY", core.SecretAnthropicKey, []byte("sk-ant-test\n"))
	pf.f = &core.Factory{ID: core.NewID("fac"), TeamID: "team1", Name: "payments", Alias: core.NewID("bot"), Source: core.DefinitionManaged}
	if err := st.CreateFactory(ctx, pf.f); err != nil {
		t.Fatal(err)
	}
	def := DefaultTestDefinition()
	def.CredentialStrategy = strategy
	base := def.Automations[0]
	def.Automations = nil
	for _, a := range []struct{ name, runAs string }{{"plain", ""}, {"as-dana", "user:dana@example.com"}, {"as-ci", "serviceAccount:ci"}} {
		au := base
		au.Name, au.RunAs = a.name, a.runAs
		def.Automations = append(def.Automations, au)
	}
	pf.eng = New(Options{Store: st, Bus: NewFakeBus(), Compile: CompilerFor(def), Match: SimpleMatch,
		Secrets: sec, Forges: linkForges{linked: map[string]bool{"usr_dana": true}}, Logger: DiscardLogger()})
	if _, err := pf.eng.ApplyDefinition(ctx, pf.f.ID, map[string]string{"factory.yaml": "ok"}, "tester", "initial", ""); err != nil {
		t.Fatalf("apply definition: %v", err)
	}
	autos, _ := st.ListAutomations(ctx, pf.f.ID)
	for _, a := range autos {
		pf.autos[a.Name] = a.ID
	}
	return pf
}

func samePrincipal(p *core.Principal, typ core.PrincipalType, id string) bool {
	if p == nil {
		return typ == "" && id == ""
	}
	return p.Type == typ && p.ID == id
}

// TestRunPrincipals covers the principal of each kind of run.
func TestRunPrincipals(t *testing.T) {
	pf := newPrincipalFixture(t, "EXECUTOR")
	ctx := context.Background()
	n := 0
	dispatch := func(req core.DispatchRequest) *core.Run {
		t.Helper()
		n++
		req.FactoryID = pf.f.ID
		if req.Prompt == "" {
			req.Prompt = "Do the work."
		}
		req.Source = core.SourceRef{Provider: "api", Kind: "task", ExternalID: fmt.Sprintf("t%d", n)}
		_, run, err := pf.eng.Dispatch(ctx, req)
		if err != nil {
			t.Fatalf("dispatch %+v: %v", req, err)
		}
		got, _ := pf.st.GetRun(ctx, run.ID)
		return got
	}
	for _, tc := range []struct {
		name        string
		req         core.DispatchRequest
		creatorType core.PrincipalType
		creatorID   string
		runAsType   core.PrincipalType
		runAsID     string
	}{
		{"person through the API", core.DispatchRequest{CreatorID: "usr_dana", TriggeredBy: "api"}, core.PrincipalUser, "usr_dana", core.PrincipalUser, "usr_dana"},
		{"service account key", core.DispatchRequest{CreatorID: "sa_ci", TriggeredBy: "api"}, core.PrincipalServiceAccount, "sa_ci", core.PrincipalServiceAccount, "sa_ci"},
		{"mention from a linked account", core.DispatchRequest{Event: &core.Event{Actor: core.Actor{UserID: "usr_erin"}}, TriggeredBy: "mention"}, core.PrincipalUser, "usr_erin", core.PrincipalUser, "usr_erin"},
		{"automation without runAs", core.DispatchRequest{AutomationID: pf.autos["plain"], CreatorID: "usr_erin", TriggeredBy: "automation:x"}, core.PrincipalUser, "usr_erin", core.PrincipalFactory, pf.f.ID},
		{"schedule", core.DispatchRequest{AutomationID: pf.autos["plain"], TriggeredBy: "automation:x"}, "", "", core.PrincipalFactory, pf.f.ID},
		{"automation runs as a user", core.DispatchRequest{AutomationID: pf.autos["as-dana"], TriggeredBy: "automation:x"}, "", "", core.PrincipalUser, "usr_dana"},
		{"automation runs as a service account", core.DispatchRequest{AutomationID: pf.autos["as-ci"], CreatorID: "usr_erin", TriggeredBy: "automation:x"}, core.PrincipalUser, "usr_erin", core.PrincipalServiceAccount, "sa_ci"},
		{"benchmark", core.DispatchRequest{CreatorID: "usr_dana", TriggeredBy: "benchmark:b1"}, "", "", core.PrincipalFactory, pf.f.ID},
		{"self-improvement", core.DispatchRequest{TriggeredBy: "self_improvement"}, "", "", core.PrincipalFactory, pf.f.ID},
		{"unknown creator", core.DispatchRequest{CreatorID: "usr_ghost_of_other_team", TriggeredBy: "api"}, "", "", core.PrincipalFactory, pf.f.ID},
	} {
		run := dispatch(tc.req)
		if !samePrincipal(run.Creator, tc.creatorType, tc.creatorID) || !samePrincipal(run.RunAs, tc.runAsType, tc.runAsID) {
			t.Errorf("%s: creator %+v, run_as %+v", tc.name, run.Creator, run.RunAs)
		}
	}

	// A child run gets the principals of its parent.
	parent := dispatch(core.DispatchRequest{CreatorID: "sa_ci", TriggeredBy: "api"})
	agents, _ := pf.st.ListAgents(ctx, pf.f.ID)
	var implement string
	for _, a := range agents {
		if a.Type == core.AgentImplement {
			implement = a.ID
		}
	}
	kids, err := pf.eng.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: parent.ID,
		AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: implement, Prompt: "build it"}}})
	if err != nil || len(kids) != 1 {
		t.Fatalf("run agents: %v %v", kids, err)
	}
	if !samePrincipal(kids[0].Creator, core.PrincipalServiceAccount, "sa_ci") || !samePrincipal(kids[0].RunAs, core.PrincipalServiceAccount, "sa_ci") {
		t.Fatalf("child: creator %+v run_as %+v", kids[0].Creator, kids[0].RunAs)
	}

	// A filter by creator.
	list, err := pf.st.ListRuns(ctx, core.RunFilter{TeamID: "team1", CreatorID: "sa_ci"})
	if err != nil || len(list) < 2 {
		t.Fatalf("creator filter: %d runs, %v", len(list), err)
	}
}

// TestRunAsThatDoesNotExistFails checks that a runAs that names no principal
// fails the dispatch: the engine never falls back to the factory.
func TestRunAsThatDoesNotExistFails(t *testing.T) {
	pf := newPrincipalFixture(t, "EXECUTOR")
	ctx := context.Background()
	if err := pf.st.DeleteUser(ctx, "sa_ci"); err != nil {
		t.Fatal(err)
	}
	_, _, err := pf.eng.Dispatch(ctx, core.DispatchRequest{FactoryID: pf.f.ID, Prompt: "x", AutomationID: pf.autos["as-ci"], TriggeredBy: "automation:x"})
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("dispatch = %v", err)
	}
}

// TestCreatorWithoutLink covers credentialStrategy CREATOR: a person
// without a linked account gets external_authentication_required. An API
// request creates no run. A run that an automation starts is created and
// fails before it gets a task. A run that acts as the factory or a service
// account uses the credential of the factory.
func TestCreatorWithoutLink(t *testing.T) {
	pf := newPrincipalFixture(t, "CREATOR")
	ctx := context.Background()
	x := NewPlanner(PlannerOptions{Store: pf.st, Forges: pf.eng.forges, Logger: DiscardLogger()})
	rt, err := LoadFactoryRuntime(ctx, pf.st, pf.f.ID)
	if err != nil {
		t.Fatal(err)
	}

	before, _ := pf.st.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: pf.f.ID})
	_, _, err = pf.eng.Dispatch(ctx, core.DispatchRequest{FactoryID: pf.f.ID, Prompt: "x", CreatorID: "usr_erin", TriggeredBy: "api"})
	if !core.IsExternalAuthRequired(err) || !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("api dispatch = %v", err)
	}
	after, _ := pf.st.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: pf.f.ID})
	if len(after) != len(before) {
		t.Fatal("a refused API request must not create a work item")
	}

	// A linked person passes.
	_, run, err := pf.eng.Dispatch(ctx, core.DispatchRequest{FactoryID: pf.f.ID, Prompt: "x", CreatorID: "usr_dana", TriggeredBy: "api",
		Source: core.SourceRef{Provider: "api", ExternalID: "ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.CredentialPreflight(ctx, run); err != nil {
		t.Fatalf("linked preflight = %v", err)
	}

	// A mention from erin: the run is created and fails before a task.
	_, run, err = pf.eng.Dispatch(ctx, core.DispatchRequest{FactoryID: pf.f.ID, Prompt: "x", TriggeredBy: "mention",
		Event: &core.Event{Actor: core.Actor{UserID: "usr_erin"}}, Source: core.SourceRef{Provider: "slack", ExternalID: "m1"}})
	if err != nil {
		t.Fatalf("mention dispatch = %v", err)
	}
	var fe *core.FailureCodeError
	if err := x.CredentialPreflight(ctx, run); !errors.As(err, &fe) || fe.Code != core.FailureExternalAuthenticationRequired {
		t.Fatalf("mention preflight = %v", err)
	}
	if credentialFailureCode(fmt.Errorf("repository acme/api: %w", core.ErrExternalAuthRequired)) != core.FailureExternalAuthenticationRequired {
		t.Fatal("the credential error must map to its failure code")
	}

	// Factory and service account principals use the factory credential.
	for _, auto := range []string{"plain", "as-ci"} {
		_, run, err := pf.eng.Dispatch(ctx, core.DispatchRequest{FactoryID: pf.f.ID, Prompt: "x", AutomationID: pf.autos[auto], CreatorID: "usr_erin",
			TriggeredBy: "automation:x", Source: core.SourceRef{Provider: "api", ExternalID: auto}})
		if err != nil {
			t.Fatal(err)
		}
		if u := x.credentialUser(ctx, run, rt); u != "" {
			t.Fatalf("%s: credential user %q", auto, u)
		}
		if err := x.CredentialPreflight(ctx, run); err != nil {
			t.Fatalf("%s: preflight = %v", auto, err)
		}
	}
}
