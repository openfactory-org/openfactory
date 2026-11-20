// Package storetest is a conformance suite for core.Store implementations.
//
// Every case uses freshly generated IDs, team IDs, emails, KV keys and worker
// hosts, so the suite can also run against a shared database (PostgreSQL)
// without cross-test interference.
package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// Run executes the whole suite. newStore must return a ready store; the suite
// closes it at the end of each subtest.
func Run(t *testing.T, newStore func(t *testing.T) core.Store) {
	cases := []struct {
		name string
		fn   func(t *testing.T, s core.Store)
	}{
		{"Teams", testTeams},
		{"Users", testUsers},
		{"ExternalIdentities", testExternalIdentities},
		{"APIKeys", testAPIKeys},
		{"ServiceAccounts", testServiceAccounts},
		{"RunPrincipals", testRunPrincipals},
		{"Factories", testFactories},
		{"DefinitionVersions", testDefinitionVersions},
		{"FactoryResources", testFactoryResources},
		{"WorkItems", testWorkItems},
		{"WorkItemPagination", testWorkItemPagination},
		{"WorkItemEvents", testWorkItemEvents},
		{"Runs", testRuns},
		{"RunPagination", testRunPagination},
		{"RunMessages", testRunMessages},
		{"RunMessagesConcurrent", testRunMessagesConcurrent},
		{"RunnerSessions", testRunnerSessions},
		{"RunnerEvents", testRunnerEvents},
		{"RunnerCommands", testRunnerCommands},
		{"RunInputs", testRunInputs},
		{"RunTimeline", testRunTimeline},
		{"RunnerTx", testRunnerTx},
		{"Inbox", testInbox},
		{"Secrets", testSecrets},
		{"WebhookDeliveries", testWebhookDeliveries},
		{"Integrations", testIntegrations},
		{"FactoryIntegrations", testFactoryIntegrations},
		{"Scores", testScores},
		{"BenchmarkRuns", testBenchmarkRuns},
		{"Workers", testWorkers},
		{"Environments", testEnvironments},
		{"MCPServers", testMCPServers},
		{"KV", testKV},
		{"KVExpiry", testKVExpiry},
		{"CompareAndSwap", testCompareAndSwap},
		{"Prune", testPrune},
		{"JSONFidelity", testJSONFidelity},
		{"RunVersion", testRunVersion},
		{"RunVersionRace", testRunVersionRace},
		{"WorkItemVersion", testWorkItemVersion},
		{"InboxVersion", testInboxVersion},
		{"PullRequestFacts", testPullRequestFacts},
		{"DispatchLedger", testDispatchLedger},
		{"DispatchLedgerRace", testDispatchLedgerRace},
		{"DispatchLedgerTx", testDispatchLedgerTx},
		{"WebhookRetention", testWebhookRetention},
		{"WebhookRetentionManyRows", testWebhookRetentionManyRows},
		{"LinkExternalIdentity", testLinkExternalIdentity},
		{"LinkExternalIdentityRace", testLinkExternalIdentityRace},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newStore(t)
			t.Cleanup(func() { _ = s.Close() })
			c.fn(t, s)
		})
	}
	runCoordinator(t, newStore)
	runTx(t, newStore)
}

// ---------- helpers ----------

var ctx = context.Background()

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func isErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got error %v, want %v", err, want)
	}
}

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func timeEq(t *testing.T, got, want time.Time, what string) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func jsonEq(t *testing.T, got, want json.RawMessage, what string) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("%s: invalid json %q: %v", what, got, err)
	}
	_ = json.Unmarshal(want, &b)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if !bytes.Equal(ab, bb) {
		t.Fatalf("%s: got %s, want %s", what, got, want)
	}
}

func uid(prefix string) string { return core.NewID(prefix) }

// base is a fixed reference time with sub-microsecond precision.
var base = time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)

func mkTeam(t *testing.T, s core.Store) *core.Team {
	t.Helper()
	tm := &core.Team{Name: "team"}
	ok(t, s.CreateTeam(ctx, tm))
	return tm
}

// anyFactory is a factory ID for runs of tests that do not read the factory.
// The store does not check that the factory exists.
const anyFactory = "fac_any"

func mkFactory(t *testing.T, s core.Store, teamID string) *core.Factory {
	t.Helper()
	f := &core.Factory{TeamID: teamID, Name: "Factory " + uid(""), Alias: uid("bot"), Source: core.DefinitionManaged}
	ok(t, s.CreateFactory(ctx, f))
	return f
}

func runIDs(rs []core.Run) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// ---------- identity ----------

func testTeams(t *testing.T, s core.Store) {
	tm := &core.Team{Name: "Acme"}
	ok(t, s.CreateTeam(ctx, tm))
	if tm.ID == "" || tm.CreatedAt.IsZero() {
		t.Fatalf("CreateTeam must default ID and CreatedAt: %+v", tm)
	}
	got, err := s.GetTeam(ctx, tm.ID)
	ok(t, err)
	eq(t, got.Name, "Acme", "name")
	timeEq(t, got.CreatedAt, tm.CreatedAt, "created_at")

	isErr(t, s.CreateTeam(ctx, &core.Team{ID: tm.ID, Name: "dup"}), core.ErrConflict)
	_, err = s.GetTeam(ctx, uid("team"))
	isErr(t, err, core.ErrNotFound)

	list, err := s.ListTeams(ctx)
	ok(t, err)
	found := false
	for _, x := range list {
		found = found || x.ID == tm.ID
	}
	if !found {
		t.Fatalf("ListTeams missing %s", tm.ID)
	}
}

func testUsers(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	email := uid("u") + "@Example.com"
	u := &core.User{TeamID: tm.ID, Email: email, Name: "Ann", Role: core.RoleAdmin}
	ok(t, s.CreateUser(ctx, u))
	ok(t, s.CreateUser(ctx, &core.User{TeamID: tm.ID, Name: "No email"}))
	ok(t, s.CreateUser(ctx, &core.User{TeamID: tm.ID, Name: "No email 2"}))

	got, err := s.GetUser(ctx, u.ID)
	ok(t, err)
	eq(t, got.Role, core.RoleAdmin, "role")

	got, err = s.GetUserByEmail(ctx, strings.ToUpper(email))
	ok(t, err)
	eq(t, got.ID, u.ID, "by email (case-insensitive)")

	isErr(t, s.CreateUser(ctx, &core.User{TeamID: tm.ID, Email: strings.ToLower(email)}), core.ErrConflict)
	_, err = s.GetUserByEmail(ctx, "missing-"+email)
	isErr(t, err, core.ErrNotFound)
	_, err = s.GetUserByEmail(ctx, "")
	isErr(t, err, core.ErrNotFound)
	_, err = s.GetUser(ctx, uid("usr"))
	isErr(t, err, core.ErrNotFound)

	list, err := s.ListUsers(ctx, tm.ID)
	ok(t, err)
	eq(t, len(list), 3, "users in team")
	eq(t, list[0].ID, u.ID, "oldest first")

	created := got.CreatedAt
	upd := *u
	upd.Name, upd.Role, upd.Email = "Ann B", core.RoleUser, "renamed-"+email
	upd.CreatedAt = time.Time{}
	ok(t, s.UpdateUser(ctx, &upd))
	got, err = s.GetUser(ctx, u.ID)
	ok(t, err)
	eq(t, got.Name, "Ann B", "updated name")
	eq(t, got.Role, core.RoleUser, "updated role")
	timeEq(t, got.CreatedAt, created, "created at preserved")
	got, err = s.GetUserByEmail(ctx, "RENAMED-"+email)
	ok(t, err)
	eq(t, got.ID, u.ID, "by new email")
	_, err = s.GetUserByEmail(ctx, email)
	isErr(t, err, core.ErrNotFound)
	isErr(t, s.UpdateUser(ctx, &core.User{ID: uid("usr"), TeamID: tm.ID}), core.ErrNotFound)
}

func testExternalIdentities(t *testing.T, s core.Store) {
	ext := uid("U")
	x := &core.ExternalIdentity{UserID: "u1", Provider: "slack", ExternalID: ext, Handle: "ann"}
	ok(t, s.UpsertExternalIdentity(ctx, x))
	x.UserID, x.Handle = "u2", "annie"
	ok(t, s.UpsertExternalIdentity(ctx, x))
	got, err := s.FindExternalIdentity(ctx, "slack", ext)
	ok(t, err)
	eq(t, got.UserID, "u2", "upserted user")
	eq(t, got.Handle, "annie", "upserted handle")
	_, err = s.FindExternalIdentity(ctx, "github", ext)
	isErr(t, err, core.ErrNotFound)
}

