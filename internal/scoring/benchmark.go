package scoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Benchmark run states (core.BenchmarkRun.State).
const (
	BenchmarkRunning   = "running"   // trials are still executing
	BenchmarkScoring   = "scoring"   // every trial finished; some are still being scored
	BenchmarkComplete  = "complete"  // every trial finished and was scored
	BenchmarkFailed    = "failed"    // no trial could be started
	BenchmarkCancelled = "cancelled" // cancelled by a user
)

// Trial run metadata keys (become OF_METADATA_* in the sandbox).
const (
	MetaBenchmarkRun           = "benchmark_run"
	MetaBenchmarkTrial         = "benchmark_trial"
	MetaBenchmarkConfiguration = "benchmark_configuration"
	MetaBenchmarkTask          = "benchmark_task"
	MetaBenchmarkRepetition    = "benchmark_repetition"
)

// Limits.
const (
	MaxBenchmarkConfigurations = 6
	MaxBenchmarkRepetitions    = 10
	maxMetadataValue           = 256
)

// CorrectnessScorer is the key of the built-in Correctness Scorer in trial results.
const CorrectnessScorer = "correctness"

// TrialStatus is the lifecycle of one benchmark trial.
type TrialStatus string

const (
	TrialPending   TrialStatus = "pending"
	TrialRunning   TrialStatus = "running"
	TrialFinished  TrialStatus = "finished" // run reached a terminal (or idle) state
	TrialBlocked   TrialStatus = "blocked"  // run is idle waiting on a human; its evidence is still scored
	TrialError     TrialStatus = "error"    // the trial could not be started
	TrialCancelled TrialStatus = "cancelled"
)

// BenchmarkRequest launches a suite.
type BenchmarkRequest struct {
	FactoryID string
	Suite     string // suite slug (or name)
	// Configurations to compare; empty uses the suite's configurations, and a
	// suite without configurations runs one "default" configuration.
	Configurations []core.BenchmarkConfiguration
	// Repetitions per configuration × task (default 1).
	Repetitions int
	// Scorers are additional factory scorers (ID or slug) applied to every
	// trial besides the built-in Correctness Scorer.
	Scorers []string
}

// TrialScore is one scorer's classification of a trial.
type TrialScore struct {
	Scorer    string  `json:"scorer"`
	Label     string  `json:"label,omitempty"`
	Score     float64 `json:"score"`
	Passed    bool    `json:"passed"`
	Rationale string  `json:"rationale,omitempty"`
	Error     string  `json:"error,omitempty"`
	// JudgeRunID is the judge run of this score. Pending is true
	// while it works.
	JudgeRunID   string  `json:"judge_run_id,omitempty"`
	Pending      bool    `json:"pending,omitempty"`
	JudgeCostUSD float64 `json:"judge_cost_usd,omitempty"`
}

// Trial is one run of one task under one configuration.
type Trial struct {
	ID               string              `json:"id"`
	Configuration    string              `json:"configuration"`
	Task             string              `json:"task"`
	Repetition       int                 `json:"repetition"`
	Status           TrialStatus         `json:"status"`
	RunID            string              `json:"run_id,omitempty"`
	WorkItemID       string              `json:"work_item_id,omitempty"`
	RunState         core.RunState       `json:"run_state,omitempty"`
	Error            string              `json:"error,omitempty"`
	StartingRepoRefs []core.RepoAtCommit `json:"starting_repo_refs,omitempty"`
	CostUSD          float64             `json:"cost_usd"`
	InputTokens      int64               `json:"input_tokens"`
	OutputTokens     int64               `json:"output_tokens"`
	DurationMS       int64               `json:"duration_ms"`
	StartedAt        *time.Time          `json:"started_at,omitempty"`
	FinishedAt       *time.Time          `json:"finished_at,omitempty"`
	Scored           bool                `json:"scored"`
	Scores           []TrialScore        `json:"scores,omitempty"`
	// JudgeCostUSD is the cost of the judge runs of the trial.
	JudgeCostUSD float64 `json:"judge_cost_usd,omitempty"`
}

// done reports whether the trial needs no further work.
func (t *Trial) done() bool {
	switch t.Status {
	case TrialError:
		return true
	case TrialCancelled:
		return t.Scored || t.RunID == ""
	case TrialFinished, TrialBlocked:
		return t.Scored
	}
	return false
}

func (t *Trial) runFinished() bool {
	return t.Status == TrialFinished || t.Status == TrialBlocked || t.Status == TrialCancelled || t.Status == TrialError
}

func (t *Trial) score(key string) *TrialScore {
	for i := range t.Scores {
		if t.Scores[i].Scorer == key {
			return &t.Scores[i]
		}
	}
	return nil
}

// BenchmarkScorer snapshots a scorer used by a benchmark run, so later suite
// or scorer edits don't change past results.
type BenchmarkScorer struct {
	Key          string             `json:"key"`
	ScorerID     string             `json:"scorer_id,omitempty"`
	Name         string             `json:"name"`
	Model        string             `json:"model"`
	Rubric       string             `json:"rubric"`
	Labels       []core.ScorerLabel `json:"labels"`
	PassingScore float64            `json:"passing_score"`
	// The execution settings of the judge run.
	JudgeExec
}

// ConfigurationResult aggregates one configuration's trials.
type ConfigurationResult struct {
	Name     string `json:"name"`
	Trials   int    `json:"trials"`
	Finished int    `json:"finished"`
	Blocked  int    `json:"blocked"`
	Errored  int    `json:"errored"`
	// Scored / Passed count Correctness Scorer results (judge errors excluded).
	Scored   int     `json:"scored"`
	Passed   int     `json:"passed"`
	PassRate float64 `json:"pass_rate"`
	// ScorerAverages is the mean label score per scorer key; Overall is the
	// mean of ScorerAverages.
	ScorerAverages   map[string]float64 `json:"scorer_averages"`
	Overall          float64            `json:"overall"`
	TotalCostUSD     float64            `json:"total_cost_usd"`
	AvgCostUSD       float64            `json:"avg_cost_usd"`
	AvgDurationMS    int64              `json:"avg_duration_ms"`
	MedianDurationMS int64              `json:"median_duration_ms"`
	InputTokens      int64              `json:"input_tokens"`
	OutputTokens     int64              `json:"output_tokens"`
	// JudgeCostUSD is the cost of the judge runs of the trials. It is not in
	// TotalCostUSD and AvgCostUSD, which compare the configurations.
	JudgeCostUSD float64 `json:"judge_cost_usd"`
}

