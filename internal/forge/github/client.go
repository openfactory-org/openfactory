// Package github implements core.Forge for GitHub.com and GitHub Enterprise
// Server (REST v3), GitHub App and personal-access-token authentication, the
// `openfactory/definition` check, and the GitHub webhook receiver that
// normalizes repository activity into core.Event values.
package github

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// ForgeName is the code-forge identifier used in definitions.
const ForgeName = "GITHUB"

// DefaultBaseURL is the GitHub.com REST API root.
const DefaultBaseURL = "https://api.github.com"

// Limits applied by GetFiles.
const (
	MaxTreeFiles = 2000
	MaxFileBytes = 1 << 20
)

// Config configures a Client. Either Token (PAT) or AppID+PrivateKey must be set.
type Config struct {
	// BaseURL is the REST API root: https://api.github.com or https://HOST/api/v3 for GHES.
	BaseURL string
	// WebURL is the web/git host root (derived from BaseURL when empty).
	WebURL string
	// Token is a personal access token (fallback when no App is configured).
	Token string
	// AppID and PrivateKey (PEM) enable GitHub App authentication.
	AppID      string
	PrivateKey []byte
	// InstallationID pins the App installation; when 0 it is discovered per repository owner.
	InstallationID int64
	HTTPClient     *http.Client
	// Now overrides the clock (tests).
	Now func() time.Time
	// OnAuthFailure is called when GitHub refuses the credential itself:
	// the app was removed or suspended, or the token was revoked. The
	// resolver sets the status of the integration record. Optional.
	OnAuthFailure func(err error)
}

// tokenMinLife is the least life that a cached installation token must have
// left. A git push or fetch that starts with the token must end before it
// expires.
const tokenMinLife = 10 * time.Minute

// Client is a GitHub REST client implementing core.Forge.
type Client struct {
	cfg    Config
	api    *forgeutil.Client
	key    *rsa.PrivateKey
	webURL string
	now    func() time.Time

	mu            sync.Mutex
	tokens        map[int64]cachedToken // installation id -> token
	installations map[string]int64      // owner -> installation id

	// scope limits the client to these repositories: lower-case owner ->
	// repository names. Nil means no limit. See Scoped.
	scope map[string][]string
	// scoped caches the scoped clients by their repository list, so each
	// keeps its token cache.
	scoped map[string]*Client
	// openRuns holds the IDs of the check runs that PublishCheckRun left in
	// progress, so that the completed result updates the same run.
	openRuns map[string]int64
}

type cachedToken struct {
	token   string
	expires time.Time
}

var _ core.Forge = (*Client)(nil)

// New creates a client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	c := &Client{
		cfg:           cfg,
		now:           cfg.Now,
		tokens:        map[int64]cachedToken{},
		installations: map[string]int64{},
	}
	if c.now == nil {
		c.now = time.Now
	}
	if cfg.AppID != "" || len(cfg.PrivateKey) > 0 {
		if cfg.AppID == "" || len(cfg.PrivateKey) == 0 {
			return nil, errors.New("github: app authentication requires both app_id and private key")
		}
		k, err := forgeutil.ParseRSAPrivateKey(cfg.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("github: %w", err)
		}
		c.key = k
	} else if cfg.Token == "" {
		return nil, errors.New("github: a token or app credentials are required")
	}
	c.webURL = strings.TrimRight(cfg.WebURL, "/")
	if c.webURL == "" {
		c.webURL = deriveWebURL(cfg.BaseURL)
	}
	c.api = &forgeutil.Client{
		BaseURL:   cfg.BaseURL,
		HTTP:      cfg.HTTPClient,
		UserAgent: "openfactory-github",
		Header: http.Header{
			"Accept":               {"application/vnd.github+json"},
			"X-Github-Api-Version": {"2022-11-28"},
		},
		Authorize: func(ctx context.Context, req *http.Request) error {
			if tok, _ := ctx.Value(authKey{}).(string); tok != "" {
				req.Header.Set("Authorization", tok)
			}
			return nil
		},
	}
	return c, nil
}

func deriveWebURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return "https://github.com"
	}
	if u.Host == "api.github.com" {
		return "https://github.com"
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v3")
	return strings.TrimRight(u.String(), "/")
}

// Name implements core.Forge.
func (c *Client) Name() string { return ForgeName }

// IsApp reports whether the client authenticates as a GitHub App.
func (c *Client) IsApp() bool { return c.key != nil }

// WebURL returns the web host root, e.g. https://github.com.
func (c *Client) WebURL() string { return c.webURL }

type authKey struct{}

func withAuth(ctx context.Context, header string) context.Context {
	return context.WithValue(ctx, authKey{}, header)
}

// AppJWT returns a short-lived JWT authenticating as the App itself.
func (c *Client) AppJWT() (string, error) {
	if c.key == nil {
		return "", errors.New("github: not configured as an app")
	}
	now := c.now()
	claims := map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(), // allow for clock drift
		"exp": now.Add(9 * time.Minute).Unix(),   // GitHub caps at 10 minutes
		"iss": c.cfg.AppID,
	}
	return forgeutil.SignRS256(c.key, nil, claims)
}

// Scoped returns a client with the credentials of c that is limited to
// repos ("owner/name"). For an App, each installation token is requested
// with the repository list of its owner, so GitHub itself limits the token.
// A personal access token cannot be limited by the server; the client then
// only refuses to give the token for a repository outside the list. An
// empty list returns c.
func (c *Client) Scoped(repos []string) *Client {
	if len(repos) == 0 {
		return c
	}
	sorted := append([]string(nil), repos...)
	for i := range sorted {
		sorted[i] = strings.ToLower(sorted[i])
	}
	slices.Sort(sorted)
	key := strings.Join(slices.Compact(sorted), ",")
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.scoped[key]; ok {
		return n
	}
	scope := map[string][]string{}
	for _, full := range repos {
		owner, name, ok := strings.Cut(full, "/")
		if !ok || owner == "" || name == "" {
			continue
		}
		k := strings.ToLower(owner)
		scope[k] = append(scope[k], name)
	}
	n := &Client{
		cfg: c.cfg, api: c.api, key: c.key, webURL: c.webURL, now: c.now,
		tokens: map[int64]cachedToken{}, installations: map[string]int64{}, scope: scope,
	}
	if c.scoped == nil {
		c.scoped = map[string]*Client{}
	}
	c.scoped[key] = n
	return n
}

// inScope reports whether owner/repo is a repository of the scope. A call
// for an owner only (repo "") is allowed when the owner has a repository in
// the scope.
func (c *Client) inScope(owner, repo string) bool {
	if c.scope == nil {
		return true
	}
	names, ok := c.scope[strings.ToLower(owner)]
	if !ok {
		return false
	}
	if repo == "" {
		return true
	}
	for _, n := range names {
		if strings.EqualFold(n, repo) {
			return true
		}
	}
	return false
}

