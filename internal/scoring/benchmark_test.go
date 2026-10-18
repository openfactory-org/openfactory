package scoring

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

const sha = "0123456789abcdef0123456789abcdef01234567"

func benchEnv(t *testing.T) *env {
	e := newEnv(t)
	e.def.Benchmarks = []core.BenchmarkSuite{{
		Slug:  "fix",
		Name:  "Fix",
		Agent: "implement",
		Configurations: []core.BenchmarkConfiguration{
			{Name: "Baseline", Agents: []core.BenchmarkAgentPin{{Agent: "implement", Model: "model-big"}}},
			{Name: "Candidate", Agents: []core.BenchmarkAgentPin{
				{Agent: "implement", Harness: &core.HarnessConfig{Type: "claude-code", Model: "model-small"}},
				{Agent: "review", Model: "model-review"},
			}},
		},
		Tasks: []core.BenchmarkTask{
			{Slug: "broken-link", Title: "Fix link", Prompt: "Fix the broken link.", SuccessCriteria: "The link resolves.",
				StartingRepoRefs: []core.RepoAtCommit{{CodeForge: "GITHUB", Owner: "acme", Repo: "app", Ref: sha}, {CodeForge: "GITHUB", Owner: "acme", Repo: "docs", Ref: sha}}},
			{Slug: "typo", Title: "Fix typo", Prompt: "Fix the typo.", SuccessCriteria: "Typo gone."},
		},
	}}
	// The correctness judge passes trials whose transcript says "fixed".
	e.judge.fn = func(model, rubric, transcript string) (string, string, error) {
		if strings.Contains(rubric, "Correctness Scorer") {
			if strings.Contains(transcript, "fixed") {
				return "correct", "criteria met", nil
			}
			return "incorrect", "criteria not met", nil
		}
		if strings.Contains(transcript, "go test") {
			return "tests_run", "", nil
		}
		return "tests_skipped", "", nil
	}
	return e
}

// finishTrial completes a trial run in the store with usage and an assistant message.
func (e *env) finishTrial(t *testing.T, runID string, state core.RunState, msg string, cost float64, dur time.Duration) *core.Run {
	t.Helper()
	r, err := e.st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	fin := r.CreatedAt.Add(dur)
	r.State = state
	r.FinishedAt = &fin
	r.Usage = core.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: cost}
	e.st.putRun(r)
	e.st.addMessage(runID, "assistant", msg)
	return r
}

