package image

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ErrPull wraps every failure to get an image: the backend reports
// image_pull.
var ErrPull = errors.New("image pull failed")

// ErrDiskFull means the image did not fit on the disk.
var ErrDiskFull = errors.New("disk full")

// Image is a converted image.
type Image struct {
	Digest     string    `json:"digest"`
	Ref        string    `json:"ref"`
	Path       string    `json:"-"`
	Size       int64     `json:"size"`
	Env        []string  `json:"env,omitempty"`
	WorkingDir string    `json:"working_dir,omitempty"`
	User       string    `json:"user,omitempty"`
	Created    time.Time `json:"created"`
	LastUsed   time.Time `json:"last_used"`
}

// Store converts and caches images under Dir:
// images/sha256-<hex>/{rootfs.ext4,meta.json}, tools/<version>-<arch>/tools.ext4
// and scratch-templates/<gib>.ext4.
type Store struct {
	Dir string
	Run Run
	// TarInput: mkfs.ext4 takes the tar stream (no host extraction).
	TarInput bool
	// Chown keeps file owners when extracting (true when root).
	Chown bool
	Log   *slog.Logger
	// RemoteOptions are added to every registry call (tests).
	RemoteOptions []remote.Option

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (s *Store) run() Run {
	if s.Run == nil {
		return ExecRun
	}
	return s.Run
}

func (s *Store) log() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

func (s *Store) lock(key string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (s *Store) imagesDir() string { return filepath.Join(s.Dir, "images") }

func (s *Store) tmpDir() (string, error) {
	base := filepath.Join(s.imagesDir(), "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	d := filepath.Join(base, hex.EncodeToString(b))
	return d, os.Mkdir(d, 0o700)
}

// CleanTmp removes builds that a crashed agent left.
func (s *Store) CleanTmp() { _ = os.RemoveAll(filepath.Join(s.imagesDir(), "tmp")) }

// DigestDir returns the cache directory of a digest.
func (s *Store) DigestDir(digest string) string {
	return filepath.Join(s.imagesDir(), strings.Replace(digest, ":", "-", 1))
}

// Ensure returns the ext4 image of ref for arch. It pulls and converts the
// image when the cache has no disk for its manifest digest.
func (s *Store) Ensure(ctx context.Context, ref, arch string, auth authn.Authenticator) (Image, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return Image{}, fmt.Errorf("%w: %w", ErrPull, err)
	}
	opts := append([]remote.Option{
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: arch}),
		remote.WithContext(ctx),
	}, s.RemoteOptions...)
	if auth != nil {
		opts = append(opts, remote.WithAuth(auth))
	}
	img, err := remote.Image(r, opts...)
	if err != nil {
		return Image{}, fmt.Errorf("%w: %s: %w", ErrPull, ref, err)
	}
	d, err := img.Digest()
	if err != nil {
		return Image{}, fmt.Errorf("%w: %w", ErrPull, err)
	}
	digest := d.String()
	defer s.lock(digest)()
	if m, err := s.Meta(digest); err == nil {
		m.LastUsed = time.Now().UTC()
		_ = s.writeMeta(m)
		return m, nil
	}
	return s.convert(ctx, img, ref, digest)
}

// Meta reads the cached image of a digest.
func (s *Store) Meta(digest string) (Image, error) {
	dir := s.DigestDir(digest)
	var m Image
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	m.Path = filepath.Join(dir, "rootfs.ext4")
	if _, err := os.Stat(m.Path); err != nil {
		return m, err
	}
	return m, nil
}

func (s *Store) writeMeta(m Image) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(s.DigestDir(m.Digest), "meta.json")
	if err := os.WriteFile(p+".tmp", data, 0o644); err != nil { //nolint:gosec // G306: image metadata is not secret
		return err
	}
	return os.Rename(p+".tmp", p)
}

