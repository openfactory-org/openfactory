package vm

import (
	"bytes"
	"errors"
	"io"
	"os"
	"syscall"
)

// linkReadOnly makes src visible at dst inside a jail: a hard link, or a
// read-only bind mount when src is on another filesystem (Linux).
func linkReadOnly(src, dst string) error {
	err := os.Link(src, dst)
	if err == nil {
		return nil
	}
	var le *os.LinkError
	if errors.As(err, &le) && errors.Is(le.Err, syscall.EXDEV) {
		return bindReadOnly(src, dst)
	}
	return err
}

// CloneFile copies src to dst: a reflink when the filesystem supports it,
// else a sparse copy that skips blocks of zeros.
func CloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if reflink(out, in) == nil {
		return out.Close()
	}
	fi, err := in.Stat()
	if err != nil {
		_ = out.Close()
		return err
	}
	if err := sparseCopy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Truncate(fi.Size()); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func sparseCopy(out *os.File, in io.Reader) error {
	buf := make([]byte, 1<<20)
	zero := make([]byte, len(buf))
	var off int64
	for {
		n, err := io.ReadFull(in, buf)
		if n > 0 {
			if !bytes.Equal(buf[:n], zero[:n]) {
				if _, werr := out.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// processAlive reports whether pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
