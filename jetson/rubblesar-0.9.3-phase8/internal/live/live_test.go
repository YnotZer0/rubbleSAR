package live

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

var t0 = time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

// clock is a settable picture clock for tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newHub(quiet bool) (*Hub, *clock) {
	c := &clock{t: t0}
	return NewHub(HubOptions{Incident: "inc", Quiet: quiet, Clock: c.now}), c
}

func texts(p *Picture, kind string) []string {
	var out []string
	for _, it := range p.Timeline {
		if kind == "" || it.Kind == kind {
			out = append(out, it.Text)
		}
	}
	return out
}

func has(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, want) {
			return
		}
	}
	t.Errorf("no line with %q in:\n  %s", want, strings.Join(lines, "\n  "))
}

func drone(id, state string, queued, unacked uint32, resent uint64) *Drone {
	return &Drone{ID: id, Role: "scout", State: state, Lat: 36.2, Lon: 36.16, Alt: 40, Battery: 90, Steps: 40, Step: 3,
		Link: &Link{BudgetBps: 256000, QueuedMsgs: queued, Unacked: unacked, Resent: resent}}
}

func TestDroneSurveyBlackoutAndFlush(t *testing.T) {
	h, c := newHub(false)
	h.SetDrone(drone("scout-1", "takeoff", 0, 0, 0), at(0))
	h.SetDrone(drone("scout-1", "survey", 0, 0, 0), at(2))
	for i := 1; i <= 5; i++ {
		d := drone("scout-1", "survey", 0, 0, 0)
		d.Lat += float64(i) * 1e-4
		h.SetDrone(d, at(2+2*float64(i)))
	}
	// A blackout: nothing heard for 60 s.
	c.t = at(13)
	h.Tick(c.t)
	c.t = at(20)
	h.Tick(c.t)
	p := h.Snapshot()
	has(t, texts(p, "survey"), "scout-1 is surveying the collapse sites (40 survey legs, 40 m up)")
	has(t, texts(p, "outage"), "scout-1 not heard for 6 s: link lost")
	if len(p.Drones) != 1 || len(p.Drones[0].Trail) < 5 {
		t.Fatalf("trail: %+v", p.Drones)
	}
	if !p.Drones[0].Stale {
		t.Error("a drone not heard for 8 s should be stale")
	}
	// Heard again with 12 reports waiting; then the queue drains.
	c.t = at(72)
	h.SetDrone(drone("scout-1", "survey", 9, 3, 4), c.t)
	h.SetDrone(drone("scout-1", "survey", 2, 1, 7), at(80))
	h.SetDrone(drone("scout-1", "survey", 0, 0, 9), at(90))
	p = h.Snapshot()
	out := texts(p, "outage")
	has(t, out, "scout-1 heard again after 60 s: 12 reports waiting on board")
	has(t, out, "every report held during the blackout reached the command post and was acknowledged (5 re-sent, none lost)")
	// Only one "not heard" line for one silence.
	n := 0
	for _, l := range out {
		if strings.Contains(l, "not heard") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d 'not heard' lines, want 1", n)
	}
}

func TestBudgetAndDegrade(t *testing.T) {
	h, _ := newHub(false)
	d := drone("inv-1", "transit", 0, 0, 0)
	h.SetDrone(d, at(0))
	d2 := drone("inv-1", "transit", 30, 2, 0)
	d2.Link.BudgetBps, d2.Link.Degrade = 32000, 2
	h.SetDrone(d2, at(5))
	d3 := drone("inv-1", "transit", 0, 0, 0)
	h.SetDrone(d3, at(40))
	out := texts(h.Snapshot(), "link")
	has(t, out, "inv-1 radio budget now 32 kbit/s (was 256 kbit/s)")
	has(t, out, "inv-1 link falling behind (30 reports queued): sending thermal crops only")
	has(t, out, "inv-1 link caught up: full reports again")
}

