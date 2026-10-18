package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// RequestReviewers asks users for a review of a pull request. It implements
// core.ReviewRequester. A login that cannot review (for example the author of
// the pull request) makes GitHub answer with status 422.
func (c *Client) RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error {
	if len(logins) == 0 {
		return nil
	}
	path := repoPath(owner, repo) + "/pulls/" + strconv.Itoa(number) + "/requested_reviewers"
	if _, err := c.do(ctx, owner, repo, http.MethodPost, path, map[string]any{"reviewers": logins}, nil); err != nil {
		return fmt.Errorf("github: request review of %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// Login returns the login of the account of the credential. It implements
// core.LoginReader and works for a personal token, not for a GitHub App.
func (c *Client) Login(ctx context.Context) (string, error) {
	if c.key != nil {
		return "", errors.New("github: an app credential has no user login")
	}
	var out struct {
		Login string `json:"login"`
	}
	if _, err := c.do(ctx, "", "", http.MethodGet, "/user", nil, &out); err != nil {
		return "", fmt.Errorf("github: read the user of the token: %w", err)
	}
	if strings.TrimSpace(out.Login) == "" {
		return "", errors.New("github: the token has no user login")
	}
	return strings.TrimSpace(out.Login), nil
}
