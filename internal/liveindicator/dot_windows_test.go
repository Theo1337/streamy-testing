//go:build windows

package liveindicator

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestDotPixelsHasATransparentCornerAndARedCentre(t *testing.T) {
	const size = 16
	pixels := dotPixels(size)
	if len(pixels) != size*size*4 {
		t.Fatalf("len(dotPixels(%d)) = %d, want %d", size, len(pixels), size*size*4)
	}

	corner := 0 // top-left pixel, BGRA
	if pixels[corner+3] != 0 {
		t.Errorf("corner alpha = %d, want 0", pixels[corner+3])
	}

	centre := ((size/2)*size + size/2) * 4
	if pixels[centre+3] != 255 {
		t.Errorf("centre alpha = %d, want 255", pixels[centre+3])
	}
	if pixels[centre+2] < 200 {
		t.Errorf("centre red = %d, want a bright red dot", pixels[centre+2])
	}
	// Premultiplied alpha means no colour channel may exceed alpha.
	for index := 0; index < len(pixels); index += 4 {
		for channel := 0; channel < 3; channel++ {
			if pixels[index+channel] > pixels[index+3] {
				t.Fatalf("pixel %d channel %d = %d exceeds alpha %d; not premultiplied",
					index/4, channel, pixels[index+channel], pixels[index+3])
			}
		}
	}
}

func TestPositionForCorners(t *testing.T) {
	area := windows.Rect{Left: 0, Top: 0, Right: 1000, Bottom: 800}

	cases := []struct {
		position string
		x, y     int
	}{
		{TopLeft, 10, 10},
		{TopRight, 1000 - 20 - 10, 10},
		{BottomLeft, 10, 800 - 20 - 10},
		{BottomRight, 1000 - 20 - 10, 800 - 20 - 10},
	}
	for _, tc := range cases {
		x, y := positionFor(area, 20, 10, tc.position)
		if x != tc.x || y != tc.y {
			t.Errorf("%s = (%d,%d), want (%d,%d)", tc.position, x, y, tc.x, tc.y)
		}
	}
}
