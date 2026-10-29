package ops

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/dispatch"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

type fakeSys struct {
	mu     sync.Mutex
	calls  []string
	active map[string]string
	ssh    []string
}

func (f *fakeSys) Systemctl(_ context.Context, action string, units ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, action+" "+strings.Join(units, " "))
	return nil
}

func (f *fakeSys) Active(_ context.Context, unit string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.active[unit]; ok {
		return s
	}
	return "active"
}

func (f *fakeSys) SSH(_ context.Context, target, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ssh = append(f.ssh, target+": "+command)
	return "active\n0.7.0-phase6\n", nil
}

// world: fake services on HTTP, TCP listeners for zenohd and taky, the memory bus.
type world struct {
	cfg    *config.ZBook
	sys    *fakeSys
	b      *bus.Memory
	disp   *httptest.Server
	mu     sync.Mutex
	state  *sarpb.DispatchState
	resets []string
}

func newWorld(t *testing.T) *world {
	cfg, err := config.LoadZBook("../../configs/zbook/sar.toml")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{cfg: cfg, sys: &fakeSys{active: map[string]string{}}, b: bus.NewMemory()}
	t.Cleanup(func() { w.b.Close() })
	serve := func(h http.HandlerFunc) string {
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		return strings.TrimPrefix(s.URL, "http://")
	}
	jsonOK := func(v any) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.mu.Lock()
				w.resets = append(w.resets, r.URL.Path+"?"+r.URL.RawQuery)
				w.mu.Unlock()
			}
			_ = json.NewEncoder(rw).Encode(v)
		}
	}
	cfg.Analyst.Listen = serve(jsonOK(map[string]any{"version": "casualty-analyst 0.7.0-phase6", "state": "running", "casualties": map[string]int{"n": 0}}))
	cfg.Planner.Listen = serve(jsonOK(map[string]any{"version": "mission-planner 0.7.0-phase6", "state": "running"}))
	cfg.Dispatch.Listen = serve(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.mu.Lock()
			w.resets = append(w.resets, "dispatch "+r.URL.Path)
			w.mu.Unlock()
			_ = json.NewEncoder(rw).Encode(map[string]any{"ok": true})
		case r.URL.Path == "/v1/chats":
			_ = json.NewEncoder(rw).Encode([]aar.ChatLine{{At: time.Now(), From: "SAR-OPS", To: "All Chat Rooms", Text: "TEAM-1 → C-001 RED: 400 m."}})
		default:
			_ = json.NewEncoder(rw).Encode(map[string]any{"version": "rescue-dispatch 0.7.0-phase6", "dispatch": dispatch.Status{Run: "r1",
				TAK: dispatch.TAKStatus{Connected: true, Users: []string{"VADER"}}, Roads: dispatch.RoadsStatus{Loaded: true, Source: "osm", LengthKm: 120},
				Teams: []dispatch.TeamRow{{ID: "team-1", State: "idle"}, {ID: "vader", State: "offline"}}}})
		}
	})
	cfg.Web.Listen = serve(jsonOK(map[string]any{"version": "sar-web", "maps": []map[string]any{{"incident": cfg.Site.IncidentID, "tiles": 1877}}, "reports": 2}))
	listen := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		return ln.Addr().String()
	}
	cfg.Zenoh.Connect = []string{"tcp/" + listen()}
	cfg.Dispatch.TakServer = listen()
	cfg.Web.Root = t.TempDir()
	cfg.Site.ScenarioDir = t.TempDir()
	cfg.Check.ExpectUAVs = []string{"scout-1"}
	cfg.Ops.Jetson = "nvidia@192.0.2.60"
	return w
}

