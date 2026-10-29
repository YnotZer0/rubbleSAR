package check

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"rubblesar/internal/aar"
	"rubblesar/internal/basemap"
	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/cot"
	"rubblesar/internal/dispatch"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/live"
	"rubblesar/internal/msg"
	"rubblesar/internal/roads"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/version"
	"rubblesar/internal/web"
)

// p6World is the phase 6 flow in one process: the phase 4 world plus
// rescue-dispatch on a synthetic street grid around the scenario's rubble, a
// TAK router standing in for taky (the acceptance suite plays the TAK team
// CHECK-1), the offline map (drawn when Python and Pillow are here) and
// sar-web. Phase 7 adds the live operator view on the same bus.
type p6World struct {
	pw     *p4World
	svc    *dispatch.Service
	o6     Phase6Options
	drawn  bool
	webDir string
	hub    *live.Hub
	crops  *live.Crops
	ws     *httptest.Server
	apis   live.Services
}

func startP6World(t *testing.T) *p6World {
	t.Helper()
	pw := startP4World(t)
	srv, err := cot.ListenServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	centre := geo.Point{Lat: pw.zc.Site.CenterLat, Lon: pw.zc.Site.CenterLon}
	local := geo.NewLocal(centre)
	st := pw.s.Sites[0]
	stage := local.ToPoint(st.CenterE, st.CenterN-60)
	d := config.Dispatch{Listen: "127.0.0.1:0", TakServer: srv.Addr(), Callsign: "SAR-OPS", Group: "Cyan", BlockMarginM: 1,
		MarkerRefresh: config.Duration{Duration: 5 * time.Second}, MarkerStale: config.Duration{Duration: time.Minute},
		DroneEvery: config.Duration{Duration: time.Second}, ShowInvestigations: true, Triage: []string{"red", "yellow"}, ArriveM: 15,
		OnScene: config.Duration{Duration: 2 * time.Minute}, TimeScale: 20, TeamOffline: config.Duration{Duration: 30 * time.Second},
		Extrication: true, RescueTimeScale: 600, TakAckTimeout: config.Duration{Duration: 2 * time.Minute},
		StagingLat: stage.Lat, StagingLon: stage.Lon, MinAge: config.Duration{Duration: 5 * time.Second}, PublishInterval: config.Duration{Duration: 2 * time.Second},
		Teams: []config.Team{{ID: "team-1", Callsign: "TEAM-1", Mode: "foot", Source: "sim"}, {ID: "team-2", Callsign: "TEAM-2", Mode: "vehicle", Source: "sim"},
			{ID: "check-1", Callsign: "CHECK-1", Mode: "foot", Source: "tak"}}}
	pw.zc.Dispatch = d
	n, err := roads.Parse(roads.Synthetic(centre, 300, 60), local)
	if err != nil {
		t.Fatal(err)
	}
	blocks := dispatch.Blockages(pw.s, local)
	n.Block(blocks, d.BlockMarginM)
	svc, err := dispatch.Open(dispatch.Config{Incident: "inc", Centre: centre, GroundAltMSL: 80, Staging: stage, D: d, Roads: n, RoadsPath: "synthetic",
		Blockages: blocks, Scene: pw.s, Tick: 200 * time.Millisecond}, pw.m, pw.log)
	if err != nil {
		t.Fatal(err)
	}
	go svc.Run(pw.ctx)
	t.Cleanup(svc.Close)

	o := DefaultPhase5Options(pw.zc, pw.m, pw.o)
	o.TakAddr = srv.Addr()
	o.ReachWait = 5 * time.Minute
	o.Streets = n
	o.Dispatch = func() (*dispatch.Status, error) { s := svc.Status(); return &s, nil }
	o.DispatchReset = func() error { svc.Reset(); return nil }

	tmp := t.TempDir()
	webRoot := filepath.Join(tmp, "www")
	mapDir := filepath.Join(webRoot, "map", "inc")
	streetFile := filepath.Join(tmp, "streets.json")
	if err := os.WriteFile(streetFile, roads.Synthetic(centre, 300, 60), 0o644); err != nil {
		t.Fatal(err)
	}
	L, err := basemap.Build(basemap.Input{Incident: "inc", Centre: centre, RadiusM: 350, Staging: stage, StreetFile: streetFile, Network: n,
		MarginM: d.BlockMarginM, Blockages: blocks, Scene: pw.s})
	if err != nil {
		t.Fatal(err)
	}
	layers := filepath.Join(tmp, "layers.json")
	if err := L.Write(layers); err != nil {
		t.Fatal(err)
	}
	drawn := false
	if py, err := exec.LookPath("python3"); err == nil && exec.Command(py, "-c", "import PIL").Run() == nil {
		cmd := exec.Command(py, "../../tools/map/render_map.py", "--layers", layers, "--out", mapDir, "--mbtiles", filepath.Join(mapDir, "inc.mbtiles"),
			"--max-zoom", "18", "--legend", filepath.Join(mapDir, "legend.png"), "--overview", filepath.Join(mapDir, "overview.png"), "--jobs", "2")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("render_map.py: %v\n%s", err, b)
		}
		drawn = true
	} else {
		t.Log("python3 with Pillow not here: P6-02 not expected to pass")
	}
	scnDir := filepath.Join(tmp, "scenario")
	_ = os.MkdirAll(scnDir, 0o755)
	if b, err := json.Marshal(pw.s); err == nil {
		_ = os.WriteFile(filepath.Join(scnDir, "scenario.json"), b, 0o644)
	}
	// The services' status APIs, as their programs serve them.
	pj := protojson.MarshalOptions{UseProtoNames: true}
	api := func(f func() []byte) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(f())
		}))
		t.Cleanup(s.Close)
		return strings.TrimPrefix(s.URL, "http://")
	}
	apis := live.Services{
		Analyst: api(func() []byte { b, _ := pj.Marshal(&sarpb.CasualtyList{Casualties: pw.an.Casualties()}); return b }),
		Planner: api(func() []byte { b, _ := pj.Marshal(&sarpb.TaskList{Tasks: pw.pl.Tasks()}); return b }),
	}
	dmux := http.NewServeMux()
	dmux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, _ *http.Request) { b, _ := pj.Marshal(svc.State()); _, _ = w.Write(b) })
	dmux.HandleFunc("GET /v1/chats", func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(svc.Chats()) })
	ds := httptest.NewServer(dmux)
	t.Cleanup(ds.Close)
	apis.Dispatch = strings.TrimPrefix(ds.URL, "http://")
	// The live operator view on the same bus.
	hub := live.NewHub(live.HubOptions{Incident: "inc", Quiet: true})
	crops := &live.Crops{Dir: pw.anDir}
	lv, err := live.Start(live.Options{Incident: "inc", Bus: pw.m, Hub: hub, Crops: crops, Chats: apis.Chats})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lv.Close)
	hub.SetBus("connected")
	in := web.SceneInput{Incident: "inc", Version: "sar-web test", Centre: [2]float64{centre.Lat, centre.Lon}, Staging: [2]float64{stage.Lat, stage.Lon},
		RadiusM: 350, ScenarioDir: scnDir, MapDir: mapDir, Triage: d.Triage, TimeScale: d.TimeScale, Rescue: d.RescueTimeScale}
	for _, tm := range d.Teams {
		in.Teams = append(in.Teams, web.SceneTeam{ID: tm.ID, Callsign: tm.Callsign, Mode: tm.Mode, Source: tm.Source})
	}
	wsrv := &web.Server{Root: webRoot, Version: "sar-web test", Live: hub, Crops: crops, Scene: func() *web.Scene { return web.BuildScene(in) }, Present: web.NewPresenter()}
	var ws *httptest.Server
	if addr := os.Getenv("RUBBLESAR_DEV_SERVE"); addr != "" {
		// Development: serve the view on a fixed address to look at it in a browser.
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		ws = &httptest.Server{Listener: ln, Config: &http.Server{Handler: wsrv.Handler()}}
		ws.Start()
		t.Logf("operator view on http://%s/ops/", addr)
	} else {
		ws = httptest.NewServer(wsrv.Handler())
	}
	t.Cleanup(ws.Close)
	o6 := Phase6Options{P5: o, WebAddr: strings.TrimPrefix(ws.URL, "http://"), MapDir: mapDir, LayersFile: layers,
		ReportsDir: filepath.Join(webRoot, "reports", "inc"), ScenarioDir: scnDir,
		Chats: func() ([]aar.ChatLine, error) {
			var out []aar.ChatLine
			for _, c := range svc.Chats() {
				out = append(out, aar.ChatLine{At: c.At, From: c.From, To: c.To, Text: c.Text})
			}
			return out, nil
		},
		System: func(context.Context) []SystemRow {
			return []SystemRow{{Group: "ZBook", Name: "rescue-dispatch", Detail: "TAK connected"}, {Group: "Jetson (test@jetson)", Name: "edge-eye", Detail: "active"},
				{Group: "Drones", Name: "scout-1", Detail: "survey, battery 99%, edge-eye " + version.Version}}
		}}
	return &p6World{pw: pw, svc: svc, o6: o6, drawn: drawn, webDir: webRoot, hub: hub, crops: crops, ws: ws, apis: apis}
}

