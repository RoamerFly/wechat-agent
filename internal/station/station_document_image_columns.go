package station

import (
	"math"
	"unicode/utf8"
)

type stationImageColumnBand struct {
	box   stationImageBBox
	lines [][]string
}

// Keep a single, measured band of two or three horizontal prose columns.
// Each gutter must stay empty across multiple rows. A title spanning the
// gutters and a footer separated vertically remain in the normal page flow.
func stationImageColumns(rows []stationAlignedRow, height float64) *stationImageColumnBand {
	for _, row := range rows {
		if len(row.cells) == 0 {
			return nil
		}
	}
	var found *stationImageColumnBand
	for start := 0; start < len(rows); {
		first := rows[start]
		columns := len(first.cells)
		if columns < 2 || columns > 3 {
			start++
			continue
		}
		left := append([]float64(nil), first.ends[:columns-1]...)
		right := append([]float64(nil), first.starts[1:]...)
		end := start + 1
		for end < len(rows) && stationAdjacentColumnRows(rows[end-1], rows[end], height) &&
			stationExtendColumnGutters(rows[end], first.starts, left, right, height) {
			end++
		}
		if end-start >= 3 {
			band := stationBuildColumnBand(rows, start, end, first.starts, left, right, height)
			if band != nil {
				if found != nil {
					return nil // Conflicting bands need a richer page model.
				}
				found = band
			}
		}
		start = end
	}
	return found
}

func stationAdjacentColumnRows(a, b stationAlignedRow, height float64) bool {
	return a.box.Bottom <= b.box.Top && b.center-a.center <= height*3
}

func stationExtendColumnGutters(row stationAlignedRow, starts, left, right []float64, height float64) bool {
	if len(row.cells) != len(starts) {
		return false
	}
	for i, x := range row.starts {
		if math.Abs(x-starts[i]) > height*0.9 {
			return false
		}
		if i > 0 && min(right[i-1], x)-max(left[i-1], row.ends[i-1]) < height*1.3 {
			return false
		}
	}
	for i := range left {
		left[i], right[i] = max(left[i], row.ends[i]), min(right[i], row.starts[i+1])
	}
	return true
}

func stationBuildColumnBand(rows []stationAlignedRow, start, end int, starts, left, right []float64, height float64) *stationImageColumnBand {
	// Short labels and repeated values alone cannot establish prose columns.
	for col := range starts {
		prose := 0
		for _, row := range rows[start:end] {
			if utf8.RuneCountInString(row.cells[col]) >= 9 {
				prose++
			}
		}
		if prose < 2 {
			return nil
		}
	}
	// A shorter column can end first. Keep neighboring, aligned lines in
	// their established column only if the entire line fits one side.
	singleColumn := func(row stationAlignedRow) int {
		if len(row.cells) != 1 {
			return -1
		}
		for col, x := range starts {
			if math.Abs(row.starts[0]-x) <= height*0.9 &&
				(col == 0 || row.starts[0] >= (left[col-1]+right[col-1])/2) &&
				(col == len(left) || row.ends[0] <= (left[col]+right[col])/2) {
				return col
			}
		}
		return -1
	}
	first, last := start, end
	for first > 0 && stationAdjacentColumnRows(rows[first-1], rows[first], height) && singleColumn(rows[first-1]) >= 0 {
		first--
	}
	for last < len(rows) && stationAdjacentColumnRows(rows[last-1], rows[last], height) && singleColumn(rows[last]) >= 0 {
		last++
	}
	if (first > 0 && rows[first-1].box.Bottom > rows[first].box.Top) ||
		(last < len(rows) && rows[last-1].box.Bottom > rows[last].box.Top) {
		return nil
	}
	band := &stationImageColumnBand{box: rows[first].box, lines: make([][]string, len(starts))}
	for i := first; i < last; i++ {
		row := rows[i]
		band.box = stationEncloseBoxes(band.box, row.box)
		if i >= start && i < end {
			for col, text := range row.cells {
				band.lines[col] = append(band.lines[col], text)
			}
		} else {
			col := singleColumn(row)
			band.lines[col] = append(band.lines[col], row.cells[0])
		}
	}
	return band
}
