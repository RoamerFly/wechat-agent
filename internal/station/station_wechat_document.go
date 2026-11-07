package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
)

const (
	stationDocumentLimit = 128 << 20
	stationOCRImageLimit = 32 << 20
)

type stationDocumentTools struct {
	available              func(context.Context, string) bool
	inspect                func(context.Context, stationDocumentInput) (string, error)
	exportAvailable        func(context.Context) bool
	exportPDF              func(context.Context, stationDocumentInput, string, func(context.Context, *os.File, stationDocumentProduct) error) (stationDocumentProduct, error)
	imageAvailable         func(context.Context) bool
	illustrationsAvailable func(context.Context) bool
	reconstructImage       func(context.Context, stationDocumentInput, func(context.Context, *os.File, stationDocumentProduct, stationImageLayout) error) (stationImageLayout, error)
	reviseAvailable        func(context.Context) bool
	reviseTitle            func(context.Context, stationDocumentInput, int, func(context.Context, *os.File, stationDocumentProduct) error) error
}

type stationDocumentInput struct {
	record              stationAttachmentRecord
	file                *os.File
	illustrationRegions []stationIllustrationRect
	mergedTable         bool
	borderlessTable     bool
	extension, epoch    string
	requester, trace    string
	handoffKey          string
	versions            map[string]int
	sourceRef           companion.SourceRef
	history             bool
	page, offset, limit int
	ocrMode             string
	ocrDPI              int
	check               func(context.Context) error
}

func stationDocumentExtension(name string) string {
	switch ext := strings.ToLower(filepath.Ext(name)); ext {
	case ".docx", ".pdf":
		return ext
	default:
		return ""
	}
}

func (t *stationAttachmentTools) documentRefs(messageType int) []*companion.SourceRef {
	var refs []*companion.SourceRef
	for _, ref := range []*companion.SourceRef{t.trace.Source, t.trace.QuotedSource} {
		if ref != nil && ref.MessageType == messageType {
			refs = append(refs, ref)
		}
	}
	for _, event := range t.trace.UnreadMessages {
		if ref := event.Source; ref != nil && ref.MessageType == messageType && ref.MessageID == event.MessageID &&
			ref.Sender == t.trace.Sender && event.Sender == t.trace.Sender {
			refs = append(refs, ref)
		}
	}
	return refs
}

func (t *stationAttachmentTools) documentAccess(ctx context.Context, message string, messageType int) (stationAttachmentAccess, error) {
	for _, ref := range t.documentRefs(messageType) {
		if ref.MessageID == message {
			request := t.trace
			request.TraceID, request.Source = message, ref
			access, err := t.attachmentAccess(request, message, t.allowed)
			// The worker verified this exact quote against the native message.
			// It may predate the local memory index.
			access.history = t.trace.QuotedSource != nil && ref == t.trace.QuotedSource
			return access, err
		}
	}
	m, visible, err := t.historyMessage(ctx, message)
	requestAt, requestErr := time.Parse(time.RFC3339Nano, t.trace.MessageTime)
	fileAt, fileErr := time.Parse(time.RFC3339Nano, m.CreatedAt)
	if err != nil || !visible || requestErr != nil || fileErr != nil || fileAt.After(requestAt) ||
		m.Direction != "incoming" || m.SenderID != t.trace.Sender || m.Type != messageType ||
		messageType == 49 && stationDocumentExtension(m.FileAttachmentName()) == "" {
		return stationAttachmentAccess{}, wechatapp.ErrAttachmentDenied
	}
	ref := &companion.SourceRef{Account: m.AccountID, Thread: m.ThreadID, MessageID: m.ID, Sender: m.SenderID,
		ServerID: m.ServerID, OccurredAt: m.CreatedAt, MessageType: m.Type, BodyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(m.Body)))}
	return stationAttachmentAccess{native: t.native, dir: t.dir, thread: m.ThreadID, message: m.ID,
		grant: t.grants[m.ThreadID], store: t.store, ref: ref, history: true,
		allowed: func() bool { return t.allowed() && t.scopeAllowed(t.trace, t.reads) }}, nil
}

