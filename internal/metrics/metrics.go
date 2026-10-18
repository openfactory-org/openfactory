package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// DefaultLookback is how far before `since` Collect loads work items and runs,
// so items created earlier but completed (or costed) in the window are counted.
const DefaultLookback = 90 * 24 * time.Hour

// MostExpensiveLimit is the number of PRs listed in PRStats.MostExpensive.
const MostExpensiveLimit = 5

// Dashboard is the computed metrics page for one factory and window.
type Dashboard struct {
	FactoryID    string         `json:"factory_id"`
	Since        time.Time      `json:"since"`
	Until        time.Time      `json:"until"`
	WorkItems    WorkItemStats  `json:"work_items"`
	PullRequests PRStats        `json:"pull_requests"`
	Autonomy     AutonomyStats  `json:"autonomy"`
	CycleTime    CycleTimeStats `json:"cycle_time"`
	HumanWait    HumanWaitStats `json:"human_wait"`
	Runs         RunStats       `json:"runs"`
	Cost         CostStats      `json:"cost"`
	Scorers      []ScorerStats  `json:"scorers"`
	Daily        []DailyPoint   `json:"daily"`
}

// WorkItemStats summarizes work items.
type WorkItemStats struct {
	Created   int                `json:"created"`
	Completed int                `json:"completed"`
	Cancelled int                `json:"cancelled"`
	ByStage   map[core.Stage]int `json:"by_stage"`
}

// PRStats summarizes pull requests opened in the window.
type PRStats struct {
	Opened int `json:"opened"`
	Merged int `json:"merged"`
	Closed int `json:"closed"`
	Open   int `json:"open"` // open or draft
	// MergeRate is Merged / Opened.
	MergeRate        float64  `json:"merge_rate"`
	MedianCostUSD    float64  `json:"median_cost_usd"`
	TotalCostUSD     float64  `json:"total_cost_usd"`
	MostExpensive    []PRCost `json:"most_expensive"`
	MedianTimeToPRMS int64    `json:"median_time_to_pr_ms"`
	// BySize groups the cost of the PRs opened in the window by their
	// changed lines (pull request facts). PRs without facts are in "unknown".
	BySize []PRSizeCost `json:"by_size"`
}

// PR size buckets by changed lines (additions plus deletions).
var prSizes = []struct {
	Size string
	Max  int // exclusive; 0 = no limit
}{
	{"xs", 10}, {"s", 50}, {"m", 250}, {"l", 1000}, {"xl", 0},
}

// PRSizeCost is the cost of the PRs of one size.
type PRSizeCost struct {
	Size          string  `json:"size"`  // xs, s, m, l, xl or unknown
	Lines         string  `json:"lines"` // the range of changed lines, e.g. "10-49"
	Count         int     `json:"count"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	MedianCostUSD float64 `json:"median_cost_usd"`
}

// PRCost is one PR's attributed cost.
type PRCost struct {
	Forge      string    `json:"forge,omitempty"`
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	URL        string    `json:"url,omitempty"`
	State      string    `json:"state"`
	WorkItemID string    `json:"work_item_id"`
	OpenedAt   time.Time `json:"opened_at"`
	CostUSD    float64   `json:"cost_usd"`
}

// AutonomyStats reports autonomy (see the package documentation).
type AutonomyStats struct {
	CompletedWorkItems  int     `json:"completed_work_items"`
	AutonomousWorkItems int     `json:"autonomous_work_items"`
	Rate                float64 `json:"rate"`
	// MergedPRs counts the PRs of the factory that merged in the window and
	// have pull request facts. AutonomousMergedPRs counts those that no
	// person opened or pushed to before the merge.
	MergedPRs           int     `json:"merged_prs"`
	AutonomousMergedPRs int     `json:"autonomous_merged_prs"`
	PRRate              float64 `json:"pr_rate"`
	// MergedPRsWithoutFacts counts merged PRs (by work item state) that have
	// no facts, because their events arrived before the forge was connected.
	MergedPRsWithoutFacts int `json:"merged_prs_without_facts"`
	// FactsSince is the time of the first pull request fact of the factory.
	// The PR figures start then.
	FactsSince *time.Time `json:"facts_since,omitempty"`
}

// CycleTimeStats reports created → complete durations, in milliseconds.
type CycleTimeStats struct {
	Count         int                  `json:"count"`
	MedianMS      int64                `json:"median_ms"`
	P90MS         int64                `json:"p90_ms"`
	MeanMS        int64                `json:"mean_ms"`
	StageMedianMS map[core.Stage]int64 `json:"stage_median_ms"`
	// PullRequests is the cycle time of the PRs that merged in the window,
	// from pull request facts.
	PullRequests PRCycleStats `json:"pull_requests"`
}

// PRCycleStats are medians, in milliseconds, of the parts of the cycle of a
// merged PR: from the start of its work item to the PR, from the PR to the
// first review, and from the first review to the merge. Each part is the
// median of the PRs that have both ends, so the parts do not sum to
// StartToMergeMS.
type PRCycleStats struct {
	Count                int   `json:"count"`
	StartToMergeMS       int64 `json:"start_to_merge_ms"`
	StartToPRMS          int64 `json:"start_to_pr_ms"`
	PRToFirstReviewMS    int64 `json:"pr_to_first_review_ms"`
	FirstReviewToMergeMS int64 `json:"first_review_to_merge_ms"`
	Reviewed             int   `json:"reviewed"`
}

// HumanWaitStats reports time spent waiting on people, in milliseconds.
type HumanWaitStats struct {
	Items    int                    `json:"items"`
	Pending  int                    `json:"pending"`
	TotalMS  int64                  `json:"total_ms"`
	MedianMS int64                  `json:"median_ms"`
	P90MS    int64                  `json:"p90_ms"`
	ByKind   map[core.InboxKind]int `json:"by_kind"`
}

// RunStats counts runs created in the window.
type RunStats struct {
	Total        int                              `json:"total"`
	ByAgent      map[string]int                   `json:"by_agent"`
	ByState      map[core.RunState]int            `json:"by_state"`
	ByAgentState map[string]map[core.RunState]int `json:"by_agent_state"`
	ByModel      map[string]int                   `json:"by_model"`
	BySource     map[string]int                   `json:"by_source"`
}

// UsageTotals sums run usage.
type UsageTotals struct {
	Runs         int     `json:"runs"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	DurationMS   int64   `json:"duration_ms"`
	// ComputeSeconds is the life of the sandboxes of the runs. It is
	// zero for runs without recorded sessions.
	ComputeSeconds float64 `json:"compute_seconds"`
}

