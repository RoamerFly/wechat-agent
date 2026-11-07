//go:build !windows

package stationbrowser

import (
	"context"
	"errors"
)

var (
	ErrDouyinLoginActive           = errors.New("抖音登录窗口已打开")
	ErrDouyinLoginCloseUnconfirmed = errors.New("抖音登录窗口退出未确认")
)

type LoginWindow struct{}

func DouyinLoginProfileLockPath(string) (string, error)             { return "", errCDPUnavailable }
func OpenDouyinLogin(context.Context, string) (*LoginWindow, error) { return nil, errCDPUnavailable }
func (*LoginWindow) Active() bool                                   { return false }
func (*LoginWindow) Close(context.Context) error                    { return nil }
