package harness

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

func TestAcceptsCredentialType(t *testing.T) {
	tests := []struct {
		harness rtcore.HarnessType
		typ     rtcore.SecretType
		want    bool
	}{
		{rtcore.HarnessClaude, rtcore.SecretAnthropicKey, true},
		{rtcore.HarnessClaude, rtcore.SecretAnthropicOAuth, true},
		{rtcore.HarnessClaude, "anthropic_vertex_key", false},
		{"claude-code", rtcore.SecretAnthropicOAuth, true},
		{rtcore.HarnessClaude, rtcore.SecretOpenAIKey, false},
		{rtcore.HarnessClaude, "generic", false},
		{rtcore.HarnessCodex, rtcore.SecretOpenAIKey, true},
		{rtcore.HarnessCodex, rtcore.SecretAnthropicKey, false},
		{"open", rtcore.SecretAnthropicKey, false},
	}
	for _, tc := range tests {
		if got := AcceptsCredentialType(tc.harness, tc.typ); got != tc.want {
			t.Errorf("%s accepts %s = %v, want %v", tc.harness, tc.typ, got, tc.want)
		}
	}
}

func TestHasCredentialEnv(t *testing.T) {
	tests := []struct {
		harness rtcore.HarnessType
		env     map[string]string
		want    bool
	}{
		{rtcore.HarnessClaude, nil, false},
		{rtcore.HarnessClaude, map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}, true},
		{rtcore.HarnessClaude, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-fake"}, true},
		{rtcore.HarnessClaude, map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}, true},
		{rtcore.HarnessClaude, map[string]string{"ANTHROPIC_API_KEY": ""}, false},
		{rtcore.HarnessClaude, map[string]string{"OPENAI_API_KEY": "sk-fake"}, false},
		{rtcore.HarnessCodex, map[string]string{"OPENAI_API_KEY": "sk-fake"}, true},
		{rtcore.HarnessCodex, map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}, false},
		{"open", map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}, false},
	}
	for _, tc := range tests {
		if got := HasCredentialEnv(tc.harness, tc.env); got != tc.want {
			t.Errorf("%s with %v = %v, want %v", tc.harness, tc.env, got, tc.want)
		}
	}
}