func (u *UsageTotals) add(x core.Usage) {
	u.Runs++
	u.CostUSD += x.CostUSD
	u.InputTokens += x.InputTokens
	u.OutputTokens += x.OutputTokens
	u.DurationMS += x.DurationMS
}

// CostStats reports cost and tokens of runs created in the window.
type CostStats struct {
	Total   UsageTotals            `json:"total"`
	ByAgent map[string]UsageTotals `json:"by_agent"`
	ByModel map[string]UsageTotals `json:"by_model"`
	// ComputeByShape groups the sandbox time of the runs by instance shape
	// (core.InstanceShape.Label, "default" for a session without a shape).
	ComputeByShape map[string]ComputeStats `json:"compute_by_shape"`
}

// ComputeStats is the sandbox time of the sessions of one instance shape.
type ComputeStats struct {
	Sessions int     `json:"sessions"`
	Seconds  float64 `json:"seconds"`
}

// ScorerStats summarizes one scorer's results in the window.
type ScorerStats struct {
	ScorerID string         `json:"scorer_id"`
	Slug     string         `json:"slug,omitempty"`
	Name     string         `json:"name,omitempty"`
	Total    int            `json:"total"`
	Passed   int            `json:"passed"`
	PassRate float64        `json:"pass_rate"`
	Labels   map[string]int `json:"labels"`
}

