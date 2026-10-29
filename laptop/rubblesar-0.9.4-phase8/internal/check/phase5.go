package check

import (
	"context"

	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/casualty"
	"rubblesar/internal/config"
	"rubblesar/internal/cot"
	"rubblesar/internal/dispatch"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/roads"
	"rubblesar/internal/sarpb"
)

// Phase5Options configure the phase 5 acceptance run.
type Phase5Options struct {
	// P4 drives the run (drones, casualty-analyst, mission-planner) as in phase 4.
	P4 Phase4Options
	// TakAddr is the TAK server (default [dispatch] tak_server).
	TakAddr string
	// Team is the callsign sar-check plays on the TAK server: a [[dispatch.team]]
	// with source "tak" (default CHECK-1).
	Team string
	// Dispatch checks rescue-dispatch is ready and returns its status (nil: its status API).
	Dispatch func() (*dispatch.Status, error)
	// DispatchReset forgets every assignment (nil: the status API).
	DispatchReset func() error
	// Quick reuses casualty-analyst's current casualties: no new survey.
	Quick bool
	// ReachWait: after the drones are done, how long to wait for every
	// Red and Yellow casualty to be reached (default 25 min: teams are busy
	// freeing casualties for a while, see [dispatch] extrication).
	ReachWait time.Duration
	// Streets is the street network, for its rivers (nil: the [dispatch] roads file).
	Streets *roads.Network
	// Report collects the results (nil: a new "phase5" report); phase 6 adds its own checks around these.
	Report *Report
}

// DefaultPhase5Options is the full acceptance run (about 30-40 minutes).
func DefaultPhase5Options(cfg *config.ZBook, b bus.Bus, p4 Phase4Options) Phase5Options {
	return Phase5Options{P4: p4, TakAddr: cfg.Dispatch.TakServer, Team: "CHECK-1", ReachWait: 25 * time.Minute}
}

// Thresholds of the phase 5 acceptance.
const (
	p5MarkerM    = 2.0  // marker within this of the casualty record
	p5ChatPass   = 0.95 // dispatch messages received on TAK
	p5ChatWarn   = 0.80
	p5RouteTolM  = 0.5 // street part at most this far inside a rubble area
	p5DroneAge   = 20 * time.Second
	p5DoneWithin = 15 * time.Second // CHECK-1 "done" -> assignment done
)

var assignmentRE = regexp.MustCompile(`Assignment (A-[0-9a-z]+-[0-9]+)`)

// --- What arrives on the TAK server ---------------------------------------------

type p5chat struct {
	at     time.Time
	from   string
	text   string
	direct bool // addressed to our callsign
}

type p5tak struct {
	mu       sync.Mutex
	team     string // our callsign
	latest   map[string]*cot.Event
	at       map[string]time.Time
	routes   map[string][][2]float64 // assignment id -> route points
	routeAt  map[string]time.Time
	chats    []p5chat
	opsUID   string
	opsCS    string
	events   int
	upSince  time.Time
	onRoute  func(uid string, e *cot.Event) // route lines for our team
	ourRoute string
}

func newP5Tak(team, ourRoute string) *p5tak {
	return &p5tak{team: team, ourRoute: ourRoute, latest: map[string]*cot.Event{}, at: map[string]time.Time{}, routes: map[string][][2]float64{}, routeAt: map[string]time.Time{}}
}

func (t *p5tak) onEvent(e *cot.Event) {
	now := time.Now()
	t.mu.Lock()
	t.events++
	var route func(string, *cot.Event)
	switch {
	case e.Type == "t-x-c-t":
	case e.IsDelete():
		uid := e.DeleteUID()
		t.latest[uid], t.at[uid] = nil, now
		if uid == t.ourRoute {
			route = t.onRoute
		}
	case e.IsChat():
		direct := strings.EqualFold(e.Detail.Chat.Chatroom, t.team)
		for _, d := range e.Dests() {
			direct = direct || strings.EqualFold(d, t.team)
		}
		t.chats = append(t.chats, p5chat{at: now, from: e.Detail.Chat.SenderCallsign, text: e.ChatText(), direct: direct})
	default:
		t.latest[e.UID], t.at[e.UID] = e, now
		if e.Type == "u-d-f" && e.Detail.Remarks != nil {
			if m := assignmentRE.FindStringSubmatch(e.Detail.Remarks.Text); m != nil {
				t.routes[m[1]] = e.LinePoints()
				if _, ok := t.routeAt[m[1]]; !ok {
					t.routeAt[m[1]] = now
				}
			}
		}
		if e.IsPLI() && strings.HasSuffix(e.UID, ".ops") && strings.HasPrefix(e.UID, "rubblesar.") {
			t.opsUID, t.opsCS = e.UID, e.Callsign()
		}
		if e.UID == t.ourRoute {
			route = t.onRoute
		}
	}
	t.mu.Unlock()
	if route != nil {
		route(e.UID, e)
	}
}

// --- rescue-dispatch's published state -------------------------------------------

type p5disp struct {
	mu     sync.Mutex
	latest *sarpb.DispatchState
	at     time.Time
	states int
	asg    map[string]*sarpb.Assignment
	order  []string
	// cases: every casualty an assignment has been for (it follows its
	// casualty into a merged record: C-014 merged into C-015).
	cases  map[string][]string
	online map[string]time.Time // team -> first seen not offline
	scene  map[string]time.Time // assignment -> first seen on scene
	done   map[string]time.Time // assignment -> first seen done
	start  time.Time
	// redSpans: when each casualty was listed Red (from, to), after the start.
	redSpans map[string][][2]time.Time
	lastList time.Time
}

func newP5Disp() *p5disp {
	return &p5disp{asg: map[string]*sarpb.Assignment{}, cases: map[string][]string{}, online: map[string]time.Time{}, scene: map[string]time.Time{}, done: map[string]time.Time{},
		redSpans: map[string][][2]time.Time{}}
}

func (d *p5disp) onState(s bus.Sample) {
	var st sarpb.DispatchState
	if msg.Unmarshal(s.Payload, &st) != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.latest, d.at = &st, s.Received
	d.states++
	if d.start.IsZero() || s.Received.Before(d.start) {
		return
	}
	for _, t := range st.Teams {
		if t.State != sarpb.TeamState_TEAM_STATE_OFFLINE {
			if _, ok := d.online[t.TeamId]; !ok {
				d.online[t.TeamId] = s.Received
			}
		}
	}
	for _, a := range st.Assignments {
		if _, ok := d.asg[a.AssignmentId]; !ok {
			d.order = append(d.order, a.AssignmentId)
		}
		d.asg[a.AssignmentId] = a
		if cs := d.cases[a.AssignmentId]; len(cs) == 0 || cs[len(cs)-1] != a.CasualtyId {
			d.cases[a.AssignmentId] = append(cs, a.CasualtyId)
		}
		switch a.State {
		case sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
			if _, ok := d.scene[a.AssignmentId]; !ok {
				d.scene[a.AssignmentId] = s.Received
			}
		case sarpb.AssignmentState_ASSIGNMENT_STATE_DONE:
			if _, ok := d.done[a.AssignmentId]; !ok {
				d.done[a.AssignmentId] = s.Received
			}
		}
	}
}

