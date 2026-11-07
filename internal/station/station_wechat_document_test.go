package station

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wechatapp "wechat-bot/connected-apps/wechat/adapter"
	"wechat-bot/internal/companion"
	"wechat-bot/internal/linuxnative"
	"wechat-bot/internal/mcptools"
)

func TestStationWechatDocumentScope(t *testing.T) {
	testStationWechatDocument(t, stationDocumentTools{
		available: func(context.Context, string) bool { return true },
		inspect: func(ctx context.Context, in stationDocumentInput) (string, error) {
			dir := t.TempDir()
			if _, err := in.materialize(ctx, dir); err != nil {
				return "", err
			}
			if in.extension != ".docx" || in.offset != 0 || in.limit != 2 {
				t.Fatalf("unexpected document input: %#v", in)
			}
			raw, _ := json.Marshal(map[string]any{"kind": "docx", "sha256": in.record.SHA256,
				"bytes": in.record.Bytes, "text": "合成", "text_offset": 0, "text_total_characters": 4})
			view, err := stationDocumentResult(in, raw)
			if err != nil {
				return "", err
			}
			data, err := json.Marshal(view)
			return string(data), err
		},
	}, false, nil)
}

func TestLiveStationWechatDocumentLPAC(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST") != "1" {
		t.Skip("set AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST=1 for Windows document acceptance")
	}
	f := newLocalHomeFixture(t, filepath.Join(t.TempDir(), "host"), filepath.Join(t.TempDir(), "workspace"))
	options := localProjectToolWindowsOptions(t)
	f.handler.chat.projectTools.options = options
	f.login()
	if code, out := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize home: %d %#v", code, out)
	}
	if code, out := f.request("PUT", "/api/local/project-tools/document-workshop", map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install document tool: %d %#v", code, out)
	}
	revision := localProjectToolRevision(t, localProjectToolsView(t, f))
	if code, out := f.request("PUT", "/api/local/project-tools/document-workshop", map[string]any{"action": "enable", "revision": revision}, true); code != http.StatusOK {
		t.Fatalf("enable document tool: %d %#v", code, out)
	}
	home := f.handler.chat.projectTools.home
	testStationWechatDocument(t, stationDocumentTools{available: home.stationDocumentAvailable, inspect: home.inspectStationDocument,
		imageAvailable: home.stationDocumentImageAvailable, reconstructImage: home.reconstructStationDocumentImage,
		illustrationsAvailable: home.stationDocumentIllustrationsAvailable,
		reviseAvailable:        home.stationDocumentRevisionAvailable, reviseTitle: home.reviseStationDocumentTitle,
		exportAvailable: home.stationDocumentExportAvailable, exportPDF: home.exportStationDocumentPDF}, true,
		func(data []byte) string { return localPDFExtractText(t, options, data) })
	entries, err := os.ReadDir(filepath.Join(home.dataDir, "project-tool-tasks"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("isolated input remained after parsing: %v, %v", entries, err)
	}
}

