package appbundle

import (
	"bytes"
	"encoding/binary"
	"flag"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "write icon/AppIcon.icns from the drawing")

// The app's icon is the logo (docs/assets/logo.svg) on macOS's rounded
// square, the car 40 shorter so that it fits the square: the body, roof
// and frame shortened, the ends and wheels moved in by 20 each, seven
// ribs for ten, the cupola as it is. Its shapes are in the logo's own
// coordinates, in the order the SVG paints them. The logo's light
// outline is left out: on the light tile it would not show.
type shape struct {
	kind       byte // 'r' rounded rect, 'c' circle, 's' stroked segment
	a, b, c, d float64
	r          float64 // corner radius, circle radius, half stroke width
	fill       color.NRGBA
}

var (
	cream = color.NRGBA{0xFB, 0xEF, 0xD9, 0xFF}
	ink   = color.NRGBA{0x2A, 0x25, 0x21, 0xFF}
	steel = color.NRGBA{0x8A, 0x81, 0x7A, 0xFF}
	red   = color.NRGBA{0xC8, 0x47, 0x2F, 0xFF}
	dark  = color.NRGBA{0x9E, 0x33, 0x22, 0xFF}
	amber = color.NRGBA{0xF2, 0xB2, 0x33, 0xFF}
)

func rr(x, y, w, h, rx float64, c color.NRGBA) shape {
	return shape{'r', x, y, x + w, y + h, rx, c}
}

func logo() []shape {
	s := []shape{
		{'c', 94, 132, 0, 0, 13, ink}, {'c', 146, 132, 0, 0, 13, ink},
		rr(56, 112, 128, 9, 2, ink),
		rr(48, 108, 20, 5, 1.5, ink), rr(172, 108, 20, 5, 1.5, ink),
		rr(50, 80, 3, 30, 0, ink), rr(187, 80, 3, 30, 0, ink),
		rr(50, 80, 18, 3, 1.5, ink), rr(172, 80, 18, 3, 1.5, ink),
		rr(64, 56, 112, 8, 3, ink),
		rr(90, 24, 60, 8, 3, ink),
		{'c', 94, 132, 0, 0, 4, steel}, {'c', 146, 132, 0, 0, 4, steel},
		rr(68, 63, 104, 50, 3, red),
	}
	// Seven ribs, 16 in from each end of the body, as the logo's ten are.
	for i := 0.0; i < 7; i++ {
		s = append(s, rr(84+i*68/6, 70, 4, 38, 2, dark))
	}
	s = append(s,
		rr(68, 63, 8, 8, 1.5, dark), rr(164, 63, 8, 8, 1.5, dark),
		rr(68, 105, 8, 8, 1.5, dark), rr(164, 105, 8, 8, 1.5, dark),
		rr(96, 31, 48, 26, 0, red), rr(102, 36, 36, 17, 2, cream),
		shape{'s', 108, 40, 114, 44.5, 1.3, red}, shape{'s', 114, 44.5, 108, 49, 1.3, red},
		rr(118, 47.5, 12, 2.6, 1.3, red),
		shape{'c', 192, 74, 0, 0, 5, amber},
	)
	return s
}

func (s shape) has(x, y float64) bool {
	switch s.kind {
	case 'c':
		return (x-s.a)*(x-s.a)+(y-s.b)*(y-s.b) <= s.r*s.r
	case 's':
		dx, dy := s.c-s.a, s.d-s.b
		t := math.Max(0, math.Min(1, ((x-s.a)*dx+(y-s.b)*dy)/(dx*dx+dy*dy)))
		px, py := s.a+t*dx-x, s.b+t*dy-y
		return px*px+py*py <= s.r*s.r
	}
	return inRounded(x, y, s.a, s.b, s.c, s.d, s.r)
}

func inRounded(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+r, math.Min(x, x1-r))
	cy := math.Max(y0+r, math.Min(y, y1-r))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

// The tile, in a 1024 canvas as Apple's template has it: 824 square,
// 100 in from each side, its corners 185 round; the car about 600 wide
// in it, centred on the logo's centre, 122.5, 84.5.
const (
	tileIn, tileR = 100.0, 185.0
	logoScale     = 4.0
)

// paint is the colour at (x, y) of the 1024 canvas.
func paint(x, y float64, shapes []shape) color.NRGBA {
	if !inRounded(x, y, tileIn, tileIn, 1024-tileIn, 1024-tileIn, tileR) {
		return color.NRGBA{}
	}
	lx := (x-512)/logoScale + 22.5 + 100
	ly := (y-512)/logoScale + 18.5 + 66
	c := cream
	for _, s := range shapes {
		if s.has(lx, ly) {
			c = s.fill
		}
	}
	return c
}