func (d *p5disp) onList(s bus.Sample) {
	var l sarpb.CasualtyList
	if msg.Unmarshal(s.Payload, &l) != nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.start.IsZero() || s.Received.Before(d.start) {
		return
	}
	red := map[string]bool{}
	for _, c := range l.Casualties {
		if c.Triage == sarpb.Triage_TRIAGE_RED {
			red[c.CasualtyId] = true
		}
	}
	for id := range red {
		sp := d.redSpans[id]
		if n := len(sp); n > 0 && sp[n-1][1].Equal(d.lastList) {
			sp[n-1][1] = s.Received // still Red
		} else {
			sp = append(sp, [2]time.Time{s.Received, s.Received})
		}
		d.redSpans[id] = sp
	}
	d.lastList = s.Received
}

func (d *p5disp) teamAssignment(team string) *sarpb.Assignment {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.latest == nil {
		return nil
	}
	for _, t := range d.latest.Teams {
		if t.TeamId == team && t.AssignmentId != "" {
			return d.asg[t.AssignmentId]
		}
	}
	return nil
}

func (d *p5disp) counts() (active, done, cancelled, waiting int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range d.order {
		switch d.asg[id].State {
		case sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE, sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
			active++
		case sarpb.AssignmentState_ASSIGNMENT_STATE_DONE:
			done++
		case sarpb.AssignmentState_ASSIGNMENT_STATE_CANCELLED:
			cancelled++
		}
	}
	if d.latest != nil {
		waiting = len(d.latest.Waiting)
	}
	return
}

// --- The TAK team sar-check plays ------------------------------------------------

type p5loop struct {
	asg, cas            string
	routeAt, chatAt     time.Time
	direct              bool
	points              int
	startAt, arrivedAt  time.Time
	sceneAt, doneSentAt time.Time
	firstDone, doneAt   time.Time
	cancelled           bool
	lengthM             float64
}

type p5player struct {
	mu       sync.Mutex
	cl       *cot.Client
	tak      *p5tak
	disp     *p5disp
	uid, cs  string
	teamID   string
	local    geo.Local
	pos      geom.P
	walk     []geom.P
	walkAt   time.Time
	speed    float64
	course   float64
	cur      *p5loop
	loops    []*p5loop
	stopped  bool
	pliCount int
}

func (p *p5player) pli(now time.Time) *cot.Event {
	g := p.local.ToPoint(p.pos.X, p.pos.Y)
	sp := 0.0
	if p.walk != nil {
		sp = p.speed
	}
	p.pliCount++
	return cot.PLI(p.uid, p.cs, "Cyan", "Team Member", g.Lat, g.Lon, p.course, sp, "sar-check phase 5 test team", now, time.Minute)
}

// onRoute: a route line for our team arrived (or was deleted).
func (p *p5player) onRoute(uid string, e *cot.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e.IsDelete() {
		return // deleted when we arrive (on scene) or on cancellation: the dispatch state tells which
	}
	m := assignmentRE.FindStringSubmatch(e.Detail.Remarks.Text)
	if m == nil {
		return
	}
	// Only assignments of this run: rescue-dispatch may hand the test team a
	// casualty of the previous run in the seconds before its reset.
	if !p.disp.running() {
		return
	}
	pts := e.LinePoints()
	if len(pts) < 2 {
		return
	}
	if p.cur != nil && p.cur.asg == m[1] {
		return // refresh or re-route of the walk we are on: keep going
	}
	now := time.Now()
	l := &p5loop{asg: m[1], routeAt: now, points: len(pts)}
	p.loops = append(p.loops, l)
	p.cur = l
	p.walk = nil
	for _, q := range pts {
		x, y := p.local.ToLocal(geo.Point{Lat: q[0], Lon: q[1]})
		p.walk = append(p.walk, geom.P{X: x, Y: y})
	}
	// Start from where we are.
	p.walk[0] = p.pos
	for i := 1; i < len(p.walk); i++ {
		l.lengthM += p.walk[i-1].Dist(p.walk[i])
	}
	p.walkAt, l.startAt = now, now
}

// step moves along the walk, handles arrival and reports our position; called every second.
func (p *p5player) step(now time.Time) {
	sendDone := p.advance(now)
	p.mu.Lock()
	e := p.pli(now)
	pos := p.pos
	p.mu.Unlock()
	p.cl.Send(e)
	if !sendDone {
		return
	}
	p.tak.mu.Lock()
	to := cot.ChatTo{UID: p.tak.opsUID, Callsign: p.tak.opsCS}
	p.tak.mu.Unlock()
	if to.UID == "" {
		to = cot.ChatTo{} // SAR-OPS not seen: All Chat Rooms reaches it too
	}
	g := p.local.ToPoint(pos.X, pos.Y)
	p.cl.Send(cot.GeoChat(p.uid, p.cs, to, "done", g.Lat, g.Lon, now))
}

// advance moves along the current walk; true when it is time to say "done".
func (p *p5player) advance(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := p.cur
	if cur == nil {
		return false
	}
	// Our assignment as rescue-dispatch sees it.
	if a := p.disp.asgByID(cur.asg); a != nil {
		cur.cas = a.CasualtyId
		switch a.State {
		case sarpb.AssignmentState_ASSIGNMENT_STATE_CANCELLED:
			cur.cancelled = true
			p.walk, p.cur = nil, nil
			return false
		case sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
			if cur.sceneAt.IsZero() {
				cur.sceneAt = now
			}
		case sarpb.AssignmentState_ASSIGNMENT_STATE_DONE:
			if cur.sceneAt.IsZero() {
				cur.sceneAt = now
			}
			if cur.doneAt.IsZero() {
				cur.doneAt = now
			}
			p.walk, p.cur = nil, nil
			return false
		}
	}
	if p.walk != nil && cur.arrivedAt.IsZero() {
		d := now.Sub(p.walkAt).Seconds() * p.speed
		pos, arrived := p.walk[len(p.walk)-1], true
		for i := 1; i < len(p.walk); i++ {
			seg := p.walk[i-1].Dist(p.walk[i])
			if d <= seg {
				pos = geom.Lerp(p.walk[i-1], p.walk[i], d/math.Max(seg, 1e-9))
				v := p.walk[i].Sub(p.walk[i-1])
				p.course = math.Mod(math.Atan2(v.X, v.Y)*180/math.Pi+360, 360)
				arrived = false
				break
			}
			d -= seg
		}
		p.pos = pos
		if arrived {
			cur.arrivedAt = now
		}
	}
	// Arrived: once rescue-dispatch has us on scene (or after 5 s), say "done" (again every 20 s until released).
	if cur.arrivedAt.IsZero() || (cur.sceneAt.IsZero() && now.Sub(cur.arrivedAt) < 5*time.Second) {
		return false
	}
	if cur.doneSentAt.IsZero() || now.Sub(cur.doneSentAt) >= 20*time.Second {
		if cur.doneSentAt.IsZero() {
			cur.firstDone = now
		}
		cur.doneSentAt = now
		return true
	}
	return false
}

// running reports whether the run (and rescue-dispatch's reset) has started.
func (d *p5disp) running() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.start.IsZero()
}

func (d *p5disp) asgByID(id string) *sarpb.Assignment {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a := d.asg[id]; a != nil {
		return a
	}
	if d.latest != nil {
		for _, a := range d.latest.Assignments {
			if a.AssignmentId == id {
				return a
			}
		}
	}
	return nil
}

// --- The suite ---------------------------------------------------------------------

