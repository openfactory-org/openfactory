package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rewrap"
)

func TestPrintRewrapCountsAndExitStatus(t *testing.T) {
	var out, errw bytes.Buffer
	if err := printRewrap(&out, &errw, rewrap.Report{Rewrapped: 3, Current: 2, Changed: 1}, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rewrapped: 3", "already current: 2", "changed during the run: 1", "failed: 0"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q lacks %q", out.String(), want)
		}
	}
	out.Reset()
	err := printRewrap(&out, &errw, rewrap.Report{Rewrapped: 1, Failed: 1, Errors: []string{"secret A: boom"}}, true)
	if !errors.Is(err, errSilent) {
		t.Fatalf("a failure must give a non-zero exit, got %v", err)
	}
	if !strings.Contains(out.String(), "would rewrap: 1") || !strings.Contains(errw.String(), "secret A: boom") {
		t.Fatalf("out %q err %q", out.String(), errw.String())
	}
}

func TestSecretRewrapCommandIsRegistered(t *testing.T) {
	c, _, err := newRootCmd().Find([]string{"secret", "rewrap"})
	if err != nil || c.Name() != "rewrap" || c.Flags().Lookup("dry-run") == nil {
		t.Fatalf("command: %v %v", c, err)
	}
}
