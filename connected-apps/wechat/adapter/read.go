package adapter

import (
	"context"
	"encoding/json"
	"errors"

	"wechat-bot/internal/linuxnative"
)

// CatalogReader is an account-bound host service, without its login material.
type CatalogReader interface {
	Catalog(context.Context, string, string, string, string) (linuxnative.CatalogPage, error)
}

var (
	ErrReadDenied      = errors.New("只有开启跨会话权限的可信主人可读取其他已允许会话")
	ErrReadUnavailable = errors.New("后台不支持会话读取")
)

type CatalogRead struct {
	Page   linuxnative.CatalogPage
	Kind   string
	Thread string
}

// ReadCatalog owns the WeChat tool protocol. The host authorizer is live,
// checked again after reading so a revoked read cannot return a late page.
// Only the caller can register a successfully returned page as task evidence.
func ReadCatalog(ctx context.Context, native CatalogReader, currentThread string, args json.RawMessage, authorize func(string) bool) (CatalogRead, error) {
	var in struct {
		Kind   string `json:"kind"`
		Cursor string `json:"cursor"`
		Thread string `json:"thread_id"`
	}
	if json.Unmarshal(args, &in) != nil || (in.Kind != "messages" && in.Kind != "members") {
		return CatalogRead{}, &linuxnative.CatalogError{Code: "catalog_kind_invalid"}
	}
	if in.Thread == "" {
		in.Thread = currentThread
	}
	if authorize == nil || !authorize(in.Thread) {
		return CatalogRead{}, ErrReadDenied
	}
	if native == nil {
		return CatalogRead{}, ErrReadUnavailable
	}
	if err := ctx.Err(); err != nil {
		return CatalogRead{}, err
	}
	page, err := native.Catalog(ctx, in.Kind, in.Thread, "", in.Cursor)
	if err != nil {
		return CatalogRead{}, err
	}
	if err := ctx.Err(); err != nil {
		return CatalogRead{}, err
	}
	if !authorize(in.Thread) {
		return CatalogRead{}, ErrReadDenied
	}
	if in.Kind == "messages" {
		for _, row := range page.Items {
			if thread, _ := row["threadId"].(string); thread != "" && thread != in.Thread {
				return CatalogRead{}, &linuxnative.CatalogError{Code: "catalog_response_invalid"}
			}
		}
	}
	return CatalogRead{Page: page, Kind: in.Kind, Thread: in.Thread}, nil
}
