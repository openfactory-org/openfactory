// Package config loads the Open Factory server configuration from defaults,
// an optional YAML file and OF_* environment variables (later sources win:
// defaults < file < environment). The only flag is --config.
package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/secrets"
)

// Sandbox backends. Kubernetes is the only production backend; the local
// worktree backend exists for development and tests.
const (
	BackendLocal      = "local"
	BackendKubernetes = "kubernetes"
)

// Config is the server configuration. Each field is settable from the YAML
// file (yaml tag) and the environment (env tag). There is no flag for a field.
type Config struct {
	Listen      string `yaml:"listen" env:"OF_LISTEN"`
	PublicURL   string `yaml:"public_url" env:"OF_PUBLIC_URL"`
	InternalURL string `yaml:"internal_url" env:"OF_INTERNAL_URL"`
	// OIDCIssuerURL is the issuer (iss claim) of the run tokens that the server
	// signs for cloud identity federation. Cloud providers read its two
	// /.well-known/ documents. Do not confuse it with OIDCIssuer, the external
	// identity provider for sign-in.
	OIDCIssuerURL string `yaml:"oidc_issuer_url" env:"OF_OIDC_ISSUER_URL"`
	DatabaseURL   string `yaml:"database_url" env:"OF_DATABASE_URL"`
	// SecretKey is the base64-encoded 32-byte master key. In dev mode it is
	// created on first start under DataDir when unset.
	SecretKey string `yaml:"secret_key" env:"OF_SECRET_KEY"`
	// SecretKeyPrevious lists old keys (comma-separated) that still read
	// secrets during a key rotation. New writes use SecretKey.
	SecretKeyPrevious string `yaml:"secret_key_previous" env:"OF_SECRET_KEY_PREVIOUS"`
	// SigningKey is the base64-encoded 32-byte key from which the signing keys
	// derive. When it is empty, they derive from SecretKey.
	SigningKey string `yaml:"signing_key" env:"OF_SIGNING_KEY"`
	// SigningKeyPrevious lists old signing keys that still verify during a rotation.
	SigningKeyPrevious string `yaml:"signing_key_previous" env:"OF_SIGNING_KEY_PREVIOUS"`
	DataDir            string `yaml:"data_dir" env:"OF_DATA_DIR"`

	// SandboxBackend defaults to kubernetes, or to local in dev mode. The
	// local backend outside dev mode is allowed but logged as a warning
	// (see LocalBackendWarning).
	SandboxBackend string `yaml:"sandbox_backend" env:"OF_SANDBOX_BACKEND"`
	SandboxRoot    string `yaml:"sandbox_root" env:"OF_SANDBOX_ROOT"`
	// SandboxImage is the default sandbox container image.
	SandboxImage string `yaml:"sandbox_image" env:"OF_SANDBOX_IMAGE"`
	K8sNamespace string `yaml:"k8s_namespace" env:"OF_K8S_NAMESPACE"`
	// K8sPodTemplate is a path to a PodSpec template file (YAML or JSON).
	K8sPodTemplate string `yaml:"k8s_pod_template" env:"OF_K8S_POD_TEMPLATE"`
	Kubeconfig     string `yaml:"kubeconfig" env:"OF_KUBECONFIG"`

	// Kubernetes task Job settings (runner path). Empty means the default.
	// The accessor methods in k8s.go return the parsed values.
	K8sLabels                  string `yaml:"k8s_labels" env:"OF_K8S_LABELS"`
	K8sAnnotations             string `yaml:"k8s_annotations" env:"OF_K8S_ANNOTATIONS"`
	K8sImagePullPolicy         string `yaml:"k8s_image_pull_policy" env:"OF_K8S_IMAGE_PULL_POLICY"`
	K8sRunAsUser               string `yaml:"k8s_run_as_user" env:"OF_K8S_RUN_AS_USER"`
	K8sWorkspaceSizeLimit      string `yaml:"k8s_workspace_size_limit" env:"OF_K8S_WORKSPACE_SIZE_LIMIT"`
	K8sPreflightJob            bool   `yaml:"k8s_preflight_job" env:"OF_K8S_PREFLIGHT_JOB"`
	K8sControlPlaneNamespace   string `yaml:"k8s_control_plane_namespace" env:"OF_K8S_CONTROL_PLANE_NAMESPACE"`
	K8sControlPlaneSelector    string `yaml:"k8s_control_plane_selector" env:"OF_K8S_CONTROL_PLANE_SELECTOR"`
	K8sEgressProxySidecarImage string `yaml:"k8s_egress_proxy_sidecar_image" env:"OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE"`
	// K8sEgressLock closes the direct path on ports 80 and 443 next to the
	// proxy sidecar.
	K8sEgressLock string `yaml:"k8s_egress_lock" env:"OF_K8S_EGRESS_LOCK"`
	// K8sNetworkPolicyCheck and K8sNetworkPolicyEnforcement control the check
	// of the cluster network plugin.
	K8sNetworkPolicyCheck       string `yaml:"k8s_network_policy_check" env:"OF_K8S_NETWORK_POLICY_CHECK"`
	K8sNetworkPolicyEnforcement string `yaml:"k8s_network_policy_enforcement" env:"OF_K8S_NETWORK_POLICY_ENFORCEMENT"`
	// K8sEgressWaitTimeout and K8sEgressWaitAddr control the init container
	// that holds a restricted task back until its NetworkPolicy is active.
	K8sEgressWaitTimeout string `yaml:"k8s_egress_wait_timeout" env:"OF_K8S_EGRESS_WAIT_TIMEOUT"`
	K8sEgressWaitAddr    string `yaml:"k8s_egress_wait_addr" env:"OF_K8S_EGRESS_WAIT_ADDR"`
	// AllowUnenforcedNetworkPolicy lets a restricted task start when the
	// backend cannot create its NetworkPolicy.
	AllowUnenforcedNetworkPolicy bool `yaml:"allow_unenforced_network_policy" env:"OF_ALLOW_UNENFORCED_NETWORK_POLICY"`

	// RunnerCommand starts the runner on the host (local backend). Empty means this binary with the argument runner.
	RunnerCommand string `yaml:"runner_command" env:"OF_RUNNER_COMMAND"`
	// RunnerImage holds the runner binary at /usr/local/bin/openfactory. The
	// kubernetes task backend copies it into each task. On the kubernetes
	// backend and on plugin backends, empty means DefaultRunnerImage (the
	// server image).
	RunnerImage string `yaml:"runner_image" env:"OF_RUNNER_IMAGE"`
	// ClaudeToolImage and CodexToolImage hold the harness CLIs. The
	// kubernetes task backend copies each one into the tasks of its harness.
	// Empty means: derived from RunnerImage (see ToolImageFor).
	ClaudeToolImage string `yaml:"claude_tool_image" env:"OF_CLAUDE_TOOL_IMAGE"`
	CodexToolImage  string `yaml:"codex_tool_image" env:"OF_CODEX_TOOL_IMAGE"`
	// RunnerExecCommand is for development and tests only. It selects the exec harness in runner mode.
	RunnerExecCommand string `yaml:"runner_exec_command" env:"OF_RUNNER_EXEC_COMMAND"`

	Dev       bool   `yaml:"dev" env:"OF_DEV"`
	LogLevel  string `yaml:"log_level" env:"OF_LOG_LEVEL"`
	LogFormat string `yaml:"log_format" env:"OF_LOG_FORMAT"`

	// ReplicaID names this replica in leases and worker routing. Empty means
	// the hostname (Helm sets it to the pod name).
	ReplicaID string `yaml:"replica_id" env:"OF_REPLICA_ID"`
	// Optional lease tuning; empty or zero keeps the built-in defaults.
	LeaderLeaseTTL    string `yaml:"leader_lease_ttl" env:"OF_LEADER_LEASE_TTL"`
	OutboxMaxAttempts int    `yaml:"outbox_max_attempts" env:"OF_OUTBOX_MAX_ATTEMPTS"`
	// ShutdownDrain is the time that a stopping server keeps serving after
	// /readyz reports not ready, before it closes its listener. A load
	// balancer can send new connections until it sees the change. Go
	// duration syntax; empty means the default (5s), 0 means no drain.
	ShutdownDrain string `yaml:"shutdown_drain" env:"OF_SHUTDOWN_DRAIN"`

	// Execution limits (runner path). The duration settings use Go duration
	// syntax. An empty value means the default. The accessor methods return
	// the parsed values.
	QueueTimeout        string `yaml:"queue_timeout" env:"OF_QUEUE_TIMEOUT"`
	SandboxStartTimeout string `yaml:"sandbox_start_timeout" env:"OF_SANDBOX_START_TIMEOUT"`
	MaxSessionDuration  string `yaml:"max_session_duration" env:"OF_MAX_SESSION_DURATION"`
	SessionStopGrace    string `yaml:"session_stop_grace" env:"OF_SESSION_STOP_GRACE"`
	MaxSandboxes        int    `yaml:"max_sandboxes" env:"OF_MAX_SANDBOXES"`
	IdleTimeoutDefault  string `yaml:"idle_timeout_default" env:"OF_IDLE_TIMEOUT_DEFAULT"`
	IdleTimeoutMax      string `yaml:"idle_timeout_max" env:"OF_IDLE_TIMEOUT_MAX"`

	// MaxOrchestrationDepth limits how deep a tree of runs can grow. A
	// run at depth d can start child runs only if d is less than this value.
	// A root run has depth 0. 0 means the default, 1: only a root run starts
	// agents.
	MaxOrchestrationDepth int `yaml:"max_orchestration_depth" env:"OF_MAX_ORCHESTRATION_DEPTH"`

	// Team spend limits in US dollars, for each team of the server.
	// Empty or 0 means no team limit. A factory sets its own limit with
	// spendLimit in factory.yaml.
	TeamSpendLimitDaily   string `yaml:"team_spend_limit_daily" env:"OF_TEAM_SPEND_LIMIT_DAILY"`
	TeamSpendLimitMonthly string `yaml:"team_spend_limit_monthly" env:"OF_TEAM_SPEND_LIMIT_MONTHLY"`

	// SnapshotMaxBytes limits the size of one snapshot part. Default 2 GiB.
	SnapshotMaxBytes int64 `yaml:"snapshot_max_bytes" env:"OF_SNAPSHOT_MAX_BYTES"`

	// ObjectStoreURL selects run-data storage: s3://, gs://, azblob:// or
	// file://. Empty means local disk under DataDir/objects.
	// The same variables are read by objectstore.ConfigFromEnv; the fields
	// exist so they can also come from the YAML file or flags.
	ObjectStoreURL            string `yaml:"object_store_url" env:"OF_OBJECT_STORE_URL"`
	ObjectStoreTranscriptsURL string `yaml:"object_store_transcripts_url" env:"OF_OBJECT_STORE_TRANSCRIPTS_URL"`
	ObjectStoreArtifactsURL   string `yaml:"object_store_artifacts_url" env:"OF_OBJECT_STORE_ARTIFACTS_URL"`
	ObjectStoreAttachmentsURL string `yaml:"object_store_attachments_url" env:"OF_OBJECT_STORE_ATTACHMENTS_URL"`
	ObjectStoreSnapshotsURL   string `yaml:"object_store_snapshots_url" env:"OF_OBJECT_STORE_SNAPSHOTS_URL"`
	ObjectStoreRetention      string `yaml:"object_store_retention" env:"OF_OBJECT_STORE_RETENTION"`
	ObjectStoreRetentionEvery string `yaml:"object_store_retention_interval" env:"OF_OBJECT_STORE_RETENTION_INTERVAL"`
	ObjectStorePresignTTL     string `yaml:"object_store_presign_ttl" env:"OF_OBJECT_STORE_PRESIGN_TTL"`
	ObjectStoreSigningKey     string `yaml:"object_store_signing_key" env:"OF_OBJECT_STORE_SIGNING_KEY"`

	// OIDC login (auth.OIDCConfigFromEnv reads the same variables). Login is
	// enabled when OIDCIssuer is set. The session key is derived from the
	// master key (DeriveKey with LabelWebSession), so it is stable across
	// restarts and replicas.
	OIDCIssuer               string `yaml:"oidc_issuer" env:"OF_OIDC_ISSUER"`
	OIDCClientID             string `yaml:"oidc_client_id" env:"OF_OIDC_CLIENT_ID"`
	OIDCClientSecret         string `yaml:"oidc_client_secret" env:"OF_OIDC_CLIENT_SECRET"`
	OIDCRedirectURL          string `yaml:"oidc_redirect_url" env:"OF_OIDC_REDIRECT_URL"`
	OIDCScopes               string `yaml:"oidc_scopes" env:"OF_OIDC_SCOPES"`
	OIDCAllowedDomains       string `yaml:"oidc_allowed_domains" env:"OF_OIDC_ALLOWED_DOMAINS"`
	OIDCGroupsClaim          string `yaml:"oidc_groups_claim" env:"OF_OIDC_GROUPS_CLAIM"`
	OIDCAdminGroups          string `yaml:"oidc_admin_groups" env:"OF_OIDC_ADMIN_GROUPS"`
	OIDCEditorGroups         string `yaml:"oidc_editor_groups" env:"OF_OIDC_EDITOR_GROUPS"`
	OIDCDefaultRole          string `yaml:"oidc_default_role" env:"OF_OIDC_DEFAULT_ROLE"`
	OIDCDisplayName          string `yaml:"oidc_display_name" env:"OF_OIDC_DISPLAY_NAME"`
	OIDCRequireVerifiedEmail bool   `yaml:"oidc_require_verified_email" env:"OF_OIDC_REQUIRE_VERIFIED_EMAIL"`

	// PluginDir holds gRPC plugin executables (GitLab, Azure DevOps, ...).
	// Only executables named openfactory-plugin-<name> are launched.
	PluginDir string `yaml:"plugin_dir" env:"OF_PLUGIN_DIR"`
	// Plugins is a comma-separated list of extra plugin executable paths;
	// see PluginPaths.
	Plugins string `yaml:"plugins" env:"OF_PLUGINS"`

	// PluginEnvAllow lists server environment variables that every plugin
	// process gets, besides PATH, HOME, TMPDIR, LANG and TZ; see PluginEnvAllowList.
	PluginEnvAllow string `yaml:"plugin_env_allow" env:"OF_PLUGIN_ENV_ALLOW"`

	// PluginEnv lists server environment variables for single plugins, in
	// addition to PluginEnvAllow; see PluginEnvMap. Format:
	// "name=VAR1,VAR2;other=VAR3". In the YAML file the key can also hold a map
	// from plugin name to a list of names.
	PluginEnv string `yaml:"plugin_env" env:"OF_PLUGIN_ENV"`
}

