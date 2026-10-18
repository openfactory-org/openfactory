package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/config"
)

// k8sTestConfig returns a config for the kubernetes backend with a kubeconfig
// file that needs no cluster: the backend does not connect when it is built.
func k8sTestConfig(t *testing.T) *config.Config {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "kubeconfig")
	err := os.WriteFile(kc, []byte(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: "https://127.0.0.1:6443"}
users:
- name: u
  user: {token: t}
contexts:
- name: x
  context: {cluster: c, user: u}
current-context: x
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{SandboxBackend: config.BackendKubernetes, Kubeconfig: kc, RunnerImage: "example.test/runner:1"}
}

// TestBuildTaskBackendKubernetesEgressWait checks that the settings of the
// wait for the NetworkPolicy reach the options of the backend: the backend
// rejects a bad target, so a good target and a bad one give different results.
func TestBuildTaskBackendKubernetesEgressWait(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := k8sTestConfig(t)
	cfg.K8sEgressWaitAddr = "10.0.0.1:10250"
	cfg.K8sEgressWaitTimeout = "45s"
	if b, _, err := buildTaskBackend(cfg, nil, log); err != nil || b == nil {
		t.Fatalf("good wait settings: %v %v", b, err)
	}

	cfg = k8sTestConfig(t)
	cfg.K8sEgressWaitAddr = "not-an-address"
	if _, _, err := buildTaskBackend(cfg, nil, log); err == nil || !strings.Contains(err.Error(), "egress wait target") {
		t.Fatalf("bad wait target reached no backend check: %v", err)
	}
}
