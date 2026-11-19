// Package pick interprets a canvas point against authored geometry and the
// canonical compositing model.
//
// A click is not answered with the topmost bounding box. Each shape is evaluated
// through the same fill rule, transform, layer clip/mask, brush alpha and blend
// order that produce the PNG. What the user sees can therefore be:
//
//   - a hole (evenodd or self-intersecting outline), which is not the shape;
//   - outside a layer clip or mask, which paints nothing;
//   - a transparent part of a solid or gradient brush;
//   - a faint contribution (below min_contribution, still reported as evidence);
//   - a low-opacity shape, which stays selectable when its contribution clears
//     min_contribution;
//   - a shape covered by later opaque normal paint, which cannot be selected by
//     visibility but stays in the evidence list;
//   - a joint blend of several shapes, where no single object owns the pixel.
package pick

import (
	"math"
	"sort"

	"prismplate/internal/geom"
	"prismplate/internal/paint"
	"prismplate/internal/scene"
)

const (
	// ContributionReference is the mean premultiplied linear-light RGB change at
	// which a shape's contribution is considered fully evident. It normalizes the
	// contribution component; it does not affect the raw contribution value.
	ContributionReference = 0.25
	// DefaultEdgeTolerancePX is a document-space distance. A click outside the
	// fill but within this distance of the flattened outline remains a candidate.
	// Distance is measured against the same flattened geometry the rasterizer and
	// SVG exporter use, in document pixels, so curve subdivision error is inside
	// the same bound as the rendered proof.
	DefaultEdgeTolerancePX = 1.5
	// DefaultMinContribution is the measurable-change floor in premultiplied
	// linear light. The default is deliberately small so a 5% group opacity still
	// clears it; a shape below it is reported as faint evidence and stays
	// reachable by lowering this value. Raise it to restrict selection to clearly
	// visible paint.
	DefaultMinContribution = 0.0005
	// DefaultAmbiguityMargin is the score gap below which two contributing
	// candidates are reported as not uniquely attributable. The ordering is still
	// stable; only the claim of uniqueness is withheld.
	DefaultAmbiguityMargin = 0.06
	// DefaultMaxCandidates bounds the evidence payload; ordering beyond it is not
	// observable and therefore not claimed.
	DefaultMaxCandidates = 8
	// DefaultJointShare is the share of the pixel's deviation from the background
	// that the top candidate must own for the appearance to be attributed to one
	// object. Below it, two or more shapes jointly form what is seen.
	DefaultJointShare = 0.75
	// occludedEpsilon is the contribution below which a painted shape is treated
	// as making no measurable difference to the composite at the point.
	occludedEpsilon = 1e-9
)

// Weights make the ordering adjustable without changing component meaning.
// Scores are divided by the weight sum so values remain comparable across
// configurations. Defaults favor being inside the visible fill, then actual
// pixel contribution, then opacity, then document order. Document order is a
// weak tie-breaker, not an answer in itself.
type Weights struct {
	Coverage     float64 `json:"coverage"`
	Contribution float64 `json:"contribution"`
	Opacity      float64 `json:"opacity"`
	Topmost      float64 `json:"topmost"`
}

func DefaultWeights() Weights {
	// Coverage dominates; contribution and opacity separate paint from geometry;
	// topmost is a prior that resolves otherwise equal candidates toward the
	// object that paints last, without letting a bounding box win on order alone.
	return Weights{Coverage: 0.40, Contribution: 0.25, Opacity: 0.15, Topmost: 0.20}
}

// Options are tolerances and weights. Omitting a field uses its default; an
// explicit 0 is honoured, so edge_tolerance_px=0 truly disables edge picking and
// min_contribution=0 accepts any measurable change. The resolved values are
// echoed in every result so an ordering is reproducible and an editor can vary
// one number without silently changing the others.
type Options struct {
	EdgeTolerancePX *float64 `json:"edge_tolerance_px,omitempty"`
	MinContribution *float64 `json:"min_contribution,omitempty"`
	AmbiguityMargin *float64 `json:"ambiguity_margin,omitempty"`
	MaxCandidates   *int     `json:"max_candidates,omitempty"`
	// IncludeCovered exposes shapes hidden behind later paint as selectable.
	// It is off by default because a covered object is not what the user sees,
	// but editors commonly need an explicit pick-through gesture.
	IncludeCovered bool `json:"include_covered,omitempty"`
	// JointShare is the minimum fraction of the visible pixel change owned by the
	// top candidate for unique attribution. Lower values allow "joint" results.
	JointShare *float64 `json:"joint_share,omitempty"`
	Weights    *Weights `json:"weights,omitempty"`
}

