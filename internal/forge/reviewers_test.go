package forge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
)

// TestRequestReview shows that a review is requested from the forge login of
// a team member, read with the personal token of the member, and that a
// member without a linked account gets an external-authentication error.
func TestRequestReview(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user":
			if r.Header.Get("Authorization") != "token alice-pat" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "alice-dev"})
		case "/repos/acme/def/pulls/7/requested_reviewers":
			var body struct {
				Reviewers []string `json:"reviewers"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Authorization") != "token team-gh" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mu.Lock()
			requested = append(requested, body.Reviewers...)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "i1", TeamID: "team_1", Provider: "github", Config: map[string]string{"base_url": srv.URL, "org": "acme"}, SecretRef: "GH_TOKEN"}}
	ut := &UserTokens{Store: store, Cipher: fakeCipher{}}
	r := NewResolver(store, forgetest.NewSecrets(map[string]string{"GH_TOKEN": "team-gh"}), Options{UserTokens: ut})
	if err := ut.Set(ctx, "team_1", "usr_alice", "GITHUB", "alice-pat"); err != nil {
		t.Fatal(err)
	}
	pr := core.PullRequestRef{Forge: "GITHUB", Repo: "acme/def", Number: 7, URL: "https://github.com/acme/def/pull/7"}
	if err := r.RequestReview(ctx, "team_1", pr, "usr_alice"); err != nil {
		t.Fatal(err)
	}
	if len(requested) != 1 || requested[0] != "alice-dev" {
		t.Fatalf("requested %v", requested)
	}
	if err := r.RequestReview(ctx, "team_1", pr, "usr_bob"); !core.IsExternalAuthRequired(err) {
		t.Fatalf("no linked account: %v", err)
	}
	if err := r.RequestReview(ctx, "team_1", core.PullRequestRef{Repo: "nope"}, "usr_alice"); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("bad pull request: %v", err)
	}
}
