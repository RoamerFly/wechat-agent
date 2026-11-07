package station

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStationImageWordLayoutUsesMeasuredTableRowsOnce(t *testing.T) {
	evidence := json.RawMessage(`{
		"kind":"image","needs_clarification":false,
		"ocr":{"available":true,"attempted":true,"pages":[{
			"source_page":1,
			"blocks":[
				{"block":1,"bbox":{"left":10,"top":10,"right":310,"bottom":40},"lines":[{"line":1,"text":"活动安排","confidence":96,"bbox":{"left":10,"top":10,"right":150,"bottom":40},"words":[{"text":"活动安排","confidence":96,"bbox":{"left":10,"top":10,"right":150,"bottom":40}}]}]},
				{"block":2,"bbox":{"left":10,"top":80,"right":310,"bottom":180},"lines":[{"line":1,"text":"项目 数量","confidence":97,"bbox":{"left":20,"top":90,"right":200,"bottom":110},"words":[{"text":"项目","confidence":97,"bbox":{"left":20,"top":90,"right":70,"bottom":110}},{"text":"数量","confidence":97,"bbox":{"left":160,"top":90,"right":210,"bottom":110}}]},{"line":2,"text":"苹果 2","confidence":96,"bbox":{"left":20,"top":140,"right":190,"bottom":160},"words":[{"text":"苹果","confidence":96,"bbox":{"left":20,"top":140,"right":70,"bottom":160}},{"text":"2","confidence":96,"bbox":{"left":170,"top":140,"right":180,"bottom":160}}]}]},
				{"block":3,"bbox":{"left":10,"top":230,"right":310,"bottom":260},"lines":[{"line":1,"text":"请按时参加","confidence":95,"bbox":{"left":10,"top":230,"right":130,"bottom":260},"words":[{"text":"请按时参加","confidence":95,"bbox":{"left":10,"top":230,"right":130,"bottom":260}}]}]}
			],
			"tables":[{"bbox":{"left":10,"top":80,"right":310,"bottom":180},"rows":[{"row":0,"cells":["项目","数量"]},{"row":1,"cells":["苹果","2"]}],"cells_unreadable":false}]
		}]}}
	`)
	got, err := stationImageWordLayout(evidence, stationImageGrid{})
	if err != nil {
		t.Fatalf("stationImageWordLayout: %v", err)
	}
	if got.NeedsClarification || got.Tables != 1 || got.Headings != 1 || got.Paragraphs != 1 {
		t.Fatalf("unexpected metadata: %+v", got)
	}
	var layout struct {
		Blocks []struct {
			Kind string     `json:"kind"`
			Text string     `json:"text"`
			Rows [][]string `json:"rows"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(got.Layout, &layout); err != nil {
		t.Fatalf("decode layout: %v", err)
	}
	if len(layout.Blocks) != 3 || layout.Blocks[0].Kind != "heading" || layout.Blocks[0].Text != "活动安排" {
		t.Fatalf("unexpected block order: %#v", layout.Blocks)
	}
	if layout.Blocks[1].Kind != "table" || len(layout.Blocks[1].Rows) != 2 || layout.Blocks[1].Rows[1][0] != "苹果" {
		t.Fatalf("table did not preserve native rows: %#v", layout.Blocks[1])
	}
	if layout.Blocks[2].Kind != "paragraph" || layout.Blocks[2].Text != "请按时参加" {
		t.Fatalf("unexpected prose: %#v", layout.Blocks[2])
	}
	for _, block := range layout.Blocks {
		if strings.Contains(block.Text, "项目") {
			t.Fatalf("table text leaked into prose: %#v", layout.Blocks)
		}
	}
}

func TestStationImageWordLayoutMarksLowConfidenceForReview(t *testing.T) {
	evidence := json.RawMessage(`{"kind":"image","ocr":{"available":true,"attempted":true,"pages":[{"source_page":1,"blocks":[{"block":1,"bbox":{"left":1,"top":1,"right":100,"bottom":30},"lines":[{"line":1,"text":"模糊文字","bbox":{"left":1,"top":1,"right":100,"bottom":30},"words":[{"text":"模糊文字","confidence":32,"bbox":{"left":1,"top":1,"right":100,"bottom":30}}]}]}]}]}}`)
	got, err := stationImageWordLayout(evidence, stationImageGrid{})
	if err != nil {
		t.Fatalf("review evidence should remain inspectable: %v", err)
	}
	if !got.NeedsClarification || len(got.Uncertain) != 1 || got.Layout == nil {
		t.Fatalf("expected explicit review result, got %+v", got)
	}
}

func TestStationImageWordLayoutRejectsMissingOCR(t *testing.T) {
	got, err := stationImageWordLayout(json.RawMessage(`{"kind":"image","ocr":{"available":false,"attempted":true}}`), stationImageGrid{})
	if err == nil || !got.NeedsClarification || strings.Contains(err.Error(), "path") {
		t.Fatalf("expected actionable sanitized failure, got result=%+v err=%v", got, err)
	}
}

func TestStationJoinOCRWordsKeepsChineseNaturalAndLatinReadable(t *testing.T) {
	if got, want := stationJoinOCRWords([]string{"妈妈", "给我", "发来", "视频"}), "妈妈给我发来视频"; got != want {
		t.Fatalf("Chinese OCR words = %q, want %q", got, want)
	}
	if got, want := stationJoinOCRWords([]string{"Agent", "home", "station"}), "Agent home station"; got != want {
		t.Fatalf("Latin OCR words = %q, want %q", got, want)
	}
}

func TestStationImageWordLayoutBuildsNativeRowsFromHostGrid(t *testing.T) {
	evidence := json.RawMessage(`{"kind":"image","ocr":{"available":true,"attempted":true,"pages":[{"source_page":1,"blocks":[
		{"block":1,"bbox":{"left":10,"top":80,"right":300,"bottom":180},"lines":[
			{"line":1,"text":"姓名 年龄","bbox":{"left":20,"top":90,"right":210,"bottom":110},"words":[{"text":"姓名","confidence":95,"bbox":{"left":20,"top":90,"right":70,"bottom":110}},{"text":"年龄","confidence":95,"bbox":{"left":170,"top":90,"right":210,"bottom":110}}]},
			{"line":2,"text":"小鱼 3","bbox":{"left":20,"top":140,"right":180,"bottom":160},"words":[{"text":"小鱼","confidence":95,"bbox":{"left":20,"top":140,"right":70,"bottom":160}},{"text":"3","confidence":95,"bbox":{"left":170,"top":140,"right":180,"bottom":160}}]}]},
		{"block":2,"bbox":{"left":10,"top":220,"right":200,"bottom":250},"lines":[{"line":1,"text":"备注文字","bbox":{"left":10,"top":220,"right":100,"bottom":250},"words":[{"text":"备注文字","confidence":95,"bbox":{"left":10,"top":220,"right":100,"bottom":250}}]}]}
	]}]}}`)
	got, err := stationImageWordLayout(evidence, stationImageGrid{Horizontal: []int{80, 130, 180}, Vertical: []int{10, 150, 300}})
	if err != nil || got.NeedsClarification || got.Tables != 1 || got.Paragraphs != 1 {
		t.Fatalf("grid layout metadata=%+v err=%v", got, err)
	}
	var layout struct {
		Blocks []struct {
			Kind string     `json:"kind"`
			Text string     `json:"text"`
			Rows [][]string `json:"rows"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(got.Layout, &layout); err != nil {
		t.Fatal(err)
	}
	if len(layout.Blocks) != 2 || layout.Blocks[0].Kind != "table" || layout.Blocks[0].Rows[0][0] != "姓名" || layout.Blocks[0].Rows[1][1] != "3" || layout.Blocks[1].Text != "备注文字" {
		t.Fatalf("grid table layout = %#v", layout.Blocks)
	}
}

func TestStationImageWordLayoutPreservesKnownEmptyNativeCell(t *testing.T) {
	evidence := json.RawMessage(`{"kind":"image","ocr":{"available":true,"attempted":true,"pages":[{"source_page":1,"blocks":[{"block":1,"bbox":{"left":1,"top":1,"right":100,"bottom":50},"lines":[{"line":1,"text":"已识别","bbox":{"left":1,"top":1,"right":100,"bottom":50},"words":[{"text":"已识别","confidence":95,"bbox":{"left":1,"top":1,"right":100,"bottom":50}}]}]}],"tables":[{"bbox":{"left":1,"top":1,"right":100,"bottom":50},"rows":[{"row":0,"cells":["已识别",""]}],"cells_unreadable":false}]}]}}`)
	got, err := stationImageWordLayout(evidence, stationImageGrid{})
	if err != nil || got.NeedsClarification || got.Tables != 1 {
		t.Fatalf("metadata=%+v err=%v", got, err)
	}
	var layout struct {
		Blocks []struct {
			Rows [][]string `json:"rows"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(got.Layout, &layout); err != nil {
		t.Fatal(err)
	}
	if len(layout.Blocks) != 1 || len(layout.Blocks[0].Rows) != 1 || layout.Blocks[0].Rows[0][1] != "" {
		t.Fatalf("known empty cell was not preserved: %#v", layout.Blocks)
	}
}
