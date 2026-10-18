package linear

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// The write-back of the tracker (core.IssueTracker).

var _ core.IssueTracker = (*Handler)(nil)

// teamClient returns a client for the Linear record of a team that serves a
// source, with the selection rule: the records whose
// organization_id is the workspace of the source, the record of the factory
// in ctx (core.WithFactory) first, then the team record.
func (h *Handler) teamClient(ctx context.Context, teamID string, src core.SourceRef) (*Client, error) {
	in, err := integrations.SelectTeam(ctx, h.opts.Store, teamID, Provider, core.FactoryFrom(ctx), src.Workspace)
	if err != nil {
		return nil, err
	}
	return h.client(ctx, in)
}

// issueID returns the issue of a source: the UUID or the identifier.
func issueID(src core.SourceRef) string {
	if src.Channel != "" {
		return src.Channel
	}
	return src.ExternalID
}

// WorkflowState is a state of a Linear team.
type WorkflowState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Type     string  `json:"type"` // triage, backlog, unstarted, started, completed, canceled
	Position float64 `json:"position"`
}

// PickState chooses the workflow state for a generic issue state. It returns
// "" when no state fits, or when the issue is already in a fitting state.
func PickState(states []WorkflowState, current WorkflowState, state string) string {
	sorted := append([]WorkflowState(nil), states...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Position < sorted[j].Position })
	find := func(typ string, prefer string) string {
		first := ""
		for _, s := range sorted {
			if s.Type != typ {
				continue
			}
			if prefer != "" && strings.Contains(strings.ToLower(s.Name), prefer) {
				return s.ID
			}
			if first == "" {
				first = s.ID
			}
		}
		return first
	}
	var want string
	switch state {
	case core.IssueStateDone:
		if current.Type == "completed" {
			return ""
		}
		want = find("completed", "done")
	case core.IssueStateInProgress:
		if current.Type == "started" || current.Type == "completed" {
			return ""
		}
		want = find("started", "progress")
	case core.IssueStateInReview:
		if current.Type == "completed" || strings.Contains(strings.ToLower(current.Name), "review") {
			return ""
		}
		for _, s := range sorted {
			if s.Type == "started" && strings.Contains(strings.ToLower(s.Name), "review") {
				want = s.ID
				break
			}
		}
		if want == "" && current.Type != "started" {
			want = find("started", "progress")
		}
	}
	if want == current.ID {
		return ""
	}
	return want
}

// SetIssueState implements core.IssueTracker. It moves the issue to the
// first state of the matching type of its team.
func (h *Handler) SetIssueState(ctx context.Context, teamID string, issue core.SourceRef, state string) error {
	id := issueID(issue)
	if id == "" {
		return errors.New("linear: issue has no ID")
	}
	c, err := h.teamClient(ctx, teamID, issue)
	if err != nil {
		return err
	}
	var out struct {
		Issue struct {
			ID    string        `json:"id"`
			State WorkflowState `json:"state"`
			Team  struct {
				States struct {
					Nodes []WorkflowState `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	const q = `query IssueStates($id: String!) { issue(id: $id) { id state { id name type position } team { states { nodes { id name type position } } } } }`
	if err := c.Query(ctx, q, map[string]any{"id": id}, &out); err != nil {
		return fmt.Errorf("linear: read states of %s: %w", id, err)
	}
	stateID := PickState(out.Issue.Team.States.Nodes, out.Issue.State, state)
	if stateID == "" {
		return nil
	}
	var res struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	const m = `mutation IssueUpdate($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
	if err := c.Query(ctx, m, map[string]any{"id": out.Issue.ID, "input": map[string]any{"stateId": stateID}}, &res); err != nil {
		return fmt.Errorf("linear: set state of %s: %w", id, err)
	}
	if !res.IssueUpdate.Success {
		return errors.New("linear: issueUpdate was not successful")
	}
	return nil
}

// AttachPullRequest implements core.IssueTracker with an attachment. Linear
// keeps one attachment for each URL, so a second call updates it.
func (h *Handler) AttachPullRequest(ctx context.Context, teamID string, issue core.SourceRef, pr core.PullRequestRef) error {
	id := issueID(issue)
	if id == "" {
		return errors.New("linear: issue has no ID")
	}
	if pr.URL == "" {
		return fmt.Errorf("%w: linear: the pull request has no URL", core.ErrInvalid)
	}
	c, err := h.teamClient(ctx, teamID, issue)
	if err != nil {
		return err
	}
	var res struct {
		AttachmentCreate struct {
			Success bool `json:"success"`
		} `json:"attachmentCreate"`
	}
	title := fmt.Sprintf("Pull request %s#%d", pr.Repo, pr.Number)
	const m = `mutation AttachmentCreate($input: AttachmentCreateInput!) { attachmentCreate(input: $input) { success } }`
	input := map[string]any{"issueId": id, "url": pr.URL, "title": title}
	if pr.State != "" {
		input["subtitle"] = pr.State
	}
	if err := c.Query(ctx, m, map[string]any{"input": input}, &res); err != nil {
		return fmt.Errorf("linear: attach pull request to %s: %w", id, err)
	}
	if !res.AttachmentCreate.Success {
		return errors.New("linear: attachmentCreate was not successful")
	}
	return nil
}
