package aar

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/bus"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
)

// A short run on the in-memory bus: two casualties (one Red that gets a team
// and is freed, one Black), a close look, a drone flying, chat; then the
// report is built and checked.
func TestRecordAndReport(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()
	dir := filepath.Join(t.TempDir(), "run")
	var chats []ChatLine
	r, err := Start(Options{Bus: m, Incident: "inc", Dir: dir, TelEvery: time.Millisecond, TeamsEvery: time.Millisecond,
		Meta: Meta{Run: "abc", Suite: "mission", Host: "zbook", TimeScale: 5, RescueTimeScale: 30, Teams: []string{"TEAM-1 (foot, sim)"},
			Scenario: "antakya-demo", Staging: [2]float64{36.2016, 36.1585}},
		Chats: func() ([]ChatLine, error) { return chats, nil }})
	if err != nil {
		t.Fatal(err)
	}
	var seq msg.Seq
	put := func(key string, v proto.Message, set func(h *sarpb.Header)) {
		set(msg.NewHeader("inc", "test", seq.Next()))
		b, err := msg.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Put(key, b, bus.PubOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	pos := func(lat, lon float64) *sarpb.GeoPoint { return &sarpb.GeoPoint{LatDeg: lat, LonDeg: lon} }
	list := func(cs ...*sarpb.Casualty) {
		l := &sarpb.CasualtyList{Casualties: cs}
		put(keys.Casualties("inc"), l, func(h *sarpb.Header) { l.Header = h })
	}
	tel := func(lat, lon float64, state sarpb.FlightState, bat float32, reports uint64) {
		x := &sarpb.Telemetry{UavId: "scout-1", Role: sarpb.UavRole_UAV_ROLE_SCOUT, Position: pos(lat, lon), State: state, BatteryPct: bat, Software: "edge-eye 0.7.0-phase6 (commit x)",
			Perception: &sarpb.PerceptionStats{Reports: reports, MissionSteps: 10}}
		put(keys.Telemetry("inc", "scout-1"), x, func(h *sarpb.Header) { x.Header = h })
	}
	disp := func(as ...*sarpb.Assignment) {
		st := &sarpb.DispatchState{Assignments: as, Teams: []*sarpb.Team{{TeamId: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim", Position: pos(36.2016, 36.1585)}}}
		put(keys.Dispatch("inc"), st, func(h *sarpb.Header) { st.Header = h })
	}
	t0 := time.Now()
	tel(36.2010, 36.1580, sarpb.FlightState_FLIGHT_STATE_TAKEOFF, 100, 0)
	tel(36.2030, 36.1600, sarpb.FlightState_FLIGHT_STATE_SURVEY, 98, 4)
	red := &sarpb.Casualty{CasualtyId: "C-001", Triage: sarpb.Triage_TRIAGE_RED, Provisional: true, Position: pos(36.20407, 36.16117), CepM: 2,
		UavIds: []string{"scout-1"}, FirstSeenUnixNs: t0.UnixNano(), Reason: "warm"}
	black := &sarpb.Casualty{CasualtyId: "C-002", Triage: sarpb.Triage_TRIAGE_BLACK, Position: pos(36.2050, 36.1650), CepM: 2, Reason: "cold"}
	list(red, black)
	list(red, black) // unchanged: not recorded again
	tasks := &sarpb.TaskList{Tasks: []*sarpb.Task{{TaskId: "T-1", CasualtyId: "C-001", UavId: "inv-1", State: sarpb.TaskState_TASK_STATE_DONE, Result: &sarpb.TaskResult{Found: true}}}}
	put(keys.Tasks("inc"), tasks, func(h *sarpb.Header) { tasks.Header = h })
	red.Provisional = false
	list(red, black)
	route := &sarpb.RoutePlan{LengthM: 420, EtaS: 300, Points: []*sarpb.GeoPoint{pos(36.2016, 36.1585), pos(36.2030, 36.1600), pos(36.20407, 36.16117)}, Streets: []string{"Kurtuluş Caddesi"}}
	a := &sarpb.Assignment{AssignmentId: "A-1", CasualtyId: "C-001", TeamId: "team-1", Triage: sarpb.Triage_TRIAGE_RED, State: sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE,
		Route: route, CreatedUnixNs: time.Now().UnixNano(), NeededUnixNs: t0.UnixNano()}
	disp(a)
	disp(a) // unchanged
	tel(36.2040, 36.1610, sarpb.FlightState_FLIGHT_STATE_SURVEY, 95, 9)
	a.State, a.OnSceneUnixNs, a.ExtricationS, a.Extrication = sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE, time.Now().UnixNano(), 7800, "heavy rescue, about 2 h (pancake collapse)"
	disp(a)
	chats = append(chats, ChatLine{At: time.Now(), From: "SAR-OPS", To: "All Chat Rooms", Text: "TEAM-1 reached C-001 RED."})
	time.Sleep(50 * time.Millisecond)
	a.State, a.DoneUnixNs = sarpb.AssignmentState_ASSIGNMENT_STATE_DONE, time.Now().UnixNano()
	disp(a)
	tel(36.2010, 36.1580, sarpb.FlightState_FLIGHT_STATE_LANDED, 90, 9)
	r.Note("survey finished")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if n := strings.Count(string(b), `"k":"cas"`); n != 2 {
		t.Fatalf("%d casualty snapshots, want 2 (an unchanged list is not recorded)", n)
	}
	if n := strings.Count(string(b), `"k":"asg"`); n != 3 {
		t.Fatalf("%d assignment events, want 3", n)
	}

	// A scenario with the Red's victim; an offline map overview.
	sc := &scenario.Scenario{Victims: []scenario.Victim{{ID: "v04", SiteID: "s01", Status: "alive_unresponsive", ExpectedTriage: "red", Pos: scenario.LatLon{Lat: 36.20409, Lon: 36.16118}},
		{ID: "v09", SiteID: "s02", Status: "alive_responsive", ExpectedTriage: "yellow", Pos: scenario.LatLon{Lat: 36.1990, Lon: 36.1550}}},
		Sites:     []scenario.Site{{ID: "s01", Center: scenario.LatLon{Lat: 36.2041, Lon: 36.1612}}},
		Blockages: []scenario.Blockage{{SiteID: "s01", Polygon: []scenario.LatLon{{Lat: 36.2040, Lon: 36.1610}, {Lat: 36.2040, Lon: 36.1613}, {Lat: 36.2043, Lon: 36.1613}}}}}
	mapDir := t.TempDir()
	writeOverview(t, mapDir)
	rep, err := Build(dir, BuildOptions{Scene: sc, MapDir: mapDir})
	if err != nil {
		t.Fatal(err)
	}
	k := rep.KPI
	if k.Victims != 2 || k.Found != 1 || k.Records != 2 || k.FalseRecords != 1 || k.TriageAgree != 1 || k.RedYellow != 1 || k.Rescued != 1 || k.Reached != 1 {
		t.Fatalf("kpis %+v", k)
	}
	c := rep.Casualties[0]
	if c.ID != "C-001" || !c.Rescued || c.Victim != "v04" || c.Settled.IsZero() || c.LookAt.IsZero() || len(c.History) != 2 || c.Asg == nil || c.Asg.ExtricationS != 7800 {
		t.Fatalf("C-001 %+v", c)
	}
	if len(rep.Drones) != 1 || rep.Drones[0].Reports != 9 || rep.Drones[0].DistanceM < 100 || rep.Drones[0].Software == "" || len(rep.Chats) != 1 || len(rep.Notes) != 1 {
		t.Fatalf("drones %+v chats %d notes %v", rep.Drones, len(rep.Chats), rep.Notes)
	}
	if err := rep.WriteFiles(dir, BuildOptions{Scene: sc, MapDir: mapDir}); err != nil {
		t.Fatal(err)
	}
	page, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	for _, want := range []string{"After-action report", "1 / 2", "scenario victims found", "data:image/png;base64,", "C-001 RESCUED", "TEAM-1 → C-001: 420 m",
		"v09", "<b>not found</b>", "heavy rescue", "TEAM-1 reached C-001 RED.", "Kurtuluş Caddesi", "scout-1 flight path", "triage settled"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("report misses %q", want)
		}
	}
	for _, f := range []string{"report.json", "summary.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Error(err)
		}
	}
	t.Log(rep.Summary())
	// Without a map: a plain background, still a map.
	page2, err := rep.HTML(BuildOptions{Scene: sc})
	if err != nil || !strings.Contains(string(page2), "No offline map installed") || !strings.Contains(string(page2), `class="mapbg"`) {
		t.Fatalf("report without a map: %v", err)
	}
}

// writeOverview writes map tiles (zoom 17, plain) covering the test area.
func writeOverview(t *testing.T, dir string) {
	z := 17
	n := float64(int(1) << z)
	x0, y0 := mercN(36.2070, 36.1560)
	x1, y1 := mercN(36.1980, 36.1680)
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for i := range img.Pix {
		img.Pix[i] = 230
	}
	img.Set(10, 10, color.RGBA{255, 0, 0, 255})
	for tx := int(x0 * n); tx <= int(x1*n); tx++ {
		for ty := int(y0 * n); ty <= int(y1*n); ty++ {
			p := filepath.Join(dir, "17", fmt.Sprint(tx), fmt.Sprintf("%d.png", ty))
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			f, err := os.Create(p)
			if err != nil {
				t.Fatal(err)
			}
			_ = png.Encode(f, img)
			f.Close()
		}
	}
}
