package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/api/apitest"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

// testSecret is a secret of a test: an empty factory is the team scope.
type testSecret struct {
	factory string
	name    string
	typ     core.SecretType
	value   string
}

// scopedSecrets returns a secret store that keeps the scopes apart (team1,
// factory fac_1), with the secrets of list.
func scopedSecrets(t *testing.T, list []testSecret) core.SecretStore {
	t.Helper()
	ss := &apitest.Secrets{Store: testdb.New(t)}
	for _, s := range list {
		v := s.value
		if v == "" {
			v = "value-of-" + s.name
		}
		if _, err := ss.Put(context.Background(), "team1", s.factory, s.name, s.typ, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	return ss
}

func TestFindDefaultCredential(t *testing.T) {
	const fac = "fac_1"
	tests := []struct {
		name    string
		harness core.HarnessType
		secrets []testSecret
		want    HarnessCredential
	}{
		{name: "no secret", harness: core.HarnessClaude,
			want: HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
		{name: "team oauth token for claude", harness: core.HarnessClaude,
			secrets: []testSecret{{"", "CLAUDE_OAUTH_TOKEN", core.SecretAnthropicOAuth, ""}, {"", "DB_URL", core.SecretGeneric, ""}},
			want:    HarnessCredential{Source: CredentialDefault, SecretName: "CLAUDE_OAUTH_TOKEN", Scope: CredentialScopeTeam, Type: core.SecretAnthropicOAuth}},
		{name: "harness alias", harness: "claude-code",
			secrets: []testSecret{{"", "KEY", core.SecretAnthropicKey, ""}},
			want:    HarnessCredential{Source: CredentialDefault, SecretName: "KEY", Scope: CredentialScopeTeam, Type: core.SecretAnthropicKey}},
		{name: "bedrock key for claude", harness: core.HarnessClaude,
			secrets: []testSecret{{"", "BEDROCK", core.SecretBedrockKey, ""}},
			want:    HarnessCredential{Source: CredentialDefault, SecretName: "BEDROCK", Scope: CredentialScopeTeam, Type: core.SecretBedrockKey}},
		{name: "generic secret is not a credential", harness: core.HarnessClaude,
			secrets: []testSecret{{"", "ANTHROPIC_API_KEY", core.SecretGeneric, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
		{name: "factory secret wins over team secret", harness: core.HarnessClaude,
			secrets: []testSecret{{"", "TEAM_TOKEN", core.SecretAnthropicOAuth, ""}, {fac, "FACTORY_KEY", core.SecretAnthropicKey, ""}},
			want:    HarnessCredential{Source: CredentialDefault, SecretName: "FACTORY_KEY", Scope: CredentialScopeFactory, Type: core.SecretAnthropicKey}},
		{name: "secret of another factory does not count", harness: core.HarnessClaude,
			secrets: []testSecret{{"fac_2", "OTHER", core.SecretAnthropicKey, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
		{name: "codex takes the openai key", harness: core.HarnessCodex,
			secrets: []testSecret{{"", "ANTHROPIC", core.SecretAnthropicKey, ""}, {"", "OPENAI", core.SecretOpenAIKey, ""}},
			want:    HarnessCredential{Source: CredentialDefault, SecretName: "OPENAI", Scope: CredentialScopeTeam, Type: core.SecretOpenAIKey}},
		{name: "codex ignores anthropic secrets", harness: core.HarnessCodex,
			secrets: []testSecret{{"", "TOKEN", core.SecretAnthropicOAuth, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
		{name: "claude ignores the openai key", harness: core.HarnessClaude,
			secrets: []testSecret{{"", "OPENAI", core.SecretOpenAIKey, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
		{name: "harness without a mapping", harness: "open",
			secrets: []testSecret{{"", "KEY", core.SecretAnthropicKey, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
		{name: "two team secrets are ambiguous", harness: core.HarnessClaude,
			secrets: []testSecret{{"", "TOKEN", core.SecretAnthropicOAuth, ""}, {"", "KEY", core.SecretAnthropicKey, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonAmbiguous, Scope: CredentialScopeTeam, Candidates: []string{"KEY", "TOKEN"}}},
		{name: "ambiguous factory secrets do not fall back to the team", harness: core.HarnessClaude,
			secrets: []testSecret{{fac, "A", core.SecretAnthropicKey, ""}, {fac, "B", core.SecretAnthropicKey, ""}, {"", "TEAM", core.SecretAnthropicKey, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonAmbiguous, Scope: CredentialScopeFactory, Candidates: []string{"A", "B"}}},
		{name: "one factory secret settles ambiguous team secrets", harness: core.HarnessClaude,
			secrets: []testSecret{{fac, "A", core.SecretAnthropicKey, ""}, {"", "B", core.SecretAnthropicKey, ""}, {"", "C", core.SecretAnthropicOAuth, ""}},
			want:    HarnessCredential{Source: CredentialDefault, SecretName: "A", Scope: CredentialScopeFactory, Type: core.SecretAnthropicKey}},
		{name: "factory secret of the same name hides the team secret", harness: core.HarnessClaude,
			secrets: []testSecret{{fac, "TOKEN", core.SecretGeneric, ""}, {"", "TOKEN", core.SecretAnthropicOAuth, ""}},
			want:    HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := findDefaultCredential(context.Background(), scopedSecrets(t, tc.secrets), "team1", fac, tc.harness)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	if got, _ := findDefaultCredential(context.Background(), nil, "team1", fac, core.HarnessClaude); got.IsDefault() {
		t.Errorf("no secret store: got %+v", got)
	}
}

// resolveHarnessAuth: explicit harness.auth, then a credential in the run
// environment, then the default credential, then none.
func TestResolveHarnessAuth(t *testing.T) {
	managed := &core.HarnessAuth{Source: CredentialManagedSecret, SecretName: "EXPLICIT"}
	both := []testSecret{{"", "EXPLICIT", core.SecretAnthropicKey, "sk-explicit"}, {"fac_1", "FACTORY_TOKEN", core.SecretAnthropicOAuth, "tok-factory"}}
	tests := []struct {
		name     string
		harness  core.HarnessType
		auth     *core.HarnessAuth
		env      map[string]string
		secrets  []testSecret
		wantEnv  map[string]string
		wantCred HarnessCredential
	}{
		{name: "explicit managed secret wins over the default", harness: core.HarnessClaude, auth: managed, secrets: both,
			wantEnv:  map[string]string{"ANTHROPIC_API_KEY": "sk-explicit"},
			wantCred: HarnessCredential{Source: CredentialManagedSecret, SecretName: "EXPLICIT", Type: core.SecretAnthropicKey}},
		{name: "explicit worker environment passes nothing", harness: core.HarnessClaude, auth: &core.HarnessAuth{Source: CredentialWorkerEnvironment}, secrets: both,
			wantCred: HarnessCredential{Source: CredentialWorkerEnvironment}},
		{name: "run environment credential stays", harness: core.HarnessClaude, env: map[string]string{"ANTHROPIC_API_KEY": "sk-env"}, secrets: both,
			wantCred: HarnessCredential{Source: CredentialNone, Reason: CredentialReasonRunEnv}},
		{name: "default factory secret", harness: core.HarnessClaude, secrets: both,
			wantEnv:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-factory"},
			wantCred: HarnessCredential{Source: CredentialDefault, SecretName: "FACTORY_TOKEN", Scope: CredentialScopeFactory, Type: core.SecretAnthropicOAuth}},
		{name: "default team secret", harness: core.HarnessClaude, secrets: []testSecret{{"", "CLAUDE_OAUTH_TOKEN", core.SecretAnthropicOAuth, "tok-team"}},
			wantEnv:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-team"},
			wantCred: HarnessCredential{Source: CredentialDefault, SecretName: "CLAUDE_OAUTH_TOKEN", Scope: CredentialScopeTeam, Type: core.SecretAnthropicOAuth}},
		{name: "default codex key", harness: core.HarnessCodex, secrets: []testSecret{{"", "OPENAI", core.SecretOpenAIKey, "sk-openai"}},
			wantEnv:  map[string]string{"OPENAI_API_KEY": "sk-openai", "CODEX_API_KEY": "sk-openai"},
			wantCred: HarnessCredential{Source: CredentialDefault, SecretName: "OPENAI", Scope: CredentialScopeTeam, Type: core.SecretOpenAIKey}},
		{name: "ambiguous gives none", harness: core.HarnessClaude, secrets: []testSecret{{"", "A", core.SecretAnthropicKey, ""}, {"", "B", core.SecretAnthropicOAuth, ""}},
			wantCred: HarnessCredential{Source: CredentialNone, Reason: CredentialReasonAmbiguous, Scope: CredentialScopeTeam, Candidates: []string{"A", "B"}}},
		{name: "no secret gives none", harness: core.HarnessClaude,
			wantCred: HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			x := NewPlanner(PlannerOptions{Store: testdb.New(t), Secrets: scopedSecrets(t, tc.secrets), Logger: DiscardLogger()})
			r := &core.Run{ID: "run_1", TeamID: "team1", FactoryID: "fac_1",
				Config: core.AgentConfig{Harness: core.HarnessConfig{Type: tc.harness, Auth: tc.auth}}}
			env, cred, err := x.resolveHarnessAuth(context.Background(), r, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if len(env) != 0 || len(tc.wantEnv) != 0 {
				if !reflect.DeepEqual(env, tc.wantEnv) {
					t.Errorf("env = %v, want %v", env, tc.wantEnv)
				}
			}
			if !reflect.DeepEqual(cred, tc.wantCred) {
				t.Errorf("credential = %+v, want %+v", cred, tc.wantCred)
			}
		})
	}
}

// The redaction of a run covers the value of the default credential.
func TestRedactForDefaultCredential(t *testing.T) {
	x := NewPlanner(PlannerOptions{Store: testdb.New(t), Logger: DiscardLogger(),
		Secrets: scopedSecrets(t, []testSecret{{"", "CLAUDE_OAUTH_TOKEN", core.SecretAnthropicOAuth, "tok-default-fake"}})})
	r := &core.Run{ID: "run_1", TeamID: "team1", FactoryID: "fac_1", Config: core.AgentConfig{Harness: core.HarnessConfig{Type: core.HarnessClaude}}}
	if got := x.RedactFor(context.Background(), r)("the token is tok-default-fake"); strings.Contains(got, "tok-default-fake") {
		t.Errorf("redacted = %q", got)
	}
}
