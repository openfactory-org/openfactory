package gitgeneric

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestURLs(t *testing.T) {
	tests := []struct {
		cfg  Config
		want string
	}{
		{Config{BaseURL: "https://bitbucket.org", Username: "x-bitbucket-api-token-auth", Token: "t"}, "https://x-bitbucket-api-token-auth:t@bitbucket.org/acme/api.git"},
		{Config{URLTemplate: "https://ado.local/Coll/{owner}/_git/{repo}", Token: "t"}, "https://x-token-auth:t@ado.local/Coll/acme/_git/api"},
		{Config{BaseURL: "git@git.example.com:"}, "git@git.example.com:acme/api.git"},
		{Config{BaseURL: "ssh://git@host:2222", Token: "ignored"}, "ssh://git@host:2222/acme/api.git"},
	}
	for _, tt := range tests {
		c, err := New(tt.cfg)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := c.CloneURL(context.Background(), "acme", "api")
		if got != tt.want {
			t.Errorf("CloneURL = %q want %q", got, tt.want)
		}
	}
}

func TestUnsupported(t *testing.T) {
	c, _ := New(Config{BaseURL: "https://x"})
	ctx := context.Background()
	if _, err := c.CreatePullRequest(ctx, core.PullRequestInput{}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if err := c.Comment(ctx, "a", "b", 1, "x"); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if err := c.SetCommitStatus(ctx, "a", "b", "s", core.CommitStatus{}); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestGetFilesAndDefaultBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	src := filepath.Join(root, "acme", "def")
	if err := os.MkdirAll(filepath.Join(src, "factory", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, src, "init", "-q", "-b", "trunk")
	_ = os.WriteFile(filepath.Join(src, "factory", "factory.yaml"), []byte("name: g\n"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "factory", "agents", "a.md"), []byte("hi"), 0o644)
	_ = os.WriteFile(filepath.Join(src, "README.md"), []byte("readme"), 0o644)
	// A symbolic link to a directory outside the repository must not be read.
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644)
	if err := os.Symlink(outside, filepath.Join(src, "escape")); err != nil {
		t.Fatal(err)
	}
	run(t, src, "add", ".")
	run(t, src, "commit", "-q", "-m", "init")

	c, err := New(Config{BaseURL: "file://" + root, NoGitSuffix: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b, err := c.DefaultBranch(ctx, "acme", "def")
	if err != nil || b != "trunk" {
		t.Fatalf("DefaultBranch = %q %v", b, err)
	}
	files, sha, err := c.GetFiles(ctx, "acme", "def", "", "factory")
	if err != nil {
		t.Fatal(err)
	}
	if len(sha) != 40 || len(files) != 2 || files["factory.yaml"] != "name: g\n" || files["agents/a.md"] != "hi" {
		t.Fatalf("files = %v sha = %s", files, sha)
	}
	for _, p := range []string{"escape", "../../../" + filepath.Base(outside)} {
		if files, _, err := c.GetFiles(ctx, "acme", "def", "", p); err == nil {
			t.Fatalf("GetFiles(%q) read outside the repository: %v", p, files)
		}
	}
	all, _, err := c.GetFiles(ctx, "acme", "def", "", "")
	if err != nil || all["README.md"] != "readme" || all["escape/secret.txt"] != "" {
		t.Fatalf("GetFiles(\"\") = %v %v", all, err)
	}
}
