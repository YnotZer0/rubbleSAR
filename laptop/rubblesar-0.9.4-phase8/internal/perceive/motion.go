package perceive

import (
	"math"
	"sort"
	"time"

	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
)

// Movement during a hover (phase 4).
//
// While a drone holds over a casualty its camera barely moves, but it does
// drift a little (wind, position hold). Every hover frame is first registered
// to the first one (a whole-pixel translation found by matching brightness,
// coarse to fine), then each part on its own (the ring of rubble around it);
// the part's outline is moved back by that shift and compared with the part's
// first outline. Outline pixels that moved by no more than a small tolerance
// (the model's edge jitter) do not count.
//
// A real model's outlines change on their own, though: a forearm's mask takes
// in the hand in one frame and not the next, a part's class flips, a track
// picks up the neighbouring part. So a changed outline only counts when the
// picture changed too: where the old and new outlines disagree, the share of
// pixels that no longer match the hover's first frame (within the tolerance,
// after matching the exposure on the ring of rubble around the part), beyond
// the ring's own share. Where a limb swung, it is gone and the rubble shows (or
// the other way round): most of those pixels changed. Where a still part's
// outline wobbled, the picture is the same: almost none did.

// MotionParams tune the movement check.
type MotionParams struct {
	Change    float64 // share of the outline (0..1) that must change for a frame to count as moved
	TolPx     int     // outline pixels displaced by up to this much are jitter
	MinFrames int     // moved frames needed to call it movement
}

// DefaultMotionParams match the edge-eye defaults.
func DefaultMotionParams() MotionParams { return MotionParams{Change: 0.12, TolPx: 2, MinFrames: 2} }

// Motion is a track's movement record during hovers.
type Motion struct {
	Frames      int
	MovedFrames int
	MaxChange   float64
	First, Last time.Time
	ref         *masks.Bitmap // first hover outline, in the hover's reference frame
}

// photoProbe compares the picture around one part in a hover frame with the
// hover's first frame, after the part's own registration (localAlign).
type photoProbe struct {
	ref, cur  *luma   // first frame (whole, full resolution); this frame around the part
	sx, sy    int     // a point (x, y) of the first frame is at (x+sx, y+sy) here
	gain, off float64 // this frame's brightness = gain * first frame's + off (exposure; gain 1, off from the ring)
	tau       float64 // brightness difference that is a change
	tol       int     // pixels of slack (as for outlines)
	ring      float64 // share of the ring's pixels that changed: this frame's level of change around the part
}

// changedAt reports whether first-frame pixel (x, y) changed: its brightness
// lies outside the range of this frame's pixels within tol of it (widened by
// tau), or this frame's pixel there lies outside the range of the first
// frame's. A range rather than a match: on a sharp edge shifted by part of a
// pixel, a pixel takes an in-between brightness found nowhere in the other
// frame, but it lies between its neighbours there. ok is false off either image.
func (p *photoProbe) changedAt(x, y int) (changed, ok bool) {
	a, ok1 := p.ref.at(x, y)
	b, ok2 := p.cur.at(x+p.sx, y+p.sy)
	if !ok1 || !ok2 {
		return false, false
	}
	within := func(v float64, img *luma, cx, cy int, g, o float64) bool {
		lo, hi := math.Inf(1), math.Inf(-1)
		for dy := -p.tol; dy <= p.tol; dy++ {
			for dx := -p.tol; dx <= p.tol; dx++ {
				if c, ok := img.at(cx+dx, cy+dy); ok {
					w := g*float64(c) + o
					lo, hi = math.Min(lo, w), math.Max(hi, w)
				}
			}
		}
		return v >= lo-p.tau && v <= hi+p.tau
	}
	if !within(p.gain*float64(a)+p.off, p.cur, x+p.sx, y+p.sy, 1, 0) {
		return true, true
	}
	return !within(float64(b), p.ref, x, y, p.gain, p.off), true
}

