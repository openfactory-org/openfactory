package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// flakyStore fails CreateRun one time, at call number failAt (1-based).
type flakyStore struct {
	*testStore
	mu     sync.Mutex
	calls  int
	failAt int
}

var errTransient = errors.New("database is busy")

func (s *flakyStore) CreateRun(ctx context.Context, r *core.Run) error {
	s.mu.Lock()
	s.calls++
	fail := s.calls == s.failAt
	s.mu.Unlock()
	if fail {
		return errTransient
	}
	return s.testStore.CreateRun(ctx, r)
}

// dispatchCount returns the number of rows of the dispatch ledger.
func dispatchCount(t *testing.T, st *testStore) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM dispatches").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// newIntakeEnv builds an engine on st with the default test definition.
func newIntakeEnv(t *testing.T, st core.Store, fs *testStore, clock func() time.Time, modify ...func(*core.CompiledDefinition)) *TestEnv {
	t.Helper()
	ctx := context.Background()
	def := DefaultTestDefinition()
	for _, m := range modify {
		m(def)
	}
	f := &core.Factory{ID: "fac_1", TeamID: "team1", Name: "payments", Source: core.DefinitionManaged, CreatedAt: time.Now()}
	if err := fs.CreateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	bus := NewFakeBus()
	eng := New(Options{Store: st, Bus: bus, Compile: CompilerFor(def), Match: SimpleMatch, Forges: FakeForges{}, Logger: DiscardLogger(), Clock: clock})
	if _, err := eng.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok"}, "tester", "initial", ""); err != nil {
		t.Fatalf("apply definition: %v", err)
	}
	f, _ = fs.GetFactory(ctx, f.ID)
	return &TestEnv{Store: fs, Bus: bus, Engine: eng, Factory: f, Def: def}
}

// twoAgents adds a second automation for the same event with the triage agent.
func twoAgents(d *core.CompiledDefinition) {
	d.Automations = append(d.Automations, core.Automation{
		Name: "issue-triage", Enabled: true, Agent: "triage", Prompt: "Triage this issue.",
		Triggers: []core.Trigger{{Provider: "github", Event: "issue_labeled"}},
	})
}

func runsOfWorkItem(t *testing.T, st *testStore, wiID string) []core.Run {
	t.Helper()
	runs, err := st.ListRuns(context.Background(), core.RunFilter{WorkItemID: wiID})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// A transient failure after the first dispatch returns an error, so the
// job is retried. The retry starts the missing run and not the first again.
func TestHandleEventRetryAfterTransientFailure(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		fs := newTestStore(t)
		st := &flakyStore{testStore: fs, failAt: failAt}
		env := newIntakeEnv(t, st, fs, nil, twoAgents)
		ctx := context.Background()

		ev := IssueEvent("issue_labeled", "Invoices omit the PO number", "alice")
		ev.DeliveryID = "d-retry:issue_labeled"
		payload, _ := json.Marshal(ev)

		// The durable consumer gets the job, and the first attempt fails.
		job := core.Job{ID: "job1", Payload: payload}
		if err := env.Engine.handleInboundJob(ctx, job); !errors.Is(err, errTransient) {
			t.Fatalf("failAt=%d: first attempt err = %v, want the transient error for a retry", failAt, err)
		}
		// The retry delivers the same job again.
		if err := env.Engine.handleInboundJob(ctx, job); err != nil {
			t.Fatalf("failAt=%d: retry err = %v", failAt, err)
		}
		// A third delivery changes nothing.
		if err := env.Engine.handleInboundJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		wi, err := fs.FindWorkItemBySource(ctx, env.Factory.ID, ev.Source.Key())
		if err != nil {
			t.Fatal(err)
		}
		runs := runsOfWorkItem(t, fs, wi.ID)
		agents := map[string]int{}
		for _, r := range runs {
			agents[r.AgentName]++
		}
		if len(runs) != 2 || agents["foreman"] != 1 || agents["triage"] != 1 {
			t.Fatalf("failAt=%d: runs = %v, want one foreman run and one triage run", failAt, agents)
		}
		if msgs := fs.MessagesOf(wi.RootRunID); len(msgs) != 1 {
			t.Fatalf("failAt=%d: root messages = %d, want 1", failAt, len(msgs))
		}
	}
}

