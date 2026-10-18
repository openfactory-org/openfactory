// Command devseed fills a dev server database with sample data (runs, work
// items, inbox items, scores, workers, environments and users) so that the
// dashboard can be checked with realistic content. Dev use only.
//
// Usage (after `make run-dev` or any OF_DEV server created the team and some
// factories):
//
//	go run ./scripts/devseed -db "$OF_DATA_DIR/openfactory.db"
//
// The data is random but repeatable (-seed). Run it once per database.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store"
)

func main() {
	db := flag.String("db", "", "store URL or SQLite path of the dev server")
	seed := flag.Int64("seed", 7, "random seed")
	days := flag.Int("days", 14, "days of history to create")
	flag.Parse()
	if *db == "" {
		fmt.Fprintln(os.Stderr, "devseed: -db is required")
		os.Exit(2)
	}
	if err := run(context.Background(), *db, *seed, *days); err != nil {
		fmt.Fprintln(os.Stderr, "devseed:", err)
		os.Exit(1)
	}
}

type seeder struct {
	ctx   context.Context
	s     core.Store
	r     *rand.Rand
	now   time.Time
	team  core.Team
	users []core.User
	runs  int
}

func run(ctx context.Context, url string, seed int64, days int) error {
	s, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer s.Close()
	teams, err := s.ListTeams(ctx)
	if err != nil {
		return fmt.Errorf("list teams: %w", err)
	}
	if len(teams) == 0 {
		return fmt.Errorf("no team in the database: start the server with OF_DEV=true first")
	}
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // repeatable sample data, not for security
	sd := &seeder{ctx: ctx, s: s, r: rng, now: time.Now().UTC(), team: teams[0]}
	if err := sd.seedUsers(); err != nil {
		return err
	}
	if err := sd.seedTeamResources(); err != nil {
		return err
	}
	facs, err := s.ListFactories(ctx, sd.team.ID, "")
	if err != nil {
		return err
	}
	if len(facs) == 0 {
		return fmt.Errorf("no factory: create some first (openfactory factory create)")
	}
	for i := range facs {
		if err := sd.seedFactory(&facs[i], days); err != nil {
			return fmt.Errorf("factory %s: %w", facs[i].Name, err)
		}
	}
	fmt.Printf("devseed: %d factories, %d runs, %d users\n", len(facs), sd.runs, len(sd.users))
	return nil
}

func (sd *seeder) seedUsers() error {
	people := []struct {
		name, email string
		role        core.Role
	}{
		{"Maya Chen", "maya@acme.test", core.RoleEditor},
		{"Jonas Weber", "jonas@acme.test", core.RoleUser},
		{"Priya Raman", "priya@acme.test", core.RoleEditor},
	}
	for _, p := range people {
		if _, err := sd.s.GetUserByEmail(sd.ctx, p.email); err == nil {
			continue
		}
		u := &core.User{ID: core.NewID("usr"), TeamID: sd.team.ID, Email: p.email, Name: p.name, Role: p.role}
		if err := sd.s.CreateUser(sd.ctx, u); err != nil {
			return fmt.Errorf("user %s: %w", p.email, err)
		}
	}
	us, err := sd.s.ListUsers(sd.ctx, sd.team.ID)
	if err != nil {
		return err
	}
	for _, u := range us {
		if !u.IsServiceAccount() {
			sd.users = append(sd.users, u)
		}
	}
	return nil
}

