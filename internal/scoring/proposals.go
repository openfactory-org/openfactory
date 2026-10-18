package scoring

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// Limits of a proposed definition change.
const (
	maxProposalFiles   = 200
	maxProposalBytes   = 2 << 20
	maxDiffBodyBytes   = 60_000
	maxDiffLinesPerHun = 4000
)

// ProposalRequest is the input of propose_definition_change.
type ProposalRequest struct {
	// RunID is the run that proposes the change.
	RunID string
	// Files maps a path (relative to the definition root) to its full new content.
	Files map[string]string
	// Delete lists the paths to remove.
	Delete []string
	// Message says what the change does and why.
	Message string
}

// ProposalError is a proposal that the compiler or the preflight refused.
type ProposalError struct {
	Errors []core.ValidationError
}

func (e *ProposalError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "the proposed definition has %d error(s):", len(e.Errors))
	for i, ve := range e.Errors {
		if i == 20 {
			fmt.Fprintf(&b, "\n- (%d more)", len(e.Errors)-i)
			break
		}
		fmt.Fprintf(&b, "\n- %s", ve.Error())
		if ve.Line > 0 {
			fmt.Fprintf(&b, " (line %d)", ve.Line)
		}
	}
	return b.String()
}

// Unwrap makes a ProposalError an ErrInvalid.
func (e *ProposalError) Unwrap() error { return core.ErrInvalid }

// ValidationErrors returns the errors (the API shows them).
func (e *ProposalError) ValidationErrors() []core.ValidationError { return e.Errors }

// ProposeDefinitionChange stores a proposed version of the managed definition
// of the factory of a run. It merges the changes
// onto the active version, runs the compiler and the preflight, stores the
// version with the state "proposed" and creates an inbox item of the kind
// review with the diff against the base version.
func (s *Service) ProposeDefinitionChange(ctx context.Context, req ProposalRequest) (*core.DefinitionVersion, error) {
	run, err := s.opts.Store.GetRun(ctx, req.RunID)
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", req.RunID, err)
	}
	if core.IsTrialRun(run) {
		return nil, fmt.Errorf("%w: a benchmark trial cannot propose a definition change", core.ErrInvalid)
	}
	if strings.TrimSpace(req.Message) == "" {
		return nil, fmt.Errorf("%w: message is required", core.ErrInvalid)
	}
	if len(req.Files) == 0 && len(req.Delete) == 0 {
		return nil, fmt.Errorf("%w: files or delete must name at least one file", core.ErrInvalid)
	}
	if len(req.Files)+len(req.Delete) > maxProposalFiles {
		return nil, fmt.Errorf("%w: a proposal can change at most %d files", core.ErrInvalid, maxProposalFiles)
	}
	unlock := s.lockKey("proposal:" + run.FactoryID)
	defer unlock()
	f, err := s.opts.Store.GetFactory(ctx, run.FactoryID)
	if err != nil {
		return nil, fmt.Errorf("get factory %s: %w", run.FactoryID, err)
	}
	if f.Source == core.DefinitionGit {
		return nil, fmt.Errorf("%w: the definition of this factory is in git: open a pull request against %s instead", core.ErrInvalid, f.GitRepo)
	}
	var base map[string]string
	if f.ActiveVersionID != "" {
		v, err := s.opts.Store.GetDefinitionVersion(ctx, f.ActiveVersionID)
		if err != nil {
			return nil, fmt.Errorf("get definition version %s: %w", f.ActiveVersionID, err)
		}
		base = v.Files
	}
	root := definitionRoot(base)
	files := make(map[string]string, len(base)+len(req.Files))
	for p, c := range base {
		files[p] = c
	}
	size := 0
	for p, c := range req.Files {
		clean, err := cleanDefinitionPath(p)
		if err != nil {
			return nil, err
		}
		size += len(c)
		files[root+clean] = c
	}
	if size > maxProposalBytes {
		return nil, fmt.Errorf("%w: the changed files are larger than %d bytes", core.ErrInvalid, maxProposalBytes)
	}
	for _, p := range req.Delete {
		clean, err := cleanDefinitionPath(p)
		if err != nil {
			return nil, err
		}
		if _, ok := files[root+clean]; !ok {
			return nil, fmt.Errorf("%w: cannot delete %s: the file does not exist", core.ErrInvalid, clean)
		}
		delete(files, root+clean)
	}
	diff := DiffFiles(base, files)
	if diff == "" {
		return nil, fmt.Errorf("%w: the proposal does not change the definition", core.ErrInvalid)
	}
	if err := s.checkProposal(ctx, f.ID, files); err != nil {
		return nil, err
	}
	now := s.now()
	v := &core.DefinitionVersion{
		ID:            core.NewID("defv"),
		FactoryID:     f.ID,
		Files:         files,
		Author:        "run:" + run.ID,
		Message:       strings.TrimSpace(req.Message),
		Valid:         true,
		CreatedAt:     now,
		State:         core.DefinitionProposed,
		BaseVersionID: f.ActiveVersionID,
		AuthorRunID:   run.ID,
	}
	item := &core.InboxItem{
		ID:         core.NewID("inbox"),
		FactoryID:  f.ID,
		WorkItemID: run.WorkItemID,
		RunID:      run.ID,
		Kind:       core.InboxReview,
		Title:      "Review definition change: " + truncate(oneLine(v.Message), 120),
		Body:       proposalBody(v, diff),
		Options:    []string{"approve", "reject"},
		CreatedAt:  now,
	}
	if run.Metadata[MetaSelfImprovement] == "true" {
		// The reviewer is the one that was chosen when the run started, so
		// that all review items of the run go to one person.
		if track, _ := s.loadImprovementTrack(ctx, run.WorkItemID); track != nil && run.WorkItemID != "" {
			item.AssigneeID = track.ReviewerID
		} else {
			_, _, def, _ := s.activeDefinition(ctx, f.ID)
			if assignee, err := s.pickReviewer(ctx, f.TeamID, s.improvementSettings(def)); err == nil {
				item.AssigneeID = assignee
			}
		}
	}
	v.InboxItemID = item.ID
	if err := s.opts.Store.CreateDefinitionVersion(ctx, v); err != nil {
		return nil, fmt.Errorf("store proposed version: %w", err)
	}
	if err := s.opts.Store.CreateInboxItem(ctx, item); err != nil {
		return nil, fmt.Errorf("create review inbox item: %w", err)
	}
	if s.opts.Bus != nil {
		s.opts.Bus.Publish(ctx, core.TopicInbox, item)
	}
	s.log.Info("definition change proposed", "factory_id", f.ID, "version_id", v.ID, "run_id", run.ID)
	return v, nil
}