// Dispatch records expire, so their number stays stable.
func TestDispatchRecordsStayStable(t *testing.T) {
	// A long test with its own store and engine: it runs beside the other long one.
	t.Parallel()
	fs := newTestStore(t)
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	env := newIntakeEnv(t, fs, fs, clock)
	ctx := context.Background()

	maxRows := 0
	for i := 0; i < 24*30; i++ { // one event each hour for 30 days
		ev := IssueEvent("issue_labeled", "report", "alice")
		ev.Source.Number = 1000 + i
		ev.DeliveryID = "d" + itoa(i)
		if _, err := env.Engine.HandleEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if i%24 == 0 {
			if _, err := env.Engine.SweepDispatches(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if n := dispatchCount(t, fs); n > maxRows {
			maxRows = n
		}
		mu.Lock()
		now = now.Add(time.Hour)
		mu.Unlock()
	}
	// Two records for each event (automation and work item), kept 7 days,
	// plus up to one day between sweeps.
	if limit := 2 * 24 * 8; maxRows > limit {
		t.Fatalf("dispatch records grew to %d, want at most %d", maxRows, limit)
	}
	// No marker in the key-value space.
	for _, k := range fs.kvKeys() {
		if strings.HasPrefix(k, "engine:delivery:") || strings.HasPrefix(k, "engine:dispatch:") {
			t.Fatalf("unexpected key %s", k)
		}
	}
}

// One comment with a mention gives the foreman exactly one message.
func TestOneActionOneMessage(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	runs, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_labeled", "Invoices omit the PO number", "alice"))
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	root := runs[0]
	before := len(env.Store.MessagesOf(root.ID))

	// GitHub: a comment event and a mention event of one delivery.
	for _, typ := range []string{"issue_commented", "issue_mentioned"} {
		ev := IssueEvent(typ, "@payments please also cover credit notes", "bob")
		ev.DeliveryID = "gh-1:" + typ
		ev.ActionKey = "github:gh-1"
		ev.Attributes["alias"] = "payments"
		if _, err := env.Engine.HandleEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(env.Store.MessagesOf(root.ID)) - before; got != 1 {
		t.Fatalf("GitHub: foreman got %d messages, want 1", got)
	}

	// Slack: a mention and the channel message of one post (no ActionKey set:
	// the engine derives it from the payload).
	src := core.SourceRef{Provider: "slack", Kind: "thread", Channel: "C1", ThreadID: "100.1"}
	_, sroot, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "start", Source: src})
	if err != nil {
		t.Fatal(err)
	}
	before = len(env.Store.MessagesOf(sroot.ID))
	for i, typ := range []string{"app_mention", "message_posted"} {
		ev := &core.Event{
			TeamID: "team1", Provider: "slack", Type: typ, DeliveryID: "Ev" + itoa(i),
			Actor:      core.Actor{Provider: "slack", ExternalID: "U1", Handle: "carol", UserID: "usr_carol"},
			Source:     src,
			Attributes: map[string]any{"alias": "payments"},
			Payload:    json.RawMessage(`{"channel":"C1","ts":"100.2","team":"T1"}`),
			Text:       "@payments any news?",
		}
		if _, err := env.Engine.HandleEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(env.Store.MessagesOf(sroot.ID)) - before; got != 1 {
		t.Fatalf("Slack: foreman got %d messages, want 1", got)
	}
}

// A match with a different agent starts a side run attached to the
// work item. A later action continues it. A side run does not own the stage.
func TestSideRuns(t *testing.T) {
	env := NewTestEnv(t, twoAgents)
	ctx := context.Background()
	ev := IssueEvent("issue_labeled", "Invoices omit the PO number", "alice")
	ev.DeliveryID = "a1"
	runs, err := env.Engine.HandleEvent(ctx, ev)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs=%v err=%v", runs, err)
	}
	wi, _ := env.Store.GetWorkItem(ctx, runs[0].WorkItemID)
	root, side := runs[0], runs[1]
	if wi.RootRunID != root.ID || root.AgentName != "foreman" || side.AgentName != "triage" || side.WorkItemID != wi.ID || side.ParentRunID != "" {
		t.Fatalf("root=%+v side=%+v wi=%+v", root, side, wi)
	}
	if !env.Engine.IsSideRun(ctx, side) || env.Engine.IsSideRun(ctx, root) {
		t.Fatal("IsSideRun is wrong")
	}

	// A later action: the root run gets one event message, and the side run
	// gets a message; no new run starts.
	rootBefore, sideBefore := len(env.Store.MessagesOf(root.ID)), len(env.Store.MessagesOf(side.ID))
	ev2 := IssueEvent("issue_labeled", "Invoices omit the PO number", "alice")
	ev2.DeliveryID = "a2"
	if _, err := env.Engine.HandleEvent(ctx, ev2); err != nil {
		t.Fatal(err)
	}
	if n := len(runsOfWorkItem(t, env.Store, wi.ID)); n != 2 {
		t.Fatalf("runs of work item = %d, want 2", n)
	}
	if len(env.Store.MessagesOf(root.ID)) != rootBefore+1 || len(env.Store.MessagesOf(side.ID)) != sideBefore+1 {
		t.Fatal("each run should get one message")
	}
}

// An issue from an outside contributor starts the triage automation on
// GitHub and on GitLab in the same way.
func TestOutsideContributorStartsTriageOnEachForge(t *testing.T) {
	env := NewTestEnv(t, func(d *core.CompiledDefinition) {
		d.Automations = []core.Automation{{
			Name: "triage", Enabled: true, Agent: "triage", Prompt: "Triage.",
			Triggers: []core.Trigger{{Provider: "github", Event: "issue_created"}, {Provider: "gitlab", Event: "issue_created"}},
		}}
	})
	ctx := context.Background()
	for i, provider := range []string{"github", "gitlab"} {
		ev := IssueEvent("issue_created", "Crash on save", "outsider")
		ev.Provider, ev.Actor.Provider, ev.Source.Provider = provider, provider, provider
		ev.Actor.UserID = ""
		ev.Source.Number = 50 + i
		runs, err := env.Engine.HandleEvent(ctx, ev)
		if err != nil || len(runs) != 1 || runs[0].AgentName != "triage" {
			t.Fatalf("%s: runs=%v err=%v", provider, runs, err)
		}
	}

	// requireLinkedActor drops actions from senders without a link.
	strict := NewTestEnv(t, func(d *core.CompiledDefinition) { d.RequireLinkedActor = true })
	ev := IssueEvent("issue_labeled", "x", "outsider")
	ev.Actor.UserID = ""
	if runs, err := strict.Engine.HandleEvent(ctx, ev); err != nil || len(runs) != 0 {
		t.Fatalf("requireLinkedActor: runs=%v err=%v", runs, err)
	}
	if runs, err := strict.Engine.HandleEvent(ctx, IssueEvent("issue_labeled", "x", "alice")); err != nil || len(runs) != 1 {
		t.Fatalf("requireLinkedActor member: runs=%v err=%v", runs, err)
	}
}

// A work item accepts at most 20 bot actions in one hour.
func TestBotActionLimit(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	runs, _ := env.Engine.HandleEvent(ctx, IssueEvent("issue_labeled", "x", "alice"))
	root := runs[0]
	before := len(env.Store.MessagesOf(root.ID))
	for i := 0; i < maxBotActions+5; i++ {
		ev := IssueEvent("issue_commented", "ping "+itoa(i), "other[bot]")
		ev.Actor.IsBot, ev.Actor.UserID = true, ""
		ev.DeliveryID = "bot" + itoa(i)
		if _, err := env.Engine.HandleEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(env.Store.MessagesOf(root.ID)) - before; got != maxBotActions {
		t.Fatalf("delivered %d bot messages, want %d", got, maxBotActions)
	}
	evs, _ := env.Store.ListWorkItemEvents(ctx, root.WorkItemID)
	n := 0
	for _, e := range evs {
		if e.Kind == "bot_limit" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("bot_limit timeline entries = %d, want 1", n)
	}
}

// Text from a sender without a link cannot resolve an approval or a
// question in any integration. A linked member can, and the timeline shows
// who and how the engine identified the person.
func TestSenderCheckBeforeContinuation(t *testing.T) {
	ctx := context.Background()

	t.Run("public GitHub comment", func(t *testing.T) {
		env := NewTestEnv(t)
		runs, _ := env.Engine.HandleEvent(ctx, IssueEvent("issue_labeled", "x", "alice"))
		root := runs[0]
		it, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxApproval, "Spec approval", "ok?", nil)
		if err != nil {
			t.Fatal(err)
		}
		c := IssueEvent("issue_commented", "lgtm", "drive-by")
		c.Actor.UserID = ""
		if _, err := env.Engine.HandleEvent(ctx, c); err != nil {
			t.Fatal(err)
		}
		got, _ := env.Store.GetInboxItem(ctx, it.ID)
		if got.Resolved {
			t.Fatal("an unlinked account resolved an approval")
		}
		msgs := env.Store.MessagesOf(root.ID)
		if last := msgs[len(msgs)-1]; !strings.Contains(last.Content, "not a trusted team member") || !strings.Contains(last.Content, "lgtm") {
			t.Fatalf("untrusted message = %q", last.Content)
		}
		// A linked member approves.
		m := IssueEvent("issue_commented", "lgtm", "maintainer")
		if _, err := env.Engine.HandleEvent(ctx, m); err != nil {
			t.Fatal(err)
		}
		got, _ = env.Store.GetInboxItem(ctx, it.ID)
		if !got.Resolved || !strings.HasPrefix(got.Resolution, "approved") {
			t.Fatalf("member approval not applied: %+v", got)
		}
		checks := timelineOf(t, env, root.WorkItemID, "sender_checked")
		if len(checks) != 2 {
			t.Fatalf("sender_checked entries = %d, want 2", len(checks))
		}
		if checks[0]["trust"] != core.TrustExternal || checks[0]["result"] != "delivered_untrusted" {
			t.Fatalf("first check = %v", checks[0])
		}
		if checks[1]["trust"] != core.TrustMember || checks[1]["result"] != "approval_approved" ||
			checks[1]["user_id"] != "usr_maintainer" || checks[1]["identified_by"] != "linked github account" {
			t.Fatalf("second check = %v", checks[1])
		}
	})

	t.Run("unlinked Slack reply", func(t *testing.T) {
		env := NewTestEnv(t)
		src := core.SourceRef{Provider: "slack", Kind: "thread", Channel: "C1", ThreadID: "9.1"}
		_, root, err := env.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: env.Factory.ID, Prompt: "start", Source: src})
		if err != nil {
			t.Fatal(err)
		}
		q, err := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Which currency?", "Which currency?", nil)
		if err != nil {
			t.Fatal(err)
		}
		reply := func(user, text string) *core.Event {
			return &core.Event{
				TeamID: "team1", Provider: "slack", Type: "message_posted",
				Actor:  core.Actor{Provider: "slack", ExternalID: "U-" + user, Handle: user, UserID: user},
				Source: src, Attributes: map[string]any{}, Text: text,
			}
		}
		if _, err := env.Engine.HandleEvent(ctx, reply("", "EUR")); err != nil {
			t.Fatal(err)
		}
		if got, _ := env.Store.GetInboxItem(ctx, q.ID); got.Resolved {
			t.Fatal("an unlinked Slack user answered a question")
		}
		if _, err := env.Engine.HandleEvent(ctx, reply("usr_carol", "USD")); err != nil {
			t.Fatal(err)
		}
		if got, _ := env.Store.GetInboxItem(ctx, q.ID); !got.Resolved || got.Resolution != "USD" {
			t.Fatalf("member answer not applied: %+v", got)
		}
	})

	t.Run("approvers and restrictFollowupsToStarter", func(t *testing.T) {
		env := NewTestEnv(t)
		runs, _ := env.Engine.HandleEvent(ctx, IssueEvent("issue_labeled", "x", "alice"))
		root := runs[0]
		rt, _ := LoadFactoryRuntime(ctx, env.Store, env.Factory.ID)
		rt.Approvers = []string{"lead@example.test"}
		if err := saveFactoryRuntime(ctx, env.Store, env.Factory.ID, rt); err != nil {
			t.Fatal(err)
		}
		env.Store.PutUser(&core.User{ID: "usr_dev", TeamID: "team1", Email: "dev@example.test"})
		env.Store.PutUser(&core.User{ID: "usr_lead", TeamID: "team1", Email: "lead@example.test"})
		it, _ := env.Engine.RequestHuman(ctx, root.ID, core.InboxApproval, "Spec approval", "ok?", nil)
		if _, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "approve", "dev")); err != nil {
			t.Fatal(err)
		}
		if got, _ := env.Store.GetInboxItem(ctx, it.ID); got.Resolved {
			t.Fatal("a member outside approvers approved")
		}
		if _, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "approve", "lead")); err != nil {
			t.Fatal(err)
		}
		if got, _ := env.Store.GetInboxItem(ctx, it.ID); !got.Resolved {
			t.Fatal("an approver could not approve")
		}

		// restrictFollowupsToStarter: only alice (the creator) is trusted.
		rt.Approvers = nil
		rt.RestrictFollowupsToStarter = true
		_ = saveFactoryRuntime(ctx, env.Store, env.Factory.ID, rt)
		q, _ := env.Engine.RequestHuman(ctx, root.ID, core.InboxQuestion, "Which?", "Which?", nil)
		if _, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "this one", "dev")); err != nil {
			t.Fatal(err)
		}
		if got, _ := env.Store.GetInboxItem(ctx, q.ID); got.Resolved {
			t.Fatal("a member who did not start the work item answered")
		}
		if _, err := env.Engine.HandleEvent(ctx, IssueEvent("issue_commented", "that one", "alice")); err != nil {
			t.Fatal(err)
		}
		if got, _ := env.Store.GetInboxItem(ctx, q.ID); !got.Resolved {
			t.Fatal("the starter could not answer")
		}
	})
}

