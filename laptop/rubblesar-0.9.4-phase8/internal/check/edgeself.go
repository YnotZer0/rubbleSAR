package check

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/edge"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/linkbudget"
	"rubblesar/internal/msg"
	"rubblesar/internal/outbox"
	"rubblesar/internal/perceive"
	"rubblesar/internal/render"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
	"rubblesar/internal/vcam"
)

// EdgeSelfOptions configure 'edge-eye selftest'.
type EdgeSelfOptions struct {
	Frames int  // per altitude (scout 40 m and investigator 15 m)
	Oracle bool // use the truth oracle instead of the model (pipeline mechanics only)
}

// Thresholds of the edge self-test (the same targets as sar-check phase3).
const (
	stPipelinePassMS = 250.0
	stPipelineWarnMS = 600.0
	stMedianErrM     = 5.0
	stBudgetBps      = 64000
	stOutage         = 60 * time.Second
)

type selfFrame struct {
	alt   float64
	role  string
	reps  []*sarpb.Detection
	times perceive.StageTimes
	rndMS float64
}

// EdgeSelfTest proves the onboard pipeline on this machine without the ZBook:
// the model and scenario load, frames are processed fast enough, reports land
// near the true body parts, and the store-and-forward queue delivers every
// report through a slow link with an outage.
func EdgeSelfTest(cfg *config.Edge, o EdgeSelfOptions, out io.Writer) *Report {
	r := NewToolReport("edge-eye", "edge-selftest", out)
	if o.Frames <= 0 {
		o.Frames = 20
	}
	var w *vcam.World
	var det perceive.Detector
	var oracle *edge.Oracle
	backend := ""
	r.Context["scenario"] = cfg.Site.Scenario()
	r.Context["model"] = cfg.Perception.ModelDir
	res := r.Run("ST-01", "scenario and model load", func() Result {
		return safe("E-CHK-301", func() Result {
			var err error
			if w, err = edge.OpenWorld(cfg); err != nil {
				return Result{Status: Fail, Code: "E-CHK-301", Detail: "scenario not usable", Evidence: []string{err.Error()}}
			}
			if o.Oracle {
				oracle = &edge.Oracle{}
				det, backend = oracle, edge.OracleName
				return Result{Status: Warn, Code: "E-CHK-301", Detail: "using the truth oracle: tests the pipeline mechanics only, not the model",
					Hint: "run without --oracle to test the installed model"}
			}
			var model string
			if det, backend, model, err = edge.LoadDetector(cfg.Perception); err != nil {
				return Result{Status: Fail, Code: "E-CHK-301", Detail: "the model could not be loaded", Evidence: []string{err.Error()}}
			}
			return Result{Status: Pass, Detail: fmt.Sprintf("scenario %s (%d sites, %d victims); model %s on %s", w.Scn.ID, len(w.Scn.Sites), len(w.Scn.Victims), model, backend)}
		})
	})
	if res.Status == Fail {
		for _, id := range []string{"ST-02", "ST-03", "ST-04", "ST-05", "ST-06"} {
			r.Skip(id, map[string]string{"ST-02": "pipeline speed", "ST-03": "reports and positions", "ST-04": "store-and-forward through a slow link",
				"ST-05": "queue survives a restart", "ST-06": "movement seen while hovering"}[id], "needs ST-01")
		}
		return r
	}
	r.Context["backend"] = backend

	var frames []selfFrame
	var all []*sarpb.Detection
	res = r.Run("ST-02", "pipeline speed", func() Result {
		return safe("E-CHK-302", func() Result {
			var err error
			frames, err = selfFly(cfg, w, det, oracle, o.Frames)
			if err != nil {
				return Result{Status: Fail, Code: "E-CHK-302", Detail: "processing failed", Evidence: []string{err.Error()}}
			}
			var pipe, seg, th, tk, pk, rnd []float64
			for _, f := range frames {
				ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
				p := ms(f.times.Segment) + ms(f.times.Thermal) + ms(f.times.Track) + ms(f.times.Package)
				pipe, seg, th, tk, pk, rnd = append(pipe, p), append(seg, ms(f.times.Segment)), append(th, ms(f.times.Thermal)),
					append(tk, ms(f.times.Track)), append(pk, ms(f.times.Package)), append(rnd, f.rndMS)
				all = append(all, f.reps...)
			}
			m := med(pipe)
			detail := fmt.Sprintf("%d frames: median %.0f ms per frame (segmentation %.0f, thermal %.1f, tracking %.1f, packets %.1f); rendering the simulated frame (not part of the pipeline) %.0f ms",
				len(frames), m, med(seg), med(th), med(tk), med(pk), med(rnd))
			switch {
			case m <= stPipelinePassMS:
				return Result{Status: Pass, Detail: detail}
			case m <= stPipelineWarnMS:
				return Result{Status: Warn, Code: "E-CHK-302", Detail: detail}
			}
			return Result{Status: Fail, Code: "E-CHK-302", Detail: detail}
		})
	})
	if res.Status == Fail && len(frames) == 0 {
		return r
	}
	r.Run("ST-03", "reports and positions", func() Result {
		return safe("E-CHK-303", func() Result { return selfPositions(w, all, o.Oracle) })
	})
	r.Run("ST-04", "store-and-forward through a slow link", func() Result {
		return safe("E-CHK-304", func() Result { return selfLink(all) })
	})
	r.Run("ST-05", "queue survives a restart", func() Result {
		return safe("E-CHK-305", func() Result { return selfRestart(all) })
	})
	r.Run("ST-06", "movement seen while hovering", func() Result {
		return safe("E-CHK-306", func() Result { return selfMovement(cfg, w, det, oracle) })
	})
	return r
}