func dispatchStatus(addr string) (*dispatch.Status, error) {
	var doc struct {
		State    string          `json:"state"`
		Dispatch dispatch.Status `json:"dispatch"`
	}
	code, err := getJSON(addr, "/v1/status", &doc)
	switch {
	case err != nil:
		return nil, fmt.Errorf("rescue-dispatch status API at %s: %v", addr, err)
	case code != http.StatusOK:
		return nil, fmt.Errorf("rescue-dispatch is %s", doc.State)
	}
	return &doc.Dispatch, nil
}

// Phase5 runs the phase 5 acceptance test: the phase 4 run (survey,
// casualty records, investigations) with rescue-dispatch sending the teams,
// watched from the TAK server as a TAK client that also plays one rescue team.
func Phase5(ctx context.Context, o Phase5Options, out io.Writer) *Report {
	r := o.Report
	if r == nil {
		r = NewReport("phase5", out)
	}
	r.Share = true
	p4 := o.P4
	cfg := p4.Cfg
	inc := cfg.Site.IncidentID
	prog := p4.Progress
	if prog == nil {
		prog = io.Discard
	}
	if o.ReachWait <= 0 {
		o.ReachWait = 25 * time.Minute
	}
	if o.Team == "" {
		o.Team = "CHECK-1"
	}
	if o.TakAddr == "" {
		o.TakAddr = cfg.Dispatch.TakServer
	}
	r.Context["incident"] = inc
	r.Context["tak_server"] = o.TakAddr
	mode := "full run (survey, investigations, rescue)"
	if o.Quick {
		mode = "quick (casualties of the last run, rescue only)"
	}
	r.Context["mode"] = mode
	names := map[string]string{"P5-01": "TAK server, rescue-dispatch and the services", "P5-02": "TAK map: casualties, drones, teams",
		"P5-03": "routes avoid the rubble", "P5-04": "dispatch order and every Red and Yellow reached", "P5-05": "TAK team loop (CHECK-1)",
		"P5-06": "chat notifications"}
	ids := []string{"P5-02", "P5-03", "P5-04", "P5-05", "P5-06"}
	skipAll := func(why string) *Report {
		for _, id := range ids {
			r.Skip(id, names[id], why)
		}
		return r
	}
	var team *config.Team
	for i, t := range cfg.Dispatch.Teams {
		if strings.EqualFold(t.Callsign, o.Team) && t.Source == "tak" {
			team = &cfg.Dispatch.Teams[i]
		}
	}
	if team == nil {
		r.Run("P5-01", names["P5-01"], func() Result {
			return Result{Status: Fail, Code: "E-CHK-511", Detail: fmt.Sprintf("no [[dispatch.team]] with callsign %q and source \"tak\" in the config", o.Team),
				Hint: "add to /etc/rubblesar/sar.toml:  [[dispatch.team]] id = \"check-1\" callsign = \"CHECK-1\" mode = \"foot\" source = \"tak\"  and restart rescue-dispatch"}
		})
		return skipAll("needs P5-01")
	}
	prefix := "rubblesar." + inc + "."
	tak := newP5Tak(team.Callsign, prefix+"route."+team.ID)
	disp := newP5Disp()
	col := newP4Collector(inc)
	var subs []bus.Subscription
	defer func() {
		for _, s := range subs {
			_ = s.Close()
		}
	}()
	for _, s := range []struct {
		key string
		h   bus.Handler
	}{{keys.Root + "/" + inc + "/uav/*/**", col.onUAV}, {keys.SimTruthAll(inc), col.onTruth}, {keys.Casualties(inc), col.onList},
		{keys.CasualtyAll(inc), col.onCasualty}, {keys.Tasks(inc), col.onTasks}, {keys.Dispatch(inc), disp.onState},
		{keys.Casualties(inc), disp.onList}} {
		sub, err := p4.Bus.Subscribe(s.key, s.h)
		if err != nil {
			r.Run("P5-01", names["P5-01"], func() Result {
				return Result{Status: Fail, Code: "E-CHK-511", Detail: "cannot subscribe to " + s.key, Evidence: []string{err.Error()}}
			})
			return r
		}
		subs = append(subs, sub)
	}
	ctl, err := newController(p4.Bus, inc)
	if err != nil {
		r.Run("P5-01", names["P5-01"], func() Result {
			return Result{Status: Fail, Code: "E-CHK-511", Detail: "cannot open the control channel", Evidence: []string{err.Error()}}
		})
		return r
	}
	defer ctl.close()

	// sar-check on the TAK server, as the TAK team.
	loc := geo.NewLocal(geo.Point{Lat: cfg.Site.CenterLat, Lon: cfg.Site.CenterLon})
	slat, slon := cfg.Staging()
	pl := &p5player{tak: tak, disp: disp, uid: "rubblesar-check." + inc + "." + team.ID, cs: team.Callsign, teamID: team.ID, local: loc, speed: 1.4}
	pl.pos.X, pl.pos.Y = loc.ToLocal(geo.Point{Lat: slat, Lon: slon})
	pl.pos = pl.pos.Add(geom.P{X: 8, Y: 0})
	tak.onRoute = pl.onRoute
	tctx, tcancel := context.WithCancel(ctx)
	defer tcancel()
	pl.cl = cot.NewClient(cot.ClientConfig{Addr: o.TakAddr, OnEvent: tak.onEvent, PingUID: pl.uid, MaxBackoff: 5 * time.Second,
		Hello: func(now time.Time) []*cot.Event {
			pl.mu.Lock()
			defer pl.mu.Unlock()
			return []*cot.Event{pl.pli(now)}
		}})
	var pwg sync.WaitGroup
	pwg.Add(2)
	go func() { defer pwg.Done(); pl.cl.Run(tctx) }()
	defer pl.cl.Close()
	go func() {
		defer pwg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-tctx.Done():
				return
			case now := <-t.C:
				pl.step(now)
			}
		}
	}()

	dstat := o.Dispatch
	if dstat == nil {
		dstat = func() (*dispatch.Status, error) { return dispatchStatus(cfg.Dispatch.Listen) }
	}
	var ds *dispatch.Status
	run := &p4run{}
	normal := map[string]uint32{}
	res := r.Run("P5-01", names["P5-01"], func() Result {
		return safe("E-CHK-511", func() Result {
			var p4res Result
			if !o.Quick {
				p4res = p4Ready(ctx, p4, col, run, normal)
				if p4res.Status == Fail {
					p4res.Code = "E-CHK-511"
					return p4res
				}
			}
			res, st := p5Ready(ctx, o, cfg, pl, tak, dstat, team)
			ds = st
			if res.Status == Pass && p4res.Status == Warn {
				res.Status, res.Hint = Warn, p4res.Hint
				res.Detail += "; " + p4res.Detail
			}
			res.Evidence = append(p4res.Evidence, res.Evidence...)
			return res
		})
	})
	if res.Status == Fail {
		return skipAll("needs P5-01")
	}
	if ds != nil {
		pl.speed = 1.4 * ds.TimeScale
		r.Context["time_scale"] = fmt.Sprintf("%g", ds.TimeScale)
		r.Context["streets"] = fmt.Sprintf("%s %.1f km, %d pieces closed", ds.Roads.Source, ds.Roads.LengthKm, ds.Roads.BlockedEdges)
	}

	// The run.
	dreset := o.DispatchReset
	if dreset == nil {
		dreset = func() error { return postJSON(cfg.Dispatch.Listen, "/v1/reset") }
	}
	extra := func() string {
		a, d, c, w := disp.counts()
		return fmt.Sprintf("rescue: %d active, %d done, %d cancelled, %d waiting", a, d, c, w)
	}
	startDispatch := func() {
		disp.mu.Lock()
		disp.start = time.Now()
		disp.mu.Unlock()
	}
	if o.Quick {
		if err := dreset(); err != nil {
			return failRun(r, names, ids, "rescue-dispatch reset: "+err.Error())
		}
		startDispatch()
		col.mu.Lock()
		run.start = time.Now()
		col.start = run.start
		col.mu.Unlock()
		fmt.Fprintf(prog, "  quick run: rescue-dispatch reset; the casualties of the last run get teams again\n")
	} else {
		p4.ExtraProgress = extra
		if p4.Suite == "" {
			p4.Suite = "phase5"
		}
		inner := p4.Reset
		p4.Reset = func(stamp string) error {
			if inner == nil {
				if err := resetServices(cfg, stamp); err != nil {
					return err
				}
			} else if err := inner(stamp); err != nil {
				return err
			}
			if err := dreset(); err != nil {
				return fmt.Errorf("rescue-dispatch reset: %v", err)
			}
			startDispatch()
			return nil
		}
		if err := p4Drive(ctx, p4, col, ctl, run, prog); err != nil {
			return failRun(r, names, ids, err.Error())
		}
		fmt.Fprintf(prog, "  drones done; waiting for the rescue teams (up to %.0f min)\n", o.ReachWait.Minutes())
	}
	reached, limited := p5WaitRescue(ctx, o, cfg, col, disp, prog, extra)
	if ctx.Err() != nil {
		return failRun(r, names, ids, "interrupted")
	}
	// One more marker refresh so the map is complete, then compare.
	wait := cfg.Dispatch.MarkerRefresh.Duration + 5*time.Second
	if ds != nil {
		wait = 35 * time.Second
	}
	fmt.Fprintf(prog, "  rescue %s; checking the TAK map after one more marker refresh (%.0f s)\n", reached, wait.Seconds())
	select {
	case <-ctx.Done():
		return failRun(r, names, ids, "interrupted")
	case <-time.After(wait):
	}
	// The latest casualty list at least 3 s old, so rescue-dispatch has put any change on the map.
	for i := 0; i < 15; i++ {
		col.mu.Lock()
		age := time.Since(col.casAt)
		col.mu.Unlock()
		if age >= 3*time.Second {
			break
		}
		time.Sleep(time.Second)
	}
	run.end = time.Now()
	run.timeLimited = run.timeLimited || limited
	final, _ := dstat()
	// Stop playing the team: the checks below read what was collected.
	tcancel()
	pl.cl.Close()
	pwg.Wait()
	col.mu.Lock()
	nCas := 0
	if col.cas != nil {
		nCas = len(col.cas.Casualties)
	}
	col.mu.Unlock()
	disp.mu.Lock()
	r.Context["run"] = fmt.Sprintf("%.0f s, %d casualties, %d assignments, %d dispatch states, %d TAK events", run.end.Sub(run.start).Seconds(), nCas, len(disp.order), disp.states, tak.events)
	disp.mu.Unlock()
	blocks := dispatch.Blockages(p4.World.Scn, loc)
	// The rivers from the installed street file: no walk off the streets may cross one.
	water := o.Streets
	if water == nil {
		if n, err := roads.Load(cfg.RoadsPath(), loc); err == nil {
			water = n
		}
	}
	r.Run("P5-02", names["P5-02"], func() Result {
		return safe("E-CHK-512", func() Result { return p5Map(cfg, col, tak, disp, prefix, final) })
	})
	r.Run("P5-03", names["P5-03"], func() Result {
		return safe("E-CHK-513", func() Result { return p5Routes(disp, tak, blocks, water, loc, final) })
	})
	r.Run("P5-04", names["P5-04"], func() Result { return safe("E-CHK-514", func() Result { return p5Order(cfg, col, disp, run) }) })
	r.Run("P5-05", names["P5-05"], func() Result { return safe("E-CHK-515", func() Result { return p5Loop(pl, tak, disp, team) }) })
	r.Run("P5-06", names["P5-06"], func() Result { return safe("E-CHK-516", func() Result { return p5Chat(disp, tak, cfg) }) })
	return r
}

