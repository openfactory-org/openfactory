// Package image turns OCI sandbox images into read-only ext4 disks, cached
// by manifest digest, and builds the tools disk and the scratch templates.
package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Whiteout markers of the OCI layer format.
const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// capabilityXattr is the PAX record of file capabilities. The image policy
// is "no setuid binaries and no file capabilities".
const capabilityXattr = "SCHILY.xattr.security.capability"

// Stats describes a flattened image.
type Stats struct {
	Entries int
	// Bytes is the sum of file sizes, each rounded up to 4 KiB.
	Bytes int64
	// Stripped counts setuid, setgid and capability changes.
	Stripped int
}

// cleanPath normalizes a tar entry name to a relative path without "./"
// and without a trailing "/". It returns "" for the root and an error for a
// name that leaves the root.
func cleanPath(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		name = strings.TrimLeft(name, "/")
	}
	p := path.Clean("/" + name)
	if p == "/" {
		return "", nil
	}
	rel := strings.TrimPrefix(p, "/")
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("tar entry %q leaves the root", name)
		}
	}
	return rel, nil
}

// underAny reports whether p or one of its parents is in set.
func underAny(p string, set map[string]bool) bool {
	for q := p; q != "" && q != "."; q = path.Dir(q) {
		if set[q] {
			return true
		}
		if !strings.Contains(q, "/") {
			break
		}
	}
	return false
}

// underOpaque reports whether p is strictly inside a directory of set.
func underOpaque(p string, set map[string]bool) bool {
	for q := path.Dir(p); q != "." && q != "/" && q != ""; q = path.Dir(q) {
		if set[q] {
			return true
		}
		if !strings.Contains(q, "/") {
			break
		}
	}
	return false
}

// Flatten writes the merged file system of layers (lowest first) as one tar
// stream. It reads the layers from top to bottom, applies whiteouts and
// opaque directories, clears setuid and setgid bits and drops file
// capabilities. Hard links point at paths that appear in the stream.
func Flatten(layers []v1.Layer, w io.Writer, log *slog.Logger) (Stats, error) {
	if log == nil {
		log = slog.Default()
	}
	var st Stats
	tw := tar.NewWriter(w)
	seen := map[string]bool{}
	whited := map[string]bool{}
	opaque := map[string]bool{}
	var links []*tar.Header
	for i := len(layers) - 1; i >= 0; i-- {
		rc, err := layers[i].Uncompressed()
		if err != nil {
			return st, fmt.Errorf("layer %d: %w", i, err)
		}
		layerOpaque := map[string]bool{}
		tr := tar.NewReader(rc)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				rc.Close()
				return st, fmt.Errorf("layer %d: %w", i, err)
			}
			p, err := cleanPath(h.Name)
			if err != nil {
				rc.Close()
				return st, err
			}
			if p == "" {
				continue
			}
			dir, base := path.Split(p)
			dir = strings.TrimSuffix(dir, "/")
			if base == opaqueMarker {
				if dir != "" {
					layerOpaque[dir] = true
				}
				continue
			}
			if name, ok := strings.CutPrefix(base, whiteoutPrefix); ok {
				whited[path.Join(dir, name)] = true
				continue
			}
			if seen[p] || underAny(p, whited) || underOpaque(p, opaque) {
				continue
			}
			seen[p] = true
			h.Name = p
			if h.Typeflag == tar.TypeDir {
				h.Name = p + "/"
			}
			if h.Mode&04000 != 0 || h.Mode&02000 != 0 {
				log.Info("image: cleared setuid/setgid", "path", p)
				h.Mode &^= 06000
				st.Stripped++
			}
			if _, ok := h.PAXRecords[capabilityXattr]; ok {
				log.Info("image: dropped file capabilities", "path", p)
				delete(h.PAXRecords, capabilityXattr)
				st.Stripped++
			}
			if h.Xattrs != nil { //nolint:staticcheck // old archives use Xattrs
				delete(h.Xattrs, "security.capability") //nolint:staticcheck // old archives use Xattrs
			}
			if h.Typeflag == tar.TypeLink {
				lp, err := cleanPath(h.Linkname)
				if err != nil {
					rc.Close()
					return st, err
				}
				h.Linkname = lp
				links = append(links, h)
				continue
			}
			st.Entries++
			st.Bytes += (h.Size + 4095) &^ 4095
			if err := tw.WriteHeader(h); err != nil {
				rc.Close()
				return st, err
			}
			if h.Typeflag == tar.TypeReg {
				if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // G110: layers come from the configured registry; ext4 size bounds them
					rc.Close()
					return st, err
				}
			}
		}
		rc.Close()
		for d := range layerOpaque {
			opaque[d] = true
		}
	}
	// Hard links last: a link of a higher layer can name a file that a lower layer holds.
	for _, h := range links {
		if !seen[h.Linkname] {
			continue
		}
		st.Entries++
		if err := tw.WriteHeader(h); err != nil {
			return st, err
		}
	}
	return st, tw.Close()
}