// The whole flow of phases 5 to 7 in one process, scored by the phase 7
// acceptance suite (which runs the phase 6 one inside).
func TestPhase7InProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("about 5 minutes")
	}
	w := startP6World(t)
	if os.Getenv("RUBBLESAR_DEV_STAGE") != "" {
		// Development: a blackout of the scout during its survey, for the demo's radio chapter.
		go func() {
			for w.pw.ctx.Err() == nil && len(w.hub.Snapshot().Casualties) == 0 {
				time.Sleep(500 * time.Millisecond)
			}
			time.Sleep(2 * time.Second)
			if err := SetLinkOutage(w.pw.m, "inc", "scout-1", 20, 3*time.Second); err != nil {
				t.Logf("staged blackout: %v", err)
			}
		}()
	}
	var out bytes.Buffer
	r := Phase7(w.pw.ctx, Phase7Options{P6: w.o6, Services: w.apis, AnalystDir: w.pw.anDir, Every: 10 * time.Second, StreamFor: 6 * time.Second}, &out)
	txt := out.String()
	t.Log("\n" + txt)
	t.Log("\n" + w.svc.Status().Text())
	want := []string{"[PASS] P7-01", "[PASS] P6-01", "[WARN] P5-01", "[PASS] P5-02", "[PASS] P5-03", "[PASS] P5-05", "[PASS] P5-06", "[PASS] P6-03",
		"[PASS] P7-02", "[PASS] P7-03", "[PASS] P7-04", "[PASS] P7-05"}
	if !w.drawn {
		want[0] = "[WARN] P7-01"
	}
	if w.drawn && strings.Contains(txt, "[FAIL] P6-02") {
		t.Error("P6-02 failed with the map drawn")
	}
	for _, x := range want {
		if !strings.Contains(txt, x) {
			t.Errorf("missing %q", x)
		}
	}
	if dir := r.Context["report"]; dir != "" {
		if b, err := os.ReadFile(filepath.Join(dir, "index.html")); err == nil {
			_ = os.WriteFile(filepath.Join(os.TempDir(), "rubblesar-p6-report.html"), b, 0o644)
		}
		if keep := os.Getenv("RUBBLESAR_DEV_KEEP"); keep != "" {
			// Development: keep the recorded run, the map and the scenario (for replays).
			_ = exec.Command("cp", "-a", w.webDir, keep).Run()
			_ = exec.Command("cp", "-a", w.pw.dir, filepath.Join(keep, "scenario")).Run() // rendered, for demo media
			t.Logf("kept the web folder in %s", keep)
		}
	}
	// P5-04 depends on the triage of six victims; it must not fail.
	if strings.Contains(txt, "[FAIL] P5-04") {
		t.Error("P5-04 failed")
	}
	if os.Getenv("P5_STRICT") != "" && !r.Passed() {
		t.Fatal("phase 7 in-process run failed")
	}
	if wait := os.Getenv("RUBBLESAR_DEV_LINGER"); wait != "" {
		d, _ := time.ParseDuration(wait)
		t.Logf("lingering %s", d)
		time.Sleep(d)
	}
}

