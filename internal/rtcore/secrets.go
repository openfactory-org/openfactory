package rtcore

// SecretType is the type of a managed secret.
type SecretType string

// The secret types that the harness adapters read.
const (
	SecretAnthropicKey SecretType = "anthropic_api_key" //nolint:gosec // G101: a secret type name, not a credential
	// SecretAnthropicOAuth is a long-lived Claude Code OAuth token, made with
	// `claude setup-token`. Only the claude harness uses it.
	SecretAnthropicOAuth SecretType = "anthropic_oauth_token" //nolint:gosec // G101: a secret type name, not a credential
	SecretOpenAIKey      SecretType = "openai_api_key"        //nolint:gosec // G101: a secret type name, not a credential
)
