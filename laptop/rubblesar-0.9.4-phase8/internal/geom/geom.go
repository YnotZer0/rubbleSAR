// Package geom is small 2-D geometry in local metres (X = east, Y = north).
package geom

import "math"

// P is a point or vector.
type P struct{ X, Y float64 }

// Add, Sub, Scale, Dot, Len are the usual vector operations.
func (a P) Add(b P) P          { return P{a.X + b.X, a.Y + b.Y} }
func (a P) Sub(b P) P          { return P{a.X - b.X, a.Y - b.Y} }
func (a P) Scale(s float64) P  { return P{a.X * s, a.Y * s} }
func (a P) Dot(b P) float64    { return a.X*b.X + a.Y*b.Y }
func (a P) Len() float64       { return math.Hypot(a.X, a.Y) }
func (a P) Rot(rad float64) P  { s, c := math.Sincos(rad); return P{a.X*c - a.Y*s, a.X*s + a.Y*c} }
func (a P) Dist(b P) float64   { return a.Sub(b).Len() }
func Lerp(a, b P, t float64) P { return P{a.X + (b.X-a.X)*t, a.Y + (b.Y-a.Y)*t} }

// Poly is a simple polygon (no repeated closing vertex).
type Poly []P

// RotRect returns a rectangle centred at c, length along heading (degrees
// clockwise from north) and width across it.
func RotRect(c P, length, width, headingDeg float64) Poly {
	// Heading clockwise from north = rotation of the +Y axis by -heading.
	rad := -headingDeg * math.Pi / 180
	hl, hw := length/2, width/2
	pts := []P{{-hw, -hl}, {hw, -hl}, {hw, hl}, {-hw, hl}}
	out := make(Poly, 4)
	for i, p := range pts {
		out[i] = p.Rot(rad).Add(c)
	}
	return out
}

// Area returns the absolute area.
func (p Poly) Area() float64 {
	s := 0.0
	for i := range p {
		j := (i + 1) % len(p)
		s += p[i].X*p[j].Y - p[j].X*p[i].Y
	}
	return math.Abs(s) / 2
}

// Centroid returns the area centroid (vertex mean for degenerate polygons).
func (p Poly) Centroid() P {
	var a, cx, cy float64
	for i := range p {
		j := (i + 1) % len(p)
		cr := p[i].X*p[j].Y - p[j].X*p[i].Y
		a += cr
		cx += (p[i].X + p[j].X) * cr
		cy += (p[i].Y + p[j].Y) * cr
	}
	if math.Abs(a) < 1e-12 {
		var m P
		for _, q := range p {
			m = m.Add(q)
		}
		return m.Scale(1 / float64(len(p)))
	}
	return P{cx / (3 * a), cy / (3 * a)}
}

// Contains reports whether q is inside (even-odd rule).
func (p Poly) Contains(q P) bool {
	in := false
	for i, j := 0, len(p)-1; i < len(p); j, i = i, i+1 {
		a, b := p[i], p[j]
		if (a.Y > q.Y) != (b.Y > q.Y) && q.X < (b.X-a.X)*(q.Y-a.Y)/(b.Y-a.Y)+a.X {
			in = !in
		}
	}
	return in
}

// SegDist returns the distance from q to segment ab and the parameter t in [0,1].
func SegDist(q, a, b P) (float64, float64) {
	ab := b.Sub(a)
	l2 := ab.Dot(ab)
	t := 0.0
	if l2 > 0 {
		t = math.Max(0, math.Min(1, q.Sub(a).Dot(ab)/l2))
	}
	return q.Dist(a.Add(ab.Scale(t))), t
}

// EdgeDist returns the distance from q to the polygon boundary.
func (p Poly) EdgeDist(q P) float64 {
	d := math.Inf(1)
	for i := range p {
		j := (i + 1) % len(p)
		if v, _ := SegDist(q, p[i], p[j]); v < d {
			d = v
		}
	}
	return d
}

// SignedDist is negative inside, positive outside.
func (p Poly) SignedDist(q P) float64 {
	d := p.EdgeDist(q)
	if p.Contains(q) {
		return -d
	}
	return d
}

// Box is an axis-aligned bounding box.
type Box struct{ Min, Max P }

// Bounds returns the polygon's bounding box.
func (p Poly) Bounds() Box {
	b := Box{P{math.Inf(1), math.Inf(1)}, P{math.Inf(-1), math.Inf(-1)}}
	for _, q := range p {
		b.Min.X, b.Min.Y = math.Min(b.Min.X, q.X), math.Min(b.Min.Y, q.Y)
		b.Max.X, b.Max.Y = math.Max(b.Max.X, q.X), math.Max(b.Max.Y, q.Y)
	}
	return b
}

// Grow returns the box expanded by m on every side.
func (b Box) Grow(m float64) Box {
	return Box{P{b.Min.X - m, b.Min.Y - m}, P{b.Max.X + m, b.Max.Y + m}}
}

// Overlaps reports whether two boxes intersect.
func (b Box) Overlaps(o Box) bool {
	return b.Min.X <= o.Max.X && o.Min.X <= b.Max.X && b.Min.Y <= o.Max.Y && o.Min.Y <= b.Max.Y
}

// ContainsP reports whether q lies in the box.
func (b Box) ContainsP(q P) bool {
	return q.X >= b.Min.X && q.X <= b.Max.X && q.Y >= b.Min.Y && q.Y <= b.Max.Y
}

// PolyDist returns the minimum distance between two polygons (0 if they touch or overlap).
func PolyDist(a, b Poly) float64 {
	for _, q := range a {
		if b.Contains(q) {
			return 0
		}
	}
	for _, q := range b {
		if a.Contains(q) {
			return 0
		}
	}
	d := math.Inf(1)
	for _, q := range a {
		d = math.Min(d, b.EdgeDist(q))
	}
	for _, q := range b {
		d = math.Min(d, a.EdgeDist(q))
	}
	return d
}
