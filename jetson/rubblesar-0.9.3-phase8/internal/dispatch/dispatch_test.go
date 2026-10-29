package dispatch

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/cot"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/roads"
	"rubblesar/internal/sarpb"
)

var centre = geo.Point{Lat: 36.2025, Lon: 36.16056}

type inbox struct {
	mu sync.Mutex
	ev []*cot.Event
}

func (in *inbox) add(e *cot.Event) { in.mu.Lock(); in.ev = append(in.ev, e); in.mu.Unlock() }

func (in *inbox) find(f func(*cot.Event) bool) []*cot.Event {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []*cot.Event
	for _, e := range in.ev {
		if f(e) {
			out = append(out, e)
		}
	}
	return out
}

// latest returns the last event per uid (nil when the last was a delete).
func (in *inbox) latest() map[string]*cot.Event {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := map[string]*cot.Event{}
	for _, e := range in.ev {
		if e.IsDelete() {
			out[e.DeleteUID()] = nil
			continue
		}
		out[e.UID] = e
	}
	return out
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func casualtyAt(local geo.Local, id string, e, n float64, tri sarpb.Triage, prov bool) *sarpb.Casualty {
	g := local.ToPoint(e, n)
	return &sarpb.Casualty{CasualtyId: id, Position: &sarpb.GeoPoint{LatDeg: g.Lat, LonDeg: g.Lon, AltMslM: 82}, Triage: tri, Provisional: prov, CepM: 1.5,
		Reason: "test reason", Summary: "Warm. Test.", UavIds: []string{"scout-1"}}
}

type harness struct {
	t      *testing.T
	m      *bus.Memory
	svc    *Service
	list   bus.Publisher
	one    func(c *sarpb.Casualty)
	cas    []*sarpb.Casualty
	local  geo.Local
	seq    msg.Seq
	states []*sarpb.DispatchState
	smu    sync.Mutex
}

func (h *harness) publish() {
	b, _ := msg.Marshal(&sarpb.CasualtyList{Header: msg.NewHeader("inc", "test", h.seq.Next()), Casualties: h.cas})
	_ = h.list.Put(b)
}

func (h *harness) state() *sarpb.DispatchState {
	h.smu.Lock()
	defer h.smu.Unlock()
	if len(h.states) == 0 {
		return nil
	}
	return h.states[len(h.states)-1]
}

func (h *harness) assignment(cas string) *sarpb.Assignment {
	st := h.svc.State()
	var last *sarpb.Assignment
	for _, a := range st.Assignments {
		if a.CasualtyId == cas {
			last = a
		}
	}
	return last
}

func newHarness(t *testing.T, takAddr string, teams []config.Team) *harness {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("DSP_LOG") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	local := geo.NewLocal(centre)
	n, err := roads.Parse(roads.Synthetic(centre, 400, 60), local)
	if err != nil {
		t.Fatal(err)
	}
	// Rubble across avenue x=0 between the staging area and the casualties, and around C-004.
	block := []geom.Poly{{{X: -10, Y: -40}, {X: 10, Y: -40}, {X: 10, Y: -20}, {X: -10, Y: -20}},
		{{X: 112, Y: 112}, {X: 128, Y: 112}, {X: 128, Y: 128}, {X: 112, Y: 128}}}
	n.Block(block, 1)
	d := config.Dispatch{TakServer: takAddr, Callsign: "SAR-OPS", Group: "Cyan", MarkerRefresh: config.Duration{Duration: 2 * time.Second},
		MarkerStale: config.Duration{Duration: time.Minute}, DroneEvery: config.Duration{Duration: 200 * time.Millisecond}, ShowInvestigations: true,
		Triage: []string{"red", "yellow"}, ArriveM: 15, OnScene: config.Duration{Duration: 20 * time.Second}, TimeScale: 100,
		TeamOffline: config.Duration{Duration: 3 * time.Second}, PublishInterval: config.Duration{Duration: time.Second}, Teams: teams}
	stg := local.ToPoint(0, -150)
	m := bus.NewMemory()
	svc, err := Open(Config{Incident: "inc", Centre: centre, GroundAltMSL: 80, Staging: stg, D: d, Roads: n, RoadsPath: "synthetic", Blockages: block,
		Tick: 50 * time.Millisecond}, m, log)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, m: m, svc: svc, local: local}
	h.list, _ = m.DeclarePublisher(keys.Casualties("inc"), bus.QoSCasualty)
	m.Subscribe(keys.Dispatch("inc"), func(x bus.Sample) {
		var st sarpb.DispatchState
		if msg.Unmarshal(x.Payload, &st) == nil {
			h.smu.Lock()
			h.states = append(h.states, &st)
			h.smu.Unlock()
		}
	})
	h.one = func(c *sarpb.Casualty) {
		p, _ := m.DeclarePublisher(keys.Casualty("inc", c.CasualtyId), bus.QoSCasualty)
		c.Header = msg.NewHeader("inc", "test", h.seq.Next())
		b, _ := msg.Marshal(c)
		_ = p.Put(b)
	}
	return h
}

