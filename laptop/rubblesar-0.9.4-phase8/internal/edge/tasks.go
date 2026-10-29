package edge

import (
	"math"
	"sort"
	"sync"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/flightsim"
	"rubblesar/internal/geo"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// Investigation tasks (phase 4): mission-planner sends Investigate; the drone
// flies there, holds over the target and watches it; telemetry carries the
// task's progress and result; when the hover ends every part watched near the
// target is reported again (reason INVESTIGATED) with its movement verdict.

// taskTrack is what the edge knows about a drone's current or last task
// beyond the flight simulator: times, frames watched, parts seen near the target.
type taskTrack struct {
	mu                sync.Mutex
	id                string
	started, finished time.Time
	frames            int
	tracks            []string
	moved             bool
	maxOffset         float64
	lastState         flightsim.TaskState
}

func (t *taskTrack) reset(id string, now time.Time) {
	t.id, t.started, t.finished, t.frames, t.tracks, t.moved, t.maxOffset, t.lastState = id, now, time.Time{}, 0, nil, false, 0, flightsim.TaskEnRoute
}

func (r *Runtime) investigate(d *Drone, inv *sarpb.Investigate, from string) error {
	switch {
	case inv.GetTaskId() == "" || len(inv.GetTaskId()) > 64:
		return diag.New("E-EDGE-007", "task id %q must be 1-64 characters", inv.GetTaskId())
	case inv.GetTarget() == nil:
		return diag.New("E-EDGE-007", "task %s has no target", inv.GetTaskId())
	case inv.GetAltAglM() < 5 || inv.GetAltAglM() > 120:
		return diag.New("E-EDGE-007", "task %s height %.0f m must be in 5..120", inv.GetTaskId(), inv.GetAltAglM())
	case inv.GetHoverS() < 1 || inv.GetHoverS() > 600:
		return diag.New("E-EDGE-007", "task %s hover %d s must be in 1..600", inv.GetTaskId(), inv.GetHoverS())
	}
	e, n := r.local.ToLocal(geo.Point{Lat: inv.GetTarget().GetLatDeg(), Lon: inv.GetTarget().GetLonDeg()})
	if math.Hypot(e, n) > 1.5*r.cfg.Site.RadiusM {
		return diag.New("E-EDGE-007", "task %s target is %.0f m from the incident centre (more than 1.5 x the %0.f m radius)", inv.GetTaskId(), math.Hypot(e, n), r.cfg.Site.RadiusM)
	}
	radius := float64(inv.GetRadiusM())
	if radius <= 0 {
		radius = 6
	}
	t := flightsim.Task{ID: inv.GetTaskId(), CasualtyID: inv.GetCasualtyId(), E: e, N: n, AltAGL: float64(inv.GetAltAglM()),
		HoverS: float64(inv.GetHoverS()), RadiusM: radius}
	d.mu.Lock()
	prev := d.sim.TaskStatus()
	d.sim.StartTask(t)
	st := d.sim.State()
	d.mu.Unlock()
	d.task.mu.Lock()
	if d.task.id != t.ID {
		d.task.reset(t.ID, r.now())
	}
	d.task.mu.Unlock()
	attrs := []any{"uav", d.ID, "task", t.ID, "casualty", t.CasualtyID, "target_e", math.Round(e), "target_n", math.Round(n),
		"distance_m", math.Round(math.Hypot(e-st.East, n-st.North)), "alt_m", t.AltAGL, "hover_s", t.HoverS, "from", from}
	if prev.State == flightsim.TaskEnRoute || prev.State == flightsim.TaskOnStation {
		if prev.Task.ID != t.ID {
			attrs = append(attrs, "replaces", prev.Task.ID)
		}
	}
	r.log.Info("investigation task", attrs...)
	return nil
}

func (r *Runtime) cancelTask(d *Drone, c *sarpb.CancelTask, from string) error {
	d.mu.Lock()
	ok := d.sim.CancelTask(c.GetTaskId())
	d.mu.Unlock()
	if !ok {
		return diag.New("E-EDGE-007", "no task %q in progress", c.GetTaskId())
	}
	r.log.Info("investigation task cancelled", "uav", d.ID, "task", c.GetTaskId(), "from", from)
	return nil
}

// taskStatus is the telemetry view of the current or last task (nil if none yet).
func (r *Runtime) taskStatus(d *Drone, ti flightsim.TaskInfo, re, rn float64) *sarpb.TaskStatus {
	if ti.State == flightsim.TaskNone {
		return nil
	}
	tt := &d.task
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if tt.id != ti.Task.ID {
		tt.reset(ti.Task.ID, r.now())
	}
	if ti.State == flightsim.TaskOnStation {
		tt.maxOffset = math.Max(tt.maxOffset, math.Hypot(re-ti.Task.E, rn-ti.Task.N))
	}
	if (ti.State == flightsim.TaskDone || ti.State == flightsim.TaskAborted) && tt.finished.IsZero() {
		tt.finished = r.now()
	}
	tt.lastState = ti.State
	ll := r.local.ToPoint(ti.Task.E, ti.Task.N)
	ts := &sarpb.TaskStatus{TaskId: ti.Task.ID, CasualtyId: ti.Task.CasualtyID, State: taskStateOf(ti.State),
		Target:  &sarpb.GeoPoint{LatDeg: ll.Lat, LonDeg: ll.Lon, AltMslM: r.cfg.Site.GroundAltMSLm},
		AltAglM: float32(ti.Task.AltAGL), HoverS: uint32(ti.Task.HoverS), HoverElapsedS: float32(math.Round(ti.HoverElapsed*10) / 10),
		Frames: uint32(tt.frames), TrackIds: append([]string(nil), tt.tracks...), Moved: tt.moved, Reason: ti.Reason,
		StartedUnixNs: tt.started.UnixNano(), OffsetM: float32(math.Round(tt.maxOffset*100) / 100)}
	if !tt.finished.IsZero() {
		ts.FinishedUnixNs = tt.finished.UnixNano()
	}
	return ts
}

func taskStateOf(s flightsim.TaskState) sarpb.TaskState {
	switch s {
	case flightsim.TaskEnRoute:
		return sarpb.TaskState_TASK_STATE_EN_ROUTE
	case flightsim.TaskOnStation:
		return sarpb.TaskState_TASK_STATE_ON_STATION
	case flightsim.TaskDone:
		return sarpb.TaskState_TASK_STATE_DONE
	case flightsim.TaskAborted:
		return sarpb.TaskState_TASK_STATE_ABORTED
	}
	return sarpb.TaskState_TASK_STATE_UNSPECIFIED
}

// watchTask runs before every frame: it notices the end of a hover (task done,
// replaced, cancelled or aborted) and sends the INVESTIGATED reports, and it
// says whether this frame is a hover frame.
func (r *Runtime) watchTask(d *Drone, ti flightsim.TaskInfo, now time.Time) (hover bool, err error) {
	pc := d.perc
	if pc.hoverTask != "" && (ti.Task.ID != pc.hoverTask || !ti.Holding) {
		pkts := pc.pl.EndHover(pc.hoverTask, pc.hoverE, pc.hoverN, pc.hoverR, now)
		ids, moved := trackSummary(pkts)
		d.task.mu.Lock()
		if d.task.id == pc.hoverTask {
			d.task.tracks, d.task.moved = ids, moved
		}
		d.task.mu.Unlock()
		r.log.Info("investigation hover ended", "uav", d.ID, "task", pc.hoverTask, "parts", len(pkts), "moved", moved, "state", ti.State.String())
		pc.hoverTask = ""
		if err := r.queue(d, pkts); err != nil {
			return false, err
		}
	}
	if ti.Holding {
		if pc.hoverTask != ti.Task.ID {
			pc.hoverTask, pc.hoverE, pc.hoverN, pc.hoverR = ti.Task.ID, ti.Task.E, ti.Task.N, ti.Task.RadiusM
		}
		return true, nil
	}
	return false, nil
}

// afterHoverFrame records what the hover has seen so far (for telemetry).
func (r *Runtime) afterHoverFrame(d *Drone) {
	pc := d.perc
	near := pc.pl.Tracker.Near(pc.hoverE, pc.hoverN, pc.hoverR, pc.hoverTask)
	ids := make([]string, 0, len(near))
	moved := false
	mp := pc.pl.Tracker.Params().Motion
	for _, t := range near {
		ids = append(ids, t.ID)
		moved = moved || t.Motion.Moved(mp)
	}
	sort.Strings(ids)
	d.task.mu.Lock()
	if d.task.id == pc.hoverTask {
		d.task.frames++
		d.task.tracks, d.task.moved = ids, moved
	}
	d.task.mu.Unlock()
}

func trackSummary(pkts []*sarpb.Detection) ([]string, bool) {
	var ids []string
	moved := false
	for _, p := range pkts {
		ids = append(ids, p.GetTrackId())
		moved = moved || p.GetMovement().GetMoved()
	}
	sort.Strings(ids)
	return ids, moved
}

// queue puts packets in the drone's outbox.
func (r *Runtime) queue(d *Drone, pkts []*sarpb.Detection) error {
	pc := d.perc
	for _, p := range pkts {
		p.Header = msg.NewHeader(r.cfg.Site.IncidentID, "uav/"+d.ID, pc.seq.Next())
		if err := pc.ob.Add(p); err != nil {
			return err
		}
		pc.reports.Add(1)
		if p.GetPart() == sarpb.PartClass_PART_CLASS_WARM_SPOT {
			pc.warmReports.Add(1)
		}
	}
	return nil
}
