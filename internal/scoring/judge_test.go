package scoring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ---------- more fakes for judge runs and proposals ----------

type kvStore struct {
	mu sync.Mutex
	kv map[string]string
}

func (k *kvStore) get(key string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.kv[key]
	if !ok {
		return "", core.ErrNotFound
	}
	return v, nil
}

func (k *kvStore) set(key, v string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.kv == nil {
		k.kv = map[string]string{}
	}
	k.kv[key] = v
}

var testKV = map[*fakeStore]*kvStore{}
var testKVMu sync.Mutex

func kvOf(f *fakeStore) *kvStore {
	testKVMu.Lock()
	defer testKVMu.Unlock()
	k := testKV[f]
	if k == nil {
		k = &kvStore{}
		testKV[f] = k
	}
	return k
}

func (f *fakeStore) KVGet(ctx context.Context, key string) (string, error) { return kvOf(f).get(key) }
func (f *fakeStore) KVSet(ctx context.Context, key, value string) error {
	kvOf(f).set(key, value)
	return nil
}

func (f *fakeStore) CreateDefinitionVersion(ctx context.Context, v *core.DefinitionVersion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *v
	f.versions[v.ID] = &cp
	return nil
}

func (f *fakeStore) UpdateDefinitionVersion(ctx context.Context, v *core.DefinitionVersion) error {
	return f.CreateDefinitionVersion(ctx, v)
}

func (f *fakeStore) ListDefinitionVersions(ctx context.Context, factoryID string, opts core.ListOpts) ([]core.DefinitionVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.DefinitionVersion
	for _, v := range f.versions {
		if v.FactoryID == factoryID {
			out = append(out, *v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if opts.Cursor != "" {
		return nil, nil
	}
	return out, nil
}

func (f *fakeStore) updateFactory(fn func(*core.Factory)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.factories[testFactory])
}

// judgeEngine adds the engine calls of proposals to fakeEngine.
type judgeEngine struct {
	*fakeEngine
	applied   []map[string]string
	resolved  []string
	followUps []string
}

func (e *judgeEngine) ApplyDefinition(ctx context.Context, factoryID string, files map[string]string, author, message, commitSHA string) (*core.DefinitionVersion, error) {
	e.applied = append(e.applied, files)
	v := &core.DefinitionVersion{ID: core.NewID("defv"), FactoryID: factoryID, Files: files, Valid: true, CreatedAt: e.clock()}
	_ = e.st.CreateDefinitionVersion(ctx, v)
	e.st.updateFactory(func(f *core.Factory) { f.ActiveVersionID = v.ID })
	return v, nil
}

func (e *judgeEngine) ResolveInbox(ctx context.Context, inboxID, resolution, actor string) error {
	e.resolved = append(e.resolved, inboxID+"="+resolution)
	return nil
}

func (e *judgeEngine) FollowUp(ctx context.Context, runID, message, actor string) error {
	e.followUps = append(e.followUps, runID+": "+message)
	return nil
}

type judgeStarter struct {
	mu   sync.Mutex
	st   *fakeStore
	now  func() time.Time
	reqs []core.JudgeRunRequest
	runs []*core.Run
}

func (j *judgeStarter) start(ctx context.Context, req core.JudgeRunRequest) (*core.Run, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r := &core.Run{ID: core.NewID("run"), FactoryID: req.FactoryID, AgentName: req.Name, Name: req.Name, State: core.RunQueued,
		Prompt: req.Prompt, Metadata: req.Metadata, TriggeredBy: req.TriggeredBy, CreatedAt: j.now()}
	j.st.putRun(r)
	j.reqs = append(j.reqs, req)
	j.runs = append(j.runs, r)
	return r, nil
}

func (j *judgeStarter) last() (core.JudgeRunRequest, *core.Run) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.reqs[len(j.reqs)-1], j.runs[len(j.runs)-1]
}

func (j *judgeStarter) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.reqs)
}

type judgeEnvT struct {
	*env
	starter *judgeStarter
	jeng    *judgeEngine
}