// Recommendations names standout configurations. Each is empty when the
// results don't support the comparison (fewer than two scored configurations).
type Recommendations struct {
	Overall       string `json:"overall,omitempty"`        // highest Overall quality (ties: lower cost)
	MostEfficient string `json:"most_efficient,omitempty"` // highest Overall per dollar
	LowestCost    string `json:"lowest_cost,omitempty"`    // lowest average trial cost
}

// BenchmarkResults is the JSON stored in core.BenchmarkRun.Results. It keeps
// the run's inputs so later suite changes don't alter past results.
type BenchmarkResults struct {
	Suite            core.BenchmarkSuite           `json:"suite"`
	Configurations   []core.BenchmarkConfiguration `json:"configurations"`
	Repetitions      int                           `json:"repetitions"`
	CorrectnessModel string                        `json:"correctness_model"`
	// CorrectnessJudge is the execution of the judge runs of the
	// Correctness Scorer: scorerDefaults over agentDefaults.
	CorrectnessJudge JudgeExec             `json:"correctness_judge"`
	Scorers          []BenchmarkScorer     `json:"scorers,omitempty"`
	Trials           []Trial               `json:"trials"`
	Summary          []ConfigurationResult `json:"summary"`
	Recommendations  Recommendations       `json:"recommendations"`
	// TotalCostUSD is the cost of the trials and of the judge runs.
	TotalCostUSD float64 `json:"total_cost_usd"`
	// JudgeCostUSD is the part of TotalCostUSD that the judge runs used.
	JudgeCostUSD float64 `json:"judge_cost_usd"`
	CancelledBy  string  `json:"cancelled_by,omitempty"`
}

// DecodeBenchmarkResults parses core.BenchmarkRun.Results.
func DecodeBenchmarkResults(b *core.BenchmarkRun) (*BenchmarkResults, error) {
	res := &BenchmarkResults{}
	if len(b.Results) == 0 {
		return res, nil
	}
	if err := json.Unmarshal(b.Results, res); err != nil {
		return nil, fmt.Errorf("decode benchmark results %s: %w", b.ID, err)
	}
	return res, nil
}

// StartBenchmark launches suiteSlug with the given configurations (empty =
// the suite's configurations), one repetition and only the Correctness Scorer.
func (s *Service) StartBenchmark(ctx context.Context, factoryID, suiteSlug string, configs []core.BenchmarkConfiguration) (*core.BenchmarkRun, error) {
	return s.StartBenchmarkWithOptions(ctx, BenchmarkRequest{FactoryID: factoryID, Suite: suiteSlug, Configurations: configs})
}

// StartBenchmarkWithOptions launches a benchmark: one isolated trial run of
// the suite agent per configuration × task × repetition, each pinned to the
// configuration's model/harness, starting from the task's repo commits (after
// benchmarkRepoSubstitutions) with the task prompt.
func (s *Service) StartBenchmarkWithOptions(ctx context.Context, req BenchmarkRequest) (*core.BenchmarkRun, error) {
	if s.opts.Engine == nil {
		return nil, errors.New("scoring: Engine is required for benchmarks")
	}
	f, v, def, err := s.activeDefinition(ctx, req.FactoryID)
	if err != nil {
		return nil, err
	}
	if def == nil {
		if s.opts.Compile == nil {
			return nil, errors.New("scoring: Compile is required for benchmarks")
		}
		return nil, fmt.Errorf("%w: factory %s has no active definition", core.ErrInvalid, f.ID)
	}
	suite, ok := findSuite(def.Benchmarks, req.Suite)
	if !ok {
		return nil, fmt.Errorf("benchmark suite %q: %w", req.Suite, core.ErrNotFound)
	}
	if len(suite.Tasks) == 0 {
		return nil, fmt.Errorf("%w: benchmark suite %q has no tasks", core.ErrInvalid, suite.Slug)
	}
	configs := req.Configurations
	if len(configs) == 0 {
		configs = suite.Configurations
	}
	if len(configs) == 0 {
		configs = []core.BenchmarkConfiguration{{Name: "default"}}
	}
	if err := validateConfigurations(configs, def.Agents, def.Machines); err != nil {
		return nil, err
	}
	reps := req.Repetitions
	if reps <= 0 {
		reps = 1
	}
	if reps > MaxBenchmarkRepetitions {
		return nil, fmt.Errorf("%w: repetitions must be at most %d", core.ErrInvalid, MaxBenchmarkRepetitions)
	}
	extra, err := s.benchmarkScorers(ctx, f.ID, req.Scorers)
	if err != nil {
		return nil, err
	}
	subs := def.BenchmarkRepoSubstitutions
	if subs == nil && v != nil {
		if subs, err = ParseRepoSubstitutions(v.Files); err != nil {
			return nil, err
		}
	}

	now := s.now()
	bench := &core.BenchmarkRun{
		ID:        core.NewID("bench"),
		FactoryID: f.ID,
		Suite:     suite.Slug,
		State:     BenchmarkRunning,
		CreatedAt: now,
	}
	res := &BenchmarkResults{
		Suite:            suite,
		Configurations:   configs,
		Repetitions:      reps,
		CorrectnessModel: s.opts.CorrectnessModel,
		CorrectnessJudge: s.correctnessJudge(def),
		Scorers:          extra,
	}
	for _, cfg := range configs {
		for _, task := range suite.Tasks {
			for rep := 1; rep <= reps; rep++ {
				res.Trials = append(res.Trials, Trial{
					ID:               core.NewID("trial"),
					Configuration:    cfg.Name,
					Task:             task.Slug,
					Repetition:       rep,
					Status:           TrialPending,
					StartingRepoRefs: ApplyRepoSubstitutions(task.StartingRepoRefs, subs),
				})
			}
		}
	}

	// Hold the benchmark lock while dispatching so trial completions that
	// arrive early wait until every trial's run id is recorded.
	unlock := s.lockKey("bench:" + bench.ID)
	defer unlock()
	if err := s.saveBenchmark(ctx, bench, res, true); err != nil {
		return nil, err
	}
	cfgByName := map[string]core.BenchmarkConfiguration{}
	for _, c := range configs {
		cfgByName[c.Name] = c
	}
	taskBySlug := map[string]core.BenchmarkTask{}
	for _, t := range suite.Tasks {
		taskBySlug[t.Slug] = t
	}
	started := 0
	for i := range res.Trials {
		if ctx.Err() != nil {
			break
		}
		t := &res.Trials[i]
		dreq := trialDispatch(bench, suite, cfgByName[t.Configuration], taskBySlug[t.Task], t, reps, agentNames(def.Agents))
		wi, run, err := s.opts.Engine.Dispatch(ctx, dreq)
		if err == nil && run == nil {
			err = errors.New("engine returned no run")
		}
		if err != nil {
			t.Status = TrialError
			t.Error = err.Error()
			s.log.Error("benchmark trial dispatch", "benchmark_run_id", bench.ID, "trial", t.ID, "err", err)
			continue
		}
		started++
		t.Status = TrialRunning
		t.RunID = run.ID
		if wi != nil {
			t.WorkItemID = wi.ID
		}
		ts := s.now()
		t.StartedAt = &ts
	}
	for i := range res.Trials {
		if res.Trials[i].Status == TrialPending {
			res.Trials[i].Status = TrialError
			res.Trials[i].Error = "not started: " + errString(ctx.Err())
		}
	}
	if started == 0 {
		bench.State = BenchmarkFailed
		fin := s.now()
		bench.FinishedAt = &fin
	}
	if err := s.saveBenchmark(ctx, bench, res, false); err != nil {
		return nil, err
	}
	s.log.Info("benchmark started", "benchmark_run_id", bench.ID, "suite", suite.Slug, "trials", len(res.Trials), "started", started)
	return bench, nil
}

