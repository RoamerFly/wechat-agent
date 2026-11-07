// Package station holds host-side helpers shared by the desktop station.
package station

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	stationImageEvidenceLimit = 2 << 20
	stationImageMaxBlocks     = 500
	stationImageMaxTables     = 100
	stationImageMaxRows       = 200
	stationImageMaxText       = 20_000
)

// stationImageLayout is the bounded editable subset accepted by the document
// workshop's document.reconstruct capability. Uncertain contains locations in
// OCR evidence only; it never repeats unverified text.
type stationImageLayout struct {
	Layout                  json.RawMessage
	Tables                  int
	Paragraphs              int
	Headings                int
	ImageWidth, ImageHeight int
	Illustrations           []stationIllustrationRect `json:",omitempty"`
	MergedCells             int
	BorderlessTables        int
	ReadingColumns          int
	NeedsClarification      bool
	Uncertain               []string
}

type stationImageEvidence struct {
	Kind               string   `json:"kind"`
	NeedsClarification bool     `json:"needs_clarification"`
	QualityFailures    []string `json:"quality_failures"`
	OCR                struct {
		Available bool               `json:"available"`
		Attempted bool               `json:"attempted"`
		Pages     []stationImagePage `json:"pages"`
	} `json:"ocr"`
}

type stationImagePage struct {
	SourcePage int                 `json:"source_page"`
	Blocks     []stationImageBlock `json:"blocks"`
	Tables     []stationImageTable `json:"tables"`
}

type stationImageBlock struct {
	Block int                `json:"block"`
	BBox  stationImageBBox   `json:"bbox"`
	Lines []stationImageLine `json:"lines"`
}

type stationImageLine struct {
	Line       int                `json:"line"`
	Text       string             `json:"text"`
	Confidence *float64           `json:"confidence"`
	BBox       stationImageBBox   `json:"bbox"`
	Words      []stationImageWord `json:"words"`
}

type stationImageWord struct {
	Text       string           `json:"text"`
	Confidence *float64         `json:"confidence"`
	BBox       stationImageBBox `json:"bbox"`
}

type stationImageBBox struct {
	Left   float64 `json:"left"`
	Top    float64 `json:"top"`
	Right  float64 `json:"right"`
	Bottom float64 `json:"bottom"`
}

type stationImageTable struct {
	BBox            stationImageBBox       `json:"bbox"`
	Rows            []stationImageTableRow `json:"rows"`
	Merges          []stationTableMerge
	Borderless      bool `json:"-"`
	CellsUnreadable bool `json:"cells_unreadable"`
}

type stationImageTableRow struct {
	Row   int      `json:"row"`
	Cells []string `json:"cells"`
}

type stationLayoutBlock struct {
	Kind       string              `json:"kind"`
	Text       string              `json:"text,omitempty"`
	Level      int                 `json:"level,omitempty"`
	Rows       [][]string          `json:"rows,omitempty"`
	Merges     []stationTableMerge `json:"merges,omitempty"`
	Borderless bool                `json:"borderless,omitempty"`
	ImageIndex *int                `json:"image_index,omitempty"`
}

type stationVisualBlock struct {
	bbox             stationImageBBox
	order            int
	block            stationLayoutBlock
	leadingLargeLine bool
}

