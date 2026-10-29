package render

import (
	"math"

	"rubblesar/internal/geom"
)

// shape is anything that can be rasterised: a bounding box and a signed
// distance (negative inside, metres; approximate for ellipses).
type shape interface {
	bounds() geom.Box
	sd(p geom.P) float64
}

type capsule struct {
	a, b geom.P
	r    float64
}

func (c capsule) bounds() geom.Box {
	return geom.Box{Min: geom.P{X: math.Min(c.a.X, c.b.X), Y: math.Min(c.a.Y, c.b.Y)},
		Max: geom.P{X: math.Max(c.a.X, c.b.X), Y: math.Max(c.a.Y, c.b.Y)}}.Grow(c.r)
}

func (c capsule) sd(p geom.P) float64 {
	d, _ := geom.SegDist(p, c.a, c.b)
	return d - c.r
}

// axisFrac returns the position along the capsule axis (0 at a, 1 at b).
func (c capsule) axisFrac(p geom.P) float64 {
	_, t := geom.SegDist(p, c.a, c.b)
	return t
}

type ellipse struct {
	c      geom.P
	u      geom.P // unit vector of the first semi-axis
	ax, ay float64
}

func (e ellipse) bounds() geom.Box {
	r := math.Max(e.ax, e.ay)
	return geom.Box{Min: geom.P{X: e.c.X - r, Y: e.c.Y - r}, Max: geom.P{X: e.c.X + r, Y: e.c.Y + r}}
}

// local returns coordinates along u and across (to the right of) u.
func (e ellipse) local(p geom.P) (x, y float64) {
	q := p.Sub(e.c)
	return q.Dot(e.u), q.X*e.u.Y - q.Y*e.u.X
}

func (e ellipse) k(p geom.P) float64 {
	x, y := e.local(p)
	return math.Hypot(x/e.ax, y/e.ay)
}

func (e ellipse) sd(p geom.P) float64 { return (e.k(p) - 1) * math.Min(e.ax, e.ay) }

type polygon struct {
	p  geom.Poly
	bb geom.Box
}

func newPolygon(p geom.Poly) polygon { return polygon{p, p.Bounds()} }

func (p polygon) bounds() geom.Box { return p.bb }

func (p polygon) sd(q geom.P) float64 {
	if !p.bb.Grow(0.05).ContainsP(q) {
		return 1
	}
	return p.p.SignedDist(q)
}

// dirVec converts a heading (degrees clockwise from north) to a unit vector.
func dirVec(deg float64) geom.P {
	r := deg * math.Pi / 180
	return geom.P{X: math.Sin(r), Y: math.Cos(r)}
}

// rightOf returns the unit vector 90 degrees clockwise of u.
func rightOf(u geom.P) geom.P { return geom.P{X: u.Y, Y: -u.X} }

// rotAbout rotates p about c by deg degrees clockwise.
func rotAbout(p, c geom.P, deg float64) geom.P {
	return p.Sub(c).Rot(-deg * math.Pi / 180).Add(c)
}

// roughPoly returns an irregular convex-ish polygon around c.
func roughPoly(rnd func() float64, c geom.P, sizeX, sizeY, headingDeg float64, verts int) geom.Poly {
	u := dirVec(headingDeg)
	w := rightOf(u)
	out := make(geom.Poly, verts)
	phase := rnd() * 2 * math.Pi
	for i := 0; i < verts; i++ {
		a := phase + (float64(i)+0.35*(rnd()-0.5))*2*math.Pi/float64(verts)
		r := 0.62 + 0.38*rnd()
		x := math.Cos(a) * sizeX / 2 * r
		y := math.Sin(a) * sizeY / 2 * r
		out[i] = c.Add(w.Scale(x)).Add(u.Scale(y))
	}
	return out
}