func (s *Service) checkProposal(ctx context.Context, factoryID string, files map[string]string) error {
	if s.opts.CheckDefinition != nil {
		chk, err := s.opts.CheckDefinition(ctx, factoryID, files)
		if err != nil {
			return err
		}
		if !chk.Valid() {
			return &ProposalError{Errors: chk.Errors}
		}
		return nil
	}
	if s.opts.Compile == nil {
		return errors.New("scoring: CheckDefinition or Compile is required for proposals")
	}
	if def, verrs := s.opts.Compile(files); def == nil || len(verrs) > 0 {
		if len(verrs) == 0 {
			verrs = []core.ValidationError{{File: "factory.yaml", Message: "the definition does not compile"}}
		}
		return &ProposalError{Errors: verrs}
	}
	return nil
}

func cleanDefinitionPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	clean := path.Clean("/" + p)[1:]
	if clean == "" || clean != strings.TrimPrefix(p, "./") || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q is not a valid path in the definition", core.ErrInvalid, p)
	}
	return clean, nil
}

func proposalBody(v *core.DefinitionVersion, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run %s proposes a change to the factory definition.\n\n", v.AuthorRunID)
	fmt.Fprintf(&b, "%s\n\n", v.Message)
	fmt.Fprintf(&b, "Proposed version: `%s`. Base version: `%s`. Approve applies the version. Reject sends your comment to the run.\n\n", v.ID, v.BaseVersionID)
	b.WriteString("```diff\n")
	if len(diff) > maxDiffBodyBytes {
		b.WriteString(truncate(diff, maxDiffBodyBytes))
		b.WriteString("\n")
	} else {
		b.WriteString(diff)
	}
	b.WriteString("```\n")
	return b.String()
}

