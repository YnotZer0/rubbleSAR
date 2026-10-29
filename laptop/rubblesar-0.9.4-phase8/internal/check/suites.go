package check

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/edge"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/monitor"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// Opener opens a bus for a zenoh configuration (zenoh.Opener in production,
// a shared memory bus in unit tests).
type Opener func(ctx context.Context, z config.Zenoh) (bus.Bus, error)

// Env is what every suite needs.
type Env struct {
	Cfg  *config.ZBook
	Log  *slog.Logger
	Open Opener
	Out  io.Writer
}

func (e *Env) openRetry(ctx context.Context, z config.Zenoh) (bus.Bus, error) {
	return bus.OpenRetry(ctx, e.Log, z.OpenTimeout.Duration, false, func(ctx context.Context) (bus.Bus, error) {
		return e.Open(ctx, z)
	})
}

func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// exchange publishes n numbered messages from a to b on key and verifies
// that every one arrives, intact and in order.
func exchange(a, b bus.Bus, key string, n, size int, timeout time.Duration) Result {
	var mu sync.Mutex
	var got [][]byte
	done := make(chan struct{})
	sub, err := b.Subscribe(key, func(s bus.Sample) {
		mu.Lock()
		got = append(got, s.Payload)
		if len(got) == n {
			close(done)
		}
		mu.Unlock()
	})
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-009", Detail: "subscribe failed", Evidence: []string{err.Error()}}
	}
	defer sub.Close()
	time.Sleep(500 * time.Millisecond) // declaration propagation
	pub, err := a.DeclarePublisher(key, bus.QoSControl)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-009", Detail: "declare publisher failed", Evidence: []string{err.Error()}}
	}
	defer pub.Close()
	start := time.Now()
	for i := 0; i < n; i++ {
		p := make([]byte, size)
		copy(p, fmt.Sprintf("%08d", i))
		for j := 8; j < size; j++ {
			p[j] = byte(i + j)
		}
		if err := pub.Put(p); err != nil {
			return Result{Status: Fail, Code: "E-CHK-009", Detail: fmt.Sprintf("put %d failed", i), Evidence: []string{err.Error()}}
		}
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
	elapsed := time.Since(start)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != n {
		return Result{Status: Fail, Code: "E-CHK-009", Detail: fmt.Sprintf("received %d of %d messages", len(got), n)}
	}
	for i, p := range got {
		want := fmt.Sprintf("%08d", i)
		if len(p) != size || !bytes.HasPrefix(p, []byte(want)) || (size > 8 && p[size-1] != byte(i+size-1)) {
			return Result{Status: Fail, Code: "E-CHK-009", Detail: fmt.Sprintf("message %d corrupted or out of order", i)}
		}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%d x %d B delivered intact and in order in %v", n, size, elapsed.Round(time.Millisecond))}
}

// Loopback proves the local zenoh library works: two peer sessions on this
// machine exchange messages over 127.0.0.1. No router is needed.
func Loopback(ctx context.Context, e *Env) *Report {
	r := NewReport("loopback", e.Out)
	r.Context["test"] = "two zenoh peer sessions on 127.0.0.1"
	var a, b bus.Bus
	res := r.Run("LB-01", "Open two local zenoh sessions", func() Result {
		port, err := freeLoopbackPort()
		if err != nil {
			return Result{Status: Fail, Code: "E-CHK-009", Detail: "no free port", Evidence: []string{err.Error()}}
		}
		ep := fmt.Sprintf("tcp/127.0.0.1:%d", port)
		tmo := config.Duration{Duration: 10 * time.Second}
		if a, err = e.Open(ctx, config.Zenoh{Mode: "peer", Listen: []string{ep}, OpenTimeout: tmo}); err != nil {
			return Result{Status: Fail, Code: "E-CHK-009", Detail: "listener session failed", Evidence: []string{err.Error()}}
		}
		if b, err = e.Open(ctx, config.Zenoh{Mode: "peer", Connect: []string{ep}, OpenTimeout: tmo}); err != nil {
			return Result{Status: Fail, Code: "E-CHK-009", Detail: "connector session failed", Evidence: []string{err.Error()}}
		}
		return Result{Status: Pass, Detail: fmt.Sprintf("%s via %s", a.Info().Backend, ep)}
	})
	if res.Status == Pass {
		defer a.Close()
		defer b.Close()
		r.Run("LB-02", "Small messages in order", func() Result { return exchange(a, b, "sar/selftest/small", 500, 200, 10*time.Second) })
		r.Run("LB-03", "Large messages intact", func() Result { return exchange(a, b, "sar/selftest/large", 20, 256*1024, 20*time.Second) })
	} else {
		r.Skip("LB-02", "Small messages in order", "sessions did not open")
		r.Skip("LB-03", "Large messages intact", "sessions did not open")
	}
	return r
}

// Router proves the configured zenoh router routes messages: two client
// sessions connect to it and exchange messages through it.
func Router(ctx context.Context, e *Env) *Report {
	r := NewReport("router", e.Out)
	z := e.Cfg.Zenoh
	r.Context["zenoh"] = z.Mode + " -> " + z.Target()
	var a, b bus.Bus
	res := r.Run("RT-01", "Connect to zenoh router (twice)", func() Result {
		var err error
		if a, err = e.openRetry(ctx, z); err != nil {
			return Result{Status: Fail, Code: "E-CHK-001", Detail: "first session failed", Evidence: []string{err.Error()}}
		}
		if b, err = e.openRetry(ctx, z); err != nil {
			return Result{Status: Fail, Code: "E-CHK-001", Detail: "second session failed", Evidence: []string{err.Error()}}
		}
		return Result{Status: Pass, Detail: a.Info().Backend + " via " + z.Target()}
	})
	if res.Status != Pass {
		r.Skip("RT-02", "Messages routed through router", "not connected")
		return r
	}
	defer a.Close()
	defer b.Close()
	host, _ := os.Hostname()
	key := fmt.Sprintf("sar/selftest/router/%s/%d", sanitizeKey(host), time.Now().UnixNano())
	r.Run("RT-02", "Messages routed through router", func() Result { return exchange(a, b, key, 300, 512, 10*time.Second) })
	return r
}

func sanitizeKey(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, s)
}

