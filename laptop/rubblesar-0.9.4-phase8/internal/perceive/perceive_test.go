package perceive

import (
	"bytes"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/evalseg"
	"rubblesar/internal/geo"
	"rubblesar/internal/infer"
	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
	"rubblesar/internal/thermal"
	"rubblesar/internal/vcam"
)

var (
	worldOnce sync.Once
	world     *vcam.World
	worldErr  error
)

// testWorld renders a small scenario once for all tests: one site, four victims.
func testWorld(t *testing.T) *vcam.World {
	t.Helper()
	worldOnce.Do(func() {
		sc := scenario.DefaultConfig()
		sc.Scenario.ID = "perceive-test"
		sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 12, 14, 40
		sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 4, 4
		sc.Render.GSDm = 0.03
		s, err := scenario.Generate(sc, nil)
		if err != nil {
			worldErr = err
			return
		}
		dir, err := os.MkdirTemp("", "perceive-test-")
		if err != nil {
			worldErr = err
			return
		}
		s.Save(dir)
		if _, err := render.RenderAll(dir, s, nil); err != nil {
			worldErr = err
			return
		}
		world, worldErr = vcam.Open(dir, false)
	})
	if worldErr != nil {
		t.Fatal(worldErr)
	}
	return world
}

// oracle answers with the frame's merged truth (one detection per visible part).
type oracle struct {
	fr   *vcam.Frame
	drop bool // answer nothing (to exercise warm spots)
}

func (o *oracle) Name() string { return "oracle" }
func (o *oracle) DetectFrame(*raster.RGB) ([]infer.Detection, infer.Timing, error) {
	if o.drop {
		return nil, infer.Timing{}, nil
	}
	var ds []infer.Detection
	for _, t := range evalseg.MergedTruth(o.fr.PartsRGB, o.fr.InstRGB, 6) {
		ds = append(ds, infer.Detection{Class: t.Class, Name: infer.ClassNames[t.Class], Conf: 0.8, Mask: t.Mask, Area: t.Area, CX: t.CX, CY: t.CY,
			Box: [4]float32{float32(t.Mask.X0), float32(t.Mask.Y0), float32(t.Mask.X0 + t.Mask.W), float32(t.Mask.Y0 + t.Mask.H)}})
	}
	return ds, infer.Timing{}, nil
}

func pos(w *vcam.World) Position {
	return func(e, n float64) (float64, float64, float64) {
		p := w.Scn.Local().ToPoint(e, n)
		return p.Lat, p.Lon, w.Scn.GroundAltMSL
	}
}

type run struct {
	packets []*sarpb.Detection
	pl      *Pipeline
	frames  int
}

// fly runs the pipeline over frames along a line across the site at a height.
func fly(t *testing.T, w *vcam.World, alt float64, offsetE float64, drop bool) run {
	t.Helper()
	site := w.Scn.Sites[0]
	or := &oracle{drop: drop}
	p := Params{Thermal: ThermalParams{SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, MinPixels: 9, WarmSpotMinPx: 3},
		WarmSpots: true, Track: DefaultTrackParams(), Errors: DefaultPoseErrors(1.5, 1)}
	pl := NewPipeline("t-1", or, vcam.DefaultRGB, vcam.DefaultThermal, p, pos(w))
	var r run
	r.pl = pl
	t0 := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	step := 3.0
	if alt > 30 {
		step = 6
	}
	for k := -5; k <= 5; k++ {
		truePose := vcam.Pose{E: site.CenterE, N: site.CenterN + float64(k)*step, AltAGL: alt, YawDeg: 0, PitchDeg: -90}
		rep := truePose
		rep.E += offsetE
		fr, err := w.Render(vcam.Request{UAV: "t-1", Pose: truePose, Reported: rep, TOffsetS: 600, Seed: int64(k + 100)})
		if err != nil {
			t.Fatal(err)
		}
		or.fr = fr
		c := Capture{FrameID: "f", Time: t0.Add(time.Duration(k+5) * time.Second), SimTimeS: 600, Reported: rep, RGB: fr.RGB, Thermal: fr.Thermal,
			PoseLL: PoseReport{AltAGL: alt, PitchDeg: -90}}
		pk, _, err := pl.Process(c)
		if err != nil {
			t.Fatal(err)
		}
		r.packets = append(r.packets, pk...)
		r.frames++
	}
	return r
}

