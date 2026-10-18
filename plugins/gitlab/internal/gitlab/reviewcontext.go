package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// The review context operations, the reviewer request and the
// user lookup that it needs.

var (
	_ core.ReviewContextReader = (*Client)(nil)
	_ core.ReviewRequester     = (*Client)(nil)
	_ core.LoginReader         = (*Client)(nil)
)

// maxPages limits the pages (of 100) that one read follows.
const maxPages = 30

// ReviewThread implements core.ReviewContextReader. threadRef is the ID of a
// merge request discussion.
func (c *Client) ReviewThread(ctx context.Context, owner, repo string, number int, threadRef string) (*core.ReviewThread, error) {
	if strings.TrimSpace(threadRef) == "" {
		return nil, fmt.Errorf("%w: gitlab: a discussion ID is required", core.ErrInvalid)
	}
	var d struct {
		ID    string `json:"id"`
		Notes []struct {
			ID         int    `json:"id"`
			Body       string `json:"body"`
			CreatedAt  string `json:"created_at"`
			System     bool   `json:"system"`
			Resolved   bool   `json:"resolved"`
			Resolvable bool   `json:"resolvable"`
			Author     struct {
				Username string `json:"username"`
			} `json:"author"`
			Position *struct {
				NewPath string `json:"new_path"`
				OldPath string `json:"old_path"`
				NewLine int    `json:"new_line"`
				OldLine int    `json:"old_line"`
			} `json:"position"`
		} `json:"notes"`
	}
	if err := c.api.Do(ctx, http.MethodGet, discussionPath(owner, repo, number, threadRef), nil, &d); err != nil {
		return nil, fmt.Errorf("gitlab: read discussion %s of !%d: %w", threadRef, number, err)
	}
	th := &core.ReviewThread{Ref: threadRef}
	resolvable, resolved := 0, 0
	for _, n := range d.Notes {
		if n.System {
			continue
		}
		tc := core.ThreadComment{ID: strconv.Itoa(n.ID), Author: n.Author.Username, Body: n.Body}
		if n.Position != nil {
			tc.Path, tc.Line = n.Position.NewPath, n.Position.NewLine
			if tc.Path == "" {
				tc.Path = n.Position.OldPath
			}
			if tc.Line == 0 {
				tc.Line = n.Position.OldLine
			}
			if th.Path == "" {
				th.Path, th.Line = tc.Path, tc.Line
			}
		}
		if t, err := time.Parse(time.RFC3339, n.CreatedAt); err == nil {
			tc.CreatedAt = t.UTC()
		}
		if n.Resolvable {
			resolvable++
			if n.Resolved {
				resolved++
			}
		}
		th.Comments = append(th.Comments, tc)
	}
	th.Resolved = resolvable > 0 && resolved == resolvable
	return th, nil
}

// FileDiff implements core.ReviewContextReader. GitLab leaves the diff of a
// large or generated file empty: the error then wraps core.ErrNotFound.
func (c *Client) FileDiff(ctx context.Context, owner, repo string, number int, path string) (*core.FileDiff, error) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, fmt.Errorf("%w: gitlab: a file path is required", core.ErrInvalid)
	}
	type change struct {
		OldPath   string `json:"old_path"`
		NewPath   string `json:"new_path"`
		Diff      string `json:"diff"`
		Collapsed bool   `json:"collapsed"`
		TooLarge  bool   `json:"too_large"`
	}
	mrPath := fmt.Sprintf("%s/merge_requests/%d", projectPath(owner, repo), number)
	var hit *change
	for page := 1; page <= maxPages && hit == nil; page++ {
		var cs []change
		hdr, err := c.api.DoRaw(ctx, http.MethodGet, fmt.Sprintf("%s/diffs?per_page=100&page=%d", mrPath, page), nil, &cs)
		if forgeutil.IsNotFound(err) && page == 1 {
			// GitLab before 15.7 has only the "changes" call, which holds all files.
			var old struct {
				Changes []change `json:"changes"`
			}
			if err = c.api.Do(ctx, http.MethodGet, mrPath+"/changes", nil, &old); err == nil {
				cs, hdr = old.Changes, http.Header{}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("gitlab: read the diff of !%d: %w", number, err)
		}
		for i := range cs {
			if cs[i].NewPath == path || cs[i].OldPath == path {
				hit = &cs[i]
				break
			}
		}
		if hdr.Get("X-Next-Page") == "" || len(cs) == 0 {
			break
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("gitlab: %s is not changed in !%d: %w", path, number, core.ErrNotFound)
	}
	if strings.TrimSpace(hit.Diff) == "" {
		reason := "the diff is empty"
		if hit.TooLarge || hit.Collapsed {
			reason = "GitLab left the diff out because the file is too large"
		}
		return nil, fmt.Errorf("gitlab: %s: %s: %w", path, reason, core.ErrNotFound)
	}
	return &core.FileDiff{Path: path, Patch: hit.Diff}, nil
}

// Login implements core.LoginReader: the username of the account of the
// token.
func (c *Client) Login(ctx context.Context) (string, error) {
	var u struct {
		Username string `json:"username"`
	}
	if err := c.api.Do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return "", fmt.Errorf("gitlab: read the user of the token: %w", err)
	}
	if strings.TrimSpace(u.Username) == "" {
		return "", fmt.Errorf("gitlab: the token has no username (a project or group token is not a user)")
	}
	return strings.TrimSpace(u.Username), nil
}

// RequestReviewers implements core.ReviewRequester. logins are usernames.
// GitLab replaces the reviewers of a merge request on update, so the call
// reads the present reviewers and keeps them.
func (c *Client) RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error {
	if len(logins) == 0 {
		return nil
	}
	mrPath := fmt.Sprintf("%s/merge_requests/%d", projectPath(owner, repo), number)
	var mr struct {
		Reviewers []struct {
			ID int `json:"id"`
		} `json:"reviewers"`
	}
	if err := c.api.Do(ctx, http.MethodGet, mrPath, nil, &mr); err != nil {
		return fmt.Errorf("gitlab: read !%d: %w", number, err)
	}
	ids := make([]int, 0, len(mr.Reviewers)+len(logins))
	seen := map[int]bool{}
	for _, r := range mr.Reviewers {
		ids, seen[r.ID] = append(ids, r.ID), true
	}
	for _, login := range logins {
		login = strings.TrimSpace(login)
		var users []struct {
			ID       int    `json:"id"`
			Username string `json:"username"`
		}
		if err := c.api.Do(ctx, http.MethodGet, "/users?username="+url.QueryEscape(login), nil, &users); err != nil {
			return fmt.Errorf("gitlab: find user %q: %w", login, err)
		}
		id := 0
		for _, u := range users {
			if strings.EqualFold(u.Username, login) {
				id = u.ID
			}
		}
		if id == 0 {
			return fmt.Errorf("gitlab: user %q: %w", login, core.ErrNotFound)
		}
		if !seen[id] {
			ids, seen[id] = append(ids, id), true
		}
	}
	if err := c.api.Do(ctx, http.MethodPut, mrPath, map[string]any{"reviewer_ids": ids}, nil); err != nil {
		return fmt.Errorf("gitlab: request review of !%d: %w", number, err)
	}
	return nil
}