func (s *Store) convert(ctx context.Context, img v1.Image, ref, digest string) (Image, error) {
	cfg, err := img.ConfigFile()
	if err != nil {
		return Image{}, fmt.Errorf("%w: config: %w", ErrPull, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return Image{}, fmt.Errorf("%w: layers: %w", ErrPull, err)
	}
	tmp, err := s.tmpDir()
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(tmp)
	tarPath := filepath.Join(tmp, "image.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		return Image{}, err
	}
	st, err := Flatten(layers, f, s.log())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Image{}, wrapDisk(fmt.Errorf("%w: flatten: %w", ErrPull, err))
	}
	src := tarPath
	if !s.TarInput {
		root := filepath.Join(tmp, "root")
		tf, err := os.Open(tarPath)
		if err != nil {
			return Image{}, err
		}
		err = Extract(tf, root, s.Chown)
		tf.Close()
		if err != nil {
			return Image{}, wrapDisk(fmt.Errorf("%w: extract: %w", ErrPull, err))
		}
		_ = os.Remove(tarPath)
		src = root
	}
	out := filepath.Join(tmp, "rootfs.ext4")
	if _, err := s.run()(ctx, "mkfs.ext4", MkfsImageArgs("of-image", src, out, st)...); err != nil {
		return Image{}, wrapDisk(fmt.Errorf("%w: mkfs.ext4: %w", ErrPull, err))
	}
	if err := os.Chmod(out, 0o444); err != nil {
		return Image{}, err
	}
	final := s.DigestDir(digest)
	if err := os.MkdirAll(final, 0o755); err != nil {
		return Image{}, err
	}
	if err := os.Rename(out, filepath.Join(final, "rootfs.ext4")); err != nil {
		return Image{}, err
	}
	now := time.Now().UTC()
	m := Image{Digest: digest, Ref: ref, Size: Size(st), Env: cfg.Config.Env, WorkingDir: cfg.Config.WorkingDir, User: cfg.Config.User, Created: now, LastUsed: now}
	if err := s.writeMeta(m); err != nil {
		return Image{}, err
	}
	m.Path = filepath.Join(final, "rootfs.ext4")
	s.log().Info("image converted", "ref", ref, "digest", digest, "entries", st.Entries, "stripped", st.Stripped)
	return m, nil
}

func wrapDisk(err error) error {
	if errors.Is(err, syscall.ENOSPC) || strings.Contains(err.Error(), "No space left") {
		return fmt.Errorf("%w: %w", ErrDiskFull, err)
	}
	return err
}

// EnsureTools returns the tools disk of a runtime version: the static
// runtime binary at /openfactory, an empty /etc and the mount points. exe
// is the binary to put in it, normally the agent's own executable.
func (s *Store) EnsureTools(ctx context.Context, version, arch, exe string) (string, error) {
	dir := filepath.Join(s.Dir, "tools", version+"-"+arch)
	out := filepath.Join(dir, "tools.ext4")
	defer s.lock("tools:" + version + arch)()
	if _, err := os.Stat(out); err == nil {
		now := time.Now()
		_ = os.Chtimes(dir, now, now)
		return out, nil
	}
	tmp, err := s.tmpDir()
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	root := filepath.Join(tmp, "root")
	for _, d := range []string{"etc", "proc", "sys", "dev", "run", "mnt", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return "", err
		}
	}
	in, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer in.Close()
	bin, err := os.OpenFile(filepath.Join(root, "openfactory"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(bin, in)
	if cerr := bin.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	st := Stats{Entries: 16, Bytes: (n + 4095) &^ 4095}
	img := filepath.Join(tmp, "tools.ext4")
	if _, err := s.run()(ctx, "mkfs.ext4", MkfsImageArgs("of-tools", root, img, st)...); err != nil {
		return "", wrapDisk(err)
	}
	if err := os.Chmod(img, 0o444); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return out, os.Rename(img, out)
}

// EnsureScratch returns the empty scratch template of gib GiB.
func (s *Store) EnsureScratch(ctx context.Context, gib int) (string, error) {
	dir := filepath.Join(s.Dir, "scratch-templates")
	out := filepath.Join(dir, fmt.Sprintf("%d.ext4", gib))
	defer s.lock("scratch:" + out)()
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := out + ".tmp"
	if _, err := s.run()(ctx, "mkfs.ext4", MkfsScratchArgs(tmp, gib)...); err != nil {
		_ = os.Remove(tmp)
		return "", wrapDisk(err)
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return "", err
	}
	return out, os.Rename(tmp, out)
}

// Prune deletes least recently used images until images/ and tools/ use at
// most maxBytes. inUse protects the digests of live VMs.
func (s *Store) Prune(maxBytes int64, inUse func(digest string) bool) (freed int64, err error) {
	entries, err := os.ReadDir(s.imagesDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var imgs []Image
	var total int64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "sha256-") {
			continue
		}
		m, err := s.Meta(strings.Replace(e.Name(), "-", ":", 1))
		if err != nil {
			continue
		}
		if fi, err := os.Stat(m.Path); err == nil {
			m.Size = DiskUsage(fi)
		}
		total += m.Size
		imgs = append(imgs, m)
	}
	total += dirUsage(filepath.Join(s.Dir, "tools"))
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].LastUsed.Before(imgs[j].LastUsed) })
	for _, m := range imgs {
		if total <= maxBytes {
			break
		}
		if inUse != nil && inUse(m.Digest) {
			continue
		}
		if err := os.RemoveAll(s.DigestDir(m.Digest)); err != nil {
			return freed, err
		}
		s.log().Info("image pruned", "digest", m.Digest, "ref", m.Ref)
		total -= m.Size
		freed += m.Size
	}
	return freed, nil
}

func dirUsage(dir string) int64 {
	var n int64
	_ = filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += DiskUsage(fi)
		}
		return nil
	})
	return n
}
