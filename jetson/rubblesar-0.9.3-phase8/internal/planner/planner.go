// Package planner is mission-planner's service: it turns casualties that need
// a close look (casualty-analyst's needs_investigation) into investigation
// tasks, gives each to the nearest free investigator that has the battery for
// it, follows the drones' progress in their telemetry and publishes the task
// list (sar/{incident}/tasks), which casualty-analyst reads for the outcome.
//
// Order: tier 1 (provisionally Red: may be alive, not yet watched close up)
// before tier 2 (unknown: temperature unreadable, or cold from far away)
// before tier 3 (position too uncertain); within a tier the look nearest a
// free investigator, unless one has waited 4 minutes. When no drone is free,
// a more urgent task takes a drone still flying to a less urgent one (which waits).
//
// A casualty gets at most max_attempts completed looks. A drone that does not
// answer a command, aborts (battery, restart) or makes no progress for
// task_timeout gives the task back to the queue (at most 3 assignments).
package planner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"rubblesar/internal/bus"
	"rubblesar/internal/casualty"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// Config configures the service.
type Config struct {
	Incident string
	Source   string
	Centre   geo.Point
	P        config.Planner
	// StateFile keeps the tasks across restarts ("" = not kept).
	StateFile string
	// Tick: how often tasks are planned (default 1 s).
	Tick time.Duration
	// Settle: after a look, how long to wait for its reports to reach
	// casualty-analyst before another look at the same casualty (default 20 s).
	Settle time.Duration
	// OnlineAfter: a drone with no telemetry for this long is offline (default 10 s).
	OnlineAfter time.Duration
	// MinAge: a casualty gets a task once it has been listed this long
	// (default 10 s): a new record firms up as the scout passes over it.
	MinAge time.Duration
}

// FromZBook builds the service config.
func FromZBook(c *config.ZBook, source string) Config {
	return Config{Incident: c.Site.IncidentID, Source: source, Centre: geo.Point{Lat: c.Site.CenterLat, Lon: c.Site.CenterLon},
		P: c.Planner, StateFile: filepath.Join(c.Planner.StateDir, c.Site.IncidentID+".json")}
}

const (
	maxAssignments = 3                // per task
	marginS        = 60.0             // take-off, climb, descent and landing, seconds
	backoff        = 30 * time.Second // a drone that failed a command is left alone this long
	minTakeoffPct  = 50.0             // a landed drone takes off only with this much (flightsim)
)

// Drone is mission-planner's view of one drone.
type Drone struct {
	ID          string    `json:"id"`
	Role        string    `json:"role"`
	Investigate bool      `json:"investigator"`
	State       string    `json:"state"`
	BatteryPct  float32   `json:"battery_pct"`
	FlightLeftS uint32    `json:"flight_time_left_s"`
	E           float64   `json:"e"`
	N           float64   `json:"n"`
	Task        string    `json:"task,omitempty"`       // ours, assigned
	DroneTask   string    `json:"drone_task,omitempty"` // what its telemetry says
	DroneState  string    `json:"drone_task_state,omitempty"`
	LastSeen    time.Time `json:"last_seen"`
	Online      bool      `json:"online"`
	Note        string    `json:"note,omitempty"`

	tel     *sarpb.Telemetry
	homeE   float64
	homeN   float64
	cruise  float64
	blocked time.Time
}

type pending struct {
	task, uav string
	sent      time.Time
	cancel    bool
}

// Planner is safe for concurrent use.
type Planner struct {
	cfg   Config
	b     bus.Bus
	log   *slog.Logger
	local geo.Local
	subs  []bus.Subscription
	pub   bus.Publisher
	ctl   map[string]bus.Publisher

	mu       sync.Mutex
	run      string // run token in task ids
	next     int
	tasks    map[string]*sarpb.Task
	order    []string
	progress map[string]time.Time // task -> last progress
	seen     map[string]bool      // task -> the drone has shown it in progress since it was assigned
	drones   map[string]*Drone
	cas      map[string]*sarpb.Casualty
	casAt    time.Time
	casFirst map[string]time.Time // when each casualty id was first listed
	lists    uint64
	cmds     map[uint64]pending
	seq      msg.Seq
	hseq     msg.Seq
	dirty    bool
	resets   int
}

type persisted struct {
	Run   string            `json:"run"`
	Next  int               `json:"next"`
	Tasks []json.RawMessage `json:"tasks"`
}

