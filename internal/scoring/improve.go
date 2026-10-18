package scoring

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Self-improvement run metadata keys (become OF_METADATA_* in the sandbox).
const (
	MetaSelfImprovement      = "self_improvement"
	MetaSelfImprovementAgent = "self_improvement_agent"
	// TriggeredBySelfImprovement is the Run.TriggeredBy value of Self-improvement runs.
	TriggeredBySelfImprovement = "self_improvement"
	// SourceSelfImprovement is the SourceRef.Provider of Self-improvement work items.
	SourceSelfImprovement = "self_improvement"
)

// Reviewer pool types (selfImprovement.reviewerType).
const (
	ReviewerAdmins = "admins"
	ReviewerTeam   = "team"
	ReviewerCustom = "custom"
	ReviewerNone   = "none"
)

const (
	maxFailuresInPrompt = 25
	maxFailureExcerpt   = 3000
	maxPromptFileBytes  = 20_000
	maxPromptFilesTotal = 120_000
	maxPromptExcerpt    = 1500
)

// Failure is one unreviewed failed score feeding Self-improvement.
type Failure struct {
	Score  core.Score
	Scorer core.Scorer
}

// improvementSettings is the effective selfImprovement configuration.
type improvementSettings struct {
	Threshold      int
	ReviewerType   string
	ReviewerEmails []string
	AnalysisModel  string
}

func (s *Service) improvementSettings(def *core.CompiledDefinition) improvementSettings {
	cfg := improvementSettings{Threshold: s.opts.DefaultFailedRunThreshold, ReviewerType: ReviewerAdmins}
	if def != nil && def.SelfImprovement != nil {
		si := def.SelfImprovement
		if si.FailedRunThreshold > 0 {
			cfg.Threshold = clampThreshold(si.FailedRunThreshold)
		}
		if si.ReviewerType != "" {
			cfg.ReviewerType = si.ReviewerType
		}
		cfg.ReviewerEmails = si.ReviewerEmails
		cfg.AnalysisModel = si.AnalysisModel
	}
	if cfg.AnalysisModel == "" {
		cfg.AnalysisModel = s.opts.AnalysisModel
	}
	return cfg
}

// UnreviewedFailures returns, per agent, the distinct (one per run) failing
// scores from scorers with selfImprovement enabled that are not yet attached
// to a Self-improvement run. Each agent's failures are ordered oldest first.
func (s *Service) UnreviewedFailures(ctx context.Context, factoryID string) (map[string][]Failure, error) {
	scorers, err := s.opts.Store.ListScorers(ctx, factoryID)
	if err != nil {
		return nil, fmt.Errorf("list scorers: %w", err)
	}
	byRun := map[string]map[string]Failure{} // agent -> run -> latest failure
	for _, sc := range scorers {
		if !sc.SelfImprovement {
			continue
		}
		scores, err := s.opts.Store.ListScores(ctx, factoryID, sc.ID, nil)
		if err != nil {
			return nil, fmt.Errorf("list scores: %w", err)
		}
		for _, sc0 := range scores {
			if sc0.Passed || sc0.ImprovementRunID != "" || sc0.AgentName == "" || sc0.ScorerID != sc.ID {
				continue
			}
			m := byRun[sc0.AgentName]
			if m == nil {
				m = map[string]Failure{}
				byRun[sc0.AgentName] = m
			}
			if prev, ok := m[sc0.RunID]; !ok || sc0.CreatedAt.After(prev.Score.CreatedAt) {
				m[sc0.RunID] = Failure{Score: sc0, Scorer: sc}
			}
		}
	}
	out := map[string][]Failure{}
	for agent, m := range byRun {
		list := make([]Failure, 0, len(m))
		for _, f := range m {
			list = append(list, f)
		}
		sort.Slice(list, func(i, j int) bool {
			if !list[i].Score.CreatedAt.Equal(list[j].Score.CreatedAt) {
				return list[i].Score.CreatedAt.Before(list[j].Score.CreatedAt)
			}
			return list[i].Score.ID < list[j].Score.ID
		})
		out[agent] = list
	}
	return out, nil
}

