package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Credential map keys accepted in HarnessRequest.Auth (in addition to env-var names).
//
// The planner (engine.Planner) resolves an agent's harness.auth into req.Auth. Two shapes are accepted:
//
//  1. Env-var shaped: {"ANTHROPIC_API_KEY": "..."}, {"AWS_BEARER_TOKEN_BEDROCK": "...", "AWS_REGION": "..."},
//     {"AWS_ACCESS_KEY_ID": ..., "AWS_SECRET_ACCESS_KEY": ..., "AWS_SESSION_TOKEN": ..., "AWS_REGION": ...},
//     {"OPENAI_API_KEY": "..."}.
//  2. Typed secret: {"type": "<core.SecretType>", "value": "<secret value>"} where value is either the raw key
//     or a JSON object. Bedrock JSON fields: apiKey|api_key|token, accessKeyId|access_key_id,
//     secretAccessKey|secret_access_key, sessionToken|session_token, region. "region" may also be a
//     sibling key of "type"/"value".
const (
	AuthKeyType   = "type"
	AuthKeyValue  = "value"
	AuthKeyRegion = "region"
)

// ErrAuthMismatch is returned when the credential type is not usable by the harness.
var ErrAuthMismatch = errors.New("credential type not supported by harness")

// ClaudeAuthEnv maps credentials for Claude Code:
//   - Anthropic API key        -> ANTHROPIC_API_KEY
//   - Claude Code OAuth token  -> CLAUDE_CODE_OAUTH_TOKEN (made with `claude setup-token`)
//   - Bedrock API key          -> AWS_BEARER_TOKEN_BEDROCK, AWS_REGION, CLAUDE_CODE_USE_BEDROCK=1
//   - Bedrock access key       -> AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY[, AWS_SESSION_TOKEN], AWS_REGION, CLAUDE_CODE_USE_BEDROCK=1
func ClaudeAuthEnv(auth map[string]string) (map[string]string, error) {
	env := map[string]string{}
	if len(auth) == 0 {
		return env, nil
	}
	region := firstNonEmpty(auth["AWS_REGION"], auth[AuthKeyRegion], auth["AWS_DEFAULT_REGION"])
	copyIf(env, auth, "ANTHROPIC_BASE_URL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_BEDROCK_BASE_URL")

	if typ := auth[AuthKeyType]; typ != "" {
		val := auth[AuthKeyValue]
		fields := parseSecretJSON(val)
		if r := pick(fields, "region", "aws_region", "awsRegion"); r != "" {
			region = r
		}
		switch core.SecretType(typ) {
		case core.SecretAnthropicKey:
			key := firstNonEmpty(pick(fields, "apiKey", "api_key", "key"), rawIfNotJSON(val, fields))
			if key == "" {
				return nil, errors.New("anthropic_api_key secret is empty")
			}
			env["ANTHROPIC_API_KEY"] = key
			return env, nil
		case core.SecretAnthropicOAuth:
			tok := strings.TrimSpace(rawIfNotJSON(val, fields))
			if tok == "" {
				return nil, errors.New("anthropic_oauth_token secret is empty")
			}
			env["CLAUDE_CODE_OAUTH_TOKEN"] = tok
			return env, nil
		case core.SecretBedrockKey:
			tok := firstNonEmpty(pick(fields, "apiKey", "api_key", "token", "bearerToken"), rawIfNotJSON(val, fields))
			return bedrockEnv(env, map[string]string{"AWS_BEARER_TOKEN_BEDROCK": tok}, region)
		case core.SecretBedrockAccess:
			return bedrockEnv(env, map[string]string{
				"AWS_ACCESS_KEY_ID":     pick(fields, "accessKeyId", "access_key_id", "aws_access_key_id"),
				"AWS_SECRET_ACCESS_KEY": pick(fields, "secretAccessKey", "secret_access_key", "aws_secret_access_key"),
				"AWS_SESSION_TOKEN":     pick(fields, "sessionToken", "session_token", "aws_session_token"),
			}, region)
		default:
			return nil, fmt.Errorf("%w: claude accepts anthropic_api_key, anthropic_oauth_token, anthropic_bedrock_api_key or anthropic_bedrock_access_key, got %q", ErrAuthMismatch, typ)
		}
	}

	switch {
	case auth["ANTHROPIC_API_KEY"] != "":
		env["ANTHROPIC_API_KEY"] = auth["ANTHROPIC_API_KEY"]
		return env, nil
	case auth["ANTHROPIC_AUTH_TOKEN"] != "":
		env["ANTHROPIC_AUTH_TOKEN"] = auth["ANTHROPIC_AUTH_TOKEN"]
		return env, nil
	case auth["CLAUDE_CODE_OAUTH_TOKEN"] != "":
		env["CLAUDE_CODE_OAUTH_TOKEN"] = auth["CLAUDE_CODE_OAUTH_TOKEN"]
		return env, nil
	case auth["AWS_BEARER_TOKEN_BEDROCK"] != "":
		return bedrockEnv(env, map[string]string{"AWS_BEARER_TOKEN_BEDROCK": auth["AWS_BEARER_TOKEN_BEDROCK"]}, region)
	case auth["AWS_ACCESS_KEY_ID"] != "":
		return bedrockEnv(env, map[string]string{
			"AWS_ACCESS_KEY_ID":     auth["AWS_ACCESS_KEY_ID"],
			"AWS_SECRET_ACCESS_KEY": auth["AWS_SECRET_ACCESS_KEY"],
			"AWS_SESSION_TOKEN":     auth["AWS_SESSION_TOKEN"],
		}, region)
	case auth["OPENAI_API_KEY"] != "" || auth["CODEX_API_KEY"] != "":
		return nil, fmt.Errorf("%w: claude cannot use an OpenAI API key", ErrAuthMismatch)
	}
	return env, nil
}

