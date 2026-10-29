package dispatch

import (
	"context"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"rubblesar/internal/config"
	"rubblesar/internal/cot"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/roads"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
)

func TestExtricationModel(t *testing.T) {
	centre := geo.Point{Lat: 36.2025, Lon: 36.16056}
	local := geo.NewLocal(centre)
	at := func(e, n float64) scenario.LatLon {
		g := local.ToPoint(e, n)
		return scenario.LatLon{Lat: g.Lat, Lon: g.Lon}
	}
	sc := &scenario.Scenario{Center: scenario.LatLon{Lat: centre.Lat, Lon: centre.Lon},
		Sites: []scenario.Site{
			{ID: "s01", Kind: "pancake", HeightM: 2.6, Footprint: [][2]float64{{0, 0}, {20, 0}, {20, 20}, {0, 20}}},
			{ID: "s02", Kind: "mound", HeightM: 1.5, Footprint: [][2]float64{{200, 0}, {220, 0}, {220, 20}, {200, 20}}},
		},
		Victims: []scenario.Victim{
			{ID: "v01", SiteID: "s01", Pos: at(5, 5), Exposed: []scenario.Exposed{{Kind: scenario.ExpHand}}, OcclusionFrac: 0.3},
			{ID: "v02", SiteID: "s01", Pos: at(15, 15), Exposed: []scenario.Exposed{{Kind: scenario.ExpHeadShoulders}}, OcclusionFrac: 0.05},
			{ID: "v03", SiteID: "s02", Pos: at(210, 10), Exposed: []scenario.Exposed{{Kind: scenario.ExpForearmHand}}, OcclusionFrac: 0.1},
		}}
	m := newSceneModel(sc, local)
	// Same casualty, run and team: the same estimate; the variation spans runs.
	hand := m.estimate("C-001", geom.P{X: 5.5, Y: 5}, roads.Foot, "abc")
	if again := m.estimate("C-001", geom.P{X: 5.5, Y: 5}, roads.Foot, "abc"); again != hand {
		t.Fatalf("not repeatable: %+v %+v", hand, again)
	}
	// Averaged over runs: only a hand showing takes longer than head and shoulders uncovered,
	// a pancake longer than a mound, a vehicle team less long than one on foot.
	avg := func(id string, p geom.P, mode roads.Mode) time.Duration {
		var sum time.Duration
		for i := range 50 {
			e := m.estimate(id, p, mode, string(rune('a'+i%26))+string(rune('a'+i/26)))
			if e.Dur < 15*time.Minute || e.Dur > 6*time.Hour || e.Dur%(5*time.Minute) != 0 {
				t.Fatalf("%s: %v out of 15 min..6 h or not in 5 min steps", id, e.Dur)
			}
			sum += e.Dur
		}
		return sum / 50
	}
	h, hs, mo := avg("C-001", geom.P{X: 5, Y: 5}, roads.Foot), avg("C-002", geom.P{X: 15, Y: 15}, roads.Foot), avg("C-003", geom.P{X: 210, Y: 10}, roads.Foot)
	hv := avg("C-001", geom.P{X: 5, Y: 5}, roads.Vehicle)
	t.Logf("hand %v, head and shoulders %v, mound forearm %v, hand by vehicle team %v", h, hs, mo, hv)
	if !(h > hs && h > mo && hv < h) {
		t.Fatalf("order: hand %v, head %v, mound %v, vehicle %v", h, hs, mo, hv)
	}
	if !strings.Contains(hand.Why, "pancake collapse, 2.6 m of rubble") || !strings.Contains(hand.Why, "only a hand showing, 30% covered") {
		t.Fatalf("why %q", hand.Why)
	}
	// In a collapse but no victim matched (a false record nearby): the site alone.
	if e := m.estimate("C-009", geom.P{X: 25, Y: 10}, roads.Foot, "abc"); !strings.HasPrefix(e.Why, "pancake collapse") || strings.Contains(e.Why, "showing") {
		t.Fatalf("site only: %+v", e)
	}
	// Nowhere near a collapse, or no scenario: a light rescue.
	var none *sceneModel
	for _, e := range []extrication{m.estimate("C-010", geom.P{X: 500, Y: 500}, roads.Foot, "abc"), none.estimate("C-010", geom.P{}, roads.Foot, "abc")} {
		if e.Class != "light rescue" || e.Dur > 30*time.Minute {
			t.Fatalf("no collapse: %+v", e)
		}
	}
	for d, want := range map[time.Duration]string{45 * time.Minute: "45 min", 2 * time.Hour: "2 h", 130 * time.Minute: "2 h 10 min", 30 * time.Second: "30 s"} {
		if got := hmin(d); got != want {
			t.Errorf("hmin(%v) = %q, want %q", d, got, want)
		}
	}
}

