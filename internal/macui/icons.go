package macui

import (
	_ "embed"

	"github.com/bfreis/caboose/internal/menubar"
)

// The status item's images: caboose's railcar, black on clear, drawn as
// template images (AppKit tints them for the menu bar's appearance). Each
// is iconPointsW by iconPointsH points, at 1x and 2x; icons_test.go draws them.
var (
	//go:embed icons/railcar.png
	railcarPNG []byte
	//go:embed icons/railcar@2x.png
	railcar2xPNG []byte
	//go:embed icons/railcar-fill.png
	railcarFillPNG []byte
	//go:embed icons/railcar-fill@2x.png
	railcarFill2xPNG []byte
)

// iconPoints is the images' size in points.
const (
	iconPointsW = 26
	iconPointsH = 18
)

// iconPNG is the image name's PNGs at 1x and 2x, false for a name with
// none.
func iconPNG(name string) (x1, x2 []byte, ok bool) {
	switch name {
	case menubar.ImageIdle:
		return railcarPNG, railcar2xPNG, true
	case menubar.ImageRunning:
		return railcarFillPNG, railcarFill2xPNG, true
	}
	return nil, nil, false
}
