//go:build ignore

// Generates the extension's artwork. Run from the extension/ directory:
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
// All three are rounded tiles in a Terraform-purple gradient. The logo and the
// drift check show a 2x2 grid of resources with one knocked out of its slot and
// tilted, in amber - drift, in one picture - so a step in the run reads as the
// same thing as the tab. The installer is a download arrow into an amber tray.
// The 32x32 drift icon is the same drawing with heavier geometry: at that size
// thin strokes and narrow gaps are what disappear first.
//
// The Marketplace does not allow brand names or marks in a listing's artwork,
// so nothing here borrows from the Terraform logo beyond its colour.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

// supersample is the factor each image is rendered at before being box-filtered
// down. Straight rasterisation of a diagonal or a corner radius at 32x32 is
// visibly jagged; the icons are small enough that the edges are most of what
// you see.
const supersample = 8

// shape reports whether a point in the unit square is inside it, so one
// definition serves every output size.
type shape func(x, y float64) bool

// paint is a straight (non-premultiplied) colour with alpha, in 0..1.
type paint struct{ r, g, b, a float64 }

// layer is a shape filled with a colour that may vary across the image. Layers
// are composited in order, first at the back.
type layer struct {
	shape shape
	fill  func(x, y float64) paint
}

var (
	white  = rgb(0xFFFFFF, 1)
	amber  = rgb(0xFFB547, 1)
	ghost  = rgb(0xFFFFFF, 0.55) // the empty slot the drifted block left
	shadow = rgb(0x2A0E55, 0.35)
)

func main() {
	targets := []struct {
		path   string
		size   int
		layers []layer
	}{
		{filepath.Join("images", "logo.png"), 128, drift(false)},
		{filepath.Join("tasks", "tf-snag", "icon.png"), 32, drift(true)},
		{filepath.Join("tasks", "tf-snag-install", "icon.png"), 32, install()},
	}

	for _, t := range targets {
		if err := write(t.path, render(t.size, t.layers)); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s (%dx%d)", t.path, t.size, t.size)
	}
}

// --- artwork ---------------------------------------------------------------

// tile is the rounded background, a diagonal gradient either side of Terraform
// purple (#7B42BC). Outside its corners the image is transparent.
func tile() layer {
	from, to := rgb(0x9557E0, 1), rgb(0x5A2A9C, 1)
	return layer{roundRect(0, 0, 1, 1, 0.22), func(x, y float64) paint {
		t := clamp((x + y) / 2)
		return paint{lerp(from.r, to.r, t), lerp(from.g, to.g, t), lerp(from.b, to.b, t), 1}
	}}
}

// drift is three resources in place, the outline of the slot the fourth one
// should be in, and that fourth one knocked up and out of it, tilted. The grid
// sits low and left so the drifted block has room inside the tile.
func drift(small bool) []layer {
	left, top, span, gap, radius, strokeWidth := 0.15, 0.31, 0.56, 0.075, 0.035, 0.03
	dx, dy, tilt := 0.155, -0.155, 16.0
	if small {
		left, top, span, gap, radius, strokeWidth = 0.12, 0.30, 0.60, 0.10, 0.04, 0.06
		dx, dy, tilt = 0.14, -0.14, 14
	}

	cell := (span - gap) / 2
	col := [2][2]float64{{left, left + cell}, {left + cell + gap, left + span}}
	row := [2][2]float64{{top, top + cell}, {top + cell + gap, top + span}}

	// The slot is the top-right cell; the block is that cell moved by (dx, dy)
	// and turned about its own centre.
	sx0, sy0, sx1, sy1 := col[1][0], row[0][0], col[1][1], row[0][1]
	cx, cy := (sx0+sx1)/2+dx, (sy0+sy1)/2+dy
	block := func(ox, oy float64) shape {
		return rotate(roundRect(sx0+dx+ox, sy0+dy+oy, sx1+dx+ox, sy1+dy+oy, radius), cx, cy, tilt)
	}

	return []layer{
		tile(),
		{roundRect(col[0][0], row[0][0], col[0][1], row[0][1], radius), solid(white)},
		{roundRect(col[0][0], row[1][0], col[0][1], row[1][1], radius), solid(white)},
		{roundRect(col[1][0], row[1][0], col[1][1], row[1][1], radius), solid(white)},
		{outline(sx0, sy0, sx1, sy1, radius, strokeWidth), solid(ghost)},
		{block(0.012, 0.025), solid(shadow)},
		{block(0, 0), solid(amber)},
	}
}

