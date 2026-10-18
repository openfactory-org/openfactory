// Package helm holds tests for the Helm chart in ./openfactory. Each case
// renders the chart with the real `helm template` command and checks the
// manifests that it prints. The test skips when Helm is not installed. CI
// installs Helm, so the test never skips there.
package helm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const chartDir = "openfactory"

// helmPath returns the path of the helm command, or skips the test.
func helmPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed: install Helm (https://helm.sh/docs/intro/install/) to run the chart test")
	}
	return p
}

// chart runs helm against the chart directory.
type chart struct {
	helm     string
	dir      string // values files
	metadata map[string]any
}

func loadChart(t *testing.T) *chart {
	t.Helper()
	c := &chart{helm: helmPath(t), dir: t.TempDir()}
	readYAML(t, filepath.Join(chartDir, "Chart.yaml"), &c.metadata)
	return c
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// merge deep-merges src into dst (src wins).
func merge(dst, src map[string]any) map[string]any {
	out := maps.Clone(dst)
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := out[k].(map[string]any); ok {
				out[k] = merge(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// set builds a nested override from dotted keys: set("a.b", 1) = {a: {b: 1}}.
func set(kv ...any) map[string]any {
	out := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		parts := strings.Split(kv[i].(string), ".")
		m := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := m[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[p] = next
			}
			m = next
		}
		m[parts[len(parts)-1]] = kv[i+1]
	}
	return out
}

// rendered is the output of one chart render.
type rendered struct {
	docs []map[string]any
	// notes renders NOTES.txt for the same values (helm template does not
	// print it).
	notes func() (string, error)
}

// Notes returns the rendered NOTES.txt.
func (r rendered) Notes(t *testing.T) string {
	t.Helper()
	n, err := r.notes()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (r rendered) find(kind, nameSuffix string) map[string]any {
	for _, d := range r.docs {
		if d["kind"] == kind && strings.HasSuffix(str(dig(d, "metadata", "name")), nameSuffix) {
			return d
		}
	}
	return nil
}

func (r rendered) count(kind string) int {
	n := 0
	for _, d := range r.docs {
		if d["kind"] == kind {
			n++
		}
	}
	return n
}

func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				return nil
			}
			v = s[k]
		}
	}
	return v
}

func (c *chart) render(overrides map[string]any) (rendered, error) {
	return c.renderAs(overrides, "factory", "of")
}