// stationImageWordLayout converts only measured OCR/table evidence into the
// restricted JSON layout consumed by document.reconstruct. It deliberately
// does not guess missing OCR text or table cells. A non-empty result marked
// NeedsClarification must be reviewed by the caller before reconstruction.
func stationImageWordLayout(evidence json.RawMessage, grid stationImageGrid, illustrations ...stationIllustrationRect) (stationImageLayout, error) {
	result := stationImageLayout{Illustrations: illustrations}
	if len(evidence) == 0 || len(evidence) > stationImageEvidenceLimit {
		return markImageLayoutReview(result, "图片识别结果无效，请重新读取原图")
	}
	var source stationImageEvidence
	if err := json.Unmarshal(evidence, &source); err != nil || source.Kind != "image" {
		return markImageLayoutReview(result, "图片识别结果格式无效，请重新读取原图")
	}
	if !source.OCR.Attempted || !source.OCR.Available {
		return markImageLayoutReview(result, "图片文字识别不可用，请检查本机 OCR 后重试")
	}
	if len(source.OCR.Pages) == 0 || len(source.OCR.Pages) > 1 {
		return markImageLayoutReview(result, "图片文字识别缺少单页布局证据，请重新读取原图")
	}
	page := source.OCR.Pages[0]
	if len(page.Blocks) > stationImageMaxBlocks || len(page.Tables) > stationImageMaxTables {
		return markImageLayoutReview(result, "图片布局内容过多，请拆分图片后重试")
	}
	if stationOCRNeedsReview(page) {
		// Repeat the runtime's confidence rule at this trust boundary. A stale
		// or hand-crafted observation cannot turn low-confidence text into an
		// apparently verified editable document just by omitting its flag.
		result.NeedsClarification = true
	}
	for _, reason := range source.QualityFailures {
		if reason != "" {
			result.NeedsClarification = true
			break
		}
	}
	if source.NeedsClarification {
		result.NeedsClarification = true
	}

	// The project-local runtime intentionally has no Pillow. When it therefore
	// cannot offer native OCR table rows, the host's measured grid is enough to
	// place the already measured TSV words into real cells. This is still OCR
	// evidence, never guessed text.
	if grid.Ambiguous {
		return markImageLayoutReview(result, "图片表格边界不完整或相互冲突，请核对原图")
	}
	var columnBand *stationImageColumnBand
	if len(page.Tables) == 0 {
		if len(grid.Horizontal) >= 2 && len(grid.Vertical) >= 2 {
			inferred, review := stationTableFromGrid(page, grid)
			if review {
				result.NeedsClarification = true
				result.Uncertain = append(result.Uncertain, stationTableLocation(page.SourcePage, 1))
			} else if inferred != nil {
				page.Tables = append(page.Tables, *inferred)
			}
		} else {
			rows, height := stationImageAlignedRows(page)
			if inferred := stationBorderlessTable(rows, height); inferred != nil {
				page.Tables = append(page.Tables, *inferred)
				result.BorderlessTables = 1
			} else if len(illustrations) == 0 {
				columnBand = stationImageColumns(rows, height)
			}
		}
	}
	visual := make([]stationVisualBlock, 0, len(page.Blocks)+len(page.Tables))
	tableBoxes := make([]stationImageBBox, 0, len(page.Tables))
	for tableIndex, table := range page.Tables {
		if !table.BBox.valid() {
			return markImageLayoutReview(result, "图片表格位置无效，请重新读取原图")
		}
		tableBoxes = append(tableBoxes, table.BBox)
		if table.CellsUnreadable {
			result.NeedsClarification = true
			result.Uncertain = append(result.Uncertain, stationTableLocation(page.SourcePage, tableIndex+1))
			continue
		}
		if len(table.Rows) == 0 {
			continue
		}
		if len(table.Rows) > stationImageMaxRows {
			return markImageLayoutReview(result, "图片表格行数过多，请拆分图片后重试")
		}
		rows := make([][]string, 0, len(table.Rows))
		for rowIndex, row := range table.Rows {
			if len(row.Cells) == 0 || len(row.Cells) > stationImageMaxRows {
				return markImageLayoutReview(result, "图片表格结构无效，请重新读取原图")
			}
			cells := make([]string, len(row.Cells))
			for column, cell := range row.Cells {
				clean, ok := stationImageString(cell)
				if !ok {
					return markImageLayoutReview(result, "图片表格文字无效，请重新读取原图")
				}
				cells[column] = clean
			}
			if !stationRowHasText(cells) && len(table.Merges) == 0 {
				result.NeedsClarification = true
				result.Uncertain = append(result.Uncertain, stationTableLocation(page.SourcePage, tableIndex+1)+":row_"+stationImageNumber(rowIndex+1))
				continue
			}
			rows = append(rows, cells)
		}
		if len(rows) == 0 {
			result.NeedsClarification = true
			continue
		}
		visual = append(visual, stationVisualBlock{bbox: table.BBox, order: tableIndex,
			block: stationLayoutBlock{Kind: "table", Rows: rows, Merges: table.Merges, Borderless: table.Borderless}})
	}

	for i, box := range illustrations {
		visual = append(visual, stationVisualBlock{
			bbox:  stationImageBBox{Left: float64(box.Left), Top: float64(box.Top), Right: float64(box.Right), Bottom: float64(box.Bottom)},
			order: i, block: stationLayoutBlock{Kind: "image", ImageIndex: &i},
		})
	}
	separators := make([]stationImageBBox, len(visual))
	for i, item := range visual {
		separators[i] = item.bbox
	}
	proseMasks := tableBoxes
	if columnBand != nil {
		result.ReadingColumns = len(columnBand.lines)
		separators = append(separators, columnBand.box)
		proseMasks = append(proseMasks, columnBand.box)
		order := 0
		for _, column := range columnBand.lines {
			for _, text := range column {
				visual = append(visual, stationVisualBlock{bbox: columnBand.box, order: order,
					block: stationLayoutBlock{Kind: "paragraph", Text: text}})
				order++
			}
		}
	}
	appendParagraph := func(lines []string, first, second stationImageLine, order int) bool {
		if len(lines) == 0 {
			return true
		}
		text, ok := stationImageText(strings.Join(lines, "\n"))
		if !ok {
			return false
		}
		leadingLargeLine := len(lines) > 1 && first.BBox.Bottom <= second.BBox.Top && stationTypicalGlyphHeight(first) >= 1.25*stationTypicalGlyphHeight(second)
		visual = append(visual, stationVisualBlock{bbox: first.BBox, order: order,
			block: stationLayoutBlock{Kind: "paragraph", Text: text}, leadingLargeLine: leadingLargeLine})
		return true
	}
	for blockIndex, block := range page.Blocks {
		if !block.BBox.valid() || len(block.Lines) == 0 {
			return markImageLayoutReview(result, "图片段落位置无效，请重新读取原图")
		}
		lines := append([]stationImageLine(nil), block.Lines...)
		sort.SliceStable(lines, func(i, j int) bool { return stationBefore(lines[i].BBox, lines[j].BBox, lines[i].Line, lines[j].Line) })
		textLines := make([]string, 0, len(lines))
		var first, second, prior stationImageLine
		for lineIndex, line := range lines {
			if !line.BBox.valid() {
				return markImageLayoutReview(result, "图片文字位置无效，请重新读取原图")
			}
			text, uncertain, err := stationFilteredOCRLine(line, proseMasks)
			if err != nil {
				return markImageLayoutReview(result, "图片文字识别结果无效，请重新读取原图")
			}
			if uncertain {
				result.NeedsClarification = true
				result.Uncertain = append(result.Uncertain, stationLineLocation(page.SourcePage, blockIndex+1, lineIndex+1))
			}
			if text != "" {
				for _, region := range separators {
					if len(textLines) > 0 && prior.BBox.Bottom <= region.Top && line.BBox.Top >= region.Bottom {
						if !appendParagraph(textLines, first, second, blockIndex) {
							return markImageLayoutReview(result, "图片段落文字无效，请重新读取原图")
						}
						textLines = textLines[:0]
						break
					}
				}
				prior = line
				switch len(textLines) {
				case 0:
					first = line
				case 1:
					second = line
				}
				textLines = append(textLines, text)
			}
		}
		if !appendParagraph(textLines, first, second, blockIndex) {
			return markImageLayoutReview(result, "图片段落文字无效，请重新读取原图")
		}
	}
	if len(visual) == len(illustrations) {
		return markImageLayoutReview(result, "图片未识别到可重建的文字或表格，请提供更清晰原图")
	}
	sort.SliceStable(visual, func(i, j int) bool {
		if stationBefore(visual[i].bbox, visual[j].bbox, visual[i].order, visual[j].order) {
			return true
		}
		if stationBefore(visual[j].bbox, visual[i].bbox, visual[j].order, visual[i].order) {
			return false
		}
		return visual[i].block.Kind == "table" && visual[j].block.Kind != "table"
	})
	blocks := make([]stationLayoutBlock, 0, len(visual)+1)
	// Tesseract can group a heading and body text into one paragraph. Its
	// measured first line can still supply the separate Word heading style.
	if visual[0].block.Kind == "paragraph" && (columnBand == nil || visual[0].bbox != columnBand.box) {
		first := &visual[0]
		title, rest, multiline := strings.Cut(first.block.Text, "\n")
		if utf8.RuneCountInString(title) <= 64 {
			switch {
			case first.leadingLargeLine:
				blocks = append(blocks, stationLayoutBlock{Kind: "heading", Text: title, Level: 1})
				result.Headings++
				first.block.Text = rest
			case !multiline && len(visual) > 1:
				first.block.Kind, first.block.Level = "heading", 1
			}
		}
	}
	for _, item := range visual {
		blocks = append(blocks, item.block)
		switch item.block.Kind {
		case "table":
			result.Tables++
			result.MergedCells += len(item.block.Merges)
		case "heading":
			result.Headings++
		case "paragraph":
			result.Paragraphs++
		}
	}
	if len(blocks) > stationImageMaxBlocks {
		return markImageLayoutReview(result, "图片布局内容过多，请拆分图片后重试")
	}
	layout, err := json.Marshal(struct {
		Blocks []stationLayoutBlock `json:"blocks"`
	}{Blocks: blocks})
	if err != nil {
		return markImageLayoutReview(result, "图片布局生成失败，请重新读取原图")
	}
	result.Layout = layout
	if result.NeedsClarification && len(result.Uncertain) == 0 {
		result.Uncertain = append(result.Uncertain, "page_"+stationImageNumber(page.SourcePage)+":ocr")
	}
	result.Uncertain = stationUniqueLocations(result.Uncertain)
	return result, nil
}