func testAPIKeys(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	prefix := uid("ofk")
	k := &core.APIKey{TeamID: tm.ID, UserID: "u", Name: "ci", Prefix: prefix, HashedKey: "hash-1"}
	ok(t, s.CreateAPIKey(ctx, k))
	got, err := s.GetAPIKeyByPrefix(ctx, prefix)
	ok(t, err)
	eq(t, got.HashedKey, "hash-1", "hashed key must persist")
	if got.LastUsedAt != nil {
		t.Fatal("LastUsedAt should be nil")
	}
	isErr(t, s.CreateAPIKey(ctx, &core.APIKey{TeamID: tm.ID, Prefix: prefix}), core.ErrConflict)

	at := base.Add(time.Hour)
	ok(t, s.TouchAPIKey(ctx, k.ID, at))
	got, err = s.GetAPIKeyByPrefix(ctx, prefix)
	ok(t, err)
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(at) {
		t.Fatalf("LastUsedAt = %v, want %v", got.LastUsedAt, at)
	}
	eq(t, got.HashedKey, "hash-1", "hashed key survives touch")
	isErr(t, s.TouchAPIKey(ctx, uid("key"), at), core.ErrNotFound)

	k2 := &core.APIKey{TeamID: tm.ID, Name: "second", Prefix: uid("ofk"), HashedKey: "hash-2"}
	ok(t, s.CreateAPIKey(ctx, k2))
	list, err := s.ListAPIKeys(ctx, tm.ID)
	ok(t, err)
	eq(t, len(list), 2, "keys")
	for _, x := range list {
		if x.HashedKey == "" {
			t.Fatal("ListAPIKeys must include HashedKey")
		}
	}
	ok(t, s.DeleteAPIKey(ctx, k.ID))
	isErr(t, s.DeleteAPIKey(ctx, k.ID), core.ErrNotFound)
	_, err = s.GetAPIKeyByPrefix(ctx, prefix)
	isErr(t, err, core.ErrNotFound)
}

// testServiceAccounts covers service account records: users of kind
// service_account without an email, and DeleteUser, which removes the API
// keys of the account too.
func testServiceAccounts(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	d, okDel := s.(core.UserDeleter)
	if !okDel {
		t.Fatal("the store must implement core.UserDeleter")
	}
	a := &core.User{ID: uid("sa"), TeamID: tm.ID, Name: "ci", Role: core.RoleEditor, Kind: core.UserKindServiceAccount}
	b := &core.User{ID: uid("sa"), TeamID: tm.ID, Name: "bot", Role: core.RoleUser, Kind: core.UserKindServiceAccount}
	ok(t, s.CreateUser(ctx, a))
	ok(t, s.CreateUser(ctx, b)) // two accounts without an email
	got, err := s.GetUser(ctx, a.ID)
	ok(t, err)
	if !got.IsServiceAccount() || got.AsPrincipal().Type != core.PrincipalServiceAccount {
		t.Fatalf("kind must persist: %+v", got)
	}
	ka := &core.APIKey{TeamID: tm.ID, UserID: a.ID, Name: "k1", Prefix: uid("ofa"), HashedKey: "h1"}
	kb := &core.APIKey{TeamID: tm.ID, UserID: b.ID, Name: "k2", Prefix: uid("ofa"), HashedKey: "h2"}
	ok(t, s.CreateAPIKey(ctx, ka))
	ok(t, s.CreateAPIKey(ctx, kb))
	ok(t, d.DeleteUser(ctx, a.ID))
	isErr(t, d.DeleteUser(ctx, a.ID), core.ErrNotFound)
	_, err = s.GetUser(ctx, a.ID)
	isErr(t, err, core.ErrNotFound)
	_, err = s.GetAPIKeyByPrefix(ctx, ka.Prefix)
	isErr(t, err, core.ErrNotFound)
	_, err = s.GetAPIKeyByPrefix(ctx, kb.Prefix)
	ok(t, err)
}

// testRunPrincipals covers the creator and run_as principals of a run
// and the creator filter of ListRuns.
func testRunPrincipals(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	alice := &core.Principal{Type: core.PrincipalUser, ID: uid("usr"), Name: "Alice"}
	ci := &core.Principal{Type: core.PrincipalServiceAccount, ID: uid("sa"), Name: "ci"}
	fac := &core.Principal{Type: core.PrincipalFactory, ID: uid("fac")}
	r1 := &core.Run{FactoryID: anyFactory, TeamID: tm.ID, Prompt: "a", Creator: alice, RunAs: alice}
	r2 := &core.Run{FactoryID: anyFactory, TeamID: tm.ID, Prompt: "b", Creator: ci, RunAs: ci}
	r3 := &core.Run{FactoryID: anyFactory, TeamID: tm.ID, Prompt: "c", RunAs: fac}
	for _, r := range []*core.Run{r1, r2, r3} {
		ok(t, s.CreateRun(ctx, r))
	}
	got, err := s.GetRun(ctx, r2.ID)
	ok(t, err)
	if got.Creator == nil || *got.Creator != *ci || got.RunAs == nil || *got.RunAs != *ci {
		t.Fatalf("principals must persist: %+v %+v", got.Creator, got.RunAs)
	}
	list, err := s.ListRuns(ctx, core.RunFilter{TeamID: tm.ID, CreatorID: alice.ID})
	ok(t, err)
	if len(list) != 1 || list[0].ID != r1.ID {
		t.Fatalf("creator filter: %+v", list)
	}
	// An update keeps the filter column in step.
	r1.Creator = ci
	ok(t, s.UpdateRun(ctx, r1))
	list, err = s.ListRuns(ctx, core.RunFilter{TeamID: tm.ID, CreatorID: ci.ID})
	ok(t, err)
	eq(t, len(list), 2, "runs of the service account")
}

// ---------- factories ----------

func testFactories(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	alias := uid("Bot")
	f := &core.Factory{TeamID: tm.ID, Name: "Zeta builders", Alias: alias, Source: core.DefinitionGit, GitRepo: "github.com/acme/def", GitBranch: "main"}
	ok(t, s.CreateFactory(ctx, f))
	if f.CreatedAt.IsZero() || f.UpdatedAt.IsZero() {
		t.Fatal("timestamps must be defaulted")
	}
	got, err := s.GetFactory(ctx, f.ID)
	ok(t, err)
	eq(t, got.GitRepo, "github.com/acme/def", "git repo")

	for _, a := range []string{alias, strings.ToLower(alias), "@" + alias} {
		got, err = s.GetFactoryByAlias(ctx, tm.ID, a)
		ok(t, err)
		eq(t, got.ID, f.ID, "by alias "+a)
	}
	_, err = s.GetFactoryByAlias(ctx, tm.ID, "nope-"+alias)
	isErr(t, err, core.ErrNotFound)

	isErr(t, s.CreateFactory(ctx, &core.Factory{TeamID: tm.ID, Name: "dup", Alias: strings.ToUpper(alias)}), core.ErrConflict)
	other := mkTeam(t, s)
	ok(t, s.CreateFactory(ctx, &core.Factory{TeamID: other.ID, Name: "same alias other team", Alias: alias}))
	// Empty aliases never collide.
	ok(t, s.CreateFactory(ctx, &core.Factory{TeamID: tm.ID, Name: "Alpha"}))
	ok(t, s.CreateFactory(ctx, &core.Factory{TeamID: tm.ID, Name: "Beta"}))

	before := f.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	f.Description = "updated"
	ok(t, s.UpdateFactory(ctx, f))
	got, err = s.GetFactory(ctx, f.ID)
	ok(t, err)
	eq(t, got.Description, "updated", "description")
	if !got.UpdatedAt.After(before) {
		t.Fatalf("UpdatedAt not advanced: %v vs %v", got.UpdatedAt, before)
	}
	isErr(t, s.UpdateFactory(ctx, &core.Factory{ID: uid("fac"), TeamID: tm.ID}), core.ErrNotFound)

	list, err := s.ListFactories(ctx, tm.ID, "")
	ok(t, err)
	eq(t, len(list), 3, "factories in team")
	eq(t, list[0].Name, "Alpha", "sorted by name")
	list, err = s.ListFactories(ctx, tm.ID, "zeta")
	ok(t, err)
	eq(t, len(list), 1, "search by name")
	list, err = s.ListFactories(ctx, tm.ID, "100%")
	ok(t, err)
	eq(t, len(list), 0, "LIKE wildcards are escaped")

	ok(t, s.DeleteFactory(ctx, f.ID))
	isErr(t, s.DeleteFactory(ctx, f.ID), core.ErrNotFound)
	_, err = s.GetFactory(ctx, f.ID)
	isErr(t, err, core.ErrNotFound)
}

