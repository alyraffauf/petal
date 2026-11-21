package tui

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"

	"bloomfx.dev/bloom/anim"
)

// Native 24×24 raster of docs/petal.svg.
//
//go:embed orchid.png
var orchidPNG []byte

// Two pixels share a cell. At the silhouette, paint only the occupied
// half so the terminal's own background shows through.
var orchidSprite = func() image.Image {
	sprite, err := png.Decode(bytes.NewReader(orchidPNG))
	if err != nil {
		panic(err)
	}
	return sprite
}()

func renderOrchid(progress float64) []string {
	sprite := orchidSprite
	ink := func(x, y int) string {
		p := color.NRGBAModel.Convert(sprite.At(x, y)).(color.NRGBA)
		level := anim.InOutCubic(max(0, min(1, progress*2.5-math.Hypot(float64(x)-11.5, float64(y)-11.5)/10)))
		if p.A == 0 || level == 0 {
			return ""
		}
		return fmt.Sprintf("%d;%d;%d", 19+int(level*(float64(p.R)-19)/2), 17+int(level*(float64(p.G)-17)/2), 23+int(level*(float64(p.B)-23)/2))
	}
	var rows []string
	for y := 0; y < sprite.Bounds().Dy(); y += 2 {
		var line strings.Builder
		for x := 0; x < sprite.Bounds().Dx(); x++ {
			top, bottom := ink(x, y), ink(x, y+1)
			switch {
			case top == "" && bottom == "":
				line.WriteByte(' ')
			case bottom == "":
				fmt.Fprintf(&line, "\x1b[38;2;%sm▀\x1b[0m", top)
			case top == "":
				fmt.Fprintf(&line, "\x1b[38;2;%sm▄\x1b[0m", bottom)
			default:
				fmt.Fprintf(&line, "\x1b[38;2;%s;48;2;%sm▀\x1b[0m", top, bottom)
			}
		}
		rows = append(rows, line.String())
	}
	return rows
}

var orchidRows = renderOrchid(2)
