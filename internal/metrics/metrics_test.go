package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

var (
	day0  = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	since = day0
	until = day0.AddDate(0, 0, 7)
)

func at(d int, h int) time.Time  { return day0.AddDate(0, 0, d).Add(time.Duration(h) * time.Hour) }
func ptr(t time.Time) *time.Time { return &t }

func ev(wi, kind string, t time.Time, data any) core.WorkItemEvent {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	return core.WorkItemEvent{ID: core.NewID("ev"), WorkItemID: wi, Kind: kind, Data: raw, CreatedAt: t}
}

func fixture() *Data {
	pr1 := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/app", Number: 1, URL: "https://x/1", State: "merged"}
	pr2 := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/app", Number: 2, URL: "https://x/2", State: "merged"}
	pr3 := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/app", Number: 3, URL: "https://x/3", State: "open"}
	pr4 := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/app", Number: 4, URL: "https://x/4", State: "closed"}
	return &Data{
		WorkItems: []core.WorkItem{
			// Autonomous, completed in 10h, one merged PR.
			{ID: "w1", Stage: core.StageComplete, CreatedAt: at(0, 0), CompletedAt: ptr(at(0, 10)), PullRequests: []core.PullRequestRef{pr1}},
			// Needed a human answer; completed in 20h; merged PR + open PR.
			{ID: "w2", Stage: core.StageComplete, CreatedAt: at(1, 0), CompletedAt: ptr(at(1, 20)), PullRequests: []core.PullRequestRef{pr2, pr3}},
			// Approval only (still autonomous), completed in 30h.
			{ID: "w3", Stage: core.StageComplete, CreatedAt: at(2, 0), CompletedAt: ptr(at(3, 6))},
			// Created before the window, completed inside: counts for cycle time, not created.
			{ID: "w4", Stage: core.StageComplete, CreatedAt: since.Add(-48 * time.Hour), CompletedAt: ptr(at(0, 2))},
			// Cancelled in window.
			{ID: "w5", Stage: core.StageCancelled, CreatedAt: at(3, 0), CompletedAt: ptr(at(3, 1))},
			// Still building, with a closed PR first observed via event.
			{ID: "w6", Stage: core.StageBuilding, CreatedAt: at(4, 0), PullRequests: []core.PullRequestRef{pr4}},
			// Taken over locally.
			{ID: "w7", Stage: core.StageComplete, CreatedAt: at(5, 0), CompletedAt: ptr(at(5, 1)), LocalOwner: "dev@laptop"},
			// Completed after the window.
			{ID: "w8", Stage: core.StageComplete, CreatedAt: at(6, 0), CompletedAt: ptr(until.Add(time.Hour))},
		},
		Events: map[string][]core.WorkItemEvent{
			"w1": {
				ev("w1", "message_in", at(0, 0), nil),
				ev("w1", "run_started", at(0, 0), nil),
				ev("w1", "stage_changed", at(0, 1), map[string]string{"from": "intake", "to": "building"}),
				ev("w1", "pr_linked", at(0, 5), pr1),
				ev("w1", "stage_changed", at(0, 9), map[string]string{"to": "reviewing"}),
				ev("w1", "stage_changed", at(0, 10), map[string]string{"to": "complete"}),
			},
			"w2": {
				ev("w2", "message_in", at(1, 0), nil),
				ev("w2", "run_started", at(1, 0), nil),
				ev("w2", "question", at(1, 2), nil),
				ev("w2", "pr_linked", at(1, 8), map[string]any{"pull_request": pr2}),
				ev("w2", "pr_linked", at(1, 9), pr3),
			},
			"w3": {
				ev("w3", "run_started", at(2, 0), nil),
				ev("w3", "approval_requested", at(2, 1), nil),
				ev("w3", "approval_decided", at(2, 5), nil),
				ev("w3", "stage_changed", at(2, 3), map[string]string{"stage": "awaiting_approval"}),
			},
			"w6": {
				ev("w6", "pr_linked", at(4, 3), map[string]string{"note": "unparsable"}),
			},
		},
		Runs: []core.Run{
			{ID: "r1", WorkItemID: "w1", AgentName: "foreman", State: core.RunSucceeded, TriggeredBy: "automation:a1", CreatedAt: at(0, 0),
				Config: core.AgentConfig{Harness: core.HarnessConfig{Model: "big"}}, Usage: core.Usage{CostUSD: 1, InputTokens: 100, OutputTokens: 10}},
			{ID: "r1c", WorkItemID: "w1", ParentRunID: "r1", AgentName: "implement", State: core.RunSucceeded, TriggeredBy: "run:r1", CreatedAt: at(0, 1),
				Config: core.AgentConfig{Harness: core.HarnessConfig{Model: "small"}}, Usage: core.Usage{CostUSD: 2, InputTokens: 200, OutputTokens: 20}},
			{ID: "r2", WorkItemID: "w2", AgentName: "foreman", State: core.RunFailed, TriggeredBy: "api", CreatedAt: at(1, 0),
				Config: core.AgentConfig{Harness: core.HarnessConfig{Model: "big"}}, Usage: core.Usage{CostUSD: 4, InputTokens: 400, OutputTokens: 40}},
			{ID: "r6", WorkItemID: "w6", AgentName: "implement", State: core.RunRunning, TriggeredBy: "benchmark:b1", CreatedAt: at(4, 0),
				Usage: core.Usage{CostUSD: 0.5}},
			// Before the window: counts toward PR cost (none here) but not run stats.
			{ID: "r4", WorkItemID: "w4", AgentName: "foreman", State: core.RunSucceeded, CreatedAt: since.Add(-47 * time.Hour), Usage: core.Usage{CostUSD: 100}},
		},
		Inbox: []core.InboxItem{
			{ID: "i1", WorkItemID: "w2", Kind: core.InboxQuestion, CreatedAt: at(1, 2), Resolved: true, ResolvedAt: ptr(at(1, 3))},
			{ID: "i2", WorkItemID: "w3", Kind: core.InboxApproval, CreatedAt: at(2, 1), Resolved: true, ResolvedAt: ptr(at(2, 5))},
			{ID: "i3", WorkItemID: "w6", Kind: core.InboxApproval, CreatedAt: until.Add(-2 * time.Hour)},
			{ID: "i4", Kind: core.InboxReview, CreatedAt: at(1, 0)},
		},
		Scorers: []core.Scorer{{ID: "s1", Slug: "tests-run", Name: "Tests run"}, {ID: "s2", Slug: "idle"}},
		Scores: []core.Score{
			{ScorerID: "s1", Label: "tests_run", Passed: true, CreatedAt: at(0, 3)},
			{ScorerID: "s1", Label: "tests_skipped", Passed: false, CreatedAt: at(0, 4)},
			{ScorerID: "s1", Label: "tests_run", Passed: true, CreatedAt: at(2, 4)},
			{ScorerID: "s1", Label: "tests_run", Passed: true, CreatedAt: until}, // excluded
		},
	}
}

