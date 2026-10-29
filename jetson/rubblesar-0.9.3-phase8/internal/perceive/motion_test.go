package perceive

import (
	"math"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/geo"
	"rubblesar/internal/infer"
	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
	"rubblesar/internal/vcam"
)

func rect(x0, y0, w, h int) *masks.Bitmap {
	b := &masks.Bitmap{X0: x0, Y0: y0, W: w, H: h, Pix: make([]bool, w*h)}
	for i := range b.Pix {
		b.Pix[i] = true
	}
	return b
}

func TestMaskChange(t *testing.T) {
	a := rect(100, 100, 40, 10)
	if c := MaskChange(a, rect(100, 100, 40, 10), 3); c != 0 {
		t.Fatalf("identical: %v", c)
	}
	if c := MaskChange(a, rect(102, 101, 40, 10), 3); c > 0.01 {
		t.Fatalf("2 px jitter within a 3 px tolerance: %v", c)
	}
	// A 40 px limb rotated about one end by 20 degrees: its far end moves ~14 px.
	rot := &masks.Bitmap{X0: 90, Y0: 80, W: 60, H: 40, Pix: make([]bool, 60*40)}
	th := 20 * math.Pi / 180
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			px, py := float64(x+90-100), float64(y+80-105)
			u := px*math.Cos(th) + py*math.Sin(th)
			v := -px*math.Sin(th) + py*math.Cos(th)
			rot.Pix[y*60+x] = u >= 0 && u < 40 && math.Abs(v) < 5
		}
	}
	if c := MaskChange(a, rot, 3); c < 0.2 {
		t.Fatalf("swung limb: change %v, want > 0.2", c)
	}
	if c := MaskChange(a, rect(300, 300, 40, 10), 3); c != 1 {
		t.Fatalf("far apart: %v", c)
	}
}

var (
	motionOnce  sync.Once
	motionWorld *vcam.World
	motionErr   error
)

// motionTestWorld: one site with two victims; v01 swings its forearm every
// 6 s for 2 s, v02 is still.
func motionTestWorld(t *testing.T) *vcam.World {
	t.Helper()
	motionOnce.Do(func() {
		sc := scenario.DefaultConfig()
		sc.Scenario.ID = "motion-test"
		sc.Scenario.Seed = 11
		sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 10, 12, 30
		sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 2, 2
		sc.Render.GSDm = 0.02
		s, err := scenario.Generate(sc, nil)
		if err != nil {
			motionErr = err
			return
		}
		for i := range s.Victims {
			v := &s.Victims[i]
			v.Status, v.Thermal.Status, v.OcclusionFrac = "alive_unresponsive", "alive_unresponsive", 0
			if v.Thermal.Alpha0 == 0 {
				v.Thermal.Alpha0 = 0.65
			}
			v.Exposed = v.Exposed[:1]
			v.Exposed[0].Kind, v.Exposed[0].Side = "forearm_hand", "right"
			v.Movement = scenario.Movement{}
		}
		s.Victims[0].Status, s.Victims[0].Thermal.Status = "alive_responsive", "alive_responsive"
		s.Victims[0].Movement = scenario.Movement{Moves: true, PeriodS: 6, DurationS: 2, PhaseS: 0, SwingDeg: 25}
		dir, err := os.MkdirTemp("", "motion-test-")
		if err != nil {
			motionErr = err
			return
		}
		s.Save(dir)
		if _, err := render.RenderAll(dir, s, nil); err != nil {
			motionErr = err
			return
		}
		motionWorld, motionErr = vcam.Open(dir, false)
	})
	if motionErr != nil {
		t.Fatal(motionErr)
	}
	return motionWorld
}