func testDefinitionVersions(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	isErr(t, s.CreateDefinitionVersion(ctx, &core.DefinitionVersion{FactoryID: uid("fac")}), core.ErrNotFound)

	var ids []string
	for i := 0; i < 7; i++ {
		v := &core.DefinitionVersion{
			FactoryID: f.ID,
			Files:     map[string]string{"factory.yaml": fmt.Sprintf("version: %d", i)},
			Valid:     i%2 == 0,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		if !v.Valid {
			v.Errors = []core.ValidationError{{File: "factory.yaml", Line: 3, Message: "bad"}}
		}
		ok(t, s.CreateDefinitionVersion(ctx, v))
		ids = append(ids, v.ID)
	}
	got, err := s.GetDefinitionVersion(ctx, ids[1])
	ok(t, err)
	eq(t, got.Files["factory.yaml"], "version: 1", "files")
	eq(t, len(got.Errors), 1, "errors")
	eq(t, got.Errors[0].Line, 3, "error line")

	applied := base.Add(time.Minute)
	got.AppliedAt = &applied
	ok(t, s.UpdateDefinitionVersion(ctx, got))
	got, err = s.GetDefinitionVersion(ctx, ids[1])
	ok(t, err)
	if got.AppliedAt == nil || !got.AppliedAt.Equal(applied) {
		t.Fatal("AppliedAt not persisted")
	}
	isErr(t, s.UpdateDefinitionVersion(ctx, &core.DefinitionVersion{ID: uid("defv"), FactoryID: f.ID}), core.ErrNotFound)

	// Pagination: newest first, 3 per page.
	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		list, err := s.ListDefinitionVersions(ctx, f.ID, core.ListOpts{Limit: 3, Cursor: cursor})
		ok(t, err)
		if len(list) == 0 {
			break
		}
		for _, v := range list {
			seen = append(seen, v.ID)
		}
		last := list[len(list)-1]
		cursor = storeutil.EncodeCursor(last.CreatedAt, last.ID)
	}
	eq(t, len(seen), 7, "paged total")
	for i := range seen {
		eq(t, seen[i], ids[6-i], fmt.Sprintf("page order %d", i))
	}
	all, err := s.ListDefinitionVersions(ctx, f.ID, core.ListOpts{})
	ok(t, err)
	eq(t, len(all), 7, "default limit returns all")
	_, err = s.ListDefinitionVersions(ctx, f.ID, core.ListOpts{Cursor: "%%%not-a-cursor"})
	isErr(t, err, core.ErrInvalid)
	_, err = s.GetDefinitionVersion(ctx, uid("defv"))
	isErr(t, err, core.ErrNotFound)
}

func testFactoryResources(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	agents := []core.AgentConfig{
		{ID: uid("agt"), Name: "foreman", Type: core.AgentForeman, Harness: core.HarnessConfig{Type: core.HarnessClaude, Model: "m"},
			MCPServers: map[string]core.MCPServer{"gh": {Command: "gh-mcp", Args: []string{"--x"}, Env: map[string]string{"T": "{{TOKEN}}"}}},
			Skills:     []core.Skill{{Name: "s1", Content: "body"}}, IdleTimeoutMinutes: 15},
		{ID: uid("agt"), Name: "implementer", Type: core.AgentImplement, SpawnableBy: []string{"foreman"}},
	}
	filter := json.RawMessage(`{"labels":["bug"]}`)
	automations := []core.Automation{
		{ID: uid("auto"), Name: "on-issue", Enabled: true, Triggers: []core.Trigger{{Provider: "github", Event: "issue_labeled", Filter: filter}}},
		{ID: uid("auto"), Name: "disabled", Enabled: false},
		{ID: uid("auto"), Name: "nightly", Enabled: true, Triggers: []core.Trigger{{Provider: "schedule", Schedule: &core.Schedule{Cron: "0 3 * * *"}}}},
	}
	scorers := []core.Scorer{{ID: uid("scr"), Slug: "quality", Labels: []core.ScorerLabel{{Value: "good", Score: 1}}, PassingScore: 0.5}}
	webhooks := []core.Webhook{{ID: uid("wh"), Name: "pagerduty", AuthMode: "signature", SignatureScheme: "pagerduty", Enabled: true}}
	ok(t, s.ReplaceFactoryResources(ctx, f.ID, agents, automations, scorers, webhooks))

	la, err := s.ListAgents(ctx, f.ID)
	ok(t, err)
	eq(t, len(la), 2, "agents")
	eq(t, la[0].Name, "foreman", "agent order preserved")
	eq(t, la[0].FactoryID, f.ID, "factory id set")
	eq(t, la[0].MCPServers["gh"].Env["T"], "{{TOKEN}}", "nested map")
	eq(t, la[0].Skills[0].Content, "body", "skills")
	if la[1].SpawnableBy[0] != "foreman" {
		t.Fatal("SpawnableBy lost")
	}
	ga, err := s.GetAgent(ctx, agents[1].ID)
	ok(t, err)
	eq(t, ga.Name, "implementer", "get agent")
	ga, err = s.GetAgentByName(ctx, f.ID, "foreman")
	ok(t, err)
	eq(t, ga.ID, agents[0].ID, "by name")
	_, err = s.GetAgentByName(ctx, f.ID, "missing")
	isErr(t, err, core.ErrNotFound)
	_, err = s.GetAgent(ctx, uid("agt"))
	isErr(t, err, core.ErrNotFound)

	lau, err := s.ListAutomations(ctx, f.ID)
	ok(t, err)
	eq(t, len(lau), 3, "automations")
	jsonEq(t, lau[0].Triggers[0].Filter, filter, "trigger filter")
	eq(t, lau[2].Triggers[0].Schedule.Cron, "0 3 * * *", "schedule")
	enabled, err := s.ListAllEnabledAutomations(ctx)
	ok(t, err)
	var mine []string
	for _, a := range enabled {
		if a.FactoryID == f.ID {
			mine = append(mine, a.Name)
		}
	}
	eq(t, strings.Join(mine, ","), "on-issue,nightly", "enabled automations")

	ls, err := s.ListScorers(ctx, f.ID)
	ok(t, err)
	eq(t, len(ls), 1, "scorers")
	eq(t, ls[0].Labels[0].Value, "good", "scorer labels")
	lw, err := s.ListWebhooks(ctx, f.ID)
	ok(t, err)
	eq(t, len(lw), 1, "webhooks")
	gw, err := s.GetWebhook(ctx, webhooks[0].ID)
	ok(t, err)
	eq(t, gw.SignatureScheme, "pagerduty", "webhook")
	_, err = s.GetWebhook(ctx, uid("wh"))
	isErr(t, err, core.ErrNotFound)

	// Failed replacement (duplicate agent names) leaves previous resources intact.
	dup := []core.AgentConfig{{ID: uid("agt"), Name: "x"}, {ID: uid("agt"), Name: "x"}}
	isErr(t, s.ReplaceFactoryResources(ctx, f.ID, dup, nil, nil, nil), core.ErrConflict)
	la, err = s.ListAgents(ctx, f.ID)
	ok(t, err)
	eq(t, len(la), 2, "agents intact after failed replace")

	// Successful replacement swaps everything.
	ok(t, s.ReplaceFactoryResources(ctx, f.ID, []core.AgentConfig{{Name: "solo", Type: core.AgentForeman}}, nil, nil, nil))
	la, err = s.ListAgents(ctx, f.ID)
	ok(t, err)
	eq(t, len(la), 1, "agents replaced")
	if la[0].ID == "" {
		t.Fatal("agent id must be defaulted")
	}
	lau, err = s.ListAutomations(ctx, f.ID)
	ok(t, err)
	eq(t, len(lau), 0, "automations replaced")
	_, err = s.GetWebhook(ctx, webhooks[0].ID)
	isErr(t, err, core.ErrNotFound)

	isErr(t, s.ReplaceFactoryResources(ctx, uid("fac"), nil, nil, nil, nil), core.ErrNotFound)

	// Deleting the factory removes its resources.
	ok(t, s.DeleteFactory(ctx, f.ID))
	la, err = s.ListAgents(ctx, f.ID)
	ok(t, err)
	eq(t, len(la), 0, "agents removed with factory")
}

