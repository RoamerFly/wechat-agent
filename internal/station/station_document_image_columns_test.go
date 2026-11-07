package station

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestStationImageColumnReadingOrder(t *testing.T) {
	heading, footer := "跨栏标题用来检查整页阅读顺序", "最后请核对整页内容"
	columns := [][]string{
		{"左栏第一行保留全部文字", "左栏第二行按原来的顺序", "左栏第三行读完再换一栏", "左栏还有一行保留在这里"},
		{"中栏第一行保留全部文字", "中栏第二行按原来的顺序", "中栏第三行接下来是右栏"},
		{"右栏第一行保留全部文字", "右栏第二行按原来的顺序", "右栏第三行读完再核对"},
	}
	for _, tc := range []struct {
		name     string
		count    int
		separate bool
	}{
		{"combined OCR rows", 2, false},
		{"independent OCR blocks", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			confidence := 96.0
			lineFor := func(text string, box stationImageBBox) stationImageLine {
				return stationImageLine{BBox: box, Text: text, Words: []stationImageWord{{Text: text, Confidence: &confidence, BBox: box}}}
			}
			title := lineFor(heading, stationImageBBox{Left: 55, Top: 10, Right: 600, Bottom: 42})
			after := lineFor(footer, stationImageBBox{Left: 30, Top: 340, Right: 280, Bottom: 362})
			page := stationImagePage{SourcePage: 1, Blocks: []stationImageBlock{{BBox: title.BBox, Lines: []stationImageLine{title}}}}
			appendBlock := func(lines []stationImageLine) {
				box := lines[0].BBox
				for _, line := range lines[1:] {
					box = stationEncloseBoxes(box, line.BBox)
				}
				page.Blocks = append(page.Blocks, stationImageBlock{BBox: box, Lines: lines})
			}
			makeLine := func(col, row int, text string) stationImageLine {
				x, y := 30+float64(col)*320, 100+float64(row)*36
				return lineFor(text, stationImageBBox{Left: x, Top: y, Right: x + 200, Bottom: y + 22})
			}
			if tc.separate {
				for col := tc.count - 1; col >= 0; col-- {
					var lines []stationImageLine
					for row, text := range columns[col] {
						lines = append(lines, makeLine(col, row, text))
					}
					appendBlock(lines)
				}
			} else {
				var lines []stationImageLine
				for row := range columns[0] {
					left := makeLine(0, row, columns[0][row])
					if row < len(columns[1]) {
						right := makeLine(1, row, columns[1][row])
						left.BBox = stationEncloseBoxes(left.BBox, right.BBox)
						left.Words = append(left.Words, right.Words...)
					}
					lines = append(lines, left)
				}
				appendBlock(lines)
			}
			appendBlock([]stationImageLine{after})
			evidence := stationImageEvidence{Kind: "image"}
			evidence.OCR.Available, evidence.OCR.Attempted, evidence.OCR.Pages = true, true, []stationImagePage{page}
			raw, err := json.Marshal(evidence)
			if err != nil {
				t.Fatal(err)
			}
			result, err := stationImageWordLayout(raw, stationImageGrid{})
			if err != nil || result.NeedsClarification || result.ReadingColumns != tc.count || result.Tables != 0 || result.Headings != 1 {
				t.Fatalf("layout=%+v error=%v", result, err)
			}
			var layout struct{ Blocks []stationLayoutBlock }
			if err := json.Unmarshal(result.Layout, &layout); err != nil {
				t.Fatal(err)
			}
			expected := []string{heading}
			for _, column := range columns[:tc.count] {
				expected = append(expected, column...)
			}
			expected = append(expected, footer)
			var actual []string
			for _, block := range layout.Blocks {
				actual = append(actual, block.Text)
			}
			if !slices.Equal(actual, expected) {
				t.Fatalf("reading order = %q, want %q", actual, expected)
			}
		})
	}
}