func truthParts(w *vcam.World) []render.TruthPart {
	var out []render.TruthPart
	for _, st := range w.Scn.Sites {
		out = append(out, w.Manifest(st.ID).Truth...)
	}
	return out
}

func nearestTruth(w *vcam.World, e, n float64, class int) (render.TruthPart, float64) {
	best, bd := render.TruthPart{}, math.Inf(1)
	for _, tp := range truthParts(w) {
		if class >= 0 && tp.Class-1 != class {
			continue
		}
		if d := math.Hypot(tp.E-e, tp.N-n); d < bd {
			best, bd = tp, d
		}
	}
	return best, bd
}

func scenarioPoint(g *sarpb.GeoPoint) geo.Point { return geo.Point{Lat: g.LatDeg, Lon: g.LonDeg} }

func local(w *vcam.World, g *sarpb.GeoPoint) (float64, float64) {
	return w.Scn.Local().ToLocal(scenarioPoint(g))
}

// Close-range pass with the oracle: every reported part lies on its true part,
// each part is reported once as NEW, temperatures of living people are warm,
// and packets carry decodable crops and a short outline.
func TestPipelineCloseRange(t *testing.T) {
	w := testWorld(t)
	r := fly(t, w, 15, 0, false)
	news := map[string]bool{}
	alive := map[string]bool{}
	for _, v := range w.Scn.Victims {
		alive[v.ID] = v.Status != thermal.Deceased
	}
	var warmAlive, readableAlive, errs int
	var maxErr float64
	for _, d := range r.packets {
		if d.Reason == sarpb.ReportReason_REPORT_REASON_NEW {
			if news[d.TrackId] {
				t.Fatalf("track %s reported NEW twice", d.TrackId)
			}
			news[d.TrackId] = true
		}
		c := PartIndex(d.Part)
		if c == WarmClass {
			continue
		}
		e, n := local(w, d.Position)
		tp, dist := nearestTruth(w, e, n, c)
		maxErr = math.Max(maxErr, dist)
		// Flat ground: a part on top of the pile lands up to height x tan(off-nadir) away (35 deg at the image edge).
		if tol := 0.6 + tp.HeightM*0.75; dist > tol {
			errs++
			t.Errorf("%s %s at %.1f m from the nearest true %s at height %.1f m (cep %.1f)", d.DetectionId, PartNames[c], dist, PartNames[c], tp.HeightM, d.CepM)
		}
		if alive[tp.VictimID] && d.Thermal.Readable {
			readableAlive++
			if d.Thermal.Warm {
				warmAlive++
			}
		}
		checkPacket(t, d)
	}
	t.Logf("%d frames, %d packets, %d tracks reported, max position error %.2f m, living parts readable %d warm %d",
		r.frames, len(r.packets), len(news), maxErr, readableAlive, warmAlive)
	if len(news) == 0 {
		t.Fatal("nothing reported")
	}
	// The truth parts in view should mostly have been reported once each.
	if len(news) > 2*len(truthParts(w)) {
		t.Fatalf("%d tracks for %d true parts: duplicates", len(news), len(truthParts(w)))
	}
	if readableAlive == 0 || float64(warmAlive) < 0.7*float64(readableAlive) {
		t.Fatalf("living people's parts: %d readable, only %d warm", readableAlive, warmAlive)
	}
	if errs > 0 {
		t.Fatalf("%d parts placed further from the truth than flat ground explains", errs)
	}
}