// DefaultRunnerImage is the runner image when OF_RUNNER_IMAGE is not set. It
// is the server image: the server image holds the static openfactory binary
// at /usr/local/bin/openfactory. A release build sets the tag of the release
// with -ldflags "-X github.com/openfactory-org/openfactory/internal/config.DefaultRunnerImage=<image>:<tag>".
// The initializer must stay a plain string literal so that -X can replace it.
var DefaultRunnerImage = "ghcr.io/openfactory-org/openfactory:latest"

// DefaultSnapshotMaxBytes is the default size limit of one snapshot part (2 GiB).
const DefaultSnapshotMaxBytes int64 = 2 << 30

// Defaults returns the built-in defaults.
func Defaults() Config {
	return Config{
		Listen:    ":8080",
		PublicURL: "http://localhost:8080",
		LogLevel:  "info",
		LogFormat: "text",

		SnapshotMaxBytes: DefaultSnapshotMaxBytes,

		QueueTimeout:        "30m",
		SandboxStartTimeout: "30m",
		MaxSessionDuration:  "8h",
		SessionStopGrace:    "2m",
		IdleTimeoutDefault:  "10m",
		IdleTimeoutMax:      "60m",
		ShutdownDrain:       "5s",

		OIDCRequireVerifiedEmail: true,
	}
}

