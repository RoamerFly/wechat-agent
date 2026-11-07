package station

import (
	"image"
	"image/color"
	"image/draw"
	"testing"
)

func TestStationIllustrationRegionsChooseMeasuredColorAndRespectExplicitBounds(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 600, 600))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	paint := func(box image.Rectangle, shade color.Color) {
		draw.Draw(img, box, image.NewUniform(shade), image.Point{}, draw.Src)
	}
	paint(image.Rect(120, 120, 240, 240), color.RGBA{R: 210, G: 60, B: 20, A: 255})
	paint(image.Rect(240, 120, 360, 240), color.RGBA{R: 30, G: 60, B: 200, A: 255})
	paint(image.Rect(120, 300, 480, 360), color.Gray{Y: 92})

	regions, err := stationIllustrations(img, stationImageGrid{}, nil)
	if err != nil || len(regions) != 1 || regions[0].rectangle() != image.Rect(120, 120, 360, 240) {
		t.Fatalf("measured color illustration = %#v, %v", regions, err)
	}
	regions, err = stationIllustrations(img, stationImageGrid{}, []stationIllustrationRect{})
	if err != nil || len(regions) != 0 {
		t.Fatalf("explicitly disabled illustration detection = %#v, %v", regions, err)
	}
	gray := stationIllustrationRect{Left: 120, Top: 300, Right: 480, Bottom: 360}
	regions, err = stationIllustrations(img, stationImageGrid{}, []stationIllustrationRect{gray})
	if err != nil || len(regions) != 1 || regions[0] != gray {
		t.Fatalf("explicit grayscale illustration = %#v, %v", regions, err)
	}

	for _, tc := range []struct {
		name  string
		grid  stationImageGrid
		input []stationIllustrationRect
	}{
		{"whole page", stationImageGrid{}, []stationIllustrationRect{{Right: 600, Bottom: 600}}},
		{"inverted", stationImageGrid{}, []stationIllustrationRect{{Left: 120, Right: 100, Bottom: 300}}},
		{"out of bounds", stationImageGrid{}, []stationIllustrationRect{{Right: 601, Bottom: 60}}},
		{"overlap", stationImageGrid{}, []stationIllustrationRect{gray, {Left: 240, Top: 330, Right: 420, Bottom: 370}}},
		{"table", stationImageGrid{Horizontal: []int{280, 320, 390}, Vertical: []int{100, 240, 490}}, []stationIllustrationRect{gray}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := stationIllustrations(img, tc.grid, tc.input); err == nil {
				t.Fatal("accepted an invalid illustration crop")
			}
		})
	}
}
