package render

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/raster"
	"rubblesar/internal/scenario"
)

// pad is the extra border rendered around a window so that blurs see the same
// neighbourhood as in a full render.
const pad = 40

type slab struct {
	poly   geom.Poly
	bb     geom.Box
	c      geom.P
	z0     float64
	gx, gy float64
	col    [3]float64
	seed   uint64
}

type chunk struct {
	sh     shape
	kind   int // chunkBlock, chunkPlank, chunkRebar, chunkSheet
	c      geom.P
	u      geom.P
	z      float64
	tx, ty float64 // top tilt, m per m
	mat    uint8
	col    [3]float64
	tbase  uint8
	seed   uint64
}

const (
	chunkBlock = iota
	chunkPlank
	chunkRebar
	chunkSheet
)

// siteModel is the full deterministic description of one site.
type siteModel struct {
	s       *scenario.Scenario
	st      scenario.Site
	fr      Frame
	local   geo.Local
	poly    geom.Poly
	polyBB  geom.Box
	spill   float64
	rampIn  float64
	seed    uint64
	gseed   uint64 // ground seed (whole scenario)
	slabs   []slab
	chunks  []chunk
	decoys  []Decoy
	decoyCh []chunk
	victims []*victimGeom
}

func newSiteModel(s *scenario.Scenario, st scenario.Site) *siteModel {
	m := &siteModel{s: s, st: st, fr: SiteFrame(s, st), local: s.Local(), poly: st.Poly(), spill: st.SpillM,
		seed: uint64(st.Seed), gseed: uint64(s.Seed)*0x9E3779B97F4A7C15 + 17}
	m.polyBB = m.poly.Bounds()
	// Deepest inside point (coarse search) sets the mound shape.
	inner := 0.5
	for x := m.polyBB.Min.X; x <= m.polyBB.Max.X; x += 0.5 {
		for y := m.polyBB.Min.Y; y <= m.polyBB.Max.Y; y += 0.5 {
			if d := -m.poly.SignedDist(geom.P{X: x, Y: y}); d > inner {
				inner = d
			}
		}
	}
	if st.Kind == "mound" {
		m.rampIn = inner
	} else {
		m.rampIn = math.Min(1.5, inner)
	}
	rng := rand.New(rand.NewSource(st.Seed))
	m.makeSlabs(rng)
	m.makeChunks(rng)
	vs := s.VictimsAt(st.ID)
	for _, v := range vs {
		g := buildVictim(v, st.Seed^int64(v.Index)*7919)
		g.z0 = m.surfaceAt(geom.P{X: v.E, Y: v.N}) + 0.01
		maxLift := 0.0
		for _, p := range g.primsA {
			maxLift = math.Max(maxLift, p.lift)
		}
		for i := range g.occl {
			g.occl[i].z = g.z0 + maxLift + 0.03
		}
		m.victims = append(m.victims, g)
	}
	m.makeDecoys(rng)
	return m
}

func (m *siteModel) profile(d float64) float64 {
	if d >= m.spill {
		return 0
	}
	return smoothstep((m.spill - d) / (m.spill + m.rampIn))
}

// baseHeight is the rubble pile without slabs or loose debris.
func (m *siteModel) baseHeight(p geom.P, d float64) float64 {
	prof := m.profile(d)
	if prof <= 0 {
		return 0
	}
	s := m.seed
	h := m.st.HeightM * prof * (0.8 + 0.2*raster.FBM(s+21, p.X/4, p.Y/4, 3))
	h += prof * (0.3*raster.FBM(s+22, p.X/0.7, p.Y/0.7, 2) + 0.04*raster.Value(s+23, p.X/0.06, p.Y/0.06))
	return math.Max(0, h)
}

func (m *siteModel) slabTop(p geom.P) (float64, int) {
	best, idx := -1.0, -1
	for i := range m.slabs {
		sl := &m.slabs[i]
		if !sl.bb.ContainsP(p) {
			continue
		}
		q := geom.P{X: p.X + 0.35*raster.Value(sl.seed, p.X/1.2, p.Y/1.2), Y: p.Y + 0.35*raster.Value(sl.seed+1, p.X/1.2, p.Y/1.2)}
		if !sl.poly.Contains(q) {
			continue
		}
		z := sl.z0 + sl.gx*(p.X-sl.c.X) + sl.gy*(p.Y-sl.c.Y)
		if z > best {
			best, idx = z, i
		}
	}
	return best, idx
}