// A constant 3 m GPS error east moves every position by about 3 m east.
func TestPipelineFollowsReportedPose(t *testing.T) {
	w := testWorld(t)
	r := fly(t, w, 15, 3, false)
	var de, n float64
	for _, d := range r.packets {
		c := PartIndex(d.Part)
		if c == WarmClass || d.Reason != sarpb.ReportReason_REPORT_REASON_NEW {
			continue
		}
		e, nn := local(w, d.Position)
		tp, _ := nearestTruth(w, e-3, nn, c)
		de += e - tp.E
		n++
	}
	if n == 0 || math.Abs(de/n-3) > 0.7 {
		t.Fatalf("mean east offset %.2f m over %v parts, want about 3", de/n, n)
	}
}

// With no colour detections at all, living victims still show up as warm spots.
func TestWarmSpotsWithoutModel(t *testing.T) {
	w := testWorld(t)
	r := fly(t, w, 15, 0, true)
	var found int
	for _, d := range r.packets {
		if d.Part != sarpb.PartClass_PART_CLASS_WARM_SPOT {
			t.Fatalf("only warm spots expected, got %v", d.Part)
		}
		if d.FusedConfidence > 0.6 || !d.Thermal.Warm {
			t.Fatalf("warm spot %+v", d.Thermal)
		}
		e, n := local(w, d.Position)
		if _, dist := nearestTruth(w, e, n, -1); dist < 2 {
			found++
		}
		checkPacket(t, d)
	}
	t.Logf("%d warm-spot packets, %d within 2 m of a body part", len(r.packets), found)
	if found == 0 {
		t.Fatal("no warm spot near any victim")
	}
	if found < len(r.packets)/2 {
		t.Fatalf("most warm spots are not on people: %d of %d", found, len(r.packets))
	}
}

func checkPacket(t *testing.T, d *sarpb.Detection) {
	t.Helper()
	if d.VisibleCrop == nil {
		t.Fatalf("%s: visible crop missing", d.DetectionId)
	}
	if d.ThermalCrop == nil {
		// Only parts outside the thermal camera's narrower view have no thermal crop.
		if d.Thermal.NotReadableReason != "outside thermal view" {
			t.Fatalf("%s: thermal crop missing (%+v)", d.DetectionId, d.Thermal)
		}
		return
	}
	img, err := jpeg.Decode(bytes.NewReader(d.VisibleCrop.Data))
	if err != nil || img.Bounds().Dx() != int(d.VisibleCrop.Width) || max(img.Bounds().Dx(), img.Bounds().Dy()) > CropFull {
		t.Fatalf("%s: visible crop %v %v", d.DetectionId, err, img.Bounds())
	}
	th, err := png.Decode(bytes.NewReader(d.ThermalCrop.Data))
	if err != nil || th.Bounds().Dx() != int(d.ThermalCrop.Width) {
		t.Fatalf("%s: thermal crop %v", d.DetectionId, err)
	}
	if len(d.Outline) > 2*maxOutline || len(d.Outline)%2 != 0 {
		t.Fatalf("%s: outline %d values", d.DetectionId, len(d.Outline))
	}
	for _, v := range d.Outline {
		if v < -0.01 || v > 1.01 {
			t.Fatalf("%s: outline value %v outside the crop", d.DetectionId, v)
		}
	}
	b, _ := proto.Marshal(d)
	if len(b) > 12000 {
		t.Fatalf("%s: packet %d bytes", d.DetectionId, len(b))
	}
	sizes := []int{len(b)}
	for lvl := LevelSmall; lvl <= LevelMetadata; lvl++ {
		dd := Degrade(d, lvl)
		bb, _ := proto.Marshal(dd)
		sizes = append(sizes, len(bb))
		if int(dd.Payload) != lvl+1 {
			t.Fatalf("level %d payload %v", lvl, dd.Payload)
		}
	}
	if !(sizes[0] > sizes[1] && sizes[1] > sizes[2] && sizes[2] > sizes[3]) {
		t.Fatalf("%s: degraded sizes not decreasing: %v", d.DetectionId, sizes)
	}
	if sizes[3] > 1200 {
		t.Fatalf("%s: metadata-only packet %d bytes", d.DetectionId, sizes[3])
	}
	if d.VisibleCrop.Width == 0 || !bytes.Equal(Degrade(d, LevelFull).VisibleCrop.Data, d.VisibleCrop.Data) {
		t.Fatal("full level must not change the packet")
	}
}