func TestCasualtiesTasksRescue(t *testing.T) {
	h, _ := newHub(false)
	teams := []*Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim", State: "idle"},
		{ID: "vader", Callsign: "VADER", Mode: "foot", Source: "tak", State: "idle"}}
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams}, at(0))
	c1 := &Casualty{ID: "C-001", Triage: "Red", Prov: true, Reason: "warm; not yet watched close up", UAVs: []string{"scout-1"}, First: ms(at(10)),
		Parts: []*Part{{UAV: "scout-1", Det: "scout-1-x-0001.1", Class: "hand"}}}
	c2 := &Casualty{ID: "C-002", Triage: "Grey", Prov: true, Reason: "cold from far away", UAVs: []string{"scout-1"}}
	h.AddDetection(&Det{ID: "scout-1-x-0001.1", UAV: "scout-1", Part: "hand", Bytes: 4200})
	h.AddDetection(&Det{ID: "scout-1-x-0001.1", UAV: "scout-1", Part: "hand", Bytes: 4200}) // repeat: counted once
	h.SetCasualties([]*Casualty{c1, c2}, at(10))
	h.SetTasks([]*Task{{ID: "T-1", Casualty: "C-001", State: "queued", Reason: "provisionally Red, not yet seen close up"}}, at(11))
	h.SetTasks([]*Task{{ID: "T-1", Casualty: "C-001", State: "assigned", UAV: "inv-1", Reason: "provisionally Red, not yet seen close up"}}, at(12))
	h.SetTasks([]*Task{{ID: "T-1", Casualty: "C-001", State: "on_station", UAV: "inv-1"}}, at(40))
	h.SetTasks([]*Task{{ID: "T-1", Casualty: "C-001", State: "done", UAV: "inv-1", Found: true}}, at(75))
	c1s := *c1
	c1s.Prov, c1s.Reason, c1s.Close = false, "warm, no movement in 30 s watched close up", true
	h.SetCasualties([]*Casualty{&c1s, c2}, at(76))
	// C-002 merged into C-001.
	h.SetMerged("C-002", "C-001")
	h.SetCasualties([]*Casualty{&c1s}, at(80))
	route := [][2]float64{{36.2, 36.15}, {36.201, 36.151}}
	a := &Assignment{ID: "A-1", Casualty: "C-001", Team: "vader", State: "en_route", Triage: "Red", LengthM: 640, ETAs: 540, Streets: []string{"Kurtuluş Cd."}, Route: route}
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams, Assignments: []*Assignment{a}}, at(90))
	if p := h.Snapshot(); len(p.Assignments) != 1 || len(p.Assignments[0].Route) != 2 || p.Casualties[0].Team != "VADER" || p.Casualties[0].AsgState != "en_route" {
		t.Fatalf("assignment while en route: %+v %+v", p.Assignments, p.Casualties[0])
	}
	b := *a
	b.State, b.Route, b.Extrication, b.ExtricationS = "on_scene", nil, "medium rescue: under 1.2 m of rubble; legs trapped", 5400
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams, Assignments: []*Assignment{&b}}, at(300))
	if p := h.Snapshot(); len(p.Assignments[0].Route) != 2 {
		t.Error("the route should stay while the team is on scene (not sent again)")
	}
	cc := b
	cc.State, cc.Done = "done", ms(at(500))
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams, Assignments: []*Assignment{&cc}}, at(500))
	p := h.Snapshot()
	all := texts(p, "")
	has(t, all, "C-001 found by scout-1, Red?: warm; not yet watched close up")
	has(t, all, "C-002 found by scout-1, Grey?: cold from far away")
	has(t, all, "inv-1 sent for a close look at C-001 (provisionally Red, not yet seen close up)")
	has(t, all, "inv-1 over C-001, watching it")
	has(t, all, "close look at C-001 by inv-1: seen close up, not moving")
	has(t, all, "C-001 confirmed Red: warm, no movement in 30 s watched close up")
	has(t, all, "C-002 merged into C-001: the same person, seen twice")
	has(t, all, "VADER sent to C-001 (Red): 640 m via Kurtuluş Cd., about 9 min on foot; sent to the team's TAK device")
	has(t, all, "VADER reached C-001: medium rescue: under 1.2 m of rubble")
	has(t, all, "C-001 freed and handed over by VADER")
	if p.KPI.Rescued != 1 || p.KPI.NeedTeam != 1 || p.KPI.Records != 1 || p.KPI.LooksDone != 1 || p.KPI.Detections != 1 || p.KPI.Bytes != 4200 {
		t.Errorf("KPI %+v", p.KPI)
	}
	if !p.Casualties[0].Rescued || p.Assignments[0].Route != nil {
		t.Errorf("rescued casualty %+v, route %v", p.Casualties[0], p.Assignments[0].Route)
	}
	if p.Detections[0].Casualty != "C-001" {
		t.Errorf("detection not linked to its casualty: %+v", p.Detections[0])
	}
	// A new run token clears the run (live).
	h.SetDispatch(Dispatch{Run: "r2", Teams: teams}, at(600))
	if p := h.Snapshot(); len(p.Casualties) != 0 || len(p.Assignments) != 0 || !strings.Contains(p.Timeline[len(p.Timeline)-1].Text, "new run") {
		t.Errorf("after a reset: %d casualties, %d assignments, timeline %v", len(p.Casualties), len(p.Assignments), texts(p, ""))
	}
}