// controller sends control messages and matches replies.
type controller struct {
	b        bus.Bus
	incident string
	seq      msg.Seq
	mu       sync.Mutex
	waiting  map[string]chan *sarpb.ControlReply
	sub      bus.Subscription
	rejected []string
}

func newController(b bus.Bus, incident string) (*controller, error) {
	c := &controller{b: b, incident: incident, waiting: map[string]chan *sarpb.ControlReply{}}
	sub, err := b.Subscribe(keys.ControlReplyAll(incident), c.onReply)
	if err != nil {
		return nil, err
	}
	c.sub = sub
	return c, nil
}

func waitKey(uav string, seq uint64) string { return fmt.Sprintf("%s#%d", uav, seq) }

func (c *controller) onReply(s bus.Sample) {
	var rep sarpb.ControlReply
	if err := msg.Unmarshal(s.Payload, &rep); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.waiting[waitKey(rep.GetUavId(), rep.GetInReplyToSeq())]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- &rep:
		default:
		}
	}
}

// send issues one command and waits for its reply.
func (c *controller) send(uav string, set func(*sarpb.Control), timeout time.Duration) (*sarpb.ControlReply, time.Duration, error) {
	seq := c.seq.Next()
	ctl := &sarpb.Control{Header: msg.NewHeader(c.incident, "zbook/sar-check", seq), UavId: uav}
	set(ctl)
	b, err := msg.Marshal(ctl)
	if err != nil {
		return nil, 0, err
	}
	ch := make(chan *sarpb.ControlReply, 1)
	k := waitKey(uav, seq)
	c.mu.Lock()
	c.waiting[k] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.waiting, k); c.mu.Unlock() }()
	start := time.Now()
	if err := c.b.Put(keys.Control(c.incident, uav), b, bus.QoSControl); err != nil {
		return nil, 0, err
	}
	select {
	case rep := <-ch:
		rtt := time.Since(start)
		if !rep.GetOk() {
			return rep, rtt, fmt.Errorf("drone rejected command: %s", rep.GetError())
		}
		return rep, rtt, nil
	case <-time.After(timeout):
		return nil, 0, fmt.Errorf("no reply from %s within %v", uav, timeout)
	}
}