func (t *stationAttachmentTools) inspectDocument(ctx context.Context, raw json.RawMessage) (string, error) {
	args := struct {
		Message string `json:"message_id"`
		Page    *int   `json:"page"`
		Offset  int    `json:"text_offset"`
		Limit   *int   `json:"text_limit"`
		Hash    string `json:"expected_sha256"`
		OCRMode string `json:"ocr_mode"`
		OCRDPI  *int   `json:"ocr_dpi"`
	}{OCRMode: "auto"}
	if !t.allowed() || t.documents.available == nil || t.documents.inspect == nil {
		return "", wechatapp.ErrAttachmentDenied
	}
	if strictLocalMemoryDecode(raw, &args) != nil || args.Message == "" || len(args.Message) > 8192 || args.Offset < 0 ||
		args.OCRMode != "auto" && args.OCRMode != "off" && args.OCRMode != "always" ||
		args.OCRDPI != nil && (*args.OCRDPI < 96 || *args.OCRDPI > 300 || args.OCRMode == "off") ||
		args.Page != nil && *args.Page < 1 || args.Limit != nil && (*args.Limit < 1 || *args.Limit > 100000) ||
		args.Hash != "" && !artifactID("sha-"+args.Hash, "sha-") {
		return "", mcptools.ArgumentError("$", "需要原消息编号、有效页码和正文范围。OCR 支持 auto、off 或 always，识别分辨率为 96–300 DPI")
	}
	in, err := t.openDocumentInput(ctx, args.Message, args.Hash, 49, func(ext string) error {
		if ext == ".docx" && args.Page != nil {
			return mcptools.ArgumentError("$.page", "DOCX 请按 text_offset 续读正文")
		}
		if ext == ".docx" && (args.OCRMode != "auto" || args.OCRDPI != nil) {
			return mcptools.ArgumentError("$.ocr_mode", "OCR 模式和分辨率只用于 PDF 页")
		}
		if !t.documents.available(ctx, ext) {
			return errors.New("当前文档格式的本地文档工坊或运行环境未启用")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	defer in.file.Close()
	in.offset, in.limit = args.Offset, 4000
	if in.extension == ".pdf" {
		in.ocrMode, in.ocrDPI = args.OCRMode, 220
		if args.OCRDPI != nil {
			in.ocrDPI = *args.OCRDPI
		}
		in.page = 1
		if args.Page != nil {
			in.page = *args.Page
		}
	}
	if args.Limit != nil {
		in.limit = *args.Limit
	}
	return t.documents.inspect(ctx, in)
}

func (t *stationAttachmentTools) openDocumentInput(ctx context.Context, message, expectedHash string, messageType int, format func(string) error) (stationDocumentInput, error) {
	var in stationDocumentInput
	access, err := t.documentAccess(ctx, message, messageType)
	if err != nil {
		return in, err
	}
	access.maxBytes = stationDocumentLimit
	if messageType == 3 {
		access.maxBytes = stationOCRImageLimit
	}
	epoch, err := t.store.RecallEpoch(ctx)
	if err != nil {
		return in, wechatapp.ErrAttachmentDenied
	}
	source, err := access.source(ctx, t.trace.Thread, message)
	if err != nil {
		return in, err
	}
	sourceInput := companion.Input{Channel: "wechat", Account: source.Account, Thread: source.Thread, Sender: source.Sender,
		TraceID: source.Message, MessageTime: source.Occurred, Source: access.ref}
	var versions map[string]int
	if stored, ok := t.store.SourceForInput(sourceInput); ok {
		access.ref = stored
		versions = map[string]int{stored.EventID: stored.Revision}
	} else if !access.history || access.ref.EventID != "" || !t.store.LegacyHistoryAllowed(source.Account, source.Thread) {
		return in, wechatapp.ErrAttachmentDenied
	}
	ext := ""
	if messageType == 3 {
		if source.Kind != "image" {
			return in, wechatapp.ErrAttachmentDenied
		}
	} else {
		ext = stationDocumentExtension(source.Filename)
		if source.Kind != "file" || ext == "" {
			return in, mcptools.ArgumentError("$.message_id", "此工具读取已关联的 Word（.docx）或 PDF 附件")
		}
		if err := format(ext); err != nil {
			return in, err
		}
	}
	check := func(ctx context.Context) error { return access.confirmObservationSource(ctx, source, epoch) }
	if err := check(ctx); err != nil {
		return in, err
	}
	record, file, err := access.open(ctx, source)
	if errors.Is(err, os.ErrNotExist) {
		if err := access.DownloadAttachment(ctx, source.Thread, source.Message); err != nil {
			return in, err
		}
		record, file, err = access.open(ctx, source)
	}
	if err != nil {
		return in, err
	}
	if expectedHash != "" && record.SHA256 != expectedHash {
		err = errors.New("文档原件摘要已变化，请重新核对原消息")
	} else if record.Bytes > access.maxBytes {
		err = fmt.Errorf("原件超过文档工坊的 %d MiB 输入上限", access.maxBytes>>20)
	} else if messageType == 3 {
		ext, record.MIME, err = stationDocumentImageFormat(file, record.Bytes)
	} else {
		err = verifyStationDocument(file, record.Bytes, ext)
	}
	if err == nil {
		err = check(ctx)
	}
	if err != nil {
		file.Close()
		return in, err
	}
	historyGrant := access.history && (t.trace.QuotedSource == nil || t.trace.QuotedSource.MessageID != message)
	return stationDocumentInput{record: record, file: file, extension: ext, epoch: epoch, requester: t.trace.Sender, trace: t.trace.TraceID,
		versions: versions, sourceRef: *access.ref, history: historyGrant, check: check}, nil
}

func stationDocumentImageFormat(file *os.File, size int64) (string, string, error) {
	config, format, err := image.DecodeConfig(io.NewSectionReader(file, 0, size))
	if err != nil || config.Width < 1 || config.Height < 1 || config.Width > 8000 || config.Height > 8000 ||
		int64(config.Width)*int64(config.Height) > 32_000_000 {
		return "", "", errors.New("需提供有效且不超过 3200 万像素的 PNG 或 JPEG 图片")
	}
	switch format {
	case "png":
		return ".png", "image/png", nil
	case "jpeg":
		return ".jpg", "image/jpeg", nil
	default:
		return "", "", errors.New("文档复刻仅支持原始 PNG 或 JPEG")
	}
}

func verifyStationDocument(file *os.File, size int64, ext string) error {
	if ext == ".docx" {
		if validateLocalDOCX(file, size) != nil {
			return errors.New("Word 附件缺少有效的 DOCX 文档结构")
		}
	} else {
		var header [5]byte
		if _, err := file.ReadAt(header[:], 0); err != nil || !bytes.Equal(header[:], []byte("%PDF-")) {
			return errors.New("附件缺少有效的 PDF 文件头")
		}
	}
	return nil
}