// fields enumerates the configurable struct fields.
func fields(c *Config) []field {
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	out := make([]field, 0, t.NumField())
	for i := range t.NumField() {
		sf := t.Field(i)
		out = append(out, field{
			yaml: sf.Tag.Get("yaml"), env: sf.Tag.Get("env"), v: v.Field(i),
		})
	}
	return out
}

type field struct {
	yaml, env string
	v         reflect.Value
}

func (f field) set(s string) error {
	switch f.v.Kind() {
	case reflect.String:
		f.v.SetString(s)
	case reflect.Int:
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("%s: %q is not an integer", f.env, s)
		}
		f.v.SetInt(int64(n))
	case reflect.Int64:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return fmt.Errorf("%s: %q is not an integer", f.env, s)
		}
		f.v.SetInt(n)
	case reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			return fmt.Errorf("%s: %q is not a boolean", f.env, s)
		}
		f.v.SetBool(b)
	}
	return nil
}

// BindFlags registers the only flag of the server commands, --config. The
// settings come from the YAML file and the environment, not from flags.
func BindFlags(fs *pflag.FlagSet) {
	fs.String("config", os.Getenv("OF_CONFIG"), "path to a YAML configuration file (env OF_CONFIG)")
}

// Load builds the configuration: defaults, then the YAML file named by
// --config (or OF_CONFIG), then OF_* environment variables. fs may be nil.
// The result is validated.
func Load(fs *pflag.FlagSet) (*Config, error) {
	return LoadFrom(fs, os.LookupEnv)
}

