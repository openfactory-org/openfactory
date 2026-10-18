package main

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/server"
)

func TestWorkerTaskEnv(t *testing.T) {
	env, err := workerTaskEnv(func(k string) string {
		if k == "OF_IDLE_TIMEOUT_MAX_SECONDS" {
			return " 600 "
		}
		return ""
	})
	if err != nil || env["OF_IDLE_TIMEOUT_MAX_SECONDS"] != "600" || len(env) != 1 {
		t.Fatalf("env = %v, %v", env, err)
	}
	env, err = workerTaskEnv(func(string) string { return "" })
	if err != nil || len(env) != 0 {
		t.Fatalf("empty env = %v, %v", env, err)
	}
	for _, bad := range []string{"abc", "-5", "1.5"} {
		if _, err := workerTaskEnv(func(string) string { return bad }); err == nil {
			t.Errorf("value %q must fail", bad)
		}
	}
}

func TestWorkerBackendConfig(t *testing.T) {
	t.Setenv("OF_SANDBOX_ROOT", t.TempDir())
	cfg, err := workerBackendConfig(config.BackendLocal)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SandboxBackend != config.BackendLocal || !cfg.Dev {
		t.Fatalf("config = %+v", cfg)
	}
	b, _, err := server.BuildTaskBackend(cfg, cfg.Logger())
	if err != nil || b.Name() != "local" {
		t.Fatalf("backend = %v, %v", b, err)
	}
}
