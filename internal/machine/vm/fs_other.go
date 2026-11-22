//go:build !linux

package vm

import (
	"errors"
	"os"
)

// Worker hosts are Linux only. These stubs keep the package building for
// unit tests on other systems.

func bindReadOnly(string, string) error { return errors.New("bind mounts need Linux") }

func reflink(*os.File, *os.File) error { return errors.ErrUnsupported }

func unmountAll(string) {}