func TestBuild(t *testing.T) {
	d := Build("fac", fixture(), since, until)

	t.Run("work items", func(t *testing.T) {
		w := d.WorkItems
		if w.Created != 7 || w.Completed != 5 || w.Cancelled != 1 {
			t.Fatalf("%+v", w)
		}
		if w.ByStage[core.StageComplete] != 5 || w.ByStage[core.StageBuilding] != 1 || w.ByStage[core.StageCancelled] != 1 {
			t.Fatalf("by stage %+v", w.ByStage)
		}
	})
	t.Run("autonomy", func(t *testing.T) {
		a := d.Autonomy
		// Completed in window: w1 (auto), w2 (question), w3 (approval only => auto), w4 (no events => auto), w7 (local).
		if a.CompletedWorkItems != 5 || a.AutonomousWorkItems != 3 || !approxEq(a.Rate, 0.6) {
			t.Fatalf("%+v", a)
		}
		// PR autonomy needs pull request facts: the fixture has none, so
		// the two merged PRs are counted apart.
		if a.MergedPRs != 0 || a.MergedPRsWithoutFacts != 2 || a.PRRate != 0 || a.FactsSince != nil {
			t.Fatalf("pr autonomy %+v", a)
		}
	})
	t.Run("cycle time", func(t *testing.T) {
		c := d.CycleTime
		h := int64(time.Hour / time.Millisecond)
		// durations: w1 10h, w2 20h, w3 30h, w4 50h, w7 1h
		if c.Count != 5 || c.MedianMS != 20*h || c.P90MS != 50*h || c.MeanMS != (111*h)/5 {
			t.Fatalf("%+v", c)
		}
		// w1 stage times: intake 1h, building 8h, reviewing 1h.
		// w3: intake 3h then awaiting_approval until completion (27h).
		if c.StageMedianMS[core.StageBuilding] != 8*h || c.StageMedianMS[core.StageReviewing] != h || c.StageMedianMS[core.StageIntake] != 2*h || c.StageMedianMS[core.StageAwaitingApproval] != 27*h {
			t.Fatalf("stage medians %+v", c.StageMedianMS)
		}
	})
	t.Run("pull requests", func(t *testing.T) {
		p := d.PullRequests
		if p.Opened != 4 || p.Merged != 2 || p.Open != 1 || p.Closed != 1 || p.MergeRate != 0.5 {
			t.Fatalf("%+v", p)
		}
		// Costs: pr1 = 3 (w1 runs), pr2 = pr3 = 2 (w2 cost 4 split), pr4 = 0.5.
		if !approxEq(p.MedianCostUSD, 2) || !approxEq(p.TotalCostUSD, 7.5) {
			t.Fatalf("cost %+v", p)
		}
		if len(p.MostExpensive) != 4 || p.MostExpensive[0].Number != 1 || p.MostExpensive[3].Number != 4 {
			t.Fatalf("most expensive %+v", p.MostExpensive)
		}
		// pr4 first observed at its (unparsable) pr_linked event.
		if !p.MostExpensive[3].OpenedAt.Equal(at(4, 3)) {
			t.Fatalf("pr4 opened at %v", p.MostExpensive[3].OpenedAt)
		}
		// time to PR: 5h, 8h, 9h, 3h => median 6.5h
		if p.MedianTimeToPRMS != int64(6.5*float64(time.Hour/time.Millisecond)) {
			t.Fatalf("time to PR %d", p.MedianTimeToPRMS)
		}
	})
	t.Run("human wait", func(t *testing.T) {
		h := d.HumanWait
		hr := int64(time.Hour / time.Millisecond)
		if h.Items != 3 || h.Pending != 1 || h.MedianMS != (hr+4*hr)/2 || h.P90MS != 4*hr || h.TotalMS != 7*hr {
			t.Fatalf("%+v", h)
		}
		if h.ByKind[core.InboxApproval] != 2 || h.ByKind[core.InboxReview] != 0 {
			t.Fatalf("by kind %+v", h.ByKind)
		}
	})
	t.Run("runs and cost", func(t *testing.T) {
		r := d.Runs
		if r.Total != 4 || r.ByAgent["foreman"] != 2 || r.ByAgent["implement"] != 2 || r.ByState[core.RunSucceeded] != 2 {
			t.Fatalf("%+v", r)
		}
		if r.ByAgentState["foreman"][core.RunFailed] != 1 || r.BySource["AUTOMATION"] != 1 || r.BySource["BENCHMARK"] != 1 || r.BySource["ORCHESTRATION"] != 1 || r.BySource["API"] != 1 || len(r.BySource) != 4 || r.ByModel["(default)"] != 1 {
			t.Fatalf("breakdowns %+v", r)
		}
		c := d.Cost
		if !approxEq(c.Total.CostUSD, 7.5) || c.Total.InputTokens != 700 || c.Total.Runs != 4 {
			t.Fatalf("total %+v", c.Total)
		}
		if !approxEq(c.ByAgent["foreman"].CostUSD, 5) || !approxEq(c.ByModel["big"].CostUSD, 5) || c.ByModel["small"].OutputTokens != 20 {
			t.Fatalf("by agent/model %+v %+v", c.ByAgent, c.ByModel)
		}
	})
	t.Run("scorers", func(t *testing.T) {
		if len(d.Scorers) != 2 {
			t.Fatalf("%+v", d.Scorers)
		}
		var s1 ScorerStats
		for _, s := range d.Scorers {
			if s.ScorerID == "s1" {
				s1 = s
			}
		}
		if s1.Total != 3 || s1.Passed != 2 || !approxEq(s1.PassRate, 2.0/3) || s1.Labels["tests_run"] != 2 || s1.Name != "Tests run" {
			t.Fatalf("%+v", s1)
		}
	})
	t.Run("daily", func(t *testing.T) {
		if len(d.Daily) != 7 || d.Daily[0].Date != "2026-05-01" || d.Daily[6].Date != "2026-05-07" {
			t.Fatalf("days %+v", d.Daily)
		}
		p0 := d.Daily[0]
		if p0.Runs != 2 || !approxEq(p0.CostUSD, 3) || p0.WorkItemsCreated != 1 || p0.WorkItemsCompleted != 2 || p0.PRsOpened != 1 || p0.PRsMerged != 1 || p0.Scores != 2 || p0.ScoresPassed != 1 {
			t.Fatalf("day 0 %+v", p0)
		}
		p1 := d.Daily[1]
		if p1.PRsOpened != 2 || p1.PRsMerged != 1 {
			t.Fatalf("day 1 %+v", p1)
		}
		var runs int
		for _, p := range d.Daily {
			runs += p.Runs
		}
		if runs != d.Runs.Total {
			t.Fatalf("daily runs %d != total %d", runs, d.Runs.Total)
		}
	})
}

