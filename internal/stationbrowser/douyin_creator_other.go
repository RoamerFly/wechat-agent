//go:build !windows

package stationbrowser

import "context"

func OpenDouyinCreatorLogin(context.Context, string) (*LoginWindow, error) {
	return nil, ErrBrowserUnavailable
}

func ContinueDouyinPublication(context.Context, string, CreatorPublicationRequest, func() error) (CreatorPublicationResult, error) {
	return CreatorPublicationResult{}, ErrBrowserUnavailable
}
func QueryDouyinPublication(context.Context, string, string, func() error) (CreatorPublicationResult, error) {
	return CreatorPublicationResult{}, ErrBrowserUnavailable
}

func FindDouyinPublication(context.Context, string, CreatorPublicationQuery, func() error) (CreatorPublicationResult, error) {
	return CreatorPublicationResult{}, ErrBrowserUnavailable
}