// LoadFrom is Load with an injectable environment lookup (tests).
func LoadFrom(fs *pflag.FlagSet, lookup func(string) (string, bool)) (*Config, error) {
	c := Defaults()
	path, _ := lookup("OF_CONFIG")
	if fs != nil {
		if f := fs.Lookup("config"); f != nil && f.Changed {
			path = f.Value.String()
		}
	}
	if path != "" {
		if err := c.loadFile(path); err != nil {
			return nil, err
		}
	}
	for _, f := range fields(&c) {
		if v, ok := lookup(f.env); ok && v != "" {
			if err := f.set(v); err != nil {
				return nil, err
			}
		}
	}
	c.applyDerivedDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) loadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	known := map[string]field{}
	for _, f := range fields(c) {
		known[f.yaml] = f
	}
	for k, v := range raw {
		f, ok := known[k]
		if !ok {
			return fmt.Errorf("config file %s: unknown key %q", path, k)
		}
		if v == nil {
			continue
		}
		if f.yaml == "plugin_env" {
			if m, ok := v.(map[string]any); ok {
				v = pluginEnvString(m)
			}
		}
		if err := f.set(fmt.Sprint(v)); err != nil {
			return fmt.Errorf("config file %s: %w", path, err)
		}
	}
	return nil
}

// Normalize applies the derived defaults (data, sandbox and plugin
// directories, sandbox backend, ...) to a hand-built Config and validates it.
// Load and LoadFrom already do this.
func (c *Config) Normalize() error {
	c.applyDerivedDefaults()
	return c.Validate()
}

func (c *Config) applyDerivedDefaults() {
	if c.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.TempDir()
		}
		c.DataDir = filepath.Join(home, ".openfactory")
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = "sqlite://" + filepath.Join(c.DataDir, "openfactory.db")
	}
	if c.SandboxRoot == "" {
		c.SandboxRoot = filepath.Join(c.DataDir, "sandboxes")
	}
	if c.PluginDir == "" {
		c.PluginDir = filepath.Join(c.DataDir, "plugins")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	c.InternalURL = strings.TrimRight(strings.TrimSpace(c.InternalURL), "/")
	c.OIDCIssuerURL = strings.TrimRight(strings.TrimSpace(c.OIDCIssuerURL), "/")
	c.SandboxBackend = strings.ToLower(strings.TrimSpace(c.SandboxBackend))
	if c.SandboxBackend == "" {
		c.SandboxBackend = DefaultBackend(c.Dev)
	}
	c.RunnerImage = strings.TrimSpace(c.RunnerImage)
	if c.RunnerImage == "" && (c.SandboxBackend == BackendKubernetes || c.IsPluginBackend()) {
		c.RunnerImage = DefaultRunnerImage
	}
	c.LogFormat = strings.ToLower(strings.TrimSpace(c.LogFormat))
}

// SandboxURL returns the base URL that sandboxes use to reach the server:
// InternalURL when set, else PublicURL.
func (c *Config) SandboxURL() string {
	if c.InternalURL != "" {
		return c.InternalURL
	}
	return c.PublicURL
}

// IssuerURL returns the issuer of the run tokens for cloud identity
// federation: OIDCIssuerURL when set, else PublicURL.
func (c *Config) IssuerURL() string {
	if c.OIDCIssuerURL != "" {
		return c.OIDCIssuerURL
	}
	return c.PublicURL
}

