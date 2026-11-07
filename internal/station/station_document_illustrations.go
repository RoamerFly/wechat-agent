package station

import (
	"archive/zip"
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
	"sort"
	"strings"
)

const stationIllustrationLimit = 20

type stationIllustrationRect struct {
	Left   int    `json:"left"`
	Top    int    `json:"top"`
	Right  int    `json:"right"`
	Bottom int    `json:"bottom"`
	SHA256 string `json:"sha256,omitempty"`
}

func (r stationIllustrationRect) rectangle() image.Rectangle {
	return image.Rectangle{Min: image.Pt(r.Left, r.Top), Max: image.Pt(r.Right, r.Bottom)}
}

func stationIllustrations(img image.Image, grid stationImageGrid, requested []stationIllustrationRect) ([]stationIllustrationRect, error) {
	bounds := img.Bounds()
	var table image.Rectangle
	if len(grid.Horizontal) > 0 && len(grid.Vertical) > 0 {
		table = image.Rect(grid.Vertical[0], grid.Horizontal[0], grid.Vertical[len(grid.Vertical)-1]+1, grid.Horizontal[len(grid.Horizontal)-1]+1)
	}
	regions := append([]stationIllustrationRect(nil), requested...)
	if requested == nil {
		regions = detectStationIllustrations(img, table)
	}
	if len(regions) > stationIllustrationLimit {
		return nil, errors.New("一张原图最多可保留 20 个独立插图区域")
	}
	total := int64(0)
	for i, region := range regions {
		box := region.rectangle()
		if region.SHA256 != "" || box.Empty() || !box.In(bounds) {
			return nil, fmt.Errorf("插图区域 %d 超出 %d×%d 原图边界或坐标无效", i+1, bounds.Dx(), bounds.Dy())
		}
		for _, prior := range regions[:i] {
			if box.Overlaps(prior.rectangle()) {
				return nil, errors.New("插图区域不能相互重叠")
			}
		}
		if box.Overlaps(table) {
			return nil, errors.New("插图区域与可编辑表格重叠，请调整范围")
		}
		total += int64(box.Dx()) * int64(box.Dy())
	}
	if total >= int64(bounds.Dx())*int64(bounds.Dy()) {
		return nil, errors.New("插图区域覆盖了整张原图，请只选独立插图")
	}
	sort.SliceStable(regions, func(i, j int) bool {
		if regions[i].Top != regions[j].Top {
			return regions[i].Top < regions[j].Top
		}
		return regions[i].Left < regions[j].Left
	})
	return regions, nil
}

// Detect separated, substantially colored graphics. Text, rules, margins,
// and flat colored bands remain on the OCR path. Explicit rectangles handle
// illustrations whose grayscale or separated elements have no such signal.
func detectStationIllustrations(img image.Image, table image.Rectangle) []stationIllustrationRect {
	bounds := img.Bounds()
	step := max(12, max(bounds.Dx(), bounds.Dy())/120)
	cols, rows := (bounds.Dx()+step-1)/step, (bounds.Dy()+step-1)/step
	type tile struct {
		box         image.Rectangle
		ink, colors int
	}
	tiles := make([]tile, cols*rows)
	for y := range rows {
		for x := range cols {
			start := bounds.Min.Add(image.Pt(x*step, y*step))
			area := image.Rectangle{Min: start, Max: start.Add(image.Pt(step, step))}.Intersect(bounds)
			if area.Overlaps(table) {
				continue
			}
			var entry tile
			for py := area.Min.Y; py < area.Max.Y; py += 2 {
				for px := area.Min.X; px < area.Max.X; px += 2 {
					r, g, b, a := img.At(px, py).RGBA()
					hi, lo := max(r, g, b), min(r, g, b)
					if a < 0xf000 || hi-lo < 0x2800 || lo > 0xd000 {
						continue
					}
					entry.ink++
					entry.box = entry.box.Union(image.Rect(px, py, px+2, py+2).Intersect(bounds))
					switch hi {
					case r:
						entry.colors |= 1
					case g:
						entry.colors |= 2
					case b:
						entry.colors |= 4
					}
				}
			}
			if entry.ink*12 >= area.Dx()*area.Dy() {
				tiles[y*cols+x] = entry
			}
		}
	}
	var regions []stationIllustrationRect
	for i, entry := range tiles {
		if entry.ink == 0 {
			continue
		}
		pending := []int{i}
		tiles[i] = tile{}
		for len(pending) != 0 {
			at := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			for _, delta := range [...]image.Point{image.Pt(-1, 0), image.Pt(1, 0), image.Pt(0, -1), image.Pt(0, 1)} {
				x, y := at%cols+delta.X, at/cols+delta.Y
				if x < 0 || y < 0 || x >= cols || y >= rows {
					continue
				}
				next := y*cols + x
				if other := tiles[next]; other.ink != 0 {
					tiles[next] = tile{}
					pending = append(pending, next)
					entry.box = entry.box.Union(other.box)
					entry.ink += other.ink
					entry.colors |= other.colors
				}
			}
		}
		box := entry.box
		area := box.Dx() * box.Dy()
		if entry.colors&(entry.colors-1) == 0 || min(box.Dx(), box.Dy()) < max(36, step*3) ||
			area*100 < bounds.Dx()*bounds.Dy() || area*3 > bounds.Dx()*bounds.Dy() ||
			entry.ink*9 < area || !box.In(bounds.Inset(2)) || box.Overlaps(table) {
			continue
		}
		regions = append(regions, stationIllustrationRect{Left: box.Min.X, Top: box.Min.Y, Right: box.Max.X, Bottom: box.Max.Y})
	}
	return regions
}