// A simulated team frees its casualty (the extrication, scaled), then the
// marker turns green and says RESCUED.
func TestExtricationAndRescuedMarker(t *testing.T) {
	srv, err := cot.ListenServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := newHarness(t, srv.Addr(), []config.Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim"}})
	defer h.m.Close()
	svc := h.svc
	svc.cfg.D.Extrication = true
	svc.cfg.D.RescueTimeScale = 3000 // a light rescue (15..30 min) takes under a second
	var in inbox
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher := cot.NewClient(cot.ClientConfig{Addr: srv.Addr(), OnEvent: in.add, MaxBackoff: 200 * time.Millisecond,
		Hello: func(now time.Time) []*cot.Event {
			return []*cot.Event{cot.PLI("WATCHER", "WATCHER", "Cyan", "Team Member", 36.2, 36.16, 0, 0, "", now, time.Minute)}
		}})
	go watcher.Run(ctx)
	go svc.Run(ctx)
	defer svc.Close()
	h.cas = []*sarpb.Casualty{casualtyAt(h.local, "C-001", 60, 0, sarpb.Triage_TRIAGE_RED, false)}
	h.publish()
	waitFor(t, "on scene", 10*time.Second, func() bool {
		a := h.assignment("C-001")
		return a != nil && a.State >= sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE
	})
	waitFor(t, "done", 10*time.Second, func() bool {
		a := h.assignment("C-001")
		return a != nil && a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_DONE
	})
	a := h.assignment("C-001")
	if !strings.HasPrefix(a.Extrication, "light rescue, about ") || a.ExtricationS < float32((15*time.Minute+svc.cfg.D.OnScene.Duration).Seconds()) {
		t.Fatalf("extrication %q %.0f s", a.Extrication, a.ExtricationS)
	}
	var reached, finished bool
	for _, c := range svc.Chats() {
		reached = reached || strings.HasPrefix(c.Text, "TEAM-1 reached C-001 RED") && strings.Contains(c.Text, "Light rescue, about")
		finished = finished || strings.HasPrefix(c.Text, "TEAM-1 finished at C-001: freed after ")
	}
	if !reached || !finished {
		t.Fatalf("chat: %+v", svc.Chats())
	}
	waitFor(t, "green RESCUED marker", 5*time.Second, func() bool {
		e := in.latest()["rubblesar.inc.cas.C-001"]
		if e == nil {
			return false
		}
		c, _ := e.MarkerColor()
		return c == cot.Green && e.Callsign() == "C-001 RESCUED" && strings.Contains(e.Detail.Remarks.Text, "RESCUED by TEAM-1")
	})
	if c, l := MarkerLook("C-002", &sarpb.Casualty{Triage: sarpb.Triage_TRIAGE_YELLOW, Provisional: true}, false); c != cot.Yellow || l != "C-002 YELLOW?" {
		t.Fatalf("look %d %q", c, l)
	}
}

// A TAK team that neither answers nor moves loses its casualty to another team.
func TestTakTeamThatDoesNotAnswer(t *testing.T) {
	srv, err := cot.ListenServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	h := newHarness(t, srv.Addr(), []config.Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim"},
		{ID: "desk", Callsign: "DESK", Mode: "foot", Source: "tak"}})
	defer h.m.Close()
	svc := h.svc
	svc.cfg.D.TakAckTimeout = config.Duration{Duration: 1500 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// DESK sits right by the casualty, so it is the quickest team, and never moves.
	g := h.local.ToPoint(0, 100)
	desk := cot.NewClient(cot.ClientConfig{Addr: srv.Addr(), MaxBackoff: 200 * time.Millisecond,
		Hello: func(now time.Time) []*cot.Event {
			return []*cot.Event{cot.PLI("WINTAK-desk", "DESK", "Cyan", "Team Member", g.Lat, g.Lon, 0, 0, "", now, time.Minute)}
		}})
	go desk.Run(ctx)
	go func() {
		for ctx.Err() == nil {
			desk.Send(cot.PLI("WINTAK-desk", "DESK", "Cyan", "Team Member", g.Lat, g.Lon, 0, 0, "", time.Now(), time.Minute))
			time.Sleep(300 * time.Millisecond)
		}
	}()
	go svc.Run(ctx)
	defer svc.Close()
	waitFor(t, "DESK online", 5*time.Second, func() bool {
		for _, tm := range svc.State().Teams {
			if tm.TeamId == "desk" && tm.State == sarpb.TeamState_TEAM_STATE_IDLE {
				return true
			}
		}
		return false
	})
	h.cas = []*sarpb.Casualty{casualtyAt(h.local, "C-001", 0, 160, sarpb.Triage_TRIAGE_RED, false)}
	h.publish()
	waitFor(t, "DESK assigned", 5*time.Second, func() bool { a := h.assignment("C-001"); return a != nil && a.TeamId == "desk" })
	waitFor(t, "C-001 goes to TEAM-1", 8*time.Second, func() bool { a := h.assignment("C-001"); return a != nil && a.TeamId == "team-1" })
	var cancelled *sarpb.Assignment
	for _, a := range svc.State().Assignments {
		if a.TeamId == "desk" {
			cancelled = a
		}
	}
	if cancelled == nil || cancelled.State != sarpb.AssignmentState_ASSIGNMENT_STATE_CANCELLED || !strings.Contains(cancelled.Note, "DESK did not answer or move towards it within 2 s") {
		t.Fatalf("DESK's assignment %+v", cancelled)
	}
}

// The scenario's rubble areas go to the TAK map as filled shapes.
func TestCollapseShapes(t *testing.T) {
	centre := geo.Point{Lat: 36.2025, Lon: 36.16056}
	sc := &scenario.Scenario{Center: scenario.LatLon{Lat: centre.Lat, Lon: centre.Lon},
		Sites: []scenario.Site{{ID: "s01", Kind: "pancake", HeightM: 2.6}},
		Blockages: []scenario.Blockage{{ID: "b01", SiteID: "s01", Polygon: []scenario.LatLon{{Lat: 36.2040, Lon: 36.1610}, {Lat: 36.2040, Lon: 36.1613},
			{Lat: 36.2043, Lon: 36.1613}, {Lat: 36.2043, Lon: 36.1610}}}}}
	d := config.Dispatch{Callsign: "SAR-OPS", ShowCollapses: true, MarkerStale: config.Duration{Duration: time.Minute}, BlockMarginM: 1}
	s := New(Config{Incident: "inc", Centre: centre, D: d, Scene: sc}, nil, nil)
	ev := s.hello(time.Now())
	var shape *cot.Event
	for _, e := range ev {
		if e.UID == "rubblesar.inc.rubble.1" {
			shape = e
		}
	}
	if shape == nil || shape.Type != "u-d-f" || shape.Detail.FillColor == nil || len(shape.LinePoints()) != 5 || shape.Callsign() != "s01 collapse" {
		t.Fatalf("rubble shape %+v", shape)
	}
	// Named after its street once the street file has given the sites names.
	s.cfg.SiteNames = map[string]string{"s01": "Gündüz Caddesi"}
	for _, e := range s.collapsesLocked(time.Now()) {
		if e.UID == "rubblesar.inc.rubble.1" && e.Callsign() != "Gündüz Caddesi collapse" {
			t.Fatalf("named rubble shape %q", e.Callsign())
		}
	}
	if b, _ := shape.Marshal(); !strings.Contains(string(b), `<fillColor value="`) {
		t.Fatalf("xml %s", b)
	}
	s.cfg.D.ShowCollapses = false
	if len(s.collapsesLocked(time.Now())) != 0 {
		t.Fatal("shapes when show_collapses is off")
	}
}

// A TAK team with at_command_post stands at the command post whatever its device reports.
func TestTAKTeamAtCommandPost(t *testing.T) {
	centre := geo.Point{Lat: 36.2025, Lon: 36.16056}
	staging := geo.Point{Lat: 36.2016, Lon: 36.1585}
	d := config.Dispatch{Callsign: "SAR-OPS", MarkerStale: config.Duration{Duration: time.Minute}, TeamOffline: config.Duration{Duration: time.Minute},
		Teams: []config.Team{{ID: "atak-1", Callsign: "ATAK-1", Mode: "foot", Source: "tak", AtCommandPost: true}, {ID: "vader", Callsign: "VADER", Mode: "foot", Source: "tak"}}}
	s := New(Config{Incident: "inc", Centre: centre, Staging: staging, D: d}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	s.onTAK(cot.PLI("ANDROID-phone", "ATAK-1", "Cyan", "Team Member", 51.5, -0.12, 0, 3, "", now, time.Minute)) // a phone in London
	s.onTAK(cot.PLI("WINTAK-desk", "VADER", "Cyan", "Team Member", 51.5, -0.12, 0, 0, "", now, time.Minute))
	var atak, vader *team
	for _, tm := range s.teams {
		switch tm.cfg.ID {
		case "atak-1":
			atak = tm
		case "vader":
			vader = tm
		}
	}
	if atak == nil || vader == nil || atak.lastSeen.IsZero() {
		t.Fatalf("teams %+v %+v", atak, vader)
	}
	if atak.pos != s.stage || atak.speed != 0 {
		t.Errorf("ATAK-1 at %+v (speed %v), want the command post %+v", atak.pos, atak.speed, s.stage)
	}
	if dist := math.Hypot(vader.pos.X-s.stage.X, vader.pos.Y-s.stage.Y); dist < 1e6 {
		t.Errorf("VADER should be where its device is (London), %.0f m from the command post", dist)
	}
}
