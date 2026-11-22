package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
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

func TestLocalBackendOptions(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{"OF_SANDBOX_ROOT": root, "OF_RUNNER_EXEC_COMMAND": "cat"}
	exe := func() (string, error) { return "/bin/openfactory-runtime", nil }
	opts, err := localBackendOptions(func(k string) string { return env[k] }, exe)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Root != filepath.Join(root, "tasks") || !reflect.DeepEqual(opts.Command, []string{"/bin/openfactory-runtime", "runner"}) || opts.ExtraEnv["OF_RUNNER_EXEC_COMMAND"] != "cat" {
		t.Fatalf("options = %+v", opts)
	}
	env = map[string]string{"OF_DATA_DIR": root, "OF_RUNNER_COMMAND": "/x/of runner --flag"}
	opts, err = localBackendOptions(func(k string) string { return env[k] }, exe)
	if err != nil || opts.Root != filepath.Join(root, "sandboxes", "tasks") || len(opts.Command) != 3 {
		t.Fatalf("options = %+v, %v", opts, err)
	}
	if _, err := localBackendOptions(func(string) string { return "" }, func() (string, error) { return "", errors.New("no") }); err == nil {
		t.Fatal("a missing executable must fail")
	}
}

func TestBuildWorkerBackend(t *testing.T) {
	root := t.TempDir()
	b, err := buildWorkerBackend(func(k string) string {
		if k == "OF_SANDBOX_ROOT" {
			return root
		}
		return ""
	})
	if err != nil || b.Name() != "local" {
		t.Fatalf("backend = %v, %v", b, err)
	}
}
