package stationbrowser

import "testing"

func TestDouyinInteractionRequestRejectsUnsafeNewReply(t *testing.T) {
	base := DouyinInteractionRequest{VideoID: "123", Kind: DouyinInteractionReplyComment, CommentID: "456", Text: "已读"}
	if validDouyinInteraction(base) {
		t.Fatal("new reply without host-signed parent hash was accepted")
	}
	base.ExpectedCommentSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !validDouyinInteraction(base) {
		t.Fatal("reply with exact parent hash was rejected")
	}
	base.Submitted, base.ExpectedCommentSHA256 = true, ""
	if !validDouyinInteraction(base) {
		t.Fatal("old submitted reply should remain queryable")
	}
}

func TestDouyinInteractionRequestRejectsAmbiguousKinds(t *testing.T) {
	if validDouyinInteraction(DouyinInteractionRequest{VideoID: "123", Kind: DouyinInteractionLike, CommentID: "456"}) {
		t.Fatal("like with comment target accepted")
	}
	if validDouyinInteraction(DouyinInteractionRequest{VideoID: "123", Kind: "reply", CommentID: "456", Text: "x"}) {
		t.Fatal("unknown action accepted")
	}
}