// Resolved is the fully specified option set with no pointers and no defaults
// left implicit. This is what every result echoes.
type Resolved struct {
	EdgeTolerancePX float64 `json:"edge_tolerance_px"`
	MinContribution float64 `json:"min_contribution"`
	AmbiguityMargin float64 `json:"ambiguity_margin"`
	MaxCandidates   int     `json:"max_candidates"`
	IncludeCovered  bool    `json:"include_covered"`
	JointShare      float64 `json:"joint_share"`
	Weights         Weights `json:"weights"`
}

// Resolved validates and fills defaults.
func (o Options) Resolve() (Resolved, error) {
	r := Resolved{
		EdgeTolerancePX: DefaultEdgeTolerancePX,
		MinContribution: DefaultMinContribution,
		AmbiguityMargin: DefaultAmbiguityMargin,
		MaxCandidates:   DefaultMaxCandidates,
		IncludeCovered:  o.IncludeCovered,
		JointShare:      DefaultJointShare,
		Weights:         DefaultWeights(),
	}
	if o.EdgeTolerancePX != nil {
		r.EdgeTolerancePX = *o.EdgeTolerancePX
	}
	if o.MinContribution != nil {
		r.MinContribution = *o.MinContribution
	}
	if o.AmbiguityMargin != nil {
		r.AmbiguityMargin = *o.AmbiguityMargin
	}
	if o.MaxCandidates != nil {
		r.MaxCandidates = *o.MaxCandidates
	}
	if o.JointShare != nil {
		r.JointShare = *o.JointShare
	}
	if o.Weights != nil {
		r.Weights = *o.Weights
	}
	if !finite(r.EdgeTolerancePX) || r.EdgeTolerancePX < 0 || r.EdgeTolerancePX > 64 {
		return r, optionError("edge_tolerance_px must be finite and within 0..64")
	}
	if !finite(r.MinContribution) || r.MinContribution < 0 || r.MinContribution > 1 {
		return r, optionError("min_contribution must be finite and within 0..1")
	}
	if !finite(r.AmbiguityMargin) || r.AmbiguityMargin < 0 || r.AmbiguityMargin > 1 {
		return r, optionError("ambiguity_margin must be finite and within 0..1")
	}
	if r.MaxCandidates < 1 || r.MaxCandidates > 32 {
		return r, optionError("max_candidates must be within 1..32")
	}
	if !finite(r.JointShare) || r.JointShare < 0 || r.JointShare > 1 {
		return r, optionError("joint_share must be finite and within 0..1")
	}
	w := r.Weights
	sum := w.Coverage + w.Contribution + w.Opacity + w.Topmost
	for _, v := range []float64{w.Coverage, w.Contribution, w.Opacity, w.Topmost} {
		if !finite(v) || v < 0 || v > 10 {
			return r, optionError("weights must be finite and within 0..10")
		}
	}
	if sum <= 0 {
		return r, optionError("at least one weight must be positive")
	}
	return r, nil
}

type optionError string

func (e optionError) Error() string { return string(e) }
func errOption(s string) error      { return optionError(s) }