// ---------- work items ----------

func testWorkItems(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	src := core.SourceRef{Provider: "github", Kind: "issue", Repo: "acme/api", Number: 42, Title: "Crash"}
	w := &core.WorkItem{FactoryID: f.ID, Title: "Fix crash", Source: src, CreatorID: "u1",
		Labels: map[string]string{"prio": "p1"}, CreatedAt: base}
	ok(t, s.CreateWorkItem(ctx, w))
	eq(t, w.Stage, core.StageIntake, "default stage")
	eq(t, w.SourceKey, src.Key(), "source key defaulted")

	got, err := s.GetWorkItem(ctx, w.ID)
	ok(t, err)
	eq(t, got.Labels["prio"], "p1", "labels")
	eq(t, got.Source.Number, 42, "source")
	timeEq(t, got.CreatedAt, base, "created_at precision")

	found, err := s.FindWorkItemBySource(ctx, f.ID, src.Key())
	ok(t, err)
	eq(t, found.ID, w.ID, "find by source")

	// A newer non-terminal item with the same source wins.
	w2 := &core.WorkItem{FactoryID: f.ID, Title: "Again", Source: src, CreatedAt: base.Add(time.Minute)}
	ok(t, s.CreateWorkItem(ctx, w2))
	found, err = s.FindWorkItemBySource(ctx, f.ID, src.Key())
	ok(t, err)
	eq(t, found.ID, w2.ID, "most recent")

	// Terminal items are ignored.
	w2.Stage = core.StageComplete
	done := base.Add(2 * time.Minute)
	w2.CompletedAt = &done
	w2.PullRequests = []core.PullRequestRef{{Forge: "GITHUB", Repo: "acme/api", Number: 7, State: "merged"}}
	ok(t, s.UpdateWorkItem(ctx, w2))
	found, err = s.FindWorkItemBySource(ctx, f.ID, src.Key())
	ok(t, err)
	eq(t, found.ID, w.ID, "terminal ignored")
	got, err = s.GetWorkItem(ctx, w2.ID)
	ok(t, err)
	eq(t, got.PullRequests[0].Number, 7, "pull requests")

	w.Stage = core.StageCancelled
	ok(t, s.UpdateWorkItem(ctx, w))
	_, err = s.FindWorkItemBySource(ctx, f.ID, src.Key())
	isErr(t, err, core.ErrNotFound)
	_, err = s.FindWorkItemBySource(ctx, f.ID, "")
	isErr(t, err, core.ErrNotFound)
	_, err = s.FindWorkItemBySource(ctx, uid("fac"), src.Key())
	isErr(t, err, core.ErrNotFound)

	isErr(t, s.UpdateWorkItem(ctx, &core.WorkItem{ID: uid("wi"), FactoryID: f.ID}), core.ErrNotFound)
	isErr(t, s.CreateWorkItem(ctx, &core.WorkItem{ID: w.ID, FactoryID: f.ID}), core.ErrConflict)
	_, err = s.GetWorkItem(ctx, uid("wi"))
	isErr(t, err, core.ErrNotFound)
}

func testWorkItemPagination(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	stages := []core.Stage{core.StageIntake, core.StageBuilding, core.StageComplete}
	var all []*core.WorkItem
	for i := 0; i < 12; i++ {
		w := &core.WorkItem{FactoryID: f.ID, Title: fmt.Sprintf("Item %02d", i), Stage: stages[i%3],
			CreatorID: fmt.Sprintf("u%d", i%2), CreatedAt: base.Add(time.Duration(i) * time.Second)}
		if i == 5 {
			w.Title = "Upgrade 100% of deps"
		}
		ok(t, s.CreateWorkItem(ctx, w))
		all = append(all, w)
	}
	// Same timestamp tie-break by id.
	tie := &core.WorkItem{FactoryID: f.ID, Title: "Tie", CreatedAt: all[11].CreatedAt}
	ok(t, s.CreateWorkItem(ctx, tie))

	var got []string
	cursor := ""
	for {
		page, err := s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, ListOpts: core.ListOpts{Limit: 5, Cursor: cursor}})
		ok(t, err)
		for _, w := range page {
			got = append(got, w.ID)
		}
		if len(page) < 5 {
			break
		}
		cursor = storeutil.EncodeCursor(page[len(page)-1].CreatedAt, page[len(page)-1].ID)
	}
	eq(t, len(got), 13, "total paged")
	uniq := map[string]bool{}
	for _, id := range got {
		uniq[id] = true
	}
	eq(t, len(uniq), 13, "no duplicates across pages")
	eq(t, got[12], all[0].ID, "oldest last")

	list, err := s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, Stages: []core.Stage{core.StageBuilding, core.StageComplete}})
	ok(t, err)
	eq(t, len(list), 8, "stage filter")
	list, err = s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, CreatorID: "u1"})
	ok(t, err)
	eq(t, len(list), 6, "creator filter")
	list, err = s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, Search: "100%"})
	ok(t, err)
	eq(t, len(list), 1, "search")
	list, err = s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, Search: "ITEM 0"})
	ok(t, err)
	eq(t, len(list), 9, "case-insensitive search")
	since := base.Add(10 * time.Second)
	list, err = s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, Since: &since})
	ok(t, err)
	eq(t, len(list), 3, "since filter (inclusive)")
	list, err = s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, ListOpts: core.ListOpts{Limit: 100000}})
	ok(t, err)
	eq(t, len(list), 13, "limit capped but sufficient")
	_, err = s.ListWorkItems(ctx, core.WorkItemFilter{FactoryID: f.ID, ListOpts: core.ListOpts{Cursor: "bm9wZQ"}})
	isErr(t, err, core.ErrInvalid)
}

func testWorkItemEvents(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	w := &core.WorkItem{FactoryID: f.ID, Title: "x"}
	ok(t, s.CreateWorkItem(ctx, w))
	ts := base
	for i := 0; i < 5; i++ {
		// Identical timestamps: order must follow insertion.
		e := &core.WorkItemEvent{WorkItemID: w.ID, Kind: "note", Actor: "a", Data: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)), CreatedAt: ts}
		ok(t, s.AppendWorkItemEvent(ctx, e))
		if e.ID == "" {
			t.Fatal("event id must be defaulted")
		}
	}
	list, err := s.ListWorkItemEvents(ctx, w.ID)
	ok(t, err)
	eq(t, len(list), 5, "events")
	for i, e := range list {
		jsonEq(t, e.Data, json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)), "event order")
	}
	isErr(t, s.AppendWorkItemEvent(ctx, &core.WorkItemEvent{WorkItemID: uid("wi"), Kind: "note"}), core.ErrNotFound)
	list, err = s.ListWorkItemEvents(ctx, uid("wi"))
	ok(t, err)
	eq(t, len(list), 0, "no events")
}

// ---------- runs ----------

func testRunVersion(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	r := &core.Run{FactoryID: anyFactory, TeamID: tm.ID, Name: "versioned"}
	ok(t, s.CreateRun(ctx, r))
	eq(t, r.Version, int64(1), "version after create")
	got, err := s.GetRun(ctx, r.ID)
	ok(t, err)
	eq(t, got.Version, int64(1), "stored version")

	stale := *got
	got.Result = "first"
	ok(t, s.UpdateRun(ctx, got))
	eq(t, got.Version, int64(2), "caller version after update")
	again, err := s.GetRun(ctx, r.ID)
	ok(t, err)
	eq(t, again.Version, int64(2), "stored version after update")
	eq(t, again.Result, "first", "result")

	stale.Result = "lost"
	isErr(t, s.UpdateRun(ctx, &stale), core.ErrConflict)
	again, err = s.GetRun(ctx, r.ID)
	ok(t, err)
	eq(t, again.Result, "first", "stale write is not stored")
	eq(t, again.Version, int64(2), "version after conflict")

	// The same struct can be written twice.
	got.Result = "second"
	ok(t, s.UpdateRun(ctx, got))
	eq(t, got.Version, int64(3), "version after second update")

	isErr(t, s.UpdateRun(ctx, &core.Run{ID: uid("run"), Version: 1}), core.ErrNotFound)

	if tr, isTx := s.(core.Transactor); isTx {
		ok(t, tr.InTx(ctx, func(ctx context.Context) error {
			cur, err := s.GetRun(ctx, r.ID)
			if err != nil {
				return err
			}
			cur.Result = "in tx"
			return s.UpdateRun(ctx, cur)
		}))
		again, err = s.GetRun(ctx, r.ID)
		ok(t, err)
		eq(t, again.Version, int64(4), "version after tx update")
		eq(t, again.Result, "in tx", "tx result")
	}
}

