package scoring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

const testFactory = "fac_1"

type env struct {
	st     *fakeStore
	eng    *fakeEngine
	judge  *fakeJudge
	clock  *fixedClock
	svc    *Service
	def    *core.CompiledDefinition
	bus    *fakeBus
	labels []core.ScorerLabel
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		st:    newFakeStore(),
		judge: &fakeJudge{},
		clock: &fixedClock{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)},
		bus:   newFakeBus(),
		labels: []core.ScorerLabel{
			{Value: "tests_run", Score: 1},
			{Value: "tests_skipped", Score: 0},
		},
	}
	e.judge.fn = func(model, rubric, transcript string) (string, string, error) {
		if strings.Contains(transcript, "go test") {
			return "tests_run", "ran tests", nil
		}
		return "tests_skipped", "no test evidence", nil
	}
	e.eng = &fakeEngine{st: e.st, clock: e.clock.Now}
	e.def = &core.CompiledDefinition{
		Name: "demo",
		Agents: []core.AgentConfig{
			{Name: "foreman", Type: core.AgentForeman},
			{Name: "implement", Type: core.AgentImplement},
			{Name: "review", Type: core.AgentReview},
		},
	}
	e.st.teams = []core.Team{{ID: "team_1"}}
	e.st.factories[testFactory] = &core.Factory{ID: testFactory, TeamID: "team_1", Name: "demo", Source: core.DefinitionManaged, ActiveVersionID: "ver_1"}
	e.st.versions["ver_1"] = &core.DefinitionVersion{ID: "ver_1", FactoryID: testFactory, Files: map[string]string{
		"factory.yaml":                          "name: demo\nbenchmarkRepoSubstitutions:\n  - source: {codeForge: GITHUB, owner: acme, name: app}\n    target: {codeForge: GITHUB, owner: acme-bench, name: app-fork}\n",
		"agents/implement/agent.md":             "---\nname: implement\n---\nAlways write code.\n",
		"agents/implement/skills/tdd/SKILL.md":  "Run tests first.",
		"agents/review/agent.md":                "Review things.",
		"scorers/tests-run/scorer.md":           "---\nname: tests-run\n---\nDid the agent run tests?",
		"benchmarks/fix/suite.yaml":             "name: Fix",
		"benchmarks/fix/tasks/broken-link.yaml": "title: x",
	}}
	e.st.users = []core.User{
		{ID: "u_admin", TeamID: "team_1", Email: "admin@example.com", Role: core.RoleAdmin},
		{ID: "u_dev", TeamID: "team_1", Email: "dev@example.com", Role: core.RoleEditor},
		{ID: "u_other", TeamID: "team_2", Email: "x@example.com", Role: core.RoleAdmin},
	}
	svc, err := NewService(Options{
		Store:         e.st,
		Bus:           e.bus,
		Classify:      e.judge.classify,
		Engine:        e.eng,
		Compile:       func(files map[string]string) (*core.CompiledDefinition, []core.ValidationError) { return e.def, nil },
		Clock:         e.clock.Now,
		Rand:          rand.New(rand.NewPCG(1, 2)),
		PublicURL:     "https://factory.example.com/",
		CheckInterval: -1,
		Logger:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *env) addScorer(sc core.Scorer) {
	if sc.FactoryID == "" {
		sc.FactoryID = testFactory
	}
	if sc.Labels == nil {
		sc.Labels = e.labels
	}
	if sc.Model == "" {
		sc.Model = "judge-model"
	}
	e.st.scorers[testFactory] = append(e.st.scorers[testFactory], sc)
}

func (e *env) finishedRun(id, agent string, state core.RunState, transcript ...string) *core.Run {
	r := &core.Run{ID: id, FactoryID: testFactory, AgentName: agent, State: state, CreatedAt: e.clock.Now()}
	e.st.putRun(r)
	for _, m := range transcript {
		e.st.addMessage(id, "assistant", m)
	}
	return r
}

func TestShouldSample(t *testing.T) {
	tests := []struct {
		name string
		rate float64
		want float64 // expected fraction sampled
	}{
		{"zero disables", 0, 0},
		{"rounds below two decimals to zero", 0.004, 0},
		{"negative", -5, 0},
		{"ten percent", 10, 0.10},
		{"quarter", 25, 0.25},
		{"all", 100, 1},
		{"over 100", 150, 1},
	}
	const n = 20000
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits := 0
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("run_%d", i)
				got := ShouldSample("scorer_a", id, tt.rate)
				if got != ShouldSample("scorer_a", id, tt.rate) {
					t.Fatalf("non-deterministic for %s", id)
				}
				if got {
					hits++
				}
			}
			frac := float64(hits) / n
			if d := frac - tt.want; d > 0.015 || d < -0.015 {
				t.Fatalf("sampled fraction %.4f, want ~%.2f", frac, tt.want)
			}
		})
	}
	// Different scorers sample independently.
	diff := 0
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("run_%d", i)
		if ShouldSample("a", id, 50) != ShouldSample("b", id, 50) {
			diff++
		}
	}
	if diff < 400 || diff > 600 {
		t.Fatalf("scorers not independent: %d/1000 differ", diff)
	}
	// Monotonic in rate: a run sampled at 10% is sampled at 20%.
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("run_%d", i)
		if ShouldSample("s", id, 10) && !ShouldSample("s", id, 20) {
			t.Fatalf("not monotonic for %s", id)
		}
	}
}

