package azuredevops

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// The tracker write-back for work items. The engine
// uses generic states. Each work item type has its own list of states, and
// each state has a category: Proposed, InProgress, Resolved, Completed or
// Removed. The client maps the generic state to a state of the type of the
// work item.

type workItem struct {
	Fields    map[string]any `json:"fields"`
	Relations []struct {
		Rel string `json:"rel"`
		URL string `json:"url"`
	} `json:"relations"`
}

func (w workItem) field(name string) string {
	s, _ := w.Fields[name].(string)
	return s
}

type typeState struct {
	Name     string `json:"name"`
	Category string `json:"category"`
}

func workItemPath(project string, id int, q url.Values) string {
	return withVersion(fmt.Sprintf("/%s/_apis/wit/workitems/%d", url.PathEscape(project), id), q)
}

func (c *Client) getWorkItem(ctx context.Context, project string, id int, expand bool) (*workItem, error) {
	var q url.Values
	if expand {
		q = url.Values{"$expand": {"relations"}}
	}
	var w workItem
	if err := c.api.Do(ctx, http.MethodGet, workItemPath(project, id, q), nil, &w); err != nil {
		return nil, fmt.Errorf("azuredevops: get work item %d: %w", id, err)
	}
	return &w, nil
}

func (c *Client) patchWorkItem(ctx context.Context, project string, id int, ops []map[string]any) error {
	if err := c.api.Do(ctx, http.MethodPatch, workItemPath(project, id, nil), ops, nil); err != nil {
		return fmt.Errorf("azuredevops: update work item %d: %w", id, err)
	}
	return nil
}

// pickState returns the state to set for a generic state, given the states
// of the work item type and the current state. It returns "" when the work
// item stays as it is.
func pickState(state string, states []typeState, current string) string {
	category := func(name string) string {
		for _, s := range states {
			if strings.EqualFold(s.Name, name) {
				return s.Category
			}
		}
		return ""
	}
	first := func(match func(typeState) bool) string {
		for _, s := range states {
			if match(s) {
				return s.Name
			}
		}
		return ""
	}
	cur := category(current)
	finished := cur == "Completed" || cur == "Removed"
	var target string
	switch state {
	case core.IssueStateDone:
		if cur == "Completed" || cur == "Removed" {
			return ""
		}
		target = first(func(s typeState) bool { return s.Category == "Completed" })
	case core.IssueStateInReview:
		if finished {
			return ""
		}
		target = first(func(s typeState) bool {
			return (s.Category == "InProgress" || s.Category == "Resolved") && strings.Contains(strings.ToLower(s.Name), "review")
		})
		if target == "" && cur == "Proposed" {
			// No review state: the work item is at least in progress.
			target = first(func(s typeState) bool { return s.Category == "InProgress" })
		}
	case core.IssueStateInProgress:
		if cur != "Proposed" {
			return "" // it is already in progress, or further
		}
		target = first(func(s typeState) bool { return s.Category == "InProgress" })
	}
	if target == "" || strings.EqualFold(target, current) {
		return ""
	}
	return target
}

// SetWorkItemState moves a work item to a generic state (in_progress,
// in_review or done). A workflow without a review state leaves the work item
// in progress.
func (c *Client) SetWorkItemState(ctx context.Context, project string, id int, state string) error {
	switch state {
	case core.IssueStateInProgress, core.IssueStateInReview, core.IssueStateDone:
	default:
		return fmt.Errorf("%w: azuredevops: unknown work item state %q", core.ErrInvalid, state)
	}
	w, err := c.getWorkItem(ctx, project, id, false)
	if err != nil {
		return err
	}
	typ, current := w.field("System.WorkItemType"), w.field("System.State")
	if typ == "" {
		return fmt.Errorf("azuredevops: work item %d has no type", id)
	}
	var list struct {
		Value []typeState `json:"value"`
	}
	path := withVersion(fmt.Sprintf("/%s/_apis/wit/workitemtypes/%s/states", url.PathEscape(project), url.PathEscape(typ)), nil)
	if err := c.api.Do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return fmt.Errorf("azuredevops: list states of %s: %w", typ, err)
	}
	target := pickState(state, list.Value, current)
	if target == "" {
		return nil
	}
	return c.patchWorkItem(ctx, project, id, []map[string]any{{"op": "add", "path": "/fields/System.State", "value": target}})
}

// AttachPullRequestToWorkItem adds an artifact link from the work item to a
// pull request. It does nothing when the link exists.
func (c *Client) AttachPullRequestToWorkItem(ctx context.Context, project string, id int, pr core.PullRequestRef) error {
	repoProject, repo := forgeutil.SplitRepo(pr.Repo)
	if repoProject == "" {
		repoProject = project
	}
	if repo == "" || pr.Number == 0 {
		return fmt.Errorf("%w: azuredevops: a pull request needs a repository and a number", core.ErrInvalid)
	}
	var r struct {
		ID      string `json:"id"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := c.api.Do(ctx, http.MethodGet, withVersion(repoAPI(repoProject, repo), nil), nil, &r); err != nil {
		return fmt.Errorf("azuredevops: get repository %s: %w", pr.Repo, err)
	}
	if r.ID == "" || r.Project.ID == "" {
		return fmt.Errorf("azuredevops: repository %s has no ID", pr.Repo)
	}
	// vstfs:///Git/PullRequestId/{project ID}%2F{repository ID}%2F{number}
	link := fmt.Sprintf("vstfs:///Git/PullRequestId/%s%%2F%s%%2F%d", r.Project.ID, r.ID, pr.Number)
	w, err := c.getWorkItem(ctx, project, id, true)
	if err != nil {
		return err
	}
	for _, rel := range w.Relations {
		if rel.Rel == "ArtifactLink" && strings.EqualFold(rel.URL, link) {
			return nil
		}
	}
	return c.patchWorkItem(ctx, project, id, []map[string]any{{
		"op": "add", "path": "/relations/-",
		"value": map[string]any{"rel": "ArtifactLink", "url": link, "attributes": map[string]string{"name": "Pull Request"}},
	}})
}