func (c *controller) close() { _ = c.sub.Close() }

// SelfTestIncident derives the incident id used by self-tests.
func SelfTestIncident(inc string) string {
	const suffix = "-selftest"
	if len(inc)+len(suffix) > 32 {
		inc = inc[:32-len(suffix)]
	}
	return inc + suffix
}

// SelfTestEdgeConfig builds an in-process edge-eye configuration matching the
// ZBook's expectations, used by "sar-check phase0 --self-test".
func SelfTestEdgeConfig(z *config.ZBook) *config.Edge {
	ec := &config.Edge{}
	ec.Common = z.Common
	ec.Seed = 1
	budget := uint32(256)
	for i, id := range z.Check.ExpectUAVs {
		u := config.UAV{ID: id, Role: "investigator", Behaviour: "hover", AltAGLm: 15, SpeedMps: 8,
			TelemetryHz: z.Check.ExpectRateHz, EnduranceMin: 30, GPSNoiseM: 1.5, BudgetKbps: &budget,
			HomeOffsetEastM: float64(i) * 20, HomeOffsetNorthM: -200}
		if i == 0 {
			u.Role, u.Behaviour, u.AltAGLm, u.OrbitRadiusM = "scout", "orbit", 40, 150
		}
		ec.UAVs = append(ec.UAVs, u)
	}
	return ec
}

// Phase0Options tune the phase 0 suite.
type Phase0Options struct {
	// SelfTest runs simulated drones inside sar-check (no Jetson needed).
	SelfTest bool
	// Quick shortens the measurement windows (for development).
	Quick bool
}