func TestAutoScoring(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		scorer     core.Scorer
		run        func(e *env) *core.Run
		wantScores int
		wantLabel  string
		wantPassed bool
	}{
		{
			name:   "scores finished run of selected agent",
			scorer: core.Scorer{ID: "sc_1", Slug: "tests-run", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100},
			run: func(e *env) *core.Run {
				return e.finishedRun("run_a", "implement", core.RunSucceeded, "I ran go test ./... and it passed")
			},
			wantScores: 1, wantLabel: "tests_run", wantPassed: true,
		},
		{
			name:   "failed runs are scored too",
			scorer: core.Scorer{ID: "sc_1", Slug: "tests-run", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100},
			run: func(e *env) *core.Run {
				return e.finishedRun("run_b", "implement", core.RunFailed, "gave up")
			},
			wantScores: 1, wantLabel: "tests_skipped", wantPassed: false,
		},
		{
			name:   "other agent ignored",
			scorer: core.Scorer{ID: "sc_1", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100},
			run: func(e *env) *core.Run {
				return e.finishedRun("run_c", "review", core.RunSucceeded, "go test")
			},
		},
		{
			name:   "sampling rate zero disables",
			scorer: core.Scorer{ID: "sc_1", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 0},
			run: func(e *env) *core.Run {
				return e.finishedRun("run_d", "implement", core.RunSucceeded, "go test")
			},
		},
		{
			name:   "idle and cancelled runs are not scored",
			scorer: core.Scorer{ID: "sc_1", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100},
			run: func(e *env) *core.Run {
				return e.finishedRun("run_e", "implement", core.RunIdle, "go test")
			},
		},
		{
			name:   "benchmark descendants are not production-scored",
			scorer: core.Scorer{ID: "sc_1", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100},
			run: func(e *env) *core.Run {
				e.st.putRun(&core.Run{ID: "trial_root", FactoryID: testFactory, AgentName: "foreman", TriggeredBy: "benchmark:bench_x", State: core.RunRunning})
				r := e.finishedRun("run_f", "implement", core.RunSucceeded, "go test")
				r.ParentRunID = "trial_root"
				e.st.putRun(r)
				return r
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.addScorer(tt.scorer)
			run := tt.run(e)
			// Deliver twice: re-delivery must not double-score.
			for i := 0; i < 2; i++ {
				if err := e.svc.HandleRunUpdate(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			scores := e.st.scoresFor(run.ID)
			if len(scores) != tt.wantScores {
				t.Fatalf("got %d scores, want %d", len(scores), tt.wantScores)
			}
			if e.judge.count() != tt.wantScores {
				t.Fatalf("judge called %d times, want %d", e.judge.count(), tt.wantScores)
			}
			if tt.wantScores == 0 {
				return
			}
			s := scores[0]
			if s.Label != tt.wantLabel || s.Passed != tt.wantPassed || s.AgentName != run.AgentName || s.Rationale == "" {
				t.Fatalf("unexpected score %+v", s)
			}
		})
	}
}

func TestTranscriptIncludesDescendants(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addScorer(core.Scorer{ID: "sc_1", Agents: []core.ScorerAgent{{Name: "foreman", IncludeDescendants: true}}, PassingScore: 1, SamplingRate: 100})
	root := e.finishedRun("root", "foreman", core.RunSucceeded, "dispatching", "m2", "m3", "done")
	child := &core.Run{ID: "child", FactoryID: testFactory, ParentRunID: "root", AgentName: "implement", State: core.RunSucceeded, CreatedAt: e.clock.Now()}
	e.st.putRun(child)
	e.st.addMessage("child", "tool_call", "bash: go test ./...")
	grand := &core.Run{ID: "grand", FactoryID: testFactory, ParentRunID: "child", AgentName: "review", State: core.RunSucceeded, CreatedAt: e.clock.Now()}
	e.st.putRun(grand)
	e.st.addMessage("grand", "assistant", "LGTM grandchild")

	if err := e.svc.HandleRunUpdate(ctx, root); err != nil {
		t.Fatal(err)
	}
	s := e.st.scoresFor("root")
	if len(s) != 1 || !s[0].Passed {
		t.Fatalf("expected passing score from descendant evidence, got %+v", s)
	}
	tr := e.judge.calls[0].Transcript
	for _, want := range []string{"=== Run root", "m3", "done", "=== Child run child", "go test", "LGTM grandchild"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript missing %q:\n%s", want, tr)
		}
	}

	// Without includeDescendants, child evidence is excluded.
	plain, err := e.svc.BuildTranscript(ctx, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, "go test") {
		t.Fatal("transcript without descendants contains child messages")
	}
}

