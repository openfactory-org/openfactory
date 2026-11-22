package cache

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/gitcachekey"
)

func fakeMkfs(_ context.Context, _ string, args ...string) (string, error) {
	return "", os.WriteFile(args[len(args)-2], []byte("ext4"), 0o644)
}

func makeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "one")
	return dir
}

func TestEnsureMirrorAndDisks(t *testing.T) {
	src := makeRepo(t)
	url := "file://localhost" + src
	key := gitcachekey.Key(url)
	c := &Cache{Dir: t.TempDir(), Mkfs: fakeMkfs}
	if c.Latest(key) != "" {
		t.Fatal("disk before ensure")
	}
	if err := c.Ensure(context.Background(), key, url); err != nil {
		t.Fatal(err)
	}
	first := c.Latest(key)
	if first == "" {
		t.Fatal("no disk")
	}
	out, err := exec.Command("git", "--git-dir", c.MirrorDir(key), "config", "--get-all", "remote.origin.fetch").Output()
	if err != nil || !strings.Contains(string(out), "+refs/heads/*:refs/heads/*") || !strings.Contains(string(out), "+refs/tags/*:refs/tags/*") {
		t.Fatalf("fetch specs %q %v", out, err)
	}
	// Fresh disk: no rebuild.
	if err := c.Ensure(context.Background(), key, url); err != nil || c.Latest(key) != first {
		t.Fatal("rebuilt a fresh disk")
	}
	// Old disk: fetch and build a new generation; keep two.
	for i := 0; i < 3; i++ {
		old := c.Latest(key)
		_ = os.Chtimes(old, timeAgo(), timeAgo())
		if err := c.Ensure(context.Background(), key, url); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(c.generations(key)); n != 2 {
		t.Fatalf("generations %d", n)
	}
	if err := c.Ensure(context.Background(), "0000000000000000", url); err == nil {
		t.Fatal("a wrong key must fail")
	}
	if c.Prune(0, func(k string) bool { return k == key }) != 0 {
		t.Fatal("pruned a key in use")
	}
	if c.Prune(0, nil) == 0 || c.Latest(key) != "" {
		t.Fatal("prune")
	}
}

func TestHelperArgs(t *testing.T) {
	c := &Cache{Helper: "/usr/local/bin/openfactory-runtime worker git-credential"}
	got := strings.Join(c.gitArgs("fetch"), " ")
	if got != "-c credential.helper= -c credential.helper=!/usr/local/bin/openfactory-runtime worker git-credential fetch" {
		t.Fatalf("args %q", got)
	}
}

func TestCredentialHelper(t *testing.T) {
	var gotRepo, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRepo, gotAuth = r.URL.Query().Get("repo"), r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/workers/wkr_1/git-credential" {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(gotRepo, "secret") {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"username":"x-access-token","password":"ghs_test","expires_at":"2026-11-22T12:00:00Z"}`))
	}))
	defer srv.Close()
	env := map[string]string{EnvControlURL: srv.URL, EnvWorkerID: "wkr_1", EnvAccessToken: "ofw_test"}
	var out bytes.Buffer
	in := strings.NewReader("protocol=https\nhost=github.com\npath=acme/app.git\n\n")
	if err := HelperMain(context.Background(), "get", in, &out, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	if out.String() != "username=x-access-token\npassword=ghs_test\n" || gotRepo != "https://github.com/acme/app.git" || gotAuth != "Bearer ofw_test" {
		t.Fatalf("out %q repo %q auth %q", out.String(), gotRepo, gotAuth)
	}
	_, err := FetchCredential(context.Background(), srv.Client(), srv.URL, "wkr_1", "ofw_test", "https://github.com/acme/secret")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("refused: %v", err)
	}
	out.Reset()
	if err := HelperMain(context.Background(), "store", strings.NewReader(""), &out, func(string) string { return "" }); err != nil || out.Len() != 0 {
		t.Fatal("store must be ignored")
	}
}

func timeAgo() time.Time { return time.Now().Add(-2 * MaxAge) }