// writeValues writes the overrides to a values file. JSON is valid YAML.
func writeValues(dir string, overrides map[string]any) (string, error) {
	if overrides == nil {
		overrides = map[string]any{}
	}
	b, err := json.Marshal(overrides)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "values-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// run runs helm and returns its standard output. The error holds the
// standard error output, where helm prints the message of `fail`.
func (c *chart) run(args ...string) ([]byte, error) {
	cmd := exec.Command(c.helm, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// notes renders NOTES.txt with `helm install --dry-run=client`, which needs
// no cluster.
func (c *chart) notes(release, namespace, values string) (string, error) {
	b, err := c.run("install", release, chartDir, "--dry-run=client", "--namespace", namespace, "-f", values, "-o", "json")
	if err != nil {
		return "", err
	}
	var rel struct {
		Info struct {
			Notes string `json:"notes"`
		} `json:"info"`
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return "", fmt.Errorf("helm install --dry-run output: %w", err)
	}
	return rel.Info.Notes, nil
}

// renderAs renders the chart with `helm template` for a release name and
// namespace. The output includes the hook manifests.
func (c *chart) renderAs(overrides map[string]any, release, namespace string) (rendered, error) {
	var out rendered
	values, err := writeValues(c.dir, overrides)
	if err != nil {
		return out, err
	}
	text, err := c.run("template", release, chartDir, "--namespace", namespace, "-f", values)
	if err != nil {
		return out, err
	}
	var notes string
	var notesErr error
	done := false
	out.notes = func() (string, error) {
		if done {
			return notes, notesErr
		}
		done = true
		notes, notesErr = c.notes(release, namespace, values)
		return notes, notesErr
	}
	dec := yaml.NewDecoder(bytes.NewReader(text))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("invalid YAML: %w\n%s", err, text)
		}
		if doc == nil {
			continue
		}
		if doc["apiVersion"] == nil || doc["kind"] == nil || dig(doc, "metadata", "name") == nil {
			return out, fmt.Errorf("manifest without apiVersion, kind or metadata.name: %v", doc)
		}
		out.docs = append(out.docs, doc)
	}
	return out, nil
}

func envMap(container any) map[string]any {
	out := map[string]any{}
	env, _ := dig(container, "env").([]any)
	for _, e := range env {
		m := e.(map[string]any)
		if v, ok := m["value"]; ok {
			out[str(m["name"])] = v
		} else {
			out[str(m["name"])] = m["valueFrom"]
		}
	}
	return out
}

const key = "dGVzdC1rZXktdGVzdC1rZXktdGVzdC1rZXktdGVzdC0="

func TestChartRenders(t *testing.T) {
	c := loadChart(t)
	if c.metadata["name"] != "openfactory" {
		t.Fatalf("chart name %v", c.metadata["name"])
	}
	cases := []struct {
		name      string
		overrides map[string]any
		wantErr   string
		check     func(t *testing.T, r rendered)
	}{
		{
			name:      "sqlite default",
			overrides: set("secretKey", key),
			check: func(t *testing.T, r rendered) {
				sts := r.find("StatefulSet", "openfactory")
				if sts == nil || r.find("Deployment", "openfactory") != nil {
					t.Fatal("sqlite mode must render a StatefulSet and no server Deployment")
				}
				if toInt(dig(sts, "spec", "replicas")) != 1 {
					t.Errorf("replicas %v", dig(sts, "spec", "replicas"))
				}
				if dig(sts, "spec", "volumeClaimTemplates", 0, "metadata", "name") != "data" {
					t.Error("missing data volume claim template")
				}
				if r.find("Service", "-headless") == nil {
					t.Error("missing headless service")
				}
				if r.count("PodDisruptionBudget") != 0 {
					t.Error("PDB rendered in sqlite mode")
				}
				ctr := dig(sts, "spec", "template", "spec", "containers", 0)
				env := envMap(ctr)
				if dig(env["OF_REPLICA_ID"], "fieldRef", "fieldPath") != "metadata.name" {
					t.Errorf("OF_REPLICA_ID: %v", env["OF_REPLICA_ID"])
				}
				cm := r.find("ConfigMap", "factory-openfactory")
				for k, want := range map[string]string{
					"OF_SANDBOX_BACKEND": "kubernetes", "OF_PLUGIN_DIR": "/opt/openfactory/plugins",
					"OF_SANDBOX_IMAGE": "ghcr.io/openfactory-org/sandbox:latest", "OF_K8S_POD_TEMPLATE": "/etc/openfactory/pod-template.yaml",
				} {
					if got := dig(cm, "data", k); got != want {
						t.Errorf("configmap %s = %v, want %q", k, got, want)
					}
				}
				if dig(cm, "data", "OF_OIDC_ISSUER") != nil || dig(cm, "data", "OF_OBJECT_STORE_URL") != nil {
					t.Error("unset features leaked into the configmap")
				}
				if dig(r.find("Secret", "factory-openfactory"), "stringData", "OF_DATABASE_URL") != nil {
					t.Error("sqlite mode must not set OF_DATABASE_URL")
				}
				role := r.find("Role", "-sandbox")
				if role == nil || r.find("RoleBinding", "-sandbox-server") == nil {
					t.Fatal("missing sandbox RBAC")
				}
				// The control plane never runs a command in a pod.
				if strings.Contains(fmt.Sprint(dig(role, "rules")), "pods/exec") {
					t.Error("the sandbox role has a pods/exec rule")
				}
				if !strings.Contains(fmt.Sprint(dig(role, "rules")), "jobs") {
					t.Error("the sandbox role cannot manage Jobs")
				}
				if notes := r.Notes(t); !strings.Contains(notes, "sqlite (StatefulSet, 1 replica)") || strings.Contains(notes, "master key") {
					t.Errorf("notes: %s", notes)
				}
				if r.find("Job", "-key-init") != nil {
					t.Error("secretKey is set, but the chart renders the key Job")
				}
				if dig(r.find("Secret", "factory-openfactory"), "stringData", "OF_SECRET_KEY") != key {
					t.Error("secretKey is not in the chart Secret")
				}
			},
		},
		{
			name:      "no values",
			overrides: nil,
			check: func(t *testing.T, r rendered) {
				sts := r.find("StatefulSet", "openfactory")
				if sts == nil || toInt(dig(sts, "spec", "replicas")) != 1 {
					t.Fatal("the default install must be one StatefulSet replica in sqlite mode")
				}
				if r.count("Ingress") != 0 {
					t.Error("the default install renders an Ingress")
				}
				// The hook Job creates the master key. No key is in a manifest.
				job := r.find("Job", "factory-openfactory-key-init")
				if job == nil {
					t.Fatal("no key Job")
				}
				if got := str(dig(job, "metadata", "annotations", "helm.sh/hook")); got != "pre-install,pre-upgrade" {
					t.Errorf("key Job hook %q", got)
				}
				ctr := dig(job, "spec", "template", "spec", "containers", 0)
				if dig(ctr, "image") != "ghcr.io/openfactory-org/openfactory:"+str(c.metadata["appVersion"]) {
					t.Errorf("key Job image %v", dig(ctr, "image"))
				}
				if !strings.Contains(fmt.Sprint(dig(ctr, "command")), "409") {
					t.Error("the key Job does not keep an existing Secret")
				}
				role := r.find("Role", "-key-init")
				if got := fmt.Sprint(dig(role, "rules")); got != "[map[apiGroups:[] resources:[secrets] verbs:[create]]]" {
					t.Errorf("key Job role rules %s", got)
				}
				if dig(role, "metadata", "annotations", "helm.sh/hook") == nil || r.find("RoleBinding", "-key-init") == nil ||
					r.find("ServiceAccount", "-key-init") == nil {
					t.Error("the key Job needs its own hook ServiceAccount, Role and RoleBinding")
				}
				if dig(r.find("Secret", "factory-openfactory"), "stringData", "OF_SECRET_KEY") != nil {
					t.Error("the chart Secret has a master key")
				}
				env := envMap(dig(sts, "spec", "template", "spec", "containers", 0))
				if dig(env["OF_SECRET_KEY"], "secretKeyRef", "name") != "factory-openfactory-key" {
					t.Errorf("OF_SECRET_KEY: %v", env["OF_SECRET_KEY"])
				}
				cm := r.find("ConfigMap", "factory-openfactory")
				if got := dig(cm, "data", "OF_RUNNER_IMAGE"); got != "ghcr.io/openfactory-org/openfactory:"+str(c.metadata["appVersion"]) {
					t.Errorf("OF_RUNNER_IMAGE = %v, want the server image", got)
				}
				notes := r.Notes(t)
				for _, want := range []string{"port-forward svc/factory-openfactory", "/api/v1/bootstrap", "Secret factory-openfactory-key"} {
					if !strings.Contains(notes, want) {
						t.Errorf("notes do not contain %q:\n%s", want, notes)
					}
				}
			},
		},
		{
			name:      "existing secret renders no key Job",
			overrides: set("existingSecret", "of-env"),
			check: func(t *testing.T, r rendered) {
				if r.find("Job", "-key-init") != nil || r.find("Secret", "factory-openfactory") != nil {
					t.Error("existingSecret is set, but the chart renders a key Job or a Secret")
				}
				env := envMap(dig(r.find("StatefulSet", "openfactory"), "spec", "template", "spec", "containers", 0))
				if env["OF_SECRET_KEY"] != nil {
					t.Error("OF_SECRET_KEY must come from existingSecret")
				}
			},
		},
		{
			name:      "sqlite rejects replicas",
			overrides: set("secretKey", key, "server.replicaCount", 2),
			wantErr:   "exactly one replica",
		},
		{
			name:      "sqlite existing claim",
			overrides: set("secretKey", key, "database.sqlite.persistence.existingClaim", "old-data"),
			check: func(t *testing.T, r rendered) {
				sts := r.find("StatefulSet", "openfactory")
				if dig(sts, "spec", "volumeClaimTemplates") != nil {
					t.Error("existingClaim must not render a claim template")
				}
				if dig(sts, "spec", "template", "spec", "volumes", 0, "persistentVolumeClaim", "claimName") != "old-data" {
					t.Errorf("volumes: %v", dig(sts, "spec", "template", "spec", "volumes"))
				}
			},
		},
		{
			name: "postgres with replicas",
			overrides: set("secretKey", key, "database.mode", "postgres", "server.replicaCount", 3,
				"database.postgres.url", "postgres://of:p%40ss@db.example.com:5432/of?sslmode=require", "objectStore.url", "s3://runs/of?region=eu-west-1",
				"objectStore.credentials", map[string]any{"AWS_ACCESS_KEY_ID": "a", "AWS_SECRET_ACCESS_KEY": "b"}),
			check: func(t *testing.T, r rendered) {
				dep := r.find("Deployment", "factory-openfactory")
				if dep == nil || r.count("StatefulSet") != 0 {
					t.Fatal("postgres mode must render a Deployment and no StatefulSet")
				}
				if toInt(dig(dep, "spec", "replicas")) != 3 {
					t.Errorf("replicas %v", dig(dep, "spec", "replicas"))
				}
				ctr := dig(dep, "spec", "template", "spec", "containers", 0)
				if dig(ctr, "readinessProbe", "httpGet", "path") != "/readyz" {
					t.Error("missing readiness probe")
				}
				if dig(dep, "spec", "template", "spec", "topologySpreadConstraints") == nil {
					t.Error("missing topology spread")
				}
				if r.find("PodDisruptionBudget", "openfactory") == nil {
					t.Error("missing PDB")
				}
				if r.find("Service", "-headless") != nil {
					t.Error("headless service rendered in postgres mode")
				}
				sec := r.find("Secret", "factory-openfactory")
				if got := str(dig(sec, "stringData", "OF_DATABASE_URL")); got != "postgres://of:p%40ss@db.example.com:5432/of?sslmode=require" {
					t.Errorf("database url %q", got)
				}
				if dig(sec, "stringData", "AWS_ACCESS_KEY_ID") != "a" {
					t.Error("object store credentials missing from the secret")
				}
				if dig(r.find("ConfigMap", "factory-openfactory"), "data", "OF_OBJECT_STORE_URL") != "s3://runs/of?region=eu-west-1" {
					t.Error("OF_OBJECT_STORE_URL missing")
				}
			},
		},
		{
			name:      "postgres external secret single replica",
			overrides: set("secretKey", key, "database.mode", "postgres", "database.postgres.existingSecret", "db", "server.replicaCount", 1),
			check: func(t *testing.T, r rendered) {
				dep := r.find("Deployment", "factory-openfactory")
				env := envMap(dig(dep, "spec", "template", "spec", "containers", 0))
				if dig(env["OF_DATABASE_URL"], "secretKeyRef", "name") != "db" {
					t.Errorf("OF_DATABASE_URL: %v", env["OF_DATABASE_URL"])
				}
				if r.count("PodDisruptionBudget") != 0 {
					t.Error("PDB with one replica blocks node drains")
				}
			},
		},
		{
			name:      "postgres needs a database",
			overrides: set("secretKey", key, "database.mode", "postgres"),
			wantErr:   "needs database.postgres.url or database.postgres.existingSecret",
		},
		{
			name:      "postgres replicas need object storage",
			overrides: set("secretKey", key, "database.mode", "postgres", "database.postgres.url", "postgres://x", "server.replicaCount", 2),
			wantErr:   "objectStore.url is required",
		},
		{
			name:      "unknown mode",
			overrides: set("secretKey", key, "database.mode", "mysql"),
			wantErr:   "want sqlite or postgres",
		},
		{
			name:      "docker backend removed",
			overrides: set("secretKey", key, "server.sandboxBackend", "docker"),
			wantErr:   "the only value is kubernetes",
		},
		{
			name: "oidc",
			overrides: set("secretKey", key, "server.publicURL", "https://factory.example.com", "oidc.enabled", true,
				"oidc.issuer", "https://idp.example.com", "oidc.clientID", "of", "oidc.clientSecret", "s3cr3t", "oidc.adminGroups", "platform"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				for k, want := range map[string]string{
					"OF_OIDC_ISSUER": "https://idp.example.com", "OF_OIDC_CLIENT_ID": "of",
					"OF_OIDC_REDIRECT_URL": "https://factory.example.com/auth/oidc/callback", "OF_OIDC_ADMIN_GROUPS": "platform",
					"OF_OIDC_DEFAULT_ROLE": "user", "OF_OIDC_REQUIRE_VERIFIED_EMAIL": "true",
				} {
					if got := dig(cm, "data", k); got != want {
						t.Errorf("%s = %v, want %q", k, got, want)
					}
				}
				if dig(r.find("Secret", "factory-openfactory"), "stringData", "OF_OIDC_CLIENT_SECRET") != "s3cr3t" {
					t.Error("client secret not in the secret")
				}
				if !strings.Contains(r.Notes(t), "/auth/oidc/callback") {
					t.Error("notes do not show the redirect URI")
				}
			},
		},
		{
			name:      "oidc requires issuer",
			overrides: set("secretKey", key, "oidc.enabled", true),
			wantErr:   "oidc.enabled requires",
		},
		{
			name: "plugins",
			overrides: set("secretKey", key, "plugins.images", []any{
				map[string]any{"name": "gitlab", "image": "ghcr.io/openfactory-org/openfactory-plugin-gitlab:0.2.0"},
			}),
			check: func(t *testing.T, r rendered) {
				spec := dig(r.find("StatefulSet", "openfactory"), "spec", "template", "spec")
				bundled := dig(spec, "initContainers", 0)
				if dig(bundled, "name") != "bundled-plugins" || !strings.Contains(fmt.Sprint(dig(bundled, "command")), "cp -a /usr/local/lib/openfactory/plugins/. /opt/openfactory/plugins/") {
					t.Errorf("bundled plugins init container: %v", bundled)
				}
				ic := dig(spec, "initContainers", 1)
				if dig(ic, "name") != "plugin-gitlab" || !strings.Contains(fmt.Sprint(dig(ic, "command")), "cp -a /plugins/. /opt/openfactory/plugins/") {
					t.Errorf("init container: %v", ic)
				}
			},
		},
		{
			name:      "no bundled plugins",
			overrides: set("secretKey", key, "plugins.bundled", false),
			check: func(t *testing.T, r rendered) {
				spec := dig(r.find("StatefulSet", "openfactory"), "spec", "template", "spec")
				if ic := dig(spec, "initContainers"); ic != nil {
					t.Errorf("unexpected init containers: %v", ic)
				}
			},
		},
		{
			name: "worker",
			overrides: set("secretKey", key, "worker.enabled", true, "worker.apiKey", "of_x_y", "worker.metricsAddr", ":9090",
				"worker.labels.region", "eu", "worker.labels.pool", "gpu"),
			check: func(t *testing.T, r rendered) {
				w := r.find("Deployment", "-worker")
				if w == nil {
					t.Fatal("missing worker")
				}
				// The chart sets the worker with environment variables only.
				args := fmt.Sprint(dig(w, "spec", "template", "spec", "containers", 0, "args"))
				if args != "[worker]" {
					t.Errorf("worker args: %s", args)
				}
				env := envMap(dig(w, "spec", "template", "spec", "containers", 0))
				for k, want := range map[string]string{
					"OF_SANDBOX_BACKEND": "kubernetes", "OF_WORKER_ID": "k8s-worker", "OF_WORKER_NAME": "k8s-worker",
					"OF_WORKER_CAPACITY": "8", "OF_WORKER_LABELS": "pool=gpu,region=eu", "OF_METRICS_ADDR": ":9090",
				} {
					if got := env[k]; got != want {
						t.Errorf("worker %s = %v, want %q", k, got, want)
					}
				}
				if _, ok := env["OF_API_KEY"]; !ok {
					t.Error("worker has no OF_API_KEY")
				}
				if r.find("RoleBinding", "-sandbox-worker") == nil {
					t.Error("missing worker RBAC")
				}
				if got := envMap(dig(w, "spec", "template", "spec", "containers", 0))["OF_RUNNER_IMAGE"]; got != "ghcr.io/openfactory-org/openfactory:"+str(c.metadata["appVersion"]) {
					t.Errorf("worker OF_RUNNER_IMAGE = %v, want the server image", got)
				}
			},
		},
		{
			name:      "worker only",
			overrides: set("server.enabled", false, "worker.enabled", true, "worker.existingSecret", "wk", "worker.serverURL", "https://factory.example.com"),
			check: func(t *testing.T, r rendered) {
				if r.count("StatefulSet") != 0 || r.find("Deployment", "factory-openfactory") != nil || r.count("Service") != 0 || r.count("Secret") != 0 {
					t.Error("worker-only install rendered server resources")
				}
				w := r.find("Deployment", "-worker")
				if envMap(dig(w, "spec", "template", "spec", "containers", 0))["OF_SERVER"] != "https://factory.example.com" {
					t.Error("worker does not dial worker.serverURL")
				}
				if r.find("RoleBinding", "-sandbox-worker") == nil || r.find("RoleBinding", "-sandbox-server") != nil {
					t.Error("worker-only RBAC is wrong")
				}
				if r.find("ConfigMap", "-pod-template") == nil {
					t.Error("worker needs the pod template")
				}
				if notes := r.Notes(t); !strings.Contains(notes, "Worker-only install") {
					t.Errorf("notes: %s", notes)
				}
			},
		},
		{
			name:      "worker only needs server URL",
			overrides: set("server.enabled", false, "worker.enabled", true, "worker.apiKey", "k"),
			wantErr:   "worker.serverURL is required",
		},
		{
			name:      "nothing to install",
			overrides: set("server.enabled", false),
			wantErr:   "nothing to install",
		},
		{
			name: "sandbox k8s settings",
			overrides: set("secretKey", key,
				"sandbox.k8s.labels", map[string]any{"team": "ops", "env": "prod"},
				"sandbox.k8s.annotations", map[string]any{"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"},
				"sandbox.k8s.imagePullPolicy", "Always",
				"sandbox.k8s.runAsUser", "image",
				"sandbox.k8s.workspaceSizeLimit", "20Gi",
				"sandbox.k8s.preflightJob", true,
				"sandbox.k8s.controlPlane.namespace", "of",
				"sandbox.k8s.controlPlane.selector", "app.kubernetes.io/name=openfactory",
				"sandbox.k8s.egressProxy.sidecarImage", "example.test/runner:1",
				"sandbox.k8s.egressProxy.lock", "iptables",
				"sandbox.k8s.networkPolicy.check", "probe",
				"sandbox.k8s.networkPolicy.enforcement", "require",
				"sandbox.k8s.networkPolicy.waitTimeout", "60s",
				"sandbox.k8s.networkPolicy.waitAddr", "10.0.0.1:10250",
				"sandbox.k8s.allowUnenforcedNetworkPolicy", true,
				"worker.enabled", true, "worker.apiKey", "of_x_y"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				// A remote worker runs the same backend, so it must get the
				// same sandbox.k8s settings as the server.
				workerEnv := envMap(dig(r.find("Deployment", "-worker"), "spec", "template", "spec", "containers", 0))
				for k, v := range dig(cm, "data").(map[string]any) {
					if strings.HasPrefix(k, "OF_K8S_") || k == "OF_ALLOW_UNENFORCED_NETWORK_POLICY" {
						if got := workerEnv[k]; got != v {
							t.Errorf("worker %s = %v, want %v as on the server", k, got, v)
						}
					}
				}
				for k, want := range map[string]string{
					"OF_K8S_LABELS": "env=prod,team=ops", "OF_K8S_ANNOTATIONS": "cluster-autoscaler.kubernetes.io/safe-to-evict=false",
					"OF_K8S_IMAGE_PULL_POLICY": "Always",
					"OF_K8S_RUN_AS_USER":       "image", "OF_K8S_WORKSPACE_SIZE_LIMIT": "20Gi",
					"OF_K8S_PREFLIGHT_JOB": "true", "OF_K8S_CONTROL_PLANE_NAMESPACE": "of",
					"OF_K8S_CONTROL_PLANE_SELECTOR":     "app.kubernetes.io/name=openfactory",
					"OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE": "example.test/runner:1", "OF_ALLOW_UNENFORCED_NETWORK_POLICY": "true",
					"OF_K8S_EGRESS_LOCK":          "iptables",
					"OF_K8S_NETWORK_POLICY_CHECK": "probe", "OF_K8S_NETWORK_POLICY_ENFORCEMENT": "require",
					"OF_K8S_EGRESS_WAIT_TIMEOUT": "60s", "OF_K8S_EGRESS_WAIT_ADDR": "10.0.0.1:10250",
				} {
					if got := dig(cm, "data", k); got != want {
						t.Errorf("configmap %s = %v, want %q", k, got, want)
					}
				}
				if dig(cm, "data", "OF_K8S_CONTROL_PLANE_SELECTOR") == nil || dig(cm, "data", "OF_K8S_EGRESS_WAIT_ADDR") == nil {
					t.Error("a set value is missing from the configmap")
				}
			},
		},
		{
			name:      "sandbox k8s defaults set only the sidecar image and the control plane",
			overrides: set("secretKey", key, "worker.enabled", true, "worker.apiKey", "of_x_y"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				defaultKey := func(k string) bool {
					switch k {
					case "OF_K8S_NAMESPACE", "OF_K8S_POD_TEMPLATE", "OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE",
						"OF_K8S_CONTROL_PLANE_NAMESPACE", "OF_K8S_CONTROL_PLANE_SELECTOR":
						return true
					}
					return false
				}
				for k := range dig(cm, "data").(map[string]any) {
					if strings.HasPrefix(k, "OF_K8S_") && !defaultKey(k) {
						t.Errorf("default render sets %s", k)
					}
				}
				workerEnv := envMap(dig(r.find("Deployment", "-worker"), "spec", "template", "spec", "containers", 0))
				for k := range workerEnv {
					if (strings.HasPrefix(k, "OF_K8S_") && !defaultKey(k)) || k == "OF_ALLOW_UNENFORCED_NETWORK_POLICY" {
						t.Errorf("default render sets %s on the worker", k)
					}
				}
				// The sidecar image defaults to the runner image (the server image).
				runner := dig(cm, "data", "OF_RUNNER_IMAGE")
				if runner == nil || dig(cm, "data", "OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE") != runner || workerEnv["OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE"] != runner {
					t.Errorf("sidecar image = %v, want the runner image %v", dig(cm, "data", "OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE"), runner)
				}
				if dig(cm, "data", "OF_ALLOW_UNENFORCED_NETWORK_POLICY") != nil {
					t.Error("OF_ALLOW_UNENFORCED_NETWORK_POLICY is set by default")
				}
				role := r.find("Role", "-sandbox")
				if !strings.Contains(fmt.Sprint(dig(role, "rules")), "networkpolicies") {
					t.Error("the sandbox role has no networkpolicies rule")
				}
			},
		},
		{
			// A restricted sandbox dials the server Service (port 80), which
			// sends the connection to the server Pods on the container port.
			// NetworkPolicy matches the Pod after that translation, so the
			// default rule must select the server Pods, on all their ports.
			name:      "control plane defaults to the server pods of the release",
			overrides: set("secretKey", key, "worker.enabled", true, "worker.apiKey", "of_x_y"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				want := "app.kubernetes.io/component=server,app.kubernetes.io/instance=factory,app.kubernetes.io/name=openfactory"
				if got := dig(cm, "data", "OF_K8S_CONTROL_PLANE_SELECTOR"); got != want {
					t.Errorf("selector = %v, want %q", got, want)
				}
				if got := dig(cm, "data", "OF_K8S_CONTROL_PLANE_NAMESPACE"); got != "of" {
					t.Errorf("namespace = %v, want the release namespace of", got)
				}
				workerEnv := envMap(dig(r.find("Deployment", "-worker"), "spec", "template", "spec", "containers", 0))
				if workerEnv["OF_K8S_CONTROL_PLANE_SELECTOR"] != want || workerEnv["OF_K8S_CONTROL_PLANE_NAMESPACE"] != "of" {
					t.Errorf("worker control plane = %v in %v", workerEnv["OF_K8S_CONTROL_PLANE_SELECTOR"], workerEnv["OF_K8S_CONTROL_PLANE_NAMESPACE"])
				}
				// Each term must match a label of the server Pods, and the
				// worker Pods must not match.
				sts := r.find("StatefulSet", "openfactory")
				podLabels := dig(sts, "spec", "template", "metadata", "labels").(map[string]any)
				workerLabels := dig(r.find("Deployment", "-worker"), "spec", "template", "metadata", "labels").(map[string]any)
				workerMatches := true
				for _, term := range strings.Split(want, ",") {
					k, v, _ := strings.Cut(term, "=")
					if str(podLabels[k]) != v {
						t.Errorf("server Pod label %s = %v, want %q", k, podLabels[k], v)
					}
					if str(workerLabels[k]) != v {
						workerMatches = false
					}
				}
				if workerMatches {
					t.Errorf("the selector also matches the worker Pods: %v", workerLabels)
				}
				// The internal URL is the Service on port 80; the Service
				// sends it to the named port http of the server container.
				if got := dig(cm, "data", "OF_INTERNAL_URL"); got != "http://factory-openfactory.of.svc:80" {
					t.Errorf("OF_INTERNAL_URL = %v", got)
				}
				var svc map[string]any
				for _, d := range r.docs {
					if d["kind"] == "Service" && str(dig(d, "metadata", "name")) == "factory-openfactory" {
						svc = d
					}
				}
				if dig(svc, "spec", "ports", 0, "targetPort") != "http" {
					t.Errorf("service targetPort = %v, want http", dig(svc, "spec", "ports", 0, "targetPort"))
				}
				ctr := dig(sts, "spec", "template", "spec", "containers", 0)
				if dig(ctr, "ports", 0, "name") != "http" || toInt(dig(ctr, "ports", 0, "containerPort")) != 8080 {
					t.Errorf("server container ports = %v", dig(ctr, "ports"))
				}
			},
		},
		{
			name: "control plane selector override wins",
			overrides: set("secretKey", key, "worker.enabled", true, "worker.apiKey", "of_x_y",
				"sandbox.k8s.controlPlane.selector", "team=platform"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				workerEnv := envMap(dig(r.find("Deployment", "-worker"), "spec", "template", "spec", "containers", 0))
				if got := dig(cm, "data", "OF_K8S_CONTROL_PLANE_SELECTOR"); got != "team=platform" || workerEnv["OF_K8S_CONTROL_PLANE_SELECTOR"] != "team=platform" {
					t.Errorf("selector = %v (worker %v), want team=platform", got, workerEnv["OF_K8S_CONTROL_PLANE_SELECTOR"])
				}
				// An empty namespace keeps its meaning: the sandbox namespace.
				if got := dig(cm, "data", "OF_K8S_CONTROL_PLANE_NAMESPACE"); got != nil {
					t.Errorf("namespace = %v, want unset", got)
				}
			},
		},
		{
			name: "control plane namespace override keeps the default selector",
			overrides: set("secretKey", key,
				"sandbox.k8s.controlPlane.namespace", "other"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				if got := dig(cm, "data", "OF_K8S_CONTROL_PLANE_NAMESPACE"); got != "other" {
					t.Errorf("namespace = %v, want other", got)
				}
				if !strings.Contains(str(dig(cm, "data", "OF_K8S_CONTROL_PLANE_SELECTOR")), "app.kubernetes.io/instance=factory") {
					t.Errorf("selector = %v, want the default", dig(cm, "data", "OF_K8S_CONTROL_PLANE_SELECTOR"))
				}
			},
		},
		{
			// A custom internal URL may not lead to the server Pods, and an IP
			// address in it gets its own rule, which a selector would replace.
			name:      "no default control plane with a custom internal URL",
			overrides: set("secretKey", key, "server.internalURL", "http://10.0.0.5:8080"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				for _, k := range []string{"OF_K8S_CONTROL_PLANE_SELECTOR", "OF_K8S_CONTROL_PLANE_NAMESPACE"} {
					if got := dig(cm, "data", k); got != nil {
						t.Errorf("%s = %v, want unset", k, got)
					}
				}
			},
		},
		{
			name: "no default control plane for a worker-only install",
			overrides: set("server.enabled", false, "worker.enabled", true, "worker.apiKey", "of_x_y",
				"worker.serverURL", "https://factories.example.com"),
			check: func(t *testing.T, r rendered) {
				workerEnv := envMap(dig(r.find("Deployment", "-worker"), "spec", "template", "spec", "containers", 0))
				for _, k := range []string{"OF_K8S_CONTROL_PLANE_SELECTOR", "OF_K8S_CONTROL_PLANE_NAMESPACE"} {
					if got, ok := workerEnv[k]; ok {
						t.Errorf("worker %s = %v, want unset", k, got)
					}
				}
			},
		},
		{
			name: "plugin env allow list and runner image",
			overrides: set("secretKey", key,
				"plugins.envAllow", []any{"ACME_API_URL", "ACME_API_TOKEN"},
				"runner.image", "example.test/runner:1"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				for k, want := range map[string]string{
					"OF_PLUGIN_ENV_ALLOW":               "ACME_API_URL,ACME_API_TOKEN",
					"OF_RUNNER_IMAGE":                   "example.test/runner:1",
					"OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE": "example.test/runner:1",
				} {
					if got := dig(cm, "data", k); got != want {
						t.Errorf("configmap %s = %v, want %q", k, got, want)
					}
				}
			},
		},
		{
			name: "plugin env for single plugins and the cloud identity issuer",
			overrides: set("secretKey", key,
				"plugins.env.acme", []any{"ACME_API_URL", "ACME_API_TOKEN"},
				"plugins.env.other", []any{"OTHER_URL"},
				"server.oidcIssuerURL", "https://issuer.example.com"),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				for k, want := range map[string]string{
					"OF_PLUGIN_ENV":      "acme=ACME_API_URL,ACME_API_TOKEN;other=OTHER_URL",
					"OF_OIDC_ISSUER_URL": "https://issuer.example.com",
				} {
					if got := dig(cm, "data", k); got != want {
						t.Errorf("configmap %s = %v, want %q", k, got, want)
					}
				}
			},
		},
		{
			name:      "no execution setting in the configmap",
			overrides: set("secretKey", key),
			check: func(t *testing.T, r rendered) {
				cm := r.find("ConfigMap", "factory-openfactory")
				for _, k := range []string{"OF_EXECUTION", "OF_CONCURRENCY", "OF_NETGUARD", "OF_RUN_LEASE_TTL"} {
					if dig(cm, "data", k) != nil {
						t.Errorf("%s is in the configmap: the executor path was removed", k)
					}
				}
				if dig(cm, "data", "OF_PLUGIN_ENV_ALLOW") != nil || dig(cm, "data", "OF_PLUGIN_ENV") != nil ||
					dig(cm, "data", "OF_OIDC_ISSUER_URL") != nil {
					t.Error("empty plugin allow list leaked into the configmap")
				}
			},
		},
		{
			name:      "local backend refused",
			overrides: set("secretKey", key, "server.sandboxBackend", "local"),
			wantErr:   "the local backend runs agents in the server Pod without isolation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := c.render(tc.overrides)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, r)
		})
	}
}