func TestThermalMapRoundTrip(t *testing.T) {
	m := NewThermalMap(vcam.DefaultRGB, vcam.DefaultThermal)
	u, v := m.ToThermal(960, 540)
	if math.Abs(u-320) > 1e-9 || math.Abs(v-256) > 1e-9 {
		t.Fatalf("centre maps to %.2f,%.2f", u, v)
	}
	for _, p := range [][2]float64{{0, 0}, {400, 100}, {1919, 1079}} {
		a, b := m.ToThermal(p[0], p[1])
		x, y := m.ToRGB(a, b)
		if math.Abs(x-p[0]) > 1e-9 || math.Abs(y-p[1]) > 1e-9 {
			t.Fatalf("round trip %v -> %.3f,%.3f", p, x, y)
		}
	}
}

func TestGeolocateUncertainty(t *testing.T) {
	cam, _ := vcam.New(vcam.DefaultRGB, vcam.Pose{AltAGL: 40, PitchDeg: -90})
	pe := DefaultPoseErrors(1.5, 1)
	c, ok := Geolocate(cam, 960, 540, pe)
	edge, ok2 := Geolocate(cam, 1900, 1000, pe)
	if !ok || !ok2 {
		t.Fatal("no ground hit")
	}
	if math.Hypot(c.E, c.N) > 1e-6 || math.Abs(c.RangeM-40) > 1e-6 {
		t.Fatalf("centre: %+v", c)
	}
	if CEP(c.Var()) < 1.7 || CEP(c.Var()) > 2.0 {
		t.Fatalf("centre CEP %.2f m (GPS 1.5 m alone gives 1.77)", CEP(c.Var()))
	}
	if CEP(edge.Var()) <= CEP(c.Var()) {
		t.Fatal("the image edge must be less certain than the centre")
	}
	if math.Abs(c.GSDm-40/cam.F) > 1e-9 {
		t.Fatalf("GSD %.4f", c.GSDm)
	}
}

func TestTrackerConfirmAndUpdate(t *testing.T) {
	tr := NewTracker("u", TrackParams{ConfirmHits: 2, UpdateMinInterval: 5 * time.Second, CorrTau: 30 * time.Second})
	ev := func() *Evidence { return &Evidence{FrameID: "x"} }
	t0 := time.Unix(1000, 0)
	o := func(e, n float64, conf float32, dt float64) Obs {
		return Obs{Class: 1, Conf: conf, Fix: Fix{E: e, N: n, VarIndep: 0.1, VarCorr: 2.25}, Time: t0.Add(time.Duration(dt * float64(time.Second))), Evidence: ev}
	}
	if r := tr.Update([]Obs{o(10, 10, 0.5, 0)}, t0); len(r) != 0 {
		t.Fatal("reported after one sighting")
	}
	r := tr.Update([]Obs{o(10.2, 10.1, 0.6, 1)}, t0.Add(time.Second))
	if len(r) != 1 || r[0].Reason != 1 {
		t.Fatalf("second sighting should report NEW: %+v", r)
	}
	tr.MarkReported(r[0].Track, t0.Add(time.Second))
	// Same position, nothing new, too soon: no update.
	if r := tr.Update([]Obs{o(10.1, 10, 0.6, 2)}, t0.Add(2*time.Second)); len(r) != 0 {
		t.Fatalf("unexpected update %+v", r)
	}
	// A much more confident sighting later: update.
	if r := tr.Update([]Obs{o(10.1, 10, 0.95, 9)}, t0.Add(9*time.Second)); len(r) != 1 || r[0].Reason != 2 {
		t.Fatalf("expected an update, got %+v", r)
	}
	// A sighting 20 m away is a different track.
	tr.Update([]Obs{o(30, 10, 0.6, 10)}, t0.Add(10*time.Second))
	if len(tr.Tracks()) != 2 {
		t.Fatalf("%d tracks", len(tr.Tracks()))
	}
	// A different class right on top joins the track (arm/leg confusion) ...
	ob := o(10.1, 10.05, 0.7, 11)
	ob.Class = 2
	tr.Update([]Obs{ob}, t0.Add(11*time.Second))
	if len(tr.Tracks()) != 2 {
		t.Fatalf("class flip made a new track: %d tracks", len(tr.Tracks()))
	}
	// ... but not from 1.5 m away.
	ob = o(11.6, 10, 0.7, 12)
	ob.Class = 2
	tr.Update([]Obs{ob}, t0.Add(12*time.Second))
	if len(tr.Tracks()) != 3 {
		t.Fatalf("expected a new leg track: %d tracks", len(tr.Tracks()))
	}
}

