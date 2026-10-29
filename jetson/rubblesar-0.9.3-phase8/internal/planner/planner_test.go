package planner

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "google.golang.org/protobuf/proto"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

var (
	centre = geo.Point{Lat: 36.2, Lon: 36.16}
	local  = geo.NewLocal(centre)
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(state string) Config {
	return Config{Incident: "inc", Centre: centre, StateFile: state, Tick: 30 * time.Millisecond, Settle: 300 * time.Millisecond,
		OnlineAfter: time.Second, MinAge: 100 * time.Millisecond,
		P: config.Planner{AltAGLm: 15, HoverS: 1, RadiusM: 6, ReservePct: 25, MaxAttempts: 2, TaskTimeout: config.Duration{Duration: 2 * time.Second},
			CommandTimeout: config.Duration{Duration: 300 * time.Millisecond}, PublishInterval: config.Duration{Duration: time.Second}}}
}

// fake is a drone: it answers Investigate and CancelTask, flies a task in
// a few hundred milliseconds and shows it in telemetry.
type fake struct {
	mu       sync.Mutex
	id       string
	role     sarpb.UavRole
	e, n     float64
	battery  float32
	state    sarpb.FlightState
	task     *sarpb.TaskStatus
	got      []string // task ids received
	refuse   string   // error to answer with
	silent   bool     // do not answer
	abort    bool     // abort the next task when on station
	slow     bool     // stay en route
	stop     chan struct{}
	tel, rep bus.Publisher
}

func newFake(t *testing.T, m *bus.Memory, id string, role sarpb.UavRole, e, n float64, battery float32) *fake {
	f := &fake{id: id, role: role, e: e, n: n, battery: battery, state: sarpb.FlightState_FLIGHT_STATE_LANDED, stop: make(chan struct{})}
	f.tel, _ = m.DeclarePublisher(keys.Telemetry("inc", id), bus.QoSTelemetry)
	f.rep, _ = m.DeclarePublisher(keys.ControlReply("inc", id), bus.QoSControl)
	var seq msg.Seq
	m.Subscribe(keys.Control("inc", id), func(s bus.Sample) {
		var c sarpb.Control
		if msg.Unmarshal(s.Payload, &c) != nil {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.silent {
			return
		}
		r := &sarpb.ControlReply{Header: msg.NewHeader("inc", "uav/"+id, seq.Next()), UavId: id, InReplyToSeq: c.Header.Seq, Ok: true}
		switch cmd := c.Cmd.(type) {
		case *sarpb.Control_Investigate:
			if f.refuse != "" {
				r.Ok, r.Error = false, f.refuse
				break
			}
			inv := cmd.Investigate
			f.got = append(f.got, inv.TaskId)
			f.task = &sarpb.TaskStatus{TaskId: inv.TaskId, CasualtyId: inv.CasualtyId, State: sarpb.TaskState_TASK_STATE_EN_ROUTE, Target: inv.Target,
				HoverS: inv.HoverS, StartedUnixNs: time.Now().UnixNano()}
			f.state = sarpb.FlightState_FLIGHT_STATE_INVESTIGATE
			go f.fly(inv)
		case *sarpb.Control_CancelTask:
		}
		b, _ := msg.Marshal(r)
		_ = f.rep.Put(b)
	})
	go func() {
		var tseq msg.Seq
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-f.stop:
				return
			case <-tk.C:
			}
			f.mu.Lock()
			p := local.ToPoint(f.e, f.n)
			h := local.ToPoint(e, n)
			tm := &sarpb.Telemetry{Header: msg.NewHeader("inc", "uav/"+id, tseq.Next()), UavId: id, Role: f.role, Position: &sarpb.GeoPoint{LatDeg: p.Lat, LonDeg: p.Lon},
				BatteryPct: f.battery, State: f.state, FlightTimeLeftS: uint32(float64(f.battery) / 100 * 30 * 60), CruiseMps: 10,
				Home: &sarpb.GeoPoint{LatDeg: h.Lat, LonDeg: h.Lon}, GroundSpeedMps: 5}
			if f.task != nil {
				tm.Task = copyStatus(f.task)
			}
			f.mu.Unlock()
			b, _ := msg.Marshal(tm)
			_ = f.tel.Put(b)
		}
	}()
	t.Cleanup(func() { close(f.stop) })
	return f
}

func copyStatus(ts *sarpb.TaskStatus) *sarpb.TaskStatus {
	return pb.Clone(ts).(*sarpb.TaskStatus)
}