func testRuns(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	r := &core.Run{TeamID: tm.ID, FactoryID: f.ID, WorkItemID: "wi1", AgentName: "foreman", Name: "root", Prompt: "do it",
		Config:   core.AgentConfig{Name: "foreman", Harness: core.HarnessConfig{Type: core.HarnessClaude, Model: "x"}, IdleTimeoutMinutes: 5},
		Metadata: map[string]string{"k": "v"}, TriggeredBy: "api"}
	ok(t, s.CreateRun(ctx, r))
	eq(t, r.State, core.RunQueued, "default state")
	child := &core.Run{TeamID: tm.ID, FactoryID: f.ID, WorkItemID: "wi1", ParentRunID: r.ID, AgentName: "implementer", Name: "impl", State: core.RunRunning}
	ok(t, s.CreateRun(ctx, child))
	other := &core.Run{TeamID: tm.ID, FactoryID: mkFactory(t, s, tm.ID).ID, Name: "other", State: core.RunSucceeded}
	ok(t, s.CreateRun(ctx, other))
	// Each run belongs to a factory.
	isErr(t, s.CreateRun(ctx, &core.Run{TeamID: tm.ID, Name: "no factory"}), core.ErrInvalid)

	got, err := s.GetRun(ctx, r.ID)
	ok(t, err)
	eq(t, got.Config.Harness.Type, core.HarnessClaude, "config snapshot")
	eq(t, got.Config.IdleTimeoutMinutes, 5, "config idle")
	eq(t, got.Metadata["k"], "v", "metadata")

	fin := base.Add(time.Hour)
	got.State = core.RunSucceeded
	got.Result = "done"
	got.Usage = core.Usage{InputTokens: 10, OutputTokens: 20, CostUSD: 0.5, DurationMS: 1000}
	got.FinishedAt = &fin
	got.ConsumedSeq = 3
	got.SessionID = "sess"
	ok(t, s.UpdateRun(ctx, got))
	got, err = s.GetRun(ctx, r.ID)
	ok(t, err)
	eq(t, got.State, core.RunSucceeded, "state")
	eq(t, got.Usage.CostUSD, 0.5, "usage")
	eq(t, got.ConsumedSeq, int64(3), "consumed seq")
	timeEq(t, *got.FinishedAt, fin, "finished_at")

	isErr(t, s.UpdateRun(ctx, &core.Run{ID: uid("run")}), core.ErrNotFound)
	isErr(t, s.CreateRun(ctx, &core.Run{ID: r.ID, FactoryID: f.ID}), core.ErrConflict)
	_, err = s.GetRun(ctx, uid("run"))
	isErr(t, err, core.ErrNotFound)

	cases := []struct {
		name string
		f    core.RunFilter
		want int
	}{
		{"team", core.RunFilter{TeamID: tm.ID}, 3},
		{"factory", core.RunFilter{FactoryID: f.ID}, 2},
		{"work item", core.RunFilter{WorkItemID: "wi1", TeamID: tm.ID}, 2},
		{"parent", core.RunFilter{ParentRunID: r.ID}, 1},
		{"agent", core.RunFilter{TeamID: tm.ID, AgentName: "implementer"}, 1},
		{"name", core.RunFilter{TeamID: tm.ID, Name: "other"}, 1},
		{"states", core.RunFilter{TeamID: tm.ID, States: []core.RunState{core.RunSucceeded, core.RunRunning}}, 3},
		{"state", core.RunFilter{TeamID: tm.ID, States: []core.RunState{core.RunRunning}}, 1},
	}
	for _, c := range cases {
		list, err := s.ListRuns(ctx, c.f)
		ok(t, err)
		eq(t, len(list), c.want, "filter "+c.name)
	}
}

func testRunPagination(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	var want []string
	for i := 0; i < 9; i++ {
		r := &core.Run{FactoryID: anyFactory, TeamID: tm.ID, CreatedAt: base.Add(time.Duration(i) * time.Millisecond)}
		ok(t, s.CreateRun(ctx, r))
		want = append([]string{r.ID}, want...)
	}
	var got []string
	cursor := ""
	for i := 0; i < 10; i++ {
		page, err := s.ListRuns(ctx, core.RunFilter{TeamID: tm.ID, ListOpts: core.ListOpts{Limit: 4, Cursor: cursor}})
		ok(t, err)
		got = append(got, runIDs(page)...)
		if len(page) < 4 {
			break
		}
		cursor = storeutil.EncodeCursor(page[3].CreatedAt, page[3].ID)
	}
	eq(t, strings.Join(got, ","), strings.Join(want, ","), "run pages")
	// Ascending: the oldest first, with the cursor of the last item.
	got, cursor = nil, ""
	for i := 0; i < 10; i++ {
		page, err := s.ListRuns(ctx, core.RunFilter{TeamID: tm.ID, ListOpts: core.ListOpts{Limit: 4, Cursor: cursor, Ascending: true}})
		ok(t, err)
		got = append(got, runIDs(page)...)
		if len(page) < 4 {
			break
		}
		cursor = storeutil.EncodeCursor(page[3].CreatedAt, page[3].ID)
	}
	slices.Reverse(want)
	eq(t, strings.Join(got, ","), strings.Join(want, ","), "ascending run pages")
	since := base.Add(5 * time.Millisecond)
	list, err := s.ListRuns(ctx, core.RunFilter{TeamID: tm.ID, Since: &since})
	ok(t, err)
	eq(t, len(list), 4, "since")
}

func testRunMessages(t *testing.T, s core.Store) {
	r := &core.Run{FactoryID: anyFactory, TeamID: uid("team")}
	ok(t, s.CreateRun(ctx, r))
	other := &core.Run{FactoryID: anyFactory, TeamID: r.TeamID}
	ok(t, s.CreateRun(ctx, other))
	roles := []string{"user", "assistant", "tool_call", "tool_result", "assistant"}
	for i, role := range roles {
		m := &core.RunMessage{RunID: r.ID, Role: role, Content: fmt.Sprintf("m%d", i)}
		if role == "tool_call" {
			m.Data = json.RawMessage(`{"name":"run_agents","args":{"n":1}}`)
		}
		ok(t, s.AppendRunMessage(ctx, m))
		eq(t, m.Seq, int64(i+1), "assigned seq")
		if m.ID == "" || m.CreatedAt.IsZero() {
			t.Fatal("message id/created_at must be defaulted")
		}
	}
	m := &core.RunMessage{RunID: other.ID, Role: "user", Content: "x"}
	ok(t, s.AppendRunMessage(ctx, m))
	eq(t, m.Seq, int64(1), "seq is per run")

	list, err := s.ListRunMessages(ctx, r.ID, 0)
	ok(t, err)
	eq(t, len(list), 5, "messages")
	for i, x := range list {
		eq(t, x.Seq, int64(i+1), "ordered by seq")
		eq(t, x.Role, roles[i], "role")
	}
	jsonEq(t, list[2].Data, json.RawMessage(`{"name":"run_agents","args":{"n":1}}`), "message data")
	list, err = s.ListRunMessages(ctx, r.ID, 3)
	ok(t, err)
	eq(t, len(list), 2, "after seq")
	eq(t, list[0].Seq, int64(4), "first after seq")

	// Updating the run must not reset the sequence.
	got, err := s.GetRun(ctx, r.ID)
	ok(t, err)
	got.State = core.RunRunning
	ok(t, s.UpdateRun(ctx, got))
	m = &core.RunMessage{RunID: r.ID, Role: "user", Content: "follow-up"}
	ok(t, s.AppendRunMessage(ctx, m))
	eq(t, m.Seq, int64(6), "seq continues after UpdateRun")

	isErr(t, s.AppendRunMessage(ctx, &core.RunMessage{RunID: uid("run"), Role: "user"}), core.ErrNotFound)
	list, err = s.ListRunMessages(ctx, uid("run"), 0)
	ok(t, err)
	eq(t, len(list), 0, "unknown run has no messages")
}

