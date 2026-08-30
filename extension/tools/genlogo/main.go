//go:build ignore

// Generates extension/images/logo.png: a flat tile with a white delta (change)
// glyph. Run from the extension/ directory:
//
//	go run ./tools/genlogo/main.go
//
// Replace images/logo.png with real branding whenever you have it; the manifest
// just needs a 128x128 PNG at that path.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

const size = 128

func main() {
	bg := color.NRGBA{0x7B, 0x42, 0xBC, 0xFF}  // Terraform purple
	fg := color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}

	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if inTriangle(float64(x), float64(y)) {
				img.SetNRGBA(x, y, fg)
			} else {
				img.SetNRGBA(x, y, bg)
			}
		}
	}

	out := filepath.Join("images", "logo.png")
	if err := os.MkdirAll("images", 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(out)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%dx%d)", out, size, size)
}

// inTriangle reports whether (px,py) is inside an upward delta with a little
// notch cut out, so the glyph reads as a triangle outline rather than a solid.
func inTriangle(px, py float64) bool {
	ax, ay := 64.0, 26.0    // apex
	bx, by := 24.0, 102.0   // base left
	cx, cy := 104.0, 102.0  // base right

	if !pointInTri(px, py, ax, ay, bx, by, cx, cy) {
		return false
	}
	// inner cut-out: same triangle scaled toward the centroid
	gx, gy := (ax+bx+cx)/3, (ay+by+cy)/3
	const k = 0.46
	return !pointInTri(px, py,
		gx+(ax-gx)*k, gy+(ay-gy)*k,
		gx+(bx-gx)*k, gy+(by-gy)*k,
		gx+(cx-gx)*k, gy+(cy-gy)*k)
}

func pointInTri(px, py, ax, ay, bx, by, cx, cy float64) bool {
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
