package github

import (
	"context"
	"testing"
)

func TestRequestReviewersAndLogin(t *testing.T) {
	c, calls := reviewServer(t, func(r recorded) any {
		if r.Path == "/user" {
			return map[string]any{"login": "octo-lead"}
		}
		return nil
	})
	ctx := context.Background()
	if err := c.RequestReviewers(ctx, "acme", "api", 7, []string{"octo-lead"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RequestReviewers(ctx, "acme", "api", 7, nil); err != nil {
		t.Fatal(err)
	}
	login, err := c.Login(ctx)
	if err != nil || login != "octo-lead" {
		t.Fatalf("login %q %v", login, err)
	}
	got := *calls
	if len(got) != 2 {
		t.Fatalf("calls %+v", got)
	}
	if got[0].Method != "POST" || got[0].Path != "/repos/acme/api/pulls/7/requested_reviewers" {
		t.Fatalf("request call %+v", got[0])
	}
	if rs, _ := got[0].Body["reviewers"].([]any); len(rs) != 1 || rs[0] != "octo-lead" {
		t.Fatalf("reviewers %+v", got[0].Body)
	}
}
