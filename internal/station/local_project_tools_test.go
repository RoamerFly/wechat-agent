package station

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/pluginruntime"
)

// localProjectToolWindowsOptions deliberately receives its runtime from an
// explicit deployment directory.  It never searches PATH: this test is proof
// of the packaged/isolated Windows path, not a host-Python fallback.
func localProjectToolWindowsOptions(t *testing.T) pluginruntime.Options {
	t.Helper()
	return localProjectToolWindowsOptionsFor(t, pluginruntime.DocumentWorkshopID)
}

func localProjectToolWindowsOptionsFor(t *testing.T, plugin string) pluginruntime.Options {
	t.Helper()
	runtimeRoot := strings.TrimSpace(os.Getenv("AHS_LOCAL_PROJECT_TOOL_RUNTIME_ROOT"))
	if runtimeRoot == "" {
		t.Fatal("set AHS_LOCAL_PROJECT_TOOL_RUNTIME_ROOT to the Windows preview runtimes directory")
	}
	runtimeRoot, err := filepath.Abs(runtimeRoot)
	if err != nil {
		t.Fatal(err)
	}
	// The current signed catalog travels with the tested preview.  Pointing the
	// acceptance test at source-tree plugin files can pair a new runner with an
	// old manifest, which is not a deployable Windows configuration.
	catalog, err := filepath.Abs(filepath.Join(filepath.Dir(runtimeRoot), "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(runtimeRoot, "windows-python-cli")
	options := pluginruntime.Options{
		Python:              filepath.Join(python, "python.exe"),
		Pythonw:             filepath.Join(python, "pythonw.exe"),
		WindowsHelper:       filepath.Join(runtimeRoot, "windows-agent-host", "windows_plugin_host.pyw"),
		RuntimeRoots:        []string{python},
		DocumentRuntimeRoot: filepath.Join(runtimeRoot, "document-workshop"),
		CatalogDirectory:    catalog,
	}
	required := []string{
		options.Python, options.Pythonw, options.WindowsHelper,
		filepath.Join(filepath.Dir(options.WindowsHelper), "windows_lpac.py"),
		filepath.Join(options.CatalogDirectory, plugin, "plugin.json"),
	}
	if plugin == pluginruntime.DocumentWorkshopID {
		required = append(required, filepath.Join(options.DocumentRuntimeRoot, "runtime-manifest.json"))
	}
	for _, path := range required {
		if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() {
			t.Fatalf("Windows project-tool runtime unavailable: %s (%v)", path, statErr)
		}
	}
	return options
}

func localProjectToolsView(t *testing.T, f *localHomeFixture) map[string]any {
	t.Helper()
	return localProjectToolByID(t, f, pluginruntime.DocumentWorkshopID)
}

func localProjectToolByID(t *testing.T, f *localHomeFixture, id string) map[string]any {
	t.Helper()
	code, page := f.request("GET", "/api/local/project-tools", nil, true)
	if code != http.StatusOK {
		t.Fatalf("project tools list: %d %#v", code, page)
	}
	tools, ok := page["tools"].([]any)
	if !ok {
		t.Fatalf("unexpected project tool list: %#v", page)
	}
	for _, value := range tools {
		if tool, ok := value.(map[string]any); ok && tool["id"] == id {
			return tool
		}
	}
	t.Fatalf("project tool %q missing: %#v", id, page)
	return nil
}

func localProjectToolRevision(t *testing.T, tool map[string]any) int {
	t.Helper()
	value, ok := tool["revision"].(float64)
	if !ok || value < 1 {
		t.Fatalf("invalid project tool revision: %#v", tool)
	}
	return int(value)
}

func localArtifactDownload(t *testing.T, f *localHomeFixture, id string) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.server.URL+"/api/local/artifacts/"+id+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", f.server.URL)
	req.Header.Set("X-CSRF-Token", f.csrf)
	response, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Header.Get("Content-Disposition"), "attachment") || len(body) == 0 {
		t.Fatalf("artifact download: %s %#v", response.Status, response.Header)
	}
	return body
}

