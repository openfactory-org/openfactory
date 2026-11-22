//go:build !linux

package image

import "archive/tar"

// Worker hosts are Linux only; other systems skip device nodes.
func mknod(string, *tar.Header) error { return nil }