// TestNoValuesRenderIsStable renders the chart with no values two times. A
// GitOps tool renders with `helm template` on each sync, so the output must
// not change and must hold no master key.
func TestNoValuesRenderIsStable(t *testing.T) {
	c := loadChart(t)
	first, err := c.run("template", "factory", chartDir, "--namespace", "of")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.run("template", "factory", chartDir, "--namespace", "of")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("two renders with no values differ")
	}
	if bytes.Contains(first, []byte("OF_SECRET_KEY: ")) {
		t.Error("a render with no values holds a master key")
	}
}

// serverPodSpec returns the Pod spec of the server workload.
func serverPodSpec(t *testing.T, r rendered) map[string]any {
	t.Helper()
	for _, kind := range []string{"Deployment", "StatefulSet"} {
		if d := r.find(kind, "openfactory"); d != nil && !strings.Contains(str(dig(d, "metadata", "name")), "worker") {
			spec, _ := dig(d, "spec", "template", "spec").(map[string]any)
			return spec
		}
	}
	t.Fatal("no server workload")
	return nil
}

func namesOf(list any) []string {
	var out []string
	items, _ := list.([]any)
	for _, it := range items {
		out = append(out, str(dig(it, "name")))
	}
	return out
}

func containerNamed(spec map[string]any, name string) map[string]any {
	items, _ := spec["containers"].([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && m["name"] == name {
			return m
		}
	}
	return nil
}

// extraValues are the extra* values of a sidecar that gets credentials from a
// projected token (the shape of the local test stack).
func extraValues() map[string]any {
	return set(
		"server.extraEnvVars", []any{
			map[string]any{"name": "AWS_REGION", "value": "us-east-1"},
			map[string]any{"name": "TOKEN_FROM_SECRET", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "tok", "key": "k"}}},
		},
		"server.extraVolumes", []any{
			map[string]any{"name": "aws-local-ca", "configMap": map[string]any{"name": "aws-local-ca"}},
			map[string]any{"name": "pod-identity-token", "projected": map[string]any{"sources": []any{map[string]any{
				"serviceAccountToken": map[string]any{"audience": "pods.eks.amazonaws.com", "expirationSeconds": 3600, "path": "eks-pod-identity-token"},
			}}}},
		},
		"server.extraVolumeMounts", []any{
			map[string]any{"name": "aws-local-ca", "mountPath": "/etc/aws-local-ca", "readOnly": true},
			map[string]any{"name": "pod-identity-token", "mountPath": "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount", "readOnly": true},
		},
		"server.extraContainers", []any{
			map[string]any{
				"name": "pod-identity-agent", "image": "of-local/testkit:latest", "imagePullPolicy": "Never",
				"args":         []any{"pod-identity-agent", "-listen", "127.0.0.1:2703", "-upstream", "https://aws.k8s.orb.local:9443/__pod-identity", "-ca", "/etc/aws-local-ca/ca.crt"},
				"volumeMounts": []any{map[string]any{"name": "aws-local-ca", "mountPath": "/etc/aws-local-ca", "readOnly": true}},
			},
		},
		"server.service.annotations", map[string]any{"example.com/lb": "internal"},
	)
}

