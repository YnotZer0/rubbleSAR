// Package masks handles instance masks: connected components of label
// images, exact outline polygons for training labels, polygon filling, and
// mask overlap measures for evaluation.
package masks

import (
	"math"
	"sort"
)

// Component is an 8-connected set of pixels sharing one key.
type Component struct {
	Key            uint32
	Pixels         []int32 // indices y*W+x into the source image
	X0, Y0, X1, Y1 int     // bounding box, X1/Y1 exclusive
	CX, CY         float64 // centroid in pixel coordinates (pixel centres at +0.5)
}

// Area is the pixel count.
func (c *Component) Area() int { return len(c.Pixels) }

// Components finds the 8-connected regions of equal non-zero key.
// key(i) is called with pixel index y*w+x.
func Components(w, h int, key func(i int) uint32) []Component {
	keys := make([]uint32, w*h)
	any := false
	for i := range keys {
		keys[i] = key(i)
		if keys[i] != 0 {
			any = true
		}
	}
	if !any {
		return nil
	}
	seen := make([]bool, w*h)
	var out []Component
	var queue []int32
	for start, k := range keys {
		if k == 0 || seen[start] {
			continue
		}
		c := Component{Key: k, X0: w, Y0: h}
		queue = append(queue[:0], int32(start))
		seen[start] = true
		var sx, sy float64
		for len(queue) > 0 {
			p := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			c.Pixels = append(c.Pixels, p)
			x, y := int(p)%w, int(p)/w
			sx += float64(x) + 0.5
			sy += float64(y) + 0.5
			c.X0, c.Y0 = min(c.X0, x), min(c.Y0, y)
			c.X1, c.Y1 = max(c.X1, x+1), max(c.Y1, y+1)
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					nx, ny := x+dx, y+dy
					if (dx == 0 && dy == 0) || nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					q := ny*w + nx
					if !seen[q] && keys[q] == k {
						seen[q] = true
						queue = append(queue, int32(q))
					}
				}
			}
		}
		sort.Slice(c.Pixels, func(i, j int) bool { return c.Pixels[i] < c.Pixels[j] })
		c.CX, c.CY = sx/float64(len(c.Pixels)), sy/float64(len(c.Pixels))
		out = append(out, c)
	}
	return out
}

// Bitmap is a binary mask placed at (X0, Y0) in some image.
type Bitmap struct {
	X0, Y0, W, H int
	Pix          []bool
}

// At reports whether image pixel (x, y) is set.
func (b *Bitmap) At(x, y int) bool {
	x -= b.X0
	y -= b.Y0
	return x >= 0 && y >= 0 && x < b.W && y < b.H && b.Pix[y*b.W+x]
}

// Area counts set pixels.
func (b *Bitmap) Area() int {
	n := 0
	for _, v := range b.Pix {
		if v {
			n++
		}
	}
	return n
}

// Bitmap returns the component as a mask over its bounding box (imageW is the
// source image width).
func (c *Component) Bitmap(imageW int) Bitmap {
	b := Bitmap{X0: c.X0, Y0: c.Y0, W: c.X1 - c.X0, H: c.Y1 - c.Y0}
	b.Pix = make([]bool, b.W*b.H)
	for _, p := range c.Pixels {
		x, y := int(p)%imageW, int(p)/imageW
		b.Pix[(y-c.Y0)*b.W+(x-c.X0)] = true
	}
	return b
}