// Token returns a credential usable for API and git operations on owner/repo.
// For Apps it is an installation access token (cached until tokenMinLife
// before expiry). A scoped client returns an error that wraps
// core.ErrForbidden for a repository outside its scope.
func (c *Client) Token(ctx context.Context, owner, repo string) (string, error) {
	if !c.inScope(owner, repo) {
		return "", fmt.Errorf("github: %s/%s is not a repository of this factory: %w", owner, repo, core.ErrForbidden)
	}
	if c.key == nil {
		return c.cfg.Token, nil
	}
	inst, err := c.installationID(ctx, owner, repo)
	if err != nil {
		if forgeutil.StatusCode(err) == http.StatusUnauthorized {
			// The app key is not valid. A 404 here only says that the app
			// is not installed for this owner.
			c.authFailure(err)
		}
		return "", err
	}
	c.mu.Lock()
	t, ok := c.tokens[inst]
	c.mu.Unlock()
	if ok && c.now().Before(t.expires.Add(-tokenMinLife)) {
		return t.token, nil
	}
	jwt, err := c.AppJWT()
	if err != nil {
		return "", err
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{}
	if c.scope != nil {
		body["repositories"] = c.scope[strings.ToLower(owner)]
	}
	if err := c.api.Do(withAuth(ctx, "Bearer "+jwt), http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", inst), body, &resp); err != nil {
		c.authFailure(err)
		return "", fmt.Errorf("github: create installation token: %w", err)
	}
	if resp.Token == "" {
		return "", errors.New("github: empty installation token")
	}
	if resp.ExpiresAt.IsZero() {
		resp.ExpiresAt = c.now().Add(55 * time.Minute)
	}
	c.mu.Lock()
	c.tokens[inst] = cachedToken{token: resp.Token, expires: resp.ExpiresAt}
	c.mu.Unlock()
	return resp.Token, nil
}

// authFailure calls OnAuthFailure when GitHub refused the credential:
// 401 (bad app key or revoked token), 403 (suspended installation) or 404
// (removed installation).
func (c *Client) authFailure(err error) {
	if c.cfg.OnAuthFailure == nil {
		return
	}
	switch forgeutil.StatusCode(err) {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		c.cfg.OnAuthFailure(err)
	}
}

func (c *Client) installationID(ctx context.Context, owner, repo string) (int64, error) {
	if c.cfg.InstallationID != 0 {
		return c.cfg.InstallationID, nil
	}
	key := strings.ToLower(owner)
	c.mu.Lock()
	id, ok := c.installations[key]
	c.mu.Unlock()
	if ok {
		return id, nil
	}
	jwt, err := c.AppJWT()
	if err != nil {
		return 0, err
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/installation"
	if repo == "" {
		path = "/users/" + url.PathEscape(owner) + "/installation"
	}
	if err := c.api.Do(withAuth(ctx, "Bearer "+jwt), http.MethodGet, path, nil, &resp); err != nil {
		return 0, fmt.Errorf("github: find app installation for %s: %w", owner, err)
	}
	c.mu.Lock()
	c.installations[key] = resp.ID
	c.mu.Unlock()
	return resp.ID, nil
}

// do performs an authenticated API call scoped to owner/repo.
func (c *Client) do(ctx context.Context, owner, repo, method, path string, in, out any) (http.Header, error) {
	tok, err := c.Token(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	scheme := "Bearer "
	if c.key == nil {
		scheme = "token "
	}
	h, err := c.api.DoRaw(withAuth(ctx, scheme+tok), method, path, in, out)
	if c.key == nil && forgeutil.StatusCode(err) == http.StatusUnauthorized {
		// A personal access token that GitHub refuses was revoked or expired.
		c.authFailure(err)
	}
	return h, err
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// CloneURL implements core.Forge; the URL embeds an x-access-token credential.
func (c *Client) CloneURL(ctx context.Context, owner, repo string) (string, error) {
	tok, err := c.Token(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(c.webURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("x-access-token", tok)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + owner + "/" + repo + ".git"
	return u.String(), nil
}

// DefaultBranch implements core.Forge.
func (c *Client) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := c.do(ctx, owner, repo, http.MethodGet, repoPath(owner, repo), nil, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// PullRequest is the subset of the GitHub pull request object used here.
type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	Merged  bool   `json:"merged"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	User    User   `json:"user"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []User `json:"assignees"`
	Head      Ref    `json:"head"`
	Base      Ref    `json:"base"`
	MergedAt  string `json:"merged_at"`
}

// User is a GitHub account.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Ref is a pull request head or base.
type Ref struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

// LabelNames returns the PR's label names.
func (p *PullRequest) LabelNames() []string {
	out := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		out = append(out, l.Name)
	}
	return out
}

func (p *PullRequest) ref(owner, repo string) *core.PullRequestRef {
	state := p.State
	switch {
	case p.Merged || p.MergedAt != "":
		state = "merged"
	case p.State == "open" && p.Draft:
		state = "draft"
	}
	return &core.PullRequestRef{
		Forge: ForgeName, Repo: owner + "/" + repo, Number: p.Number,
		URL: p.HTMLURL, Branch: p.Head.Ref, State: state,
	}
}

// CreatePullRequest implements core.Forge.
func (c *Client) CreatePullRequest(ctx context.Context, in core.PullRequestInput) (*core.PullRequestRef, error) {
	body := map[string]any{"title": in.Title, "head": in.Head, "base": in.Base, "body": in.Body, "draft": in.Draft}
	if in.Base == "" {
		b, err := c.DefaultBranch(ctx, in.Owner, in.Repo)
		if err != nil {
			return nil, err
		}
		body["base"] = b
	}
	var pr PullRequest
	if _, err := c.do(ctx, in.Owner, in.Repo, http.MethodPost, repoPath(in.Owner, in.Repo)+"/pulls", body, &pr); err != nil {
		return nil, fmt.Errorf("github: create pull request: %w", err)
	}
	return pr.ref(in.Owner, in.Repo), nil
}

// GetPullRequestDetails returns the full pull request object.
func (c *Client) GetPullRequestDetails(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	var pr PullRequest
	if _, err := c.do(ctx, owner, repo, http.MethodGet, fmt.Sprintf("%s/pulls/%d", repoPath(owner, repo), number), nil, &pr); err != nil {
		return nil, err
	}
	return &pr, nil
}

// GetPullRequest implements core.Forge.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*core.PullRequestRef, error) {
	pr, err := c.GetPullRequestDetails(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	return pr.ref(owner, repo), nil
}

// Comment implements core.Forge (issue and pull request conversation comments).
func (c *Client) Comment(ctx context.Context, owner, repo string, number int, body string) error {
	_, err := c.do(ctx, owner, repo, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", repoPath(owner, repo), number),
		map[string]string{"body": body}, nil)
	if err != nil {
		return fmt.Errorf("github: comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// CommentIssue posts on an issue (same endpoint as pull request comments).
func (c *Client) CommentIssue(ctx context.Context, owner, repo string, number int, body string) error {
	return c.Comment(ctx, owner, repo, number, body)
}

// AddLabels adds labels to an issue or pull request.
func (c *Client) AddLabels(ctx context.Context, owner, repo string, number int, labels ...string) error {
	_, err := c.do(ctx, owner, repo, http.MethodPost, fmt.Sprintf("%s/issues/%d/labels", repoPath(owner, repo), number),
		map[string]any{"labels": labels}, nil)
	return err
}

// ListPullRequestFiles returns the paths changed by a pull request (up to 3000, GitHub's limit).
func (c *Client) ListPullRequestFiles(ctx context.Context, owner, repo string, number int) ([]string, error) {
	var out []string
	for page := 1; page <= 30; page++ {
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		path := fmt.Sprintf("%s/pulls/%d/files?per_page=100&page=%d", repoPath(owner, repo), number, page)
		if _, err := c.do(ctx, owner, repo, http.MethodGet, path, nil, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, f.Filename)
			if f.PreviousFilename != "" {
				out = append(out, f.PreviousFilename)
			}
		}
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

// SetCommitStatus implements core.Forge. With App authentication it publishes a
// check run (the App holds checks:write, not statuses:write); with a token it
// uses the commit statuses API.
func (c *Client) SetCommitStatus(ctx context.Context, owner, repo, sha string, st core.CommitStatus) error {
	if c.IsApp() {
		cr := CheckRun{Name: st.Context, HeadSHA: sha, DetailsURL: st.TargetURL, Title: st.Description, Summary: st.Description}
		switch st.State {
		case "pending", "":
			cr.Status = "in_progress"
		case "success":
			cr.Status, cr.Conclusion = "completed", "success"
		case "failure":
			cr.Status, cr.Conclusion = "completed", "failure"
		default: // error
			cr.Status, cr.Conclusion = "completed", "action_required"
		}
		_, err := c.CreateCheckRun(ctx, owner, repo, cr)
		return err
	}
	state := st.State
	if state == "" {
		state = "pending"
	}
	body := map[string]string{
		"state":       state,
		"context":     st.Context,
		"description": forgeutil.Truncate(st.Description, 140),
	}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	_, err := c.do(ctx, owner, repo, http.MethodPost, repoPath(owner, repo)+"/statuses/"+url.PathEscape(sha), body, nil)
	if err != nil {
		return fmt.Errorf("github: set commit status: %w", err)
	}
	return nil
}

// CheckRun describes a check run to create or update.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Status     string // queued, in_progress, completed
	Conclusion string // success, failure, neutral, cancelled, skipped, timed_out, action_required
	DetailsURL string
	ExternalID string
	Title      string
	Summary    string // markdown, up to 65535 chars
	Text       string // markdown details
}

func (cr CheckRun) body(create bool) map[string]any {
	b := map[string]any{}
	if create {
		b["name"] = cr.Name
		b["head_sha"] = cr.HeadSHA
	}
	if cr.Status != "" {
		b["status"] = cr.Status
	}
	if cr.Conclusion != "" {
		b["conclusion"] = cr.Conclusion
		b["completed_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	if cr.DetailsURL != "" {
		b["details_url"] = cr.DetailsURL
	}
	if cr.ExternalID != "" {
		b["external_id"] = cr.ExternalID
	}
	if cr.Title != "" || cr.Summary != "" {
		title := cr.Title
		if title == "" {
			title = cr.Name
		}
		out := map[string]string{"title": forgeutil.Truncate(title, 255), "summary": forgeutil.Truncate(cr.Summary, 65535)}
		if cr.Text != "" {
			out["text"] = forgeutil.Truncate(cr.Text, 65535)
		}
		b["output"] = out
	}
	return b
}

// CreateCheckRun creates a check run and returns its ID.
func (c *Client) CreateCheckRun(ctx context.Context, owner, repo string, cr CheckRun) (int64, error) {
	var resp struct {
		ID int64 `json:"id"`
	}
	if _, err := c.do(ctx, owner, repo, http.MethodPost, repoPath(owner, repo)+"/check-runs", cr.body(true), &resp); err != nil {
		return 0, fmt.Errorf("github: create check run: %w", err)
	}
	return resp.ID, nil
}

// UpdateCheckRun updates an existing check run.
func (c *Client) UpdateCheckRun(ctx context.Context, owner, repo string, id int64, cr CheckRun) error {
	_, err := c.do(ctx, owner, repo, http.MethodPatch, fmt.Sprintf("%s/check-runs/%d", repoPath(owner, repo), id), cr.body(false), nil)
	if err != nil {
		return fmt.Errorf("github: update check run: %w", err)
	}
	return nil
}

// maxOpenCheckRuns bounds the cache of check runs that are in progress.
const maxOpenCheckRuns = 256

func checkRunKey(owner, repo, sha, name string) string {
	return strings.ToLower(owner + "/" + repo + "@" + sha + "#" + name)
}

// PublishCheckRun publishes a check run for a commit. A run with the status
// completed updates the run that is in progress for the same commit and
// name (the client keeps its ID, or finds it on GitHub), and creates one if
// there is none. A client without App authentication cannot write check
// runs, so it sets a commit status with the title as description instead.
// The server uses this method for the check of a definition pull request.
func (c *Client) PublishCheckRun(ctx context.Context, owner, repo string, cr core.CheckRun) error {
	if !c.IsApp() {
		st := core.CommitStatus{Context: cr.Name, TargetURL: cr.DetailsURL, Description: cr.Title}
		switch {
		case cr.Status != "completed":
			st.State = "pending"
		case cr.Conclusion == "success":
			st.State = "success"
		default:
			st.State = "failure"
		}
		return c.SetCommitStatus(ctx, owner, repo, cr.HeadSHA, st)
	}
	gr := CheckRun{Name: cr.Name, HeadSHA: cr.HeadSHA, Status: cr.Status, Conclusion: cr.Conclusion,
		DetailsURL: cr.DetailsURL, Title: cr.Title, Summary: cr.Summary, Text: cr.Text}
	if gr.Status == "" {
		gr.Status = "in_progress"
	}
	key := checkRunKey(owner, repo, cr.HeadSHA, cr.Name)
	if gr.Status != "completed" {
		id, err := c.CreateCheckRun(ctx, owner, repo, gr)
		if err != nil {
			return err
		}
		c.mu.Lock()
		if c.openRuns == nil || len(c.openRuns) >= maxOpenCheckRuns {
			c.openRuns = map[string]int64{}
		}
		c.openRuns[key] = id
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	id := c.openRuns[key]
	delete(c.openRuns, key)
	c.mu.Unlock()
	if id == 0 {
		id = c.findCheckRun(ctx, owner, repo, cr.HeadSHA, cr.Name)
	}
	if id != 0 {
		if err := c.UpdateCheckRun(ctx, owner, repo, id, gr); err == nil {
			return nil
		} else if forgeutil.StatusCode(err) != http.StatusNotFound {
			return err
		}
	}
	_, err := c.CreateCheckRun(ctx, owner, repo, gr)
	return err
}

// findCheckRun returns the ID of the latest check run of this App with the
// given name on a commit, or 0. Errors give 0: the caller creates a new run.
func (c *Client) findCheckRun(ctx context.Context, owner, repo, sha, name string) int64 {
	var resp struct {
		CheckRuns []struct {
			ID  int64 `json:"id"`
			App struct {
				ID int64 `json:"id"`
			} `json:"app"`
		} `json:"check_runs"`
	}
	path := fmt.Sprintf("%s/commits/%s/check-runs?filter=latest&check_name=%s", repoPath(owner, repo), url.PathEscape(sha), url.QueryEscape(name))
	if _, err := c.do(ctx, owner, repo, http.MethodGet, path, nil, &resp); err != nil {
		return 0
	}
	appID, _ := strconv.ParseInt(c.cfg.AppID, 10, 64)
	for _, r := range resp.CheckRuns {
		if appID == 0 || r.App.ID == appID {
			return r.ID
		}
	}
	return 0
}

// GetFiles implements core.Forge: it reads every blob under path at ref using the
// git trees and blobs APIs. Returned keys are relative to path.
func (c *Client) GetFiles(ctx context.Context, owner, repo, ref, path string) (map[string]string, string, error) {
	if ref == "" {
		b, err := c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return nil, "", err
		}
		ref = b
	}
	var commit struct {
		SHA    string `json:"sha"`
		Commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
	}
	if _, err := c.do(ctx, owner, repo, http.MethodGet, repoPath(owner, repo)+"/commits/"+url.PathEscape(ref), nil, &commit); err != nil {
		return nil, "", fmt.Errorf("github: resolve %s: %w", ref, err)
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if _, err := c.do(ctx, owner, repo, http.MethodGet, repoPath(owner, repo)+"/git/trees/"+commit.Commit.Tree.SHA+"?recursive=1", nil, &tree); err != nil {
		return nil, "", fmt.Errorf("github: read tree: %w", err)
	}
	if tree.Truncated {
		return nil, "", errors.New("github: repository tree too large (truncated by GitHub)")
	}
	prefix := strings.Trim(path, "/")
	if prefix != "" {
		prefix += "/"
	}
	files := map[string]string{}
	for _, e := range tree.Tree {
		if e.Type != "blob" || !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		if len(files) >= MaxTreeFiles {
			return nil, "", fmt.Errorf("github: more than %d files under %q", MaxTreeFiles, path)
		}
		if e.Size > MaxFileBytes {
			return nil, "", fmt.Errorf("github: %s exceeds %d bytes", e.Path, MaxFileBytes)
		}
		var blob struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		if _, err := c.do(ctx, owner, repo, http.MethodGet, repoPath(owner, repo)+"/git/blobs/"+e.SHA, nil, &blob); err != nil {
			return nil, "", fmt.Errorf("github: read %s: %w", e.Path, err)
		}
		content := blob.Content
		if blob.Encoding == "base64" {
			b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(blob.Content, "\n", ""))
			if err != nil {
				return nil, "", fmt.Errorf("github: decode %s: %w", e.Path, err)
			}
			content = string(b)
		}
		files[strings.TrimPrefix(e.Path, prefix)] = content
	}
	return files, commit.SHA, nil
}

// ParseInstallationID parses a decimal installation id ("" -> 0).
func ParseInstallationID(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(strings.TrimSpace(s), 10, 64)
}