// Phase0 is the phase 0 acceptance test: the ZBook sees every drone's
// telemetry at the right rate, intact, plausible and in time, can command
// each drone, and can throttle a drone's link.
func Phase0(ctx context.Context, e *Env, o Phase0Options) *Report {
	cfg := e.Cfg
	if o.SelfTest {
		// Use a separate incident id so a self-test never mixes with a running Jetson.
		c := *cfg
		c.Site.IncidentID = SelfTestIncident(cfg.Site.IncidentID)
		cfg = &c
	}
	suite := "phase0"
	if o.SelfTest {
		suite = "phase0-selftest"
	}
	r := NewReport(suite, e.Out)
	inc := cfg.Site.IncidentID
	r.Context["incident"] = inc
	r.Context["zenoh"] = cfg.Zenoh.Mode + " -> " + cfg.Zenoh.Target()
	r.Context["expect"] = strings.Join(cfg.Check.ExpectUAVs, ",")
	r.Context["mode"] = "Jetson (edge-eye)"
	if o.SelfTest {
		r.Context["mode"] = "self-test (simulated drones inside sar-check)"
	}
	window := cfg.Check.RateWindow.Duration
	throttleWindow := 15 * time.Second
	if o.Quick {
		window, throttleWindow = 8*time.Second, 8*time.Second
	}
	names := []struct{ id, name string }{
		{"CHK-01", "Connect to zenoh router"},
		{"CHK-02", "Discover expected drones"},
		{"CHK-03", "Telemetry rate"},
		{"CHK-04", "Message integrity"},
		{"CHK-05", "Telemetry plausibility"},
		{"CHK-06", "Clock offset"},
		{"CHK-07", "Control round trip"},
		{"CHK-08", "Link throttle"},
	}
	skipRest := func(from int, why string) {
		for _, n := range names[from:] {
			r.Skip(n.id, n.name, why)
		}
	}

	var b bus.Bus
	res := r.Run(names[0].id, names[0].name, func() Result {
		var err error
		b, err = e.openRetry(ctx, cfg.Zenoh)
		if err != nil {
			return Result{Status: Fail, Code: "E-CHK-001", Detail: "could not open a session", Evidence: []string{err.Error()}, Hint: diag.HintOf(err)}
		}
		return Result{Status: Pass, Detail: b.Info().Backend + " via " + cfg.Zenoh.Target()}
	})
	if res.Status != Pass {
		skipRest(1, "not connected")
		return r
	}
	defer b.Close()

	if o.SelfTest {
		eb, err := e.openRetry(ctx, cfg.Zenoh) // separate session so traffic crosses the router
		if err != nil {
			r.Skip("CHK-02", names[1].name, "self-test drones could not connect: "+err.Error())
			skipRest(2, "self-test drones not running")
			return r
		}
		defer eb.Close()
		rt, err := edge.New(SelfTestEdgeConfig(cfg), eb, e.Log.With("component", "self-test-edge"))
		if err != nil {
			r.Skip("CHK-02", names[1].name, "self-test drones failed: "+err.Error())
			skipRest(2, "self-test drones not running")
			return r
		}
		ectx, cancel := context.WithCancel(ctx)
		defer func() { cancel(); rt.Close() }()
		go func() { _ = rt.Run(ectx) }()
	}

	tr := monitor.NewTracker(inc, geo.Point{Lat: cfg.Site.CenterLat, Lon: cfg.Site.CenterLon})
	sub, err := b.Subscribe(keys.TelemetryAll(inc), tr.Handle)
	if err != nil {
		r.Skip("CHK-02", names[1].name, "subscribe failed: "+err.Error())
		skipRest(2, "no subscription")
		return r
	}
	defer sub.Close()
	ctl, err := newController(b, inc)
	if err != nil {
		r.Skip("CHK-02", names[1].name, "subscribe failed: "+err.Error())
		skipRest(2, "no subscription")
		return r
	}
	defer ctl.close()

	// CHK-02 discover
	res = r.Run(names[1].id, names[1].name, func() Result {
		deadline := time.Now().Add(cfg.Check.DiscoverTimeout.Duration)
		for {
			s := tr.Snapshot()
			var missing, seen []string
			for _, id := range cfg.Check.ExpectUAVs {
				if _, ok := s.Get(id); !ok {
					missing = append(missing, id)
				}
			}
			for _, u := range s.UAVs {
				seen = append(seen, u.ID)
			}
			if len(missing) == 0 {
				return Result{Status: Pass, Detail: "seen " + strings.Join(seen, ", ")}
			}
			if time.Now().After(deadline) {
				ev := []string{"missing: " + strings.Join(missing, ", "), "seen: " + orNone(seen),
					"listening on " + keys.TelemetryAll(inc)}
				if s.Mismatch > 0 && s.LastErr != nil {
					ev = append(ev, fmt.Sprintf("%d messages rejected, last: %v", s.Mismatch, s.LastErr))
				}
				return Result{Status: Fail, Code: "E-CHK-002", Detail: fmt.Sprintf("%d of %d drones not heard within %v", len(missing), len(cfg.Check.ExpectUAVs), cfg.Check.DiscoverTimeout.Duration), Evidence: ev}
			}
			select {
			case <-ctx.Done():
				return Result{Status: Fail, Code: "E-CHK-002", Detail: "interrupted"}
			case <-time.After(250 * time.Millisecond):
			}
		}
	})
	if res.Status != Pass {
		skipRest(2, "drones not discovered")
		return r
	}

	// Observation window shared by CHK-03..06.
	fmt.Fprintf(e.Out, "         observing telemetry for %v ...\n", window)
	tr.Reset()
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(window):
	}
	elapsed := time.Since(start)
	snap := tr.Snapshot()
	subDropped := sub.Dropped()

	r.Run(names[2].id, names[2].name, func() Result { return checkRate(cfg, snap, elapsed) })
	r.Run(names[3].id, names[3].name, func() Result { return checkIntegrity(cfg, snap, subDropped) })
	r.Run(names[4].id, names[4].name, func() Result { return checkPlausible(cfg, tr, snap) })
	r.Run(names[5].id, names[5].name, func() Result { return checkClock(cfg, snap) })
	r.Run(names[6].id, names[6].name, func() Result { return checkControl(cfg, ctl) })
	r.Run(names[7].id, names[7].name, func() Result { return checkThrottle(ctx, cfg, ctl, tr, throttleWindow) })
	return r
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "(none)"
	}
	return strings.Join(s, ", ")
}