func failRun(r *Report, names map[string]string, ids []string, why string) *Report {
	r.Run(ids[0], names[ids[0]], func() Result {
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "the run could not be completed: " + why,
			Hint: "'rescue-dispatch status', 'casualty-analyst status', 'mission-planner status'; 'journalctl -u rescue-dispatch -n 30'"}
	})
	for _, id := range ids[1:] {
		r.Skip(id, names[id], "the run could not be completed")
	}
	return r
}

// p5Ready: taky answers, rescue-dispatch runs with streets and a TAK
// connection, and sees sar-check's TAK team.
func p5Ready(ctx context.Context, o Phase5Options, cfg *config.ZBook, pl *p5player, tak *p5tak, dstat func() (*dispatch.Status, error), team *config.Team) (Result, *dispatch.Status) {
	var ev []string
	c, err := net.DialTimeout("tcp", o.TakAddr, 3*time.Second)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "the TAK server does not answer on " + o.TakAddr, Evidence: []string{err.Error()},
			Hint: "taky runs in Docker on the ZBook: 'sudo systemctl status rubblesar-taky'; install it with scripts/zbook/34-taky.sh"}, nil
	}
	c.Close()
	ev = append(ev, "TAK server answers on "+o.TakAddr)
	st, err := dstat()
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "rescue-dispatch is not ready", Evidence: append(ev, err.Error()),
			Hint: "install the phase 5 ZBook release (scripts/zbook/30-install.sh); 'rescue-dispatch status' must answer"}, nil
	}
	if !st.TAK.Enabled {
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "rescue-dispatch has no TAK server configured", Evidence: ev,
			Hint: "set [dispatch] tak_server = \"127.0.0.1:8087\" in /etc/rubblesar/sar.toml and restart rescue-dispatch"}, st
	}
	// Our TAK connection, and rescue-dispatch seeing us.
	deadline := time.Now().Add(30 * time.Second)
	online := false
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if s, err := dstat(); err == nil {
			st = s
			for _, t := range s.Teams {
				if t.ID == team.ID && t.State != "offline" {
					online = true
				}
			}
		}
		if online && pl.cl.Connected() {
			break
		}
		time.Sleep(time.Second)
	}
	cs := pl.cl.Stats()
	ev = append(ev, fmt.Sprintf("sar-check on the TAK server as %s: connected %v, %d sent, %d received", team.Callsign, cs.Connected, cs.Sent, cs.Received))
	ev = append(ev, fmt.Sprintf("rescue-dispatch: TAK %s connected %v (%d sent), users %s; streets %s %.1f km (%d pieces closed by %d rubble areas); %d teams; time scale %g",
		st.TAK.Server, st.TAK.Connected, st.TAK.Sent, strings.Join(st.TAK.Users, ","), nonEmptyS(st.Roads.Source, "NONE"), st.Roads.LengthKm, st.Roads.BlockedEdges, st.Roads.Blockages, len(st.Teams), st.TimeScale))
	switch {
	case !cs.Connected:
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "sar-check cannot stay connected to the TAK server " + o.TakAddr, Evidence: append(ev, cs.LastError),
			Hint: "'sudo systemctl status rubblesar-taky'; 'docker logs rubblesar-taky'"}, st
	case !st.TAK.Connected:
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "rescue-dispatch is not connected to the TAK server: " + st.TAK.LastError, Evidence: ev,
			Hint: "check [dispatch] tak_server and 'journalctl -u rescue-dispatch -n 30'"}, st
	case !online:
		return Result{Status: Fail, Code: "E-CHK-511", Detail: fmt.Sprintf("rescue-dispatch did not see sar-check's position reports as %s within 30 s", team.Callsign), Evidence: ev,
			Hint: "the TAK server does not pass position reports between clients: 'docker logs rubblesar-taky'"}, st
	case !st.Roads.Loaded:
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "rescue-dispatch has no street file: routes would be straight lines", Evidence: ev,
			Hint: "run scripts/zbook/32-roads.sh (downloads the streets once; --synthetic without internet), then 'sudo systemctl restart rescue-dispatch'"}, st
	case st.Roads.Blockages == 0:
		return Result{Status: Fail, Code: "E-CHK-511", Detail: "rescue-dispatch knows no rubble areas (scenario not loaded)", Evidence: ev,
			Hint: "[site] scenario_dir must hold scenario.json (scripts/zbook/60-scenario.sh); restart rescue-dispatch"}, st
	}
	d := fmt.Sprintf("TAK server and rescue-dispatch ready (%s streets, %d rubble areas, %d teams); sar-check plays %s", st.Roads.Source, st.Roads.Blockages, len(st.Teams), team.Callsign)
	if st.Roads.Source == "synthetic" {
		return Result{Status: Warn, Code: "E-CHK-511", Detail: d + "; the streets are a synthetic grid, not OpenStreetMap", Evidence: ev,
			Hint: "with internet on the ZBook run scripts/zbook/32-roads.sh --force for the real streets, then restart rescue-dispatch"}, st
	}
	return Result{Status: Pass, Detail: d, Evidence: ev}, st
}