// CheckSelfImprovement is the scheduled check: for every agent whose distinct
// unreviewed failures reach failedRunThreshold, or whose oldest unreviewed
// failure is at least MaxFailureAge old, it starts one Self-improvement run.
func (s *Service) CheckSelfImprovement(ctx context.Context, factoryID string) ([]*core.Run, error) {
	return s.check(ctx, factoryID, "", false)
}

// TriggerSelfImprovement runs the check now ("Run now"): any agent with at
// least one unreviewed failure gets a Self-improvement run. An empty agent
// processes every agent with failures.
func (s *Service) TriggerSelfImprovement(ctx context.Context, factoryID, agent string) ([]*core.Run, error) {
	if s.opts.Engine == nil {
		return nil, errors.New("scoring: Engine is required for self-improvement")
	}
	return s.check(ctx, factoryID, agent, true)
}

func (s *Service) check(ctx context.Context, factoryID, onlyAgent string, adHoc bool) ([]*core.Run, error) {
	failures, err := s.UnreviewedFailures(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	agents := make([]string, 0, len(failures))
	for a := range failures {
		if onlyAgent == "" || a == onlyAgent {
			agents = append(agents, a)
		}
	}
	sort.Strings(agents)
	var runs []*core.Run
	var errs []error
	for _, a := range agents {
		run, err := s.checkAgent(ctx, factoryID, a, adHoc)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent %s: %w", a, err))
			continue
		}
		if run != nil {
			runs = append(runs, run)
		}
	}
	return runs, errors.Join(errs...)
}

