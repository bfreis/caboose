package macui

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/menubar"
)

// The status item's images are drawn here, from caboose's logo
// (docs/assets/logo.svg) reduced to what reads at 18 points: the railcar's
// silhouette (the cupola under its overhanging roof, the roof overhanging
// the body, the underframe) and two wheels below it, without the logo's
// slats, railings and lamp, and shortened as the app's icon is. Every edge but the wheels' lies on the 1x
// pixel grid, so both scales are crisp. Running is the silhouette filled,
// with the cupola's window cut out and a hub in each wheel; idle is the same silhouette's outline,
// one point wide, with the roof's lower edge drawn across, and the wheels
// as rings. At 1x the wheels too are whole pixels. The test renders them and holds
// the committed files (icons/) to the drawing; after a change to it,
//
//	go test ./internal/macui -run TestIcons -update
//
// writes the PNGs and the SVGs (the same drawing, for viewing and editing
// elsewhere) again; a run without -update then checks them.

var updateIcons = flag.Bool("update", false, "write the icons in icons/ from the drawing")

const (
	iconW = iconPointsW
	iconH = iconPointsH
)

// box is a rectangle in points, from the top left; x1 and y1 excluded.
type box struct{ x0, y0, x1, y1 int }

func (b box) has(x, y float64) bool {
	return x >= float64(b.x0) && x < float64(b.x1) && y >= float64(b.y0) && y < float64(b.y1)
}

var (
	// layers are the silhouette's rows of boxes, top to bottom, each
	// starting where the one before it ends.
	layers = []box{
		{6, 0, 16, 1},   // the cupola's roof
		{7, 1, 15, 4},   // the cupola
		{2, 4, 20, 6},   // the roof
		{3, 6, 19, 10},  // the body
		{1, 10, 21, 12}, // the underframe
	}
	// window is the cupola's window, cut from the filled silhouette;
	// eave is the roof's lower edge across the body, a line in the
	// outline, which closes the cupola's window there.
	window = box{9, 2, 13, 3}
	eave   = box{2, 5, 20, 6}
	// wheels are circles, a point clear of the underframe.
	wheels = []struct{ cx, cy, r float64 }{{6.5, 15.5, 2.5}, {15.5, 15.5, 2.5}}
	// stroke is the outline's width, hub the radius of the hole in a
	// filled wheel, in points: one whole pixel at 1x, four at 2x.
	stroke = 1
	hub    = 0.75
)

func inSilhouette(x, y float64) bool {
	for _, l := range layers {
		if l.has(x, y) {
			return true
		}
	}
	return false
}

// render draws the named image at scale pixels per point: black, its
// alpha the share of the pixel the drawing covers.
func render(name string, scale int) *image.NRGBA {
	w, h := iconW*scale, iconH*scale
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	at := func(px, py int) (float64, float64) {
		return (float64(px) + 0.5) / float64(scale), (float64(py) + 0.5) / float64(scale)
	}
	in := func(px, py int) bool {
		if px < 0 || py < 0 || px >= w || py >= h {
			return false
		}
		return inSilhouette(at(px, py))
	}
	// edge: a pixel of the silhouette within stroke of a pixel outside it.
	edge := func(px, py int) bool {
		d := stroke * scale
		for dy := -d; dy <= d; dy++ {
			for dx := -d; dx <= d; dx++ {
				if !in(px+dx, py+dy) {
					return true
				}
			}
		}
		return false
	}
	const ss = 16 // the wheels' samples per pixel, each way
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			on := false
			if in(px, py) {
				switch name {
				case menubar.ImageRunning:
					on = !window.has(at(px, py))
				case menubar.ImageIdle:
					on = edge(px, py) || eave.has(at(px, py))
				}
			}
			if on {
				img.SetNRGBA(px, py, color.NRGBA{A: 255})
				continue
			}
			n := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(scale)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(scale)
					for _, c := range wheels {
						d2 := (x-c.cx)*(x-c.cx) + (y-c.cy)*(y-c.cy)
						inner := c.r - float64(stroke)
						if name == menubar.ImageRunning {
							inner = hub
						}
						if d2 <= c.r*c.r && d2 > inner*inner {
							n++
						}
					}
				}
			}
			a := (n*255 + ss*ss/2) / (ss * ss)
			if scale == 1 {
				// At 1x a wheel is a pattern of whole pixels: those it
				// mostly covers.
				a = 255 * min(1, 2*n/(ss*ss))
			}
			img.SetNRGBA(px, py, color.NRGBA{A: uint8(a)})
		}
	}
	return img
}

