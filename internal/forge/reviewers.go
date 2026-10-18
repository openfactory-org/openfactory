package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// RequestReview asks a team member for a review of a pull request.
//
// The forge login of the member comes from the personal token that the member
// linked in the dashboard. The request goes through the credential of the
// team record of the owner of the repository. The call
// fails with an error that wraps core.ErrExternalAuthRequired when the member
// has no linked account, and with core.ErrOperationNotSupported when the
// forge cannot do it. GitHub does it
// itself. GitLab and Azure DevOps do it through their plugins, with the Login
// and RequestReviewers operations of the forge service. The login is a GitHub
// or GitLab username, or an Azure DevOps identity ID.
func (r *Resolver) RequestReview(ctx context.Context, teamID string, pr core.PullRequestRef, userID string) error {
	codeForge := pr.Forge
	if codeForge == "" {
		codeForge = GitHub
	}
	owner, repo, ok := strings.Cut(pr.Repo, "/")
	if !ok || owner == "" || repo == "" || pr.Number <= 0 {
		return fmt.Errorf("%w: pull request %q needs a repository (owner/name) and a number", core.ErrInvalid, pr.URL)
	}
	l := core.ForgeLookup{TeamID: teamID, CodeForge: codeForge, Repo: pr.Repo}
	uf, _, err := r.ForgeForUser(ctx, l, userID)
	if err != nil {
		return err
	}
	lr, ok := uf.(core.LoginReader)
	if !ok {
		return fmt.Errorf("%w: %s cannot read the login of a user", core.ErrOperationNotSupported, strings.ToLower(codeForge))
	}
	login, err := lr.Login(ctx)
	if err != nil {
		if errors.Is(err, core.ErrOperationNotSupported) {
			return err
		}
		return fmt.Errorf("read the forge login of user %s: %w", userID, err)
	}
	tf, err := r.Forge(ctx, l)
	if err != nil {
		return err
	}
	rr, ok := tf.(core.ReviewRequester)
	if !ok {
		return fmt.Errorf("%w: %s cannot request a review", core.ErrOperationNotSupported, strings.ToLower(codeForge))
	}
	return rr.RequestReviewers(ctx, owner, repo, pr.Number, []string{login})
}