// Registration recovers the image shift of a drifted camera to a pixel.
func TestRegisterShift(t *testing.T) {
	w := motionTestWorld(t)
	v := w.Scn.Victims[0]
	p0 := vcam.Pose{E: v.E, N: v.N, AltAGL: 15, YawDeg: 30, PitchDeg: -90}
	p1 := p0
	p1.E, p1.N = p0.E+0.25, p0.N-0.15
	f0, err := w.Render(vcam.Request{UAV: "t", Pose: p0, Reported: p0, TOffsetS: 1000, Seed: 1, NoTherm: true})
	if err != nil {
		t.Fatal(err)
	}
	f1, err := w.Render(vcam.Request{UAV: "t", Pose: p1, Reported: p1, TOffsetS: 1000, Seed: 2, NoTherm: true})
	if err != nil {
		t.Fatal(err)
	}
	// The image moves as the surface under the camera does: the rubble the
	// victim lies on (parallax), so compare at the victim's height.
	h := 0.0
	for _, tp := range w.Manifest(v.SiteID).Truth {
		if tp.VictimID == v.ID {
			h = tp.HeightM
		}
	}
	c0, _ := vcam.New(vcam.DefaultRGB, p0)
	c1, _ := vcam.New(vcam.DefaultRGB, p1)
	u0, v0, _ := c0.Project(v.E, v.N, h)
	u1, v1, _ := c1.Project(v.E, v.N, h)
	t0 := time.Now()
	dx, dy := newRegRef(f0.RGB).Register(f1.RGB)
	t.Logf("shift %d,%d px (expected %.1f,%.1f for the surface %.1f m up) in %v", dx, dy, u1-u0, v1-v0, h, time.Since(t0))
	if math.Abs(float64(dx)-(u1-u0)) > 2 || math.Abs(float64(dy)-(v1-v0)) > 2 {
		t.Fatalf("registered %d,%d; expected %.1f,%.1f", dx, dy, u1-u0, v1-v0)
	}
}

// A 30 s hover with the truth oracle (exact outlines): the swinging forearm
// is reported as moving, the still one is not, despite the drone's drift.
func TestHoverMovement(t *testing.T) {
	w := motionTestWorld(t)
	moving, still := w.Scn.Victims[0], w.Scn.Victims[1]
	ce, cn := (moving.E+still.E)/2, (moving.N+still.N)/2
	if math.Hypot(moving.E-still.E, moving.N-still.N) > 9 {
		t.Skipf("victims %.1f m apart: not both in one 15 m view", math.Hypot(moving.E-still.E, moving.N-still.N))
	}
	or := &oracle{}
	p := Params{Thermal: ThermalParams{SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, MinPixels: 9, WarmSpotMinPx: 3},
		Track: DefaultTrackParams(), Errors: DefaultPoseErrors(1.5, 1)}
	pl := NewPipeline("inv-1", or, vcam.DefaultRGB, vcam.DefaultThermal, p, pos(w))
	t0 := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	var maxShift float64
	for k := 0; k < 30; k++ {
		// Drift of up to ~0.3 m, as the simulator's hover produces.
		we, wn := 0.2*math.Sin(float64(k)/4), 0.15*math.Cos(float64(k)/5)
		truePose := vcam.Pose{E: ce + we, N: cn + wn, AltAGL: 15, YawDeg: 45, PitchDeg: -90}
		rep := truePose
		rep.E, rep.N = rep.E+0.8, rep.N-0.5 // GPS error (slow: constant over the hover)
		fr, err := w.Render(vcam.Request{UAV: "inv-1", Pose: truePose, Reported: rep, TOffsetS: 1000 + float64(k), Seed: int64(k)})
		if err != nil {
			t.Fatal(err)
		}
		or.fr = fr
		_, st, err := pl.Process(Capture{FrameID: "f", Time: t0.Add(time.Duration(k) * time.Second), SimTimeS: 1000 + float64(k), Reported: rep,
			RGB: fr.RGB, Thermal: fr.Thermal, PoseLL: PoseReport{AltAGL: 15, PitchDeg: -90}, Hover: true, TaskID: "T-1"})
		if err != nil {
			t.Fatal(err)
		}
		maxShift = math.Max(maxShift, math.Hypot(float64(st.ShiftX), float64(st.ShiftY)))
	}
	if maxShift < 5 {
		t.Fatalf("registration shift at most %.0f px: drift not seen", maxShift)
	}
	pk := pl.EndHover("T-1", ce+0.8, cn-0.5, 10, t0.Add(31*time.Second))
	if len(pk) == 0 {
		t.Fatal("no INVESTIGATED reports")
	}
	verdict := map[string]bool{}
	for _, d := range pk {
		e, n := w.Scn.Local().ToLocal(geoOf(d))
		m := d.GetMovement()
		if d.Reason != 3 || d.TaskId != "T-1" || m == nil || m.HoverFrames < 20 || m.WatchedS < 25 {
			t.Fatalf("report %s: reason %v task %q movement %+v", d.DetectionId, d.Reason, d.TaskId, m)
		}
		who := still.ID
		if math.Hypot(e-moving.E, n-moving.N) < math.Hypot(e-still.E, n-still.N) {
			who = moving.ID
		}
		t.Logf("%s (%s): %d/%d frames moved, max change %.2f, moved %v", d.DetectionId, who, m.MovedFrames, m.HoverFrames, m.MaxChange, m.Moved)
		verdict[who] = verdict[who] || m.Moved
		if who == still.ID && m.Moved {
			t.Errorf("still victim %s reported moving (%d frames, max change %.2f)", still.ID, m.MovedFrames, m.MaxChange)
		}
	}
	if !verdict[moving.ID] {
		t.Errorf("moving victim %s not reported moving", moving.ID)
	}
}