// New makes a planner; Open subscribes it.
func New(cfg Config, b bus.Bus, log *slog.Logger) *Planner {
	if cfg.Tick <= 0 {
		cfg.Tick = time.Second
	}
	if cfg.Settle <= 0 {
		cfg.Settle = 20 * time.Second
	}
	if cfg.OnlineAfter <= 0 {
		cfg.OnlineAfter = 10 * time.Second
	}
	if cfg.MinAge <= 0 {
		cfg.MinAge = 10 * time.Second
	}
	if cfg.Source == "" {
		cfg.Source = "zbook/mission-planner"
	}
	return &Planner{cfg: cfg, b: b, log: log, local: geo.NewLocal(cfg.Centre), ctl: map[string]bus.Publisher{}, run: runToken(time.Now()),
		next: 1, tasks: map[string]*sarpb.Task{}, progress: map[string]time.Time{}, seen: map[string]bool{}, drones: map[string]*Drone{},
		cas: map[string]*sarpb.Casualty{}, casFirst: map[string]time.Time{}, cmds: map[uint64]pending{}}
}

// runToken: three base-36 characters from the start time, so task ids of a
// new run differ from the last run's (which drones may still show).
func runToken(t time.Time) string {
	s := strconv.FormatInt(t.Unix()/2%46656, 36)
	return strings.Repeat("0", 3-len(s)) + s
}

// Open loads the state file and subscribes to telemetry, control replies and casualties.
func Open(cfg Config, b bus.Bus, log *slog.Logger) (*Planner, error) {
	p := New(cfg, b, log)
	if err := p.load(); err != nil {
		return nil, err
	}
	var err error
	if p.pub, err = b.DeclarePublisher(keys.Tasks(cfg.Incident), bus.QoSCasualty); err != nil {
		return nil, err
	}
	for _, s := range []struct {
		key string
		h   bus.Handler
	}{{keys.TelemetryAll(cfg.Incident), p.onTelemetry}, {keys.ControlReplyAll(cfg.Incident), p.onReply}, {keys.Casualties(cfg.Incident), p.onCasualties}} {
		sub, err := b.Subscribe(s.key, s.h)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.subs = append(p.subs, sub)
	}
	return p, nil
}

func (p *Planner) load() error {
	if p.cfg.StateFile == "" {
		return nil
	}
	b, err := os.ReadFile(p.cfg.StateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return diag.Wrap("E-PLN-002", err, "reading %s", p.cfg.StateFile)
	}
	var st persisted
	if err := json.Unmarshal(b, &st); err != nil {
		return diag.Wrap("E-PLN-002", err, "reading %s", p.cfg.StateFile)
	}
	if st.Run != "" {
		p.run = st.Run
	}
	p.next = max(st.Next, 1)
	for _, raw := range st.Tasks {
		t := &sarpb.Task{}
		if err := protoJSONUnmarshal(raw, t); err != nil {
			return diag.Wrap("E-PLN-002", err, "reading %s", p.cfg.StateFile)
		}
		p.tasks[t.TaskId] = t
		p.order = append(p.order, t.TaskId)
		p.progress[t.TaskId] = time.Now()
	}
	return nil
}

func (p *Planner) saveLocked() {
	if p.cfg.StateFile == "" {
		return
	}
	st := persisted{Run: p.run, Next: p.next}
	for _, id := range p.order {
		b, err := protoJSONMarshal(p.tasks[id])
		if err == nil {
			st.Tasks = append(st.Tasks, b)
		}
	}
	b, _ := json.MarshalIndent(st, "", " ")
	if err := os.MkdirAll(filepath.Dir(p.cfg.StateFile), 0o755); err != nil {
		p.log.Warn("saving tasks", "err", diag.Wrap("E-PLN-002", err, "creating %s", filepath.Dir(p.cfg.StateFile)))
		return
	}
	tmp := p.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		p.log.Warn("saving tasks", "err", diag.Wrap("E-PLN-002", err, "writing %s", tmp))
		return
	}
	_ = os.Rename(tmp, p.cfg.StateFile)
}

// --- Inputs -----------------------------------------------------------------

func (p *Planner) onCasualties(s bus.Sample) {
	var l sarpb.CasualtyList
	if msg.Unmarshal(s.Payload, &l) != nil || msg.CheckHeader(l.GetHeader(), p.cfg.Incident) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cas = map[string]*sarpb.Casualty{}
	now := time.Now()
	for _, c := range l.Casualties {
		p.cas[c.CasualtyId] = c
		if _, ok := p.casFirst[c.CasualtyId]; !ok {
			p.casFirst[c.CasualtyId] = now
		}
	}
	p.casAt = now
	p.lists++
}

func (p *Planner) investigator(t *sarpb.Telemetry) bool {
	if len(p.cfg.P.Investigators) > 0 {
		for _, id := range p.cfg.P.Investigators {
			if id == t.UavId {
				return true
			}
		}
		return false
	}
	return t.Role == sarpb.UavRole_UAV_ROLE_INVESTIGATOR
}

