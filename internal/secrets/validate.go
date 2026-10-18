package secrets

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/openfactory-org/openfactory/internal/core"
)

// MaxNameLength bounds secret names.
const MaxNameLength = 128

// MaxEnvEntry is the largest injected NAME=value entry, including the
// terminating NUL byte (128 KiB).
const MaxEnvEntry = 128 << 10

var nameRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// ReservedPrefixes cannot start a secret name: OF_METADATA_ is reserved for run
// metadata and OF_ for platform-injected variables.
var ReservedPrefixes = []string{"OF_"}

// ValidateName checks that name is an UPPER_SNAKE_CASE environment variable
// name (letters A–Z, digits and underscores, not starting with a digit,
// 1–128 characters) and does not use a reserved prefix.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: secret name is required", core.ErrInvalid)
	}
	if len(name) > MaxNameLength {
		return fmt.Errorf("%w: secret name exceeds %d characters", core.ErrInvalid, MaxNameLength)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: secret name %q must use uppercase letters, digits and underscores and must not start with a digit (e.g. METABASE_API_KEY)", core.ErrInvalid, name)
	}
	for _, p := range ReservedPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("%w: secret name %q uses the reserved prefix %s", core.ErrInvalid, name, p)
		}
	}
	return nil
}

// DockerRegistryCredential is the value of a docker_registry secret.
type DockerRegistryCredential struct {
	Registry string `json:"registry"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// ECRCredential is the value of an aws_ecr_credential secret. Either RoleARN
// (assumed with base credentials from the secret or the worker environment) or
// static access keys must be present. Registry defaults to
// <account>.dkr.ecr.<region>.amazonaws.com derived from the role ARN.
type ECRCredential struct {
	Region          string `json:"region"`
	RoleARN         string `json:"role_arn,omitempty"`
	ExternalID      string `json:"external_id,omitempty"`
	AccessKeyID     string `json:"access_key_id,omitempty"`
	SecretAccessKey string `json:"secret_access_key,omitempty"`
	SessionToken    string `json:"session_token,omitempty"`
	Registry        string `json:"registry,omitempty"`
}

// BedrockAccessKey is the value of an anthropic_bedrock_access_key secret.
type BedrockAccessKey struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
	Region          string `json:"region"`
}

var (
	regionRe  = regexp.MustCompile(`^[a-z]{2}(-gov|-iso[a-z]*)?-[a-z]+-\d$`)
	roleARNRe = regexp.MustCompile(`^arn:aws[a-z-]*:iam::(\d{12}):role/[\w+=,.@/-]{1,512}$`)
	ecrHostRe = regexp.MustCompile(`^(\d{12})\.dkr\.ecr(-fips)?\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$`)
)

// ValidateValue validates a secret value for its type, including the injected
// environment entry size limit.
func ValidateValue(name string, typ core.SecretType, value []byte) error {
	if len(value) == 0 {
		return fmt.Errorf("%w: secret value is required", core.ErrInvalid)
	}
	if !utf8.Valid(value) {
		return fmt.Errorf("%w: secret value must be UTF-8 text", core.ErrInvalid)
	}
	if strings.ContainsRune(string(value), 0) {
		return fmt.Errorf("%w: secret value must not contain NUL bytes", core.ErrInvalid)
	}
	if len(name)+1+len(value)+1 > MaxEnvEntry {
		return fmt.Errorf("%w: secret %s is too large: NAME=value must fit within %d bytes including the terminating null byte", core.ErrInvalid, name, MaxEnvEntry)
	}
	switch typ {
	case core.SecretGeneric:
		return nil
	case core.SecretAnthropicKey, core.SecretAnthropicOAuth, core.SecretOpenAIKey, core.SecretBedrockKey:
		if strings.IndexFunc(string(value), unicode.IsSpace) >= 0 {
			return fmt.Errorf("%w: %s must be a single token without whitespace", core.ErrInvalid, typ)
		}
		return nil
	case core.SecretBedrockAccess:
		var v BedrockAccessKey
		if err := strictJSON(value, &v); err != nil {
			return fmt.Errorf("%w: %s must be JSON {access_key_id, secret_access_key, region[, session_token]}: %w", core.ErrInvalid, typ, err)
		}
		if v.AccessKeyID == "" || v.SecretAccessKey == "" || v.Region == "" {
			return fmt.Errorf("%w: %s requires access_key_id, secret_access_key and region", core.ErrInvalid, typ)
		}
		if !regionRe.MatchString(v.Region) {
			return fmt.Errorf("%w: invalid AWS region %q", core.ErrInvalid, v.Region)
		}
		return nil
	case core.SecretDockerRegistry:
		_, err := ParseDockerRegistry(value)
		return err
	case core.SecretAWSECR:
		_, err := ParseECR(value)
		return err
	default:
		return fmt.Errorf("%w: unknown secret type %q", core.ErrInvalid, typ)
	}
}

func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// ParseDockerRegistry parses and validates a docker_registry secret value.
func ParseDockerRegistry(value []byte) (*DockerRegistryCredential, error) {
	var c DockerRegistryCredential
	if err := strictJSON(value, &c); err != nil {
		return nil, fmt.Errorf("%w: docker_registry must be JSON {registry, username, password}: %w", core.ErrInvalid, err)
	}
	if c.Registry == "" || c.Username == "" || c.Password == "" {
		return nil, fmt.Errorf("%w: docker_registry requires registry, username and password", core.ErrInvalid)
	}
	host, err := normalizeRegistry(c.Registry)
	if err != nil {
		return nil, err
	}
	c.Registry = host
	return &c, nil
}

// ParseECR parses and validates an aws_ecr_credential secret value, deriving
// the registry host from the role ARN when absent.
func ParseECR(value []byte) (*ECRCredential, error) {
	var c ECRCredential
	if err := strictJSON(value, &c); err != nil {
		return nil, fmt.Errorf("%w: aws_ecr_credential must be JSON {region, role_arn | access_key_id+secret_access_key[, registry, external_id, session_token]}: %w", core.ErrInvalid, err)
	}
	if c.Region == "" || !regionRe.MatchString(c.Region) {
		return nil, fmt.Errorf("%w: aws_ecr_credential requires a valid AWS region", core.ErrInvalid)
	}
	if (c.AccessKeyID == "") != (c.SecretAccessKey == "") {
		return nil, fmt.Errorf("%w: aws_ecr_credential requires both access_key_id and secret_access_key", core.ErrInvalid)
	}
	account := ""
	if c.RoleARN != "" {
		m := roleARNRe.FindStringSubmatch(c.RoleARN)
		if m == nil {
			return nil, fmt.Errorf("%w: invalid IAM role ARN %q", core.ErrInvalid, c.RoleARN)
		}
		account = m[1]
	} else if c.AccessKeyID == "" {
		return nil, fmt.Errorf("%w: aws_ecr_credential requires role_arn or access keys", core.ErrInvalid)
	}
	if c.Registry != "" {
		host, err := normalizeRegistry(c.Registry)
		if err != nil {
			return nil, err
		}
		m := ecrHostRe.FindStringSubmatch(host)
		if m == nil {
			return nil, fmt.Errorf("%w: %q is not an Amazon ECR registry host", core.ErrInvalid, host)
		}
		if m[3] != c.Region {
			return nil, fmt.Errorf("%w: registry region %s does not match region %s", core.ErrInvalid, m[3], c.Region)
		}
		c.Registry = host
	} else {
		if account == "" {
			return nil, fmt.Errorf("%w: aws_ecr_credential requires registry when no role_arn is given", core.ErrInvalid)
		}
		suffix := ".amazonaws.com"
		if strings.HasPrefix(c.Region, "cn-") {
			suffix = ".amazonaws.com.cn"
		}
		c.Registry = account + ".dkr.ecr." + c.Region + suffix
	}
	return &c, nil
}

func normalizeRegistry(r string) (string, error) {
	r = strings.TrimSpace(strings.ToLower(r))
	if strings.Contains(r, "://") {
		u, err := url.Parse(r)
		if err != nil || u.Host == "" {
			return "", fmt.Errorf("%w: invalid registry %q", core.ErrInvalid, r)
		}
		r = u.Host
	}
	r = strings.TrimSuffix(r, "/")
	if r == "" || strings.ContainsAny(r, "/ ") {
		return "", fmt.Errorf("%w: registry must be a host name such as registry.example.com", core.ErrInvalid)
	}
	return canonicalRegistry(r), nil
}