func errString(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

func findSuite(suites []core.BenchmarkSuite, slug string) (core.BenchmarkSuite, bool) {
	for _, s := range suites {
		if s.Slug == slug {
			return s, true
		}
	}
	for _, s := range suites {
		if s.Name == slug {
			return s, true
		}
	}
	return core.BenchmarkSuite{}, false
}

func agentNames(agents []core.AgentConfig) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Name)
	}
	return out
}

// correctnessJudge returns the execution of the Correctness Scorer judge:
// scorerDefaults (resolved over agentDefaults by the compiler). A model that
// the server sets (Options.CorrectnessModel) replaces the model.
func (s *Service) correctnessJudge(def *core.CompiledDefinition) JudgeExec {
	var ex JudgeExec
	if def == nil || def.ScorerDefaults == nil {
		return ex
	}
	sd := def.ScorerDefaults
	ex.Machine, ex.Secrets, ex.MCPServers = sd.Machine, sd.Secrets, sd.MCPServers
	if sd.Harness != nil {
		h := *sd.Harness
		if s.correctnessModelSet && sd.Model == "" {
			h.Model = s.opts.CorrectnessModel
		}
		ex.Harness = &h
	}
	return ex
}

func validateConfigurations(configs []core.BenchmarkConfiguration, agents []core.AgentConfig, machines map[string]core.MachineSpec) error {
	if len(configs) > MaxBenchmarkConfigurations {
		return fmt.Errorf("%w: at most %d configurations", core.ErrInvalid, MaxBenchmarkConfigurations)
	}
	known := map[string]bool{}
	for _, a := range agents {
		known[a.Name] = true
	}
	names := map[string]bool{}
	for _, c := range configs {
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("%w: configuration name is required", core.ErrInvalid)
		}
		if names[c.Name] {
			return fmt.Errorf("%w: duplicate configuration name %q", core.ErrInvalid, c.Name)
		}
		names[c.Name] = true
		if c.Machine != "" {
			if _, ok := machines[c.Machine]; !ok {
				return fmt.Errorf("%w: configuration %q: unknown machine %q", core.ErrInvalid, c.Name, c.Machine)
			}
		}
		pinned := map[string]bool{}
		for _, p := range c.Agents {
			if p.Agent == "" {
				return fmt.Errorf("%w: configuration %q: agent is required", core.ErrInvalid, c.Name)
			}
			if len(known) > 0 && !known[p.Agent] {
				return fmt.Errorf("%w: configuration %q: unknown agent %q", core.ErrInvalid, c.Name, p.Agent)
			}
			if pinned[p.Agent] {
				return fmt.Errorf("%w: configuration %q lists agent %q more than once", core.ErrInvalid, c.Name, p.Agent)
			}
			pinned[p.Agent] = true
			hasModel, hasHarness := p.Model != "", p.Harness != nil
			if hasModel == hasHarness {
				return fmt.Errorf("%w: configuration %q agent %q: set exactly one of model or harness", core.ErrInvalid, c.Name, p.Agent)
			}
			if hasHarness && (p.Harness.Type == "" || p.Harness.Model == "") {
				return fmt.Errorf("%w: configuration %q agent %q: harness requires type and model", core.ErrInvalid, c.Name, p.Agent)
			}
		}
	}
	return nil
}

func (s *Service) benchmarkScorers(ctx context.Context, factoryID string, ids []string) ([]BenchmarkScorer, error) {
	var out []BenchmarkScorer
	seen := map[string]bool{}
	for _, id := range ids {
		sc, err := s.findScorer(ctx, factoryID, id)
		if err != nil {
			return nil, err
		}
		if seen[sc.ID] {
			continue
		}
		seen[sc.ID] = true
		key := scorerName(*sc)
		if key == CorrectnessScorer {
			key = "scorer:" + key
		}
		out = append(out, BenchmarkScorer{
			Key: key, ScorerID: sc.ID, Name: sc.Name, Model: sc.Model, Rubric: sc.Rubric,
			Labels: sc.Labels, PassingScore: sc.PassingScore, JudgeExec: scorerExec(*sc),
		})
	}
	return out, nil
}

