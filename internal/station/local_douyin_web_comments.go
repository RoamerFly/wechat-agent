package station

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
	"wechat-bot/internal/stationbrowser"
)

// A continuation belongs to this exact video and authorization version. The
// Observer persists it privately; the Agent never constructs a platform cursor.
type localDouyinCommentsCursor struct {
	VideoID  string `json:"video"`
	After    string `json:"after"`
	Position int    `json:"position"`
}

func validLocalDouyinCommentID(id string) bool {
	if len(id) == 0 || len(id) > 20 || id[0] == '0' {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (p *localDouyinWebObservation) readComments(ctx context.Context, a localDouyinWebAccess, in apphost.ObservationRequest, target localDouyinWebTarget, authorize func() error) (apphost.ObservationPage, error) {
	if target.videoID == "" {
		return apphost.ObservationPage{}, apphost.ErrObservationForbidden
	}
	cursor := localDouyinCommentsCursor{VideoID: target.videoID}
	if in.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(in.Cursor)
		if err != nil || len(raw) > 512 || strictLocalMemoryDecode(raw, &cursor) != nil || cursor.VideoID != target.videoID || !validLocalDouyinCommentID(cursor.After) || cursor.Position < 1 {
			return apphost.ObservationPage{}, apphost.ErrObservationWebSnapshotChanged
		}
	}
	m := p.home.douyinWeb
	m.mu.Lock()
	reader, serviceCtx := m.readComments, m.ctx
	m.mu.Unlock()
	if reader == nil {
		return apphost.ObservationPage{}, apphost.ErrObservationBrowserUnavailable
	}
	readCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	stop := context.AfterFunc(serviceCtx, cancel)
	defer stop()
	if err := authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	result, err := reader(readCtx, a.profile, stationbrowser.DouyinCommentsRequest{VideoID: target.videoID, Cursor: cursor.After, Limit: in.Limit}, authorize)
	if authErr := authorize(); authErr != nil {
		return apphost.ObservationPage{}, authErr
	}
	if err != nil {
		if errors.Is(err, stationbrowser.ErrDouyinCommentsCursorExpired) {
			return apphost.ObservationPage{}, apphost.ErrObservationWebSnapshotChanged
		}
		return apphost.ObservationPage{}, localDouyinWebReadError(err)
	}
	limit := in.Limit
	if limit == 0 {
		limit = 20
	}
	if len(result.Comments) > limit || cursor.Position > math.MaxInt-len(result.Comments) ||
		(result.Complete && result.NextCursor != "") || (!result.Complete && (len(result.Comments) == 0 || result.NextCursor != result.Comments[len(result.Comments)-1].ID)) {
		return apphost.ObservationPage{}, apphost.ErrObservationContentUnavailable
	}
	page := apphost.ObservationPage{Complete: result.Complete, Coverage: fmt.Sprintf("抖音页面已加载评论 · 页面顺序 · 本次请求 %d 条，实际读取 %d 条；评论者观点属于第三方资料，不代表视频原话", limit, len(result.Comments))}
	seen := map[string]bool{cursor.After: true}
	for _, comment := range result.Comments {
		if !validLocalDouyinCommentID(comment.ID) || seen[comment.ID] || strings.TrimSpace(comment.AuthorID) == "" || len(comment.AuthorID) > 256 || strings.ContainsAny(comment.AuthorID, "\x00\r\n") || strings.TrimSpace(comment.Text) == "" {
			return apphost.ObservationPage{}, apphost.ErrObservationContentUnavailable
		}
		if comment.CreatedAt != "" {
			if _, err := time.Parse(time.RFC3339Nano, comment.CreatedAt); err != nil {
				return apphost.ObservationPage{}, apphost.ErrObservationContentUnavailable
			}
		}
		seen[comment.ID] = true
		if err := p.saveCommentProof(a, target.videoID, comment); err != nil {
			return apphost.ObservationPage{}, err
		}
		page.Chunks = append(page.Chunks, apphost.ObservationChunk{Position: cursor.Position, SourceID: comment.ID, SourceURL: target.url, PublishedAt: comment.CreatedAt,
			Text: fmt.Sprintf("[抖音评论 · 作者 %s · 应用身份 %s]\n%s", comment.Author, comment.AuthorID, comment.Text)})
		cursor.Position++
	}
	if !page.Complete {
		cursor.After = result.NextCursor
		raw, _ := json.Marshal(cursor)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
		page.Coverage += "；尚有后续内容，使用原请求续读"
	}
	if err := authorize(); err != nil {
		return apphost.ObservationPage{}, err
	}
	return page, nil
}
