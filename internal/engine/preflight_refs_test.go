package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The environment errors name the real key and carry the line, and an agent
// that inherits agentDefaults.environmentId gets the error in factory.yaml.
func TestPreflightEnvironmentKeyAndLine(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	def := DefaultTestDefinition()
	def.Agents[0].EnvironmentID = "env_x"   // own key in agent.md
	def.Agents[1].EnvironmentID = "env_def" // inherited from factory.yaml
	autoEnv := "env_auto"
	def.Automations[0].Overrides = &core.AgentOverrides{EnvironmentID: &autoEnv}
	env.Engine.compile = CompilerFor(def)
	files := map[string]string{
		"factory.yaml":                          "name: payments\nagentDefaults:\n  environmentId: env_def\n",
		agentFile(def.Agents[0].Name):           "---\ndescription: d\nagentType: FOREMAN\nenvironmentId: env_x\n---\nGo.\n",
		agentFile(def.Agents[1].Name):           "---\ndescription: d\n---\nGo.\n",
		automationFile(def.Automations[0].Name): "---\nenvironmentId: env_auto\ntriggers: []\n---\nGo.\n",
	}
	chk, err := env.Engine.CheckDefinition(ctx, env.Factory.ID, files)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.ValidationError{
		{File: agentFile(def.Agents[0].Name), Line: 4, Field: "environmentId", Message: `environment "env_x" does not exist in the team`},
		{File: "automations/issue-intake/automation.md", Line: 2, Field: "environmentId", Message: `environment "env_auto" does not exist in the team`},
		{File: "factory.yaml", Line: 3, Field: "agentDefaults.environmentId", Message: `environment "env_def" does not exist in the team`},
	}
	if fmt.Sprintf("%+v", chk.Errors) != fmt.Sprintf("%+v", want) {
		t.Fatalf("errors =\n %+v\nwant\n %+v", chk.Errors, want)
	}
}

// A webhook_ids entry that is a UID must belong to a webhook that the
// definition keeps.
func TestPreflightWebhookFilterUIDs(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	_, _ = env.Secrets.Put(ctx, "team1", "", "HOOK", core.SecretGeneric, []byte("tok"))
	def := DefaultTestDefinition()
	def.Webhooks = []core.Webhook{{Name: "deploys", AuthMode: "token", SecretName: "HOOK", Enabled: true}}
	env.Engine.compile = CompilerFor(def)
	if _, err := env.Engine.ApplyDefinition(ctx, env.Factory.ID, map[string]string{"factory.yaml": "ok"}, "a", "m", ""); err != nil {
		t.Fatal(err)
	}
	ws, _ := env.Store.ListWebhooks(ctx, env.Factory.ID)
	if len(ws) != 1 {
		t.Fatalf("webhooks = %+v", ws)
	}
	uid := ws[0].ID
	autoFile := automationFile(def.Automations[0].Name)
	content := "---\ntriggers:\n  - provider: webhook\n    event: received\n    filter:\n      webhook_ids: [x]\n---\nGo.\n"
	for _, tc := range []struct {
		name     string
		filter   string
		webhooks []core.Webhook
		want     string // "" for valid
	}{
		{"stored uid", `{"webhook_ids":["` + uid + `"]}`, def.Webhooks, ""},
		{"stored uid in", `{"webhook_ids":{"in":["` + uid + `"]}}`, def.Webhooks, ""},
		{"name", `{"webhook_ids":["deploys"]}`, def.Webhooks, ""},
		{"unknown uid", `{"webhook_ids":["whk_nope"]}`, def.Webhooks, `webhook "whk_nope" does not exist; known: deploys`},
		{"uid of removed webhook", `{"webhook_ids":["` + uid + `"]}`, nil, "known: none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := DefaultTestDefinition()
			d.Webhooks = tc.webhooks
			d.Automations[0].Triggers = []core.Trigger{{Provider: "webhook", Event: "received", Filter: json.RawMessage(tc.filter)}}
			env.Engine.compile = CompilerFor(d)
			chk, err := env.Engine.CheckDefinition(ctx, env.Factory.ID, map[string]string{"factory.yaml": "ok", autoFile: content})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if !chk.Valid() {
					t.Fatalf("errors = %+v", chk.Errors)
				}
				return
			}
			field := "triggers[0].filter.webhook_ids[0]"
			if !hasIssue(chk.Errors, autoFile, field, tc.want) {
				t.Fatalf("missing %q in %+v", tc.want, chk.Errors)
			}
			for _, e := range chk.Errors {
				if e.Field == field && e.Line != 6 {
					t.Errorf("line = %d, want 6", e.Line)
				}
			}
		})
	}
}

func TestWebhookFilterIDs(t *testing.T) {
	for _, tc := range []struct{ in, field, ids string }{
		{`{"webhook_ids":["a"]}`, "webhook_ids", "[a]"},
		{`{"webhook_ids":{"in":["b"]}}`, "webhook_ids.in", "[b]"},
		{`{"payload":{}}`, "", "[]"},
		{`not json`, "", "[]"},
	} {
		field, ids := webhookFilterIDs(json.RawMessage(tc.in))
		if field != tc.field || fmt.Sprint(ids) != tc.ids || strings.Contains(field, " ") {
			t.Errorf("webhookFilterIDs(%s) = %q, %v", tc.in, field, ids)
		}
	}
}
