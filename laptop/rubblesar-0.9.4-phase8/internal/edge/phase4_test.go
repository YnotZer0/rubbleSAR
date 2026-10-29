package edge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// p4 is a small ZBook side for the phase 4 edge tests: stores and acknowledges
// detections of every drone, keeps telemetry and control replies.
type p4 struct {
	mu      sync.Mutex
	dets    map[string]*sarpb.Detection
	tel     []*sarpb.Telemetry
	replies map[uint64]*sarpb.ControlReply
	ctl     bus.Publisher
	seq     msg.Seq
}

func startP4(t *testing.T, m *bus.Memory, uav string) *p4 {
	z := &p4{dets: map[string]*sarpb.Detection{}, replies: map[uint64]*sarpb.ControlReply{}}
	ack, _ := m.DeclarePublisher(keys.Ack("inc", uav), bus.QoSAck)
	z.ctl, _ = m.DeclarePublisher(keys.Control("inc", uav), bus.QoSControl)
	var aseq msg.Seq
	m.Subscribe(keys.DetectionAll("inc"), func(s bus.Sample) {
		var d sarpb.Detection
		if msg.Unmarshal(s.Payload, &d) != nil {
			return
		}
		z.mu.Lock()
		z.dets[d.DetectionId] = &d
		z.mu.Unlock()
		b, _ := msg.Marshal(&sarpb.DetectionAck{Header: msg.NewHeader("inc", "zbook/test", aseq.Next()), UavId: d.UavId, DetectionIds: []string{d.DetectionId}})
		_ = ack.Put(b)
	})
	m.Subscribe(keys.TelemetryAll("inc"), func(s bus.Sample) {
		var tm sarpb.Telemetry
		if msg.Unmarshal(s.Payload, &tm) == nil {
			z.mu.Lock()
			z.tel = append(z.tel, &tm)
			z.mu.Unlock()
		}
	})
	m.Subscribe(keys.ControlReplyAll("inc"), func(s bus.Sample) {
		var r sarpb.ControlReply
		if msg.Unmarshal(s.Payload, &r) == nil {
			z.mu.Lock()
			z.replies[r.InReplyToSeq] = &r
			z.mu.Unlock()
		}
	})
	return z
}

func (z *p4) send(t *testing.T, uav string, set func(*sarpb.Control)) *sarpb.ControlReply {
	t.Helper()
	c := &sarpb.Control{Header: msg.NewHeader("inc", "zbook/test", z.seq.Next()), UavId: uav}
	set(c)
	b, _ := msg.Marshal(c)
	if err := z.ctl.Put(b); err != nil {
		t.Fatal(err)
	}
	var rep *sarpb.ControlReply
	waitFor(t, "control reply", 5*time.Second, func() bool {
		z.mu.Lock()
		defer z.mu.Unlock()
		rep = z.replies[c.Header.Seq]
		return rep != nil
	})
	return rep
}

func (z *p4) lastTask() *sarpb.TaskStatus {
	z.mu.Lock()
	defer z.mu.Unlock()
	for i := len(z.tel) - 1; i >= 0; i-- {
		if z.tel[i].Task != nil {
			return z.tel[i].Task
		}
	}
	return nil
}

func p4Config(t *testing.T) *config.Edge {
	c := p3Config(t, "truth-oracle")
	_, s := testScenario(t)
	site := s.Sites[0]
	b := uint32(256)
	c.UAVs = []config.UAV{{ID: "inv-1", Role: "investigator", Behaviour: "standby", AltAGLm: 15, SpeedMps: 10,
		HomeOffsetEastM: site.CenterE + 20, HomeOffsetNorthM: site.CenterN - 20, TelemetryHz: 2, EnduranceMin: 30, GPSNoiseM: 1.5,
		YawNoiseDeg: 1, BudgetKbps: &b, TaskLoiterS: 5, HoverWanderM: 0.15}}
	c.Perception.DetectionSigmaM, c.Perception.MovementChange, c.Perception.MovementTolerancePx, c.Perception.MovementMinFrames = 1, 0.12, 2, 2
	return c
}

