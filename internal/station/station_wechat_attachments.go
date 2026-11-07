package station

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/identity"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/mediaspec"
	"wechat-bot/internal/workspacefs"
)

const stationAttachmentLimit = 512 << 20

type stationAttachmentSource struct {
	Account, Dataset, Thread, Message, Sender, Server, Occurred, BodyHash, Kind, Filename string
	Grant                                                                                 time.Time
	Origin                                                                                string `json:"origin,omitempty"`
	Binding                                                                               string `json:"binding,omitempty"`
}

type stationAttachmentRecord struct {
	ID                    string
	Source                stationAttachmentSource
	Bytes                 int64
	MIME, SHA256, Variant string
}

type stationAttachmentAccess struct {
	native                  *linuxnative.Sender
	dir, thread, message    string
	grant                   time.Time
	allowed                 func() bool
	ref                     *companion.SourceRef
	store                   *companion.Store
	history                 bool
	videoOnly               bool
	fetchVideo              stationVideoFetch
	shareHost               func() (*stationShareHost, error)
	requester, requestTrace string
	maxBytes                int64
}

func (a stationAttachmentAccess) CurrentThread() string                  { return a.thread }
func (a stationAttachmentAccess) InvalidAttachment(message string) error { return errors.New(message) }
func (a stationAttachmentAccess) Allowed(ctx context.Context, thread string) bool {
	return ctx.Err() == nil && thread == a.thread && !a.grant.IsZero() && a.allowed()
}

func stationAttachmentKind(m linuxnative.Message) (kind, filename string) {
	switch m.Type {
	case 3:
		kind = "image"
	case 34:
		kind = "voice"
	case 43:
		kind = "video"
	case 49:
		if wechatapp.IsChannelsShare(m) {
			return "video", "shared-video.mp4"
		}
		name := m.FileAttachmentName()
		if mediaspec.SafeFilename(name) && name != "." && name != ".." && !strings.ContainsAny(name, ":*?<>|") {
			return "file", name
		}
	}
	if kind == "" {
		return "", ""
	}
	for _, attachment := range m.Attachments {
		if attachment.Kind == kind && mediaspec.SafeFilename(attachment.Filename) && attachment.Filename != "." && attachment.Filename != ".." && !strings.ContainsAny(attachment.Filename, ":*?<>|") {
			return kind, attachment.Filename
		}
	}
	return kind, kind + ".bin"
}

func (a stationAttachmentAccess) source(ctx context.Context, thread, message string) (stationAttachmentSource, error) {
	denied := wechatapp.ErrAttachmentDenied
	if message != a.message || !a.Allowed(ctx, thread) {
		return stationAttachmentSource{}, denied
	}
	m, err := a.native.Message(ctx, message)
	if err != nil {
		return stationAttachmentSource{}, err
	}
	kind, filename := stationAttachmentKind(m)
	if kind == "" || m.ID != message || m.AccountID != a.native.Account() || m.ThreadID != thread || m.Direction != "incoming" || m.SenderID == "" {
		return stationAttachmentSource{}, denied
	}
	channels := wechatapp.IsChannelsShare(m)
	if a.videoOnly && kind != "video" || channels && !a.videoOnly {
		return stationAttachmentSource{}, denied
	}
	sum := sha256.Sum256([]byte(m.Body))
	bodyHash := hex.EncodeToString(sum[:])
	if a.ref != nil {
		r := a.ref
		occurred, timeErr := time.Parse(time.RFC3339Nano, m.CreatedAt)
		referenced, refTimeErr := time.Parse(time.RFC3339Nano, r.OccurredAt)
		if r.MessageID != message || r.Sender != m.SenderID || r.Account != m.AccountID || r.Thread != m.ThreadID ||
			r.ServerID != m.ServerID || r.MessageType != m.Type || timeErr != nil || refTimeErr != nil || !referenced.Equal(occurred) ||
			r.EventID == "" && r.BodyHash != bodyHash {
			return stationAttachmentSource{}, denied
		}
		in := companion.Input{Channel: "wechat", Account: m.AccountID, Thread: m.ThreadID, Sender: m.SenderID, TraceID: m.ID, MessageTime: m.CreatedAt, Source: r}
		if a.history {
			if visible, err := a.store.NativeRecallAllowed(ctx, in); err != nil || !visible {
				return stationAttachmentSource{}, denied
			}
		} else {
			if _, ok := a.store.SourceForInput(in); !ok {
				return stationAttachmentSource{}, denied
			}
		}
	}
	dataset, err := a.native.Identity(ctx)
	if err != nil {
		return stationAttachmentSource{}, err
	}
	if !a.Allowed(ctx, thread) {
		return stationAttachmentSource{}, denied
	}
	source := stationAttachmentSource{Account: m.AccountID, Dataset: dataset, Thread: thread, Message: message, Sender: m.SenderID, Server: m.ServerID, Occurred: m.CreatedAt,
		BodyHash: bodyHash, Kind: kind, Filename: filename, Grant: a.grant}
	if channels {
		channel, err := wechatapp.ReadChannelsSource(ctx, m, a.native.Account(), a.native)
		if err != nil {
			return stationAttachmentSource{}, err
		}
		source.Origin = "channels"
		source.Binding = channelsVideoBinding(m, channel)
	}
	return source, nil
}

