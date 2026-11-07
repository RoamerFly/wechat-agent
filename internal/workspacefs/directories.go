package workspacefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var ErrDirectoryLink = errors.New("managed directory cannot be a symbolic link")
var ErrDirectoryChanged = errors.New("managed directory changed while opening")

// EnsurePath walks from the filesystem root using pinned parent handles.
// It never creates a child through an existing ancestor symlink.
func EnsurePath(path string) (*os.Root, error) {
	return openPath(path, true)
}

// OpenPath pins an existing absolute directory without creating components.
func OpenPath(path string) (*os.Root, error) {
	return openPath(path, false)
}

func openPath(path string, create bool) (*os.Root, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("managed directory must be an absolute clean path")
	}
	base := filepath.VolumeName(path) + string(os.PathSeparator)
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return nil, err
	}
	return walkDirectory(root, rel, create)
}

// EnsureDirectory returns a separately owned root for a relative directory.
// Each component is checked and pinned before resolving its child; caller
// retains ownership of parent. Symlinks cannot redirect reads or creation.
func EnsureDirectory(parent *os.Root, path string) (*os.Root, error) {
	return walkDirectory(parent, path, true)
}

// OpenExistingDirectory pins an existing relative directory without following
// symbolic links or creating missing components.
func OpenExistingDirectory(parent *os.Root, path string) (*os.Root, error) {
	return walkDirectory(parent, path, false)
}

func walkDirectory(parent *os.Root, path string, create bool) (*os.Root, error) {
	if filepath.IsAbs(path) || filepath.Clean(path) != path || path == ".." || strings.HasPrefix(path, ".."+string(os.PathSeparator)) {
		return nil, errors.New("invalid managed relative directory")
	}
	root, err := parent.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if path == "." {
		return root, nil
	}
	for _, part := range strings.Split(path, string(os.PathSeparator)) {
		info, err := root.Lstat(part)
		if create && os.IsNotExist(err) {
			if err = root.Mkdir(part, 0700); err != nil && !os.IsExist(err) {
				root.Close()
				return nil, err
			}
			info, err = root.Lstat(part)
		}
		if err != nil {
			root.Close()
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			root.Close()
			return nil, ErrDirectoryLink
		}
		if !info.IsDir() {
			root.Close()
			return nil, kindError(part, true)
		}
		next, err := openChildDirectory(root, part, info)
		root.Close()
		if err != nil {
			return nil, err
		}
		root = next
	}
	return root, nil
}

func openChildDirectory(parent *os.Root, name string, expected os.FileInfo) (*os.Root, error) {
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	f, err := OpenDirectory(root, ".")
	if err != nil {
		root.Close()
		return nil, err
	}
	actual, err := f.Stat()
	f.Close()
	if err != nil {
		root.Close()
		return nil, err
	}
	if !os.SameFile(expected, actual) {
		root.Close()
		return nil, ErrDirectoryChanged
	}
	return root, nil
}
