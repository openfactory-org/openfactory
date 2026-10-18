// Package gitlab implements core.Forge for GitLab (REST v4) with personal,
// project, group or OAuth access tokens, plus the GitLab webhook receiver.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// ForgeName is the code-forge identifier used in definitions.
const ForgeName = "GITLAB"

// DefaultBaseURL is the GitLab.com API root.
const DefaultBaseURL = "https://gitlab.com/api/v4"

// Limits applied by GetFiles.
const (
	MaxTreeFiles = 2000
	MaxFileBytes = 1 << 20
)

// Config configures a Client.
type Config struct {
	// BaseURL is the API root, e.g. https://gitlab.com/api/v4 or https://gitlab.example.com/api/v4.
	BaseURL string
	// WebURL is the git host root (derived from BaseURL when empty).
	WebURL string
	Token  string
	// TokenType is "private" (PRIVATE-TOKEN header; personal/project/group tokens, default) or "oauth" (Bearer).
	TokenType  string
	HTTPClient *http.Client
}

// Client is a GitLab REST client implementing core.Forge.
type Client struct {
	cfg    Config
	api    *forgeutil.Client
	webURL string
}

var _ core.Forge = (*Client)(nil)

// New creates a client.
func New(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("gitlab: a token is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(cfg.BaseURL, "/api/v4") {
		cfg.BaseURL += "/api/v4"
	}
	c := &Client{cfg: cfg, webURL: strings.TrimRight(cfg.WebURL, "/")}
	if c.webURL == "" {
		c.webURL = strings.TrimSuffix(cfg.BaseURL, "/api/v4")
	}
	c.api = &forgeutil.Client{
		BaseURL:   cfg.BaseURL,
		HTTP:      cfg.HTTPClient,
		UserAgent: "openfactory-gitlab",
		Authorize: func(_ context.Context, req *http.Request) error {
			if strings.EqualFold(cfg.TokenType, "oauth") || strings.EqualFold(cfg.TokenType, "bearer") {
				req.Header.Set("Authorization", "Bearer "+cfg.Token)
			} else {
				req.Header.Set("PRIVATE-TOKEN", cfg.Token)
			}
			return nil
		},
	}
	return c, nil
}

// Name implements core.Forge.
func (c *Client) Name() string { return ForgeName }

// projectPath returns /projects/<url-encoded owner/repo>. owner may contain subgroups.
func projectPath(owner, repo string) string {
	full := strings.Trim(owner+"/"+repo, "/")
	return "/projects/" + url.PathEscape(full)
}

// CloneURL implements core.Forge (oauth2:<token> credential).
func (c *Client) CloneURL(_ context.Context, owner, repo string) (string, error) {
	u, err := url.Parse(c.webURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("oauth2", c.cfg.Token)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.Trim(owner+"/"+repo, "/") + ".git"
	return u.String(), nil
}

// DefaultBranch implements core.Forge.
func (c *Client) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	var p struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.api.Do(ctx, http.MethodGet, projectPath(owner, repo), nil, &p); err != nil {
		return "", err
	}
	return p.DefaultBranch, nil
}

type mergeRequest struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	State        string `json:"state"` // opened, closed, locked, merged
	Draft        bool   `json:"draft"`
	WorkInProg   bool   `json:"work_in_progress"`
	SourceBranch string `json:"source_branch"`
}

func (m mergeRequest) ref(owner, repo string) *core.PullRequestRef {
	state := m.State
	switch m.State {
	case "opened":
		state = "open"
		if m.Draft || m.WorkInProg {
			state = "draft"
		}
	case "locked":
		state = "open"
	}
	return &core.PullRequestRef{
		Forge: ForgeName, Repo: strings.Trim(owner+"/"+repo, "/"), Number: m.IID,
		URL: m.WebURL, Branch: m.SourceBranch, State: state,
	}
}

// CreatePullRequest implements core.Forge by opening a merge request (drafts use the "Draft:" title prefix).
func (c *Client) CreatePullRequest(ctx context.Context, in core.PullRequestInput) (*core.PullRequestRef, error) {
	base := in.Base
	if base == "" {
		b, err := c.DefaultBranch(ctx, in.Owner, in.Repo)
		if err != nil {
			return nil, err
		}
		base = b
	}
	title := in.Title
	if in.Draft && !strings.HasPrefix(strings.ToLower(title), "draft:") {
		title = "Draft: " + title
	}
	body := map[string]any{
		"source_branch":        in.Head,
		"target_branch":        base,
		"title":                title,
		"description":          in.Body,
		"remove_source_branch": true,
	}
	var mr mergeRequest
	if err := c.api.Do(ctx, http.MethodPost, projectPath(in.Owner, in.Repo)+"/merge_requests", body, &mr); err != nil {
		return nil, fmt.Errorf("gitlab: create merge request: %w", err)
	}
	return mr.ref(in.Owner, in.Repo), nil
}