// IsOptionError reports whether err came from invalid picking options or a
// point outside the canvas, so transport can answer 400 without guessing.
func IsOptionError(err error) bool {
	_, ok := err.(optionError)
	return ok
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Components is the exact score basis. Every value is 0..1.
type Components struct {
	Coverage     float64 `json:"coverage"`
	Contribution float64 `json:"contribution"`
	Opacity      float64 `json:"opacity"`
	Topmost      float64 `json:"topmost"`
}

// Occlusion values explain why a shape does or does not paint at the click.
//
//	visible    inside the fill and measurably changes the composite
//	faint      inside the fill, changes pixels, below min_contribution
//	covered    inside the fill, but later paint leaves no measurable change
//	transparent brush alpha is zero at this point
//	hole       the outline encloses the point in a hole, not a fill
//	outside    outside the fill and outside edge tolerance
//	clipped    the layer clip excludes the point
//	masked     the layer mask excludes the point
const (
	OcclusionVisible     = "visible"
	OcclusionFaint       = "faint"
	OcclusionCovered     = "covered"
	OcclusionTransparent = "transparent"
	OcclusionHole        = "hole"
	OcclusionOutside     = "outside"
	OcclusionClipped     = "clipped"
	OcclusionMasked      = "masked"
)

// Candidate is one locally relevant authored shape with separate geometry,
// compositing and ordering evidence.
type Candidate struct {
	Rank       int    `json:"rank"` // 1-based after stable sorting
	LayerID    string `json:"layer_id"`
	ShapeID    string `json:"shape_id"`
	LayerIndex int    `json:"layer_index"`
	ShapeIndex int    `json:"shape_index"`

	FillRule  string      `json:"fill_rule"`
	Brush     string      `json:"brush"`
	Transform geom.Matrix `json:"transform"`
	Local     geom.Point  `json:"local_point"`

	Inside     bool    `json:"inside"`
	Hole       bool    `json:"hole"`
	Edge       bool    `json:"edge"`
	EdgeDistPX float64 `json:"edge_distance_px"`
	LocalAlpha float64 `json:"local_alpha"`
	Opacity    float64 `json:"layer_opacity"`

	Contribution float64  `json:"contribution"`
	Occlusion    string   `json:"occlusion"`
	OccludedBy   []string `json:"occluded_by,omitempty"`
	LayerGate    bool     `json:"layer_gate_at_point"`
	LayerClip    string   `json:"layer_clip,omitempty"`

	Selectable bool       `json:"selectable"`
	Selected   bool       `json:"selected"`
	Components Components `json:"components"`
	Score      float64    `json:"score"`
	Reasons    []string   `json:"reasons"`
}

// Result is deterministic for a fixed document, point and resolved options.
// Selected names the stable top candidate; Ambiguous is present when attribution
// is not unique and repeats the ordered shape IDs the caller should offer.
// Candidates is always an array, empty when nothing is locally relevant.
type Result struct {
	Plate       string      `json:"plate,omitempty"`
	Revision    int         `json:"revision,omitempty"`
	SceneDigest string      `json:"scene_digest,omitempty"`
	X           float64     `json:"x"`
	Y           float64     `json:"y"`
	Selected    string      `json:"selected,omitempty"`
	Candidates  []Candidate `json:"candidates"`
	Ambiguous   []string    `json:"ambiguous,omitempty"`
	Note        string      `json:"note,omitempty"`
	Options     Resolved    `json:"options"`
}

type state struct {
	cand    Candidate
	sample  paint.Pixel
	painted bool
}

// layerFold is a compositing fold for one layer. prefix[i] is the layer-local
// source before shape i; suffix[i] is the source formed by shapes i..n-1 alone.
// Source-over is associative for premultiplied pixels, so prefix[i] over
// suffix[i+1] reproduces exactly the layer source with shape i omitted.
type layerFold struct {
	prefix []paint.Pixel
	suffix []paint.Pixel
}

func makeFold(n int, samples []paint.Pixel, active []bool) layerFold {
	prefix := make([]paint.Pixel, n+1)
	for i := 0; i < n; i++ {
		prefix[i+1] = prefix[i]
		if active[i] {
			prefix[i+1] = paint.Over(prefix[i+1], samples[i], "normal")
		}
	}
	suffix := make([]paint.Pixel, n+1)
	for i := n - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1]
		if active[i] {
			suffix[i] = paint.Over(samples[i], suffix[i+1], "normal")
		}
	}
	return layerFold{prefix: prefix[:n], suffix: suffix}
}

// without returns the layer source with exactly shape si omitted.
func (f layerFold) without(si int) paint.Pixel {
	return paint.Over(f.prefix[si], f.suffix[si+1], "normal")
}

