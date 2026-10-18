package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errb.String(), err
}

func TestInitWritesV1(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	if _, _, err := runCLI(t, "init", dir, "--repo", "acme/app"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "factory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "schemaVersion: v1\n") {
		t.Fatalf("init did not write schemaVersion v1:\n%s", b)
	}
	if _, stderr, err := runCLI(t, "validate", dir); err != nil || strings.Contains(stderr, "warning") {
		t.Fatalf("validate v1: %v %q", err, stderr)
	}
}

func TestWorkerRejectsUnknownBackends(t *testing.T) {
	for _, tc := range []struct{ backend, want string }{
		{"docker", "unknown --backend"},
		{"vm", "unknown --backend"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			_, _, err := runCLI(t, "worker", "--api-key", "k", "--id", "w1", "--backend", tc.backend)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}
