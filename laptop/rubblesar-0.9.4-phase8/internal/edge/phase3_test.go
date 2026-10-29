package edge

import (
	"bytes"
	"context"
	"image/jpeg"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/render"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
)

var (
	scnOnce sync.Once
	scnDir  string
	scnErr  error
	scnObj  *scenario.Scenario
)

// testScenario renders a one-site scenario once (shared by the phase 3 tests).
func testScenario(t *testing.T) (string, *scenario.Scenario) {
	t.Helper()
	scnOnce.Do(func() {
		sc := scenario.DefaultConfig()
		sc.Scenario.ID = "inc"
		sc.Site.CenterLat, sc.Site.CenterLon = 36.2025, 36.16056
		sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 12, 14, 40
		sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 4, 4
		sc.Render.GSDm = 0.03
		s, err := scenario.Generate(sc, nil)
		if err != nil {
			scnErr = err
			return
		}
		scnDir, scnErr = os.MkdirTemp("", "edge-p3-")
		if scnErr != nil {
			return
		}
		s.Save(scnDir)
		_, scnErr = render.RenderAll(scnDir, s, nil)
		scnObj = s
	})
	if scnErr != nil {
		t.Fatal(scnErr)
	}
	return scnDir, scnObj
}

func p3Config(t *testing.T, detector string) *config.Edge {
	dir, s := testScenario(t)
	c := testConfig()
	c.Site.ScenarioDir = dir
	c.Perception = config.Perception{Enabled: true, ModelDir: "/nonexistent-model", Detector: detector, Conf: 0.25, FrameHz: 2,
		SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, MinThermalPx: 9, WarmSpots: true, WarmSpotMinPx: 3, ConfirmHits: 2,
		UpdateMinInterval: config.Duration{Duration: 5 * time.Second}, PublishTruth: true, SimStartS: 600, CameraPitchDeg: -90}
	c.Outbox = config.Outbox{Dir: t.TempDir(), MaxQueueMB: 50, AckTimeout: config.Duration{Duration: 2 * time.Second},
		MaxBackoff: config.Duration{Duration: 10 * time.Second}, ProbeAfter: config.Duration{Duration: 5 * time.Second}}
	site := s.Sites[0]
	b := uint32(256)
	c.UAVs = []config.UAV{{ID: "scout-1", Role: "scout", Behaviour: "survey", AltAGLm: 15, SpeedMps: 6, SurveySpeedMps: 3, SideOverlap: 0.7,
		HomeOffsetEastM: site.CenterE, HomeOffsetNorthM: site.CenterN - 15, TelemetryHz: 2, EnduranceMin: 30, GPSNoiseM: 1.5, YawNoiseDeg: 1, BudgetKbps: &b}}
	return c
}

// intake is a minimal ZBook side: stores detections and acknowledges them.
type intake struct {
	mu     sync.Mutex
	got    map[string]*sarpb.Detection
	dupes  int
	truths int
	tel    []*sarpb.Telemetry
}

func startIntake(t *testing.T, m *bus.Memory) *intake {
	in := &intake{got: map[string]*sarpb.Detection{}}
	ackPub, _ := m.DeclarePublisher(keys.Ack("inc", "scout-1"), bus.QoSAck)
	var seq msg.Seq
	m.Subscribe(keys.DetectionAll("inc"), func(s bus.Sample) {
		var d sarpb.Detection
		if err := msg.Unmarshal(s.Payload, &d); err != nil {
			t.Errorf("bad detection: %v", err)
			return
		}
		in.mu.Lock()
		if _, ok := in.got[d.DetectionId]; ok {
			in.dupes++
		}
		in.got[d.DetectionId] = &d
		in.mu.Unlock()
		b, _ := msg.Marshal(&sarpb.DetectionAck{Header: msg.NewHeader("inc", "zbook/test", seq.Next()), UavId: d.UavId, DetectionIds: []string{d.DetectionId}})
		_ = ackPub.Put(b)
	})
	m.Subscribe(keys.SimTruthAll("inc"), func(bus.Sample) { in.mu.Lock(); in.truths++; in.mu.Unlock() })
	m.Subscribe(keys.TelemetryAll("inc"), func(s bus.Sample) {
		var tm sarpb.Telemetry
		if msg.Unmarshal(s.Payload, &tm) == nil {
			in.mu.Lock()
			in.tel = append(in.tel, &tm)
			in.mu.Unlock()
		}
	})
	return in
}

func (in *intake) count() int { in.mu.Lock(); defer in.mu.Unlock(); return len(in.got) }