// Trace returns the outer boundary of the region containing the first set
// pixel (in raster order) as a polygon through pixel corners, in image
// coordinates. The polygon encloses exactly the region's pixels (holes are
// filled); 8-connected pixels are kept together. Vertices are only emitted
// where the boundary turns.
func Trace(b Bitmap) [][2]int {
	start := -1
	for i, v := range b.Pix {
		if v {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	in := func(x, y int) bool { // local coordinates
		return x >= 0 && y >= 0 && x < b.W && y < b.H && b.Pix[y*b.W+x]
	}
	// Directions: 0 east, 1 south, 2 west, 3 north (y grows downwards).
	dx := [4]int{1, 0, -1, 0}
	dy := [4]int{0, 1, 0, -1}
	// Pixels ahead of corner (x, y) when moving in direction d:
	// front-left and front-right, as offsets from the corner.
	flx := [4]int{0, 0, -1, -1}
	fly := [4]int{-1, 0, 0, -1}
	frx := [4]int{0, -1, -1, 0}
	fry := [4]int{0, 0, -1, -1}

	sx, sy := start%b.W, start/b.W
	x, y, d := sx+1, sy, 0 // walked along the top edge of the first pixel, heading east
	poly := [][2]int{{sx + b.X0, sy + b.Y0}}
	for steps := 0; steps < 4*(b.W+2)*(b.H+2)+8; steps++ {
		// The start corner touches only the first pixel, so the walk passes it once.
		if x == sx && y == sy {
			return poly
		}
		nd := d
		switch {
		case in(x+flx[d], y+fly[d]):
			nd = (d + 3) % 4 // turn left
		case in(x+frx[d], y+fry[d]):
			// straight on
		default:
			nd = (d + 1) % 4 // turn right
		}
		if nd != d {
			poly = append(poly, [2]int{x + b.X0, y + b.Y0})
			d = nd
		}
		x += dx[d]
		y += dy[d]
	}
	return poly // not reached for valid input
}

// Simplify reduces a closed polygon with the Douglas-Peucker rule (eps in
// the polygon's units). At least 3 vertices are kept.
func Simplify(poly [][2]float64, eps float64) [][2]float64 {
	n := len(poly)
	if n <= 3 || eps <= 0 {
		return append([][2]float64(nil), poly...)
	}
	// Split the ring at vertex 0 and the vertex farthest from it.
	far, best := 0, -1.0
	for i := 1; i < n; i++ {
		if d := dist(poly[0], poly[i]); d > best {
			far, best = i, d
		}
	}
	a := dp(poly[:far+1], eps)
	ring := append(append([][2]float64{}, poly[far:]...), poly[0])
	b := dp(ring, eps)
	out := append(a[:len(a)-1], b[:len(b)-1]...)
	if len(out) < 3 {
		return append([][2]float64(nil), poly...)
	}
	return out
}

func dist(a, b [2]float64) float64 { return math.Hypot(a[0]-b[0], a[1]-b[1]) }

func segDist(p, a, b [2]float64) float64 {
	vx, vy := b[0]-a[0], b[1]-a[1]
	l2 := vx*vx + vy*vy
	if l2 == 0 {
		return dist(p, a)
	}
	t := ((p[0]-a[0])*vx + (p[1]-a[1])*vy) / l2
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(p[0]-(a[0]+t*vx), p[1]-(a[1]+t*vy))
}

func dp(pts [][2]float64, eps float64) [][2]float64 {
	if len(pts) <= 2 {
		return append([][2]float64(nil), pts...)
	}
	idx, best := 0, -1.0
	for i := 1; i < len(pts)-1; i++ {
		if d := segDist(pts[i], pts[0], pts[len(pts)-1]); d > best {
			idx, best = i, d
		}
	}
	if best <= eps {
		return [][2]float64{pts[0], pts[len(pts)-1]}
	}
	a := dp(pts[:idx+1], eps)
	b := dp(pts[idx:], eps)
	return append(a[:len(a)-1], b...)
}

// Fill rasterises a polygon: pixel (x, y) is set when its centre lies inside
// (even-odd rule). The result covers [x0, x0+w) x [y0, y0+h).
func Fill(poly [][2]float64, x0, y0, w, h int) Bitmap {
	b := Bitmap{X0: x0, Y0: y0, W: w, H: h, Pix: make([]bool, w*h)}
	n := len(poly)
	if n < 3 {
		return b
	}
	xs := make([]float64, 0, 16)
	for row := 0; row < h; row++ {
		cy := float64(y0+row) + 0.5
		xs = xs[:0]
		for i, j := 0, n-1; i < n; j, i = i, i+1 {
			a, c := poly[i], poly[j]
			if (a[1] > cy) != (c[1] > cy) {
				xs = append(xs, a[0]+(cy-a[1])*(c[0]-a[0])/(c[1]-a[1]))
			}
		}
		sort.Float64s(xs)
		for k := 0; k+1 < len(xs); k += 2 {
			// Pixel centres strictly between the crossings.
			from := int(math.Ceil(xs[k] - 0.5))
			to := int(math.Ceil(xs[k+1]-0.5)) - 1
			for x := max(from, x0); x <= to && x < x0+w; x++ {
				b.Pix[row*w+(x-x0)] = true
			}
		}
	}
	return b
}

// IoU returns the intersection over union of two bitmaps.
func IoU(a, b *Bitmap) float64 {
	inter := 0
	x0, y0 := max(a.X0, b.X0), max(a.Y0, b.Y0)
	x1, y1 := min(a.X0+a.W, b.X0+b.W), min(a.Y0+a.H, b.Y0+b.H)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			if a.At(x, y) && b.At(x, y) {
				inter++
			}
		}
	}
	u := a.Area() + b.Area() - inter
	if u == 0 {
		return 0
	}
	return float64(inter) / float64(u)
}