// share is the share of a region's pixels (first-frame coordinates) that changed.
func (p *photoProbe) share(region *masks.Bitmap) float64 {
	var n, changed, area int
	for y := 0; y < region.H; y++ {
		for x := 0; x < region.W; x++ {
			if !region.Pix[y*region.W+x] {
				continue
			}
			area++
			if c, ok := p.changedAt(region.X0+x, region.Y0+y); ok {
				n++
				if c {
					changed++
				}
			}
		}
	}
	if n < photoMinPx || n < area/2 {
		return 0 // too few pixels, or mostly off the pictures compared: cannot tell
	}
	return float64(changed) / float64(n)
}

// change is how much more of the region changed than of the ring.
func (p *photoProbe) change(region *masks.Bitmap) float64 { return math.Max(0, p.share(region)-p.ring) }

// Moved reports the verdict with the given parameters.
func (m *Motion) Moved(p MotionParams) bool { return m != nil && m.MovedFrames >= p.MinFrames }

// WatchedS is the time the part was watched in hovers, seconds (one frame
// counts as one frame interval, estimated from the frames themselves).
func (m *Motion) WatchedS() float64 {
	if m == nil || m.Frames == 0 {
		return 0
	}
	span := m.Last.Sub(m.First).Seconds()
	if m.Frames > 1 {
		return span * float64(m.Frames) / float64(m.Frames-1)
	}
	return 1
}

// observe adds one hover outline (already in the reference frame's
// coordinates) and, when there is one, the picture around it to check a
// changed outline against (nil: the outline alone decides).
func (m *Motion) observe(mask *masks.Bitmap, photo *photoProbe, at time.Time, p MotionParams) {
	if m.Frames == 0 {
		m.First = at
	}
	m.Frames++
	m.Last = at
	if m.ref == nil {
		c := *mask
		c.Pix = append([]bool(nil), mask.Pix...)
		m.ref = &c
		return
	}
	ch, diff := maskDiff(m.ref, mask, p.TolPx)
	if ch >= glitchChange {
		// The outline is nowhere near where it was: another object taken for
		// this part, or a frame that did not register. A swinging limb keeps
		// part of its outline in place. Not counted.
		m.Frames--
		return
	}
	if photo != nil && ch > p.Change && !devNoPicture {
		// Where the outlines disagree: where the part was and is no longer,
		// and where it is now and was not.
		ph := photo.change(diff)
		if motionTrace != nil {
			motionTrace(m, ch, ph, photo.change(unionMasks(m.ref, mask)))
		}
		if motionImages != nil {
			motionImages(m, ch, ph, photo, diff, mask)
		}
		ch = math.Min(ch, ph)
	}
	m.MaxChange = math.Max(m.MaxChange, ch)
	if ch > p.Change {
		m.MovedFrames++
	}
}

// unionMasks is the union of two outlines.
func unionMasks(a, b *masks.Bitmap) *masks.Bitmap {
	x0, y0 := min(a.X0, b.X0), min(a.Y0, b.Y0)
	x1, y1 := max(a.X0+a.W, b.X0+b.W), max(a.Y0+a.H, b.Y0+b.H)
	u := &masks.Bitmap{X0: x0, Y0: y0, W: x1 - x0, H: y1 - y0}
	u.Pix = make([]bool, u.W*u.H)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			u.Pix[(y-y0)*u.W+x-x0] = a.At(x, y) || b.At(x, y)
		}
	}
	return u
}

// motionTrace, when set (development), sees each changed outline's change and
// the picture's.
var motionTrace func(m *Motion, outline, picture, pictureUnion float64)

// motionImages, when set (development), gets the pictures behind each changed outline.
var motionImages func(m *Motion, outline, picture float64, photo *photoProbe, diff, mask *masks.Bitmap)

// devNoPicture (development): outlines alone decide, as before 0.5.1.
var devNoPicture bool

// glitchChange: outline changes from this up are not movement (see observe).
const glitchChange = 0.95

// MaskChange is the share of two outlines' pixels that lie more than tol
// pixels (Chebyshev distance) from the other outline: 0 for identical masks,
// near 0 for jitter of up to tol pixels, 1 for masks far apart.
func MaskChange(a, b *masks.Bitmap, tol int) float64 {
	ch, _ := maskDiff(a, b, tol)
	return ch
}