func (source stationAttachmentSource) id() string {
	raw, _ := json.Marshal(source)
	sum := sha256.Sum256(raw)
	return "wa-" + hex.EncodeToString(sum[:])
}

func (a stationAttachmentAccess) confirmObservationSource(ctx context.Context, source stationAttachmentSource, epoch string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	current, err := a.source(ctx, source.Thread, source.Message)
	recall, recallErr := a.store.RecallEpoch(ctx)
	if err != nil || current != source || recallErr != nil || recall != epoch {
		return wechatapp.ErrAttachmentDenied
	}
	if ref := a.ref; ref != nil && ref.EventID != "" {
		in := companion.Input{Channel: "wechat", Account: source.Account, Thread: source.Thread, Sender: source.Sender, TraceID: source.Message, MessageTime: source.Occurred, Source: ref}
		if current, ok := a.store.SourceForInput(in); !ok || current.Revision != ref.Revision || current.BodyHash != ref.BodyHash {
			return wechatapp.ErrAttachmentDenied
		}
	}
	return nil
}

func stationAttachmentID(id string) bool {
	raw, err := hex.DecodeString(strings.TrimPrefix(id, "wa-"))
	return strings.HasPrefix(id, "wa-") && err == nil && len(raw) == sha256.Size && strings.ToLower(id) == id
}

func readStationAttachmentRecord(root *os.Root) (stationAttachmentRecord, error) {
	var record stationAttachmentRecord
	file, err := workspacefs.OpenRegular(root, "record.json")
	if err != nil {
		return record, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 32<<10))
	if err != nil || len(data) >= 32<<10 || json.Unmarshal(data, &record) != nil || record.ID != record.Source.id() ||
		record.Bytes <= 0 || record.Bytes > stationAttachmentLimit {
		return stationAttachmentRecord{}, errors.New("station_attachment_record_invalid")
	}
	return record, nil
}

