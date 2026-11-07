package station

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStationBorderlessTableReconstructsAlignedRowsInReadingOrder(t *testing.T) {
	cases := []struct {
		name    string
		offsets [][]float64
		want    bool
	}{
		{"three aligned fields", [][]float64{{30, 200, 400}, {32, 204, 397}, {30, 199, 402}}, true},
		{"two columns of prose", [][]float64{{30, 200}, {30, 200}, {30, 200}}, false},
		{"uneven tab stops", [][]float64{{30, 200, 400}, {32, 285, 397}, {30, 199, 402}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			confidence := 97.0
			text := [][]string{{"项目", "时间", "结果"}, {"文档", "上午", "通过"}, {"图片", "下午", "完成"}}
			block := stationImageBlock{Block: 1, BBox: stationImageBBox{Left: 30, Top: 10, Right: 500, Bottom: 280}}
			addLine := func(top float64, starts []float64, fields []string) {
				line := stationImageLine{Line: len(block.Lines) + 1}
				for col, x := range starts {
					box := stationImageBBox{Left: x, Top: top, Right: x + 35, Bottom: top + 24}
					line.Words = append(line.Words, stationImageWord{Text: fields[col], Confidence: &confidence, BBox: box})
				}
				line.BBox = stationEncloseBoxes(line.Words[0].BBox, line.Words[len(line.Words)-1].BBox)
				block.Lines = append(block.Lines, line)
			}
			addLine(10, []float64{30}, []string{"登记结果"})
			for i, row := range tc.offsets {
				addLine(80+float64(i)*48, row, text[i])
			}
			addLine(260, []float64{30}, []string{"请核对以上信息"})
			evidence := stationImageEvidence{Kind: "image"}
			evidence.OCR.Attempted, evidence.OCR.Available = true, true
			evidence.OCR.Pages = []stationImagePage{{SourcePage: 1, Blocks: []stationImageBlock{block}}}
			raw, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			result, err := stationImageWordLayout(raw, stationImageGrid{})
			if err != nil || result.NeedsClarification {
				t.Fatalf("layout failed: %+v, %v", result, err)
			}
			if !tc.want {
				if result.Tables != 0 || result.ReadingColumns != 0 {
					t.Fatalf("invented table or prose columns from ambiguous evidence: %s", result.Layout)
				}
				return
			}
			var layout struct {
				Blocks []stationLayoutBlock
			}
			if err := json.Unmarshal(result.Layout, &layout); err != nil {
				t.Fatal(err)
			}
			if result.Tables != 1 || result.BorderlessTables != 1 || len(layout.Blocks) != 3 ||
				layout.Blocks[0].Text != "登记结果" || !layout.Blocks[1].Borderless || !reflect.DeepEqual(layout.Blocks[1].Rows, text) ||
				layout.Blocks[2].Text != "请核对以上信息" {
				t.Fatalf("layout lost original reading order or cells: %s", result.Layout)
			}
		})
	}
}