// maskDiff is MaskChange with the pixels that changed (nil when none did).
func maskDiff(a, b *masks.Bitmap, tol int) (float64, *masks.Bitmap) {
	x0, y0 := min(a.X0, b.X0)-tol, min(a.Y0, b.Y0)-tol
	x1, y1 := max(a.X0+a.W, b.X0+b.W)+tol, max(a.Y0+a.H, b.Y0+b.H)+tol
	w, h := x1-x0, y1-y0
	if w <= 0 || h <= 0 {
		return 0, nil
	}
	// Summed-area tables of both masks give "any pixel within tol" in O(1).
	sat := func(m *masks.Bitmap) []int32 {
		s := make([]int32, (w+1)*(h+1))
		for y := 0; y < h; y++ {
			var row int32
			for x := 0; x < w; x++ {
				if m.At(x+x0, y+y0) {
					row++
				}
				s[(y+1)*(w+1)+x+1] = s[y*(w+1)+x+1] + row
			}
		}
		return s
	}
	sa, sb := sat(a), sat(b)
	near := func(s []int32, x, y int) bool {
		xa, ya, xb, yb := max(x-tol, 0), max(y-tol, 0), min(x+tol+1, w), min(y+tol+1, h)
		return s[yb*(w+1)+xb]-s[ya*(w+1)+xb]-s[yb*(w+1)+xa]+s[ya*(w+1)+xa] > 0
	}
	var union, changed int
	diff := &masks.Bitmap{X0: x0, Y0: y0, W: w, H: h, Pix: make([]bool, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ia, ib := a.At(x+x0, y+y0), b.At(x+x0, y+y0)
			if !ia && !ib {
				continue
			}
			union++
			if (ia && !near(sb, x, y)) || (ib && !near(sa, x, y)) {
				changed++
				diff.Pix[y*w+x] = true
			}
		}
	}
	if union == 0 || changed == 0 {
		return 0, nil
	}
	return float64(changed) / float64(union), diff
}

// shifted returns the mask moved by (-dx, -dy): from the current frame into
// the reference frame of the hover.
func shifted(m *masks.Bitmap, dx, dy int) *masks.Bitmap {
	c := *m
	c.X0 -= dx
	c.Y0 -= dy
	return &c
}

// --- Registration --------------------------------------------------------

// luma is brightness at one pyramid level over a region (X0, Y0 in level pixels).
type luma struct {
	X0, Y0, W, H int
	Pix          []uint8
}

func (l *luma) at(x, y int) (uint8, bool) {
	x -= l.X0
	y -= l.Y0
	if x < 0 || y < 0 || x >= l.W || y >= l.H {
		return 0, false
	}
	return l.Pix[y*l.W+x], true
}

// lumaRegion is the brightness at 1/f resolution over level-pixel region
// (x0, y0, w, h), clipped to the image. Each output averages s x s samples
// spread over its f x f block (s = f for an exact box average).
func lumaRegion(img *raster.RGB, f, s, x0, y0, w, h int) *luma {
	lw, lh := img.W/f, img.H/f
	x0, y0 = max(x0, 0), max(y0, 0)
	w, h = min(w, lw-x0), min(h, lh-y0)
	out := &luma{X0: x0, Y0: y0, W: max(w, 0), H: max(h, 0)}
	out.Pix = make([]uint8, out.W*out.H)
	step := f / s
	for y := 0; y < out.H; y++ {
		for x := 0; x < out.W; x++ {
			var sum int
			for j := 0; j < s; j++ {
				i := (((y+y0)*f+j*step)*img.W + (x+x0)*f) * 3
				for k := 0; k < s; k++ {
					sum += int(img.Pix[i]) + 2*int(img.Pix[i+1]) + int(img.Pix[i+2])
					i += 3 * step
				}
			}
			out.Pix[y*out.W+x] = uint8(sum / (4 * s * s))
		}
	}
	return out
}

// sad is the mean absolute difference between ref(x, y) and cur(x+dx, y+dy)
// over ref's region less a margin.
func sad(ref, cur *luma, dx, dy, margin int) float64 {
	var s, n int
	for y := ref.Y0 + margin; y < ref.Y0+ref.H-margin; y++ {
		for x := ref.X0 + margin; x < ref.X0+ref.W-margin; x++ {
			c, ok := cur.at(x+dx, y+dy)
			if !ok {
				continue
			}
			d := int(ref.Pix[(y-ref.Y0)*ref.W+x-ref.X0]) - int(c)
			if d < 0 {
				d = -d
			}
			s += d
			n++
		}
	}
	if n < (ref.W-2*margin)*(ref.H-2*margin)/3 {
		return math.Inf(1) // too little overlap to trust
	}
	return float64(s) / float64(n)
}

