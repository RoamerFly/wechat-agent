package main

import "wechat-bot/internal/admin"

func newManager(dir, stateDir string, native admin.Native) (*admin.Manager, error) {
	return admin.NewManagerWithRetiredDeviceControl(dir, stateDir, native)
}