// Kinds of exposure whose movement shows in the outline (a head turning barely changes it).
var limbKinds = map[string]bool{scenario.ExpHand: true, scenario.ExpForearmHand: true, scenario.ExpArmFull: true, scenario.ExpLowerLeg: true, scenario.ExpFoot: true}

func limbs(v scenario.Victim) bool {
	for _, e := range v.Exposed {
		if limbKinds[e.Kind] {
			return true
		}
	}
	return false
}

// selfMovement hovers at 15 m over victims of the installed scenario as an
// investigation does (the target off by up to a metre, any heading, the drone
// drifting): up to two who move (a limb swinging now and then) and up to five
// who do not, living and dead, and reports what the movement detector saw.
// Each moving victim should be reported moving and no still one; the figures
// show, part by part, how much of the outline changed and how much of the
// picture under it, to set [perception] movement_change for the model.
func selfMovement(cfg *config.Edge, w *vcam.World, det perceive.Detector, oracle *edge.Oracle) Result {
	var moving, still []*scenario.Victim
	for i := range w.Scn.Victims {
		v := &w.Scn.Victims[i]
		if !limbs(*v) {
			continue
		}
		switch {
		case v.Movement.Moves && len(moving) < 2:
			moving = append(moving, v)
		case !v.Movement.Moves && len(still) < 5:
			still = append(still, v)
		}
	}
	if len(moving) == 0 || len(still) == 0 {
		return Result{Status: Warn, Code: "E-CHK-306", Detail: "the scenario has no moving victim with a visible limb, or no still one: nothing to compare"}
	}
	pc := cfg.Perception
	mp := perceive.MotionParams{Change: pc.MovementChange, TolPx: pc.MovementTolerancePx, MinFrames: pc.MovementMinFrames}
	scn := w.Scn
	pos := func(e, nn float64) (float64, float64, float64) {
		p := scn.Local().ToPoint(e, nn)
		return p.Lat, p.Lon, scn.GroundAltMSL
	}
	truth := truthParts(w)
	rng := rand.New(rand.NewSource(cfg.Seed + 6))
	type outcome struct {
		tracked, moved bool
		frames         int // moved frames, all parts
		maxChange      float64
		lines          []string
	}
	// A moving victim moves for a second or two every 6-20 s: 30 frames see it
	// at least once; 20 are enough to show a still one stays still.
	hover := func(v *scenario.Victim, k0 int) (outcome, error) {
		frames := 20
		if v.Movement.Moves {
			frames = 30
		}
		var out outcome
		pp := perceive.Params{Thermal: perceive.ThermalParams{SkinMinC: pc.SkinMinC, SkinMaxC: pc.SkinMaxC, WarmDeltaC: pc.WarmDeltaC, MinPixels: pc.MinThermalPx, WarmSpotMinPx: pc.WarmSpotMinPx},
			Track:  perceive.TrackParams{ConfirmHits: pc.ConfirmHits, UpdateMinInterval: pc.UpdateMinInterval.Duration, CorrTau: 30 * time.Second, DetSigmaM: pc.DetectionSigmaM, Motion: mp},
			Errors: selfPoseErrors(cfg)}
		pl := perceive.NewPipeline("selftest-hover", det, vcam.DefaultRGB, vcam.DefaultThermal, pp, pos)
		t0 := time.Date(2026, 2, 10, 9, 30, 0, 0, time.UTC)
		const ge, gn = 0.8, -0.5 // GPS error: slow, about constant over the hover
		te, tn := v.E+0.7*rng.NormFloat64(), v.N+0.7*rng.NormFloat64()
		yaw := math.Round(rng.Float64() * 360)
		task := "ST-06-" + v.ID
		for k := 0; k < frames; k++ {
			we, wn := 0.2*math.Sin(float64(k)/4+yaw), 0.15*math.Cos(float64(k)/5+yaw)
			p := vcam.Pose{E: te + we, N: tn + wn, AltAGL: 15, YawDeg: yaw, PitchDeg: -90}
			rep := p
			rep.E, rep.N = p.E+ge, p.N+gn
			sim := 1200 + float64(k0+k)
			fr, err := w.Render(vcam.Request{UAV: "selftest-hover", FrameID: fmt.Sprintf("%s-%02d", task, k), Pose: p, Reported: rep, TOffsetS: sim, Seed: int64(50000 + k0 + k)})
			if err != nil {
				return out, err
			}
			if oracle != nil {
				oracle.SetFrame(fr)
			}
			ll := scn.Local().ToPoint(rep.E, rep.N)
			if _, _, err := pl.Process(perceive.Capture{FrameID: fmt.Sprintf("%s-%02d", task, k), Time: t0.Add(time.Duration(k) * time.Second), SimTimeS: sim,
				Reported: rep, PoseLL: perceive.PoseReport{Lat: ll.Lat, Lon: ll.Lon, AltAGL: 15, YawDeg: yaw, PitchDeg: -90}, RGB: fr.RGB, Thermal: fr.Thermal, Hover: true, TaskID: task}); err != nil {
				return out, err
			}
		}
		for _, d := range pl.EndHover(task, te+ge, tn+gn, 4, t0.Add(time.Duration(frames)*time.Second)) {
			m := d.GetMovement()
			if m == nil || m.HoverFrames < 3 {
				continue
			}
			// Whose part: the nearest true part (the report carries the GPS error).
			e, n := scn.Local().ToLocal(geoOf(d.Position))
			best, who := math.Inf(1), ""
			for _, tp := range truth {
				if dd := math.Hypot(tp.E-(e-ge), tp.N-(n-gn)); dd < best {
					best, who = dd, tp.VictimID
				}
			}
			if who != v.ID || best > 2.5 {
				continue
			}
			out.tracked = true
			out.moved = out.moved || m.Moved
			out.frames += int(m.MovedFrames)
			out.maxChange = math.Max(out.maxChange, float64(m.MaxChange))
			out.lines = append(out.lines, fmt.Sprintf("%s %s: changed in %d of %d frames (largest %.0f%%), moved %v",
				v.ID, strings.ToLower(strings.TrimPrefix(d.Part.String(), "PART_CLASS_")), m.MovedFrames, m.HoverFrames, 100*m.MaxChange, m.Moved))
		}
		// As casualty-analyst decides: the parts' moved frames together.
		out.moved = out.moved || out.frames >= mp.MinFrames
		if !out.tracked {
			out.lines = append(out.lines, fmt.Sprintf("%s: no part followed through the hover", v.ID))
		}
		return out, nil
	}
	var ev []string
	ev = append(ev, fmt.Sprintf("settings: a frame moved when over %.0f%% of an outline shifts by more than %d px and the picture changed where it did; movement needs %d such frames (all the victim's parts together)",
		100*mp.Change, mp.TolPx, mp.MinFrames))
	count := func(vs []*scenario.Victim, k0 int) (tracked, moved int, maxCh float64, ids []string, err error) {
		for i, v := range vs {
			o, err := hover(v, k0+100*i)
			if err != nil {
				return 0, 0, 0, nil, err
			}
			ev = append(ev, o.lines...)
			if o.tracked {
				tracked++
				maxCh = math.Max(maxCh, o.maxChange)
				if o.moved {
					moved++
					ids = append(ids, v.ID)
				}
			}
		}
		return tracked, moved, maxCh, ids, nil
	}
	mt, mm, mmax, _, err := count(moving, 0)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-306", Detail: "hover simulation failed", Evidence: []string{err.Error()}}
	}
	st, sm, smax, sids, err := count(still, 1000)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-306", Detail: "hover simulation failed", Evidence: []string{err.Error()}}
	}
	detail := fmt.Sprintf("moving victims seen moving: %d of %d (largest change %.0f%%); still victims seen moving: %d of %d (largest change %.0f%%)",
		mm, mt, 100*mmax, sm, st, 100*smax)
	switch {
	case mt == 0 || st == 0:
		return Result{Status: Warn, Code: "E-CHK-306", Detail: detail + "; the model followed no part of the moving or of the still victims from 15 m", Evidence: ev}
	case mm > 0 && sm == 0:
		return Result{Status: Pass, Detail: detail, Evidence: ev}
	}
	hint := "movement and stillness overlap for this model: paste the block"
	if sm > 0 {
		detail += " (" + strings.Join(sids, ", ") + ")"
	}
	if mmax > smax {
		hint = fmt.Sprintf("try [perception] movement_change = %.2f in /etc/rubblesar/edge-eye.toml (between the two), then run the self-test again", (mmax+smax)/2)
	}
	return Result{Status: Warn, Code: "E-CHK-306", Detail: detail, Evidence: ev, Hint: hint}
}

