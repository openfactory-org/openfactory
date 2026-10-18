// Package scoring implements Scorers (LLM-judged classification of sampled
// runs), Self-improvement (turning repeated scored failures into follow-up
// runs that propose fixes as pull requests) and Benchmarks (running a suite of
// tasks under several agent configurations and scoring every trial).
//
// The package depends only on internal/core. The LLM judge, the engine and the
// definition compiler are injected through Options.
package scoring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/links"
)

// ClassifyFunc asks an LLM judge to classify a transcript with exactly one of
// the given labels. It returns the label value and the judge's reasoning.
type ClassifyFunc func(ctx context.Context, model, rubric, transcript string, labels []core.ScorerLabel) (label, rationale string, err error)

// Defaults.
const (
	// DefaultFailedRunThreshold is the number of distinct unreviewed failures per
	// agent that triggers a scheduled Self-improvement run when the factory
	// definition does not set selfImprovement.failedRunThreshold.
	DefaultFailedRunThreshold = 25
	// MinFailedRunThreshold and MaxFailedRunThreshold bound failedRunThreshold.
	MinFailedRunThreshold = 1
	MaxFailedRunThreshold = 50
	// DefaultMaxFailureAge triggers a scheduled run when the oldest unreviewed
	// failure of an agent reaches this age, even below the threshold.
	DefaultMaxFailureAge = 7 * 24 * time.Hour
	// DefaultCheckInterval is how often the background sweep runs the scheduled
	// Self-improvement check and reconciles active benchmarks.
	DefaultCheckInterval = 15 * time.Minute
	// DefaultMaxTranscriptBytes caps the transcript handed to the judge.
	DefaultMaxTranscriptBytes = 200_000
	// DefaultCorrectnessModel judges benchmark trials when Options.CorrectnessModel is empty.
	DefaultCorrectnessModel = "claude-sonnet-4-5"
	// DefaultWorkers is the number of concurrent automatic-scoring workers.
	DefaultWorkers = 4
)

// Options configures a Service.
type Options struct {
	Store core.Store
	Bus   core.EventBus
	// StartJudge starts the judge of a scorer as a run in a sandbox
	// (engine.StartJudgeRun). When it is set, every scorer and the
	// Correctness Scorer of benchmarks judge with a run, and Classify is not
	// used.
	StartJudge func(ctx context.Context, req core.JudgeRunRequest) (*core.Run, error)
	// Classify is a direct model call that judges a transcript. It is used
	// only when StartJudge is nil (tests, or a server without an engine).
	Classify ClassifyFunc
	// CheckDefinition runs the compiler and the preflight on a definition
	// tree (engine.CheckDefinition). Proposed definition changes need it.
	CheckDefinition func(ctx context.Context, factoryID string, files map[string]string) (*core.DefinitionCheck, error)
	// RequestReview asks a team member for a review of a pull request on the
	// forge (forge.Resolver.RequestReview). It is optional: without it, the
	// reviewer of a Self-improvement pull request only gets the inbox item.
	RequestReview func(ctx context.Context, teamID string, pr core.PullRequestRef, userID string) error
	// Engine starts Self-improvement and benchmark trial runs.
	Engine core.Engine
	// Compile compiles a definition tree; used to read benchmark suites,
	// selfImprovement settings and benchmarkRepoSubstitutions from the
	// factory's active definition version.
	Compile core.DefinitionCompiler
	// Clock returns the current time (default time.Now).
	Clock func() time.Time
	// Rand is the randomness source used to pick one eligible reviewer for a
	// Self-improvement review. Sampling of runs is deliberately NOT random: it is
	// a deterministic hash of (scorer, run) so re-delivered run updates never
	// double-sample. Default: a time-seeded PCG source.
	Rand   *rand.Rand
	Logger *slog.Logger
	// PublicURL is the external base URL (OF_PUBLIC_URL) used for links to runs
	// in Self-improvement prompts and review items.
	PublicURL string

	// Workers is the number of automatic-scoring workers (default 4).
	Workers int
	// CheckInterval is the background sweep interval (default 15m; <0 disables the sweep).
	CheckInterval time.Duration
	// MaxTranscriptBytes caps judge transcripts (default 200 000).
	MaxTranscriptBytes int

	// CorrectnessModel is the judge model of the built-in benchmark Correctness Scorer.
	CorrectnessModel string
	// AnalysisModel optionally overrides the model of Self-improvement runs.
	AnalysisModel string
	// ImprovementAgent is the agent that runs Self-improvement (default: the foreman).
	ImprovementAgent string
	// DefaultFailedRunThreshold applies when the definition omits failedRunThreshold (default 25).
	DefaultFailedRunThreshold int
	// MaxFailureAge triggers a scheduled run when the oldest unreviewed failure is at least this old (default 7 days; <0 disables).
	MaxFailureAge time.Duration
}