func TestTranscriptCap(t *testing.T) {
	s := strings.Repeat("a", 1000) + "MIDDLE" + strings.Repeat("z", 1000)
	got := capMiddle(s, 500)
	if len(got) > 500 || !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "zzz") || strings.Contains(got, "MIDDLE") {
		t.Fatalf("bad cap: len=%d", len(got))
	}
	if capMiddle("short", 500) != "short" {
		t.Fatal("short string changed")
	}
	if tr := truncate("héllo wörld", 2); !strings.HasPrefix(tr, "h…") {
		t.Fatalf("truncate split rune: %q", tr)
	}
}

func TestScoreRunReplacesPreviousResult(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.addScorer(core.Scorer{ID: "sc_1", Slug: "tests-run", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 0})
	e.finishedRun("run_a", "implement", core.RunSucceeded, "nothing")

	first, err := e.svc.ScoreRun(ctx, "tests-run", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if first.Passed {
		t.Fatal("expected failure")
	}
	e.judge.fn = func(model, rubric, transcript string) (string, string, error) { return " Tests_Run ", "now ok", nil }
	second, err := e.svc.ScoreRun(ctx, "sc_1", "run_a")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || !second.Passed || second.Label != "tests_run" {
		t.Fatalf("expected replaced passing score with same id, got %+v (first %+v)", second, first)
	}
	if n := len(e.st.scoresFor("run_a")); n != 1 {
		t.Fatalf("got %d stored scores, want 1", n)
	}

	if _, err := e.svc.ScoreRun(ctx, "missing", "run_a"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	e.judge.fn = func(model, rubric, transcript string) (string, string, error) { return "maybe", "", nil }
	if _, err := e.svc.ScoreRun(ctx, "sc_1", "run_a"); err == nil || !strings.Contains(err.Error(), "undeclared label") {
		t.Fatalf("want undeclared label error, got %v", err)
	}
}

func TestPassingThreshold(t *testing.T) {
	labels := []core.ScorerLabel{{Value: "good", Score: 1}, {Value: "ok", Score: 0.5}, {Value: "bad", Score: 0}}
	tests := []struct {
		label   string
		passing float64
		want    bool
	}{
		{"good", 1, true}, {"ok", 1, false}, {"ok", 0.5, true}, {"bad", 0, true}, {"bad", 0.1, false},
	}
	for _, tt := range tests {
		e := newEnv(t)
		e.judge.fn = func(model, rubric, transcript string) (string, string, error) { return tt.label, "", nil }
		jr, err := e.svc.judge(context.Background(), "m", "r", "t", labels, tt.passing)
		if err != nil {
			t.Fatal(err)
		}
		if jr.Passed != tt.want {
			t.Errorf("label %s passing %.1f: passed=%v want %v", tt.label, tt.passing, jr.Passed, tt.want)
		}
	}
}

func TestSelfImprovementThreshold(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.def.SelfImprovement = &core.SelfImprovementConfig{FailedRunThreshold: 3, ReviewerType: ReviewerCustom, ReviewerEmails: []string{"DEV@example.com"}}
	e.addScorer(core.Scorer{ID: "sc_si", Slug: "tests-run", Name: "tests-run", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100, SelfImprovement: true, Rubric: "Did the agent run tests?"})
	e.addScorer(core.Scorer{ID: "sc_plain", Slug: "other", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100})

	for i := 1; i <= 2; i++ {
		r := e.finishedRun(fmt.Sprintf("run_%d", i), "implement", core.RunSucceeded, fmt.Sprintf("skipped tests in attempt %d", i))
		if err := e.svc.HandleRunUpdate(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if e.eng.dispatchCount() != 0 {
		t.Fatal("self-improvement started below threshold")
	}
	r3 := e.finishedRun("run_3", "implement", core.RunFailed, "skipped tests again")
	r3.Error = "boom"
	e.st.putRun(r3)
	if err := e.svc.HandleRunUpdate(ctx, r3); err != nil {
		t.Fatal(err)
	}
	if e.eng.dispatchCount() != 1 {
		t.Fatalf("dispatches = %d, want 1", e.eng.dispatchCount())
	}
	req := e.eng.dispatches[0]
	if req.TriggeredBy != TriggeredBySelfImprovement || req.Metadata[MetaSelfImprovementAgent] != "implement" || req.Source.Provider != SourceSelfImprovement {
		t.Fatalf("unexpected dispatch request: %+v", req)
	}
	for _, want := range []string{
		"`implement`", "run_1", "run_3", "no test evidence", "Regressions addressed",
		"https://factory.example.com/runs/run_2", "Always write code.", "Run tests first.",
		"Did the agent run tests?", "pull request", "propose_definition_change", "ERROR: boom",
	} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(req.Prompt, "Review things.") {
		t.Error("prompt includes unrelated agent definition")
	}

	// All self-improvement failures are now marked; non-SI scorer failures are not.
	var impRun string
	for _, s := range e.st.scores {
		if s.ScorerID == "sc_si" {
			if s.ImprovementRunID == "" {
				t.Fatalf("score %s not marked", s.ID)
			}
			impRun = s.ImprovementRunID
		} else if s.ImprovementRunID != "" {
			t.Fatalf("non-self-improvement score marked: %+v", s)
		}
	}
	// A managed definition gets its review item with the proposal, not at the start.
	if len(e.st.inbox) != 0 || impRun == "" {
		t.Fatalf("inbox items = %d, want 0", len(e.st.inbox))
	}

	// The counter resets: one more failure does not trigger again.
	r4 := e.finishedRun("run_4", "implement", core.RunSucceeded, "skipped")
	if err := e.svc.HandleRunUpdate(ctx, r4); err != nil {
		t.Fatal(err)
	}
	if e.eng.dispatchCount() != 1 {
		t.Fatal("self-improvement re-triggered below threshold")
	}
	// "Run now" processes an agent with a single unreviewed failure.
	runs, err := e.svc.TriggerSelfImprovement(ctx, testFactory, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || e.eng.dispatchCount() != 2 {
		t.Fatalf("manual trigger: runs=%d dispatches=%d", len(runs), e.eng.dispatchCount())
	}
	// Nothing left to process.
	runs, err = e.svc.TriggerSelfImprovement(ctx, testFactory, "implement")
	if err != nil || len(runs) != 0 {
		t.Fatalf("expected no runs, got %d (%v)", len(runs), err)
	}
}

func TestSelfImprovementDistinctRunsAndAge(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t) // no selfImprovement config => default threshold 25
	// A git definition is reviewed on its pull request.
	e.st.factories[testFactory].Source = core.DefinitionGit
	e.st.factories[testFactory].GitRepo = "github.com/acme/def"
	e.addScorer(core.Scorer{ID: "sc_a", Slug: "a", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100, SelfImprovement: true})
	e.addScorer(core.Scorer{ID: "sc_b", Slug: "b", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100, SelfImprovement: true})
	r := e.finishedRun("run_1", "implement", core.RunSucceeded, "nope")
	if err := e.svc.HandleRunUpdate(ctx, r); err != nil {
		t.Fatal(err)
	}
	f, err := e.svc.UnreviewedFailures(ctx, testFactory)
	if err != nil {
		t.Fatal(err)
	}
	if len(f["implement"]) != 1 {
		t.Fatalf("two scorers failing one run must count as one distinct failure, got %d", len(f["implement"]))
	}
	if runs, _ := e.svc.CheckSelfImprovement(ctx, testFactory); len(runs) != 0 {
		t.Fatal("scheduled check started below threshold")
	}
	e.clock.Advance(DefaultMaxFailureAge)
	runs, err := e.svc.CheckSelfImprovement(ctx, testFactory)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("aged failure should trigger a run, got %d", len(runs))
	}
	// The review item is not created at the start: no change exists yet.
	if len(e.st.inbox) != 0 {
		t.Fatalf("inbox items at the start = %d, want 0", len(e.st.inbox))
	}
	// The run opens a pull request. When its turn ends, the item is created
	// for the reviewer of the default reviewerType admins (the team admin).
	var imp *core.Run
	for _, r := range e.st.runs {
		if r.Metadata[MetaSelfImprovement] == "true" {
			imp = r
		}
	}
	if imp == nil {
		t.Fatal("no self-improvement run")
	}
	e.st.putWorkItem(&core.WorkItem{ID: imp.WorkItemID, FactoryID: testFactory, PullRequests: []core.PullRequestRef{{Forge: "GITHUB", Repo: "acme/def", Number: 7, URL: "https://github.com/acme/def/pull/7", State: "open"}}})
	imp.State = core.RunIdle
	e.st.putRun(imp)
	if err := e.svc.HandleRunUpdate(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if len(e.st.inbox) != 1 || e.st.inbox[0].AssigneeID != "u_admin" || e.st.inbox[0].Kind != core.InboxReview {
		t.Fatalf("inbox after the pull request = %+v", e.st.inbox)
	}
	it := e.st.inbox[0]
	if !strings.Contains(it.Body, "acme/def#7") || !strings.Contains(it.Body, "Regressions addressed") || it.RunID != imp.ID || it.WorkItemID != imp.WorkItemID {
		t.Fatalf("review item %+v", it)
	}
	// The same pull request does not create a second item.
	if err := e.svc.HandleRunUpdate(ctx, imp); err != nil {
		t.Fatal(err)
	}
	if len(e.st.inbox) != 1 {
		t.Fatalf("inbox after a second update = %d", len(e.st.inbox))
	}
}

func TestImprovementSettings(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		cfg  *core.SelfImprovementConfig
		want int
	}{
		{nil, DefaultFailedRunThreshold},
		{&core.SelfImprovementConfig{}, DefaultFailedRunThreshold},
		{&core.SelfImprovementConfig{FailedRunThreshold: 7}, 7},
		{&core.SelfImprovementConfig{FailedRunThreshold: 99}, MaxFailedRunThreshold},
	}
	for _, tt := range tests {
		got := e.svc.improvementSettings(&core.CompiledDefinition{SelfImprovement: tt.cfg})
		if got.Threshold != tt.want {
			t.Errorf("threshold for %+v = %d, want %d", tt.cfg, got.Threshold, tt.want)
		}
		if tt.cfg == nil && got.ReviewerType != ReviewerAdmins {
			t.Errorf("default reviewer type = %q", got.ReviewerType)
		}
	}
}

func TestEligibleReviewers(t *testing.T) {
	users := []core.User{
		{ID: "b", Email: "B@x.com", Role: core.RoleAdmin},
		{ID: "a", Email: "a@x.com", Role: core.RoleEditor},
		{ID: "c", Email: "c@x.com", Role: core.RoleUser},
	}
	ids := func(us []core.User) string {
		var s []string
		for _, u := range us {
			s = append(s, u.ID)
		}
		return strings.Join(s, ",")
	}
	tests := []struct {
		typ    string
		emails []string
		want   string
	}{
		{ReviewerAdmins, nil, "b"},
		{"", nil, "b"},
		{ReviewerTeam, nil, "a,b,c"},
		{ReviewerCustom, []string{"b@x.com", " c@X.com "}, "b,c"},
		{ReviewerCustom, []string{"nobody@x.com"}, ""},
		{ReviewerNone, nil, ""},
	}
	for _, tt := range tests {
		if got := ids(EligibleReviewers(users, tt.typ, tt.emails)); got != tt.want {
			t.Errorf("%s %v: got %q want %q", tt.typ, tt.emails, got, tt.want)
		}
	}
}

func TestStartConsumesBus(t *testing.T) {
	e := newEnv(t)
	e.addScorer(core.Scorer{ID: "sc_1", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer e.svc.Close()
	if err := e.svc.Start(ctx); err == nil {
		t.Fatal("second Start should fail")
	}
	r := e.finishedRun("run_bus", "implement", core.RunSucceeded, "go test ok")
	e.bus.Publish(ctx, core.TopicRunUpdated, r)
	deadline := time.Now().Add(5 * time.Second)
	for len(e.st.scoresFor("run_bus")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("run was not scored via the bus")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