func geoOf(d interface {
	GetPosition() *sarpb.GeoPoint
}) geo.Point {
	return geo.Point{Lat: d.GetPosition().GetLatDeg(), Lon: d.GetPosition().GetLonDeg()}
}

// sloppy wraps the oracle with the faults of a real segmentation model: the
// outline grows or shrinks by a pixel or two and shifts by one, a forearm's
// mask sometimes takes in the hand (the hand then not reported), the class of
// a part flips between hand and arm now and then, and parts are missed.
type sloppy struct {
	oracle
	rng *rand.Rand
}

func (s *sloppy) DetectFrame(img *raster.RGB) ([]infer.Detection, infer.Timing, error) {
	ds, tm, err := s.oracle.DetectFrame(img)
	if err != nil {
		return nil, tm, err
	}
	hand, arm := classIndex("hand"), classIndex("arm")
	var out []infer.Detection
	used := map[int]bool{}
	for i := range ds {
		if used[i] {
			continue
		}
		d := ds[i]
		if d.Class == arm && s.rng.Float64() < 0.4 {
			// Forearm and hand as one outline.
			for j := range ds {
				if !used[j] && ds[j].Class == hand && touching(&d.Mask, &ds[j].Mask) {
					d.Mask = unionBitmap(&d.Mask, &ds[j].Mask)
					used[j] = true
					break
				}
			}
		}
		if s.rng.Float64() < 0.12 {
			continue // missed this frame
		}
		if s.rng.Float64() < 0.12 && (d.Class == hand || d.Class == arm) {
			d.Class = hand + arm - d.Class
		}
		m := growBitmap(&d.Mask, s.rng.Intn(5)-2)
		m.X0 += s.rng.Intn(3) - 1
		m.Y0 += s.rng.Intn(3) - 1
		if m.Area() == 0 {
			continue
		}
		d.Mask = *m
		d.Area = m.Area()
		var sx, sy float64
		for y := 0; y < m.H; y++ {
			for x := 0; x < m.W; x++ {
				if m.Pix[y*m.W+x] {
					sx, sy = sx+float64(m.X0+x), sy+float64(m.Y0+y)
				}
			}
		}
		d.CX, d.CY = sx/float64(d.Area), sy/float64(d.Area)
		d.Box = [4]float32{float32(m.X0), float32(m.Y0), float32(m.X0 + m.W), float32(m.Y0 + m.H)}
		d.Name = infer.ClassNames[d.Class]
		out = append(out, d)
	}
	return out, tm, nil
}

func classIndex(name string) int {
	for i, n := range infer.ClassNames {
		if n == name {
			return i
		}
	}
	panic(name)
}

func touching(a, b *masks.Bitmap) bool {
	for y := 0; y < a.H; y++ {
		for x := 0; x < a.W; x++ {
			if !a.Pix[y*a.W+x] {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					if b.At(a.X0+x+dx, a.Y0+y+dy) {
						return true
					}
				}
			}
		}
	}
	return false
}

func unionBitmap(a, b *masks.Bitmap) masks.Bitmap {
	x0, y0 := min(a.X0, b.X0), min(a.Y0, b.Y0)
	x1, y1 := max(a.X0+a.W, b.X0+b.W), max(a.Y0+a.H, b.Y0+b.H)
	u := masks.Bitmap{X0: x0, Y0: y0, W: x1 - x0, H: y1 - y0, Pix: make([]bool, (x1-x0)*(y1-y0))}
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			u.Pix[(y-y0)*u.W+x-x0] = a.At(x, y) || b.At(x, y)
		}
	}
	return u
}