// DailyPoint is one UTC day of the time series.
type DailyPoint struct {
	Date               string  `json:"date"` // YYYY-MM-DD
	Runs               int     `json:"runs"`
	CostUSD            float64 `json:"cost_usd"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	WorkItemsCreated   int     `json:"work_items_created"`
	WorkItemsCompleted int     `json:"work_items_completed"`
	PRsOpened          int     `json:"prs_opened"`
	PRsMerged          int     `json:"prs_merged"`
	Scores             int     `json:"scores"`
	ScoresPassed       int     `json:"scores_passed"`
}

// Data is the raw input of Build.
type Data struct {
	WorkItems []core.WorkItem
	// Events holds work-item timelines keyed by work item ID (only items that
	// matter for the window need to be present).
	Events  map[string][]core.WorkItemEvent
	Runs    []core.Run
	Inbox   []core.InboxItem
	Scorers []core.Scorer
	Scores  []core.Score
	// Facts are the pull request facts of the PRs of the work items, keyed
	// by the source key of the PR in lower case (PRFactsKey).
	Facts map[string]core.PullRequestFacts
	// Sessions are the sessions of the runs, keyed by run ID. They exist
	// only for runs on the runner path, and only when the store has
	// core.RunnerStore.
	Sessions map[string][]core.RunSession
}

// PRFactsKey is the key of Data.Facts for a pull request.
func PRFactsKey(pr core.PullRequestRef) string {
	return strings.ToLower(core.PullRequestSource(pr).Key())
}

// Compute loads the factory's data from the store and builds the dashboard
// for [since, until).
func Compute(ctx context.Context, store core.Store, factoryID string, since, until time.Time) (*Dashboard, error) {
	d, err := Collect(ctx, store, factoryID, since, until, DefaultLookback)
	if err != nil {
		return nil, err
	}
	return Build(factoryID, d, since, until), nil
}

// Collect loads everything Build needs. Work items and runs are loaded from
// since−lookback so earlier items completed (or costed) in the window count.
func Collect(ctx context.Context, store core.Store, factoryID string, since, until time.Time, lookback time.Duration) (*Data, error) {
	if factoryID == "" {
		return nil, fmt.Errorf("%w: factory id is required", core.ErrInvalid)
	}
	if !until.After(since) {
		return nil, fmt.Errorf("%w: until must be after since", core.ErrInvalid)
	}
	if lookback < 0 {
		lookback = 0
	}
	from := since.Add(-lookback)
	d := &Data{Events: map[string][]core.WorkItemEvent{}}

	wf := core.WorkItemFilter{FactoryID: factoryID, Since: &from}
	wf.Limit = storeutil.MaxLimit
	for {
		page, err := store.ListWorkItems(ctx, wf)
		if err != nil {
			return nil, fmt.Errorf("list work items: %w", err)
		}
		for _, w := range page {
			if w.CreatedAt.Before(until) {
				d.WorkItems = append(d.WorkItems, w)
			}
		}
		if len(page) < wf.Limit {
			break
		}
		last := page[len(page)-1]
		next := storeutil.EncodeCursor(last.CreatedAt, last.ID)
		if next == wf.Cursor {
			break
		}
		wf.Cursor = next
	}
	for _, w := range d.WorkItems {
		if !needsEvents(w, since, until) {
			continue
		}
		ev, err := store.ListWorkItemEvents(ctx, w.ID)
		if err != nil {
			return nil, fmt.Errorf("list work item events %s: %w", w.ID, err)
		}
		d.Events[w.ID] = ev
	}

	rf := core.RunFilter{FactoryID: factoryID, Since: &from}
	rf.Limit = storeutil.MaxLimit
	for {
		page, err := store.ListRuns(ctx, rf)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}
		for _, r := range page {
			if r.CreatedAt.Before(until) {
				d.Runs = append(d.Runs, r)
			}
		}
		if len(page) < rf.Limit {
			break
		}
		last := page[len(page)-1]
		next := storeutil.EncodeCursor(last.CreatedAt, last.ID)
		if next == rf.Cursor {
			break
		}
		rf.Cursor = next
	}

	inbox, err := store.ListInboxItems(ctx, factoryID, true)
	if err != nil {
		return nil, fmt.Errorf("list inbox items: %w", err)
	}
	d.Inbox = inbox
	if d.Scorers, err = store.ListScorers(ctx, factoryID); err != nil {
		return nil, fmt.Errorf("list scorers: %w", err)
	}
	if d.Scores, err = store.ListScores(ctx, factoryID, "", &since); err != nil {
		return nil, fmt.Errorf("list scores: %w", err)
	}
	if err := collectFacts(ctx, store, factoryID, d); err != nil {
		return nil, err
	}
	if err := collectSessions(ctx, store, since, until, d); err != nil {
		return nil, err
	}
	return d, nil
}

// collectSessions loads the sessions of the runs created in the window.
func collectSessions(ctx context.Context, store core.Store, since, until time.Time, d *Data) error {
	rs, ok := store.(core.RunnerStore)
	if !ok {
		return nil
	}
	d.Sessions = map[string][]core.RunSession{}
	for _, r := range d.Runs {
		if !inWindow(r.CreatedAt, since, until) {
			continue
		}
		list, err := rs.ListRunSessions(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("list run sessions %s: %w", r.ID, err)
		}
		if len(list) > 0 {
			d.Sessions[r.ID] = list
		}
	}
	return nil
}

// sessionSeconds is the sandbox time of a session. A session that did not
// end counts until the end of the window.
func sessionSeconds(s core.RunSession, until time.Time) float64 {
	if s.ComputeSeconds > 0 {
		return s.ComputeSeconds
	}
	from := s.AssignedAt
	if s.StartedAt != nil {
		from = *s.StartedAt
	}
	end := until
	if s.EndedAt != nil {
		end = *s.EndedAt
	}
	if from.IsZero() || !end.After(from) {
		return 0
	}
	return end.Sub(from).Seconds()
}

// collectFacts loads the pull request facts of the PRs of the work items.
func collectFacts(ctx context.Context, store core.Store, factoryID string, d *Data) error {
	ps, ok := store.(core.PullRequestFactStore)
	if !ok {
		return nil
	}
	f, err := store.GetFactory(ctx, factoryID)
	if err != nil {
		return fmt.Errorf("get factory: %w", err)
	}
	seen := map[string]bool{}
	var keys []string
	for _, w := range d.WorkItems {
		for _, pr := range w.PullRequests {
			if pr.Repo == "" || pr.Number == 0 {
				continue
			}
			k := core.PullRequestSource(pr).Key()
			if !seen[strings.ToLower(k)] {
				seen[strings.ToLower(k)] = true
				keys = append(keys, k)
			}
		}
	}
	d.Facts = map[string]core.PullRequestFacts{}
	for len(keys) > 0 {
		n := min(len(keys), 200)
		facts, err := ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{TeamID: f.TeamID, Keys: keys[:n]})
		if err != nil {
			return fmt.Errorf("list pull request facts: %w", err)
		}
		for _, x := range facts {
			d.Facts[strings.ToLower(x.Key)] = x
		}
		keys = keys[n:]
	}
	// Facts whose repository is spelled in another case.
	if len(d.Facts) < len(seen) {
		all, err := ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{TeamID: f.TeamID})
		if err != nil {
			return fmt.Errorf("list pull request facts: %w", err)
		}
		for _, x := range all {
			if k := strings.ToLower(x.Key); seen[k] {
				d.Facts[k] = x
			}
		}
	}
	return nil
}

func needsEvents(w core.WorkItem, since, until time.Time) bool {
	if inWindow(w.CreatedAt, since, until) {
		return true
	}
	if w.CompletedAt != nil && inWindow(*w.CompletedAt, since, until) {
		return true
	}
	return len(w.PullRequests) > 0 && !w.UpdatedAt.Before(since)
}

func inWindow(t, since, until time.Time) bool {
	return !t.Before(since) && t.Before(until)
}

// Build derives the dashboard from raw data. It is pure and deterministic.
func Build(factoryID string, d *Data, since, until time.Time) *Dashboard {
	since, until = since.UTC(), until.UTC()
	if d == nil {
		d = &Data{}
	}
	db := &Dashboard{FactoryID: factoryID, Since: since, Until: until}
	daily := newDaily(since, until)

	inboxByWI := map[string][]core.InboxItem{}
	for _, it := range d.Inbox {
		if it.WorkItemID != "" {
			inboxByWI[it.WorkItemID] = append(inboxByWI[it.WorkItemID], it)
		}
	}

	// ----- Work items, autonomy, cycle time -----
	db.WorkItems.ByStage = map[core.Stage]int{}
	db.CycleTime.StageMedianMS = map[core.Stage]int64{}
	autonomous := map[string]bool{}
	var cycle []int64
	stageDur := map[core.Stage][]int64{}
	for _, w := range d.WorkItems {
		ev := sortedEvents(d.Events[w.ID])
		autonomous[w.ID] = isAutonomous(w, ev, inboxByWI[w.ID])
		if inWindow(w.CreatedAt, since, until) {
			db.WorkItems.Created++
			db.WorkItems.ByStage[w.Stage]++
			daily.at(w.CreatedAt, func(p *DailyPoint) { p.WorkItemsCreated++ })
		}
		if w.CompletedAt == nil || !inWindow(*w.CompletedAt, since, until) {
			continue
		}
		switch w.Stage {
		case core.StageCancelled:
			db.WorkItems.Cancelled++
		case core.StageComplete:
			db.WorkItems.Completed++
			daily.at(*w.CompletedAt, func(p *DailyPoint) { p.WorkItemsCompleted++ })
			db.Autonomy.CompletedWorkItems++
			if autonomous[w.ID] {
				db.Autonomy.AutonomousWorkItems++
			}
			if dur := w.CompletedAt.Sub(w.CreatedAt); dur >= 0 {
				cycle = append(cycle, dur.Milliseconds())
			}
			for st, ms := range stageDurations(w, ev) {
				stageDur[st] = append(stageDur[st], ms)
			}
		}
	}
	db.Autonomy.Rate = ratio(db.Autonomy.AutonomousWorkItems, db.Autonomy.CompletedWorkItems)
	db.CycleTime.Count = len(cycle)
	db.CycleTime.MedianMS = median(cycle)
	db.CycleTime.P90MS = percentile(cycle, 90)
	db.CycleTime.MeanMS = mean(cycle)
	for st, xs := range stageDur {
		db.CycleTime.StageMedianMS[st] = median(xs)
	}

	// ----- Run cost per work item -----
	wiCost := map[string]float64{}
	for _, r := range d.Runs {
		if r.WorkItemID != "" {
			wiCost[r.WorkItemID] += r.Usage.CostUSD
		}
	}

	// ----- Pull requests -----
	prs := collectPRs(d.WorkItems, d.Events, wiCost, d.Facts)
	var prCosts []float64
	var timeToPR []int64
	sizeCosts := map[string][]float64{}
	for _, p := range prs {
		if !inWindow(p.OpenedAt, since, until) {
			continue
		}
		db.PullRequests.Opened++
		prCosts = append(prCosts, p.CostUSD)
		db.PullRequests.TotalCostUSD += p.CostUSD
		db.PullRequests.MostExpensive = append(db.PullRequests.MostExpensive, p.PRCost)
		if p.wiCreated.Before(p.OpenedAt) || p.wiCreated.Equal(p.OpenedAt) {
			timeToPR = append(timeToPR, p.OpenedAt.Sub(p.wiCreated).Milliseconds())
		}
		sizeCosts[prSize(p.facts)] = append(sizeCosts[prSize(p.facts)], p.CostUSD)
		merged := strings.EqualFold(p.State, "merged")
		switch {
		case merged:
			db.PullRequests.Merged++
		case strings.EqualFold(p.State, "closed"):
			db.PullRequests.Closed++
		default:
			db.PullRequests.Open++
		}
		daily.at(p.OpenedAt, func(dp *DailyPoint) {
			dp.PRsOpened++
			if merged {
				dp.PRsMerged++
			}
		})
	}
	db.PullRequests.MergeRate = ratio(db.PullRequests.Merged, db.PullRequests.Opened)
	db.PullRequests.MedianCostUSD = medianFloat(prCosts)
	db.PullRequests.MedianTimeToPRMS = median(timeToPR)
	sort.SliceStable(db.PullRequests.MostExpensive, func(i, j int) bool {
		a, b := db.PullRequests.MostExpensive[i], db.PullRequests.MostExpensive[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return a.OpenedAt.Before(b.OpenedAt)
	})
	if len(db.PullRequests.MostExpensive) > MostExpensiveLimit {
		db.PullRequests.MostExpensive = db.PullRequests.MostExpensive[:MostExpensiveLimit]
	}
	if db.PullRequests.MostExpensive == nil {
		db.PullRequests.MostExpensive = []PRCost{}
	}
	db.PullRequests.BySize = sizeBuckets(sizeCosts)
	buildPRFacts(db, prs, since, until)
	db.Autonomy.PRRate = ratio(db.Autonomy.AutonomousMergedPRs, db.Autonomy.MergedPRs)

	// ----- Human wait -----
	db.HumanWait.ByKind = map[core.InboxKind]int{}
	var waits []int64
	for _, it := range d.Inbox {
		if it.Kind == core.InboxReview || !inWindow(it.CreatedAt, since, until) {
			continue
		}
		db.HumanWait.Items++
		db.HumanWait.ByKind[it.Kind]++
		if it.Resolved && it.ResolvedAt != nil {
			w := it.ResolvedAt.Sub(it.CreatedAt).Milliseconds()
			if w < 0 {
				w = 0
			}
			waits = append(waits, w)
			db.HumanWait.TotalMS += w
		} else {
			db.HumanWait.Pending++
			if w := until.Sub(it.CreatedAt).Milliseconds(); w > 0 {
				db.HumanWait.TotalMS += w
			}
		}
	}
	db.HumanWait.MedianMS = median(waits)
	db.HumanWait.P90MS = percentile(waits, 90)

	// ----- Runs & cost -----
	db.Runs = RunStats{
		ByAgent:      map[string]int{},
		ByState:      map[core.RunState]int{},
		ByAgentState: map[string]map[core.RunState]int{},
		ByModel:      map[string]int{},
		BySource:     map[string]int{},
	}
	db.Cost = CostStats{ByAgent: map[string]UsageTotals{}, ByModel: map[string]UsageTotals{}, ComputeByShape: map[string]ComputeStats{}}
	for _, r := range d.Runs {
		if !inWindow(r.CreatedAt, since, until) {
			continue
		}
		agent := runAgent(r)
		model := runModel(r)
		db.Runs.Total++
		db.Runs.ByAgent[agent]++
		db.Runs.ByState[r.State]++
		if db.Runs.ByAgentState[agent] == nil {
			db.Runs.ByAgentState[agent] = map[core.RunState]int{}
		}
		db.Runs.ByAgentState[agent][r.State]++
		db.Runs.ByModel[model]++
		db.Runs.BySource[runSource(r)]++

		var compute float64
		for _, ss := range d.Sessions[r.ID] {
			sec := sessionSeconds(ss, until)
			compute += sec
			label := ss.InstanceShape.Label()
			c := db.Cost.ComputeByShape[label]
			c.Sessions++
			c.Seconds += sec
			db.Cost.ComputeByShape[label] = c
		}
		db.Cost.Total.add(r.Usage)
		db.Cost.Total.ComputeSeconds += compute
		ua := db.Cost.ByAgent[agent]
		ua.add(r.Usage)
		ua.ComputeSeconds += compute
		db.Cost.ByAgent[agent] = ua
		um := db.Cost.ByModel[model]
		um.add(r.Usage)
		um.ComputeSeconds += compute
		db.Cost.ByModel[model] = um

		daily.at(r.CreatedAt, func(p *DailyPoint) {
			p.Runs++
			p.CostUSD += r.Usage.CostUSD
			p.InputTokens += r.Usage.InputTokens
			p.OutputTokens += r.Usage.OutputTokens
		})
	}

	// ----- Scorers -----
	byScorer := map[string]*ScorerStats{}
	for _, sc := range d.Scorers {
		byScorer[sc.ID] = &ScorerStats{ScorerID: sc.ID, Slug: sc.Slug, Name: sc.Name, Labels: map[string]int{}}
	}
	for _, s := range d.Scores {
		if !inWindow(s.CreatedAt, since, until) {
			continue
		}
		st := byScorer[s.ScorerID]
		if st == nil {
			st = &ScorerStats{ScorerID: s.ScorerID, Labels: map[string]int{}}
			byScorer[s.ScorerID] = st
		}
		st.Total++
		if s.Passed {
			st.Passed++
		}
		st.Labels[s.Label]++
		daily.at(s.CreatedAt, func(p *DailyPoint) {
			p.Scores++
			if s.Passed {
				p.ScoresPassed++
			}
		})
	}
	db.Scorers = make([]ScorerStats, 0, len(byScorer))
	for _, st := range byScorer {
		st.PassRate = ratio(st.Passed, st.Total)
		db.Scorers = append(db.Scorers, *st)
	}
	sort.Slice(db.Scorers, func(i, j int) bool {
		a, b := db.Scorers[i], db.Scorers[j]
		if a.Slug != b.Slug {
			return a.Slug < b.Slug
		}
		return a.ScorerID < b.ScorerID
	})

	db.Daily = daily.points
	return db
}

// ---------- Autonomy ----------

// isAutonomous applies the package-level autonomy definition.
func isAutonomous(w core.WorkItem, events []core.WorkItemEvent, inbox []core.InboxItem) bool {
	if w.LocalOwner != "" {
		return false
	}
	for _, it := range inbox {
		switch it.Kind {
		case core.InboxQuestion, core.InboxHandoff, core.InboxFailure:
			return false
		}
	}
	started, firstMessage := false, false
	for _, e := range events {
		switch e.Kind {
		case "question", "answer":
			return false
		case "run_started":
			started = true
		case "message_in":
			if started || firstMessage {
				return false
			}
			firstMessage = true
		}
	}
	return true
}

// ---------- Stages ----------

// stageDurations returns time spent per stage, from stage_changed events.
// The item starts in intake at CreatedAt; the final stage ends at CompletedAt.
func stageDurations(w core.WorkItem, events []core.WorkItemEvent) map[core.Stage]int64 {
	out := map[core.Stage]int64{}
	cur := core.StageIntake
	at := w.CreatedAt
	seen := false
	for _, e := range events {
		if e.Kind != "stage_changed" {
			continue
		}
		to := stageFromEvent(e)
		if to == "" {
			continue
		}
		seen = true
		if d := e.CreatedAt.Sub(at); d > 0 {
			out[cur] += d.Milliseconds()
		}
		cur, at = to, e.CreatedAt
	}
	if !seen {
		return nil
	}
	if w.CompletedAt != nil && !cur.Terminal() {
		if d := w.CompletedAt.Sub(at); d > 0 {
			out[cur] += d.Milliseconds()
		}
	}
	return out
}

func stageFromEvent(e core.WorkItemEvent) core.Stage {
	if len(e.Data) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(e.Data, &m); err != nil {
		var s string
		if json.Unmarshal(e.Data, &s) == nil {
			return core.Stage(s)
		}
		return ""
	}
	for _, k := range []string{"to", "stage", "new_stage"} {
		if s, ok := m[k].(string); ok && s != "" {
			return core.Stage(s)
		}
	}
	return ""
}

// ---------- Pull requests ----------

type prEntry struct {
	PRCost
	wiCreated time.Time
	facts     *core.PullRequestFacts
}

// collectPRs deduplicates PRs across work items and attributes first
// observation time and cost.
func collectPRs(items []core.WorkItem, events map[string][]core.WorkItemEvent, wiCost map[string]float64, facts map[string]core.PullRequestFacts) []prEntry {
	byKey := map[string]*prEntry{}
	var order []string
	for _, w := range items {
		if len(w.PullRequests) == 0 {
			continue
		}
		linked := prLinkTimes(events[w.ID])
		share := wiCost[w.ID] / float64(len(w.PullRequests))
		for _, pr := range w.PullRequests {
			key := prKey(pr)
			if key == "" {
				continue
			}
			opened, ok := linked.find(pr)
			if !ok {
				opened = w.CreatedAt
			}
			e := byKey[key]
			if e == nil {
				e = &prEntry{PRCost: PRCost{Forge: pr.Forge, Repo: pr.Repo, Number: pr.Number, URL: pr.URL, State: pr.State, WorkItemID: w.ID, OpenedAt: opened}, wiCreated: w.CreatedAt}
				byKey[key] = e
				order = append(order, key)
				e.CostUSD = share
				continue
			}
			// Seen on another work item: keep the earliest observation, sum cost.
			e.CostUSD += share
			if opened.Before(e.OpenedAt) {
				e.OpenedAt, e.WorkItemID, e.wiCreated = opened, w.ID, w.CreatedAt
			}
			if stateRank(pr.State) > stateRank(e.State) {
				e.State = pr.State
			}
		}
	}
	out := make([]prEntry, 0, len(order))
	for _, k := range order {
		e := *byKey[k]
		if f, ok := facts[PRFactsKey(core.PullRequestRef{Forge: e.Forge, Repo: e.Repo, Number: e.Number})]; ok {
			fc := f
			e.facts = &fc
			// The forge knows when the PR opened.
			if f.OpenedAt != nil {
				e.OpenedAt = f.OpenedAt.UTC()
			}
			if f.MergedAt != nil {
				e.State = "merged"
			}
		}
		out = append(out, e)
	}
	return out
}

// prSize returns the size bucket of a PR.
func prSize(f *core.PullRequestFacts) string {
	if f == nil {
		return "unknown"
	}
	n, ok := f.ChangedLines()
	if !ok {
		return "unknown"
	}
	for _, s := range prSizes {
		if s.Max == 0 || n < s.Max {
			return s.Size
		}
	}
	return "xl"
}

func sizeBuckets(costs map[string][]float64) []PRSizeCost {
	out := make([]PRSizeCost, 0, len(prSizes)+1)
	lo := 0
	for _, s := range prSizes {
		lines := fmt.Sprintf("%d-%d", lo, s.Max-1)
		if s.Max == 0 {
			lines = fmt.Sprintf("%d+", lo)
		}
		out = append(out, sizeCost(s.Size, lines, costs[s.Size]))
		lo = s.Max
	}
	return append(out, sizeCost("unknown", "", costs["unknown"]))
}

func sizeCost(size, lines string, xs []float64) PRSizeCost {
	c := PRSizeCost{Size: size, Lines: lines, Count: len(xs), MedianCostUSD: medianFloat(xs)}
	for _, x := range xs {
		c.TotalCostUSD += x
	}
	return c
}

// buildPRFacts derives the PR autonomy and the PR cycle time from the pull
// request facts of the PRs that merged in the window.
func buildPRFacts(db *Dashboard, prs []prEntry, since, until time.Time) {
	var startToMerge, startToPR, prToReview, reviewToMerge []int64
	for _, p := range prs {
		f := p.facts
		if f != nil && f.OpenedAt != nil {
			if t := f.OpenedAt.UTC(); db.Autonomy.FactsSince == nil || t.Before(*db.Autonomy.FactsSince) {
				db.Autonomy.FactsSince = &t
			}
		}
		if f == nil || f.MergedAt == nil {
			if f == nil && strings.EqualFold(p.State, "merged") && inWindow(p.OpenedAt, since, until) {
				db.Autonomy.MergedPRsWithoutFacts++
			}
			continue
		}
		merged := f.MergedAt.UTC()
		if !inWindow(merged, since, until) {
			continue
		}
		db.Autonomy.MergedPRs++
		if !f.PersonPushedBefore(merged) {
			db.Autonomy.AutonomousMergedPRs++
		}
		db.CycleTime.PullRequests.Count++
		ms := func(a, b time.Time) (int64, bool) {
			if b.Before(a) {
				return 0, false
			}
			return b.Sub(a).Milliseconds(), true
		}
		if v, ok := ms(p.wiCreated, merged); ok {
			startToMerge = append(startToMerge, v)
		}
		opened := p.OpenedAt
		if v, ok := ms(p.wiCreated, opened); ok {
			startToPR = append(startToPR, v)
		}
		if f.FirstReviewAt != nil {
			db.CycleTime.PullRequests.Reviewed++
			rev := f.FirstReviewAt.UTC()
			if v, ok := ms(opened, rev); ok {
				prToReview = append(prToReview, v)
			}
			if v, ok := ms(rev, merged); ok {
				reviewToMerge = append(reviewToMerge, v)
			}
		}
	}
	db.CycleTime.PullRequests.StartToMergeMS = median(startToMerge)
	db.CycleTime.PullRequests.StartToPRMS = median(startToPR)
	db.CycleTime.PullRequests.PRToFirstReviewMS = median(prToReview)
	db.CycleTime.PullRequests.FirstReviewToMergeMS = median(reviewToMerge)
}

func stateRank(s string) int {
	switch strings.ToLower(s) {
	case "merged":
		return 3
	case "closed":
		return 2
	case "open":
		return 1
	}
	return 0
}

func prKey(pr core.PullRequestRef) string {
	if pr.Repo != "" && pr.Number != 0 {
		return strings.ToLower(pr.Forge + "|" + pr.Repo + "#" + strconv.Itoa(pr.Number))
	}
	return strings.ToLower(pr.URL)
}

type linkTimes struct {
	byKey map[string]time.Time
	// anon is the earliest pr_linked event whose data couldn't be matched.
	anon    time.Time
	hasAnon bool
}

func (l linkTimes) find(pr core.PullRequestRef) (time.Time, bool) {
	if t, ok := l.byKey[prKey(pr)]; ok {
		return t, true
	}
	if pr.URL != "" {
		if t, ok := l.byKey[strings.ToLower(pr.URL)]; ok {
			return t, true
		}
	}
	return l.anon, l.hasAnon
}

func prLinkTimes(events []core.WorkItemEvent) linkTimes {
	lt := linkTimes{byKey: map[string]time.Time{}}
	for _, e := range events {
		if e.Kind != "pr_linked" {
			continue
		}
		var keys []string
		if pr, ok := parsePR(e.Data); ok {
			if k := prKey(pr); k != "" {
				keys = append(keys, k)
			}
			if pr.URL != "" {
				keys = append(keys, strings.ToLower(pr.URL))
			}
		}
		if len(keys) == 0 {
			if !lt.hasAnon || e.CreatedAt.Before(lt.anon) {
				lt.anon, lt.hasAnon = e.CreatedAt, true
			}
			continue
		}
		for _, k := range keys {
			if t, ok := lt.byKey[k]; !ok || e.CreatedAt.Before(t) {
				lt.byKey[k] = e.CreatedAt
			}
		}
	}
	return lt
}

// parsePR reads a PullRequestRef from event data, either at the top level or
// under "pull_request"/"pr".
func parsePR(data json.RawMessage) (core.PullRequestRef, bool) {
	if len(data) == 0 {
		return core.PullRequestRef{}, false
	}
	var pr core.PullRequestRef
	if json.Unmarshal(data, &pr) == nil && (pr.URL != "" || (pr.Repo != "" && pr.Number != 0)) {
		return pr, true
	}
	var wrap map[string]json.RawMessage
	if json.Unmarshal(data, &wrap) != nil {
		return core.PullRequestRef{}, false
	}
	for _, k := range []string{"pull_request", "pr"} {
		if raw, ok := wrap[k]; ok {
			var inner core.PullRequestRef
			if json.Unmarshal(raw, &inner) == nil && (inner.URL != "" || (inner.Repo != "" && inner.Number != 0)) {
				return inner, true
			}
		}
	}
	return core.PullRequestRef{}, false
}

// ---------- Runs ----------

func runAgent(r core.Run) string {
	switch {
	case r.AgentName != "":
		return r.AgentName
	case r.Config.Name != "":
		return r.Config.Name
	}
	return "(unknown)"
}

func runModel(r core.Run) string {
	if r.Config.Harness.Model != "" {
		return r.Config.Harness.Model
	}
	return "(default)"
}

// runSource returns the API source of the run, one value of the closed list
// core.RunSources. An empty or unknown TriggeredBy is API.
func runSource(r core.Run) string {
	src, _ := r.SourceAndRef()
	return string(src)
}

// ---------- Daily series ----------

type dailySeries struct {
	start  time.Time
	points []DailyPoint
}

const maxDailyPoints = 3660

func newDaily(since, until time.Time) *dailySeries {
	start := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	ds := &dailySeries{start: start}
	for d := start; d.Before(until) && len(ds.points) < maxDailyPoints; d = d.AddDate(0, 0, 1) {
		ds.points = append(ds.points, DailyPoint{Date: d.Format("2006-01-02")})
	}
	return ds
}

func (ds *dailySeries) at(t time.Time, fn func(*DailyPoint)) {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if day.Before(ds.start) {
		return
	}
	// Calendar-day index (DST-free in UTC).
	i := int(day.Sub(ds.start).Hours() / 24)
	if i >= 0 && i < len(ds.points) {
		fn(&ds.points[i])
	}
}

// ---------- Helpers ----------

func sortedEvents(ev []core.WorkItemEvent) []core.WorkItemEvent {
	out := append([]core.WorkItemEvent(nil), ev...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// percentile uses the nearest-rank method.
func percentile(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(s) {
		rank = len(s)
	}
	return s[rank-1]
}

func mean(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	var sum int64
	for _, x := range xs {
		sum += x
	}
	return sum / int64(len(xs))
}
