package vcam

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/thermal"
)

// Request describes one capture by one drone.
type Request struct {
	UAV      string
	FrameID  string
	Pose     Pose // true camera pose (used for rendering)
	Reported Pose // pose as the drone's GPS/IMU report it (what the edge must use)
	TOffsetS float64
	RGB      Intrinsics
	Thermal  Intrinsics
	Seed     int64 // sensor noise seed
	NoRGB    bool
	NoTherm  bool
	// Optional sensor variation (training data): RGB noise sigma in grey
	// levels (0 = 1.8) and optical blur sigma in pixels (0 = none).
	NoiseSigma float64
	BlurSigma  float64
}

// Frame is a rendered capture with its ground truth.
type Frame struct {
	Meta     Meta
	RGB      *raster.RGB
	Thermal  *raster.U16 // centikelvin
	PartsRGB *raster.U8  // class per RGB pixel
	InstRGB  *raster.U8  // victim index per RGB pixel (0 = none)
	PartsTh  *raster.U8
	InstTh   *raster.U8
}

// PoseLL is a pose in geographic terms.
type PoseLL struct {
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	AltAGL   float64 `json:"alt_agl_m"`
	AltMSL   float64 `json:"alt_msl_m"`
	YawDeg   float64 `json:"yaw_deg"`
	PitchDeg float64 `json:"pitch_deg"`
	RollDeg  float64 `json:"roll_deg"`
}

// CamMeta records a camera's intrinsics.
type CamMeta struct {
	W       int     `json:"width"`
	H       int     `json:"height"`
	HFOVDeg float64 `json:"hfov_deg"`
	Fx      float64 `json:"fx"`
	Fy      float64 `json:"fy"`
	Cx      float64 `json:"cx"`
	Cy      float64 `json:"cy"`
}

// Visible is one body-part class of one victim seen in the RGB frame.
type Visible struct {
	Victim     int        `json:"victim"`
	VictimID   string     `json:"victim_id"`
	Class      int        `json:"class"`
	ClassName  string     `json:"class_name"`
	Pixels     int        `json:"pixels"`
	BBox       [4]int     `json:"bbox_xyxy"`
	CentroidPx [2]float64 `json:"centroid_px"`
	TruthLat   float64    `json:"truth_lat"`
	TruthLon   float64    `json:"truth_lon"`
	TruthE     float64    `json:"truth_e_m"`
	TruthN     float64    `json:"truth_n_m"`
	Moving     bool       `json:"pose_b"`
}

// Meta is frame.json. Everything under "truth" keys is ground truth for
// offline evaluation only; the onboard pipeline must use pose_reported.
type Meta struct {
	Schema       int          `json:"schema"`
	ScenarioID   string       `json:"scenario_id"`
	ScenarioHash string       `json:"scenario_hash"`
	FrameID      string       `json:"frame_id"`
	UAV          string       `json:"uav"`
	TOffsetS     float64      `json:"t_offset_s"`
	Time         string       `json:"time"`
	PoseReported PoseLL       `json:"pose_reported"`
	PoseTrue     PoseLL       `json:"truth_pose"`
	RGB          CamMeta      `json:"rgb"`
	Thermal      CamMeta      `json:"thermal"`
	AirC         float64      `json:"air_c"`
	SunElevDeg   float64      `json:"sun_elev_deg"`
	SunAzDeg     float64      `json:"sun_az_deg"`
	GSDCenterM   float64      `json:"gsd_center_m"`
	Footprint    [][2]float64 `json:"footprint_latlon"`
	Sites        []string     `json:"sites_in_view"`
	Visible      []Visible    `json:"truth_visible"`
	RenderMS     float64      `json:"render_ms"`
}

func camMeta(c *Camera) CamMeta {
	return CamMeta{W: c.In.W, H: c.In.H, HFOVDeg: c.In.HFOVDeg, Fx: round(c.F, 3), Fy: round(c.F, 3), Cx: c.Cx, Cy: c.Cy}
}

func round(v float64, d int) float64 { p := math.Pow(10, float64(d)); return math.Round(v*p) / p }

func (w *World) poseLL(p Pose) PoseLL {
	ll := w.Scn.Local().ToPoint(p.E, p.N)
	return PoseLL{Lat: round(ll.Lat, 7), Lon: round(ll.Lon, 7), AltAGL: round(p.AltAGL, 2), AltMSL: round(p.AltAGL+w.Scn.GroundAltMSL, 2),
		YawDeg: round(p.YawDeg, 2), PitchDeg: round(p.PitchDeg, 2), RollDeg: round(p.RollDeg, 2)}
}

