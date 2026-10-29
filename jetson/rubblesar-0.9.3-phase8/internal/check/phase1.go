package check

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/score"
	"rubblesar/internal/thermal"
	"rubblesar/internal/vcam"
)

// Phase1Options configures the phase 1 acceptance test.
type Phase1Options struct {
	ScenarioDir string
	Site        config.Site // [site] from this machine's config: the scenario must match it
	Quick       bool        // test a third of the victims (plus every moving one)
}

// Phase 1 thresholds.
const (
	p1MinSites       = 3
	p1MinVictims     = 20
	p1AlignTolM      = 0.15
	p1FramePassMS    = 2500
	p1FrameWarnMS    = 6000
	p1LivingMarginC  = 6.0
	p1DeceasedMargin = 6.0
)

// safe turns a panic inside a check into a failed result with the stack top.
func safe(code diag.Code, fn func() Result) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			st := strings.Split(string(debug.Stack()), "\n")
			if len(st) > 14 {
				st = st[:14]
			}
			res = Result{Status: Fail, Code: code, Detail: fmt.Sprintf("internal error: %v", r), Evidence: st}
		}
	}()
	return fn()
}

func limit(ev []string, n int) []string {
	if len(ev) > n {
		return append(ev[:n], fmt.Sprintf("... and %d more", len(ev)-n))
	}
	return ev
}

// Phase1 runs the scenario and imagery acceptance test.
func Phase1(o Phase1Options, out io.Writer) *Report {
	r := NewReport("phase1", out)
	r.Context["scenario"] = o.ScenarioDir
	r.Context["cpus"] = fmt.Sprint(runtime.NumCPU())

	var s *scenario.Scenario
	res := r.Run("P1-01", "scenario", func() Result { return safe("E-CHK-101", func() Result { return checkScenario(o, &s) }) })
	if res.Status == Fail {
		for _, id := range []string{"P1-02", "P1-03", "P1-04", "P1-05", "P1-06", "P1-07", "P1-08"} {
			r.Skip(id, p1Names[id], "needs a valid scenario (P1-01)")
		}
		return r
	}
	r.Context["scenario_hash"] = s.Hash()[:16]
	var w *vcam.World
	res = r.Run("P1-02", p1Names["P1-02"], func() Result { return safe("E-CHK-102", func() Result { return checkLayers(o, s, &w) }) })
	r.Run("P1-03", p1Names["P1-03"], func() Result { return safe("E-CHK-103", checkGeometry) })
	if res.Status == Fail {
		for _, id := range []string{"P1-04", "P1-05", "P1-06", "P1-07", "P1-08"} {
			r.Skip(id, p1Names[id], "needs intact layers (P1-02)")
		}
		return r
	}
	victims := s.Victims
	if o.Quick {
		var q []scenario.Victim
		for i, v := range victims {
			if i%3 == 0 || v.Movement.Moves { // always keep the moving victims for P1-06
				q = append(q, v)
			}
		}
		victims = q
	}
	r.Run("P1-04", p1Names["P1-04"], func() Result { return safe("E-CHK-104", func() Result { return checkVisibility(w, victims) }) })
	r.Run("P1-05", p1Names["P1-05"], func() Result { return safe("E-CHK-105", func() Result { return checkThermal(w, victims) }) })
	r.Run("P1-06", p1Names["P1-06"], func() Result { return safe("E-CHK-106", func() Result { return checkMovement(w, victims) }) })
	r.Run("P1-07", p1Names["P1-07"], func() Result { return safe("E-CHK-107", func() Result { return checkSpeed(w) }) })
	r.Run("P1-08", p1Names["P1-08"], func() Result { return safe("E-CHK-108", func() Result { return checkScorer(w) }) })
	return r
}

var p1Names = map[string]string{
	"P1-01": "scenario",
	"P1-02": "layer integrity",
	"P1-03": "camera geometry",
	"P1-04": "ground truth visible & aligned",
	"P1-05": "thermal plausibility",
	"P1-06": "victim movement",
	"P1-07": "frame render time",
	"P1-08": "scorer self-test",
}