func TestServerExtras(t *testing.T) {
	c := loadChart(t)
	modes := map[string]map[string]any{
		"postgres (Deployment)": set("secretKey", key, "database.mode", "postgres", "database.postgres.url", "postgres://u:p@db:5432/d",
			"server.replicaCount", 2, "objectStore.url", "s3://b/p?region=us-east-1"),
		"sqlite (StatefulSet)": set("secretKey", key),
	}
	for name, base := range modes {
		t.Run(name, func(t *testing.T) {
			r, err := c.render(merge(base, extraValues()))
			if err != nil {
				t.Fatal(err)
			}
			spec := serverPodSpec(t, r)
			if got := namesOf(spec["containers"]); strings.Join(got, ",") != "server,pod-identity-agent" {
				t.Fatalf("containers %v", got)
			}
			agent := containerNamed(spec, "pod-identity-agent")
			if agent["image"] != "of-local/testkit:latest" || agent["imagePullPolicy"] != "Never" {
				t.Errorf("sidecar %v", agent)
			}
			if args := fmt.Sprint(agent["args"]); !strings.Contains(args, "pod-identity-agent -listen 127.0.0.1:2703 -upstream https://aws.k8s.orb.local:9443/__pod-identity -ca /etc/aws-local-ca/ca.crt") {
				t.Errorf("sidecar args %s", args)
			}
			if got := namesOf(dig(agent, "volumeMounts")); strings.Join(got, ",") != "aws-local-ca" {
				t.Errorf("sidecar mounts %v", got)
			}

			// The chart's own volumes come first, the extra ones after them.
			if got := namesOf(spec["volumes"]); !strings.HasSuffix(strings.Join(got, ","), "pod-template,aws-local-ca,pod-identity-token") {
				t.Errorf("volumes %v", got)
			}
			tok := dig(spec["volumes"], len(namesOf(spec["volumes"]))-1)
			if dig(tok, "projected", "sources", 0, "serviceAccountToken", "audience") != "pods.eks.amazonaws.com" ||
				toInt(dig(tok, "projected", "sources", 0, "serviceAccountToken", "expirationSeconds")) != 3600 {
				t.Errorf("token volume %v", tok)
			}

			server := containerNamed(spec, "server")
			mounts := namesOf(server["volumeMounts"])
			if len(mounts) < 2 || mounts[len(mounts)-2] != "aws-local-ca" || mounts[len(mounts)-1] != "pod-identity-token" {
				t.Errorf("server mounts %v", mounts)
			}
			if dig(server["volumeMounts"], len(mounts)-1, "mountPath") != "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount" {
				t.Errorf("server mounts %v", server["volumeMounts"])
			}
			env := envMap(server)
			if env["AWS_REGION"] != "us-east-1" || dig(env["TOKEN_FROM_SECRET"], "secretKeyRef", "name") != "tok" {
				t.Errorf("server env %v", env)
			}
			if dig(env["OF_REPLICA_ID"], "fieldRef", "fieldPath") != "metadata.name" {
				t.Error("the chart's own env is gone")
			}
			names := namesOf(server["env"])
			if names[0] != "OF_REPLICA_ID" || names[len(names)-1] != "TOKEN_FROM_SECRET" {
				t.Errorf("env order %v", names)
			}
			if len(envMap(agent)) != 0 {
				t.Error("the extra env leaked into the sidecar")
			}

			svc := r.find("Service", "factory-openfactory")
			if dig(svc, "metadata", "annotations", "example.com/lb") != "internal" {
				t.Errorf("service %v", dig(svc, "metadata"))
			}
		})
	}
}