// P5-04 flags a Yellow given a team while a Red waited, and nothing else.
func TestP5Order(t *testing.T) {
	cfg := &config.ZBook{}
	cfg.Dispatch.Triage = []string{"red", "yellow"}
	cfg.Dispatch.MinAge = config.Duration{Duration: 10 * time.Second}
	t0 := time.Date(2026, 2, 10, 9, 0, 0, 0, time.UTC)
	ns := func(s int) int64 { return t0.Add(time.Duration(s) * time.Second).UnixNano() }
	mk := func(id, cas string, prio uint32, created, needed int) *sarpb.Assignment {
		tri := sarpb.Triage_TRIAGE_RED
		if prio > 2 {
			tri = sarpb.Triage_TRIAGE_YELLOW
		}
		return &sarpb.Assignment{AssignmentId: id, CasualtyId: cas, Priority: prio, Triage: tri, CreatedUnixNs: ns(created), NeededUnixNs: ns(needed),
			State: sarpb.AssignmentState_ASSIGNMENT_STATE_DONE, OnSceneUnixNs: ns(created + 60)}
	}
	run := func(spans map[string][][2]time.Time, asg ...*sarpb.Assignment) Result {
		d := newP5Disp()
		for _, a := range asg {
			d.asg[a.AssignmentId] = a
			d.order = append(d.order, a.AssignmentId)
		}
		d.redSpans = spans
		col := newP4Collector("inc")
		l := &sarpb.CasualtyList{}
		for _, a := range asg {
			l.Casualties = append(l.Casualties, &sarpb.Casualty{CasualtyId: a.CasualtyId, Triage: a.Triage})
		}
		col.cas = l
		return p5Order(cfg, col, d, &p4run{})
	}
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	// C-002 turned Red at 100 s, after C-001 (Yellow) got its team at 50 s: fine.
	r := run(map[string][][2]time.Time{"C-002": {{at(100), at(400)}}}, mk("A1", "C-001", 3, 50, 30), mk("A2", "C-002", 1, 120, 100))
	if r.Status != Pass {
		t.Fatalf("in order: %+v", r)
	}
	// C-002 Red since 0 s and waiting while C-001 (Yellow) got a team at 50 s: an order problem.
	r = run(map[string][][2]time.Time{"C-002": {{at(0), at(400)}}}, mk("A1", "C-001", 3, 50, 30), mk("A2", "C-002", 1, 120, 0))
	if r.Status != Fail || !strings.Contains(strings.Join(r.Evidence, "\n"), "C-001 (Yellow) got a team") {
		t.Fatalf("out of order: %+v", r)
	}
	// The same, but rescue-dispatch says no free team could take C-002: a warning with its reason.
	j := mk("A1", "C-001", 3, 50, 30)
	j.Note = "more urgent, but not possible now: C-002 Red: no free team can reach it (casualty cut off by rubble)"
	r = run(map[string][][2]time.Time{"C-002": {{at(0), at(400)}}}, j, mk("A2", "C-002", 1, 120, 0))
	if r.Status != Warn || !strings.Contains(strings.Join(r.Evidence, "\n"), "cut off by rubble") {
		t.Fatalf("explained skip: %+v", r)
	}
	// C-014 had a team from 40 s and was then merged into C-015, so the
	// assignment now says C-015: C-014 was not waiting when C-001 (Yellow) got
	// its team at 50 s (the ZBook's phase 7 run of 21 Oct 2026 flagged this).
	d := newP5Disp()
	d.start = at(-10)
	for _, st := range []struct {
		t   int
		asg []*sarpb.Assignment
	}{{40, []*sarpb.Assignment{mk("A1", "C-014", 2, 40, 0)}}, {45, []*sarpb.Assignment{mk("A1", "C-015", 2, 40, 0)}},
		{50, []*sarpb.Assignment{mk("A1", "C-015", 2, 40, 0), mk("A2", "C-001", 3, 50, 30)}}} {
		b, _ := msg.Marshal(&sarpb.DispatchState{Assignments: st.asg})
		d.onState(bus.Sample{Payload: b, Received: at(st.t)})
	}
	d.redSpans = map[string][][2]time.Time{"C-014": {{at(0), at(400)}}}
	col := newP4Collector("inc")
	col.cas = &sarpb.CasualtyList{Casualties: []*sarpb.Casualty{{CasualtyId: "C-015", Triage: sarpb.Triage_TRIAGE_RED}, {CasualtyId: "C-001", Triage: sarpb.Triage_TRIAGE_YELLOW}}}
	if r := p5Order(cfg, col, d, &p4run{}); r.Status != Pass || strings.Join(d.cases["A1"], ",") != "C-014,C-015" {
		t.Fatalf("a Red that had a team before its merge: %+v; cases %v", r, d.cases)
	}
	// Red only until 20 s (then Black, never dispatched): fine.
	r = run(map[string][][2]time.Time{"C-003": {{at(0), at(20)}}}, mk("A1", "C-001", 3, 50, 30))
	if r.Status != Pass {
		t.Fatalf("red that went black: %+v", r)
	}
}

