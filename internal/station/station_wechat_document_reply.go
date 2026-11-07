package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/workspacefs"
)

const (
	stationPDFPrefix       = "docpdf-"
	stationDOCXPrefix      = "docword-"
	stationPDFRuntimeName  = "converted.pdf"
	stationDOCXRuntimeName = "editable.docx"
)

type stationDocumentProduct struct {
	Filename, SHA256 string
	Bytes            int64
	Pages            int
}

type stationDocumentReply struct {
	Source                          stationAttachmentSource
	Reference                       companion.SourceRef
	OriginalSHA, Epoch, Trace, User string
	MediaGrant, HistoryGrant        time.Time
	Name                            string
	RequestAt                       string              `json:",omitempty"`
	IllustrationKey                 string              `json:",omitempty"`
	TitleHalfPoints                 int                 `json:",omitempty"`
	ParentAsset                     string              `json:",omitempty"`
	Layout                          *stationImageLayout `json:",omitempty"`
	Product                         stationDocumentProduct
}

func (r stationDocumentReply) id() string {
	prefix := stationPDFPrefix
	if r.extension() == ".docx" {
		prefix = stationDOCXPrefix
	}
	key := []any{r.Source.id(), r.Reference.EventID, r.Reference.Revision,
		r.OriginalSHA, r.Epoch, r.Trace, r.User, r.MediaGrant, r.HistoryGrant, r.Name}
	if r.ParentAsset != "" {
		key = append(key, r.ParentAsset)
	}
	if r.IllustrationKey != "" {
		key = append(key, r.IllustrationKey)
	}
	if r.TitleHalfPoints != 0 {
		key = append(key, r.TitleHalfPoints)
	}
	return prefix + localCheckpointKey(key...)
}

func (r stationDocumentReply) extension() string { return strings.ToLower(filepath.Ext(r.Name)) }

func stationDocumentReplyID(id string) bool {
	return strings.HasPrefix(id, stationPDFPrefix) || strings.HasPrefix(id, stationDOCXPrefix)
}

func (r stationDocumentReply) media() companion.GeneratedMedia {
	id := r.id()
	parent, mimeType := r.Source.id(), "application/pdf"
	if r.ParentAsset != "" {
		parent = r.ParentAsset
	}
	if r.extension() == ".docx" {
		mimeType = localDOCXMIME
	}
	return companion.GeneratedMedia{ID: "reply-" + id, AssetID: id, ParentAssetID: parent,
		Kind: "file", Title: r.Name, CachedMIME: mimeType}
}

func validStationPDFName(name string) bool {
	return validStationDocumentName(name, ".pdf")
}

func validStationDocumentName(name, ext string) bool {
	return mediaspec.SafeFilename(name) && filepath.IsLocal(name) && strings.EqualFold(filepath.Ext(name), ext) &&
		utf8.ValidString(name) && utf8.RuneCountInString(name) <= 120 && len(name) <= 256 && !strings.HasPrefix(name, ".") &&
		strings.TrimSpace(name) == name && !strings.ContainsAny(name, `:*?<>"|`) && strings.IndexFunc(name, unicode.IsControl) == -1
}

func (t *stationAttachmentTools) canExportDocument(ctx context.Context) bool {
	return t.documents.exportAvailable != nil && t.documents.exportPDF != nil && t.documents.exportAvailable(ctx)
}

