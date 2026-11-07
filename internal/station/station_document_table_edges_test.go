package station

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStationGridDetectsNativeRectangularMerges(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "synthetic-wechat-merged-table.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	grid := stationGridFromImage(img)
	want := []stationTableMerge{{Row: 0, Column: 0, RowSpan: 1, ColSpan: 3}, {Row: 1, Column: 0, RowSpan: 2, ColSpan: 1}}
	if grid.Ambiguous || !slices.Equal(grid.Merges, want) || len(grid.Horizontal) != 4 || len(grid.Vertical) != 4 {
		t.Fatalf("measured merges = %#v", grid)
	}
	for _, rule := range grid.Rules {
		if image.Pt(405, 320).In(rule) || image.Pt(245, 496).In(rule) {
			t.Fatalf("OCR mask erased an absent separator: %v", rule)
		}
	}
}

func TestStationGridDoesNotInventLShapedMerge(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 360, 340))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	for _, line := range []image.Rectangle{
		image.Rect(30, 35, 330, 40), image.Rect(30, 300, 330, 305),
		image.Rect(30, 35, 35, 305), image.Rect(325, 35, 330, 305),
		image.Rect(177, 167, 182, 305), image.Rect(177, 167, 330, 172),
	} {
		draw.Draw(img, line, image.NewUniform(color.Gray{Y: 20}), image.Point{}, draw.Src)
	}
	if grid := stationGridFromImage(img); !grid.Ambiguous {
		t.Fatalf("L shaped cell did not request review: %#v", grid)
	}
}