// Service is the scoring, Self-improvement and benchmark service.
type Service struct {
	opts Options
	log  *slog.Logger
	// correctnessModelSet is true when Options.CorrectnessModel was set. A
	// judge run then uses that model; else the model of scorerDefaults.
	correctnessModelSet bool

	randMu sync.Mutex

	keyMu    sync.Mutex
	keyLocks map[string]*keyLock

	queue chan *core.Run
	wg    sync.WaitGroup

	startOnce sync.Once
	stop      context.CancelFunc
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// NewService validates options, applies defaults and returns a Service.
// Call Start to subscribe to run updates and begin the background sweep.
func NewService(opts Options) (*Service, error) {
	if opts.Store == nil {
		return nil, errors.New("scoring: Store is required")
	}
	if opts.Classify == nil && opts.StartJudge == nil {
		return nil, errors.New("scoring: StartJudge or Classify is required")
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Rand == nil {
		now := uint64(time.Now().UnixNano())
		opts.Rand = rand.New(rand.NewPCG(now, now>>17|1)) //nolint:gosec // picks a reviewer, not a secret
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.CheckInterval == 0 {
		opts.CheckInterval = DefaultCheckInterval
	}
	if opts.MaxTranscriptBytes <= 0 {
		opts.MaxTranscriptBytes = DefaultMaxTranscriptBytes
	}
	correctnessModelSet := opts.CorrectnessModel != ""
	if opts.CorrectnessModel == "" {
		opts.CorrectnessModel = DefaultCorrectnessModel
	}
	if opts.DefaultFailedRunThreshold <= 0 {
		opts.DefaultFailedRunThreshold = DefaultFailedRunThreshold
	}
	opts.DefaultFailedRunThreshold = clampThreshold(opts.DefaultFailedRunThreshold)
	if opts.MaxFailureAge == 0 {
		opts.MaxFailureAge = DefaultMaxFailureAge
	}
	return &Service{
		opts:                opts,
		correctnessModelSet: correctnessModelSet,
		log:                 opts.Logger.With("component", "scoring"),
		keyLocks:            map[string]*keyLock{},
		queue:               make(chan *core.Run, 1024),
	}, nil
}

// Start subscribes to core.TopicRunUpdated, starts the scoring workers and the
// periodic sweep. It returns immediately; everything stops when ctx is
// cancelled or Close is called.
func (s *Service) Start(ctx context.Context) error {
	if s.opts.Bus == nil {
		return errors.New("scoring: Bus is required to Start")
	}
	started := false
	s.startOnce.Do(func() {
		started = true
		ctx, cancel := context.WithCancel(ctx)
		s.stop = cancel
		// Score each finished run on the replica that finished it: with
		// several replicas, run updates are also relayed to the others and
		// every replica would otherwise score the same run.
		var ch <-chan any
		var unsubscribe func()
		if ls, ok := s.opts.Bus.(interface {
			SubscribeLocal(topic string) (<-chan any, func())
		}); ok {
			ch, unsubscribe = ls.SubscribeLocal(core.TopicRunUpdated)
		} else {
			ch, unsubscribe = s.opts.Bus.Subscribe(core.TopicRunUpdated)
		}
		for i := 0; i < s.opts.Workers; i++ {
			s.wg.Add(1)
			go s.worker(ctx)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer unsubscribe()
			for {
				select {
				case <-ctx.Done():
					return
				case msg, ok := <-ch:
					if !ok {
						return
					}
					run, ok := msg.(*core.Run)
					if !ok || run == nil || !finishedForScoring(run.State) {
						continue
					}
					cp := *run
					select {
					case s.queue <- &cp:
					case <-ctx.Done():
						return
					default:
						s.log.Warn("scoring queue full; dropping run update", "run_id", run.ID)
					}
				}
			}
		}()
		if s.opts.CheckInterval > 0 {
			s.wg.Add(1)
			go s.sweepLoop(ctx)
		}
	})
	if !started {
		return errors.New("scoring: already started")
	}
	return nil
}

// Close stops background work and waits for in-flight handlers.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
	return nil
}

func (s *Service) worker(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case run := <-s.queue:
			if err := s.HandleRunUpdate(ctx, run); err != nil && ctx.Err() == nil {
				s.log.Error("handle run update", "run_id", run.ID, "err", err)
			}
		}
	}
}

func (s *Service) sweepLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(s.opts.CheckInterval)
	defer t.Stop()
	s.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Sweep(ctx)
		}
	}
}

