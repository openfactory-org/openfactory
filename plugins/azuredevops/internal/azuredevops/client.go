// Package azuredevops implements core.Forge for Azure DevOps Services (REST 7.1)
// with personal-access-token basic authentication, plus the service-hook
// receiver for pull request and work item events.
//
// Repository addressing: owner is the Azure DevOps project, repo is the
// repository name (or ID); the organization comes from the client config.
// SourceRef.Repo for Azure DevOps events is therefore "project/repository".
package azuredevops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// ForgeName is the code-forge identifier used in definitions.
const ForgeName = "AZURE_DEVOPS"

// DefaultBaseURL is the Azure DevOps Services root.
const DefaultBaseURL = "https://dev.azure.com"

const apiVersion = "7.1"

// Limits applied by GetFiles.
const (
	MaxTreeFiles = 2000
	MaxFileBytes = 1 << 20
)

// Config configures a Client.
type Config struct {
	Organization string
	// BaseURL is https://dev.azure.com (default).
	BaseURL    string
	Token      string // personal access token
	HTTPClient *http.Client
}

// Client implements core.Forge for Azure DevOps.
type Client struct {
	cfg Config
	api *forgeutil.Client
}

var _ core.Forge = (*Client)(nil)

// New creates a client.
func New(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, errors.New("azuredevops: a personal access token is required")
	}
	if cfg.Organization == "" {
		return nil, errors.New("azuredevops: organization is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+cfg.Token))
	c := &Client{cfg: cfg}
	c.api = &forgeutil.Client{
		BaseURL:   cfg.BaseURL + "/" + url.PathEscape(cfg.Organization),
		HTTP:      cfg.HTTPClient,
		UserAgent: "openfactory-azuredevops",
		Authorize: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", auth)
			if req.Method == http.MethodPatch && strings.Contains(strings.ToLower(req.URL.Path), "/_apis/wit/workitems/") {
				// A work item is changed with a JSON Patch document.
				req.Header.Set("Content-Type", "application/json-patch+json")
			}
			return nil
		},
	}
	return c, nil
}

// Name implements core.Forge.
func (c *Client) Name() string { return ForgeName }

func repoAPI(project, repo string) string {
	return "/" + url.PathEscape(project) + "/_apis/git/repositories/" + url.PathEscape(repo)
}

func withVersion(path string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api-version", apiVersion)
	return path + "?" + q.Encode()
}

// CloneURL implements core.Forge.
func (c *Client) CloneURL(_ context.Context, project, repo string) (string, error) {
	u, err := url.Parse(c.cfg.BaseURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("pat", c.cfg.Token)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + c.cfg.Organization + "/" + project + "/_git/" + repo
	return u.String(), nil
}

type repository struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	WebURL        string `json:"webUrl"`
}

// DefaultBranch implements core.Forge.
func (c *Client) DefaultBranch(ctx context.Context, project, repo string) (string, error) {
	var r repository
	if err := c.api.Do(ctx, http.MethodGet, withVersion(repoAPI(project, repo), nil), nil, &r); err != nil {
		return "", err
	}
	return strings.TrimPrefix(r.DefaultBranch, "refs/heads/"), nil
}

type pullRequest struct {
	PullRequestID int        `json:"pullRequestId"`
	Status        string     `json:"status"` // active, abandoned, completed
	IsDraft       bool       `json:"isDraft"`
	SourceRefName string     `json:"sourceRefName"`
	Repository    repository `json:"repository"`
}

func (p pullRequest) ref(project, repo string) *core.PullRequestRef {
	state := "open"
	switch p.Status {
	case "completed":
		state = "merged"
	case "abandoned":
		state = "closed"
	default:
		if p.IsDraft {
			state = "draft"
		}
	}
	u := ""
	if p.Repository.WebURL != "" {
		u = fmt.Sprintf("%s/pullrequest/%d", p.Repository.WebURL, p.PullRequestID)
	}
	return &core.PullRequestRef{
		Forge: ForgeName, Repo: project + "/" + repo, Number: p.PullRequestID, URL: u,
		Branch: strings.TrimPrefix(p.SourceRefName, "refs/heads/"), State: state,
	}
}

func headsRef(b string) string {
	if strings.HasPrefix(b, "refs/") {
		return b
	}
	return "refs/heads/" + b
}

// CreatePullRequest implements core.Forge.
func (c *Client) CreatePullRequest(ctx context.Context, in core.PullRequestInput) (*core.PullRequestRef, error) {
	base := in.Base
	if base == "" {
		b, err := c.DefaultBranch(ctx, in.Owner, in.Repo)
		if err != nil {
			return nil, err
		}
		base = b
	}
	body := map[string]any{
		"sourceRefName": headsRef(in.Head),
		"targetRefName": headsRef(base),
		"title":         in.Title,
		"description":   forgeutil.Truncate(in.Body, 4000),
		"isDraft":       in.Draft,
	}
	var pr pullRequest
	if err := c.api.Do(ctx, http.MethodPost, withVersion(repoAPI(in.Owner, in.Repo)+"/pullrequests", nil), body, &pr); err != nil {
		return nil, fmt.Errorf("azuredevops: create pull request: %w", err)
	}
	return pr.ref(in.Owner, in.Repo), nil
}

// GetPullRequest implements core.Forge.
func (c *Client) GetPullRequest(ctx context.Context, project, repo string, number int) (*core.PullRequestRef, error) {
	var pr pullRequest
	if err := c.api.Do(ctx, http.MethodGet, withVersion(fmt.Sprintf("%s/pullrequests/%d", repoAPI(project, repo), number), nil), nil, &pr); err != nil {
		return nil, err
	}
	return pr.ref(project, repo), nil
}