func (t *stationAttachmentTools) prepareDocumentReply(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Message string `json:"message_id"`
		Name    string `json:"filename"`
		Hash    string `json:"expected_sha256"`
	}
	if !t.allowed() || !t.replyAllowed(t.trace) || t.trace.TraceID == "" || t.trace.Sender == "" {
		return "", wechatapp.ErrAttachmentDenied
	}
	if strictLocalMemoryDecode(raw, &args) != nil || args.Message == "" || len(args.Message) > 8192 ||
		args.Name != "" && !validStationPDFName(args.Name) || args.Hash != "" && !artifactID("sha-"+args.Hash, "sha-") {
		return "", mcptools.ArgumentError("$", "请提供 Word 原消息编号，可指定有效的 .pdf 文件名及原件 SHA-256")
	}
	if !t.canExportDocument(ctx) {
		return "", errors.New("文档工坊的本机 PDF 导出未启用")
	}
	in, err := t.openDocumentInput(ctx, args.Message, args.Hash, 49, func(ext string) error {
		if ext != ".docx" {
			return mcptools.ArgumentError("$.message_id", "请指定要转换为 PDF 的 Word（.docx）原件")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	defer in.file.Close()
	if args.Name == "" {
		args.Name = strings.TrimSuffix(in.record.Source.Filename, filepath.Ext(in.record.Source.Filename)) + ".pdf"
		if !validStationPDFName(args.Name) {
			args.Name = "document.pdf"
		}
	}
	reply := t.documentReplyForInput(&in, args.Name)
	record, err := t.preparePDFHandoff(ctx, reply, in)
	if err != nil {
		return "", err
	}
	return localProcessJSON(map[string]any{"status": "prepared", "media": []companion.GeneratedMedia{record.media()},
		"message_id": record.Source.Message, "source_sha256": record.OriginalSHA, "sha256": record.Product.SHA256, "page_count": record.Product.Pages})
}

func (t *stationAttachmentTools) documentReplyForInput(in *stationDocumentInput, name string) stationDocumentReply {
	reply := stationDocumentReply{Source: in.record.Source, Reference: in.sourceRef, OriginalSHA: in.record.SHA256,
		Epoch: in.epoch, Trace: t.trace.TraceID, User: t.trace.Sender, RequestAt: t.trace.MessageTime, MediaGrant: t.replies[t.trace.Thread], Name: name}
	if in.history {
		reply.HistoryGrant = t.reads[t.trace.Thread]
	}
	sourceCheck := in.check
	in.check = func(ctx context.Context) error {
		if !t.replyAllowed(t.trace) {
			return wechatapp.ErrAttachmentDenied
		}
		return sourceCheck(ctx)
	}
	return reply
}

func (t *stationAttachmentTools) preparePDFHandoff(ctx context.Context, reply stationDocumentReply, in stationDocumentInput) (stationDocumentReply, error) {
	id := reply.id()
	in.handoffKey = id
	record, file, err := t.openDocumentReply(ctx, t.trace, id)
	if errors.Is(err, os.ErrNotExist) {
		// The renderer sees a fixed task-local filename. The delivery name is
		// independent and never used to resolve a path inside the runtime.
		_, err = t.documents.exportPDF(ctx, in, stationPDFRuntimeName, func(ctx context.Context, pdf *os.File, product stationDocumentProduct) error {
			reply.Product = product
			return t.saveDocumentReply(ctx, reply, pdf, in.check)
		})
		if err != nil {
			return stationDocumentReply{}, err
		}
		record, file, err = t.openDocumentReply(ctx, t.trace, id)
	}
	if err != nil {
		return stationDocumentReply{}, err
	}
	file.Close()
	if err := in.check(ctx); err != nil {
		return stationDocumentReply{}, err
	}
	return record, nil
}

func (t *stationAttachmentTools) documentReplyAccess(ctx context.Context, in companion.Input, r stationDocumentReply) (stationAttachmentAccess, error) {
	return t.documentSourceAccess(ctx, in, r, false)
}

func (t *stationAttachmentTools) documentSourceAccess(ctx context.Context, in companion.Input, r stationDocumentReply, followup bool) (stationAttachmentAccess, error) {
	denied := wechatapp.ErrAttachmentDenied
	if !t.replyAllowed(in) || !t.scopeAllowed(in, t.grants) || in.TraceID == "" || !followup && in.TraceID != r.Trace || in.Sender != r.User ||
		in.Account != r.Source.Account || in.Thread != r.Source.Thread || !t.grants[in.Thread].Equal(r.Source.Grant) ||
		!t.replies[in.Thread].Equal(r.MediaGrant) {
		return stationAttachmentAccess{}, denied
	}
	allowed := func() bool {
		return t.replyAllowed(in) && t.scopeAllowed(in, t.grants) &&
			(!followup || t.scopeAllowed(in, t.reads)) && (r.HistoryGrant.IsZero() || t.reads[in.Thread].Equal(r.HistoryGrant) && t.scopeAllowed(in, t.reads))
	}
	epoch, err := t.store.RecallEpoch(ctx)
	if err != nil || r.Epoch != epoch || !allowed() {
		return stationAttachmentAccess{}, denied
	}
	if r.Reference.EventID != "" {
		source := companion.Input{Channel: "wechat", Account: r.Source.Account, Thread: r.Source.Thread, Sender: r.Source.Sender,
			TraceID: r.Source.Message, MessageTime: r.Source.Occurred, Source: &r.Reference}
		if _, ok := t.store.SourceForInput(source); !ok {
			return stationAttachmentAccess{}, denied
		}
	} else if !t.store.LegacyHistoryAllowed(in.Account, in.Thread) {
		return stationAttachmentAccess{}, denied
	}
	return stationAttachmentAccess{native: t.native, dir: t.dir, thread: in.Thread, message: r.Source.Message,
		grant: r.Source.Grant, allowed: allowed, ref: &r.Reference, store: t.store, history: r.Reference.EventID == "" || !r.HistoryGrant.IsZero()}, nil
}

func (t *stationAttachmentTools) readDocumentReply(id string) (stationDocumentReply, error) {
	var r stationDocumentReply
	if !artifactID(id, stationPDFPrefix) && !artifactID(id, stationDOCXPrefix) {
		return r, wechatapp.ErrAttachmentDenied
	}
	root, err := workspacefs.OpenPath(filepath.Join(t.dir, "document-replies", id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return r, os.ErrNotExist
		}
		return r, errors.New("文档回复目录不可用")
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, "record.json")
	if err != nil {
		return r, errors.New("文档回复记录不可用")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 64<<10))
	if err != nil || len(raw) >= 64<<10 || json.Unmarshal(raw, &r) != nil || r.id() != id ||
		!validStationDocumentName(r.Name, r.extension()) || !validStationDocumentProduct(r.Product, r.extension()) ||
		!artifactID("sha-"+r.OriginalSHA, "sha-") || r.ParentAsset != "" && !artifactID(r.ParentAsset, stationDOCXPrefix) ||
		r.TitleHalfPoints != 0 && (r.extension() != ".docx" || r.ParentAsset == "" || r.TitleHalfPoints < 16 || r.TitleHalfPoints > 96) {
		return stationDocumentReply{}, errors.New("文档回复记录无效")
	}
	return r, nil
}

func validStationPDFProduct(p stationDocumentProduct) bool {
	return validStationDocumentProduct(p, ".pdf")
}

func validStationDocumentProduct(p stationDocumentProduct, ext string) bool {
	return p.Bytes > 0 && p.Bytes <= stationDocumentLimit && artifactID("sha-"+p.SHA256, "sha-") &&
		(ext == ".pdf" && p.Filename == stationPDFRuntimeName && p.Pages > 0 ||
			ext == ".docx" && p.Filename == stationDOCXRuntimeName && p.Pages == 0)
}

func (t *stationAttachmentTools) saveDocumentReply(ctx context.Context, r stationDocumentReply, product *os.File, check func(context.Context) error) error {
	ext := r.extension()
	if !validStationDocumentProduct(r.Product, ext) || verifyStationDocument(product, r.Product.Bytes, ext) != nil {
		return errors.New("文档成品校验失败")
	}
	root, err := workspacefs.EnsurePath(filepath.Join(t.dir, "document-replies"))
	if err != nil {
		return err
	}
	defer root.Close()
	stage := ".render-" + identity.RandomToken()
	if err := root.Mkdir(stage, 0700); err != nil {
		return err
	}
	path, record := filepath.Join(stage, "content"+ext), filepath.Join(stage, "record.json")
	defer func() { _ = root.Remove(path); _ = root.Remove(record); _ = root.Remove(stage) }()
	dest, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	w := attachmentGrantWriter{allow: func() bool { return ctx.Err() == nil && t.replyAllowed(t.trace) }, out: io.MultiWriter(dest, hash)}
	n, err := io.Copy(w, io.NewSectionReader(product, 0, r.Product.Bytes+1))
	if err == nil {
		err = dest.Sync()
	}
	if err = errors.Join(err, dest.Close()); err != nil || n != r.Product.Bytes || hex.EncodeToString(hash.Sum(nil)) != r.Product.SHA256 {
		return errors.New("文档成品长度或摘要不匹配")
	}
	if err := writeLegacyStagedFile(root, record, r); err != nil {
		return err
	}
	if err := check(ctx); err != nil {
		return err
	}
	if err := root.Rename(stage, r.id()); err != nil {
		// A repeated invocation may have published the same handoff first.
		if prior, readErr := t.readDocumentReply(r.id()); readErr != nil || prior.Product != r.Product {
			return err
		}
	}
	return nil
}

func (t *stationAttachmentTools) openDocumentReply(ctx context.Context, in companion.Input, id string) (stationDocumentReply, *os.File, error) {
	r, err := t.readDocumentReply(id)
	if err != nil {
		return r, nil, err
	}
	if _, err := t.documentReplyAccess(ctx, in, r); err != nil {
		return r, nil, err
	}
	file, err := t.openDocumentProduct(ctx, in, r)
	return r, file, err
}

func (t *stationAttachmentTools) openDocumentProduct(ctx context.Context, in companion.Input, r stationDocumentReply) (*os.File, error) {
	root, err := workspacefs.OpenPath(filepath.Join(t.dir, "document-replies", r.id()))
	if err != nil {
		return nil, errors.New("文档回复目录不可用")
	}
	defer root.Close()
	file, err := workspacefs.OpenRegular(root, "content"+r.extension())
	if err != nil {
		return nil, errors.New("文档成品不可用")
	}
	hash := sha256.New()
	reader := attachmentGrantWriter{allow: func() bool { return ctx.Err() == nil && t.replyAllowed(in) }, out: hash}
	n, err := io.Copy(reader, io.LimitReader(file, r.Product.Bytes+1))
	if err != nil || n != r.Product.Bytes || hex.EncodeToString(hash.Sum(nil)) != r.Product.SHA256 {
		file.Close()
		return nil, errors.New("文档成品长度或摘要不匹配")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func (t *stationAttachmentTools) verifyDocumentReplySource(ctx context.Context, in companion.Input, r stationDocumentReply) error {
	access, err := t.documentReplyAccess(ctx, in, r)
	if err != nil {
		return err
	}
	return access.confirmObservationSource(ctx, r.Source, r.Epoch)
}

func (t *stationAttachmentTools) stageDocumentReply(ctx context.Context, in companion.Input, id string) (string, error) {
	r, file, err := t.openDocumentReply(ctx, in, id)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if err := t.verifyDocumentReplySource(ctx, in, r); err != nil {
		return "", err
	}
	upload, err := t.native.UploadVerified(ctx, r.Name, r.media().CachedMIME, file, r.Product.Bytes, r.Product.SHA256)
	if err != nil {
		return "", err
	}
	if err := t.verifyDocumentReplySource(ctx, in, r); err != nil {
		return "", err
	}
	return upload.ID, nil
}
