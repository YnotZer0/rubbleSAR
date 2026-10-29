package monitor

import (
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/geo"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

func sample(t *testing.T, inc, key, uav string, seq uint64, at time.Time) bus.Sample {
	t.Helper()
	h := msg.NewHeader(inc, "uav/"+uav, seq)
	h.SentUnixNs = at.Add(-20 * time.Millisecond).UnixNano()
	b, err := msg.Marshal(&sarpb.Telemetry{Header: h, UavId: uav,
		Position: &sarpb.GeoPoint{LatDeg: 36.2025, LonDeg: 36.16056}})
	if err != nil {
		t.Fatal(err)
	}
	return bus.Sample{Key: key, Payload: b, Received: at}
}

func TestTrackerCountsGapsAndRate(t *testing.T) {
	tr := NewTracker("inc", geo.Point{Lat: 36.2025, Lon: 36.16056})
	t0 := time.Now()
	key := "sar/inc/uav/a/telemetry"
	for i, seq := range []uint64{1, 2, 3, 5, 6, 6, 7, 1, 2} { // gap of 1, a duplicate, a restart
		tr.Handle(sample(t, "inc", key, "a", seq, t0.Add(time.Duration(i)*time.Second)))
	}
	s := tr.Snapshot()
	u, ok := s.Get("a")
	if !ok {
		t.Fatal("drone a not tracked")
	}
	if u.Count != 9 || u.Gaps != 1 || u.OutOfOrder != 1 || u.Restarts != 1 || u.LastSeq != 2 {
		t.Fatalf("stats wrong: count=%d gaps=%d ooo=%d restarts=%d last=%d", u.Count, u.Gaps, u.OutOfOrder, u.Restarts, u.LastSeq)
	}
	if r := u.RateHz(t0.Add(8*time.Second+time.Millisecond), 5*time.Second); r != 1 {
		t.Fatalf("rate = %v want 1", r)
	}
	if l := u.MedianLatencyMs(); l < 19 || l > 21 {
		t.Fatalf("median latency %v want ~20 ms", l)
	}
	if d := tr.DistanceFromCenter(u); d < 0 || d > 0.01 {
		t.Fatalf("distance %v", d)
	}
}

func TestTrackerRejects(t *testing.T) {
	tr := NewTracker("inc", geo.Point{})
	now := time.Now()
	tr.Handle(bus.Sample{Key: "sar/inc/uav/a/telemetry", Payload: []byte{0xff, 0xff}, Received: now})
	tr.Handle(sample(t, "other", "sar/inc/uav/a/telemetry", "a", 1, now))
	tr.Handle(sample(t, "inc", "sar/inc/uav/b/telemetry", "a", 1, now))
	s := tr.Snapshot()
	if s.DecodeErr != 1 || s.Mismatch != 2 || len(s.UAVs) != 0 || s.LastErr == nil {
		t.Fatalf("snapshot %+v", s)
	}
	tr.Reset()
	if s := tr.Snapshot(); s.DecodeErr != 0 || s.Mismatch != 0 {
		t.Fatalf("reset failed: %+v", s)
	}
}

func TestBoard(t *testing.T) {
	b := NewBoard("inc")
	cl, _ := msg.Marshal(&sarpb.CasualtyList{Header: msg.NewHeader("inc", "zbook/casualty-analyst", 1), Casualties: []*sarpb.Casualty{{CasualtyId: "C-001"}}})
	b.HandleCasualties(bus.Sample{Key: "sar/inc/casualties", Payload: cl, Received: time.Now()})
	other, _ := msg.Marshal(&sarpb.TaskList{Header: msg.NewHeader("other", "zbook/mission-planner", 1), Tasks: []*sarpb.Task{{TaskId: "T-1"}}})
	b.HandleTasks(bus.Sample{Key: "sar/other/tasks", Payload: other, Received: time.Now()})
	cas, _, tasks, _ := b.Snapshot()
	if cas == nil || len(cas.Casualties) != 1 || tasks != nil {
		t.Fatalf("board: %v %v", cas, tasks)
	}
}
