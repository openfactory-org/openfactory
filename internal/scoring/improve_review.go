package scoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The review of a Self-improvement pull request. The review item is not
// created when the run starts, because no change exists then. It is created
// when the run has linked a pull request to its work item. At that time the
// service also asks the chosen reviewer for a review on the pull request, if
// the forge can do it.

// improvementTrack is what the service remembers about a Self-improvement run
// until its pull requests are reviewed.
type improvementTrack struct {
	// ReviewerID is the reviewer that was chosen from the pool when the run started.
	ReviewerID string `json:"reviewer_id,omitempty"`
	// RunID is the Self-improvement run (the root run of the work item).
	RunID string `json:"run_id"`
	Agent string `json:"agent"`
	// Regressions is the section of the failing runs, for the review item.
	Regressions string `json:"regressions,omitempty"`
}

func improveTrackKey(workItemID string) string { return "scoring:improve:wi:" + workItemID }

func improvePRKey(workItemID string, pr core.PullRequestRef) string {
	id := pr.URL
	if pr.Repo != "" && pr.Number != 0 {
		id = fmt.Sprintf("%s#%d", strings.ToLower(pr.Repo), pr.Number)
	}
	return "scoring:improve:pr:" + workItemID + ":" + id
}

func (s *Service) saveImprovementTrack(ctx context.Context, workItemID string, t improvementTrack) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.opts.Store.KVSet(ctx, improveTrackKey(workItemID), string(b))
}

func (s *Service) loadImprovementTrack(ctx context.Context, workItemID string) (*improvementTrack, error) {
	v, err := s.opts.Store.KVGet(ctx, improveTrackKey(workItemID))
	if errors.Is(err, core.ErrNotFound) || (err == nil && v == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t improvementTrack
	if err := json.Unmarshal([]byte(v), &t); err != nil {
		return nil, fmt.Errorf("decode improvement track of %s: %w", workItemID, err)
	}
	return &t, nil
}

// reviewImprovementPRs creates the review item of each open pull request that
// a Self-improvement run linked to its work item, and asks the reviewer for a
// review on the forge. It runs when a run of the work item updates, so it
// finds the pull request when the agent finishes its turn.
func (s *Service) reviewImprovementPRs(ctx context.Context, run *core.Run) error {
	if run.WorkItemID == "" {
		return nil
	}
	track, err := s.loadImprovementTrack(ctx, run.WorkItemID)
	if err != nil || track == nil {
		return err
	}
	unlock := s.lockKey("improve-review:" + run.WorkItemID)
	defer unlock()
	wi, err := s.opts.Store.GetWorkItem(ctx, run.WorkItemID)
	if err != nil {
		return fmt.Errorf("get work item %s: %w", run.WorkItemID, err)
	}
	f, err := s.opts.Store.GetFactory(ctx, wi.FactoryID)
	if err != nil {
		return fmt.Errorf("get factory %s: %w", wi.FactoryID, err)
	}
	var errs []error
	for _, pr := range wi.PullRequests {
		if pr.State == "merged" || pr.State == "closed" {
			continue
		}
		key := improvePRKey(wi.ID, pr)
		if v, err := s.opts.Store.KVGet(ctx, key); err == nil && v != "" {
			continue
		} else if err != nil && !errors.Is(err, core.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		if err := s.opts.Store.KVSet(ctx, key, "1"); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := s.createPRReviewItem(ctx, f, wi, track, pr); err != nil {
			_ = s.opts.Store.KVSet(ctx, key, "")
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) createPRReviewItem(ctx context.Context, f *core.Factory, wi *core.WorkItem, track *improvementTrack, pr core.PullRequestRef) error {
	ref := pr.URL
	if pr.Repo != "" && pr.Number != 0 {
		ref = fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
	}
	note := "No reviewer was chosen: reviewerType is none, or no team member is eligible."
	if track.ReviewerID != "" {
		note = "The item is for the reviewer that was chosen from the pool."
		if s.opts.RequestReview == nil {
			note += " This server does not request reviews on the forge."
		} else if err := s.opts.RequestReview(ctx, f.TeamID, pr, track.ReviewerID); err != nil {
			s.log.Warn("self-improvement: review request failed", "factory_id", f.ID, "pull_request", ref, "reviewer", track.ReviewerID, "err", err)
			note += " A review could not be requested on the forge (" + oneLine(err.Error()) + "). Add the reviewer on the pull request by hand."
		} else {
			note += " A review is requested from the reviewer on the pull request."
		}
	}
	var body strings.Builder
	fmt.Fprintf(&body, "A Self-improvement run for agent **%s** opened the pull request [%s](%s). ", track.Agent, ref, pr.URL)
	body.WriteString("Review it with the same standards as a human-authored change. Nothing is adopted without review.\n\n")
	body.WriteString(note + "\n\n")
	body.WriteString(track.Regressions)
	item := &core.InboxItem{
		ID:         core.NewID("inbox"),
		FactoryID:  f.ID,
		WorkItemID: wi.ID,
		RunID:      track.RunID,
		Kind:       core.InboxReview,
		Title:      "Review self-improvement pull request " + ref,
		Body:       body.String(),
		AssigneeID: track.ReviewerID,
		CreatedAt:  s.now(),
	}
	if err := s.opts.Store.CreateInboxItem(ctx, item); err != nil {
		return fmt.Errorf("create review inbox item: %w", err)
	}
	if s.opts.Bus != nil {
		s.opts.Bus.Publish(ctx, core.TopicInbox, item)
	}
	return nil
}
