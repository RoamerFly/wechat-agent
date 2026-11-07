package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
)

// Regression coverage for the image-to-document handoff.  It deliberately
// keeps rendering local and uses a fake native server: preparing either DOCX
// or PDF must never upload the original or either generated product.
func TestStationWechatImageDocumentReconstructionPreparesEditableProducts(t *testing.T) {
	const account, thread, sender, message = "one", "room@chatroom", "owner", "image-message"
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	original := syntheticStationDocumentPNG(t)
	docx, pdf := syntheticStationDOCX(t), syntheticStationPDF()
	originalHash := fmt.Sprintf("%x", sha256.Sum256(original))
	docxHash, pdfHash := fmt.Sprintf("%x", sha256.Sum256(docx)), fmt.Sprintf("%x", sha256.Sum256(pdf))
	nativeMessage := linuxnative.Message{ID: message, AccountID: account, ThreadID: thread, SenderID: sender, ServerID: "3301", Type: 3,
		Direction: "incoming", Body: "[图片：合成排版样张]", CreatedAt: now.Format(time.RFC3339)}

	var transfers, uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/status":
			write(map[string]any{"pid": 4243, "dataDir": root, "serverId": "image-document-fixture", "ownershipGeneration": "fixture"})
		case "GET /v1/accounts/one/messages/image-message":
			write(nativeMessage)
		case "GET /v1/accounts/one/messages/image-message/media":
			// A thumbnail may already be listed by the native app, but only a
			// pending original is acceptable as the reconstruction source.  The
			// worker must create the original transfer below rather than treating
			// preview bytes as an editable-document input.
			write(map[string]any{"items": []map[string]any{
				{"accountId": account, "messageId": message, "kind": "image", "variant": "thumbnail", "actualVariant": "thumbnail", "state": "available"},
				{"accountId": account, "messageId": message, "kind": "image", "variant": "original", "state": "pending"},
			}})
		case "POST /v1/accounts/one/messages/image-message/media-transfers":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["quality"] != "original" {
				t.Errorf("original image transfer = %#v, %v", request, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			transfers.Add(1)
			write(map[string]any{"id": "download-image", "accountId": account, "kind": "download", "state": "available", "assetId": "native-image"})
		case "GET /v1/accounts/one/media/native-image":
			write(map[string]any{"id": "native-image", "accountId": account, "messageId": message, "kind": "image", "state": "available", "variant": "original", "actualVariant": "original", "byteLength": len(original), "contentType": "image/png", "sha256": originalHash})
		case "GET /v1/accounts/one/media/native-image/content":
			_, _ = w.Write(original)
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
			if readErr != nil || header.Filename != "样张.docx" || !bytes.Equal(got, docx) {
				t.Errorf("DOCX selection upload = %q %d %v", header.Filename, len(got), readErr)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			write(map[string]any{"id": "upload-docx", "name": header.Filename, "byteLength": len(docx), "sha256": docxHash})
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
	ref := &companion.SourceRef{InstanceID: memory.SourceInstanceID(), Account: account, Thread: thread, Sender: sender, MessageID: message, ServerID: nativeMessage.ServerID, MessageType: 3, OccurredAt: nativeMessage.CreatedAt, BodyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(nativeMessage.Body))), Revision: 1}
	config := companion.Defaults()
	config.Permissions.Remember = true
	if err := memory.Observe(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: sender, TraceID: message, Text: nativeMessage.Body, MessageTime: nativeMessage.CreatedAt, Source: ref}, config, now); err != nil {
		t.Fatal(err)
	}

	var reconstructs, exports atomic.Int32
	tools := &stationAttachmentTools{native: native, store: memory, dir: root,
		grants: map[string]time.Time{thread: now.Add(-time.Minute)}, replies: map[string]time.Time{thread: now.Add(-time.Minute)},
		blocked: func() bool { return false },
		documents: stationDocumentTools{
			imageAvailable:  func(context.Context) bool { return true },
			exportAvailable: func(context.Context) bool { return true },
			reconstructImage: func(ctx context.Context, in stationDocumentInput, persist func(context.Context, *os.File, stationDocumentProduct, stationImageLayout) error) (stationImageLayout, error) {
				reconstructs.Add(1)
				if in.extension != ".png" || in.record.SHA256 != originalHash {
					t.Fatalf("unexpected image input: extension=%q hash=%q", in.extension, in.record.SHA256)
				}
				layout := stationImageLayout{Headings: 1, Paragraphs: 2, Tables: 1}
				if in.trace == "clarify-request" {
					layout.NeedsClarification = true
				}
				path := filepath.Join(t.TempDir(), stationDOCXRuntimeName)
				if err := os.WriteFile(path, docx, 0600); err != nil {
					return stationImageLayout{}, err
				}
				file, err := os.Open(path)
				if err != nil {
					return stationImageLayout{}, err
				}
				defer file.Close()
				product := stationDocumentProduct{Filename: stationDOCXRuntimeName, SHA256: docxHash, Bytes: int64(len(docx))}
				if err := persist(ctx, file, product, layout); err != nil {
					return stationImageLayout{}, err
				}
				return layout, nil
			},
			exportPDF: func(ctx context.Context, in stationDocumentInput, filename string, persist func(context.Context, *os.File, stationDocumentProduct) error) (stationDocumentProduct, error) {
				exports.Add(1)
				if in.extension != ".docx" || in.record.SHA256 != docxHash || filename != stationPDFRuntimeName {
					t.Fatalf("unexpected PDF input: extension=%q hash=%q name=%q", in.extension, in.record.SHA256, filename)
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
		}}
	trace := companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: sender, TraceID: "image-request", Text: "照这张图做 Word", MessageTime: now.Add(time.Second).Format(time.RFC3339), Mode: "direct", Directed: true, Source: ref}
	tools.SetTrace(trace)
	call := func(value string) map[string]any {
		t.Helper()
		out, err := tools.Call(t.Context(), "wechat_document_reconstruct_image", json.RawMessage(value))
		if err != nil {
			t.Fatalf("image reconstruction: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatalf("invalid reconstruction result %q: %v", out, err)
		}
		return decoded
	}

	wordOnly := call(fmt.Sprintf(`{"message_id":%q,"base_name":"样张","format":"docx","expected_sha256":%q}`, message, originalHash))
	media := wordOnly["media"].([]any)
	if wordOnly["status"] != "prepared" || len(media) != 1 || reconstructs.Load() != 1 || exports.Load() != 0 || uploads.Load() != 0 || transfers.Load() != 1 {
		t.Fatalf("Word-only reconstruction unexpectedly exported/uploaded: %#v", wordOnly)
	}
	editable := wordOnly["editable_document"].(map[string]any)
	word := media[0].(map[string]any)
	wordID, _ := word["asset_id"].(string)
	if !strings.HasPrefix(wordID, stationDOCXPrefix) || wordID != editable["asset_id"] || word["title"] != "样张.docx" {
		t.Fatalf("editable document handle mismatch: %#v", wordOnly)
	}
	selected, ok := tools.Media("reply-" + wordID)
	if !ok || selected.AssetID != wordID || selected.ParentAssetID == "" {
		t.Fatalf("Word-only selection unavailable: %#v %v", selected, ok)
	}
	if source, err := tools.stageReply(t.Context(), trace, selected.Kind, selected.AssetID); err != nil || source != "upload-docx" || uploads.Load() != 1 {
		t.Fatalf("Word selected upload = %q, %v", source, err)
	}

	tools.SetTrace(trace)
	pdfOnly := call(fmt.Sprintf(`{"message_id":%q,"base_name":"样张","expected_sha256":%q}`, message, originalHash))
	pdfMedia := pdfOnly["media"].([]any)
	pdfInfo := pdfOnly["pdf"].(map[string]any)
	pdfID, _ := pdfMedia[0].(map[string]any)["asset_id"].(string)
	if len(pdfMedia) != 1 || !strings.HasPrefix(pdfID, stationPDFPrefix) || pdfMedia[0].(map[string]any)["parent_asset_id"] != wordID || pdfInfo["sha256"] != pdfHash || reconstructs.Load() != 1 || exports.Load() != 1 || uploads.Load() != 1 {
		t.Fatalf("default PDF result did not reuse Word or preserve lineage: %#v", pdfOnly)
	}
	tools.SetTrace(trace)
	both := call(fmt.Sprintf(`{"message_id":%q,"base_name":"样张","format":"both","expected_sha256":%q}`, message, originalHash))
	if files := both["media"].([]any); len(files) != 2 || reconstructs.Load() != 1 || exports.Load() != 1 || transfers.Load() != 1 || uploads.Load() != 1 {
		t.Fatalf("cached both-format reconstruction repeated work: %#v", both)
	}

	tools.replies[thread] = time.Time{}
	if _, ok := tools.Media("reply-" + wordID); ok {
		t.Fatal("revoked media authorization retained editable Word selection")
	}
	if _, err := tools.stageReply(t.Context(), trace, "file", wordID); err == nil || uploads.Load() != 1 {
		t.Fatalf("revoked Word stage reached native upload: %v", err)
	}
	tools.replies[thread] = now.Add(-time.Minute)
	clarify := trace
	clarify.TraceID = "clarify-request"
	tools.SetTrace(clarify)
	clarified := call(fmt.Sprintf(`{"message_id":%q,"base_name":"待确认","format":"both","expected_sha256":%q}`, message, originalHash))
	if clarified["status"] != "needs_clarification" || reconstructs.Load() != 2 || exports.Load() != 1 {
		t.Fatalf("unclear layout invented a PDF instead of requesting clarification: %#v", clarified)
	}
}

func syntheticStationDocumentPNG(t *testing.T) []byte {
	t.Helper()
	image := image.NewRGBA(image.Rect(0, 0, 12, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 12; x++ {
			image.Set(x, y, color.RGBA{R: uint8(20 * x), G: uint8(25 * y), B: 180, A: 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, image); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