func lowerEnum(s, prefix string) string { return strings.ToLower(strings.TrimPrefix(s, prefix)) }

func (p *Planner) onTelemetry(s bus.Sample) {
	var t sarpb.Telemetry
	if msg.Unmarshal(s.Payload, &t) != nil || msg.CheckHeader(t.GetHeader(), p.cfg.Incident) != nil || t.UavId == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.drones[t.UavId]
	if d == nil {
		d = &Drone{ID: t.UavId}
		p.drones[t.UavId] = d
	}
	d.tel = &t
	d.Role, d.Investigate = lowerEnum(t.Role.String(), "UAV_ROLE_"), p.investigator(&t)
	d.State, d.BatteryPct, d.FlightLeftS = lowerEnum(t.State.String(), "FLIGHT_STATE_"), t.BatteryPct, t.FlightTimeLeftS
	d.E, d.N = p.local.ToLocal(geo.Point{Lat: t.GetPosition().GetLatDeg(), Lon: t.GetPosition().GetLonDeg()})
	if h := t.Home; h != nil {
		d.homeE, d.homeN = p.local.ToLocal(geo.Point{Lat: h.LatDeg, Lon: h.LonDeg})
	}
	d.cruise = float64(t.CruiseMps)
	d.LastSeen = time.Now()
	d.DroneTask, d.DroneState = "", ""
	ts := t.Task
	if ts == nil {
		return
	}
	d.DroneTask, d.DroneState = ts.TaskId, lowerEnum(ts.State.String(), "TASK_STATE_")
	task := p.tasks[ts.TaskId]
	if task == nil || task.UavId != t.UavId || final(task.State) {
		return
	}
	now := time.Now()
	switch ts.State {
	case sarpb.TaskState_TASK_STATE_EN_ROUTE:
		p.seen[task.TaskId] = true
		if task.State != sarpb.TaskState_TASK_STATE_EN_ROUTE {
			p.set(task, sarpb.TaskState_TASK_STATE_EN_ROUTE, "")
			p.progress[task.TaskId] = now
		}
		if t.GroundSpeedMps > 0.5 || math.Abs(float64(t.ClimbMps)) > 0.2 {
			p.progress[task.TaskId] = now
		}
	case sarpb.TaskState_TASK_STATE_ON_STATION:
		p.seen[task.TaskId] = true
		if task.State != sarpb.TaskState_TASK_STATE_ON_STATION {
			task.OnStationUnixNs = now.UnixNano()
			p.set(task, sarpb.TaskState_TASK_STATE_ON_STATION, "")
			p.progress[task.TaskId] = now
		}
		if r := task.Result; r == nil || r.Frames != ts.Frames {
			p.progress[task.TaskId] = now
		}
		task.Result = result(ts)
	case sarpb.TaskState_TASK_STATE_DONE:
		// Only once this assignment was seen in progress (the drone shows its
		// last task until the next: a stale DONE must not count).
		if !p.seen[task.TaskId] || task.State != sarpb.TaskState_TASK_STATE_ON_STATION {
			return
		}
		task.Result = result(ts)
		task.FinishedUnixNs = ts.FinishedUnixNs
		if task.FinishedUnixNs == 0 {
			task.FinishedUnixNs = now.UnixNano()
		}
		if task.OnStationUnixNs == 0 {
			task.OnStationUnixNs = ts.StartedUnixNs
		}
		note := fmt.Sprintf("%d frames, %d parts near the target", ts.Frames, len(ts.TrackIds))
		if !task.Result.Found {
			note = fmt.Sprintf("%d frames, nothing seen within %.0f m of the target", ts.Frames, p.cfg.P.RadiusM)
		} else if ts.Moved {
			note += ", movement seen"
		}
		p.set(task, sarpb.TaskState_TASK_STATE_DONE, note)
		d.Task = ""
		p.log.Info("investigation done", "task", task.TaskId, "casualty", task.CasualtyId, "uav", d.ID, "found", task.Result.Found, "moved", ts.Moved, "frames", ts.Frames)
	case sarpb.TaskState_TASK_STATE_ABORTED:
		if !p.seen[task.TaskId] {
			return
		}
		d.Task = ""
		p.requeue(task, "aborted by "+d.ID+": "+nonEmpty(ts.Reason, "no reason given"))
	}
}

