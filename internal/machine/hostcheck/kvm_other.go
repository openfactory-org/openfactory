//go:build !linux

package hostcheck

import (
	"errors"
	"runtime"
)

func checkKVM() error {
	return errors.New("worker hosts need Linux with KVM; " + runtime.GOOS + " is not supported")
}

func uname() (string, string) { return runtime.GOOS, runtime.GOARCH }

func freeDisk(string) (int64, int64, error) { return 0, 0, errors.ErrUnsupported }
