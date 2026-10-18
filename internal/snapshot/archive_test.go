package snapshot

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	base := t.TempDir()
	writeFile(t, filepath.Join(base, "a.txt"), "alpha", 0o644)
	writeFile(t, filepath.Join(base, "dir/sub/b.txt"), "beta", 0o644)
	writeFile(t, filepath.Join(base, "dir/run.sh"), "#!/bin/sh\n", 0o755)
	if err := os.Symlink("a.txt", filepath.Join(base, "dir/link")); err != nil {
		t.Fatal(err)
	}
	rels := []string{"a.txt", "dir/sub/b.txt", "dir/run.sh", "dir/link"}
	arc := filepath.Join(t.TempDir(), "x.tar.zst")
	n, err := PackFiles(arc, base, rels)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(arc); n <= 0 || st.Size() != n {
		t.Fatalf("returned size %d, file size %v", n, st)
	}
	dst := t.TempDir()
	if err := Unpack(arc, dst); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"a.txt": "alpha", "dir/sub/b.txt": "beta", "dir/run.sh": "#!/bin/sh\n"} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil || string(got) != want {
			t.Errorf("%s: got %q, err %v", rel, got, err)
		}
	}
	st, err := os.Stat(filepath.Join(dst, "dir/run.sh"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode: %v, %v", st, err)
	}
	target, err := os.Readlink(filepath.Join(dst, "dir/link"))
	if err != nil || target != "a.txt" {
		t.Errorf("link target %q, err %v", target, err)
	}
	// Unpacking again replaces files that exist.
	writeFile(t, filepath.Join(dst, "a.txt"), "changed", 0o644)
	if err := Unpack(arc, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(got) != "alpha" {
		t.Errorf("a.txt not replaced: %q", got)
	}
}

func TestPackEmpty(t *testing.T) {
	arc := filepath.Join(t.TempDir(), "empty.tar.zst")
	if _, err := PackFiles(arc, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(arc, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// handArchive writes an archive with the given entries.
func handArchive(t *testing.T, hdrs []tar.Header, body string) string {
	t.Helper()
	arc := filepath.Join(t.TempDir(), "hand.tar.zst")
	f, err := os.Create(arc)
	if err != nil {
		t.Fatal(err)
	}
	zw, err := zstd.NewWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(zw)
	for i := range hdrs {
		h := hdrs[i]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return arc
}

func TestUnpackRefusesTraversal(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	arc := handArchive(t, []tar.Header{{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644}}, "x")
	if err := Unpack(arc, dst); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(filepath.Join(root, "evil.txt")); !os.IsNotExist(err) {
		t.Fatalf("evil.txt must not exist: %v", err)
	}
}

func TestUnpackRefusesAbsolute(t *testing.T) {
	arc := handArchive(t, []tar.Header{{Name: "/tmp/evil.txt", Typeflag: tar.TypeReg, Mode: 0o644}}, "x")
	if err := Unpack(arc, t.TempDir()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestUnpackRefusesSymlinkParent(t *testing.T) {
	dst := t.TempDir()
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(dst, "link")); err != nil {
		t.Fatal(err)
	}
	arc := handArchive(t, []tar.Header{{Name: "link/x.txt", Typeflag: tar.TypeReg, Mode: 0o644}}, "x")
	if err := Unpack(arc, dst); err == nil {
		t.Fatal("expected an error")
	}
	entries, err := os.ReadDir(other)
	if err != nil || len(entries) != 0 {
		t.Fatalf("other directory must stay empty: %v, %v", entries, err)
	}
}

func TestUnpackRefusesSymlinkEntryOutside(t *testing.T) {
	root := t.TempDir()
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	arc := handArchive(t, []tar.Header{{Name: "../l", Typeflag: tar.TypeSymlink, Linkname: "x"}}, "")
	if err := Unpack(arc, dst); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Lstat(filepath.Join(root, "l")); !os.IsNotExist(err) {
		t.Fatalf("link must not exist: %v", err)
	}
}