func timelineOf(t *testing.T, env *TestEnv, wiID, kind string) []map[string]any {
	t.Helper()
	evs, err := env.Store.ListWorkItemEvents(context.Background(), wiID)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range evs {
		if e.Kind != kind {
			continue
		}
		m := map[string]any{}
		_ = json.Unmarshal(e.Data, &m)
		out = append(out, m)
	}
	return out
}

func TestActionKey(t *testing.T) {
	tests := []struct {
		ev   core.Event
		want string
	}{
		{core.Event{ActionKey: "x"}, "x"},
		{core.Event{Provider: "github", Type: "issue_commented", DeliveryID: "d1:issue_commented"}, "github:d1"},
		{core.Event{Provider: "linear", Type: "comment_created", DeliveryID: "abc"}, "linear:abc"},
		{core.Event{Provider: "slack", Type: "app_mention", DeliveryID: "Ev1", TeamID: "T", Payload: json.RawMessage(`{"channel":"C","ts":"1.2"}`)}, "slack:T:C:1.2"},
		{core.Event{Provider: "slack", Type: "reaction_added", DeliveryID: "Ev2", Payload: json.RawMessage(`{"reaction":"eyes"}`)}, "slack:Ev2"},
		{core.Event{Provider: "github", Type: "x"}, ""},
	}
	for _, tt := range tests {
		if got := ActionKey(&tt.ev); got != tt.want {
			t.Errorf("ActionKey(%+v) = %q, want %q", tt.ev, got, tt.want)
		}
	}
}

