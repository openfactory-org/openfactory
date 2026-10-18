package api

import (
	"sync"
	"testing"
)

// A changed secret or integration record must drop the cached forge clients
// of the team. Else a rotated token applies only after the cache expires.
func TestForgeCredentialsChangedOnSecretAndIntegrationWrites(t *testing.T) {
	var mu sync.Mutex
	var teams []string
	e := newTestEnv(t, func(o *Options) {
		o.ForgeCredentialsChanged = func(teamID string) {
			mu.Lock()
			defer mu.Unlock()
			teams = append(teams, teamID)
		}
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(teams)
	}
	step := func(name string, want int) {
		t.Helper()
		if got := count(); got != want {
			t.Fatalf("%s: %d calls, want %d", name, got, want)
		}
	}

	e.must(e.do("POST", "/secrets", e.editor.Key, map[string]any{"name": "GH_TOKEN", "value": "one"}), 201)
	step("secret create", 1)
	e.must(e.do("PUT", "/secrets/GH_TOKEN", e.editor.Key, map[string]any{"value": "two"}), 200)
	step("secret update", 2)
	e.must(e.do("PUT", "/secrets/MISSING", e.editor.Key, map[string]any{"value": "x"}), 404)
	step("failed update", 2)
	in := e.must(e.do("POST", "/integrations", e.admin.Key, map[string]any{"provider": "github", "secret_ref": "GH_TOKEN", "config": map[string]string{"org": "acme"}}), 201)
	step("integration create", 3)
	iid := str(in.Body, "id")
	e.must(e.do("PATCH", "/integrations/"+iid, e.admin.Key, map[string]any{"secret_ref": "GH_TOKEN"}), 200)
	step("integration update", 4)
	e.must(e.do("DELETE", "/integrations/"+iid, e.admin.Key, nil), 204)
	step("integration delete", 5)
	e.must(e.do("DELETE", "/secrets/GH_TOKEN", e.editor.Key, nil), 204)
	step("secret delete", 6)

	mu.Lock()
	defer mu.Unlock()
	for _, id := range teams {
		if id != e.team.ID {
			t.Fatalf("invalidated team %q, want %q", id, e.team.ID)
		}
	}
}