// surfaceAt is the analytic rubble surface (pile and slabs) used to seat
// debris and bodies; it does not depend on the render window.
func (m *siteModel) surfaceAt(p geom.P) float64 {
	h := m.baseHeight(p, m.poly.SignedDist(p))
	if z, i := m.slabTop(p); i >= 0 && z > h {
		h = z
	}
	return h
}

func (m *siteModel) randomPileP(rng *rand.Rand, minProf float64) (geom.P, float64, bool) {
	b := m.polyBB.Grow(m.spill)
	for tries := 0; tries < 50; tries++ {
		p := geom.P{X: b.Min.X + rng.Float64()*(b.Max.X-b.Min.X), Y: b.Min.Y + rng.Float64()*(b.Max.Y-b.Min.Y)}
		d := m.poly.SignedDist(p)
		prof := m.profile(d)
		if prof > minProf && rng.Float64() < 0.25+0.75*prof {
			return p, d, true
		}
	}
	return geom.P{}, 0, false
}

func (m *siteModel) makeSlabs(rng *rand.Rand) {
	if m.st.Kind != "pancake" {
		return
	}
	n := 3 + rng.Intn(4)
	for i := 0; i < n; i++ {
		var c geom.P
		for tries := 0; tries < 100; tries++ {
			c = geom.P{X: m.polyBB.Min.X + rng.Float64()*(m.polyBB.Max.X-m.polyBB.Min.X), Y: m.polyBB.Min.Y + rng.Float64()*(m.polyBB.Max.Y-m.polyBB.Min.Y)}
			if m.poly.SignedDist(c) < -1 {
				break
			}
		}
		l, w := 4+6*rng.Float64(), 3+4*rng.Float64()
		poly := geom.RotRect(c, l, w, rng.Float64()*180)
		mat := matConcrete
		if rng.Float64() < 0.3 {
			mat = matConcreteDark
		}
		m.slabs = append(m.slabs, slab{poly: poly, bb: poly.Bounds().Grow(0.4), c: c,
			z0: m.st.HeightM * (0.75 + 0.3*rng.Float64()), gx: 0.24 * (rng.Float64() - 0.5), gy: 0.24 * (rng.Float64() - 0.5),
			col: jitterColour(rng, materials[mat]), seed: rng.Uint64()})
	}
}

func pickMat(rng *rand.Rand, table []uint8, weights []float64) uint8 {
	x := rng.Float64()
	for i, w := range weights {
		if x < w {
			return table[i]
		}
		x -= w
	}
	return table[len(table)-1]
}

