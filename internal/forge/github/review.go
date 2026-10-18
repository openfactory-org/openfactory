package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The review operations of core.Forge.

// commentID parses a thread reference: the ID of a review comment.
func commentID(threadRef string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(threadRef), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: github: thread reference %q is not a review comment ID", core.ErrInvalid, threadRef)
	}
	return id, nil
}

// ReplyInThread implements core.Forge. threadRef is the ID of a review
// comment of the thread. GitHub adds the reply to the thread of that comment.
func (c *Client) ReplyInThread(ctx context.Context, owner, repo string, number int, threadRef, body string) error {
	id, err := commentID(threadRef)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/pulls/%d/comments/%d/replies", repoPath(owner, repo), number, id)
	if _, err := c.do(ctx, owner, repo, http.MethodPost, path, map[string]string{"body": body}, nil); err != nil {
		return fmt.Errorf("github: reply in thread %s of %s/%s#%d: %w", threadRef, owner, repo, number, err)
	}
	return nil
}

// graphqlPath is the path of the GraphQL endpoint for the API base URL. On
// GitHub Enterprise Server the REST base is <host>/api/v3 and GraphQL is
// <host>/api/graphql.
func (c *Client) graphqlPath() string {
	base := strings.TrimRight(c.api.BaseURL, "/")
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/v3") + "/graphql"
	}
	return "/graphql"
}

type graphqlError struct {
	Message string `json:"message"`
}

func (c *Client) graphql(ctx context.Context, owner, repo, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   any            `json:"data"`
		Errors []graphqlError `json:"errors"`
	}
	resp.Data = out
	if _, err := c.do(ctx, owner, repo, http.MethodPost, c.graphqlPath(), map[string]any{"query": query, "variables": vars}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			msgs = append(msgs, e.Message)
		}
		return errors.New("github graphql: " + strings.Join(msgs, "; "))
	}
	return nil
}

const reviewThreadsQuery = `query($owner:String!,$repo:String!,$number:Int!,$after:String){
  repository(owner:$owner,name:$repo){pullRequest(number:$number){
    reviewThreads(first:100,after:$after){
      pageInfo{hasNextPage endCursor}
      nodes{id isResolved comments(first:100){nodes{databaseId}}}
    }}}}`

// reviewThreadID finds the node ID of the review thread that contains the
// review comment commentID.
func (c *Client) reviewThreadID(ctx context.Context, owner, repo string, number int, id int64) (string, bool, error) {
	var after any
	for page := 0; page < 20; page++ {
		var data struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool   `json:"hasNextPage"`
							EndCursor   string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []struct {
							ID         string `json:"id"`
							IsResolved bool   `json:"isResolved"`
							Comments   struct {
								Nodes []struct {
									DatabaseID int64 `json:"databaseId"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": owner, "repo": repo, "number": number, "after": after}
		if err := c.graphql(ctx, owner, repo, reviewThreadsQuery, vars, &data); err != nil {
			return "", false, err
		}
		rt := data.Repository.PullRequest.ReviewThreads
		for _, t := range rt.Nodes {
			for _, cm := range t.Comments.Nodes {
				if cm.DatabaseID == id {
					return t.ID, t.IsResolved, nil
				}
			}
		}
		if !rt.PageInfo.HasNextPage {
			break
		}
		after = rt.PageInfo.EndCursor
	}
	return "", false, fmt.Errorf("%w: github: no review thread of %s/%s#%d has comment %d", core.ErrNotFound, owner, repo, number, id)
}

// ResolveThread implements core.Forge.
func (c *Client) ResolveThread(ctx context.Context, owner, repo string, number int, threadRef string) error {
	id, err := commentID(threadRef)
	if err != nil {
		return err
	}
	nodeID, resolved, err := c.reviewThreadID(ctx, owner, repo, number, id)
	if err != nil {
		return fmt.Errorf("github: resolve thread: %w", err)
	}
	if resolved {
		return nil
	}
	const m = `mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{id isResolved}}}`
	if err := c.graphql(ctx, owner, repo, m, map[string]any{"id": nodeID}, &struct{}{}); err != nil {
		return fmt.Errorf("github: resolve thread %s of %s/%s#%d: %w", threadRef, owner, repo, number, err)
	}
	return nil
}

// SubmitReview implements core.Forge.
func (c *Client) SubmitReview(ctx context.Context, in core.ReviewInput) error {
	event := "COMMENT"
	switch strings.ToLower(in.Event) {
	case "", core.ReviewEventComment:
	case core.ReviewEventApprove:
		event = "APPROVE"
	case core.ReviewEventRequestChanges:
		event = "REQUEST_CHANGES"
	default:
		return fmt.Errorf("%w: unknown review event %q", core.ErrInvalid, in.Event)
	}
	type comment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	comments := make([]comment, 0, len(in.Comments))
	for _, cm := range in.Comments {
		if cm.Path == "" || cm.Line <= 0 || strings.TrimSpace(cm.Body) == "" {
			return fmt.Errorf("%w: an inline comment needs a path, a line and a body", core.ErrInvalid)
		}
		side := strings.ToUpper(cm.Side)
		if side == "" {
			side = "RIGHT"
		}
		comments = append(comments, comment{Path: cm.Path, Line: cm.Line, Side: side, Body: cm.Body})
	}
	body := map[string]any{"event": event, "body": in.Body, "comments": comments}
	path := fmt.Sprintf("%s/pulls/%d/reviews", repoPath(in.Owner, in.Repo), in.Number)
	if _, err := c.do(ctx, in.Owner, in.Repo, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("github: submit review on %s/%s#%d: %w", in.Owner, in.Repo, in.Number, err)
	}
	return nil
}

// MarkReady implements core.Forge. The REST API cannot change the draft
// state, so it uses the GraphQL mutation.
func (c *Client) MarkReady(ctx context.Context, owner, repo string, number int) error {
	var pr struct {
		NodeID string `json:"node_id"`
		Draft  bool   `json:"draft"`
	}
	if _, err := c.do(ctx, owner, repo, http.MethodGet, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number), nil, &pr); err != nil {
		return fmt.Errorf("github: read %s/%s#%d: %w", owner, repo, number, err)
	}
	if !pr.Draft {
		return nil
	}
	const m = `mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{id isDraft}}}`
	if err := c.graphql(ctx, owner, repo, m, map[string]any{"id": pr.NodeID}, &struct{}{}); err != nil {
		return fmt.Errorf("github: mark %s/%s#%d ready: %w", owner, repo, number, err)
	}
	return nil
}