// checkAgent starts a Self-improvement run for one agent when its trigger
// condition holds. It returns (nil, nil) when nothing was started.
func (s *Service) checkAgent(ctx context.Context, factoryID, agent string, adHoc bool) (*core.Run, error) {
	unlock := s.lockKey("improve:" + factoryID + ":" + agent)
	defer unlock()

	// Re-read under the lock so concurrent triggers don't double-start.
	all, err := s.UnreviewedFailures(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	failures := all[agent]
	if len(failures) == 0 {
		return nil, nil
	}
	f, v, def, err := s.activeDefinition(ctx, factoryID)
	if err != nil && f == nil {
		return nil, err
	}
	if err != nil {
		s.log.Warn("self-improvement: using default settings", "factory_id", factoryID, "err", err)
	}
	cfg := s.improvementSettings(def)
	if !adHoc && !s.thresholdReached(failures, cfg.Threshold) {
		return nil, nil
	}
	return s.startImprovement(ctx, f, v, agent, failures, cfg)
}

func (s *Service) thresholdReached(failures []Failure, threshold int) bool {
	if len(failures) >= threshold {
		return true
	}
	if s.opts.MaxFailureAge > 0 && len(failures) > 0 {
		return s.now().Sub(failures[0].Score.CreatedAt) >= s.opts.MaxFailureAge
	}
	return false
}

func (s *Service) startImprovement(ctx context.Context, f *core.Factory, v *core.DefinitionVersion, agent string, failures []Failure, cfg improvementSettings) (*core.Run, error) {
	if s.opts.Engine == nil {
		return nil, errors.New("scoring: Engine is required for self-improvement")
	}
	runs := make(map[string]*core.Run, len(failures))
	for _, fl := range failures {
		r, err := s.opts.Store.GetRun(ctx, fl.Score.RunID)
		if err == nil {
			runs[r.ID] = r
		} else if !errors.Is(err, core.ErrNotFound) {
			return nil, fmt.Errorf("get run %s: %w", fl.Score.RunID, err)
		}
	}
	prompt := s.ImprovementPrompt(ctx, f, v, agent, failures, runs)
	req := core.DispatchRequest{
		FactoryID: f.ID,
		Prompt:    prompt,
		Source: core.SourceRef{
			Provider:   SourceSelfImprovement,
			Kind:       "task",
			ExternalID: "self-improvement/" + agent + "/" + core.NewID(""),
			Title:      "Self-improvement: " + agent,
		},
		AgentName:   s.opts.ImprovementAgent,
		Title:       fmt.Sprintf("Self-improvement: %s (%d failing runs)", agent, len(failures)),
		TriggeredBy: TriggeredBySelfImprovement,
		Metadata: map[string]string{
			MetaSelfImprovement:      "true",
			MetaSelfImprovementAgent: agent,
		},
	}
	if cfg.AnalysisModel != "" {
		req.Overrides = &core.AgentOverrides{Model: cfg.AnalysisModel}
	}
	wi, run, err := s.opts.Engine.Dispatch(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("dispatch self-improvement run: %w", err)
	}
	if run == nil {
		return nil, errors.New("dispatch self-improvement run: engine returned no run")
	}
	var errs []error
	for _, fl := range failures {
		sc := fl.Score
		sc.ImprovementRunID = run.ID
		if err := s.opts.Store.UpdateScore(ctx, &sc); err != nil {
			errs = append(errs, fmt.Errorf("mark score %s: %w", sc.ID, err))
		}
	}
	// The review item is not created now, because no change exists yet. A
	// managed definition gets it when the run proposes a change
	// (propose_definition_change). A pull request gets it when the run has
	// linked the pull request to its work item (reviewImprovementPRs).
	if wi != nil {
		reviewer, err := s.pickReviewer(ctx, f.TeamID, cfg)
		if err != nil {
			s.log.Warn("self-improvement: no reviewer selected", "factory_id", f.ID, "err", err)
		}
		track := improvementTrack{ReviewerID: reviewer, RunID: run.ID, Agent: agent, Regressions: s.regressionsSection(failures)}
		if err := s.saveImprovementTrack(ctx, wi.ID, track); err != nil {
			errs = append(errs, fmt.Errorf("record self-improvement run: %w", err))
		}
	}
	s.log.Info("self-improvement run started", "factory_id", f.ID, "agent", agent, "run_id", run.ID, "failures", len(failures))
	if len(errs) > 0 {
		s.log.Error("self-improvement bookkeeping", "run_id", run.ID, "err", errors.Join(errs...))
	}
	return run, nil
}

// pickReviewer randomly selects one eligible reviewer per reviewerType.
// It returns "" for reviewerType none.
func (s *Service) pickReviewer(ctx context.Context, teamID string, cfg improvementSettings) (string, error) {
	if cfg.ReviewerType == ReviewerNone {
		return "", nil
	}
	users, err := s.opts.Store.ListUsers(ctx, teamID)
	if err != nil {
		return "", fmt.Errorf("list users: %w", err)
	}
	pool := EligibleReviewers(users, cfg.ReviewerType, cfg.ReviewerEmails)
	if len(pool) == 0 {
		return "", fmt.Errorf("no eligible reviewers for reviewerType %q", cfg.ReviewerType)
	}
	return pool[s.intN(len(pool))].ID, nil
}

// EligibleReviewers filters team members by reviewer type: admins (team
// admins), team (any member), custom (members whose email is listed), none
// (nobody). Unknown types fall back to admins. Results are ordered by user ID.
func EligibleReviewers(users []core.User, reviewerType string, emails []string) []core.User {
	var pool []core.User
	switch reviewerType {
	case ReviewerNone:
		return nil
	case ReviewerTeam:
		pool = append(pool, users...)
	case ReviewerCustom:
		want := map[string]bool{}
		for _, e := range emails {
			want[strings.ToLower(strings.TrimSpace(e))] = true
		}
		for _, u := range users {
			if want[strings.ToLower(u.Email)] {
				pool = append(pool, u)
			}
		}
	default:
		for _, u := range users {
			if u.Role == core.RoleAdmin {
				pool = append(pool, u)
			}
		}
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].ID < pool[j].ID })
	return pool
}

func (s *Service) regressionsSection(failures []Failure) string {
	var b strings.Builder
	b.WriteString("## Regressions addressed\n\n")
	for _, fl := range failures {
		ref := fl.Score.RunID
		if u := s.runURL(fl.Score.RunID); u != "" {
			ref = fmt.Sprintf("[%s](%s)", fl.Score.RunID, u)
		}
		fmt.Fprintf(&b, "- Run %s: scorer `%s` labeled `%s` (score %.2f, passing %.2f)\n",
			ref, scorerName(fl.Scorer), fl.Score.Label, fl.Score.Score, fl.Scorer.PassingScore)
	}
	return b.String()
}