func result(ts *sarpb.TaskStatus) *sarpb.TaskResult {
	return &sarpb.TaskResult{Frames: ts.Frames, TrackIds: append([]string(nil), ts.TrackIds...), Found: len(ts.TrackIds) > 0, Moved: ts.Moved, OffsetM: ts.OffsetM}
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func (p *Planner) onReply(s bus.Sample) {
	var r sarpb.ControlReply
	if msg.Unmarshal(s.Payload, &r) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.cmds[r.InReplyToSeq]
	if !ok || c.uav != r.UavId {
		return
	}
	delete(p.cmds, r.InReplyToSeq)
	task := p.tasks[c.task]
	if task == nil || c.cancel {
		return
	}
	if r.Ok {
		if task.State == sarpb.TaskState_TASK_STATE_ASSIGNED && task.UavId == c.uav {
			p.progress[task.TaskId] = time.Now()
		}
		return
	}
	if d := p.drones[c.uav]; d != nil {
		d.blocked, d.Note = time.Now().Add(backoff), r.Error
		if d.Task == task.TaskId {
			d.Task = ""
		}
	}
	if task.UavId == c.uav && !final(task.State) {
		p.requeue(task, c.uav+" refused: "+r.Error)
	}
}

// --- State changes ------------------------------------------------------------

func final(s sarpb.TaskState) bool {
	switch s {
	case sarpb.TaskState_TASK_STATE_DONE, sarpb.TaskState_TASK_STATE_FAILED, sarpb.TaskState_TASK_STATE_CANCELLED:
		return true
	}
	return false
}

func active(s sarpb.TaskState) bool {
	switch s {
	case sarpb.TaskState_TASK_STATE_ASSIGNED, sarpb.TaskState_TASK_STATE_EN_ROUTE, sarpb.TaskState_TASK_STATE_ON_STATION:
		return true
	}
	return false
}

func (p *Planner) set(t *sarpb.Task, s sarpb.TaskState, note string) {
	t.State = s
	if note != "" {
		t.Note = note
	}
	if final(s) && t.FinishedUnixNs == 0 {
		t.FinishedUnixNs = time.Now().UnixNano()
	}
	p.dirty = true
}

// requeue puts a task back in the queue, or fails it after maxAssignments.
func (p *Planner) requeue(t *sarpb.Task, why string) {
	p.log.Warn("investigation interrupted", "task", t.TaskId, "casualty", t.CasualtyId, "uav", t.UavId, "attempt", t.Attempt, "why", why)
	if d := p.drones[t.UavId]; d != nil && d.Task == t.TaskId {
		d.Task = ""
	}
	if t.Attempt >= maxAssignments {
		p.set(t, sarpb.TaskState_TASK_STATE_FAILED, why+fmt.Sprintf(" (given up after %d assignments)", t.Attempt))
		return
	}
	t.UavId, t.AssignedUnixNs, t.OnStationUnixNs, t.Result = "", 0, 0, nil
	p.set(t, sarpb.TaskState_TASK_STATE_QUEUED, why)
}

// --- Planning -------------------------------------------------------------------

// Step plans once: new tasks, cancellations, timeouts and assignments.
func (p *Planner) Step() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, d := range p.drones {
		d.Online = now.Sub(d.LastSeen) <= p.cfg.OnlineAfter
	}
	// Commands nobody answered.
	for seq, c := range p.cmds {
		if now.Sub(c.sent) < p.cfg.P.CommandTimeout.Duration {
			continue
		}
		delete(p.cmds, seq)
		if d := p.drones[c.uav]; d != nil {
			d.blocked, d.Note = now.Add(backoff), fmt.Sprintf("no answer within %v", p.cfg.P.CommandTimeout.Duration)
		}
		if t := p.tasks[c.task]; t != nil && !c.cancel && t.UavId == c.uav && active(t.State) {
			p.requeue(t, fmt.Sprintf("%s did not answer within %v", c.uav, p.cfg.P.CommandTimeout.Duration))
		}
	}
	// No progress, or the drone went silent.
	for _, id := range p.order {
		t := p.tasks[id]
		if !active(t.State) {
			continue
		}
		if now.Sub(p.progress[id]) > p.cfg.P.TaskTimeout.Duration {
			p.requeue(t, fmt.Sprintf("no progress for %v", p.cfg.P.TaskTimeout.Duration))
		}
	}
	p.planTasks(now)
	p.assign(now)
}