func (a stationAttachmentAccess) open(ctx context.Context, source stationAttachmentSource) (stationAttachmentRecord, *os.File, error) {
	if !a.Allowed(ctx, source.Thread) {
		return stationAttachmentRecord{}, nil, wechatapp.ErrAttachmentDenied
	}
	root, err := workspacefs.OpenPath(filepath.Join(a.dir, "attachments", source.id()))
	if err != nil {
		return stationAttachmentRecord{}, nil, err
	}
	defer root.Close()
	record, err := readStationAttachmentRecord(root)
	if err != nil || record.Source != source {
		return stationAttachmentRecord{}, nil, errors.New("station_attachment_source_mismatch")
	}
	file, err := workspacefs.OpenRegular(root, "original")
	if err != nil {
		return stationAttachmentRecord{}, nil, err
	}
	hash := sha256.New()
	check := func() bool { return a.Allowed(ctx, source.Thread) }
	n, err := io.Copy(attachmentGrantWriter{allow: check, out: hash}, io.LimitReader(file, record.Bytes+1))
	if err != nil || n != record.Bytes || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 || !check() {
		file.Close()
		return stationAttachmentRecord{}, nil, errors.New("station_attachment_integrity_or_grant_changed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return stationAttachmentRecord{}, nil, err
	}
	return record, file, nil
}

func (a stationAttachmentAccess) ResolveAttachment(ctx context.Context, thread, message string) (wechatapp.Attachment, error) {
	source, err := a.source(ctx, thread, message)
	if err != nil {
		return wechatapp.Attachment{}, err
	}
	record, file, err := a.open(ctx, source)
	if errors.Is(err, os.ErrNotExist) {
		return wechatapp.Attachment{}, wechatapp.ErrAttachmentNotCached
	}
	if err != nil {
		return wechatapp.Attachment{}, err
	}
	file.Close()
	return wechatapp.Attachment{AssetID: record.ID, MessageID: message, Filename: source.Filename, MIME: record.MIME,
		Bytes: record.Bytes, Kind: source.Kind, SHA256: record.SHA256, Variant: record.Variant}, nil
}

type attachmentGrantWriter struct {
	allow func() bool
	out   io.Writer
}

func (w attachmentGrantWriter) Write(p []byte) (int, error) {
	if !w.allow() {
		return 0, wechatapp.ErrAttachmentDenied
	}
	return w.out.Write(p)
}

func (a stationAttachmentAccess) DownloadAttachment(ctx context.Context, thread, message string) error {
	source, err := a.source(ctx, thread, message)
	if err != nil {
		return err
	}
	root, err := workspacefs.EnsurePath(filepath.Join(a.dir, "attachments"))
	if err != nil {
		return err
	}
	defer root.Close()
	stage := ".transfer-" + identity.RandomToken()
	if err := root.Mkdir(stage, 0700); err != nil {
		return err
	}
	original, metadata := filepath.Join(stage, "original"), filepath.Join(stage, "record.json")
	defer func() {
		_ = root.Remove(original)
		_ = root.Remove(metadata)
		_ = root.Remove(stage)
	}()
	file, err := root.OpenFile(original, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	check := func() bool { return a.Allowed(ctx, thread) }
	var result linuxnative.VerifiedIncomingMedia
	if source.Origin == "channels" {
		result, err = a.downloadChannelsVideo(ctx, source, file)
	} else {
		limit := a.maxBytes
		if limit == 0 {
			limit = stationAttachmentLimit
		}
		result, err = a.native.StreamVerifiedIncomingMedia(ctx, message, source.Kind, limit, attachmentGrantWriter{allow: check, out: file})
	}
	if err == nil {
		err = file.Sync()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return err
	}
	verified, err := a.source(ctx, thread, message)
	if err != nil || verified != source {
		return wechatapp.ErrAttachmentDenied
	}
	typ, _, err := mime.ParseMediaType(result.MIME)
	if err != nil || typ == "" {
		typ = "application/octet-stream"
	}
	record := stationAttachmentRecord{ID: source.id(), Source: source, MIME: typ, Bytes: result.Bytes, SHA256: result.SHA256, Variant: result.Variant}
	if err := writeLegacyStagedFile(root, metadata, record); err != nil {
		return err
	}
	if !check() {
		return wechatapp.ErrAttachmentDenied
	}
	if err := root.Rename(stage, record.ID); err != nil {
		// Another request may have published this exact verified source.
		_, cached, cacheErr := a.open(ctx, source)
		if cacheErr != nil {
			return err
		}
		cached.Close()
	}
	return nil
}

type stationAttachmentTools struct {
	native            *linuxnative.Sender
	store             *companion.Store
	dir               string
	grants            map[string]time.Time
	replies           map[string]time.Time
	reads             map[string]time.Time
	readCursors       map[string]stationReadCursor
	legacyReadCursors map[string]stationLegacyReadCursor
	blocked           func() bool
	trace             companion.Input
	prepared          map[string]companion.GeneratedMedia
	work              *stationWorkAccess
	fetchVideo        stationVideoFetch
	shareHost         func() (*stationShareHost, error)
	documents         stationDocumentTools
}

func (*stationAttachmentTools) AutonomousActions() bool { return true }

func (t *stationAttachmentTools) ForkTools() companion.ToolAccess {
	copy := *t
	copy.trace = companion.Input{}
	copy.prepared = nil
	copy.readCursors = nil
	copy.legacyReadCursors = nil
	return &copy
}

func (t *stationAttachmentTools) SetTrace(in companion.Input) {
	t.trace, t.prepared, t.readCursors, t.legacyReadCursors = in, nil, nil, nil
}

func (t *stationAttachmentTools) allowed() bool {
	return t.scopeAllowed(t.trace, t.grants)
}

func (t *stationAttachmentTools) scopeAllowed(in companion.Input, grants map[string]time.Time) bool {
	grant := grants[in.Thread]
	stamp, err := time.Parse(time.RFC3339Nano, in.MessageTime)
	return !t.blocked() && !in.Preview && !in.Surf && !in.ReportOnly && in.Account == t.native.Account() &&
		!grant.IsZero() && err == nil && !stamp.Before(grant)
}

func (t *stationAttachmentTools) replyAllowed(in companion.Input) bool {
	return in.Mode == "direct" && in.Directed && !in.HistoricalReview && t.scopeAllowed(in, t.replies)
}

func (t *stationAttachmentTools) List(ctx context.Context) ([]mcptools.Tool, error) {
	if ctx.Err() != nil {
		return nil, nil
	}
	tools := []mcptools.Tool{}
	if t.scopeAllowed(t.trace, t.reads) {
		tools = append(tools, wechatapp.StationCatalogTool(), wechatapp.StationMessageTool())
		if t.legacyMemoryAvailable(ctx) {
			tools = append(tools, wechatapp.StationLegacyMemoryTool())
		}
	}
	if !t.allowed() {
		return tools, nil
	}
	if stationVideoRef(t.trace.Source) || stationVideoRef(t.trace.QuotedSource) || t.historyAllowed(ctx) {
		tools = append(tools, wechatapp.StationVideoTool(), wechatapp.StationVideoObservationTool())
	}
	if stationVoiceRef(t.trace.Source) || stationVoiceRef(t.trace.QuotedSource) {
		tools = append(tools, wechatapp.StationVoiceTranscriptionTool())
	}
	if (stationImageRef(t.trace.Source) || stationImageRef(t.trace.QuotedSource)) && t.imageObservationAvailable(ctx, t.trace.Account) {
		tools = append(tools, wechatapp.StationImageObservationTool())
	}
	if len(t.documentRefs(49)) != 0 || t.historyAllowed(ctx) {
		if t.documents.available != nil && t.documents.inspect != nil && t.documents.available(ctx, "") {
			tools = append(tools, wechatapp.StationDocumentInspectionTool())
		}
		if t.replyAllowed(t.trace) && t.canExportDocument(ctx) {
			tools = append(tools, wechatapp.StationDocumentPDFReplyTool())
		}
	}
	if (len(t.documentRefs(3)) != 0 || t.historyAllowed(ctx)) && t.replyAllowed(t.trace) && t.canReconstructImage(ctx) {
		tools = append(tools, wechatapp.StationImageDocumentTool())
	}
	if t.canReviseDocument(ctx) {
		if info, err := os.Stat(filepath.Join(t.dir, "document-replies")); err == nil && info.IsDir() {
			tools = append(tools, wechatapp.StationDocumentRevisionTool())
		}
	}
	if stationAttachmentRef(t.trace.Source) || stationAttachmentRef(t.trace.QuotedSource) {
		tools = append(tools, wechatapp.StationAttachmentTool())
		if t.replyAllowed(t.trace) {
			tools = append(tools, wechatapp.StationAttachmentReplyTool())
		}
	}
	if t.work != nil && t.replyAllowed(t.trace) {
		tools = append(tools, wechatapp.StationWorkReplyTool())
	}
	return tools, nil
}

func stationAttachmentRef(ref *companion.SourceRef) bool {
	return ref != nil && (ref.MessageType == 3 || ref.MessageType == 34 || ref.MessageType == 43 || ref.MessageType == 49)
}

func stationVideoRef(ref *companion.SourceRef) bool {
	return ref != nil && (ref.MessageType == 43 || ref.MessageType == 49)
}

func stationVoiceRef(ref *companion.SourceRef) bool {
	return ref != nil && ref.MessageType == 34
}

func stationImageRef(ref *companion.SourceRef) bool {
	return ref != nil && ref.MessageType == 3
}

func (t *stationAttachmentTools) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if name == "wechat_video_read" {
		return t.readVideo(ctx, raw)
	}
	if name == "wechat_video_observe" {
		return t.observeVideo(ctx, raw)
	}
	if name == "wechat_voice_transcribe" {
		return t.voiceTranscription(ctx, raw)
	}
	if name == "wechat_image_observe" {
		return t.observeImage(ctx, raw)
	}
	if name == "wechat_document_inspect" {
		return t.inspectDocument(ctx, raw)
	}
	if name == "wechat_document_export_pdf" {
		return t.prepareDocumentReply(ctx, raw)
	}
	if name == "wechat_document_revision" {
		return t.documentRevision(ctx, raw)
	}
	if name == "wechat_document_reconstruct_image" {
		return t.prepareImageDocument(ctx, raw)
	}
	if name == "wechat_read" {
		return t.readHistory(ctx, raw)
	}
	if name == "wechat_message_read" {
		return t.readMessage(ctx, raw)
	}
	if name == "wechat_read_legacy" {
		return t.readLegacyMemory(ctx, raw)
	}
	if name == "wechat_work_reply" {
		return t.workReply(ctx, raw)
	}
	if name == "wechat_attachment_reply_prepare" {
		return t.prepareReply(ctx, raw)
	}
	var in struct {
		Message string `json:"message_id"`
	}
	if name != "wechat_file_read" || !t.allowed() || strictLocalMemoryDecode(raw, &in) != nil || in.Message == "" {
		return "", wechatapp.ErrAttachmentDenied
	}
	access, err := t.attachmentAccess(t.trace, in.Message, t.allowed)
	if err != nil {
		return "", err
	}
	result, err := wechatapp.ReadAttachment(ctx, raw, access)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(result)
	return string(data), err
}

func (t *stationAttachmentTools) attachmentAccess(in companion.Input, message string, allowed func() bool) (stationAttachmentAccess, error) {
	ref := in.Source
	if message != in.TraceID {
		ref = in.QuotedSource
	}
	if !stationAttachmentRef(ref) || ref.MessageID != message {
		return stationAttachmentAccess{}, wechatapp.ErrAttachmentDenied
	}
	return stationAttachmentAccess{native: t.native, dir: t.dir, thread: in.Thread, message: message,
		grant: t.grants[in.Thread], allowed: allowed, ref: ref, store: t.store, fetchVideo: t.fetchVideo}, nil
}
