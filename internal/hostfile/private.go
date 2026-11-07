// Package hostfile provides durable private host state primitives.
package hostfile

import (
	"errors"
	"os"
	"path/filepath"
)

// WritePrivate replaces a private record through a synchronized sibling file.
func WritePrivate(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".settings-*")
	if err != nil {
		return errors.New("无法创建私有配置文件")
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return errors.New("无法写入配置文件")
	}
	if os.Rename(file.Name(), path) != nil {
		return errors.New("无法保存配置文件")
	}
	return nil
}
