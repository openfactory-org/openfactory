package api

import "testing"

// TestIntegrationBoundToFactory covers integration records bound
// to a factory: one record per provider and factory, and the status that an
// admin sets.
func TestIntegrationBoundToFactory(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("bound")
	gh := func(fac, org string) map[string]any {
		m := map[string]any{"provider": "github", "secret_ref": "GH_TOKEN", "config": map[string]string{"org": org}}
		if fac != "" {
			m["factory_id"] = fac
		}
		return m
	}
	team := e.must(e.do("POST", "/integrations", e.admin.Key, gh("", "acme")), 201)
	if team.Body["factory_id"] != nil {
		t.Fatalf("team record: %v", team.Body)
	}
	bound := e.must(e.do("POST", "/integrations", e.admin.Key, gh(fid, "acme")), 201)
	if str(bound.Body, "factory_id") != fid {
		t.Fatalf("bound record: %v", bound.Body)
	}
	e.must(e.do("POST", "/integrations", e.admin.Key, gh(fid, "ACME")), 409)
	e.must(e.do("POST", "/integrations", e.admin.Key, gh("fac_unknown", "acme")), 400)

	id := str(bound.Body, "id")
	e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"factory_id": "other"}), 400)
	e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"status": "auth_failed"}), 400)
	d := e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"status": "disabled"}), 200)
	if str(d.Body, "status") != "disabled" || str(d.Body, "status_reason") == "" || d.Body["status_updated_at"] == nil {
		t.Fatalf("disabled record: %v", d.Body)
	}
	a := e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"status": "active"}), 200)
	if str(a.Body, "status") != "active" {
		t.Fatalf("active record: %v", a.Body)
	}
	e.must(e.do("PATCH", "/integrations/"+id, e.editor.Key, map[string]any{"status": "disabled"}), 403)
}

// TestForgeRecordChecks covers the checks of forge records: a forge record
// needs config.org and a credential (400), and a team has one
// record for each provider, org and factory (409).
func TestForgeRecordChecks(t *testing.T) {
	e := newTestEnv(t)
	post := func(body map[string]any, want int) map[string]any {
		t.Helper()
		return e.must(e.do("POST", "/integrations", e.admin.Key, body), want).Body
	}
	for _, p := range []string{"github", "gitlab", "azure_devops"} {
		post(map[string]any{"provider": p, "secret_ref": "TOK"}, 400)                                            // no org
		post(map[string]any{"provider": p, "config": map[string]string{"org": "acme"}}, 400)                     // no credential
		post(map[string]any{"provider": p, "secret_ref": "TOK", "config": map[string]string{"org": "a/b"}}, 400) // not one name
		post(map[string]any{"provider": p, "secret_ref": "TOK", "config": map[string]string{"org": "acme"}}, 201)
		post(map[string]any{"provider": p, "secret_ref": "TOK2", "config": map[string]string{"org": " Acme "}}, 409)
		post(map[string]any{"provider": p, "secret_ref": "TOK", "config": map[string]string{"org": "other"}}, 201)
	}
	// An installation alone is no credential: the record carries its app (app_id
	// and the private key).
	post(map[string]any{"provider": "github", "config": map[string]string{"org": "inst", "installation_id": "7"}}, 400)
	post(map[string]any{"provider": "github", "config": map[string]string{"org": "nokey", "app_id": "1", "installation_id": "7"}}, 400)
	post(map[string]any{"provider": "github", "config": map[string]string{"org": "app", "app_id": "1", "installation_id": "7", "private_key_ref": "KEY", "app_slug": "acme-factory"}}, 201)
	// A change that removes org or the credential, or makes a second record, is refused.
	in := post(map[string]any{"provider": "github", "secret_ref": "TOK", "config": map[string]string{"org": "beta"}}, 201)
	id := str(in, "id")
	e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"config": map[string]string{}}), 400)
	e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"secret_ref": ""}), 400)
	e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"config": map[string]string{"org": "acme"}}), 409)
	e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"config": map[string]string{"org": "beta", "base_url": "https://ghe.local/api/v3"}}), 200)
	// Other providers do not need org.
	post(map[string]any{"provider": "linear", "secret_ref": "LIN", "config": map[string]string{"organization_id": "org-1"}}, 201)
}

// TestWorkspaceRecordChecks covers the checks of chat and tracker
// records: the workspace key is necessary (400), and a
// team has one record for each provider, workspace and factory (409).
func TestWorkspaceRecordChecks(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("chat")
	post := func(body map[string]any, want int) map[string]any {
		t.Helper()
		return e.must(e.do("POST", "/integrations", e.admin.Key, body), want).Body
	}
	cases := []struct{ provider, key, value, same, other string }{
		{"slack", "team_id", "T1", " T1 ", "T2"},
		{"microsoft_teams", "team_id", "11111111-aaaa", "11111111-AAAA", "22222222-bbbb"},
		{"linear", "organization_id", "org-1", "ORG-1", "org-2"},
		{"jira", "site", "acme.atlassian.net", "https://ACME.atlassian.net/", "beta.atlassian.net"},
	}
	for _, c := range cases {
		rec := func(v, fac string) map[string]any {
			m := map[string]any{"provider": c.provider, "secret_ref": "TOK", "config": map[string]string{c.key: v}}
			if fac != "" {
				m["factory_id"] = fac
			}
			return m
		}
		post(map[string]any{"provider": c.provider, "secret_ref": "TOK"}, 400) // no workspace key
		post(rec("  ", ""), 400)
		post(rec(c.value, ""), 201)
		post(rec(c.same, ""), 409)   // the same workspace for the team
		post(rec(c.value, fid), 201) // the factory record
		post(rec(c.same, fid), 409)  // the same workspace for the factory
		in := post(rec(c.other, ""), 201)
		id := str(in, "id")
		e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"config": map[string]string{}}), 400)
		e.must(e.do("PATCH", "/integrations/"+id, e.admin.Key, map[string]any{"config": map[string]string{c.key: c.value}}), 409)
	}
	// A Jira site is kept as its host name.
	in := post(map[string]any{"provider": "jira", "secret_ref": "TOK", "config": map[string]string{"site": "https://Gamma.atlassian.net/jira"}}, 201)
	if cfg, _ := in["config"].(map[string]any); cfg["site"] != "gamma.atlassian.net" {
		t.Fatalf("jira site = %v", in["config"])
	}
}