func TestQuietSeed(t *testing.T) {
	h, _ := newHub(true)
	h.SetCasualties([]*Casualty{{ID: "C-001", Triage: "Red"}, {ID: "C-002", Triage: "Black"}}, at(0))
	h.SetCasualties([]*Casualty{{ID: "C-001", Triage: "Red"}, {ID: "C-002", Triage: "Black"}, {ID: "C-003", Triage: "Yellow"}}, at(5))
	out := texts(h.Snapshot(), "")
	if len(out) != 2 || !strings.Contains(out[0], "picked up 2 casualty records") || !strings.Contains(out[1], "C-003 found") {
		t.Errorf("quiet seed: %q", out)
	}
}

func TestChatsSorted(t *testing.T) {
	h, _ := newHub(false)
	h.Note(at(20), "", "info", "later note")
	h.AddChats([]*Chat{{At: ms(at(10)), From: "SAR-OPS", To: "VADER", Text: "go to C-001"}, {At: ms(at(30)), From: "VADER", To: "SAR-OPS", Text: "done"}})
	h.AddChats([]*Chat{{At: ms(at(10)), From: "SAR-OPS", To: "VADER", Text: "go to C-001"}})
	p := h.Snapshot()
	if len(p.Chats) != 2 {
		t.Fatalf("chats %d", len(p.Chats))
	}
	got := texts(p, "")
	if len(got) != 3 || !strings.HasPrefix(got[0], "SAR-OPS → VADER") || got[1] != "later note" || got[2] != "VADER → SAR-OPS: done" {
		t.Errorf("timeline order %q", got)
	}
}