func scorerName(sc core.Scorer) string {
	if sc.Slug != "" {
		return sc.Slug
	}
	return sc.Name
}

// ImprovementPrompt builds the starting prompt of a Self-improvement run.
func (s *Service) ImprovementPrompt(ctx context.Context, f *core.Factory, v *core.DefinitionVersion, agent string, failures []Failure, runs map[string]*core.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are running Self-improvement for the factory %q.\n\n", f.Name)
	fmt.Fprintf(&b, "Scorers flagged %d distinct failing runs of the agent `%s`. ", len(failures), agent)
	b.WriteString("Investigate why these runs failed, find the common root cause, and propose a fix as a pull request.\n\n")

	b.WriteString("## What to change\n\n")
	b.WriteString("Pick the smallest change that addresses the root cause:\n")
	b.WriteString("- The factory definition: the agent's instructions (`agents/" + agent + "/agent.md`), its skills (`agents/" + agent + "/skills/*/SKILL.md` or factory-wide `skills/*/SKILL.md`), its model/harness, or other configuration in `factory.yaml`.\n")
	b.WriteString("- Application code, when the failures are caused by the repository itself (missing scripts, broken tooling, unclear docs).\n\n")
	b.WriteString("Do not weaken or edit the scorers to make failures disappear. Keep definition changes valid against the definition schema.\n\n")

	b.WriteString("## How to deliver\n\n")
	switch {
	case f.Source == core.DefinitionGit && f.GitRepo != "":
		branch := f.GitBranch
		if branch == "" {
			branch = "main"
		}
		fmt.Fprintf(&b, "The factory definition lives in `%s` (production branch `%s`", f.GitRepo, branch)
		if f.GitPath != "" {
			fmt.Fprintf(&b, ", directory `%s`", f.GitPath)
		}
		b.WriteString("). Open a pull request against that branch for definition changes; definition checks validate it and merging applies it.\n")
	default:
		b.WriteString("The factory definition is managed by Open Factory (not in a git repository). For definition changes, call the factory tool `propose_definition_change` with the full new content of every changed file, keyed by path, and a short message. The tool checks the files and stores a proposed version; a reviewer sees the diff in the inbox and applies it with one approval. If the tool reports errors, fix the files and call it again. A rejection comes back to you as a message with the reason.\n")
	}
	b.WriteString("Open application-code changes as a pull request against the affected repository.\n")
	b.WriteString("Every pull request body must include a `Regressions addressed` section listing the failing runs and scorer results below, so reviewers can trace the change to its evidence. Nothing is adopted without human review.\n\n")

	b.WriteString(s.regressionsSection(failures))
	b.WriteString("\n## Failing runs\n\n")
	shown := failures
	if len(shown) > maxFailuresInPrompt {
		shown = shown[len(shown)-maxFailuresInPrompt:]
		fmt.Fprintf(&b, "(Showing the %d most recent of %d failures.)\n\n", maxFailuresInPrompt, len(failures))
	}
	for i, fl := range shown {
		fmt.Fprintf(&b, "### %d. Run %s\n\n", i+1, fl.Score.RunID)
		fmt.Fprintf(&b, "- Scorer: `%s`", scorerName(fl.Scorer))
		if fl.Scorer.Description != "" {
			fmt.Fprintf(&b, " — %s", oneLine(fl.Scorer.Description))
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "- Classification: `%s` (score %.2f; passing score %.2f)\n", fl.Score.Label, fl.Score.Score, fl.Scorer.PassingScore)
		if u := s.runURL(fl.Score.RunID); u != "" {
			fmt.Fprintf(&b, "- Link: %s\n", u)
		}
		if fl.Score.Rationale != "" {
			fmt.Fprintf(&b, "- Judge rationale: %s\n", truncate(oneLine(fl.Score.Rationale), 1500))
		}
		if r := runs[fl.Score.RunID]; r != nil {
			fmt.Fprintf(&b, "- Run state: %s\n", r.State)
			if r.Prompt != "" {
				fmt.Fprintf(&b, "\nPrompt excerpt:\n\n```\n%s\n```\n", truncate(r.Prompt, maxPromptExcerpt))
			}
			if summary := s.runSummary(ctx, r); summary != "" {
				fmt.Fprintf(&b, "\nTranscript summary (final messages):\n\n```\n%s\n```\n", summary)
			}
		}
		b.WriteString("\n")
	}

	if rubrics := scorerRubrics(failures); rubrics != "" {
		b.WriteString("## Scorer criteria\n\n")
		b.WriteString(rubrics)
	}
	if v != nil && len(v.Files) > 0 {
		b.WriteString("## Current factory definition\n\n")
		b.WriteString(definitionContext(v.Files, agent, failures))
	}
	return b.String()
}

