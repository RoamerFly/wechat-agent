package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wechat-bot/internal/apphost"
)

const (
	defaultArchiveListSize  = 20
	maxArchiveListSize      = 50
	maxArchiveListItems     = 50
	maxArchiveMetadataBytes = 1024
)

// ArchiveListRequest is a read-only, host-selected page of the authenticated
// account's archive list. PageSize zero selects the documented default of 20.
// Explicit page sizes keep the provider's 1..50 boundary.
type ArchiveListRequest struct {
	Page     int
	PageSize int
	Status   string
}

// ArchiveCandidate contains only the metadata needed to let the owner compare
// a platform candidate with an already persisted publication task. It does not
// establish that a candidate belongs to a task.
type ArchiveCandidate struct {
	ResourceID string
	Title      string
	CreatedAt  *int64
}

// ArchiveList is a single actual provider page. NextPage is zero when this is
// the final page according to the returned total.
type ArchiveList struct {
	Page     int
	PageSize int
	Total    int
	NextPage int
	Items    []ArchiveCandidate
}

func (r ArchiveListRequest) normalized() (ArchiveListRequest, error) {
	if r.Page == 0 {
		r.Page = 1
	}
	if r.PageSize == 0 {
		r.PageSize = defaultArchiveListSize
	}
	if r.Page < 1 || r.PageSize < 1 || r.PageSize > maxArchiveListSize {
		return ArchiveListRequest{}, ErrOpenUploadInvalid
	}
	switch r.Status {
	case "", "all", "is_pubing", "pubed", "not_pubed":
		return r, nil
	default:
		return ArchiveListRequest{}, ErrOpenUploadInvalid
	}
}

// ListArchives reads one signed page from the official ARC_BASE endpoint. It
// deliberately has no retry or submission behaviour.
func (c *OpenUploadClient) ListArchives(ctx context.Context, request ArchiveListRequest) (ArchiveList, error) {
	request, err := request.normalized()
	if err != nil || c == nil || c.client == nil || c.now == nil || c.nonce == nil {
		return ArchiveList{}, ErrOpenUploadInvalid
	}
	query := make(url.Values)
	query.Set("pn", strconv.Itoa(request.Page))
	query.Set("ps", strconv.Itoa(request.PageSize))
	if request.Status != "" {
		query.Set("status", request.Status)
	}
	data, err := c.signedRequest(ctx, http.MethodGet, openArchiveListURL, query, nil)
	if err != nil {
		return ArchiveList{}, err
	}
	return decodeArchiveList(data, request)
}

func decodeArchiveList(data []byte, request ArchiveListRequest) (ArchiveList, error) {
	var response struct {
		List json.RawMessage `json:"list"`
		Page *struct {
			Page     int `json:"pn"`
			PageSize int `json:"ps"`
			Total    int `json:"total"`
		} `json:"page"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Page == nil || response.Page.Page != request.Page || response.Page.PageSize != request.PageSize || response.Page.Total < 0 {
		return ArchiveList{}, ErrOpenUploadResponse
	}
	if len(response.List) == 0 {
		return ArchiveList{}, ErrOpenUploadResponse
	}
	if string(response.List) == "null" {
		if response.Page.Total != 0 {
			return ArchiveList{}, ErrOpenUploadResponse
		}
		response.List = []byte("[]")
	}
	var items []struct {
		ResourceID string `json:"resource_id"`
		Title      string `json:"title"`
		CreatedAt  *int64 `json:"ctime"`
	}
	if err := json.Unmarshal(response.List, &items); err != nil || items == nil || len(items) > maxArchiveListItems || len(items) > request.PageSize {
		return ArchiveList{}, ErrOpenUploadResponse
	}
	result := ArchiveList{Page: response.Page.Page, PageSize: response.Page.PageSize, Total: response.Page.Total, Items: make([]ArchiveCandidate, 0, len(items))}
	lastPage := response.Page.Total / request.PageSize
	if response.Page.Total%request.PageSize != 0 {
		lastPage++
	}
	if request.Page < lastPage {
		result.NextPage = request.Page + 1
	}
	for _, item := range items {
		if !safeArchiveResourceID(item.ResourceID) || !safeArchiveMetadata(item.Title) || (item.CreatedAt != nil && *item.CreatedAt < 0) {
			return ArchiveList{}, ErrOpenUploadResponse
		}
		result.Items = append(result.Items, ArchiveCandidate{ResourceID: item.ResourceID, Title: item.Title, CreatedAt: item.CreatedAt})
	}
	return result, nil
}

func safeArchiveMetadata(value string) bool {
	return value != "" && len(value) <= maxArchiveMetadataBytes && !containsArchiveControl(value) && strings.TrimSpace(value) != ""
}

// listPublicationCandidates is deliberately separate from the posting
// transport interface. Existing test doubles that only model write stages do
// not accidentally gain account-list access. Production OpenUploadClient
// supplies the narrow read-only capability above.
func listPublicationCandidates(ctx context.Context, transport publicationTransport, page, pageSize int) (apphost.PublicationReconciliation, error) {
	reader, ok := transport.(interface {
		ListArchives(context.Context, ArchiveListRequest) (ArchiveList, error)
	})
	if !ok {
		return apphost.PublicationReconciliation{}, apphost.ErrPublicationReconciliationRead
	}
	result, err := reader.ListArchives(ctx, ArchiveListRequest{Page: page, PageSize: pageSize, Status: "all"})
	if err != nil {
		return apphost.PublicationReconciliation{}, apphost.ErrPublicationReconciliationRead
	}
	out := apphost.PublicationReconciliation{Page: result.Page, PageSize: result.PageSize, NextPage: result.NextPage, HasMore: result.NextPage != 0, Items: make([]apphost.PublicationCandidate, 0, len(result.Items))}
	for _, item := range result.Items {
		candidate := apphost.PublicationCandidate{BVID: item.ResourceID, Title: item.Title}
		if item.CreatedAt != nil && *item.CreatedAt > 0 {
			candidate.CreatedAt = time.Unix(*item.CreatedAt, 0).UTC().Format(time.RFC3339)
		}
		// A candidate's state is platform metadata, not evidence that this
		// particular task produced it. Keep Published unset until the owner
		// selects an exact BV and verifies it with the single-item endpoint.
		out.Items = append(out.Items, candidate)
	}
	return out, nil
}