func checkScenario(o Phase1Options, out **scenario.Scenario) Result {
	s, err := scenario.Load(filepath.Join(o.ScenarioDir, "scenario.json"))
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-101", Detail: "scenario.json unusable", Evidence: []string{err.Error()}}
	}
	*out = s
	var fails, warns []string
	if len(s.Sites) < p1MinSites {
		fails = append(fails, fmt.Sprintf("%d sites, need >= %d", len(s.Sites), p1MinSites))
	}
	if len(s.Victims) < p1MinVictims {
		fails = append(fails, fmt.Sprintf("%d victims, need >= %d", len(s.Victims), p1MinVictims))
	}
	if o.Site.IncidentID != "" && s.ID != o.Site.IncidentID {
		fails = append(fails, fmt.Sprintf("scenario id %q but [site] incident_id is %q", s.ID, o.Site.IncidentID))
	}
	if o.Site.CenterLat != 0 {
		d := geo.Distance(geo.Point{Lat: s.Center.Lat, Lon: s.Center.Lon}, geo.Point{Lat: o.Site.CenterLat, Lon: o.Site.CenterLon})
		if d > 1 {
			fails = append(fails, fmt.Sprintf("scenario centre is %.0f m from [site] centre in the config", d))
		}
	}
	statuses := map[string]int{}
	classes := map[int]int{}
	for _, v := range s.Victims {
		statuses[v.Status]++
		for _, c := range v.Classes() {
			classes[c]++
		}
	}
	for _, st := range []string{thermal.AliveResponsive, thermal.AliveUnresponsive, thermal.Deteriorating, thermal.Deceased} {
		if statuses[st] == 0 {
			warns = append(warns, "no victim with status "+st)
		}
	}
	for c := 1; c < scenario.NumParts; c++ {
		if classes[c] == 0 {
			warns = append(warns, "no victim shows a "+scenario.PartNames[c])
		}
	}
	detail := fmt.Sprintf("%s: %d sites, %d victims (%d responsive, %d unresponsive, %d deteriorating, %d deceased)", s.ID, len(s.Sites), len(s.Victims),
		statuses[thermal.AliveResponsive], statuses[thermal.AliveUnresponsive], statuses[thermal.Deteriorating], statuses[thermal.Deceased])
	switch {
	case len(fails) > 0:
		return Result{Status: Fail, Code: "E-CHK-101", Detail: detail, Evidence: append(fails, warns...)}
	case len(warns) > 0:
		return Result{Status: Warn, Code: "E-CHK-101", Detail: detail, Evidence: warns}
	}
	return Result{Status: Pass, Detail: detail}
}

func checkLayers(o Phase1Options, s *scenario.Scenario, out **vcam.World) Result {
	var ev []string
	var bytes int64
	nPatches, nDecoys := 0, 0
	for _, st := range s.Sites {
		site, err := render.LoadSite(o.ScenarioDir, s, st.ID, true)
		if err != nil {
			ev = append(ev, fmt.Sprintf("site %s: %v", st.ID, err))
			continue
		}
		m := site.Manifest
		nPatches += len(m.Patches)
		nDecoys += len(m.Decoys)
		shown := map[int]bool{}
		for _, t := range m.Truth {
			shown[t.Victim] = true
		}
		for _, v := range s.VictimsAt(st.ID) {
			if !shown[v.Index] {
				ev = append(ev, fmt.Sprintf("site %s: victim %s has no visible body pixels", st.ID, v.ID))
			}
		}
		filepath.Walk(render.SiteDir(o.ScenarioDir, st.ID), func(_ string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				bytes += fi.Size()
			}
			return nil
		})
	}
	if len(ev) > 0 {
		return Result{Status: Fail, Code: "E-CHK-102", Detail: fmt.Sprintf("%d problem(s)", len(ev)), Evidence: limit(ev, 10)}
	}
	w, err := vcam.Open(o.ScenarioDir, false)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-102", Detail: "cannot open the rendered scenario", Evidence: []string{err.Error()}}
	}
	*out = w
	return Result{Status: Pass, Detail: fmt.Sprintf("%d sites verified (sha256), %d pose-B patches, %d decoys, %.0f MB", len(s.Sites), nPatches, nDecoys, float64(bytes)/1e6)}
}

