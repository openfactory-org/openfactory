package azuredevops

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The review operations of core.Forge. A thread
// reference is the ID of a pull request comment thread.

func threadID(threadRef string) (int, error) {
	id, err := strconv.Atoi(strings.TrimSpace(threadRef))
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: azuredevops: thread reference %q is not a thread ID", core.ErrInvalid, threadRef)
	}
	return id, nil
}

// ReplyInThread implements core.Forge.
func (c *Client) ReplyInThread(ctx context.Context, project, repo string, number int, threadRef, body string) error {
	id, err := threadID(threadRef)
	if err != nil {
		return err
	}
	path := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads/%d/comments", repoAPI(project, repo), number, id), nil)
	payload := map[string]any{"parentCommentId": 1, "content": body, "commentType": 1}
	if err := c.api.Do(ctx, http.MethodPost, path, payload, nil); err != nil {
		return fmt.Errorf("azuredevops: reply in thread %d of pull request %d: %w", id, number, err)
	}
	return nil
}

// ResolveThread implements core.Forge. It sets the thread status to "fixed".
func (c *Client) ResolveThread(ctx context.Context, project, repo string, number int, threadRef string) error {
	id, err := threadID(threadRef)
	if err != nil {
		return err
	}
	path := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads/%d", repoAPI(project, repo), number, id), nil)
	if err := c.api.Do(ctx, http.MethodPatch, path, map[string]any{"status": "fixed"}, nil); err != nil {
		return fmt.Errorf("azuredevops: resolve thread %d of pull request %d: %w", id, number, err)
	}
	return nil
}

// SubmitReview implements core.Forge for comment reviews. Each inline comment
// becomes a thread on its file. A vote needs the reviewer identity of the
// credential, so approve and request_changes are not supported.
func (c *Client) SubmitReview(ctx context.Context, in core.ReviewInput) error {
	switch strings.ToLower(in.Event) {
	case "", core.ReviewEventComment:
	case core.ReviewEventApprove, core.ReviewEventRequestChanges:
		return fmt.Errorf("azuredevops: review vote: %w", core.ErrOperationNotSupported)
	default:
		return fmt.Errorf("%w: unknown review event %q", core.ErrInvalid, in.Event)
	}
	path := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads", repoAPI(in.Owner, in.Repo), in.Number), nil)
	for _, cm := range in.Comments {
		if cm.Path == "" || cm.Line <= 0 || strings.TrimSpace(cm.Body) == "" {
			return fmt.Errorf("%w: an inline comment needs a path, a line and a body", core.ErrInvalid)
		}
		file := cm.Path
		if !strings.HasPrefix(file, "/") {
			file = "/" + file
		}
		pos := map[string]any{"line": cm.Line, "offset": 1}
		ctxKey, endKey := "rightFileStart", "rightFileEnd"
		if strings.EqualFold(cm.Side, "LEFT") {
			ctxKey, endKey = "leftFileStart", "leftFileEnd"
		}
		payload := map[string]any{
			"comments":      []map[string]any{{"parentCommentId": 0, "content": cm.Body, "commentType": 1}},
			"status":        1,
			"threadContext": map[string]any{"filePath": file, ctxKey: pos, endKey: pos},
		}
		if err := c.api.Do(ctx, http.MethodPost, path, payload, nil); err != nil {
			return fmt.Errorf("azuredevops: inline comment on %s:%d of pull request %d: %w", cm.Path, cm.Line, in.Number, err)
		}
	}
	if strings.TrimSpace(in.Body) != "" {
		return c.Comment(ctx, in.Owner, in.Repo, in.Number, in.Body)
	}
	return nil
}

// MarkReady implements core.Forge.
func (c *Client) MarkReady(ctx context.Context, project, repo string, number int) error {
	path := withVersion(fmt.Sprintf("%s/pullrequests/%d", repoAPI(project, repo), number), nil)
	if err := c.api.Do(ctx, http.MethodPatch, path, map[string]any{"isDraft": false}, nil); err != nil {
		return fmt.Errorf("azuredevops: mark pull request %d ready: %w", number, err)
	}
	return nil
}
