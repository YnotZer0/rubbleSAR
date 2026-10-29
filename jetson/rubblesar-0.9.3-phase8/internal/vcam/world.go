package vcam

import (
	"math"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"rubblesar/internal/geom"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
)

// Levels is the number of colour/height detail levels (1.5, 3, 6, 12 cm by default).
const Levels = 4

// lvl is one detail level of colour and height.
type lvl struct {
	W, H int
	rgb  []uint8
	h    []float32 // metres
}

func baseLevel(l *render.Layers) *lvl {
	o := &lvl{W: l.Rect.W(), H: l.Rect.H(), rgb: l.RGB.Pix, h: make([]float32, len(l.Height.Pix))}
	for i, v := range l.Height.Pix {
		o.h[i] = float32(v) / 1000
	}
	return o
}

func (a *lvl) half() *lvl {
	w, h := a.W/2, a.H/2
	if w < 1 || h < 1 {
		w, h = max(w, 1), max(h, 1)
	}
	o := &lvl{W: w, H: h, rgb: make([]uint8, w*h*3), h: make([]float32, w*h)}
	raster.Rows(h, func(y int) {
		for x := 0; x < w; x++ {
			x0, y0 := min(2*x, a.W-1), min(2*y, a.H-1)
			x1, y1 := min(x0+1, a.W-1), min(y0+1, a.H-1)
			idx := [4]int{y0*a.W + x0, y0*a.W + x1, y1*a.W + x0, y1*a.W + x1}
			for c := 0; c < 3; c++ {
				s := 0
				for _, i := range idx {
					s += int(a.rgb[3*i+c])
				}
				o.rgb[3*(y*w+x)+c] = uint8((s + 2) / 4)
			}
			o.h[y*w+x] = (a.h[idx[0]] + a.h[idx[1]] + a.h[idx[2]] + a.h[idx[3]]) / 4
		}
	})
	return o
}

// layerSet is a base or patch layer with its detail levels. Level k pixel
// (x, y) covers level-0 pixels [x*2^k, (x+1)*2^k) offset by the rect origin.
type layerSet struct {
	rect raster.Rect // level-0 site pixels
	l0   *render.Layers
	lv   [Levels]*lvl
}

func newLayerSet(l *render.Layers) *layerSet {
	s := &layerSet{rect: l.Rect, l0: l}
	s.lv[0] = baseLevel(l)
	for k := 1; k < Levels; k++ {
		s.lv[k] = s.lv[k-1].half()
	}
	return s
}

// siteEntry is one site: always its manifest, its layers once needed.
type siteEntry struct {
	id      string
	man     *render.Manifest
	fr      render.Frame
	box     geom.Box // local metres covered by the layers
	mu      sync.Mutex
	base    *layerSet
	patches map[int]*layerSet
	used    time.Time
	shadow  *shadowMap
}

// World is a rendered scenario ready to be photographed.
type World struct {
	Dir      string
	Scn      *scenario.Scenario
	Sites    []*siteEntry
	Verify   bool // check layer SHA-256 sums when loading
	MaxSites int  // sites kept in memory at once
	gseed    uint64
	rw       sync.RWMutex // read: rendering; write: evicting layers
	victims  map[int]*scenario.Victim
}

// Open reads scenario.json and every site.json under dir. Layers are loaded
// lazily, when a frame first sees a site.
func Open(dir string, verify bool) (*World, error) {
	s, err := scenario.Load(filepath.Join(dir, "scenario.json"))
	if err != nil {
		return nil, err
	}
	w := &World{Dir: dir, Scn: s, Verify: verify, MaxSites: 3, gseed: uint64(s.Seed)*0x9E3779B97F4A7C15 + 17, victims: map[int]*scenario.Victim{}}
	for i := range s.Victims {
		w.victims[s.Victims[i].Index] = &s.Victims[i]
	}
	for _, st := range s.Sites {
		m, err := render.LoadManifest(render.SiteDir(dir, st.ID), s)
		if err != nil {
			return nil, err
		}
		fr := m.Frame
		w.Sites = append(w.Sites, &siteEntry{id: st.ID, man: m, fr: fr,
			box: geom.Box{Min: geom.P{X: fr.E0, Y: fr.N0 - float64(fr.H)*fr.GSD}, Max: geom.P{X: fr.E0 + float64(fr.W)*fr.GSD, Y: fr.N0}}})
	}
	return w, nil
}