func (sd *seeder) seedTeamResources() error {
	workers := []core.Worker{
		{Name: "build-pool-a", Backend: "kubernetes", Capacity: 8, Active: 3, Labels: map[string]string{"region": "eu-west-1", "arch": "amd64"}},
		{Name: "build-pool-b", Backend: "kubernetes", Capacity: 8, Active: 1, Labels: map[string]string{"region": "us-east-1", "arch": "arm64"}},
		{Name: "gpu-runner", Backend: "direct", Capacity: 2, Active: 0, Labels: map[string]string{"gpu": "l4"}},
	}
	for i, w := range workers {
		w.ID = core.NewID("wrk")
		w.TeamID = sd.team.ID
		w.Version = "5d0bb88"
		w.LastHeartbeat = sd.now.Add(-time.Duration(5+i*40) * time.Second)
		if i == 2 {
			w.LastHeartbeat = sd.now.Add(-26 * time.Minute) // a stale worker
		}
		if err := sd.s.UpsertWorker(sd.ctx, &w); err != nil {
			return fmt.Errorf("worker: %w", err)
		}
	}
	envs := []core.Environment{
		{Name: "payments-ci", Description: "Go 1.24, PostgreSQL 16 and the payments monorepo", Repos: []string{"github:acme/payments"}, Image: "ghcr.io/acme/payments-ci:2026.11", SetupCommands: []string{"make deps", "make db-up"}, EnvVars: map[string]string{"GOFLAGS": "-mod=mod"}},
		{Name: "web-node22", Description: "Node 22 with pnpm for the web app", Repos: []string{"github:acme/web", "github:acme/docs"}, SetupCommands: []string{"corepack enable", "pnpm install --frozen-lockfile"}},
	}
	existing, _ := sd.s.ListEnvironments(sd.ctx, sd.team.ID)
	if len(existing) > 0 {
		return nil
	}
	for _, e := range envs {
		e.ID = core.NewID("env")
		e.TeamID = sd.team.ID
		if err := sd.s.CreateEnvironment(sd.ctx, &e); err != nil {
			return fmt.Errorf("environment: %w", err)
		}
	}
	return nil
}

var titles = []string{
	"Retry webhook deliveries with exponential backoff",
	"Fix flaky TestLedgerReconcile on PostgreSQL",
	"Add idempotency keys to the refunds endpoint",
	"Upgrade the Stripe SDK to v82",
	"Dark mode for the settings page",
	"Paginate the invoices list API",
	"Remove the deprecated v1 export job",
	"Speed up the CI cache restore step",
	"Handle currency rounding for JPY",
	"Add audit log entries for API key changes",
	"Crash on empty CSV import",
	"Document the payouts state machine",
	"Rate limit the login endpoint",
	"Migrate cron jobs to the scheduler",
	"Show the last sync time on the dashboard",
	"Translate error messages to French",
	"Fix N+1 query in the customers page",
	"Add a health check for the queue worker",
	"Bump Go to 1.24 in all images",
	"Investigate p99 latency spike on checkout",
	"Support SEPA direct debit mandates",
	"Clean up feature flags older than 90 days",
}

var prompts = []string{
	"Implement the change described in the issue and open a pull request.",
	"Review the pull request and leave comments for anything risky.",
	"Triage the new issue: label it, estimate it and find the owner.",
	"Plan the work: list the files to change and the tests to add.",
	"Address the review comments and push a new commit.",
	"Investigate the failing check and propose a fix.",
}

var results = []string{
	"Opened a pull request with the change and 4 new tests. All checks pass.",
	"Left 3 review comments: a missing nil check, an unbounded loop and a typo in the migration name.",
	"Labeled as bug, priority P2. The owner is the billing team. Estimated at 2 points.",
	"Plan ready: change 3 files in internal/ledger, add a table test and update the docs.",
	"Pushed a commit that addresses all 5 review comments.",
	"The check failed because the test depends on the local time zone. The fix sets TZ=UTC in the test.",
}

var failures = []struct {
	code core.FailureCode
	msg  string
}{
	{core.FailureAgentFailed, "the agent stopped: tests still fail after 3 attempts"},
	{core.FailureHarnessAuthFailed, "the harness rejected the API key (401)"},
	{core.FailureSandboxCreateFailed, "no node in the pool has 8 GiB of free memory"},
	{core.FailureInfrastructureTimeout, "the sandbox did not start within 10 minutes"},
}

var sources = []core.RunSource{core.SourceAutomation, core.SourceAutomation, core.SourceMention, core.SourceDashboard, core.SourceWebhook, core.SourceSchedule, core.SourceOrchestration}

func (sd *seeder) pick(n int) int { return sd.r.Intn(n) }