func testRunMessagesConcurrent(t *testing.T, s core.Store) {
	r := &core.Run{FactoryID: anyFactory, TeamID: uid("team")}
	ok(t, s.CreateRun(ctx, r))
	const workers, per = 8, 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*per)
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := s.AppendRunMessage(ctx, &core.RunMessage{RunID: r.ID, Role: "log", Content: fmt.Sprintf("%d-%d", g, i)}); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent append: %v", err)
	}
	list, err := s.ListRunMessages(ctx, r.ID, 0)
	ok(t, err)
	eq(t, len(list), workers*per, "messages")
	for i, m := range list {
		eq(t, m.Seq, int64(i+1), "gapless monotonic seq")
	}
}

// ---------- inbox ----------

func testInbox(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	a := &core.InboxItem{FactoryID: f.ID, Kind: core.InboxApproval, Title: "Approve plan", Options: []string{"approve", "reject"}, CreatedAt: base}
	b := &core.InboxItem{FactoryID: f.ID, Kind: core.InboxQuestion, Title: "Which db?", CreatedAt: base.Add(time.Second)}
	ok(t, s.CreateInboxItem(ctx, a))
	ok(t, s.CreateInboxItem(ctx, b))
	list, err := s.ListInboxItems(ctx, f.ID, false)
	ok(t, err)
	eq(t, len(list), 2, "open items")
	eq(t, list[0].ID, b.ID, "newest first")
	eq(t, len(list[1].Options), 2, "options")

	at := base.Add(time.Minute)
	a.Resolved, a.Resolution, a.ResolvedBy, a.ResolvedAt = true, "approve", "u1", &at
	ok(t, s.UpdateInboxItem(ctx, a))
	list, err = s.ListInboxItems(ctx, f.ID, false)
	ok(t, err)
	eq(t, len(list), 1, "unresolved only")
	list, err = s.ListInboxItems(ctx, f.ID, true)
	ok(t, err)
	eq(t, len(list), 2, "include resolved")
	got, err := s.GetInboxItem(ctx, a.ID)
	ok(t, err)
	eq(t, got.Resolution, "approve", "resolution")
	isErr(t, s.UpdateInboxItem(ctx, &core.InboxItem{ID: uid("inbox")}), core.ErrNotFound)
	_, err = s.GetInboxItem(ctx, uid("inbox"))
	isErr(t, err, core.ErrNotFound)
}

// ---------- secrets ----------

func testSecrets(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	team := &core.Secret{TeamID: tm.ID, Name: "API_TOKEN", Type: core.SecretGeneric}
	ok(t, s.PutSecret(ctx, team, []byte("team-ct")))
	if team.ID == "" || team.CreatedAt.IsZero() {
		t.Fatal("secret id/created_at must be defaulted")
	}
	onlyTeam := &core.Secret{TeamID: tm.ID, Name: "ONLY_TEAM"}
	ok(t, s.PutSecret(ctx, onlyTeam, []byte{0, 1, 2, 255}))

	// Factory scope falls back to team scope.
	sec, ct, err := s.GetSecret(ctx, tm.ID, f.ID, "ONLY_TEAM")
	ok(t, err)
	eq(t, sec.ID, onlyTeam.ID, "fallback id")
	if !bytes.Equal(ct, []byte{0, 1, 2, 255}) {
		t.Fatalf("binary ciphertext mangled: %v", ct)
	}
	eq(t, sec.Type, core.SecretGeneric, "default type")

	fac := &core.Secret{TeamID: tm.ID, FactoryID: f.ID, Name: "API_TOKEN", Type: core.SecretAnthropicKey}
	ok(t, s.PutSecret(ctx, fac, []byte("factory-ct")))
	_, ct, err = s.GetSecret(ctx, tm.ID, f.ID, "API_TOKEN")
	ok(t, err)
	eq(t, string(ct), "factory-ct", "factory scope overrides team")
	_, ct, err = s.GetSecret(ctx, tm.ID, "", "API_TOKEN")
	ok(t, err)
	eq(t, string(ct), "team-ct", "team scope")

	// Put again updates in place.
	upd := &core.Secret{TeamID: tm.ID, Name: "API_TOKEN", Type: core.SecretOpenAIKey}
	time.Sleep(2 * time.Millisecond)
	ok(t, s.PutSecret(ctx, upd, []byte("team-ct-2")))
	eq(t, upd.ID, team.ID, "id preserved on overwrite")
	timeEq(t, upd.CreatedAt, team.CreatedAt, "created_at preserved")
	sec, ct, err = s.GetSecret(ctx, tm.ID, "", "API_TOKEN")
	ok(t, err)
	eq(t, string(ct), "team-ct-2", "overwritten")
	eq(t, sec.Type, core.SecretOpenAIKey, "type updated")
	if !sec.UpdatedAt.After(sec.CreatedAt) {
		t.Fatal("UpdatedAt must advance on overwrite")
	}

	list, err := s.ListSecrets(ctx, tm.ID, "")
	ok(t, err)
	eq(t, len(list), 2, "team secrets")
	eq(t, list[0].Name, "API_TOKEN", "sorted by name")
	list, err = s.ListSecrets(ctx, tm.ID, f.ID)
	ok(t, err)
	eq(t, len(list), 1, "factory secrets")

	_, _, err = s.GetSecret(ctx, tm.ID, f.ID, "MISSING")
	isErr(t, err, core.ErrNotFound)
	_, _, err = s.GetSecret(ctx, uid("team"), "", "API_TOKEN")
	isErr(t, err, core.ErrNotFound)

	ok(t, s.DeleteSecret(ctx, fac.ID))
	isErr(t, s.DeleteSecret(ctx, fac.ID), core.ErrNotFound)
	_, ct, err = s.GetSecret(ctx, tm.ID, f.ID, "API_TOKEN")
	ok(t, err)
	eq(t, string(ct), "team-ct-2", "falls back after factory secret deleted")
	isErr(t, s.PutSecret(ctx, &core.Secret{TeamID: tm.ID}, nil), core.ErrInvalid)
}

// ---------- webhook deliveries ----------

func testWebhookDeliveries(t *testing.T, s core.Store) {
	wh := uid("wh")
	body := json.RawMessage(`{"event":{"type":"incident.triggered"},"n":[1,2]}`)
	d := &core.WebhookDelivery{WebhookID: wh, DeliveryID: "abc", Status: "accepted", Headers: map[string]string{"X-Id": "abc"}, Body: body, ReceivedAt: base}
	ok(t, s.CreateWebhookDelivery(ctx, d))
	isErr(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: wh, DeliveryID: "abc", Status: "accepted"}), core.ErrConflict)
	ok(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: uid("wh"), DeliveryID: "abc", Status: "accepted"}))
	for i := 0; i < 3; i++ {
		// Empty delivery ids never collide.
		ok(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: wh, Status: "rejected", Reason: "bad signature", ReceivedAt: base.Add(time.Duration(i+1) * time.Second)}))
	}
	got, err := s.FindWebhookDelivery(ctx, wh, "abc")
	ok(t, err)
	eq(t, got.ID, d.ID, "find")
	jsonEq(t, got.Body, body, "body")
	eq(t, got.Headers["X-Id"], "abc", "headers")
	_, err = s.FindWebhookDelivery(ctx, wh, "zzz")
	isErr(t, err, core.ErrNotFound)
	_, err = s.FindWebhookDelivery(ctx, wh, "")
	isErr(t, err, core.ErrNotFound)

	d.RunIDs = []string{"run_1", "run_2"}
	ok(t, s.UpdateWebhookDelivery(ctx, d))
	got, err = s.GetWebhookDelivery(ctx, d.ID)
	ok(t, err)
	eq(t, len(got.RunIDs), 2, "run ids")
	isErr(t, s.UpdateWebhookDelivery(ctx, &core.WebhookDelivery{ID: uid("whd"), WebhookID: wh}), core.ErrNotFound)
	_, err = s.GetWebhookDelivery(ctx, uid("whd"))
	isErr(t, err, core.ErrNotFound)

	list, err := s.ListWebhookDeliveries(ctx, wh, 2)
	ok(t, err)
	eq(t, len(list), 2, "limit")
	eq(t, list[0].ReceivedAt.Equal(base.Add(3*time.Second)), true, "newest first")
	list, err = s.ListWebhookDeliveries(ctx, wh, 0)
	ok(t, err)
	eq(t, len(list), 4, "default limit")

	// Repeated deliveries are logged with the delivery ID of the accepted
	// entry; a second accepted entry still conflicts.
	for i := 0; i < 2; i++ {
		ok(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: wh, DeliveryID: "abc", Status: core.WebhookDeliveryDeduplicated,
			Reason: "duplicate", ReceivedAt: base.Add(time.Duration(10+i) * time.Second)}))
	}
	isErr(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: wh, DeliveryID: "abc", Status: "accepted", ReceivedAt: base.Add(20 * time.Second)}), core.ErrConflict)
	got, err = s.FindWebhookDelivery(ctx, wh, "abc")
	ok(t, err)
	eq(t, got.Status, core.WebhookDeliveryDeduplicated, "find returns the newest entry")
	list, err = s.ListWebhookDeliveries(ctx, wh, 0)
	ok(t, err)
	eq(t, len(list), 6, "duplicates are listed")
}