// flakyTxStore fails CreateRun one time on a transactional store.
type flakyTxStore struct {
	txTestStore
	mu     sync.Mutex
	calls  int
	failAt int
}

func (s *flakyTxStore) CreateRun(ctx context.Context, r *core.Run) error {
	s.mu.Lock()
	s.calls++
	fail := s.calls == s.failAt
	s.mu.Unlock()
	if fail {
		return errTransient
	}
	return s.txTestStore.CreateRun(ctx, r)
}

// On the memory and SQLite stores, the retry starts each matching run
// exactly one time.
func TestHandleEventRetryTransactional(t *testing.T) {
	for name, open := range txTestStores() {
		for _, failAt := range []int{1, 2} {
			t.Run(name+"/"+itoa(failAt), func(t *testing.T) {
				ctx := context.Background()
				base := open(t)
				st := &flakyTxStore{txTestStore: base, failAt: failAt}
				if err := st.CreateTeam(ctx, &core.Team{ID: "team1", Name: "team"}); err != nil {
					t.Fatal(err)
				}
				f := &core.Factory{ID: core.NewID("fac"), TeamID: "team1", Name: "payments", Alias: core.NewID("bot"), Source: core.DefinitionManaged}
				if err := st.CreateFactory(ctx, f); err != nil {
					t.Fatal(err)
				}
				def := DefaultTestDefinition()
				twoAgents(def)
				sec := NewFakeSecrets()
				_, _ = sec.Put(ctx, "team1", "", "ANTHROPIC_KEY", core.SecretAnthropicKey, []byte("sk-ant-test\n"))
				eng := New(Options{Store: st, Bus: NewFakeBus(), Compile: CompilerFor(def), Match: SimpleMatch, Secrets: sec, Forges: FakeForges{}, Logger: DiscardLogger()})
				if _, err := eng.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok"}, "tester", "initial", ""); err != nil {
					t.Fatal(err)
				}
				ev := IssueEvent("issue_labeled", "Invoices omit the PO number", "alice")
				ev.Actor.UserID = ""
				ev.DeliveryID = "tx-1:issue_labeled"
				payload, _ := json.Marshal(ev)
				job := core.Job{ID: "job-tx", Payload: payload}
				if err := eng.handleInboundJob(ctx, job); !errors.Is(err, errTransient) {
					t.Fatalf("first attempt err = %v", err)
				}
				for i := 0; i < 2; i++ {
					if err := eng.handleInboundJob(ctx, job); err != nil {
						t.Fatalf("retry %d err = %v", i, err)
					}
				}
				wi, err := st.FindWorkItemBySource(ctx, f.ID, ev.Source.Key())
				if err != nil {
					t.Fatal(err)
				}
				runs, err := st.ListRuns(ctx, core.RunFilter{WorkItemID: wi.ID})
				if err != nil {
					t.Fatal(err)
				}
				agents := map[string]int{}
				for _, r := range runs {
					agents[r.AgentName]++
				}
				if len(runs) != 2 || agents["foreman"] != 1 || agents["triage"] != 1 {
					t.Fatalf("runs = %v", agents)
				}
				root, err := st.GetRun(ctx, wi.RootRunID)
				if err != nil || root.AgentName != "foreman" {
					t.Fatalf("root = %+v err = %v", root, err)
				}
			})
		}
	}
}