func bedrockEnv(env, creds map[string]string, region string) (map[string]string, error) {
	if tok, ok := creds["AWS_BEARER_TOKEN_BEDROCK"]; ok && tok == "" {
		return nil, errors.New("bedrock API key is empty")
	}
	if _, ok := creds["AWS_ACCESS_KEY_ID"]; ok && (creds["AWS_ACCESS_KEY_ID"] == "" || creds["AWS_SECRET_ACCESS_KEY"] == "") {
		return nil, errors.New("bedrock access key requires an access key id and secret access key")
	}
	if region == "" {
		return nil, errors.New("bedrock credentials require an AWS region")
	}
	for k, v := range creds {
		if v != "" {
			env[k] = v
		}
	}
	env["AWS_REGION"] = region
	env["CLAUDE_CODE_USE_BEDROCK"] = "1"
	return env, nil
}

// CodexAuthEnv maps credentials for Codex: an OpenAI API key -> OPENAI_API_KEY (and CODEX_API_KEY,
// which `codex exec` reads with precedence).
func CodexAuthEnv(auth map[string]string) (map[string]string, error) {
	env := map[string]string{}
	if len(auth) == 0 {
		return env, nil
	}
	copyIf(env, auth, "OPENAI_BASE_URL", "OPENAI_ORG_ID", "OPENAI_PROJECT_ID")
	key := ""
	if typ := auth[AuthKeyType]; typ != "" {
		if core.SecretType(typ) != core.SecretOpenAIKey {
			return nil, fmt.Errorf("%w: codex accepts openai_api_key, got %q", ErrAuthMismatch, typ)
		}
		fields := parseSecretJSON(auth[AuthKeyValue])
		key = firstNonEmpty(pick(fields, "apiKey", "api_key", "key"), rawIfNotJSON(auth[AuthKeyValue], fields))
		if key == "" {
			return nil, errors.New("openai_api_key secret is empty")
		}
	} else {
		key = firstNonEmpty(auth["CODEX_API_KEY"], auth["OPENAI_API_KEY"])
		if key == "" && (auth["ANTHROPIC_API_KEY"] != "" || auth["AWS_BEARER_TOKEN_BEDROCK"] != "" || auth["AWS_ACCESS_KEY_ID"] != "") {
			return nil, fmt.Errorf("%w: codex cannot use Anthropic or Bedrock credentials", ErrAuthMismatch)
		}
	}
	if key != "" {
		env["OPENAI_API_KEY"] = key
		env["CODEX_API_KEY"] = key
	}
	return env, nil
}

func parseSecretJSON(v string) map[string]string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "{") {
		return nil
	}
	var raw map[string]any
	if json.Unmarshal([]byte(v), &raw) != nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, val := range raw {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func rawIfNotJSON(v string, fields map[string]string) string {
	if fields != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func pick(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func copyIf(dst, src map[string]string, keys ...string) {
	for _, k := range keys {
		if v := src[k]; v != "" {
			dst[k] = v
		}
	}
}

// CredentialHint returns a note for the error of a failed turn when the run
// has no harness credential, or "" when it has one. A factory needs a
// credential (harness.auth) before its first run. Without one the CLI uses
// only the credentials that the sandbox already has.
func CredentialHint(req core.HarnessRequest) string {
	if len(req.Auth) > 0 || (req.Run != nil && req.Run.Config.Harness.Auth != nil) {
		return ""
	}
	return " (the agent has no harness credential: set harness.auth in the definition, for example a managed secret of type anthropic_api_key or openai_api_key)"
}