func approxEq(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestAutonomyRules(t *testing.T) {
	w := core.WorkItem{ID: "w"}
	tests := []struct {
		name   string
		w      core.WorkItem
		events []core.WorkItemEvent
		inbox  []core.InboxItem
		want   bool
	}{
		{"no events", w, nil, nil, true},
		{"initial message only", w, []core.WorkItemEvent{{Kind: "message_in"}, {Kind: "run_started"}, {Kind: "message_out"}}, nil, true},
		{"follow-up message after start", w, []core.WorkItemEvent{{Kind: "run_started"}, {Kind: "message_in"}}, nil, false},
		{"two messages before start", w, []core.WorkItemEvent{{Kind: "message_in"}, {Kind: "message_in"}}, nil, false},
		{"approvals allowed", w, []core.WorkItemEvent{{Kind: "approval_requested"}, {Kind: "approval_decided"}}, []core.InboxItem{{Kind: core.InboxApproval}, {Kind: core.InboxReview}}, true},
		{"answer", w, []core.WorkItemEvent{{Kind: "answer"}}, nil, false},
		{"handoff inbox", w, nil, []core.InboxItem{{Kind: core.InboxHandoff}}, false},
		{"failure inbox", w, nil, []core.InboxItem{{Kind: core.InboxFailure}}, false},
		{"local takeover", core.WorkItem{LocalOwner: "x"}, nil, nil, false},
	}
	for _, tt := range tests {
		if got := isAutonomous(tt.w, tt.events, tt.inbox); got != tt.want {
			t.Errorf("%s: got %v want %v", tt.name, got, tt.want)
		}
	}
}

func TestPercentiles(t *testing.T) {
	tests := []struct {
		xs          []int64
		med, p90, m int64
	}{
		{nil, 0, 0, 0},
		{[]int64{5}, 5, 5, 5},
		{[]int64{4, 1, 3, 2}, 2, 4, 2},
		{[]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 5, 9, 5},
	}
	for _, tt := range tests {
		if median(tt.xs) != tt.med || percentile(tt.xs, 90) != tt.p90 || mean(tt.xs) != tt.m {
			t.Errorf("%v: median %d p90 %d mean %d", tt.xs, median(tt.xs), percentile(tt.xs, 90), mean(tt.xs))
		}
	}
}

func TestPRDedupAcrossWorkItems(t *testing.T) {
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "a/b", Number: 9, State: "open"}
	merged := pr
	merged.State = "merged"
	d := &Data{
		WorkItems: []core.WorkItem{
			{ID: "x", CreatedAt: at(2, 0), PullRequests: []core.PullRequestRef{pr}},
			{ID: "y", CreatedAt: at(1, 0), PullRequests: []core.PullRequestRef{merged}},
		},
		Runs: []core.Run{{WorkItemID: "x", Usage: core.Usage{CostUSD: 1}, CreatedAt: at(2, 0)}, {WorkItemID: "y", Usage: core.Usage{CostUSD: 2}, CreatedAt: at(1, 0)}},
	}
	db := Build("f", d, since, until)
	if db.PullRequests.Opened != 1 || db.PullRequests.Merged != 1 || !approxEq(db.PullRequests.TotalCostUSD, 3) {
		t.Fatalf("%+v", db.PullRequests)
	}
	if got := db.PullRequests.MostExpensive[0]; got.WorkItemID != "y" || !got.OpenedAt.Equal(at(1, 0)) {
		t.Fatalf("earliest observation not kept: %+v", got)
	}
}

