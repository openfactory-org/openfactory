package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestWebhookCreateSecret checks which secret a new webhook uses and whether
// the create response shows its value.
func TestWebhookCreateSecret(t *testing.T) {
	for _, c := range []struct {
		name string
		// pre is the value of the secret before the create ("" = no secret).
		pre string
		// otherWebhook makes a webhook of another factory use the secret.
		otherWebhook bool
		body         map[string]any
		secretName   string
		wantReveal   bool
		wantReused   bool
	}{
		{name: "new secret", body: map[string]any{"name": "deploys"}, secretName: "DEPLOYS_WEBHOOK_SECRET", wantReveal: true},
		{
			name: "left over default secret is replaced", pre: "left-over",
			body: map[string]any{"name": "deploys"}, secretName: "DEPLOYS_WEBHOOK_SECRET", wantReveal: true,
		},
		{
			name: "default secret of another webhook is kept", pre: "shared", otherWebhook: true,
			body: map[string]any{"name": "deploys"}, secretName: "DEPLOYS_WEBHOOK_SECRET", wantReused: true,
		},
		{
			name: "named secret is kept", pre: "named-value",
			body: map[string]any{"name": "deploys", "secret_name": "CI_TOKEN"}, secretName: "CI_TOKEN", wantReused: true,
		},
		{
			name: "provider secret is kept", pre: "provider-value",
			body:       map[string]any{"name": "deploys", "auth_mode": "signature", "signature_scheme": "github"},
			secretName: "DEPLOYS_WEBHOOK_SECRET", wantReused: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			ctx := context.Background()
			if c.otherWebhook {
				other := e.createFactory("other")
				e.must(e.do("POST", "/factories/"+other+"/webhooks", e.editor.Key, c.body), 201)
			}
			if c.pre != "" {
				if _, err := e.srv.opts.Secrets.Put(ctx, e.team.ID, "", c.secretName, core.SecretGeneric, []byte(c.pre)); err != nil {
					t.Fatal(err)
				}
			}
			fid := e.createFactory("hooks")
			r := e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, c.body), 201)
			stored, err := e.srv.opts.Secrets.Get(ctx, e.team.ID, "", c.secretName)
			if err != nil {
				t.Fatal(err)
			}
			got := str(r.Body, "secret")
			if c.wantReveal {
				if !strings.HasPrefix(got, "whsec_") || got != string(stored) {
					t.Fatalf("secret = %q, stored %q", got, stored)
				}
			} else if got != "" || string(stored) != c.pre {
				t.Fatalf("secret = %q, stored %q, want hidden %q", got, stored, c.pre)
			}
			if reused, _ := r.Body["secret_reused"].(bool); reused != c.wantReused {
				t.Fatalf("secret_reused = %v: %s", reused, r.Raw)
			}
		})
	}
}

// TestWebhookCreateFailedApplyRestoresSecret checks that a create whose
// definition does not apply leaves the secrets as they were, and that a retry
// shows the secret.
func TestWebhookCreateFailedApplyRestoresSecret(t *testing.T) {
	for _, c := range []struct {
		name    string
		pre     string
		preType core.SecretType
		body    map[string]any
	}{
		{name: "generated secret", body: map[string]any{"name": "deploys"}},
		{name: "provider secret over an existing one", pre: "old-provider", body: map[string]any{
			"name": "deploys", "auth_mode": "signature", "signature_scheme": "github", "secret": "new-provider",
		}},
		{name: "keeps the type of the old secret", pre: "old-key", preType: core.SecretAnthropicKey, body: map[string]any{
			"name": "deploys", "auth_mode": "signature", "signature_scheme": "github", "secret": "new-provider",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			ctx := context.Background()
			fid := e.createFactory("hooks")
			const name = "DEPLOYS_WEBHOOK_SECRET"
			preType := c.preType
			if preType == "" {
				preType = core.SecretGeneric
			}
			if c.pre != "" {
				if _, err := e.srv.opts.Secrets.Put(ctx, e.team.ID, "", name, preType, []byte(c.pre)); err != nil {
					t.Fatal(err)
				}
			}
			e.engine.Err = fmt.Errorf("%w: preflight failed", core.ErrInvalid)
			e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, c.body), 400)
			v, err := e.srv.opts.Secrets.Get(ctx, e.team.ID, "", name)
			switch {
			case c.pre == "" && err == nil:
				t.Fatalf("secret left after a failed create: %q", v)
			case c.pre != "" && string(v) != c.pre:
				t.Fatalf("secret = %q, want restored %q", v, c.pre)
			}
			if c.pre != "" {
				secs, err := e.srv.opts.Secrets.List(ctx, e.team.ID, "")
				if err != nil {
					t.Fatal(err)
				}
				for _, sec := range secs {
					if sec.Name == name && sec.Type != preType {
						t.Fatalf("restored secret type = %q, want %q", sec.Type, preType)
					}
				}
			}

			e.engine.Err = nil
			r := e.must(e.do("POST", "/factories/"+fid+"/webhooks", e.editor.Key, c.body), 201)
			if c.body["secret"] == nil && str(r.Body, "secret") == "" {
				t.Fatalf("retry hides the secret: %s", r.Raw)
			}
		})
	}
}