func localDOCXDocumentXML(t *testing.T, body []byte) string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("artifact is not a DOCX archive: %v", err)
	}
	for _, file := range reader.File {
		if file.Name != "word/document.xml" {
			continue
		}
		stream, openErr := file.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		data, readErr := io.ReadAll(stream)
		stream.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(data)
	}
	t.Fatal("DOCX has no word/document.xml")
	return ""
}

// localPDFExtractText deliberately reads the bytes after the normal download
// endpoint.  It uses the pinned pypdf package staged with the Windows
// document runtime; this is an independent acceptance check of the PDF that
// the LPAC document process actually produced, not an inspection of a host
// scratch path.
func localPDFExtractText(t *testing.T, options pluginruntime.Options, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "downloaded.pdf")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	const script = `import sys
from pathlib import Path
sys.path.insert(0, str(Path(sys.argv[2]) / "python-packages"))
from pypdf import PdfReader
sys.stdout.buffer.write("\\n".join(page.extract_text() or "" for page in PdfReader(sys.argv[1], strict=False).pages).encode("utf-8"))`
	output, err := exec.Command(options.Python, "-I", "-c", script, path, options.DocumentRuntimeRoot).CombinedOutput()
	if err != nil {
		t.Fatalf("downloaded PDF cannot be read by the pinned pypdf runtime: %v: %s", err, output)
	}
	return string(output)
}

