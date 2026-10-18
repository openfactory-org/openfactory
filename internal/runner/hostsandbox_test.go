package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a posix shell")
	}
}

func TestHostSandboxIdentity(t *testing.T) {
	dir := t.TempDir()
	if s := newHostSandbox("", dir, nil); s.ID() != "local" || s.WorkDir() != dir {
		t.Fatalf("id %q dir %q", s.ID(), s.WorkDir())
	}
	if s := newHostSandbox("sbx_1", dir, nil); s.ID() != "sbx_1" {
		t.Fatalf("id %q", s.ID())
	}
	if err := newHostSandbox("", dir, nil).Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHostSandboxExec(t *testing.T) {
	skipWindows(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := newHostSandbox("", dir, []string{"PATH=" + os.Getenv("PATH"), "BASE=one", "BOTH=base"})
	ctx := context.Background()

	res, err := s.Exec(ctx, core.ExecRequest{
		Cmd:   []string{"sh", "-c", `cat; echo "$BASE $BOTH $EXTRA"; pwd; echo err >&2; exit 3`},
		Dir:   "sub",
		Env:   map[string]string{"BOTH": "req", "EXTRA": "two"},
		Stdin: strings.NewReader("input\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("exit code %d", res.ExitCode)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "sub"))
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) != 3 || lines[0] != "input" || lines[1] != "one req two" {
		t.Fatalf("stdout %q", res.Stdout)
	}
	if got, _ := filepath.EvalSymlinks(lines[2]); got != want {
		t.Fatalf("dir %q, want %q", got, want)
	}
	if strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("stderr %q", res.Stderr)
	}

	// An empty dir is the workspace.
	res, err = s.Exec(ctx, core.ExecRequest{Cmd: []string{"pwd"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout)); got != func() string { p, _ := filepath.EvalSymlinks(dir); return p }() {
		t.Fatalf("default dir %q", res.Stdout)
	}

	if _, err := s.Exec(ctx, core.ExecRequest{Cmd: []string{"/no/such/binary"}}); err == nil {
		t.Fatal("start failure is not an error")
	}
	if _, err := s.Exec(ctx, core.ExecRequest{}); err == nil {
		t.Fatal("empty command is not an error")
	}
}

func TestHostSandboxExecTimeoutAndCap(t *testing.T) {
	skipWindows(t)
	s := newHostSandbox("", t.TempDir(), []string{"PATH=" + os.Getenv("PATH")})
	t0 := time.Now()
	res, err := s.Exec(context.Background(), core.ExecRequest{Cmd: []string{"sleep", "30"}, Timeout: 100 * time.Millisecond})
	if err == nil || res == nil || res.ExitCode != -1 {
		t.Fatalf("timeout: res %+v err %v", res, err)
	}
	if time.Since(t0) > 10*time.Second {
		t.Fatal("timeout did not kill the process")
	}

	// 5 MiB of output: keep the end.
	res, err = s.Exec(context.Background(), core.ExecRequest{Cmd: []string{"sh", "-c", "head -c 5242880 /dev/zero | tr '\\0' a; printf END"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != maxCapturedOutput || !strings.HasSuffix(res.Stdout, "END") {
		t.Fatalf("stdout len %d", len(res.Stdout))
	}
}

func TestHostSandboxStartAndKill(t *testing.T) {
	skipWindows(t)
	s := newHostSandbox("", t.TempDir(), []string{"PATH=" + os.Getenv("PATH")})
	ctx := context.Background()

	p, err := s.Start(ctx, core.ExecRequest{Cmd: []string{"sh", "-c", "read x; echo got:$x; echo oops >&2; exit 4"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p.Stdin(), "hello\n"); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(p.Stdout())
	errOut, _ := io.ReadAll(p.Stderr())
	code, err := p.Wait()
	if err != nil || code != 4 {
		t.Fatalf("wait: %d %v", code, err)
	}
	if strings.TrimSpace(string(out)) != "got:hello" || strings.TrimSpace(string(errOut)) != "oops" {
		t.Fatalf("out %q err %q", out, errOut)
	}
	if code, _ := p.Wait(); code != 4 {
		t.Fatalf("second wait: %d", code)
	}

	// Kill ends the whole group: the child "sleep" holds the pipe open.
	p, err = s.Start(ctx, core.ExecRequest{Cmd: []string{"sh", "-c", "sleep 30 & echo started; wait"}})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 7)
	if _, err := io.ReadFull(p.Stdout(), buf); err != nil || string(buf) != "started" {
		t.Fatalf("read %q %v", buf, err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.ReadAll(p.Stdout()) // ends only if the child "sleep" died too
		p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("kill did not end the process group")
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("kill after exit: %v", err)
	}
}

func TestHostSandboxFiles(t *testing.T) {
	dir := t.TempDir()
	s := newHostSandbox("", dir, nil)
	ctx := context.Background()
	if err := s.WriteFile(ctx, "a/b/c.txt", []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadFile(ctx, "a/b/c.txt")
	if err != nil || string(got) != "data" {
		t.Fatalf("read %q %v", got, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(dir, "a", "b", "c.txt")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v %v", fi, err)
		}
	}
	// An absolute path and an overwrite.
	abs := filepath.Join(dir, "x", "y.txt")
	if err := s.WriteFile(ctx, abs, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFile(ctx, abs, []byte("22"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadFile(ctx, abs); err != nil || string(got) != "22" {
		t.Fatalf("read %q %v", got, err)
	}
	if _, err := s.ReadFile(ctx, "missing"); err == nil {
		t.Fatal("missing file is not an error")
	}
}
