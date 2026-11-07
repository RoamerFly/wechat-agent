package station

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

// This exercises the boundary between an authorized Word attachment and the
// regular media delivery route.  The export callback is intentionally local:
// producing a PDF is not itself a WeChat upload.  A native upload only becomes
// possible after the returned media handle is selected by the reply plan.
func TestStationWechatDocumentPDFExportPreparesBeforeVerifiedDelivery(t *testing.T) {
	const account, thread, sender, message = "one", "room@chatroom", "owner", "word-message"
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	docx := syntheticStationDOCX(t)
	pdf := syntheticStationPDF()
	docxHash := fmt.Sprintf("%x", sha256.Sum256(docx))
	pdfHash := fmt.Sprintf("%x", sha256.Sum256(pdf))
	input := linuxnative.Message{
		ID: message, AccountID: account, ThreadID: thread, SenderID: sender, ServerID: "1314", Direction: "incoming", Type: 49,
		CreatedAt: now.Format(time.RFC3339), Body: "<msg><appmsg><type>6</type><title>source.docx</title></appmsg></msg>",
	}

	var transfers, uploads, sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			write(map[string]any{"pid": 4242, "dataDir": root, "serverId": "document-delivery-fixture", "ownershipGeneration": "fixture"})
		case "GET /v1/accounts/one/messages/word-message":
			write(input)
		case "GET /v1/accounts/one/messages/word-message/media":
			write(map[string]any{"items": []map[string]any{{"accountId": account, "messageId": message, "kind": "file", "variant": "original", "state": "available"}}})
		case "POST /v1/accounts/one/messages/word-message/media-transfers":
			var args map[string]string
			if err := json.NewDecoder(r.Body).Decode(&args); err != nil || args["quality"] != "original" {
				t.Errorf("document transfer request = %#v, %v", args, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			transfers.Add(1)
			write(map[string]any{"id": "download-word", "accountId": account, "kind": "download", "state": "available", "assetId": "asset-word"})
		case "GET /v1/accounts/one/media/asset-word":
			write(map[string]any{"id": "asset-word", "accountId": account, "messageId": message, "kind": "file", "state": "available", "variant": "original", "actualVariant": "original", "byteLength": len(docx), "contentType": localDOCXMIME, "sha256": docxHash})
		case "GET /v1/accounts/one/media/asset-word/content":
			_, _ = w.Write(docx)
		case "POST /v1/accounts/one/uploads":
			uploads.Add(1)
			file, header, err := r.FormFile("file")
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			got, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil || header.Filename != "converted.pdf" || string(got) != string(pdf) {
				t.Errorf("verified PDF upload = name=%q bytes=%d err=%v", header.Filename, len(got), readErr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			write(map[string]any{"id": "upload-pdf", "name": header.Filename, "byteLength": len(pdf), "sha256": pdfHash})
		case "POST /v1/accounts/one/sends":
			sends.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["threadId"] != thread || body["kind"] != "file" || body["sourceId"] != "upload-pdf" || r.Header.Get("Idempotency-Key") == "" {
				t.Errorf("PDF delivery request = %#v, %v", body, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			write(map[string]any{"id": "send-pdf", "accountId": account, "threadId": thread, "kind": "send", "sourceId": "upload-pdf", "state": "accepted", "serverMessageId": "2418"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	token := filepath.Join(root, "native-token")
	if err := os.WriteFile(token, []byte("offline-token"), 0600); err != nil {
		t.Fatal(err)
	}
	native, err := linuxnative.New(linuxnative.Config{URL: server.URL, Account: account, TokenFile: token})
	if err != nil {
		t.Fatal(err)
	}
	memory, err := companion.NewStore(filepath.Join(root, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	ref := &companion.SourceRef{InstanceID: memory.SourceInstanceID(), Account: account, Thread: thread, Sender: sender, MessageID: message, ServerID: input.ServerID, MessageType: input.Type, OccurredAt: input.CreatedAt, BodyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(input.Body))), Revision: 1}
	trace := companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: sender, TraceID: "export-request", Text: "把这份 Word 转成 PDF 发回", MessageTime: now.Add(time.Second).Format(time.RFC3339), Mode: "direct", Directed: true, Source: ref}
	config := companion.Defaults()
	config.Permissions.Remember = true
	if err := memory.Observe(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: sender, TraceID: message, MessageTime: input.CreatedAt, Source: ref, Text: input.Body}, config, now); err != nil {
		t.Fatal(err)
	}

	var exportCalls atomic.Int32
	tools := &stationAttachmentTools{
		native: native, store: memory, dir: root,
		grants: map[string]time.Time{thread: now.Add(-time.Minute)}, replies: map[string]time.Time{thread: now.Add(-time.Minute)},
		blocked: func() bool { return false },
		documents: stationDocumentTools{
			available:       func(context.Context, string) bool { return true },
			exportAvailable: func(context.Context) bool { return true },
			exportPDF: func(ctx context.Context, in stationDocumentInput, filename string, persist func(context.Context, *os.File, stationDocumentProduct) error) (stationDocumentProduct, error) {
				exportCalls.Add(1)
				if in.extension != ".docx" || in.record.SHA256 != docxHash || filename != "converted.pdf" {
					t.Fatalf("unexpected export input: extension=%q filename=%q hash=%q", in.extension, filename, in.record.SHA256)
				}
				path := filepath.Join(t.TempDir(), filename)
				if err := os.WriteFile(path, pdf, 0600); err != nil {
					return stationDocumentProduct{}, err
				}
				file, err := os.Open(path)
				if err != nil {
					return stationDocumentProduct{}, err
				}
				defer file.Close()
				product := stationDocumentProduct{Filename: filename, SHA256: pdfHash, Bytes: int64(len(pdf)), Pages: 1}
				if err := persist(ctx, file, product); err != nil {
					return stationDocumentProduct{}, err
				}
				return product, nil
			},
		},
	}
	tools.SetTrace(trace)
	listed, err := tools.List(t.Context())
	if err != nil || !slices.ContainsFunc(listed, func(tool mcptools.Tool) bool { return tool.Name == "wechat_document_export_pdf" }) {
		t.Fatalf("PDF export tool not available: %#v, %v", listed, err)
	}
	result, err := tools.Call(t.Context(), "wechat_document_export_pdf", json.RawMessage(fmt.Sprintf(`{"message_id":%q,"filename":"converted.pdf","expected_sha256":%q}`, message, docxHash)))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Status    string                     `json:"status"`
		Media     []companion.GeneratedMedia `json:"media"`
		SHA256    string                     `json:"sha256"`
		PageCount int                        `json:"page_count"`
	}
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("invalid export result %q: %v", result, err)
	}
	if response.Status != "prepared" || response.SHA256 != pdfHash || response.PageCount != 1 || len(response.Media) != 1 ||
		!strings.HasPrefix(response.Media[0].AssetID, "docpdf-") || response.Media[0].ID != "reply-"+response.Media[0].AssetID ||
		response.Media[0].Kind != "file" || response.Media[0].Title != "converted.pdf" || exportCalls.Load() != 1 {
		t.Fatalf("unexpected prepared PDF: %#v", response)
	}
	if transfers.Load() != 1 || uploads.Load() != 0 || sends.Load() != 0 {
		t.Fatalf("export must not upload or send: downloads=%d uploads=%d sends=%d", transfers.Load(), uploads.Load(), sends.Load())
	}
	selected, ok := tools.Media(response.Media[0].ID)
	if !ok || selected != response.Media[0] {
		t.Fatalf("reply selection cannot use prepared PDF: %#v %v", selected, ok)
	}
	// A revocation after preparation still prevents the prepared local product
	// from entering the native upload path.
	tools.replies[thread] = time.Time{}
	if _, err := tools.stageReply(t.Context(), trace, selected.Kind, selected.AssetID); err == nil || uploads.Load() != 0 {
		t.Fatalf("revoked PDF reply still staged: %v, uploads=%d", err, uploads.Load())
	}
	tools.replies[thread] = now.Add(-time.Minute)

	// A delivery worker may start after the export task has exited. Re-open the
	// recorded PDF as a fresh tool instance, rather than relying on memory.
	restarted := &stationAttachmentTools{native: native, store: memory, dir: root,
		grants: map[string]time.Time{thread: now.Add(-time.Minute)}, replies: map[string]time.Time{thread: now.Add(-time.Minute)},
		blocked: func() bool { return false }}
	restarted.SetTrace(trace)
	selected, ok = restarted.Media(response.Media[0].ID)
	if !ok || selected.AssetID != response.Media[0].AssetID {
		t.Fatalf("prepared PDF disappeared after restart: %#v %v", selected, ok)
	}
	sourceID, err := restarted.stageReply(t.Context(), trace, selected.Kind, selected.AssetID)
	if err != nil || sourceID != "upload-pdf" || uploads.Load() != 1 || sends.Load() != 0 {
		t.Fatalf("selected PDF staging = %q, %v; uploads=%d sends=%d", sourceID, err, uploads.Load(), sends.Load())
	}
	receipt, err := native.Submit(t.Context(), linuxnative.ToolSend{ThreadID: thread, Kind: "file", SourceID: sourceID, IdempotencyKey: "document-pdf-delivery"})
	if err != nil || receipt.State != "accepted" || receipt.ServerID != "2418" || sends.Load() != 1 {
		t.Fatalf("PDF delivery receipt = %#v, %v; sends=%d", receipt, err, sends.Load())
	}

	tools.SetTrace(trace)
	if _, err := tools.Call(t.Context(), "wechat_document_export_pdf", json.RawMessage(fmt.Sprintf(`{"message_id":%q,"expected_sha256":%q}`, message, strings.Repeat("0", 64)))); err == nil {
		t.Fatal("changed source digest was accepted for export")
	}
	if uploads.Load() != 1 || sends.Load() != 1 {
		t.Fatal("failed export attempted another upload or send")
	}
	if err := os.WriteFile(filepath.Join(root, "document-replies", selected.AssetID, "content.pdf"), []byte("not-a-pdf"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.stageReply(t.Context(), trace, selected.Kind, selected.AssetID); err == nil {
		t.Fatal("tampered PDF was accepted for delivery")
	} else if uploads.Load() != 1 || sends.Load() != 1 {
		t.Fatalf("tampered product reached native delivery: %v; uploads=%d sends=%d", err, uploads.Load(), sends.Load())
	}
}