// The live feed from the bus, end to end.
func TestLiveFromBus(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()
	h := NewHub(HubOptions{Incident: "inc"})
	crops := &Crops{}
	l, err := Start(Options{Incident: "inc", Bus: m, Hub: h, Crops: crops,
		Chats: func() ([]*Chat, error) { return []*Chat{{At: ms(t0), From: "SAR-OPS", To: "All", Text: "hello"}}, nil }, ChatEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	put := func(key string, m2 interface{ GetHeader() *sarpb.Header }, b []byte) {
		if err := m.Put(key, b, bus.QoSCasualty); err != nil {
			t.Fatal(err)
		}
	}
	tel := &sarpb.Telemetry{Header: msg.NewHeader("inc", "uav/scout-1", 1), UavId: "scout-1", Role: sarpb.UavRole_UAV_ROLE_SCOUT,
		State: sarpb.FlightState_FLIGHT_STATE_SURVEY, Position: &sarpb.GeoPoint{LatDeg: 36.2, LonDeg: 36.16}, AltAglM: 40, BatteryPct: 88,
		Link:       &sarpb.LinkStats{BudgetBps: 256000, UsedBps: 41000, QueuedMsgs: 2},
		Perception: &sarpb.PerceptionStats{Enabled: true, Backend: "tensorrt 10.3.0 on Orin", FrameHzActual: 1, SegmentMs: 71, Reports: 5}}
	b, _ := msg.Marshal(tel)
	put(keys.Telemetry("inc", "scout-1"), tel, b)
	other := &sarpb.Telemetry{Header: msg.NewHeader("other", "uav/x", 1), UavId: "x", Position: &sarpb.GeoPoint{}}
	b, _ = msg.Marshal(other)
	put(keys.Telemetry("inc", "x"), other, b)
	cl := &sarpb.CasualtyList{Header: msg.NewHeader("inc", "zbook/casualty-analyst", 1), Casualties: []*sarpb.Casualty{{CasualtyId: "C-001",
		Triage: sarpb.Triage_TRIAGE_RED, Provisional: true, Position: &sarpb.GeoPoint{LatDeg: 36.2, LonDeg: 36.16}, CepM: 2.1, UavIds: []string{"scout-1"},
		Thermal: &sarpb.CasualtyThermal{Readable: true, Warm: true, SkinC: 31.5, AmbientC: 9}, BestDetectionId: "scout-1-k-0001.1"}}}
	b, _ = msg.Marshal(cl)
	put(keys.Casualties("inc"), cl, b)
	det := &sarpb.Detection{Header: msg.NewHeader("inc", "uav/scout-1", 1), UavId: "scout-1", DetectionId: "scout-1-k-0001.1", TrackId: "scout-1-k-0001",
		Revision: 1, Part: sarpb.PartClass_PART_CLASS_HAND, Confidence: 0.81, Payload: sarpb.Payload_PAYLOAD_FULL,
		VisibleCrop: &sarpb.Crop{Format: "jpeg", Data: []byte{0xff, 0xd8, 1, 2}}, ThermalCrop: &sarpb.Crop{Format: "png16-dk", Data: []byte{1}}}
	b, _ = msg.Marshal(det)
	put(keys.Detection("inc", "scout-1"), det, b)
	ds := &sarpb.DispatchState{Header: msg.NewHeader("inc", "zbook/rescue-dispatch", 1), Run: "r1",
		Teams: []*sarpb.Team{{TeamId: "team-1", Callsign: "TEAM-1", State: sarpb.TeamState_TEAM_STATE_IDLE, Position: &sarpb.GeoPoint{LatDeg: 36.201, LonDeg: 36.158}}},
		Tak:   &sarpb.TakLink{Connected: true, Users: []string{"VADER"}}}
	b, _ = msg.Marshal(ds)
	put(keys.Dispatch("inc"), ds, b)
	deadline := time.Now().Add(5 * time.Second)
	var p *Picture
	for time.Now().Before(deadline) {
		p = h.Snapshot()
		if len(p.Drones) == 1 && len(p.Casualties) == 1 && len(p.Detections) == 1 && len(p.Teams) == 1 && len(p.Chats) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(p.Drones) != 1 || p.Drones[0].Perc.Backend != "tensorrt 10.3.0 on Orin" || p.Drones[0].Link.UsedBps != 41000 || p.Drones[0].Stored != 1 {
		t.Fatalf("drones %+v", p.Drones)
	}
	if len(p.Casualties) != 1 || p.Casualties[0].Triage != "Red" || !p.Casualties[0].Prov || p.Casualties[0].Thermal.SkinC != 31.5 {
		t.Fatalf("casualties %+v", p.Casualties)
	}
	if len(p.Detections) != 1 || p.Detections[0].Part != "hand" || p.Detections[0].Casualty != "C-001" || p.Detections[0].Bytes != len(b) && p.Detections[0].Bytes == 0 {
		t.Fatalf("detections %+v", p.Detections)
	}
	if v, ok := crops.Visible("scout-1-k-0001.1"); !ok || !bytes.Equal(v, []byte{0xff, 0xd8, 1, 2}) {
		t.Fatal("crop not kept")
	}
	if len(p.Teams) != 1 || p.TAK == nil || !p.TAK.Connected || len(p.Chats) != 1 {
		t.Fatalf("teams %+v tak %+v chats %d", p.Teams, p.TAK, len(p.Chats))
	}
	if rec, rej := l.Received(); rec["telemetry"] != 1 || rej != 1 {
		t.Errorf("received %v rejected %d (the other incident's telemetry must be rejected)", rec, rej)
	}
}

func TestThermalView(t *testing.T) {
	im := image.NewGray16(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			c := 8.0
			if x >= 3 && x <= 4 && y >= 3 && y <= 4 {
				c = 33
			}
			im.SetGray16(x, y, color.Gray16{Y: uint16(math.Round((c + 273.15) * 10))})
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, im)
	out, err := ThermalView(b.Bytes(), 6, 35, 64)
	if err != nil {
		t.Fatal(err)
	}
	v, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if v.Bounds().Dx() != 64 {
		t.Fatalf("size %v", v.Bounds())
	}
	hot := color.NRGBAModel.Convert(v.At(32, 32)).(color.NRGBA)
	cold := color.NRGBAModel.Convert(v.At(2, 2)).(color.NRGBA)
	if !(hot.R > 200 && hot.G > 150) || !(cold.R < 80) {
		t.Errorf("hot %v cold %v", hot, cold)
	}
	if !ValidID("scout-1-k-0001.1") || ValidID("../etc/passwd") || ValidID("a/b") {
		t.Error("ValidID")
	}
}

// Every assignment gets a "sent to" line: one first seen already on scene
// (a team a few metres away gets there between two dispatch states), and one
// that follows its casualty into a merged record (P7-05 on the ZBook, 0.9.1).
func TestSentLinesForQuickAndMergedAssignments(t *testing.T) {
	h, _ := newHub(false)
	teams := []*Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim", State: "idle"}}
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams}, at(0))
	quick := &Assignment{ID: "A-1", Casualty: "C-015", Team: "team-1", State: "on_scene", Triage: "Red", LengthM: 9, ETAs: 2}
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams, Assignments: []*Assignment{quick}}, at(10))
	moved := &Assignment{ID: "A-2", Casualty: "C-020", Team: "team-1", State: "en_route", Triage: "Red", LengthM: 300, ETAs: 200}
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams, Assignments: []*Assignment{quick, moved}}, at(20))
	m2 := *moved
	m2.Casualty = "C-016"
	h.SetDispatch(Dispatch{Run: "r1", Teams: teams, Assignments: []*Assignment{quick, &m2}}, at(30))
	sent := map[string]int{}
	for _, it := range h.Timeline() {
		if it.Kind == "team" && strings.Contains(it.Text, " sent to ") {
			sent[it.Ref]++
		}
	}
	if sent["C-015"] != 1 || sent["C-016"] != 1 || sent["C-020"] != 1 {
		t.Errorf("sent lines per casualty %v", sent)
	}
	p := h.Snapshot()
	has(t, texts(p, "team"), "TEAM-1 reached C-015")
	has(t, texts(p, "team"), "TEAM-1 now sent to C-016: C-020 was merged into it")
}