func (p *Planner) planTasks(now time.Time) {
	done := map[string]int{}         // completed looks per casualty
	open := map[string]*sarpb.Task{} // queued or active task per casualty
	lastDone := map[string]time.Time{}
	for _, id := range p.order {
		t := p.tasks[id]
		switch {
		case t.State == sarpb.TaskState_TASK_STATE_DONE:
			done[t.CasualtyId]++
			if f := time.Unix(0, t.FinishedUnixNs); f.After(lastDone[t.CasualtyId]) {
				lastDone[t.CasualtyId] = f
			}
		case !final(t.State):
			open[t.CasualtyId] = t
		}
	}
	if p.casAt.IsZero() {
		return
	}
	// Queued tasks whose casualty no longer needs a look (or is gone) are cancelled;
	// their target and tier follow the casualty.
	for cid, t := range open {
		if t.State != sarpb.TaskState_TASK_STATE_QUEUED {
			// A task under way keeps its drone, but follows its casualty's
			// urgency (so a more urgent task does not take the drone from it).
			if c := p.cas[cid]; c != nil && casualty.Tier(c) > 0 && casualty.Tier(c) != t.Tier {
				t.Tier, p.dirty = casualty.Tier(c), true
			}
			continue
		}
		c := p.cas[cid]
		switch {
		case c == nil:
			p.set(t, sarpb.TaskState_TASK_STATE_CANCELLED, "casualty merged into another or gone")
		case casualty.Tier(c) == 0:
			p.set(t, sarpb.TaskState_TASK_STATE_CANCELLED, "no longer needs a look: "+c.Reason)
		default:
			t.Tier, t.Reason = casualty.Tier(c), c.InvestigationReason
			if !proto.Equal(t.Target, c.Position) {
				t.Target = proto.Clone(c.Position).(*sarpb.GeoPoint)
			}
		}
	}
	ids := make([]string, 0, len(p.cas))
	for id := range p.cas {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := p.cas[id]
		tier := casualty.Tier(c)
		if tier == 0 || open[id] != nil || done[id] >= p.cfg.P.MaxAttempts || now.Sub(p.casFirst[id]) < p.cfg.MinAge {
			continue
		}
		// After a look, wait until casualty-analyst has had its reports.
		if l := lastDone[id]; !l.IsZero() && (p.casAt.Before(l.Add(p.cfg.Settle)) || int(c.Investigations) < done[id]) {
			continue
		}
		t := &sarpb.Task{TaskId: fmt.Sprintf("T-%s-%03d", p.run, p.next), CasualtyId: id, Kind: "investigate", Tier: tier,
			Reason: c.InvestigationReason, State: sarpb.TaskState_TASK_STATE_QUEUED, Target: proto.Clone(c.Position).(*sarpb.GeoPoint),
			AltAglM: float32(p.cfg.P.AltAGLm), HoverS: uint32(p.cfg.P.HoverS), CreatedUnixNs: now.UnixNano()}
		p.next++
		p.tasks[t.TaskId], p.order = t, append(p.order, t.TaskId)
		p.dirty = true
		p.log.Info("investigation queued", "task", t.TaskId, "casualty", id, "tier", tier, "why", t.Reason)
	}
}

// cost of a task for a drone: distance to the target, and the battery it needs
// (there, the hover, and home, plus take-off and landing).
func (p *Planner) cost(d *Drone, t *sarpb.Task) (dist, needPct float64, ok bool, why string) {
	te, tn := p.local.ToLocal(geo.Point{Lat: t.Target.GetLatDeg(), Lon: t.Target.GetLonDeg()})
	dist = math.Hypot(te-d.E, tn-d.N)
	home := math.Hypot(te-d.homeE, tn-d.homeN)
	cruise := d.cruise
	if cruise <= 0 {
		cruise = 5
	}
	needS := (dist+home)/cruise + float64(t.HoverS) + marginS
	bat := float64(d.BatteryPct)
	if d.FlightLeftS == 0 || bat <= 0 {
		return dist, 100, false, "battery empty"
	}
	needPct = needS / float64(d.FlightLeftS) * bat
	if bat-needPct < p.cfg.P.ReservePct {
		return dist, needPct, false, fmt.Sprintf("battery %.0f%%: needs %.0f%% and keeps %.0f%%", bat, needPct, p.cfg.P.ReservePct)
	}
	if d.State == "landed" && bat < minTakeoffPct {
		return dist, needPct, false, fmt.Sprintf("charging (%.0f%%, takes off at %.0f%%)", bat, minTakeoffPct)
	}
	return dist, needPct, true, ""
}

// free: an online investigator with no task of ours and none of its own in progress.
func (p *Planner) free(d *Drone, now time.Time) bool {
	if !d.Online || !d.Investigate || d.Task != "" || now.Before(d.blocked) {
		return false
	}
	if d.DroneTask != "" && (d.DroneState == "en_route" || d.DroneState == "on_station") {
		if t := p.tasks[d.DroneTask]; t == nil || t.UavId != d.ID || !active(t.State) {
			return false // flying a task we do not know (from before a restart)
		}
	}
	return true
}