// Hit evaluates the point and returns ranked evidence. Candidates that cannot
// paint at the point (hole, clip, covered) remain in the list with
// selectable=false so an editor can explain or deliberately pick through them.
func Hit(c scene.Compiled, x, y float64, opt Options) (Result, error) {
	res := Result{X: x, Y: y, Candidates: []Candidate{}}
	resolved, err := opt.Resolve()
	if err != nil {
		return res, err
	}
	res.Options = resolved
	if !finite(x) || !finite(y) || x < 0 || y < 0 || x > float64(c.Document.Width) || y > float64(c.Document.Height) {
		return res, errOption("point must be finite and inside the canvas")
	}
	p := geom.Point{X: x, Y: y}
	base := paint.Composite(c, p)

	total := 0
	for _, l := range c.Layers {
		total += len(l.Shapes)
	}
	if total == 0 {
		res.Note = "document has no shapes"
		return res, nil
	}

	states := make([][]state, len(c.Layers))
	folds := make([]layerFold, len(c.Layers))
	gates := make([]bool, len(c.Layers))
	clipKinds := make([]string, len(c.Layers))
	order := 0
	for li, l := range c.Layers {
		// Use the renderer's gate, then explain why it failed. The distinction
		// between clip and mask is for the caller; the pass/fail is shared.
		gates[li] = paint.LayerGate(l, p)
		if !gates[li] {
			if len(l.Clip) > 0 && !l.Clip.Contains(p, "nonzero") {
				clipKinds[li] = OcclusionClipped
			} else {
				clipKinds[li] = OcclusionMasked
			}
		}
		states[li] = make([]state, len(l.Shapes))
		samples := make([]paint.Pixel, len(l.Shapes))
		active := make([]bool, len(l.Shapes))
		for si, s := range l.Shapes {
			order++
			q := s.Inverse.Apply(p)
			inside := s.Path.Contains(q, s.Shape.FillRule)
			dist := documentDistance(s, p)
			cand := Candidate{
				LayerID:    l.Layer.ID,
				ShapeID:    s.Shape.ID,
				LayerIndex: li,
				ShapeIndex: si,
				FillRule:   s.Shape.FillRule,
				Brush:      s.Shape.Brush.Kind,
				Transform:  s.Shape.Transform,
				Local:      roundPoint(q),
				Inside:     inside,
				Hole:       !inside && s.Path.ContainsAnyRing(q),
				Edge:       dist <= resolved.EdgeTolerancePX,
				EdgeDistPX: round(dist, 4),
				Opacity:    l.Layer.Opacity,
				LayerGate:  gates[li],
				LayerClip:  clipKinds[li],
			}
			if total > 1 {
				cand.Components.Topmost = float64(order-1) / float64(total-1)
			} else {
				cand.Components.Topmost = 1
			}
			st := state{cand: cand}
			if inside {
				st.sample = paint.Sample(s.Shape.Brush, q)
				samples[si] = st.sample
				active[si] = gates[li]
				st.cand.LocalAlpha = round(st.sample.A, 6)
				st.painted = gates[li]
			}
			states[li][si] = st
		}
		folds[li] = makeFold(len(l.Shapes), samples, active)
	}

	for li := range c.Layers {
		for si := range c.Layers[li].Shapes {
			st := &states[li][si]
			if !st.painted {
				st.cand.Occlusion = occlusionWithoutPaint(st.cand, st.sample)
				// Edge proximity is a geometric affordance: a click just outside a
				// thin outline or inside a hole rim may still target that shape.
				// It never applies through a layer clip, mask or cover.
				st.cand.Selectable = st.cand.Edge && st.cand.LayerGate &&
					st.cand.Occlusion != OcclusionCovered
				st.cand.Components = componentsOf(st.cand, st.cand.Selectable, resolved)
				st.cand.Score = round(weighted(st.cand.Components, resolved.Weights), 4)
				st.cand.Reasons = reasonsOf(st.cand)
				continue
			}
			// Contribution is the actual final-composite change when exactly this
			// shape is omitted from its layer. Holes, stacking, group opacity and
			// every blend mode are already inside the shared compositing model.
			without := paint.CompositeLayerSource(c, p, li, folds[li].without(si))
			raw := colorDelta(base, without)
			st.cand.Contribution = round(raw, 6)
			switch {
			case st.sample.A <= 0 || st.cand.Opacity <= 0:
				// The brush or the whole group paints nothing at the point (e.g. the
				// transparent end of a gradient, or a zero-opacity layer). This is
				// authored coverage, not occlusion.
				st.cand.Occlusion = OcclusionTransparent
			case raw <= occludedEpsilon:
				st.cand.Occlusion = OcclusionCovered
				st.cand.OccludedBy = laterPaint(c, p, li, si, gates)
			case raw < resolved.MinContribution:
				// Below the caller's selection floor, but the PNG still changes.
				st.cand.Occlusion = OcclusionFaint
			default:
				st.cand.Occlusion = OcclusionVisible
			}
			// Only paint that meets the selection floor is selected by visibility.
			// Covered paint is selectable solely through an explicit pick-through
			// request; faint paint stays evidence and is reachable by lowering
			// min_contribution rather than being silently dropped.
			st.cand.Selectable = st.cand.Occlusion == OcclusionVisible
			if st.cand.Occlusion == OcclusionCovered && resolved.IncludeCovered {
				st.cand.Selectable = true
			}
			if !st.cand.Selectable && st.cand.Edge && st.cand.LayerGate &&
				st.cand.Occlusion != OcclusionCovered {
				st.cand.Selectable = true
			}
			st.cand.Components = componentsOf(st.cand, st.cand.Selectable, resolved)
			st.cand.Score = round(weighted(st.cand.Components, resolved.Weights), 4)
			st.cand.Reasons = reasonsOf(st.cand)
		}
	}

	var local []Candidate
	for _, layerStates := range states {
		for _, st := range layerStates {
			// A shape enters the evidence list because the point is geometrically
			// near it. "Excluded by the layer clip" is then an explanation for that
			// shape, not a reason to list every shape in a clipped layer.
			if st.cand.Inside || st.cand.Edge || st.cand.Hole {
				local = append(local, st.cand)
			}
		}
	}
	sort.SliceStable(local, func(i, j int) bool {
		if local[i].Selectable != local[j].Selectable {
			return local[i].Selectable
		}
		if local[i].Score != local[j].Score {
			return local[i].Score > local[j].Score
		}
		// Later document order is the stable tie-break, matching paint order.
		if local[i].LayerIndex != local[j].LayerIndex {
			return local[i].LayerIndex > local[j].LayerIndex
		}
		return local[i].ShapeIndex > local[j].ShapeIndex
	})
	for i := range local {
		local[i].Rank = i + 1
	}
	if len(local) > resolved.MaxCandidates {
		local = local[:resolved.MaxCandidates]
	}

	var selectable []int
	for i, cand := range local {
		if cand.Selectable {
			selectable = append(selectable, i)
		}
	}
	if len(selectable) == 0 {
		res.Note = "no selectable shape at this point; background or an empty/clipped region is under the cursor"
	} else {
		top := selectable[0]
		local[top].Selected = true
		res.Selected = local[top].ShapeID
		for _, i := range selectable[1:] {
			if local[top].Score-local[i].Score <= resolved.AmbiguityMargin {
				if len(res.Ambiguous) == 0 {
					res.Ambiguous = append(res.Ambiguous, local[top].ShapeID)
				}
				res.Ambiguous = append(res.Ambiguous, local[i].ShapeID)
			}
		}
		// Score ties are one reason attribution is not unique. A second reason is
		// joint appearance: when the top shape's removal explains less than
		// joint_share of the pixel's deviation from the background, several
		// objects together form the color the user sees. Only shapes that
		// measurably contribute can be joint authors.
		if len(res.Ambiguous) < 2 {
			if total := colorDelta(base, paint.From(c.Document.Background)); total > occludedEpsilon {
				share := local[top].Contribution / total
				if share < resolved.JointShare {
					parts := []string{local[top].ShapeID}
					for _, i := range selectable[1:] {
						if local[i].Contribution > occludedEpsilon {
							parts = append(parts, local[i].ShapeID)
						}
					}
					if len(parts) > 1 {
						res.Ambiguous = parts
					}
				}
			}
		}
		switch {
		case len(res.Ambiguous) > 1:
			res.Note = "the composite is a joint effect within joint_share/ambiguity_margin; ranking is stable but no single shape owns the pixel"
		case local[top].Occlusion == OcclusionFaint:
			res.Note = "selected a faint contribution; raise min_contribution to exclude low-opacity paint from selection"
		case !local[top].Inside:
			res.Note = "selected by edge tolerance; the click is outside the fill"
		}
	}
	res.Candidates = local
	if res.Candidates == nil {
		res.Candidates = []Candidate{}
	}
	return res, nil
}

