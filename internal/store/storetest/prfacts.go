package storetest

import (
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// testPullRequestFacts checks core.PullRequestFactStore.
func testPullRequestFacts(t *testing.T, s core.Store) {
	ps, has := s.(core.PullRequestFactStore)
	if !has {
		t.Skip("store has no pull request facts")
	}
	opened := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	adds, dels := 12, 3
	f := &core.PullRequestFacts{
		TeamID: "team_a", Key: "github:acme/api#4", Provider: "github", Repo: "Acme/API", Number: 4, HeadBranch: "fix",
		Opener: "of[bot]", OpenerKind: core.PushByAgent, OpenedAt: &opened, Additions: &adds, Deletions: &dels,
		Pushes:    []core.PullRequestPush{{At: opened.Add(time.Hour), Actor: "dana", Kind: core.PushByPerson, SHA: "abc"}},
		UpdatedAt: opened,
	}
	ok(t, ps.PutPullRequestFacts(ctx, f))
	eq(t, f.ID, core.PullRequestFactsID("team_a", "github:acme/api#4"), "id")
	other := &core.PullRequestFacts{TeamID: "team_a", Key: "github:acme/api#5", Provider: "github", Repo: "acme/api", Number: 5, HeadBranch: "other"}
	ok(t, ps.PutPullRequestFacts(ctx, other))
	ok(t, ps.PutPullRequestFacts(ctx, &core.PullRequestFacts{TeamID: "team_b", Key: "github:acme/api#4", Provider: "github", Repo: "acme/api", Number: 4}))

	got, err := ps.GetPullRequestFacts(ctx, f.ID)
	ok(t, err)
	if got.Repo != "Acme/API" || got.OpenerKind != core.PushByAgent || len(got.Pushes) != 1 || got.Pushes[0].Kind != core.PushByPerson ||
		got.Additions == nil || *got.Additions != 12 || got.OpenedAt == nil || !got.OpenedAt.Equal(opened) {
		t.Fatalf("facts = %+v", got)
	}
	if _, err := ps.GetPullRequestFacts(ctx, "team_a|nothing"); err == nil {
		t.Fatal("missing facts found")
	}

	list, err := ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{TeamID: "team_a"})
	ok(t, err)
	eq(t, len(list), 2, "team list")
	list, err = ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{TeamID: "team_a", Keys: []string{"github:acme/api#4"}})
	ok(t, err)
	eq(t, len(list), 1, "list by key")
	list, err = ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{TeamID: "team_a", Repo: "acme/api", HeadBranch: "fix", OpenOnly: true})
	ok(t, err)
	eq(t, len(list), 1, "open by branch")

	// A merge replaces the row; the merged pull request is no longer open.
	merged := opened.Add(2 * time.Hour)
	got.MergedAt = &merged
	ok(t, ps.PutPullRequestFacts(ctx, got))
	list, err = ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{TeamID: "team_a", HeadBranch: "fix", OpenOnly: true})
	ok(t, err)
	eq(t, len(list), 0, "open after merge")
	list, err = ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{TeamID: "team_a", HeadBranch: "fix"})
	ok(t, err)
	if len(list) != 1 || list[0].MergedAt == nil {
		t.Fatalf("after merge = %+v", list)
	}
}