// Manifest returns a site's manifest.
func (w *World) Manifest(siteID string) *render.Manifest {
	for _, s := range w.Sites {
		if s.id == siteID {
			return s.man
		}
	}
	return nil
}

// load makes sure a site's layers are in memory, evicting the least
// recently used site beyond MaxSites.
func (w *World) load(se *siteEntry) error {
	se.mu.Lock()
	defer se.mu.Unlock()
	se.used = time.Now()
	if se.base != nil {
		return nil
	}
	site, err := render.LoadSite(w.Dir, w.Scn, se.id, w.Verify)
	if err != nil {
		return err
	}
	se.base = newLayerSet(site.Base)
	se.patches = map[int]*layerSet{}
	for v, p := range site.Patches {
		se.patches[v] = newLayerSet(p)
	}
	return nil
}

func (w *World) evict() {
	w.rw.Lock()
	defer w.rw.Unlock()
	var loaded []*siteEntry
	for _, s := range w.Sites {
		if s.base != nil {
			loaded = append(loaded, s)
		}
	}
	if len(loaded) <= w.MaxSites {
		return
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].used.Before(loaded[j].used) })
	for _, s := range loaded[:len(loaded)-w.MaxSites] {
		s.mu.Lock()
		s.base, s.patches, s.shadow = nil, nil, nil
		s.mu.Unlock()
	}
}

// view is one site prepared for a frame: active patches and sun shadows.
type view struct {
	se     *siteEntry
	active []*layerSet
	shadow *shadowMap
	sunOK  bool
}

// pick returns the layer set (base or an active patch) covering level-0 site pixel (x, y).
func (v *view) pick(x, y float64) *layerSet {
	for _, p := range v.active {
		if x >= float64(p.rect.X0) && x < float64(p.rect.X1) && y >= float64(p.rect.Y0) && y < float64(p.rect.Y1) {
			return p
		}
	}
	return v.se.base
}

// sampleH returns the height (m) at local metres using detail level k (bilinear).
func (v *view) sampleH(e, n float64, k int) float64 {
	x0, y0 := v.se.fr.ENToPix(e, n)
	ls := v.pick(x0, y0)
	l := ls.lv[k]
	s := float64(int(1) << k)
	x := (x0-float64(ls.rect.X0)+0.5)/s - 0.5
	y := (y0-float64(ls.rect.Y0)+0.5)/s - 0.5
	return bilinF(l.h, l.W, l.H, x, y)
}

// sampleRGB returns the colour at local metres using level k (bilinear).
func (v *view) sampleRGB(e, n float64, k int) [3]float64 {
	x0, y0 := v.se.fr.ENToPix(e, n)
	ls := v.pick(x0, y0)
	l := ls.lv[k]
	s := float64(int(1) << k)
	x := (x0-float64(ls.rect.X0)+0.5)/s - 0.5
	y := (y0-float64(ls.rect.Y0)+0.5)/s - 0.5
	return bilinRGB(l.rgb, l.W, l.H, x, y)
}

// nearest0 returns the level-0 layers and index at local metres.
func (v *view) nearest0(e, n float64) (*render.Layers, int, bool) {
	x0, y0 := v.se.fr.ENToPix(e, n)
	ls := v.pick(x0, y0)
	x := int(math.Floor(x0+0.5)) - ls.rect.X0
	y := int(math.Floor(y0+0.5)) - ls.rect.Y0
	if x < 0 || y < 0 || x >= ls.rect.W() || y >= ls.rect.H() {
		return nil, 0, false
	}
	return ls.l0, y*ls.rect.W() + x, true
}

func bilinF(p []float32, w, h int, x, y float64) float64 {
	x = math.Max(0, math.Min(float64(w-1), x))
	y = math.Max(0, math.Min(float64(h-1), y))
	x0, y0 := int(x), int(y)
	x1, y1 := min(x0+1, w-1), min(y0+1, h-1)
	fx, fy := x-float64(x0), y-float64(y0)
	a := float64(p[y0*w+x0])*(1-fx) + float64(p[y0*w+x1])*fx
	b := float64(p[y1*w+x0])*(1-fx) + float64(p[y1*w+x1])*fx
	return a*(1-fy) + b*fy
}

