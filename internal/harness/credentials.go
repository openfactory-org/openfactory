package harness

import (
	"slices"

	"github.com/openfactory-org/openfactory/internal/core"
)

// CredentialSecretTypes returns the secret types that a harness accepts as
// its model credential, in the order of ClaudeAuthEnv and CodexAuthEnv. It
// returns nil for a harness without a known mapping. The generic type is not
// in the list: a generic secret is not a model credential by its type.
func CredentialSecretTypes(h core.HarnessType) []core.SecretType {
	switch NormalizeType(string(h)) {
	case core.HarnessClaude:
		return []core.SecretType{core.SecretAnthropicKey, core.SecretAnthropicOAuth, core.SecretBedrockKey, core.SecretBedrockAccess}
	case core.HarnessCodex:
		return []core.SecretType{core.SecretOpenAIKey}
	}
	return nil
}

// AcceptsCredentialType reports whether typ is a model credential type of
// the harness.
func AcceptsCredentialType(h core.HarnessType, typ core.SecretType) bool {
	return slices.Contains(CredentialSecretTypes(h), typ)
}

// credentialEnvNames are the environment variables that hold a model
// credential for each harness CLI.
var credentialEnvNames = map[core.HarnessType][]string{
	core.HarnessClaude: {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"AWS_BEARER_TOKEN_BEDROCK", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"},
	core.HarnessCodex: {"OPENAI_API_KEY", "CODEX_API_KEY"},
}

// HasCredentialEnv reports whether env already holds a model credential for
// the harness, for example ANTHROPIC_API_KEY for claude. An empty value does
// not count.
func HasCredentialEnv(h core.HarnessType, env map[string]string) bool {
	for _, k := range credentialEnvNames[NormalizeType(string(h))] {
		if env[k] != "" {
			return true
		}
	}
	return false
}
