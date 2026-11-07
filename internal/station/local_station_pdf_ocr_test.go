package station

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func TestDocumentOCRTextPreservesReadingOrderAndConfidence(t *testing.T) {
	const header = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\r\n"
	if text, low, valid := readDocumentOCRText(header); text != "" || low || !valid {
		t.Fatalf("blank page reported text: %q, %v, %v", text, low, valid)
	}
	rows := "5\t1\t1\t1\t1\t1\t0\t0\t4\t5\t98\t合成\r\n" +
		"5\t1\t1\t1\t1\t2\t5\t0\t4\t5\t97\t扫描\r\n" +
		"5\t1\t1\t1\t2\t1\t0\t7\t4\t5\t32\t另起一行\r\n"
	if text, low, valid := readDocumentOCRText(header + rows); text != "合成扫描\n另起一行" || !low || !valid {
		t.Fatalf("unexpected OCR text/confidence: %q, %v, %v", text, low, valid)
	}
	for _, row := range []string{"5\t2\t1\t1\t1\t1\t0\t0\t4\t5\t99\t错误页面", "5\t1\t1\t1\t1\t1\t0\t0\t4\t5\tNaN\t异常分数"} {
		if _, _, valid := readDocumentOCRText(header + row); valid {
			t.Fatalf("malformed OCR was accepted: %s", row)
		}
	}
}

func TestPDFScanFailureKeepsNextPageAndHidesDiagnostics(t *testing.T) {
	for _, tc := range []struct{ failure, status, code string }{
		{"pdf_scan_unavailable", "unavailable", "pdf_scan_unavailable"},
		{"pdf_scan_render_timeout", "failed", "pdf_scan_render_timeout"},
		{"private system details", "failed", "pdf_scan_failed"},
	} {
		t.Run(tc.failure, func(t *testing.T) {
			p := stationDocumentPolicy{
				in:         stationDocumentInput{ocrMode: "auto", ocrDPI: 220},
				capability: localDocumentInspect,
				check:      func(context.Context, pluginhost.Job) error { return nil },
				scanPDF: func(context.Context) (pluginruntime.DocumentOCRObservation, error) {
					return pluginruntime.DocumentOCRObservation{}, errors.New(tc.failure)
				},
			}
			view := map[string]any{"status": "no_extractable_text", "text": "", "next_page": 3}
			err := p.applyPDFScan(t.Context(), pluginhost.Job{}, pluginhost.Capability{ID: localDocumentInspect}, view)
			if err != nil || view["next_page"] != 3 || view["text_source"] != "none" || view["ocr_status"] != tc.status ||
				view["ocr_error"] != tc.code || view["text"] != "" {
				t.Fatalf("failed PDF scan lost status or continuation: %#v, %v", view, err)
			}
		})
	}
}

func verifyStationWechatPDFScan(t *testing.T, tools *stationAttachmentTools) {
	t.Helper()
	read := func(args string) map[string]any {
		t.Helper()
		out, err := tools.Call(t.Context(), "wechat_document_inspect", json.RawMessage(args))
		var got map[string]any
		if err != nil || json.Unmarshal([]byte(out), &got) != nil {
			t.Fatalf("PDF read %s: %s, %v", args, out, err)
		}
		return got
	}
	digital := read(`{"message_id":"mixed","page":1}`)
	if digital["text_source"] != "pdf_text_layer" || digital["ocr_status"] != "not_needed" ||
		digital["next_page"] != float64(2) || !strings.Contains(digital["text"].(string), "Digital page") {
		t.Fatalf("PDF text layer was not preserved: %#v", digital)
	}
	disabled := read(`{"message_id":"mixed","page":2,"ocr_mode":"off"}`)
	if disabled["text_source"] != "none" || disabled["ocr_status"] != "disabled" || disabled["text"] != "" {
		t.Fatalf("scan ran despite explicit off: %#v", disabled)
	}
	first := read(fmt.Sprintf(`{"message_id":"mixed","page":2,"ocr_dpi":200,"text_limit":5,"expected_sha256":%q}`, digital["sha256"]))
	if first["text_source"] != "ocr" || first["ocr_dpi"] != float64(200) || first["next_text_offset"] != float64(5) ||
		first["text"] != "文件交接记" || first["next_page"] != nil {
		t.Fatalf("scan page did not provide the first five actual OCR characters: %#v", first)
	}
	rest := read(fmt.Sprintf(`{"message_id":"mixed","page":2,"ocr_dpi":200,"text_offset":5,"expected_sha256":%q}`, first["sha256"]))
	if rest["text_source"] != "ocr" || rest["next_page"] != float64(3) || !strings.Contains(rest["text"].(string), "本次测试用于核对文档复刻结果") {
		t.Fatalf("scan continuation did not finish the same page: %#v", rest)
	}
	blank := read(`{"message_id":"mixed","page":3}`)
	if blank["text_source"] != "none" || blank["ocr_status"] != "no_words" || blank["text"] != "" || blank["next_page"] != nil {
		t.Fatalf("blank page was reported as a read failure or invented text: %#v", blank)
	}
	forced := read(`{"message_id":"mixed","page":1,"ocr_mode":"always"}`)
	if forced["text_source"] != "ocr" || !strings.Contains(forced["text"].(string), "Digital") {
		t.Fatalf("forced OCR did not render a digital PDF page: %#v", forced)
	}
	if _, err := tools.Call(t.Context(), "wechat_document_inspect", json.RawMessage(`{"message_id":"mixed","page":4}`)); err == nil {
		t.Fatal("nonexistent page was accepted")
	}
}