func checkGeometry() Result {
	poses := []vcam.Pose{
		{E: 10, N: -5, AltAGL: 40, YawDeg: 0, PitchDeg: -90},
		{E: 0, N: 0, AltAGL: 15, YawDeg: 137, PitchDeg: -90},
		{E: -30, N: 20, AltAGL: 60, YawDeg: 300, PitchDeg: -45, RollDeg: 3},
		{E: 300, N: -200, AltAGL: 120, YawDeg: 211, PitchDeg: -70},
	}
	worst := 0.0
	var ev []string
	n := 0
	for _, p := range poses {
		for _, in := range []vcam.Intrinsics{vcam.DefaultRGB, vcam.DefaultThermal} {
			c, err := vcam.New(in, p)
			if err != nil {
				return Result{Status: Fail, Code: "E-CHK-103", Detail: err.Error()}
			}
			for v := 0.5; v < float64(in.H); v += float64(in.H) / 7 {
				for u := 0.5; u < float64(in.W); u += float64(in.W) / 9 {
					for _, h := range []float64{0, 3} {
						e, nn, ok := c.GroundAt(u, v, h)
						if !ok {
							ev = append(ev, fmt.Sprintf("pose %+v pixel (%.1f,%.1f) misses the ground", p, u, v))
							continue
						}
						u2, v2, ok := c.Project(e, nn, h)
						d := math.Hypot(u2-u, v2-v)
						if !ok || d > 1e-6 {
							ev = append(ev, fmt.Sprintf("pose %+v pixel (%.1f,%.1f) round trip error %.2e px", p, u, v, d))
						}
						worst = math.Max(worst, d)
						n++
					}
				}
			}
		}
	}
	c, _ := vcam.New(vcam.DefaultRGB, vcam.Pose{AltAGL: 40, PitchDeg: -90})
	fp := c.Footprint()
	want := 2 * 40 * math.Tan(35*math.Pi/180)
	if math.Abs(fp[1][0]-fp[0][0]-want) > 1e-6 {
		ev = append(ev, fmt.Sprintf("40 m nadir footprint %.4f m wide, expected %.4f", fp[1][0]-fp[0][0], want))
	}
	if len(ev) > 0 {
		return Result{Status: Fail, Code: "E-CHK-103", Detail: fmt.Sprintf("%d of %d projections wrong", len(ev), n), Evidence: limit(ev, 8)}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d pixel->ground->pixel round trips, worst %.1e px; 40 m footprint %.1f x %.1f m, %.1f cm/px",
		n, worst, want, want*9/16, c.GSDCenter()*100)}
}

func nadir(v scenario.Victim, alt float64) vcam.Pose {
	return vcam.Pose{E: v.E, N: v.N, AltAGL: alt, PitchDeg: -90}
}