func TestBuildEmpty(t *testing.T) {
	db := Build("f", nil, since, until)
	if db.Runs.Total != 0 || len(db.Daily) != 7 || db.PullRequests.MostExpensive == nil || db.Scorers == nil {
		t.Fatalf("%+v", db)
	}
	if _, err := json.Marshal(db); err != nil {
		t.Fatal(err)
	}
}

// pagingStore is a fake core.Store honoring ListOpts limits and cursors.
type pagingStore struct {
	core.Store
	items      []core.WorkItem
	events     map[string][]core.WorkItemEvent
	runs       []core.Run
	inbox      []core.InboxItem
	scorers    []core.Scorer
	scores     []core.Score
	eventCalls []string
	pages      int
}

func pageOf[T any](all []T, created func(T) time.Time, id func(T) string, since *time.Time, opts core.ListOpts) ([]T, error) {
	c, err := storeutil.DecodeCursor(opts.Cursor)
	if err != nil {
		return nil, err
	}
	var filtered []T
	for _, x := range all {
		if since != nil && created(x).Before(*since) {
			continue
		}
		if !c.After(created(x).UnixNano(), id(x)) {
			continue
		}
		filtered = append(filtered, x)
	}
	sort.Slice(filtered, func(i, j int) bool {
		a, b := created(filtered[i]), created(filtered[j])
		if !a.Equal(b) {
			return a.After(b)
		}
		return id(filtered[i]) > id(filtered[j])
	})
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (s *pagingStore) ListWorkItems(ctx context.Context, f core.WorkItemFilter) ([]core.WorkItem, error) {
	s.pages++
	return pageOf(s.items, func(w core.WorkItem) time.Time { return w.CreatedAt }, func(w core.WorkItem) string { return w.ID }, f.Since, f.ListOpts)
}

func (s *pagingStore) ListRuns(ctx context.Context, f core.RunFilter) ([]core.Run, error) {
	s.pages++
	return pageOf(s.runs, func(r core.Run) time.Time { return r.CreatedAt }, func(r core.Run) string { return r.ID }, f.Since, f.ListOpts)
}

func (s *pagingStore) ListWorkItemEvents(ctx context.Context, id string) ([]core.WorkItemEvent, error) {
	s.eventCalls = append(s.eventCalls, id)
	return s.events[id], nil
}

func (s *pagingStore) ListInboxItems(ctx context.Context, factoryID string, includeResolved bool) ([]core.InboxItem, error) {
	return s.inbox, nil
}

func (s *pagingStore) ListScorers(ctx context.Context, factoryID string) ([]core.Scorer, error) {
	return s.scorers, nil
}

func (s *pagingStore) ListScores(ctx context.Context, factoryID, scorerID string, since *time.Time) ([]core.Score, error) {
	var out []core.Score
	for _, x := range s.scores {
		if since == nil || !x.CreatedAt.Before(*since) {
			out = append(out, x)
		}
	}
	return out, nil
}

func TestCompute(t *testing.T) {
	fx := fixture()
	st := &pagingStore{events: fx.Events, inbox: fx.Inbox, scorers: fx.Scorers, scores: fx.Scores}
	st.items = append(st.items, fx.WorkItems...)
	st.runs = append(st.runs, fx.Runs...)
	// Bulk runs to force several pages (500 per page).
	for i := 0; i < 1200; i++ {
		st.runs = append(st.runs, core.Run{ID: fmt.Sprintf("bulk_%04d", i), AgentName: "triage", State: core.RunSucceeded, TriggeredBy: "automation:x", CreatedAt: at(6, 0).Add(time.Duration(i%5) * time.Second), Usage: core.Usage{CostUSD: 0.01}})
	}
	// A work item far older than the lookback is excluded.
	st.items = append(st.items, core.WorkItem{ID: "ancient", Stage: core.StageComplete, CreatedAt: since.AddDate(-1, 0, 0), CompletedAt: ptr(at(0, 1))})

	got, err := Compute(context.Background(), st, "fac", since, until)
	if err != nil {
		t.Fatal(err)
	}
	want := Build("fac", fx, since, until)
	if got.Runs.Total != want.Runs.Total+1200 || got.Runs.ByAgent["triage"] != 1200 {
		t.Fatalf("runs %d (want %d)", got.Runs.Total, want.Runs.Total+1200)
	}
	if got.CycleTime.Count != want.CycleTime.Count || got.CycleTime.MedianMS != want.CycleTime.MedianMS || got.CycleTime.P90MS != want.CycleTime.P90MS || got.Autonomy != want.Autonomy || got.PullRequests.Opened != want.PullRequests.Opened {
		t.Fatalf("mismatch:\n got %+v %+v\nwant %+v %+v", got.CycleTime, got.Autonomy, want.CycleTime, want.Autonomy)
	}
	if st.pages < 4 {
		t.Fatalf("expected paging, got %d list calls", st.pages)
	}
	for _, id := range st.eventCalls {
		if id == "ancient" {
			t.Fatalf("loaded events for %s", id)
		}
	}

	if _, err := Compute(context.Background(), st, "fac", until, since); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("want ErrInvalid for inverted window, got %v", err)
	}
}

func TestRunSourceClosedList(t *testing.T) {
	cases := map[string]string{
		"":                  "API",
		"api":               "API",
		"mcp":               "MCP",
		"mention":           "MENTION",
		"follow_up":         "MENTION",
		"automation:a1":     "AUTOMATION",
		"schedule:s1":       "SCHEDULE",
		"run:r1":            "ORCHESTRATION",
		"scorer:x":          "SCORER",
		"self_improvement":  "SELF_IMPROVEMENT",
		"something_else:42": "API",
	}
	for in, want := range cases {
		if got := runSource(core.Run{TriggeredBy: in}); got != want {
			t.Errorf("runSource(%q) = %q, want %q", in, got, want)
		}
	}
}