func TestServerExtrasDefaultsAreEmpty(t *testing.T) {
	c := loadChart(t)
	cases := []struct {
		name        string
		overrides   map[string]any
		wantVolumes string
	}{
		// The StatefulSet gets its data volume from a claim template.
		{"sqlite", set("secretKey", key), "tmp,plugins,pod-template"},
		{"postgres", set("secretKey", key, "database.mode", "postgres", "database.postgres.url", "postgres://u:p@db/d"), "data,tmp,plugins,pod-template"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := c.render(tc.overrides)
			if err != nil {
				t.Fatal(err)
			}
			spec := serverPodSpec(t, r)
			if got := strings.Join(namesOf(spec["containers"]), ","); got != "server" {
				t.Errorf("containers %s", got)
			}
			if got := strings.Join(namesOf(spec["volumes"]), ","); got != tc.wantVolumes {
				t.Errorf("volumes %s, want %s", got, tc.wantVolumes)
			}
			server := containerNamed(spec, "server")
			if got := strings.Join(namesOf(server["volumeMounts"]), ","); got != "data,tmp,plugins,pod-template" {
				t.Errorf("mounts %s", got)
			}
			if got := strings.Join(namesOf(server["env"]), ","); got != "OF_REPLICA_ID" {
				t.Errorf("env %s", got)
			}
			if dig(r.find("Service", "factory-openfactory"), "metadata", "annotations") != nil {
				t.Error("the Service has annotations by default")
			}
		})
	}
}