// trialDispatch builds the engine request for one trial. The trial runs in
// trial mode (core.MetaTrialMode): its child runs inherit the metadata.
func trialDispatch(bench *core.BenchmarkRun, suite core.BenchmarkSuite, cfg core.BenchmarkConfiguration, task core.BenchmarkTask, t *Trial, reps int, agents []string) core.DispatchRequest {
	md := map[string]string{
		core.MetaTrialMode:         "true",
		MetaBenchmarkRun:           bench.ID,
		MetaBenchmarkTrial:         t.ID,
		MetaBenchmarkConfiguration: clip(cfg.Name, maxMetadataValue),
		MetaBenchmarkTask:          clip(task.Slug, maxMetadataValue),
		MetaBenchmarkRepetition:    strconv.Itoa(t.Repetition),
	}
	var overrides *core.AgentOverrides
	var pins map[string]core.AgentOverrides
	for _, p := range cfg.Agents {
		ov := pinOverrides(p)
		if p.Agent == suite.Agent {
			overrides = ov
			continue
		}
		if ov == nil {
			continue
		}
		if pins == nil {
			pins = map[string]core.AgentOverrides{}
		}
		pins[p.Agent] = *ov
	}
	if cfg.Machine != "" {
		// The machine of the configuration applies to every agent of the trial.
		if overrides == nil {
			overrides = &core.AgentOverrides{}
		}
		overrides.Machine = cfg.Machine
		for _, a := range agents {
			if a == suite.Agent {
				continue
			}
			if pins == nil {
				pins = map[string]core.AgentOverrides{}
			}
			pin := pins[a]
			pin.Machine = cfg.Machine
			pins[a] = pin
		}
	}
	return core.DispatchRequest{
		FactoryID: bench.FactoryID,
		Prompt:    trialPrompt(suite, cfg, task, t, reps),
		Source: core.SourceRef{
			Provider:   "benchmark",
			Kind:       "task",
			ExternalID: bench.ID + "/" + t.ID,
			Title:      task.Title,
		},
		AgentName:        suite.Agent,
		Title:            fmt.Sprintf("Benchmark %s · %s · %s #%d", suite.Name, cfg.Name, task.Title, t.Repetition),
		Overrides:        overrides,
		Metadata:         md,
		TriggeredBy:      "benchmark:" + bench.ID,
		StartingRepoRefs: t.StartingRepoRefs,
		AgentPins:        pins,
	}
}

func pinOverrides(p core.BenchmarkAgentPin) *core.AgentOverrides {
	if p.Harness != nil {
		h := *p.Harness
		h.Type = normalizeHarness(h.Type)
		// The pinned model travels in Harness.Model (overrides allow at most one of model and harness).
		return &core.AgentOverrides{Harness: &h}
	}
	if p.Model != "" {
		return &core.AgentOverrides{Model: p.Model}
	}
	return nil
}

func normalizeHarness(t core.HarnessType) core.HarnessType {
	if strings.EqualFold(string(t), "claude-code") {
		return core.HarnessClaude
	}
	return core.HarnessType(strings.ToLower(string(t)))
}

func trialPrompt(suite core.BenchmarkSuite, cfg core.BenchmarkConfiguration, task core.BenchmarkTask, t *Trial, reps int) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(task.Prompt))
	b.WriteString("\n\n---\n")
	fmt.Fprintf(&b, "Benchmark trial: suite %q, configuration %q, task %q, repetition %d of %d.\n", suite.Name, cfg.Name, task.Slug, t.Repetition, reps)
	if len(t.StartingRepoRefs) > 0 {
		b.WriteString("Starting state: check out exactly these commits before you begin, and work from them:\n")
		for _, r := range t.StartingRepoRefs {
			fmt.Fprintf(&b, "- %s\n", FormatRepoAtCommit(r))
		}
	}
	b.WriteString("This trial runs in trial mode: the write tools of the integrations (tracker comments, chat messages) and your updates are recorded in this run. They do not reach the real services.\n")
	if task.TrackerSeed != nil {
		if seed, err := yaml.Marshal(task.TrackerSeed); err == nil {
			b.WriteString("Tracker start state: the tracker of this trial starts with this state. Treat it as the current state of the tracker:\n\n```yaml\n")
			b.Write(seed)
			b.WriteString("```\n")
		}
	}
	return b.String()
}

