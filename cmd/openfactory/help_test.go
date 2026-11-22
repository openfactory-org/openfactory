package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The help text of every command must not show the value of an environment
// variable that holds a secret. The flag defaults come from the environment,
// so a secret value in the environment must not reach the help.
func TestHelpDoesNotShowSecretValues(t *testing.T) {
	const secret = "of_SENTINEL_secret_value_9f3a"
	for _, name := range []string{
		"OF_API_KEY", "OF_SERVER", "OF_SECRET_KEY", "OF_SIGNING_KEY", "OF_DATABASE_URL",
		"OF_OIDC_CLIENT_SECRET", "OF_BOOTSTRAP_TOKEN", "OF_RUNNER_TOKEN",
		"OF_ENROLLMENT_TOKEN",
	} {
		t.Setenv(name, secret)
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		var buf bytes.Buffer
		c.SetOut(&buf)
		c.SetErr(&buf)
		if err := c.Help(); err != nil {
			t.Errorf("%s: help: %v", c.CommandPath(), err)
		}
		if strings.Contains(buf.String(), secret) {
			t.Errorf("%s --help shows the value of a secret:\n%s", c.CommandPath(), buf.String())
		}
		for _, f := range []string{c.Flags().FlagUsages(), c.InheritedFlags().FlagUsages(), c.UsageString()} {
			if strings.Contains(f, secret) {
				t.Errorf("%s: flag usage shows the value of a secret", c.CommandPath())
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd())
}

// The environment still sets the values, and a flag wins over it.
func TestGlobalFlagsReadEnvironment(t *testing.T) {
	t.Setenv("OF_API_KEY", "key-from-env")
	t.Setenv("OF_SERVER", "http://env.test")
	root := newRootCmd()
	if err := root.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	// newRootCmd keeps its globals private, so check through a subcommand.
	sub, _, err := root.Find([]string{"worker", "status"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api-key", "server"} {
		f := sub.InheritedFlags().Lookup(name)
		if f == nil {
			t.Fatalf("no flag --%s", name)
		}
		if f.Changed {
			t.Errorf("--%s is marked as set", name)
		}
	}
	if f := root.PersistentFlags().Lookup("api-key"); f.DefValue != "" || f.Value.String() != "key-from-env" {
		t.Errorf("api-key: default %q, value %q", f.DefValue, f.Value.String())
	}
	if f := root.PersistentFlags().Lookup("server"); f.DefValue != "http://localhost:8787" || f.Value.String() != "http://env.test" {
		t.Errorf("server: default %q, value %q", f.DefValue, f.Value.String())
	}
	if err := root.PersistentFlags().Parse([]string{"--api-key", "key-from-flag"}); err != nil {
		t.Fatal(err)
	}
	if got := root.PersistentFlags().Lookup("api-key").Value.String(); got != "key-from-flag" {
		t.Errorf("api-key after the flag = %q", got)
	}
}

// Every command that the root help names in parentheses exists.
func TestRootHelpNamesExistingCommands(t *testing.T) {
	root := newRootCmd()
	for _, group := range regexp.MustCompile(`\(([^)]*)\)`).FindAllStringSubmatch(root.Long, -1) {
		for _, name := range strings.Split(group[1], ",") {
			name = strings.TrimSpace(name)
			if c, _, err := root.Find([]string{name}); err != nil || c.Name() != name {
				t.Errorf("root help names %q, which is not a command", name)
			}
		}
	}
}

// Each worker flag has an environment variable.
func TestWorkerFlagsReadEnvironment(t *testing.T) {
	t.Setenv("OF_WORKER_ID", "w-1")
	t.Setenv("OF_WORKER_NAME", "Worker one")
	t.Setenv("OF_WORKER_CAPACITY", "4")
	t.Setenv("OF_WORKER_LABELS", "region=eu, pool=gpu")
	t.Setenv("OF_WORKER_ONE_SHOT", "true")
	t.Setenv("OF_METRICS_ADDR", ":9090")
	f := newWorkerCmd(&globals{}).Flags()
	for name, want := range map[string]string{
		"id": "w-1", "name": "Worker one", "capacity": "4", "label": "[region=eu,pool=gpu]",
		"one-shot": "true", "metrics-addr": ":9090",
	} {
		if got := f.Lookup(name).Value.String(); got != want {
			t.Errorf("--%s = %q, want %q", name, got, want)
		}
	}
}