// Merge joins components (normally the pieces of one occluded object, sharing
// a key) into one: all pixels, the union bounding box and the area-weighted
// centroid. The key of the first component is kept.
func Merge(cs []Component) Component {
	if len(cs) == 0 {
		return Component{}
	}
	m := Component{Key: cs[0].Key, X0: cs[0].X0, Y0: cs[0].Y0, X1: cs[0].X1, Y1: cs[0].Y1}
	var sx, sy float64
	for _, c := range cs {
		m.Pixels = append(m.Pixels, c.Pixels...)
		m.X0, m.Y0 = min(m.X0, c.X0), min(m.Y0, c.Y0)
		m.X1, m.Y1 = max(m.X1, c.X1), max(m.Y1, c.Y1)
		sx += c.CX * float64(c.Area())
		sy += c.CY * float64(c.Area())
	}
	if n := float64(len(m.Pixels)); n > 0 {
		m.CX, m.CY = sx/n, sy/n
	}
	return m
}

// Bridge joins several outlines (the pieces of one object split by an
// occluder) into one polygon, the way Ultralytics merges multi-part COCO
// segments: the pieces are chained by their closest vertices and the joins are
// travelled there and back, so they enclose no area. Filling the result gives
// the union of the pieces. Rings are in order of the chain (see ChainOrder).
func Bridge(rings [][][2]float64) [][2]float64 {
	n := len(rings)
	if n == 0 {
		return nil
	}
	if n == 1 {
		return rings[0]
	}
	// in[i]/out[i]: vertex of ring i joined to the previous/next ring.
	in, out := make([]int, n), make([]int, n)
	for i := 0; i+1 < n; i++ {
		bd := math.Inf(1)
		for a, p := range rings[i] {
			for b, q := range rings[i+1] {
				if d := dist(p, q); d < bd {
					bd, out[i], in[i+1] = d, a, b
				}
			}
		}
	}
	at := func(r [][2]float64, k int) [2]float64 { return r[((k%len(r))+len(r))%len(r)] }
	var poly [][2]float64
	// First ring: all the way round, starting and ending at its exit vertex.
	r0 := rings[0]
	for k := 0; k <= len(r0); k++ {
		poly = append(poly, at(r0, out[0]+k))
	}
	// Middle rings, outward: from the entry vertex forward to the exit vertex.
	fwd := make([]int, n) // steps from in to out going forward
	for i := 1; i < n-1; i++ {
		r := rings[i]
		fwd[i] = ((out[i]-in[i])%len(r) + len(r)) % len(r)
		for k := 0; k <= fwd[i]; k++ {
			poly = append(poly, at(r, in[i]+k))
		}
	}
	// Last ring: all the way round from its entry vertex.
	rl := rings[n-1]
	for k := 0; k <= len(rl); k++ {
		poly = append(poly, at(rl, in[n-1]+k))
	}
	// Middle rings, back: the rest of each ring, from the exit vertex round to the entry vertex.
	for i := n - 2; i >= 1; i-- {
		r := rings[i]
		for k := fwd[i]; k <= len(r); k++ {
			poly = append(poly, at(r, in[i]+k))
		}
	}
	// The closing edge returns to the first ring's exit vertex.
	return poly
}

// ChainOrder orders pieces for Bridge: from the largest, always on to the
// nearest piece not yet used (by closest vertices).
func ChainOrder(rings [][][2]float64, areas []int) []int {
	n := len(rings)
	if n == 0 {
		return nil
	}
	start := 0
	for i := range areas {
		if areas[i] > areas[start] {
			start = i
		}
	}
	used := make([]bool, n)
	order := []int{start}
	used[start] = true
	for len(order) < n {
		cur := rings[order[len(order)-1]]
		best, bd := -1, math.Inf(1)
		for j := 0; j < n; j++ {
			if used[j] {
				continue
			}
			for _, p := range cur {
				for _, q := range rings[j] {
					if d := dist(p, q); d < bd {
						best, bd = j, d
					}
				}
			}
		}
		used[best] = true
		order = append(order, best)
	}
	return order
}