func TestBenchmarkLifecycle(t *testing.T) {
	ctx := context.Background()
	e := benchEnv(t)
	e.addScorer(core.Scorer{ID: "sc_tests", Slug: "tests-run", Name: "tests-run", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100})

	bench, err := e.svc.StartBenchmarkWithOptions(ctx, BenchmarkRequest{FactoryID: testFactory, Suite: "fix", Repetitions: 2, Scorers: []string{"tests-run"}})
	if err != nil {
		t.Fatal(err)
	}
	if bench.State != BenchmarkRunning || bench.Suite != "fix" {
		t.Fatalf("unexpected bench %+v", bench)
	}
	if got := e.eng.dispatchCount(); got != 8 { // 2 configs × 2 tasks × 2 reps
		t.Fatalf("dispatches = %d, want 8", got)
	}
	for _, d := range e.eng.dispatches {
		if d.AgentName != "implement" || d.TriggeredBy != "benchmark:"+bench.ID || d.Metadata[MetaBenchmarkRun] != bench.ID || d.Source.Provider != "benchmark" {
			t.Fatalf("bad dispatch %+v", d)
		}
		switch d.Metadata[MetaBenchmarkConfiguration] {
		case "Baseline":
			if d.Overrides == nil || d.Overrides.Model != "model-big" || d.Overrides.Harness != nil {
				t.Fatalf("baseline overrides %+v", d.Overrides)
			}
		case "Candidate":
			if d.Overrides == nil || d.Overrides.Harness == nil || d.Overrides.Harness.Type != core.HarnessClaude || d.Overrides.Harness.Model != "model-small" || d.Overrides.Model != "" {
				t.Fatalf("candidate overrides %+v", d.Overrides)
			}
			if pin, ok := d.AgentPins["review"]; !ok || pin.Model != "model-review" {
				t.Fatalf("missing review pin: %v", d.AgentPins)
			}
		default:
			t.Fatalf("unknown configuration %v", d.Metadata)
		}
		if d.Metadata[MetaBenchmarkTask] == "broken-link" {
			// benchmarkRepoSubstitutions: acme/app -> acme-bench/app-fork; acme/docs unchanged.
			if len(d.StartingRepoRefs) != 2 || FormatRepoAtCommit(d.StartingRepoRefs[0]) != "github.com:acme-bench/app-fork@"+sha || FormatRepoAtCommit(d.StartingRepoRefs[1]) != "github.com:acme/docs@"+sha {
				t.Fatalf("starting repo refs %v", d.StartingRepoRefs)
			}
			if !strings.Contains(d.Prompt, "Fix the broken link.") || !strings.Contains(d.Prompt, "github.com:acme-bench/app-fork@"+sha) {
				t.Fatalf("trial prompt %q", d.Prompt)
			}
		}
	}

	_, res := mustLoad(t, e, bench.ID)
	// Baseline: all correct, costly and slow. Candidate: half correct, cheap and fast.
	for i, tr := range res.Trials {
		var msg string
		var cost float64
		var dur time.Duration
		if tr.Configuration == "Baseline" {
			msg, cost, dur = "fixed it; ran go test", 2.0, 10*time.Minute
		} else {
			msg, cost, dur = "attempted", 0.5, 2*time.Minute
			if tr.Repetition == 1 {
				msg = "fixed quickly"
			}
		}
		// A child run's cost counts toward the trial.
		if i == 0 {
			e.st.putRun(&core.Run{ID: "child_of_trial", FactoryID: testFactory, ParentRunID: tr.RunID, AgentName: "review", State: core.RunSucceeded,
				Metadata: map[string]string{MetaBenchmarkRun: bench.ID}, Usage: core.Usage{CostUSD: 1, InputTokens: 5, OutputTokens: 5}, CreatedAt: e.clock.Now()})
			// The descendant's own update (it inherits trial metadata) is ignored.
			child, _ := e.st.GetRun(ctx, "child_of_trial")
			if err := e.svc.HandleRunUpdate(ctx, child); err != nil {
				t.Fatal(err)
			}
		}
		run := e.finishTrial(t, tr.RunID, core.RunSucceeded, msg, cost, dur)
		if err := e.svc.HandleRunUpdate(ctx, run); err != nil {
			t.Fatal(err)
		}
		// Re-delivery is a no-op.
		if err := e.svc.HandleRunUpdate(ctx, run); err != nil {
			t.Fatal(err)
		}
		b, _ := mustLoad(t, e, bench.ID)
		if i < len(res.Trials)-1 && b.State != BenchmarkRunning {
			t.Fatalf("state after %d trials = %s", i+1, b.State)
		}
	}
	if got := e.judge.count(); got != 16 { // 8 trials × (correctness + tests-run)
		t.Fatalf("judge calls = %d, want 16", got)
	}
	b, res := mustLoad(t, e, bench.ID)
	if b.State != BenchmarkComplete || b.FinishedAt == nil {
		t.Fatalf("final state %s", b.State)
	}
	if len(res.Summary) != 2 {
		t.Fatalf("summary %+v", res.Summary)
	}
	base, cand := res.Summary[0], res.Summary[1]
	if base.Name != "Baseline" || base.Trials != 4 || base.Scored != 4 || base.Passed != 4 || base.PassRate != 1 {
		t.Fatalf("baseline %+v", base)
	}
	if !approx(base.TotalCostUSD, 9) || !approx(base.AvgCostUSD, 2.25) || base.MedianDurationMS != (10*time.Minute).Milliseconds() || base.InputTokens != 405 {
		t.Fatalf("baseline usage %+v", base)
	}
	if base.ScorerAverages[CorrectnessScorer] != 1 || base.ScorerAverages["tests-run"] != 1 || base.Overall != 1 {
		t.Fatalf("baseline averages %+v", base.ScorerAverages)
	}
	if cand.Passed != 2 || cand.PassRate != 0.5 || !approx(cand.AvgCostUSD, 0.5) || !approx(cand.Overall, 0.25) {
		t.Fatalf("candidate %+v", cand)
	}
	if res.Recommendations.Overall != "Baseline" || res.Recommendations.LowestCost != "Candidate" || res.Recommendations.MostEfficient != "Candidate" {
		t.Fatalf("recommendations %+v", res.Recommendations)
	}
	if !approx(res.TotalCostUSD, 11) {
		t.Fatalf("total cost %v", res.TotalCostUSD)
	}
	// Benchmark trials are never production-scored.
	for _, tr := range res.Trials {
		if len(e.st.scoresFor(tr.RunID)) != 0 {
			t.Fatal("trial was production-scored")
		}
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func mustLoad(t *testing.T, e *env, id string) (*core.BenchmarkRun, *BenchmarkResults) {
	t.Helper()
	b, res, err := e.svc.loadBenchmark(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return b, res
}

func TestBenchmarkBlockedAndReconcile(t *testing.T) {
	ctx := context.Background()
	e := benchEnv(t)
	bench, err := e.svc.StartBenchmark(ctx, testFactory, "fix", []core.BenchmarkConfiguration{{Name: "only"}})
	if err != nil {
		t.Fatal(err)
	}
	_, res := mustLoad(t, e, bench.ID)
	if len(res.Trials) != 2 || e.eng.dispatches[0].Overrides != nil {
		t.Fatalf("trials %d overrides %+v", len(res.Trials), e.eng.dispatches[0].Overrides)
	}
	// Trial 1 goes idle waiting on a human: blocked, still scored, then released.
	t1 := res.Trials[0]
	r1, _ := e.st.GetRun(ctx, t1.RunID)
	r1.State = core.RunIdle
	e.st.putRun(r1)
	e.st.addMessage(r1.ID, "assistant", "fixed, awaiting approval")
	e.st.inbox = append(e.st.inbox, core.InboxItem{ID: "in_1", FactoryID: testFactory, RunID: r1.ID, Kind: core.InboxApproval})
	// Trial 2 finishes but its update is "dropped"; reconcile must pick it up.
	e.finishTrial(t, res.Trials[1].RunID, core.RunFailed, "could not do it", 0.1, time.Minute)

	if err := e.svc.ReconcileBenchmark(ctx, bench.ID); err != nil {
		t.Fatal(err)
	}
	b, res := mustLoad(t, e, bench.ID)
	if b.State != BenchmarkComplete {
		t.Fatalf("state %s", b.State)
	}
	if res.Trials[0].Status != TrialBlocked || !res.Trials[0].Scored || !res.Trials[0].Scores[0].Passed {
		t.Fatalf("trial 1 %+v", res.Trials[0])
	}
	if res.Trials[1].Status != TrialFinished || res.Trials[1].RunState != core.RunFailed || res.Trials[1].Scores[0].Passed {
		t.Fatalf("trial 2 %+v", res.Trials[1])
	}
	if res.Summary[0].Blocked != 1 || res.Summary[0].PassRate != 0.5 {
		t.Fatalf("summary %+v", res.Summary[0])
	}
	if len(e.eng.cancels) != 1 || e.eng.cancels[0] != r1.ID {
		t.Fatalf("idle trial not released: %v", e.eng.cancels)
	}
	// Sweep over completed benchmarks is a no-op.
	e.svc.Sweep(ctx)
}

func TestBenchmarkCancel(t *testing.T) {
	ctx := context.Background()
	e := benchEnv(t)
	bench, err := e.svc.StartBenchmark(ctx, testFactory, "fix", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, res := mustLoad(t, e, bench.ID)
	if len(res.Trials) != 4 {
		t.Fatalf("trials %d", len(res.Trials))
	}
	first := e.finishTrial(t, res.Trials[0].RunID, core.RunSucceeded, "fixed", 1, time.Minute)
	if err := e.svc.HandleRunUpdate(ctx, first); err != nil {
		t.Fatal(err)
	}
	b, err := e.svc.CancelBenchmark(ctx, bench.ID, "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	if b.State != BenchmarkCancelled || b.FinishedAt == nil {
		t.Fatalf("state %s", b.State)
	}
	// The scored succeeded trial is released too (its session can stay live on the runner path).
	if len(e.eng.cancels) != 4 {
		t.Fatalf("cancelled runs %v, want 4", e.eng.cancels)
	}
	// Late completions are ignored; only results scored before the stop remain.
	late := e.finishTrial(t, res.Trials[1].RunID, core.RunSucceeded, "fixed", 1, time.Minute)
	if err := e.svc.HandleRunUpdate(ctx, late); err != nil {
		t.Fatal(err)
	}
	_, res = mustLoad(t, e, bench.ID)
	if res.CancelledBy != "user:alice" || !res.Trials[0].Scored || res.Trials[1].Scored || res.Trials[1].Status != TrialCancelled {
		t.Fatalf("unexpected trials after cancel: %+v", res.Trials[:2])
	}
	if res.Summary[0].Scored != 1 {
		t.Fatalf("summary %+v", res.Summary[0])
	}
	// Cancelling again is idempotent; cancelling a complete run conflicts.
	if _, err := e.svc.CancelBenchmark(ctx, bench.ID, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestBenchmarkValidation(t *testing.T) {
	ctx := context.Background()
	e := benchEnv(t)
	tooMany := make([]core.BenchmarkConfiguration, 7)
	for i := range tooMany {
		tooMany[i].Name = string(rune('a' + i))
	}
	tests := []struct {
		name    string
		req     BenchmarkRequest
		wantErr error
	}{
		{"unknown suite", BenchmarkRequest{FactoryID: testFactory, Suite: "nope"}, core.ErrNotFound},
		{"too many configs", BenchmarkRequest{FactoryID: testFactory, Suite: "fix", Configurations: tooMany}, core.ErrInvalid},
		{"duplicate names", BenchmarkRequest{FactoryID: testFactory, Suite: "fix", Configurations: []core.BenchmarkConfiguration{{Name: "a"}, {Name: "a"}}}, core.ErrInvalid},
		{"model and harness", BenchmarkRequest{FactoryID: testFactory, Suite: "fix", Configurations: []core.BenchmarkConfiguration{{Name: "a", Agents: []core.BenchmarkAgentPin{{Agent: "implement", Model: "m", Harness: &core.HarnessConfig{Type: "codex", Model: "m"}}}}}}, core.ErrInvalid},
		{"unknown agent", BenchmarkRequest{FactoryID: testFactory, Suite: "fix", Configurations: []core.BenchmarkConfiguration{{Name: "a", Agents: []core.BenchmarkAgentPin{{Agent: "ghost", Model: "m"}}}}}, core.ErrInvalid},
		{"too many repetitions", BenchmarkRequest{FactoryID: testFactory, Suite: "fix", Repetitions: 11}, core.ErrInvalid},
		{"unknown scorer", BenchmarkRequest{FactoryID: testFactory, Suite: "fix", Scorers: []string{"ghost"}}, core.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := e.svc.StartBenchmarkWithOptions(ctx, tt.req); !errors.Is(err, tt.wantErr) {
				t.Fatalf("got %v, want %v", err, tt.wantErr)
			}
		})
	}
	e.def.Benchmarks[0].Tasks = nil
	if _, err := e.svc.StartBenchmark(ctx, testFactory, "fix", nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("suite without tasks: %v", err)
	}
}

func TestBenchmarkAllDispatchesFail(t *testing.T) {
	ctx := context.Background()
	e := benchEnv(t)
	e.def.Benchmarks[0].Tasks = []core.BenchmarkTask{{Slug: "t", Title: "T", Prompt: "FAIL please", SuccessCriteria: "x"}}
	e.eng.failAgent = "implement"
	b, err := e.svc.StartBenchmark(ctx, testFactory, "fix", nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.State != BenchmarkFailed || b.FinishedAt == nil {
		t.Fatalf("state %s", b.State)
	}
	_, res := mustLoad(t, e, b.ID)
	for _, tr := range res.Trials {
		if tr.Status != TrialError || tr.Error == "" {
			t.Fatalf("trial %+v", tr)
		}
	}
	if res.Summary[0].Errored != 1 {
		t.Fatalf("summary %+v", res.Summary)
	}
}

func TestAggregate(t *testing.T) {
	fin := time.Now()
	trial := func(cfg string, correct bool, cost float64, durMS int64, scored bool) Trial {
		score := 0.0
		if correct {
			score = 1
		}
		tr := Trial{Configuration: cfg, Status: TrialFinished, RunID: "r", FinishedAt: &fin, CostUSD: cost, DurationMS: durMS, Scored: scored}
		if scored {
			tr.Scores = []TrialScore{{Scorer: CorrectnessScorer, Score: score, Passed: correct}}
		}
		return tr
	}
	configs := []core.BenchmarkConfiguration{{Name: "A"}, {Name: "B"}, {Name: "C"}}
	trials := []Trial{
		trial("A", true, 1, 100, true),
		trial("A", false, 3, 300, true),
		trial("A", true, 2, 200, true),
		trial("B", true, 0.5, 50, true),
		trial("B", true, 0.5, 70, true),
		trial("B", false, 0.5, 90, false), // finished but unscored
		{Configuration: "B", Status: TrialRunning, RunID: "r"},
		{Configuration: "C", Status: TrialError, Error: "boom"},
		{Configuration: "B", Status: TrialFinished, RunID: "r", FinishedAt: &fin, Scored: true,
			Scores: []TrialScore{{Scorer: CorrectnessScorer, Error: "judge down"}}},
	}
	got, rec, total := Aggregate(configs, trials, []string{CorrectnessScorer})
	if len(got) != 3 {
		t.Fatalf("results %+v", got)
	}
	a, b, c := got[0], got[1], got[2]
	if a.Scored != 3 || a.Passed != 2 || !approx(a.PassRate, 2.0/3) || !approx(a.AvgCostUSD, 2) || a.MedianDurationMS != 200 || a.AvgDurationMS != 200 {
		t.Fatalf("A %+v", a)
	}
	if b.Trials != 5 || b.Finished != 4 || b.Scored != 2 || b.PassRate != 1 || b.MedianDurationMS != 60 || !approx(b.TotalCostUSD, 1.5) {
		t.Fatalf("B %+v", b)
	}
	if c.Trials != 1 || c.Errored != 1 || c.Finished != 0 || len(c.ScorerAverages) != 0 {
		t.Fatalf("C %+v", c)
	}
	if rec.Overall != "B" || rec.LowestCost != "B" || rec.MostEfficient != "B" {
		t.Fatalf("recommendations %+v", rec)
	}
	if !approx(total, 7.5) {
		t.Fatalf("total %v", total)
	}
	// One scored configuration: no recommendations.
	_, rec, _ = Aggregate(configs[:1], trials[:3], []string{CorrectnessScorer})
	if rec != (Recommendations{}) {
		t.Fatalf("expected no recommendations, got %+v", rec)
	}
	// Equal quality: lower cost wins overall; equal cost: no cost recommendations.
	_, rec, _ = Aggregate(nil, []Trial{trial("X", true, 2, 1, true), trial("Y", true, 1, 1, true)}, []string{CorrectnessScorer})
	if rec.Overall != "Y" {
		t.Fatalf("tie-break %+v", rec)
	}
	_, rec, _ = Aggregate(nil, []Trial{trial("X", true, 1, 1, true), trial("Y", false, 1, 1, true)}, []string{CorrectnessScorer})
	if rec.Overall != "X" || rec.LowestCost != "" || rec.MostEfficient != "" {
		t.Fatalf("equal cost %+v", rec)
	}
}

func TestRepoSubstitutions(t *testing.T) {
	subs, err := ParseRepoSubstitutions(map[string]string{
		"defs/factory.yaml":      "benchmarkRepoSubstitutions:\n  - source: {codeForge: GITHUB, owner: Acme, name: App}\n    target: {codeForge: GITHUB, owner: bench, name: app}\n",
		"defs/agents/x/agent.md": "",
	})
	if err != nil || len(subs) != 1 {
		t.Fatalf("subs %+v err %v", subs, err)
	}
	refs := []core.RepoAtCommit{
		{CodeForge: "GITHUB", Owner: "acme", Repo: "app", Ref: sha},
		{CodeForge: "GITLAB", Owner: "acme", Repo: "app", Ref: sha},
	}
	got := ApplyRepoSubstitutions(refs, subs)
	if got[0].Owner != "bench" || got[0].Repo != "app" || got[0].Ref != sha {
		t.Fatalf("substituted %+v", got[0])
	}
	if got[1] != refs[1] {
		t.Fatalf("gitlab ref changed: %+v", got[1])
	}
	if refs[0].Owner != "acme" {
		t.Fatal("input mutated")
	}
	if FormatRepoAtCommit(refs[1]) != "gitlab.com:acme/app@"+sha {
		t.Fatal(FormatRepoAtCommit(refs[1]))
	}
	if _, err := ParseRepoSubstitutions(map[string]string{"factory.yaml": "benchmarkRepoSubstitutions: [unclosed"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if s, err := ParseRepoSubstitutions(nil); err != nil || s != nil {
		t.Fatal("nil files")
	}
}

func TestCorrectnessScorerRubric(t *testing.T) {
	sc := CorrectnessScorerFor(core.BenchmarkTask{Title: "T", Prompt: "Do X", SuccessCriteria: "X is done"}, "m")
	if sc.Key != CorrectnessScorer || sc.Model != "m" || sc.PassingScore != 1 || len(sc.Labels) != 2 {
		t.Fatalf("%+v", sc)
	}
	if !strings.Contains(sc.Rubric, "X is done") || !strings.Contains(sc.Rubric, "Do X") {
		t.Fatal(sc.Rubric)
	}
}
