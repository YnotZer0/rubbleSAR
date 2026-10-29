package analyst

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

var centre = geo.Point{Lat: 36.2, Lon: 36.16}

func testConfig(dir string) Config {
	f := config.Fusion{LinkM: 2.0, SpanM: 2.5, DuplicateM: 1.5, Window: config.Duration{Duration: 10 * time.Second}, MatchMinM: 5, MatchCEPs: 3}
	tr := config.Triage{SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, ClothedMinC: 15, ClothedDeltaC: 8, CloseRangeM: 20, MinWatchS: 20, MovedFrames: 2, CoolingCPerH: 0.5,
		TrendMinSpan: config.Duration{Duration: 10 * time.Minute}, MaxCEPm: 5, NotFoundLooks: 2}
	return Config{Incident: "inc", Dir: dir, AckInterval: 50 * time.Millisecond, Fusion: f, Triage: tr, Centre: centre,
		GroundAltMSL: 80, UpdateInterval: 50 * time.Millisecond}
}

type sink struct {
	mu    sync.Mutex
	cas   map[string]*sarpb.Casualty
	lists []*sarpb.CasualtyList
	acks  int
}

func listen(t *testing.T, m *bus.Memory) *sink {
	s := &sink{cas: map[string]*sarpb.Casualty{}}
	m.Subscribe(keys.CasualtyAll("inc"), func(x bus.Sample) {
		var c sarpb.Casualty
		if msg.Unmarshal(x.Payload, &c) == nil {
			s.mu.Lock()
			s.cas[c.CasualtyId] = &c
			s.mu.Unlock()
		}
	})
	m.Subscribe(keys.Casualties("inc"), func(x bus.Sample) {
		var l sarpb.CasualtyList
		if msg.Unmarshal(x.Payload, &l) == nil {
			s.mu.Lock()
			s.lists = append(s.lists, &l)
			s.mu.Unlock()
		}
	})
	m.Subscribe(keys.AckAll("inc"), func(x bus.Sample) {
		s.mu.Lock()
		s.acks++
		s.mu.Unlock()
	})
	return s
}

func (s *sink) last() *sarpb.CasualtyList {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lists) == 0 {
		return nil
	}
	return s.lists[len(s.lists)-1]
}

var seq msg.Seq

// report publishes one detection from a drone: a part at (e, n) local metres.
func report(t *testing.T, m *bus.Memory, uav, track string, rev uint32, part sarpb.PartClass, e, n float64, skin float32, at time.Time, task string, moved bool) {
	t.Helper()
	p := geo.NewLocal(centre).ToPoint(e, n)
	d := &sarpb.Detection{Header: msg.NewHeader("inc", "uav/"+uav, seq.Next()), UavId: uav, DetectionId: track + "." + itoa(rev), TrackId: track,
		Revision: rev, Reason: sarpb.ReportReason_REPORT_REASON_NEW, Part: part, Confidence: 0.8, FusedConfidence: 0.8,
		Position: &sarpb.GeoPoint{LatDeg: p.Lat, LonDeg: p.Lon}, CepM: 2.4, Observations: 10,
		FirstSeenUnixNs: at.UnixNano(), LastSeenUnixNs: at.Add(5 * time.Second).UnixNano(), CaptureUnixNs: at.UnixNano(),
		Thermal: &sarpb.ThermalReading{Readable: true, SkinC: skin, AmbientC: 6, DeltaC: skin - 6, Pixels: 40, Warm: skin > 20}, RangeM: 41}
	if task != "" {
		d.Reason, d.TaskId, d.RangeM, d.MinRangeM = sarpb.ReportReason_REPORT_REASON_INVESTIGATED, task, 15, 15
		d.LastSeenUnixNs = at.Add(30 * time.Second).UnixNano()
		d.Movement = &sarpb.Movement{HoverFrames: 30, MovedFrames: 0, Moved: moved, WatchedS: 30}
		if moved {
			d.Movement.MovedFrames = 8
		}
	}
	b, _ := msg.Marshal(d)
	if err := m.Put(keys.Detection("inc", uav), b, bus.QoSDetection); err != nil {
		t.Fatal(err)
	}
}

func itoa(v uint32) string {
	s := ""
	for {
		s = string(rune('0'+v%10)) + s
		v /= 10
		if v == 0 {
			return s
		}
	}
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func start(t *testing.T, m *bus.Memory, dir string) (*Analyst, func()) {
	a, err := Open(testConfig(dir), m, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx, 200*time.Millisecond); close(done) }()
	return a, func() { cancel(); <-done; a.Close() }
}