func bestShift(ref, cur *luma, cx, cy, r, margin int) (int, int) {
	bx, by, best := cx, cy, math.Inf(1)
	for dy := cy - r; dy <= cy+r; dy++ {
		for dx := cx - r; dx <= cx+r; dx++ {
			if v := sad(ref, cur, dx, dy, margin); v < best {
				bx, by, best = dx, dy, v
			}
		}
	}
	return bx, by
}

// regRef is the reference frame of a hover: the whole frame at 1/8, the
// central region at 1/2 and full resolution; last is the latest shift.
type regRef struct {
	l8, l2, l1 *luma
	full       *luma // the whole frame at full resolution (for localShift)
	w, h       int
	lastX      int
	lastY      int
}

func central(w, h int, share float64) (x0, y0, cw, ch int) {
	cw, ch = int(float64(w)*share), int(float64(h)*share)
	return (w - cw) / 2, (h - ch) / 2, cw, ch
}

func newRegRef(img *raster.RGB) *regRef {
	r := &regRef{w: img.W, h: img.H}
	r.l8 = lumaRegion(img, 8, 2, 0, 0, img.W/8, img.H/8)
	x, y, cw, ch := central(img.W/2, img.H/2, 0.35)
	r.l2 = lumaRegion(img, 2, 2, x, y, cw, ch)
	x, y, cw, ch = central(img.W, img.H, 0.2)
	r.l1 = lumaRegion(img, 1, 1, x, y, cw, ch)
	r.full = lumaRegion(img, 1, 1, 0, 0, img.W, img.H)
	return r
}

// Local registration of one part (localShift).
const (
	ringPad   = 16 // pixels of surroundings compared around the part's box
	ringExcl  = 4  // and left out next to it (the part itself may move)
	localSpan = 8  // search ±8 px around the frame's shift
)