func TestPodTemplateValue(t *testing.T) {
	c := loadChart(t)
	template := `automountServiceAccountToken: false
volumes:
  - name: aws-local-ca
    configMap: {name: aws-local-ca}
containers:
  - name: sandbox
    env:
      - {name: SSL_CERT_DIR, value: /etc/ssl/certs:/etc/aws-local-ca}
    volumeMounts:
      - {name: aws-local-ca, mountPath: /etc/aws-local-ca, readOnly: true}
`
	r, err := c.render(set("secretKey", key, "sandbox.podTemplate", template))
	if err != nil {
		t.Fatal(err)
	}
	cm := r.find("ConfigMap", "-pod-template")
	if cm == nil {
		t.Fatal("no pod template ConfigMap")
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(str(dig(cm, "data", "pod-template.yaml"))), &got); err != nil {
		t.Fatal(err)
	}
	if got["automountServiceAccountToken"] != false || dig(got, "volumes", 0, "configMap", "name") != "aws-local-ca" ||
		dig(got, "containers", 0, "name") != "sandbox" || dig(got, "containers", 0, "env", 0, "name") != "SSL_CERT_DIR" ||
		dig(got, "containers", 0, "volumeMounts", 0, "mountPath") != "/etc/aws-local-ca" {
		t.Errorf("pod template %v", got)
	}
	cfg := r.find("ConfigMap", "factory-openfactory")
	if dig(cfg, "data", "OF_K8S_POD_TEMPLATE") != "/etc/openfactory/pod-template.yaml" {
		t.Error("OF_K8S_POD_TEMPLATE is not set")
	}
}

