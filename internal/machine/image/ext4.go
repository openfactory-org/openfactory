package image

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Run runs a command and returns its combined output.
type Run func(ctx context.Context, name string, args ...string) (string, error)

// ExecRun runs the command without a shell.
func ExecRun(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204: fixed tools, agent-built arguments
	if err != nil {
		return string(out), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Size returns the size of a read-only image disk for st:
// 1.15 × sum(file sizes rounded up to 4 KiB) + 64 MiB, rounded up to 1 MiB.
func Size(st Stats) int64 {
	n := st.Bytes + st.Bytes*15/100 + 64<<20
	return (n + 1<<20 - 1) &^ (1<<20 - 1)
}

// Inodes returns the inode count for st: 1.2 × entries + 1024.
func Inodes(st Stats) int {
	return st.Entries + st.Entries/5 + 1024
}

// MkfsImageArgs returns the mkfs.ext4 arguments of a read-only image disk
// built from src (a directory or, with tar input, a tar file).
func MkfsImageArgs(label, src, out string, st Stats) []string {
	return []string{
		"-q", "-F", "-L", label, "-m", "0", "-O", "^has_journal",
		"-E", "lazy_itable_init=0,lazy_journal_init=0,root_owner=0:0",
		"-N", strconv.Itoa(Inodes(st)), "-d", src, out, strconv.FormatInt(Size(st)>>10, 10) + "K",
	}
}

// MkfsScratchArgs returns the arguments of an empty scratch disk of gib GiB.
// It keeps the journal: the guest writes it.
func MkfsScratchArgs(out string, gib int) []string {
	return []string{"-q", "-F", "-L", "of-scratch", "-m", "0", "-E", "lazy_itable_init=1,nodiscard", out, strconv.Itoa(gib) + "G"}
}

// ProbeTarInput reports whether mkfs.ext4 takes a tar file for -d
// (e2fsprogs 1.47.1+ built with libarchive).
func ProbeTarInput(ctx context.Context, run Run, tmp string) bool {
	if run == nil {
		run = ExecRun
	}
	dir, err := os.MkdirTemp(tmp, "probe")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "probe", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("ok"))
	_ = tw.Close()
	tarPath := filepath.Join(dir, "p.tar")
	if os.WriteFile(tarPath, b.Bytes(), 0o600) != nil {
		return false
	}
	_, err = run(ctx, "mkfs.ext4", "-q", "-F", "-d", tarPath, filepath.Join(dir, "p.ext4"), "4M")
	return err == nil
}