func checkRate(cfg *config.ZBook, s monitor.Snapshot, elapsed time.Duration) Result {
	want := cfg.Check.ExpectRateHz
	tol := cfg.Check.RateTolerancePct / 100
	var ev []string
	bad := 0
	for _, id := range cfg.Check.ExpectUAVs {
		u, _ := s.Get(id)
		rate := float64(u.Count) / elapsed.Seconds()
		ok := math.Abs(rate-want) <= want*tol
		mark := "ok"
		if !ok {
			mark = "OUT OF TOLERANCE"
			bad++
		}
		ev = append(ev, fmt.Sprintf("%s: %.2f Hz (%d msgs in %.1fs) %s", id, rate, u.Count, elapsed.Seconds(), mark))
	}
	detail := fmt.Sprintf("expected %.2f Hz +/- %.0f%%", want, cfg.Check.RateTolerancePct)
	if bad > 0 {
		return Result{Status: Fail, Code: "E-CHK-003", Detail: fmt.Sprintf("%d drone(s) outside %s", bad, detail), Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail + " for all drones", Evidence: ev}
}

func checkIntegrity(cfg *config.ZBook, s monitor.Snapshot, subDropped uint64) Result {
	var ev []string
	fail := false
	if s.DecodeErr > 0 || s.Mismatch > 0 {
		fail = true
		ev = append(ev, fmt.Sprintf("decode errors %d, rejected (incident/key) %d, last error: %v", s.DecodeErr, s.Mismatch, s.LastErr))
	}
	if subDropped > 0 {
		fail = true
		ev = append(ev, fmt.Sprintf("%d messages dropped inside sar-check (CPU overloaded?)", subDropped))
	}
	var total, gaps uint64
	for _, id := range cfg.Check.ExpectUAVs {
		u, _ := s.Get(id)
		total += u.Count
		gaps += u.Gaps
		if u.Gaps > 0 || u.OutOfOrder > 0 || u.Restarts > 0 {
			ev = append(ev, fmt.Sprintf("%s: %d missing, %d out of order, %d restarts", id, u.Gaps, u.OutOfOrder, u.Restarts))
		}
		if u.OutOfOrder > 0 || u.Restarts > 0 {
			fail = true
		}
	}
	lossPct := 0.0
	if total+gaps > 0 {
		lossPct = 100 * float64(gaps) / float64(total+gaps)
	}
	if lossPct > 1 {
		fail = true
	}
	detail := fmt.Sprintf("%d messages, %.2f%% lost, %d decode errors", total, lossPct, s.DecodeErr)
	if fail {
		return Result{Status: Fail, Code: "E-CHK-004", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail}
}

func checkPlausible(cfg *config.ZBook, tr *monitor.Tracker, s monitor.Snapshot) Result {
	var ev []string
	limit := cfg.Site.RadiusM * 1.5
	for _, id := range cfg.Check.ExpectUAVs {
		u, _ := s.Get(id)
		t := u.Latest
		if t == nil || t.Position == nil {
			ev = append(ev, id+": no position")
			continue
		}
		d := tr.DistanceFromCenter(u)
		switch {
		case d > limit:
			ev = append(ev, fmt.Sprintf("%s: %.0f m from the incident centre (limit %.0f m)", id, d, limit))
		case t.BatteryPct < 0 || t.BatteryPct > 100:
			ev = append(ev, fmt.Sprintf("%s: battery %.1f%%", id, t.BatteryPct))
		case t.AltAglM < 0 || t.AltAglM > 130:
			ev = append(ev, fmt.Sprintf("%s: altitude %.1f m AGL", id, t.AltAglM))
		case t.State == sarpb.FlightState_FLIGHT_STATE_UNSPECIFIED:
			ev = append(ev, id+": flight state missing")
		case t.GpsHaccM <= 0:
			ev = append(ev, id+": GPS accuracy missing")
		}
	}
	if len(ev) > 0 {
		return Result{Status: Fail, Code: "E-CHK-005", Detail: fmt.Sprintf("%d problem(s)", len(ev)), Evidence: ev}
	}
	var sum []string
	for _, id := range cfg.Check.ExpectUAVs {
		u, _ := s.Get(id)
		sum = append(sum, fmt.Sprintf("%s %s %.0fm", id, strings.TrimPrefix(strings.ToLower(u.Latest.State.String()), "flight_state_"), tr.DistanceFromCenter(u)))
	}
	return Result{Status: Pass, Detail: strings.Join(sum, "; ")}
}

func checkClock(cfg *config.ZBook, s monitor.Snapshot) Result {
	var all []float64
	var ev []string
	for _, id := range cfg.Check.ExpectUAVs {
		u, _ := s.Get(id)
		all = append(all, u.LatencyMs...)
		ev = append(ev, fmt.Sprintf("%s: median receive minus send %.1f ms", id, u.MedianLatencyMs()))
	}
	if len(all) == 0 {
		return Result{Status: Fail, Code: "E-CHK-006", Detail: "no samples"}
	}
	sort.Float64s(all)
	med := all[len(all)/2]
	limit := float64(cfg.Check.MaxClockOffset.Milliseconds())
	detail := fmt.Sprintf("median receive-minus-send %.1f ms (limit %.0f ms; includes network delay and clock offset)", med, limit)
	switch {
	case math.Abs(med) <= limit:
		return Result{Status: Pass, Detail: detail}
	case math.Abs(med) <= 2000:
		return Result{Status: Warn, Code: "E-CHK-006", Detail: detail, Evidence: ev}
	default:
		return Result{Status: Fail, Code: "E-CHK-006", Detail: detail, Evidence: ev}
	}
}

func checkControl(cfg *config.ZBook, c *controller) Result {
	var ev []string
	var rtts []time.Duration
	failed := 0
	for _, id := range cfg.Check.ExpectUAVs {
		for i := 0; i < 5; i++ {
			nonce := rand.Uint64()
			rep, rtt, err := c.send(id, func(m *sarpb.Control) { m.Cmd = &sarpb.Control_Ping{Ping: &sarpb.Ping{Nonce: nonce}} }, cfg.Check.PingTimeout.Duration)
			if err == nil && rep.GetPong().GetNonce() != nonce {
				err = fmt.Errorf("pong carried nonce %d, sent %d", rep.GetPong().GetNonce(), nonce)
			}
			if err != nil {
				failed++
				ev = append(ev, fmt.Sprintf("%s ping %d: %v", id, i+1, err))
				continue
			}
			rtts = append(rtts, rtt)
		}
	}
	if failed > 0 {
		return Result{Status: Fail, Code: "E-CHK-007", Detail: fmt.Sprintf("%d of %d pings failed", failed, 5*len(cfg.Check.ExpectUAVs)), Evidence: ev}
	}
	sort.Slice(rtts, func(i, j int) bool { return rtts[i] < rtts[j] })
	return Result{Status: Pass, Detail: fmt.Sprintf("%d pings, round trip min %v / median %v / max %v", len(rtts),
		rtts[0].Round(100*time.Microsecond), rtts[len(rtts)/2].Round(100*time.Microsecond), rtts[len(rtts)-1].Round(100*time.Microsecond))}
}

// throttleBps is the budget used by CHK-08: 50 bytes per second, about a third
// of one telemetry message per second, so the effect is unmistakable.
const throttleBps = 400

func checkThrottle(ctx context.Context, cfg *config.ZBook, c *controller, tr *monitor.Tracker, window time.Duration) Result {
	id := cfg.Check.ExpectUAVs[0]
	timeout := cfg.Check.PingTimeout.Duration
	rep, _, err := c.send(id, func(m *sarpb.Control) { m.Cmd = &sarpb.Control_GetStatus{GetStatus: &sarpb.GetStatus{}} }, timeout)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-008", Detail: "could not read current budget", Evidence: []string{err.Error()}}
	}
	original := rep.GetLink().GetBudgetBps()
	var ev []string
	restore := func() {
		rep, _, err := c.send(id, func(m *sarpb.Control) {
			m.Cmd = &sarpb.Control_SetLinkBudget{SetLinkBudget: &sarpb.SetLinkBudget{Bps: original}}
		}, timeout)
		if err != nil || rep.GetLink().GetBudgetBps() != original {
			ev = append(ev, fmt.Sprintf("WARNING: could not restore %s budget to %d bps (%v); restart edge-eye", id, original, err))
		}
	}
	rep, _, err = c.send(id, func(m *sarpb.Control) {
		m.Cmd = &sarpb.Control_SetLinkBudget{SetLinkBudget: &sarpb.SetLinkBudget{Bps: throttleBps}}
	}, timeout)
	if err != nil || rep.GetLink().GetBudgetBps() != throttleBps {
		ev = append(ev, fmt.Sprint(err))
		restore()
		return Result{Status: Fail, Code: "E-CHK-008", Detail: "drone did not accept the new budget", Evidence: ev}
	}

	select { // let any saved allowance drain
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}
	before, _ := tr.Snapshot().Get(id)
	start := time.Now()
	select {
	case <-ctx.Done():
	case <-time.After(window):
	}
	elapsed := time.Since(start)
	after, _ := tr.Snapshot().Get(id)

	n := after.Count - before.Count
	rate := float64(n) / elapsed.Seconds()
	avgSize := 150.0
	if after.Count > 0 {
		avgSize = float64(after.Bytes) / float64(after.Count)
	}
	expected := math.Min(cfg.Check.ExpectRateHz, float64(throttleBps)/8/avgSize)
	reported := after.Latest.GetLink().GetBudgetBps()
	droppedBefore := before.Latest.GetLink().GetDroppedMsgs()
	droppedAfter := after.Latest.GetLink().GetDroppedMsgs()
	ev = append(ev,
		fmt.Sprintf("%s: %.2f Hz while throttled to %d bit/s (expected about %.2f Hz for %.0f-byte messages, normally %.2f Hz)", id, rate, throttleBps, expected, avgSize, cfg.Check.ExpectRateHz),
		fmt.Sprintf("drone reports budget %d bit/s, %d messages dropped during the test", reported, droppedAfter-droppedBefore))
	ok := rate <= expected*1.3+0.07 && rate >= expected*0.5 && reported == throttleBps && droppedAfter > droppedBefore
	restore()
	if !ok {
		return Result{Status: Fail, Code: "E-CHK-008", Detail: "telemetry rate did not follow the budget", Evidence: ev}
	}
	if len(ev) > 2 { // restore failed
		return Result{Status: Warn, Code: "E-CHK-008", Detail: "throttle worked but the original budget was not restored", Evidence: ev}
	}
	restored := "unlimited"
	if original > 0 {
		restored = fmt.Sprintf("%d bit/s", original)
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%s fell from %.2f to %.2f Hz at %d bit/s, then restored to %s", id, cfg.Check.ExpectRateHz, rate, throttleBps, restored), Evidence: ev}
}