// TestLocalStackValues renders the values shape of the local test stack
// (docs/self-hosting/local-testing.md): PostgreSQL mode with two replicas, the
// Pod Identity agent sidecar and a sandbox namespace of its own.
func TestLocalStackValues(t *testing.T) {
	c := loadChart(t)
	r, err := c.renderAs(merge(set("secretKey", key), localStackValues()), "openfactory", "of-local")
	if err != nil {
		t.Fatal(err)
	}
	dep := r.find("Deployment", "openfactory")
	if dep == nil || toInt(dig(dep, "spec", "replicas")) != 2 {
		t.Fatalf("deployment %v", dep)
	}
	spec := serverPodSpec(t, r)
	if got := namesOf(spec["containers"]); strings.Join(got, ",") != "server,pod-identity-agent" {
		t.Errorf("containers %v", got)
	}
	cfg := r.find("ConfigMap", "openfactory")
	if dig(cfg, "data", "OF_K8S_NAMESPACE") != "of-local-sandboxes" {
		t.Errorf("sandbox namespace %v", dig(cfg, "data", "OF_K8S_NAMESPACE"))
	}
	if r.find("Role", "-sandbox") == nil {
		t.Error("no sandbox Role")
	}
}

func localStackValues() map[string]any {
	return merge(set(
		"fullnameOverride", "openfactory",
		"image.repository", "of-local/openfactory", "image.tag", "latest", "image.pullPolicy", "Never",
		"database.mode", "postgres",
		"database.postgres.url", "postgres://openfactory:pw@postgres.of-local-infra.svc:5432/openfactory?sslmode=disable",
		"server.replicaCount", 2,
		"server.service.type", "LoadBalancer",
		"server.publicURL", "http://of.k8s.orb.local",
		"server.env", map[string]any{"OF_OBJECT_STORE_RETENTION": "24h"},
		"objectStore.url", "s3://of-local-runs/data?region=us-east-1",
		"sandbox.namespace", "of-local-sandboxes", "sandbox.createNamespace", true,
		"sandbox.image", "of-local/sandbox:latest",
		"sandbox.k8s.imagePullPolicy", "Never",
		"sandbox.podTemplate", "automountServiceAccountToken: false\nenableServiceLinks: false\n",
	), extraValues())
}

