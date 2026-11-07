// Package packagecheckout provides the filesystem safety boundary shared by
// immutable-package checkouts.  Callers still own authorization and the
// identity check immediately before publishing a staged checkout.
package packagecheckout

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/workspacefs"
)

// ExistingDirectory reports whether name is an existing, non-linked directory
// within root. It verifies that the directory was not replaced while opening
// it, so a caller never publishes into a junction substituted concurrently.
func ExistingDirectory(root *os.Root, name string) (bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("plugin_checkout_destination_invalid")
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return false, errors.New("plugin_checkout_destination_invalid")
	}
	defer child.Close()
	opened, err := workspacefs.OpenDirectory(child, ".")
	if err != nil {
		return false, errors.New("plugin_checkout_destination_invalid")
	}
	actual, statErr := opened.Stat()
	closeErr := opened.Close()
	if statErr != nil || closeErr != nil || !os.SameFile(info, actual) {
		return false, errors.New("plugin_checkout_destination_changed")
	}
	return true, nil
}

// CopyCheckedFile copies one declared package file to a private stage. It
// rejects traversal and links, honors cancellation, enforces the package byte
// budget, and verifies the expected SHA-256 before making it publishable.
func CopyCheckedFile(ctx context.Context, source, stage *os.Root, name, want string, total *int64) error {
	return CopyCheckedFileWithBudget(ctx, source, stage, name, want, total, pluginhost.MaxExpandedBytes)
}

// CopyCheckedFileWithBudget applies the same verified copy boundary to an
// explicitly sized checkout, such as an owner's selected project directory.
func CopyCheckedFileWithBudget(ctx context.Context, source, stage *os.Root, name, want string, total *int64, maxBytes int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if total == nil || maxBytes <= 0 || maxBytes == math.MaxInt64 || *total < 0 || *total > maxBytes {
		return errors.New("plugin_checkout_source_limit")
	}
	if !SafePath(name) {
		return errors.New("plugin_checkout_source_path_invalid")
	}
	input, err := workspacefs.OpenRegular(source, filepath.FromSlash(name))
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || info.Size() < 0 || info.Size() > maxBytes-*total {
		return errors.New("plugin_checkout_source_limit")
	}
	wantSize := info.Size()
	remaining := maxBytes - *total
	if parent := filepath.Dir(filepath.FromSlash(name)); parent != "." {
		directory, e := workspacefs.EnsureDirectory(stage, parent)
		if e != nil {
			return e
		}
		if e = directory.Close(); e != nil {
			return e
		}
	}
	output, err := stage.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	copied, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(contextReader{ctx: ctx, r: input}, remaining+1))
	if err == nil && copied > remaining {
		err = errors.New("plugin_checkout_source_limit")
	}
	if err == nil && copied != wantSize {
		err = errors.New("plugin_checkout_source_changed")
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = output.Sync()
	}
	closeErr := output.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if hex.EncodeToString(hash.Sum(nil)) != want {
		return errors.New("plugin_checkout_source_changed")
	}
	*total += info.Size()
	return nil
}

// SafePath accepts one clean slash-separated, relative package path.
func SafePath(name string) bool {
	return fs.ValidPath(name) && path.Clean(name) == name && !strings.ContainsAny(name, `\\:`+"\x00")
}

// NewStage creates a private unpublished directory under root. The caller
// owns closing it and removing the directory after an unsuccessful publish.
func NewStage(root *os.Root) (string, *os.Root, error) {
	for range 8 {
		var nonce [12]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", nil, err
		}
		name := ".checkout-" + hex.EncodeToString(nonce[:])
		if err := root.Mkdir(name, 0700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", nil, err
		}
		stage, err := workspacefs.EnsureDirectory(root, name)
		if err != nil {
			_ = root.RemoveAll(name)
			return "", nil, err
		}
		return name, stage, nil
	}
	return "", nil, errors.New("plugin_checkout_stage_collision")
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