// svg is the same drawing as an SVG: the silhouette as one path, filled
// with the window cut out, or stroked twice the outline's width and
// clipped to itself, with the eave.
func svg(name string) string {
	f := func(v float64) string { return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.3f", v), "0"), ".") }
	// The outline: down the right side, then up the left.
	var pts []string
	for _, l := range layers {
		pts = append(pts, fmt.Sprintf("%d %d", l.x1, l.y0), fmt.Sprintf("%d %d", l.x1, l.y1))
	}
	for i := len(layers) - 1; i >= 0; i-- {
		l := layers[i]
		pts = append(pts, fmt.Sprintf("%d %d", l.x0, l.y1), fmt.Sprintf("%d %d", l.x0, l.y0))
	}
	path := "M" + strings.Join(pts, " L") + " Z"
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d">`+"\n", iconW, iconH, iconW, iconH)
	switch name {
	case menubar.ImageRunning:
		fmt.Fprintf(&b, `  <path d="%s M%d %d H%d V%d H%d Z" fill="#000" fill-rule="evenodd"/>`+"\n",
			path, window.x0, window.y0, window.x1, window.y1, window.x0)
		for _, c := range wheels {
			fmt.Fprintf(&b, `  <circle cx="%s" cy="%s" r="%s" fill="none" stroke="#000" stroke-width="%s"/>`+"\n",
				f(c.cx), f(c.cy), f((c.r+hub)/2), f(c.r-hub))
		}
	case menubar.ImageIdle:
		fmt.Fprintf(&b, `  <clipPath id="s"><path d="%s"/></clipPath>`+"\n", path)
		fmt.Fprintf(&b, `  <path d="%s" fill="none" stroke="#000" stroke-width="%d" clip-path="url(#s)"/>`+"\n", path, 2*stroke)
		fmt.Fprintf(&b, `  <rect x="%d" y="%d" width="%d" height="%d" fill="#000"/>`+"\n", eave.x0, eave.y0, eave.x1-eave.x0, eave.y1-eave.y0)
		for _, c := range wheels {
			fmt.Fprintf(&b, `  <circle cx="%s" cy="%s" r="%s" fill="none" stroke="#000" stroke-width="%d"/>`+"\n",
				f(c.cx), f(c.cy), f(c.r-float64(stroke)/2), stroke)
		}
	}
	b.WriteString("</svg>\n")
	return b.String()
}

// iconBase is each image's file name in icons/, without the scale.
var iconBase = map[string]string{
	menubar.ImageIdle:    "railcar",
	menubar.ImageRunning: "railcar-fill",
}

// iconFiles are the files each image is written to.
func iconFiles(name string) (x1, x2, vector string) {
	base := iconBase[name]
	return filepath.Join("icons", base+".png"), filepath.Join("icons", base+"@2x.png"), filepath.Join("icons", base+".svg")
}

func TestIcons(t *testing.T) {
	for _, name := range []string{menubar.ImageIdle, menubar.ImageRunning} {
		p1, p2, pv := iconFiles(name)
		if *updateIcons {
			for path, scale := range map[string]int{p1: 1, p2: 2} {
				var buf bytes.Buffer
				if err := png.Encode(&buf, render(name, scale)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(pv, []byte(svg(name)), 0o644); err != nil {
				t.Fatal(err)
			}
			continue // the binary embeds what was there: run again to check
		}
		x1, x2, ok := iconPNG(name)
		if !ok {
			t.Fatalf("%s: no embedded image", name)
		}
		for scale, data := range map[int][]byte{1: x1, 2: x2} {
			checkIcon(t, fmt.Sprintf("%s@%dx", name, scale), data, render(name, scale), scale)
		}
		got, err := os.ReadFile(pv)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != svg(name) {
			t.Errorf("%s is not the drawing: go test ./internal/macui -run TestIcons -update", pv)
		}
	}
}

// checkIcon holds an embedded PNG to what a template image must be (its
// size, black with alpha only, something drawn), to crispness (at 1x
// nothing partly covered, at 2x nothing but the wheels' round edges) and
// to the drawing, within a rounding's difference.
func checkIcon(t *testing.T, what string, data []byte, want *image.NRGBA, scale int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if b := img.Bounds(); b.Dx() != iconW*scale || b.Dy() != iconH*scale || b.Min != (image.Point{}) {
		t.Fatalf("%s: %v, want %dx%d", what, b, iconW*scale, iconH*scale)
	}
	drawn, opaque := 0, 0
	for y := 0; y < iconH*scale; y++ {
		for x := 0; x < iconW*scale; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			if c.A != 0 && (c.R != 0 || c.G != 0 || c.B != 0) {
				t.Fatalf("%s: (%d,%d) is %v, not black", what, x, y, c)
			}
			if c.A != 0 {
				drawn++
			}
			if c.A == 255 {
				opaque++
			}
			if c.A != 0 && c.A != 255 && (scale == 1 || !nearWheel(x, y, scale)) {
				t.Errorf("%s: (%d,%d) has alpha %d off the wheels", what, x, y, c.A)
			}
			if d := int(c.A) - int(want.NRGBAAt(x, y).A); d > 2 || d < -2 {
				t.Fatalf("%s: (%d,%d) has alpha %d, the drawing %d: go test ./internal/macui -run TestIcons -update",
					what, x, y, c.A, want.NRGBAAt(x, y).A)
			}
		}
	}
	if opaque < iconW*scale || drawn > iconW*iconH*scale*scale*3/4 {
		t.Errorf("%s: %d pixels drawn, %d opaque", what, drawn, opaque)
	}
}

// nearWheel is whether pixel (x, y) at scale is within a wheel's square.
func nearWheel(x, y, scale int) bool {
	px, py := (float64(x)+0.5)/float64(scale), (float64(y)+0.5)/float64(scale)
	for _, c := range wheels {
		if px > c.cx-c.r-1 && px < c.cx+c.r+1 && py > c.cy-c.r-1 && py < c.cy+c.r+1 {
			return true
		}
	}
	return false
}

// TestIconsDrawing holds the drawing to its design: the silhouette's edges
// on the 1x grid, the wheels a point clear of it and of each other, and
// the silhouette's rows stacked with no gap.
func TestIconsDrawing(t *testing.T) {
	for i, l := range layers {
		if i > 0 && l.y0 != layers[i-1].y1 {
			t.Errorf("layer %d starts at %d, the one before ends at %d", i, l.y0, layers[i-1].y1)
		}
		if l.x0 < 0 || l.x1 > iconW || l.y1 > iconH || l.x0 >= l.x1 {
			t.Errorf("layer %d is %v", i, l)
		}
	}
	bottom := float64(layers[len(layers)-1].y1)
	for _, c := range wheels {
		if c.cy-c.r < bottom+1 || c.cy+c.r > iconH || c.r <= float64(stroke) {
			t.Errorf("wheel %v: not a point clear of the underframe, or off the canvas", c)
		}
	}
	if w := wheels[1].cx - wheels[0].cx; w < wheels[0].r+wheels[1].r+1 {
		t.Errorf("the wheels touch")
	}
}

// TestIconsDiffer: running is the filled railcar, idle its outline, so
// running covers more.
func TestIconsDiffer(t *testing.T) {
	cover := func(name string) int {
		n := 0
		pix := render(name, 2).Pix
		for i := 3; i < len(pix); i += 4 {
			n += int(pix[i])
		}
		return n
	}
	if i, r := cover(menubar.ImageIdle), cover(menubar.ImageRunning); r <= i*5/4 {
		t.Errorf("running covers %d, idle %d", r, i)
	}
	if _, _, ok := iconPNG("shippingbox"); ok {
		t.Error("an image for an unknown name")
	}
}