// sunVec is the unit vector towards the sun.
func sunVec(elev, az float64) V3 {
	e, a := elev*math.Pi/180, az*math.Pi/180
	return V3{math.Sin(a) * math.Cos(e), math.Cos(a) * math.Cos(e), math.Sin(e)}
}

// frameCtx is everything shared by the pixels of one frame.
type frameCtx struct {
	w       *World
	views   []*view
	sun     V3
	sunOK   bool
	air     float64
	peak    float64
	skin    map[int]float64
	gsd0    float64
	moving  map[int]bool
	ambient float64
	direct  float64
	sinE    float64 // sine of the sun elevation (0 below the horizon)
	warm    float64 // 0..1: how far surfaces have warmed since sunrise (thermal inertia)
}

// relief softens direct-sun contrast between slopes (sky light, inter-reflection).
func (fc *frameCtx) relief(ns float64) float64 { return 0.65*ns + 0.35*fc.sinE }

func (fc *frameCtx) viewAt(e, n float64) *view {
	for _, v := range fc.views {
		if v.se.box.ContainsP(geom.P{X: e, Y: n}) {
			return v
		}
	}
	return nil
}

// surface finds where a ray meets the rubble (two height refinements).
func (fc *frameCtx) surface(c *Camera, d V3, k int) (e, n, t float64, v *view, ok bool) {
	e, n, t, ok = c.Hit(d, 0)
	if !ok {
		return
	}
	v = fc.viewAt(e, n)
	if v == nil {
		return
	}
	for it := 0; it < 2; it++ {
		h := v.sampleH(e, n, k)
		e2, n2, t2, ok2 := c.Hit(d, h)
		if !ok2 {
			break
		}
		e, n, t = e2, n2, t2
		if nv := fc.viewAt(e, n); nv != nil {
			v = nv
		}
	}
	return e, n, t, v, true
}

func (fc *frameCtx) normal(v *view, e, n float64, k int) V3 {
	dd := fc.gsd0 * float64(int(1)<<k)
	hx := (v.sampleH(e+dd, n, k) - v.sampleH(e-dd, n, k)) / (2 * dd)
	hy := (v.sampleH(e, n+dd, k) - v.sampleH(e, n-dd, k)) / (2 * dd)
	return V3{-hx, -hy, 1}.norm()
}

// level picks the detail level for a ground footprint of g metres per pixel.
func (fc *frameCtx) level(g float64) int {
	r := g / fc.gsd0
	if r < 1.4 {
		return 0
	}
	k := int(math.Floor(math.Log2(r) + 0.3))
	if k >= Levels {
		k = Levels - 1
	}
	return k
}

