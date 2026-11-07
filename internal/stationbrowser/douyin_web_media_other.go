//go:build !windows

package stationbrowser

import "context"

func ReadDouyinWebMedia(context.Context, string, string, func() error) (DouyinWebMedia, error) {
	return DouyinWebMedia{}, ErrBrowserUnavailable
}
