package check

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/edge"
	"rubblesar/internal/intake"
	"rubblesar/internal/render"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
	"rubblesar/internal/vcam"
)

// The whole phase 3 run in one process: edge-eye (truth oracle) flying a
// survey over a small scenario, the intake acknowledging, and the acceptance
// suite driving an outage and a budget cut on a compressed timeline.
func TestPhase3InProcess(t *testing.T) {
	sc := scenario.DefaultConfig()
	sc.Scenario.ID = "inc"
	sc.Site.CenterLat, sc.Site.CenterLon = 36.2025, 36.16056
	sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 12, 14, 40
	sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 4, 4
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ec := &config.Edge{}
	ec.Site = config.Site{IncidentID: "inc", CenterLat: 36.2025, CenterLon: 36.16056, GroundAltMSLm: 80, RadiusM: 805, ScenarioDir: dir}
	ec.Seed = 1
	ec.Perception = config.Perception{Enabled: true, Detector: "truth-oracle", Conf: 0.25, FrameHz: 2, SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3,
		MinThermalPx: 9, WarmSpots: true, WarmSpotMinPx: 3, ConfirmHits: 2, UpdateMinInterval: config.Duration{Duration: 5 * time.Second},
		PublishTruth: true, SimStartS: 600, CameraPitchDeg: -90}
	ec.Outbox = config.Outbox{Dir: t.TempDir(), MaxQueueMB: 50, AckTimeout: config.Duration{Duration: 2 * time.Second},
		MaxBackoff: config.Duration{Duration: 8 * time.Second}, ProbeAfter: config.Duration{Duration: 4 * time.Second}}
	b := uint32(256)
	site := s.Sites[0]
	ec.UAVs = []config.UAV{{ID: "scout-1", Role: "scout", Behaviour: "survey", AltAGLm: 15, SpeedMps: 6, SurveySpeedMps: 3, SideOverlap: 0.7,
		HomeOffsetEastM: site.CenterE, HomeOffsetNorthM: site.CenterN - 15, TelemetryHz: 2, EnduranceMin: 30, GPSNoiseM: 1.5, YawNoiseDeg: 1, BudgetKbps: &b}}

	m := bus.NewMemory()
	defer m.Close()
	rt, err := edge.New(ec, m, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = rt.Run(ctx) }()
	defer rt.Close()
	in, err := intake.Open(intake.Config{Incident: "inc", Dir: t.TempDir(), AckInterval: 100 * time.Millisecond, SaveCrops: true}, m, log)
	if err != nil {
		t.Fatal(err)
	}
	go in.Run(ctx)
	defer in.Close()

	zc := &config.ZBook{}
	zc.Site = ec.Site
	zc.Check = config.Check{ExpectUAVs: []string{"scout-1"}, DiscoverTimeout: config.Duration{Duration: 15 * time.Second}, PingTimeout: config.Duration{Duration: 2 * time.Second}}
	stored := func() (map[string]uint64, error) {
		out := map[string]uint64{}
		for id, u := range in.Stats().UAVs {
			out[id] = u.Stored
		}
		return out, nil
	}
	o := Phase3Options{Cfg: zc, Bus: m, World: w, Analyst: func() error { return nil }, AnalystStored: stored, OutageAt: 22 * time.Second, Outage: 6 * time.Second,
		LowAt: 32 * time.Second, LowFor: 8 * time.Second, LowBps: 16000, MaxRun: 80 * time.Second, Drain: 20 * time.Second,
		AllowOracle: true, Progress: os.Stderr, ProgressEvery: 10 * time.Second}
	var out bytes.Buffer
	r := Phase3(ctx, o, &out)
	txt := out.String()
	t.Log("\n" + txt)
	for _, want := range []string{"[WARN] P3-01", "[PASS] P3-02", "[PASS] P3-03", "[PASS] P3-04", "[PASS] P3-06", "[PASS] P3-07", "casualty-analyst stored"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !r.Passed() {
		t.Fatal("phase 3 in-process run failed")
	}
}

// P3-06 fails when reports arrived but casualty-analyst did not store them
// (for example dropped as repeats after edge-eye restarted).
func TestP3OutageNotStored(t *testing.T) {
	col := newCollector("inc")
	start := time.Now()
	for i, id := range []string{"inv-1-x-0001.1", "inv-1-x-0002.1", "inv-1-x-0003.1"} {
		col.dets[id] = &detRec{d: &sarpb.Detection{UavId: "inv-1", DetectionId: id}, at: start.Add(time.Duration(i+1) * time.Second)}
		col.order = append(col.order, id)
		if col.seqs["inv-1"] == nil {
			col.seqs["inv-1"] = map[uint64]bool{}
		}
		col.seqs["inv-1"][uint64(i+1)] = true
	}
	zc := &config.ZBook{}
	zc.Check.ExpectUAVs = []string{"inv-1"}
	run := &p3run{start: start, outageStart: start, outageEnd: start, createdBefore: map[string]uint64{},
		storedBefore: map[string]uint64{"inv-1": 5}, storedAfter: map[string]uint64{"inv-1": 6}}
	r := p3Outage(col, run, Phase3Options{Cfg: zc})
	if r.Status != Fail || !strings.Contains(r.Detail, "2 reports arrived but casualty-analyst did not store them") {
		t.Fatalf("%v %s %v", r.Status, r.Detail, r.Evidence)
	}
	run.storedAfter["inv-1"] = 8
	if r := p3Outage(col, run, Phase3Options{Cfg: zc}); r.Status != Pass {
		t.Fatalf("all stored: %v %s %v", r.Status, r.Detail, r.Evidence)
	}
}
