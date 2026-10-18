package jira

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The write-back of the tracker (core.IssueTracker).

var _ core.IssueTracker = (*Handler)(nil)

type jiraTransition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	To   struct {
		Name           string `json:"name"`
		StatusCategory struct {
			Key string `json:"key"` // new, indeterminate, done
		} `json:"statusCategory"`
	} `json:"to"`
}

// pickTransition chooses the transition for a generic issue state. It
// returns "" when no transition fits.
func pickTransition(ts []jiraTransition, state string) string {
	byCategory := func(cat string, prefer ...string) string {
		first := ""
		for _, t := range ts {
			if t.To.StatusCategory.Key != cat {
				continue
			}
			for _, p := range prefer {
				if strings.Contains(strings.ToLower(t.To.Name), p) || strings.Contains(strings.ToLower(t.Name), p) {
					return t.ID
				}
			}
			if first == "" {
				first = t.ID
			}
		}
		return first
	}
	switch state {
	case core.IssueStateDone:
		return byCategory("done", "done", "closed", "resolved")
	case core.IssueStateInProgress:
		return byCategory("indeterminate", "progress")
	case core.IssueStateInReview:
		for _, t := range ts {
			if t.To.StatusCategory.Key == "indeterminate" && strings.Contains(strings.ToLower(t.To.Name), "review") {
				return t.ID
			}
		}
	}
	return ""
}

// SetIssueState implements core.IssueTracker. It runs the first transition of
// the issue that leads to a status of the matching category. A workflow
// without a review status leaves the issue as it is for IssueStateInReview.
func (h *Handler) SetIssueState(ctx context.Context, teamID string, issue core.SourceRef, state string) error {
	key := issue.ExternalID
	if key == "" {
		return errors.New("jira: issue has no key")
	}
	in, err := h.integrationFor(ctx, teamID, issue)
	if err != nil {
		return err
	}
	c, err := h.apiClient(ctx, in)
	if err != nil {
		return err
	}
	var out struct {
		Transitions []jiraTransition `json:"transitions"`
	}
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "/transitions"
	if err := c.Do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return fmt.Errorf("jira: list transitions of %s: %w", key, err)
	}
	id := pickTransition(out.Transitions, state)
	if id == "" {
		return nil
	}
	if err := c.Do(ctx, http.MethodPost, path, map[string]any{"transition": map[string]string{"id": id}}, nil); err != nil {
		return fmt.Errorf("jira: transition %s to %s: %w", key, state, err)
	}
	return nil
}

// AttachPullRequest implements core.IssueTracker with a remote link.
func (h *Handler) AttachPullRequest(ctx context.Context, teamID string, issue core.SourceRef, pr core.PullRequestRef) error {
	key := issue.ExternalID
	if key == "" {
		return errors.New("jira: issue has no key")
	}
	if pr.URL == "" {
		return fmt.Errorf("%w: jira: the pull request has no URL", core.ErrInvalid)
	}
	in, err := h.integrationFor(ctx, teamID, issue)
	if err != nil {
		return err
	}
	c, err := h.apiClient(ctx, in)
	if err != nil {
		return err
	}
	title := fmt.Sprintf("Pull request %s#%d", pr.Repo, pr.Number)
	body := map[string]any{
		// The global ID makes a second call update the link and not add one.
		"globalId": "openfactory:pr:" + pr.URL,
		"object":   map[string]any{"url": pr.URL, "title": title},
	}
	if err := c.Do(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/remotelink", body, nil); err != nil {
		return fmt.Errorf("jira: link pull request to %s: %w", key, err)
	}
	return nil
}
