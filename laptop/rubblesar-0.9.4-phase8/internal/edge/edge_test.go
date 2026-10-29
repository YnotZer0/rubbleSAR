package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/monitor"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

func testConfig() *config.Edge {
	b := uint32(256)
	c := &config.Edge{}
	c.Site = config.Site{IncidentID: "inc", CenterLat: 36.2025, CenterLon: 36.16056, GroundAltMSLm: 80, RadiusM: 805}
	c.Seed = 1
	for _, u := range []config.UAV{
		{ID: "scout-1", Role: "scout", Behaviour: "orbit", AltAGLm: 40, OrbitRadiusM: 150},
		{ID: "inv-1", Role: "investigator", Behaviour: "hover", AltAGLm: 15},
		{ID: "inv-2", Role: "investigator", Behaviour: "landed"},
	} {
		u.SpeedMps, u.TelemetryHz, u.EnduranceMin, u.GPSNoiseM, u.BudgetKbps = 8, 5, 30, 1.5, &b
		c.UAVs = append(c.UAVs, u)
	}
	return c
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func start(t *testing.T) (*Runtime, *bus.Memory, context.CancelFunc) {
	t.Helper()
	m := bus.NewMemory()
	r, err := New(testConfig(), m, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Run(ctx) }()
	t.Cleanup(func() { cancel(); r.Close(); m.Close() })
	return r, m, cancel
}

func TestTelemetryFlows(t *testing.T) {
	_, m, _ := start(t)
	tr := monitor.NewTracker("inc", geo.Point{Lat: 36.2025, Lon: 36.16056})
	if _, err := m.Subscribe(keys.TelemetryAll("inc"), tr.Handle); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond) // 5 Hz for ~2 s = ~10 per drone
	s := tr.Snapshot()
	if len(s.UAVs) != 3 {
		t.Fatalf("saw %d drones, want 3", len(s.UAVs))
	}
	for _, u := range s.UAVs {
		if u.Count < 8 || u.Count > 12 || u.Gaps != 0 || u.OutOfOrder != 0 {
			t.Errorf("%s: count=%d gaps=%d ooo=%d", u.ID, u.Count, u.Gaps, u.OutOfOrder)
		}
		if d := tr.DistanceFromCenter(u); d < 0 || d > 805 {
			t.Errorf("%s: %v m from centre", u.ID, d)
		}
	}
	if s.DecodeErr != 0 || s.Mismatch != 0 {
		t.Fatalf("decode=%d mismatch=%d", s.DecodeErr, s.Mismatch)
	}
}

