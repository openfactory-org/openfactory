package snapshot

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// PackFiles writes a zstd compressed tar archive to the file dst. It adds
// each path of relPaths, which is relative to baseDir. It supports regular
// files and symbolic links and skips other file types. It returns the size of
// dst in bytes.
func PackFiles(dst string, baseDir string, relPaths []string) (bytes int64, err error) {
	f, err := os.Create(dst)
	if err != nil {
		return 0, fmt.Errorf("snapshot: create archive: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("snapshot: close archive: %w", cerr)
		}
		if err == nil {
			if st, serr := os.Stat(dst); serr != nil {
				err = fmt.Errorf("snapshot: stat archive: %w", serr)
			} else {
				bytes = st.Size()
			}
		}
	}()
	zw, err := zstd.NewWriter(f)
	if err != nil {
		return 0, fmt.Errorf("snapshot: create zstd writer: %w", err)
	}
	tw := tar.NewWriter(zw)
	for _, rel := range relPaths {
		if err := addEntry(tw, baseDir, rel); err != nil {
			tw.Close()
			zw.Close()
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		zw.Close()
		return 0, fmt.Errorf("snapshot: finish tar: %w", err)
	}
	if err := zw.Close(); err != nil {
		return 0, fmt.Errorf("snapshot: finish zstd: %w", err)
	}
	return 0, nil
}

func addEntry(tw *tar.Writer, baseDir, rel string) error {
	full := filepath.Join(baseDir, rel)
	info, err := os.Lstat(full)
	if err != nil {
		return fmt.Errorf("snapshot: stat %s: %w", rel, err)
	}
	mode := info.Mode()
	var link string
	switch {
	case mode.IsRegular():
	case mode&fs.ModeSymlink != 0:
		link, err = os.Readlink(full)
		if err != nil {
			return fmt.Errorf("snapshot: read link %s: %w", rel, err)
		}
	default:
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return fmt.Errorf("snapshot: header for %s: %w", rel, err)
	}
	hdr.Name = filepath.ToSlash(rel)
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("snapshot: write header for %s: %w", rel, err)
	}
	if !mode.IsRegular() {
		return nil
	}
	in, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("snapshot: open %s: %w", rel, err)
	}
	defer in.Close()
	if _, err := io.Copy(tw, in); err != nil {
		return fmt.Errorf("snapshot: copy %s: %w", rel, err)
	}
	return nil
}

// Unpack reads an archive made by PackFiles and writes its entries under
// dstDir. It refuses an entry that leaves dstDir, an entry whose parent
// directory in dstDir is a symbolic link, and a link entry whose name leaves
// dstDir. It replaces files that exist.
func Unpack(src string, dstDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("snapshot: open archive: %w", err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("snapshot: create zstd reader: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("snapshot: read tar: %w", err)
		}
		if err := unpackEntry(tr, hdr, dstDir); err != nil {
			return err
		}
	}
}

func unpackEntry(tr *tar.Reader, hdr *tar.Header, dstDir string) error {
	name := path.Clean(strings.ReplaceAll(hdr.Name, "\\", "/"))
	if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("snapshot: entry %q leaves the destination", hdr.Name)
	}
	if name == "." {
		return nil
	}
	rel := filepath.FromSlash(name)
	target := filepath.Join(dstDir, rel)
	parent := filepath.Dir(rel)
	if err := checkParents(dstDir, parent); err != nil {
		return fmt.Errorf("snapshot: entry %q: %w", hdr.Name, err)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return mkdirAll(target)
	case tar.TypeReg:
		if err := mkdirAll(filepath.Dir(target)); err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("snapshot: replace %s: %w", name, err)
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("snapshot: create %s: %w", name, err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return fmt.Errorf("snapshot: write %s: %w", name, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("snapshot: close %s: %w", name, err)
		}
		if err := os.Chmod(target, hdr.FileInfo().Mode().Perm()); err != nil {
			return fmt.Errorf("snapshot: chmod %s: %w", name, err)
		}
		return nil
	case tar.TypeSymlink:
		if err := mkdirAll(filepath.Dir(target)); err != nil {
			return err
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("snapshot: replace %s: %w", name, err)
		}
		if err := os.Symlink(hdr.Linkname, target); err != nil {
			return fmt.Errorf("snapshot: link %s: %w", name, err)
		}
		return nil
	default:
		return nil
	}
}

func mkdirAll(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("snapshot: create directory: %w", err)
	}
	return nil
}

// checkParents returns an error if a component of the relative directory
// parent, below base, exists and is a symbolic link.
func checkParents(base, parent string) error {
	if parent == "." || parent == "" {
		return nil
	}
	cur := base
	for _, el := range strings.Split(parent, string(filepath.Separator)) {
		cur = filepath.Join(cur, el)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("parent %q is a symbolic link", el)
		}
	}
	return nil
}