// TestRenderToFile renders the chart to one multi-document YAML file. The
// local test stack runs it, then applies the file with kubectl, so that Helm
// is not needed:
//
//	OF_HELM_RENDER_VALUES=values.yaml OF_HELM_RENDER_OUT=out.yaml \
//	  go test ./deploy/helm -run TestRenderToFile -count=1
//
// OF_HELM_RENDER_VALUES is a values file that overrides the chart defaults.
// OF_HELM_RENDER_NAMESPACE (default "default") and OF_HELM_RENDER_RELEASE
// (default "openfactory") set the release namespace and name. The manifests
// get metadata.namespace (the release namespace) when the chart sets none, so
// `kubectl apply -f` needs no --namespace flag. The order is the order of the
// templates, and the keys of each manifest are sorted.
func TestRenderToFile(t *testing.T) {
	valuesPath, outPath := os.Getenv("OF_HELM_RENDER_VALUES"), os.Getenv("OF_HELM_RENDER_OUT")
	if valuesPath == "" || outPath == "" {
		t.Skip("set OF_HELM_RENDER_VALUES and OF_HELM_RENDER_OUT to render the chart to a file")
	}
	namespace := cmpOr(os.Getenv("OF_HELM_RENDER_NAMESPACE"), "default")
	release := cmpOr(os.Getenv("OF_HELM_RENDER_RELEASE"), "openfactory")
	var overrides map[string]any
	readYAML(t, valuesPath, &overrides)
	if overrides == nil {
		overrides = map[string]any{}
	}
	c := loadChart(t)
	r, err := c.renderAs(overrides, release, namespace)
	if err != nil {
		t.Fatal(err)
	}
	// Each manifest gets the release namespace unless it names one (the Role
	// of the sandbox namespace) or is cluster-scoped, so that `kubectl apply
	// -f` needs no --namespace flag.
	var out bytes.Buffer
	for _, doc := range r.docs {
		if !clusterScoped[str(doc["kind"])] && dig(doc, "metadata", "namespace") == nil {
			doc["metadata"].(map[string]any)["namespace"] = namespace
		}
		out.WriteString("---\n")
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := enc.Encode(doc); err != nil {
			t.Fatal(err)
		}
		if err := enc.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outPath, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d manifests to %s", len(r.docs), outPath)
}

// clusterScoped lists the kinds of the chart that have no namespace.
var clusterScoped = map[string]bool{"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