// drone publishes scout-1's telemetry: a survey of steps that ends after `fly`.
func (w *world) drone(ctx context.Context, fly time.Duration) {
	var seq msg.Seq
	start := time.Now()
	for ctx.Err() == nil {
		el := time.Since(start)
		step := uint32(min(3, int(el*3/fly)))
		t := &sarpb.Telemetry{Header: msg.NewHeader(w.cfg.Site.IncidentID, "uav/scout-1", seq.Next()), UavId: "scout-1", Role: sarpb.UavRole_UAV_ROLE_SCOUT,
			Position: &sarpb.GeoPoint{LatDeg: 36.2016 + float64(step)*1e-4, LonDeg: 36.1585}, State: sarpb.FlightState_FLIGHT_STATE_SURVEY, BatteryPct: 90,
			Software: "edge-eye 0.7.0-phase6 (commit t)", Perception: &sarpb.PerceptionStats{MissionStep: step, MissionSteps: 3, MissionDone: el >= fly}}
		b, _ := msg.Marshal(t)
		_ = w.b.Put(keys.Telemetry(w.cfg.Site.IncidentID, "scout-1"), b, bus.PubOptions{})
		l := &sarpb.CasualtyList{Header: msg.NewHeader(w.cfg.Site.IncidentID, "zbook/casualty-analyst", seq.Next()),
			Casualties: []*sarpb.Casualty{{CasualtyId: "C-001", Triage: sarpb.Triage_TRIAGE_RED, Position: &sarpb.GeoPoint{LatDeg: 36.2040, LonDeg: 36.1611}}}}
		b, _ = msg.Marshal(l)
		_ = w.b.Put(keys.Casualties(w.cfg.Site.IncidentID), b, bus.PubOptions{})
		st := &sarpb.DispatchState{Header: msg.NewHeader(w.cfg.Site.IncidentID, "zbook/rescue-dispatch", seq.Next()), Assignments: []*sarpb.Assignment{{AssignmentId: "A-1",
			CasualtyId: "C-001", TeamId: "team-1", State: sarpb.AssignmentState_ASSIGNMENT_STATE_DONE, CreatedUnixNs: start.UnixNano(), OnSceneUnixNs: start.UnixNano(),
			DoneUnixNs: time.Now().UnixNano(), Route: &sarpb.RoutePlan{LengthM: 400}}}}
		b, _ = msg.Marshal(st)
		_ = w.b.Put(keys.Dispatch(w.cfg.Site.IncidentID), b, bus.PubOptions{})
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStatusUpDown(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.drone(ctx, time.Hour)
	e := &Env{Cfg: w.cfg, Sys: w.sys, Bus: w.b, TelWait: 2 * time.Second}
	st := GetStatus(ctx, e)
	txt := st.Text()
	t.Log("\n" + txt)
	for _, want := range []string{"RESULT: everything up", "TAK connected (users VADER)", "2 teams (1 free, 1 offline)", "map 1877 tiles; 2 reports",
		"Jetson (nvidia@192.0.2.60)", "active, 0.7.0-phase6", "scout-1", "edge-eye 0.7.0-phase6 (commit t)"} {
		if !strings.Contains(txt, want) {
			t.Errorf("status misses %q", want)
		}
	}
	// A stopped service is a problem with a fix.
	w.sys.active["mission-planner"] = "inactive"
	st = GetStatus(ctx, e)
	if st.Problems() != 1 || !strings.Contains(st.Text(), "mission-planner    FAIL  not running") {
		t.Fatalf("stopped service:\n%s", st.Text())
	}
	delete(w.sys.active, "mission-planner")
	var out bytes.Buffer
	if err := Up(ctx, e, &out, UpOptions{Wait: 5 * time.Second}); err != nil {
		t.Fatalf("up: %v\n%s", err, out.String())
	}
	if len(w.sys.calls) != 1 || w.sys.calls[0] != "start zenohd casualty-analyst mission-planner rubblesar-taky rescue-dispatch sar-web" ||
		!strings.Contains(strings.Join(w.sys.ssh, "\n"), "sudo -n systemctl start edge-eye") || !strings.Contains(out.String(), "all 1 drones report") {
		t.Fatalf("up: %v %v\n%s", w.sys.calls, w.sys.ssh, out.String())
	}
	out.Reset()
	if err := Down(ctx, e, &out, DownOptions{KeepTAK: true}); err != nil {
		t.Fatal(err)
	}
	if w.sys.calls[1] != "stop sar-web rescue-dispatch mission-planner casualty-analyst zenohd" || !strings.Contains(w.sys.ssh[len(w.sys.ssh)-1], "sudo -n systemctl stop edge-eye") {
		t.Fatalf("down: %v %v", w.sys.calls, w.sys.ssh)
	}
}

func TestMission(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.drone(ctx, 1500*time.Millisecond)
	e := &Env{Cfg: w.cfg, Sys: w.sys, Bus: w.b}
	restarted := ""
	var out bytes.Buffer
	res, err := Mission(ctx, e, &out, MissionOptions{Max: 30 * time.Second, Settle: 500 * time.Millisecond, Every: 300 * time.Millisecond, MinRun: time.Millisecond,
		Restart: func(_ bus.Bus, _ string, uavs []string, _ time.Duration) error {
			restarted = strings.Join(uavs, ",")
			return nil
		}})
	t.Log("\n" + out.String())
	if err != nil {
		t.Fatal(err)
	}
	if res.Partial || !strings.HasPrefix(res.Reason, "finished") || restarted != "scout-1" {
		t.Fatalf("result %+v restarted %q", res, restarted)
	}
	w.mu.Lock()
	resets := strings.Join(w.resets, " ")
	w.mu.Unlock()
	if !strings.Contains(resets, "/v1/reset?stamp=") || !strings.Contains(resets, "-before-mission") || !strings.Contains(resets, "dispatch /v1/reset") {
		t.Fatalf("resets %q", resets)
	}
	if res.Report == nil || res.Report.KPI.Rescued != 1 || len(res.Report.Chats) == 0 || res.Report.Meta.Run != "r1" {
		t.Fatalf("report %+v", res.Report)
	}
	for _, f := range []string{"index.html", "events.jsonl", "meta.json", "summary.json"} {
		if _, err := os.Stat(filepath.Join(res.Dir, f)); err != nil {
			t.Error(err)
		}
	}
	if !strings.HasPrefix(res.Dir, w.cfg.ReportsDir()) || len(Runs(w.cfg.ReportsDir())) != 1 {
		t.Fatalf("dir %s runs %v", res.Dir, Runs(w.cfg.ReportsDir()))
	}
	// Rebuilding the report from the recording.
	if rep, err := WriteReport(w.cfg.MapDir(), w.cfg.Site.Scenario(), res.Dir, nil); err != nil || rep.KPI.Rescued != 1 {
		t.Fatalf("rebuild: %v", err)
	}
}
