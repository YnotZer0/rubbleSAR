package check

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"rubblesar/internal/analyst"
	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/edge"
	"rubblesar/internal/geo"
	"rubblesar/internal/intake"
	"rubblesar/internal/planner"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/vcam"
)

// p4World is the phase 4 flow running in one process: edge-eye (truth
// oracle) with a scout surveying one site at 40 m and two investigators on
// standby, casualty-analyst fusing and triaging, mission-planner tasking.
type p4World struct {
	s     *scenario.Scenario
	w     *vcam.World
	m     *bus.Memory
	an    *analyst.Analyst
	pl    *planner.Planner
	zc    *config.ZBook
	o     Phase4Options
	ctx   context.Context
	log   *slog.Logger
	dir   string
	anDir string
}

func startP4World(t *testing.T) *p4World {
	t.Helper()
	sc := scenario.DefaultConfig()
	sc.Scenario.ID = "inc"
	sc.Site.CenterLat, sc.Site.CenterLon = 36.2025, 36.16056
	// One site as dense as the demo scenario's (victims about 5 m apart).
	sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 24, 26, 40
	sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 6, 6
	sc.Victims.AliveFraction, sc.Victims.ResponsiveFraction = 0.7, 0.5
	sc.Scenario.Seed = 21 // two responsive (moving) victims, two unresponsive, one deteriorating, one deceased
	sc.Render.GSDm = 0.03
	s, err := scenario.Generate(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s.Save(dir)
	if _, err := render.RenderAll(dir, s, nil); err != nil {
		t.Fatal(err)
	}
	w, err := vcam.Open(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range s.Victims {
		t.Logf("%s at %.1f,%.1f %s expected %s movement %+v exposed %d", v.ID, v.E, v.N, v.Status, v.ExpectedTriage, v.Movement, len(v.Exposed))
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("P4_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	site := config.Site{IncidentID: "inc", CenterLat: 36.2025, CenterLon: 36.16056, GroundAltMSLm: 80, RadiusM: 805, ScenarioDir: dir}
	ec := &config.Edge{}
	ec.Site = site
	ec.Seed = 1
	ec.Perception = config.Perception{Enabled: true, Detector: "truth-oracle", Conf: 0.25, FrameHz: 2, SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3,
		MinThermalPx: 9, WarmSpots: true, WarmSpotMinPx: 3, ConfirmHits: 2, UpdateMinInterval: config.Duration{Duration: 5 * time.Second},
		PublishTruth: true, SimStartS: 600, CameraPitchDeg: -90, DetectionSigmaM: 1, DebrisHeightM: 2.5, MovementChange: 0.12, MovementTolerancePx: 2, MovementMinFrames: 2}
	ec.Outbox = config.Outbox{Dir: t.TempDir(), MaxQueueMB: 50, AckTimeout: config.Duration{Duration: 2 * time.Second},
		MaxBackoff: config.Duration{Duration: 8 * time.Second}, ProbeAfter: config.Duration{Duration: 4 * time.Second}}
	b := uint32(256)
	st := s.Sites[0]
	inv := func(id string, de float64) config.UAV {
		return config.UAV{ID: id, Role: "investigator", Behaviour: "standby", AltAGLm: 15, SpeedMps: 10, HomeOffsetEastM: st.CenterE + de,
			HomeOffsetNorthM: st.CenterN - 25, TelemetryHz: 2, EnduranceMin: 30, GPSNoiseM: 1.5, YawNoiseDeg: 1, BudgetKbps: &b, TaskLoiterS: 5, HoverWanderM: 0.15}
	}
	ec.UAVs = []config.UAV{{ID: "scout-1", Role: "scout", Behaviour: "survey", AltAGLm: 40, SpeedMps: 8, SurveySpeedMps: 4, SideOverlap: 0.7,
		HomeOffsetEastM: st.CenterE, HomeOffsetNorthM: st.CenterN - 30, TelemetryHz: 2, EnduranceMin: 30, GPSNoiseM: 1.5, YawNoiseDeg: 1, BudgetKbps: &b},
		inv("inv-1", -20), inv("inv-2", 20)}

	m := bus.NewMemory()
	t.Cleanup(func() { m.Close() })
	rt, err := edge.New(ec, m, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = rt.Run(ctx) }()
	t.Cleanup(rt.Close)

	fu := config.Fusion{LinkM: 2.0, SpanM: 2.5, DuplicateM: 1.5, Window: config.Duration{Duration: 10 * time.Second}, MatchMinM: 5, MatchCEPs: 3}
	tr := config.Triage{SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, ClothedMinC: 15, ClothedDeltaC: 8, CloseRangeM: 20, MinWatchS: 20, MovedFrames: 2, CoolingCPerH: 0.5,
		TrendMinSpan: config.Duration{Duration: 10 * time.Minute}, MaxCEPm: 5, NotFoundLooks: 2}
	centre := geo.Point{Lat: site.CenterLat, Lon: site.CenterLon}
	anDir := t.TempDir()
	an, err := analyst.Open(analyst.Config{Incident: "inc", Dir: anDir, AckInterval: 100 * time.Millisecond, Fusion: fu, Triage: tr, SaveCrops: true,
		Centre: centre, GroundAltMSL: 80}, m, log)
	if err != nil {
		t.Fatal(err)
	}
	go an.Run(ctx, 2*time.Second)
	t.Cleanup(an.Close)
	pl, err := planner.Open(planner.Config{Incident: "inc", Centre: centre, Settle: 5 * time.Second,
		P: config.Planner{AltAGLm: 15, HoverS: 30, RadiusM: 6, ReservePct: 25, MaxAttempts: 2, TaskTimeout: config.Duration{Duration: 2 * time.Minute},
			CommandTimeout: config.Duration{Duration: 3 * time.Second}, PublishInterval: config.Duration{Duration: 2 * time.Second}}}, m, log)
	if err != nil {
		t.Fatal(err)
	}
	go pl.Run(ctx)
	t.Cleanup(pl.Close)

	zc := &config.ZBook{}
	zc.Site = site
	zc.Check = config.Check{ExpectUAVs: []string{"scout-1", "inv-1", "inv-2"}, DiscoverTimeout: config.Duration{Duration: 15 * time.Second},
		PingTimeout: config.Duration{Duration: 2 * time.Second}}
	stored := func() (map[string]uint64, error) {
		out := map[string]uint64{}
		for id, u := range an.Intake().Stats().UAVs {
			out[id] = u.Stored
		}
		return out, nil
	}
	o := Phase4Options{Cfg: zc, Bus: m, World: w, Services: func() error { return nil }, AnalystStored: stored,
		Reset: func(stamp string) error {
			if _, err := an.Reset(stamp); err != nil {
				return err
			}
			pl.Reset()
			return nil
		},
		MaxRun: 7 * time.Minute, Settle: 12 * time.Second, AllowOracle: true, Progress: os.Stderr, ProgressEvery: 20 * time.Second}
	return &p4World{s: s, w: w, m: m, an: an, pl: pl, zc: zc, o: o, ctx: ctx, log: log, dir: dir, anDir: anDir}
}

// The whole phase 4 flow in one process, scored by the acceptance suite.
func TestPhase4InProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("about 4 minutes")
	}
	pw := startP4World(t)
	s, w, an, pl, o, ctx, anDir := pw.s, pw.w, pw.an, pw.pl, pw.o, pw.ctx, pw.anDir
	_ = s
	var out bytes.Buffer
	r := Phase4(ctx, o, &out)
	txt := out.String()
	t.Log("\n" + txt)
	t.Log("\n" + an.Stats().Text())
	t.Log("\n" + pl.Status().Text())
	if os.Getenv("P4_DEBUG") != "" {
		loc := w.Scn.Local()
		for _, tp := range truthParts(w) {
			t.Logf("truth %s %-6s %6.1f %6.1f", tp.VictimID, tp.ClassName, tp.E, tp.N)
		}
		recs, _ := intake.ReadRecords(anDir)
		last := map[string]intake.Record{}
		var order []string
		for _, r := range recs {
			k := r.UAV + "/" + r.TrackID
			if _, ok := last[k]; !ok {
				order = append(order, k)
			}
			last[k] = r
			if r.Movement != nil {
				t.Logf("mv %s %s %-6s %s hover %d moved %d max %.3f watched %.0f", r.UAV, r.TrackID, r.Part, r.TaskID, r.Movement.HoverFrames, r.Movement.MovedFrames, r.Movement.MaxChange, r.Movement.WatchedS)
			}
		}
		for _, k := range order {
			r := last[k]
			e, n := loc.ToLocal(geo.Point{Lat: r.Lat, Lon: r.Lon})
			best, be, bn, bv := 1e9, 0.0, 0.0, ""
			for _, tp := range truthParts(w) {
				if strings.ReplaceAll(tp.ClassName, "_", " ") != r.Part && r.Part != "warm spot" {
					continue
				}
				if d := math.Hypot(tp.E-e, tp.N-n); d < best {
					best, be, bn, bv = d, e-tp.E, n-tp.N, tp.VictimID
				}
			}
			t.Logf("err %-7s %-22s %-9s %-10s range %4.1f min %4.1f cep %.1f -> %s %.1f m (%+.1f,%+.1f) first %s | thermal %v %.1f/%.1f d%.1f px %d warm %v %s", r.UAV, r.TrackID, r.Part, r.TaskID, r.RangeM, r.MinRangeM, r.CEPm, bv, best, be, bn,
				time.Unix(0, r.FirstSeenNs).Format("15:04:05"), r.Thermal.Readable, r.Thermal.SkinC, r.Thermal.AmbientC, r.Thermal.DeltaC, r.Thermal.Pixels, r.Thermal.Warm, r.Thermal.Reason)
		}
		for _, c := range an.Casualties() {
			ce, cn := loc.ToLocal(geoOf(c.Position))
			t.Logf("%s %s at %.1f,%.1f cep %.1f thermal %+v | %s", c.CasualtyId, c.Triage, ce, cn, c.CepM, c.Thermal, c.Summary)
			for _, p := range c.Parts {
				pe, pn := loc.ToLocal(geoOf(p.Position))
				t.Logf("    %s %-10s %-22s %6.1f %6.1f cep %.1f range %.0f", p.UavId, p.TrackId, p.Part, pe, pn, p.CepM, p.MinRangeM)
			}
		}
	}
	// The mechanics must pass; the quality checks (records per victim,
	// positions, triage, movement) only must not fail: on 6 victims one
	// duplicate record is already 17 % (the fusion quality itself is measured
	// on hundreds of sites by internal/casualty TestFusionQuality).
	for _, want := range []string{"[WARN] P4-01", "[PASS] P4-04", "[PASS] P4-05", "[PASS] P4-08"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !r.Passed() {
		t.Fatal("phase 4 in-process run failed")
	}
}
