package integrations

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
)

// The "no replace" check of a link is atomic on the real stores. Two
// users who link the same external account at the same moment give one owner,
// and the other one gets ErrLinkedToOther.
func TestCreateLinkIsAtomicOnRealStores(t *testing.T) {
	stores := map[string]func(t *testing.T) core.Store{
		"sqlite": func(t *testing.T) core.Store {
			s, err := sqlite.OpenStore(context.Background(), filepath.Join(t.TempDir(), "of.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			if _, ok := st.(core.IdentityLinker); !ok {
				t.Fatalf("%s store has no atomic link", name)
			}
			ctx := context.Background()
			for round := 0; round < 20; round++ {
				ext := core.NewID("U")
				users := []string{"usr_a", "usr_b", "usr_c", "usr_d"}
				errs := make([]error, len(users))
				var wg sync.WaitGroup
				start := make(chan struct{})
				for i, u := range users {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						errs[i] = createLink(ctx, st, &core.ExternalIdentity{UserID: u, Provider: "slack", ExternalID: ext, Handle: u})
					}()
				}
				close(start)
				wg.Wait()
				wins := 0
				for _, err := range errs {
					switch {
					case err == nil:
						wins++
					case !errors.Is(err, ErrLinkedToOther):
						t.Fatalf("unexpected error: %v", err)
					}
				}
				if wins != 1 {
					t.Fatalf("round %d: %d links succeeded, want 1 (%v)", round, wins, errs)
				}
				x, err := st.FindExternalIdentity(ctx, "slack", ext)
				if err != nil {
					t.Fatal(err)
				}
				for i, u := range users {
					if (errs[i] == nil) != (x.UserID == u) {
						t.Fatalf("round %d: owner %q does not match the winner", round, x.UserID)
					}
				}
			}
		})
	}
}
