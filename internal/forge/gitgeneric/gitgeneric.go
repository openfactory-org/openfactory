// Package gitgeneric implements core.Forge for "other code forges" (Bitbucket,
// Azure DevOps Server, self-managed GitLab, Gitea, plain git servers...) using
// plain git over HTTPS or SSH.
//
// Only repository access is supported: CloneURL, DefaultBranch and GetFiles.
// Pull request, comment and commit-status operations return ErrUnsupported.
//
// Such forges deliver no provider-specific events or automations. To start
// factory work from their activity, configure the forge's own webhook to POST
// to a custom Open Factory webhook (webhooks/<name>.yaml, authMode token,
// url_token or signature) and add a `provider: webhook` / `event: received`
// automation with a payload filter. Alternatively start work from Slack,
// Teams, Linear, Jira, the factory API, Factory MCP or a schedule.
package gitgeneric

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// ForgeName is the code-forge identifier for plain git repositories.
const ForgeName = "GIT"

// ErrUnsupported is returned for operations a plain git remote cannot perform.
var ErrUnsupported = errors.New("operation not supported for generic git forges: they provide repository access only; use a custom webhook for events and open pull requests in the forge's own UI or API")

// Limits applied by GetFiles.
const (
	MaxTreeFiles = 2000
	MaxFileBytes = 1 << 20
)

// Config configures a Client.
type Config struct {
	// BaseURL is the clone URL prefix, e.g. https://bitbucket.org, https://SERVER/COLLECTION
	// or ssh://git@host:2222. Repositories resolve to BaseURL/owner/repo(.git).
	// For Azure DevOps Server use a template (see URLTemplate).
	BaseURL string
	// URLTemplate overrides BaseURL with "{owner}" and "{repo}" placeholders,
	// e.g. "https://SERVER/COLLECTION/{owner}/_git/{repo}".
	URLTemplate string
	// Username for HTTPS credentials (default "x-token-auth"; Bitbucket Cloud
	// uses "x-bitbucket-api-token-auth").
	Username string
	// Token is the HTTPS password/token (CODE_FORGE_TOKEN). Empty for SSH or public repositories.
	Token string
	// AppendGitSuffix adds ".git" to BaseURL-derived URLs (default true when URLTemplate is empty).
	NoGitSuffix bool
	// GitBinary is the git executable (default "git").
	GitBinary string
	// Env is extra environment for git (e.g. GIT_SSH_COMMAND).
	Env []string
}

// Client implements core.Forge for plain git remotes.
type Client struct{ cfg Config }

var _ core.Forge = (*Client)(nil)

// New creates a client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" && cfg.URLTemplate == "" {
		return nil, errors.New("gitgeneric: base_url or url_template is required")
	}
	if cfg.GitBinary == "" {
		cfg.GitBinary = "git"
	}
	if cfg.Username == "" {
		cfg.Username = "x-token-auth"
	}
	return &Client{cfg: cfg}, nil
}

// Name implements core.Forge.
func (c *Client) Name() string { return ForgeName }

// RemoteURL returns the repository URL without credentials.
func (c *Client) RemoteURL(owner, repo string) string {
	if c.cfg.URLTemplate != "" {
		return strings.NewReplacer("{owner}", owner, "{repo}", repo).Replace(c.cfg.URLTemplate)
	}
	base := strings.TrimRight(c.cfg.BaseURL, "/")
	sep := "/"
	if strings.HasSuffix(base, ":") { // scp-like "git@host:"
		sep = ""
	}
	u := base + sep + strings.Trim(owner+"/"+repo, "/")
	if !c.cfg.NoGitSuffix && !strings.HasSuffix(u, ".git") {
		u += ".git"
	}
	return u
}

// CloneURL implements core.Forge; HTTPS URLs embed the configured credentials.
func (c *Client) CloneURL(_ context.Context, owner, repo string) (string, error) {
	raw := c.RemoteURL(owner, repo)
	if c.cfg.Token == "" || (!strings.HasPrefix(raw, "https://") && !strings.HasPrefix(raw, "http://")) {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("gitgeneric: parse clone url: %w", err)
	}
	u.User = url.UserPassword(c.cfg.Username, c.cfg.Token)
	return u.String(), nil
}