func (m *siteModel) makeChunks(rng *rand.Rand) {
	area := m.poly.Area()
	per := 0.0
	for i := range m.poly {
		per += m.poly[i].Dist(m.poly[(i+1)%len(m.poly)])
	}
	area += per * m.spill * 0.6
	mats := []uint8{matConcrete, matConcreteDark, matPlaster, matBrick, matTile, matDust}
	weights := []float64{0.45, 0.2, 0.15, 0.1, 0.05, 0.05}
	if m.st.Kind == "mound" {
		weights = []float64{0.25, 0.1, 0.15, 0.4, 0.1, 0.0}
	}
	add := func(count float64, f func(p geom.P)) {
		n := int(count * area)
		for i := 0; i < n; i++ {
			p, _, ok := m.randomPileP(rng, 0.02)
			if ok {
				f(p)
			}
		}
	}
	block := func(minS, maxS float64) func(p geom.P) {
		return func(p geom.P) {
			size := minS + (maxS-minS)*rng.Float64()*rng.Float64()
			mat := pickMat(rng, mats, weights)
			aspect := 0.55 + 0.45*rng.Float64()
			poly := roughPoly(rng.Float64, p, size, size*aspect, rng.Float64()*180, 5+rng.Intn(4))
			m.chunks = append(m.chunks, chunk{sh: newPolygon(poly), kind: chunkBlock, c: p, z: m.surfaceAt(p) + size*(0.12+0.35*rng.Float64()),
				tx: 0.6 * (rng.Float64() - 0.5), ty: 0.6 * (rng.Float64() - 0.5), mat: mat, col: jitterColour(rng, materials[mat]), seed: rng.Uint64()})
		}
	}
	add(0.07, block(0.9, 2.2))
	add(1.6, block(0.3, 0.9))
	add(0.25, func(p geom.P) { // timber planks and beams
		l, w := 0.8+2.2*rng.Float64(), 0.08+0.14*rng.Float64()
		hd := rng.Float64() * 180
		m.chunks = append(m.chunks, chunk{sh: newPolygon(geom.RotRect(p, l, w, hd)), kind: chunkPlank, c: p, u: dirVec(hd),
			z: m.surfaceAt(p) + 0.05 + 0.2*rng.Float64(), tx: 0.2 * (rng.Float64() - 0.5), ty: 0.2 * (rng.Float64() - 0.5),
			mat: matTimber, col: jitterColour(rng, materials[matTimber]), seed: rng.Uint64()})
	})
	add(12.0, block(0.05, 0.3))
	add(0.08, func(p geom.P) { // fabric, foam, carpet fragments
		size := 0.3 + 0.9*rng.Float64()
		mat, col := matFabric, fabricColours[rng.Intn(len(fabricColours))]
		if rng.Float64() < 0.3 {
			mat, col = matFoam, materials[matFoam].rgb
		}
		m.chunks = append(m.chunks, chunk{sh: newPolygon(roughPoly(rng.Float64, p, size, size*(0.4+0.6*rng.Float64()), rng.Float64()*180, 7)),
			kind: chunkBlock, c: p, z: m.surfaceAt(p) + 0.03, mat: mat, col: col, seed: rng.Uint64()})
	})
	add(0.3, func(p geom.P) { // glass shards
		size := 0.04 + 0.08*rng.Float64()
		m.chunks = append(m.chunks, chunk{sh: newPolygon(roughPoly(rng.Float64, p, size, size*0.6, rng.Float64()*180, 4)),
			kind: chunkBlock, c: p, z: m.surfaceAt(p) + 0.02, mat: matGlass, col: jitterColour(rng, materials[matGlass]), seed: rng.Uint64()})
	})
	add(0.2, func(p geom.P) { // reinforcing bar sticking out
		l := 0.4 + 1.6*rng.Float64()
		u := dirVec(rng.Float64() * 360)
		m.chunks = append(m.chunks, chunk{sh: capsule{p, p.Add(u.Scale(l)), 0.006 + 0.006*rng.Float64()}, kind: chunkRebar, c: p, u: u,
			z: m.surfaceAt(p) + 0.08 + 0.25*rng.Float64(), mat: matRebar, col: jitterColour(rng, materials[matRebar]), seed: rng.Uint64()})
	})
}

func (m *siteModel) farFromVictims(p geom.P, dist float64) bool {
	for _, g := range m.victims {
		for _, ax := range g.axes {
			if ax.Anchor.Dist(p) < dist {
				return false
			}
		}
		if (geom.P{X: g.v.E, Y: g.v.N}).Dist(p) < dist {
			return false
		}
	}
	return true
}