func (p *Planner) assign(now time.Time) {
	var queue []*sarpb.Task
	for _, id := range p.order {
		if t := p.tasks[id]; t.State == sarpb.TaskState_TASK_STATE_QUEUED {
			queue = append(queue, t)
		}
	}
	ids := make([]string, 0, len(p.drones))
	for id := range p.drones {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var free []*Drone
	for _, id := range ids {
		if p.free(p.drones[id], now) {
			free = append(free, p.drones[id])
		}
	}
	p.orderQueue(queue, free, now)
	for _, t := range queue {
		var best *Drone
		bestD, bestNeed := math.Inf(1), 0.0
		var whyNot []string
		for _, id := range ids {
			d := p.drones[id]
			if !p.free(d, now) {
				continue
			}
			dist, need, ok, why := p.cost(d, t)
			if !ok {
				whyNot = append(whyNot, d.ID+": "+why)
				continue
			}
			if dist < bestD {
				best, bestD, bestNeed = d, dist, need
			}
		}
		if best == nil {
			best, bestD, bestNeed = p.preempt(t, ids, now)
		}
		if best == nil {
			note := "waiting for a free investigator"
			if len(whyNot) > 0 {
				note += " (" + strings.Join(whyNot, "; ") + ")"
			}
			if t.Note != note {
				t.Note, p.dirty = note, true
			}
			continue
		}
		p.send(best, t, bestD, bestNeed, now)
	}
}

// queueAge: a close look waiting this long goes first in its tier.
const queueAge = 4 * time.Minute

// orderQueue sorts the queued close looks: the most urgent tier first; within
// a tier, any look waiting longer than queueAge (oldest first), then the one
// nearest a free investigator, so the drones work through a site before
// crossing the incident instead of flying in the order things were found.
func (p *Planner) orderQueue(queue []*sarpb.Task, free []*Drone, now time.Time) {
	near := map[string]float64{}
	for _, t := range queue {
		te, tn := p.local.ToLocal(geo.Point{Lat: t.Target.GetLatDeg(), Lon: t.Target.GetLonDeg()})
		d := math.Inf(1)
		for _, dr := range free {
			d = math.Min(d, math.Hypot(te-dr.E, tn-dr.N))
		}
		near[t.TaskId] = d
	}
	old := func(t *sarpb.Task) bool { return now.Sub(time.Unix(0, t.CreatedUnixNs)) >= queueAge }
	sort.SliceStable(queue, func(i, j int) bool {
		a, b := queue[i], queue[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if old(a) != old(b) {
			return old(a)
		}
		if old(a) || near[a.TaskId] == near[b.TaskId] {
			return a.CreatedUnixNs < b.CreatedUnixNs
		}
		return near[a.TaskId] < near[b.TaskId]
	})
}

// preempt takes a drone flying to a less urgent task (not yet on station).
func (p *Planner) preempt(t *sarpb.Task, ids []string, now time.Time) (*Drone, float64, float64) {
	var best *Drone
	var bestT *sarpb.Task
	bestD, bestNeed := math.Inf(1), 0.0
	for _, id := range ids {
		d := p.drones[id]
		if !d.Online || !d.Investigate || d.Task == "" || now.Before(d.blocked) {
			continue
		}
		cur := p.tasks[d.Task]
		if cur == nil || cur.Tier <= t.Tier || (cur.State != sarpb.TaskState_TASK_STATE_EN_ROUTE && cur.State != sarpb.TaskState_TASK_STATE_ASSIGNED) {
			continue
		}
		dist, need, ok, _ := p.cost(d, t)
		if ok && dist < bestD {
			best, bestT, bestD, bestNeed = d, cur, dist, need
		}
	}
	if best == nil {
		return nil, 0, 0
	}
	bestT.Attempt-- // not the task's fault
	p.requeue(bestT, fmt.Sprintf("%s taken for the more urgent %s (tier %d)", best.ID, t.TaskId, t.Tier))
	return best, bestD, bestNeed
}

func (p *Planner) send(d *Drone, t *sarpb.Task, dist, need float64, now time.Time) {
	pub := p.ctl[d.ID]
	if pub == nil {
		var err error
		if pub, err = p.b.DeclarePublisher(keys.Control(p.cfg.Incident, d.ID), bus.QoSControl); err != nil {
			p.log.Warn("control publisher", "uav", d.ID, "err", err)
			return
		}
		p.ctl[d.ID] = pub
	}
	seq := p.seq.Next()
	c := &sarpb.Control{Header: msg.NewHeader(p.cfg.Incident, p.cfg.Source, seq), UavId: d.ID, Cmd: &sarpb.Control_Investigate{Investigate: &sarpb.Investigate{
		TaskId: t.TaskId, CasualtyId: t.CasualtyId, Target: t.Target, AltAglM: t.AltAglM, HoverS: t.HoverS, RadiusM: float32(p.cfg.P.RadiusM), Priority: t.Tier}}}
	b, err := msg.Marshal(c)
	if err == nil {
		err = pub.Put(b)
	}
	if err != nil {
		p.log.Warn("sending a task", "uav", d.ID, "task", t.TaskId, "err", err)
		d.blocked = now.Add(backoff)
		return
	}
	p.cmds[seq] = pending{task: t.TaskId, uav: d.ID, sent: now}
	t.UavId, t.AssignedUnixNs, t.Attempt = d.ID, now.UnixNano(), t.Attempt+1
	t.DistanceM, t.BatteryNeededPct, t.BatteryPct = float32(math.Round(dist)), float32(math.Round(need*10)/10), d.BatteryPct
	t.Note = fmt.Sprintf("nearest free investigator: %.0f m away, needs %.0f%% of %.0f%% battery", dist, need, d.BatteryPct)
	p.set(t, sarpb.TaskState_TASK_STATE_ASSIGNED, "")
	p.progress[t.TaskId], p.seen[t.TaskId] = now, false
	d.Task = t.TaskId
	p.log.Info("investigation assigned", "task", t.TaskId, "casualty", t.CasualtyId, "tier", t.Tier, "uav", d.ID, "distance_m", math.Round(dist),
		"battery_needed_pct", math.Round(need), "attempt", t.Attempt)
}

// --- Outputs ----------------------------------------------------------------------

// Tasks returns a copy of every task, oldest first.
func (p *Planner) Tasks() []*sarpb.Task {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tasksLocked()
}

func (p *Planner) tasksLocked() []*sarpb.Task {
	out := make([]*sarpb.Task, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, proto.Clone(p.tasks[id]).(*sarpb.Task))
	}
	return out
}

// Publish sends the task list (and saves it) if it changed, or if force.
func (p *Planner) Publish(force bool) {
	p.mu.Lock()
	if !force && !p.dirty {
		p.mu.Unlock()
		return
	}
	if p.dirty {
		p.saveLocked()
	}
	p.dirty = false
	l := &sarpb.TaskList{Header: msg.NewHeader(p.cfg.Incident, p.cfg.Source, p.hseq.Next()), Tasks: p.tasksLocked()}
	p.mu.Unlock()
	b, err := msg.Marshal(l)
	if err == nil {
		err = p.pub.Put(b)
	}
	if err != nil {
		p.log.Warn("publishing the task list", "err", err)
	}
}

// Reset forgets every task (a new acceptance run); drones flying one carry on
// but are no longer followed. New task ids get a new run token.
func (p *Planner) Reset() {
	p.mu.Lock()
	p.run = runToken(time.Now())
	if len(p.order) > 0 && strings.HasPrefix(p.order[0], "T-"+p.run+"-") {
		p.run = runToken(time.Now().Add(2 * time.Second))
	}
	p.next, p.tasks, p.order, p.progress, p.seen, p.cmds = 1, map[string]*sarpb.Task{}, nil, map[string]time.Time{}, map[string]bool{}, map[uint64]pending{}
	p.cas, p.casAt, p.casFirst = map[string]*sarpb.Casualty{}, time.Time{}, map[string]time.Time{}
	for _, d := range p.drones {
		d.Task, d.blocked, d.Note = "", time.Time{}, ""
	}
	p.resets++
	p.dirty = true
	p.mu.Unlock()
	p.log.Info("reset: tasks forgotten")
	p.Publish(true)
}

func protoJSONMarshal(t *sarpb.Task) ([]byte, error) {
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(t)
}

func protoJSONUnmarshal(b []byte, t *sarpb.Task) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(b, t)
}