func (sd *seeder) seedFactory(f *core.Factory, days int) error {
	agents, err := sd.s.ListAgents(sd.ctx, f.ID)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return nil
	}
	repo := "acme/" + strings.ToLower(strings.ReplaceAll(f.Name, " ", "-"))
	creator := sd.users[sd.pick(len(sd.users))]

	// Work items with a foreman run and child runs each.
	nItems := 6 + sd.pick(5)
	stages := []core.Stage{core.StageComplete, core.StageComplete, core.StageComplete, core.StageBuilding, core.StageReviewing, core.StageAwaitingApproval, core.StagePlanning, core.StageTriage, core.StageIntake, core.StageCancelled, core.StageHandoff}
	for i := 0; i < nItems; i++ {
		age := time.Duration(sd.r.Float64()*float64(days)*24) * time.Hour
		if i < 3 {
			age = time.Duration(10+sd.pick(300)) * time.Minute // some recent items
		}
		created := sd.now.Add(-age)
		stage := stages[sd.pick(len(stages))]
		num := 100 + sd.pick(900)
		wi := &core.WorkItem{
			ID:        core.NewID("wi"),
			FactoryID: f.ID,
			Title:     titles[sd.pick(len(titles))],
			Stage:     stage,
			Source:    core.SourceRef{Provider: "github", Kind: "issue", Repo: repo, Number: num, URL: fmt.Sprintf("https://github.com/%s/issues/%d", repo, num)},
			SourceKey: fmt.Sprintf("github:%s#%d", repo, num),
			CreatorID: creator.ID,
			Labels:    map[string]string{"priority": []string{"P1", "P2", "P3"}[sd.pick(3)]},
			CreatedAt: created,
			UpdatedAt: created.Add(time.Duration(sd.pick(180)) * time.Minute),
		}
		if stage == core.StageComplete || stage == core.StageReviewing || stage == core.StageHandoff {
			state := "open"
			if stage == core.StageComplete {
				state = "merged"
			}
			pr := 1000 + sd.pick(9000)
			wi.PullRequests = []core.PullRequestRef{{Forge: "github", Repo: repo, Number: pr, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, pr), Branch: fmt.Sprintf("of/wi-%d", num), State: state}}
		}
		if stage == core.StageComplete || stage == core.StageCancelled {
			t := wi.UpdatedAt
			wi.CompletedAt = &t
		}
		if err := sd.s.CreateWorkItem(sd.ctx, wi); err != nil {
			return fmt.Errorf("work item: %w", err)
		}
		_ = sd.s.AppendWorkItemEvent(sd.ctx, &core.WorkItemEvent{ID: core.NewID("wie"), WorkItemID: wi.ID, Kind: "stage_changed", Actor: "foreman", Data: mustJSON(map[string]string{"from": "intake", "to": string(stage)}), CreatedAt: wi.UpdatedAt})

		terminal := stage == core.StageComplete || stage == core.StageCancelled
		foreman := sd.newRun(f, agents[0], wi, nil, created, terminal, creator)
		if err := sd.saveRun(foreman); err != nil {
			return err
		}
		wi.RootRunID = foreman.ID
		if err := sd.s.UpdateWorkItem(sd.ctx, wi); err != nil {
			return fmt.Errorf("work item update: %w", err)
		}
		for c := 0; c < 1+sd.pick(3) && len(agents) > 1; c++ {
			a := agents[1+sd.pick(len(agents)-1)]
			child := sd.newRun(f, a, wi, foreman, created.Add(time.Duration(c*7+2)*time.Minute), terminal || c > 0, creator)
			if err := sd.saveRun(child); err != nil {
				return err
			}
		}

		if stage == core.StageAwaitingApproval {
			if err := sd.s.CreateInboxItem(sd.ctx, &core.InboxItem{
				ID: core.NewID("inb"), FactoryID: f.ID, WorkItemID: wi.ID, RunID: foreman.ID, Kind: core.InboxApproval,
				Title:   "Approve the plan for: " + wi.Title,
				Body:    "The planner proposes changing 3 files and adding 2 tests.\n\n1. Add the retry policy to `internal/webhooks/deliver.go`.\n2. Store the attempt count on the delivery.\n3. Add a table test for the backoff values.",
				Options: []string{"Approve", "Request changes"}, Recipients: []string{creator.ID}, CreatedAt: wi.UpdatedAt,
			}); err != nil {
				return fmt.Errorf("inbox: %w", err)
			}
		}
	}

	// Ad-hoc runs without a work item spread across the window (for the charts).
	for i := 0; i < 18+sd.pick(14); i++ {
		created := sd.now.Add(-time.Duration(sd.r.Float64() * float64(days) * 24 * float64(time.Hour)))
		a := agents[sd.pick(len(agents))]
		if err := sd.saveRun(sd.newRun(f, a, nil, nil, created, true, sd.users[sd.pick(len(sd.users))])); err != nil {
			return err
		}
	}

	// Open inbox items of other kinds.
	q := &core.InboxItem{
		ID: core.NewID("inb"), FactoryID: f.ID, Kind: core.InboxQuestion,
		Title:     "Which database should the new report job read from?",
		Body:      "The reporting replica lags by up to 5 minutes. The primary is accurate but adds load during business hours.",
		Questions: []core.Question{{ID: "q1", Text: "Database for the report job", Type: "single_select", Options: []string{"Reporting replica", "Primary"}, RecommendedOption: "Reporting replica"}},
		CreatedAt: sd.now.Add(-42 * time.Minute),
	}
	fail := &core.InboxItem{
		ID: core.NewID("inb"), FactoryID: f.ID, Kind: core.InboxFailure,
		Title:   "Run failed: harness rejected the API key",
		Body:    "The builder run stopped with harness_auth_failed. Rotate the ANTHROPIC_API_KEY secret and retry.",
		Options: []string{"Retry", "Dismiss"}, CreatedAt: sd.now.Add(-3 * time.Hour),
	}
	done := sd.now.Add(-20 * time.Hour)
	resolved := &core.InboxItem{
		ID: core.NewID("inb"), FactoryID: f.ID, Kind: core.InboxHandoff,
		Title: "Hand off: the migration needs a DBA review", Body: "The change adds an index on a 40 GB table.",
		Resolved: true, Resolution: "Reviewed, run it off-peak.", ResolvedBy: creator.ID, ResolvedVia: "dashboard",
		CreatedAt: done.Add(-2 * time.Hour), ResolvedAt: &done,
	}
	for _, it := range []*core.InboxItem{q, fail, resolved} {
		if err := sd.s.CreateInboxItem(sd.ctx, it); err != nil {
			return fmt.Errorf("inbox: %w", err)
		}
	}

	return sd.seedScores(f)
}

