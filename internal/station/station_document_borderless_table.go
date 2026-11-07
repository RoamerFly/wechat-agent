package station

import (
	"math"
	"sort"
	"unicode/utf8"
)

type stationAlignedRow struct {
	center float64
	box    stationImageBBox
	starts []float64
	ends   []float64
	cells  []string
}

// Restore physical text rows before deciding whether gaps are table cells or
// a column gutter. OCR may return both columns in one line or separate blocks.
func stationImageAlignedRows(page stationImagePage) ([]stationAlignedRow, float64) {
	var words []stationImageWord
	var heights []float64
	for _, block := range page.Blocks {
		for _, line := range block.Lines {
			if len(line.Words) == 0 {
				return nil, 0 // Cannot prove complete assignment from line-only evidence.
			}
			for _, word := range line.Words {
				if !word.BBox.valid() {
					return nil, 0
				}
				words = append(words, word)
				heights = append(heights, word.BBox.Bottom-word.BBox.Top)
			}
		}
	}
	if len(words) == 0 {
		return nil, 0
	}
	sort.Float64s(heights)
	height := heights[len(heights)/2]
	midY := func(w stationImageWord) float64 { return (w.BBox.Top + w.BBox.Bottom) / 2 }
	sort.SliceStable(words, func(i, j int) bool { return midY(words[i]) < midY(words[j]) })

	var rows []stationAlignedRow
	for start := 0; start < len(words); {
		center := midY(words[start])
		end := start + 1
		for end < len(words) && midY(words[end])-center <= height*0.65 {
			end++
		}
		rows = append(rows, stationAlignedTextRow(words[start:end], center, height))
		start = end
	}

	return rows, height
}

// Infer a single regular table only from repeating, widely separated fields.
// Two-column prose, ragged lists, and competing tables retain their OCR text.
func stationBorderlessTable(rows []stationAlignedRow, height float64) *stationImageTable {
	var found *stationImageTable
	for first := 0; first < len(rows); {
		end := first + 1
		for end < len(rows) && stationBorderlessRowMatches(rows[first], rows[end-1], rows[end], height) {
			end++
		}
		separateAbove := first == 0 || rows[first-1].box.Bottom <= rows[first].box.Top
		separateBelow := end == len(rows) || rows[end-1].box.Bottom <= rows[end].box.Top
		if end-first >= 3 && separateAbove && separateBelow && stationBorderlessHasShortField(rows[first:end]) {
			if found != nil {
				return nil
			}
			found = &stationImageTable{BBox: rows[first].box, Borderless: true}
			for _, row := range rows[first:end] {
				found.Rows = append(found.Rows, stationImageTableRow{Row: len(found.Rows), Cells: row.cells})
				found.BBox = stationEncloseBoxes(found.BBox, row.box)
			}
		}
		first = end
	}
	return found
}

func stationAlignedTextRow(words []stationImageWord, center, height float64) stationAlignedRow {
	sort.SliceStable(words, func(i, j int) bool { return words[i].BBox.Left < words[j].BBox.Left })
	row := stationAlignedRow{center: center, box: words[0].BBox}
	right := words[0].BBox.Right
	var field []string
	for _, word := range words {
		value, ok := stationImageText(word.Text)
		if !ok {
			return stationAlignedRow{center: center}
		}
		if len(field) == 0 || word.BBox.Left-right >= height*1.8 {
			if len(field) != 0 {
				row.cells = append(row.cells, stationJoinOCRWords(field))
				row.ends = append(row.ends, right)
				field = field[:0]
			}
			row.starts = append(row.starts, word.BBox.Left)
		}
		field = append(field, value)
		right = max(right, word.BBox.Right)
		row.box = stationEncloseBoxes(row.box, word.BBox)
	}
	row.cells = append(row.cells, stationJoinOCRWords(field))
	row.ends = append(row.ends, right)
	return row
}

func stationBorderlessRowMatches(first, prior, current stationAlignedRow, height float64) bool {
	columns := len(first.starts)
	if columns < 3 || columns > 16 || len(current.starts) != columns {
		return false
	}
	distance := current.center - prior.center
	if distance < height*1.25 || distance > height*4 || prior.box.Bottom > current.box.Top {
		return false
	}
	for i, x := range current.starts {
		if math.Abs(x-first.starts[i]) > height*0.55 {
			return false
		}
	}
	return true
}

func stationBorderlessHasShortField(rows []stationAlignedRow) bool {
	for col := range rows[0].cells {
		short := true
		for _, row := range rows {
			if utf8.RuneCountInString(row.cells[col]) > 8 {
				short = false
				break
			}
		}
		if short {
			return true
		}
	}
	return false
}

func stationEncloseBoxes(a, b stationImageBBox) stationImageBBox {
	return stationImageBBox{Left: min(a.Left, b.Left), Top: min(a.Top, b.Top), Right: max(a.Right, b.Right), Bottom: max(a.Bottom, b.Bottom)}
}