// newJudgeEnv is newEnv with judge runs and without a direct model call.
func newJudgeEnv(t *testing.T, bench bool) *judgeEnvT {
	t.Helper()
	var e *env
	if bench {
		e = benchEnv(t)
	} else {
		e = newEnv(t)
	}
	js := &judgeStarter{st: e.st, now: e.clock.Now}
	je := &judgeEngine{fakeEngine: e.eng}
	svc, err := NewService(Options{
		Store:      e.st,
		Bus:        e.bus,
		StartJudge: js.start,
		Engine:     je,
		Compile: func(files map[string]string) (*core.CompiledDefinition, []core.ValidationError) {
			if strings.Contains(files["factory.yaml"], "INVALID") {
				return nil, []core.ValidationError{{File: "factory.yaml", Message: "broken on purpose"}}
			}
			return e.def, nil
		},
		Clock:         e.clock.Now,
		Rand:          rand.New(rand.NewPCG(1, 2)),
		CheckInterval: -1,
		Logger:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return &judgeEnvT{env: e, starter: js, jeng: je}
}

// finishJudge completes a judge run with a final message and usage.
func (e *judgeEnvT) finishJudge(t *testing.T, r *core.Run, state core.RunState, result string, cost float64) *core.Run {
	t.Helper()
	cur, err := e.st.GetRun(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur.State = state
	cur.Result = result
	cur.Usage = core.Usage{InputTokens: 10, OutputTokens: 5, CostUSD: cost}
	fin := e.clock.Now()
	cur.FinishedAt = &fin
	e.st.putRun(cur)
	return cur
}

// ---------- tests ----------

func TestJudgeRunScoresRun(t *testing.T) {
	ctx := context.Background()
	e := newJudgeEnv(t, false)
	h := &core.HarnessConfig{Type: core.HarnessClaude, Model: "team-model", Auth: &core.HarnessAuth{SecretName: "TEAM_KEY"}}
	e.addScorer(core.Scorer{
		ID: "sc_1", Slug: "tests-run", Name: "tests-run", Rubric: "Did the agent run the tests?",
		Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100, Model: "judge-model",
		Harness: h, Machine: "big", Secrets: []string{"JUDGE_TOKEN"},
		MCPServers: map[string]core.MCPServer{"docs": {URL: "https://docs.example.com/mcp"}},
	})
	long := strings.Repeat("x", 10_000) + " go test ./... passed"
	run := e.finishedRun("run_a", "implement", core.RunSucceeded, long)
	if err := core.SaveStartCommits(ctx, e.st, run.ID, []core.RepoStart{{CodeForge: "GITHUB", Owner: "acme", Repo: "app", Ref: sha, Resolved: true}}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.HandleRunUpdate(ctx, run); err != nil {
		t.Fatal(err)
	}
	if e.starter.count() != 1 {
		t.Fatalf("judge runs = %d, want 1", e.starter.count())
	}
	req, judge := e.starter.last()
	// Each scorer field changes the judge run.
	if req.Harness.Type != core.HarnessClaude || req.Harness.Model != "judge-model" || req.Harness.Auth == nil || req.Harness.Auth.SecretName != "TEAM_KEY" {
		t.Fatalf("harness %+v", req.Harness)
	}
	if req.Machine != "big" || len(req.Secrets) != 1 || req.Secrets[0] != "JUDGE_TOKEN" || req.MCPServers["docs"].URL == "" {
		t.Fatalf("execution %+v", req)
	}
	if !strings.Contains(req.Instructions, "Did the agent run the tests?") || !strings.Contains(req.Instructions, "`tests_run`") {
		t.Fatalf("instructions %q", req.Instructions)
	}
	if tr := req.Files["transcript.md"]; !strings.Contains(tr, long) {
		t.Fatalf("the judge must get the full transcript (%d bytes)", len(tr))
	}
	if len(req.StartingRepoRefs) != 1 || req.StartingRepoRefs[0].Ref != sha {
		t.Fatalf("start commits %+v", req.StartingRepoRefs)
	}
	if req.TriggeredBy != "scorer:sc_1" || req.Metadata[MetaJudge] != JudgeScore || req.Metadata[MetaJudgeScoredRun] != run.ID {
		t.Fatalf("metadata %+v %q", req.Metadata, req.TriggeredBy)
	}
	if len(e.st.scoresFor(run.ID)) != 0 {
		t.Fatal("the score is stored before the judge finished")
	}
	// A repeated update does not start a second judge.
	if err := e.svc.HandleRunUpdate(ctx, run); err != nil {
		t.Fatal(err)
	}
	if e.starter.count() != 1 {
		t.Fatalf("judge runs after re-delivery = %d", e.starter.count())
	}

	done := e.finishJudge(t, judge, core.RunSucceeded, "The tests ran.\n{\"label\": \"tests_run\", \"rationale\": \"go test passed\"}", 0.25)
	if err := e.svc.HandleRunUpdate(ctx, done); err != nil {
		t.Fatal(err)
	}
	scores := e.st.scoresFor(run.ID)
	if len(scores) != 1 || scores[0].Label != "tests_run" || !scores[0].Passed || scores[0].JudgeRunID != judge.ID || scores[0].JudgeUsage == nil || scores[0].JudgeUsage.CostUSD != 0.25 {
		t.Fatalf("scores %+v", scores)
	}
	// The judge run is never scored, and a repeated update changes nothing.
	if err := e.svc.HandleRunUpdate(ctx, done); err != nil {
		t.Fatal(err)
	}
	if len(e.st.scoresFor(judge.ID)) != 0 || e.starter.count() != 1 || len(e.st.scoresFor(run.ID)) != 1 {
		t.Fatal("judge run was scored or scored twice")
	}
}

func TestJudgeRunWithoutLabel(t *testing.T) {
	ctx := context.Background()
	e := newJudgeEnv(t, false)
	e.addScorer(core.Scorer{ID: "sc_1", Slug: "s", Name: "s", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100,
		Harness: &core.HarnessConfig{Type: core.HarnessCodex, Model: "m"}})
	run := e.finishedRun("run_b", "implement", core.RunFailed, "oops")
	if _, err := e.svc.ScoreRun(ctx, "s", run.ID); err != nil {
		t.Fatal(err)
	}
	_, judge := e.starter.last()
	if err := e.svc.HandleRunUpdate(ctx, e.finishJudge(t, judge, core.RunSucceeded, "I am not sure.", 0)); err != nil {
		t.Fatal(err)
	}
	if len(e.st.scoresFor(run.ID)) != 0 {
		t.Fatal("a judge without a label stored a score")
	}
	// Scoring again starts a new judge run.
	sc, err := e.svc.ScoreRun(ctx, "s", run.ID)
	if err != nil || sc.ID != "" || sc.JudgeRunID == judge.ID || e.starter.count() != 2 {
		t.Fatalf("rescore: %+v %v (judges %d)", sc, err, e.starter.count())
	}
}

// TestJudgeThatWaitsIsStopped shows that a judge run that ends its turn and
// waits for input is cancelled after its answer. A judge has one turn.
func TestJudgeThatWaitsIsStopped(t *testing.T) {
	ctx := context.Background()
	e := newJudgeEnv(t, false)
	e.addScorer(core.Scorer{ID: "sc_1", Slug: "s", Name: "s", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100,
		Labels:  []core.ScorerLabel{{Value: "ok", Score: 1}, {Value: "bad", Score: 0}},
		Harness: &core.HarnessConfig{Type: core.HarnessCodex, Model: "m"}})
	run := e.finishedRun("run_i", "implement", core.RunSucceeded, "done")
	if _, err := e.svc.ScoreRun(ctx, "s", run.ID); err != nil {
		t.Fatal(err)
	}
	_, judge := e.starter.last()
	if err := e.svc.HandleRunUpdate(ctx, e.finishJudge(t, judge, core.RunIdle, `{"label": "ok", "rationale": "fine"}`, 0.1)); err != nil {
		t.Fatal(err)
	}
	if got := e.st.scoresFor(run.ID); len(got) != 1 || got[0].Label != "ok" {
		t.Fatalf("scores %+v", got)
	}
	if len(e.eng.cancels) != 1 || !strings.Contains(e.eng.cancels[0], judge.ID) {
		t.Fatalf("cancels %v", e.eng.cancels)
	}
}

func TestScorerWithoutHarnessFails(t *testing.T) {
	e := newJudgeEnv(t, false)
	e.addScorer(core.Scorer{ID: "sc_1", Slug: "s", Name: "s", Agents: []core.ScorerAgent{{Name: "implement"}}, PassingScore: 1, SamplingRate: 100})
	run := e.finishedRun("run_c", "implement", core.RunSucceeded, "x")
	if _, err := e.svc.ScoreRun(context.Background(), "s", run.ID); err == nil || !strings.Contains(err.Error(), "no harness") {
		t.Fatalf("want an error about the harness, got %v", err)
	}
}

func TestParseJudgeAnswer(t *testing.T) {
	cases := []struct{ in, label, rationale string }{
		{`{"label":"a","rationale":"r"}`, "a", "r"},
		{"text {\"label\": \"first\"}\nmore {\"label\": \"last\", \"rationale\": \"why\"}", "last", "why"},
		{"```json\n{\"label\": \"b\"}\n```", "b", ""},
		{`{"x": {"label": "nested"}}`, "nested", ""},
	}
	for _, c := range cases {
		l, r, ok := parseJudgeAnswer(c.in)
		if !ok || l != c.label || r != c.rationale {
			t.Errorf("%q: got %q %q %v", c.in, l, r, ok)
		}
	}
	if _, _, ok := parseJudgeAnswer("no json here {"); ok {
		t.Error("want no answer")
	}
}

func TestBenchmarkJudgeRuns(t *testing.T) {
	ctx := context.Background()
	e := newJudgeEnv(t, true)
	e.def.Machines = map[string]core.MachineSpec{"large": {Name: "large"}}
	e.def.ScorerDefaults = &core.AgentOverrides{Machine: "large", Harness: &core.HarnessConfig{Type: core.HarnessClaude, Model: "default-judge"}}
	e.def.Benchmarks[0].Configurations[1].Machine = "large"
	e.def.Benchmarks[0].Tasks[1].TrackerSeed = map[string]any{"issue": "ENG-1", "state": "Todo"}

	bench, err := e.svc.StartBenchmarkWithOptions(ctx, BenchmarkRequest{FactoryID: testFactory, Suite: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range e.eng.dispatches {
		if d.Metadata[core.MetaTrialMode] != "true" {
			t.Fatalf("trial not in trial mode: %v", d.Metadata)
		}
		if !strings.Contains(d.Prompt, "trial mode") {
			t.Fatalf("prompt does not explain trial mode: %q", d.Prompt)
		}
		if d.Metadata[MetaBenchmarkTask] == "typo" && !strings.Contains(d.Prompt, "ENG-1") {
			t.Fatalf("prompt has no tracker seed: %q", d.Prompt)
		}
		if d.Metadata[MetaBenchmarkConfiguration] == "Candidate" {
			if d.Overrides.Machine != "large" || d.AgentPins["review"].Machine != "large" || d.AgentPins["foreman"].Machine != "large" {
				t.Fatalf("machine of the configuration: %+v %+v", d.Overrides, d.AgentPins)
			}
		} else if d.Overrides.Machine != "" {
			t.Fatalf("baseline has a machine: %+v", d.Overrides)
		}
	}
	_, res := mustLoad(t, e.env, bench.ID)
	if res.CorrectnessJudge.Harness == nil || res.CorrectnessJudge.Harness.Model != "default-judge" || res.CorrectnessJudge.Machine != "large" {
		t.Fatalf("correctness judge %+v", res.CorrectnessJudge)
	}
	for _, tr := range res.Trials {
		run := e.finishTrial(t, tr.RunID, core.RunSucceeded, "fixed it", 1, time.Minute)
		if err := e.svc.HandleRunUpdate(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	if e.starter.count() != len(res.Trials) {
		t.Fatalf("judge runs = %d, want %d", e.starter.count(), len(res.Trials))
	}
	b, _ := mustLoad(t, e.env, bench.ID)
	if b.State != BenchmarkScoring {
		t.Fatalf("state with judges at work = %s", b.State)
	}
	e.starter.mu.Lock()
	judges := append([]*core.Run(nil), e.starter.runs...)
	reqs := append([]core.JudgeRunRequest(nil), e.starter.reqs...)
	e.starter.mu.Unlock()
	for i, j := range judges {
		if reqs[i].Machine != "large" || reqs[i].Harness.Model != "default-judge" || reqs[i].Metadata[MetaJudge] != JudgeTrial {
			t.Fatalf("trial judge request %+v", reqs[i])
		}
		if reqs[i].Metadata[core.MetaTrialMode] != "" || reqs[i].Metadata[MetaBenchmarkRun] != "" {
			t.Fatalf("a judge run must not look like a trial: %v", reqs[i].Metadata)
		}
		done := e.finishJudge(t, j, core.RunSucceeded, `{"label": "correct", "rationale": "ok"}`, 0.5)
		if err := e.svc.HandleRunUpdate(ctx, done); err != nil {
			t.Fatal(err)
		}
	}
	b, res = mustLoad(t, e.env, bench.ID)
	if b.State != BenchmarkComplete {
		t.Fatalf("final state %s", b.State)
	}
	n := float64(len(res.Trials))
	if !approx(res.JudgeCostUSD, 0.5*n) || !approx(res.TotalCostUSD, 1.5*n) {
		t.Fatalf("costs: judge %v total %v", res.JudgeCostUSD, res.TotalCostUSD)
	}
	for _, s := range res.Summary {
		if s.PassRate != 1 || !approx(s.AvgCostUSD, 1) {
			t.Fatalf("summary %+v", s)
		}
	}
}

func TestBenchmarkCancelStopsJudges(t *testing.T) {
	ctx := context.Background()
	e := newJudgeEnv(t, true)
	e.def.ScorerDefaults = &core.AgentOverrides{Harness: &core.HarnessConfig{Type: core.HarnessClaude, Model: "m"}}
	bench, err := e.svc.StartBenchmark(ctx, testFactory, "fix", []core.BenchmarkConfiguration{{Name: "only"}})
	if err != nil {
		t.Fatal(err)
	}
	_, res := mustLoad(t, e.env, bench.ID)
	run := e.finishTrial(t, res.Trials[0].RunID, core.RunSucceeded, "fixed", 1, time.Minute)
	if err := e.svc.HandleRunUpdate(ctx, run); err != nil {
		t.Fatal(err)
	}
	_, judge := e.starter.last()
	if _, err := e.svc.CancelBenchmark(ctx, bench.ID, "user:u1"); err != nil {
		t.Fatal(err)
	}
	cancelled := map[string]bool{}
	for _, id := range e.eng.cancels {
		cancelled[id] = true
	}
	if !cancelled[judge.ID] || !cancelled[res.Trials[1].RunID] {
		t.Fatalf("cancels %v", e.eng.cancels)
	}
	b, res := mustLoad(t, e.env, bench.ID)
	if b.State != BenchmarkCancelled || res.Trials[0].Scores[0].Pending {
		t.Fatalf("after cancel: %s %+v", b.State, res.Trials[0].Scores)
	}
}

func TestProposeApproveReject(t *testing.T) {
	ctx := context.Background()
	e := newJudgeEnv(t, false)
	author := &core.Run{ID: "run_si", FactoryID: testFactory, WorkItemID: "wi_1", AgentName: "foreman", State: core.RunRunning,
		Metadata: map[string]string{MetaSelfImprovement: "true"}, CreatedAt: e.clock.Now()}
	e.st.putRun(author)

	// An invalid change is refused with the errors.
	_, err := e.svc.ProposeDefinitionChange(ctx, ProposalRequest{RunID: author.ID, Message: "x", Files: map[string]string{"factory.yaml": "INVALID"}})
	var pe *ProposalError
	if !errors.As(err, &pe) || len(pe.Errors) != 1 {
		t.Fatalf("want a ProposalError, got %v", err)
	}
	if _, err := e.svc.ProposeDefinitionChange(ctx, ProposalRequest{RunID: author.ID, Message: "x", Files: map[string]string{"../etc/passwd": "x"}}); err == nil {
		t.Fatal("want an error for a path outside the definition")
	}

	v, err := e.svc.ProposeDefinitionChange(ctx, ProposalRequest{RunID: author.ID, Message: "Tell implement to run tests",
		Files: map[string]string{"agents/implement/agent.md": "---\nname: implement\n---\nAlways write code. Run the tests.\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if v.State != core.DefinitionProposed || v.BaseVersionID != "ver_1" || v.AuthorRunID != author.ID || v.InboxItemID == "" {
		t.Fatalf("proposal %+v", v)
	}
	if len(e.st.inbox) != 1 {
		t.Fatalf("inbox %+v", e.st.inbox)
	}
	it := e.st.inbox[0]
	if it.Kind != core.InboxReview || it.RunID != author.ID || it.ID != v.InboxItemID || it.AssigneeID != "u_admin" ||
		!strings.Contains(it.Body, "+Always write code. Run the tests.") || !strings.Contains(it.Body, "-Always write code.") {
		t.Fatalf("review item %+v", it)
	}
	list, err := e.svc.ListProposals(ctx, testFactory, false)
	if err != nil || len(list) != 1 || list[0].ID != v.ID {
		t.Fatalf("list %+v %v", list, err)
	}

	// A second proposal on the same base; approve the first, the second conflicts.
	v2, err := e.svc.ProposeDefinitionChange(ctx, ProposalRequest{RunID: author.ID, Message: "Other", Delete: []string{"agents/review/agent.md"}})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := e.svc.ApproveProposal(ctx, testFactory, v.ID, "user:u_admin")
	if err != nil {
		t.Fatal(err)
	}
	if approved.State != core.DefinitionApproved || approved.AppliedVersionID == "" || len(e.jeng.applied) != 1 ||
		e.jeng.applied[0]["agents/implement/agent.md"] == "" || len(e.jeng.resolved) != 1 {
		t.Fatalf("approve: %+v applied=%d resolved=%v", approved, len(e.jeng.applied), e.jeng.resolved)
	}
	if _, err := e.svc.ApproveProposal(ctx, testFactory, v2.ID, "user:u_admin"); err == nil || !strings.Contains(err.Error(), "changed after the proposal") {
		t.Fatalf("want a conflict, got %v", err)
	}
	rej, err := e.svc.RejectProposal(ctx, testFactory, v2.ID, "user:u_admin", "Keep the review agent.")
	if err != nil || rej.State != core.DefinitionRejected || rej.ReviewComment != "Keep the review agent." {
		t.Fatalf("reject: %+v %v", rej, err)
	}
	if last := e.jeng.resolved[len(e.jeng.resolved)-1]; !strings.Contains(last, "rejected: Keep the review agent.") {
		t.Fatalf("rejection not sent to the run: %v", e.jeng.resolved)
	}
	if _, err := e.svc.RejectProposal(ctx, testFactory, v.ID, "user:u_admin", ""); err == nil {
		t.Fatal("an approved proposal cannot be rejected")
	}

	// A git definition is reviewed on a pull request.
	e.st.updateFactory(func(f *core.Factory) { f.Source = core.DefinitionGit; f.GitRepo = "github.com/acme/def" })
	if _, err := e.svc.ProposeDefinitionChange(ctx, ProposalRequest{RunID: author.ID, Message: "x", Files: map[string]string{"a.md": "x"}}); err == nil || !strings.Contains(err.Error(), "pull request") {
		t.Fatalf("git source: %v", err)
	}
}

func TestMakeTaskFromRun(t *testing.T) {
	ctx := context.Background()
	e := newJudgeEnv(t, true)
	e.st.versions["ver_1"].Files["benchmarks/fix/suite.yaml"] = "name: Fix\nagent: implement\ntasks:\n  - broken-link\n"
	run := e.finishedRun("run_src", "implement", core.RunSucceeded, "done")
	run.Prompt = "Fix the login page."
	run.Name = "Login fix"
	e.st.putRun(run)
	_, err := e.svc.MakeTaskFromRun(ctx, TaskFromRunRequest{FactoryID: testFactory, Suite: "fix", RunID: run.ID, SuccessCriteria: "Login works."})
	if err != nil {
		t.Fatal(err) // no repositories: a task without starting refs
	}
	if err := core.SaveStartCommits(ctx, e.st, run.ID, []core.RepoStart{{CodeForge: "GITHUB", Owner: "acme", Repo: "app", Ref: "main"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.MakeTaskFromRun(ctx, TaskFromRunRequest{FactoryID: testFactory, Suite: "fix", RunID: run.ID, SuccessCriteria: "Login works."}); err == nil {
		t.Fatal("want an error for a start commit that is not resolved")
	}
	if err := core.SaveStartCommits(ctx, e.st, run.ID, []core.RepoStart{{CodeForge: "GITHUB", Owner: "acme", Repo: "app", Ref: sha, Resolved: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.MakeTaskFromRun(ctx, TaskFromRunRequest{FactoryID: testFactory, Suite: "fix", RunID: run.ID, SuccessCriteria: "Login works."})
	if err != nil {
		t.Fatal(err)
	}
	if got.Task.Slug != "login-fix-run-src" || got.Path != "benchmarks/fix/tasks/login-fix-run-src.yaml" {
		t.Fatalf("task %+v path %s", got.Task, got.Path)
	}
	task := got.Files[got.Path]
	if !strings.Contains(task, "github.com:acme/app@"+sha) || !strings.Contains(task, "sourceRunId: run_src") || !strings.Contains(task, "Fix the login page.") {
		t.Fatalf("task file %q", task)
	}
	if suite := got.Files["benchmarks/fix/suite.yaml"]; !strings.Contains(suite, "- broken-link") || !strings.Contains(suite, "- login-fix-run-src") {
		t.Fatalf("suite file %q", suite)
	}
	if got.Definition["factory.yaml"] == "" || got.Definition[got.Path] == "" {
		t.Fatal("full definition is missing files")
	}
	if _, err := e.svc.MakeTaskFromRun(ctx, TaskFromRunRequest{FactoryID: testFactory, Suite: "fix", RunID: run.ID}); err == nil {
		t.Fatal("success criteria are required")
	}
}

func TestDiffFiles(t *testing.T) {
	d := DiffFiles(map[string]string{"a": "1\n2\n3\n", "gone": "x\n"}, map[string]string{"a": "1\n3\n4\n", "new": "y\n"})
	for _, want := range []string{"--- a/a\n+++ b/a\n@@ -1,3 +1,3 @@\n 1\n-2\n 3\n+4\n", "--- a/gone\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-x\n", "--- /dev/null\n+++ b/new\n@@ -0,0 +1,1 @@\n+y\n"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff misses %q:\n%s", want, d)
		}
	}
	if DiffFiles(map[string]string{"a": "1"}, map[string]string{"a": "1"}) != "" {
		t.Error("equal trees give a diff")
	}
	// A change in a long file shows only the lines around it.
	var long []string
	for i := 1; i <= 100; i++ {
		long = append(long, fmt.Sprintf("line %d", i))
	}
	old := strings.Join(long, "\n") + "\n"
	long[49] = "changed"
	d = DiffFiles(map[string]string{"f": old}, map[string]string{"f": strings.Join(long, "\n") + "\n"})
	if !strings.Contains(d, "@@ -47,7 +47,7 @@\n line 47\n line 48\n line 49\n-line 50\n+changed\n line 51\n") || strings.Contains(d, "line 1\n") || strings.Count(d, "\n") > 12 {
		t.Errorf("long file diff:\n%s", d)
	}
}

func TestAnalysisModelFromDefinition(t *testing.T) {
	e := newEnv(t)
	e.svc.opts.AnalysisModel = "server-model"
	if got := e.svc.improvementSettings(nil).AnalysisModel; got != "server-model" {
		t.Fatalf("server default: %q", got)
	}
	def := &core.CompiledDefinition{SelfImprovement: &core.SelfImprovementConfig{AnalysisModel: "def-model"}}
	if got := e.svc.improvementSettings(def).AnalysisModel; got != "def-model" {
		t.Fatalf("definition value: %q", got)
	}
}