// localAlign refines the frame's shift (gx, gy) for one part whose box is
// (x, y, w, h) in img: the shift that best aligns the ring of pixels around
// the box with the reference frame. It returns the picture comparison for the
// part at that shift (exposure and noise measured on the ring; tol as for
// outlines). When the drone drifts, a part on rubble
// higher or lower than the scene's dominant surface moves by more or less than
// the whole frame (parallax: about 5 px for 4 m of rubble and 0.15 m of drift
// at 15 m); a hand is only about 10 px across, so the frame's shift alone
// would make it look as if it moved.
func (r *regRef) localAlign(img *raster.RGB, x, y, w, h, gx, gy, tol int) *photoProbe {
	if r.full == nil || w <= 0 || h <= 0 {
		return nil
	}
	pad := ringPad + localSpan + tol
	cur := lumaRegion(img, 1, 1, x-pad, y-pad, w+2*pad, h+2*pad)
	// The ring in reference coordinates (the box moved back by the frame's shift).
	rx0, ry0, rw, rh := x-gx-ringPad, y-gy-ringPad, w+2*ringPad, h+2*ringPad
	ex0, ey0, ex1, ey1 := x-gx-ringExcl, y-gy-ringExcl, x-gx+w+ringExcl, y-gy+h+ringExcl
	ringPx := rw*rh - (ex1-ex0)*(ey1-ey0)
	bx, by, best := gx, gy, math.Inf(1)
	for sy := gy - localSpan; sy <= gy+localSpan; sy++ {
		for sx := gx - localSpan; sx <= gx+localSpan; sx++ {
			var sum, n int
			for ry := ry0; ry < ry0+rh; ry++ {
				inRows := ry >= ey0 && ry < ey1
				for rx := rx0; rx < rx0+rw; rx++ {
					if inRows && rx >= ex0 && rx < ex1 {
						rx = ex1 - 1
						continue
					}
					a, ok := r.full.at(rx, ry)
					if !ok {
						continue
					}
					b, ok := cur.at(rx+sx, ry+sy)
					if !ok {
						continue
					}
					d := int(a) - int(b)
					if d < 0 {
						d = -d
					}
					sum += d
					n++
				}
			}
			if n < ringPx/3 {
				continue
			}
			// Ties keep the shift nearest the frame's.
			v := float64(sum)/float64(n) + 1e-3*math.Hypot(float64(sx-gx), float64(sy-gy))
			if v < best {
				bx, by, best = sx, sy, v
			}
		}
	}
	if math.IsInf(best, 1) {
		return nil
	}
	pr := &photoProbe{ref: r.full, cur: cur, sx: bx, sy: by, gain: 1, tol: tol}
	// Exposure: the median brightness difference over the ring (within a
	// hover the exposure barely changes; a fitted gain is pulled towards 0 on
	// a dull ring). Noise: the median absolute residual. Then the ring's own
	// share of changed pixels.
	var ring [][2]int
	for ry := ry0; ry < ry0+rh; ry++ {
		for rx := rx0; rx < rx0+rw; rx++ {
			if ry >= ey0 && ry < ey1 && rx >= ex0 && rx < ex1 {
				continue
			}
			ring = append(ring, [2]int{rx, ry})
		}
	}
	var diffs []float64
	for _, q := range ring {
		a, ok1 := r.full.at(q[0], q[1])
		b, ok2 := cur.at(q[0]+bx, q[1]+by)
		if ok1 && ok2 {
			diffs = append(diffs, float64(b)-float64(a))
		}
	}
	if len(diffs) < 20 {
		return nil
	}
	sort.Float64s(diffs)
	pr.off = diffs[len(diffs)/2]
	res := make([]float64, 0, len(ring))
	for _, q := range ring {
		a, ok1 := r.full.at(q[0], q[1])
		b, ok2 := cur.at(q[0]+bx, q[1]+by)
		if ok1 && ok2 {
			res = append(res, math.Abs(float64(b)-pr.gain*float64(a)-pr.off))
		}
	}
	sort.Float64s(res)
	pr.tau = math.Max(photoTauMin, math.Min(photoTauMax, 3*1.4826*res[len(res)/2]))
	var changed, valid int
	for _, q := range ring {
		if c, ok := pr.changedAt(q[0], q[1]); ok {
			valid++
			if c {
				changed++
			}
		}
	}
	if valid > 0 {
		pr.ring = float64(changed) / float64(valid)
	}
	return pr
}

// A brightness difference (luma, 0-255) between the two frames counts as a
// change from 3 times the ring's noise, within these bounds. A region of
// fewer than photoMinPx pixels is too small to tell (one pixel in nine would
// already be 11 %).
const (
	photoTauMin = 12
	photoTauMax = 40
	photoMinPx  = 12
)

// Register finds the whole-pixel shift (dx, dy) of img against the reference:
// a point at (x, y) in the reference appears at (x+dx, y+dy) in img. The drift
// is continuous, so the coarse search (1/8 resolution, ±6, i.e. ±48 px) starts
// from the previous frame's shift; it is refined at 1/2 (±2) and full
// resolution (±1) on the central part of the frame.
func (r *regRef) Register(img *raster.RGB) (dx, dy int) {
	c8 := lumaRegion(img, 8, 2, 0, 0, img.W/8, img.H/8)
	x8, y8 := bestShift(r.l8, c8, int(math.Round(float64(r.lastX)/8)), int(math.Round(float64(r.lastY)/8)), 6, 10)
	m := 4 + 2
	c2 := lumaRegion(img, 2, 2, r.l2.X0+4*x8-m, r.l2.Y0+4*y8-m, r.l2.W+2*m, r.l2.H+2*m)
	x2, y2 := bestShift(r.l2, c2, 4*x8, 4*y8, 2, 0)
	c1 := lumaRegion(img, 1, 1, r.l1.X0+2*x2-m, r.l1.Y0+2*y2-m, r.l1.W+2*m, r.l1.H+2*m)
	x1, y1 := bestShift(r.l1, c1, 2*x2, 2*y2, 1, 0)
	r.lastX, r.lastY = x1, y1
	return x1, y1
}