// p5FreeingWait: once every Red and Yellow has been reached, how long to wait
// for teams still freeing their casualty.
const p5FreeingWait = 8 * time.Minute

// p5WaitRescue waits until every Red and Yellow casualty has been reached and
// no team is busy (at most p5FreeingWait for teams still on scene).
func p5WaitRescue(ctx context.Context, o Phase5Options, cfg *config.ZBook, col *p4collector, disp *p5disp, prog io.Writer, extra func() string) (string, bool) {
	start := time.Now()
	lastProg := time.Now()
	quiet, allReached := time.Time{}, time.Time{}
	line := ""
	for {
		select {
		case <-ctx.Done():
			return "interrupted", true
		case <-time.After(time.Second):
		}
		need, got := p5Needed(cfg, col, disp)
		active, _, _, waiting := disp.counts()
		line = fmt.Sprintf("%d of %d Red/Yellow casualties reached", got, need)
		if got >= need && waiting == 0 && need > 0 {
			if allReached.IsZero() {
				allReached = time.Now()
			}
			// Teams still freeing casualties: give them a while, not the whole limit.
			if active > 0 && time.Since(allReached) >= p5FreeingWait {
				return fmt.Sprintf("%s (%d still freeing their casualty)", line, active), false
			}
		} else {
			allReached = time.Time{}
		}
		if got >= need && active == 0 && waiting == 0 && need > 0 {
			if quiet.IsZero() {
				quiet = time.Now()
			} else if time.Since(quiet) >= 10*time.Second {
				return line, false
			}
		} else {
			quiet = time.Time{}
		}
		if time.Since(lastProg) >= 30*time.Second {
			fmt.Fprintf(prog, "  +%3.0f s  %s | %s\n", time.Since(start).Seconds(), line, extra())
			lastProg = time.Now()
		}
		if time.Since(start) >= o.ReachWait {
			fmt.Fprintf(prog, "  stopping (time limit for the rescue)\n")
			return line, true
		}
	}
}

// p5Needed: Red/Yellow casualties in the latest list and how many were reached.
func p5Needed(cfg *config.ZBook, col *p4collector, disp *p5disp) (need, got int) {
	col.mu.Lock()
	var cas []*sarpb.Casualty
	if col.cas != nil {
		cas = col.cas.Casualties
	}
	reachedBy := map[string]bool{}
	disp.mu.Lock()
	for _, id := range disp.order {
		a := disp.asg[id]
		if a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_DONE || a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE {
			reachedBy[col.resolve(a.CasualtyId)] = true
		}
	}
	disp.mu.Unlock()
	col.mu.Unlock()
	for _, c := range cas {
		if needsTeam(cfg, c) {
			need++
			if reachedBy[c.CasualtyId] {
				got++
			}
		}
	}
	return need, got
}

func needsTeam(cfg *config.ZBook, c *sarpb.Casualty) bool {
	name := strings.ToLower(casualty.TriageName(c.Triage))
	for _, t := range cfg.Dispatch.Triage {
		if strings.ToLower(t) == name {
			return true
		}
	}
	return false
}

