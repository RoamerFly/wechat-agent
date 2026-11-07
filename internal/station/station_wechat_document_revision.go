package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/workspacefs"
)

func (t *stationAttachmentTools) canReviseDocument(ctx context.Context) bool {
	return t.allowed() && t.replyAllowed(t.trace) && t.historyAllowed(ctx) && t.trace.Sender != "" &&
		t.documents.reviseAvailable != nil && t.documents.reviseTitle != nil && t.documents.reviseAvailable(ctx)
}

func (t *stationAttachmentTools) editableDocumentSource(ctx context.Context, r stationDocumentReply) (stationAttachmentAccess, error) {
	requestAt := r.RequestAt
	if requestAt == "" {
		requestAt = r.Source.Occurred
	}
	before, err := time.Parse(time.RFC3339Nano, requestAt)
	current, currentErr := time.Parse(time.RFC3339Nano, t.trace.MessageTime)
	if r.Source.Kind != "image" || r.extension() != ".docx" || r.Layout == nil || r.Layout.NeedsClarification ||
		r.Trace == t.trace.TraceID || err != nil || currentErr != nil || before.After(current) {
		return stationAttachmentAccess{}, wechatapp.ErrAttachmentDenied
	}
	return t.documentSourceAccess(ctx, t.trace, r, true)
}

type stationEditableDocument struct {
	AssetID   string  `json:"asset_id"`
	Filename  string  `json:"filename"`
	SHA256    string  `json:"sha256"`
	MessageID string  `json:"source_message_id"`
	At        string  `json:"requested_at"`
	Previous  string  `json:"revised_from,omitempty"`
	TitleSize float64 `json:"title_font_size_pt,omitempty"`
	at        time.Time
}

func (t *stationAttachmentTools) listEditableDocuments(ctx context.Context, after, name string, limit int) (string, error) {
	type page struct {
		Items []stationEditableDocument `json:"items"`
		After string                    `json:"next_after,omitempty"`
	}
	out := page{Items: []stationEditableDocument{}}
	root, err := workspacefs.OpenPath(filepath.Join(t.dir, "document-replies"))
	if errors.Is(err, os.ErrNotExist) {
		return localProcessJSON(out)
	}
	if err != nil {
		return "", err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return "", err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return "", err
	}
	var matches []stationEditableDocument
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !entry.IsDir() || !artifactID(entry.Name(), stationDOCXPrefix) {
			continue
		}
		r, err := t.readDocumentReply(entry.Name())
		if err != nil || !strings.Contains(strings.ToLower(r.Name), name) {
			continue
		}
		if _, err := t.editableDocumentSource(ctx, r); err != nil {
			continue
		}
		requestAt := r.RequestAt
		if requestAt == "" {
			requestAt = r.Source.Occurred
		}
		at, _ := time.Parse(time.RFC3339Nano, requestAt)
		matches = append(matches, stationEditableDocument{AssetID: entry.Name(), Filename: r.Name, SHA256: r.Product.SHA256,
			MessageID: r.Source.Message, At: requestAt, Previous: r.ParentAsset, TitleSize: float64(r.TitleHalfPoints) / 2, at: at})
	}
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].at.Equal(matches[j].at) {
			return matches[i].at.After(matches[j].at)
		}
		return matches[i].AssetID < matches[j].AssetID
	})
	if after != "" {
		i := 0
		for i < len(matches) && matches[i].AssetID != after {
			i++
		}
		if i == len(matches) {
			return "", mcptools.ArgumentError("$.after", "续页位置已失效，请从第一页查询")
		}
		matches = matches[i+1:]
	}
	if len(matches) > limit {
		out.After = matches[limit-1].AssetID
		matches = matches[:limit]
	}
	if len(matches) > 0 {
		out.Items = matches
	}
	if !t.historyAllowed(ctx) || !t.allowed() || !t.replyAllowed(t.trace) {
		return "", wechatapp.ErrAttachmentDenied
	}
	return localProcessJSON(out)
}

