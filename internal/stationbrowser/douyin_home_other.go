//go:build !windows

package stationbrowser

import (
	"context"
	"errors"
)

var (
	ErrDouyinHomeUnavailable  = errors.New("抖音主页正文暂时无法安全读取")
	ErrDouyinHomeTooLarge     = errors.New("抖音主页正文超过安全读取大小")
	ErrDouyinWebLoginRequired = errors.New("抖音网页登录状态不可用")
	ErrDouyinWebTextSize      = ErrDouyinHomeTooLarge
)

type DouyinWebLink struct{ VideoID, Text string }
type DouyinWebPage struct {
	Title, Text string
	Links       []DouyinWebLink
}

func ReadDouyinHome(context.Context, string, func() error) (DouyinWebPage, error) {
	return DouyinWebPage{}, ErrDouyinHomeUnavailable
}
func DouyinVideoURL(string) (string, error) { return "", ErrDouyinHomeUnavailable }
func ReadDouyinVideo(context.Context, string, string, func() error) (DouyinWebPage, error) {
	return DouyinWebPage{}, ErrDouyinHomeUnavailable
}