func (f *fake) fly(inv *sarpb.Investigate) {
	time.Sleep(150 * time.Millisecond)
	f.mu.Lock()
	if f.task == nil || f.task.TaskId != inv.TaskId || f.slow {
		f.mu.Unlock()
		return
	}
	f.task.State = sarpb.TaskState_TASK_STATE_ON_STATION
	f.e, f.n = local.ToLocal(geo.Point{Lat: inv.Target.LatDeg, Lon: inv.Target.LonDeg})
	abort := f.abort
	f.abort = false
	f.mu.Unlock()
	time.Sleep(time.Duration(inv.HoverS) * 300 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.task == nil || f.task.TaskId != inv.TaskId {
		return
	}
	if abort {
		f.task.State, f.task.Reason = sarpb.TaskState_TASK_STATE_ABORTED, "battery low"
	} else {
		f.task.State, f.task.Frames, f.task.TrackIds, f.task.FinishedUnixNs = sarpb.TaskState_TASK_STATE_DONE, 30, []string{f.id + "-7"}, time.Now().UnixNano()
	}
	f.state = sarpb.FlightState_FLIGHT_STATE_HOVER
}

func (f *fake) tasks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// analyst stands in for casualty-analyst: it publishes a casualty list.
type analyst struct {
	pub bus.Publisher
	seq msg.Seq
}

func cas(id string, e, n float64, tier uint32, looks uint32) *sarpb.Casualty {
	p := local.ToPoint(e, n)
	c := &sarpb.Casualty{CasualtyId: id, Position: &sarpb.GeoPoint{LatDeg: p.Lat, LonDeg: p.Lon}, Investigations: looks}
	switch tier {
	case 1:
		c.Triage, c.Provisional, c.NeedsInvestigation, c.InvestigationReason = sarpb.Triage_TRIAGE_RED, true, true, "provisionally Red"
	case 2:
		c.Triage, c.Provisional, c.NeedsInvestigation, c.InvestigationReason = sarpb.Triage_TRIAGE_GREY, true, true, "temperature not readable"
	case 3:
		c.Triage, c.NeedsInvestigation, c.InvestigationReason = sarpb.Triage_TRIAGE_BLACK, true, "position uncertain"
	default:
		c.Triage = sarpb.Triage_TRIAGE_YELLOW
	}
	return c
}

func (a *analyst) send(t *testing.T, cs ...*sarpb.Casualty) {
	b, _ := msg.Marshal(&sarpb.CasualtyList{Header: msg.NewHeader("inc", "zbook/casualty-analyst", a.seq.Next()), Casualties: cs})
	if err := a.pub.Put(b); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type env struct {
	m     *bus.Memory
	p     *Planner
	a     *analyst
	stop  func()
	mu    sync.Mutex
	lists []*sarpb.TaskList
}

func setup(t *testing.T, state string) *env {
	m := bus.NewMemory()
	e := &env{m: m, a: &analyst{}}
	e.a.pub, _ = m.DeclarePublisher(keys.Casualties("inc"), bus.QoSCasualty)
	m.Subscribe(keys.Tasks("inc"), func(s bus.Sample) {
		var l sarpb.TaskList
		if msg.Unmarshal(s.Payload, &l) == nil {
			e.mu.Lock()
			e.lists = append(e.lists, &l)
			e.mu.Unlock()
		}
	})
	e.start(t, state)
	return e
}

func (e *env) start(t *testing.T, state string) {
	p, err := Open(testConfig(state), e.m, quiet())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	e.p = p
	e.stop = func() { cancel(); <-done; p.Close() }
	t.Cleanup(func() {
		if e.stop != nil {
			e.stop()
			e.stop = nil
		}
	})
}

func (e *env) task(cid string, st sarpb.TaskState) *sarpb.Task {
	for _, t := range e.p.Tasks() {
		if t.CasualtyId == cid && t.State == st {
			return t
		}
	}
	return nil
}

func (e *env) count(cid string) (n, done int) {
	for _, t := range e.p.Tasks() {
		if t.CasualtyId == cid {
			n++
			if t.State == sarpb.TaskState_TASK_STATE_DONE {
				done++
			}
		}
	}
	return
}

// The nearest free investigator gets each casualty; a look that settles the
// casualty ends it; one still unknown gets a second look after the analyst
// has caught up, and no third. The scout and a drone short of battery are
// never sent. The task list carries the results.
func TestAssignAndLooks(t *testing.T) {
	e := setup(t, filepath.Join(t.TempDir(), "inc.json"))
	inv1 := newFake(t, e.m, "inv-1", sarpb.UavRole_UAV_ROLE_INVESTIGATOR, -50, 0, 100)
	inv2 := newFake(t, e.m, "inv-2", sarpb.UavRole_UAV_ROLE_INVESTIGATOR, 50, 0, 100)
	low := newFake(t, e.m, "inv-3", sarpb.UavRole_UAV_ROLE_INVESTIGATOR, -41, 0, 26) // nearest to C-001, short of battery
	scout := newFake(t, e.m, "scout-1", sarpb.UavRole_UAV_ROLE_SCOUT, -40, 1, 100)
	waitFor(t, "fleet", 10*time.Second, func() bool { return len(e.p.Status().Drones) == 4 })

	e.a.send(t, cas("C-001", -40, 0, 1, 0), cas("C-002", 40, 0, 2, 0), cas("C-003", 0, 30, 0, 0))
	waitFor(t, "two looks done", 10*time.Second, func() bool {
		_, d1 := e.count("C-001")
		_, d2 := e.count("C-002")
		return d1 == 1 && d2 == 1
	})
	t1, t2 := e.task("C-001", sarpb.TaskState_TASK_STATE_DONE), e.task("C-002", sarpb.TaskState_TASK_STATE_DONE)
	if t1.UavId != "inv-1" || t2.UavId != "inv-2" || t1.Tier != 1 || t2.Tier != 2 {
		t.Fatalf("assignments: %v / %v", t1, t2)
	}
	if !t1.Result.Found || t1.Result.Frames != 30 || len(t1.Result.TrackIds) != 1 || t1.OnStationUnixNs == 0 || t1.FinishedUnixNs == 0 || t1.Attempt != 1 {
		t.Fatalf("result %v", t1)
	}
	if n, _ := e.count("C-003"); n != 0 || len(low.tasks()) != 0 || len(scout.tasks()) != 0 {
		t.Fatalf("C-003 tasks %d, low-battery drone %v, scout %v", n, low.tasks(), scout.tasks())
	}
	if st := e.p.Status(); !strings.Contains(st.Text(), "inv-3") || st.ByState["done"] != 2 {
		t.Fatalf("status:\n%s", st.Text())
	}
	// No second look until the analyst's list shows the first.
	time.Sleep(400 * time.Millisecond)
	if n, _ := e.count("C-002"); n != 1 {
		t.Fatalf("C-002 re-tasked before the analyst counted the look: %d tasks", n)
	}
	// C-001 settled; C-002 still unknown after one look.
	e.a.send(t, cas("C-001", -40, 0, 0, 1), cas("C-002", 40, 0, 2, 1), cas("C-003", 0, 30, 0, 0))
	waitFor(t, "second look at C-002", 10*time.Second, func() bool { _, d := e.count("C-002"); return d == 2 })
	e.a.send(t, cas("C-001", -40, 0, 0, 1), cas("C-002", 40, 0, 2, 2), cas("C-003", 0, 30, 0, 0))
	time.Sleep(500 * time.Millisecond)
	if n, _ := e.count("C-002"); n != 2 {
		t.Fatalf("C-002 got %d tasks (max_attempts 2)", n)
	}
	if n, _ := e.count("C-001"); n != 1 {
		t.Fatalf("C-001 got %d tasks after it settled", n)
	}
	if len(inv1.tasks())+len(inv2.tasks()) != 3 {
		t.Fatalf("tasks sent: inv-1 %v inv-2 %v", inv1.tasks(), inv2.tasks())
	}
	e.mu.Lock()
	last := e.lists[len(e.lists)-1]
	e.mu.Unlock()
	if len(last.Tasks) != 3 || last.Header.GetIncidentId() != "inc" {
		t.Fatalf("published task list %v", last)
	}

	// Restart: the tasks come back from the state file.
	e.stop()
	e.stop = nil
	e.start(t, e.p.cfg.StateFile)
	if got := e.p.Tasks(); len(got) != 3 || got[0].TaskId != t1.TaskId || got[0].State != sarpb.TaskState_TASK_STATE_DONE {
		t.Fatalf("after restart: %v", got)
	}
	// Reset: forgotten, new ids.
	e.p.Reset()
	if len(e.p.Tasks()) != 0 {
		t.Fatal("tasks after reset")
	}
	e.a.send(t, cas("C-001", -40, 0, 1, 0))
	waitFor(t, "a task after reset", 10*time.Second, func() bool { return len(e.p.Tasks()) == 1 })
	if id := e.p.Tasks()[0].TaskId; id == t1.TaskId || !strings.HasSuffix(id, "-001") {
		t.Fatalf("task id after reset %s (before %s)", id, t1.TaskId)
	}
}

// A refusal or no answer gives the task to the other drone; an abort puts it
// back in the queue; a tier-1 casualty takes a drone flying to a tier-3 one.
func TestFailuresAndPreemption(t *testing.T) {
	e := setup(t, "")
	bad := newFake(t, e.m, "inv-1", sarpb.UavRole_UAV_ROLE_INVESTIGATOR, -50, 0, 100)
	good := newFake(t, e.m, "inv-2", sarpb.UavRole_UAV_ROLE_INVESTIGATOR, 50, 0, 100)
	bad.refuse = "E-EDGE-007 test refusal"
	waitFor(t, "fleet", 10*time.Second, func() bool { return len(e.p.Status().Drones) == 2 })
	e.a.send(t, cas("C-001", -45, 0, 1, 0))
	waitFor(t, "done by inv-2", 10*time.Second, func() bool {
		t := e.task("C-001", sarpb.TaskState_TASK_STATE_DONE)
		return t != nil && t.UavId == "inv-2"
	})
	if tk := e.task("C-001", sarpb.TaskState_TASK_STATE_DONE); tk.Attempt != 2 {
		t.Fatalf("attempts %d", tk.Attempt)
	}
	if st := e.p.Status(); !strings.Contains(st.Drones[0].Note, "test refusal") {
		t.Fatalf("drone note %q", st.Drones[0].Note)
	}

	// Silent drone: no answer within the command timeout.
	e.p.Reset()
	bad.mu.Lock()
	bad.refuse, bad.silent = "", true
	bad.mu.Unlock()
	good.mu.Lock()
	good.abort = true
	good.mu.Unlock()
	e.a.send(t, cas("C-005", -55, 0, 2, 0)) // inv-1 nearest (inv-2 is now at C-001)
	waitFor(t, "done after a timeout and an abort", 10*time.Second, func() bool { return e.task("C-005", sarpb.TaskState_TASK_STATE_DONE) != nil })
	tk := e.task("C-005", sarpb.TaskState_TASK_STATE_DONE)
	if tk.UavId != "inv-2" || tk.Attempt != 3 {
		t.Fatalf("after timeout and abort: %v", tk)
	}

	// Pre-emption.
	e.p.Reset()
	bad.stop <- struct{}{} // inv-1 goes offline
	good.mu.Lock()
	good.slow = true
	good.mu.Unlock()
	time.Sleep(1200 * time.Millisecond)
	e.a.send(t, cas("C-010", 40, 0, 3, 0))
	waitFor(t, "tier 3 en route", 10*time.Second, func() bool { return e.task("C-010", sarpb.TaskState_TASK_STATE_EN_ROUTE) != nil })
	good.mu.Lock()
	good.slow = false
	good.mu.Unlock()
	e.a.send(t, cas("C-010", 40, 0, 3, 0), cas("C-011", 45, 5, 1, 0))
	waitFor(t, "tier 1 done first", 10*time.Second, func() bool { return e.task("C-011", sarpb.TaskState_TASK_STATE_DONE) != nil })
	if q := e.task("C-010", sarpb.TaskState_TASK_STATE_DONE); q == nil {
		waitFor(t, "tier 3 done after", 10*time.Second, func() bool { return e.task("C-010", sarpb.TaskState_TASK_STATE_DONE) != nil })
	}
	if tk := e.task("C-010", sarpb.TaskState_TASK_STATE_DONE); tk.Attempt != 1 || tk.UavId != "inv-2" {
		t.Fatalf("pre-empted task %v", tk)
	}
}

// Within a tier the close look nearest a free investigator goes first (not
// the oldest), unless one has waited four minutes; a more urgent tier always first.
func TestQueueOrder(t *testing.T) {
	m := bus.NewMemory()
	defer m.Close()
	p, err := Open(testConfig(filepath.Join(t.TempDir(), "inc.json")), m, quiet())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	now := time.Unix(1_800_000_000, 0)
	at := func(e, n float64) *sarpb.GeoPoint {
		g := p.local.ToPoint(e, n)
		return &sarpb.GeoPoint{LatDeg: g.Lat, LonDeg: g.Lon}
	}
	mk := func(id string, tier uint32, ageS int, e, n float64) *sarpb.Task {
		return &sarpb.Task{TaskId: id, Tier: tier, Target: at(e, n), CreatedUnixNs: now.Add(-time.Duration(ageS) * time.Second).UnixNano()}
	}
	drone := &Drone{ID: "inv-1", E: 0, N: 0}
	order := func(q ...*sarpb.Task) string {
		p.orderQueue(q, []*Drone{drone}, now)
		var ids []string
		for _, x := range q {
			ids = append(ids, x.TaskId)
		}
		return strings.Join(ids, " ")
	}
	// Far (older) and near (newer) in the same tier: the near one first.
	if got := order(mk("far", 2, 60, 400, 0), mk("near", 2, 10, 30, 0)); got != "near far" {
		t.Errorf("same tier: %s", got)
	}
	// A more urgent tier first, however far.
	if got := order(mk("near2", 2, 10, 30, 0), mk("far1", 1, 5, 500, 0)); got != "far1 near2" {
		t.Errorf("tiers: %s", got)
	}
	// One waiting over four minutes goes first in its tier.
	if got := order(mk("near", 2, 10, 30, 0), mk("stale", 2, 300, 600, 0)); got != "stale near" {
		t.Errorf("stale: %s", got)
	}
}
