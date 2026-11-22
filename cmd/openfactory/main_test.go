package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestCommandTree(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{
		{"runner"}, {"netguard"}, {"worker"}, {"vm-init"}, {"version"},
		{"worker", "enroll"}, {"worker", "run"}, {"worker", "status"}, {"worker", "drain"}, {"worker", "doctor"},
	} {
		c, _, err := root.Find(path)
		if err != nil || c.Name() != path[len(path)-1] {
			t.Errorf("command %v not found: %v", path, err)
		}
	}
	if root.Name() != RuntimeName {
		t.Errorf("root command %q, want %q", root.Name(), RuntimeName)
	}
	// The control plane and the client commands moved to TypeScript.
	for _, gone := range []string{"serve", "mcp", "validate", "schema", "init", "bootstrap", "factory", "run", "work-item", "inbox", "secret", "webhook", "benchmark", "mcp-registry", "netprobe", "agent"} {
		if c, _, err := root.Find([]string{gone}); err == nil && c.Name() == gone {
			t.Errorf("the %s command must not exist", gone)
		}
	}
	if c, _, err := root.Find([]string{"worker", "list"}); err == nil && c.Name() == "list" {
		t.Error("the worker list command must not exist")
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil || !strings.HasPrefix(out.String(), RuntimeName+" ") {
		t.Fatalf("version: %v %q", err, out.String())
	}
	out.Reset()
	root.SetArgs([]string{"version", "--short"})
	if err := root.Execute(); err != nil || out.String() != version+"\n" {
		t.Fatalf("version --short: %v %q", err, out.String())
	}
	for _, path := range [][]string{{"netlock", "probe"}, {"netlock", "rules"}, {"runner", "git-credential"}, {"runner", "cloud-token"}} {
		if c, _, err := root.Find(path); err != nil || c.Name() != path[len(path)-1] {
			t.Errorf("command %v not found: %v", path, err)
		}
	}
}

// vm-init refuses to run outside a microVM guest.
func TestVMInitRefusesOutsideGuest(t *testing.T) {
	root := newRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"vm-init"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "PID 1") {
		t.Fatalf("vm-init: %v", err)
	}
}

// TestNetlockRules prints the rules of the lock.
func TestNetlockRules(t *testing.T) {
	t.Setenv("OF_LOOPBACK_DAEMON_PORTS", "8911")
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"netlock", "rules"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"iptables -w 5 -N OF_LOCK", "--dport 8911", "ip6tables -w 5 -I OUTPUT 1 -j OF_LOCK"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("rules lack %q:\n%s", want, out.String())
		}
	}
}

// TestNetlockProbe runs the probe command with a specification that has no
// network target: only the loopback check runs.
func TestNetlockProbe(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"netlock", "probe"})
	old := os.Stdin
	r, w, _ := os.Pipe()
	_, _ = w.WriteString(`{"proxy_url":"http://127.0.0.1:1","direct_targets":[],"restricted":false}`)
	w.Close()
	os.Stdin = r
	defer func() { os.Stdin = old }()
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"name":"loopback","ok":true`) {
		t.Errorf("probe output: %s", out.String())
	}
}