func (sd *seeder) newRun(f *core.Factory, a core.AgentConfig, wi *core.WorkItem, parent *core.Run, created time.Time, terminal bool, creator core.User) *core.Run {
	r := &core.Run{
		ID: core.NewID("run"), TeamID: sd.team.ID, FactoryID: f.ID, AgentID: a.ID, AgentName: a.Name, Name: a.Name,
		Prompt: prompts[sd.pick(len(prompts))], Config: a, CreatedAt: created,
		Source:  sources[sd.pick(len(sources))],
		Creator: &core.Principal{Type: core.PrincipalUser, ID: creator.ID, Name: creator.Name},
		RunAs:   &core.Principal{Type: core.PrincipalFactory, ID: f.ID, Name: f.Name},
	}
	if wi != nil {
		r.WorkItemID = wi.ID
	}
	if parent != nil {
		r.ParentRunID = parent.ID
		r.Source = core.SourceOrchestration
		r.SourceRef = parent.ID
	}
	r.TriggeredBy = strings.ToLower(string(r.Source))
	start := created.Add(time.Duration(2+sd.pick(40)) * time.Second)
	r.StartedAt = &start
	dur := time.Duration(40+sd.pick(1500)) * time.Second
	in := int64(8000 + sd.pick(240000))
	out := int64(600 + sd.pick(18000))
	r.Usage = core.Usage{InputTokens: in, OutputTokens: out, CostUSD: float64(in)*3e-6 + float64(out)*15e-6, DurationMS: dur.Milliseconds()}
	r.Turns = int64(1 + sd.pick(6))
	if !terminal {
		switch sd.pick(4) {
		case 0:
			r.State = core.RunBlocked
			r.BlockedReason = "human_request"
		case 1:
			r.State = core.RunQueued
			r.StartedAt = nil
			r.Usage = core.Usage{}
		default:
			r.State = core.RunRunning
		}
		return r
	}
	end := start.Add(dur)
	r.FinishedAt = &end
	switch p := sd.pick(100); {
	case p < 78:
		r.State = core.RunSucceeded
		r.Result = results[sd.pick(len(results))]
	case p < 92:
		fl := failures[sd.pick(len(failures))]
		r.State = core.RunFailed
		r.FailureCode = fl.code
		r.Error = fl.msg
		if fl.code == core.FailureSandboxCreateFailed || fl.code == core.FailureInfrastructureTimeout {
			r.State = core.RunError
		}
	default:
		r.State = core.RunCancelled
		r.CancelledBy = "user:" + creator.ID
	}
	return r
}

