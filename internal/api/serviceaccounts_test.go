package api

import (
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestServiceAccounts covers service accounts: only an admin manages
// them, their keys start with ofa_, a CI job starts a run with a key that
// belongs to no person, and a delete removes the keys.
func TestServiceAccounts(t *testing.T) {
	e := newTestEnv(t)

	e.must(e.do("POST", "/service-accounts", e.editor.Key, map[string]any{"name": "ci"}), 403)
	e.must(e.do("GET", "/service-accounts", e.editor.Key, nil), 403)
	e.must(e.do("POST", "/service-accounts", e.admin.Key, map[string]any{"name": "has space"}), 400)
	e.must(e.do("POST", "/service-accounts", e.admin.Key, map[string]any{"name": "ci", "role": "owner"}), 400)

	c := e.must(e.do("POST", "/service-accounts", e.admin.Key, map[string]any{"name": "ci", "role": "editor", "create_api_key": true}), 201)
	sa, _ := c.Body["service_account"].(map[string]any)
	id := str(sa, "id")
	key := str(c.Body, "key")
	if !strings.HasPrefix(id, "sa_") || !strings.HasPrefix(key, ServiceAccountKeyPrefix) || sa["role"] != "editor" {
		t.Fatal(c.Body)
	}
	e.must(e.do("POST", "/service-accounts", e.admin.Key, map[string]any{"name": "ci"}), 409)
	e.must(e.do("POST", "/service-accounts", e.admin.Key, map[string]any{"name": "bot"}), 201) // a second account without an email

	// Service accounts are not listed as users, and the user key routes
	// refuse them.
	for _, u := range list(e.must(e.do("GET", "/users", e.admin.Key, nil), 200).Body, "items") {
		if m, _ := u.(map[string]any); strings.HasPrefix(str(m, "id"), "sa_") {
			t.Fatal("a service account is listed as a user")
		}
	}
	if l := list(e.must(e.do("GET", "/service-accounts", e.admin.Key, nil), 200).Body, "items"); len(l) != 2 {
		t.Fatal(l)
	}
	e.must(e.do("POST", "/api-keys", e.admin.Key, map[string]any{"name": "x", "user_id": id}), 400)

	// The key works on the API with the role of the account.
	me := e.must(e.do("GET", "/me", key, nil), 200)
	if !strings.Contains(string(me.Raw), id) {
		t.Fatal(me.Body)
	}
	e.must(e.do("POST", "/service-accounts", key, map[string]any{"name": "x"}), 403) // editor role
	e.must(e.do("POST", "/api-keys", key, map[string]any{"name": "mine"}), 403)

	// A CI job starts a run: its creator and run_as are the service account.
	fid := e.createFactory("nightly")
	run := e.must(e.do("POST", "/runs", key, runBody(fid, "Run the nightly checks.")), 201)
	for _, f := range []string{"creator", "run_as"} {
		p, _ := run.Body[f].(map[string]any)
		if p["type"] != string(core.PrincipalServiceAccount) || p["id"] != id {
			t.Fatalf("%s = %v", f, run.Body[f])
		}
	}
	e.must(e.do("POST", "/runs", e.editor.Key, runBody(fid, "Other work.")), 201)
	byCI := list(e.must(e.do("GET", "/runs?creator="+id, e.viewer.Key, nil), 200).Body, "items")
	if len(byCI) != 1 {
		t.Fatalf("creator filter: %v", byCI)
	}

	// Keys: list, create, delete.
	k2 := e.must(e.do("POST", "/service-accounts/ci/keys", e.admin.Key, map[string]any{"name": "deploy"}), 201)
	if !strings.HasPrefix(str(k2.Body, "key"), ServiceAccountKeyPrefix) {
		t.Fatal(k2.Body)
	}
	if l := list(e.must(e.do("GET", "/service-accounts/"+id+"/keys", e.admin.Key, nil), 200).Body, "items"); len(l) != 2 {
		t.Fatal(l)
	}
	k2id := str(k2.Body["api_key"].(map[string]any), "id")
	e.must(e.do("DELETE", "/service-accounts/"+id+"/keys/"+k2id, e.admin.Key, nil), 204)
	e.must(e.do("DELETE", "/service-accounts/"+id+"/keys/"+k2id, e.admin.Key, nil), 404)

	// A user key cannot pretend to be a service account key.
	e.must(e.do("GET", "/me", ServiceAccountKeyPrefix+strings.TrimPrefix(e.viewer.Key, KeyPrefix), nil), 401)

	// Change the role.
	if u := e.must(e.do("PATCH", "/service-accounts/"+id, e.admin.Key, map[string]any{"role": "user"}), 200); u.Body["role"] != "user" {
		t.Fatal(u.Body)
	}

	// A delete removes the account and its keys.
	e.must(e.do("DELETE", "/service-accounts/"+id, e.admin.Key, nil), 204)
	e.must(e.do("GET", "/service-accounts/"+id, e.admin.Key, nil), 404)
	e.must(e.do("GET", "/me", key, nil), 401)
}
