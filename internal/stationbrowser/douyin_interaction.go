package stationbrowser

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

// DouyinInteractionRequest describes one narrow, user-authorized interaction
// with one fixed Douyin video.  It is deliberately not a general browser
// command interface.
type DouyinInteractionRequest struct {
	VideoID   string
	Kind      string // like or reply_comment
	CommentID string
	Text      string
	Desired   bool
	Submitted bool
	RemoteID  string
	// ExpectedCommentSHA256 is signed in by the host after reading the exact
	// parent comment. It is not model-provided text and prevents a delayed
	// reply from being sent to edited/replaced comment content.
	ExpectedCommentSHA256 string
	BeforeSubmit          func(context.Context) error
	expectedParentText    string
}

type DouyinInteractionResult struct {
	State     string // unknown, applied, failed, cancelled
	RemoteID  string
	Code      string
	Submitted bool
}

const (
	DouyinInteractionLike         = "like"
	DouyinInteractionReplyComment = "reply_comment"
	DouyinInteractionUnknown      = "unknown"
	DouyinInteractionApplied      = "applied"
	DouyinInteractionFailed       = "failed"
	DouyinInteractionCancelled    = "cancelled"
)

var (
	ErrDouyinInteractionUnavailable = errors.New("抖音互动暂时无法安全执行")
	ErrDouyinInteractionInput       = errors.New("抖音互动请求无效")
	ErrDouyinInteractionUnknown     = errors.New("抖音互动结果尚未确认")
	ErrDouyinInteractionRejected    = errors.New("抖音拒绝了这次互动")
)

var (
	douyinInteractionHash    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	douyinInteractionVideoID = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
)

func validDouyinInteraction(in DouyinInteractionRequest) bool {
	if !douyinInteractionVideoID.MatchString(in.VideoID) || (in.Kind != DouyinInteractionLike && in.Kind != DouyinInteractionReplyComment) {
		return false
	}
	if in.Kind == DouyinInteractionLike {
		return in.CommentID == "" && in.Text == "" && in.ExpectedCommentSHA256 == ""
	}
	if !douyinCommentID.MatchString(in.CommentID) || strings.TrimSpace(in.Text) == "" {
		return false
	}
	// Submitted requests are query-only; recovery must not require new authority.
	return in.Submitted || douyinInteractionHash.MatchString(in.ExpectedCommentSHA256)
}
