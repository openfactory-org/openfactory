package snapshot

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv makes git ignore the configuration of the machine.
func gitEnv(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on the PATH")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return strings.TrimRight(out.String(), "\n")
}

// newRepo makes a bare remote and a clone with one pushed commit.
func newRepo(t *testing.T) (remote, work, firstSHA string) {
	t.Helper()
	gitEnv(t)
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")
	runGit(t, root, "init", "--bare", "-b", "main", remote)
	// No "git clone" here: on some machines a local clone waits for seconds
	// in upload-pack. An init with a remote gives the same result.
	runGit(t, root, "init", "-q", "-b", "main", work)
	runGit(t, work, "remote", "add", "origin", remote)
	runGit(t, work, "config", "user.name", "Test")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "checkout", "-q", "-B", "main")
	writeFile(t, filepath.Join(work, "a.txt"), "first\n", 0o644)
	writeFile(t, filepath.Join(work, ".gitignore"), "ignored/\n", 0o644)
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "first")
	runGit(t, work, "push", "-q", "-u", "origin", "main")
	return remote, work, runGit(t, work, "rev-parse", "HEAD")
}

func cloneOf(t *testing.T, remote string) string {
	t.Helper()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "second")
	// A bundle carries the commits of the remote without upload-pack.
	bundle := filepath.Join(tmp, "remote.bundle")
	runGit(t, remote, "bundle", "create", "-q", bundle, "--all")
	runGit(t, tmp, "init", "-q", "-b", "main", dir)
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "remote", "add", "origin", remote)
	runGit(t, dir, "fetch", "-q", bundle, "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, dir, "checkout", "-q", "-B", "main", "origin/main")
	return dir
}

func capture(t *testing.T, work, base string) (RepoEntry, []Skipped, string, error) {
	t.Helper()
	out := t.TempDir()
	e, sk, err := CaptureRepo(context.Background(), CaptureOptions{
		RepoDir: work, Path: "api", BaseSHA: base, OutDir: out, Index: 0,
	})
	return e, sk, out, err
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCaptureRestoreFull(t *testing.T) {
	remote, work, first := newRepo(t)
	writeFile(t, filepath.Join(work, "a.txt"), "local\n", 0o644)
	runGit(t, work, "commit", "-q", "-am", "local")
	local := runGit(t, work, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(work, "a.txt"), "local\nwip\n", 0o644)
	writeFile(t, filepath.Join(work, "b.txt"), "new tracked\n", 0o644)
	runGit(t, work, "add", "b.txt")
	writeFile(t, filepath.Join(work, "notes/todo.txt"), "todo\n", 0o644)
	writeFile(t, filepath.Join(work, "ignored/cache.bin"), "cache", 0o644)

	e, _, out, err := capture(t, work, first)
	if err != nil {
		t.Fatal(err)
	}
	if e.Bundle == "" || e.Patch == "" || e.Untracked == "" {
		t.Fatalf("entry incomplete: %+v", e)
	}
	for _, f := range []string{e.Bundle, e.Patch, e.Untracked} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if e.HeadSHA != local || e.UpstreamSHA != first || e.Branch != "main" || e.Path != "api" || e.Remote != remote {
		t.Fatalf("unexpected entry: %+v", e)
	}

	second := cloneOf(t, remote)
	if err := RestoreRepo(context.Background(), second, e, out); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, second, "rev-parse", "HEAD"); got != e.HeadSHA {
		t.Errorf("HEAD %s, want %s", got, e.HeadSHA)
	}
	for _, f := range []string{"a.txt", "b.txt", "notes/todo.txt"} {
		if a, b := readFile(t, filepath.Join(work, f)), readFile(t, filepath.Join(second, f)); a != b {
			t.Errorf("%s: %q != %q", f, a, b)
		}
	}
	if _, err := os.Stat(filepath.Join(second, "ignored/cache.bin")); !os.IsNotExist(err) {
		t.Errorf("ignored file must not exist: %v", err)
	}
	if staged := runGit(t, second, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("changes must not be staged: %q", staged)
	}
}

