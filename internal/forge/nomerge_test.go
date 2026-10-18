package forge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/gitgeneric"
	"github.com/openfactory-org/openfactory/internal/forge/github"
)

// TestRunCredentialCannotMerge checks the rule that agents never merge
// on the forge side. The forge credential of a run is made by
// Forge.CloneURL. It can push a branch, and the platform opens the pull
// request with CreatePullRequest. No platform code path merges: the Forge
// interface and its built-in clients have no merge or auto-merge operation,
// so the credential of a run has no platform call that merges through the
// forge API. A merge that an agent sends to the forge API by hand is limited
// by branch rules, not by the platform (docs/security.md, "Agents never
// merge").
func TestRunCredentialCannotMerge(t *testing.T) {
	var types = []reflect.Type{
		reflect.TypeOf((*core.Forge)(nil)).Elem(),
		reflect.TypeOf((*github.Client)(nil)),
		reflect.TypeOf((*gitgeneric.Client)(nil)),
	}
	for _, typ := range types {
		for i := 0; i < typ.NumMethod(); i++ {
			name := typ.Method(i).Name
			lower := strings.ToLower(name)
			if strings.Contains(lower, "merge") || strings.Contains(lower, "enqueue") || strings.Contains(lower, "accept") {
				t.Errorf("%s has the method %s: a forge client of a run must not merge", typ, name)
			}
		}
	}
	// The interface still has the operations that an agent run needs.
	if _, ok := types[0].MethodByName("CreatePullRequest"); !ok {
		t.Fatal("core.Forge must open pull requests")
	}
}

// TestGenericGitCredentialHasNoAPI checks that the generic git forge has no
// forge API behind its credential: it clones and pushes, and it refuses the
// pull request operations.
func TestGenericGitCredentialHasNoAPI(t *testing.T) {
	c, err := gitgeneric.New(gitgeneric.Config{BaseURL: "https://git.example.test", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreatePullRequest(context.Background(), core.PullRequestInput{Owner: "o", Repo: "r", Head: "h", Base: "b"}); !errors.Is(err, gitgeneric.ErrUnsupported) {
		t.Fatalf("CreatePullRequest = %v, want ErrUnsupported", err)
	}
	if _, err := c.GetPullRequest(context.Background(), "o", "r", 1); !errors.Is(err, gitgeneric.ErrUnsupported) {
		t.Fatalf("GetPullRequest = %v, want ErrUnsupported", err)
	}
}
