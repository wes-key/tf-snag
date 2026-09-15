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
//	../.github/assets/logo.png        256x256, the repository README's header
//
// The README copy lives outside extension/ so it is not packaged into the
// .vsix, and is drawn at twice the size the README displays it so it stays sharp
// on high-density screens.
//
// A pipeline task with no icon.png beside its task.json falls back to the
// generic document-and-gears icon in the step list and the task picker, which is
// how you tell a task nobody has finished from one somebody shipped. 32x32 is
// the size Azure DevOps asks for.
//
// All three are rounded tiles in a Terraform-purple gradient. The logo and the
// drift check are a magnifying glass with an amber lens, and in it a purple ~ -
// the marker a Terraform plan puts beside a resource that changed, which is
// exactly what tf-snag looks for - so a step in the run reads as the same thing
// as the tab. The installer is a download arrow into an amber tray. The 32x32
// drift icon is the same drawing with heavier strokes: at that size thin lines
// are what disappear first.
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
	purple = rgb(0x5A2A9C, 1) // the dark end of the tile gradient
)

func main() {
	targets := []struct {
		path   string
		size   int
		layers []layer
	}{
		{filepath.Join("images", "logo.png"), 128, magnifier(false)},
		{filepath.Join("tasks", "tf-snag", "icon.png"), 32, magnifier(true)},
		{filepath.Join("tasks", "tf-snag-install", "icon.png"), 32, install()},
		{filepath.Join("..", ".github", "assets", "logo.png"), 256, magnifier(false)},
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

// magnifier is a white-rimmed amber lens with a handle to the bottom right, and a
// purple ~ in the lens. small thickens the rim, the handle and the ~ for 32x32,
// where the 128x128 strokes would blur into the tile.
func magnifier(small bool) []layer {
	rim, wave := 0.075, 0.045
	if small {
		rim, wave = 0.10, 0.065
	}
	cx, cy, r := 0.45, 0.45, 0.27

	// The handle starts inside the rim so the two join without a seam, and is
	// cut back to the rim's inner edge so its round end does not bulge into the
	// lens.
	handle := capsule(cx+0.19, cy+0.19, 0.78, 0.78, rim*1.15)
	lens := circle(cx, cy, r-rim)
	return []layer{
		tile(),
		{func(x, y float64) bool { return handle(x, y) && !lens(x, y) }, solid(white)},
		{circle(cx, cy, r), solid(amber)},
		{annulus(cx, cy, r-rim, r), solid(white)},
		{tilde(cx-0.13, cx+0.13, cy, 0.05, wave), solid(purple)},
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

// circle is a filled disc centred on (cx, cy).
func circle(cx, cy, r float64) shape {
	return func(x, y float64) bool { return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r }
}

// annulus is a ring centred on (cx, cy) between radii r0 and r1.
func annulus(cx, cy, r0, r1 float64) shape {
	return func(x, y float64) bool {
		d := (x-cx)*(x-cx) + (y-cy)*(y-cy)
		return d >= r0*r0 && d <= r1*r1
	}
}

// capsule is a line from (ax, ay) to (bx, by), r thick either side, with round
// ends.
func capsule(ax, ay, bx, by, r float64) shape {
	return func(x, y float64) bool {
		dx, dy := bx-ax, by-ay
		t := clamp(((x-ax)*dx + (y-ay)*dy) / (dx*dx + dy*dy))
		px, py := ax+t*dx-x, ay+t*dy-y
		return px*px+py*py <= r*r
	}
}

// tilde is one period of a sine wave from x0 to x1 about cy, drawn as a chain of
// capsules so the stroke keeps an even width through the curves and ends round.
func tilde(x0, x1, cy, amplitude, r float64) shape {
	const segments = 48
	point := func(i int) (float64, float64) {
		t := float64(i) / segments
		return x0 + (x1-x0)*t, cy - amplitude*math.Sin(t*2*math.Pi)
	}
	parts := make([]shape, 0, segments)
	for i := 0; i < segments; i++ {
		ax, ay := point(i)
		bx, by := point(i + 1)
		parts = append(parts, capsule(ax, ay, bx, by, r))
	}
	return union(parts...)
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
