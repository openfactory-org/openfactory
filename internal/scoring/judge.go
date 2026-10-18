package scoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Judge run metadata keys. A judge run carries MetaJudge; the other
// keys say what it scores.
const (
	MetaJudge          = "judge"            // JudgeScore or JudgeTrial
	MetaJudgeScorer    = "judge_scorer"     // the scorer ID (JudgeScore)
	MetaJudgeScoredRun = "judge_scored_run" // the run that the judge scores
	MetaJudgeBenchmark = "judge_benchmark"  // the benchmark run (JudgeTrial)
	MetaJudgeTrial     = "judge_trial"      // the trial (JudgeTrial)
	MetaJudgeKey       = "judge_key"        // the benchmark scorer key (JudgeTrial)

	JudgeScore = "score"
	JudgeTrial = "trial"
)

// TriggeredByScorer is the Run.TriggeredBy prefix of judge runs of scorers.
const TriggeredByScorer = "scorer:"

// DefaultMaxJudgeFileBytes caps the transcript file of a judge run.
const DefaultMaxJudgeFileBytes = 8 << 20

// JudgeExec holds the execution settings of a judge run.
type JudgeExec struct {
	Harness    *core.HarnessConfig       `json:"harness,omitempty"`
	Machine    string                    `json:"machine,omitempty"`
	Secrets    []string                  `json:"secrets,omitempty"`
	MCPServers map[string]core.MCPServer `json:"mcp_servers,omitempty"`
}

func scorerExec(sc core.Scorer) JudgeExec {
	ex := JudgeExec{Machine: sc.Machine, Secrets: sc.Secrets, MCPServers: sc.MCPServers}
	if sc.Harness != nil {
		h := *sc.Harness
		if sc.Model != "" {
			h.Model = sc.Model
		}
		ex.Harness = &h
	}
	return ex
}

// pendingJudgeKey is the key/value store key that holds the judge run of a
// scorer for a run while the judge works.
func pendingJudgeKey(scorerID, runID string) string {
	return "scoring:judge:" + scorerID + ":" + runID
}

