//go:build ignore

// Generates the extension's placeholder artwork. Run from the extension/
// directory:
//
//	go run ./tools/genlogo/main.go
//
// Writes:
//
//	images/logo.png                    128x128, the Marketplace tile
//	tasks/tf-snag/icon.png              32x32, the drift-check task
//	tasks/tf-snag-install/icon.png      32x32, the installer task
//
// A pipeline task with no icon.png beside its task.json falls back to the
// generic document-and-gears icon in the step list and the task picker, which is
// how you tell a task nobody has finished from one somebody shipped. 32x32 is
// the size Azure DevOps asks for.
//
// All three are flat tiles in Terraform purple: the drift check carries the same
// delta (change) outline as the extension logo, so a step in the run reads as
// the same thing as the tab; the installer carries a download glyph - a solid
// delta pointing into a tray.
//
// Replace any of them with real branding whenever you have it; nothing here
// depends on these being generated.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

// Glyphs are described in a unit square so one definition serves both sizes.
type glyph func(x, y float64) bool

// supersample is the factor each image is rendered at before being box-filtered
// down. Straight rasterisation of a diagonal at 32x32 is visibly jagged; the
// icons are small enough that the edges are most of what you see.
const supersample = 8

func main() {
	targets := []struct {
		path  string
		size  int
		glyph glyph
	}{
		{filepath.Join("images", "logo.png"), 128, delta(0.46)},
		{filepath.Join("tasks", "tf-snag", "icon.png"), 32, delta(0.38)},
		{filepath.Join("tasks", "tf-snag-install", "icon.png"), 32, download},
	}

	for _, t := range targets {
		if err := write(t.path, render(t.size, t.glyph)); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s (%dx%d)", t.path, t.size, t.size)
	}
}

func render(size int, in glyph) *image.NRGBA {
	bg := color.NRGBA{0x7B, 0x42, 0xBC, 0xFF} // Terraform purple
	fg := color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			// Coverage of this pixel, sampled on a supersample x supersample
			// grid, then used to mix the two flat colours.
			hits := 0
			for sy := 0; sy < supersample; sy++ {
				for sx := 0; sx < supersample; sx++ {
					px := (float64(x) + (float64(sx)+0.5)/supersample) / float64(size)
					py := (float64(y) + (float64(sy)+0.5)/supersample) / float64(size)
					if in(px, py) {
						hits++
					}
				}
			}
			img.SetNRGBA(x, y, mix(bg, fg, float64(hits)/float64(supersample*supersample)))
		}
	}
	return img
}

func mix(from, to color.NRGBA, t float64) color.NRGBA {
	lerp := func(a, b uint8) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t + 0.5) }
	return color.NRGBA{lerp(from.R, to.R), lerp(from.G, to.G), lerp(from.B, to.B), 0xFF}
}

func write(path string, img image.Image) error {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

// delta is an upward triangle with a smaller one cut out of it, so the glyph
// reads as an outline rather than a solid. k is how far the cut-out is scaled
// toward the centroid - lower leaves a thicker stroke, which is what the 32x32
// icon needs to stay legible.
func delta(k float64) glyph {
	ax, ay := 0.500, 0.203 // apex
	bx, by := 0.188, 0.797 // base left
	cx, cy := 0.813, 0.797 // base right
	gx, gy := (ax+bx+cx)/3, (ay+by+cy)/3

	return func(px, py float64) bool {
		if !inTriangle(px, py, ax, ay, bx, by, cx, cy) {
			return false
		}
		return !inTriangle(px, py,
			gx+(ax-gx)*k, gy+(ay-gy)*k,
			gx+(bx-gx)*k, gy+(by-gy)*k,
			gx+(cx-gx)*k, gy+(cy-gy)*k)
	}
}

// download is a solid delta pointing down into a tray - the same triangle the
// rest of the artwork uses, arranged as the glyph everyone already reads as
// "fetch this".
func download(px, py float64) bool {
	if inTriangle(px, py, 0.500, 0.660, 0.195, 0.180, 0.805, 0.180) {
		return true
	}
	return px >= 0.180 && px <= 0.820 && py >= 0.760 && py <= 0.870
}

func inTriangle(px, py, ax, ay, bx, by, cx, cy float64) bool {
	d1 := sign(px, py, ax, ay, bx, by)
	d2 := sign(px, py, bx, by, cx, cy)
	d3 := sign(px, py, cx, cy, ax, ay)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func sign(px, py, ax, ay, bx, by float64) float64 {
	return (px-bx)*(ay-by) - (ax-bx)*(py-by)
}
