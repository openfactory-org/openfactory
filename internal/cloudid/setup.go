package cloudid

import (
	"encoding/json"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// GCP token types and URLs of an external_account credentials file.
const (
	gcpTokenType = "urn:ietf:params:oauth:token-type:jwt" //nolint:gosec // G101: a token type URN, not a credential
	gcpTokenURL  = "https://sts.googleapis.com/v1/token"  //nolint:gosec // G101: a public endpoint URL, not a credential
	// GCPAllowExecutablesEnv must be 1 before the Google SDKs run an
	// executable credential source.
	GCPAllowExecutablesEnv = "GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES"
)

// Setup is the cloud identity of a factory, without tokens. The runner uses
// it to write the credential files of the SDKs.
type Setup struct {
	AWS *AWSSetup
	GCP *GCPSetup
}

// AWSSetup is the AWS part of a Setup.
type AWSSetup struct {
	RoleARN  string
	Audience string
}

// GCPSetup is the GCP part of a Setup.
type GCPSetup struct {
	Audience            string
	ProjectNumber       string
	ServiceAccountEmail string
}

// SetupFor returns the setup of cp, or nil when cp declares no provider.
func SetupFor(cp *core.CloudProviders) *Setup {
	if cp == nil {
		return nil
	}
	s := &Setup{}
	if a := cp.AWS; a != nil && a.RoleARN != "" {
		s.AWS = &AWSSetup{RoleARN: a.RoleARN, Audience: AWSAudience}
	}
	if g := cp.GCP; g != nil && g.ProjectNumber != "" {
		s.GCP = &GCPSetup{
			Audience:            GCPAudience(g.ProjectNumber, g.WorkloadIdentityFederationPoolID, g.WorkloadIdentityFederationProviderID),
			ProjectNumber:       g.ProjectNumber,
			ServiceAccountEmail: g.ServiceAccountEmail,
		}
	}
	if s.AWS == nil && s.GCP == nil {
		return nil
	}
	return s
}

// AllowedAudience reports whether a run of a factory with cp can get a token
// for aud: the audience of a configured provider, or one of cp.Audiences.
func AllowedAudience(cp *core.CloudProviders, aud string) bool {
	aud = strings.TrimSpace(aud)
	if cp == nil || aud == "" {
		return false
	}
	if s := SetupFor(cp); s != nil {
		if s.AWS != nil && s.AWS.Audience == aud {
			return true
		}
		if s.GCP != nil && s.GCP.Audience == aud {
			return true
		}
	}
	return slices.Contains(cp.Audiences, aud)
}

// SessionName is a valid AWS role session name for a run.
func SessionName(runID string) string { return sessionName(runID) }

// GCPCredentialFile returns an external_account credentials file. With a
// command, the SDK runs the command to get a new token each time it needs
// one (an executable source; GCPAllowExecutablesEnv must be 1). Without a
// command, the SDK reads the token from tokenFile, which must be kept fresh.
func GCPCredentialFile(g *GCPSetup, command, tokenFile string) ([]byte, error) {
	var source map[string]any
	if command != "" {
		source = map[string]any{"executable": map[string]any{"command": command, "timeout_millis": 30000}}
	} else {
		source = map[string]any{"file": tokenFile, "format": map[string]string{"type": "text"}}
	}
	cred := map[string]any{
		"type":               "external_account",
		"audience":           g.Audience,
		"subject_token_type": gcpTokenType,
		"token_url":          gcpTokenURL,
		"credential_source":  source,
	}
	if g.ServiceAccountEmail != "" {
		cred["service_account_impersonation_url"] = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + g.ServiceAccountEmail + ":generateAccessToken"
	}
	return json.MarshalIndent(cred, "", "  ")
}

// GCPExecutableResponse is the output of an executable credential source.
func GCPExecutableResponse(token string, expires time.Time) ([]byte, error) {
	return json.Marshal(map[string]any{
		"version":         1,
		"success":         true,
		"token_type":      gcpTokenType,
		"id_token":        token,
		"expiration_time": expires.Unix(),
	})
}

// TokenFiles returns the token files of a setup in dir, by audience.
func TokenFiles(s *Setup, dir string) map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	if s.AWS != nil {
		out[path.Join(dir, "aws-web-identity-token")] = s.AWS.Audience
	}
	if s.GCP != nil {
		out[path.Join(dir, "gcp-subject-token")] = s.GCP.Audience
	}
	return out
}