// Reports in, casualties out: two victims seen by the scout become two
// provisional casualties; an investigation of one (task list DONE, the
// investigator's reports with movement) settles it; a restart keeps the ids;
// a reset archives the run and empties the list.
func TestAnalystService(t *testing.T) {
	m := bus.NewMemory()
	s := listen(t, m)
	dir := t.TempDir()
	a, stop := start(t, m, dir)
	t0 := time.Now().Add(-10 * time.Minute)
	report(t, m, "scout-1", "s-1", 1, sarpb.PartClass_PART_CLASS_HAND, 0, 0, 27, t0, "", false)
	report(t, m, "scout-1", "s-2", 1, sarpb.PartClass_PART_CLASS_HEAD, 12, 0, 8, t0, "", false)
	waitFor(t, "two casualties", 5*time.Second, func() bool { l := s.last(); return l != nil && len(l.Casualties) == 2 })
	l := s.last()
	if c := l.Casualties[0]; c.CasualtyId != "C-001" || c.Triage != sarpb.Triage_TRIAGE_RED || !c.Provisional || !c.NeedsInvestigation {
		t.Fatalf("first casualty %v", c)
	}
	if c := l.Casualties[1]; c.Triage != sarpb.Triage_TRIAGE_GREY || !c.NeedsInvestigation {
		t.Fatalf("second casualty %v", c)
	}
	waitFor(t, "acks", 2*time.Second, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.acks > 0 })

	// mission-planner sends inv-1 to C-001; it sees the hand move.
	pub, _ := m.DeclarePublisher(keys.Tasks("inc"), bus.QoSCasualty)
	target := l.Casualties[0].Position
	task := &sarpb.Task{TaskId: "T-0001", CasualtyId: "C-001", State: sarpb.TaskState_TASK_STATE_ON_STATION, Target: target, UavId: "inv-1"}
	b, _ := msg.Marshal(&sarpb.TaskList{Header: msg.NewHeader("inc", "zbook/mission-planner", 1), Tasks: []*sarpb.Task{task}})
	_ = pub.Put(b)
	report(t, m, "inv-1", "i-1", 1, sarpb.PartClass_PART_CLASS_HAND, 0.8, -0.5, 28, t0.Add(2*time.Minute), "T-0001", true)
	task.State, task.FinishedUnixNs = sarpb.TaskState_TASK_STATE_DONE, t0.Add(3*time.Minute).UnixNano()
	task.Result = &sarpb.TaskResult{Frames: 30, TrackIds: []string{"i-1"}, Found: true, Moved: true}
	b, _ = msg.Marshal(&sarpb.TaskList{Header: msg.NewHeader("inc", "zbook/mission-planner", 2), Tasks: []*sarpb.Task{task}})
	_ = pub.Put(b)
	waitFor(t, "C-001 settled Yellow", 5*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		c := s.cas["C-001"]
		return c != nil && c.Triage == sarpb.Triage_TRIAGE_YELLOW && c.Investigations == 1 && len(c.UavIds) == 2
	})
	if l := s.last(); len(l.Casualties) != 2 {
		t.Fatalf("after the investigation: %d casualties", len(l.Casualties))
	}
	st := a.Stats()
	if st.Count != 2 || st.ByTriage["Yellow"] != 1 || st.TaskLists < 2 || !strings.Contains(st.Text(), "C-001  Yellow") {
		t.Fatalf("stats %+v\n%s", st, st.Text())
	}
	stop()

	// Restart: rebuilt from detections.jsonl and the state file, same ids.
	a, stop = start(t, m, dir)
	waitFor(t, "rebuilt list", 5*time.Second, func() bool {
		cs := a.Casualties()
		return len(cs) == 2 && cs[0].CasualtyId == "C-001" && cs[0].Triage == sarpb.Triage_TRIAGE_YELLOW && cs[1].CasualtyId == "C-002"
	})

	// Reset: archived, empty.
	dst, err := a.Reset("run1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "detections.jsonl")); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "casualties-state.json")); err != nil {
		t.Fatalf("archive: %v", err)
	}
	waitFor(t, "empty list", 5*time.Second, func() bool { l := s.last(); return l != nil && len(l.Casualties) == 0 })
	report(t, m, "scout-1", "s-9", 1, sarpb.PartClass_PART_CLASS_LEG, 30, 0, 27, time.Now(), "", false)
	waitFor(t, "a fresh C-001", 5*time.Second, func() bool {
		cs := a.Casualties()
		return len(cs) == 1 && cs[0].CasualtyId == "C-001" && cs[0].Parts[0].GetTrackId() == "s-9"
	})
	stop()
}
