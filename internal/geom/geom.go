// Package geom compiles bounded vector contours and affine coordinate mappings.
package geom

import "math"

type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}
type Matrix [6]float64

var Identity = Matrix{1, 0, 0, 1, 0, 0}

func (m Matrix) Apply(p Point) Point {
	return Point{m[0]*p.X + m[2]*p.Y + m[4], m[1]*p.X + m[3]*p.Y + m[5]}
}
func (m Matrix) Inverse() (Matrix, bool) {
	d := m[0]*m[3] - m[1]*m[2]
	if math.Abs(d) < 1e-10 {
		return Matrix{}, false
	}
	return Matrix{m[3] / d, -m[1] / d, -m[2] / d, m[0] / d, (m[2]*m[5] - m[3]*m[4]) / d, (m[1]*m[4] - m[0]*m[5]) / d}, true
}

type Op struct {
	Kind   string  `json:"kind"`
	Points []Point `json:"points,omitempty"`
}
type Contour []Point
type Path []Contour

// Flatten uses a fixed subdivision contract shared by raster and SVG export.
func Flatten(ops []Op) Path {
	var out Path
	var ring Contour
	var at Point
	flush := func() {
		if len(ring) >= 3 {
			out = append(out, ring)
		}
		ring = nil
	}
	for _, o := range ops {
		switch o.Kind {
		case "M":
			flush()
			at = o.Points[0]
			ring = append(ring, at)
		case "L":
			at = o.Points[0]
			ring = append(ring, at)
		case "Q":
			start := at
			control, end := o.Points[0], o.Points[1]
			for i := 1; i <= 16; i++ {
				t := float64(i) / 16
				u := 1 - t
				ring = append(ring, Point{u*u*start.X + 2*u*t*control.X + t*t*end.X, u*u*start.Y + 2*u*t*control.Y + t*t*end.Y})
			}
			at = end
		case "C":
			start := at
			a, b, end := o.Points[0], o.Points[1], o.Points[2]
			for i := 1; i <= 24; i++ {
				t := float64(i) / 24
				u := 1 - t
				ring = append(ring, Point{u*u*u*start.X + 3*u*u*t*a.X + 3*u*t*t*b.X + t*t*t*end.X, u*u*u*start.Y + 3*u*u*t*a.Y + 3*u*t*t*b.Y + t*t*t*end.Y})
			}
			at = end
		case "Z":
			flush()
		}
	}
	flush()
	return out
}
func (path Path) Contains(p Point, rule string) bool {
	winding := 0
	crossings := 0
	for _, ring := range path {
		for i, a := range ring {
			b := ring[(i+1)%len(ring)]
			if (a.Y <= p.Y && b.Y > p.Y) || (b.Y <= p.Y && a.Y > p.Y) {
				x := a.X + (p.Y-a.Y)*(b.X-a.X)/(b.Y-a.Y)
				if x > p.X {
					crossings++
					if b.Y > a.Y {
						winding++
					} else {
						winding--
					}
				}
			}
		}
	}
	if rule == "evenodd" {
		return crossings%2 == 1
	}
	return winding != 0
}

// ContainsAnyRing reports whether any single contour encloses p, independent of
// the fill rule. Pick uses it to distinguish a real hole from a point merely
// inside a bounding box: a hole test is false while this test is true.
func (path Path) ContainsAnyRing(p Point) bool {
	for _, ring := range path {
		winding := 0
		for i, a := range ring {
			b := ring[(i+1)%len(ring)]
			if (a.Y <= p.Y && b.Y > p.Y) || (b.Y <= p.Y && a.Y > p.Y) {
				x := a.X + (p.Y-a.Y)*(b.X-a.X)/(b.Y-a.Y)
				if x > p.X {
					if b.Y > a.Y {
						winding++
					} else {
						winding--
					}
				}
			}
		}
		if winding != 0 {
			return true
		}
	}
	return false
}

// Bounds returns the axis-aligned bounds of every flattened vertex.
func (path Path) Bounds() (Point, Point) {
	lo := Point{X: math.Inf(1), Y: math.Inf(1)}
	hi := Point{X: math.Inf(-1), Y: math.Inf(-1)}
	for _, ring := range path {
		for _, v := range ring {
			lo.X = math.Min(lo.X, v.X)
			lo.Y = math.Min(lo.Y, v.Y)
			hi.X = math.Max(hi.X, v.X)
			hi.Y = math.Max(hi.Y, v.Y)
		}
	}
	return lo, hi
}

// Distance returns the shortest distance from p to any contour segment. It is
// the edge tolerance basis: zero on the outline, positive on either side.
func (path Path) Distance(p Point) float64 {
	best := math.Inf(1)
	for _, ring := range path {
		for i, a := range ring {
			b := ring[(i+1)%len(ring)]
			best = math.Min(best, SegmentDistance(p, a, b))
		}
	}
	return best
}

// SegmentDistance is the Euclidean distance from p to segment ab.
func SegmentDistance(p, a, b Point) float64 {
	dx, dy := b.X-a.X, b.Y-a.Y
	if dx == 0 && dy == 0 {
		return math.Hypot(p.X-a.X, p.Y-a.Y)
	}
	t := ((p.X-a.X)*dx + (p.Y-a.Y)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p.X-(a.X+t*dx), p.Y-(a.Y+t*dy))
}
