// Package stationauth loads the local owner's startup credential from the
// station's private data root.
package stationauth

import (
	"crypto/rand"
	"errors"
	"io"
	"os"
	"strings"

	"wechat-bot/internal/websession"
	"wechat-bot/internal/workspacefs"
)

const maxPasswordFile = 1024

// Password uses the established station filenames and precedence. Existing
// files are read in place, and an environment override is never persisted.
func Password(dir, explicit string) (string, error) {
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return "", errors.New("本机主人私有目录不可用")
	}
	defer root.Close()

	if password, found, err := readPassword(root, "admin-password"); found || err != nil {
		return password, err
	}
	if explicit != "" {
		if !websession.ValidPassword(explicit) {
			return "", errors.New("本机主人环境变量密码格式无效")
		}
		return explicit, nil
	}
	if password, found, err := readPassword(root, "admin-token"); found || err != nil {
		return password, err
	}
	return bootstrapPassword(root)
}

func readPassword(root *os.Root, name string) (string, bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	unavailable := errors.New("本机主人凭据文件不可用")
	if err != nil || !info.Mode().IsRegular() {
		return "", false, unavailable
	}
	file, err := workspacefs.OpenRegular(root, name)
	if err != nil {
		return "", false, unavailable
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", false, unavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPasswordFile+1))
	if err != nil {
		return "", false, unavailable
	}
	password := strings.TrimSpace(string(data))
	if len(data) > maxPasswordFile || !websession.ValidPassword(password) {
		return "", false, errors.New("本机主人凭据文件格式无效")
	}
	return password, true, nil
}

func bootstrapPassword(root *os.Root) (string, error) {
	const password = "admin"
	unavailable := errors.New("无法保存本机主人初始凭据")
	temp := ".owner-" + rand.Text()
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", unavailable
	}
	defer root.Remove(temp)
	_, writeErr := io.WriteString(file, password+"\n")
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return "", unavailable
	}
	// Another startup may already have created the credential while this
	// temporary file was prepared. Prefer the value already on disk.
	if existing, found, err := readPassword(root, "admin-token"); found || err != nil {
		return existing, err
	}
	if err := root.Rename(temp, "admin-token"); err != nil {
		return "", unavailable
	}
	return password, nil
}
