//go:build !windows

package hostfile

import (
	"os"
	"syscall"
)

func Lock(path string) (func(), error) {
	return lock(path, syscall.LOCK_EX)
}

func TryLock(path string) (func(), error) {
	return lock(path, syscall.LOCK_EX|syscall.LOCK_NB)
}

func lock(path string, flags int) (func(), error) {
	if err := validateRegistryLockPath(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), flags); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
