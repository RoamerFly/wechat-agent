//go:build !windows

package main

import (
	"wechat-bot/internal/admin"
	"wechat-bot/internal/devicebroker"
)

func newManager(dir, stateDir string, native admin.Native) (*admin.Manager, error) {
	return admin.NewManagerWithDeviceIO(dir, stateDir, native, devicebroker.Host{})
}