func TestCaptureCleanRepo(t *testing.T) {
	remote, work, first := newRepo(t)
	e, sk, out, err := capture(t, work, first)
	if err != nil {
		t.Fatal(err)
	}
	if e.Bundle != "" || e.Patch != "" || e.Untracked != "" || e.UpstreamSHA != e.HeadSHA || len(sk) != 0 {
		t.Fatalf("unexpected entry: %+v, %v", e, sk)
	}
	second := cloneOf(t, remote)
	if err := RestoreRepo(context.Background(), second, e, out); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureDetachedHead(t *testing.T) {
	remote, work, first := newRepo(t)
	runGit(t, work, "checkout", "-q", "--detach")
	writeFile(t, filepath.Join(work, "a.txt"), "detached\n", 0o644)
	runGit(t, work, "commit", "-q", "-am", "detached")
	e, _, out, err := capture(t, work, first)
	if err != nil {
		t.Fatal(err)
	}
	if e.Branch != "" || e.UpstreamSHA != first || e.Bundle == "" {
		t.Fatalf("unexpected entry: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(out, e.Bundle)); err != nil {
		t.Fatal(err)
	}
	second := cloneOf(t, remote)
	if err := RestoreRepo(context.Background(), second, e, out); err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, second, "rev-parse", "HEAD"); got != e.HeadSHA {
		t.Errorf("HEAD %s, want %s", got, e.HeadSHA)
	}
	cmd := exec.Command("git", "-C", second, "symbolic-ref", "-q", "HEAD")
	if err := cmd.Run(); err == nil {
		t.Error("HEAD must be detached")
	}
}

func TestCaptureNoUpstreamNoBase(t *testing.T) {
	_, work, _ := newRepo(t)
	runGit(t, work, "checkout", "-q", "--detach")
	if _, _, _, err := capture(t, work, ""); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCaptureBinaryPatch(t *testing.T) {
	remote, work, _ := newRepo(t)
	bin := []byte{0x00, 0xFF, 0x10, 0x00, 0xFE, 0x80}
	if err := os.WriteFile(filepath.Join(work, "data.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "add", "data.bin")
	runGit(t, work, "commit", "-q", "-m", "bin")
	runGit(t, work, "push", "-q")
	first := runGit(t, work, "rev-parse", "HEAD")
	changed := []byte{0xFF, 0x00, 0x00, 0x01, 0xFF, 0x7F, 0x00}
	if err := os.WriteFile(filepath.Join(work, "data.bin"), changed, 0o644); err != nil {
		t.Fatal(err)
	}
	e, _, out, err := capture(t, work, first)
	if err != nil {
		t.Fatal(err)
	}
	if e.Patch == "" {
		t.Fatal("expected a patch")
	}
	second := cloneOf(t, remote)
	if err := RestoreRepo(context.Background(), second, e, out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(second, "data.bin"))
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("got %v, err %v", got, err)
	}
}

func TestCaptureSkippedOtherBranch(t *testing.T) {
	_, work, first := newRepo(t)
	runGit(t, work, "checkout", "-q", "-b", "side")
	writeFile(t, filepath.Join(work, "s.txt"), "side\n", 0o644)
	runGit(t, work, "add", "s.txt")
	runGit(t, work, "commit", "-q", "-m", "side")
	runGit(t, work, "checkout", "-q", "main")
	_, sk, _, err := capture(t, work, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sk {
		if s.Reason == "other_branch" && strings.HasSuffix(s.Path, "@side") {
			return
		}
	}
	t.Fatalf("no other_branch entry: %v", sk)
}

func TestCaptureSkippedLFS(t *testing.T) {
	_, work, first := newRepo(t)
	writeFile(t, filepath.Join(work, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n", 0o644)
	_, sk, _, err := capture(t, work, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sk {
		if s.Reason == "lfs" {
			return
		}
	}
	t.Fatalf("no lfs entry: %v", sk)
}
