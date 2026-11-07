package stationbrowser

import (
	"errors"
	"regexp"
)

// DouyinCommentsRequest describes one forward-only, read-only comment page.
// Cursor is the exact comment ID returned by the preceding page, never an
// opaque value copied from the web site.
type DouyinCommentsRequest struct {
	VideoID string
	Cursor  string
	Limit   int
}

// DouyinComment contains only rendered, public page fields.  AuthorID is a
// Douyin platform identity (normally sec_uid), not a WeChat identity.
type DouyinComment struct {
	ID        string
	AuthorID  string
	Author    string
	Text      string
	CreatedAt string // RFC3339 only; relative page labels are deliberately omitted.
}

// DouyinCommentPage is deliberately explicit about incomplete reads.  When
// Complete is false, NextCursor is the last verified comment ID and can be
// supplied to a subsequent request.
type DouyinCommentPage struct {
	Comments   []DouyinComment
	NextCursor string
	Complete   bool
}

var (
	ErrDouyinCommentsUnavailable   = errors.New("抖音评论暂时无法安全读取")
	ErrDouyinCommentsCursorExpired = errors.New("抖音评论续读位置已失效")
	ErrDouyinCommentsTooLarge      = errors.New("抖音评论请求超过单次安全读取范围")
)

const (
	defaultDouyinCommentsLimit = 20
	// maxDouyinCommentsBatch is a browser resource boundary, not a public
	// parameter ceiling. An explicit larger Limit is served in resumable
	// batches rather than silently rewritten or rejected.
	maxDouyinCommentsBatch = 200
)

var (
	douyinCommentID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	douyinAuthorID  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,256}$`)
)

func normalizedDouyinCommentsLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultDouyinCommentsLimit, nil
	}
	if limit < 0 {
		return 0, ErrDouyinCommentsTooLarge
	}
	return limit, nil
}