func bilinRGB(p []uint8, w, h int, x, y float64) [3]float64 {
	x = math.Max(0, math.Min(float64(w-1), x))
	y = math.Max(0, math.Min(float64(h-1), y))
	x0, y0 := int(x), int(y)
	x1, y1 := min(x0+1, w-1), min(y0+1, h-1)
	fx, fy := x-float64(x0), y-float64(y0)
	var out [3]float64
	i00, i10, i01, i11 := 3*(y0*w+x0), 3*(y0*w+x1), 3*(y1*w+x0), 3*(y1*w+x1)
	for c := 0; c < 3; c++ {
		a := float64(p[i00+c])*(1-fx) + float64(p[i10+c])*fx
		b := float64(p[i01+c])*(1-fx) + float64(p[i11+c])*fx
		out[c] = a*(1-fy) + b*fy
	}
	return out
}

// shadowMap holds the fraction of direct sunlight (0..1) per level-k pixel of
// a site, for one sun position.
type shadowMap struct {
	elev, az float64
	k        int
	w, h     int
	light    []float32
}

// computeShadow marches from every level-2 pixel towards the sun through the
// height field: up to 1.5 m in level-2 steps (small debris), then in level-3
// steps (rubble piles, long morning shadows). Patches are ignored (moving
// limbs cast negligible shadows).
func computeShadow(ls *layerSet, elev, az float64, gsd0 float64) *shadowMap {
	const k = 2
	l, l3 := ls.lv[k], ls.lv[k+1]
	sm := &shadowMap{elev: elev, az: az, k: k, w: l.W, h: l.H, light: make([]float32, l.W*l.H)}
	if elev <= 0.5 {
		return sm // sun below the horizon: everything in shade
	}
	maxH := float32(0)
	for _, v := range l.h {
		if v > maxH {
			maxH = v
		}
	}
	tanE := math.Tan(elev * math.Pi / 180)
	pix2, pix3 := gsd0*4, gsd0*8
	// Unit step towards the sun in pixels: east = +x, north = -y.
	sx, sy := math.Sin(az*math.Pi/180), -math.Cos(az*math.Pi/180)
	const nearM = 1.5
	raster.Rows(l.H, func(y int) {
		for x := 0; x < l.W; x++ {
			h0 := float64(l.h[y*l.W+x]) + 0.01
			reach := math.Min((float64(maxH)-h0)/tanE, 40) // metres until the ray clears everything
			lit := 1.0
			test := func(hh, dist float64) bool {
				if over := hh - (h0 + dist*tanE); over > 0 {
					lit = math.Min(lit, math.Max(0, 1-over/0.03))
				}
				return lit == 0
			}
			done := false
			for s := 1; !done && float64(s)*pix2 <= math.Min(reach, nearM); s++ {
				px, py := float64(x)+sx*float64(s), float64(y)+sy*float64(s)
				if px < 0 || py < 0 || px > float64(l.W-1) || py > float64(l.H-1) {
					done = true
					break
				}
				done = test(bilinF(l.h, l.W, l.H, px, py), float64(s)*pix2)
			}
			x3, y3 := (float64(x)+0.5)/2-0.5, (float64(y)+0.5)/2-0.5
			for s := int(nearM/pix3) + 1; !done && float64(s)*pix3 <= reach; s++ {
				px, py := x3+sx*float64(s), y3+sy*float64(s)
				if px < 0 || py < 0 || px > float64(l3.W-1) || py > float64(l3.H-1) {
					break
				}
				done = test(bilinF(l3.h, l3.W, l3.H, px, py), float64(s)*pix3)
			}
			sm.light[y*l.W+x] = float32(lit)
		}
	})
	return sm
}

// light returns the direct-sun fraction at local metres (bilinear).
func (v *view) light(e, n float64) float64 {
	if !v.sunOK {
		return 0
	}
	if v.shadow == nil {
		return 1
	}
	x0, y0 := v.se.fr.ENToPix(e, n)
	s := float64(int(1) << v.shadow.k)
	return bilinF(v.shadow.light, v.shadow.w, v.shadow.h, (x0+0.5)/s-0.5, (y0+0.5)/s-0.5)
}
