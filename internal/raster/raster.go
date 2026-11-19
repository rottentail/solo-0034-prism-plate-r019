// Package raster produces deterministic, supersampled PNG exposures.
package raster

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"prismplate/internal/geom"
	"prismplate/internal/paint"
	"prismplate/internal/scene"
)

func Render(ctx context.Context, c scene.Compiled) ([]byte, error) {
	d := c.Document
	img := image.NewNRGBA(image.Rect(0, 0, d.Width, d.Height))
	for y := 0; y < d.Height; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < d.Width; x++ {
			sum := paint.Pixel{}
			for sy := 0; sy < 2; sy++ {
				for sx := 0; sx < 2; sx++ {
					p := geom.Point{X: float64(x) + (float64(sx)+0.5)/2, Y: float64(y) + (float64(sy)+0.5)/2}
					dst := paint.Composite(c, p)
					sum.R += dst.R / 4
					sum.G += dst.G / 4
					sum.B += dst.B / 4
					sum.A += dst.A / 4
				}
			}
			r, g, b, a := sum.RGBA8()
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: a})
		}
	}
	var out bytes.Buffer
	err := png.Encode(&out, img)
	return out.Bytes(), err
}
