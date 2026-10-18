package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The review operations of core.Forge. A thread
// reference is the ID of a merge request discussion.

func discussionPath(owner, repo string, number int, discussionID string) string {
	return fmt.Sprintf("%s/merge_requests/%d/discussions/%s", projectPath(owner, repo), number, url.PathEscape(discussionID))
}

// ReplyInThread implements core.Forge.
func (c *Client) ReplyInThread(ctx context.Context, owner, repo string, number int, threadRef, body string) error {
	if strings.TrimSpace(threadRef) == "" {
		return fmt.Errorf("%w: gitlab: a discussion ID is required", core.ErrInvalid)
	}
	if err := c.ReplyDiscussion(ctx, owner, repo, number, threadRef, body); err != nil {
		return fmt.Errorf("gitlab: reply in discussion %s of !%d: %w", threadRef, number, err)
	}
	return nil
}

// ResolveThread implements core.Forge.
func (c *Client) ResolveThread(ctx context.Context, owner, repo string, number int, threadRef string) error {
	if strings.TrimSpace(threadRef) == "" {
		return fmt.Errorf("%w: gitlab: a discussion ID is required", core.ErrInvalid)
	}
	if err := c.api.Do(ctx, http.MethodPut, discussionPath(owner, repo, number, threadRef)+"?resolved=true", nil, nil); err != nil {
		return fmt.Errorf("gitlab: resolve discussion %s of !%d: %w", threadRef, number, err)
	}
	return nil
}

type mrDetails struct {
	Title    string `json:"title"`
	Draft    bool   `json:"draft"`
	DiffRefs struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

func (c *Client) mrDetails(ctx context.Context, owner, repo string, number int) (*mrDetails, error) {
	var mr mrDetails
	if err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", projectPath(owner, repo), number), nil, &mr); err != nil {
		return nil, err
	}
	return &mr, nil
}

// SubmitReview implements core.Forge. Each inline comment becomes a diff
// discussion, and the body becomes a note. An approval approves the merge
// request. GitLab has no "request changes" call in its API.
func (c *Client) SubmitReview(ctx context.Context, in core.ReviewInput) error {
	switch strings.ToLower(in.Event) {
	case "", core.ReviewEventComment, core.ReviewEventApprove:
	case core.ReviewEventRequestChanges:
		return fmt.Errorf("gitlab: request changes: %w", core.ErrOperationNotSupported)
	default:
		return fmt.Errorf("%w: unknown review event %q", core.ErrInvalid, in.Event)
	}
	if len(in.Comments) > 0 {
		mr, err := c.mrDetails(ctx, in.Owner, in.Repo, in.Number)
		if err != nil {
			return fmt.Errorf("gitlab: read !%d: %w", in.Number, err)
		}
		for _, cm := range in.Comments {
			if cm.Path == "" || cm.Line <= 0 || strings.TrimSpace(cm.Body) == "" {
				return fmt.Errorf("%w: an inline comment needs a path, a line and a body", core.ErrInvalid)
			}
			pos := map[string]any{
				"position_type": "text",
				"base_sha":      mr.DiffRefs.BaseSHA,
				"head_sha":      mr.DiffRefs.HeadSHA,
				"start_sha":     mr.DiffRefs.StartSHA,
				"new_path":      cm.Path,
				"old_path":      cm.Path,
			}
			if strings.EqualFold(cm.Side, "LEFT") {
				pos["old_line"] = cm.Line
			} else {
				pos["new_line"] = cm.Line
			}
			path := fmt.Sprintf("%s/merge_requests/%d/discussions", projectPath(in.Owner, in.Repo), in.Number)
			if err := c.api.Do(ctx, http.MethodPost, path, map[string]any{"body": cm.Body, "position": pos}, nil); err != nil {
				return fmt.Errorf("gitlab: inline comment on %s:%d of !%d: %w", cm.Path, cm.Line, in.Number, err)
			}
		}
	}
	if strings.TrimSpace(in.Body) != "" {
		if err := c.Comment(ctx, in.Owner, in.Repo, in.Number, in.Body); err != nil {
			return err
		}
	}
	if strings.EqualFold(in.Event, core.ReviewEventApprove) {
		if err := c.api.Do(ctx, http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/approve", projectPath(in.Owner, in.Repo), in.Number), nil, nil); err != nil {
			return fmt.Errorf("gitlab: approve !%d: %w", in.Number, err)
		}
	}
	return nil
}

// draftPrefixes are the title prefixes that make a merge request a draft.
var draftPrefixes = []string{"draft:", "[draft]", "(draft)", "wip:", "[wip]"}

// MarkReady implements core.Forge. GitLab keeps the draft state in the
// title, so it removes the draft prefix.
func (c *Client) MarkReady(ctx context.Context, owner, repo string, number int) error {
	mr, err := c.mrDetails(ctx, owner, repo, number)
	if err != nil {
		return fmt.Errorf("gitlab: read !%d: %w", number, err)
	}
	if !mr.Draft {
		return nil
	}
	title := strings.TrimSpace(mr.Title)
	for changed := true; changed; {
		changed = false
		for _, p := range draftPrefixes {
			if len(title) >= len(p) && strings.EqualFold(title[:len(p)], p) {
				title = strings.TrimSpace(title[len(p):])
				changed = true
			}
		}
	}
	if err := c.api.Do(ctx, http.MethodPut, fmt.Sprintf("%s/merge_requests/%d", projectPath(owner, repo), number), map[string]string{"title": title}, nil); err != nil {
		return fmt.Errorf("gitlab: mark !%d ready: %w", number, err)
	}
	return nil
}