// Render produces a frame.
func (w *World) Render(req Request) (*Frame, error) {
	t0 := time.Now()
	if req.RGB.W == 0 {
		req.RGB = DefaultRGB
	}
	if req.Thermal.W == 0 {
		req.Thermal = DefaultThermal
	}
	if req.Reported == (Pose{}) {
		req.Reported = req.Pose
	}
	camRGB, err := New(req.RGB, req.Pose)
	if err != nil {
		return nil, err
	}
	camTh, err := New(req.Thermal, req.Pose)
	if err != nil {
		return nil, err
	}
	if _, err := New(req.RGB, req.Reported); err != nil {
		return nil, err
	}
	start, err := w.Scn.StartTime()
	if err != nil {
		return nil, err
	}
	at := start.Add(time.Duration(req.TOffsetS * float64(time.Second)))
	elev, az := thermal.SunPosition(w.Scn.Center.Lat, w.Scn.Center.Lon, at)
	fc := &frameCtx{w: w, sun: sunVec(elev, az), sunOK: elev > 0, air: w.Scn.Env.AirC(at), peak: w.Scn.Env.SunPeakGainC,
		skin: map[int]float64{}, gsd0: w.Scn.GSDm, moving: map[int]bool{}}
	fc.ambient = 0.55
	fc.direct = 0.75 * smooth(elev/8)
	fc.sinE = math.Max(0, math.Sin(elev*math.Pi/180))
	fc.warm = 1 - math.Exp(-hoursSinceSunrise(w.Scn.Center.Lat, w.Scn.Center.Lon, at)/1.5)
	hours := req.TOffsetS / 3600
	for i := range w.Scn.Victims {
		v := &w.Scn.Victims[i]
		fc.skin[v.Index] = v.Thermal.SkinC(hours, fc.air)
		if v.Movement.MovingAt(req.TOffsetS) {
			fc.moving[v.Index] = true
		}
	}

	// Sites in view (union of both footprints, grown for parallax).
	fb := footBox(camRGB.Footprint(), camTh.Footprint()).Grow(12)
	w.rw.RLock()
	var siteIDs []string
	for _, se := range w.Sites {
		if !se.box.Overlaps(fb) {
			continue
		}
		if err := w.load(se); err != nil {
			w.rw.RUnlock()
			return nil, err
		}
		v := &view{se: se, sunOK: fc.sunOK}
		for idx := range fc.moving {
			if p, ok := se.patches[idx]; ok {
				v.active = append(v.active, p)
			}
		}
		se.mu.Lock()
		qe, qa := math.Round(elev*2)/2, math.Round(az)
		if fc.sunOK && (se.shadow == nil || se.shadow.elev != qe || se.shadow.az != qa) {
			se.shadow = computeShadow(se.base, qe, qa, w.Scn.GSDm)
		}
		v.shadow = se.shadow
		se.mu.Unlock()
		fc.views = append(fc.views, v)
		siteIDs = append(siteIDs, se.id)
	}

	fr := &Frame{}
	if !req.NoRGB {
		noise := req.NoiseSigma
		if noise <= 0 {
			noise = 1.8
		}
		fr.RGB, fr.PartsRGB, fr.InstRGB = fc.renderRGB(camRGB, req.Seed, noise, req.BlurSigma)
	}
	if !req.NoTherm {
		fr.Thermal, fr.PartsTh, fr.InstTh = fc.renderThermal(camTh, req.Seed, req.UAV)
	}
	w.rw.RUnlock()
	w.evict()

	fp := camRGB.Footprint()
	local := w.Scn.Local()
	var fpl [][2]float64
	for _, p := range fp {
		ll := local.ToPoint(p[0], p[1])
		fpl = append(fpl, [2]float64{round(ll.Lat, 7), round(ll.Lon, 7)})
	}
	sort.Strings(siteIDs)
	fr.Meta = Meta{Schema: 1, ScenarioID: w.Scn.ID, ScenarioHash: w.Scn.Hash(), FrameID: req.FrameID, UAV: req.UAV,
		TOffsetS: req.TOffsetS, Time: at.Format(time.RFC3339Nano), PoseReported: w.poseLL(req.Reported), PoseTrue: w.poseLL(req.Pose),
		RGB: camMeta(camRGB), Thermal: camMeta(camTh), AirC: round(fc.air, 2), SunElevDeg: round(elev, 2), SunAzDeg: round(az, 2),
		GSDCenterM: round(camRGB.GSDCenter(), 4), Footprint: fpl, Sites: siteIDs}
	if siteIDs == nil {
		fr.Meta.Sites = []string{}
	}
	fr.Meta.Visible = w.visible(fr, fc)
	fr.Meta.RenderMS = round(float64(time.Since(t0).Microseconds())/1000, 1)
	return fr, nil
}

// hoursSinceSunrise looks back in 5-minute steps for the last sunrise (0 at night).
func hoursSinceSunrise(lat, lon float64, at time.Time) float64 {
	if e, _ := thermal.SunPosition(lat, lon, at); e <= 0 {
		return 0
	}
	for m := 5; m <= 18*60; m += 5 {
		if e, _ := thermal.SunPosition(lat, lon, at.Add(-time.Duration(m)*time.Minute)); e <= 0 {
			return float64(m) / 60
		}
	}
	return 18
}

func smooth(t float64) float64 {
	t = math.Max(0, math.Min(1, t))
	return t * t * (3 - 2*t)
}

func footBox(a, b [4][2]float64) geom.Box {
	bb := geom.Box{Min: geom.P{X: math.Inf(1), Y: math.Inf(1)}, Max: geom.P{X: math.Inf(-1), Y: math.Inf(-1)}}
	for _, s := range [][4][2]float64{a, b} {
		for _, p := range s {
			bb.Min.X, bb.Min.Y = math.Min(bb.Min.X, p[0]), math.Min(bb.Min.Y, p[1])
			bb.Max.X, bb.Max.Y = math.Max(bb.Max.X, p[0]), math.Max(bb.Max.Y, p[1])
		}
	}
	return bb
}