// growBitmap dilates (r > 0) or erodes (r < 0) by |r| pixels (square).
func growBitmap(b *masks.Bitmap, r int) *masks.Bitmap {
	if r == 0 {
		c := *b
		return &c
	}
	pad := max(r, 0)
	o := &masks.Bitmap{X0: b.X0 - pad, Y0: b.Y0 - pad, W: b.W + 2*pad, H: b.H + 2*pad}
	o.Pix = make([]bool, o.W*o.H)
	k := r
	if k < 0 {
		k = -k
	}
	for y := 0; y < o.H; y++ {
		for x := 0; x < o.W; x++ {
			ix, iy := o.X0+x, o.Y0+y
			any, all := false, true
			for dy := -k; dy <= k; dy++ {
				for dx := -k; dx <= k; dx++ {
					if b.At(ix+dx, iy+dy) {
						any = true
					} else {
						all = false
					}
				}
			}
			o.Pix[y*o.W+x] = (r > 0 && any) || (r < 0 && all)
		}
	}
	return o
}

// The same hover with a sloppy model: outline jitter, a forearm mask that
// sometimes takes in the hand, class flips and misses are not movement; the
// swinging forearm still is.
func TestHoverMovementSloppyMasks(t *testing.T) {
	w := motionTestWorld(t)
	moving, still := w.Scn.Victims[0], w.Scn.Victims[1]
	ce, cn := (moving.E+still.E)/2, (moving.N+still.N)/2
	for seed := int64(1); seed <= 3; seed++ {
		det := &sloppy{rng: rand.New(rand.NewSource(seed))}
		p := Params{Thermal: ThermalParams{SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, MinPixels: 9, WarmSpotMinPx: 3},
			Track: DefaultTrackParams(), Errors: DefaultPoseErrors(1.5, 1)}
		pl := NewPipeline("inv-1", det, vcam.DefaultRGB, vcam.DefaultThermal, p, pos(w))
		t0 := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
		for k := 0; k < 30; k++ {
			we, wn := 0.2*math.Sin(float64(k)/4), 0.15*math.Cos(float64(k)/5)
			truePose := vcam.Pose{E: ce + we, N: cn + wn, AltAGL: 15, YawDeg: 45, PitchDeg: -90}
			rep := truePose
			rep.E, rep.N = rep.E+0.8, rep.N-0.5
			fr, err := w.Render(vcam.Request{UAV: "inv-1", Pose: truePose, Reported: rep, TOffsetS: 1000 + float64(k), Seed: int64(k) + 100*seed})
			if err != nil {
				t.Fatal(err)
			}
			det.fr = fr
			if _, _, err := pl.Process(Capture{FrameID: "f", Time: t0.Add(time.Duration(k) * time.Second), SimTimeS: 1000 + float64(k), Reported: rep,
				RGB: fr.RGB, Thermal: fr.Thermal, PoseLL: PoseReport{AltAGL: 15, PitchDeg: -90}, Hover: true, TaskID: "T-1"}); err != nil {
				t.Fatal(err)
			}
		}
		verdict := map[string]bool{}
		for _, d := range pl.EndHover("T-1", ce+0.8, cn-0.5, 10, t0.Add(31*time.Second)) {
			e, n := w.Scn.Local().ToLocal(geoOf(d))
			m := d.GetMovement()
			if m == nil {
				continue
			}
			who := still.ID
			if math.Hypot(e-moving.E, n-moving.N) < math.Hypot(e-still.E, n-still.N) {
				who = moving.ID
			}
			t.Logf("seed %d %s %s (%s): %d/%d frames moved, max change %.2f, moved %v", seed, d.DetectionId, d.Part, who, m.MovedFrames, m.HoverFrames, m.MaxChange, m.Moved)
			verdict[who] = verdict[who] || m.Moved
		}
		if verdict[still.ID] {
			t.Errorf("seed %d: still victim %s reported moving", seed, still.ID)
		}
		if !verdict[moving.ID] {
			t.Errorf("seed %d: moving victim %s not reported moving", seed, moving.ID)
		}
	}
}
