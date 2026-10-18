package azuredevops

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The review context operations, the reviewer request and the
// identity lookup that it needs.

var (
	_ core.ReviewContextReader = (*Client)(nil)
	_ core.ReviewRequester     = (*Client)(nil)
	_ core.LoginReader         = (*Client)(nil)
)

// ReviewThread implements core.ReviewContextReader. threadRef is the ID of a
// pull request comment thread.
func (c *Client) ReviewThread(ctx context.Context, project, repo string, number int, threadRef string) (*core.ReviewThread, error) {
	id, err := threadID(threadRef)
	if err != nil {
		return nil, err
	}
	var t struct {
		ID            int    `json:"id"`
		Status        string `json:"status"`
		ThreadContext *struct {
			FilePath       string `json:"filePath"`
			RightFileStart *struct {
				Line int `json:"line"`
			} `json:"rightFileStart"`
			LeftFileStart *struct {
				Line int `json:"line"`
			} `json:"leftFileStart"`
		} `json:"threadContext"`
		Comments []struct {
			ID            int    `json:"id"`
			Content       string `json:"content"`
			CommentType   string `json:"commentType"`
			IsDeleted     bool   `json:"isDeleted"`
			PublishedDate string `json:"publishedDate"`
			Author        struct {
				DisplayName string `json:"displayName"`
				UniqueName  string `json:"uniqueName"`
			} `json:"author"`
		} `json:"comments"`
	}
	path := withVersion(fmt.Sprintf("%s/pullRequests/%d/threads/%d", repoAPI(project, repo), number, id), nil)
	if err := c.api.Do(ctx, http.MethodGet, path, nil, &t); err != nil {
		return nil, fmt.Errorf("azuredevops: read thread %d of pull request %d: %w", id, number, err)
	}
	th := &core.ReviewThread{Ref: strconv.Itoa(id)}
	switch strings.ToLower(t.Status) {
	case "fixed", "closed", "bydesign", "wontfix":
		th.Resolved = true
	}
	if tc := t.ThreadContext; tc != nil {
		th.Path = strings.TrimPrefix(tc.FilePath, "/")
		switch {
		case tc.RightFileStart != nil:
			th.Line = tc.RightFileStart.Line
		case tc.LeftFileStart != nil:
			th.Line = tc.LeftFileStart.Line
		}
	}
	for _, cm := range t.Comments {
		if cm.IsDeleted || strings.EqualFold(cm.CommentType, "system") {
			continue
		}
		author := cm.Author.UniqueName
		if author == "" {
			author = cm.Author.DisplayName
		}
		x := core.ThreadComment{ID: strconv.Itoa(cm.ID), Author: author, Body: cm.Content, Path: th.Path, Line: th.Line}
		if ts, err := time.Parse(time.RFC3339, cm.PublishedDate); err == nil {
			x.CreatedAt = ts.UTC()
		}
		th.Comments = append(th.Comments, x)
	}
	return th, nil
}

