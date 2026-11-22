package harness

import (
	"slices"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// CredentialSecretTypes returns the secret types that a harness accepts as
// its model credential, in the order of ClaudeAuthEnv and CodexAuthEnv. It
// returns nil for a harness without a known mapping. The generic type is not
// in the list: a generic secret is not a model credential by its type.
func CredentialSecretTypes(h rtcore.HarnessType) []rtcore.SecretType {
	switch NormalizeType(string(h)) {
	case rtcore.HarnessClaude:
		return []rtcore.SecretType{rtcore.SecretAnthropicKey, rtcore.SecretAnthropicOAuth}
	case rtcore.HarnessCodex:
		return []rtcore.SecretType{rtcore.SecretOpenAIKey}
	}
	return nil
}

// AcceptsCredentialType reports whether typ is a model credential type of
// the harness.
func AcceptsCredentialType(h rtcore.HarnessType, typ rtcore.SecretType) bool {
	return slices.Contains(CredentialSecretTypes(h), typ)
}

// credentialEnvNames are the environment variables that hold a model
// credential for each harness CLI.
var credentialEnvNames = map[rtcore.HarnessType][]string{
	rtcore.HarnessClaude: {"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_VERTEX"},
	rtcore.HarnessCodex:  {"OPENAI_API_KEY", "CODEX_API_KEY"},
}

// HasCredentialEnv reports whether env already holds a model credential for
// the harness, for example ANTHROPIC_API_KEY for claude. An empty value does
// not count.
func HasCredentialEnv(h rtcore.HarnessType, env map[string]string) bool {
	for _, k := range credentialEnvNames[NormalizeType(string(h))] {
		if env[k] != "" {
			return true
		}
	}
	return false
}