// Close unsubscribes.
func (p *Planner) Close() {
	for _, s := range p.subs {
		_ = s.Close()
	}
}

// Run plans every Tick and publishes the task list on change and every
// publish_interval, until ctx ends.
func (p *Planner) Run(ctx context.Context) {
	tick := time.NewTicker(p.cfg.Tick)
	defer tick.Stop()
	every := p.cfg.P.PublishInterval.Duration
	if every <= 0 {
		every = 5 * time.Second
	}
	pub := time.NewTicker(every)
	defer pub.Stop()
	p.Publish(true)
	for {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.saveLocked()
			p.mu.Unlock()
			return
		case <-tick.C:
			p.Step()
			p.Publish(false)
		case <-pub.C:
			p.Publish(true)
		}
	}
}

// Status is the status endpoint's view.
type Status struct {
	Run        string         `json:"run"`
	Drones     []Drone        `json:"drones"`
	ByState    map[string]int `json:"tasks_by_state"`
	Casualties int            `json:"casualties_known"`
	ListAgeS   float64        `json:"casualty_list_age_s"`
	Lists      uint64         `json:"casualty_lists_received"`
	Resets     int            `json:"resets"`
	Tasks      []TaskRow      `json:"tasks"`
}

// TaskRow is one task in the status listing.
type TaskRow struct {
	ID       string  `json:"id"`
	Casualty string  `json:"casualty"`
	Tier     uint32  `json:"tier"`
	State    string  `json:"state"`
	UAV      string  `json:"uav,omitempty"`
	Attempt  uint32  `json:"attempt"`
	Distance float32 `json:"distance_m"`
	Frames   uint32  `json:"frames"`
	Found    bool    `json:"found"`
	Moved    bool    `json:"moved"`
	Note     string  `json:"note,omitempty"`
}