func testStationWechatDocument(t *testing.T, documents stationDocumentTools, withPDF bool, imagePDFText func([]byte) string) {
	t.Helper()
	const account, thread, requester = "one", "room@chatroom", "owner"
	root := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	makeMessage := func(id, sender, filename string) linuxnative.Message {
		return linuxnative.Message{ID: id, AccountID: account, ThreadID: thread, SenderID: sender, ServerID: id,
			Direction: "incoming", Type: 49, CreatedAt: now.Format(time.RFC3339),
			Body: "<msg><appmsg><type>6</type><title>" + filename + "</title></appmsg></msg>"}
	}
	messages := map[string]linuxnative.Message{
		"doc": makeMessage("doc", requester, "fixture.docx"), "other": makeMessage("other", "another-member", "fixture.docx"),
		"pdf": makeMessage("pdf", "another-member", "fixture.pdf"), "unseen": makeMessage("unseen", requester, "fixture.docx"),
		"own-pdf": makeMessage("own-pdf", requester, "fixture.pdf"),
	}
	foreign := makeMessage("foreign", requester, "fixture.docx")
	foreign.ThreadID = "different@chatroom"
	messages[foreign.ID] = foreign
	future := makeMessage("future", requester, "fixture.docx")
	future.CreatedAt = now.Add(2 * time.Minute).Format(time.RFC3339)
	messages[future.ID] = future
	docx := syntheticStationDOCX(t)
	pdf := syntheticStationPDF()
	fixtures := map[string][]byte{"doc": docx, "unseen": docx, "pdf": pdf, "own-pdf": pdf}
	mimes := map[string]string{"doc": localDOCXMIME, "unseen": localDOCXMIME, "pdf": "application/pdf", "own-pdf": "application/pdf"}
	withIllustrations := imagePDFText != nil && os.Getenv("AHS_LOCAL_PROJECT_ILLUSTRATIONS_TEST") == "1"
	withMerges := imagePDFText != nil && os.Getenv("AHS_LOCAL_PROJECT_MERGED_TABLE_TEST") == "1"
	withBorderless := imagePDFText != nil && os.Getenv("AHS_LOCAL_PROJECT_BORDERLESS_TABLE_TEST") == "1"
	withColumns := imagePDFText != nil && os.Getenv("AHS_LOCAL_PROJECT_DOCUMENT_COLUMNS_TEST") == "1"
	if imagePDFText != nil {
		name := "synthetic-wechat-document-grid.png"
		if withIllustrations {
			name = "synthetic-wechat-illustrated-document.png"
		}
		if withMerges {
			name = "synthetic-wechat-merged-table.png"
		}
		if withBorderless {
			name = "synthetic-wechat-borderless-table.png"
		}
		if withColumns {
			name = "synthetic-wechat-columns.png"
		}
		image, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		messages["grid"] = linuxnative.Message{ID: "grid", AccountID: account, ThreadID: thread, SenderID: requester, ServerID: "101",
			Direction: "incoming", Type: 3, CreatedAt: now.Format(time.RFC3339), Body: "synthetic document image"}
		fixtures["grid"], mimes["grid"] = image, "image/png"
	}
	withScan := imagePDFText != nil && os.Getenv("AHS_LOCAL_PROJECT_DOCUMENT_SCAN_TEST") == "1"
	if withScan {
		data, err := os.ReadFile(filepath.Join("testdata", "synthetic-wechat-mixed.pdf"))
		if err != nil {
			t.Fatal(err)
		}
		messages["mixed"] = makeMessage("mixed", requester, "fixture-mixed.pdf")
		fixtures["mixed"], mimes["mixed"] = data, "application/pdf"
	}
	kind := func(id string) string {
		if id == "grid" {
			return "image"
		}
		return "file"
	}
	var transfers, uploads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := func(value any) { _ = json.NewEncoder(w).Encode(value) }
		if r.URL.Path == "/v1/status" {
			write(map[string]any{"pid": 1234, "dataDir": root, "serverId": "document-fixture", "ownershipGeneration": "fixture"})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/v1/accounts/one/uploads" {
			uploads.Add(1)
			file, header, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil || header.Filename != "复刻文档.pdf" || !bytes.HasPrefix(data, []byte("%PDF-")) {
				t.Error("unexpected revision upload")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			write(map[string]any{"id": "revised-pdf-upload", "name": header.Filename, "byteLength": len(data),
				"sha256": fmt.Sprintf("%x", sha256.Sum256(data))})
			return
		}
		if r.URL.Path == "/v1/accounts/one/threads/"+thread+"/messages" {
			write(map[string]any{"items": []linuxnative.Message{messages["doc"], messages["other"], messages["unseen"]}})
			return
		}
		if suffix, ok := strings.CutPrefix(r.URL.Path, "/v1/accounts/one/messages/"); ok {
			id, endpoint, _ := strings.Cut(suffix, "/")
			if message, exists := messages[id]; exists {
				switch r.Method + " " + endpoint {
				case "GET ":
					write(message)
					return
				case "GET media":
					state := "available"
					if id == "grid" {
						state = "pending"
					}
					write(map[string]any{"items": []map[string]any{{"accountId": account, "messageId": id,
						"kind": kind(id), "variant": "original", "state": state}}})
					return
				case "POST media-transfers":
					var args map[string]string
					if err := json.NewDecoder(r.Body).Decode(&args); err != nil || args["quality"] != "original" {
						t.Error("attachment transfer did not request the original", args, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					transfers.Add(1)
					write(map[string]any{"id": "download-" + id, "accountId": account, "kind": "download", "state": "available", "assetId": "asset-" + id})
					return
				}
			}
		}
		if suffix, ok := strings.CutPrefix(r.URL.Path, "/v1/accounts/one/media/asset-"); ok {
			id, endpoint, _ := strings.Cut(suffix, "/")
			if data, exists := fixtures[id]; exists {
				if endpoint == "content" {
					_, _ = w.Write(data)
				} else {
					write(map[string]any{"id": "asset-" + id, "accountId": account, "messageId": id, "kind": kind(id),
						"state": "available", "variant": "original", "actualVariant": "original", "byteLength": len(data),
						"contentType": mimes[id], "sha256": fmt.Sprintf("%x", sha256.Sum256(data))})
				}
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	token := filepath.Join(root, "token")
	if err := os.WriteFile(token, []byte("synthetic-token"), 0600); err != nil {
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
	blocked := false
	tools := &stationAttachmentTools{native: native, store: memory, dir: root, documents: documents, blocked: func() bool { return blocked },
		grants: map[string]time.Time{thread: now.Add(-time.Minute)}, replies: map[string]time.Time{thread: now.Add(-time.Minute)}}
	refs := map[string]*companion.SourceRef{}
	for id, message := range messages {
		if id == "unseen" || id == "foreign" || id == "future" {
			continue
		}
		ref := &companion.SourceRef{Account: account, Thread: thread, Sender: message.SenderID, MessageID: id, ServerID: message.ServerID,
			MessageType: message.Type, OccurredAt: message.CreatedAt, BodyHash: fmt.Sprintf("%x", sha256.Sum256([]byte(message.Body))), Revision: 1,
			InstanceID: memory.SourceInstanceID()}
		cfg := companion.Defaults()
		cfg.Permissions.Remember = true
		if err := memory.Observe(companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: message.SenderID,
			TraceID: id, MessageTime: message.CreatedAt, Source: ref, Text: message.Body}, cfg, now); err != nil {
			t.Fatal(err)
		}
		refs[id] = ref
	}
	current := companion.Input{Channel: "wechat", Account: account, Thread: thread, Sender: requester, TraceID: "read-file",
		MessageTime: now.Format(time.RFC3339), UnreadMessages: []companion.ConversationEvent{
			{MessageID: "doc", Sender: requester, Source: refs["doc"]},
			{MessageID: "other", Sender: "another-member", Source: refs["other"]},
		}}
	tools.SetTrace(current)
	list, err := tools.List(t.Context())
	if err != nil || !slices.ContainsFunc(list, func(tool mcptools.Tool) bool { return tool.Name == "wechat_document_inspect" }) {
		t.Fatalf("document tool missing: %v %v", list, err)
	}
	call := func(args string) (string, error) {
		return tools.Call(t.Context(), "wechat_document_inspect", json.RawMessage(args))
	}
	for range 2 {
		if out, err := call(`{"message_id":"doc","text_limit":2}`); err != nil || !strings.Contains(out, `"text":"合成"`) ||
			!strings.Contains(out, `"next_text_offset":2`) || strings.Contains(out, root) {
			t.Fatalf("same-sender document reading: %s, %v", out, err)
		}
	}
	if transfers.Load() != 1 {
		t.Fatalf("repeat read transferred the original %d times", transfers.Load())
	}
	if _, err := call(`{"message_id":"other"}`); !errors.Is(err, wechatapp.ErrAttachmentDenied) {
		t.Fatalf("unquoted other-sender attachment was not denied: %v", err)
	}
	if withPDF {
		current.QuotedSource = refs["pdf"]
		tools.SetTrace(current)
		if out, err := call(`{"message_id":"pdf","page":1}`); err != nil || !strings.Contains(out, `"text":"Synthetic PDF text"`) {
			t.Fatalf("quoted PDF parsing: %s, %v", out, err)
		}
	}
	if imagePDFText != nil {
		request := current
		request.TraceID, request.Mode, request.Directed, request.QuotedSource = "convert-grid", "direct", true, refs["grid"]
		tools.SetTrace(request)
		out, err := tools.Call(t.Context(), "wechat_document_reconstruct_image", json.RawMessage(`{"message_id":"grid"}`))
		var prepared struct {
			Status string
			Media  []companion.GeneratedMedia
			Layout struct {
				Headings, Paragraphs, Tables int
				MergedCells                  int `json:"merged_cells"`
				BorderlessTables             int `json:"borderless_tables"`
				ReadingColumns               int `json:"reading_columns"`
				Illustrations                []stationIllustrationRect
			}
			Word struct {
				ID string `json:"asset_id"`
			} `json:"editable_document"`
		}
		tables := 1
		if withColumns {
			tables = 0
		}
		if err != nil || json.Unmarshal([]byte(out), &prepared) != nil || prepared.Status != "prepared" ||
			prepared.Layout.Tables != tables || prepared.Layout.Paragraphs < 1 || prepared.Layout.Headings != 1 ||
			len(prepared.Media) != 1 || prepared.Media[0].ParentAssetID != prepared.Word.ID {
			t.Fatalf("isolated reconstruction did not yield an editable Word and its delivery PDF: %s, %v", out, err)
		}
		readProduct := func(id string) []byte {
			_, file, err := tools.openDocumentReply(t.Context(), request, id)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		wordBytes, pdfBytes := readProduct(prepared.Word.ID), readProduct(prepared.Media[0].AssetID)
		wordXML := localDOCXDocumentXML(t, wordBytes)
		cells := 4
		if withColumns {
			cells = 0
			if prepared.Layout.ReadingColumns != 2 {
				t.Fatalf("both source columns must be recognized: %#v", prepared.Layout)
			}
		}
		if withMerges {
			cells = 7
			if prepared.Layout.MergedCells != 2 || !strings.Contains(wordXML, `<w:gridSpan w:val="3"/>`) ||
				!strings.Contains(wordXML, `<w:vMerge w:val="restart"/>`) || !strings.Contains(wordXML, `<w:vMerge/>`) {
				t.Fatalf("Word lacks the two native merges: %#v, %s", prepared.Layout, wordXML)
			}
		}
		if withBorderless {
			cells = 9
			if prepared.Layout.BorderlessTables != 1 || strings.Count(wordXML, `w:val="nil"`) != 6 {
				t.Fatalf("Word lacks an editable borderless table: %#v, %s", prepared.Layout, wordXML)
			}
		}
		if strings.Count(wordXML, "<w:tc>") != cells || strings.Contains(wordXML, "<w:tbl>") != (tables > 0) ||
			!strings.Contains(wordXML, "Heading1") || strings.Contains(wordXML, "<w:drawing>") != withIllustrations {
			t.Fatalf("reconstructed Word did not contain editable heading and table: %s", wordXML)
		}
		if withIllustrations {
			verifyStationWordCrop(t, wordBytes, fixtures["grid"], prepared.Layout.Illustrations)
			figure := strings.Index(wordXML, "<w:drawing>")
			if figure < strings.Index(wordXML, "本次测试用于核对文档复刻结果") || figure > strings.Index(wordXML, "<w:tbl>") ||
				!strings.Contains(wordXML, `<wp:extent cx="4572000" cy="2286000"/>`) ||
				!bytes.Contains(pdfBytes, []byte("/Subtype /Image")) {
				t.Fatal("Word reading order, image aspect ratio or PDF illustration did not match the source")
			}
		}
		pdfText := imagePDFText(pdfBytes)
		expectedWords := []string{"文件交接记录", "本次测试用于核对文档复刻结果", "项目", "结果", "文档", "通过"}
		checkColumnOrder := func(text string) {
			t.Helper()
			position := -1
			for _, expected := range expectedWords {
				next := strings.Index(text, expected)
				if next <= position || strings.Count(text, expected) != 1 {
					t.Fatalf("out-of-order, missing or duplicated column text %q: %s", expected, text)
				}
				position = next
			}
		}
		if withColumns {
			expectedWords = []string{"文件交接记录", "本次测试用于核对文档复刻结果", "先读取原图并记录文件来源", "确认标题文字和正文顺序", "最后保留可编辑的文档原稿",
				"以下内容用于检查阅读顺序", "导出文件后检查正文文字", "完成之后保留文件和记录", "请核对以上信息"}
			checkColumnOrder(wordXML)
			checkColumnOrder(pdfText)
		}
		if withBorderless {
			expectedWords = []string{"文件交接记录", "本次测试用于核对文档复刻结果", "项目", "时间", "结果", "文档", "上午", "通过", "图片", "下午", "完成", "请核对以上信息"}
			tableStart := strings.Index(wordXML, "<w:tbl>")
			tableEnd := strings.Index(wordXML, "</w:tbl>")
			if tableStart < strings.Index(wordXML, "本次测试用于核对文档复刻结果") || tableEnd > strings.Index(wordXML, "请核对以上信息") {
				t.Fatalf("Word did not preserve text before and after the table: %s", wordXML)
			}
		}
		if withMerges {
			expectedWords = []string{"文件交接记录", "本次测试用于核对文档复刻结果", "课程计划", "活动分组", "上午", "下午", "已确认", "未完成"}
			for _, name := range []string{"课程计划", "活动分组"} {
				if strings.Count(wordXML, name) != 1 || strings.Count(pdfText, name) != 1 {
					t.Fatalf("merged cell %q missing or repeated: %s", name, pdfText)
				}
			}
		}
		for _, expected := range expectedWords {
			if !strings.Contains(wordXML, expected) || !strings.Contains(pdfText, expected) {
				t.Fatalf("missing %q from Word or PDF text: Word=%s PDF=%s", expected, wordXML, pdfText)
			}
		}
		followup := request
		followup.TraceID, followup.MessageTime, followup.QuotedSource = "resize-title-later", now.Add(3*time.Minute).Format(time.RFC3339), nil
		tools.SetTrace(followup)
		resize := func(raw string) (string, error) {
			return tools.Call(t.Context(), "wechat_document_revision", json.RawMessage(raw))
		}
		if _, err := resize(`{"action":"list"}`); !errors.Is(err, wechatapp.ErrAttachmentDenied) {
			t.Fatalf("follow-up did not require history grant: %v", err)
		}
		tools.reads = map[string]time.Time{thread: now.Add(-time.Minute)}
		caps, err := tools.List(t.Context())
		if err != nil || !slices.ContainsFunc(caps, func(tool mcptools.Tool) bool { return tool.Name == "wechat_document_revision" }) {
			t.Fatalf("follow-up could not discover revision tool: %v, %v", caps, err)
		}
		prior, err := tools.readDocumentReply(prepared.Word.ID)
		if err != nil {
			t.Fatal(err)
		}
		wordList, err := resize(`{"action":"list","limit":1}`)
		if err != nil || !strings.Contains(wordList, prepared.Word.ID) || !strings.Contains(wordList, prior.Product.SHA256) {
			t.Fatalf("follow-up could not locate the exact original Word version: %s, %v", wordList, err)
		}
		raw := fmt.Sprintf(`{"action":"resize_title","asset_id":%q,"expected_sha256":%q,"font_size_pt":26.5}`, prepared.Word.ID, prior.Product.SHA256)
		other := followup
		other.Sender = "another-member"
		tools.SetTrace(other)
		if list, err := resize(`{"action":"list"}`); err != nil || strings.Contains(list, prepared.Word.ID) {
			t.Fatalf("other requester saw prior Word: %s, %v", list, err)
		}
		if _, err := resize(raw); !errors.Is(err, wechatapp.ErrAttachmentDenied) {
			t.Fatalf("other requester edited prior Word: %v", err)
		}
		tools.SetTrace(followup)
		if _, ok := tools.Media("reply-" + prepared.Word.ID); ok {
			t.Fatal("prior handoff was selectable for delivery in the follow-up")
		}
		if _, err := resize(strings.Replace(raw, prior.Product.SHA256, strings.Repeat("a", 64), 1)); err == nil || !strings.Contains(err.Error(), "摘要") {
			t.Fatalf("incorrect version was not rejected: %v", err)
		}
		downloadCount := transfers.Load()
		newReply, err := resize(raw)
		var revised struct {
			Status string
			Parent string `json:"revised_from"`
			Media  []companion.GeneratedMedia
			Word   struct {
				ID string `json:"asset_id"`
			} `json:"editable_document"`
		}
		if err != nil || json.Unmarshal([]byte(newReply), &revised) != nil || revised.Status != "prepared" ||
			revised.Parent != prepared.Word.ID || revised.Word.ID == prepared.Word.ID || len(revised.Media) != 1 ||
			revised.Media[0].ParentAssetID != revised.Word.ID || transfers.Load() != downloadCount {
			t.Fatalf("title edit did not reuse Word and prepare new PDF: %s, %v", newReply, err)
		}
		readRevision := func(id string) []byte {
			_, file, err := tools.openDocumentReply(t.Context(), followup, id)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			data, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}
		revisedWord := readRevision(revised.Word.ID)
		if withIllustrations {
			verifyStationWordCrop(t, revisedWord, fixtures["grid"], prepared.Layout.Illustrations)
		}
		revisedXML := localDOCXDocumentXML(t, revisedWord)
		revisedPDF := imagePDFText(readRevision(revised.Media[0].AssetID))
		if !strings.Contains(revisedXML, `<w:sz w:val="53"/>`) || strings.Count(revisedXML, "<w:tc>") != cells ||
			!strings.Contains(revisedXML, "本次测试用于核对文档复刻结果") || !strings.Contains(revisedPDF, "文件交接记录") {
			t.Fatalf("title, table and Chinese text verification failed: %s, %s", revisedXML, revisedPDF)
		}
		if withMerges && (!strings.Contains(revisedXML, `<w:gridSpan w:val="3"/>`) || !strings.Contains(revisedXML, `<w:vMerge/>`)) {
			t.Fatal("title revision discarded a native table merge")
		}
		if withBorderless && strings.Count(revisedXML, `w:val="nil"`) != 6 {
			t.Fatalf("title revision lost the borderless table: %s", revisedXML)
		}
		if withColumns {
			checkColumnOrder(revisedXML)
			checkColumnOrder(revisedPDF)
		}
		if repeated, err := resize(raw); err != nil || repeated != newReply {
			t.Fatalf("revision retry prepared a different product: %s, %v", repeated, err)
		}
		if uploads.Load() != 0 {
			t.Fatal("preparing a revision uploaded without a selected final reply")
		}
		selected, ok := tools.Media(revised.Media[0].ID)
		if !ok || selected.AssetID != revised.Media[0].AssetID {
			t.Fatal("verified new PDF could not be selected for delivery")
		}
		if upload, err := tools.stageReply(t.Context(), followup, selected.Kind, selected.AssetID); err != nil ||
			upload != "revised-pdf-upload" || uploads.Load() != 1 {
			t.Fatalf("selected PDF did not reach the simulated native endpoint: %q, %v", upload, err)
		}
		later := followup
		later.TraceID, later.MessageTime = "later-version-list", now.Add(4*time.Minute).Format(time.RFC3339)
		tools.SetTrace(later)
		firstPage, err := resize(`{"action":"list","limit":1}`)
		var page struct {
			Items []stationEditableDocument
			After string `json:"next_after"`
		}
		if err != nil || json.Unmarshal([]byte(firstPage), &page) != nil || len(page.Items) != 1 || page.Items[0].AssetID != revised.Word.ID || page.After != revised.Word.ID {
			t.Fatalf("latest edition not listed first: %s, %v", firstPage, err)
		}
		nextPage, err := resize(fmt.Sprintf(`{"action":"list","limit":1,"after":%q}`, page.After))
		if err != nil || json.Unmarshal([]byte(nextPage), &page) != nil || len(page.Items) != 1 || page.Items[0].AssetID != prepared.Word.ID {
			t.Fatalf("old edition not available on the next page: %s, %v", nextPage, err)
		}
		tools.SetTrace(followup)
		tools.reads = nil
		if _, ok := tools.Media(revised.Media[0].ID); ok {
			t.Fatal("revised PDF remained selectable after history revocation")
		}
		if _, err := tools.stageReply(t.Context(), followup, selected.Kind, selected.AssetID); err == nil || uploads.Load() != 1 {
			t.Fatal("revoked revision reached the simulated upload endpoint")
		}
		tools.SetTrace(current)
	}
	if withScan {
		scan := current
		scan.TraceID, scan.QuotedSource = "scan-mixed", refs["mixed"]
		tools.SetTrace(scan)
		verifyStationWechatPDFScan(t, tools)
		tools.SetTrace(current)
	}
	// The user sends the instruction in another turn with no quote. The
	// history lookup can now locate the same sender's file by message ID.
	current.TraceID, current.MessageTime = "later-request", now.Add(time.Minute).Format(time.RFC3339)
	current.UnreadMessages, current.QuotedSource = nil, nil
	tools.SetTrace(current)
	deny := func(id string) {
		t.Helper()
		if _, err := call(fmt.Sprintf(`{"message_id":%q,"text_limit":2}`, id)); !errors.Is(err, wechatapp.ErrAttachmentDenied) {
			t.Fatalf("historical source %s should be denied: %v", id, err)
		}
	}
	deny("doc")
	tools.reads = map[string]time.Time{thread: now.Add(-time.Minute)}
	list, err = tools.List(t.Context())
	if err != nil || !slices.ContainsFunc(list, func(tool mcptools.Tool) bool { return tool.Name == "wechat_document_inspect" }) {
		t.Fatalf("historical document tool missing: %v %v", list, err)
	}
	catalog, err := tools.Call(t.Context(), "wechat_read", json.RawMessage(`{"kind":"messages","search":"fixture.docx"}`))
	if err != nil || !strings.Contains(catalog, `"message_id":"doc"`) || !strings.Contains(catalog, `"read_scope":"file_metadata_only"`) {
		t.Fatalf("could not find historical attachment: %s, %v", catalog, err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(docx))
	for _, id := range []string{"doc", "unseen"} {
		out, err := call(fmt.Sprintf(`{"message_id":%q,"text_limit":2,"expected_sha256":%q}`, id, digest))
		if err != nil || !strings.Contains(out, `"text":"合成"`) {
			t.Fatalf("historical document reading: %s, %v", out, err)
		}
	}
	if out, err := call(fmt.Sprintf(`{"message_id":"doc","text_limit":2,"expected_sha256":%q}`, strings.Repeat("0", 64))); err == nil || out != "" {
		t.Fatalf("changed document digest was accepted: %s %v", out, err)
	}
	if withPDF {
		out, err := call(fmt.Sprintf(`{"message_id":"doc","text_offset":2,"text_limit":2,"expected_sha256":%q}`, digest))
		if err != nil || !strings.Contains(out, `"text":"正文"`) {
			t.Fatalf("historical DOCX continuation: %s, %v", out, err)
		}
		out, err = call(`{"message_id":"own-pdf","text_limit":9}`)
		if err != nil || !strings.Contains(out, `"text":"Synthetic"`) || !strings.Contains(out, `"next_text_offset":9`) {
			t.Fatalf("historical PDF parsing: %s, %v", out, err)
		}
	}
	for _, id := range []string{"other", "foreign", "future"} {
		deny(id)
	}
	if err := memory.Clear(companion.ScopeID("wechat", account, thread)); err != nil {
		t.Fatal(err)
	}
	deny("doc")
	deny("unseen")
	blocked = true
	if _, err := call(`{"message_id":"doc"}`); !errors.Is(err, wechatapp.ErrAttachmentDenied) {
		t.Fatalf("revoked grant accepted: %v", err)
	}
}

func syntheticStationDOCX(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for name, value := range map[string]string{
		"[Content_Types].xml": `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`,
		"_rels/.rels":         `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`,
		"word/document.xml":   `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>合成正文</w:t></w:r></w:p></w:body></w:document>`,
	} {
		part, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func syntheticStationPDF() []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	const stream = "BT /F1 12 Tf 20 40 Td (Synthetic PDF text) Tj ET\n"
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Count 1 /Kids [3 0 R] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 120 60] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(stream), stream), "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var offsets []int
	for i, object := range objects {
		offsets = append(offsets, buf.Len())
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, object)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes()
}

func verifyStationWordCrop(t *testing.T, docx, original []byte, regions []stationIllustrationRect) {
	t.Helper()
	if len(regions) != 1 || regions[0].rectangle() != image.Rect(108, 224, 508, 424) {
		t.Fatalf("unexpected original illustration geometry: %#v", regions)
	}
	source, err := png.Decode(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := zip.NewReader(bytes.NewReader(docx), int64(len(docx)))
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range doc.File {
		if part.Name != "word/media/image1.png" {
			continue
		}
		input, err := part.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(input)
		_ = input.Close()
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != regions[0].SHA256 {
			t.Fatal("the embedded part did not match its verified digest")
		}
		crop, err := png.Decode(bytes.NewReader(data))
		if err != nil || crop.Bounds() != image.Rect(0, 0, 400, 200) {
			t.Fatal("unexpected native illustration dimensions")
		}
		for y := range 200 {
			for x := range 400 {
				if crop.At(x, y) != source.At(x+108, y+224) {
					r, g, b, a := crop.At(x, y).RGBA()
					sr, sg, sb, sa := source.At(x+108, y+224).RGBA()
					if r != sr || g != sg || b != sb || a != sa {
						t.Fatalf("crop differs from original at %d,%d", x, y)
					}
				}
			}
		}
		return
	}
	t.Fatal("Word omitted the original illustration")
}
