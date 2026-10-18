package runner

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/openfactory-org/openfactory/internal/cloudid"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// cloudRefreshEvery is how often the runner writes a new AWS web identity
// token. The token lives one hour; the AWS SDKs read the file again each
// time they refresh their credentials. Tests change it.
var cloudRefreshEvery = cloudid.RefreshInterval

// cloudTokenTTL is the life that the runner asks for, in seconds.
const cloudTokenTTL = 3600

// setupCloud writes the cloud credential files of the session and sets
// their variables:
//
//   - AWS: a web identity token file that the runner writes again every
//     cloudRefreshEvery. The AWS SDKs have no command source for a web
//     identity token, but they read the file again on each refresh.
//   - GCP: an external_account file with an executable source that runs the
//     cloud token helper, so the SDK gets a new token each time it needs one.
//     Without a helper command, a token file that the runner refreshes.
//
// The files are in StateDir/cloud, outside the workspace.
func (r *runner) setupCloud(ctx context.Context, ci *runnerproto.CloudIdentity) (string, error) {
	dir := filepath.Join(r.cfg.StateDir, "cloud")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	env := map[string]string{}
	files := map[string]string{} // token file -> audience
	if a := ci.AWS; a != nil && a.RoleARN != "" {
		file := filepath.Join(dir, "aws-web-identity-token")
		files[file] = a.Audience
		env["AWS_ROLE_ARN"] = a.RoleARN
		env["AWS_WEB_IDENTITY_TOKEN_FILE"] = file
		env["AWS_ROLE_SESSION_NAME"] = a.SessionName
	}
	if g := ci.GCP; g != nil && g.Audience != "" {
		tokFile := filepath.Join(dir, "gcp-subject-token")
		command := ""
		if r.cfg.CloudTokenCommand != "" {
			command = r.cfg.CloudTokenCommand + " --audience " + shellQuote(g.Audience) + " --format " + CloudTokenFormatGCP
			env[cloudid.GCPAllowExecutablesEnv] = "1"
		} else {
			files[tokFile] = g.Audience
		}
		b, err := cloudid.GCPCredentialFile(&cloudid.GCPSetup{Audience: g.Audience, ProjectNumber: g.ProjectNumber, ServiceAccountEmail: g.ServiceAccountEmail}, command, tokFile)
		if err != nil {
			return "", err
		}
		credFile := filepath.Join(dir, "gcp-credentials.json")
		if err := writeFileAtomic(credFile, b, 0o600); err != nil {
			return "", err
		}
		env["GOOGLE_APPLICATION_CREDENTIALS"] = credFile
		env["CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE"] = credFile
		env["GOOGLE_CLOUD_PROJECT_NUMBER"] = g.ProjectNumber
	}
	if r.cfg.CloudTokenCommand != "" {
		// Other OIDC services: the agent runs this command with --audience.
		env["OF_CLOUD_TOKEN_COMMAND"] = r.cfg.CloudTokenCommand
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
		if err := writeFileAtomic(file, []byte(resp.Token), 0o600); err != nil {
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
