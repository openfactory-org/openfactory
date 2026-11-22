package main

import (
	"bytes"
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

// Only the process backend is left: the worker without a subcommand needs
// --process, and the old --backend flag is gone.
func TestWorkerNeedsProcess(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no process", []string{"worker", "--api-key", "k", "--id", "w1"}, "--process"},
		{"backend flag", []string{"worker", "--api-key", "k", "--id", "w1", "--backend", "docker"}, "unknown flag: --backend"},
		{"no key", []string{"worker", "--process", "--id", "w1"}, "--api-key"},
		{"reserved id", []string{"worker", "--process", "--api-key", "k", "--id", "server"}, "reserved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OF_API_KEY", "")
			t.Setenv("OF_WORKER_KEY_FILE", "")
			_, _, err := runCLI(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}