func TestDispatchEndToEnd(t *testing.T) {
	srv, err := cot.ListenServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	teams := []config.Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim"}, {ID: "team-2", Callsign: "TEAM-2", Mode: "vehicle", Source: "sim"},
		{ID: "alpha", Callsign: "ALPHA", Mode: "foot", Source: "tak"}}
	h := newHarness(t, srv.Addr(), teams)
	defer h.m.Close()
	svc := h.svc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Run(ctx)
	defer svc.Close()

	// A real team: ALPHA on ATAK, at the staging area.
	var in inbox
	local := h.local
	alphaPos := geom.P{X: 5, Y: -150}
	alphaQuiet := false
	var amu sync.Mutex
	alpha := cot.NewClient(cot.ClientConfig{Addr: srv.Addr(), OnEvent: in.add, MaxBackoff: 200 * time.Millisecond,
		Hello: func(now time.Time) []*cot.Event {
			amu.Lock()
			g := local.ToPoint(alphaPos.X, alphaPos.Y)
			amu.Unlock()
			return []*cot.Event{cot.PLI("ANDROID-alpha", "ALPHA", "Cyan", "Team Member", g.Lat, g.Lon, 0, 0, "", now, time.Minute)}
		}})
	go alpha.Run(ctx)
	pli := func(p geom.P) {
		amu.Lock()
		alphaPos = p
		amu.Unlock()
		g := local.ToPoint(p.X, p.Y)
		alpha.Send(cot.PLI("ANDROID-alpha", "ALPHA", "Cyan", "Team Member", g.Lat, g.Lon, 0, 1.2, "", time.Now(), time.Minute))
	}
	// Like ATAK: a position report every second or so.
	go func() {
		for ctx.Err() == nil {
			amu.Lock()
			p := alphaPos
			quiet := alphaQuiet
			amu.Unlock()
			if !quiet {
				pli(p)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
	waitFor(t, "ALPHA online", 5*time.Second, func() bool {
		for _, tm := range svc.State().Teams {
			if tm.TeamId == "alpha" && tm.State == sarpb.TeamState_TEAM_STATE_IDLE {
				return true
			}
		}
		return false
	})

	// Four casualties at once: Yellow, provisional Red, Black, settled Red.
	h.cas = []*sarpb.Casualty{
		casualtyAt(local, "C-001", -60, 60, sarpb.Triage_TRIAGE_YELLOW, false),
		casualtyAt(local, "C-002", 60, 0, sarpb.Triage_TRIAGE_RED, true),
		casualtyAt(local, "C-003", -120, 0, sarpb.Triage_TRIAGE_BLACK, false),
		casualtyAt(local, "C-004", 120, 120, sarpb.Triage_TRIAGE_RED, false),
	}
	h.publish()
	waitFor(t, "three assignments", 5*time.Second, func() bool { return len(svc.State().Assignments) >= 3 })
	st := svc.State()
	order := []string{}
	for _, a := range st.Assignments {
		order = append(order, a.CasualtyId+"/"+a.TeamId)
	}
	t.Logf("assignments: %v", order)
	if st.Assignments[0].CasualtyId != "C-004" || st.Assignments[0].Priority != 1 || st.Assignments[1].CasualtyId != "C-002" || st.Assignments[2].CasualtyId != "C-001" {
		t.Fatalf("order %v (want C-004 Red, C-002 provisional Red, C-001 Yellow)", order)
	}
	if h.assignment("C-003") != nil {
		t.Fatal("Black casualty got a team")
	}
	// Routes avoid the rubble; C-004's final leg crosses rubble.
	for _, a := range st.Assignments {
		r := a.Route
		for i := 2; i <= int(r.AccessIndex) && i < len(r.Points); i++ {
			p := r.Points[i]
			e, n := local.ToLocal(geo.Point{Lat: p.LatDeg, Lon: p.LonDeg})
			if svc.cfg.Roads.InBlockage(geom.P{X: e, Y: n}) {
				t.Fatalf("%s route point %d in rubble", a.AssignmentId, i)
			}
		}
		if a.CasualtyId == "C-004" && r.RubbleM <= 0 {
			t.Fatalf("C-004 final leg %+v", r)
		}
	}

	// The TAK picture: markers in triage colours, route lines, chat.
	waitFor(t, "four markers", 5*time.Second, func() bool {
		n := 0
		for uid, e := range in.latest() {
			if strings.Contains(uid, ".cas.") && e != nil {
				n++
			}
		}
		return n == 4
	})
	lat := in.latest()
	for id, want := range map[string]int32{"C-001": cot.Yellow, "C-002": cot.Red, "C-003": cot.Black, "C-004": cot.Red} {
		e := lat["rubblesar.inc.cas."+id]
		if c, _ := e.MarkerColor(); c != want {
			t.Errorf("%s colour %d want %d", id, c, want)
		}
		if id == "C-002" && e.Callsign() != "C-002 RED?" {
			t.Errorf("label %q", e.Callsign())
		}
	}
	var alphaAsg *sarpb.Assignment
	for _, a := range svc.State().Assignments {
		if a.TeamId == "alpha" {
			alphaAsg = a
		}
	}
	if alphaAsg == nil {
		t.Fatal("ALPHA got no assignment")
	}
	waitFor(t, "ALPHA's route and direct chat", 5*time.Second, func() bool {
		return in.latest()["rubblesar.inc.route.alpha"] != nil &&
			len(in.find(func(e *cot.Event) bool {
				return e.IsChat() && len(e.Dests()) > 0 && strings.Contains(e.ChatText(), alphaAsg.CasualtyId)
			})) > 0
	})
	route := in.latest()["rubblesar.inc.route.alpha"].LinePoints()
	if len(route) < 2 {
		t.Fatalf("route %v", route)
	}
	if n := len(in.find(func(e *cot.Event) bool { return e.IsChat() && strings.Contains(e.ChatText(), "→") })); n < 3 {
		t.Fatalf("%d dispatch chats in All Chat Rooms", n)
	}
	// Simulated teams arrive, stay on_scene/time_scale, finish.
	waitFor(t, "simulated teams done", 10*time.Second, func() bool {
		done := 0
		for _, a := range svc.State().Assignments {
			if a.TeamId != "alpha" && a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_DONE {
				done++
			}
		}
		return done >= 2
	})
	// ALPHA walks to the end of its route, then says done.
	end := route[len(route)-1]
	e, n := local.ToLocal(geo.Point{Lat: end[0], Lon: end[1]})
	pli(geom.P{X: e + 3, Y: n})
	waitFor(t, "ALPHA on scene", 5*time.Second, func() bool {
		a := h.assignment(alphaAsg.CasualtyId)
		return a != nil && a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE
	})
	g := local.ToPoint(e, n)
	if cs := srv.Callsigns(); strings.Join(cs, ",") != "ALPHA,SAR-OPS,TEAM-1,TEAM-2" {
		t.Fatalf("TAK clients %v", cs)
	}
	alpha.Send(cot.GeoChat("ANDROID-alpha", "ALPHA", cot.ChatTo{UID: svc.OpsUID(), Callsign: "SAR-OPS"}, "Done", g.Lat, g.Lon, time.Now()))
	waitFor(t, "ALPHA done", 5*time.Second, func() bool {
		a := h.assignment(alphaAsg.CasualtyId)
		return a != nil && a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_DONE
	})
	waitFor(t, "route line deleted", 5*time.Second, func() bool {
		v, ok := in.latest()["rubblesar.inc.route.alpha"]
		return ok && v == nil
	})
	// Every Red and Yellow reached: nothing waits; markers say so.
	waitFor(t, "markers updated", 5*time.Second, func() bool {
		for _, id := range []string{"C-001", "C-002", "C-004"} {
			e := in.latest()["rubblesar.inc.cas."+id]
			if e == nil || !strings.Contains(e.Detail.Remarks.Text, "freed and handed over by") {
				return false
			}
		}
		return true
	})
	if w := svc.State().Waiting; len(w) != 0 {
		t.Fatalf("waiting %v", w)
	}

	// A new provisional Red that turns Black on its close look: the team is called off.
	h.cas = append(h.cas, casualtyAt(local, "C-005", -180, 120, sarpb.Triage_TRIAGE_RED, true))
	h.publish()
	waitFor(t, "C-005 assigned", 5*time.Second, func() bool { return h.assignment("C-005") != nil })
	h.cas[4] = casualtyAt(local, "C-005", -180, 120, sarpb.Triage_TRIAGE_BLACK, false)
	h.publish()
	waitFor(t, "C-005 cancelled or reached", 5*time.Second, func() bool {
		a := h.assignment("C-005")
		return a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_CANCELLED || a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_DONE ||
			a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE
	})
	// A merge: C-006 (assigned) merged into C-007, which takes the assignment over.
	h.cas = append(h.cas, casualtyAt(local, "C-006", 180, -60, sarpb.Triage_TRIAGE_YELLOW, false))
	h.publish()
	waitFor(t, "C-006 assigned", 5*time.Second, func() bool { return h.assignment("C-006") != nil })
	h.one(&sarpb.Casualty{CasualtyId: "C-006", MergedInto: "C-007"})
	h.cas = append(h.cas[:5], casualtyAt(local, "C-007", 181, -61, sarpb.Triage_TRIAGE_YELLOW, false))
	h.publish()
	waitFor(t, "C-007 carries the assignment", 5*time.Second, func() bool { return h.assignment("C-007") != nil })
	if a := h.assignment("C-006"); a != nil {
		t.Fatalf("C-006 still assigned: %+v", a)
	}
	waitFor(t, "C-006 marker deleted", 5*time.Second, func() bool {
		v, ok := in.latest()["rubblesar.inc.cas.C-006"]
		return ok && v == nil
	})
	if h.state() == nil || len(h.state().Teams) != 3 {
		t.Fatal("dispatch state not published")
	}
	t.Log("\n" + svc.Status().Text())

	// ALPHA goes quiet: offline (TeamOffline 3 s).
	amu.Lock()
	alphaQuiet = true
	amu.Unlock()
	waitFor(t, "ALPHA offline", 6*time.Second, func() bool {
		for _, tm := range svc.State().Teams {
			if tm.TeamId == "alpha" {
				return tm.State == sarpb.TeamState_TEAM_STATE_OFFLINE
			}
		}
		return false
	})
	// Reset: everything forgotten, sim teams back.
	svc.Reset()
	if st := svc.State(); len(st.Assignments) != 0 || st.Teams[0].State != sarpb.TeamState_TEAM_STATE_IDLE {
		t.Fatalf("after reset %+v", st)
	}
}

func TestNoRoadsStraightLine(t *testing.T) {
	h := newHarness(t, "", []config.Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim"}})
	defer h.m.Close()
	h.svc.router = nil
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.svc.Run(ctx)
	h.cas = []*sarpb.Casualty{casualtyAt(h.local, "C-001", 0, 0, sarpb.Triage_TRIAGE_RED, false)}
	h.publish()
	waitFor(t, "assigned", 5*time.Second, func() bool { return h.assignment("C-001") != nil })
	a := h.assignment("C-001")
	if len(a.Route.Points) != 2 || a.Note == "" {
		t.Fatalf("straight route %+v", a)
	}
}