// ---------- integrations ----------

func testIntegrations(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	i := &core.Integration{TeamID: tm.ID, Provider: "slack", Name: "Acme Slack", Config: map[string]string{"workspace": "T1"}, SecretRef: "SLACK_TOKEN"}
	ok(t, s.UpsertIntegration(ctx, i))
	if i.ID == "" || i.CreatedAt.IsZero() {
		t.Fatal("integration id/created_at must be defaulted")
	}
	created := i.CreatedAt
	upd := &core.Integration{ID: i.ID, TeamID: tm.ID, Provider: "slack", Name: "Renamed", Config: map[string]string{"workspace": "T2"}}
	ok(t, s.UpsertIntegration(ctx, upd))
	timeEq(t, upd.CreatedAt, created, "created_at preserved on upsert")
	got, err := s.GetIntegration(ctx, i.ID)
	ok(t, err)
	eq(t, got.Name, "Renamed", "name")
	eq(t, got.Config["workspace"], "T2", "config")
	ok(t, s.UpsertIntegration(ctx, &core.Integration{TeamID: tm.ID, Provider: "linear", Name: "Linear"}))
	list, err := s.ListIntegrations(ctx, tm.ID)
	ok(t, err)
	eq(t, len(list), 2, "integrations")
	eq(t, list[0].ID, i.ID, "oldest first")
	ok(t, s.DeleteIntegration(ctx, i.ID))
	isErr(t, s.DeleteIntegration(ctx, i.ID), core.ErrNotFound)
	_, err = s.GetIntegration(ctx, i.ID)
	isErr(t, err, core.ErrNotFound)
}

func testFactoryIntegrations(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	got, err := s.GetFactoryIntegrations(ctx, f.ID)
	ok(t, err)
	eq(t, len(got), 0, "empty")
	ok(t, s.SetFactoryIntegrations(ctx, f.ID, []string{"slack", "github"}, map[string]map[string]string{"slack": {"channel": "C1"}}))
	got, err = s.GetFactoryIntegrations(ctx, f.ID)
	ok(t, err)
	eq(t, len(got), 2, "providers")
	eq(t, got["slack"]["channel"], "C1", "settings")
	if got["github"] == nil {
		t.Fatal("providers without settings get an empty map")
	}
	ok(t, s.SetFactoryIntegrations(ctx, f.ID, []string{"linear"}, nil))
	got, err = s.GetFactoryIntegrations(ctx, f.ID)
	ok(t, err)
	eq(t, len(got), 1, "replaced")
	if _, ok := got["linear"]; !ok {
		t.Fatal("linear missing")
	}
	isErr(t, s.SetFactoryIntegrations(ctx, uid("fac"), []string{"slack"}, nil), core.ErrNotFound)
}

// ---------- scoring ----------

func testScores(t *testing.T, s core.Store) {
	tm := mkTeam(t, s)
	f := mkFactory(t, s, tm.ID)
	scorer := core.Scorer{ID: uid("scr"), Slug: "q"}
	ok(t, s.ReplaceFactoryResources(ctx, f.ID, nil, nil, []core.Scorer{scorer}, nil))
	run := &core.Run{TeamID: tm.ID, FactoryID: f.ID}
	ok(t, s.CreateRun(ctx, run))

	a := &core.Score{ScorerID: scorer.ID, RunID: run.ID, AgentName: "foreman", Label: "good", Score: 1, Passed: true, CreatedAt: base}
	b := &core.Score{ScorerID: uid("scr-gone"), RunID: run.ID, Label: "bad", CreatedAt: base.Add(time.Hour)}
	c := &core.Score{ScorerID: uid("scr-other"), RunID: uid("run"), Label: "orphan", CreatedAt: base}
	for _, x := range []*core.Score{a, b, c} {
		ok(t, s.CreateScore(ctx, x))
	}
	list, err := s.ListScores(ctx, f.ID, "", nil)
	ok(t, err)
	eq(t, len(list), 2, "scores by factory (via scorer and via run)")
	eq(t, list[0].ID, b.ID, "newest first")
	list, err = s.ListScores(ctx, f.ID, scorer.ID, nil)
	ok(t, err)
	eq(t, len(list), 1, "by scorer")
	since := base.Add(time.Minute)
	list, err = s.ListScores(ctx, f.ID, "", &since)
	ok(t, err)
	eq(t, len(list), 1, "since")

	a.ImprovementRunID = "run_imp"
	ok(t, s.UpdateScore(ctx, a))
	list, err = s.ListScores(ctx, f.ID, scorer.ID, nil)
	ok(t, err)
	eq(t, list[0].ImprovementRunID, "run_imp", "updated")
	isErr(t, s.UpdateScore(ctx, &core.Score{ID: uid("score")}), core.ErrNotFound)
	isErr(t, s.CreateScore(ctx, &core.Score{ID: a.ID}), core.ErrConflict)

	got, err := s.GetScore(ctx, scorer.ID, run.ID)
	ok(t, err)
	eq(t, got.ID, a.ID, "get score")
	eq(t, got.ImprovementRunID, "run_imp", "get score data")
	_, err = s.GetScore(ctx, scorer.ID, uid("run"))
	isErr(t, err, core.ErrNotFound)
}

func testBenchmarkRuns(t *testing.T, s core.Store) {
	fid := uid("fac")
	b := &core.BenchmarkRun{FactoryID: fid, Suite: "smoke", State: "running", CreatedAt: base}
	ok(t, s.CreateBenchmarkRun(ctx, b))
	ok(t, s.CreateBenchmarkRun(ctx, &core.BenchmarkRun{FactoryID: fid, Suite: "full", State: "running", CreatedAt: base.Add(time.Second)}))
	fin := base.Add(time.Hour)
	b.State, b.FinishedAt, b.Results = "complete", &fin, json.RawMessage(`{"pass":3}`)
	ok(t, s.UpdateBenchmarkRun(ctx, b))
	got, err := s.GetBenchmarkRun(ctx, b.ID)
	ok(t, err)
	eq(t, got.State, "complete", "state")
	jsonEq(t, got.Results, json.RawMessage(`{"pass":3}`), "results")
	list, err := s.ListBenchmarkRuns(ctx, fid)
	ok(t, err)
	eq(t, len(list), 2, "list")
	eq(t, list[0].Suite, "full", "newest first")
	isErr(t, s.UpdateBenchmarkRun(ctx, &core.BenchmarkRun{ID: uid("bench")}), core.ErrNotFound)
	_, err = s.GetBenchmarkRun(ctx, uid("bench"))
	isErr(t, err, core.ErrNotFound)
}

// ---------- workers & environments ----------

func testWorkers(t *testing.T, s core.Store) {
	tm := uid("team")
	w := &core.Worker{TeamID: tm, Name: "b-worker", Backend: "docker", Capacity: 4, Labels: map[string]string{"gpu": "no"}, LastHeartbeat: base}
	ok(t, s.UpsertWorker(ctx, w))
	w.Active, w.LastHeartbeat = 2, base.Add(time.Minute)
	ok(t, s.UpsertWorker(ctx, w))
	ok(t, s.UpsertWorker(ctx, &core.Worker{ID: uid("wkr"), TeamID: tm, Name: "a-worker", Backend: "direct"}))
	got, err := s.GetWorker(ctx, w.ID)
	ok(t, err)
	eq(t, got.Active, 2, "upserted")
	timeEq(t, got.LastHeartbeat, base.Add(time.Minute), "heartbeat")
	list, err := s.ListWorkers(ctx, tm)
	ok(t, err)
	eq(t, len(list), 2, "workers")
	eq(t, list[0].Name, "a-worker", "sorted by name")
	_, err = s.GetWorker(ctx, uid("wkr"))
	isErr(t, err, core.ErrNotFound)
}

