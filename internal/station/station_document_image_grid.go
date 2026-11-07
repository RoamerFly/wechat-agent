package station

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"

	"wechat-bot/internal/workspacefs"
)

type stationImageGrid struct {
	Horizontal, Vertical []int
	Merges               []stationTableMerge
	Rules                []image.Rectangle
	Ambiguous            bool
}

type stationImagePreparation struct {
	original image.Image
	grid     stationImageGrid
	regions  []stationIllustrationRect
}

type imageGridRules struct {
	left, right int
	rows        []int
}

// Tesseract's page pass can omit text inside a ruled box. Remove only the
// detected rule bands and illustration regions in a private grayscale copy, without moving any pixel
// coordinates. The OCR broker verifies the copy's own hash; authorization and
// delivery continue to refer to the original image and its verified digest.
func stationPrepareImageOCRInput(ctx context.Context, in stationDocumentInput, dataDir string) (stationDocumentInput, stationImagePreparation, func(), error) {
	empty, noop := stationImagePreparation{}, func() {}
	if err := in.check(ctx); err != nil {
		return in, empty, noop, err
	}
	if in.record.Bytes < 1 || in.record.Bytes > stationOCRImageLimit {
		return in, empty, noop, errors.New("图片 OCR 原件大小无效")
	}
	source, err := io.ReadAll(io.NewSectionReader(in.file, 0, in.record.Bytes))
	if err != nil || int64(len(source)) != in.record.Bytes || fmt.Sprintf("%x", sha256.Sum256(source)) != in.record.SHA256 {
		return in, empty, noop, errors.New("图片 OCR 原件摘要已变化")
	}
	img, _, err := image.Decode(bytes.NewReader(source))
	if err != nil {
		return in, empty, noop, errors.New("图片 OCR 无法完整解码原图")
	}
	grid := stationGridFromImage(img)
	regions, err := stationIllustrations(img, grid, in.illustrationRegions)
	if err != nil {
		return in, empty, noop, err
	}
	preparation := stationImagePreparation{original: img, grid: grid, regions: regions}
	hasGrid := len(grid.Horizontal) >= 2 && len(grid.Vertical) >= 2
	if grid.Ambiguous || !hasGrid && len(regions) == 0 {
		return in, preparation, noop, in.check(ctx)
	}
	bounds := img.Bounds()
	gray := image.NewGray(bounds)
	draw.Draw(gray, bounds, image.White, image.Point{}, draw.Src)
	draw.Draw(gray, bounds, img, bounds.Min, draw.Over)
	erase := func(r image.Rectangle) {
		draw.Draw(gray, r.Intersect(bounds), image.White, image.Point{}, draw.Src)
	}
	for _, rule := range grid.Rules {
		erase(rule)
	}
	for _, region := range regions {
		erase(region.rectangle())
	}
	dir := filepath.Join(dataDir, "project-tool-tasks")
	root, err := workspacefs.EnsurePath(dir)
	if err != nil {
		return in, empty, noop, errors.New("无法准备本机 OCR 临时目录")
	}
	root.Close()
	file, err := os.CreateTemp(dir, "ocr-image-*.png")
	if err != nil {
		return in, empty, noop, errors.New("无法创建本机 OCR 临时副本")
	}
	cleanup := func() { _ = file.Close(); _ = os.Remove(file.Name()) }
	hash := sha256.New()
	err = png.Encode(attachmentGrantWriter{allow: func() bool { return ctx.Err() == nil }, out: io.MultiWriter(file, hash)}, gray)
	size, seekErr := file.Seek(0, io.SeekCurrent)
	if err != nil || seekErr != nil || size > stationOCRImageLimit {
		cleanup()
		return in, empty, noop, errors.New("本机 OCR 临时副本无效或超过大小上限")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return in, empty, noop, errors.New("无法打开本机 OCR 临时副本")
	}
	if err := in.check(ctx); err != nil {
		cleanup()
		return in, empty, noop, err
	}
	in.file, in.extension, in.record.Bytes, in.record.MIME, in.record.SHA256 = file, ".png", size, "image/png", fmt.Sprintf("%x", hash.Sum(nil))
	return in, preparation, cleanup, nil
}

// Only a connected, axis-aligned set of long rules is interpreted as a table.
func stationGridFromImage(img image.Image) stationImageGrid {
	bounds := img.Bounds()
	minWidth := max(96, bounds.Dx()/3)
	tolerance := max(4, bounds.Dx()/50)
	var groups []imageGridRules
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		left, right := longestImageRule(img, y)
		if right-left < minWidth {
			continue
		}
		matched := false
		for i := range groups {
			group := &groups[i]
			if left >= group.left-tolerance && left <= group.left+tolerance &&
				right >= group.right-tolerance && right <= group.right+tolerance {
				group.rows = append(group.rows, y)
				matched = true
				break
			}
		}
		if !matched {
			if len(groups) == 128 {
				return stationImageGrid{}
			}
			groups = append(groups, imageGridRules{left: left, right: right, rows: []int{y}})
		}
	}
	var found stationImageGrid
	remaining := 2 * bounds.Dx() * bounds.Dy()
	for _, group := range groups {
		rows := imageRuleCenters(group.rows)
		if len(rows) < 2 || len(rows) > 51 {
			continue
		}
		top, bottom := rows[0], rows[len(rows)-1]
		if bottom-top < 32 {
			continue
		}
		left, right := max(bounds.Min.X, group.left-tolerance), min(bounds.Max.X-1, group.right+tolerance)
		remaining -= (right - left + 1) * (bottom - top + 1)
		if remaining < 0 {
			return stationImageGrid{}
		}
		if !stationOuterColumn(img, group.left, top, bottom, tolerance) ||
			!stationOuterColumn(img, group.right-1, top, bottom, tolerance) {
			continue
		}
		columns := stationPartialRules(img, true, left, right, top, bottom)
		rows = stationPartialRules(img, false, top, bottom, group.left, group.right-1)
		if len(columns) < 2 || len(rows) < 2 || len(columns) == 2 && len(rows) == 2 {
			continue
		}
		candidate := stationGridSegments(img, rows, columns)
		if len(candidate.Horizontal) == 0 {
			continue
		}
		if len(found.Horizontal) != 0 {
			found.Ambiguous = true
			return found
		}
		found = candidate
	}
	return found
}

func imageRulePixel(img image.Image, x, y int) bool {
	r, g, b, a := img.At(x, y).RGBA()
	return (r+g+b)/3+(0xffff-a) < 0x9999
}

func longestImageRule(img image.Image, y int) (int, int) {
	bounds := img.Bounds()
	left, right, start := 0, 0, bounds.Min.X
	for x := bounds.Min.X; x <= bounds.Max.X; x++ {
		if x < bounds.Max.X && imageRulePixel(img, x, y) {
			continue
		}
		if x-start > right-left {
			left, right = start, x
		}
		start = x + 1
	}
	return left, right
}

func imageRuleCenters(positions []int) []int {
	var centers []int
	for first := 0; first < len(positions); {
		end := first + 1
		for end < len(positions) && positions[end]-positions[end-1] <= 3 {
			end++
		}
		centers = append(centers, (positions[first]+positions[end-1])/2)
		first = end
	}
	return centers
}
