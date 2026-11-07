package station

import "image"

type stationTableMerge struct {
	Row     int `json:"row"`
	Column  int `json:"column"`
	RowSpan int `json:"row_span"`
	ColSpan int `json:"col_span"`
}

func stationOuterColumn(img image.Image, x, top, bottom, tolerance int) bool {
	for col := max(img.Bounds().Min.X, x-tolerance); col <= min(img.Bounds().Max.X-1, x+tolerance); col++ {
		if stationRuleState(img, true, col, top, bottom) == 1 {
			return true
		}
	}
	return false
}

func stationPartialRules(img image.Image, vertical bool, first, last, start, end int) []int {
	minimum := max(26, (end-start)/12)
	var positions []int
	for axis := first; axis <= last; axis++ {
		run := 0
		for n := start; n <= end; n++ {
			if stationRuleAt(img, vertical, axis, n) {
				run++
				if run >= minimum {
					positions = append(positions, axis)
					break
				}
			} else {
				run = 0
			}
		}
	}
	return imageRuleCenters(positions)
}

func stationRuleAt(img image.Image, vertical bool, axis, along int) bool {
	if vertical {
		return imageRulePixel(img, axis, along)
	}
	return imageRulePixel(img, along, axis)
}

// 1 = visible, 0 = absent, -1 = insufficient evidence to choose either.
func stationRuleState(img image.Image, vertical bool, axis, start, end int) int {
	margin := min(4, (end-start)/5)
	first, last := start+margin, end-margin
	dark, run, longest := 0, 0, 0
	for n := first; n <= last; n++ {
		if stationRuleAt(img, vertical, axis, n) {
			dark++
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	length := last - first + 1
	if dark*100 >= length*88 {
		return 1
	}
	// Letters in a merged cell may cross a theoretical separator.
	if dark*100 <= length*35 && longest*100 <= length*20 {
		return 0
	}
	return -1
}

func stationGridSegments(img image.Image, rows, columns []int) stationImageGrid {
	// A long glyph or shaded cell may generate a candidate axis. Retain only
	// axes that carry a measured edge between two perpendicular candidates.
	filter := func(axes, cross []int, vertical bool) []int {
		kept := []int{axes[0]}
		for _, axis := range axes[1 : len(axes)-1] {
			for j := 0; j+1 < len(cross); j++ {
				if cross[j+1]-cross[j] >= 20 && stationRuleState(img, vertical, axis, cross[j], cross[j+1]) == 1 {
					kept = append(kept, axis)
					break
				}
			}
		}
		return append(kept, axes[len(axes)-1])
	}
	columns = filter(columns, rows, true)
	rows = filter(rows, columns, false)
	columns = filter(columns, rows, true)
	grid := stationImageGrid{Horizontal: rows, Vertical: columns}
	rCount, cCount := len(rows)-1, len(columns)-1
	if rCount == 1 && cCount == 1 {
		return stationImageGrid{}
	}
	if rCount > 50 || cCount > 16 {
		grid.Ambiguous = true
		return grid
	}

	verticalEdges := make([][]bool, rCount)
	horizontalEdges := make([][]bool, rCount+1)
	for r := range horizontalEdges {
		horizontalEdges[r] = make([]bool, cCount)
		for c := range cCount {
			state := stationRuleState(img, false, rows[r], columns[c], columns[c+1])
			if state < 0 || (r == 0 || r == rCount) && state != 1 {
				grid.Ambiguous = true
				return grid
			}
			if state == 1 {
				horizontalEdges[r][c] = true
				grid.Rules = append(grid.Rules, image.Rect(columns[c]-3, rows[r]-3, columns[c+1]+4, rows[r]+4))
			}
		}
	}
	for r := range verticalEdges {
		verticalEdges[r] = make([]bool, cCount+1)
		for c := range cCount + 1 {
			state := stationRuleState(img, true, columns[c], rows[r], rows[r+1])
			if state < 0 || (c == 0 || c == cCount) && state != 1 {
				grid.Ambiguous = true
				return grid
			}
			if state == 1 {
				verticalEdges[r][c] = true
				grid.Rules = append(grid.Rules, image.Rect(columns[c]-3, rows[r]-3, columns[c]+4, rows[r+1]+4))
			}
		}
	}
	grid.Merges, grid.Ambiguous = stationRectangularMerges(horizontalEdges, verticalEdges)
	return grid
}

func stationRectangularMerges(horizontal, vertical [][]bool) ([]stationTableMerge, bool) {
	n, m := len(vertical), len(vertical[0])-1
	var merges []stationTableMerge
	visited := make([]bool, n*m)
	for start := range visited {
		if visited[start] {
			continue
		}
		row, col := start/m, start%m
		top, bottom, left, right := row, row, col, col
		queue := []int{start}
		visited[start] = true
		for i := 0; i < len(queue); i++ {
			at := queue[i]
			r, c := at/m, at%m
			top, bottom, left, right = min(top, r), max(bottom, r), min(left, c), max(right, c)
			add := func(next int, connected bool) {
				if connected && !visited[next] {
					visited[next] = true
					queue = append(queue, next)
				}
			}
			add(at-1, c > 0 && !vertical[r][c])
			add(at+1, c+1 < m && !vertical[r][c+1])
			add(at-m, r > 0 && !horizontal[r][c])
			add(at+m, r+1 < n && !horizontal[r+1][c])
		}
		if len(queue) == 1 {
			continue
		}
		if len(queue) != (bottom-top+1)*(right-left+1) {
			return nil, true
		}
		for r := top; r <= bottom; r++ {
			for c := left; c <= right; c++ {
				if (r > top && horizontal[r][c]) || (c > left && vertical[r][c]) {
					return nil, true
				}
			}
		}
		merges = append(merges, stationTableMerge{Row: top, Column: left, RowSpan: bottom - top + 1, ColSpan: right - left + 1})
	}
	return merges, false
}
