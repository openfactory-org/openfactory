package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonical(t *testing.T) {
	cases := []struct {
		name, in, wantErr string
	}{
		{"valid", `{"openapi":"3.1.0","info":{"version":"1.0.0"},"paths":{"/a":{}},"servers":[{"url":"x"}]}`, ""},
		{"not json", `{`, "not JSON"},
		{"swagger 2", `{"swagger":"2.0","info":{"version":"1.0.0"},"paths":{"/a":{}}}`, "3.x"},
		{"v2 api", `{"openapi":"3.1.0","info":{"version":"2.0.0"},"paths":{"/a":{}}}`, "1.x"},
		{"no paths", `{"openapi":"3.1.0","info":{"version":"1.0.0"}}`, "no paths"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := canonical([]byte(tc.in))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "servers") {
				t.Error("servers not stripped")
			}
		})
	}
}

func TestRunWritesAndChecks(t *testing.T) {
	dir := t.TempDir()
	spec, out := filepath.Join(dir, "spec.json"), filepath.Join(dir, "base.json")
	if err := run(spec, out, true); err == nil {
		t.Fatal("check passed without a baseline")
	}
	if err := os.WriteFile(spec, []byte(`{"openapi":"3.1.0","info":{"version":"1.0.0"},"paths":{"/a":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(spec, out, false); err != nil {
		t.Fatal(err)
	}
	if err := run(spec, out, true); err != nil {
		t.Fatalf("check after write: %v", err)
	}
}

// TestRepositoryBaseline keeps the committed baseline valid.
func TestRepositoryBaseline(t *testing.T) {
	if err := run("", filepath.Join("..", "..", "api", "openapi.v1.baseline.json"), true); err != nil {
		t.Fatal(err)
	}
}
