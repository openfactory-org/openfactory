package harness

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestAcceptsCredentialType(t *testing.T) {
	tests := []struct {
		harness core.HarnessType
		typ     core.SecretType
		want    bool
	}{
		{core.HarnessClaude, core.SecretAnthropicKey, true},
		{core.HarnessClaude, core.SecretAnthropicOAuth, true},
		{core.HarnessClaude, core.SecretBedrockKey, true},
		{core.HarnessClaude, core.SecretBedrockAccess, true},
		{"claude-code", core.SecretAnthropicOAuth, true},
		{core.HarnessClaude, core.SecretOpenAIKey, false},
		{core.HarnessClaude, core.SecretGeneric, false},
		{core.HarnessCodex, core.SecretOpenAIKey, true},
		{core.HarnessCodex, core.SecretAnthropicKey, false},
		{"open", core.SecretAnthropicKey, false},
	}
	for _, tc := range tests {
		if got := AcceptsCredentialType(tc.harness, tc.typ); got != tc.want {
			t.Errorf("%s accepts %s = %v, want %v", tc.harness, tc.typ, got, tc.want)
		}
	}
}

func TestHasCredentialEnv(t *testing.T) {
	tests := []struct {
		harness core.HarnessType
		env     map[string]string
		want    bool
	}{
		{core.HarnessClaude, nil, false},
		{core.HarnessClaude, map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}, true},
		{core.HarnessClaude, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-fake"}, true},
		{core.HarnessClaude, map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"}, true},
		{core.HarnessClaude, map[string]string{"ANTHROPIC_API_KEY": ""}, false},
		{core.HarnessClaude, map[string]string{"OPENAI_API_KEY": "sk-fake"}, false},
		{core.HarnessCodex, map[string]string{"OPENAI_API_KEY": "sk-fake"}, true},
		{core.HarnessCodex, map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}, false},
		{"open", map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}, false},
	}
	for _, tc := range tests {
		if got := HasCredentialEnv(tc.harness, tc.env); got != tc.want {
			t.Errorf("%s with %v = %v, want %v", tc.harness, tc.env, got, tc.want)
		}
	}
}
