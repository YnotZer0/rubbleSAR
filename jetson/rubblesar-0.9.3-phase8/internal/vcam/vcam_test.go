package vcam

import (
	"bytes"
	"math"
	"os"
	"testing"

	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/thermal"
)

var testDir string

// TestMain renders a small two-site scenario once for all tests.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "vcam-test-")
	if err != nil {
		panic(err)
	}
	c := scenario.DefaultConfig()
	c.Scenario.ID = "vcam-test"
	c.Sites.Count, c.Sites.MinSizeM, c.Sites.MaxSizeM, c.Sites.MaxFromCentreM = 2, 10, 13, 80
	c.Victims.PerSiteMin, c.Victims.PerSiteMax = 3, 4
	c.Victims.AliveFraction = 0.7
	c.Render.GSDm = 0.02
	s, err := scenario.Generate(c, nil)
	if err != nil {
		panic(err)
	}
	if _, err := s.Save(dir); err != nil {
		panic(err)
	}
	if _, err := render.RenderAll(dir, s, nil); err != nil {
		panic(err)
	}
	testDir = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func world(t *testing.T) *World {
	t.Helper()
	w, err := Open(testDir, true)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestProjectionRoundTrip(t *testing.T) {
	poses := []Pose{
		{E: 10, N: -5, AltAGL: 40, YawDeg: 0, PitchDeg: -90},
		{E: 0, N: 0, AltAGL: 15, YawDeg: 137, PitchDeg: -90},
		{E: -30, N: 20, AltAGL: 60, YawDeg: 300, PitchDeg: -45, RollDeg: 3},
	}
	for _, p := range poses {
		for _, in := range []Intrinsics{DefaultRGB, DefaultThermal} {
			c, err := New(in, p)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range []float64{0, 2.5} {
				for _, uv := range [][2]float64{{0.5, 0.5}, {float64(in.W) / 2, float64(in.H) / 2}, {float64(in.W) - 3, 7}, {100.25, float64(in.H) - 1}} {
					e, n, ok := c.GroundAt(uv[0], uv[1], h)
					if !ok {
						t.Fatalf("pose %+v pixel %v does not hit the ground", p, uv)
					}
					u, v, ok := c.Project(e, n, h)
					if !ok || math.Abs(u-uv[0]) > 1e-6 || math.Abs(v-uv[1]) > 1e-6 {
						t.Fatalf("pose %+v: %v -> (%.3f,%.3f) -> (%.6f,%.6f)", p, uv, e, n, u, v)
					}
				}
			}
		}
	}
	// Nadir footprint width and GSD.
	c, _ := New(DefaultRGB, Pose{AltAGL: 40, PitchDeg: -90})
	fp := c.Footprint()
	want := 2 * 40 * math.Tan(35*math.Pi/180)
	if got := fp[1][0] - fp[0][0]; math.Abs(got-want) > 1e-6 {
		t.Fatalf("footprint width %.4f want %.4f", got, want)
	}
	if g := c.GSDCenter(); math.Abs(g-40/c.F) > 1e-9 {
		t.Fatalf("gsd %v", g)
	}
}

func TestOrientation(t *testing.T) {
	c, _ := New(DefaultRGB, Pose{AltAGL: 30, YawDeg: 0, PitchDeg: -90})
	if _, v, _ := c.Project(0, 5, 0); v >= c.Cy {
		t.Fatal("yaw 0: north should be up in the image")
	}
	if u, _, _ := c.Project(5, 0, 0); u <= c.Cx {
		t.Fatal("yaw 0: east should be right")
	}
	c, _ = New(DefaultRGB, Pose{AltAGL: 30, YawDeg: 90, PitchDeg: -90})
	if _, v, _ := c.Project(5, 0, 0); v >= c.Cy {
		t.Fatal("yaw 90: east should be up")
	}
	c, _ = New(DefaultRGB, Pose{AltAGL: 30, YawDeg: 0, PitchDeg: -45})
	if _, _, ok := c.GroundAt(c.Cx, c.Cy, 0); !ok {
		t.Fatal("oblique centre must hit the ground")
	}
	if e, n, _ := c.GroundAt(c.Cx, c.Cy, 0); math.Abs(e) > 1e-9 || math.Abs(n-30) > 1e-6 {
		t.Fatalf("45 deg pitch centre should hit 30 m north, got %.3f,%.3f", e, n)
	}
	if _, err := New(DefaultRGB, Pose{AltAGL: 1, PitchDeg: -90}); err == nil {
		t.Fatal("1 m altitude accepted")
	}
	if _, err := New(Intrinsics{W: 10, H: 10, HFOVDeg: 60}, Pose{AltAGL: 10, PitchDeg: -90}); err == nil {
		t.Fatal("tiny camera accepted")
	}
}

// Every victim photographed from 12 m appears with all its truth classes,
// and label centroids geolocate back to the truth.
func TestTruthVisibleAndAligned(t *testing.T) {
	w := world(t)
	for _, v := range w.Scn.Victims {
		m := w.Manifest(v.SiteID)
		f, err := w.Render(Request{UAV: "t", Pose: Pose{E: v.E, N: v.N, AltAGL: 12, PitchDeg: -90}, NoTherm: true})
		if err != nil {
			t.Fatal(err)
		}
		c, _ := New(DefaultRGB, Pose{E: v.E, N: v.N, AltAGL: 12, PitchDeg: -90})
		for _, tp := range m.Truth {
			if tp.Victim != v.Index {
				continue
			}
			var found *Visible
			for i := range f.Meta.Visible {
				if f.Meta.Visible[i].Victim == v.Index && f.Meta.Visible[i].Class == tp.Class {
					found = &f.Meta.Visible[i]
				}
			}
			if found == nil {
				t.Errorf("%s %s not visible in a 12 m nadir frame", v.ID, tp.ClassName)
				continue
			}
			e, n, _ := c.GroundAt(found.CentroidPx[0], found.CentroidPx[1], tp.HeightM)
			if d := math.Hypot(e-tp.E, n-tp.N); d > 0.12 {
				t.Errorf("%s %s: label centroid maps %.3f m from truth", v.ID, tp.ClassName, d)
			}
		}
	}
}

func TestThermalBands(t *testing.T) {
	w := world(t)
	for _, v := range w.Scn.Victims {
		f, err := w.Render(Request{UAV: "t", Pose: Pose{E: v.E, N: v.N, AltAGL: 12, PitchDeg: -90}, NoRGB: true})
		if err != nil {
			t.Fatal(err)
		}
		air := f.Meta.AirC
		maxT, sum, n := -100.0, 0.0, 0
		for i, idx := range f.InstTh.Pix {
			if int(idx) == v.Index && f.PartsTh.Pix[i] > 0 {
				c := thermal.CentiKToC(f.Thermal.Pix[i])
				maxT = math.Max(maxT, c)
				sum += c
				n++
			}
		}
		if n == 0 {
			t.Errorf("%s: no thermal label pixels", v.ID)
			continue
		}
		skin := v.Thermal.SkinC(0, air)
		switch {
		case v.Status != thermal.Deceased && maxT < air+6:
			t.Errorf("%s (%s) hottest pixel %.1f C, air %.1f, model skin %.1f: living victims must stand out", v.ID, v.Status, maxT, air, skin)
		case v.Status == thermal.Deceased && v.Thermal.DeathHours < -10 && sum/float64(n) > air+6:
			t.Errorf("%s deceased %.0f h: mean %.1f C, air %.1f: should be near ambient", v.ID, -v.Thermal.DeathHours, sum/float64(n), air)
		}
	}
	// Open ground far from sites sits within a few degrees of air.
	f, err := w.Render(Request{UAV: "t", Pose: Pose{E: 600, N: 600, AltAGL: 40, PitchDeg: -90}, NoRGB: true})
	if err != nil {
		t.Fatal(err)
	}
	c := thermal.CentiKToC(f.Thermal.Pix[len(f.Thermal.Pix)/2])
	if c < f.Meta.AirC-1 || c > f.Meta.AirC+8 {
		t.Errorf("open ground %.1f C with air %.1f", c, f.Meta.AirC)
	}
	if len(f.Meta.Sites) != 0 || len(f.Meta.Visible) != 0 {
		t.Errorf("empty area reports sites %v visible %d", f.Meta.Sites, len(f.Meta.Visible))
	}
}

func TestMovement(t *testing.T) {
	w := world(t)
	moved, still := 0, 0
	for _, v := range w.Scn.Victims {
		var on, off float64 = -1, -1
		for ts := 0.0; ts < 120 && (on < 0 || off < 0); ts += 0.5 {
			if v.Movement.MovingAt(ts) {
				if on < 0 {
					on = ts
				}
			} else if off < 0 {
				off = ts
			}
		}
		p := Pose{E: v.E, N: v.N, AltAGL: 12, PitchDeg: -90}
		a, err := w.Render(Request{UAV: "t", Pose: p, TOffsetS: off, NoTherm: true})
		if err != nil {
			t.Fatal(err)
		}
		if !v.Movement.Moves {
			b, _ := w.Render(Request{UAV: "t", Pose: p, TOffsetS: off + 7, NoTherm: true})
			if !bytes.Equal(mask(a, v.Index), mask(b, v.Index)) {
				t.Errorf("%s does not move but its labels changed", v.ID)
			}
			still++
			continue
		}
		b, _ := w.Render(Request{UAV: "t", Pose: p, TOffsetS: on, NoTherm: true})
		if bytes.Equal(mask(a, v.Index), mask(b, v.Index)) {
			t.Errorf("%s moves but its labels are identical at t=%.1f and t=%.1f", v.ID, off, on)
		}
		moved++
	}
	if moved == 0 || still == 0 {
		t.Logf("test scenario has %d moving and %d still victims", moved, still)
	}
}

// mask returns the RGB label mask of one victim.
func mask(f *Frame, idx int) []byte {
	m := make([]byte, len(f.InstRGB.Pix))
	for i, v := range f.InstRGB.Pix {
		if int(v) == idx {
			m[i] = 1
		}
	}
	return m
}

func TestDeterministic(t *testing.T) {
	w := world(t)
	v := w.Scn.Victims[0]
	req := Request{UAV: "t", Pose: Pose{E: v.E + 3, N: v.N - 2, AltAGL: 25, YawDeg: 33, PitchDeg: -70}, TOffsetS: 1800, Seed: 9}
	a, err := w.Render(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := w.Render(req)
	if !bytes.Equal(a.RGB.Pix, b.RGB.Pix) || !bytes.Equal(a.InstRGB.Pix, b.InstRGB.Pix) {
		t.Fatal("RGB render not deterministic")
	}
	for i := range a.Thermal.Pix {
		if a.Thermal.Pix[i] != b.Thermal.Pix[i] {
			t.Fatal("thermal render not deterministic")
		}
	}
	if a.Meta.ScenarioHash != w.Scn.Hash() || len(a.Meta.Footprint) != 4 {
		t.Fatal("metadata incomplete")
	}
	dir := t.TempDir()
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"rgb.png", "thermal.png", "thermal-view.png", "labels-parts.png", "labels-victim.png", "overlay.png", "frame.json"} {
		if _, err := os.Stat(dir + "/" + f); err != nil {
			t.Errorf("missing %s", f)
		}
	}
}

func TestLayerTamperDetected(t *testing.T) {
	w := world(t)
	st := w.Scn.Sites[0]
	p := render.SiteDir(testDir, st.ID) + "/parts.png"
	orig, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer os.WriteFile(p, orig, 0o644)
	os.WriteFile(p, append(append([]byte{}, orig...), 0), 0o644)
	if _, err := render.LoadSite(testDir, w.Scn, st.ID, true); err == nil {
		t.Fatal("modified layer accepted")
	}
}

// Several drones render at once while sites are evicted and reloaded.
func TestConcurrentRenders(t *testing.T) {
	w := world(t)
	w.MaxSites = 1
	errs := make(chan error, 6)
	for g := 0; g < 6; g++ {
		go func(g int) {
			v := w.Scn.Victims[g%len(w.Scn.Victims)]
			_, err := w.Render(Request{UAV: "c", Pose: Pose{E: v.E, N: v.N, AltAGL: 20, PitchDeg: -90}, TOffsetS: float64(g * 600),
				RGB: Intrinsics{W: 320, H: 180, HFOVDeg: 70}, Thermal: Intrinsics{W: 160, H: 128, HFOVDeg: 50}})
			errs <- err
		}(g)
	}
	for g := 0; g < 6; g++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
