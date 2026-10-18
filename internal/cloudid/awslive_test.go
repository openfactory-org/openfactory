//go:build aws_live

// Live check of cloud identity against a real AWS account.
// It never runs in CI. See docs/self-hosting/local-testing.md#real-world-checks.
//
// Step 1 writes the two public issuer documents for a fixed seed:
//
//	OF_AWS_LIVE_SEED=<hex> OF_AWS_LIVE_ISSUER=https://<bucket>.s3.<region>.amazonaws.com \
//	OF_AWS_LIVE_OUT=/tmp/issuer go test -tags aws_live -run TestAWSLivePublish ./internal/cloudid
//
// Publish <out>/.well-known/openid-configuration and <out>/.well-known/jwks.json
// at the issuer URL, create an IAM OIDC provider for it (audience
// sts.amazonaws.com) and a role that trusts the subject
// team/team_live/factory/fac_live/agent/reader and may read one object.
//
// Step 2 runs the checks with the AWS CLI on PATH:
//
//	OF_AWS_LIVE_SEED=... OF_AWS_LIVE_ISSUER=... OF_AWS_LIVE_ROLE_ARN=arn:aws:iam::<acct>:role/<name> \
//	OF_AWS_LIVE_OBJECT=s3://<bucket>/<key> OF_AWS_LIVE_EXPECT=<content> OF_AWS_LIVE_REGION=<region> \
//	go test -tags aws_live -run TestAWSLiveIdentity -v -timeout 15m ./internal/cloudid
package cloudid

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func liveEnv(t *testing.T, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			t.Skipf("%s is not set", n)
		}
		out[n] = v
	}
	return out
}

func liveIssuer(t *testing.T, env map[string]string) *Issuer {
	t.Helper()
	seed, err := hex.DecodeString(env["OF_AWS_LIVE_SEED"])
	if err != nil || len(seed) < 16 {
		t.Fatalf("OF_AWS_LIVE_SEED must be at least 16 bytes of hex")
	}
	i, err := NewIssuer(seed, env["OF_AWS_LIVE_ISSUER"])
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func liveRun(agent string) *core.Run {
	return &core.Run{ID: "run_live_" + agent, TeamID: "team_live", FactoryID: "fac_live", AgentName: agent}
}

func TestAWSLivePublish(t *testing.T) {
	env := liveEnv(t, "OF_AWS_LIVE_SEED", "OF_AWS_LIVE_ISSUER", "OF_AWS_LIVE_OUT")
	i := liveIssuer(t, env)
	dir := filepath.Join(env["OF_AWS_LIVE_OUT"], ".well-known")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]map[string]any{"openid-configuration": i.Discovery(), "jwks.json": i.JWKS()} {
		b, _ := json.MarshalIndent(doc, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("wrote the issuer documents to %s; subject for the trust policy: %s", dir, Subject(liveRun("reader")))
}

// awsRead runs `aws s3 cp <object> -` with only the web identity variables,
// in an empty HOME so that no profile, cache or other credential is used.
func awsRead(t *testing.T, env map[string]string, tokenFile, roleARN string) (string, error) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command("aws", "s3", "cp", env["OF_AWS_LIVE_OBJECT"], "-")
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"AWS_CONFIG_FILE=" + filepath.Join(home, "none"), "AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(home, "none"),
		"AWS_REGION=" + env["OF_AWS_LIVE_REGION"], "AWS_ROLE_ARN=" + roleARN,
		"AWS_WEB_IDENTITY_TOKEN_FILE=" + tokenFile, "AWS_ROLE_SESSION_NAME=openfactory-live",
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestAWSLiveIdentity(t *testing.T) {
	env := liveEnv(t, "OF_AWS_LIVE_SEED", "OF_AWS_LIVE_ISSUER", "OF_AWS_LIVE_ROLE_ARN", "OF_AWS_LIVE_OBJECT", "OF_AWS_LIVE_EXPECT", "OF_AWS_LIVE_REGION")
	i := liveIssuer(t, env)
	dir := t.TempDir()
	cp := &core.CloudProviders{}
	if err := json.Unmarshal([]byte(`{"aws":{"roleArn":"`+env["OF_AWS_LIVE_ROLE_ARN"]+`"}}`), cp); err != nil {
		t.Fatal(err)
	}

	// 1. The credentials that a run gets read the object.
	creds, err := i.RunCredentials(liveRun("reader"), cp, dir)
	if err != nil || creds == nil {
		t.Fatalf("RunCredentials: %v %v", creds, err)
	}
	tokenFile := creds.Env["AWS_WEB_IDENTITY_TOKEN_FILE"]
	for f, b := range creds.Files {
		if err := os.WriteFile(f, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := awsRead(t, env, tokenFile, creds.Env["AWS_ROLE_ARN"]); err != nil || out != env["OF_AWS_LIVE_EXPECT"] {
		t.Fatalf("read with run credentials: %v: %s", err, out)
	}
	t.Log("PASS: run credentials read the object")

	// 2. A token of another agent is refused by the trust policy.
	other, _, err := i.Token(liveRun("other"), AWSAudience, 5*time.Minute, TokenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(dir, "other-token")
	_ = os.WriteFile(otherFile, []byte(other), 0o600)
	if out, err := awsRead(t, env, otherFile, creds.Env["AWS_ROLE_ARN"]); err == nil {
		t.Fatalf("a token of another agent read the object: %s", out)
	}
	t.Log("PASS: a token of another agent is refused")

	// 3. An expired token is refused, and a fresh token written to the same
	// file works again. This is how the runner refreshes cloud identity.
	short, exp, err := i.Token(liveRun("reader"), AWSAudience, 90*time.Second, TokenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(tokenFile, []byte(short), 0o600)
	if out, err := awsRead(t, env, tokenFile, creds.Env["AWS_ROLE_ARN"]); err != nil || out != env["OF_AWS_LIVE_EXPECT"] {
		t.Fatalf("read with a short token: %v: %s", err, out)
	}
	time.Sleep(time.Until(exp) + 45*time.Second)
	if out, err := awsRead(t, env, tokenFile, creds.Env["AWS_ROLE_ARN"]); err == nil {
		t.Fatalf("an expired token read the object: %s", out)
	}
	t.Log("PASS: an expired token is refused")
	fresh, _, err := i.Token(liveRun("reader"), AWSAudience, 0, TokenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(tokenFile, []byte(fresh), 0o600)
	if out, err := awsRead(t, env, tokenFile, creds.Env["AWS_ROLE_ARN"]); err != nil || out != env["OF_AWS_LIVE_EXPECT"] {
		t.Fatalf("read after refresh: %v: %s", err, out)
	}
	t.Log("PASS: a refreshed token file reads the object again")
}