// State names a task state in words.
func State(s sarpb.TaskState) string { return lowerEnum(s.String(), "TASK_STATE_") }

// Status returns a snapshot.
func (p *Planner) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	s := Status{Run: p.run, ByState: map[string]int{}, Casualties: len(p.cas), Lists: p.lists, Resets: p.resets, ListAgeS: -1}
	if !p.casAt.IsZero() {
		s.ListAgeS = math.Round(now.Sub(p.casAt).Seconds()*10) / 10
	}
	ids := make([]string, 0, len(p.drones))
	for id := range p.drones {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := *p.drones[id]
		d.Online = now.Sub(d.LastSeen) <= p.cfg.OnlineAfter
		d.tel = nil
		s.Drones = append(s.Drones, d)
	}
	for _, id := range p.order {
		t := p.tasks[id]
		s.ByState[State(t.State)]++
		r := TaskRow{ID: t.TaskId, Casualty: t.CasualtyId, Tier: t.Tier, State: State(t.State), UAV: t.UavId, Attempt: t.Attempt,
			Distance: t.DistanceM, Note: t.Note}
		if res := t.Result; res != nil {
			r.Frames, r.Found, r.Moved = res.Frames, res.Found, res.Moved
		}
		s.Tasks = append(s.Tasks, r)
	}
	return s
}

// Text renders the status as tables.
func (s Status) Text() string {
	var b strings.Builder
	age := "none received yet"
	if s.ListAgeS >= 0 {
		age = fmt.Sprintf("%d known, list %.0f s old", s.Casualties, s.ListAgeS)
	}
	fmt.Fprintf(&b, "run %s; casualties from casualty-analyst: %s\n", s.Run, age)
	fmt.Fprintf(&b, "%-8s %-13s %-8s %-12s %8s %8s  %-14s %s\n", "UAV", "ROLE", "ONLINE", "STATE", "BATTERY", "FLIGHT", "TASK", "NOTE")
	for _, d := range s.Drones {
		on := "yes"
		if !d.Online {
			on = "NO"
		}
		task := d.Task
		if task == "" && d.DroneTask != "" {
			task = d.DroneTask + " (" + d.DroneState + ")"
		}
		role := d.Role
		if d.Investigate {
			role += "*"
		}
		fmt.Fprintf(&b, "%-8s %-13s %-8s %-12s %7.0f%% %7.0fm  %-14s %s\n", d.ID, role, on, d.State, d.BatteryPct, float64(d.FlightLeftS)/60, nonEmpty(task, "-"), d.Note)
	}
	b.WriteString("(* = takes investigations)\n\n")
	var st []string
	for _, k := range []string{"queued", "assigned", "en_route", "on_station", "done", "failed", "cancelled"} {
		if n := s.ByState[k]; n > 0 {
			st = append(st, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(st) == 0 {
		st = []string{"none yet"}
	}
	fmt.Fprintf(&b, "tasks: %s\n", strings.Join(st, ", "))
	if len(s.Tasks) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "%-11s %-6s %4s %-10s %-7s %3s %6s %6s %-6s %s\n", "TASK", "CAS", "TIER", "STATE", "UAV", "TRY", "DIST m", "FRAMES", "FOUND", "NOTE")
	for _, t := range s.Tasks {
		found := "-"
		if t.State == "done" {
			found = "no"
			if t.Found {
				found = "yes"
				if t.Moved {
					found = "moved"
				}
			}
		}
		fmt.Fprintf(&b, "%-11s %-6s %4d %-10s %-7s %3d %6.0f %6d %-6s %s\n", t.ID, t.Casualty, t.Tier, t.State, nonEmpty(t.UAV, "-"), t.Attempt, t.Distance, t.Frames, found, t.Note)
	}
	return b.String()
}