// Sweep runs the scheduled Self-improvement check for every factory and
// reconciles active benchmark runs (covering run updates dropped by the bus or
// lost across restarts).
func (s *Service) Sweep(ctx context.Context) {
	teams, err := s.opts.Store.ListTeams(ctx)
	if err != nil {
		s.log.Error("sweep: list teams", "err", err)
		return
	}
	for _, t := range teams {
		factories, err := s.opts.Store.ListFactories(ctx, t.ID, "")
		if err != nil {
			s.log.Error("sweep: list factories", "team_id", t.ID, "err", err)
			continue
		}
		for _, f := range factories {
			if ctx.Err() != nil {
				return
			}
			if s.opts.Engine != nil {
				if _, err := s.CheckSelfImprovement(ctx, f.ID); err != nil {
					s.log.Error("sweep: self-improvement check", "factory_id", f.ID, "err", err)
				}
			}
			runs, err := s.opts.Store.ListBenchmarkRuns(ctx, f.ID)
			if err != nil {
				s.log.Error("sweep: list benchmark runs", "factory_id", f.ID, "err", err)
				continue
			}
			for _, b := range runs {
				if b.State == BenchmarkRunning || b.State == BenchmarkScoring {
					if err := s.ReconcileBenchmark(ctx, b.ID); err != nil {
						s.log.Error("sweep: reconcile benchmark", "benchmark_run_id", b.ID, "err", err)
					}
				}
			}
		}
	}
}

// HandleRunUpdate processes one run update synchronously: it advances any
// benchmark the run is a trial of, and performs automatic scoring for
// production runs. Start calls it for every TopicRunUpdated message; it is
// exported so callers (and tests) can drive it directly.
func (s *Service) HandleRunUpdate(ctx context.Context, run *core.Run) error {
	if run == nil || !finishedForScoring(run.State) {
		return nil
	}
	if run.Metadata[MetaJudge] != "" {
		// A judge run is never scored itself.
		return s.handleJudgeUpdate(ctx, run)
	}
	if run.Metadata[MetaSelfImprovement] == "true" && run.WorkItemID != "" {
		// A pull request of the run gets its review item.
		if err := s.reviewImprovementPRs(ctx, run); err != nil {
			s.log.Error("self-improvement review of pull requests", "run_id", run.ID, "err", err)
		}
	}
	if bid := run.Metadata[MetaBenchmarkRun]; bid != "" {
		// Root trial runs carry the benchmark id; descendants inherit metadata but
		// are not trials themselves (handleTrialUpdate ignores unknown run ids).
		return s.handleTrialUpdate(ctx, bid, run)
	}
	if run.State == core.RunError {
		return nil // scorers ignore runs in error
	}
	return s.autoScore(ctx, run)
}

// lockKey serializes work on one logical key (scorer+run, factory+agent, benchmark).
func (s *Service) lockKey(key string) func() {
	s.keyMu.Lock()
	l := s.keyLocks[key]
	if l == nil {
		l = &keyLock{}
		s.keyLocks[key] = l
	}
	l.refs++
	s.keyMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		s.keyMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(s.keyLocks, key)
		}
		s.keyMu.Unlock()
	}
}

func (s *Service) intN(n int) int {
	s.randMu.Lock()
	defer s.randMu.Unlock()
	return s.opts.Rand.IntN(n)
}

func (s *Service) now() time.Time { return s.opts.Clock().UTC() }

// activeDefinition loads the factory, its active definition version and (when a
// compiler is configured) the compiled definition.
func (s *Service) activeDefinition(ctx context.Context, factoryID string) (*core.Factory, *core.DefinitionVersion, *core.CompiledDefinition, error) {
	f, err := s.opts.Store.GetFactory(ctx, factoryID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get factory %s: %w", factoryID, err)
	}
	if f.ActiveVersionID == "" {
		return f, nil, nil, nil
	}
	v, err := s.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
	if err != nil {
		return f, nil, nil, fmt.Errorf("get definition version %s: %w", f.ActiveVersionID, err)
	}
	if s.opts.Compile == nil {
		return f, v, nil, nil
	}
	def, verrs := s.opts.Compile(v.Files)
	if def == nil {
		msg := "definition does not compile"
		if len(verrs) > 0 {
			msg += ": " + verrs[0].Error()
		}
		return f, v, nil, fmt.Errorf("%w: %s", core.ErrInvalid, msg)
	}
	return f, v, def, nil
}

func (s *Service) runURL(runID string) string {
	if s.opts.PublicURL == "" {
		return ""
	}
	return links.Run(s.opts.PublicURL, runID)
}

func clampThreshold(n int) int {
	switch {
	case n < MinFailedRunThreshold:
		return MinFailedRunThreshold
	case n > MaxFailedRunThreshold:
		return MaxFailedRunThreshold
	}
	return n
}

// finishedForScoring reports whether a run state is one the scoring service acts on.
func finishedForScoring(st core.RunState) bool {
	return st.Terminal() || st.WaitsForInput()
}
