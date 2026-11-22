package image

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
	pax              map[string]string
}

func layer(t *testing.T, entries ...entry) v1.Layer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Linkname: e.link, PAXRecords: e.pax, ModTime: time.Unix(1, 0)}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	data := b.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func readTar(t *testing.T, data []byte) map[string]*tar.Header {
	t.Helper()
	out := map[string]*tar.Header{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		out[h.Name] = h
	}
	return out
}

func TestFlattenWhiteoutsAndOpaque(t *testing.T) {
	l1 := layer(t,
		entry{name: "etc/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "etc/a", body: "a1"},
		entry{name: "etc/b", body: "b1"},
		entry{name: "opt/", typ: tar.TypeDir, mode: 0o755},
		entry{name: "opt/old/x", body: "x"},
		entry{name: "opt/keep", body: "k"},
		entry{name: "bin/su", body: "su", mode: 0o4755},
		entry{name: "bin/ping", body: "p", pax: map[string]string{capabilityXattr: "\x01"}},
	)
	l2 := layer(t,
		entry{name: "etc/.wh.b"},
		entry{name: "opt/.wh..wh..opq"},
		entry{name: "opt/new", body: "n"},
		entry{name: "etc/a", body: "a2"},
	)
	l3 := layer(t,
		entry{name: "etc/c", body: "c3"},
		entry{name: "etc/link", typ: tar.TypeLink, link: "etc/a"},
		entry{name: "etc/dead", typ: tar.TypeLink, link: "etc/b"},
	)
	var out bytes.Buffer
	st, err := Flatten([]v1.Layer{l1, l2, l3}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := readTar(t, out.Bytes())
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	want := []string{"bin/ping", "bin/su", "etc/", "etc/a", "etc/c", "etc/link", "opt/", "opt/new"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries %v, want %v", names, want)
	}
	if got["bin/su"].Mode&04000 != 0 {
		t.Error("setuid kept")
	}
	if _, ok := got["bin/ping"].PAXRecords[capabilityXattr]; ok {
		t.Error("capability kept")
	}
	if st.Stripped != 2 {
		t.Errorf("stripped %d", st.Stripped)
	}
	// The upper etc/a wins.
	tr := tar.NewReader(bytes.NewReader(out.Bytes()))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "etc/a" {
			b, _ := io.ReadAll(tr)
			if string(b) != "a2" {
				t.Errorf("etc/a = %q", b)
			}
		}
	}
	// The hard link comes after its target.
	if names := tarOrder(out.Bytes()); indexOf(names, "etc/link") < indexOf(names, "etc/a") {
		t.Error("hard link before its target")
	}
}

func tarOrder(data []byte) []string {
	var out []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err != nil {
			return out
		}
		out = append(out, h.Name)
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestFlattenRejectsEscape(t *testing.T) {
	l := layer(t, entry{name: "../evil", body: "x"})
	if _, err := Flatten([]v1.Layer{l}, io.Discard, nil); err == nil {
		t.Fatal("want an error for ..")
	}
}

func TestExtractSafe(t *testing.T) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "dir/f", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.WriteHeader(&tar.Header{Name: "dir/h", Typeflag: tar.TypeLink, Linkname: "dir/f"})
	_ = tw.WriteHeader(&tar.Header{Name: "esc", Typeflag: tar.TypeSymlink, Linkname: "/etc"})
	_ = tw.Close()
	root := filepath.Join(t.TempDir(), "root")
	if err := Extract(bytes.NewReader(b.Bytes()), root, false); err != nil {
		t.Fatal(err)
	}
	if d, _ := os.ReadFile(filepath.Join(root, "dir/h")); string(d) != "hi" {
		t.Fatalf("hard link %q", d)
	}
	// A later entry under the symlink must not follow it.
	var b2 bytes.Buffer
	tw = tar.NewWriter(&b2)
	_ = tw.WriteHeader(&tar.Header{Name: "esc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	if err := Extract(bytes.NewReader(b2.Bytes()), root, false); err == nil {
		t.Fatal("extract followed a symlink")
	}
}

func TestSizes(t *testing.T) {
	st := Stats{Entries: 1000, Bytes: 100 << 20}
	if Size(st) != (115<<20)+(64<<20) || Inodes(st) != 2224 {
		t.Fatalf("size %d inodes %d", Size(st), Inodes(st))
	}
	args := strings.Join(MkfsImageArgs("of-image", "/src", "/out.ext4", st), " ")
	if args != "-q -F -L of-image -m 0 -O ^has_journal -E lazy_itable_init=0,lazy_journal_init=0,root_owner=0:0 -N 2224 -d /src /out.ext4 183296K" {
		t.Fatalf("mkfs args %s", args)
	}
}

// fakeMkfs records calls and writes the output file.
type fakeMkfs struct {
	mu    sync.Mutex
	calls [][]string
}

func (f *fakeMkfs) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	// The output is the argument before the size (the last argument).
	out := args[len(args)-2]
	return "", os.WriteFile(out, []byte("ext4"), 0o644)
}

func pushImage(t *testing.T) (string, v1.Hash) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	img, err := mutate.AppendLayers(empty.Image, layer(t, entry{name: "etc/os-release", body: "ID=test"}))
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Config(img, v1.Config{Env: []string{"PATH=/usr/bin:/bin"}, WorkingDir: "/workspace", User: "1001"})
	if err != nil {
		t.Fatal(err)
	}
	cf, _ := img.ConfigFile()
	cf.OS, cf.Architecture = "linux", "amd64"
	img, _ = mutate.ConfigFile(img, cf)
	ref := u.Host + "/test/sandbox:1"
	r, _ := name.ParseReference(ref)
	if err := remote.Write(r, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return ref, d
}

func TestEnsureConvertsAndCaches(t *testing.T) {
	ref, digest := pushImage(t)
	fm := &fakeMkfs{}
	s := &Store{Dir: t.TempDir(), Run: fm.run}
	img, err := s.Ensure(context.Background(), ref, "amd64", nil)
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != digest.String() || img.WorkingDir != "/workspace" || img.User != "1001" || img.Env[0] != "PATH=/usr/bin:/bin" {
		t.Fatalf("image %+v", img)
	}
	if fi, err := os.Stat(img.Path); err != nil || fi.Mode().Perm() != 0o444 {
		t.Fatalf("rootfs %v %v", fi, err)
	}
	if len(fm.calls) != 1 || !strings.Contains(strings.Join(fm.calls[0], " "), "-L of-image") {
		t.Fatalf("mkfs calls %v", fm.calls)
	}
	// Extraction fallback: -d names a directory with the file.
	if !strings.HasSuffix(fm.calls[0][len(fm.calls[0])-3], "/root") {
		t.Fatalf("mkfs source %v", fm.calls[0])
	}
	// Cached: no second conversion.
	if _, err := s.Ensure(context.Background(), ref, "amd64", nil); err != nil || len(fm.calls) != 1 {
		t.Fatalf("second ensure: %v, %d calls", err, len(fm.calls))
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "images", "tmp")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(s.Dir, "images", "tmp"))
		if len(entries) != 0 {
			t.Fatal("tmp builds left")
		}
	}
	// A missing image is a pull error.
	if _, err := s.Ensure(context.Background(), strings.Replace(ref, "sandbox:1", "none:1", 1), "amd64", nil); err == nil || !strings.Contains(err.Error(), ErrPull.Error()) {
		t.Fatalf("missing image: %v", err)
	}
	if list, err := s.List(); err != nil || len(list) != 1 || list[0].Digest != digest.String() {
		t.Fatalf("list %+v %v", list, err)
	}
	if s.locks.Len() != 0 {
		t.Fatalf("%d locks kept after Ensure", s.locks.Len())
	}
	// Prune keeps an image in use and removes it when it is free.
	if freed, _ := s.Prune(0, func(string) bool { return true }); freed != 0 {
		t.Fatal("pruned an image in use")
	}
	// Prune skips a digest that Ensure holds.
	unlock := s.lock(digest.String())
	if freed, _ := s.Prune(0, nil); freed != 0 {
		t.Fatal("pruned an image that Ensure holds")
	}
	unlock()
	if _, err := s.Prune(0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Meta(digest.String()); err == nil {
		t.Fatal("image not pruned")
	}
}