func med(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// selfFly runs the pipeline along the first site's survey at 40 m (scout) and
// 15 m (investigator), with drifting GPS and heading errors.
func selfFly(cfg *config.Edge, w *vcam.World, det perceive.Detector, oracle *edge.Oracle, n int) ([]selfFrame, error) {
	var out []selfFrame
	site := w.Scn.Sites[0]
	scn := w.Scn
	pos := func(e, nn float64) (float64, float64, float64) {
		p := scn.Local().ToPoint(e, nn)
		return p.Lat, p.Lon, scn.GroundAltMSL
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	t0 := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	for ri, role := range []struct {
		name       string
		alt, speed float64
	}{{"scout", 40, 5}, {"investigator", 15, 2.5}} {
		u := config.UAV{ID: "selftest-" + role.name, AltAGLm: role.alt, SpeedMps: 8, SurveySpeedMps: role.speed, SideOverlap: 0.7, SurveySites: []string{site.ID}}
		m, err := edge.SurveyMission(w, u)
		if err != nil {
			return nil, err
		}
		pc := cfg.Perception
		pp := perceive.Params{Thermal: perceive.ThermalParams{SkinMinC: pc.SkinMinC, SkinMaxC: pc.SkinMaxC, WarmDeltaC: pc.WarmDeltaC, MinPixels: pc.MinThermalPx, WarmSpotMinPx: pc.WarmSpotMinPx},
			WarmSpots: pc.WarmSpots, Track: perceive.TrackParams{ConfirmHits: pc.ConfirmHits, UpdateMinInterval: pc.UpdateMinInterval.Duration, CorrTau: 30 * time.Second},
			Errors: selfPoseErrors(cfg)}
		pl := perceive.NewPipeline(u.ID, det, vcam.DefaultRGB, vcam.DefaultThermal, pp, pos)
		// Positions every second along the survey legs.
		var poses []vcam.Pose
		for i := 1; i < len(m.Waypoints) && len(poses) < n; i += 2 {
			a, b := m.Waypoints[i-1], m.Waypoints[i]
			l := math.Hypot(b.E-a.E, b.N-a.N)
			yaw := math.Mod(math.Atan2(b.E-a.E, b.N-a.N)*180/math.Pi+360, 360)
			for d := 0.0; d <= l && len(poses) < n; d += role.speed {
				poses = append(poses, vcam.Pose{E: a.E + (b.E-a.E)*d/l, N: a.N + (b.N-a.N)*d/l, AltAGL: role.alt, YawDeg: yaw, PitchDeg: -90})
			}
		}
		var ge, gn, gy float64 = rng.NormFloat64() * 1.5, rng.NormFloat64() * 1.5, rng.NormFloat64()
		var seq msg.Seq
		for k, p := range poses {
			a := math.Exp(-1.0 / 30)
			ge, gn = a*ge+1.5*math.Sqrt(1-a*a)*rng.NormFloat64(), a*gn+1.5*math.Sqrt(1-a*a)*rng.NormFloat64()
			gy = math.Exp(-1.0/60)*gy + math.Sqrt(1-math.Exp(-2.0/60))*rng.NormFloat64()
			rep := p
			rep.E, rep.N, rep.YawDeg = p.E+ge, p.N+gn, math.Mod(p.YawDeg+gy+360, 360)
			t := t0.Add(time.Duration(ri*1000+k) * time.Second)
			r0 := time.Now()
			fr, err := w.Render(vcam.Request{UAV: u.ID, FrameID: fmt.Sprintf("%s-%04d", u.ID, k), Pose: p, Reported: rep, TOffsetS: 900 + float64(k), Seed: int64(ri*10000 + k)})
			if err != nil {
				return nil, err
			}
			rnd := float64(time.Since(r0).Microseconds()) / 1000
			if oracle != nil {
				oracle.SetFrame(fr)
			}
			ll := scn.Local().ToPoint(rep.E, rep.N)
			reps, st, err := pl.Process(perceive.Capture{FrameID: fmt.Sprintf("%s-%04d", u.ID, k), Time: t, SimTimeS: 900 + float64(k), Reported: rep,
				PoseLL: perceive.PoseReport{Lat: ll.Lat, Lon: ll.Lon, AltAGL: rep.AltAGL, YawDeg: rep.YawDeg, PitchDeg: -90}, RGB: fr.RGB, Thermal: fr.Thermal})
			if err != nil {
				return nil, err
			}
			for _, d := range reps {
				d.Header = msg.NewHeader(cfg.Site.IncidentID, "uav/"+u.ID, seq.Next())
			}
			out = append(out, selfFrame{alt: role.alt, role: role.name, reps: reps, times: st, rndMS: rnd})
		}
	}
	return out, nil
}

// selfPositions compares reported body parts with the nearest true part of the same class.
func selfPositions(w *vcam.World, all []*sarpb.Detection, oracle bool) Result {
	var truth []render.TruthPart
	for _, st := range w.Scn.Sites {
		truth = append(truth, w.Manifest(st.ID).Truth...)
	}
	var errs []float64
	var bad []string
	warm, sizes := 0, []float64{}
	for _, d := range all {
		b, _ := proto.Marshal(d)
		sizes = append(sizes, float64(len(b)))
		if p := packetProblem(d); p != "" {
			bad = append(bad, d.DetectionId+": "+p)
		}
		c := perceive.PartIndex(d.Part)
		if d.Part == sarpb.PartClass_PART_CLASS_WARM_SPOT {
			warm++
			continue
		}
		if d.Reason != sarpb.ReportReason_REPORT_REASON_NEW {
			continue
		}
		e, n := w.Scn.Local().ToLocal(geoOf(d.Position))
		best := math.Inf(1)
		for _, tp := range truth {
			if tp.Class-1 == c {
				best = math.Min(best, math.Hypot(tp.E-e, tp.N-n))
			}
		}
		errs = append(errs, best)
	}
	detail := fmt.Sprintf("%d reports (%d warm spots), packets median %.1f KB; ", len(all), warm, med(sizes)/1024)
	if len(bad) > 0 {
		return Result{Status: Fail, Code: "E-CHK-303", Detail: detail + fmt.Sprintf("%d malformed packets", len(bad)), Evidence: limit(bad, 10)}
	}
	if len(errs) == 0 {
		st := Warn
		if oracle {
			st = Fail
		}
		return Result{Status: st, Code: "E-CHK-303", Detail: detail + "no body part reported in the test frames",
			Hint: "with the trained model at least a few parts should be found; a stand-in model finds nothing real"}
	}
	sort.Float64s(errs)
	m := errs[len(errs)/2]
	detail += fmt.Sprintf("body parts: %d, distance to the nearest true part of the same class median %.2f m, worst %.2f m", len(errs), m, errs[len(errs)-1])
	if m > stMedianErrM {
		return Result{Status: Fail, Code: "E-CHK-303", Detail: detail}
	}
	return Result{Status: Pass, Detail: detail}
}

// packetProblem checks one packet's contents ("" if fine).
func packetProblem(d *sarpb.Detection) string {
	switch {
	case d.GetHeader().GetSeq() == 0:
		return "no header"
	case d.Position == nil || d.CepM <= 0:
		return "no position or CEP"
	case len(d.Outline) > 64 || len(d.Outline)%2 != 0:
		return fmt.Sprintf("outline of %d values", len(d.Outline))
	case d.Summary == "":
		return "no summary"
	}
	if c := d.VisibleCrop; c != nil {
		if _, err := jpeg.Decode(bytes.NewReader(c.Data)); err != nil {
			return "visible crop: " + err.Error()
		}
	}
	if c := d.ThermalCrop; c != nil {
		if _, err := png.Decode(bytes.NewReader(c.Data)); err != nil {
			return "thermal crop: " + err.Error()
		}
	}
	return ""
}

// selfLink pushes the reports through an outbox over an in-memory bus at
// 64 kbit/s with a 60 s outage, on a simulated clock, with an intake that
// acknowledges what it receives (only while the link is up).
func selfLink(found []*sarpb.Detection) Result {
	if len(found) == 0 {
		return Result{Status: Skip, Detail: "no reports to send"}
	}
	// A sustained load above the budget: the reports found, repeated with new
	// ids until there are at least 300 (about 1.5 MB in two minutes, twice
	// what 64 kbit/s carries), so the payload reduction has to work.
	var all []*sarpb.Detection
	for k := 0; len(all) < max(300, len(found)); k++ {
		for _, d := range found {
			c := proto.Clone(d).(*sarpb.Detection)
			c.DetectionId = fmt.Sprintf("%s~%d", d.DetectionId, k)
			c.Header.Seq = uint64(len(all) + 1)
			all = append(all, c)
		}
	}
	dir, err := os.MkdirTemp("", "rubblesar-selftest-")
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-305", Detail: "no temporary folder", Evidence: []string{err.Error()}}
	}
	defer os.RemoveAll(dir)
	now := time.Unix(2_000_000_000, 0)
	clock := func() time.Time { return now }
	ob, err := outbox.Open(outbox.Config{Dir: dir, MaxBytes: 200 << 20, AckTimeout: 8 * time.Second, MaxBackoff: 60 * time.Second,
		ProbeAfter: 20 * time.Second, Reserve: 300, Encode: func(d *sarpb.Detection, l int) ([]byte, error) { return proto.Marshal(perceive.Degrade(d, l)) }}, clock)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-305", Detail: "queue", Evidence: []string{err.Error()}}
	}
	m := bus.NewMemory()
	defer m.Close()
	pub, _ := m.DeclarePublisher(keys.Detection("selftest", "u"), bus.QoSDetection)
	got := map[string]int{}
	var pending []string
	var bytesOnWire int
	outage := false
	done := make(chan struct{}, 1024)
	m.Subscribe(keys.DetectionAll("selftest"), func(s bus.Sample) {
		var d sarpb.Detection
		if proto.Unmarshal(s.Payload, &d) == nil && !outage {
			got[d.DetectionId]++
			pending = append(pending, d.DetectionId)
		}
		done <- struct{}{}
	})
	bucket := linkbudget.New(stBudgetBps, clock)
	ids := map[string]bool{}
	next := 0
	start := now
	var outageAt, outageEnd, caughtUp time.Time
	maxLevel := 0
	for step := 0; step < 30*60*10; step++ { // up to 30 simulated minutes in 100 ms steps
		// Reports arrive over the first 2 minutes.
		for next < len(all) && now.Sub(start) >= time.Duration(next)*120*time.Second/time.Duration(len(all)) {
			if err := ob.Add(all[next]); err != nil {
				return Result{Status: Fail, Code: "E-CHK-305", Detail: "queue write", Evidence: []string{err.Error()}}
			}
			ids[all[next].DetectionId] = true
			next++
		}
		if outageAt.IsZero() && now.Sub(start) >= 60*time.Second {
			outageAt, outageEnd = now, now.Add(stOutage)
			ob.SetOutage(stOutage)
			outage = true
		}
		if outage && !now.Before(outageEnd) {
			outage = false
		}
		sent := ob.Pump(pub, bucket)
		for i := 0; i < sent; i++ {
			<-done
		}
		if len(pending) > 0 {
			ob.Ack(pending)
			pending = nil
		}
		if l := ob.Stats().Level; l > maxLevel {
			maxLevel = l
		}
		bytesOnWire = int(ob.Stats().BytesOnWire)
		if next == len(all) && len(got) == len(ids) && ob.Stats().Queued == 0 && !outageAt.IsZero() && !outage {
			caughtUp = now
			break
		}
		now = now.Add(100 * time.Millisecond)
	}
	elapsed := now.Sub(start).Seconds()
	st := ob.Stats()
	missing := 0
	for id := range ids {
		if got[id] == 0 {
			missing++
		}
	}
	dupes := 0
	for _, n := range got {
		dupes += n - 1
	}
	limitBytes := elapsed*stBudgetBps/8 + stBudgetBps/8 + 12000
	detail := fmt.Sprintf("%d reports in 2 min at %d kbit/s with a %.0f s outage: delivered %d, missing %d, repeated %d, queue left %d, payload reduced down to level %d, %.0f s in all, %.0f kbit/s on average",
		len(ids), stBudgetBps/1000, stOutage.Seconds(), len(got), missing, dupes, st.Queued, maxLevel, elapsed, float64(bytesOnWire)*8/elapsed/1000)
	if !caughtUp.IsZero() {
		detail += fmt.Sprintf("; caught up %.0f s after the outage", caughtUp.Sub(outageEnd).Seconds())
	}
	if missing > 0 || st.Queued > 0 {
		return Result{Status: Fail, Code: "E-CHK-304", Detail: detail}
	}
	if float64(bytesOnWire) > limitBytes {
		return Result{Status: Fail, Code: "E-CHK-304", Detail: detail + ": over budget"}
	}
	return Result{Status: Pass, Detail: detail}
}