// documentDistance is the true document-space distance to the flattened outline.
// Transforming each segment endpoint keeps non-uniform scales and skews honest,
// unlike scaling a local-space distance by an average factor.
func documentDistance(s scene.CompiledShape, p geom.Point) float64 {
	best := math.Inf(1)
	for _, ring := range s.Path {
		n := len(ring)
		for i := 0; i < n; i++ {
			a := s.Shape.Transform.Apply(ring[i])
			b := s.Shape.Transform.Apply(ring[(i+1)%n])
			best = math.Min(best, geom.SegmentDistance(p, a, b))
		}
	}
	return best
}

// colorDelta is the mean absolute change in premultiplied linear-light RGB. It
// ignores alpha so a fully transparent color change cannot masquerade as paint.
func colorDelta(a, b paint.Pixel) float64 {
	return (math.Abs(a.R-b.R) + math.Abs(a.G-b.G) + math.Abs(a.B-b.B)) / 3
}

func occlusionWithoutPaint(cand Candidate, sample paint.Pixel) string {
	switch {
	case cand.LayerClip != "":
		return cand.LayerClip
	case !cand.Inside && cand.Hole:
		return OcclusionHole
	case !cand.Inside:
		return OcclusionOutside
	case cand.Opacity <= 0 || sample.A <= 0:
		return OcclusionTransparent
	default:
		return OcclusionOutside
	}
}

