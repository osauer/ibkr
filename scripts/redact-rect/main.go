// Command redact-rect masks rectangular regions of a PNG with an opaque fill.
// It deliberately avoids blur and pixelation because those transformations can
// be reversed for short digit strings.
//
// Usage:
//
//	go run ./scripts/redact-rect -in shot.png -out shot.png \
//	    -rect x0,y0,x1,y1 [-rect ...] [-color RRGGBB]
//	go run ./scripts/redact-rect -in shot.png -crop x0,y0,x1,y1 -out region.png
//
// -crop extracts a region instead (for locating coordinates visually).
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
)

type rects []image.Rectangle

func (r *rects) String() string { return fmt.Sprint(*r) }

func (r *rects) Set(s string) error {
	var x0, y0, x1, y1 int
	if _, err := fmt.Sscanf(s, "%d,%d,%d,%d", &x0, &y0, &x1, &y1); err != nil {
		return fmt.Errorf("rect %q: want x0,y0,x1,y1: %w", s, err)
	}
	*r = append(*r, image.Rect(x0, y0, x1, y1))
	return nil
}

func main() {
	var (
		in       = flag.String("in", "", "input PNG")
		out      = flag.String("out", "", "output PNG")
		fillHex  = flag.String("color", "dde3ea", "fill color, RRGGBB")
		fill     rects
		cropOnly rects
	)
	flag.Var(&fill, "rect", "region to fill, x0,y0,x1,y1 (repeatable)")
	flag.Var(&cropOnly, "crop", "extract region instead of filling")
	flag.Parse()
	if *in == "" || *out == "" || (len(fill) == 0 && len(cropOnly) != 1) {
		flag.Usage()
		os.Exit(2)
	}

	f, err := os.Open(*in)
	if err != nil {
		fatal(err)
	}
	src, err := png.Decode(f)
	f.Close()
	if err != nil {
		fatal(err)
	}

	if len(cropOnly) == 1 {
		dst := image.NewRGBA(cropOnly[0])
		draw.Draw(dst, dst.Bounds(), src, cropOnly[0].Min, draw.Src)
		writePNG(*out, dst)
		return
	}

	var c color.RGBA
	if _, err := fmt.Sscanf(strings.TrimPrefix(*fillHex, "#"), "%02x%02x%02x", &c.R, &c.G, &c.B); err != nil {
		fatal(fmt.Errorf("color %q: %w", *fillHex, err))
	}
	c.A = 0xff

	dst := image.NewRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Src)
	for _, r := range fill {
		draw.Draw(dst, r, &image.Uniform{c}, image.Point{}, draw.Src)
	}
	writePNG(*out, dst)
}

func writePNG(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		fatal(err)
	}
	if err := f.Close(); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "redact-rect:", err)
	os.Exit(1)
}