// selfRestart writes reports to a queue, reopens it and expects them all back.
func selfRestart(all []*sarpb.Detection) Result {
	if len(all) == 0 {
		return Result{Status: Skip, Detail: "no reports"}
	}
	dir, err := os.MkdirTemp("", "rubblesar-selftest-")
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-305", Detail: "no temporary folder", Evidence: []string{err.Error()}}
	}
	defer os.RemoveAll(dir)
	cfg := outbox.Config{Dir: dir, MaxBytes: 200 << 20, AckTimeout: 8 * time.Second, MaxBackoff: time.Minute, ProbeAfter: 20 * time.Second}
	ob, err := outbox.Open(cfg, nil)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-305", Detail: "queue", Evidence: []string{err.Error()}}
	}
	n := min(len(all), 50)
	for _, d := range all[:n] {
		if err := ob.Add(d); err != nil {
			return Result{Status: Fail, Code: "E-CHK-305", Detail: "queue write", Evidence: []string{err.Error()}}
		}
	}
	ob2, err := outbox.Open(cfg, nil)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-305", Detail: "reopening the queue", Evidence: []string{err.Error()}}
	}
	if got := ob2.Stats().LoadedOnOpen; got != n {
		return Result{Status: Fail, Code: "E-CHK-305", Detail: fmt.Sprintf("%d reports written, %d found after reopening", n, got)}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d reports written to disk and all found again after reopening", n)}
}

func geoOf(g *sarpb.GeoPoint) geo.Point { return geo.Point{Lat: g.GetLatDeg(), Lon: g.GetLonDeg()} }

func selfPoseErrors(cfg *config.Edge) perceive.PoseErrors {
	e := perceive.DefaultPoseErrors(1.5, 1)
	e.SurfaceM = cfg.Perception.DebrisHeightM
	return e
}
