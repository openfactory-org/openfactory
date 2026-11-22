package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type commandHookKey struct{}

// WithCommandHook returns a context whose git commands pass through hook
// before they start. The runner uses it to run git as the agent user when
// the egress lock is on: the agent controls .git/config, so git must never
// run as root in the workspace.
func WithCommandHook(ctx context.Context, hook func(*exec.Cmd)) context.Context {
	return context.WithValue(ctx, commandHookKey{}, hook)
}

// git runs "git -C dir args..." and returns the standard output without the
// trailing newline.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// noAutoMaintenance turns off the automatic maintenance of git. Without it,
// "git fetch" and other commands start "git maintenance run --auto --detach"
// (or "git gc --auto" in old versions) in the background. That process can
// still repack or write files in the repository after the command returns,
// while the runner archives or removes the directory.
var noAutoMaintenance = []string{"-c", "maintenance.auto=false", "-c", "gc.auto=0"}

// gitRaw runs "git -C dir args..." and returns the exact bytes of standard
// output.
func gitRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append(append([]string{"-C", dir}, noAutoMaintenance...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, identityEnv()...)
	if hook, ok := ctx.Value(commandHookKey{}).(func(*exec.Cmd)); ok && hook != nil {
		hook(cmd)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// identityEnv gives git a fixed identity for its reflog entries when the
// environment has none. Without an identity, git asks the resolver for the
// host name, and that lookup can wait for seconds in a sandbox. The commands
// of this package make no commits, so the identity shows only in the reflog.
func identityEnv() []string {
	var env []string
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "openfactory"},
		{"GIT_AUTHOR_EMAIL", "openfactory@localhost"},
		{"GIT_COMMITTER_NAME", "openfactory"},
		{"GIT_COMMITTER_EMAIL", "openfactory@localhost"},
	} {
		if os.Getenv(kv[0]) == "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}