// stationTableFromGrid associates OCR words with a grid that the trusted host
// measured from the source image. It does not run OCR itself and cannot fill a
// cell for which no recognized word exists.
func stationTableFromGrid(page stationImagePage, grid stationImageGrid) (*stationImageTable, bool) {
	if !stationStrictlyIncreasing(grid.Horizontal) || !stationStrictlyIncreasing(grid.Vertical) {
		return nil, true
	}
	table := &stationImageTable{BBox: stationImageBBox{
		Left: float64(grid.Vertical[0]), Top: float64(grid.Horizontal[0]),
		Right: float64(grid.Vertical[len(grid.Vertical)-1]), Bottom: float64(grid.Horizontal[len(grid.Horizontal)-1]),
	}}
	if !table.BBox.valid() || len(grid.Horizontal)-1 > stationImageMaxRows || len(grid.Vertical)-1 > stationImageMaxRows {
		return nil, true
	}
	table.Merges = grid.Merges
	type cellWord struct {
		text       string
		line, left float64
	}
	cellWords := make([][][]cellWord, len(grid.Horizontal)-1)
	for row := range cellWords {
		cellWords[row] = make([][]cellWord, len(grid.Vertical)-1)
	}
	for _, block := range page.Blocks {
		for _, line := range block.Lines {
			for _, word := range line.Words {
				if !word.BBox.valid() {
					return nil, true
				}
				text, ok := stationImageText(word.Text)
				if !ok {
					return nil, true
				}
				x, y := int((word.BBox.Left+word.BBox.Right)/2), int((word.BBox.Top+word.BBox.Bottom)/2)
				row, inRow := stationGridSlot(y, grid.Horizontal)
				column, inColumn := stationGridSlot(x, grid.Vertical)
				if inRow && inColumn {
					for _, merge := range grid.Merges {
						if row >= merge.Row && row < merge.Row+merge.RowSpan && column >= merge.Column && column < merge.Column+merge.ColSpan {
							row, column = merge.Row, merge.Column
							break
						}
					}
					cellWords[row][column] = append(cellWords[row][column], cellWord{text: text, line: line.BBox.Top, left: word.BBox.Left})
				}
			}
		}
	}
	found := false
	for row := range cellWords {
		entry := stationImageTableRow{Row: row, Cells: make([]string, len(cellWords[row]))}
		for column := range cellWords[row] {
			measured := cellWords[row][column]
			sort.SliceStable(measured, func(i, j int) bool {
				if measured[i].line != measured[j].line {
					return measured[i].line < measured[j].line
				}
				return measured[i].left < measured[j].left
			})
			var lines []string
			for first := 0; first < len(measured); {
				last := first + 1
				words := []string{measured[first].text}
				for last < len(measured) && measured[last].line == measured[first].line {
					words = append(words, measured[last].text)
					last++
				}
				lines = append(lines, stationJoinOCRWords(words))
				first = last
			}
			entry.Cells[column] = strings.Join(lines, "\n")
			found = found || entry.Cells[column] != ""
		}
		table.Rows = append(table.Rows, entry)
	}
	if !found {
		return nil, true
	}
	return table, false
}

