package scoring

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// maxAncestorDepth bounds parent-chain walks.
const maxAncestorDepth = 32

// ShouldSample reports whether a run is in a scorer's sample. ratePercent is
// the scorer's samplingRate as a percentage from 0 through 100 (rounded to two
// decimal places, as in scorer.md). The decision is a deterministic function of
// (scorerID, runID): a SHA-256 of both is mapped uniformly onto [0, 100), so
// repeated deliveries of the same run update always agree and different
// scorers sample independently.
func ShouldSample(scorerID, runID string, ratePercent float64) bool {
	rate := math.Round(ratePercent*100) / 100
	if rate <= 0 || math.IsNaN(rate) {
		return false
	}
	if rate >= 100 {
		return true
	}
	h := sha256.Sum256([]byte(scorerID + "\x00" + runID))
	// 53 bits give an exact float64 in [0, 1).
	u := float64(binary.BigEndian.Uint64(h[:8])>>11) / float64(uint64(1)<<53)
	return u*100 < rate
}

// scorerApplies returns the scorer's entry for agentName, if any.
func scorerApplies(sc core.Scorer, agentName string) (core.ScorerAgent, bool) {
	for _, a := range sc.Agents {
		if a.Name == agentName {
			return a, true
		}
	}
	return core.ScorerAgent{}, false
}

