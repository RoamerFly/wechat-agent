//go:build !windows

package stationbrowser

import "context"

// ReadDouyinComments is intentionally unavailable outside the Windows native
// application. Keeping the stub allows callers to compile without promising a
// browser integration on unsupported platforms.
func ReadDouyinComments(context.Context, string, DouyinCommentsRequest, func() error) (DouyinCommentPage, error) {
	return DouyinCommentPage{}, ErrDouyinCommentsUnavailable
}