// TestLiveLocalProjectToolCreatesImmutableDocumentThroughChat proves the
// first zero-application project-tool story end-to-end.  It has no provider
// model and no WeChat transport: the model is a deterministic local fixture,
// while document-workshop itself runs through the installed MCP + Windows
// LPAC path and produces a real editable DOCX registered in the local
// immutable artifact store.
func TestLiveLocalProjectToolCreatesImmutableDocumentThroughChat(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST") != "1" {
		t.Skip("set AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST=1 on Windows for local project-tool LPAC acceptance")
	}
	options := localProjectToolWindowsOptions(t)
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	f.handler.chat.projectTools.options = options
	if code, _ := f.request("GET", "/api/local/project-tools", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("project tools exposed before local-owner login: %d", code)
	}
	f.login()
	if code, out := f.request("POST", "/api/local/home", map[string]string{"action": "initialize"}, true); code != http.StatusOK {
		t.Fatalf("initialize local home: %d %#v", code, out)
	}

	initial := localProjectToolsView(t, f)
	if initial["catalog_available"] != true || initial["runtime_ready"] != true || initial["pdf_ready"] != false || initial["installed"] != false || initial["state"] != "not_installed" {
		t.Fatalf("unexpected zero-app tool state: %#v", initial)
	}
	if code, _ := f.request("PUT", "/api/local/project-tools/document-workshop", map[string]any{"action": "install"}, true); code != http.StatusBadRequest {
		t.Fatalf("document tool installed without artifact permission: %d", code)
	}
	if code, out := f.request("PUT", "/api/local/project-tools/document-workshop", map[string]any{"action": "install", "approve_artifacts": true}, true); code != http.StatusOK {
		t.Fatalf("install document tool: %d %#v", code, out)
	}
	installed := localProjectToolsView(t, f)
	if installed["installed"] != true || installed["enabled"] != false || installed["state"] != "disabled" {
		t.Fatalf("document tool was not installed but disabled: %#v", installed)
	}
	if code, out := f.request("PUT", "/api/local/project-tools/document-workshop", map[string]any{"action": "enable", "revision": localProjectToolRevision(t, installed)}, true); code != http.StatusOK {
		t.Fatalf("enable document tool: %d %#v", code, out)
	}
	enabled := localProjectToolsView(t, f)
	if enabled["enabled"] != true || enabled["pdf_ready"] != true || enabled["state"] != "idle" {
		t.Fatalf("document tool not idle before chat invocation: %#v", enabled)
	}

	const paragraph = "大肥鱼的本机工具成品验收。"
	const cell = "可编辑表格单元格"
	const filename = "local-project-acceptance.docx"
	args := map[string]any{
		"layout": map[string]any{"blocks": []any{
			map[string]any{"kind": "paragraph", "text": paragraph},
			map[string]any{"kind": "table", "rows": []any{[]any{"项目", "结论"}, []any{"本机成品", cell}}},
		}},
		"filename": filename,
		"title":    "本机项目工具验收",
	}
	captureArtifacts := func(turns []map[string]string) []string {
		all := localModelHistory(turns)
		seen, ids := map[string]bool{}, []string{}
		for offset := 0; ; {
			start := strings.Index(all[offset:], "work-")
			if start < 0 {
				break
			}
			start += offset
			if len(all) >= start+69 {
				candidate := all[start : start+69]
				if artifactID(candidate, "work-") && !seen[candidate] {
					seen[candidate] = true
					ids = append(ids, candidate)
				}
			}
			offset = start + len("work-")
		}
		return ids
	}
	captureArtifact := func(turns []map[string]string) string {
		ids := captureArtifacts(turns)
		if len(ids) > 0 {
			return ids[len(ids)-1]
		}
		t.Error("project tool result did not return a registered artifact id", localModelHistory(turns))
		return ""
	}
	var artifact, pdfArtifact string
	calls := 0
	f.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		return localModelFunc(func(_ context.Context, system string, turns []map[string]string) (string, error) {
			calls++
			switch calls {
			case 1:
				for _, tool := range []string{"project_document_create", "project_document_inspect", "project_document_export_pdf"} {
					if !strings.Contains(system, tool) {
						t.Errorf("local chat did not disclose the installed project document tool: %s", tool)
					}
				}
				return localToolCall("project_document_create", args), nil
			case 2:
				artifact = captureArtifact(turns)
				return localToolCall("project_document_inspect", map[string]any{"artifact_id": artifact, "include_layout": true}), nil
			case 3:
				all := localModelHistory(turns)
				if !strings.Contains(all, `\"status\":\"inspected\"`) || !strings.Contains(all, paragraph) || !strings.Contains(all, cell) {
					t.Error("DOCX inspection did not return the actual editable text and layout evidence", all)
				}
				return localToolCall("project_document_export_pdf", map[string]any{"artifact_id": artifact, "filename": "local-project-acceptance.pdf"}), nil
			default:
				for _, candidate := range captureArtifacts(turns) {
					if candidate != artifact {
						pdfArtifact = candidate
					}
				}
				return `{"action":"reply","messages":["文档和 PDF 已生成并登记：` + artifact + `、` + pdfArtifact + `。"]}`, nil
			}
		}), nil
	}
	if code, out := localChatSend(f, "local-document-project-tool", "做一份可编辑的中文文档并保存成品"); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("normal local chat document flow: %d %#v", code, out)
	}
	if calls != 4 || !artifactID(artifact, "work-") || !artifactID(pdfArtifact, "work-") || artifact == pdfArtifact {
		t.Fatalf("unexpected local document calls=%d DOCX=%q PDF=%q", calls, artifact, pdfArtifact)
	}
	if code, view := f.request("GET", "/api/local/artifacts/"+artifact, nil, true); code != http.StatusOK || view["filename"] != filename || view["status"] != "registered" {
		t.Fatalf("registered local document artifact missing: %d %#v", code, view)
	}
	xml := localDOCXDocumentXML(t, localArtifactDownload(t, f, artifact))
	if !strings.Contains(xml, paragraph) || !strings.Contains(xml, cell) || !strings.Contains(xml, "<w:tbl") {
		t.Fatalf("downloaded DOCX is not an editable Chinese document: %s", xml)
	}
	if code, view := f.request("GET", "/api/local/artifacts/"+pdfArtifact, nil, true); code != http.StatusOK || view["filename"] != "local-project-acceptance.pdf" || view["input_artifact_id"] != artifact || view["input_sha256"] == "" {
		t.Fatalf("PDF did not preserve DOCX provenance: %d %#v", code, view)
	}
	pdf := localArtifactDownload(t, f, pdfArtifact)
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		t.Fatalf("downloaded PDF does not have a PDF header: %q", pdf[:min(len(pdf), 16)])
	}
	if text := localPDFExtractText(t, options, pdf); !strings.Contains(text, paragraph) || !strings.Contains(text, cell) {
		t.Fatalf("downloaded PDF did not retain actual Chinese DOCX text: %q", text)
	}

	// Verify the already started real session can be recycled without
	// disabling the installed tool or affecting its immutable artifact.
	host := f.handler.chat.projectTools.host
	if state := host.controller.Status(pluginruntime.DocumentWorkshopID); state.State != "running" || !state.AutoManaged {
		t.Fatal("document tool did not hold an on-demand runtime lease", state)
	}
	host.controller.SetIdleTimeout(30 * time.Millisecond)
	until := time.Now().Add(5 * time.Second)
	for host.controller.Status(pluginruntime.DocumentWorkshopID).State != "stopped" && time.Now().Before(until) {
		time.Sleep(20 * time.Millisecond)
	}
	if view := localProjectToolsView(t, f); view["state"] != "idle" || view["enabled"] != true {
		t.Fatalf("idle recycle changed the configured grant or did not stop: %#v", view)
	}

	// A restart must retain the immutable artifact and must replay the original
	// request from durable chat state rather than invoking another generator.
	next := localRestartChat(t, f)
	next.handler.chat.projectTools.options = options
	next.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		t.Error("restart replay called a document generator")
		return nil, context.Canceled
	}
	if code, out := localChatSend(next, "local-document-project-tool", "做一份可编辑的中文文档并保存成品"); code != http.StatusOK || out["reused"] != true {
		t.Fatalf("restart replay regenerated document: %d %#v", code, out)
	}
	if xml = localDOCXDocumentXML(t, localArtifactDownload(t, next, artifact)); !strings.Contains(xml, paragraph) || !strings.Contains(xml, "<w:tbl") {
		t.Fatal("restart lost immutable editable DOCX")
	}

	// Interruption after the project tool, before the final chat reply, also
	// resumes through the original tool checkpoint without starting a process.
	const retryKey, retryText = "local-document-interrupted", "再做一份，保留本机成品"
	interruptedArtifact := ""
	next.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		round := 0
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			round++
			if round == 1 {
				return localToolCall("project_document_create", args), nil
			}
			interruptedArtifact = captureArtifact(turns)
			return "", context.Canceled
		}), nil
	}
	if code, out := localChatSend(next, retryKey, retryText); code != http.StatusBadGateway || !artifactID(interruptedArtifact, "work-") || interruptedArtifact == artifact {
		t.Fatalf("interrupted project tool fixture: %d %#v id=%q", code, out, interruptedArtifact)
	}
	recovered := localRestartChat(t, next)
	recovered.handler.chat.projectTools.options = options
	recovered.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		round := 0
		return localModelFunc(func(_ context.Context, _ string, turns []map[string]string) (string, error) {
			round++
			if round == 1 {
				return localToolCall("project_document_create", args), nil
			}
			id := captureArtifact(turns)
			if id != interruptedArtifact || !strings.Contains(localModelHistory(turns), "recovered") {
				t.Error("resumed document operation did not return the original artifact", id)
			}
			return `{"action":"reply","messages":["文档已登记：` + id + `。"]}`, nil
		}), nil
	}
	if code, out := localChatSend(recovered, retryKey, retryText); code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("could not resume interrupted document reply: %d %#v", code, out)
	}
	if view := localProjectToolsView(t, recovered); view["state"] != "idle" {
		t.Fatalf("resuming interrupted request started the document process again: %#v", view)
	}

	// A corrupted immutable input must be rejected before the export controller
	// is reserved. Retrying after a desktop restart cannot turn that rejection
	// into a second PDF or a duplicate dispatch.
	entries, err := os.ReadDir(filepath.Join(recovered.dir, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	artifactCount := len(entries)
	if err := os.WriteFile(filepath.Join(recovered.dir, "artifacts", artifact, "original"), []byte("tampered local document"), 0600); err != nil {
		t.Fatal(err)
	}
	// The existing PDF has its own export result, but its provenance still
	// reaches the source DOCX. A later parent-integrity failure must therefore
	// revoke this derived artifact before either its metadata or bytes are
	// exposed.
	if code, _ := recovered.request("GET", "/api/local/artifacts/"+pdfArtifact, nil, true); code == http.StatusOK {
		t.Fatal("derived PDF remained readable after its DOCX parent was tampered")
	}
	deniedExport := func(fixture *localHomeFixture, key string) int {
		fixture.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
			round := 0
			return localModelFunc(func(_ context.Context, _ string, _ []map[string]string) (string, error) {
				round++
				if round == 1 {
					return localToolCall("project_document_export_pdf", map[string]any{"artifact_id": artifact, "filename": "must-not-exist.pdf"}), nil
				}
				return "", errors.New("synthetic stop after denied artifact input")
			}), nil
		}
		code, _ := localChatSend(fixture, key, "把已有文档导出 PDF")
		return code
	}
	if code := deniedExport(recovered, "local-document-tampered"); code == http.StatusOK {
		t.Fatal("tampered DOCX was exported")
	}
	if state := recovered.handler.chat.projectTools.host.controller.Status(pluginruntime.DocumentWorkshopID); state.State != "stopped" {
		t.Fatalf("tampered input reached a project tool process: %#v", state)
	}
	entries, err = os.ReadDir(filepath.Join(recovered.dir, "artifacts"))
	if err != nil || len(entries) != artifactCount {
		t.Fatalf("tampered export wrote a new artifact: before=%d after=%d err=%v", artifactCount, len(entries), err)
	}
	retryDenied := localRestartChat(t, recovered)
	retryDenied.handler.chat.projectTools.options = options
	if code := deniedExport(retryDenied, "local-document-tampered"); code == http.StatusOK {
		t.Fatal("restart replay exported a tampered DOCX")
	}
	if state := retryDenied.handler.chat.projectTools.host.controller.Status(pluginruntime.DocumentWorkshopID); state.State != "stopped" {
		t.Fatalf("restart retry reached a project tool process: %#v", state)
	}
	entries, err = os.ReadDir(filepath.Join(retryDenied.dir, "artifacts"))
	if err != nil || len(entries) != artifactCount {
		t.Fatalf("restart retry wrote a duplicate artifact: before=%d after=%d err=%v", artifactCount, len(entries), err)
	}

	// Forgetting the current source is an independent denial: even a model that
	// attempts to use the old artifact immediately afterward cannot dispatch an
	// inspection or revive that request after restart.
	store, input, err := retryDenied.handler.chat.conversation()
	if err != nil {
		t.Fatal(err)
	}
	scope := companion.ScopeID(input.Channel, input.Account, input.Thread)
	retryDenied.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		round := 0
		return localModelFunc(func(_ context.Context, _ string, _ []map[string]string) (string, error) {
			round++
			if round == 1 {
				if clearErr := store.Clear(scope); clearErr != nil {
					t.Fatal(clearErr)
				}
				return localToolCall("project_document_inspect", map[string]any{"artifact_id": artifact, "include_layout": true}), nil
			}
			return "", errors.New("synthetic stop after forgotten source")
		}), nil
	}
	if code, _ := localChatSend(retryDenied, "local-document-forgotten", "忘记后不要再读取原文档"); code == http.StatusOK {
		t.Fatal("forgotten source reached document inspection")
	}
	if state := retryDenied.handler.chat.projectTools.host.controller.Status(pluginruntime.DocumentWorkshopID); state.State != "stopped" {
		t.Fatalf("forgotten source reached a project tool process: %#v", state)
	}
	forgottenRestart := localRestartChat(t, retryDenied)
	forgottenRestart.handler.chat.projectTools.options = options
	forgottenRestart.handler.chat.modelFactory = func() (companion.LanguageModel, error) {
		t.Error("forgotten request was resurrected after restart")
		return nil, errors.New("unexpected forgotten replay")
	}
	if code, _ := localChatSend(forgottenRestart, "local-document-forgotten", "忘记后不要再读取原文档"); code == http.StatusOK {
		t.Fatal("forgotten request was accepted after restart")
	}
}