// FormatRepoAtCommit renders a ref as "github.com:OWNER/REPO@SHA".
func FormatRepoAtCommit(r core.RepoAtCommit) string {
	host := strings.ToLower(r.CodeForge)
	switch strings.ToUpper(r.CodeForge) {
	case "GITHUB":
		host = "github.com"
	case "GITLAB":
		host = "gitlab.com"
	}
	return host + ":" + r.Owner + "/" + r.Repo + "@" + r.Ref
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ---------- Repo substitutions ----------

// RepoID identifies a repository in benchmarkRepoSubstitutions.
type RepoID = core.RepoRef

// RepoSubstitution replaces a factory repository with another (e.g. a
// sandbox fork) in benchmark starting states.
type RepoSubstitution = core.RepoSubstitution

// ParseRepoSubstitutions reads benchmarkRepoSubstitutions from the raw
// definition's factory.yaml. The compiled definition carries them as
// CompiledDefinition.BenchmarkRepoSubstitutions; this is a fallback for
// callers that only have the files.
func ParseRepoSubstitutions(files map[string]string) ([]RepoSubstitution, error) {
	if len(files) == 0 {
		return nil, nil
	}
	root := definitionRoot(files)
	content, ok := files[root+"factory.yaml"]
	if !ok {
		return nil, nil
	}
	type repoID struct {
		CodeForge string `yaml:"codeForge"`
		Owner     string `yaml:"owner"`
		Name      string `yaml:"name"`
	}
	var doc struct {
		Subs []struct {
			Source repoID `yaml:"source"`
			Target repoID `yaml:"target"`
		} `yaml:"benchmarkRepoSubstitutions"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return nil, fmt.Errorf("%w: factory.yaml benchmarkRepoSubstitutions: %w", core.ErrInvalid, err)
	}
	var out []RepoSubstitution
	for _, sub := range doc.Subs {
		out = append(out, RepoSubstitution{
			Source: core.RepoRef{CodeForge: sub.Source.CodeForge, Owner: sub.Source.Owner, Name: sub.Source.Name},
			Target: core.RepoRef{CodeForge: sub.Target.CodeForge, Owner: sub.Target.Owner, Name: sub.Target.Name},
		})
	}
	return out, nil
}

// ApplyRepoSubstitutions returns refs with every repository matching a
// substitution's source (case-insensitive) replaced by its target. Commits are kept.
func ApplyRepoSubstitutions(refs []core.RepoAtCommit, subs []RepoSubstitution) []core.RepoAtCommit {
	if len(refs) == 0 {
		return nil
	}
	out := make([]core.RepoAtCommit, len(refs))
	copy(out, refs)
	for i := range out {
		for _, sub := range subs {
			if strings.EqualFold(out[i].CodeForge, sub.Source.CodeForge) &&
				strings.EqualFold(out[i].Owner, sub.Source.Owner) &&
				strings.EqualFold(out[i].Repo, sub.Source.Name) {
				if sub.Target.CodeForge != "" {
					out[i].CodeForge = strings.ToUpper(sub.Target.CodeForge)
				}
				out[i].Owner = sub.Target.Owner
				out[i].Repo = sub.Target.Name
				break
			}
		}
	}
	return out
}

// ---------- Trial lifecycle ----------

func (s *Service) loadBenchmark(ctx context.Context, id string) (*core.BenchmarkRun, *BenchmarkResults, error) {
	b, err := s.opts.Store.GetBenchmarkRun(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("get benchmark run %s: %w", id, err)
	}
	res, err := DecodeBenchmarkResults(b)
	if err != nil {
		return nil, nil, err
	}
	return b, res, nil
}

// saveBenchmark recomputes aggregates and persists the run.
func (s *Service) saveBenchmark(ctx context.Context, b *core.BenchmarkRun, res *BenchmarkResults, create bool) error {
	res.Summary, res.Recommendations, res.TotalCostUSD = Aggregate(res.Configurations, res.Trials, s.resultScorerKeys(res))
	res.JudgeCostUSD = 0
	for _, r := range res.Summary {
		res.JudgeCostUSD += r.JudgeCostUSD
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return fmt.Errorf("encode benchmark results: %w", err)
	}
	b.Results = raw
	if create {
		err = s.opts.Store.CreateBenchmarkRun(ctx, b)
	} else {
		err = s.opts.Store.UpdateBenchmarkRun(ctx, b)
	}
	if err != nil {
		return fmt.Errorf("save benchmark run: %w", err)
	}
	return nil
}

func (s *Service) resultScorerKeys(res *BenchmarkResults) []string {
	keys := []string{CorrectnessScorer}
	for _, sc := range res.Scorers {
		keys = append(keys, sc.Key)
	}
	return keys
}

func benchmarkTerminal(state string) bool {
	return state == BenchmarkComplete || state == BenchmarkFailed || state == BenchmarkCancelled
}

// handleTrialUpdate records a finished trial run and scores it.
func (s *Service) handleTrialUpdate(ctx context.Context, benchID string, run *core.Run) error {
	trialID, err := s.markTrialFinished(ctx, benchID, run)
	if err != nil || trialID == "" {
		return err
	}
	return s.scoreTrial(ctx, benchID, trialID)
}

func (s *Service) markTrialFinished(ctx context.Context, benchID string, run *core.Run) (string, error) {
	unlock := s.lockKey("bench:" + benchID)
	defer unlock()
	b, res, err := s.loadBenchmark(ctx, benchID)
	if errors.Is(err, core.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if benchmarkTerminal(b.State) {
		return "", nil
	}
	var t *Trial
	for i := range res.Trials {
		if res.Trials[i].RunID == run.ID {
			t = &res.Trials[i]
			break
		}
	}
	if t == nil {
		return "", nil // a descendant of a trial, or an unknown run
	}
	if t.runFinished() {
		if t.Scored {
			return "", nil
		}
		return t.ID, nil // finished earlier but not yet scored (e.g. restart)
	}
	if err := s.finalizeTrial(ctx, b.FactoryID, t, run); err != nil {
		return "", err
	}
	b.State = benchmarkState(res.Trials)
	if err := s.saveBenchmark(ctx, b, res, false); err != nil {
		return "", err
	}
	return t.ID, nil
}

// finalizeTrial fills a trial's outcome, usage (including descendant runs) and duration.
func (s *Service) finalizeTrial(ctx context.Context, factoryID string, t *Trial, run *core.Run) error {
	t.RunState = run.State
	switch run.State {
	case core.RunCancelled:
		t.Status = TrialCancelled
	case core.RunError:
		t.Status = TrialError // no scoring: the run did not produce evidence to judge
	case core.RunIdle, core.RunBlocked:
		t.Status = TrialFinished
		blocked, err := s.awaitingHuman(ctx, factoryID, t, run)
		if err != nil {
			return err
		}
		if blocked {
			t.Status = TrialBlocked
		}
	default:
		t.Status = TrialFinished
	}
	if run.Error != "" {
		t.Error = run.Error
	}
	usage := run.Usage
	kids, err := descendants(ctx, s.opts.Store, run.ID)
	if err != nil {
		return fmt.Errorf("list trial descendants: %w", err)
	}
	for _, k := range kids {
		usage.InputTokens += k.Usage.InputTokens
		usage.OutputTokens += k.Usage.OutputTokens
		usage.CostUSD += k.Usage.CostUSD
	}
	t.CostUSD = usage.CostUSD
	t.InputTokens = usage.InputTokens
	t.OutputTokens = usage.OutputTokens
	now := s.now()
	end := now
	if run.FinishedAt != nil {
		end = *run.FinishedAt
	}
	t.FinishedAt = &end
	switch {
	case run.FinishedAt != nil && !run.CreatedAt.IsZero():
		t.DurationMS = run.FinishedAt.Sub(run.CreatedAt).Milliseconds()
	case run.Usage.DurationMS > 0:
		t.DurationMS = run.Usage.DurationMS
	case !run.CreatedAt.IsZero():
		t.DurationMS = now.Sub(run.CreatedAt).Milliseconds()
	}
	return nil
}

// awaitingHuman reports whether an idle trial has an unresolved inbox item.
func (s *Service) awaitingHuman(ctx context.Context, factoryID string, t *Trial, run *core.Run) (bool, error) {
	items, err := s.opts.Store.ListInboxItems(ctx, factoryID, false)
	if err != nil {
		return false, fmt.Errorf("list inbox items: %w", err)
	}
	for _, it := range items {
		if it.Resolved {
			continue
		}
		if it.RunID == run.ID || (t.WorkItemID != "" && it.WorkItemID == t.WorkItemID) {
			return true, nil
		}
	}
	return false, nil
}

func benchmarkState(trials []Trial) string {
	allRunsDone, allDone := true, true
	for i := range trials {
		if !trials[i].runFinished() {
			allRunsDone = false
		}
		if !trials[i].done() {
			allDone = false
		}
	}
	switch {
	case allDone:
		return BenchmarkComplete
	case allRunsDone:
		return BenchmarkScoring
	}
	return BenchmarkRunning
}

// scoreTrial runs the Correctness Scorer (and any extra scorers) on a
// finished trial and persists the result.
func (s *Service) scoreTrial(ctx context.Context, benchID, trialID string) error {
	unlockTrial := s.lockKey("trial:" + benchID + ":" + trialID)
	defer unlockTrial()

	b, res, err := s.loadBenchmark(ctx, benchID)
	if err != nil {
		return err
	}
	if benchmarkTerminal(b.State) {
		return nil
	}
	var t *Trial
	for i := range res.Trials {
		if res.Trials[i].ID == trialID {
			t = &res.Trials[i]
		}
	}
	if t == nil || t.Scored || t.Status == TrialError || !t.runFinished() || t.RunID == "" {
		return nil
	}
	if s.opts.StartJudge != nil {
		return s.startTrialJudges(ctx, benchID, trialID)
	}
	run, err := s.opts.Store.GetRun(ctx, t.RunID)
	if err != nil {
		return fmt.Errorf("get trial run: %w", err)
	}
	transcript, err := s.BuildTranscript(ctx, run, true)
	if err != nil {
		return err
	}
	var task core.BenchmarkTask
	for _, tk := range res.Suite.Tasks {
		if tk.Slug == t.Task {
			task = tk
		}
	}
	scorers := append([]BenchmarkScorer{CorrectnessScorerFor(task, res.CorrectnessModel)}, res.Scorers...)
	scores := make([]TrialScore, 0, len(scorers))
	for _, sc := range scorers {
		ts := TrialScore{Scorer: sc.Key}
		jr, err := s.judge(ctx, sc.Model, sc.Rubric, transcript, sc.Labels, sc.PassingScore)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			ts.Error = err.Error()
		} else {
			ts.Label, ts.Score, ts.Passed, ts.Rationale = jr.Label, jr.Score, jr.Passed, jr.Rationale
		}
		scores = append(scores, ts)
	}

	unlock := s.lockKey("bench:" + benchID)
	defer unlock()
	b, res, err = s.loadBenchmark(ctx, benchID)
	if err != nil {
		return err
	}
	if benchmarkTerminal(b.State) {
		return nil // cancelled while scoring: keep only results scored before the stop
	}
	for i := range res.Trials {
		if res.Trials[i].ID != trialID {
			continue
		}
		res.Trials[i].Scores = scores
		res.Trials[i].Scored = true
		s.releaseTrial(ctx, benchID, &res.Trials[i])
	}
	b.State = benchmarkState(res.Trials)
	if b.State == BenchmarkComplete {
		fin := s.now()
		b.FinishedAt = &fin
	}
	return s.saveBenchmark(ctx, b, res, false)
}

// releaseTrial ends the session of an idle, blocked or (runner path)
// succeeded trial once its evidence is scored.
func (s *Service) releaseTrial(ctx context.Context, benchID string, t *Trial) {
	if (t.RunState.WaitsForInput() || t.RunState == core.RunSucceeded) && s.opts.Engine != nil {
		if err := s.opts.Engine.Cancel(ctx, t.RunID, "benchmark:"+benchID); err != nil {
			s.log.Warn("cancel idle trial run", "run_id", t.RunID, "err", err)
		}
	}
}

// trialScorers returns the scorers of a trial: the Correctness Scorer of its
// task, then the extra scorers of the benchmark.
func trialScorers(res *BenchmarkResults, t *Trial) []BenchmarkScorer {
	var task core.BenchmarkTask
	for _, tk := range res.Suite.Tasks {
		if tk.Slug == t.Task {
			task = tk
		}
	}
	cs := CorrectnessScorerFor(task, res.CorrectnessModel)
	cs.JudgeExec = res.CorrectnessJudge
	return append([]BenchmarkScorer{cs}, res.Scorers...)
}

// startTrialJudges starts one judge run for each scorer of a finished trial
// that has no result and no judge run yet.
func (s *Service) startTrialJudges(ctx context.Context, benchID, trialID string) error {
	unlock := s.lockKey("bench:" + benchID)
	defer unlock()
	b, res, err := s.loadBenchmark(ctx, benchID)
	if err != nil {
		return err
	}
	if benchmarkTerminal(b.State) {
		return nil
	}
	var t *Trial
	for i := range res.Trials {
		if res.Trials[i].ID == trialID {
			t = &res.Trials[i]
		}
	}
	if t == nil || t.Scored || t.RunID == "" {
		return nil
	}
	run, err := s.opts.Store.GetRun(ctx, t.RunID)
	if err != nil {
		return fmt.Errorf("get trial run: %w", err)
	}
	var refs []core.RepoAtCommit
	if starts, err := core.LoadStartCommits(ctx, s.opts.Store, run.ID); err == nil && len(starts) > 0 {
		for _, st := range starts {
			refs = append(refs, st.RepoAtCommit())
		}
	} else {
		refs = t.StartingRepoRefs
	}
	changed := false
	for _, sc := range trialScorers(res, t) {
		if t.score(sc.Key) != nil {
			continue
		}
		ts := TrialScore{Scorer: sc.Key}
		req, err := s.judgeRequest(ctx, b.FactoryID, sc.Key, sc.Rubric, sc.Labels, sc.JudgeExec, run, true, refs)
		var judge *core.Run
		if err == nil {
			req.TriggeredBy = "benchmark:" + benchID
			req.Metadata = map[string]string{
				MetaJudge:          JudgeTrial,
				MetaJudgeBenchmark: benchID,
				MetaJudgeTrial:     trialID,
				MetaJudgeKey:       clip(sc.Key, maxMetadataValue),
				MetaJudgeScoredRun: run.ID,
			}
			judge, err = s.opts.StartJudge(ctx, req)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			ts.Error = err.Error()
		} else {
			ts.JudgeRunID, ts.Pending = judge.ID, true
		}
		setTrialScore(t, ts)
		changed = true
	}
	if !changed {
		return nil
	}
	s.completeTrialScores(ctx, benchID, res, t)
	b.State = benchmarkState(res.Trials)
	if b.State == BenchmarkComplete {
		fin := s.now()
		b.FinishedAt = &fin
	}
	return s.saveBenchmark(ctx, b, res, false)
}

func setTrialScore(t *Trial, ts TrialScore) {
	for i := range t.Scores {
		if t.Scores[i].Scorer == ts.Scorer {
			t.Scores[i] = ts
			return
		}
	}
	t.Scores = append(t.Scores, ts)
}

// completeTrialScores marks a trial scored when each of its scorers has a
// result or an error.
func (s *Service) completeTrialScores(ctx context.Context, benchID string, res *BenchmarkResults, t *Trial) {
	if t.Scored {
		return
	}
	for _, sc := range trialScorers(res, t) {
		ts := t.score(sc.Key)
		if ts == nil || ts.Pending {
			return
		}
	}
	t.Scored = true
	s.releaseTrial(ctx, benchID, t)
}

// finishTrialJudge stores the answer of a finished judge run of a trial.
func (s *Service) finishTrialJudge(ctx context.Context, judge *core.Run) error {
	benchID, trialID := judge.Metadata[MetaJudgeBenchmark], judge.Metadata[MetaJudgeTrial]
	if benchID == "" || trialID == "" {
		return nil
	}
	usage, err := s.treeUsage(ctx, judge)
	if err != nil {
		return err
	}
	label, rationale, jerr := s.judgeAnswer(ctx, judge)
	s.stopJudge(ctx, judge)

	unlock := s.lockKey("bench:" + benchID)
	defer unlock()
	b, res, err := s.loadBenchmark(ctx, benchID)
	if errors.Is(err, core.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if benchmarkTerminal(b.State) {
		return nil
	}
	var t *Trial
	for i := range res.Trials {
		if res.Trials[i].ID == trialID {
			t = &res.Trials[i]
		}
	}
	if t == nil {
		return nil
	}
	var ts *TrialScore
	for i := range t.Scores {
		if t.Scores[i].JudgeRunID == judge.ID {
			ts = &t.Scores[i]
		}
	}
	if ts == nil || !ts.Pending {
		return nil
	}
	var sc *BenchmarkScorer
	for _, x := range trialScorers(res, t) {
		if x.Key == ts.Scorer {
			x := x
			sc = &x
		}
	}
	ts.Pending = false
	ts.JudgeCostUSD = usage.CostUSD
	t.JudgeCostUSD += usage.CostUSD
	switch {
	case sc == nil:
		ts.Error = "unknown scorer"
	case jerr != nil:
		ts.Error = jerr.Error()
	default:
		if l, ok := resolveLabel(sc.Labels, label); ok {
			ts.Label, ts.Score, ts.Passed, ts.Rationale = l.Value, l.Score, l.Score >= sc.PassingScore, rationale
		} else {
			ts.Error = fmt.Sprintf("judge returned undeclared label %q", label)
		}
	}
	s.completeTrialScores(ctx, benchID, res, t)
	b.State = benchmarkState(res.Trials)
	if b.State == BenchmarkComplete {
		fin := s.now()
		b.FinishedAt = &fin
	}
	return s.saveBenchmark(ctx, b, res, false)
}

// CorrectnessScorerFor returns the built-in Correctness Scorer for a task:
// it judges the trial against the task's successCriteria.
func CorrectnessScorerFor(task core.BenchmarkTask, model string) BenchmarkScorer {
	var r strings.Builder
	r.WriteString("You are the Correctness Scorer for a benchmark trial. Decide whether the agent's work satisfies the task's correctness criteria.\n\n")
	fmt.Fprintf(&r, "Task: %s\n\nTask prompt:\n%s\n\n", task.Title, strings.TrimSpace(task.Prompt))
	fmt.Fprintf(&r, "Correctness criteria (what a successful trial must do):\n%s\n\n", strings.TrimSpace(task.SuccessCriteria))
	r.WriteString("Judge only from the evidence in the transcript, including child runs. A trial that stopped early or is blocked waiting for a human may still satisfy the criteria if its evidence shows it. Return exactly one declared label.")
	return BenchmarkScorer{
		Key:    CorrectnessScorer,
		Name:   "Correctness",
		Model:  model,
		Rubric: r.String(),
		Labels: []core.ScorerLabel{
			{Value: "correct", Description: "The trial satisfies every correctness criterion.", Score: 1},
			{Value: "incorrect", Description: "The trial does not satisfy one or more correctness criteria.", Score: 0},
		},
		PassingScore: 1,
	}
}

// ReconcileBenchmark re-checks every unfinished trial against the store and
// scores finished but unscored trials. The sweep calls it for active runs.
func (s *Service) ReconcileBenchmark(ctx context.Context, id string) error {
	b, res, err := s.loadBenchmark(ctx, id)
	if err != nil {
		return err
	}
	if benchmarkTerminal(b.State) {
		return nil
	}
	var errs []error
	for _, t := range res.Trials {
		switch {
		case t.Status == TrialRunning && t.RunID != "":
			run, err := s.opts.Store.GetRun(ctx, t.RunID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if finishedForScoring(run.State) {
				if err := s.handleTrialUpdate(ctx, id, run); err != nil {
					errs = append(errs, err)
				}
			}
		case t.runFinished() && !t.done():
			if err := s.scoreTrial(ctx, id, t.ID); err != nil {
				errs = append(errs, err)
			}
			for _, ts := range t.Scores {
				if !ts.Pending || ts.JudgeRunID == "" {
					continue
				}
				judge, err := s.opts.Store.GetRun(ctx, ts.JudgeRunID)
				if err != nil {
					errs = append(errs, err)
					continue
				}
				if finishedForScoring(judge.State) {
					if err := s.finishTrialJudge(ctx, judge); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}
	}
	// Persist a state change if every trial is already done (e.g. all errored late).
	unlock := s.lockKey("bench:" + id)
	defer unlock()
	b, res, err = s.loadBenchmark(ctx, id)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if !benchmarkTerminal(b.State) {
		if st := benchmarkState(res.Trials); st != b.State {
			b.State = st
			if st == BenchmarkComplete {
				fin := s.now()
				b.FinishedAt = &fin
			}
			if err := s.saveBenchmark(ctx, b, res, false); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// CancelBenchmark cancels a running or scoring benchmark: unfinished trial runs
// are cancelled and only results scored before the stop are kept.
func (s *Service) CancelBenchmark(ctx context.Context, id, actor string) (*core.BenchmarkRun, error) {
	unlock := s.lockKey("bench:" + id)
	defer unlock()
	b, res, err := s.loadBenchmark(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.State == BenchmarkCancelled {
		return b, nil
	}
	if benchmarkTerminal(b.State) {
		return nil, fmt.Errorf("%w: benchmark run %s is %s", core.ErrConflict, id, b.State)
	}
	var errs []error
	for i := range res.Trials {
		t := &res.Trials[i]
		switch t.Status {
		case TrialPending:
			t.Status = TrialCancelled
		case TrialRunning:
			if s.opts.Engine != nil && t.RunID != "" {
				if err := s.opts.Engine.Cancel(ctx, t.RunID, actor); err != nil && !errors.Is(err, core.ErrNotFound) {
					errs = append(errs, fmt.Errorf("cancel trial run %s: %w", t.RunID, err))
				}
			}
			t.Status = TrialCancelled
			t.RunState = core.RunCancelled
		}
		for j := range t.Scores {
			ts := &t.Scores[j]
			if !ts.Pending {
				continue
			}
			if s.opts.Engine != nil && ts.JudgeRunID != "" {
				if err := s.opts.Engine.Cancel(ctx, ts.JudgeRunID, actor); err != nil && !errors.Is(err, core.ErrNotFound) {
					errs = append(errs, fmt.Errorf("cancel judge run %s: %w", ts.JudgeRunID, err))
				}
			}
			ts.Pending = false
			ts.Error = "cancelled"
		}
	}
	b.State = BenchmarkCancelled
	fin := s.now()
	b.FinishedAt = &fin
	res.CancelledBy = actor
	if err := s.saveBenchmark(ctx, b, res, false); err != nil {
		return nil, err
	}
	if len(errs) > 0 {
		s.log.Warn("benchmark cancel", "benchmark_run_id", id, "err", errors.Join(errs...))
	}
	return b, nil
}

// ---------- Aggregation ----------

// Aggregate computes per-configuration results, recommendations and the total
// cost of the trials and their judge runs. It is a pure function of the trials. Pass rate counts Correctness
// Scorer results; judge errors are excluded from scored counts and averages.
func Aggregate(configs []core.BenchmarkConfiguration, trials []Trial, scorerKeys []string) ([]ConfigurationResult, Recommendations, float64) {
	order := make([]string, 0, len(configs))
	byName := map[string]*ConfigurationResult{}
	add := func(name string) *ConfigurationResult {
		if r, ok := byName[name]; ok {
			return r
		}
		r := &ConfigurationResult{Name: name, ScorerAverages: map[string]float64{}}
		byName[name] = r
		order = append(order, name)
		return r
	}
	for _, c := range configs {
		add(c.Name)
	}
	type acc struct {
		sum float64
		n   int
	}
	sums := map[string]map[string]*acc{}
	durations := map[string][]int64{}
	total := 0.0
	for i := range trials {
		t := &trials[i]
		r := add(t.Configuration)
		r.Trials++
		r.JudgeCostUSD += t.JudgeCostUSD
		total += t.JudgeCostUSD
		switch t.Status {
		case TrialError:
			r.Errored++
		case TrialBlocked:
			r.Blocked++
		}
		if t.RunID != "" && t.FinishedAt != nil {
			r.Finished++
			r.TotalCostUSD += t.CostUSD
			r.InputTokens += t.InputTokens
			r.OutputTokens += t.OutputTokens
			durations[t.Configuration] = append(durations[t.Configuration], t.DurationMS)
			total += t.CostUSD
		}
		if !t.Scored {
			continue
		}
		if cs := t.score(CorrectnessScorer); cs != nil && cs.Error == "" {
			r.Scored++
			if cs.Passed {
				r.Passed++
			}
		}
		for _, key := range scorerKeys {
			ts := t.score(key)
			if ts == nil || ts.Error != "" {
				continue
			}
			m := sums[t.Configuration]
			if m == nil {
				m = map[string]*acc{}
				sums[t.Configuration] = m
			}
			a := m[key]
			if a == nil {
				a = &acc{}
				m[key] = a
			}
			a.sum += ts.Score
			a.n++
		}
	}
	out := make([]ConfigurationResult, 0, len(order))
	for _, name := range order {
		r := byName[name]
		if r.Scored > 0 {
			r.PassRate = float64(r.Passed) / float64(r.Scored)
		}
		var overall float64
		var n int
		for _, key := range scorerKeys {
			if a := sums[name][key]; a != nil && a.n > 0 {
				avg := a.sum / float64(a.n)
				r.ScorerAverages[key] = avg
				overall += avg
				n++
			}
		}
		if n > 0 {
			r.Overall = overall / float64(n)
		}
		if r.Finished > 0 {
			r.AvgCostUSD = r.TotalCostUSD / float64(r.Finished)
		}
		if d := durations[name]; len(d) > 0 {
			var sum int64
			for _, x := range d {
				sum += x
			}
			r.AvgDurationMS = sum / int64(len(d))
			r.MedianDurationMS = medianInt64(d)
		}
		out = append(out, *r)
	}
	return out, recommend(out), total
}

func recommend(results []ConfigurationResult) Recommendations {
	var cands []ConfigurationResult
	for _, r := range results {
		if len(r.ScorerAverages) > 0 {
			cands = append(cands, r)
		}
	}
	var rec Recommendations
	if len(cands) < 2 {
		return rec
	}
	const eps = 1e-9
	best := cands[0]
	for _, c := range cands[1:] {
		if c.Overall > best.Overall+eps || (math.Abs(c.Overall-best.Overall) <= eps && c.AvgCostUSD < best.AvgCostUSD-eps) {
			best = c
		}
	}
	rec.Overall = best.Name

	cheapest := cands[0]
	allSameCost := true
	for _, c := range cands[1:] {
		if math.Abs(c.AvgCostUSD-cands[0].AvgCostUSD) > eps {
			allSameCost = false
		}
		if c.AvgCostUSD < cheapest.AvgCostUSD-eps {
			cheapest = c
		}
	}
	if !allSameCost {
		rec.LowestCost = cheapest.Name
		bestEff, found := 0.0, false
		for _, c := range cands {
			if c.AvgCostUSD <= 0 || c.Overall <= 0 {
				continue
			}
			eff := c.Overall / c.AvgCostUSD
			if !found || eff > bestEff+eps {
				bestEff, found = eff, true
				rec.MostEfficient = c.Name
			}
		}
	}
	return rec
}

func medianInt64(xs []int64) int64 {
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