func checkVisibility(w *vcam.World, victims []scenario.Victim) Result {
	var ev []string
	worst, parts := 0.0, 0
	for _, v := range victims {
		m := w.Manifest(v.SiteID)
		f, err := w.Render(vcam.Request{UAV: "check", Pose: nadir(v, 12), NoTherm: true})
		if err != nil {
			return Result{Status: Fail, Code: "E-CHK-104", Detail: err.Error()}
		}
		c, _ := vcam.New(vcam.DefaultRGB, nadir(v, 12))
		for _, tp := range m.Truth {
			if tp.Victim != v.Index {
				continue
			}
			var vis *vcam.Visible
			for i := range f.Meta.Visible {
				if f.Meta.Visible[i].Victim == v.Index && f.Meta.Visible[i].Class == tp.Class {
					vis = &f.Meta.Visible[i]
				}
			}
			if vis == nil {
				ev = append(ev, fmt.Sprintf("%s %s (%d layer pixels) not in the 12 m frame", v.ID, tp.ClassName, tp.Pixels))
				continue
			}
			e, n, _ := c.GroundAt(vis.CentroidPx[0], vis.CentroidPx[1], tp.HeightM)
			d := math.Hypot(e-tp.E, n-tp.N)
			worst = math.Max(worst, d)
			parts++
			if d > p1AlignTolM {
				ev = append(ev, fmt.Sprintf("%s %s label centroid geolocates %.2f m from truth", v.ID, tp.ClassName, d))
			}
		}
	}
	if len(ev) > 0 {
		return Result{Status: Fail, Code: "E-CHK-104", Detail: fmt.Sprintf("%d problem(s) in %d victims", len(ev), len(victims)), Evidence: limit(ev, 10)}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d victims, %d parts seen from 12 m; worst label/truth offset %.3f m (limit %.2f)", len(victims), parts, worst, p1AlignTolM)}
}

// victimTemps returns max and mean temperature of a victim's labelled thermal pixels.
func victimTemps(f *vcam.Frame, idx int) (maxT, mean float64, n int) {
	maxT = -273
	sum := 0.0
	for i, v := range f.InstTh.Pix {
		if int(v) == idx && f.PartsTh.Pix[i] > 0 {
			c := thermal.CentiKToC(f.Thermal.Pix[i])
			maxT = math.Max(maxT, c)
			sum += c
			n++
		}
	}
	if n > 0 {
		mean = sum / float64(n)
	}
	return
}

func checkThermal(w *vcam.World, victims []scenario.Victim) Result {
	var ev []string
	var lines []string
	cooled, deteriorating := 0, 0
	for _, v := range victims {
		f, err := w.Render(vcam.Request{UAV: "check", Pose: nadir(v, 12), NoRGB: true})
		if err != nil {
			return Result{Status: Fail, Code: "E-CHK-105", Detail: err.Error()}
		}
		mx, mean, n := victimTemps(f, v.Index)
		air := f.Meta.AirC
		if n == 0 {
			ev = append(ev, fmt.Sprintf("%s: no thermal pixels from 12 m", v.ID))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s %-18s max %5.1f C mean %5.1f C (air %.1f)", v.ID, v.Status, mx, mean, air))
		switch {
		case v.Status != thermal.Deceased && mx < air+p1LivingMarginC:
			ev = append(ev, fmt.Sprintf("%s (%s): hottest pixel %.1f C, air %.1f C: a living victim must be > %.0f C above air", v.ID, v.Status, mx, air, p1LivingMarginC))
		case v.Status == thermal.Deceased && v.Thermal.DeathHours < -10 && mean > air+p1DeceasedMargin:
			ev = append(ev, fmt.Sprintf("%s deceased %.0f h ago: mean %.1f C, air %.1f C: should be near ambient", v.ID, -v.Thermal.DeathHours, mean, air))
		}
		if v.Status == thermal.Deteriorating {
			deteriorating++
			g, err := w.Render(vcam.Request{UAV: "check", Pose: nadir(v, 12), NoRGB: true, TOffsetS: 3 * 3600})
			if err != nil {
				return Result{Status: Fail, Code: "E-CHK-105", Detail: err.Error()}
			}
			_, mean3, _ := victimTemps(g, v.Index)
			// Skin must lose warmth relative to the (warming) air.
			if mean3-g.Meta.AirC < mean-air-0.5 {
				cooled++
			} else {
				ev = append(ev, fmt.Sprintf("%s deteriorating: excess over air %.1f C at t=0 and %.1f C at t=3h (should fall)", v.ID, mean-air, mean3-g.Meta.AirC))
			}
		}
	}
	// Open ground far from all sites stays near air temperature.
	f, err := w.Render(vcam.Request{UAV: "check", Pose: vcam.Pose{E: 0, N: -700, AltAGL: 40, PitchDeg: -90}, NoRGB: true})
	if err == nil && len(f.Meta.Sites) == 0 {
		c := thermal.CentiKToC(f.Thermal.Pix[len(f.Thermal.Pix)/2])
		if c < f.Meta.AirC-1 || c > f.Meta.AirC+8 {
			ev = append(ev, fmt.Sprintf("open ground %.1f C with air %.1f C", c, f.Meta.AirC))
		}
	}
	if len(ev) > 0 {
		return Result{Status: Fail, Code: "E-CHK-105", Detail: fmt.Sprintf("%d problem(s)", len(ev)), Evidence: limit(append(ev, lines...), 14)}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d victims in their temperature bands; %d/%d deteriorating victims cooled over 3 h", len(victims), cooled, deteriorating)}
}

func mask(f *vcam.Frame, idx int) []byte {
	m := make([]byte, len(f.InstRGB.Pix))
	for i, v := range f.InstRGB.Pix {
		if int(v) == idx {
			m[i] = 1
		}
	}
	return m
}

func same(a, b []byte) bool { return string(a) == string(b) }

func checkMovement(w *vcam.World, victims []scenario.Victim) Result {
	var ev []string
	moved, still := 0, 0
	for _, v := range victims {
		on, off := -1.0, -1.0
		for ts := 0.0; ts < 300 && (on < 0 || off < 0); ts += 0.25 {
			if v.Movement.MovingAt(ts) {
				if on < 0 {
					on = ts
				}
			} else if off < 0 {
				off = ts
			}
		}
		a, err := w.Render(vcam.Request{UAV: "check", Pose: nadir(v, 12), TOffsetS: off, NoTherm: true})
		if err != nil {
			return Result{Status: Fail, Code: "E-CHK-106", Detail: err.Error()}
		}
		if v.Movement.Moves {
			b, err := w.Render(vcam.Request{UAV: "check", Pose: nadir(v, 12), TOffsetS: on, NoTherm: true})
			if err != nil {
				return Result{Status: Fail, Code: "E-CHK-106", Detail: err.Error()}
			}
			if same(mask(a, v.Index), mask(b, v.Index)) {
				ev = append(ev, fmt.Sprintf("%s should move (period %.1f s) but looks identical at t=%.2f and t=%.2f", v.ID, v.Movement.PeriodS, off, on))
			}
			moved++
		} else {
			b, err := w.Render(vcam.Request{UAV: "check", Pose: nadir(v, 12), TOffsetS: off + 13.7, NoTherm: true})
			if err != nil {
				return Result{Status: Fail, Code: "E-CHK-106", Detail: err.Error()}
			}
			if !same(mask(a, v.Index), mask(b, v.Index)) {
				ev = append(ev, fmt.Sprintf("%s (%s) should be still but changed", v.ID, v.Status))
			}
			still++
		}
	}
	if len(ev) > 0 {
		return Result{Status: Fail, Code: "E-CHK-106", Detail: fmt.Sprintf("%d problem(s)", len(ev)), Evidence: limit(ev, 10)}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d moving victims change pose, %d still victims do not", moved, still)}
}

func checkSpeed(w *vcam.World) Result {
	st := w.Scn.Sites[0]
	p := vcam.Pose{E: st.CenterE, N: st.CenterN, AltAGL: 40, YawDeg: 20, PitchDeg: -90}
	if _, err := w.Render(vcam.Request{UAV: "check", Pose: p}); err != nil { // warm-up (loads layers)
		return Result{Status: Fail, Code: "E-CHK-107", Detail: err.Error()}
	}
	var ms []float64
	for i := 0; i < 3; i++ {
		t0 := time.Now()
		if _, err := w.Render(vcam.Request{UAV: "check", Pose: p, TOffsetS: float64(i)}); err != nil {
			return Result{Status: Fail, Code: "E-CHK-107", Detail: err.Error()}
		}
		ms = append(ms, float64(time.Since(t0).Microseconds())/1000)
	}
	sort.Float64s(ms)
	med := ms[1]
	detail := fmt.Sprintf("RGB 1920x1080 + thermal 640x512 at 40 m: median %.0f ms over 3 frames (%d CPUs; pass <= %d ms)", med, runtime.NumCPU(), p1FramePassMS)
	switch {
	case med > p1FrameWarnMS:
		return Result{Status: Fail, Code: "E-CHK-107", Detail: detail, Evidence: []string{fmt.Sprintf("frames: %.0f / %.0f / %.0f ms", ms[0], ms[1], ms[2])}}
	case med > p1FramePassMS:
		return Result{Status: Warn, Code: "E-CHK-107", Detail: detail, Evidence: []string{"usable, but the simulated cameras will capture less than one frame every 2.5 s"}}
	}
	return Result{Status: Pass, Detail: detail}
}

func checkScorer(w *vcam.World) Result {
	var ms []*render.Manifest
	for _, st := range w.Scn.Sites {
		ms = append(ms, w.Manifest(st.ID))
	}
	truth := score.AllTruth(ms)
	if probs := score.SelfTest(w.Scn, truth); len(probs) > 0 {
		return Result{Status: Fail, Code: "E-CHK-108", Detail: fmt.Sprintf("%d problem(s)", len(probs)), Evidence: probs}
	}
	r := score.Score(w.Scn, truth, score.FromTruth(truth, score.DefaultOptions().MinTruthAreaM2), score.DefaultOptions())
	return Result{Status: Pass, Detail: fmt.Sprintf("constructed cases scored as expected; %d truth parts of %d victims scoreable (>= %.3f m2)",
		r.Overall.Truth, r.VictimsTotal, r.MinTruthAreaM2)}
}
