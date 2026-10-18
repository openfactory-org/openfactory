package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/openfactory-org/openfactory/internal/api/apitest"
	"github.com/openfactory-org/openfactory/internal/core"
)

// seedWithFactoryRoles adds a team member with a team-wide role and role
// overrides for single factories.
func (e *testEnv) seedWithFactoryRoles(role core.Role, overrides map[string]core.Role) apitest.Identity {
	e.t.Helper()
	ctx := context.Background()
	id, err := apitest.SeedUser(ctx, e.store, e.team.ID, role, NewAPIKey)
	if err != nil {
		e.t.Fatal(err)
	}
	id.User.FactoryRoles = overrides
	if err := e.store.UpdateUser(ctx, id.User); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestFactoryRoleOverride(t *testing.T) {
	e := newTestEnv(t)
	f1 := e.createFactory("one")
	f2 := e.createFactory("two")
	files := apitest.MinimalFiles("one")

	u := e.seedWithFactoryRoles(core.RoleUser, map[string]core.Role{f1: core.RoleEditor})
	// The override applies to its factory only.
	e.must(e.do("PUT", "/factories/"+f1+"/definition", u.Key, map[string]any{"files": files}), 200)
	r := e.must(e.do("PUT", "/factories/"+f2+"/definition", u.Key, map[string]any{"files": apitest.MinimalFiles("two")}), 403)
	if errCode(r) != CodeNotAuthorized {
		t.Fatalf("code = %q", errCode(r))
	}
	// The team-wide role decides who can create a factory and who changes
	// team resources.
	e.must(e.do("POST", "/factories", u.Key, map[string]any{"name": "three"}), 403)
	e.must(e.do("POST", "/secrets", u.Key, map[string]any{"name": "A", "value": "b"}), 403)
	// A factory editor attaches team integrations to that factory.
	e.must(e.do("PUT", "/factories/"+f1+"/integrations", u.Key, map[string]any{"integrations": []string{}}), 200)
	e.must(e.do("PUT", "/factories/"+f2+"/integrations", u.Key, map[string]any{"integrations": []string{}}), 403)

	// Objects of a factory resolve to that factory: work items and webhooks.
	d := e.must(e.do("POST", "/factories/"+f1+"/runs", u.Key, map[string]any{"prompt": "go"}), 201)
	e.must(e.do("POST", "/work-items/"+str(d.Body, "work_item_id")+"/stage", u.Key, map[string]any{"stage": "building"}), 200)
	d2 := e.must(e.do("POST", "/factories/"+f2+"/runs", u.Key, map[string]any{"prompt": "go"}), 201)
	e.must(e.do("POST", "/work-items/"+str(d2.Body, "work_item_id")+"/stage", u.Key, map[string]any{"stage": "building"}), 403)
	e.must(e.do("POST", "/runs/"+str(d.Body, "run_id")+"/score", u.Key, map[string]any{"scorer_id": "x"}), 501)
	e.must(e.do("POST", "/runs/"+str(d2.Body, "run_id")+"/score", u.Key, map[string]any{"scorer_id": "x"}), 403)

	// An override can also lower the role for one factory.
	ed := e.seedWithFactoryRoles(core.RoleEditor, map[string]core.Role{f1: core.RoleUser})
	e.must(e.do("PUT", "/factories/"+f1+"/definition", ed.Key, map[string]any{"files": files}), 403)
	e.must(e.do("PUT", "/factories/"+f2+"/definition", ed.Key, map[string]any{"files": apitest.MinimalFiles("two")}), 200)

	// A factory of another team is 404 for its editors too.
	e.must(e.do("PUT", "/factories/"+f1+"/definition", e.other.Key, map[string]any{"files": files}), 404)
}

func TestFactoryRolesAPI(t *testing.T) {
	e := newTestEnv(t)
	f1 := e.createFactory("one")
	u := e.viewer

	// GET /me shows the role.
	me := e.must(e.do("GET", "/me", u.Key, nil), 200)
	if me.Body["role"] != "user" {
		t.Fatalf("me.role = %v", me.Body["role"])
	}

	// An admin sets an override.
	p := e.must(e.do("PATCH", "/users/"+u.User.ID, e.admin.Key, map[string]any{"factory_roles": map[string]any{f1: "editor"}}), 200)
	if fr, _ := p.Body["factory_roles"].(map[string]any); fr[f1] != "editor" {
		t.Fatalf("factory_roles = %s", p.Raw)
	}
	me = e.must(e.do("GET", "/me", u.Key, nil), 200)
	if fr, _ := me.Body["factory_roles"].(map[string]any); fr[f1] != "editor" {
		t.Fatalf("me.factory_roles = %s", me.Raw)
	}
	e.must(e.do("PUT", "/factories/"+f1+"/definition", u.Key, map[string]any{"files": apitest.MinimalFiles("one")}), 200)

	// null removes the override.
	e.must(e.do("PATCH", "/users/"+u.User.ID, e.admin.Key, map[string]any{"factory_roles": map[string]any{f1: nil}}), 200)
	e.must(e.do("PUT", "/factories/"+f1+"/definition", u.Key, map[string]any{"files": apitest.MinimalFiles("one")}), 403)

	// Validation and permissions.
	e.must(e.do("PATCH", "/users/"+u.User.ID, e.admin.Key, map[string]any{"factory_roles": map[string]any{"fac_nope": "editor"}}), 400)
	e.must(e.do("PATCH", "/users/"+u.User.ID, e.admin.Key, map[string]any{"factory_roles": map[string]any{f1: "owner"}}), 400)
	e.must(e.do("PATCH", "/users/"+u.User.ID, e.editor.Key, map[string]any{"factory_roles": map[string]any{f1: "editor"}}), 403)
	// An editor override on a factory does not allow team admin routes.
	e.must(e.do("PATCH", "/users/"+u.User.ID, e.admin.Key, map[string]any{"factory_roles": map[string]any{f1: "admin"}}), 200)
	e.must(e.do("PATCH", "/users/"+e.editor.User.ID, u.Key, map[string]any{"role": "admin"}), 403)

	// A role that does not exist is refused.
	e.must(e.do("PATCH", "/users/"+e.editor.User.ID, e.admin.Key, map[string]any{"role": "viewer"}), 400)
	c := e.must(e.do("POST", "/users", e.admin.Key, map[string]any{"email": "new@example.test", "role": "user"}), 201)
	if str(c.Body, "user", "role") != "user" {
		t.Fatalf("created role = %s", c.Raw)
	}
}

func TestResolveInboxRecordsTool(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("inbox")
	_ = e.store.CreateInboxItem(ctx, &core.InboxItem{ID: "inb_a", FactoryID: fid, Kind: core.InboxApproval, Title: "Approve spec"})
	r := e.must(e.do("POST", "/inbox/inb_a/resolve", e.viewer.Key, map[string]any{"resolution": "approve"}), 200)
	if r.Body["resolved_via"] != core.InboxViaAPI || r.Body["resolved_by"] != "user:"+e.viewer.User.ID {
		t.Fatalf("resolve = %s", r.Raw)
	}
	if resolvedVia(&Principal{}) != core.InboxViaDashboard {
		t.Fatal("a session must resolve via the dashboard")
	}
	// The engine refuses an approval from a user who is not an approver.
	e.engine.Err = errors.Join(core.ErrForbidden)
	_ = e.store.CreateInboxItem(ctx, &core.InboxItem{ID: "inb_b", FactoryID: fid, Kind: core.InboxApproval, Title: "Approve plan"})
	e.must(e.do("POST", "/inbox/inb_b/resolve", e.viewer.Key, map[string]any{"resolution": "approve"}), 403)
}

func TestRestrictFollowupsToStarter(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("strict")
	files := apitest.MinimalFiles("strict")
	files["factory.yaml"] += "restrictFollowupsToStarter: true\n"
	e.must(e.do("PUT", "/factories/"+fid+"/definition", e.editor.Key, map[string]any{"files": files}), 200)

	d := e.must(e.do("POST", "/factories/"+fid+"/runs", e.viewer.Key, map[string]any{"prompt": "go"}), 201)
	wid, runID := str(d.Body, "work_item_id"), str(d.Body, "run_id")
	// The starter can send follow-ups.
	e.must(e.do("POST", "/work-items/"+wid+"/messages", e.viewer.Key, map[string]any{"message": "more"}), 202)
	e.must(e.do("POST", "/runs/"+runID+"/followups", e.viewer.Key, map[string]any{"message": "more"}), 202)
	// Other members, also admins, cannot.
	e.must(e.do("POST", "/work-items/"+wid+"/messages", e.admin.Key, map[string]any{"message": "x"}), 403)
	e.must(e.do("POST", "/runs/"+runID+"/followups", e.editor.Key, map[string]any{"message": "x"}), 403)
	e.must(e.do("POST", "/runs/"+runID+"/messages", e.editor.Key, map[string]any{"message": "x"}), 403)

	// Without the setting, every member can.
	other := e.createFactory("open")
	d2 := e.must(e.do("POST", "/factories/"+other+"/runs", e.viewer.Key, map[string]any{"prompt": "go"}), 201)
	e.must(e.do("POST", "/work-items/"+str(d2.Body, "work_item_id")+"/messages", e.editor.Key, map[string]any{"message": "x"}), 202)
}

func TestAuthorizeHookSeesFactory(t *testing.T) {
	var seen []string
	e := newTestEnv(t, func(o *Options) {
		o.Authorize = func(r *http.Request, p *Principal, min core.Role) error {
			seen = append(seen, RouteFactoryID(r.Context()))
			return nil
		}
	})
	fid := e.createFactory("hook")
	seen = nil
	e.must(e.do("GET", "/factories/"+fid+"/definition", e.viewer.Key, nil), 200)
	e.must(e.do("GET", "/secrets", e.viewer.Key, nil), 200)
	e.must(e.do("GET", "/factories/"+fid+"/definition", e.other.Key, nil), 404)
	if len(seen) != 3 || seen[0] != fid || seen[1] != "" || seen[2] != "" {
		t.Fatalf("factories seen by the authorizer = %q", seen)
	}
}
