package linuxnative

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type CatalogPage struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
	Coverage   any              `json:"coverage"`
}

func (s *Sender) Catalog(ctx context.Context, kind, thread, search, cursor string) (CatalogPage, error) {
	return s.CatalogWithLimit(ctx, kind, thread, search, cursor, 50)
}

// CatalogWithLimit forwards the requested page size. Native protocol and the
// one MiB response boundary remain authoritative; callers follow NextCursor.
func (s *Sender) CatalogWithLimit(ctx context.Context, kind, thread, search, cursor string, limit int) (CatalogPage, error) {
	if limit < 1 {
		return CatalogPage{}, &CatalogError{Code: "catalog_query_invalid"}
	}
	path := ""
	fields := []string{}
	switch kind {
	case "contacts":
		path = "/contacts"
		fields = []string{"id", "username", "displayName", "nickname", "remark", "alias", "relation"}
	case "groups":
		path = "/groups"
		fields = []string{"id", "roomId", "displayName", "name", "remark", "ownerId", "announcement", "muted", "myDisplayName", "isInGroup"}
	case "threads":
		path = "/threads"
		fields = []string{"id", "peerId", "kind", "displayName", "unreadCount"}
	case "messages":
		if !safeThreadID(thread) {
			return CatalogPage{}, errors.New("invalid thread")
		}
		path = "/threads/" + url.PathEscape(thread) + "/messages"
		fields = []string{"id", "threadId", "serverId", "senderId", "type", "direction", "body", "createdAt", "appMessageType", "media"}
	case "members":
		if !safeThreadID(thread) || !strings.HasSuffix(thread, "@chatroom") {
			return CatalogPage{}, &CatalogError{Code: "catalog_group_invalid"}
		}
		path = "/groups/" + url.PathEscape(thread) + "/members"
		fields = []string{"id", "username", "displayName", "roomDisplayName"}
	default:
		return CatalogPage{}, &CatalogError{Code: "catalog_kind_invalid"}
	}
	if len(search) > 256 || len(cursor) > 8192 {
		return CatalogPage{}, &CatalogError{Code: "catalog_query_invalid"}
	}
	query := url.Values{"limit": []string{strconv.Itoa(limit)}}
	if search != "" {
		query.Set("search", search)
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	raw, err := s.readCatalog(ctx, s.accountPath()+path+"?"+query.Encode(), cursor != "")
	if err != nil {
		return CatalogPage{}, err
	}
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"nextCursor"`
		Coverage   any              `json:"coverage"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Items == nil || len(page.Items) > limit {
		return CatalogPage{}, &CatalogError{Code: "catalog_response_invalid"}
	}
	out := CatalogPage{Items: []map[string]any{}, NextCursor: page.NextCursor, Coverage: page.Coverage}
	for _, item := range page.Items {
		if item["deleted"] == true {
			continue
		}
		row := map[string]any{}
		for _, field := range fields {
			if value, ok := item[field]; ok {
				row[field] = value
			}
		}
		if kind == "messages" && item["type"] == float64(49) {
			encoded, _ := json.Marshal(item)
			var m Message
			_ = json.Unmarshal(encoded, &m)
			if title := m.FileAttachmentName(); title != "" {
				row["file_name"] = title
				row["body"] = "[文件] " + title
				out.Items = append(out.Items, row)
				continue
			}
		}
		if kind == "messages" && row["type"] != float64(1) {
			if item["type"] == float64(34) {
				addVoiceMatchMetadata(row, item)
			}
			row["body"] = "[非文字消息，可通过附件功能处理]"
		}
		out.Items = append(out.Items, row)
	}
	return out, nil
}

func (s *Sender) readCatalog(ctx context.Context, path string, hasCursor bool) ([]byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		r, err := s.do(ctx, http.MethodGet, path, nil, "")
		if err != nil {
			if ctx.Err() != nil {
				code := "catalog_cancelled"
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					code = "catalog_timeout"
				}
				return nil, &CatalogError{Code: code}
			}
			if attempt == 0 {
				select {
				case <-ctx.Done():
					continue
				case <-time.After(200 * time.Millisecond):
				}
				continue
			}
			return nil, &CatalogError{Code: "catalog_network_unavailable"}
		}
		if r.StatusCode != 200 {
			r.Body.Close()
			code := "catalog_request_rejected"
			switch {
			case r.StatusCode == 401 || r.StatusCode == 403:
				code = "catalog_auth_denied"
			case r.StatusCode == 404:
				code = "catalog_not_found"
			case r.StatusCode == 400 && hasCursor:
				code = "catalog_cursor_invalid"
			case r.StatusCode >= 500:
				code = "catalog_upstream_unavailable"
			}
			return nil, &CatalogError{Code: code, HTTPStatus: r.StatusCode}
		}
		b, readErr := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
		r.Body.Close()
		if readErr != nil && attempt == 0 && ctx.Err() == nil {
			continue
		}
		if readErr != nil || len(b) > 1<<20 || !json.Valid(b) {
			return nil, &CatalogError{Code: "catalog_response_invalid"}
		}
		return b, nil
	}
	return nil, &CatalogError{Code: "catalog_network_unavailable"}
}