func (t *stationAttachmentTools) documentRevision(ctx context.Context, raw json.RawMessage) (string, error) {
	args := struct {
		Action string  `json:"action"`
		ID     string  `json:"asset_id"`
		SHA    string  `json:"expected_sha256"`
		Size   float64 `json:"font_size_pt"`
		Format string  `json:"format"`
		Name   string  `json:"filename_contains"`
		After  string  `json:"after"`
		Limit  *int    `json:"limit"`
	}{}
	if !t.canReviseDocument(ctx) {
		return "", wechatapp.ErrAttachmentDenied
	}
	if strictLocalMemoryDecode(raw, &args) != nil {
		return "", mcptools.ArgumentError("$", "文档修订参数无效")
	}
	if args.Action == "list" {
		if args.ID != "" || args.SHA != "" || args.Size != 0 || args.Format != "" || len(args.Name) > 256 ||
			args.After != "" && !artifactID(args.After, stationDOCXPrefix) || args.Limit != nil && (*args.Limit < 1 || *args.Limit > 100) {
			return "", mcptools.ArgumentError("$", "可以按文件名搜索及续页，每页 1–100 件")
		}
		limit := 10
		if args.Limit != nil {
			limit = *args.Limit
		}
		return t.listEditableDocuments(ctx, args.After, strings.ToLower(strings.TrimSpace(args.Name)), limit)
	}
	if args.Format == "" {
		args.Format = "pdf"
	}
	half := int(args.Size * 2)
	if args.Action != "resize_title" || args.Name != "" || args.After != "" || args.Limit != nil || !artifactID(args.ID, stationDOCXPrefix) ||
		!artifactID("sha-"+args.SHA, "sha-") || half < 16 || half > 96 || float64(half) != args.Size*2 ||
		(args.Format != "docx" && args.Format != "pdf" && args.Format != "both") {
		return "", mcptools.ArgumentError("$", "请提供原稿编号、原稿摘要、8–48 磅且以 0.5 为步长的字号，以及交付格式")
	}
	if args.Format != "docx" && !t.canExportDocument(ctx) {
		return "", errors.New("本机 PDF 导出环境未启用")
	}
	prior, err := t.readDocumentReply(args.ID)
	if err != nil {
		return "", wechatapp.ErrAttachmentDenied
	}
	check := func(ctx context.Context) error {
		access, err := t.editableDocumentSource(ctx, prior)
		if err != nil {
			return err
		}
		return access.confirmObservationSource(ctx, prior.Source, prior.Epoch)
	}
	if err := check(ctx); err != nil {
		return "", err
	}
	if args.SHA != prior.Product.SHA256 {
		return "", errors.New("Word 原稿摘要已变化，请重新查找版本")
	}
	old, err := t.openDocumentProduct(ctx, t.trace, prior)
	if err != nil {
		return "", err
	}
	defer old.Close()
	word := prior
	word.Trace, word.RequestAt, word.HistoryGrant = t.trace.TraceID, t.trace.MessageTime, t.reads[t.trace.Thread]
	word.TitleHalfPoints, word.ParentAsset = half, prior.id()
	in := stationDocumentInput{record: stationAttachmentRecord{ID: prior.id(), Source: prior.Source, Bytes: prior.Product.Bytes,
		MIME: localDOCXMIME, SHA256: prior.Product.SHA256}, file: old, extension: ".docx", trace: t.trace.TraceID,
		epoch: prior.Epoch, requester: t.trace.Sender, check: check, handoffKey: word.id(), illustrationRegions: prior.Layout.Illustrations}
	if prior.Reference.EventID != "" {
		in.versions = map[string]int{prior.Reference.EventID: prior.Reference.Revision}
	}
	current, revised, err := t.openDocumentReply(ctx, t.trace, word.id())
	if errors.Is(err, os.ErrNotExist) {
		err = t.documents.reviseTitle(ctx, in, half, func(ctx context.Context, file *os.File, product stationDocumentProduct) error {
			word.Product = product
			return t.saveDocumentReply(ctx, word, file, check)
		})
		if err != nil {
			return "", err
		}
		current, revised, err = t.openDocumentReply(ctx, t.trace, word.id())
	}
	if err != nil {
		return "", err
	}
	defer revised.Close()
	return t.prepareEditableDelivery(ctx, in, current, revised, args.Format)
}