// MeanVarFactor against a simulated Gauss–Markov process sampled at 1 Hz.
func TestMeanVarFactor(t *testing.T) {
	r := rand.New(rand.NewSource(3))
	const tau, n = 30.0, 4000
	for _, span := range []int{10, 30, 60, 120} {
		a := math.Exp(-1 / tau)
		var s2 float64
		for k := 0; k < n; k++ {
			x := r.NormFloat64()
			sum := x
			for i := 0; i < span; i++ {
				x = a*x + math.Sqrt(1-a*a)*r.NormFloat64()
				sum += x
			}
			m := sum / float64(span+1)
			s2 += m * m
		}
		got, want := s2/n, MeanVarFactor(float64(span)/tau)
		if math.Abs(got-want) > 0.06 {
			t.Errorf("span %d s: simulated %.3f, MeanVarFactor %.3f", span, got, want)
		}
	}
	if f := MeanVarFactor(0); f != 1 {
		t.Fatalf("MeanVarFactor(0) = %v", f)
	}
}

// fixed answers with the same detections every frame.
type fixed struct{ ds []infer.Detection }

func (f *fixed) Name() string { return "fixed" }
func (f *fixed) DetectFrame(*raster.RGB) ([]infer.Detection, infer.Timing, error) {
	return append([]infer.Detection(nil), f.ds...), infer.Timing{}, nil
}

// A detection without mask pixels (its centre then comes from the box, which
// may lie outside the frame) cannot be located and must not be reported.
func TestEmptyMaskNotReported(t *testing.T) {
	w := testWorld(t)
	site := w.Scn.Sites[0]
	bad := infer.Detection{Class: 0, Conf: 0.9, Area: 0, CX: 5000, CY: -300, Box: [4]float32{4900, -400, 5100, -200}}
	p := Params{Thermal: ThermalParams{SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, MinPixels: 9, WarmSpotMinPx: 3},
		Track: DefaultTrackParams(), Errors: DefaultPoseErrors(1.5, 1)}
	pl := NewPipeline("t-1", &fixed{ds: []infer.Detection{bad}}, vcam.DefaultRGB, vcam.DefaultThermal, p, pos(w))
	t0 := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	for k := 0; k < 4; k++ {
		pose := vcam.Pose{E: site.CenterE, N: site.CenterN, AltAGL: 15, PitchDeg: -90}
		fr, err := w.Render(vcam.Request{UAV: "t-1", Pose: pose, Reported: pose, TOffsetS: 600, Seed: int64(k)})
		if err != nil {
			t.Fatal(err)
		}
		pk, _, err := pl.Process(Capture{FrameID: "f", Time: t0.Add(time.Duration(k) * time.Second), Reported: pose, RGB: fr.RGB, PoseLL: PoseReport{AltAGL: 15, PitchDeg: -90}})
		if err != nil {
			t.Fatal(err)
		}
		if len(pk) != 0 || len(pl.Tracker.tracks) != 0 {
			t.Fatalf("frame %d: %d reports, %d tracks from an empty mask", k, len(pk), len(pl.Tracker.tracks))
		}
	}
}