// GetPullRequest implements core.Forge.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*core.PullRequestRef, error) {
	var mr mergeRequest
	if err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", projectPath(owner, repo), number), nil, &mr); err != nil {
		return nil, err
	}
	return mr.ref(owner, repo), nil
}

// Comment implements core.Forge by adding a merge request note.
func (c *Client) Comment(ctx context.Context, owner, repo string, number int, body string) error {
	err := c.api.Do(ctx, http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/notes", projectPath(owner, repo), number),
		map[string]string{"body": body}, nil)
	if err != nil {
		return fmt.Errorf("gitlab: comment on merge request !%d: %w", number, err)
	}
	return nil
}

// CommentIssue adds a note to an issue.
func (c *Client) CommentIssue(ctx context.Context, owner, repo string, number int, body string) error {
	err := c.api.Do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/notes", projectPath(owner, repo), number),
		map[string]string{"body": body}, nil)
	if err != nil {
		return fmt.Errorf("gitlab: comment on issue #%d: %w", number, err)
	}
	return nil
}

// ReplyDiscussion adds a note to an existing merge request discussion thread.
func (c *Client) ReplyDiscussion(ctx context.Context, owner, repo string, number int, discussionID, body string) error {
	return c.api.Do(ctx, http.MethodPost,
		fmt.Sprintf("%s/merge_requests/%d/discussions/%s/notes", projectPath(owner, repo), number, url.PathEscape(discussionID)),
		map[string]string{"body": body}, nil)
}

// SetCommitStatus implements core.Forge.
func (c *Client) SetCommitStatus(ctx context.Context, owner, repo, sha string, st core.CommitStatus) error {
	state := st.State
	switch state {
	case "", "pending":
		state = "pending"
	case "failure", "error":
		state = "failed"
	}
	body := map[string]string{
		"state":       state,
		"name":        st.Context,
		"description": forgeutil.Truncate(st.Description, 255),
	}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	if err := c.api.Do(ctx, http.MethodPost, projectPath(owner, repo)+"/statuses/"+url.PathEscape(sha), body, nil); err != nil {
		return fmt.Errorf("gitlab: set commit status: %w", err)
	}
	return nil
}

// GetFiles implements core.Forge using the repository tree and raw file APIs.
// Returned keys are relative to path.
func (c *Client) GetFiles(ctx context.Context, owner, repo, ref, path string) (map[string]string, string, error) {
	pp := projectPath(owner, repo)
	if ref == "" {
		b, err := c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return nil, "", err
		}
		ref = b
	}
	var commit struct {
		ID string `json:"id"`
	}
	if err := c.api.Do(ctx, http.MethodGet, pp+"/repository/commits/"+url.PathEscape(ref), nil, &commit); err != nil {
		return nil, "", fmt.Errorf("gitlab: resolve %s: %w", ref, err)
	}
	dir := strings.Trim(path, "/")
	files := map[string]string{}
	for page := 1; ; page++ {
		q := url.Values{"ref": {commit.ID}, "recursive": {"true"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if dir != "" {
			q.Set("path", dir)
		}
		var entries []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		}
		hdr, err := c.api.DoRaw(ctx, http.MethodGet, pp+"/repository/tree?"+q.Encode(), nil, &entries)
		if err != nil {
			return nil, "", fmt.Errorf("gitlab: read tree: %w", err)
		}
		for _, e := range entries {
			if e.Type != "blob" {
				continue
			}
			if len(files) >= MaxTreeFiles {
				return nil, "", fmt.Errorf("gitlab: more than %d files under %q", MaxTreeFiles, path)
			}
			var raw []byte
			rq := url.Values{"ref": {commit.ID}}
			if err := c.api.Do(ctx, http.MethodGet, pp+"/repository/files/"+url.PathEscape(e.Path)+"/raw?"+rq.Encode(), nil, &raw); err != nil {
				return nil, "", fmt.Errorf("gitlab: read %s: %w", e.Path, err)
			}
			if len(raw) > MaxFileBytes {
				return nil, "", fmt.Errorf("gitlab: %s exceeds %d bytes", e.Path, MaxFileBytes)
			}
			rel := e.Path
			if dir != "" {
				rel = strings.TrimPrefix(e.Path, dir+"/")
			}
			files[rel] = string(raw)
		}
		next := hdr.Get("X-Next-Page")
		if next == "" || len(entries) == 0 {
			break
		}
	}
	return files, commit.ID, nil
}
