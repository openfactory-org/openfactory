package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Extract unpacks a flattened tar stream into root. It is the fallback for
// a mkfs.ext4 without tar input. It refuses names that leave root, never
// follows a symlink of the image, creates device nodes only for char and
// block entries, and makes hard links after all files. Ownership is kept
// when chown is set (the agent runs as root).
func Extract(r io.Reader, root string, chown bool) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	type dirTime struct {
		path string
		t    time.Time
	}
	var dirs []dirTime
	var links []*tar.Header
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		rel, err := cleanPath(h.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := safeParent(root, dst); err != nil {
			return err
		}
		mode := os.FileMode(h.Mode & 0o777) //nolint:gosec // G115: tar modes fit
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			if err := os.Chmod(dst, mode); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{dst, h.ModTime})
		case tar.TypeReg:
			_ = os.Remove(dst)
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // G110: bounded by the image size
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Chmod(dst, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(dst)
			if err := os.Symlink(h.Linkname, dst); err != nil {
				return err
			}
		case tar.TypeLink:
			links = append(links, h)
			continue
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			if err := mknod(dst, h); err != nil {
				return err
			}
		default:
			continue
		}
		if chown {
			_ = os.Lchown(dst, h.Uid, h.Gid)
		}
		if h.Typeflag != tar.TypeSymlink && h.Typeflag != tar.TypeDir {
			_ = os.Chtimes(dst, h.ModTime, h.ModTime)
		}
	}
	for _, h := range links {
		rel, _ := cleanPath(h.Name)
		target, err := cleanPath(h.Linkname)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		src := filepath.Join(root, filepath.FromSlash(target))
		if err := safeParent(root, dst); err != nil {
			return err
		}
		if err := safeParent(root, src); err != nil {
			return err
		}
		if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		_ = os.Remove(dst)
		if err := os.Link(src, dst); err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chtimes(dirs[i].path, dirs[i].t, dirs[i].t)
	}
	return nil
}

// safeParent checks that no parent of p below root is a symlink, so a
// write never follows a link of the image out of root.
func safeParent(root, p string) error {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path %q leaves the root", p)
	}
	cur := root
	parts := strings.Split(filepath.Dir(rel), string(filepath.Separator))
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(cur, 0o755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("path %q goes through %q, which is not a directory", p, cur)
		}
	}
	return nil
}
