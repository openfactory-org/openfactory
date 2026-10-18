package runnerapi

import (
	"context"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func TestResolveStartCommits(t *testing.T) {
	ctx := context.Background()
	st := testdb.New(t)
	s := &Service{store: st}
	pinned := strings.Repeat("a", 40)
	head := strings.Repeat("c", 40)
	if err := core.SaveStartCommits(ctx, st, "run_1", []core.RepoStart{
		{CodeForge: "GITHUB", Owner: "acme", Repo: "api", Path: "api", Ref: pinned, Resolved: true},
		{CodeForge: "GITHUB", Owner: "acme", Repo: "web", Path: "web", Ref: "main"},
	}); err != nil {
		t.Fatal(err)
	}
	err := s.resolveStartCommits(ctx, "run_1", []runnerproto.RepositoryCommit{{Path: "api", Commit: head}, {Path: "web", Commit: head}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := core.LoadStartCommits(ctx, st, "run_1")
	if got[0].Ref != pinned || got[1].Ref != head || !got[1].Resolved {
		t.Fatalf("start commits %+v", got)
	}
	// A later session does not change a resolved commit.
	_ = s.resolveStartCommits(ctx, "run_1", []runnerproto.RepositoryCommit{{Path: "web", Commit: strings.Repeat("d", 40)}})
	if again, _ := core.LoadStartCommits(ctx, st, "run_1"); again[1].Ref != head {
		t.Fatalf("resolved commit changed: %+v", again)
	}
}