// A part watched for minutes from a hovering drone whose GPS error drifts
// (1.5 m, 30 s) keeps one track, and so do two hands 3 m apart.
func TestTrackerFollowsDrift(t *testing.T) {
	tr := NewTracker("u", TrackParams{ConfirmHits: 2, UpdateMinInterval: 5 * time.Second, CorrTau: 30 * time.Second})
	rng := rand.New(rand.NewSource(3))
	t0 := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	var ge, gn float64
	a := math.Exp(-1.0 / 30)
	for k := 0; k < 600; k++ {
		ge, gn = a*ge+1.5*math.Sqrt(1-a*a)*rng.NormFloat64(), a*gn+1.5*math.Sqrt(1-a*a)*rng.NormFloat64()
		var obs []Obs
		for _, x := range []float64{0, 3} {
			obs = append(obs, Obs{Class: 0, Conf: 0.8, Time: t0.Add(time.Duration(k) * time.Second),
				Fix: Fix{E: x + ge + 0.1*rng.NormFloat64(), N: gn + 0.1*rng.NormFloat64(), VarIndep: 0.05, VarCorr: 2.25}})
		}
		tr.Update(obs, t0.Add(time.Duration(k)*time.Second))
	}
	if n := len(tr.tracks); n != 2 {
		t.Fatalf("%d tracks for two hands watched 10 minutes", n)
	}
}

// After a new segment (a hover begins or ends) a part seen again gets a new
// track; within a segment it keeps its track.
func TestTrackerSegments(t *testing.T) {
	tr := NewTracker("u", TrackParams{ConfirmHits: 2, UpdateMinInterval: 5 * time.Second, CorrTau: 30 * time.Second})
	t0 := time.Unix(1000, 0)
	o := func(e float64, dt float64) Obs {
		return Obs{Class: 1, Conf: 0.8, Fix: Fix{E: e, N: 0, VarIndep: 0.1, VarCorr: 2.25}, Time: t0.Add(time.Duration(dt * float64(time.Second))),
			Evidence: func() *Evidence { return &Evidence{FrameID: "x"} }}
	}
	tr.Update([]Obs{o(0, 0)}, t0)
	tr.Update([]Obs{o(0.1, 1)}, t0.Add(time.Second))
	if len(tr.Tracks()) != 1 {
		t.Fatalf("%d tracks within a segment", len(tr.Tracks()))
	}
	tr.NewSegment(t0.Add(60 * time.Second))
	tr.Update([]Obs{o(0.2, 60)}, t0.Add(60*time.Second))
	tr.Update([]Obs{o(0.2, 61)}, t0.Add(61*time.Second))
	if len(tr.Tracks()) != 2 || tr.Tracks()[1].Hits != 2 {
		t.Fatalf("after a new segment: %d tracks", len(tr.Tracks()))
	}
}

// A leg reported twice (the whole leg and its lower half) is one detection;
// two legs side by side and a hand on an arm stay apart.
func TestMergeOverlapping(t *testing.T) {
	det := func(class int, conf float32, m *masks.Bitmap) infer.Detection {
		return infer.Detection{Class: class, Conf: conf, Mask: *m, Area: m.Area(), CX: float64(m.X0 + m.W/2), CY: float64(m.Y0 + m.H/2),
			Box: [4]float32{float32(m.X0), float32(m.Y0), float32(m.X0 + m.W), float32(m.Y0 + m.H)}}
	}
	leg, arm, hand := classIndex("leg"), classIndex("arm"), classIndex("hand")
	ds := []infer.Detection{
		det(leg, 0.6, rect(100, 100, 60, 12)), // whole leg
		det(leg, 0.8, rect(130, 101, 30, 10)), // its lower half, more confident
		det(leg, 0.7, rect(100, 116, 60, 12)), // the other leg, 4 px away
		det(arm, 0.9, rect(300, 100, 40, 10)),
		det(hand, 0.9, rect(330, 100, 12, 10)), // on the arm's end: another class
	}
	out := mergeOverlapping(ds)
	if len(out) != 4 {
		t.Fatalf("%d detections, want 4", len(out))
	}
	if out[0].Class != arm && out[0].Class != hand && out[0].Conf != 0.9 {
		t.Fatalf("order: %+v", out[0])
	}
	for _, d := range out {
		if d.Class == leg && d.Conf == 0.8 && (d.Area != 60*12 || d.Box[0] != 100) {
			t.Fatalf("merged leg: area %d box %v", d.Area, d.Box)
		}
	}
}
