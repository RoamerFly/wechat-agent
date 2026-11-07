package hostfile

import (
	"errors"
	"os"
)

// validateRegistryLockPath keeps the synchronization sidecar pinned beside the
// private registry rather than following a junction or symlink supplied by a
// different directory. The file itself contains no data; its identity matters
// because every process must lock the same object.
func validateRegistryLockPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("鱼身份锁文件无效")
	}
	return nil
}