func testEnvironments(t *testing.T, s core.Store) {
	tm := uid("team")
	e := &core.Environment{TeamID: tm, Name: "web", Repos: []string{"github:acme/web"}, EnvVars: map[string]string{"A": "1"}}
	ok(t, s.CreateEnvironment(ctx, e))
	isErr(t, s.CreateEnvironment(ctx, &core.Environment{ID: e.ID, TeamID: tm}), core.ErrConflict)
	e.SetupCommands = []string{"make deps"}
	ok(t, s.UpdateEnvironment(ctx, e))
	got, err := s.GetEnvironment(ctx, e.ID)
	ok(t, err)
	eq(t, got.SetupCommands[0], "make deps", "updated")
	eq(t, got.EnvVars["A"], "1", "env vars")
	ok(t, s.CreateEnvironment(ctx, &core.Environment{TeamID: tm, Name: "api"}))
	list, err := s.ListEnvironments(ctx, tm)
	ok(t, err)
	eq(t, len(list), 2, "environments")
	eq(t, list[0].Name, "api", "sorted")
	ok(t, s.DeleteEnvironment(ctx, e.ID))
	isErr(t, s.DeleteEnvironment(ctx, e.ID), core.ErrNotFound)
	isErr(t, s.UpdateEnvironment(ctx, e), core.ErrNotFound)
	_, err = s.GetEnvironment(ctx, e.ID)
	isErr(t, err, core.ErrNotFound)
}

func testMCPServers(t *testing.T, s core.Store) {
	tm := uid("team")
	m := &core.RegisteredMCPServer{TeamID: tm, Name: "sentry", URL: "https://mcp.example.test/mcp",
		Headers: map[string]string{"Authorization": "Bearer {{SENTRY_TOKEN}}"}, Connection: core.MCPConnectionProxy,
		OAuth: &core.MCPOAuthConfig{Scopes: []string{"read"}, Status: core.MCPOAuthNotConnected}, CreatedAt: base, UpdatedAt: base}
	ok(t, s.CreateMCPServer(ctx, m))
	if m.ID == "" {
		t.Fatal("CreateMCPServer did not assign an id")
	}
	isErr(t, s.CreateMCPServer(ctx, &core.RegisteredMCPServer{TeamID: tm, Name: "sentry", Command: "x"}), core.ErrConflict)
	ok(t, s.CreateMCPServer(ctx, &core.RegisteredMCPServer{TeamID: uid("team"), Name: "sentry", Command: "x"}))
	other := &core.RegisteredMCPServer{TeamID: tm, Name: "docs", Command: "npx", Args: []string{"-y", "docs-mcp"}}
	ok(t, s.CreateMCPServer(ctx, other))
	other.Name = "sentry"
	isErr(t, s.UpdateMCPServer(ctx, other), core.ErrConflict)
	m.OAuth.Status = core.MCPOAuthConnected
	ok(t, s.UpdateMCPServer(ctx, m))
	got, err := s.GetMCPServer(ctx, m.ID)
	ok(t, err)
	eq(t, got.OAuth.Status, core.MCPOAuthConnected, "updated")
	eq(t, got.Headers["Authorization"], "Bearer {{SENTRY_TOKEN}}", "headers")
	eq(t, got.Connection, core.MCPConnectionProxy, "connection")
	list, err := s.ListMCPServers(ctx, tm)
	ok(t, err)
	eq(t, len(list), 2, "team servers")
	eq(t, list[0].Name, "docs", "sorted by name")
	ok(t, s.DeleteMCPServer(ctx, m.ID))
	isErr(t, s.DeleteMCPServer(ctx, m.ID), core.ErrNotFound)
	isErr(t, s.UpdateMCPServer(ctx, m), core.ErrNotFound)
	_, err = s.GetMCPServer(ctx, m.ID)
	isErr(t, err, core.ErrNotFound)
}

func testKV(t *testing.T, s core.Store) {
	k := uid("cursor")
	_, err := s.KVGet(ctx, k)
	isErr(t, err, core.ErrNotFound)
	ok(t, s.KVSet(ctx, k, "1"))
	ok(t, s.KVSet(ctx, k, "2"))
	v, err := s.KVGet(ctx, k)
	ok(t, err)
	eq(t, v, "2", "overwrite")
	ok(t, s.KVSet(ctx, k, ""))
	v, err = s.KVGet(ctx, k)
	ok(t, err)
	eq(t, v, "", "empty value is stored")
}

// ---------- retention ----------

func testPrune(t *testing.T, s core.Store) {
	p, isPruner := s.(storeutil.Pruner)
	if !isPruner {
		t.Skip("store does not implement retention")
	}
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now()
	oldRun := &core.Run{FactoryID: anyFactory, TeamID: uid("team"), State: core.RunSucceeded, FinishedAt: &old}
	liveRun := &core.Run{FactoryID: anyFactory, TeamID: oldRun.TeamID, State: core.RunIdle}
	newRun := &core.Run{FactoryID: anyFactory, TeamID: oldRun.TeamID, State: core.RunFailed, FinishedAt: &recent}
	for _, r := range []*core.Run{oldRun, liveRun, newRun} {
		ok(t, s.CreateRun(ctx, r))
		ok(t, s.AppendRunMessage(ctx, &core.RunMessage{RunID: r.ID, Role: "user", Content: "hi"}))
	}
	wh := uid("wh")
	ok(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: wh, Status: "accepted", ReceivedAt: old}))
	ok(t, s.CreateWebhookDelivery(ctx, &core.WebhookDelivery{WebhookID: wh, Status: "accepted", ReceivedAt: recent}))
	fid := uid("fac")
	ok(t, s.CreateInboxItem(ctx, &core.InboxItem{FactoryID: fid, Kind: core.InboxQuestion, Resolved: true, ResolvedAt: &old}))
	ok(t, s.CreateInboxItem(ctx, &core.InboxItem{FactoryID: fid, Kind: core.InboxQuestion}))

	n, err := p.PruneBefore(ctx, time.Now().Add(-24*time.Hour))
	ok(t, err)
	if n < 4 {
		t.Fatalf("pruned %d rows, want >= 4", n)
	}
	_, err = s.GetRun(ctx, oldRun.ID)
	isErr(t, err, core.ErrNotFound)
	msgs, err := s.ListRunMessages(ctx, oldRun.ID, 0)
	ok(t, err)
	eq(t, len(msgs), 0, "pruned messages")
	for _, id := range []string{liveRun.ID, newRun.ID} {
		_, err = s.GetRun(ctx, id)
		ok(t, err)
	}
	ds, err := s.ListWebhookDeliveries(ctx, wh, 10)
	ok(t, err)
	eq(t, len(ds), 1, "deliveries kept")
	items, err := s.ListInboxItems(ctx, fid, true)
	ok(t, err)
	eq(t, len(items), 1, "inbox kept")
}

// ---------- fidelity ----------

func testJSONFidelity(t *testing.T, s core.Store) {
	r := &core.Run{FactoryID: anyFactory, TeamID: uid("team")}
	ok(t, s.CreateRun(ctx, r))
	contents := []string{
		"plain",
		"unicode ✓ — 日本語",
		`quotes "and" \backslashes\ and \u0000 literal`,
		"tab\tnewline\n",
		"nul\x00byte",
	}
	for _, c := range contents {
		ok(t, s.AppendRunMessage(ctx, &core.RunMessage{RunID: r.ID, Role: "tool_result", Content: c}))
	}
	msgs, err := s.ListRunMessages(ctx, r.ID, 0)
	ok(t, err)
	eq(t, len(msgs), len(contents), "messages")
	for i, c := range contents {
		got := msgs[i].Content
		if strings.Contains(c, "\x00") {
			// PostgreSQL jsonb cannot hold NUL; it is replaced with U+FFFD there.
			if got != c && got != strings.ReplaceAll(c, "\x00", "\uFFFD") {
				t.Fatalf("content %d: got %q", i, got)
			}
			continue
		}
		eq(t, got, c, fmt.Sprintf("content %d", i))
	}

	// Maps and nested slices survive.
	ev := &core.RunMessage{RunID: r.ID, Role: "log", Data: json.RawMessage(`{"z":1,"a":[true,null,"x"],"nested":{"k":"v"}}`)}
	ok(t, s.AppendRunMessage(ctx, ev))
	msgs, err = s.ListRunMessages(ctx, r.ID, int64(len(contents)))
	ok(t, err)
	jsonEq(t, msgs[0].Data, ev.Data, "raw data")
}
