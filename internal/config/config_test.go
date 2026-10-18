package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

const testKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "of.yaml")
	if err := os.WriteFile(file, []byte("listen: \":7000\"\nlog_level: warn\npublic_url: https://file.example\nsecret_key: "+testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	BindFlags(fs)
	if err := fs.Parse([]string{"--config", file}); err != nil {
		t.Fatal(err)
	}
	// A setting has no flag: only --config is a flag.
	if err := fs.Parse([]string{"--log-level", "error"}); err == nil {
		t.Error("--log-level is a flag")
	}
	c, err := LoadFrom(fs, env(map[string]string{"OF_PUBLIC_URL": "https://env.example/", "OF_DATA_DIR": dir, "OF_LOG_LEVEL": "error"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7000" {
		t.Errorf("listen from file: got %q", c.Listen)
	}
	if c.PublicURL != "https://env.example" {
		t.Errorf("public url from env: got %q", c.PublicURL)
	}
	if c.LogLevel != "error" {
		t.Errorf("log level from env: got %q", c.LogLevel)
	}
	if c.DatabaseURL != "sqlite://"+filepath.Join(dir, "openfactory.db") {
		t.Errorf("default database: %q", c.DatabaseURL)
	}
	if c.SandboxBackend != BackendKubernetes || c.LogFormat != "text" {
		t.Errorf("defaults: %+v", c)
	}
	if c.PluginDir != filepath.Join(dir, "plugins") {
		t.Errorf("default plugin dir: %q", c.PluginDir)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing key", map[string]string{}, "OF_SECRET_KEY is required"},
		{"dev ok", map[string]string{"OF_DEV": "true"}, ""},
		{"plugin backend on the default path", map[string]string{"OF_DEV": "1", "OF_SANDBOX_BACKEND": "vm"}, ""},
		{"bad backend name", map[string]string{"OF_DEV": "1", "OF_SANDBOX_BACKEND": "bad name!"}, "OF_SANDBOX_BACKEND"},
		{"bad db", map[string]string{"OF_DEV": "1", "OF_DATABASE_URL": "mysql://x"}, "OF_DATABASE_URL"},
		{"bad url", map[string]string{"OF_DEV": "1", "OF_PUBLIC_URL": "localhost"}, "OF_PUBLIC_URL"},
		{"bad internal url", map[string]string{"OF_DEV": "1", "OF_INTERNAL_URL": "svc:8080"}, "OF_INTERNAL_URL"},
		{"internal url ok", map[string]string{"OF_DEV": "1", "OF_INTERNAL_URL": "http://of.ns.svc:8080/"}, ""},
		{"bad bool", map[string]string{"OF_DEV": "maybe"}, "not a boolean"},
		{"bad key", map[string]string{"OF_SECRET_KEY": "short"}, "OF_SECRET_KEY"},
		{"bad level", map[string]string{"OF_DEV": "1", "OF_LOG_LEVEL": "loud"}, "OF_LOG_LEVEL"},
		{"missing pod template", map[string]string{"OF_DEV": "1", "OF_K8S_POD_TEMPLATE": "/nonexistent/pod.yaml"}, "OF_K8S_POD_TEMPLATE"},
		{"memory", map[string]string{"OF_SECRET_KEY": testKey, "OF_DATABASE_URL": "sqlite://:memory:"}, ""},
		{"bad lease", map[string]string{"OF_DEV": "1", "OF_LEADER_LEASE_TTL": "soon"}, "OF_LEADER_LEASE_TTL"},
		{"negative attempts", map[string]string{"OF_DEV": "1", "OF_OUTBOX_MAX_ATTEMPTS": "-1"}, "OF_OUTBOX_MAX_ATTEMPTS"},
		{"bad object store", map[string]string{"OF_DEV": "1", "OF_OBJECT_STORE_URL": "ftp://x"}, "OF_OBJECT_STORE_URL"},
		{"bad retention", map[string]string{"OF_DEV": "1", "OF_OBJECT_STORE_RETENTION": "forever"}, "OF_OBJECT_STORE_RETENTION"},
		{"short retention interval", map[string]string{"OF_DEV": "1", "OF_OBJECT_STORE_RETENTION_INTERVAL": "5s"}, "OF_OBJECT_STORE_RETENTION_INTERVAL"},
		{"object stores ok", map[string]string{"OF_DEV": "1", "OF_OBJECT_STORE_URL": "s3://b/p?region=x", "OF_OBJECT_STORE_ARTIFACTS_URL": "azblob://acct/c", "OF_OBJECT_STORE_PRESIGN_TTL": "5m"}, ""},
		{"bad snapshots url", map[string]string{"OF_DEV": "1", "OF_OBJECT_STORE_SNAPSHOTS_URL": "ftp://x"}, "OF_OBJECT_STORE_SNAPSHOTS_URL"},
		{"snapshots url ok", map[string]string{"OF_DEV": "1", "OF_OBJECT_STORE_SNAPSHOTS_URL": "s3://b/snap"}, ""},
		{"snapshot max zero", map[string]string{"OF_DEV": "1", "OF_SNAPSHOT_MAX_BYTES": "0"}, "OF_SNAPSHOT_MAX_BYTES"},
		{"snapshot max text", map[string]string{"OF_DEV": "1", "OF_SNAPSHOT_MAX_BYTES": "abc"}, "OF_SNAPSHOT_MAX_BYTES"},
		{"snapshot max ok", map[string]string{"OF_DEV": "1", "OF_SNAPSHOT_MAX_BYTES": "1048576"}, ""},
		{"oidc without client", map[string]string{"OF_DEV": "1", "OF_OIDC_ISSUER": "https://idp.example"}, "OF_OIDC_CLIENT_ID"},
		{"oidc bad issuer", map[string]string{"OF_DEV": "1", "OF_OIDC_ISSUER": "idp", "OF_OIDC_CLIENT_ID": "c"}, "OF_OIDC_ISSUER"},
		{"oidc bad role", map[string]string{"OF_DEV": "1", "OF_OIDC_ISSUER": "https://idp.example", "OF_OIDC_CLIENT_ID": "c", "OF_OIDC_DEFAULT_ROLE": "owner"}, "OF_OIDC_DEFAULT_ROLE"},
		{"oidc ok", map[string]string{"OF_DEV": "1", "OF_OIDC_ISSUER": "https://idp.example", "OF_OIDC_CLIENT_ID": "c"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.env["OF_DATA_DIR"] = t.TempDir()
			_, err := LoadFrom(nil, env(tc.env))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestUnknownFileKey(t *testing.T) {
	file := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(file, []byte("lissten: x\n"), 0o600)
	_, err := LoadFrom(nil, env(map[string]string{"OF_CONFIG": file, "OF_DEV": "1"}))
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("got %v", err)
	}
}

func TestKeys(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadFrom(nil, env(map[string]string{"OF_DEV": "1", "OF_DATA_DIR": dir}))
	if err != nil {
		t.Fatal(err)
	}
	k1, err := c.MasterKey()
	if err != nil || len(k1) != 32 {
		t.Fatalf("master key: %v %d", err, len(k1))
	}
	k2, _ := c.MasterKey()
	if !bytes.Equal(k1, k2) {
		t.Fatal("dev key not persisted")
	}
	a, b := DeriveKey(k1, LabelRunnerToken), DeriveKey(k1, LabelWebSession)
	if bytes.Equal(a, b) || len(a) != 32 {
		t.Fatal("derived keys must differ")
	}
}

func TestSandboxBackendDefaults(t *testing.T) {
	cases := []struct {
		name        string
		env         map[string]string
		wantBackend string
		wantWarning bool
	}{
		{"production default", map[string]string{"OF_SECRET_KEY": testKey}, BackendKubernetes, false},
		{"dev default", map[string]string{"OF_DEV": "true"}, BackendLocal, false},
		{"dev kubernetes", map[string]string{"OF_DEV": "true", "OF_SANDBOX_BACKEND": "kubernetes"}, BackendKubernetes, false},
		{"explicit local outside dev", map[string]string{"OF_SECRET_KEY": testKey, "OF_SANDBOX_BACKEND": "Local"}, BackendLocal, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.env["OF_DATA_DIR"] = t.TempDir()
			c, err := LoadFrom(nil, env(tc.env))
			if err != nil {
				t.Fatal(err)
			}
			if c.SandboxBackend != tc.wantBackend {
				t.Errorf("backend %q, want %q", c.SandboxBackend, tc.wantBackend)
			}
			if got := c.LocalBackendWarning() != ""; got != tc.wantWarning {
				t.Errorf("warning %v, want %v", got, tc.wantWarning)
			}
		})
	}
}

func TestSandboxImage(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{"OF_DEV": "1", "OF_DATA_DIR": t.TempDir(), "OF_SANDBOX_IMAGE": "img:2"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SandboxImage != "img:2" {
		t.Errorf("image %q, want img:2", c.SandboxImage)
	}
}

func TestPluginPathsAndOIDCDefaults(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{"OF_DEV": "1", "OF_DATA_DIR": t.TempDir(), "OF_PLUGINS": " /a/x, ,/b/y "}))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.PluginPaths(); len(got) != 2 || got[0] != "/a/x" || got[1] != "/b/y" {
		t.Errorf("plugin paths %q", got)
	}
	if !c.OIDCRequireVerifiedEmail {
		t.Error("OIDC verified email should default to true")
	}
}

// TestUnknownKeyInFile checks that the YAML file refuses a key that the
// server does not have.
func TestUnknownKeyInFile(t *testing.T) {
	for _, key := range []string{"bogus: 1", "execution: runner"} {
		dir := t.TempDir()
		file := filepath.Join(dir, "of.yaml")
		if err := os.WriteFile(file, []byte(key+"\nsecret_key: "+testKey+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadFrom(nil, env(map[string]string{"OF_CONFIG": file, "OF_DATA_DIR": dir}))
		if err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Errorf("%s: error %v, want containing %q", key, err, "unknown key")
		}
	}
}

func TestRunnerSettingsFromEnv(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{
		"OF_DEV": "1", "OF_DATA_DIR": t.TempDir(),
		"OF_RUNNER_COMMAND": "/bin/of runner", "OF_RUNNER_EXEC_COMMAND": "cat",
		"OF_RUNNER_IMAGE": "registry.test/runner:1",
	}))
	if c != nil && c.RunnerImage != "registry.test/runner:1" {
		t.Errorf("runner image %q", c.RunnerImage)
	}
	if err != nil {
		t.Fatal(err)
	}
	if c.RunnerCommand != "/bin/of runner" || c.RunnerExecCommand != "cat" {
		t.Errorf("got %q %q", c.RunnerCommand, c.RunnerExecCommand)
	}
}

func TestSnapshotMaxBytesDefault(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{"OF_DEV": "1", "OF_DATA_DIR": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SnapshotMaxBytes != 2<<30 {
		t.Fatalf("SnapshotMaxBytes = %d, want %d", c.SnapshotMaxBytes, int64(2<<30))
	}
}

func TestExecutionLimitDefaults(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{"OF_DEV": "1", "OF_DATA_DIR": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if c.QueueTimeoutDuration() != 30*time.Minute || c.SandboxStartTimeoutDuration() != 30*time.Minute ||
		c.MaxSessionDurationValue() != 8*time.Hour || c.SessionStopGraceDuration() != 2*time.Minute ||
		c.IdleTimeoutDefaultDuration() != 10*time.Minute || c.IdleTimeoutMaxDuration() != 60*time.Minute ||
		c.MaxSandboxes != 0 || c.MaxOrchestrationDepthValue() != 1 || c.ShutdownDrainDuration() != 5*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestExecutionLimitSettings(t *testing.T) {
	c, err := LoadFrom(nil, env(map[string]string{
		"OF_DEV": "1", "OF_DATA_DIR": t.TempDir(),
		"OF_QUEUE_TIMEOUT": "5m", "OF_SANDBOX_START_TIMEOUT": "10m", "OF_MAX_SESSION_DURATION": "2h",
		"OF_SESSION_STOP_GRACE": "0", "OF_MAX_SANDBOXES": "3", "OF_MAX_ORCHESTRATION_DEPTH": "2",
		"OF_IDLE_TIMEOUT_DEFAULT": "0", "OF_IDLE_TIMEOUT_MAX": "30m", "OF_SHUTDOWN_DRAIN": "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.QueueTimeoutDuration() != 5*time.Minute || c.SandboxStartTimeoutDuration() != 10*time.Minute ||
		c.MaxSessionDurationValue() != 2*time.Hour || c.SessionStopGraceDuration() != 0 ||
		c.MaxSandboxes != 3 || c.IdleTimeoutDefaultDuration() != 0 || c.IdleTimeoutMaxDuration() != 30*time.Minute ||
		c.MaxOrchestrationDepthValue() != 2 || c.ShutdownDrainDuration() != 0 {
		t.Fatalf("unexpected values: %+v", c)
	}
}

func TestExecutionLimitValidation(t *testing.T) {
	bad := []map[string]string{
		{"OF_QUEUE_TIMEOUT": "0"},
		{"OF_QUEUE_TIMEOUT": "soon"},
		{"OF_SANDBOX_START_TIMEOUT": "-1m"},
		{"OF_MAX_SESSION_DURATION": "0s"},
		{"OF_SESSION_STOP_GRACE": "-1s"},
		{"OF_SHUTDOWN_DRAIN": "-1s"},
		{"OF_MAX_SANDBOXES": "-1"},
		{"OF_MAX_ORCHESTRATION_DEPTH": "-1"},
		{"OF_IDLE_TIMEOUT_MAX": "2h"},
		{"OF_IDLE_TIMEOUT_DEFAULT": "20m", "OF_IDLE_TIMEOUT_MAX": "10m"},
		{"OF_IDLE_TIMEOUT_DEFAULT": "-1m"},
	}
	for _, m := range bad {
		m["OF_DEV"], m["OF_DATA_DIR"] = "1", t.TempDir()
		if _, err := LoadFrom(nil, env(m)); err == nil {
			t.Errorf("settings %v: want an error", m)
		}
	}
}

func TestSandboxURL(t *testing.T) {
	c, err := LoadFrom(pflag.NewFlagSet("t", pflag.ContinueOnError), env(map[string]string{"OF_DEV": "1", "OF_PUBLIC_URL": "https://pub.example"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.InternalURL != "" || c.SandboxURL() != "https://pub.example" {
		t.Errorf("default: internal %q sandbox %q", c.InternalURL, c.SandboxURL())
	}
	c, err = LoadFrom(pflag.NewFlagSet("t", pflag.ContinueOnError), env(map[string]string{"OF_DEV": "1", "OF_PUBLIC_URL": "https://pub.example", "OF_INTERNAL_URL": "http://of.ns.svc:8080/"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SandboxURL() != "http://of.ns.svc:8080" {
		t.Errorf("sandbox url = %q", c.SandboxURL())
	}
}

const (
	testKey2 = "ICEiIyQlJicoKSorLC0uLzAxMjM0NTY3ODk6Ozw9Pj8="
	testKey3 = "QEFCQ0RFRkdISUpLTE1OT1BRUlNUVVZXWFlaW1xdXl8="
)

func TestKeyValidation(t *testing.T) {
	base := func(extra map[string]string) error {
		m := map[string]string{"OF_SECRET_KEY": testKey}
		for k, v := range extra {
			m[k] = v
		}
		_, err := LoadFrom(nil, env(m))
		return err
	}
	if err := base(map[string]string{"OF_SECRET_KEY_PREVIOUS": testKey2 + "," + testKey3, "OF_SIGNING_KEY": testKey3, "OF_SIGNING_KEY_PREVIOUS": testKey2}); err != nil {
		t.Fatalf("valid key settings: %v", err)
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"bad base64 previous":    {map[string]string{"OF_SECRET_KEY_PREVIOUS": "!!!"}, "OF_SECRET_KEY_PREVIOUS item 1"},
		"bad item in list":       {map[string]string{"OF_SECRET_KEY_PREVIOUS": testKey2 + ",!!!"}, "item 2"},
		"short previous":         {map[string]string{"OF_SECRET_KEY_PREVIOUS": "AAAA"}, "must be 32 bytes"},
		"bad signing key":        {map[string]string{"OF_SIGNING_KEY": "!!!"}, "OF_SIGNING_KEY"},
		"short signing key":      {map[string]string{"OF_SIGNING_KEY": "AAAA"}, "must be 32 bytes"},
		"bad signing previous":   {map[string]string{"OF_SIGNING_KEY": testKey2, "OF_SIGNING_KEY_PREVIOUS": "AAAA"}, "OF_SIGNING_KEY_PREVIOUS"},
		"signing previous alone": {map[string]string{"OF_SIGNING_KEY_PREVIOUS": testKey2}, "needs OF_SIGNING_KEY"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := base(tc.env); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want containing %q", err, tc.want)
			}
		})
	}
}

func decode(t *testing.T, s string) []byte {
	t.Helper()
	c := Config{SecretKey: s}
	k, err := c.MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSigningAndEncryptionKeys(t *testing.T) {
	// Without OF_SIGNING_KEY the signing keys follow the secret key.
	c, err := LoadFrom(nil, env(map[string]string{"OF_SECRET_KEY": testKey2, "OF_SECRET_KEY_PREVIOUS": testKey}))
	if err != nil {
		t.Fatal(err)
	}
	ec, ep, err := c.EncryptionKeys()
	if err != nil || !bytes.Equal(ec, decode(t, testKey2)) || len(ep) != 1 || !bytes.Equal(ep[0], decode(t, testKey)) {
		t.Fatalf("encryption keys: %v", err)
	}
	sc, sp, err := c.SigningKeys()
	if err != nil || !bytes.Equal(sc, ec) || len(sp) != 1 || !bytes.Equal(sp[0], ep[0]) {
		t.Fatalf("signing keys must follow the secret key: %v", err)
	}

	// With OF_SIGNING_KEY a change of the secret key leaves signing alone.
	signing := func(secret string) ([]byte, [][]byte) {
		c, err := LoadFrom(nil, env(map[string]string{"OF_SECRET_KEY": secret, "OF_SIGNING_KEY": testKey3, "OF_SIGNING_KEY_PREVIOUS": testKey}))
		if err != nil {
			t.Fatal(err)
		}
		cur, prev, err := c.SigningKeys()
		if err != nil {
			t.Fatal(err)
		}
		return cur, prev
	}
	a, ap := signing(testKey)
	b, bp := signing(testKey2)
	if !bytes.Equal(a, b) || !bytes.Equal(a, decode(t, testKey3)) || len(ap) != 1 || !bytes.Equal(ap[0], bp[0]) {
		t.Fatal("signing keys changed with the secret key")
	}
	if !bytes.Equal(DeriveKey(a, LabelWebSession), DeriveKey(b, LabelWebSession)) {
		t.Fatal("session key changed with the secret key")
	}
	cur, prev := DeriveKeys(a, ap, LabelRunnerToken)
	if !bytes.Equal(cur, DeriveKey(a, LabelRunnerToken)) || len(prev) != 1 || !bytes.Equal(prev[0], DeriveKey(ap[0], LabelRunnerToken)) {
		t.Fatal("DeriveKeys differs from DeriveKey")
	}
}

func TestIssuerURL(t *testing.T) {
	fs := func() *pflag.FlagSet { return pflag.NewFlagSet("t", pflag.ContinueOnError) }
	c, err := LoadFrom(fs(), env(map[string]string{"OF_DEV": "1", "OF_PUBLIC_URL": "https://pub.example"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.OIDCIssuerURL != "" || c.IssuerURL() != "https://pub.example" {
		t.Errorf("default: setting %q issuer %q", c.OIDCIssuerURL, c.IssuerURL())
	}
	c, err = LoadFrom(fs(), env(map[string]string{"OF_DEV": "1", "OF_PUBLIC_URL": "https://pub.example", "OF_OIDC_ISSUER_URL": "https://issuer.example/"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.IssuerURL() != "https://issuer.example" {
		t.Errorf("issuer = %q", c.IssuerURL())
	}
	// The YAML key works.
	file := filepath.Join(t.TempDir(), "of.yaml")
	_ = os.WriteFile(file, []byte("oidc_issuer_url: https://file.example\n"), 0o600)
	c, err = LoadFrom(fs(), env(map[string]string{"OF_DEV": "1", "OF_CONFIG": file}))
	if err != nil || c.IssuerURL() != "https://file.example" {
		t.Fatalf("yaml: %v %v", c, err)
	}
	if _, err := LoadFrom(fs(), env(map[string]string{"OF_DEV": "1", "OF_OIDC_ISSUER_URL": "issuer.example"})); err == nil || !strings.Contains(err.Error(), "OF_OIDC_ISSUER_URL") {
		t.Errorf("bad issuer url: %v", err)
	}
}

func TestPluginEnv(t *testing.T) {
	fs := func() *pflag.FlagSet { return pflag.NewFlagSet("t", pflag.ContinueOnError) }
	c, err := LoadFrom(fs(), env(map[string]string{"OF_DEV": "1", "OF_PLUGIN_ENV": "acme=ACME_URL, ACME_TOKEN; other=OTHER_URL"}))
	if err != nil {
		t.Fatal(err)
	}
	m := c.PluginEnvMap()
	if strings.Join(m["acme"], ",") != "ACME_URL,ACME_TOKEN" || strings.Join(m["other"], ",") != "OTHER_URL" || len(m) != 2 {
		t.Errorf("map = %v", m)
	}
	// The YAML file can hold a map.
	file := filepath.Join(t.TempDir(), "of.yaml")
	_ = os.WriteFile(file, []byte("plugin_env:\n  acme: [ACME_URL, ACME_TOKEN]\n  other: OTHER_URL\n"), 0o600)
	c, err = LoadFrom(fs(), env(map[string]string{"OF_DEV": "1", "OF_CONFIG": file}))
	if err != nil {
		t.Fatal(err)
	}
	m = c.PluginEnvMap()
	if strings.Join(m["acme"], ",") != "ACME_URL,ACME_TOKEN" || strings.Join(m["other"], ",") != "OTHER_URL" {
		t.Errorf("yaml map = %v", m)
	}
	if _, err := LoadFrom(fs(), env(map[string]string{"OF_DEV": "1", "OF_PLUGIN_ENV": "no-equals-sign"})); err == nil || !strings.Contains(err.Error(), "OF_PLUGIN_ENV") {
		t.Errorf("bad value: %v", err)
	}
	c, _ = LoadFrom(fs(), env(map[string]string{"OF_DEV": "1"}))
	if len(c.PluginEnvMap()) != 0 {
		t.Errorf("default map = %v", c.PluginEnvMap())
	}
}