func (m *siteModel) makeDecoys(rng *rand.Rand) {
	place := func() (geom.P, bool) {
		for tries := 0; tries < 200; tries++ {
			p := geom.P{X: m.polyBB.Min.X + rng.Float64()*(m.polyBB.Max.X-m.polyBB.Min.X), Y: m.polyBB.Min.Y + rng.Float64()*(m.polyBB.Max.Y-m.polyBB.Min.Y)}
			if m.poly.SignedDist(p) < -1 && m.farFromVictims(p, 1.8) {
				return p, true
			}
		}
		return geom.P{}, false
	}
	add := func(kind string, p geom.P, size float64, ch chunk) {
		ll := m.local.ToPoint(p.X, p.Y)
		m.decoys = append(m.decoys, Decoy{Kind: kind, E: round3(p.X), N: round3(p.Y), Lat: round7(ll.Lat), Lon: round7(ll.Lon), SizeM: round3(size)})
		m.decoyCh = append(m.decoyCh, ch)
	}
	if p, ok := place(); ok { // sun-heated metal sheet
		l, w := 0.9+0.9*rng.Float64(), 0.6+0.6*rng.Float64()
		hd := rng.Float64() * 180
		add("metal_sheet", p, l, chunk{sh: newPolygon(geom.RotRect(p, l, w, hd)), kind: chunkSheet, c: p, u: dirVec(hd), z: m.surfaceAt(p) + 0.06,
			tx: 0.3 * (rng.Float64() - 0.5), ty: 0.3 * (rng.Float64() - 0.5), mat: matMetalSheet, col: jitterColour(rng, materials[matMetalSheet]), seed: rng.Uint64()})
	}
	if p, ok := place(); ok { // smouldering spot
		size := 0.25 + 0.35*rng.Float64()
		add("hot_spot", p, size, chunk{sh: newPolygon(roughPoly(rng.Float64, p, size, size*0.8, rng.Float64()*180, 8)), kind: chunkBlock, c: p,
			z: m.surfaceAt(p) + 0.03, mat: matCharred, col: jitterColour(rng, materials[matCharred]), tbase: uint8(40 + rng.Intn(41)), seed: rng.Uint64()})
	}
	if p, ok := place(); ok { // skin-coloured object (doll limb, glove, cushion): looks like a body part, is cold
		l := 0.18 + 0.12*rng.Float64()
		u := dirVec(rng.Float64() * 360)
		col := skinTones[rng.Intn(3)]
		add("skin_like", p, l, chunk{sh: capsule{p, p.Add(u.Scale(l)), 0.035 + 0.015*rng.Float64()}, kind: chunkRebar, c: p, u: u,
			z: m.surfaceAt(p) + 0.04, mat: matSkinLike, col: col, seed: rng.Uint64()})
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round7(v float64) float64 { return math.Round(v*1e7) / 1e7 }

// work holds the float buffers of one render.
type work struct {
	r      raster.Rect // in site pixels, includes the pad
	rgb    []float32   // 3 per pixel
	h      []float32
	mat    []uint8
	parts  []uint8
	victim []uint8
	couple []uint8
	tbase  []uint8
	gb     []uint8 // share of open ground in the base rubble (spill fringe), 0..255
}

func (w *work) idx(x, y int) int { return (y-w.r.Y0)*w.r.W() + (x - w.r.X0) }

func (w *work) setRGB(i int, c [3]float64) {
	w.rgb[3*i], w.rgb[3*i+1], w.rgb[3*i+2] = float32(c[0]), float32(c[1]), float32(c[2])
}

// fill calls fn for every work pixel inside sh, with the pixel centre and the
// depth inside the shape (metres from its edge).
func (m *siteModel) fill(w *work, sh shape, fn func(i int, p geom.P, depth float64)) {
	b := sh.bounds()
	x0f, y0f := m.fr.ENToPix(b.Min.X, b.Max.Y)
	x1f, y1f := m.fr.ENToPix(b.Max.X, b.Min.Y)
	r := raster.Rect{X0: int(math.Floor(x0f)), Y0: int(math.Floor(y0f)), X1: int(math.Ceil(x1f)) + 1, Y1: int(math.Ceil(y1f)) + 1}.Intersect(w.r)
	for y := r.Y0; y < r.Y1; y++ {
		for x := r.X0; x < r.X1; x++ {
			e, n := m.fr.PixToEN(float64(x), float64(y))
			p := geom.P{X: e, Y: n}
			if d := sh.sd(p); d <= 0 {
				fn(w.idx(x, y), p, -d)
			}
		}
	}
}

// renderWindow renders the window (site pixels). poseB lists victims shown in
// their alternate pose. bodyCount, if not nil, receives each victim's body
// pixel count before occluding debris is added.
func (m *siteModel) renderWindow(win raster.Rect, poseB map[int]bool, bodyCount map[int]int) *Layers {
	full := raster.Rect{X1: m.fr.W, Y1: m.fr.H}
	wr := win.Grow(pad).Intersect(full)
	n := wr.W() * wr.H()
	w := &work{r: wr, rgb: make([]float32, 3*n), h: make([]float32, n), mat: make([]uint8, n), parts: make([]uint8, n),
		victim: make([]uint8, n), couple: make([]uint8, n), tbase: make([]uint8, n), gb: make([]uint8, n)}

	// 1. Ground, rubble pile and slabs.
	s := m.seed
	concrete, dust := materials[matConcrete].rgb, materials[matDust].rgb
	raster.Rows(wr.H(), func(row int) {
		y := wr.Y0 + row
		for x := wr.X0; x < wr.X1; x++ {
			e, nn := m.fr.PixToEN(float64(x), float64(y))
			p := geom.P{X: e, Y: nn}
			i := w.idx(x, y)
			g := GroundRGB(m.gseed, e, nn, 0)
			var d float64
			if m.polyBB.Grow(m.spill + 0.1).ContainsP(p) {
				d = m.poly.SignedDist(p)
			} else {
				d = m.spill + 1
			}
			if d >= m.spill {
				w.setRGB(i, g)
				w.mat[i] = matGround
				continue
			}
			prof := m.profile(d)
			h := m.baseHeight(p, d)
			k := 0.5 + 0.5*raster.FBM(s+11, e/0.9, nn/0.9, 3)
			grain := 1 + 0.09*raster.Value(s+12, e/0.025, nn/0.025) + 0.06*raster.FBM(s+13, e/0.2, nn/0.2, 2)
			var c [3]float64
			for j := 0; j < 3; j++ {
				rub := (concrete[j]*(1-k) + dust[j]*k) * grain
				c[j] = g[j]*(1-prof) + rub*prof
			}
			mat := matDust
			// Dark voids between pieces of rubble.
			if vd := raster.FBM(s+14, e/1.1, nn/1.1, 3); vd < -0.42 && prof > 0.3 {
				k := 1 - 0.65*clamp01((-0.42-vd)/0.12)
				c = [3]float64{c[0] * k, c[1] * k, c[2] * k}
				h = math.Max(0, h-0.3*(1-k))
			}
			if z, si := m.slabTop(p); si >= 0 && z > h {
				sl := &m.slabs[si]
				h = z
				mat = matConcrete
				sg := 1 + 0.07*raster.Value(sl.seed+2, e/0.03, nn/0.03) + 0.05*raster.FBM(sl.seed+3, e/0.5, nn/0.5, 2)
				c = [3]float64{sl.col[0] * sg, sl.col[1] * sg, sl.col[2] * sg}
				if math.Abs(raster.FBM(sl.seed+4, e/1.5, nn/1.5, 3)) < 0.022 { // crack
					c = [3]float64{c[0] * 0.5, c[1] * 0.5, c[2] * 0.5}
					h -= 0.04
				}
			}
			w.h[i] = float32(h)
			w.mat[i] = mat
			if mat == matDust {
				w.gb[i] = raster.Clamp8((1 - prof) * 255)
			}
			w.setRGB(i, c)
		}
	})

	// 2. Loose debris, in a fixed order with a height test.
	for ci := range m.chunks {
		m.drawChunk(w, &m.chunks[ci], true)
	}
	// 3. Decoys lie on top.
	for ci := range m.decoyCh {
		m.drawChunk(w, &m.decoyCh[ci], false)
	}
	// 4. Bodies.
	for _, g := range m.victims {
		prims := g.primsA
		if poseB[g.v.Index] && g.primsB != nil {
			prims = g.primsB
		}
		m.drawBody(w, g, prims)
	}
	if bodyCount != nil {
		for i, v := range w.victim {
			if v > 0 && w.parts[i] > 0 {
				bodyCount[int(v)]++
			}
		}
	}
	// 5. Debris resting on bodies.
	for _, g := range m.victims {
		for _, o := range g.occl {
			o := o
			m.fill(w, o.sh, func(i int, p geom.P, depth float64) {
				if w.parts[i] > 0 {
					w.couple[i] = coupleOccl
				}
				w.parts[i] = 0
				w.mat[i] = o.mat
				w.gb[i] = 0
				w.h[i] = float32(o.z)
				gr := 1 + 0.08*raster.Value(m.seed+31, p.X/0.02, p.Y/0.02)
				if depth < 0.008 {
					gr *= 0.8
				}
				w.setRGB(i, [3]float64{o.col[0] * gr, o.col[1] * gr, o.col[2] * gr})
			})
		}
	}

	// 6. Ambient occlusion from the height field: crevices darker, ridges lighter.
	hq := make([]int32, len(w.h)) // 0.1 mm steps, so blurs are exact
	for i, v := range w.h {
		hq[i] = int32(math.Round(float64(v) * 10000))
	}
	b1 := raster.BoxBlurExact(hq, wr.W(), wr.H(), 6)
	b2 := raster.BoxBlurExact(hq, wr.W(), wr.H(), 30)
	raster.Rows(wr.H(), func(row int) {
		for x := 0; x < wr.W(); x++ {
			i := row*wr.W() + x
			if w.mat[i] == matGround {
				continue
			}
			hv := float32(hq[i])
			c1 := clampf((hv-b1[i])/600, -1, 1)
			c2 := clampf((hv-b2[i])/2500, -1, 1)
			k := float32(1 + 0.20*float64(c1) + 0.12*float64(c2))
			if w.parts[i] > 0 {
				k = 1 + 0.4*(k-1)
			}
			w.rgb[3*i] *= k
			w.rgb[3*i+1] *= k
			w.rgb[3*i+2] *= k
		}
	})

	// 7. Copy the window out.
	out := NewLayers(win)
	for y := win.Y0; y < win.Y1; y++ {
		for x := win.X0; x < win.X1; x++ {
			i := w.idx(x, y)
			o := (y-win.Y0)*win.W() + (x - win.X0)
			out.RGB.Pix[3*o] = raster.Clamp8(float64(w.rgb[3*i]))
			out.RGB.Pix[3*o+1] = raster.Clamp8(float64(w.rgb[3*i+1]))
			out.RGB.Pix[3*o+2] = raster.Clamp8(float64(w.rgb[3*i+2]))
			hmm := math.Round(float64(w.h[i]) * 1000)
			out.Height.Pix[o] = uint16(math.Max(0, math.Min(65535, hmm)))
			out.Parts.Pix[o] = w.parts[i]
			out.Victim.Pix[o] = w.victim[i]
			out.Coupling.Pix[o] = w.couple[i]
			g := float64(w.gb[i]) / 255
			out.Absorb.Pix[o] = raster.Clamp8((absorbOf(w.mat[i])*(1-g) + GroundAbsorb*g) * 255)
			out.TBase.Pix[o] = w.tbase[i]
		}
	}
	return out
}

func clampf(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absorbOf(mat uint8) float64 {
	switch mat {
	case matSkin:
		return 0.6
	case matCloth, matFabric:
		return 0.7
	case matHair:
		return 0.85
	}
	if m, ok := materials[mat]; ok {
		return m.absorb
	}
	return 0.7
}

func (m *siteModel) drawChunk(w *work, c *chunk, ztest bool) {
	m.fill(w, c.sh, func(i int, p geom.P, depth float64) {
		d := p.Sub(c.c)
		z := c.z + c.tx*d.X + c.ty*d.Y
		var gr float64
		switch c.kind {
		case chunkRebar:
			cp := c.sh.(capsule)
			k := clamp01(1 - depth/cp.r)
			z = c.z + cp.r*math.Sqrt(1-k*k)
			gr = 1.05 - 0.35*k
		case chunkPlank:
			along := d.Dot(c.u)
			across := d.X*c.u.Y - d.Y*c.u.X
			gr = 1 + 0.12*raster.Value(c.seed, along/0.35, across/0.008) + 0.05*raster.Value(c.seed+1, p.X/0.02, p.Y/0.02)
		case chunkSheet:
			along := d.Dot(c.u)
			gr = 1 + 0.12*math.Sin(along/0.025) + 0.03*raster.Value(c.seed, p.X/0.02, p.Y/0.02)
		default:
			gr = 1 + materials[c.mat].grain*raster.Value(c.seed, p.X/0.02, p.Y/0.02) + 0.05*raster.Value(c.seed+1, p.X/0.12, p.Y/0.12)
		}
		if ztest && float32(z) < w.h[i]-0.01 {
			return
		}
		if c.kind != chunkRebar && depth < 0.012 {
			gr *= 0.78
		}
		w.h[i] = float32(z)
		w.mat[i] = c.mat
		w.gb[i] = 0
		w.tbase[i] = c.tbase
		w.setRGB(i, [3]float64{c.col[0] * gr, c.col[1] * gr, c.col[2] * gr})
	})
}

func (m *siteModel) drawBody(w *work, g *victimGeom, prims []prim) {
	vs := uint64(g.v.Index)*1000003 + m.seed
	dustAmt := g.v.Look.Dust
	for _, pr := range prims {
		pr := pr
		m.fill(w, pr.sh, func(i int, p geom.P, depth float64) {
			col, cp, mat := pr.col, pr.couple, pr.mat
			var prof float64
			switch sh := pr.sh.(type) {
			case capsule:
				k := clamp01(1 - depth/sh.r)
				prof = math.Sqrt(1 - k*k)
			case ellipse:
				k := sh.k(p)
				prof = math.Sqrt(clamp01(1 - k*k))
				if pr.hair {
					x, _ := sh.local(p)
					if x >= -pr.hairX*sh.ax {
						col, cp, mat = pr.hairCol, coupleHair, matHair
						hs := 1 + 0.18*raster.Value(vs+5, p.X/0.004, p.Y/0.02)
						col = [3]float64{col[0] * hs, col[1] * hs, col[2] * hs}
					}
				}
			}
			shade := 0.72 + 0.28*prof
			gr := 1 + 0.04*raster.Value(vs+1, p.X/0.01, p.Y/0.01)
			if mat == matCloth {
				gr += 0.10 * raster.FBM(vs+2, p.X/0.04, p.Y/0.04, 2)
			}
			k := shade * gr
			c := [3]float64{col[0] * k, col[1] * k, col[2] * k}
			dk := clamp01(dustAmt*(0.55+0.45*raster.FBM(vs+3, p.X/0.05, p.Y/0.05, 2))) * 0.75
			for j := 0; j < 3; j++ {
				c[j] = c[j]*(1-dk) + dustColour[j]*dk*shade
			}
			w.setRGB(i, c)
			w.h[i] = float32(g.z0 + pr.lift*prof)
			w.parts[i] = uint8(pr.class)
			w.victim[i] = uint8(g.v.Index)
			w.couple[i] = cp
			w.mat[i] = mat
			w.gb[i] = 0
			w.tbase[i] = 0
		})
	}
	// Warmth bleeding into debris touching the body.
	const bleed = 0.12
	bb := primsBounds(prims).Grow(bleed)
	m.fill(w, polygon{p: geom.Poly{bb.Min, {X: bb.Max.X, Y: bb.Min.Y}, bb.Max, {X: bb.Min.X, Y: bb.Max.Y}}, bb: bb}, func(i int, p geom.P, _ float64) {
		if w.parts[i] > 0 || (w.victim[i] != 0 && w.victim[i] != uint8(g.v.Index)) {
			return
		}
		d := math.Inf(1)
		for _, pr := range prims {
			d = math.Min(d, pr.sh.sd(p))
		}
		if d < bleed {
			c := uint8(float64(coupleBleed) * (1 - d/bleed))
			if c > w.couple[i] {
				w.couple[i] = c
				w.victim[i] = uint8(g.v.Index)
			}
		}
	})
}

// patchRect returns the pose-B patch rectangle of a moving victim.
func (m *siteModel) patchRect(g *victimGeom) raster.Rect {
	b := primsBounds(append(append([]prim{}, g.primsA...), g.primsB...)).Grow(0.25)
	x0, y0 := m.fr.ENToPix(b.Min.X, b.Max.Y)
	x1, y1 := m.fr.ENToPix(b.Max.X, b.Min.Y)
	r := raster.Rect{X0: int(math.Floor(x0/4)) * 4, Y0: int(math.Floor(y0/4)) * 4, X1: int(math.Ceil((x1+1)/4)) * 4, Y1: int(math.Ceil((y1+1)/4)) * 4}
	return r.Intersect(raster.Rect{X1: m.fr.W, Y1: m.fr.H})
}

// RenderSite renders one site into <scenarioDir>/sites/<id>/ and returns its manifest.
func RenderSite(scenarioDir string, s *scenario.Scenario, siteID string) (*Manifest, error) {
	st, ok := s.Site(siteID)
	if !ok {
		return nil, diag.New("E-SCN-003", "scenario %s has no site %s", s.ID, siteID)
	}
	if len(s.Victims) > 255 {
		return nil, diag.New("E-SCN-003", "%d victims; the layer format supports 255", len(s.Victims))
	}
	t0 := time.Now()
	m := newSiteModel(s, *st)
	dir := SiteDir(scenarioDir, siteID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, diag.Wrap("E-RND-001", err, "creating %s", dir)
	}
	bodyCount := map[int]int{}
	base := m.renderWindow(raster.Rect{X1: m.fr.W, Y1: m.fr.H}, nil, bodyCount)
	sums, err := base.Save(dir)
	if err != nil {
		return nil, err
	}
	man := &Manifest{Schema: 1, Renderer: RendererVersion, ScenarioID: s.ID, ScenarioHash: s.Hash(), SiteID: siteID,
		Frame: m.fr, Files: sums, Decoys: m.decoys}
	if man.Decoys == nil {
		man.Decoys = []Decoy{}
	}
	// Remove stale pose-B folders, then render the current ones.
	os.RemoveAll(filepath.Join(dir, "pose-b"))
	for _, g := range m.victims {
		if g.primsB == nil {
			continue
		}
		r := m.patchRect(g)
		l := m.renderWindow(r, map[int]bool{g.v.Index: true}, nil)
		rel := filepath.Join("pose-b", g.v.ID)
		ps, err := l.Save(filepath.Join(dir, rel))
		if err != nil {
			return nil, err
		}
		man.Patches = append(man.Patches, Patch{Victim: g.v.Index, X0: r.X0, Y0: r.Y0, X1: r.X1, Y1: r.Y1, Dir: rel, Files: ps})
	}
	if man.Patches == nil {
		man.Patches = []Patch{}
	}
	man.Truth, man.Victims = m.truth(base, bodyCount)
	man.RenderSeconds = math.Round(time.Since(t0).Seconds()*100) / 100
	if err := SaveManifest(dir, man); err != nil {
		return nil, err
	}
	return man, nil
}

// truth measures what is actually visible in the base layers.
func (m *siteModel) truth(l *Layers, bodyCount map[int]int) ([]TruthPart, []TruthVictim) {
	parts := map[[2]int]*acc{}
	vict := map[int]*acc{}
	for y := 0; y < l.Rect.H(); y++ {
		for x := 0; x < l.Rect.W(); x++ {
			i := y*l.Rect.W() + x
			c := int(l.Parts.Pix[i])
			if c == 0 {
				continue
			}
			v := int(l.Victim.Pix[i])
			e, n := m.fr.PixToEN(float64(x+l.Rect.X0), float64(y+l.Rect.Y0))
			h := float64(l.Height.Pix[i]) / 1000
			for _, a := range []*acc{getAcc(parts, [2]int{v, c}), getAccV(vict, v)} {
				a.n++
				a.sx += e
				a.sy += n
				a.sh += h
			}
		}
	}
	px := m.fr.GSD * m.fr.GSD
	var tp []TruthPart
	keys := make([][2]int, 0, len(parts))
	for k := range parts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		a := parts[k]
		e, n := a.sx/float64(a.n), a.sy/float64(a.n)
		ll := m.local.ToPoint(e, n)
		vid := ""
		if v, ok := m.s.VictimByIndex(k[0]); ok {
			vid = v.ID
		}
		tp = append(tp, TruthPart{Victim: k[0], VictimID: vid, Class: k[1], ClassName: scenario.PartNames[k[1]], Pixels: a.n,
			AreaM2: round4(float64(a.n) * px), E: round3(e), N: round3(n), Lat: round7(ll.Lat), Lon: round7(ll.Lon), HeightM: round3(a.sh / float64(a.n))})
	}
	var tv []TruthVictim
	for _, g := range m.victims {
		idx := g.v.Index
		t := TruthVictim{Victim: idx, ID: g.v.ID, PatchVictim: g.primsB != nil, ClassesShown: []int{}}
		if a, ok := vict[idx]; ok && a.n > 0 {
			e, n := a.sx/float64(a.n), a.sy/float64(a.n)
			ll := m.local.ToPoint(e, n)
			t.Pixels, t.AreaM2 = a.n, round4(float64(a.n)*px)
			t.E, t.N, t.Lat, t.Lon = round3(e), round3(n), round7(ll.Lat), round7(ll.Lon)
			if bodyCount[idx] > 0 {
				t.VisibleFrac = math.Round(float64(a.n)/float64(bodyCount[idx])*1000) / 1000
			}
		}
		for _, k := range keys {
			if k[0] == idx {
				t.ClassesShown = append(t.ClassesShown, k[1])
			}
		}
		tv = append(tv, t)
	}
	if tp == nil {
		tp = []TruthPart{}
	}
	if tv == nil {
		tv = []TruthVictim{}
	}
	return tp, tv
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

type acc struct {
	n          int
	sx, sy, sh float64
}

func getAcc(m map[[2]int]*acc, k [2]int) *acc {
	if a, ok := m[k]; ok {
		return a
	}
	a := &acc{}
	m[k] = a
	return a
}

func getAccV(m map[int]*acc, k int) *acc {
	if a, ok := m[k]; ok {
		return a
	}
	a := &acc{}
	m[k] = a
	return a
}

// RenderAll renders every site of a scenario.
func RenderAll(scenarioDir string, s *scenario.Scenario, progress func(string)) ([]*Manifest, error) {
	var out []*Manifest
	for _, st := range s.Sites {
		m, err := RenderSite(scenarioDir, s, st.ID)
		if err != nil {
			return out, err
		}
		if progress != nil {
			progress(fmt.Sprintf("site %s: %s, %d victims, %d pose-B patches, %d decoys, %.1fs", st.ID, m.Frame, len(m.Victims), len(m.Patches), len(m.Decoys), m.RenderSeconds))
		}
		out = append(out, m)
	}
	return out, nil
}