// A route whose walk off the streets crosses a river fails P5-03; the street
// part may cross it (on a bridge).
func TestP5RoutesRiver(t *testing.T) {
	loc := geo.NewLocal(geo.Point{Lat: 36.2, Lon: 36.16})
	gp := func(x, y float64) *sarpb.GeoPoint {
		p := loc.ToPoint(x, y)
		return &sarpb.GeoPoint{LatDeg: p.Lat, LonDeg: p.Lon}
	}
	water := &roads.Network{Source: "osm"}
	water.AddWater(geom.Poly{{X: 0, Y: -500}, {X: 0, Y: 500}}) // a river along x = 0
	run := func(pts [][2]float64, access uint32) Result {
		d := newP5Disp()
		r := &sarpb.RoutePlan{Mode: "foot", AccessIndex: access, LengthM: 300, DirectM: 250}
		for _, p := range pts {
			r.Points = append(r.Points, gp(p[0], p[1]))
		}
		last := pts[len(pts)-1]
		d.asg["A1"] = &sarpb.Assignment{AssignmentId: "A1", TeamId: "TEAM-1", CasualtyId: "C-001", Route: r, Target: gp(last[0], last[1])}
		d.order = []string{"A1"}
		tk := &p5tak{routes: map[string][][2]float64{"A1": nil}}
		return p5Routes(d, tk, nil, water, loc, nil)
	}
	// Street from x = -100 over the bridge to x = 100, then a walk on the east bank.
	if r := run([][2]float64{{-120, 0}, {-100, 0}, {100, 0}, {130, 20}}, 2); r.Status != Pass {
		t.Fatalf("bridge: %+v", r)
	}
	// The street part ends on the west bank and the walk to the casualty wades across.
	r := run([][2]float64{{-120, 0}, {-100, 0}, {-30, 0}, {40, 20}}, 2)
	if r.Status != Fail || !strings.Contains(strings.Join(r.Evidence, "\n"), "crosses a river") {
		t.Fatalf("wading: %+v", r)
	}
	// The walk from the team to the first street point wades across.
	r = run([][2]float64{{50, 0}, {-30, 0}, {-100, 0}, {-120, 20}}, 2)
	if r.Status != Fail {
		t.Fatalf("wading to the street: %+v", r)
	}
}