func TestEnsureTarInput(t *testing.T) {
	ref, _ := pushImage(t)
	fm := &fakeMkfs{}
	s := &Store{Dir: t.TempDir(), Run: fm.run, TarInput: true}
	if _, err := s.Ensure(context.Background(), ref, "amd64", nil); err != nil {
		t.Fatal(err)
	}
	if src := fm.calls[0][len(fm.calls[0])-3]; !strings.HasSuffix(src, "image.tar") {
		t.Fatalf("mkfs source %s", src)
	}
}

func TestToolsAndScratch(t *testing.T) {
	fm := &fakeMkfs{}
	s := &Store{Dir: t.TempDir(), Run: fm.run}
	exe := filepath.Join(t.TempDir(), "openfactory")
	_ = os.WriteFile(exe, []byte("binary"), 0o755)
	p, err := s.EnsureTools(context.Background(), "1.2.3", "amd64", exe)
	if err != nil || !strings.HasSuffix(p, "tools/1.2.3-amd64/tools.ext4") {
		t.Fatalf("tools %q %v", p, err)
	}
	if _, err := s.EnsureTools(context.Background(), "1.2.3", "amd64", exe); err != nil || len(fm.calls) != 1 {
		t.Fatal("tools disk not cached")
	}
	sp, err := s.EnsureScratch(context.Background(), 40)
	if err != nil || !strings.HasSuffix(sp, "scratch-templates/40.ext4") {
		t.Fatalf("scratch %q %v", sp, err)
	}
	if got := strings.Join(fm.calls[1][1:], " "); !strings.Contains(got, "-L of-scratch -m 0 -E lazy_itable_init=1,nodiscard") || !strings.HasSuffix(got, " 40G") {
		t.Fatalf("scratch args %s", got)
	}
	if free, total, err := FreeBytes(s.Dir); err != nil || free <= 0 || total < free {
		t.Fatalf("free %d total %d %v", free, total, err)
	}
}

func TestKeyedMutex(t *testing.T) {
	var k KeyedMutex
	unlock := k.Lock("a")
	if _, ok := k.TryLock("a"); ok {
		t.Fatal("TryLock got a held key")
	}
	u2, ok := k.TryLock("b")
	if !ok {
		t.Fatal("TryLock of a free key")
	}
	got := make(chan struct{})
	go func() {
		defer close(got)
		k.Lock("a")()
	}()
	select {
	case <-got:
		t.Fatal("Lock got a held key")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()
	<-got
	u2()
	if k.Len() != 0 {
		t.Fatalf("%d keys kept after unlock", k.Len())
	}
}