func (c *Client) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.cfg.GitBinary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, c.cfg.Env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if c.cfg.Token != "" {
			msg = strings.ReplaceAll(msg, c.cfg.Token, "***")
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return stdout.String(), nil
}

// DefaultBranch implements core.Forge using `git ls-remote --symref <url> HEAD`.
func (c *Client) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	u, err := c.CloneURL(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	out, err := c.git(ctx, "", "ls-remote", "--symref", u, "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ref: ") {
			f := strings.Fields(strings.TrimPrefix(line, "ref: "))
			if len(f) > 0 {
				return strings.TrimPrefix(f[0], "refs/heads/"), nil
			}
		}
	}
	return "", errors.New("gitgeneric: remote did not advertise a default branch")
}

// CreatePullRequest is unsupported.
func (c *Client) CreatePullRequest(context.Context, core.PullRequestInput) (*core.PullRequestRef, error) {
	return nil, ErrUnsupported
}

// GetPullRequest is unsupported.
func (c *Client) GetPullRequest(context.Context, string, string, int) (*core.PullRequestRef, error) {
	return nil, ErrUnsupported
}

// Comment is unsupported.
func (c *Client) Comment(context.Context, string, string, int, string) error { return ErrUnsupported }

// SetCommitStatus is unsupported.
func (c *Client) SetCommitStatus(context.Context, string, string, string, core.CommitStatus) error {
	return ErrUnsupported
}

var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// GetFiles implements core.Forge with a shallow fetch into a temporary repository.
// Keys are relative to path.
func (c *Client) GetFiles(ctx context.Context, owner, repo, ref, path string) (map[string]string, string, error) {
	u, err := c.CloneURL(ctx, owner, repo)
	if err != nil {
		return nil, "", err
	}
	if ref == "" {
		if ref, err = c.DefaultBranch(ctx, owner, repo); err != nil {
			return nil, "", err
		}
	}
	dir, err := os.MkdirTemp("", "of-gitgeneric-*")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	if _, err := c.git(ctx, dir, "init", "-q"); err != nil {
		return nil, "", err
	}
	fetchRef := ref
	if !shaRe.MatchString(ref) && !strings.HasPrefix(ref, "refs/") {
		fetchRef = "refs/heads/" + ref
	}
	if _, err := c.git(ctx, dir, "fetch", "-q", "--depth", "1", u, fetchRef); err != nil {
		return nil, "", err
	}
	if _, err := c.git(ctx, dir, "-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"); err != nil {
		return nil, "", err
	}
	sha, err := c.git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, "", err
	}
	// The checkout is untrusted: read it through os.Root, so that neither ".."
	// in path nor a symbolic link in the repository reaches outside it.
	checkout, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	defer checkout.Close()
	root := pathpkg.Clean(strings.Trim(path, "/")) // "" becomes "."
	files := map[string]string{}
	err = fs.WalkDir(checkout.FS(), root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(files) >= MaxTreeFiles {
			return fmt.Errorf("gitgeneric: more than %d files under %q", MaxTreeFiles, path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > MaxFileBytes {
			return fmt.Errorf("gitgeneric: %s exceeds %d bytes", p, MaxFileBytes)
		}
		b, err := checkout.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return files, strings.TrimSpace(sha), nil
}

// ReplyInThread is unsupported: a plain git remote has no review threads.
func (c *Client) ReplyInThread(context.Context, string, string, int, string, string) error {
	return core.ErrOperationNotSupported
}

// ResolveThread is unsupported.
func (c *Client) ResolveThread(context.Context, string, string, int, string) error {
	return core.ErrOperationNotSupported
}

// SubmitReview is unsupported.
func (c *Client) SubmitReview(context.Context, core.ReviewInput) error {
	return core.ErrOperationNotSupported
}

// MarkReady is unsupported.
func (c *Client) MarkReady(context.Context, string, string, int) error {
	return core.ErrOperationNotSupported
}
