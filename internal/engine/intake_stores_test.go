package engine

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// On the real stores: the memory and SQLite stores have a dispatch table,
// so the engine does not use the key-value fallback, and the records expire.
func TestDispatchLedgerOnRealStores(t *testing.T) {
	// A long test with its own store and engine: it runs beside the other long one.
	t.Parallel()
	for name, open := range txTestStores() {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			if _, ok := st.(core.DispatchLedger); !ok {
				t.Fatalf("%s store has no dispatch ledger", name)
			}
			var mu sync.Mutex
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
			ctx := context.Background()
			team := &core.Team{ID: "team1", Name: "team"}
			if err := st.CreateTeam(ctx, team); err != nil {
				t.Fatal(err)
			}
			f := &core.Factory{ID: core.NewID("fac"), TeamID: team.ID, Name: "payments", Alias: core.NewID("bot"), Source: core.DefinitionManaged}
			if err := st.CreateFactory(ctx, f); err != nil {
				t.Fatal(err)
			}
			sec := NewFakeSecrets()
			_, _ = sec.Put(ctx, "team1", "", "ANTHROPIC_KEY", core.SecretAnthropicKey, []byte("sk-ant-test\n"))
			eng := New(Options{Store: st, Bus: NewFakeBus(), Compile: CompilerFor(DefaultTestDefinition()), Match: SimpleMatch,
				Secrets: sec, Forges: FakeForges{}, Logger: DiscardLogger(), Clock: clock})
			if _, err := eng.ApplyDefinition(ctx, f.ID, map[string]string{"factory.yaml": "ok"}, "tester", "initial", ""); err != nil {
				t.Fatal(err)
			}

			// One delivery twice starts one run.
			ev := IssueEvent("issue_labeled", "report", "alice")
			ev.DeliveryID = "d-twice:issue_labeled"
			if _, err := eng.HandleEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.HandleEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
			wi, err := st.FindWorkItemBySource(ctx, f.ID, ev.Source.Key())
			if err != nil {
				t.Fatal(err)
			}
			runs, err := st.ListRuns(ctx, core.RunFilter{WorkItemID: wi.ID})
			if err != nil || len(runs) != 1 {
				t.Fatalf("runs = %d (%v), want 1", len(runs), err)
			}

			// One event each hour for 30 days, with the hourly sweep.
			for i := 0; i < 24*30; i++ {
				e := IssueEvent("issue_labeled", "report", "alice")
				e.Source.Number = 1000 + i
				e.DeliveryID = "d" + strconv.Itoa(i)
				if _, err := eng.HandleEvent(ctx, e); err != nil {
					t.Fatal(err)
				}
				if _, err := eng.SweepDispatches(ctx); err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				now = now.Add(time.Hour)
				mu.Unlock()
			}
			// The old records are gone. If the engine had used the key-value
			// fallback, the ledger would hold no record at all.
			if _, err := st.(core.DispatchLedger).GetDispatch(ctx, "github:d-twice", "auto:none"); err == nil {
				t.Fatal("unexpected record")
			}
			left, err := st.(core.DispatchLedger).PurgeDispatches(ctx, now.Add(1000*24*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if limit := 2 * (24*7 + 2); left > limit {
				t.Fatalf("dispatch records after 30 days: %d, want at most %d", left, limit)
			}
			if left == 0 {
				t.Fatal("no dispatch record at all: the engine did not use the ledger")
			}
		})
	}
}