func stationIllustrationID(i int) string { return fmt.Sprintf("illustration-%d", i+1) }

func (p *stationDocumentImagePolicy) stageIllustrations(ctx context.Context, dir string, original image.Image) error {
	var total int64
	for i, region := range p.layout.Illustrations {
		if err := ctx.Err(); err != nil {
			return err
		}
		box := region.rectangle()
		crop := image.NewNRGBA(image.Rect(0, 0, box.Dx(), box.Dy()))
		draw.Draw(crop, crop.Bounds(), original, box.Min, draw.Src)
		id := stationIllustrationID(i)
		path := filepath.Join(dir, id+".png")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		hash := sha256.New()
		writer := attachmentGrantWriter{allow: func() bool { return ctx.Err() == nil }, out: io.MultiWriter(file, hash)}
		err = png.Encode(writer, crop)
		size, seekErr := file.Seek(0, io.SeekCurrent)
		if err := errors.Join(err, seekErr, file.Close()); err != nil {
			return err
		}
		total += size
		if total > stationOCRImageLimit {
			return errors.New("插图合计超过 32 MiB，请缩小插图范围")
		}
		digest := fmt.Sprintf("%x", hash.Sum(nil))
		p.layout.Illustrations[i].SHA256 = digest
		p.illustrationInputs = append(p.illustrationInputs, map[string]string{
			"artifact_id": id, "path": windowsProjectToolPath(path), "sha256": digest, "mime_type": "image/png",
		})
	}
	return nil
}

func stationVerifyWordIllustrations(file *os.File, size int64, regions []stationIllustrationRect) error {
	if len(regions) == 0 {
		return nil
	}
	invalid := errors.New("Word 中嵌入的插图与已核验原图裁片不匹配")
	doc, err := zip.NewReader(file, size)
	if err != nil {
		return invalid
	}
	pending := make(map[string]string, len(regions))
	for i, region := range regions {
		pending[fmt.Sprintf("word/media/image%d.png", i+1)] = region.SHA256
	}
	for _, part := range doc.File {
		if !strings.HasPrefix(part.Name, "word/media/") {
			continue
		}
		expected, ok := pending[part.Name]
		if !ok || part.UncompressedSize64 > stationOCRImageLimit {
			return invalid
		}
		src, err := part.Open()
		if err != nil {
			return invalid
		}
		hash := sha256.New()
		_, err = io.Copy(hash, io.LimitReader(src, stationOCRImageLimit+1))
		if closeErr := src.Close(); err != nil || closeErr != nil || fmt.Sprintf("%x", hash.Sum(nil)) != expected {
			return invalid
		}
		delete(pending, part.Name)
	}
	if len(pending) != 0 {
		return invalid
	}
	return nil
}