// Comment implements core.Forge by opening a new pull request comment thread.
func (c *Client) Comment(ctx context.Context, project, repo string, number int, body string) error {
	payload := map[string]any{
		"comments": []map[string]any{{"parentCommentId": 0, "content": body, "commentType": 1}},
		"status":   1, // active
	}
	if err := c.api.Do(ctx, http.MethodPost, withVersion(fmt.Sprintf("%s/pullRequests/%d/threads", repoAPI(project, repo), number), nil), payload, nil); err != nil {
		return fmt.Errorf("azuredevops: comment on pull request %d: %w", number, err)
	}
	return nil
}

// CommentWorkItem adds a discussion comment to a work item.
func (c *Client) CommentWorkItem(ctx context.Context, project string, id int, body string) error {
	path := fmt.Sprintf("/%s/_apis/wit/workItems/%d/comments?api-version=7.1-preview.4", url.PathEscape(project), id)
	if err := c.api.Do(ctx, http.MethodPost, path, map[string]string{"text": body}, nil); err != nil {
		return fmt.Errorf("azuredevops: comment on work item %d: %w", id, err)
	}
	return nil
}

// SetCommitStatus implements core.Forge. The context "genre/name" is split at the last slash.
func (c *Client) SetCommitStatus(ctx context.Context, project, repo, sha string, st core.CommitStatus) error {
	state := st.State
	switch state {
	case "", "pending":
		state = "pending"
	case "success":
		state = "succeeded"
	case "failure":
		state = "failed"
	case "error":
		state = "error"
	}
	genre, name := "", st.Context
	if i := strings.LastIndex(st.Context, "/"); i > 0 {
		genre, name = st.Context[:i], st.Context[i+1:]
	}
	body := map[string]any{
		"state":       state,
		"description": forgeutil.Truncate(st.Description, 255),
		"context":     map[string]string{"genre": genre, "name": name},
	}
	if st.TargetURL != "" {
		body["targetUrl"] = st.TargetURL
	}
	if err := c.api.Do(ctx, http.MethodPost, withVersion(repoAPI(project, repo)+"/commits/"+url.PathEscape(sha)+"/statuses", nil), body, nil); err != nil {
		return fmt.Errorf("azuredevops: set commit status: %w", err)
	}
	return nil
}

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// GetFiles implements core.Forge using the items and blobs APIs. Keys are relative to path.
func (c *Client) GetFiles(ctx context.Context, project, repo, ref, path string) (map[string]string, string, error) {
	if ref == "" {
		b, err := c.DefaultBranch(ctx, project, repo)
		if err != nil {
			return nil, "", err
		}
		ref = b
	}
	vtype := "branch"
	if shaRe.MatchString(ref) {
		vtype = "commit"
	}
	scope := "/" + strings.Trim(path, "/")
	q := url.Values{
		"scopePath":                     {scope},
		"recursionLevel":                {"Full"},
		"versionDescriptor.version":     {strings.TrimPrefix(ref, "refs/heads/")},
		"versionDescriptor.versionType": {vtype},
		"latestProcessedChange":         {"true"},
	}
	var items struct {
		Value []struct {
			ObjectID              string `json:"objectId"`
			GitObjectType         string `json:"gitObjectType"`
			CommitID              string `json:"commitId"`
			Path                  string `json:"path"`
			IsFolder              bool   `json:"isFolder"`
			LatestProcessedChange *struct {
				CommitID string `json:"commitId"`
			} `json:"latestProcessedChange"`
		} `json:"value"`
	}
	if err := c.api.Do(ctx, http.MethodGet, withVersion(repoAPI(project, repo)+"/items", q), nil, &items); err != nil {
		return nil, "", fmt.Errorf("azuredevops: list items: %w", err)
	}
	commitSHA := ""
	if vtype == "commit" {
		commitSHA = ref
	}
	prefix := strings.TrimRight(scope, "/") + "/"
	files := map[string]string{}
	for _, it := range items.Value {
		if commitSHA == "" && it.CommitID != "" && it.Path == scope {
			commitSHA = it.CommitID
		}
		if it.IsFolder || it.GitObjectType != "blob" {
			continue
		}
		if len(files) >= MaxTreeFiles {
			return nil, "", fmt.Errorf("azuredevops: more than %d files under %q", MaxTreeFiles, path)
		}
		var raw []byte
		bq := url.Values{"$format": {"octetstream"}}
		if err := c.api.Do(ctx, http.MethodGet, withVersion(repoAPI(project, repo)+"/blobs/"+url.PathEscape(it.ObjectID), bq), nil, &raw); err != nil {
			return nil, "", fmt.Errorf("azuredevops: read %s: %w", it.Path, err)
		}
		if len(raw) > MaxFileBytes {
			return nil, "", fmt.Errorf("azuredevops: %s exceeds %d bytes", it.Path, MaxFileBytes)
		}
		files[strings.TrimPrefix(it.Path, prefix)] = string(raw)
	}
	if commitSHA == "" {
		// Resolve the branch head explicitly.
		var refs struct {
			Value []struct {
				ObjectID string `json:"objectId"`
			} `json:"value"`
		}
		rq := url.Values{"filter": {"heads/" + strings.TrimPrefix(ref, "refs/heads/")}}
		if err := c.api.Do(ctx, http.MethodGet, withVersion(repoAPI(project, repo)+"/refs", rq), nil, &refs); err == nil && len(refs.Value) > 0 {
			commitSHA = refs.Value[0].ObjectID
		}
	}
	return files, commitSHA, nil
}
