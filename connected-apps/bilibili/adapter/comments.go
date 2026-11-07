package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
)

const (
	commentsEndpoint       = "https://api.bilibili.com/x/v2/reply/main"
	defaultCommentPageSize = 10
	maxCommentPageSize     = 30
)

// commentCursor is stored only by the observer, scoped to the content and
// grant version. Keep the upstream page size fixed so a later page cannot
// silently skip or repeat comments when the caller changes limit.
type commentCursor struct {
	BVID     string
	AID      int64
	Next     int64
	Position int
	PageSize int
}

type commentResponse struct {
	Code *int `json:"code"`
	Data struct {
		Cursor struct {
			Next int64 `json:"next"`
			End  *bool `json:"is_end"`
		} `json:"cursor"`
		Replies json.RawMessage `json:"replies"`
	} `json:"data"`
}

type publicComment struct {
	ID      int64 `json:"rpid"`
	AID     int64 `json:"oid"`
	Type    int   `json:"type"`
	Root    int64 `json:"root"`
	Parent  int64 `json:"parent"`
	Created int64 `json:"ctime"`
	Content struct {
		Message string `json:"message"`
	} `json:"content"`
}

func (p *PublicApp) readComments(ctx context.Context, request apphost.ObservationRequest, revision uint64) (apphost.ObservationPage, error) {
	var page apphost.ObservationPage
	cursor, err := commentsCursor(request)
	if err != nil {
		return page, err
	}
	readCtx, authorize, closeScope := p.observationReadScope(ctx, request.ObservationAccess, revision)
	defer closeScope()
	if err := authorize(); err != nil {
		return page, err
	}
	if cursor.AID == 0 {
		data, err := p.fetch(readCtx, request.ContentID)
		if authErr := authorize(); authErr != nil {
			return page, authErr
		}
		if err != nil {
			return page, err
		}
		if data.AID <= 0 {
			return page, apphost.ErrObservationContentUnavailable
		}
		cursor.AID = data.AID
	}
	query := url.Values{
		"type": {"1"}, "oid": {strconv.FormatInt(cursor.AID, 10)}, "mode": {"3"},
		"next": {strconv.FormatInt(cursor.Next, 10)}, "ps": {strconv.Itoa(cursor.PageSize)},
	}
	var response commentResponse
	err = p.fetchJSON(readCtx, commentsEndpoint, query, &response)
	if authErr := authorize(); authErr != nil {
		return page, authErr
	}
	if err != nil {
		return page, err
	}
	return verifiedCommentPage(response, cursor)
}

func commentsCursor(request apphost.ObservationRequest) (commentCursor, error) {
	if request.Limit < 0 || request.Limit > maxCommentPageSize {
		return commentCursor{}, apphost.ErrObservationCommentPageSize
	}
	if request.Cursor == "" {
		size := request.Limit
		if size == 0 {
			size = defaultCommentPageSize
		}
		return commentCursor{BVID: request.ContentID, PageSize: size}, nil
	}
	var cursor commentCursor
	if json.Unmarshal([]byte(request.Cursor), &cursor) != nil || cursor.BVID != request.ContentID ||
		cursor.AID <= 0 || cursor.Next <= 0 || cursor.Position <= 0 || cursor.Position > math.MaxInt-maxCommentPageSize ||
		cursor.PageSize <= 0 || cursor.PageSize > maxCommentPageSize {
		return commentCursor{}, apphost.ErrObservationInvalid
	}
	if request.Limit != 0 && request.Limit != cursor.PageSize {
		return commentCursor{}, apphost.ErrObservationCommentPageSize
	}
	return cursor, nil
}

func verifiedCommentPage(response commentResponse, cursor commentCursor) (apphost.ObservationPage, error) {
	if err := publicResponseCode(response.Code); err != nil {
		return apphost.ObservationPage{}, err
	}
	var rows []publicComment
	if response.Data.Cursor.End == nil || json.Unmarshal(response.Data.Replies, &rows) != nil || len(rows) > cursor.PageSize {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	complete := *response.Data.Cursor.End
	if !complete && (response.Data.Cursor.Next <= cursor.Next || len(rows) == 0) {
		return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
	}
	page := apphost.ObservationPage{Complete: complete, Coverage: fmt.Sprintf("B 站公开一级评论 · 平台返回顺序 · 每页请求 %d 条", cursor.PageSize)}
	seen := make(map[int64]bool, len(rows))
	withoutText := 0
	for _, row := range rows {
		if row.AID != cursor.AID || row.Type != 1 || row.Root != 0 || row.Parent != 0 || row.ID <= 0 || seen[row.ID] ||
			row.Created <= 0 || row.Created > 253402300799 {
			return apphost.ObservationPage{}, apphost.ErrObservationUpstreamUnavailable
		}
		seen[row.ID] = true
		position := cursor.Position
		cursor.Position++
		if strings.TrimSpace(row.Content.Message) == "" {
			withoutText++
			continue
		}
		page.Chunks = append(page.Chunks, apphost.ObservationChunk{
			Position: position, Text: row.Content.Message,
			SourceID: strconv.FormatInt(row.ID, 10), PublishedAt: time.Unix(row.Created, 0).UTC().Format(time.RFC3339),
		})
	}
	if withoutText > 0 {
		page.Coverage += fmt.Sprintf(" · %d 条没有文字", withoutText)
	}
	if !complete {
		cursor.Next = response.Data.Cursor.Next
		raw, err := json.Marshal(cursor)
		if err != nil {
			return apphost.ObservationPage{}, err
		}
		page.NextCursor = string(raw)
	}
	return page, nil
}
