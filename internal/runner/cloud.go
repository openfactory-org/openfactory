package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// cloudRefreshEvery is how often the runner writes a new token file. The
// token lives one hour; the SDKs read the file again each time they refresh
// their credentials. Tests change it.
var cloudRefreshEvery = 20 * time.Minute

// cloudTokenTTL is the life that the runner asks for, in seconds.
const cloudTokenTTL = 3600

// GCP token types and URLs of an external_account credentials file.
const (
	gcpTokenType = "urn:ietf:params:oauth:token-type:jwt" //nolint:gosec // G101: a token type URN, not a credential
	gcpTokenURL  = "https://sts.googleapis.com/v1/token"  //nolint:gosec // G101: a public endpoint URL, not a credential
	// gcpAllowExecutablesEnv must be 1 before the Google SDKs run an
	// executable credential source.
	gcpAllowExecutablesEnv = "GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES"
)

// gcpCredentialFile returns an external_account credentials file. With a
// command, the SDK runs the command to get a new token each time it needs
// one (an executable source; gcpAllowExecutablesEnv must be 1). Without a
// command, the SDK reads the token from tokenFile, which must be kept fresh.
func gcpCredentialFile(g *runnerproto.CloudGCP, command, tokenFile string) ([]byte, error) {
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

// gcpExecutableResponse is the output of an executable credential source.
func gcpExecutableResponse(token string, expires time.Time) ([]byte, error) {
	return json.Marshal(map[string]any{
		"version":         1,
		"success":         true,
		"token_type":      gcpTokenType,
		"id_token":        token,
		"expiration_time": expires.Unix(),
	})
}

// setupCloud writes the cloud credential files of the session and sets
// their variables. For GCP it writes an external_account file with an
// executable source that runs the cloud token helper, so the SDK gets a new
// token each time it needs one. Without a helper command, it writes a token
// file that the runner refreshes.
//
// The files are in the cloud directory, outside the workspace.
func (r *runner) setupCloud(ctx context.Context, ci *runnerproto.CloudIdentity) (string, error) {
	dir := r.cloudDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	env := map[string]string{}
	files := map[string]string{} // token file -> audience
	command := r.cloudTokenCommand()
	if g := ci.GCP; g != nil && g.Audience != "" {
		tokFile := filepath.Join(dir, "gcp-subject-token")
		gcpCommand := ""
		if command != "" {
			gcpCommand = command + " --audience " + shellQuote(g.Audience) + " --format " + CloudTokenFormatGCP
			env[gcpAllowExecutablesEnv] = "1"
		} else {
			files[tokFile] = g.Audience
		}
		b, err := gcpCredentialFile(g, gcpCommand, tokFile)
		if err != nil {
			return "", err
		}
		credFile := filepath.Join(dir, "gcp-credentials.json")
		if err := r.writeAgentFile(credFile, b, 0o600); err != nil {
			return "", err
		}
		env["GOOGLE_APPLICATION_CREDENTIALS"] = credFile
		env["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"] = credFile
		env["GOOGLE_CLOUD_PROJECT_NUMBER"] = g.ProjectNumber
	}
	if command != "" {
		// Other OIDC services: the agent runs this command with --audience.
		env["OF_CLOUD_TOKEN_COMMAND"] = command
	}
	if err := r.writeCloudTokens(ctx, files); err != nil {
		return "", err
	}
	r.cloudEnv = env
	r.cloudFiles = files
	return "", nil
}

// writeCloudTokens gets a new token for each file and writes it.
func (r *runner) writeCloudTokens(ctx context.Context, files map[string]string) error {
	for file, aud := range files {
		resp, err := r.client.CloudToken(ctx, aud, cloudTokenTTL)
		if err != nil {
			return err
		}
		if err := r.writeAgentFile(file, []byte(resp.Token), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// refreshCloud writes the token files again every cloudRefreshEvery until
// ctx is done. A failure is logged; the next tick tries again, and the old
// token stays valid until its expiry.
func (r *runner) refreshCloud(ctx context.Context) {
	if len(r.cloudFiles) == 0 {
		return
	}
	t := time.NewTicker(cloudRefreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.writeCloudTokens(ctx, r.cloudFiles); err != nil && ctx.Err() == nil {
				r.log.Warn("refresh cloud token", "error", err)
			}
		}
	}
}

// writeFileAtomic writes a temporary file and renames it, so that a reader
// never sees a partial file.
func writeFileAtomic(name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp-*")
	if err != nil {
		return err
	}
	tn := tmp.Name()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tn)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tn)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tn)
		return err
	}
	if err := os.Rename(tn, name); err != nil {
		os.Remove(tn)
		return err
	}
	return nil
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	out := "'"
	for _, c := range s {
		if c == '\'' {
			out += `'\''`
			continue
		}
		out += string(c)
	}
	return out + "'"
}