// laterPaint lists shapes that are painted after this one and cover the point
// with non-zero alpha in an enabled layer. It is evidence, not a bound test.
func laterPaint(c scene.Compiled, p geom.Point, li, si int, gates []bool) []string {
	var by []string
	for lj := li; lj < len(c.Layers); lj++ {
		if !gates[lj] {
			continue
		}
		for sj, s := range c.Layers[lj].Shapes {
			if lj == li && sj <= si {
				continue
			}
			q := s.Inverse.Apply(p)
			if !s.Path.Contains(q, s.Shape.FillRule) {
				continue
			}
			if paint.Sample(s.Shape.Brush, q).A <= 0 {
				continue
			}
			by = append(by, s.Shape.ID)
			if len(by) == 8 {
				return by
			}
		}
	}
	return by
}

func componentsOf(cand Candidate, selectable bool, opt Resolved) Components {
	cov := 0.0
	switch {
	case cand.Inside && cand.LayerGate:
		cov = 1
	case cand.Edge:
		// The edge band is half-covered by construction so an interior candidate
		// always outranks an edge-only one at equal contribution.
		if opt.EdgeTolerancePX > 0 {
			cov = 0.5 * (1 - math.Min(cand.EdgeDistPX/opt.EdgeTolerancePX, 1))
		}
	}
	if !selectable {
		// Keep non-selectable evidence below every selectable candidate regardless
		// of weights chosen by the caller.
		cov = math.Min(cov, 0.49)
	}
	contrib := math.Min(1, cand.Contribution/ContributionReference)
	opac := 0.0
	if cand.Inside {
		opac = math.Min(1, cand.LocalAlpha*cand.Opacity)
	}
	return Components{
		Coverage:     round(cov, 4),
		Contribution: round(contrib, 4),
		Opacity:      round(opac, 4),
		Topmost:      round(cand.Components.Topmost, 4),
	}
}

func weighted(c Components, w Weights) float64 {
	sum := w.Coverage + w.Contribution + w.Opacity + w.Topmost
	return (w.Coverage*c.Coverage + w.Contribution*c.Contribution + w.Opacity*c.Opacity + w.Topmost*c.Topmost) / sum
}

func reasonsOf(cand Candidate) []string {
	var r []string
	switch cand.Occlusion {
	case OcclusionClipped:
		r = append(r, "layer clip excludes the point")
	case OcclusionMasked:
		r = append(r, "layer mask excludes the point")
	case OcclusionHole:
		r = append(r, "outline encloses the point in a hole")
	case OcclusionOutside:
		if cand.Edge {
			r = append(r, "outside fill but within edge tolerance")
		} else {
			r = append(r, "outside fill")
		}
	case OcclusionTransparent:
		r = append(r, "brush alpha is zero at the point")
	case OcclusionCovered:
		r = append(r, "later paint leaves no measurable change")
	case OcclusionFaint:
		r = append(r, "faint but measurable contribution")
	case OcclusionVisible:
		r = append(r, "inside fill with measurable contribution")
	}
	if cand.Inside && cand.LayerGate {
		r = append(r, "fill applies under layer clip and mask")
	}
	if !cand.Selectable && cand.Occlusion != OcclusionHole && cand.Occlusion != OcclusionOutside && cand.LayerClip == "" {
		r = append(r, "not selectable at this point")
	}
	return r
}

func round(v float64, places int) float64 {
	s := math.Pow(10, float64(places))
	return math.Round(v*s) / s
}

func roundPoint(p geom.Point) geom.Point {
	return geom.Point{X: round(p.X, 5), Y: round(p.Y, 5)}
}
