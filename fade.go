package main

import (
	"image"
	"image/color"
	"math"
)

// When the list runs past the top or bottom of the pane, that edge fades
// out, so you can tell there's more. The fades are masks on the list's own
// pixels (its background may be the pane's, showing through), so the viewer
// draws each edge as its own small image cut from the sheet, and places the
// sheet's crop between them.

// fadePt is how tall a fade is, in points; it's rounded to whole rows.
const fadePt = 28.0

// fadeRowsFor is how many cell rows each fade takes, or 0 when the view is
// too short to spare them.
func fadeRowsFor(scale float64, cellH, rows int) int {
	n := max(1, int(math.Round(fadePt*scale/float64(cellH))))
	if rows < 2*n+2 {
		return 0
	}
	return n
}

// edgeFades is how strongly each edge fades, 0 to 1: as much as there is
// more to see that way, up to a fade's height, so a fade grows in as you
// scroll away from an end and is gone when you reach it.
func edgeFades(offsetPx, maxOffsetPx, fadePx int) (top, bottom float64) {
	f := func(more int) float64 { return math.Max(0, math.Min(1, float64(more)/float64(fadePx))) }
	return f(offsetPx), f(maxOffsetPx - offsetPx)
}

// fadeRows fades rows y0 to y0+h of img in place, out toward the top edge
// of that band (fromTop) or its bottom. On a clear image it fades to
// transparent; otherwise toward bg.
func fadeRows(img *image.RGBA, y0, h int, fromTop bool, strength float64, clear bool, bg color.RGBA) {
	if strength <= 0 || h <= 0 {
		return
	}
	b := img.Bounds()
	for y := max(y0, b.Min.Y); y < min(y0+h, b.Max.Y); y++ {
		d := y - y0 // distance from the faded edge
		if !fromTop {
			d = y0 + h - 1 - y
		}
		t := (float64(d) + 0.5) / float64(h)
		k := 1 - strength*(1-t*t*(3-2*t)) // smoothstep in from the edge
		row := img.Pix[img.PixOffset(b.Min.X, y):][:b.Dx()*4]
		for i := 0; i < len(row); i += 4 {
			if clear {
				// Premultiplied, so every channel scales with the alpha.
				for c := 0; c < 4; c++ {
					row[i+c] = uint8(float64(row[i+c])*k + 0.5)
				}
				continue
			}
			bgc := [3]uint8{bg.R, bg.G, bg.B}
			for c := 0; c < 3; c++ {
				row[i+c] = uint8(float64(row[i+c])*k + float64(bgc[c])*(1-k) + 0.5)
			}
		}
	}
}

// fadeBand copies h rows of src from srcY and fades them.
func fadeBand(src *image.RGBA, srcY, h int, fromTop bool, strength float64, clear bool, bg color.RGBA) *image.RGBA {
	w := src.Bounds().Dx()
	band := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := srcY + y
		if sy < 0 || sy >= src.Bounds().Dy() {
			continue
		}
		copy(band.Pix[y*band.Stride:][:w*4], src.Pix[sy*src.Stride:][:w*4])
	}
	fadeRows(band, 0, h, fromTop, strength, clear, bg)
	return band
}