// controlRoundTrip sends one Control and waits for its reply.
func controlRoundTrip(t *testing.T, m *bus.Memory, c *sarpb.Control) *sarpb.ControlReply {
	t.Helper()
	ch := make(chan *sarpb.ControlReply, 4)
	sub, err := m.Subscribe(keys.ControlReply("inc", c.UavId), func(s bus.Sample) {
		var r sarpb.ControlReply
		if msg.Unmarshal(s.Payload, &r) == nil {
			ch <- &r
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	b, _ := msg.Marshal(c)
	if err := m.Put(keys.Control("inc", c.UavId), b, bus.QoSControl); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no control reply")
	}
	return nil
}

func TestControlPingBudgetStatus(t *testing.T) {
	r, m, _ := start(t)
	ping := &sarpb.Control{Header: msg.NewHeader("inc", "test", 41), UavId: "inv-1",
		Cmd: &sarpb.Control_Ping{Ping: &sarpb.Ping{Nonce: 12345}}}
	rep := controlRoundTrip(t, m, ping)
	if !rep.Ok || rep.InReplyToSeq != 41 || rep.GetPong().GetNonce() != 12345 || rep.GetPong().GetRecvUnixNs() == 0 {
		t.Fatalf("ping reply %+v", rep)
	}

	set := &sarpb.Control{Header: msg.NewHeader("inc", "test", 42), UavId: "inv-1",
		Cmd: &sarpb.Control_SetLinkBudget{SetLinkBudget: &sarpb.SetLinkBudget{Bps: 400}}}
	rep = controlRoundTrip(t, m, set)
	if !rep.Ok || rep.GetLink().GetBudgetBps() != 400 {
		t.Fatalf("budget reply %+v", rep)
	}
	for _, d := range r.Status().Drones {
		want := uint32(256000)
		if d.ID == "inv-1" {
			want = 400
		}
		if d.BudgetBps != want {
			t.Errorf("%s budget %d want %d", d.ID, d.BudgetBps, want)
		}
	}

	st := &sarpb.Control{Header: msg.NewHeader("inc", "test", 43), UavId: "inv-1",
		Cmd: &sarpb.Control_GetStatus{GetStatus: &sarpb.GetStatus{}}}
	if rep = controlRoundTrip(t, m, st); !rep.Ok || rep.GetLink().GetBudgetBps() != 400 {
		t.Fatalf("status reply %+v", rep)
	}
}

func TestControlRejected(t *testing.T) {
	_, m, _ := start(t)
	cases := []struct {
		c    *sarpb.Control
		code string
	}{
		{&sarpb.Control{Header: msg.NewHeader("other", "test", 1), UavId: "inv-1", Cmd: &sarpb.Control_Ping{Ping: &sarpb.Ping{}}}, "E-MSG-003"},
		{&sarpb.Control{Header: msg.NewHeader("inc", "test", 2), UavId: "inv-1"}, "E-EDGE-003"},
	}
	for _, c := range cases {
		rep := controlRoundTrip(t, m, c.c)
		if rep.Ok || !strings.HasPrefix(rep.Error, c.code) {
			t.Errorf("want rejection %s, got ok=%v err=%q", c.code, rep.Ok, rep.Error)
		}
	}
}

func TestBudgetThrottlesTelemetry(t *testing.T) {
	r, m, _ := start(t)
	if _, err := r.SetBudget("scout-1", 400); err != nil { // 50 B/s
		t.Fatal(err)
	}
	if _, err := r.SetBudget("nope", 1); err == nil {
		t.Fatal("unknown drone accepted")
	}
	var mu sync.Mutex
	var n, size int
	_, _ = m.Subscribe(keys.Telemetry("inc", "scout-1"), func(s bus.Sample) {
		mu.Lock()
		n++
		size = len(s.Payload)
		mu.Unlock()
	})
	time.Sleep(4 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	// Offered 5 Hz; allowed about 50/size per second (size ~150 B -> ~0.33 Hz).
	maxAllowed := int(4*50.0/float64(size)) + 2
	if n > maxAllowed {
		t.Fatalf("throttled drone sent %d messages of %d B in 4 s; budget allows about %d", n, size, maxAllowed)
	}
	for _, d := range r.Status().Drones {
		if d.ID == "scout-1" && d.DroppedMsgs == 0 {
			t.Fatal("throttled drone reports no dropped messages")
		}
	}
}

func TestAdminAPI(t *testing.T) {
	r, _, _ := start(t)
	a, err := StartAdmin("127.0.0.1:0", quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	base := "http://" + a.Addr()

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz before running = %d", resp.StatusCode)
	}
	a.SetRuntime(r)
	a.SetState("running", nil)
	resp, _ = http.Get(base + "/healthz")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	post := func(body string) (int, BudgetResponse) {
		resp, err := http.Post(base+"/v1/link/budget", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var br BudgetResponse
		_ = json.NewDecoder(resp.Body).Decode(&br)
		return resp.StatusCode, br
	}
	if code, br := post(`{"uav":"all","kbps":64}`); code != 200 || !br.OK || br.Bps != 64000 || len(br.UAVs) != 3 {
		t.Fatalf("set all: %d %+v", code, br)
	}
	if code, br := post(`{"uav":"inv-2","bps":400}`); code != 200 || br.UAVs[0] != "inv-2" {
		t.Fatalf("set one: %d %+v", code, br)
	}
	for body, want := range map[string]int{
		`{"uav":"ghost","kbps":1}`:       404,
		`{"uav":"all"}`:                  400,
		`{"uav":"all","kbps":1,"bps":1}`: 400,
		`{"uav":"all","kpbs":1}`:         400,
		`not json`:                       400,
	} {
		if code, br := post(body); code != want || br.Error == "" {
			t.Errorf("%s: code %d (want %d) err=%q", body, code, want, br.Error)
		}
	}

	resp, err = http.Get(base + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc statusDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.State != "running" || doc.Runtime == nil || len(doc.Runtime.Drones) != 3 {
		t.Fatalf("status doc %+v", doc)
	}
	for _, d := range doc.Runtime.Drones {
		want := uint32(64000)
		if d.ID == "inv-2" {
			want = 400
		}
		if d.BudgetBps != want {
			t.Errorf("%s budget %d want %d", d.ID, d.BudgetBps, want)
		}
	}
}

func TestAdminPortClash(t *testing.T) {
	a, err := StartAdmin("127.0.0.1:0", quietLog())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := StartAdmin(a.Addr(), quietLog()); err == nil || !strings.Contains(err.Error(), "E-EDGE-001") {
		t.Fatalf("want E-EDGE-001, got %v", err)
	}
}
