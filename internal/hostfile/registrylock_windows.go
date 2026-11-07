//go:build windows

package hostfile

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	registryLockFileEx   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	registryUnlockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

const registryLockExclusive = 0x00000002

// Lock serializes independent Go processes which point to
// the same private registry. Closing the handle always releases LockFileEx,
// including when a process exits unexpectedly.
func Lock(path string) (func(), error) {
	return lock(path, registryLockExclusive)
}

// TryLock fails immediately if another process holds the same file range.
func TryLock(path string) (func(), error) {
	const failImmediately = 0x00000001
	return lock(path, registryLockExclusive|failImmediately)
}

func lock(path string, flags uint32) (func(), error) {
	h, err := openPinnedRegistryLock(path)
	if err != nil {
		return nil, err
	}
	var overlapped syscall.Overlapped
	ok, _, callErr := registryLockFileEx.Call(
		uintptr(h), uintptr(flags), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)),
	)
	if ok == 0 {
		_ = syscall.CloseHandle(h)
		if callErr != syscall.Errno(0) {
			return nil, callErr
		}
		return nil, errors.New("LockFileEx failed")
	}
	return func() {
		_, _, _ = registryUnlockFileEx.Call(uintptr(h), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
		_ = syscall.CloseHandle(h)
	}, nil
}

// openPinnedRegistryLock opens the sidecar itself, rather than the target of a
// possible reparse point. Handles intentionally allow read/write sharing so a
// second process can wait in LockFileEx, but deny FILE_SHARE_DELETE so the
// locked name cannot be unlinked and replaced with a different lock object.
func openPinnedRegistryLock(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	attrs := uint32(syscall.FILE_ATTRIBUTE_NORMAL | syscall.FILE_FLAG_BACKUP_SEMANTICS | syscall.FILE_FLAG_OPEN_REPARSE_POINT)
	share := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE) // deliberately excludes FILE_SHARE_DELETE
	for attempts := 0; attempts < 4; attempts++ {
		h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, share, nil, syscall.OPEN_EXISTING, attrs, 0)
		if err == syscall.ERROR_FILE_NOT_FOUND {
			// CREATE_NEW closes the race: a concurrent creator wins instead of
			// making this process follow a link that appeared after OPEN_EXISTING.
			h, err = syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, share, nil, syscall.CREATE_NEW, attrs, 0)
			if err == syscall.ERROR_FILE_EXISTS || err == syscall.ERROR_ALREADY_EXISTS {
				continue
			}
		}
		if err != nil {
			return syscall.InvalidHandle, err
		}
		var info syscall.ByHandleFileInformation
		if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
			_ = syscall.CloseHandle(h)
			return syscall.InvalidHandle, err
		}
		if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
			_ = syscall.CloseHandle(h)
			return syscall.InvalidHandle, errors.New("鱼身份锁文件无效")
		}
		return h, nil
	}
	return syscall.InvalidHandle, errors.New("鱼身份锁文件创建竞争未完成")
}