// render draws the icon size pixels square, each pixel the mean of n by
// n samples, colours weighted by their coverage.
func render(size, n int) *image.NRGBA {
	shapes := logo()
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	k := 1024 / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for sy := 0; sy < n; sy++ {
				for sx := 0; sx < n; sx++ {
					c := paint((float64(px)+(float64(sx)+0.5)/float64(n))*k, (float64(py)+(float64(sy)+0.5)/float64(n))*k, shapes)
					w := float64(c.A) / 255
					r, g, b, a = r+float64(c.R)*w, g+float64(c.G)*w, b+float64(c.B)*w, a+w
				}
			}
			if a == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r/a + 0.5), uint8(g/a + 0.5), uint8(b/a + 0.5), uint8(255*a/float64(n*n) + 0.5)})
		}
	}
	return img
}

// icnsSizes are the entries of the .icns, each a PNG: the type and its
// size in pixels (the @2x ones of macOS's 16, 32, 128, 256 and 512
// points among them).
var icnsSizes = []struct {
	typ  string
	size int
}{
	{"icp4", 16}, {"ic11", 32}, {"icp5", 32}, {"ic12", 64}, {"ic07", 128},
	{"ic13", 256}, {"ic08", 256}, {"ic14", 512}, {"ic09", 512}, {"ic10", 1024},
}

// icns is the .icns file: "icns", its length, then each entry's type,
// length (its 8-byte header included) and PNG, all big-endian.
func icns(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	for _, e := range icnsSizes {
		var p bytes.Buffer
		if err := png.Encode(&p, render(e.size, samplesFor(e.size))); err != nil {
			t.Fatal(err)
		}
		body.WriteString(e.typ)
		_ = binary.Write(&body, binary.BigEndian, uint32(8+p.Len()))
		body.Write(p.Bytes())
	}
	var out bytes.Buffer
	out.WriteString("icns")
	_ = binary.Write(&out, binary.BigEndian, uint32(8+body.Len()))
	out.Write(body.Bytes())
	return out.Bytes()
}

// samplesFor keeps the large sizes' rendering short: they have the
// pixels already.
func samplesFor(size int) int {
	switch {
	case size >= 512:
		return 2
	case size >= 128:
		return 4
	}
	return 8
}

// icnsEntries reads an .icns back: each entry's type and PNG.
func icnsEntries(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	if len(b) < 8 || string(b[:4]) != "icns" || int(binary.BigEndian.Uint32(b[4:8])) != len(b) {
		t.Fatalf("not an icns file of its length")
	}
	out := map[string][]byte{}
	for p := 8; p < len(b); {
		if p+8 > len(b) {
			t.Fatalf("an entry's header cut short at %d", p)
		}
		n := int(binary.BigEndian.Uint32(b[p+4 : p+8]))
		if n < 8 || p+n > len(b) {
			t.Fatalf("entry %q at %d: length %d", b[p:p+4], p, n)
		}
		out[string(b[p:p+4])] = b[p+8 : p+n]
		p += n
	}
	return out
}

// The icon file is the drawing: every entry a PNG of its size, and the
// 128 one (fast to draw again) pixel for pixel what render gives. Run
// with -update to write it after changing the drawing.
func TestAppIcon(t *testing.T) {
	if *update {
		if err := os.WriteFile("icon/AppIcon.icns", icns(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := icnsEntries(t, AppIcon)
	if len(got) != len(icnsSizes) {
		t.Errorf("%d entries, want %d", len(got), len(icnsSizes))
	}
	for _, e := range icnsSizes {
		img, err := png.Decode(bytes.NewReader(got[e.typ]))
		if err != nil {
			t.Errorf("%s: %v", e.typ, err)
			continue
		}
		if b := img.Bounds(); b.Dx() != e.size || b.Dy() != e.size {
			t.Errorf("%s is %v, want %d square", e.typ, b, e.size)
		}
	}
	img, _ := png.Decode(bytes.NewReader(got["ic07"]))
	want := render(128, samplesFor(128))
	if img == nil {
		return
	}
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			if g := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA); g != want.NRGBAAt(x, y) {
				t.Fatalf("ic07 at %d,%d is %v, the drawing %v: run go test -run TestAppIcon -update ./internal/appbundle", x, y, g, want.NRGBAAt(x, y))
			}
		}
	}
	// The corners are clear, the tile cream, the car red.
	for _, c := range []struct {
		x, y int
		want color.NRGBA
	}{{2, 2, color.NRGBA{}}, {64, 20, cream}, {65, 64, red}} {
		if g := want.NRGBAAt(c.x, c.y); g != c.want {
			t.Errorf("at %d,%d: %v, want %v", c.x, c.y, g, c.want)
		}
	}
}