// FileDiff implements core.ReviewContextReader. It reads the file at the
// merge base and at the head of the last iteration of the pull request, and
// makes the diff. A binary or large file gives an error that wraps
// core.ErrNotFound.
func (c *Client) FileDiff(ctx context.Context, project, repo string, number int, path string) (*core.FileDiff, error) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "/")
	if path == "" {
		return nil, fmt.Errorf("%w: azuredevops: a file path is required", core.ErrInvalid)
	}
	base := fmt.Sprintf("%s/pullRequests/%d/iterations", repoAPI(project, repo), number)
	var its struct {
		Value []struct {
			ID              int `json:"id"`
			SourceRefCommit struct {
				CommitID string `json:"commitId"`
			} `json:"sourceRefCommit"`
			CommonRefCommit struct {
				CommitID string `json:"commitId"`
			} `json:"commonRefCommit"`
		} `json:"value"`
	}
	if err := c.api.Do(ctx, http.MethodGet, withVersion(base, nil), nil, &its); err != nil {
		return nil, fmt.Errorf("azuredevops: read the iterations of pull request %d: %w", number, err)
	}
	if len(its.Value) == 0 {
		return nil, fmt.Errorf("azuredevops: pull request %d has no iteration: %w", number, core.ErrNotFound)
	}
	last := its.Value[len(its.Value)-1]
	var changes struct {
		ChangeEntries []struct {
			ChangeType   string `json:"changeType"`
			OriginalPath string `json:"originalPath"`
			Item         struct {
				Path string `json:"path"`
			} `json:"item"`
		} `json:"changeEntries"`
	}
	q := url.Values{"$compareTo": {"0"}, "$top": {"2000"}}
	if err := c.api.Do(ctx, http.MethodGet, withVersion(fmt.Sprintf("%s/%d/changes", base, last.ID), q), nil, &changes); err != nil {
		return nil, fmt.Errorf("azuredevops: read the changes of pull request %d: %w", number, err)
	}
	oldPath, kind, found := "", "", false
	for _, ch := range changes.ChangeEntries {
		if strings.TrimPrefix(ch.Item.Path, "/") == path {
			oldPath, kind, found = strings.TrimPrefix(ch.OriginalPath, "/"), strings.ToLower(ch.ChangeType), true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("azuredevops: %s is not changed in pull request %d: %w", path, number, core.ErrNotFound)
	}
	if oldPath == "" {
		oldPath = path
	}
	var before, after string
	var err error
	if !strings.Contains(kind, "add") {
		if before, err = c.fileAt(ctx, project, repo, oldPath, last.CommonRefCommit.CommitID); err != nil {
			return nil, err
		}
	}
	if !strings.Contains(kind, "delete") {
		if after, err = c.fileAt(ctx, project, repo, path, last.SourceRefCommit.CommitID); err != nil {
			return nil, err
		}
	}
	patch := unifiedDiff(before, after)
	if patch == "" {
		return nil, fmt.Errorf("azuredevops: %s has no text change in pull request %d: %w", path, number, core.ErrNotFound)
	}
	return &core.FileDiff{Path: path, Patch: patch}, nil
}

// fileAt reads a text file at a commit.
func (c *Client) fileAt(ctx context.Context, project, repo, path, commit string) (string, error) {
	q := url.Values{
		"path":                          {"/" + strings.TrimPrefix(path, "/")},
		"versionDescriptor.version":     {commit},
		"versionDescriptor.versionType": {"commit"},
		"$format":                       {"octetstream"},
	}
	var raw []byte
	if err := c.api.Do(ctx, http.MethodGet, withVersion(repoAPI(project, repo)+"/items", q), nil, &raw); err != nil {
		return "", fmt.Errorf("azuredevops: read %s at %s: %w", path, commit, err)
	}
	if len(raw) > MaxFileBytes {
		return "", fmt.Errorf("azuredevops: %s is larger than %d bytes: %w", path, MaxFileBytes, core.ErrNotFound)
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", fmt.Errorf("azuredevops: %s is a binary file: %w", path, core.ErrNotFound)
	}
	return string(raw), nil
}

// Login implements core.LoginReader. Azure DevOps names a reviewer by the ID
// of the identity, so it returns the ID of the user of the token.
func (c *Client) Login(ctx context.Context) (string, error) {
	var cd struct {
		AuthenticatedUser struct {
			ID string `json:"id"`
		} `json:"authenticatedUser"`
	}
	const path = "/_apis/connectionData?connectOptions=none&lastChangeId=-1&lastChangeId64=-1&api-version=7.1-preview"
	if err := c.api.Do(ctx, http.MethodGet, path, nil, &cd); err != nil {
		return "", fmt.Errorf("azuredevops: read the user of the token: %w", err)
	}
	if strings.TrimSpace(cd.AuthenticatedUser.ID) == "" {
		return "", fmt.Errorf("azuredevops: the token has no user identity")
	}
	return strings.TrimSpace(cd.AuthenticatedUser.ID), nil
}

// RequestReviewers implements core.ReviewRequester. Each login is the ID of
// an identity (see Login). The reviewer is optional and has no vote.
func (c *Client) RequestReviewers(ctx context.Context, project, repo string, number int, logins []string) error {
	for _, id := range logins {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("%w: azuredevops: a reviewer identity is required", core.ErrInvalid)
		}
		path := withVersion(fmt.Sprintf("%s/pullRequests/%d/reviewers/%s", repoAPI(project, repo), number, url.PathEscape(id)), nil)
		if err := c.api.Do(ctx, http.MethodPut, path, map[string]any{"id": id, "vote": 0, "isRequired": false}, nil); err != nil {
			return fmt.Errorf("azuredevops: request review of pull request %d: %w", number, err)
		}
	}
	return nil
}
