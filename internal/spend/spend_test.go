package spend

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
)

var ctx = context.Background()

// eachStore runs fn with the SQLite store.
func eachStore(t *testing.T, fn func(t *testing.T, st core.Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		s, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "of.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		fn(t, s)
	})
}

func addRun(t *testing.T, st core.Store, team, factory string, at time.Time, cost float64) {
	t.Helper()
	r := &core.Run{ID: core.NewID("run"), TeamID: team, FactoryID: factory, State: core.RunSucceeded, CreatedAt: at, Usage: core.Usage{CostUSD: cost}}
	if err := st.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func TestReportAndLimits(t *testing.T) {
	eachStore(t, testReportAndLimits)
}

func testReportAndLimits(t *testing.T, st core.Store) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	addRun(t, st, "team1", "fac1", now.Add(-time.Hour), 3)     // today
	addRun(t, st, "team1", "fac1", now.Add(-48*time.Hour), 4)  // this month
	addRun(t, st, "team1", "fac1", now.AddDate(0, -1, 0), 100) // last month
	addRun(t, st, "team1", "fac2", now.Add(-2*time.Hour), 5)   // other factory, today
	addRun(t, st, "team2", "fac9", now.Add(-2*time.Hour), 50)  // other team
	limits := map[string]*core.SpendLimit{"fac1": {DailyUSD: 3, MonthlyUSD: 20}}
	tr := New(Options{
		Store: st, Now: func() time.Time { return now },
		FactoryLimit: func(_ context.Context, id string) (*core.SpendLimit, error) { return limits[id], nil },
		TeamLimit:    core.SpendLimit{MonthlyUSD: 100},
	})
	rep, err := tr.Report(ctx, "team1", "fac1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.FactoryDailyUSD != 3 || rep.FactoryMonthlyUSD != 7 || rep.TeamMonthlyUSD != 12 || rep.TeamDailyUSD != 8 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Limits) != 3 {
		t.Fatalf("limits = %+v", rep.Limits)
	}
	ex := rep.Exceeded()
	if ex == nil || ex.Scope != ScopeFactory || ex.Period != PeriodDaily {
		t.Fatalf("exceeded = %+v", ex)
	}
	if !ex.ResetsAt.Equal(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("resets at %v", ex.ResetsAt)
	}
	// fac2 has no factory limit and the team limit is not reached.
	if l, err := tr.Exceeded(ctx, &core.Run{TeamID: "team1", FactoryID: "fac2"}); err != nil || l != nil {
		t.Fatalf("fac2: %+v %v", l, err)
	}
	// Without any limit nothing is read.
	free := New(Options{Store: st, Now: func() time.Time { return now }})
	if l, err := free.Exceeded(ctx, &core.Run{TeamID: "team1", FactoryID: "fac1"}); err != nil || l != nil {
		t.Fatalf("no limits: %+v %v", l, err)
	}
}

func TestWarnAt80Percent(t *testing.T) {
	eachStore(t, testWarnAt80Percent)
}

func testWarnAt80Percent(t *testing.T, st core.Store) {
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)
	for _, u := range []core.User{
		{ID: "u_admin", TeamID: "team1", Email: "a@example.com", Role: core.RoleAdmin},
		{ID: "u_editor", TeamID: "team1", Email: "e@example.com", Role: core.RoleEditor},
		{ID: "sa_bot", TeamID: "team1", Role: core.RoleAdmin, Kind: core.UserKindServiceAccount},
	} {
		u := u
		if err := st.CreateUser(ctx, &u); err != nil {
			t.Fatal(err)
		}
	}
	var published int
	tr := New(Options{
		Store: st, Now: func() time.Time { return now },
		FactoryLimit: func(context.Context, string) (*core.SpendLimit, error) { return &core.SpendLimit{DailyUSD: 10}, nil },
		OnInbox:      func(context.Context, *core.InboxItem) { published++ },
	})
	run := &core.Run{TeamID: "team1", FactoryID: "fac1"}
	addRun(t, st, "team1", "fac1", now.Add(-time.Minute), 7.9)
	if _, err := tr.Exceeded(ctx, run); err != nil {
		t.Fatal(err)
	}
	items, _ := st.ListInboxItems(ctx, "fac1", true)
	if len(items) != 0 {
		t.Fatalf("no item below 80 percent: %+v", items)
	}
	addRun(t, st, "team1", "fac1", now.Add(-time.Minute), 0.2)
	for i := 0; i < 3; i++ {
		if l, err := tr.Exceeded(ctx, run); err != nil || l != nil {
			t.Fatalf("not over yet: %+v %v", l, err)
		}
	}
	items, _ = st.ListInboxItems(ctx, "fac1", true)
	if len(items) != 1 || published != 1 {
		t.Fatalf("want one item for the one admin, got %d (published %d)", len(items), published)
	}
	it := items[0]
	if it.Kind != core.InboxSpendLimit || it.AssigneeID != "u_admin" || !strings.Contains(it.Title, "81%") || !strings.Contains(it.Body, "$10.00") {
		t.Fatalf("item = %+v", it)
	}
	// The next day is a new period: a new item when 80 percent is used again.
	now = now.Add(24 * time.Hour)
	addRun(t, st, "team1", "fac1", now.Add(-time.Minute), 12)
	l, err := tr.Exceeded(ctx, run)
	if err != nil || l == nil || l.Period != PeriodDaily {
		t.Fatalf("over: %+v %v", l, err)
	}
	items, _ = st.ListInboxItems(ctx, "fac1", true)
	if len(items) != 2 {
		t.Fatalf("want a second item in the new day, got %d", len(items))
	}
}
