package station

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// This is intentionally an isolated Windows acceptance: it calls the same
// station adapter used by WeChat, but avoids a native upload or real message.
// It proves that the LPAC converter's output is checked and copied before the
// temporary output root is deleted, and that the durable completed job is not
// converted a second time.
func TestLiveStationDocumentExportPDFLPAC(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST") != "1" {
		t.Skip("set AHS_LOCAL_PROJECT_DOCUMENT_LPAC_TEST=1 for Windows document export acceptance")
	}
	base := t.TempDir()
	f := newLocalHomeFixture(t, filepath.Join(base, "host"), filepath.Join(base, "workspace"))
	f.handler.chat.projectTools.options = localProjectToolWindowsOptions(t)
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
	if !home.stationDocumentExportAvailable(t.Context()) {
		t.Fatal("installed Windows document runtime did not expose PDF export")
	}

	docx := syntheticStationDOCX(t)
	sourcePath := filepath.Join(base, "source.docx")
	if err := os.WriteFile(sourcePath, docx, 0600); err != nil {
		t.Fatal(err)
	}
	inputFile, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer inputFile.Close()
	digest := fmt.Sprintf("%x", sha256.Sum256(docx))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	in := stationDocumentInput{
		record: stationAttachmentRecord{ID: "asset-station-export-fixture", Bytes: int64(len(docx)), MIME: localDOCXMIME, SHA256: digest,
			Source: stationAttachmentSource{Account: "fixture", Dataset: "fixture", Thread: "thread", Message: "message", Sender: "owner", Occurred: now, Filename: "source.docx"}},
		file: inputFile, extension: ".docx", epoch: "fixture-epoch", requester: "owner", trace: "fixture-trace", handoffKey: "fixture-handoff",
		check: func(context.Context) error { return nil },
	}
	var persisted int
	var first stationDocumentProduct
	persist := func(_ context.Context, pdf *os.File, product stationDocumentProduct) error {
		persisted++
		if !validStationPDFProduct(product) {
			t.Fatalf("invalid verified product: %#v", product)
		}
		data, err := io.ReadAll(io.LimitReader(pdf, product.Bytes+1))
		if err != nil || int64(len(data)) != product.Bytes || string(data[:5]) != "%PDF-" || fmt.Sprintf("%x", sha256.Sum256(data)) != product.SHA256 {
			t.Fatalf("persist received unverified PDF: bytes=%d product=%#v err=%v", len(data), product, err)
		}
		first = product
		return nil
	}
	product, err := home.exportStationDocumentPDF(t.Context(), in, "converted.pdf", persist)
	if err != nil || product != first || persisted != 1 {
		t.Fatalf("first isolated PDF export product=%#v persisted=%d err=%v", product, persisted, err)
	}
	if entries, err := os.ReadDir(filepath.Join(home.dataDir, "project-tool-tasks")); err != nil || len(entries) != 0 {
		t.Fatalf("export left scoped runtime files: %#v, %v", entries, err)
	}
	if _, err := inputFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	product, err = home.exportStationDocumentPDF(t.Context(), in, "converted.pdf", persist)
	if err != nil || product != first || persisted != 1 {
		t.Fatalf("completed export was not safely reused: product=%#v persisted=%d err=%v", product, persisted, err)
	}
	// A new handoff (for example a different delivery filename) must produce
	// its own prepared record even though the renderer always uses one name.
	in.handoffKey = "another-handoff"
	if _, err := inputFile.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := home.exportStationDocumentPDF(t.Context(), in, "converted.pdf", persist); err != nil || persisted != 2 {
		t.Fatalf("new handoff did not persist a product: persisted=%d err=%v", persisted, err)
	}
}