// p5Map: every casualty on the TAK map in its triage colour where the record
// says, no leftovers, the drones and the simulated teams there too.
func p5Map(cfg *config.ZBook, col *p4collector, tak *p5tak, disp *p5disp, prefix string, st *dispatch.Status) Result {
	col.mu.Lock()
	var cas []*sarpb.Casualty
	if col.cas != nil {
		cas = col.cas.Casualties
	}
	col.mu.Unlock()
	// Casualties a team has finished with (and none is with now): green, RESCUED, when Red or Yellow.
	done, busy := map[string]bool{}, map[string]bool{}
	disp.mu.Lock()
	for _, id := range disp.order {
		a := disp.asg[id]
		switch a.State {
		case sarpb.AssignmentState_ASSIGNMENT_STATE_DONE:
			done[a.CasualtyId] = true
		case sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE, sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
			busy[a.CasualtyId] = true
		}
	}
	disp.mu.Unlock()
	needsTeam := func(c *sarpb.Casualty) bool {
		for _, t := range cfg.Dispatch.Triage {
			if strings.EqualFold(t, casualty.TriageName(c.Triage)) {
				return true
			}
		}
		return false
	}
	rescued := 0
	tak.mu.Lock()
	defer tak.mu.Unlock()
	now := time.Now()
	var ev, bad []string
	listed := map[string]bool{}
	ok, worst := 0, 0.0
	byColour := map[string]int{}
	for _, c := range cas {
		listed[c.CasualtyId] = true
		e := tak.latest[prefix+"cas."+c.CasualtyId]
		isRescued := done[c.CasualtyId] && !busy[c.CasualtyId] && needsTeam(c)
		want, wantLabel := dispatch.MarkerLook(c.CasualtyId, c, isRescued)
		if e == nil {
			bad = append(bad, c.CasualtyId+": no marker")
			continue
		}
		mc, _ := e.MarkerColor()
		d := geo.Distance(geo.Point{Lat: float64(e.Point.Lat), Lon: float64(e.Point.Lon)}, geoOf(c.Position))
		worst = math.Max(worst, d)
		switch {
		case mc != want && isRescued:
			bad = append(bad, fmt.Sprintf("%s: marker colour %d, rescued wants green %d", c.CasualtyId, mc, want))
		case mc != want:
			bad = append(bad, fmt.Sprintf("%s: marker colour %d, triage %s wants %d", c.CasualtyId, mc, casualty.TriageName(c.Triage), want))
		case isRescued && e.Callsign() != wantLabel:
			bad = append(bad, fmt.Sprintf("%s: label %q, rescued wants %q", c.CasualtyId, e.Callsign(), wantLabel))
		case d > p5MarkerM:
			bad = append(bad, fmt.Sprintf("%s: marker %.1f m from the record", c.CasualtyId, d))
		case e.StaleTime().Before(now):
			bad = append(bad, c.CasualtyId+": marker stale")
		default:
			ok++
			byColour[casualty.TriageName(c.Triage)]++
			if isRescued {
				rescued++
			}
		}
	}
	var ghosts []string
	for uid, e := range tak.latest {
		if e != nil && strings.HasPrefix(uid, prefix+"cas.") && !listed[strings.TrimPrefix(uid, prefix+"cas.")] && e.StaleTime().After(now) {
			ghosts = append(ghosts, strings.TrimPrefix(uid, prefix+"cas."))
		}
	}
	sort.Strings(ghosts)
	var drones []string
	missingDrones := 0
	for _, id := range cfg.Check.ExpectUAVs {
		at, seen := tak.at[prefix+"uav."+id]
		if !seen || now.Sub(at) > p5DroneAge {
			if col.latestTel(id) != nil {
				missingDrones++
				drones = append(drones, id+" MISSING")
			}
			continue
		}
		drones = append(drones, fmt.Sprintf("%s (%.0f s ago)", id, now.Sub(at).Seconds()))
	}
	var teams []string
	missingTeams := 0
	if st != nil {
		for _, t := range st.Teams {
			if t.Source != "sim" {
				continue
			}
			if e := tak.latest[prefix+"team."+t.ID]; e != nil {
				teams = append(teams, t.Callsign)
			} else {
				missingTeams++
				teams = append(teams, t.Callsign+" MISSING")
			}
		}
	}
	var parts []string
	for _, k := range []string{"Red", "Yellow", "Green", "Black", "Grey"} {
		if byColour[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", byColour[k], k))
		}
	}
	ev = append(ev, fmt.Sprintf("markers right: %d of %d (%s; %d of them green, RESCUED); furthest %.2f m from its record", ok, len(cas), strings.Join(parts, ", "), rescued, worst))
	ev = append(ev, "drones on the map: "+nonEmptyS(strings.Join(drones, ", "), "none"))
	ev = append(ev, "simulated teams on the map: "+nonEmptyS(strings.Join(teams, ", "), "none"))
	if len(ghosts) > 0 {
		ev = append(ev, "markers left for casualties no longer listed: "+strings.Join(ghosts, ", "))
	}
	ev = append(ev, bad...)
	detail := fmt.Sprintf("%d of %d casualties on the map in their triage colour (green once rescued) within %.0f m; %d drones, %d simulated teams", ok, len(cas), p5MarkerM, len(drones)-missingDrones, len(teams)-missingTeams)
	switch {
	case len(cas) == 0:
		return Result{Status: Fail, Code: "E-CHK-512", Detail: "no casualties to show", Evidence: ev, Hint: "casualty-analyst listed no casualties: see the phase 4 checks"}
	case len(bad) > 0 || missingDrones > 0 || missingTeams > 0:
		return Result{Status: Fail, Code: "E-CHK-512", Detail: detail, Evidence: ev, Hint: "'rescue-dispatch status' (TAK link, events sent); 'docker logs rubblesar-taky'"}
	case len(ghosts) > 0:
		return Result{Status: Warn, Code: "E-CHK-512", Detail: detail + fmt.Sprintf("; %d leftover markers", len(ghosts)), Evidence: ev,
			Hint: "leftovers expire on their own (stale after [dispatch] marker_stale); paste the block"}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p5Routes: the street part of every route stays out of the rubble, the
// route ends at the casualty, and the TAK map showed it.
func p5Routes(disp *p5disp, tak *p5tak, blocks []geom.Poly, water *roads.Network, loc geo.Local, st *dispatch.Status) Result {
	disp.mu.Lock()
	defer disp.mu.Unlock()
	tak.mu.Lock()
	defer tak.mu.Unlock()
	if len(disp.order) == 0 {
		return Result{Status: Fail, Code: "E-CHK-513", Detail: "no routes: rescue-dispatch made no assignments", Hint: "'rescue-dispatch status' shows teams and waiting casualties"}
	}
	inRubble := func(q geom.P) float64 {
		worst := 0.0
		for _, b := range blocks {
			if d := b.SignedDist(q); d < 0 {
				worst = math.Max(worst, -d)
			}
		}
		return worst
	}
	var ev, bad []string
	shown, detours, crossing, wet := 0, 0, 0, 0
	var lengths, ratios []float64
	for _, id := range disp.order {
		a := disp.asg[id]
		r := a.Route
		if r == nil || len(r.Points) < 2 {
			bad = append(bad, id+": no route")
			continue
		}
		pts := make([]geom.P, len(r.Points))
		for i, g := range r.Points {
			x, y := loc.ToLocal(geoOf(g))
			pts[i] = geom.P{X: x, Y: y}
		}
		worst := 0.0
		// From where the team joins the street to where it leaves it.
		for i := 2; i <= int(r.AccessIndex) && i < len(pts); i++ {
			a, b := pts[i-1], pts[i]
			n := max(1, int(math.Ceil(a.Dist(b)/0.5)))
			for k := 0; k <= n; k++ {
				worst = math.Max(worst, inRubble(geom.Lerp(a, b, float64(k)/float64(n))))
			}
		}
		end := pts[len(pts)-1]
		tx, ty := loc.ToLocal(geoOf(a.Target))
		endOff := end.Dist(geom.P{X: tx, Y: ty})
		// The walks off the streets: to the first street point, and from the access point on.
		acrossWater := false
		if water != nil && len(water.Water) > 0 {
			acrossWater = water.CrossesWater(pts[0], pts[1])
			for i := int(r.AccessIndex) + 1; i < len(pts); i++ {
				acrossWater = acrossWater || water.CrossesWater(pts[i-1], pts[i])
			}
			if acrossWater {
				wet++
			}
		}
		switch {
		case acrossWater:
			bad = append(bad, fmt.Sprintf("%s (%s → %s): a walk off the streets crosses a river", id, a.TeamId, a.CasualtyId))
		case worst > p5RouteTolM:
			bad = append(bad, fmt.Sprintf("%s (%s → %s): street part %.1f m inside a rubble area", id, a.TeamId, a.CasualtyId, worst))
		case endOff > 1:
			bad = append(bad, fmt.Sprintf("%s: route ends %.1f m from the casualty", id, endOff))
		}
		if r.RubbleM > 0 {
			crossing++
		}
		if r.DirectM > 0 {
			ratios = append(ratios, float64(r.LengthM/r.DirectM))
			if r.LengthM > 1.4*r.DirectM && r.LengthM-r.DirectM > 50 {
				detours++
			}
		}
		lengths = append(lengths, float64(r.LengthM))
		if _, ok := tak.routes[id]; ok {
			shown++
		}
	}
	n := len(disp.order)
	ev = append(ev, fmt.Sprintf("%d routes, length median %.0f m (longest %.0f m), %.2f times the straight line (median); %d detours; %d end with a walk over rubble",
		n, pct(lengths, 0.5), pct(lengths, 1), pct(ratios, 0.5), detours, crossing))
	if st != nil {
		ev = append(ev, fmt.Sprintf("streets: %s, %.1f km, %d pieces closed by %d rubble areas", st.Roads.Source, st.Roads.LengthKm, st.Roads.BlockedEdges, st.Roads.Blockages))
	}
	ev = append(ev, fmt.Sprintf("route lines seen on the TAK map: %d of %d", shown, n))
	switch {
	case water == nil:
		ev = append(ev, "no street file to check the walks against rivers")
	case len(water.Water) == 0 && water.Source == "osm":
		ev = append(ev, "the street file has no rivers (downloaded by 0.6.0): walks across rivers not checked; scripts/zbook/32-roads.sh downloads it again")
	case len(water.Water) > 0:
		ev = append(ev, fmt.Sprintf("walks off the streets across one of %d rivers: %d", len(water.Water), wet))
	}
	ev = append(ev, bad...)
	detail := fmt.Sprintf("%d of %d routes keep the streets clear of rubble and end at their casualty; %d of %d shown on the TAK map", n-len(bad), n, shown, n)
	switch {
	case len(bad) > 0:
		return Result{Status: Fail, Code: "E-CHK-513", Detail: detail, Evidence: ev, Hint: "paste the block; 'rescue-dispatch roads' lists the closed streets per rubble area"}
	case float64(shown) < 0.9*float64(n):
		return Result{Status: Fail, Code: "E-CHK-513", Detail: detail, Evidence: ev, Hint: "route lines did not reach the TAK map: 'rescue-dispatch status' (TAK events sent), 'docker logs rubblesar-taky'"}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p5Order: Red before Yellow, no team for Black or Grey, every Red and Yellow
// reached, and how long it took.
func p5Order(cfg *config.ZBook, col *p4collector, disp *p5disp, run *p4run) Result {
	need, got := p5Needed(cfg, col, disp)
	disp.mu.Lock()
	defer disp.mu.Unlock()
	class := func(p uint32) uint32 { return (p + 1) / 2 }
	var ev, bad []string
	var waits, travel []float64
	minAge := cfg.Dispatch.MinAge.Duration + 3*time.Second
	for _, id := range disp.order {
		a := disp.asg[id]
		ok := false
		for _, t := range cfg.Dispatch.Triage {
			ok = ok || strings.EqualFold(t, casualty.TriageName(a.Triage))
		}
		if !ok {
			bad = append(bad, fmt.Sprintf("%s: a team for %s %s", id, a.CasualtyId, casualty.TriageName(a.Triage)))
		}
		if a.NeededUnixNs > 0 {
			waits = append(waits, time.Duration(a.CreatedUnixNs-a.NeededUnixNs).Seconds())
		}
		if a.OnSceneUnixNs > 0 {
			travel = append(travel, time.Duration(a.OnSceneUnixNs-a.CreatedUnixNs).Seconds())
		}
	}
	// A Yellow given a team while a Red was already waiting for one.
	var justified []string
	firstAsg := map[string]int64{}
	for _, id := range disp.order {
		a := disp.asg[id]
		cs := disp.cases[id]
		if len(cs) == 0 {
			cs = []string{a.CasualtyId}
		}
		for _, c := range cs {
			if t, ok := firstAsg[c]; !ok || a.CreatedUnixNs < t {
				firstAsg[c] = a.CreatedUnixNs
			}
		}
	}
	for _, id := range disp.order {
		a := disp.asg[id]
		if class(a.Priority) < 2 {
			continue
		}
		at := time.Unix(0, a.CreatedUnixNs)
	reds:
		for rid, spans := range disp.redSpans {
			if t, ok := firstAsg[rid]; ok && t <= a.CreatedUnixNs {
				continue // it already had a team
			}
			for _, sp := range spans {
				// Red for longer than min_age by then, and still Red (lists come every few seconds).
				if at.Sub(sp[0]) > minAge && !sp[1].Before(at.Add(-2*time.Second)) {
					line := fmt.Sprintf("%s (Yellow) got a team at %s while %s, Red since %s, was waiting", a.CasualtyId,
						at.Format("15:04:05"), rid, sp[0].Format("15:04:05"))
					// rescue-dispatch says why when no free team could take the Red.
					if i := strings.Index(a.Note, "more urgent, but not possible now: "); i >= 0 && strings.Contains(a.Note[i:], rid+" ") {
						justified = append(justified, line+": "+a.Note[i:])
					} else {
						bad = append(bad, line)
					}
					break reds
				}
			}
		}
	}
	by := map[string]int{}
	for _, id := range disp.order {
		by[casualty.TriageName(disp.asg[id].Triage)]++
	}
	ev = append(ev, fmt.Sprintf("%d assignments (%d Red, %d Yellow); %d of %d Red/Yellow casualties reached", len(disp.order), by["Red"], by["Yellow"], got, need))
	if len(waits) > 0 {
		ev = append(ev, fmt.Sprintf("waiting for a free team: median %.0f s, longest %.0f s", pct(waits, 0.5), pct(waits, 1)))
	}
	if len(travel) > 0 {
		ev = append(ev, fmt.Sprintf("dispatch to on scene (real time; simulated teams move %gx faster): median %.0f s, longest %.0f s", cfg.Dispatch.TimeScale, pct(travel, 0.5), pct(travel, 1)))
	}
	ev = append(ev, bad...)
	ev = append(ev, justified...)
	detail := fmt.Sprintf("%d of %d Red/Yellow casualties reached, %d order problems", got, need, len(bad))
	switch {
	case need == 0:
		return Result{Status: Fail, Code: "E-CHK-514", Detail: "no Red or Yellow casualty to send a team to", Evidence: ev, Hint: "see the triage check of phase 4 (sar-check phase4)"}
	case len(bad) > 0:
		return Result{Status: Fail, Code: "E-CHK-514", Detail: detail, Evidence: ev, Hint: "paste the block and 'rescue-dispatch status'"}
	case got < need && (run.timeLimited || float64(got) >= 0.8*float64(need)):
		return Result{Status: Warn, Code: "E-CHK-514", Detail: detail + " (time limit)", Evidence: ev, Hint: "'rescue-dispatch status' shows the teams still busy; more or faster teams: [[dispatch.team]], [dispatch] time_scale"}
	case got < need:
		return Result{Status: Fail, Code: "E-CHK-514", Detail: detail, Evidence: ev, Hint: "'rescue-dispatch status' lists casualties with no route or waiting; paste the block"}
	case len(justified) > 0:
		return Result{Status: Warn, Code: "E-CHK-514", Detail: fmt.Sprintf("%s; %d Yellow sent while a Red waited that no free team could take (rescue-dispatch's reason above)", detail, len(justified)), Evidence: ev,
			Hint: "the free team could not reach the Red or had declined it; check the reason is sound"}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p5Loop: the TAK team got its assignment over TAK (route line and chat),
// walked it, was seen on scene, said "done" and was released.
func p5Loop(pl *p5player, tak *p5tak, disp *p5disp, team *config.Team) Result {
	pl.mu.Lock()
	loops := append([]*p5loop(nil), pl.loops...)
	plis := pl.pliCount
	pl.mu.Unlock()
	tak.mu.Lock()
	chats := append([]p5chat(nil), tak.chats...)
	tak.mu.Unlock()
	disp.mu.Lock()
	for _, l := range loops {
		// The dispatch state is the record (the next assignment can arrive in the
		// same second as the release, before the player looked).
		if t, ok := disp.scene[l.asg]; ok && (l.sceneAt.IsZero() || t.Before(l.sceneAt)) {
			l.sceneAt = t
		}
		if t, ok := disp.done[l.asg]; ok && (l.doneAt.IsZero() || t.Before(l.doneAt)) {
			l.doneAt = t
		}
		if a := disp.asg[l.asg]; a != nil {
			l.cas = a.CasualtyId
			if a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_CANCELLED {
				l.cancelled = true
			}
		}
	}
	onlineAt, wasOnline := disp.online[team.ID]
	nAsg := 0
	for _, id := range disp.order {
		if disp.asg[id].TeamId == team.ID {
			nAsg++
		}
	}
	disp.mu.Unlock()
	var ev []string
	ev = append(ev, fmt.Sprintf("%s sent %d position reports; rescue-dispatch had it online: %v (%s); %d assignments for it", team.Callsign, plis, wasOnline, onlineAt.Format("15:04:05"), nAsg))
	complete, direct := 0, 0
	for _, l := range loops {
		// The chat that named this assignment's casualty for us.
		for _, c := range chats {
			if l.cas != "" && strings.Contains(c.text, l.cas) && !c.at.Before(l.routeAt.Add(-10*time.Second)) && c.at.Before(l.routeAt.Add(30*time.Second)) &&
				(c.direct || strings.Contains(c.text, team.Callsign)) {
				if l.chatAt.IsZero() || c.direct {
					l.chatAt, l.direct = c.at, c.direct
				}
			}
		}
		state := "en route"
		switch {
		case l.cancelled:
			state = "cancelled by rescue-dispatch"
		case !l.doneAt.IsZero():
			state = fmt.Sprintf("released %.0f s after our \"done\"", math.Max(0, l.doneAt.Sub(l.firstDone).Seconds()))
		case !l.doneSentAt.IsZero():
			state = "\"done\" sent, not yet released"
		case !l.arrivedAt.IsZero():
			state = "arrived"
		}
		on := "not seen on scene"
		if !l.sceneAt.IsZero() {
			on = fmt.Sprintf("on scene %.0f s after arriving", math.Max(0, l.sceneAt.Sub(l.arrivedAt).Seconds()))
		}
		msgWord := "no chat naming it"
		if !l.chatAt.IsZero() {
			msgWord = "All Chat Rooms message"
			if l.direct {
				msgWord = "direct message"
			}
		}
		walked := "did not arrive"
		if !l.arrivedAt.IsZero() {
			walked = fmt.Sprintf("walked in %.0f s", l.arrivedAt.Sub(l.startAt).Seconds())
		}
		ev = append(ev, fmt.Sprintf("%s → %s: route line with %d points (%.0f m), %s; %s; %s; %s", l.asg, nonEmptyS(l.cas, "?"), l.points, l.lengthM, msgWord,
			walked, on, state))
		if !l.doneAt.IsZero() && !l.sceneAt.IsZero() && !l.chatAt.IsZero() && l.doneAt.Sub(l.firstDone) <= p5DoneWithin {
			complete++
			if l.direct {
				direct++
			}
		}
	}
	detail := fmt.Sprintf("%s (sar-check on the TAK server): %d assignments completed over TAK (%d with a direct message)", team.Callsign, complete, direct)
	switch {
	case !wasOnline:
		return Result{Status: Fail, Code: "E-CHK-515", Detail: team.Callsign + " never showed as online in rescue-dispatch", Evidence: ev, Hint: "the TAK server does not pass position reports on: 'docker logs rubblesar-taky'"}
	case len(loops) == 0:
		return Result{Status: Fail, Code: "E-CHK-515", Detail: team.Callsign + " received no route line", Evidence: ev,
			Hint: fmt.Sprintf("%d assignments were made for it; if none, the simulated teams took every casualty first (fine, but the loop is untested: run again)", nAsg)}
	case complete == 0:
		return Result{Status: Fail, Code: "E-CHK-515", Detail: detail, Evidence: ev, Hint: "paste the block and 'journalctl -u rescue-dispatch -n 50'"}
	case direct == 0:
		return Result{Status: Warn, Code: "E-CHK-515", Detail: detail + "; direct messages did not arrive (only All Chat Rooms)", Evidence: ev,
			Hint: "the TAK server does not route messages addressed to one callsign (<marti><dest>); ATAK users still see All Chat Rooms"}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p5Chat: a message in All Chat Rooms for every dispatch and every arrival.
func p5Chat(disp *p5disp, tak *p5tak, cfg *config.ZBook) Result {
	disp.mu.Lock()
	var asg []*sarpb.Assignment
	for _, id := range disp.order {
		asg = append(asg, disp.asg[id])
	}
	disp.mu.Unlock()
	tak.mu.Lock()
	chats := append([]p5chat(nil), tak.chats...)
	tak.mu.Unlock()
	callsign := map[string]string{}
	for _, t := range cfg.Dispatch.Teams {
		callsign[t.ID] = t.Callsign
	}
	sent, reached, wantReached := 0, 0, 0
	var missing []string
	for _, a := range asg {
		cs := callsign[a.TeamId]
		found := false
		for _, c := range chats {
			if !c.direct && strings.HasPrefix(c.text, cs+" → ") {
				found = true
				break
			}
		}
		if found {
			sent++
		} else {
			missing = append(missing, fmt.Sprintf("no dispatch message for %s (%s → %s)", a.AssignmentId, cs, a.CasualtyId))
		}
		if a.OnSceneUnixNs > 0 {
			wantReached++
			for _, c := range chats {
				if strings.HasPrefix(c.text, cs+" reached ") {
					reached++
					break
				}
			}
		}
	}
	var sample []string
	for i, c := range chats {
		if i >= 6 {
			break
		}
		sample = append(sample, fmt.Sprintf("%s %s: %s", c.at.Format("15:04:05"), c.from, c.text))
	}
	total, got := len(asg)+wantReached, sent+min(reached, wantReached)
	ev := []string{fmt.Sprintf("%d chat messages received; dispatch messages %d of %d, arrival messages %d of %d", len(chats), sent, len(asg), min(reached, wantReached), wantReached)}
	ev = append(ev, sample...)
	if len(missing) > 8 {
		missing = append(missing[:8], fmt.Sprintf("... and %d more", len(missing)-8))
	}
	ev = append(ev, missing...)
	frac := 0.0
	if total > 0 {
		frac = float64(got) / float64(total)
	}
	detail := fmt.Sprintf("%d of %d dispatch and arrival messages arrived in All Chat Rooms (%.0f%%)", got, total, 100*frac)
	switch {
	case total == 0:
		return Result{Status: Fail, Code: "E-CHK-516", Detail: "nothing to announce: no assignments", Evidence: ev}
	case frac >= p5ChatPass:
		return Result{Status: Pass, Detail: detail, Evidence: ev}
	case frac >= p5ChatWarn:
		return Result{Status: Warn, Code: "E-CHK-516", Detail: detail, Evidence: ev, Hint: "some chat messages were lost: 'rescue-dispatch status' (TAK reconnects, dropped events)"}
	}
	return Result{Status: Fail, Code: "E-CHK-516", Detail: detail, Evidence: ev, Hint: "'rescue-dispatch status'; 'docker logs rubblesar-taky'; paste the block"}
}
