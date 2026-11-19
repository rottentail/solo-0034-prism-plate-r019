// Package paint implements linear-light, premultiplied-alpha compositing.
package paint

import (
	"math"
	"prismplate/internal/geom"
	"prismplate/internal/scene"
)

type Pixel struct{ R, G, B, A float64 }

func linear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}
func srgb(v float64) float64 {
	v = math.Max(0, math.Min(1, v))
	if v <= 0.0031308 {
		return v * 12.92
	}
	return 1.055*math.Pow(v, 1/2.4) - 0.055
}
func From(c scene.Color) Pixel {
	return Pixel{linear(c.R) * c.A, linear(c.G) * c.A, linear(c.B) * c.A, c.A}
}
func (p Pixel) Scale(a float64) Pixel { return Pixel{p.R * a, p.G * a, p.B * a, p.A * a} }
func (p Pixel) RGBA8() (uint8, uint8, uint8, uint8) {
	if p.A <= 0 {
		return 0, 0, 0, 0
	}
	return uint8(math.Round(srgb(p.R/p.A) * 255)), uint8(math.Round(srgb(p.G/p.A) * 255)), uint8(math.Round(srgb(p.B/p.A) * 255)), uint8(math.Round(math.Min(1, p.A) * 255))
}
func mix(a, b, t float64) float64 { return a + (b-a)*t }
func Sample(b scene.Brush, p geom.Point) Pixel {
	if b.Kind == "solid" {
		return From(b.Color)
	}
	dx, dy := b.To.X-b.From.X, b.To.Y-b.From.Y
	t := ((p.X-b.From.X)*dx + (p.Y-b.From.Y)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	for i := 1; i < len(b.Stops); i++ {
		a, z := b.Stops[i-1], b.Stops[i]
		if t <= z.At {
			u := (t - a.At) / (z.At - a.At)
			x, y := From(a.Color), From(z.Color)
			return Pixel{mix(x.R, y.R, u), mix(x.G, y.G, u), mix(x.B, y.B, u), mix(x.A, y.A, u)}
		}
	}
	return From(b.Stops[len(b.Stops)-1].Color)
}
func Over(dst, src Pixel, mode string) Pixel {
	blend := func(d, s float64) float64 {
		if mode == "normal" || src.A == 0 || dst.A == 0 {
			return s + d*(1-src.A)
		}
		a, b := d/dst.A, s/src.A
		v := a * b
		if mode == "screen" {
			v = a + b - a*b
		}
		return (1-src.A)*d + (1-dst.A)*s + src.A*dst.A*v
	}
	return Pixel{blend(dst.R, src.R), blend(dst.G, src.G), blend(dst.B, src.B), src.A + dst.A*(1-src.A)}
}

// LayerSource is the layer-local source before the group opacity and blend mode
// are applied. It is shared by raster and pick so hit explanations cannot drift
// from the canonical pixel model.
func LayerSource(l scene.CompiledLayer, p geom.Point) (Pixel, bool) {
	return layerSource(l, p, -1)
}
func layerSource(l scene.CompiledLayer, p geom.Point, skipShape int) (Pixel, bool) {
	if !LayerGate(l, p) {
		return Pixel{}, false
	}
	var src Pixel
	for i, s := range l.Shapes {
		if i == skipShape {
			continue
		}
		q := s.Inverse.Apply(p)
		if s.Path.Contains(q, s.Shape.FillRule) {
			src = Over(src, Sample(s.Shape.Brush, q), "normal")
		}
	}
	return src, true
}

// LayerGate reports whether the layer's clip and mask admit this point. Pick uses
// the same gate as the renderer, then explains it instead of re-implementing it.
func LayerGate(l scene.CompiledLayer, p geom.Point) bool {
	if len(l.Clip) > 0 && !l.Clip.Contains(p, "nonzero") {
		return false
	}
	if len(l.Mask) > 0 && !l.Mask.Contains(p, "nonzero") {
		return false
	}
	return true
}

// Composite renders one document-space sample with the same order and arithmetic
// as the deterministic PNG path.
func Composite(c scene.Compiled, p geom.Point) Pixel {
	return composite(c, p, -1, -1)
}
func composite(c scene.Compiled, p geom.Point, skipLayer, skipShape int) Pixel {
	dst := From(c.Document.Background)
	for i, l := range c.Layers {
		skip := -1
		if i == skipLayer {
			skip = skipShape
		}
		src, ok := layerSource(l, p, skip)
		if !ok {
			continue
		}
		dst = Over(dst, src.Scale(l.Layer.Opacity), l.Layer.Blend)
	}
	return dst
}

// CompositeLayerSource replaces the source of one layer with an already
// computed layer-local source. It keeps every other layer, the group opacity and
// the blend mode identical, so callers can measure a single shape's visual
// contribution without repeating geometry tests.
func CompositeLayerSource(c scene.Compiled, p geom.Point, layer int, source Pixel) Pixel {
	dst := From(c.Document.Background)
	for i, l := range c.Layers {
		var src Pixel
		var ok bool
		if i == layer {
			src, ok = source, true
		} else {
			src, ok = layerSource(l, p, -1)
		}
		if !ok {
			continue
		}
		dst = Over(dst, src.Scale(l.Layer.Opacity), l.Layer.Blend)
	}
	return dst
}