// runSummary returns the tail of a run's conversation: the last assistant
// messages plus its result or error.
func (s *Service) runSummary(ctx context.Context, r *core.Run) string {
	msgs, err := listAllMessages(ctx, s.opts.Store, r.ID)
	if err != nil {
		return truncate(r.Result+r.Error, maxFailureExcerpt)
	}
	var parts []string
	for i := len(msgs) - 1; i >= 0 && len(parts) < 3; i-- {
		if msgs[i].Role == "assistant" && strings.TrimSpace(msgs[i].Content) != "" {
			parts = append(parts, truncate(msgs[i].Content, 1000))
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	if r.Error != "" {
		parts = append(parts, "ERROR: "+truncate(r.Error, 1000))
	} else if r.Result != "" && (len(parts) == 0 || parts[len(parts)-1] != r.Result) {
		parts = append(parts, "RESULT: "+truncate(r.Result, 1000))
	}
	return truncate(strings.Join(parts, "\n---\n"), maxFailureExcerpt)
}

func scorerRubrics(failures []Failure) string {
	seen := map[string]bool{}
	var b strings.Builder
	for _, fl := range failures {
		if seen[fl.Scorer.ID] {
			continue
		}
		seen[fl.Scorer.ID] = true
		fmt.Fprintf(&b, "### %s\n\n%s\n\nLabels:\n", scorerName(fl.Scorer), strings.TrimSpace(fl.Scorer.Rubric))
		for _, l := range fl.Scorer.Labels {
			fmt.Fprintf(&b, "- `%s` (score %.2f)", l.Value, l.Score)
			if l.Description != "" {
				fmt.Fprintf(&b, ": %s", oneLine(l.Description))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// definitionContext renders the relevant definition files in full (factory.yaml,
// the agent's agent.md and skills, the failing scorers) and lists the rest.
func definitionContext(files map[string]string, agent string, failures []Failure) string {
	root := definitionRoot(files)
	rel := func(p string) string { return strings.TrimPrefix(p, root) }
	scorerDirs := map[string]bool{}
	for _, fl := range failures {
		if fl.Scorer.Slug != "" {
			scorerDirs["scorers/"+fl.Scorer.Slug+"/"] = true
		}
	}
	relevant := func(p string) bool {
		r := rel(p)
		if r == "factory.yaml" || strings.HasPrefix(r, "agents/"+agent+"/") {
			return true
		}
		for d := range scorerDirs {
			if strings.HasPrefix(r, d) {
				return true
			}
		}
		return false
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("All files:\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	b.WriteString("\n")
	total := 0
	for _, p := range paths {
		if !relevant(p) {
			continue
		}
		content := truncate(files[p], maxPromptFileBytes)
		if total+len(content) > maxPromptFilesTotal {
			fmt.Fprintf(&b, "(Further files omitted to fit the prompt; read them from the definition source.)\n")
			break
		}
		total += len(content)
		lang := strings.TrimPrefix(path.Ext(p), ".")
		if lang == "yml" {
			lang = "yaml"
		}
		fmt.Fprintf(&b, "#### `%s`\n\n```%s\n%s\n```\n\n", p, lang, content)
	}
	return b.String()
}

// definitionRoot returns the directory prefix (with trailing slash, or "")
// holding factory.yaml within a definition file map.
func definitionRoot(files map[string]string) string {
	best := ""
	found := false
	for p := range files {
		if path.Base(p) != "factory.yaml" {
			continue
		}
		dir := strings.TrimSuffix(p, "factory.yaml")
		if !found || len(dir) < len(best) {
			best, found = dir, true
		}
	}
	return best
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