// ListProposals returns the proposed versions of a factory, newest first.
// With all=false only the open proposals are returned.
func (s *Service) ListProposals(ctx context.Context, factoryID string, all bool) ([]core.DefinitionVersion, error) {
	var out []core.DefinitionVersion
	opts := core.ListOpts{Limit: pageSize}
	for {
		page, err := s.opts.Store.ListDefinitionVersions(ctx, factoryID, opts)
		if err != nil {
			return nil, err
		}
		for _, v := range page {
			if !isProposalState(v.State) {
				continue
			}
			if !all && v.State != core.DefinitionProposed {
				continue
			}
			out = append(out, v)
		}
		if len(page) < pageSize {
			break
		}
		last := page[len(page)-1]
		next := storeutil.EncodeCursor(last.CreatedAt, last.ID)
		if next == opts.Cursor {
			break
		}
		opts.Cursor = next
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// isProposalState reports whether a version with this state is a proposal:
// it is open, or it was approved or rejected. The other versions are applied
// versions of the definition.
func isProposalState(st string) bool {
	return st == core.DefinitionProposed || st == core.DefinitionApproved || st == core.DefinitionRejected
}

// ProposalForInboxItem returns the open proposal of a review inbox item, or
// nil when the item belongs to no open proposal.
func (s *Service) ProposalForInboxItem(ctx context.Context, factoryID, inboxID string) (*core.DefinitionVersion, error) {
	open, err := s.ListProposals(ctx, factoryID, false)
	if err != nil {
		return nil, err
	}
	for i := range open {
		if open[i].InboxItemID == inboxID {
			return &open[i], nil
		}
	}
	return nil, nil
}

// getProposal returns a proposed version of a factory.
func (s *Service) getProposal(ctx context.Context, factoryID, id string) (*core.DefinitionVersion, error) {
	v, err := s.opts.Store.GetDefinitionVersion(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.FactoryID != factoryID || !isProposalState(v.State) {
		return nil, fmt.Errorf("proposal %s: %w", id, core.ErrNotFound)
	}
	return v, nil
}

// ApproveProposal applies a proposed version. It fails with ErrConflict when
// the base version of the proposal is not the active version.
func (s *Service) ApproveProposal(ctx context.Context, factoryID, id, actor string) (*core.DefinitionVersion, error) {
	if s.opts.Engine == nil {
		return nil, errors.New("scoring: Engine is required to apply a proposal")
	}
	unlock := s.lockKey("proposal:" + factoryID)
	defer unlock()
	v, err := s.getProposal(ctx, factoryID, id)
	if err != nil {
		return nil, err
	}
	if v.State != core.DefinitionProposed {
		return nil, fmt.Errorf("%w: proposal %s is %s", core.ErrConflict, id, v.State)
	}
	f, err := s.opts.Store.GetFactory(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	if f.ActiveVersionID != v.BaseVersionID {
		return nil, fmt.Errorf("%w: the active definition changed after the proposal was made (base %s, active %s); ask the agent to propose the change again", core.ErrConflict, v.BaseVersionID, f.ActiveVersionID)
	}
	msg := v.Message
	if actor != "" {
		msg += " (proposed by run " + v.AuthorRunID + ", approved by " + actor + ")"
	}
	applied, err := s.opts.Engine.ApplyDefinition(ctx, factoryID, v.Files, actor, msg, "")
	if err != nil {
		return nil, err
	}
	now := s.now()
	v.State = core.DefinitionApproved
	v.AppliedVersionID = applied.ID
	v.ReviewedBy = actor
	v.ReviewedAt = &now
	if err := s.opts.Store.UpdateDefinitionVersion(ctx, v); err != nil {
		return nil, fmt.Errorf("update proposal: %w", err)
	}
	s.resolveProposalItem(ctx, v, fmt.Sprintf("approved: the change is applied as version %s", applied.ID), actor)
	s.log.Info("definition proposal approved", "factory_id", factoryID, "proposal_id", v.ID, "version_id", applied.ID, "actor", actor)
	return v, nil
}

// RejectProposal rejects a proposed version. The comment goes to the author
// run as a user message.
func (s *Service) RejectProposal(ctx context.Context, factoryID, id, actor, comment string) (*core.DefinitionVersion, error) {
	unlock := s.lockKey("proposal:" + factoryID)
	defer unlock()
	v, err := s.getProposal(ctx, factoryID, id)
	if err != nil {
		return nil, err
	}
	if v.State != core.DefinitionProposed {
		return nil, fmt.Errorf("%w: proposal %s is %s", core.ErrConflict, id, v.State)
	}
	now := s.now()
	v.State = core.DefinitionRejected
	v.ReviewedBy = actor
	v.ReviewComment = strings.TrimSpace(comment)
	v.ReviewedAt = &now
	if err := s.opts.Store.UpdateDefinitionVersion(ctx, v); err != nil {
		return nil, fmt.Errorf("update proposal: %w", err)
	}
	res := "rejected"
	if v.ReviewComment != "" {
		res += ": " + v.ReviewComment
	}
	if !s.resolveProposalItem(ctx, v, res, actor) && s.opts.Engine != nil && v.AuthorRunID != "" {
		// The item was resolved already: send the comment to the run directly.
		msg := fmt.Sprintf("[human response] Your proposed definition change %s was %s", v.ID, res)
		if err := s.opts.Engine.FollowUp(ctx, v.AuthorRunID, msg, actor); err != nil {
			s.log.Warn("send proposal rejection", "run_id", v.AuthorRunID, "err", err)
		}
	}
	s.log.Info("definition proposal rejected", "factory_id", factoryID, "proposal_id", v.ID, "actor", actor)
	return v, nil
}

// resolveProposalItem resolves the review item of a proposal. The engine
// sends the resolution to the author run. It reports whether it resolved it.
func (s *Service) resolveProposalItem(ctx context.Context, v *core.DefinitionVersion, resolution, actor string) bool {
	if v.InboxItemID == "" || s.opts.Engine == nil {
		return false
	}
	if err := s.opts.Engine.ResolveInbox(ctx, v.InboxItemID, resolution, actor); err != nil {
		if !errors.Is(err, core.ErrConflict) && !errors.Is(err, core.ErrNotFound) {
			s.log.Warn("resolve proposal inbox item", "inbox_id", v.InboxItemID, "err", err)
		}
		return false
	}
	return true
}

// ---------- Diff ----------

// DiffFiles returns a unified diff of two definition trees, file by file.
func DiffFiles(old, new map[string]string) string {
	paths := map[string]bool{}
	for p := range old {
		paths[p] = true
	}
	for p := range new {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var b strings.Builder
	for _, p := range sorted {
		o, inOld := old[p]
		n, inNew := new[p]
		if inOld && inNew && o == n {
			continue
		}
		from, to := "a/"+p, "b/"+p
		if !inOld {
			from = "/dev/null"
		}
		if !inNew {
			to = "/dev/null"
		}
		fmt.Fprintf(&b, "--- %s\n+++ %s\n", from, to)
		b.WriteString(lineDiff(splitLines(o), splitLines(n)))
	}
	return b.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffContext is the number of unchanged lines around a change in a diff.
const diffContext = 3

// lineDiff returns the hunks of a diff of two line lists, with diffContext
// unchanged lines around each change. It uses the longest common
// subsequence. Very long files are shown as a removal and an addition.
func lineDiff(a, b []string) string {
	ops := lineOps(a, b)
	// Find the lines to show: each change and the context around it.
	show := make([]bool, len(ops))
	for i, op := range ops {
		if op[0] == ' ' {
			continue
		}
		for j := max(0, i-diffContext); j <= min(len(ops)-1, i+diffContext); j++ {
			show[j] = true
		}
	}
	var out strings.Builder
	oldLine, newLine := 1, 1
	for i := 0; i < len(ops); {
		if !show[i] {
			if ops[i][0] != '+' {
				oldLine++
			}
			if ops[i][0] != '-' {
				newLine++
			}
			i++
			continue
		}
		j := i
		oldN, newN := 0, 0
		for j < len(ops) && show[j] {
			if ops[j][0] != '+' {
				oldN++
			}
			if ops[j][0] != '-' {
				newN++
			}
			j++
		}
		oldStart, newStart := oldLine, newLine
		if oldN == 0 {
			oldStart-- // an empty range starts at the line before
		}
		if newN == 0 {
			newStart--
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oldStart, oldN, newStart, newN)
		for _, op := range ops[i:j] {
			out.WriteString(op + "\n")
		}
		oldLine += oldN
		newLine += newN
		i = j
	}
	return out.String()
}

// lineOps returns the edit script of two line lists: each entry is a line
// with the prefix " " (kept), "-" (removed) or "+" (added).
func lineOps(a, b []string) []string {
	if len(a)*len(b) > maxDiffLinesPerHun*maxDiffLinesPerHun/4 {
		ops := make([]string, 0, len(a)+len(b))
		for _, l := range a {
			ops = append(ops, "-"+l)
		}
		for _, l := range b {
			ops = append(ops, "+"+l)
		}
		return ops
	}
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	ops := make([]string, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, " "+a[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, "-"+a[i])
			i++
		default:
			ops = append(ops, "+"+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, "-"+a[i])
	}
	for ; j < m; j++ {
		ops = append(ops, "+"+b[j])
	}
	return ops
}

// DiffProposal returns the diff of a proposed version against its base version.
func (s *Service) DiffProposal(ctx context.Context, v *core.DefinitionVersion) (string, error) {
	var base map[string]string
	if v.BaseVersionID != "" {
		b, err := s.opts.Store.GetDefinitionVersion(ctx, v.BaseVersionID)
		if err != nil {
			return "", err
		}
		base = b.Files
	}
	return DiffFiles(base, v.Files), nil
}