func (sd *seeder) saveRun(r *core.Run) error {
	if err := sd.s.CreateRun(sd.ctx, r); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	sd.runs++
	t := r.CreatedAt
	step := func() time.Time { t = t.Add(time.Duration(3+sd.pick(40)) * time.Second); return t }
	msgs := []core.RunMessage{
		{Role: "user", Content: r.Prompt},
		{Role: "assistant", Content: "I will start by reading the relevant code and the failing tests."},
		{Role: "tool_call", Content: "Bash: go test ./internal/ledger/...", Data: mustJSON(map[string]any{"name": "Bash", "input": map[string]string{"command": "go test ./internal/ledger/..."}})},
		{Role: "tool_result", Content: "--- FAIL: TestReconcile (0.42s)\n    ledger_test.go:88: got 1999, want 2000\nFAIL\nFAIL\tgithub.com/acme/payments/internal/ledger\t0.913s"},
		{Role: "assistant", Content: "The rounding happens before the currency conversion. I will move it after the conversion and add a test for JPY."},
		{Role: "tool_call", Content: "Edit: internal/ledger/reconcile.go", Data: mustJSON(map[string]any{"name": "Edit", "input": map[string]string{"file_path": "internal/ledger/reconcile.go"}})},
		{Role: "tool_result", Content: "The file was updated (12 lines changed)."},
	}
	if r.State == core.RunSucceeded && r.Result != "" {
		msgs = append(msgs, core.RunMessage{Role: "assistant", Content: r.Result})
	}
	if r.Error != "" {
		msgs = append(msgs, core.RunMessage{Role: "system", Content: "Run failed: " + r.Error})
	}
	if r.State == core.RunQueued {
		msgs = msgs[:1]
	}
	for _, m := range msgs {
		m.ID = core.NewID("msg")
		m.RunID = r.ID
		m.CreatedAt = step()
		if err := sd.s.AppendRunMessage(sd.ctx, &m); err != nil {
			return fmt.Errorf("message: %w", err)
		}
	}
	return nil
}

func (sd *seeder) seedScores(f *core.Factory) error {
	scorers, err := sd.s.ListScorers(sd.ctx, f.ID)
	if err != nil || len(scorers) == 0 {
		return err
	}
	runs, err := sd.s.ListRuns(sd.ctx, core.RunFilter{FactoryID: f.ID, ListOpts: core.ListOpts{Limit: 200}})
	if err != nil {
		return err
	}
	rationales := []string{
		"The change is small, tested and matches the issue.",
		"The pull request fixes the bug but has no test for the edge case.",
		"The description does not explain why the migration is safe.",
		"Clear commits and the CI passes on the first try.",
	}
	for _, sc := range scorers {
		for _, r := range runs {
			if r.State != core.RunSucceeded || sd.pick(3) == 0 {
				continue
			}
			v := 0.45 + sd.r.Float64()*0.55
			label := "meets the bar"
			switch {
			case v < 0.6:
				label = "missing tests"
			case v < 0.7:
				label = "needs work"
			}
			if err := sd.s.CreateScore(sd.ctx, &core.Score{
				ID: core.NewID("score"), ScorerID: sc.ID, RunID: r.ID, AgentName: r.AgentName, Label: label,
				Score: v, Passed: v >= 0.7, Rationale: rationales[sd.pick(len(rationales))], CreatedAt: r.FinishedAt.Add(time.Minute),
			}); err != nil {
				return fmt.Errorf("score: %w", err)
			}
		}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
