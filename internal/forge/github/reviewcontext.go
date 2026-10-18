package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The review context operations of core.ReviewContextReader.

var _ core.ReviewContextReader = (*Client)(nil)

// maxReviewPages limits the pages (of 100) that one read follows.
const maxReviewPages = 30

// ReviewThread implements core.ReviewContextReader. threadRef is the ID of the
// first comment of the thread. The thread has that comment and each reply to it.
func (c *Client) ReviewThread(ctx context.Context, owner, repo string, number int, threadRef string) (*core.ReviewThread, error) {
	root, err := commentID(threadRef)
	if err != nil {
		return nil, err
	}
	th := &core.ReviewThread{Ref: strconv.FormatInt(root, 10)}
	found := false
	for page := 1; page <= maxReviewPages; page++ {
		var cs []struct {
			ID        int64  `json:"id"`
			InReplyTo int64  `json:"in_reply_to_id"`
			Body      string `json:"body"`
			Path      string `json:"path"`
			Line      int    `json:"line"`
			Original  int    `json:"original_line"`
			CreatedAt string `json:"created_at"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		path := fmt.Sprintf("%s/pulls/%d/comments?per_page=100&page=%d", repoPath(owner, repo), number, page)
		if _, err := c.do(ctx, owner, repo, http.MethodGet, path, nil, &cs); err != nil {
			return nil, fmt.Errorf("github: read review comments of %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, x := range cs {
			if x.ID != root && x.InReplyTo != root {
				continue
			}
			found = true
			line := x.Line
			if line == 0 {
				line = x.Original
			}
			tc := core.ThreadComment{ID: strconv.FormatInt(x.ID, 10), Author: x.User.Login, Body: x.Body, Path: x.Path, Line: line}
			if t, err := time.Parse(time.RFC3339, x.CreatedAt); err == nil {
				tc.CreatedAt = t.UTC()
			}
			if x.ID == root {
				th.Path, th.Line = x.Path, line
			}
			th.Comments = append(th.Comments, tc)
		}
		if len(cs) < 100 {
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("github: review thread %s of %s/%s#%d: %w", threadRef, owner, repo, number, core.ErrNotFound)
	}
	return th, nil
}

// FileDiff implements core.ReviewContextReader. GitHub gives no patch for a
// file that is binary or very large: the result then has an empty patch and
// the error wraps core.ErrNotFound.
func (c *Client) FileDiff(ctx context.Context, owner, repo string, number int, path string) (*core.FileDiff, error) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, fmt.Errorf("%w: github: a file path is required", core.ErrInvalid)
	}
	for page := 1; page <= maxReviewPages; page++ {
		var files []struct {
			Filename string `json:"filename"`
			Patch    string `json:"patch"`
		}
		p := fmt.Sprintf("%s/pulls/%d/files?per_page=100&page=%d", repoPath(owner, repo), number, page)
		if _, err := c.do(ctx, owner, repo, http.MethodGet, p, nil, &files); err != nil {
			return nil, fmt.Errorf("github: read files of %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, f := range files {
			if f.Filename != path {
				continue
			}
			if f.Patch == "" {
				return nil, fmt.Errorf("github: %s has no patch (the file is binary or too large): %w", path, core.ErrNotFound)
			}
			return &core.FileDiff{Path: path, Patch: f.Patch}, nil
		}
		if len(files) < 100 {
			break
		}
	}
	return nil, fmt.Errorf("github: %s is not changed in %s/%s#%d: %w", path, owner, repo, number, core.ErrNotFound)
}