func stationOCRNeedsReview(page stationImagePage) bool {
	var total float64
	count := 0
	for _, block := range page.Blocks {
		for _, line := range block.Lines {
			for _, word := range line.Words {
				if word.Confidence == nil {
					return true
				}
				if *word.Confidence < 40 {
					return true
				}
				total += *word.Confidence
				count++
			}
		}
	}
	return count > 0 && total/float64(count) < 70
}

func stationStrictlyIncreasing(values []int) bool {
	if len(values) < 2 {
		return false
	}
	for index := 1; index < len(values); index++ {
		if values[index] <= values[index-1] {
			return false
		}
	}
	return true
}

func stationGridSlot(value int, edges []int) (int, bool) {
	for index := 0; index < len(edges)-1; index++ {
		if value >= edges[index] && value < edges[index+1] {
			return index, true
		}
	}
	return 0, false
}

func stationTypicalGlyphHeight(line stationImageLine) float64 {
	if len(line.Words) == 0 {
		return line.BBox.Bottom - line.BBox.Top
	}
	heights := make([]float64, 0, len(line.Words))
	for _, word := range line.Words {
		heights = append(heights, word.BBox.Bottom-word.BBox.Top)
	}
	sort.Float64s(heights)
	return heights[len(heights)/2]
}

func stationFilteredOCRLine(line stationImageLine, tableBoxes []stationImageBBox) (string, bool, error) {
	if len(line.Words) == 0 {
		text, ok := stationImageText(line.Text)
		if !ok {
			return "", false, errors.New("line text")
		}
		if stationInAnyTable(line.BBox, tableBoxes) {
			return "", true, nil
		}
		return text, line.Confidence == nil || *line.Confidence < 40, nil
	}
	words := append([]stationImageWord(nil), line.Words...)
	sort.SliceStable(words, func(i, j int) bool { return words[i].BBox.Left < words[j].BBox.Left })
	text := make([]string, 0, len(words))
	uncertain := false
	for _, word := range words {
		if !word.BBox.valid() {
			return "", false, errors.New("word bbox")
		}
		value, ok := stationImageText(word.Text)
		if !ok {
			return "", false, errors.New("word text")
		}
		if stationInAnyTable(word.BBox, tableBoxes) {
			continue
		}
		text = append(text, value)
		if word.Confidence == nil || *word.Confidence < 40 {
			uncertain = true
		}
	}
	if len(text) == 0 {
		return "", uncertain, nil
	}
	return stationJoinOCRWords(text), uncertain, nil
}

