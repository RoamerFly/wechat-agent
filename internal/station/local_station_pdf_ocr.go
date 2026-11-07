package station

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"wechat-bot/internal/pluginhost"
	"wechat-bot/internal/pluginruntime"
)

func (p stationDocumentPolicy) applyPDFScan(ctx context.Context, job pluginhost.Job, cap pluginhost.Capability, view map[string]any) error {
	view["text_source"] = "none"
	hasText := view["status"] == "inspected"
	if hasText {
		view["text_source"] = "pdf_text_layer"
	}
	switch {
	case p.scanPDF == nil:
		view["ocr_status"] = "disabled"
		return nil
	case hasText && p.in.ocrMode != "always":
		view["ocr_status"] = "not_needed"
		return nil
	}
	observation, err := p.scanPDF(ctx)
	if checkErr := p.Check(ctx, job, cap); checkErr != nil {
		return checkErr
	}
	view["text"], view["text_source"], view["status"] = "", "none", "no_extractable_text"
	view["text_total_characters"] = 0
	view["ocr_dpi"] = p.in.ocrDPI
	if err != nil {
		code := err.Error()
		if !pluginruntime.SafeDocumentResultCode(code) {
			code = "pdf_scan_failed"
		}
		view["ocr_error"] = code
		view["ocr_status"] = "failed"
		if code == "pdf_scan_unavailable" {
			view["ocr_status"] = "unavailable"
		}
		return nil
	}
	text, lowConfidence, valid := readDocumentOCRText(observation.TSV)
	switch {
	case !valid:
		view["ocr_status"], view["ocr_error"] = "failed", "pdf_scan_invalid_ocr_result"
	case text == "":
		view["ocr_status"] = "no_words"
	default:
		view["text"], view["text_source"], view["status"] = text, "ocr", "inspected"
		view["ocr_status"] = "recognized"
		if lowConfidence {
			view["ocr_status"] = "low_confidence"
		}
	}
	return nil
}

func readDocumentOCRText(tsv string) (string, bool, bool) {
	if !strings.HasPrefix(tsv, "level\tpage_num\t") {
		return "", false, false
	}
	var text strings.Builder
	var words []string
	lastLine := ""
	scoreTotal, count := 0.0, 0
	low := false
	appendLine := func() {
		if len(words) == 0 {
			return
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(stationJoinOCRWords(words))
		words = words[:0]
	}
	for _, row := range strings.Split(tsv, "\n")[1:] {
		columns := strings.SplitN(strings.TrimSuffix(row, "\r"), "\t", 12)
		if len(columns) == 0 || columns[0] != "5" {
			continue
		}
		if len(columns) != 12 || columns[1] != "1" {
			return "", false, false
		}
		word := strings.TrimSpace(columns[11])
		if word == "" {
			continue
		}
		score, err := strconv.ParseFloat(columns[10], 64)
		if err != nil || !(score >= 0 && score <= 100) {
			return "", false, false
		}
		line := strings.Join(columns[2:5], ":")
		if line != lastLine {
			appendLine()
			lastLine = line
		}
		words = append(words, word)
		scoreTotal += score
		count++
		low = low || score < 40
	}
	appendLine()
	return text.String(), low || count > 0 && scoreTotal/float64(count) < 70, true
}

func paginateStationPDF(private json.RawMessage, offset, limit int) (string, error) {
	var view map[string]any
	if json.Unmarshal(private, &view) != nil {
		return "", errors.New("PDF 页面结果格式无效")
	}
	value, ok := view["text"].(string)
	if !ok {
		return "", errors.New("PDF 页面缺少正文结果")
	}
	text := []rune(value)
	if offset > len(text) {
		return "", errors.New("正文偏移超过该 PDF 页的文字长度")
	}
	end := offset + min(limit, len(text)-offset)
	view["text"], view["text_offset"], view["text_total_characters"] = string(text[offset:end]), offset, len(text)
	if end < len(text) {
		view["next_text_offset"] = end
		delete(view, "next_page")
	}
	return localProcessJSON(view)
}