// autoScore performs automatic scoring of a finished production run.
func (s *Service) autoScore(ctx context.Context, run *core.Run) error {
	if run.State != core.RunSucceeded && run.State != core.RunFailed {
		return nil
	}
	if run.AgentName == "" || isJudgeRun(run) {
		return nil
	}
	scorers, err := s.opts.Store.ListScorers(ctx, run.FactoryID)
	if err != nil {
		return fmt.Errorf("list scorers: %w", err)
	}
	var eligible []core.Scorer
	for _, sc := range scorers {
		if _, ok := scorerApplies(sc, run.AgentName); !ok {
			continue
		}
		if !ShouldSample(sc.ID, run.ID, sc.SamplingRate) {
			continue
		}
		eligible = append(eligible, sc)
	}
	if len(eligible) == 0 {
		return nil
	}
	// Benchmark trials (and their descendants) are scored by the benchmark, not
	// by production scorers.
	if trial, err := s.isBenchmarkRun(ctx, run); err != nil {
		return err
	} else if trial {
		return nil
	}
	var errs []error
	for _, sc := range eligible {
		score, created, err := s.scoreOnce(ctx, sc, run, false)
		if err != nil {
			errs = append(errs, fmt.Errorf("scorer %s: %w", sc.Slug, err))
			continue
		}
		if created && score != nil && !score.Passed && sc.SelfImprovement && s.opts.Engine != nil {
			if _, err := s.checkAgent(ctx, run.FactoryID, run.AgentName, false); err != nil {
				errs = append(errs, fmt.Errorf("self-improvement check: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// isBenchmarkRun reports whether run or any ancestor was started by a benchmark.
func (s *Service) isBenchmarkRun(ctx context.Context, run *core.Run) (bool, error) {
	cur := run
	for depth := 0; depth < maxAncestorDepth; depth++ {
		if strings.HasPrefix(cur.TriggeredBy, "benchmark:") || cur.Metadata[MetaBenchmarkRun] != "" {
			return true, nil
		}
		if cur.ParentRunID == "" {
			return false, nil
		}
		parent, err := s.opts.Store.GetRun(ctx, cur.ParentRunID)
		if errors.Is(err, core.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("get parent run: %w", err)
		}
		cur = parent
	}
	return false, nil
}

// ScoreRun scores one run on demand with the given scorer (by ID or slug),
// regardless of sampling and of whether the run's agent is selected by the
// scorer. Scoring a run again replaces that scorer's previous result. With
// judge runs (Options.StartJudge), the result is a pending score: it has no
// ID, and JudgeRunID names the judge run. The score is stored when the judge
// run finishes.
func (s *Service) ScoreRun(ctx context.Context, scorerID, runID string) (*core.Score, error) {
	run, err := s.opts.Store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", runID, err)
	}
	sc, err := s.findScorer(ctx, run.FactoryID, scorerID)
	if err != nil {
		return nil, err
	}
	if isJudgeRun(run) {
		return nil, fmt.Errorf("%w: a judge run is not scored", core.ErrInvalid)
	}
	score, _, err := s.scoreOnce(ctx, *sc, run, true)
	if err != nil {
		return nil, err
	}
	if score.ID == "" {
		return score, nil // the judge run works
	}
	if !score.Passed && sc.SelfImprovement && s.opts.Engine != nil && run.AgentName != "" {
		if _, err := s.checkAgent(ctx, run.FactoryID, run.AgentName, false); err != nil {
			s.log.Error("self-improvement check after manual score", "run_id", runID, "err", err)
		}
	}
	return score, nil
}

func (s *Service) findScorer(ctx context.Context, factoryID, idOrSlug string) (*core.Scorer, error) {
	scorers, err := s.opts.Store.ListScorers(ctx, factoryID)
	if err != nil {
		return nil, fmt.Errorf("list scorers: %w", err)
	}
	for i := range scorers {
		if scorers[i].ID == idOrSlug {
			return &scorers[i], nil
		}
	}
	for i := range scorers {
		if scorers[i].Slug == idOrSlug || scorers[i].Name == idOrSlug {
			return &scorers[i], nil
		}
	}
	return nil, fmt.Errorf("scorer %q: %w", idOrSlug, core.ErrNotFound)
}

// findScore returns the stored score of scorerID for runID, if any.
func (s *Service) findScore(ctx context.Context, _ string, scorerID, runID string) (*core.Score, error) {
	sc, err := s.opts.Store.GetScore(ctx, scorerID, runID)
	if errors.Is(err, core.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get score: %w", err)
	}
	return sc, nil
}

// scoreOnce judges run with sc and stores the result. With replace=false an
// existing score is returned unchanged (created=false). With judge runs, it
// starts the judge run and returns a pending score (no ID, created=false);
// handleJudgeUpdate stores the result.
func (s *Service) scoreOnce(ctx context.Context, sc core.Scorer, run *core.Run, replace bool) (*core.Score, bool, error) {
	unlock := s.lockKey("score:" + sc.ID + ":" + run.ID)
	defer unlock()

	existing, err := s.findScore(ctx, run.FactoryID, sc.ID, run.ID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil && !replace {
		return existing, false, nil
	}
	if s.opts.StartJudge != nil {
		pending, err := s.pendingJudge(ctx, sc.ID, run.ID)
		if err != nil {
			return nil, false, err
		}
		if pending == nil || finishedForScoring(pending.State) {
			// A finished judge whose update was lost is replaced by a new one.
			if pending, err = s.startScoreJudge(ctx, sc, run); err != nil {
				return nil, false, err
			}
		}
		return &core.Score{ScorerID: sc.ID, RunID: run.ID, AgentName: run.AgentName, JudgeRunID: pending.ID}, false, nil
	}
	includeDescendants := false
	if a, ok := scorerApplies(sc, run.AgentName); ok {
		includeDescendants = a.IncludeDescendants
	}
	transcript, err := s.BuildTranscript(ctx, run, includeDescendants)
	if err != nil {
		return nil, false, err
	}
	res, err := s.judge(ctx, sc.Model, sc.Rubric, transcript, sc.Labels, sc.PassingScore)
	if err != nil {
		return nil, false, err
	}
	score := &core.Score{
		ScorerID:  sc.ID,
		RunID:     run.ID,
		AgentName: run.AgentName,
		Label:     res.Label,
		Score:     res.Score,
		Passed:    res.Passed,
		Rationale: res.Rationale,
		CreatedAt: s.now(),
	}
	if existing != nil {
		score.ID = existing.ID
		if err := s.opts.Store.UpdateScore(ctx, score); err != nil {
			return nil, false, fmt.Errorf("update score: %w", err)
		}
	} else {
		score.ID = core.NewID("score")
		if err := s.opts.Store.CreateScore(ctx, score); err != nil {
			return nil, false, fmt.Errorf("create score: %w", err)
		}
	}
	s.log.Info("run scored", "scorer", sc.Slug, "run_id", run.ID, "label", score.Label, "passed", score.Passed)
	return score, true, nil
}

// judgeResult is a resolved classification.
type judgeResult struct {
	Label     string
	Score     float64
	Passed    bool
	Rationale string
}

func (s *Service) judge(ctx context.Context, model, rubric, transcript string, labels []core.ScorerLabel, passing float64) (*judgeResult, error) {
	if len(labels) == 0 {
		return nil, fmt.Errorf("%w: scorer has no labels", core.ErrInvalid)
	}
	label, rationale, err := s.opts.Classify(ctx, model, rubric, transcript, labels)
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	l, ok := resolveLabel(labels, label)
	if !ok {
		return nil, fmt.Errorf("judge returned undeclared label %q", label)
	}
	return &judgeResult{Label: l.Value, Score: l.Score, Passed: l.Score >= passing, Rationale: rationale}, nil
}

// resolveLabel matches the judge's answer to a declared label: exactly first,
// then case-insensitively ignoring surrounding whitespace, quotes and backticks.
func resolveLabel(labels []core.ScorerLabel, got string) (core.ScorerLabel, bool) {
	for _, l := range labels {
		if l.Value == got {
			return l, true
		}
	}
	norm := strings.Trim(strings.TrimSpace(got), "\"'`.")
	for _, l := range labels {
		if strings.EqualFold(l.Value, norm) {
			return l, true
		}
	}
	return core.ScorerLabel{}, false
}

// ---------- Transcripts ----------

const (
	maxMessageChars = 4000
	maxDescendants  = 200
)

// BuildTranscript renders a run's conversation as plain text for a judge. With
// includeDescendants, every descendant run (breadth-first) is appended as
// additional evidence. The result is capped at Options.MaxTranscriptBytes,
// keeping the beginning and the end.
func (s *Service) BuildTranscript(ctx context.Context, run *core.Run, includeDescendants bool) (string, error) {
	return s.buildTranscript(ctx, run, includeDescendants, maxMessageChars, s.opts.MaxTranscriptBytes)
}

// buildTranscript is BuildTranscript with limits: perMessage caps each message
// (0 means no cap) and total caps the result.
func (s *Service) buildTranscript(ctx context.Context, run *core.Run, includeDescendants bool, perMessage, total int) (string, error) {
	var b strings.Builder
	if err := s.writeRun(ctx, &b, run, 0, perMessage); err != nil {
		return "", err
	}
	if includeDescendants {
		queue := []string{run.ID}
		seen := map[string]bool{run.ID: true}
		count := 0
		for len(queue) > 0 && count < maxDescendants {
			parent := queue[0]
			queue = queue[1:]
			children, err := listAllRuns(ctx, s.opts.Store, core.RunFilter{ParentRunID: parent})
			if err != nil {
				return "", fmt.Errorf("list child runs: %w", err)
			}
			sortRunsByCreated(children)
			for i := range children {
				c := &children[i]
				if seen[c.ID] {
					continue
				}
				seen[c.ID] = true
				count++
				if count > maxDescendants {
					break
				}
				depth := 1
				if c.ParentRunID != run.ID {
					depth = 2
				}
				if err := s.writeRun(ctx, &b, c, depth, perMessage); err != nil {
					return "", err
				}
				queue = append(queue, c.ID)
			}
		}
	}
	return capMiddle(b.String(), total), nil
}

func (s *Service) writeRun(ctx context.Context, b *strings.Builder, run *core.Run, depth, perMessage int) error {
	truncate := func(s string, n int) string {
		if perMessage <= 0 {
			return s
		}
		return truncate(s, n)
	}
	kind := "Run"
	if depth > 0 {
		kind = "Child run"
	}
	fmt.Fprintf(b, "=== %s %s", kind, run.ID)
	if run.AgentName != "" {
		fmt.Fprintf(b, " (agent: %s", run.AgentName)
		if run.Name != "" && run.Name != run.AgentName {
			fmt.Fprintf(b, ", name: %s", run.Name)
		}
		b.WriteString(")")
	}
	if run.ParentRunID != "" {
		fmt.Fprintf(b, " parent: %s", run.ParentRunID)
	}
	fmt.Fprintf(b, " state: %s ===\n", run.State)
	msgs, err := listAllMessages(ctx, s.opts.Store, run.ID)
	if err != nil {
		return fmt.Errorf("list run messages %s: %w", run.ID, err)
	}
	if len(msgs) == 0 && run.Prompt != "" {
		fmt.Fprintf(b, "[user]\n%s\n\n", truncate(run.Prompt, maxMessageChars))
	}
	for _, m := range msgs {
		fmt.Fprintf(b, "[%s]\n%s\n\n", m.Role, truncate(m.Content, maxMessageChars))
	}
	if run.Result != "" {
		fmt.Fprintf(b, "[final result]\n%s\n\n", truncate(run.Result, maxMessageChars))
	}
	if run.Error != "" {
		fmt.Fprintf(b, "[error]\n%s\n\n", truncate(run.Error, maxMessageChars))
	}
	return nil
}

// listAllMessages pages through a run's messages by Seq.
func listAllMessages(ctx context.Context, st core.Store, runID string) ([]core.RunMessage, error) {
	var out []core.RunMessage
	var after int64
	for {
		page, err := st.ListRunMessages(ctx, runID, after)
		if err != nil {
			return nil, err
		}
		progressed := false
		for _, m := range page {
			if m.Seq > after {
				out = append(out, m)
				after = m.Seq
				progressed = true
			}
		}
		if !progressed {
			return out, nil
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… [truncated %d bytes]", len(s)-cut)
}

// capMiddle keeps the head and tail of s within max bytes.
func capMiddle(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	marker := fmt.Sprintf("\n\n… [transcript truncated: %d bytes omitted] …\n\n", len(s)-max)
	keep := max - len(marker)
	if keep < 2 {
		return s[:max]
	}
	head := keep / 2
	tail := keep - head
	for head > 0 && !isRuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !isRuneStart(s[start]) {
		start++
	}
	return s[:head] + marker + s[start:]
}

func isRuneStart(c byte) bool { return c&0xC0 != 0x80 }