// Validate checks the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.Listen == "" {
		errs = append(errs, errors.New("OF_LISTEN must not be empty"))
	}
	if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("OF_PUBLIC_URL %q must be an absolute http(s) URL", c.PublicURL))
	}
	if c.InternalURL != "" {
		if u, err := url.Parse(c.InternalURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("OF_INTERNAL_URL %q must be an absolute http(s) URL", c.InternalURL))
		}
	}
	if c.OIDCIssuerURL != "" {
		if u, err := url.Parse(c.OIDCIssuerURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("OF_OIDC_ISSUER_URL %q must be an absolute http(s) URL", c.OIDCIssuerURL))
		}
	}
	if _, err := ParsePluginEnv(c.PluginEnv); err != nil {
		errs = append(errs, err)
	}
	switch {
	case strings.HasPrefix(c.DatabaseURL, "sqlite://"),
		strings.HasPrefix(c.DatabaseURL, "postgres://"), strings.HasPrefix(c.DatabaseURL, "postgresql://"):
	default:
		errs = append(errs, fmt.Errorf("OF_DATABASE_URL %q: unsupported scheme (want sqlite:// or postgres://)", c.DatabaseURL))
	}
	switch c.SandboxBackend {
	case "", BackendLocal, BackendKubernetes:
	default:
		// Another value names a plugin that serves a sandbox backend. The server
		// checks the plugin at start.
		if !validBackendPluginName(c.SandboxBackend) {
			errs = append(errs, fmt.Errorf("OF_SANDBOX_BACKEND %q: want kubernetes or local, or the name of a plugin that serves a sandbox backend", c.SandboxBackend))
		}
	}
	for _, d := range []struct{ env, v string }{
		{"OF_LEADER_LEASE_TTL", c.LeaderLeaseTTL},
		{"OF_OBJECT_STORE_PRESIGN_TTL", c.ObjectStorePresignTTL},
	} {
		if d.v == "" {
			continue
		}
		if v, err := time.ParseDuration(d.v); err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s %q: want a positive duration such as 30s", d.env, d.v))
		}
	}
	if c.ObjectStoreRetention != "" {
		if v, err := time.ParseDuration(c.ObjectStoreRetention); err != nil || v < 0 {
			errs = append(errs, fmt.Errorf("OF_OBJECT_STORE_RETENTION %q: want a duration such as 720h (0 keeps forever)", c.ObjectStoreRetention))
		}
	}
	if c.ObjectStoreRetentionEvery != "" {
		if v, err := time.ParseDuration(c.ObjectStoreRetentionEvery); err != nil || v < time.Minute {
			errs = append(errs, fmt.Errorf("OF_OBJECT_STORE_RETENTION_INTERVAL %q: want a duration of at least 1m, such as 1h", c.ObjectStoreRetentionEvery))
		}
	}
	errs = append(errs, c.validateLimits()...)
	if c.SnapshotMaxBytes <= 0 {
		errs = append(errs, fmt.Errorf("OF_SNAPSHOT_MAX_BYTES must be more than 0 (got %d)", c.SnapshotMaxBytes))
	}
	if c.OutboxMaxAttempts < 0 {
		errs = append(errs, fmt.Errorf("OF_OUTBOX_MAX_ATTEMPTS must not be negative (got %d)", c.OutboxMaxAttempts))
	}
	for _, o := range []struct{ env, v string }{
		{"OF_OBJECT_STORE_URL", c.ObjectStoreURL}, {"OF_OBJECT_STORE_TRANSCRIPTS_URL", c.ObjectStoreTranscriptsURL},
		{"OF_OBJECT_STORE_ARTIFACTS_URL", c.ObjectStoreArtifactsURL}, {"OF_OBJECT_STORE_ATTACHMENTS_URL", c.ObjectStoreAttachmentsURL},
		{"OF_OBJECT_STORE_SNAPSHOTS_URL", c.ObjectStoreSnapshotsURL},
	} {
		if o.v == "" {
			continue
		}
		u, err := url.Parse(o.v)
		if err == nil {
			switch u.Scheme {
			case "file", "s3", "gs", "azblob":
				continue
			}
		}
		errs = append(errs, fmt.Errorf("%s %q: want s3://bucket, gs://bucket, azblob://account/container or file:///path", o.env, o.v))
	}
	if c.OIDCIssuer != "" {
		if u, err := url.Parse(c.OIDCIssuer); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("OF_OIDC_ISSUER %q must be an absolute http(s) URL", c.OIDCIssuer))
		}
		if c.OIDCClientID == "" {
			errs = append(errs, errors.New("OF_OIDC_CLIENT_ID is required when OF_OIDC_ISSUER is set"))
		}
		switch strings.ToLower(strings.TrimSpace(c.OIDCDefaultRole)) {
		case "", "admin", "editor", "user":
		default:
			errs = append(errs, fmt.Errorf("OF_OIDC_DEFAULT_ROLE %q: want admin, editor or user", c.OIDCDefaultRole))
		}
	}
	if _, err := ParseLevel(c.LogLevel); err != nil {
		errs = append(errs, err)
	}
	if c.LogFormat != "text" && c.LogFormat != "json" {
		errs = append(errs, fmt.Errorf("OF_LOG_FORMAT %q: want text or json", c.LogFormat))
	}
	if c.SecretKey != "" {
		if _, err := secrets.LoadKey(c.SecretKey); err != nil {
			errs = append(errs, fmt.Errorf("OF_SECRET_KEY: %w", err))
		}
	} else if !c.Dev {
		errs = append(errs, errors.New("OF_SECRET_KEY is required (generate one with `openssl rand -base64 32`), or set OF_DEV=true to create one automatically"))
	}
	errs = append(errs, c.validateKeys()...)
	if c.K8sPodTemplate != "" {
		if _, err := os.Stat(c.K8sPodTemplate); err != nil {
			errs = append(errs, fmt.Errorf("OF_K8S_POD_TEMPLATE: %w", err))
		}
	}
	errs = append(errs, c.validateK8s()...)
	return errors.Join(errs...)
}