func runP3(t *testing.T, c *config.Edge) (*Runtime, *bus.Memory, *intake) {
	t.Helper()
	m := bus.NewMemory()
	in := startIntake(t, m)
	r, err := New(c, m, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); time.Sleep(200 * time.Millisecond); r.Close(); m.Close() })
	return r, m, in
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The scout takes off over the site, its frames go through the pipeline, the
// reports reach the intake, are acknowledged and leave the queue; telemetry
// carries the perception figures and the truth channel runs.
func TestPhase3EndToEndWithOracle(t *testing.T) {
	r, m, in := runP3(t, p3Config(t, "truth-oracle"))
	waitFor(t, "detections", 60*time.Second, func() bool { return in.count() >= 3 })
	d := r.Drones()[0]
	waitFor(t, "acknowledged queue", 30*time.Second, func() bool {
		st := d.perc.ob.Stats()
		return st.Acked >= 3 && st.Unacked == 0
	})
	in.mu.Lock()
	for id, det := range in.got {
		if det.Header.GetSeq() == 0 || det.TrackId == "" || det.Summary == "" || det.Position == nil || det.CepM <= 0 {
			t.Errorf("%s: incomplete %+v", id, det)
		}
		// Ids carry this start's run token, so they never repeat after a restart.
		if want := det.UavId + "-" + r.runToken + "-"; !strings.HasPrefix(det.TrackId, want) || !strings.HasPrefix(id, det.TrackId+".") {
			t.Errorf("track id %q / detection id %q: want %s<n> and <track>.<revision>", det.TrackId, id, want)
		}
		if det.VisibleCrop != nil {
			if _, err := jpeg.Decode(bytes.NewReader(det.VisibleCrop.Data)); err != nil {
				t.Errorf("%s: crop %v", id, err)
			}
		}
	}
	truths := in.truths
	var ps *sarpb.PerceptionStats
	for _, tm := range in.tel {
		if tm.Perception != nil && tm.Perception.Frames > 0 {
			ps = tm.Perception
		}
	}
	in.mu.Unlock()
	if truths == 0 {
		t.Fatal("no truth messages")
	}
	if ps == nil || !ps.Enabled || ps.Backend != OracleName || ps.MissionSteps == 0 || ps.FrameHzTarget != 2 {
		t.Fatalf("perception stats %+v", ps)
	}
	t.Logf("%d reports, perception %+v", in.count(), ps)

	// A 4 s simulated outage: nothing leaves the drone, then traffic resumes.
	ctl, _ := m.DeclarePublisher(keys.Control("inc", "scout-1"), bus.QoSControl)
	b, _ := msg.Marshal(&sarpb.Control{Header: msg.NewHeader("inc", "test", 1), UavId: "scout-1",
		Cmd: &sarpb.Control_SetLinkOutage{SetLinkOutage: &sarpb.SetLinkOutage{Seconds: 4}}})
	ctl.Put(b)
	time.Sleep(500 * time.Millisecond)
	in.mu.Lock()
	nTel := len(in.tel)
	in.mu.Unlock()
	if !d.perc.ob.Stats().Outage {
		t.Fatal("outage not started")
	}
	time.Sleep(3 * time.Second)
	in.mu.Lock()
	during := len(in.tel) - nTel
	in.mu.Unlock()
	if during > 0 {
		t.Fatalf("%d telemetry messages during the outage", during)
	}
	waitFor(t, "telemetry after the outage", 10*time.Second, func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return len(in.tel) > nTel
	})

	// Restart the mission with cleared tracks: new reports come with new track ids.
	b, _ = msg.Marshal(&sarpb.Control{Header: msg.NewHeader("inc", "test", 2), UavId: "scout-1",
		Cmd: &sarpb.Control_RestartMission{RestartMission: &sarpb.RestartMission{SimTimeS: 600, ClearTracks: true}}})
	ctl.Put(b)
	waitFor(t, "restart", 5*time.Second, func() bool {
		_, step, _, _ := d.sim.MissionStatus()
		return step <= 1
	})
}

// A model that cannot be loaded does not stop the drone: it keeps flying and
// says why perception is not running.
func TestPhase3MissingModelKeepsFlying(t *testing.T) {
	r, _, in := runP3(t, p3Config(t, "model"))
	waitFor(t, "telemetry", 10*time.Second, func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return len(in.tel) > 2
	})
	in.mu.Lock()
	last := in.tel[len(in.tel)-1]
	in.mu.Unlock()
	if last.Perception == nil || last.Perception.Enabled || last.Perception.Backend == "" || last.Perception.Backend[:11] != "not running" {
		t.Fatalf("perception %+v", last.Perception)
	}
	if st := r.Status().Drones[0]; st.Mission == "" || st.MissionSteps == 0 {
		t.Fatalf("survey mission missing: %+v", st)
	}
}

func TestPhase3UnknownSurveySite(t *testing.T) {
	c := p3Config(t, "truth-oracle")
	c.UAVs[0].SurveySites = []string{"s99"}
	if _, err := New(c, bus.NewMemory(), quietLog()); err == nil {
		t.Fatal("unknown survey site accepted")
	}
}

// Two starts of edge-eye never share a run token (track ids would repeat).
func TestRunTokenDiffersAcrossStarts(t *testing.T) {
	cfg := p3Config(t, "truth-oracle")
	m := bus.NewMemory()
	defer m.Close()
	a, err := New(cfg, m, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	time.Sleep(3 * time.Millisecond)
	b, err := New(cfg, m, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if a.runToken == "" || a.runToken == b.runToken {
		t.Fatalf("run tokens %q and %q", a.runToken, b.runToken)
	}
}