// A standby investigator takes an Investigate task over the bus, flies it,
// holds over the target, reports the parts it watched (reason INVESTIGATED,
// with the movement verdict and the task id) and shows the task in telemetry.
func TestInvestigateTask(t *testing.T) {
	c := p4Config(t)
	_, s := testScenario(t)
	r, m, _ := runP3(t, c)
	_ = r
	z := startP4(t, m, "inv-1")
	v := s.Victims[0]

	// Invalid tasks are refused with E-EDGE-007.
	if rep := z.send(t, "inv-1", func(c *sarpb.Control) {
		c.Cmd = &sarpb.Control_Investigate{Investigate: &sarpb.Investigate{TaskId: "bad", Target: &sarpb.GeoPoint{LatDeg: v.Pos.Lat, LonDeg: v.Pos.Lon}, AltAglM: 200, HoverS: 10}}
	}); rep.Ok || !strings.Contains(rep.Error, "E-EDGE-007") {
		t.Fatalf("invalid height accepted: %+v", rep)
	}
	if rep := z.send(t, "inv-1", func(c *sarpb.Control) {
		c.Cmd = &sarpb.Control_CancelTask{CancelTask: &sarpb.CancelTask{TaskId: "nothing"}}
	}); rep.Ok || !strings.Contains(rep.Error, "E-EDGE-007") {
		t.Fatalf("cancel of an unknown task accepted: %+v", rep)
	}

	if rep := z.send(t, "inv-1", func(c *sarpb.Control) {
		c.Cmd = &sarpb.Control_Investigate{Investigate: &sarpb.Investigate{TaskId: "T-0001", CasualtyId: "C-001",
			Target: &sarpb.GeoPoint{LatDeg: v.Pos.Lat, LonDeg: v.Pos.Lon}, AltAglM: 15, HoverS: 12, RadiusM: 6}}
	}); !rep.Ok {
		t.Fatalf("task refused: %s", rep.Error)
	}
	sawInvestigate := false
	waitFor(t, "task done", 90*time.Second, func() bool {
		z.mu.Lock()
		if n := len(z.tel); n > 0 && z.tel[n-1].State == sarpb.FlightState_FLIGHT_STATE_INVESTIGATE {
			sawInvestigate = true
		}
		z.mu.Unlock()
		ts := z.lastTask()
		return ts != nil && ts.State == sarpb.TaskState_TASK_STATE_DONE
	})
	ts := z.lastTask()
	if !sawInvestigate || ts.TaskId != "T-0001" || ts.CasualtyId != "C-001" || ts.Frames < 3 || ts.HoverElapsedS < 12 || ts.FinishedUnixNs == 0 {
		t.Fatalf("task status %+v (investigate state seen: %v)", ts, sawInvestigate)
	}
	var inv []*sarpb.Detection
	waitFor(t, "INVESTIGATED reports", 20*time.Second, func() bool {
		z.mu.Lock()
		defer z.mu.Unlock()
		inv = inv[:0]
		for _, d := range z.dets {
			if d.Reason == sarpb.ReportReason_REPORT_REASON_INVESTIGATED {
				inv = append(inv, d)
			}
		}
		return len(inv) > 0
	})
	for _, d := range inv {
		if d.TaskId != "T-0001" || d.GetMovement().GetHoverFrames() < 2 || d.MinRangeM <= 0 || d.MinRangeM > 20 {
			t.Errorf("report %s: task %q movement %+v min range %.1f", d.DetectionId, d.TaskId, d.GetMovement(), d.MinRangeM)
		}
	}
	if len(ts.TrackIds) == 0 {
		t.Errorf("telemetry lists no parts seen near the target")
	}
	// Then it waits for the next task over the target, and flies home.
	waitFor(t, "return home", 60*time.Second, func() bool {
		z.mu.Lock()
		defer z.mu.Unlock()
		n := len(z.tel)
		return n > 0 && (z.tel[n-1].State == sarpb.FlightState_FLIGHT_STATE_RETURN || z.tel[n-1].State == sarpb.FlightState_FLIGHT_STATE_LANDING ||
			z.tel[n-1].State == sarpb.FlightState_FLIGHT_STATE_LANDED)
	})
	z.mu.Lock()
	last := z.tel[len(z.tel)-1]
	z.mu.Unlock()
	if last.Home == nil || last.CruiseMps != 10 || last.FlightTimeLeftS == 0 {
		t.Fatalf("telemetry home %v cruise %v flight time %v", last.Home, last.CruiseMps, last.FlightTimeLeftS)
	}
}