// Tesseract TSV often splits a Chinese sentence into several measured words.
// Keeping a synthetic ASCII space between adjacent Han/punctuation runs makes
// an otherwise editable Word document visibly wrong. Latin/digit transitions
// retain one separator so identifiers and English prose remain readable.
func stationJoinOCRWords(words []string) string {
	if len(words) == 0 {
		return ""
	}
	var out strings.Builder
	for index, word := range words {
		if index > 0 && stationOCRNeedsSpace(words[index-1], word) {
			out.WriteByte(' ')
		}
		out.WriteString(word)
	}
	return out.String()
}

func stationOCRNeedsSpace(left, right string) bool {
	leftRune, leftOK := stationLastRune(left)
	rightRune, rightOK := stationFirstRune(right)
	if !leftOK || !rightOK {
		return true
	}
	if stationHanOrChinesePunctuation(leftRune) && stationHanOrChinesePunctuation(rightRune) {
		return false
	}
	return true
}

func stationHanOrChinesePunctuation(value rune) bool {
	return unicode.Is(unicode.Han, value) || strings.ContainsRune("，。！？；：、（）《》【】“”‘’", value)
}

func stationFirstRune(value string) (rune, bool) {
	for _, item := range value {
		return item, true
	}
	return 0, false
}

func stationLastRune(value string) (rune, bool) {
	var result rune
	ok := false
	for _, item := range value {
		result, ok = item, true
	}
	return result, ok
}

func (b stationImageBBox) valid() bool {
	return b.Left >= 0 && b.Top >= 0 && b.Right > b.Left && b.Bottom > b.Top
}
func stationInAnyTable(box stationImageBBox, tables []stationImageBBox) bool {
	x, y := (box.Left+box.Right)/2, (box.Top+box.Bottom)/2
	for _, table := range tables {
		if x >= table.Left && x < table.Right && y >= table.Top && y < table.Bottom {
			return true
		}
	}
	return false
}
func stationBefore(a, b stationImageBBox, aOrder, bOrder int) bool {
	if a.Top != b.Top {
		return a.Top < b.Top
	}
	if a.Left != b.Left {
		return a.Left < b.Left
	}
	return aOrder < bOrder
}
func stationImageText(value string) (string, bool) {
	value, ok := stationImageString(value)
	return value, ok && value != ""
}
func stationImageString(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	value = strings.TrimSpace(strings.ReplaceAll(value, "\x00", ""))
	return value, utf8.RuneCountInString(value) <= stationImageMaxText
}
func stationRowHasText(cells []string) bool {
	for _, cell := range cells {
		if cell != "" {
			return true
		}
	}
	return false
}
func stationImageNumber(value int) string { return strconv.Itoa(value) }
func stationLineLocation(page, block, line int) string {
	return "page_" + stationImageNumber(page) + ":block_" + stationImageNumber(block) + ":line_" + stationImageNumber(line)
}
func stationTableLocation(page, table int) string {
	return "page_" + stationImageNumber(page) + ":table_" + stationImageNumber(table)
}
func stationUniqueLocations(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}
func markImageLayoutReview(result stationImageLayout, message string) (stationImageLayout, error) {
	result.NeedsClarification = true
	return result, errors.New(message)
}