func (fc *frameCtx) renderRGB(c *Camera, seed int64, noise, blur float64) (*raster.RGB, *raster.U8, *raster.U8) {
	W, H := c.In.W, c.In.H
	buf := make([]float32, W*H*3)
	parts, inst := raster.NewU8(W, H), raster.NewU8(W, H)
	sky := [3]float64{170, 186, 204}
	raster.Rows(H, func(y int) {
		for x := 0; x < W; x++ {
			d := c.Ray(float64(x)+0.5, float64(y)+0.5)
			i := y*W + x
			e0, n0, t, ok := c.Hit(d, 0)
			if !ok {
				buf[3*i], buf[3*i+1], buf[3*i+2] = float32(sky[0]), float32(sky[1]), float32(sky[2])
				continue
			}
			dl := math.Sqrt(d.dot(d))
			g := t * dl / c.F / math.Max(0.25, -d.Z/dl)
			k := fc.level(g)
			var col [3]float64
			nrm := V3{0, 0, 1}
			lit := 1.0
			if !fc.sunOK {
				lit = 0
			}
			if e, n, _, v, ok := fc.surface(c, d, k); ok && v != nil {
				col = v.sampleRGB(e, n, k)
				nrm = fc.normal(v, e, n, k)
				lit = v.light(e, n)
				if l0, idx, ok := v.nearest0(e, n); ok {
					if p := l0.Parts.Pix[idx]; p > 0 {
						parts.Pix[i] = p
						inst.Pix[i] = l0.Victim.Pix[idx]
					}
				}
			} else {
				detail := k
				if detail > 2 {
					detail = 2
				}
				col = render.GroundRGB(fc.w.gseed, e0, n0, detail)
			}
			ns := math.Max(0, nrm.dot(fc.sun))
			// Sky light is reduced on steep faces (they see less sky).
			I := fc.ambient*(0.75+0.25*nrm.Z) + fc.direct*fc.relief(ns)*lit
			buf[3*i] = float32(col[0] * I)
			buf[3*i+1] = float32(col[1] * I)
			buf[3*i+2] = float32(col[2] * I)
		}
	})
	// Auto exposure on a sparse grid, then white balance, vignetting and noise.
	var sum float64
	var cnt int
	for y := 0; y < H; y += 7 {
		for x := 0; x < W; x += 7 {
			i := 3 * (y*W + x)
			sum += 0.299*float64(buf[i]) + 0.587*float64(buf[i+1]) + 0.114*float64(buf[i+2])
			cnt++
		}
	}
	gain := 1.0
	if cnt > 0 && sum > 0 {
		gain = math.Max(0.7, math.Min(1.8, 118/(sum/float64(cnt))))
	}
	warm := 1 - smooth(math.Asin(math.Max(0, fc.sun.Z))*180/math.Pi/25) // 1 at sunrise, 0 above 25 deg
	wb := [3]float64{1 + 0.05*warm, 1, 1 - 0.07*warm}
	out := raster.NewRGB(W, H)
	cx, cy := float64(W)/2, float64(H)/2
	rmax := cx*cx + cy*cy
	raster.Rows(H, func(y int) {
		rng := rand.New(rand.NewSource(seed*1000003 + int64(y)))
		for x := 0; x < W; x++ {
			i := y*W + x
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			vig := 1 - 0.18*(dx*dx+dy*dy)/rmax
			for ch := 0; ch < 3; ch++ {
				v := float64(buf[3*i+ch])*gain*wb[ch]*vig + rng.NormFloat64()*noise
				out.Pix[3*i+ch] = raster.Clamp8(v)
			}
		}
	})
	if blur > 0 {
		blurRGB(out, blur)
	}
	return out, parts, inst
}

func (fc *frameCtx) renderThermal(c *Camera, seed int64, uav string) (*raster.U16, *raster.U8, *raster.U8) {
	W, H := c.In.W, c.In.H
	temp := raster.NewF32(W, H)
	parts, inst := raster.NewU8(W, H), raster.NewU8(W, H)
	ground := fc.air + render.GroundAbsorb*fc.peak*fc.sinE*fc.warm
	const ss = 2 // supersampling per axis
	raster.Rows(H, func(y int) {
		for x := 0; x < W; x++ {
			var acc float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u, v := float64(x)+(float64(sx)+0.5)/ss, float64(y)+(float64(sy)+0.5)/ss
					acc += fc.thermalSample(c, c.Ray(u, v), ground)
				}
			}
			temp.Pix[y*W+x] = float32(acc / (ss * ss))
			// Labels at the pixel centre.
			d := c.Ray(float64(x)+0.5, float64(y)+0.5)
			if e, n, _, vw, ok := fc.surface(c, d, 1); ok && vw != nil {
				if l0, idx, ok := vw.nearest0(e, n); ok && l0.Parts.Pix[idx] > 0 {
					parts.Pix[y*W+x] = l0.Parts.Pix[idx]
					inst.Pix[y*W+x] = l0.Victim.Pix[idx]
				}
			}
		}
	})
	// Optics blur, temporal noise (NETD ~40 mK) and fixed column pattern.
	blur := temp.GaussBlur(0.6)
	col := make([]float64, W)
	crng := rand.New(rand.NewSource(int64(raster.Hash(7, int64(len(uav)), int64(hashStr(uav))))))
	for x := range col {
		col[x] = crng.NormFloat64() * 0.04
	}
	out := raster.NewU16(W, H)
	raster.Rows(H, func(y int) {
		rng := rand.New(rand.NewSource(seed*7919 + int64(y)))
		for x := 0; x < W; x++ {
			t := float64(blur.Pix[y*W+x]) + col[x] + rng.NormFloat64()*0.04
			out.Pix[y*W+x] = thermal.CToCentiK(t)
		}
	})
	return out, parts, inst
}