// install is a download arrow dropping into an amber tray.
func install() []layer {
	arrow := union(
		roundRect(0.43, 0.16, 0.57, 0.52, 0.02),
		polygon([2]float64{0.25, 0.44}, [2]float64{0.75, 0.44}, [2]float64{0.50, 0.70}),
	)
	return []layer{
		tile(),
		{arrow, solid(white)},
		{roundRect(0.18, 0.76, 0.82, 0.88, 0.04), solid(amber)},
	}
}

// --- shapes ----------------------------------------------------------------

func roundRect(x0, y0, x1, y1, r float64) shape {
	return func(x, y float64) bool {
		if x < x0 || x > x1 || y < y0 || y > y1 {
			return false
		}
		// Distance to the nearest point of the rectangle inset by r.
		nx := math.Max(x0+r, math.Min(x, x1-r))
		ny := math.Max(y0+r, math.Min(y, y1-r))
		return (x-nx)*(x-nx)+(y-ny)*(y-ny) <= r*r
	}
}

// outline is a rounded rectangle's border, w wide, drawn inside its bounds.
func outline(x0, y0, x1, y1, r, w float64) shape {
	outer := roundRect(x0, y0, x1, y1, r)
	inner := roundRect(x0+w, y0+w, x1-w, y1-w, math.Max(r-w, 0))
	return func(x, y float64) bool { return outer(x, y) && !inner(x, y) }
}

// rotate turns a shape clockwise by deg about (cx, cy).
func rotate(s shape, cx, cy, deg float64) shape {
	a := -deg * math.Pi / 180
	sin, cos := math.Sin(a), math.Cos(a)
	return func(x, y float64) bool {
		dx, dy := x-cx, y-cy
		return s(cx+dx*cos-dy*sin, cy+dx*sin+dy*cos)
	}
}

// polygon is an even-odd fill of the given vertices.
func polygon(pts ...[2]float64) shape {
	return func(x, y float64) bool {
		in := false
		for i, j := 0, len(pts)-1; i < len(pts); j, i = i, i+1 {
			xi, yi, xj, yj := pts[i][0], pts[i][1], pts[j][0], pts[j][1]
			if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
				in = !in
			}
		}
		return in
	}
}

func union(shapes ...shape) shape {
	return func(x, y float64) bool {
		for _, s := range shapes {
			if s(x, y) {
				return true
			}
		}
		return false
	}
}

// --- rendering -------------------------------------------------------------

func render(size int, layers []layer) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	samples := float64(supersample * supersample)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			// Premultiplied sums over the sample grid, so partially covered
			// edge pixels blend correctly against transparency.
			var sum paint
			for sy := 0; sy < supersample; sy++ {
				for sx := 0; sx < supersample; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/supersample) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/supersample) / float64(size)
					var c paint
					for _, l := range layers {
						if !l.shape(x, y) {
							continue
						}
						f := l.fill(x, y)
						c.r = f.r*f.a + c.r*(1-f.a)
						c.g = f.g*f.a + c.g*(1-f.a)
						c.b = f.b*f.a + c.b*(1-f.a)
						c.a = f.a + c.a*(1-f.a)
					}
					sum.r += c.r
					sum.g += c.g
					sum.b += c.b
					sum.a += c.a
				}
			}
			if sum.a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{
				R: byte255(sum.r / sum.a),
				G: byte255(sum.g / sum.a),
				B: byte255(sum.b / sum.a),
				A: byte255(sum.a / samples),
			})
		}
	}
	return img
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

// --- colour ----------------------------------------------------------------

func rgb(hex uint32, alpha float64) paint {
	return paint{float64(hex>>16&0xFF) / 255, float64(hex>>8&0xFF) / 255, float64(hex&0xFF) / 255, alpha}
}

func solid(p paint) func(x, y float64) paint {
	return func(float64, float64) paint { return p }
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func clamp(v float64) float64 { return math.Min(math.Max(v, 0), 1) }

func byte255(v float64) uint8 { return uint8(math.Round(clamp(v) * 255)) }