// DefaultBackend is the sandbox backend used when OF_SANDBOX_BACKEND is
// unset: kubernetes in production, local in dev mode.
func DefaultBackend(dev bool) string {
	if dev {
		return BackendLocal
	}
	return BackendKubernetes
}

// LocalBackendWarning returns a warning to log when the local worktree
// backend is used outside dev mode, or "" when there is nothing to report.
func (c *Config) LocalBackendWarning() string {
	if c.SandboxBackend == BackendLocal && !c.Dev {
		return "the local sandbox backend runs agents directly on this host without isolation; it is for development and tests only. Use OF_SANDBOX_BACKEND=kubernetes in production"
	}
	return ""
}

// PluginPaths returns the extra plugin executables listed in OF_PLUGINS.
func (c *Config) PluginPaths() []string {
	var out []string
	for _, p := range strings.Split(c.Plugins, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// PluginEnvAllowList returns the variable names listed in OF_PLUGIN_ENV_ALLOW.
func (c *Config) PluginEnvAllowList() []string {
	var out []string
	for _, n := range strings.Split(c.PluginEnvAllow, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// pluginEnvString turns the YAML map form of plugin_env into the string form.
// A value can be a list of names or one comma-separated string.
func pluginEnvString(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		var names []string
		switch v := m[k].(type) {
		case []any:
			for _, n := range v {
				names = append(names, fmt.Sprint(n))
			}
		case nil:
		default:
			names = append(names, fmt.Sprint(v))
		}
		parts = append(parts, k+"="+strings.Join(names, ","))
	}
	return strings.Join(parts, ";")
}

// ParsePluginEnv parses the value of OF_PLUGIN_ENV: entries "name=VAR1,VAR2"
// separated by semicolons. It returns the variable names for each plugin name.
func ParsePluginEnv(v string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, entry := range strings.Split(v, ";") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		name, list, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !ok || !validBackendPluginName(name) {
			return nil, fmt.Errorf("OF_PLUGIN_ENV entry %q: want name=VAR1,VAR2", entry)
		}
		for _, n := range strings.Split(list, ",") {
			if n = strings.TrimSpace(n); n != "" {
				out[name] = append(out[name], n)
			}
		}
	}
	return out, nil
}

// PluginEnvMap returns the variable names that single plugins get, from
// OF_PLUGIN_ENV. It returns an empty map when the value is not valid; Validate
// reports that error.
func (c *Config) PluginEnvMap() map[string][]string {
	m, err := ParsePluginEnv(c.PluginEnv)
	if err != nil {
		return map[string][]string{}
	}
	return m
}

// IsPluginBackend reports whether OF_SANDBOX_BACKEND names a plugin: any
// value other than local and kubernetes. The server checks at start that the
// plugin is loaded and serves a sandbox backend.
func (c *Config) IsPluginBackend() bool {
	return c.SandboxBackend != BackendLocal && c.SandboxBackend != BackendKubernetes && c.SandboxBackend != ""
}

// MasterKey returns the 32-byte master key: OF_SECRET_KEY, or in dev mode a key
// loaded from (or created at) DataDir/secret.key.
func (c *Config) MasterKey() ([]byte, error) {
	if c.SecretKey != "" {
		return secrets.LoadKey(c.SecretKey)
	}
	if !c.Dev {
		return nil, errors.New("OF_SECRET_KEY is not set")
	}
	return secrets.LoadOrCreateKey(filepath.Join(c.DataDir, "secret.key"))
}

// parseKeyList decodes a comma-separated list of base64 keys. Empty items are skipped.
func parseKeyList(env, list string) ([][]byte, error) {
	var out [][]byte
	for i, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		k, err := secrets.LoadKey(item)
		if err != nil {
			return nil, fmt.Errorf("%s item %d: %w", env, i+1, err)
		}
		out = append(out, k)
	}
	return out, nil
}

func (c *Config) validateKeys() []error {
	var errs []error
	if _, err := parseKeyList("OF_SECRET_KEY_PREVIOUS", c.SecretKeyPrevious); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(c.SigningKey) != "" {
		if _, err := secrets.LoadKey(c.SigningKey); err != nil {
			errs = append(errs, fmt.Errorf("OF_SIGNING_KEY: %w", err))
		}
	} else if strings.TrimSpace(c.SigningKeyPrevious) != "" {
		errs = append(errs, errors.New("OF_SIGNING_KEY_PREVIOUS needs OF_SIGNING_KEY"))
	}
	if _, err := parseKeyList("OF_SIGNING_KEY_PREVIOUS", c.SigningKeyPrevious); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// EncryptionKeys returns the key that wraps new data keys and the keys that
// only read. The current key is OF_SECRET_KEY (see MasterKey). The previous
// keys are OF_SECRET_KEY_PREVIOUS.
func (c *Config) EncryptionKeys() (current []byte, previous [][]byte, err error) {
	if current, err = c.MasterKey(); err != nil {
		return nil, nil, err
	}
	if previous, err = parseKeyList("OF_SECRET_KEY_PREVIOUS", c.SecretKeyPrevious); err != nil {
		return nil, nil, err
	}
	return current, previous, nil
}

// SigningKeys returns the key from which the signing keys derive and the keys
// that only verify. With OF_SIGNING_KEY, the current key is OF_SIGNING_KEY and
// the previous keys are OF_SIGNING_KEY_PREVIOUS. Without it, the signing keys
// derive from OF_SECRET_KEY and the previous keys are OF_SECRET_KEY_PREVIOUS,
// so one rotation moves both.
func (c *Config) SigningKeys() (current []byte, previous [][]byte, err error) {
	if strings.TrimSpace(c.SigningKey) == "" {
		if strings.TrimSpace(c.SigningKeyPrevious) != "" {
			return nil, nil, errors.New("OF_SIGNING_KEY_PREVIOUS needs OF_SIGNING_KEY")
		}
		return c.EncryptionKeys()
	}
	if current, err = secrets.LoadKey(c.SigningKey); err != nil {
		return nil, nil, fmt.Errorf("OF_SIGNING_KEY: %w", err)
	}
	if previous, err = parseKeyList("OF_SIGNING_KEY_PREVIOUS", c.SigningKeyPrevious); err != nil {
		return nil, nil, err
	}
	return current, previous, nil
}

// Classes of the derived keys. Signing keys come from SigningKeys. The secret
// store wraps data keys with the key of EncryptionKeys, which is also the key
// of the OAuth tokens of MCP servers and of the personal tokens of users.
// A derived key has no encryption label: those keys are not derived.
//
//	signing:    LabelWebSession, LabelAccountLink,
//	            LabelOIDCSigning, LabelObjectSigning, LabelRunnerToken
//	encryption: the wrapping key of the secret store (no label)

// Key labels for DeriveKey.
const (
	LabelWebSession  = "openfactory/web-session-key"
	LabelAccountLink = "openfactory/account-link-key"
	LabelOIDCSigning = "openfactory/oidc-signing-key"
	// LabelObjectSigning signs local (file://) presigned run-data URLs when
	// OF_OBJECT_STORE_SIGNING_KEY is not set.
	LabelObjectSigning = "openfactory/object-store-signing-key"
	// LabelRunnerToken signs the session tokens of runners.
	LabelRunnerToken = "openfactory/runner-token-key"
)

// DeriveKey derives an independent 32-byte sub-key from the master key.
func DeriveKey(master []byte, label string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// DeriveKeys derives the sub-keys of label for a current key and its previous keys.
func DeriveKeys(current []byte, previous [][]byte, label string) (cur []byte, prev [][]byte) {
	for _, p := range previous {
		prev = append(prev, DeriveKey(p, label))
	}
	return DeriveKey(current, label), prev
}

// ParseLevel parses OF_LOG_LEVEL.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("OF_LOG_LEVEL %q: want debug, info, warn or error", s)
}

// Logger builds the process logger.
func (c *Config) Logger() *slog.Logger {
	lvl, _ := ParseLevel(c.LogLevel)
	opts := &slog.HandlerOptions{Level: lvl}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// Default execution limits. An empty setting means the default.
const (
	defaultQueueTimeout        = 30 * time.Minute
	defaultSandboxStartTimeout = 30 * time.Minute
	defaultMaxSessionDuration  = 8 * time.Hour
	defaultSessionStopGrace    = 2 * time.Minute
	defaultIdleTimeoutDefault  = 10 * time.Minute
	defaultIdleTimeoutMax      = 60 * time.Minute
	defaultShutdownDrain       = 5 * time.Second
)

// limitDuration parses a duration setting. An empty value gives def.
func limitDuration(v string, def time.Duration) (time.Duration, error) {
	if strings.TrimSpace(v) == "" {
		return def, nil
	}
	return time.ParseDuration(strings.TrimSpace(v))
}

// TeamSpendLimitUSD returns OF_TEAM_SPEND_LIMIT_DAILY and
// OF_TEAM_SPEND_LIMIT_MONTHLY. 0 means no limit.
func (c *Config) TeamSpendLimitUSD() (daily, monthly float64) {
	daily, _ = spendLimitValue(c.TeamSpendLimitDaily)
	monthly, _ = spendLimitValue(c.TeamSpendLimitMonthly)
	return daily, monthly
}

// spendLimitValue parses a spend limit in US dollars. Empty means 0.
func spendLimitValue(v string) (float64, error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "$")
	if v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	switch {
	case err != nil:
		return 0, errors.New("want an amount in US dollars, such as 50 or 12.5")
	case f < 0 || f != f:
		return 0, errors.New("must not be negative")
	}
	return f, nil
}

// QueueTimeoutDuration returns OF_QUEUE_TIMEOUT.
func (c *Config) QueueTimeoutDuration() time.Duration {
	d, _ := limitDuration(c.QueueTimeout, defaultQueueTimeout)
	return d
}

// SandboxStartTimeoutDuration returns OF_SANDBOX_START_TIMEOUT.
func (c *Config) SandboxStartTimeoutDuration() time.Duration {
	d, _ := limitDuration(c.SandboxStartTimeout, defaultSandboxStartTimeout)
	return d
}

// MaxSessionDurationValue returns OF_MAX_SESSION_DURATION.
func (c *Config) MaxSessionDurationValue() time.Duration {
	d, _ := limitDuration(c.MaxSessionDuration, defaultMaxSessionDuration)
	return d
}

// SessionStopGraceDuration returns OF_SESSION_STOP_GRACE.
func (c *Config) SessionStopGraceDuration() time.Duration {
	d, _ := limitDuration(c.SessionStopGrace, defaultSessionStopGrace)
	return d
}

// ShutdownDrainDuration returns OF_SHUTDOWN_DRAIN.
func (c *Config) ShutdownDrainDuration() time.Duration {
	d, _ := limitDuration(c.ShutdownDrain, defaultShutdownDrain)
	return d
}

// IdleTimeoutDefaultDuration returns OF_IDLE_TIMEOUT_DEFAULT.
func (c *Config) IdleTimeoutDefaultDuration() time.Duration {
	d, _ := limitDuration(c.IdleTimeoutDefault, defaultIdleTimeoutDefault)
	return d
}

// DefaultMaxOrchestrationDepth is the default of OF_MAX_ORCHESTRATION_DEPTH.
const DefaultMaxOrchestrationDepth = 1

// MaxOrchestrationDepthValue returns OF_MAX_ORCHESTRATION_DEPTH. 0 means
// DefaultMaxOrchestrationDepth.
func (c *Config) MaxOrchestrationDepthValue() int {
	if c.MaxOrchestrationDepth <= 0 {
		return DefaultMaxOrchestrationDepth
	}
	return c.MaxOrchestrationDepth
}

// IdleTimeoutMaxDuration returns OF_IDLE_TIMEOUT_MAX.
func (c *Config) IdleTimeoutMaxDuration() time.Duration {
	d, _ := limitDuration(c.IdleTimeoutMax, defaultIdleTimeoutMax)
	return d
}

// validateLimits checks the execution limit settings.
func (c *Config) validateLimits() []error {
	var errs []error
	check := func(env, v string, def time.Duration, min time.Duration, minExclusive bool) {
		d, err := limitDuration(v, def)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s %q: want a duration such as 30m", env, v))
		case minExclusive && d <= min:
			errs = append(errs, fmt.Errorf("%s %q: must be more than 0", env, v))
		case !minExclusive && d < min:
			errs = append(errs, fmt.Errorf("%s %q: must not be negative", env, v))
		}
	}
	check("OF_QUEUE_TIMEOUT", c.QueueTimeout, defaultQueueTimeout, 0, true)
	check("OF_SANDBOX_START_TIMEOUT", c.SandboxStartTimeout, defaultSandboxStartTimeout, 0, true)
	check("OF_MAX_SESSION_DURATION", c.MaxSessionDuration, defaultMaxSessionDuration, 0, true)
	check("OF_SESSION_STOP_GRACE", c.SessionStopGrace, defaultSessionStopGrace, 0, false)
	check("OF_IDLE_TIMEOUT_DEFAULT", c.IdleTimeoutDefault, defaultIdleTimeoutDefault, 0, false)
	check("OF_IDLE_TIMEOUT_MAX", c.IdleTimeoutMax, defaultIdleTimeoutMax, 0, false)
	check("OF_SHUTDOWN_DRAIN", c.ShutdownDrain, defaultShutdownDrain, 0, false)
	if c.MaxSandboxes < 0 {
		errs = append(errs, fmt.Errorf("OF_MAX_SANDBOXES must not be negative (got %d)", c.MaxSandboxes))
	}
	if c.MaxOrchestrationDepth < 0 {
		errs = append(errs, fmt.Errorf("OF_MAX_ORCHESTRATION_DEPTH must not be negative (got %d)", c.MaxOrchestrationDepth))
	}
	for env, v := range map[string]string{"OF_TEAM_SPEND_LIMIT_DAILY": c.TeamSpendLimitDaily, "OF_TEAM_SPEND_LIMIT_MONTHLY": c.TeamSpendLimitMonthly} {
		if _, err := spendLimitValue(v); err != nil {
			errs = append(errs, fmt.Errorf("%s %q: %w", env, v, err))
		}
	}
	if len(errs) > 0 {
		return errs
	}
	def, max := c.IdleTimeoutDefaultDuration(), c.IdleTimeoutMaxDuration()
	if max > 60*time.Minute {
		errs = append(errs, fmt.Errorf("OF_IDLE_TIMEOUT_MAX %q: must be at most 60m", c.IdleTimeoutMax))
	}
	if def > max {
		errs = append(errs, fmt.Errorf("OF_IDLE_TIMEOUT_DEFAULT %q: must not be more than OF_IDLE_TIMEOUT_MAX %q", c.IdleTimeoutDefault, c.IdleTimeoutMax))
	}
	return errs
}

// validBackendPluginName reports whether s is a valid plugin name: lower case
// letters, digits, "_" and "-", starting with a letter or digit.
func validBackendPluginName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}