// judgeInstructions are the system instructions of a judge run.
func judgeInstructions(name, rubric string, labels []core.ScorerLabel) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the judge of the scorer %q. You classify one agent run with exactly one label.\n\n", name)
	b.WriteString("The evidence is in the skill `" + core.JudgeSkillName + "` in your workspace. Its file `transcript.md` holds the full transcript of the run, with its child runs. ")
	b.WriteString("The repositories in your workspace are checked out at the commits where the run started. ")
	b.WriteString("You can read files, run commands and use your tools to check the work. Do not change anything outside your workspace: do not push, comment, post messages or open pull requests.\n\n")
	b.WriteString("## Rubric\n\n")
	b.WriteString(strings.TrimSpace(rubric))
	b.WriteString("\n\n## Labels\n\n")
	for _, l := range labels {
		fmt.Fprintf(&b, "- `%s` (score %.2f)", l.Value, l.Score)
		if l.Description != "" {
			fmt.Fprintf(&b, ": %s", oneLine(l.Description))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n## Answer\n\n")
	b.WriteString("End your final message with one JSON object on its own line, and nothing after it:\n\n")
	b.WriteString("{\"label\": \"<one label value from the list>\", \"rationale\": \"<why, in a few sentences>\"}\n")
	return b.String()
}

func judgePrompt(scorerName string, run *core.Run) string {
	agent := run.AgentName
	if agent == "" {
		agent = "unknown"
	}
	return fmt.Sprintf("Score the run %s (agent %s, state %s) with the scorer %q. Read `transcript.md` of the skill `%s` first. Then give your answer as the JSON object of your instructions.",
		run.ID, agent, run.State, scorerName, core.JudgeSkillName)
}

// judgeRequest builds the request of a judge run.
func (s *Service) judgeRequest(ctx context.Context, factoryID, name, rubric string, labels []core.ScorerLabel, ex JudgeExec, scored *core.Run, includeDescendants bool, refs []core.RepoAtCommit) (core.JudgeRunRequest, error) {
	if ex.Harness == nil {
		return core.JudgeRunRequest{}, fmt.Errorf("%w: the scorer %q has no harness (set harness or model on the scorer, scorerDefaults or agentDefaults)", core.ErrInvalid, name)
	}
	transcript, err := s.buildTranscript(ctx, scored, includeDescendants, 0, DefaultMaxJudgeFileBytes)
	if err != nil {
		return core.JudgeRunRequest{}, err
	}
	if len(refs) == 0 {
		starts, err := core.LoadStartCommits(ctx, s.opts.Store, scored.ID)
		if err != nil {
			return core.JudgeRunRequest{}, err
		}
		for _, st := range starts {
			refs = append(refs, st.RepoAtCommit())
		}
	}
	return core.JudgeRunRequest{
		FactoryID:        factoryID,
		Name:             clip("scorer-"+name, 64),
		Instructions:     judgeInstructions(name, rubric, labels),
		Prompt:           judgePrompt(name, scored),
		Harness:          *ex.Harness,
		Machine:          ex.Machine,
		Secrets:          ex.Secrets,
		MCPServers:       ex.MCPServers,
		Files:            map[string]string{"transcript.md": transcript},
		StartingRepoRefs: refs,
	}, nil
}

// startScoreJudge starts the judge run of scorer sc for run. The score is
// stored when the judge run finishes (handleJudgeUpdate).
func (s *Service) startScoreJudge(ctx context.Context, sc core.Scorer, run *core.Run) (*core.Run, error) {
	includeDescendants := false
	if a, ok := scorerApplies(sc, run.AgentName); ok {
		includeDescendants = a.IncludeDescendants
	}
	req, err := s.judgeRequest(ctx, run.FactoryID, scorerName(sc), sc.Rubric, sc.Labels, scorerExec(sc), run, includeDescendants, nil)
	if err != nil {
		return nil, err
	}
	req.TriggeredBy = TriggeredByScorer + sc.ID
	req.Metadata = map[string]string{
		MetaJudge:          JudgeScore,
		MetaJudgeScorer:    sc.ID,
		MetaJudgeScoredRun: run.ID,
	}
	judge, err := s.opts.StartJudge(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("start judge run: %w", err)
	}
	if err := s.opts.Store.KVSet(ctx, pendingJudgeKey(sc.ID, run.ID), judge.ID); err != nil {
		return nil, fmt.Errorf("record judge run: %w", err)
	}
	s.log.Info("judge run started", "scorer", sc.Slug, "run_id", run.ID, "judge_run_id", judge.ID)
	return judge, nil
}

// pendingJudge returns the judge run that works on scorer × run, if any.
func (s *Service) pendingJudge(ctx context.Context, scorerID, runID string) (*core.Run, error) {
	id, err := s.opts.Store.KVGet(ctx, pendingJudgeKey(scorerID, runID))
	if errors.Is(err, core.ErrNotFound) || (err == nil && id == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r, err := s.opts.Store.GetRun(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	return r, err
}

func (s *Service) clearPendingJudge(ctx context.Context, scorerID, runID string) {
	if err := s.opts.Store.KVSet(ctx, pendingJudgeKey(scorerID, runID), ""); err != nil {
		s.log.Warn("clear judge marker", "scorer_id", scorerID, "run_id", runID, "err", err)
	}
}

// handleJudgeUpdate stores the answer of a finished judge run.
func (s *Service) handleJudgeUpdate(ctx context.Context, judge *core.Run) error {
	switch judge.Metadata[MetaJudge] {
	case JudgeScore:
		return s.finishScoreJudge(ctx, judge)
	case JudgeTrial:
		return s.finishTrialJudge(ctx, judge)
	}
	return nil
}

func (s *Service) finishScoreJudge(ctx context.Context, judge *core.Run) error {
	scorerID, runID := judge.Metadata[MetaJudgeScorer], judge.Metadata[MetaJudgeScoredRun]
	if scorerID == "" || runID == "" {
		return nil
	}
	unlock := s.lockKey("score:" + scorerID + ":" + runID)
	defer unlock()
	pending, err := s.pendingJudge(ctx, scorerID, runID)
	if err != nil {
		return err
	}
	if pending == nil || pending.ID != judge.ID {
		return nil // stored already, or a newer judge run replaced this one
	}
	scored, err := s.opts.Store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("get scored run: %w", err)
	}
	sc, err := s.findScorer(ctx, scored.FactoryID, scorerID)
	if err != nil {
		s.clearPendingJudge(ctx, scorerID, runID)
		return err
	}
	usage, err := s.treeUsage(ctx, judge)
	if err != nil {
		return err
	}
	label, rationale, jerr := s.judgeAnswer(ctx, judge)
	s.stopJudge(ctx, judge)
	if jerr != nil {
		s.clearPendingJudge(ctx, scorerID, runID)
		s.log.Error("judge run gave no label", "scorer", sc.Slug, "run_id", runID, "judge_run_id", judge.ID, "err", jerr)
		return nil
	}
	l, ok := resolveLabel(sc.Labels, label)
	if !ok {
		s.clearPendingJudge(ctx, scorerID, runID)
		s.log.Error("judge run returned an undeclared label", "scorer", sc.Slug, "run_id", runID, "judge_run_id", judge.ID, "label", label)
		return nil
	}
	existing, err := s.findScore(ctx, scored.FactoryID, sc.ID, runID)
	if err != nil {
		return err
	}
	score := &core.Score{
		ScorerID:   sc.ID,
		RunID:      runID,
		AgentName:  scored.AgentName,
		Label:      l.Value,
		Score:      l.Score,
		Passed:     l.Score >= sc.PassingScore,
		Rationale:  rationale,
		CreatedAt:  s.now(),
		JudgeRunID: judge.ID,
		JudgeUsage: &usage,
	}
	if existing != nil {
		score.ID = existing.ID
		score.ImprovementRunID = existing.ImprovementRunID
		if err := s.opts.Store.UpdateScore(ctx, score); err != nil {
			return fmt.Errorf("update score: %w", err)
		}
	} else {
		score.ID = core.NewID("score")
		if err := s.opts.Store.CreateScore(ctx, score); err != nil {
			return fmt.Errorf("create score: %w", err)
		}
	}
	s.clearPendingJudge(ctx, scorerID, runID)
	s.log.Info("run scored", "scorer", sc.Slug, "run_id", runID, "judge_run_id", judge.ID, "label", score.Label, "passed", score.Passed)
	if !score.Passed && sc.SelfImprovement && s.opts.Engine != nil && scored.AgentName != "" {
		if _, err := s.checkAgent(ctx, scored.FactoryID, scored.AgentName, false); err != nil {
			s.log.Error("self-improvement check after judge run", "run_id", runID, "err", err)
		}
	}
	return nil
}

// stopJudge cancels a judge run that waits for input. A judge has one turn:
// after its answer, nothing is left to wait for.
func (s *Service) stopJudge(ctx context.Context, judge *core.Run) {
	if judge.State.Terminal() || s.opts.Engine == nil {
		return
	}
	if err := s.opts.Engine.Cancel(ctx, judge.ID, "scoring"); err != nil && !errors.Is(err, core.ErrNotFound) {
		s.log.Warn("stop judge run", "judge_run_id", judge.ID, "err", err)
	}
}

// judgeAnswer reads the label and the rationale from a finished judge run.
func (s *Service) judgeAnswer(ctx context.Context, judge *core.Run) (string, string, error) {
	switch judge.State {
	case core.RunCancelled:
		return "", "", errors.New("the judge run was cancelled")
	case core.RunError, core.RunFailed:
		msg := judge.Error
		if msg == "" {
			msg = string(judge.State)
		}
		if label, rationale, ok := parseJudgeAnswer(judge.Result); ok && judge.State == core.RunFailed {
			return label, rationale, nil
		}
		return "", "", fmt.Errorf("the judge run ended with %s: %s", judge.State, msg)
	}
	if label, rationale, ok := parseJudgeAnswer(judge.Result); ok {
		return label, rationale, nil
	}
	msgs, err := listAllMessages(ctx, s.opts.Store, judge.ID)
	if err != nil {
		return "", "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "assistant" {
			continue
		}
		if label, rationale, ok := parseJudgeAnswer(msgs[i].Content); ok {
			return label, rationale, nil
		}
	}
	return "", "", errors.New("the final message has no JSON object with a label")
}

// parseJudgeAnswer finds the last JSON object with a "label" in text.
func parseJudgeAnswer(text string) (label, rationale string, ok bool) {
	for i := strings.LastIndex(text, "{"); i >= 0; i = strings.LastIndex(text[:i], "{") {
		var a struct {
			Label     string `json:"label"`
			Rationale string `json:"rationale"`
		}
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		if err := dec.Decode(&a); err == nil && strings.TrimSpace(a.Label) != "" {
			return strings.TrimSpace(a.Label), strings.TrimSpace(a.Rationale), true
		}
		if i == 0 {
			break
		}
	}
	return "", "", false
}

// treeUsage returns the usage of a run and its descendants.
func (s *Service) treeUsage(ctx context.Context, run *core.Run) (core.Usage, error) {
	u := run.Usage
	kids, err := descendants(ctx, s.opts.Store, run.ID)
	if err != nil {
		return u, fmt.Errorf("list descendants of %s: %w", run.ID, err)
	}
	for _, k := range kids {
		u.InputTokens += k.Usage.InputTokens
		u.OutputTokens += k.Usage.OutputTokens
		u.CostUSD += k.Usage.CostUSD
		u.DurationMS += k.Usage.DurationMS
	}
	return u, nil
}

// isJudgeRun reports whether r is a judge run.
func isJudgeRun(r *core.Run) bool {
	return r.Metadata[MetaJudge] != "" || strings.HasPrefix(r.TriggeredBy, TriggeredByScorer)
}
