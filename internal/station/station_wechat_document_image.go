package station

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

func (t *stationAttachmentTools) canReconstructImage(ctx context.Context) bool {
	return t.documents.imageAvailable != nil && t.documents.reconstructImage != nil && t.documents.imageAvailable(ctx)
}

func (t *stationAttachmentTools) prepareImageDocument(ctx context.Context, raw json.RawMessage) (string, error) {
	args := struct {
		Message       string                    `json:"message_id"`
		Base          string                    `json:"base_name"`
		Format        string                    `json:"format"`
		Hash          string                    `json:"expected_sha256"`
		Illustrations []stationIllustrationRect `json:"illustration_regions"`
	}{Base: "复刻文档", Format: "pdf"}
	if !t.allowed() || !t.replyAllowed(t.trace) || t.trace.TraceID == "" || t.trace.Sender == "" {
		return "", wechatapp.ErrAttachmentDenied
	}
	if strictLocalMemoryDecode(raw, &args) != nil || args.Message == "" || len(args.Message) > 8192 || args.Base == "" || len(args.Illustrations) > stationIllustrationLimit ||
		stationDocumentExtension(args.Base) != "" ||
		!validStationDocumentName(args.Base+".docx", ".docx") || !validStationPDFName(args.Base+".pdf") ||
		(args.Format != "docx" && args.Format != "pdf" && args.Format != "both") ||
		args.Hash != "" && !artifactID("sha-"+args.Hash, "sha-") {
		return "", mcptools.ArgumentError("$", "请提供图片原消息编号、有效文件名（不带扩展名）和 docx、pdf 或 both 格式")
	}
	if !t.canReconstructImage(ctx) || args.Format != "docx" && !t.canExportDocument(ctx) {
		return "", errors.New("本机图片文字识别、文档工坊或所选输出的运行环境未启用")
	}
	in, err := t.openDocumentInput(ctx, args.Message, args.Hash, 3, nil)
	if err != nil {
		return "", err
	}
	defer in.file.Close()
	if t.documents.illustrationsAvailable != nil && !t.documents.illustrationsAvailable(ctx) {
		if len(args.Illustrations) > 0 {
			return "", errors.New("保留插图需要文档工坊 0.1.4 或更高版本")
		}
		args.Illustrations = []stationIllustrationRect{}
	}
	sort.Slice(args.Illustrations, func(i, j int) bool {
		a, b := args.Illustrations[i], args.Illustrations[j]
		return a.Top < b.Top || a.Top == b.Top && a.Left < b.Left
	})
	in.illustrationRegions = args.Illustrations
	wordReply := t.documentReplyForInput(&in, args.Base+".docx")
	wordReply.IllustrationKey = localCheckpointKey("image-layout-v4", args.Illustrations)
	in.handoffKey = wordReply.id()
	word, wordFile, err := t.openDocumentReply(ctx, t.trace, in.handoffKey)
	if errors.Is(err, os.ErrNotExist) {
		var layout stationImageLayout
		layout, err = t.documents.reconstructImage(ctx, in, func(ctx context.Context, file *os.File, product stationDocumentProduct, layout stationImageLayout) error {
			layout.Layout = nil
			wordReply.Layout, wordReply.Product = &layout, product
			return t.saveDocumentReply(ctx, wordReply, file, in.check)
		})
		if err != nil {
			return "", err
		}
		if layout.NeedsClarification {
			if err := in.check(ctx); err != nil {
				return "", err
			}
			return localProcessJSON(map[string]any{"status": "needs_clarification", "message_id": args.Message,
				"source_sha256": in.record.SHA256, "layout": stationImageLayoutSummary(layout)})
		}
		word, wordFile, err = t.openDocumentReply(ctx, t.trace, in.handoffKey)
		if err != nil {
			return "", err
		}
	}
	if err != nil {
		return "", err
	}
	defer wordFile.Close()
	if word.Layout == nil || word.Layout.NeedsClarification {
		return "", errors.New("缺少可核对的 Word 版式记录")
	}
	return t.prepareEditableDelivery(ctx, in, word, wordFile, args.Format)
}

func (t *stationAttachmentTools) prepareEditableDelivery(ctx context.Context, in stationDocumentInput, word stationDocumentReply, wordFile *os.File, format string) (string, error) {
	files := make([]companion.GeneratedMedia, 0, 2)
	if format == "docx" || format == "both" {
		files = append(files, word.media())
	}
	result := map[string]any{"status": "prepared", "message_id": word.Source.Message, "source_sha256": word.OriginalSHA,
		"editable_document": map[string]any{"asset_id": word.id(), "sha256": word.Product.SHA256, "filename": word.Name},
		"layout":            stationImageLayoutSummary(*word.Layout)}
	if word.TitleHalfPoints != 0 {
		result["revised_from"] = word.ParentAsset
		result["title_font_size_pt"] = float64(word.TitleHalfPoints) / 2
	}
	if format != "docx" {
		pdfReply := word
		pdfReply.Name, pdfReply.ParentAsset, pdfReply.Layout = strings.TrimSuffix(word.Name, ".docx")+".pdf", word.id(), nil
		pdfReply.TitleHalfPoints = 0
		pdfInput := in
		pdfInput.record.ID, pdfInput.record.Bytes, pdfInput.record.SHA256, pdfInput.record.MIME =
			word.id(), word.Product.Bytes, word.Product.SHA256, localDOCXMIME
		pdfInput.file, pdfInput.extension, pdfInput.mergedTable = wordFile, ".docx", word.Layout.MergedCells > 0
		pdfInput.borderlessTable = word.Layout.BorderlessTables > 0
		pdf, err := t.preparePDFHandoff(ctx, pdfReply, pdfInput)
		if err != nil {
			return "", err
		}
		files = append(files, pdf.media())
		result["pdf"] = map[string]any{"sha256": pdf.Product.SHA256, "page_count": pdf.Product.Pages, "filename": pdf.Name}
	}
	if err := in.check(ctx); err != nil {
		return "", err
	}
	result["media"] = files
	return localProcessJSON(result)
}

func stationImageLayoutSummary(layout stationImageLayout) map[string]any {
	return map[string]any{"headings": layout.Headings, "paragraphs": layout.Paragraphs,
		"tables": layout.Tables, "merged_cells": layout.MergedCells, "borderless_tables": layout.BorderlessTables, "reading_columns": layout.ReadingColumns,
		"illustrations": layout.Illustrations, "image_width": layout.ImageWidth, "image_height": layout.ImageHeight,
		"needs_clarification": layout.NeedsClarification, "uncertain_locations": layout.Uncertain}
}