func hashStr(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func (fc *frameCtx) thermalSample(c *Camera, d V3, ground float64) float64 {
	e, n, _, v, ok := fc.surface(c, d, 1)
	if !ok {
		return fc.air - 20 // cold sky
	}
	if v == nil {
		return ground
	}
	l0, idx, ok := v.nearest0(e, n)
	if !ok {
		return ground
	}
	nrm := fc.normal(v, e, n, 1)
	ns := math.Max(0, nrm.dot(fc.sun))
	a := float64(l0.Absorb.Pix[idx]) / 255
	tb := float64(l0.TBase.Pix[idx]) / 4
	deb := fc.air + a*fc.peak*fc.relief(ns)*fc.warm*v.light(e, n) + tb
	cp := float64(l0.Coupling.Pix[idx]) / 255
	if cp > 0 {
		if vi := int(l0.Victim.Pix[idx]); vi > 0 {
			return (1-cp)*deb + cp*fc.skin[vi]
		}
	}
	return deb
}

// visible lists the body parts in the RGB label frame.
func (w *World) visible(fr *Frame, fc *frameCtx) []Visible {
	out := []Visible{}
	if fr.PartsRGB == nil {
		return out
	}
	type acc struct {
		n              int
		sx, sy         float64
		x0, y0, x1, y1 int
	}
	m := map[[2]int]*acc{}
	W := fr.PartsRGB.W
	for i, p := range fr.PartsRGB.Pix {
		if p == 0 {
			continue
		}
		k := [2]int{int(fr.InstRGB.Pix[i]), int(p)}
		x, y := i%W, i/W
		a := m[k]
		if a == nil {
			a = &acc{x0: x, y0: y, x1: x, y1: y}
			m[k] = a
		}
		a.n++
		a.sx += float64(x) + 0.5
		a.sy += float64(y) + 0.5
		a.x0, a.y0, a.x1, a.y1 = min(a.x0, x), min(a.y0, y), max(a.x1, x), max(a.y1, y)
	}
	truth := map[[2]int]render.TruthPart{}
	for _, se := range w.Sites {
		for _, t := range se.man.Truth {
			truth[[2]int{t.Victim, t.Class}] = t
		}
	}
	for k, a := range m {
		vis := Visible{Victim: k[0], Class: k[1], ClassName: scenario.PartNames[k[1]], Pixels: a.n,
			BBox: [4]int{a.x0, a.y0, a.x1 + 1, a.y1 + 1}, CentroidPx: [2]float64{round(a.sx/float64(a.n), 2), round(a.sy/float64(a.n), 2)},
			Moving: fc.moving[k[0]]}
		if v, ok := w.victims[k[0]]; ok {
			vis.VictimID = v.ID
		}
		if t, ok := truth[k]; ok {
			vis.TruthLat, vis.TruthLon, vis.TruthE, vis.TruthN = t.Lat, t.Lon, t.E, t.N
		}
		out = append(out, vis)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Victim != out[j].Victim {
			return out[i].Victim < out[j].Victim
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// LocalToLL converts local metres to lat/lon for callers.
func (w *World) LocalToLL(e, n float64) geo.Point { return w.Scn.Local().ToPoint(e, n) }

// blurRGB applies a Gaussian optical blur in place.
func blurRGB(m *raster.RGB, sigma float64) {
	ch := raster.NewF32(m.W, m.H)
	for c := 0; c < 3; c++ {
		for i := range ch.Pix {
			ch.Pix[i] = float32(m.Pix[3*i+c])
		}
		b := ch.GaussBlur(sigma)
		for i, v := range b.Pix {
			m.Pix[3*i+c] = raster.Clamp8(float64(v))
		}
	}
}
