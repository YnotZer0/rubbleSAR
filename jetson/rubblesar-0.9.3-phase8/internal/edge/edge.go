// Package edge is the runtime of edge-eye, the Jetson program that plays the
// payload computer of every simulated drone.
//
// Per drone: flight simulation (including survey missions), telemetry at the
// configured rate, a control channel (ping, link budget, status, mission
// restart, simulated outage) and a link budget that can be changed at run time
// from the Jetson (admin API) or the ZBook. From phase 3, with [perception]
// enabled: the virtual camera, the onboard pipeline (segmentation, thermal cue,
// geolocation, tracking, packaging) and a store-and-forward detection queue
// that shares the drone's budget with its telemetry.
package edge

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/flightsim"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/linkbudget"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/version"
)

// Drone is one simulated drone hosted by this runtime.
type Drone struct {
	ID   string
	cfg  config.UAV
	role sarpb.UavRole

	mu      sync.Mutex
	sim     *flightsim.Sim
	lastFix flightsim.Fix
	// surveySites: a survey drone's sites in the order flown (telemetry).
	surveySites []string

	bucket   *linkbudget.Bucket
	telPub   bus.Publisher
	replyPub bus.Publisher
	sub      bus.Subscription

	telSeq, replySeq msg.Seq
	published        atomic.Uint64
	publishErrors    atomic.Uint64
	controlHandled   atomic.Uint64
	controlRejected  atomic.Uint64

	period   time.Duration
	next     time.Time
	lastWarn time.Time

	// Telemetry waiting for budget (latest wins; replaced when the next is due).
	telPending  []byte
	outageUntil time.Time
	perc        *perc // nil without [perception]
	task        taskTrack
}

// Runtime runs every drone.
type Runtime struct {
	cfg     *config.Edge
	b       bus.Bus
	log     *slog.Logger
	local   geo.Local
	drones  []*Drone
	byID    map[string]*Drone
	started time.Time
	now     func() time.Time
	perc    *perception // nil without [perception]
	// runToken (this start's time in base 36) is part of every track id, so
	// ids never repeat across restarts; casualty-analyst drops repeated ids.
	runToken string
}

// New declares publishers and subscribers for every configured drone.
func New(cfg *config.Edge, b bus.Bus, log *slog.Logger) (*Runtime, error) {
	r := &Runtime{
		cfg:     cfg,
		b:       b,
		log:     log,
		local:   geo.NewLocal(geo.Point{Lat: cfg.Site.CenterLat, Lon: cfg.Site.CenterLon}),
		byID:    map[string]*Drone{},
		started: time.Now(),
		now:     time.Now,
	}
	r.runToken = strconv.FormatInt(r.started.UnixMilli(), 36)
	inc := cfg.Site.IncidentID
	for i, u := range cfg.UAVs {
		d := &Drone{
			ID:     u.ID,
			cfg:    u,
			role:   roleOf(u.Role),
			sim:    flightsim.New(u, cfg.Seed+int64(i)*7919),
			bucket: linkbudget.New(*u.BudgetKbps*1000, nil),
			period: time.Duration(float64(time.Second) / u.TelemetryHz),
		}
		r.drones = append(r.drones, d)
		r.byID[u.ID] = d
		var err error
		if d.telPub, err = b.DeclarePublisher(keys.Telemetry(inc, u.ID), bus.QoSTelemetry); err != nil {
			r.Close()
			return nil, err
		}
		if d.replyPub, err = b.DeclarePublisher(keys.ControlReply(inc, u.ID), bus.QoSControl); err != nil {
			r.Close()
			return nil, err
		}
		dd := d
		if d.sub, err = b.Subscribe(keys.Control(inc, u.ID), func(s bus.Sample) { r.handleControl(dd, s) }); err != nil {
			r.Close()
			return nil, err
		}
		// Spread the drones' first telemetry over one period.
		d.next = r.started.Add(time.Duration(i) * d.period / time.Duration(len(cfg.UAVs)))
		d.lastFix = d.sim.GPS(r.local, cfg.Site.GroundAltMSLm)
		log.Info("drone ready", "uav", u.ID, "role", u.Role, "behaviour", u.Behaviour,
			"telemetry_hz", u.TelemetryHz, "budget_kbps", *u.BudgetKbps,
			"publishes", keys.Telemetry(inc, u.ID), "listens", keys.Control(inc, u.ID))
	}
	if err := r.setupPhase3(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// setupPhase3 loads the scenario for survey missions and the perception pipeline.
func (r *Runtime) setupPhase3() error {
	needWorld := r.cfg.Perception.Enabled
	for _, d := range r.drones {
		needWorld = needWorld || d.cfg.Behaviour == "survey"
	}
	if !needWorld {
		return nil
	}
	w, err := OpenWorld(r.cfg)
	if err != nil {
		return err
	}
	for _, d := range r.drones {
		if d.cfg.Behaviour != "survey" {
			continue
		}
		m, err := SurveyMission(w, d.cfg)
		if err != nil {
			return err
		}
		d.sim.SetMission(m)
		d.mu.Lock()
		d.surveySites = m.Areas
		d.mu.Unlock()
		r.log.Info("survey mission", "uav", d.ID, "sites", strings.Join(m.Areas, " "), "waypoints", len(m.Waypoints), "altitude_m", d.cfg.AltAGLm,
			"length_m", fmt.Sprintf("%.0f", m.LengthM(d.cfg.HomeOffsetEastM, d.cfg.HomeOffsetNorthM)),
			"minutes", fmt.Sprintf("%.1f", m.DurationS(d.cfg.HomeOffsetEastM, d.cfg.HomeOffsetNorthM)/60))
	}
	if !r.cfg.Perception.Enabled {
		return nil
	}
	r.perc = newPerception(r.cfg, w)
	if r.perc.err != nil {
		r.log.Error("perception is not running: the model could not be loaded (drones keep flying and reporting telemetry)",
			"model_dir", r.cfg.Perception.ModelDir, "err", r.perc.err, "hint", diag.HintOf(r.perc.err))
	} else {
		r.log.Info("perception ready", "backend", r.perc.backend, "model", r.perc.modelName, "conf", r.cfg.Perception.Conf,
			"sim_start_s", r.cfg.Perception.SimStartS, "queue", r.cfg.Outbox.Dir, "track_ids", "<uav>-"+r.runToken+"-NNNN")
	}
	for i, d := range r.drones {
		if err := r.setupPerception(d, i); err != nil {
			return err
		}
	}
	return nil
}

func roleOf(s string) sarpb.UavRole {
	if s == "scout" {
		return sarpb.UavRole_UAV_ROLE_SCOUT
	}
	return sarpb.UavRole_UAV_ROLE_INVESTIGATOR
}

func stateOf(p flightsim.Phase) sarpb.FlightState {
	switch p {
	case flightsim.Landed:
		return sarpb.FlightState_FLIGHT_STATE_LANDED
	case flightsim.Takeoff:
		return sarpb.FlightState_FLIGHT_STATE_TAKEOFF
	case flightsim.Transit:
		return sarpb.FlightState_FLIGHT_STATE_TRANSIT
	case flightsim.Survey:
		return sarpb.FlightState_FLIGHT_STATE_SURVEY
	case flightsim.Hover:
		return sarpb.FlightState_FLIGHT_STATE_HOVER
	case flightsim.Return:
		return sarpb.FlightState_FLIGHT_STATE_RETURN
	case flightsim.Landing:
		return sarpb.FlightState_FLIGHT_STATE_LANDING
	case flightsim.Investigate:
		return sarpb.FlightState_FLIGHT_STATE_INVESTIGATE
	case flightsim.Loiter:
		return sarpb.FlightState_FLIGHT_STATE_HOVER
	}
	return sarpb.FlightState_FLIGHT_STATE_UNSPECIFIED
}

// Run steps the simulation every 100 ms, publishes telemetry on schedule and
// sends queued detections within each drone's budget (telemetry first). The
// perception loops run on their own goroutines. It returns when ctx is cancelled.
func (r *Runtime) Run(ctx context.Context) error {
	for _, d := range r.drones {
		if d.perc != nil {
			go r.perceiveLoop(ctx, d)
		}
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	last := r.now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
		now := r.now()
		dt := now.Sub(last)
		last = now
		if dt > 5*time.Second { // e.g. the process was suspended; do not teleport
			dt = 5 * time.Second
		}
		for _, d := range r.drones {
			d.mu.Lock()
			d.sim.Step(dt)
			d.mu.Unlock()
			outage := now.Before(d.outageUntil)
			if !now.Before(d.next) {
				if !outage {
					r.queueTelemetry(d)
				}
				d.next = d.next.Add(d.period)
				if d.next.Before(now) { // fell behind: resynchronise
					d.next = now.Add(d.period)
				}
			}
			if outage {
				d.telPending = nil
				continue
			}
			if d.telPending != nil {
				r.sendTelemetry(d)
			}
			if d.perc != nil && d.telPending == nil {
				d.perc.ob.Pump(d.perc.detPub, d.bucket)
			}
		}
	}
}

func (r *Runtime) linkStats(d *Drone) *sarpb.LinkStats {
	s := d.bucket.Stats()
	ls := &sarpb.LinkStats{BudgetBps: s.BudgetBps, UsedBps: s.UsedBps, DroppedMsgs: s.Dropped, SentBytes: s.SentBytes,
		Outage: r.now().Before(d.outageUntil)}
	outboxLink(ls, d.perc)
	return ls
}

// BuildTelemetry snapshots a drone into a telemetry message (exported for tests).
func (r *Runtime) BuildTelemetry(d *Drone) *sarpb.Telemetry {
	d.mu.Lock()
	st := d.sim.State()
	fix := d.sim.GPS(r.local, r.cfg.Site.GroundAltMSLm)
	d.lastFix = fix
	ti := d.sim.TaskStatus()
	re, rn, _ := d.sim.ReportedENU()
	sites := d.surveySites
	d.mu.Unlock()
	home := r.local.ToPoint(d.cfg.HomeOffsetEastM, d.cfg.HomeOffsetNorthM)
	return &sarpb.Telemetry{
		Task:            r.taskStatus(d, ti, re, rn),
		FlightTimeLeftS: uint32(math.Max(0, st.BatteryPct) / 100 * d.cfg.EnduranceMin * 60),
		CruiseMps:       float32(d.cfg.SpeedMps),
		Home:            &sarpb.GeoPoint{LatDeg: home.Lat, LonDeg: home.Lon, AltMslM: r.cfg.Site.GroundAltMSLm},
		Software:        version.Short("edge-eye"),
		Header:          msg.NewHeader(r.cfg.Site.IncidentID, "uav/"+d.ID, d.telSeq.Next()),
		UavId:           d.ID,
		Role:            d.role,
		Position:        &sarpb.GeoPoint{LatDeg: fix.Point.Lat, LonDeg: fix.Point.Lon, AltMslM: fix.AltMSL},
		AltAglM:         float32(fix.AltAGL),
		RollDeg:         float32(st.RollDeg),
		PitchDeg:        float32(st.PitchDeg),
		YawDeg:          float32(st.YawDeg),
		GroundSpeedMps:  float32(st.SpeedMps),
		CourseDeg:       float32(st.CourseDeg),
		ClimbMps:        float32(st.ClimbMps),
		BatteryPct:      float32(st.BatteryPct),
		State:           stateOf(st.Phase),
		GpsHaccM:        float32(fix.HAccM),
		Link:            r.linkStats(d),
		MissionId:       r.missionID(d),
		MissionStep:     r.missionStep(d),
		Perception:      r.phase3Stats(d),
		SurveySites:     sites,
	}
}

func (r *Runtime) missionID(d *Drone) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	id, _, _, _ := d.sim.MissionStatus()
	return id
}

func (r *Runtime) missionStep(d *Drone) uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, step, _, _ := d.sim.MissionStatus()
	return uint32(step)
}

// phase3Stats is nil for phase 0-2 configurations (no survey, no perception).
func (r *Runtime) phase3Stats(d *Drone) *sarpb.PerceptionStats {
	if r.perc == nil && d.cfg.Behaviour != "survey" {
		return nil
	}
	if r.perc == nil {
		ps := &sarpb.PerceptionStats{}
		d.mu.Lock()
		id, step, steps, done := d.sim.MissionStatus()
		d.mu.Unlock()
		ps.MissionId, ps.MissionStep, ps.MissionSteps, ps.MissionDone = id, uint32(step), uint32(steps), done
		return ps
	}
	return r.perceptionStats(d)
}

// queueTelemetry builds the next telemetry message. One still waiting for
// budget is replaced (and counted as dropped): telemetry is latest-wins.
func (r *Runtime) queueTelemetry(d *Drone) {
	t := r.BuildTelemetry(d)
	b, err := msg.Marshal(t)
	if err != nil {
		r.warn(d, "encode telemetry", err)
		return
	}
	if d.telPending != nil {
		d.bucket.CountDrop()
	}
	d.telPending = b
}

// sendTelemetry sends the waiting telemetry message if the budget allows.
// Detections wait while telemetry is pending, so telemetry always goes first.
func (r *Runtime) sendTelemetry(d *Drone) {
	b := d.telPending
	if !d.bucket.TryReserve(len(b), 0) {
		return
	}
	d.telPending = nil
	if err := d.telPub.Put(b); err != nil {
		d.publishErrors.Add(1)
		r.warn(d, "publish telemetry", err)
		return
	}
	d.published.Add(1)
}

// warn logs at most once per 10 s per drone, so a broken link does not flood the journal.
func (r *Runtime) warn(d *Drone, what string, err error) {
	now := r.now()
	d.mu.Lock()
	if now.Sub(d.lastWarn) < 10*time.Second {
		d.mu.Unlock()
		return
	}
	d.lastWarn = now
	d.mu.Unlock()
	r.log.Warn(what+" failed", "uav", d.ID, "err", err, "hint", diag.HintOf(err))
}

func (r *Runtime) handleControl(d *Drone, s bus.Sample) {
	var c sarpb.Control
	if err := msg.Unmarshal(s.Payload, &c); err != nil {
		d.controlRejected.Add(1)
		r.log.Warn("control message rejected", "uav", d.ID, "err", err)
		return
	}
	reply := &sarpb.ControlReply{
		Header:       msg.NewHeader(r.cfg.Site.IncidentID, "uav/"+d.ID, d.replySeq.Next()),
		UavId:        d.ID,
		InReplyToSeq: c.GetHeader().GetSeq(),
	}
	err := msg.CheckHeader(c.GetHeader(), r.cfg.Site.IncidentID)
	if err == nil && c.GetUavId() != d.ID {
		err = diag.New("E-MSG-004", "control for %q arrived on %q's key", c.GetUavId(), d.ID)
	}
	if err == nil {
		switch cmd := c.GetCmd().(type) {
		case *sarpb.Control_Ping:
			reply.Body = &sarpb.ControlReply_Pong{Pong: &sarpb.Pong{Nonce: cmd.Ping.GetNonce(), RecvUnixNs: s.Received.UnixNano()}}
		case *sarpb.Control_SetLinkBudget:
			d.bucket.SetBps(cmd.SetLinkBudget.GetBps())
			r.log.Info("link budget changed by control message", "uav", d.ID, "bps", cmd.SetLinkBudget.GetBps(), "from", c.GetHeader().GetSourceId())
			reply.Body = &sarpb.ControlReply_Link{Link: r.linkStats(d)}
		case *sarpb.Control_GetStatus:
			reply.Body = &sarpb.ControlReply_Link{Link: r.linkStats(d)}
		case *sarpb.Control_RestartMission:
			r.restartMission(d, cmd.RestartMission, c.GetHeader().GetSourceId())
			reply.Body = &sarpb.ControlReply_Link{Link: r.linkStats(d)}
		case *sarpb.Control_SetLinkOutage:
			defer r.startOutage(d, time.Duration(cmd.SetLinkOutage.GetSeconds())*time.Second, c.GetHeader().GetSourceId())
			reply.Body = &sarpb.ControlReply_Link{Link: r.linkStats(d)}
		case *sarpb.Control_Investigate:
			err = r.investigate(d, cmd.Investigate, c.GetHeader().GetSourceId())
		case *sarpb.Control_CancelTask:
			err = r.cancelTask(d, cmd.CancelTask, c.GetHeader().GetSourceId())
		default:
			err = diag.New("E-EDGE-003", "empty or unknown command")
		}
	}
	if err != nil {
		d.controlRejected.Add(1)
		reply.Ok = false
		reply.Error = err.Error()
		r.log.Warn("control message rejected", "uav", d.ID, "err", err)
	} else {
		d.controlHandled.Add(1)
		reply.Ok = true
	}
	b, merr := msg.Marshal(reply)
	if merr != nil {
		r.log.Error("encode control reply", "err", merr)
		return
	}
	// Control replies bypass the budget (they are tiny and essential) but are counted.
	d.bucket.Record(len(b))
	if perr := d.replyPub.Put(b); perr != nil {
		r.warn(d, "publish control reply", perr)
	}
}

// restartMission flies the drone's mission again from the start.
func (r *Runtime) restartMission(d *Drone, rm *sarpb.RestartMission, from string) {
	d.mu.Lock()
	d.sim.RestartMission()
	d.mu.Unlock()
	if r.perc != nil && rm.GetSimTimeS() >= 0 {
		r.perc.setSimTime(r.now(), rm.GetSimTimeS())
	}
	d.task.mu.Lock()
	d.task.reset("", r.now())
	d.task.mu.Unlock()
	if d.perc != nil {
		d.perc.hoverTask = ""
		if rm.GetClearTracks() && d.perc.pl != nil {
			d.perc.pl.Tracker.Reset()
		}
		if rm.GetClearQueue() {
			d.perc.ob.Clear()
		}
	}
	r.log.Info("mission restarted by control message", "uav", d.ID, "from", from, "sim_time_s", rm.GetSimTimeS(),
		"clear_tracks", rm.GetClearTracks(), "clear_queue", rm.GetClearQueue())
}

// startOutage silences the drone's radio for d (after the reply has gone out).
func (r *Runtime) startOutage(d *Drone, dur time.Duration, from string) {
	d.outageUntil = r.now().Add(dur)
	if d.perc != nil {
		d.perc.ob.SetOutage(dur)
	}
	r.log.Info("simulated link outage", "uav", d.ID, "seconds", dur.Seconds(), "from", from)
}

// SetBudget changes the link budget of one drone, or all drones with uav "all".
func (r *Runtime) SetBudget(uav string, bps uint32) ([]string, error) {
	var ids []string
	if uav == "all" || uav == "" {
		for _, d := range r.drones {
			d.bucket.SetBps(bps)
			ids = append(ids, d.ID)
		}
	} else {
		d, ok := r.byID[uav]
		if !ok {
			return nil, diag.New("E-EDGE-002", "no drone %q (have %s)", uav, r.idList())
		}
		d.bucket.SetBps(bps)
		ids = []string{uav}
	}
	r.log.Info("link budget changed locally", "uavs", ids, "bps", bps)
	return ids, nil
}

func (r *Runtime) idList() string {
	ids := make([]string, 0, len(r.drones))
	for _, d := range r.drones {
		ids = append(ids, d.ID)
	}
	sort.Strings(ids)
	return fmt.Sprint(ids)
}

// DroneStatus is one drone in the status report.
type DroneStatus struct {
	ID              string  `json:"id"`
	Role            string  `json:"role"`
	Phase           string  `json:"phase"`
	Lat             float64 `json:"lat"`
	Lon             float64 `json:"lon"`
	AltAGLm         float64 `json:"alt_agl_m"`
	BatteryPct      float64 `json:"battery_pct"`
	BudgetBps       uint32  `json:"budget_bps"`
	BudgetKbps      uint32  `json:"budget_kbps"`
	UsedBps         uint32  `json:"used_bps"`
	DroppedMsgs     uint64  `json:"dropped_msgs"`
	SentBytes       uint64  `json:"sent_bytes"`
	Published       uint64  `json:"telemetry_published"`
	PublishErrors   uint64  `json:"publish_errors"`
	ControlHandled  uint64  `json:"control_handled"`
	ControlRejected uint64  `json:"control_rejected"`
	SubDropped      uint64  `json:"subscriber_dropped"`
	// Phase 3 (perception and queue); zero without [perception].
	Mission      string  `json:"mission,omitempty"`
	MissionStep  int     `json:"mission_step"`
	MissionSteps int     `json:"mission_steps"`
	MissionDone  bool    `json:"mission_done"`
	Backend      string  `json:"backend,omitempty"`
	Frames       uint64  `json:"frames"`
	FrameHz      float32 `json:"frame_hz"`
	Tracks       uint32  `json:"tracks"`
	Reports      uint64  `json:"reports"`
	Queued       uint32  `json:"queued"`
	Unacked      uint32  `json:"unacked"`
	Acked        uint64  `json:"acked"`
	Level        uint32  `json:"degrade_level"`
	Outage       bool    `json:"outage"`
	Probing      bool    `json:"probing"`
	RenderMS     float32 `json:"render_ms"`
	PipelineMS   float32 `json:"pipeline_ms"`
	LastError    string  `json:"last_error,omitempty"`
	// Phase 4: the current or last investigation task.
	Task       string  `json:"task,omitempty"`
	TaskState  string  `json:"task_state,omitempty"`
	TaskHoverS float32 `json:"task_hover_s"`
	TaskParts  int     `json:"task_parts"`
	TaskMoved  bool    `json:"task_moved"`
}

// Status is the runtime's report for the admin API.
type Status struct {
	Incident string        `json:"incident"`
	Bus      bus.Info      `json:"bus"`
	Uptime   string        `json:"uptime"`
	Drones   []DroneStatus `json:"drones"`
}

// Status returns a snapshot.
func (r *Runtime) Status() Status {
	s := Status{Incident: r.cfg.Site.IncidentID, Bus: r.b.Info(), Uptime: time.Since(r.started).Truncate(time.Second).String()}
	for _, d := range r.drones {
		d.mu.Lock()
		st := d.sim.State()
		fix := d.lastFix
		d.mu.Unlock()
		ls := d.bucket.Stats()
		s.Drones = append(s.Drones, DroneStatus{
			ID: d.ID, Role: d.cfg.Role, Phase: st.Phase.String(),
			Lat: fix.Point.Lat, Lon: fix.Point.Lon, AltAGLm: fix.AltAGL, BatteryPct: st.BatteryPct,
			BudgetBps: ls.BudgetBps, BudgetKbps: linkbudget.Kbps(ls.BudgetBps), UsedBps: ls.UsedBps,
			DroppedMsgs: ls.Dropped, SentBytes: ls.SentBytes,
			Published: d.published.Load(), PublishErrors: d.publishErrors.Load(),
			ControlHandled: d.controlHandled.Load(), ControlRejected: d.controlRejected.Load(),
			SubDropped: subDropped(d.sub),
		})
		ds := &s.Drones[len(s.Drones)-1]
		if ps := r.phase3Stats(d); ps != nil {
			ds.Mission, ds.MissionStep, ds.MissionSteps, ds.MissionDone = ps.MissionId, int(ps.MissionStep), int(ps.MissionSteps), ps.MissionDone
			ds.Backend, ds.Frames, ds.FrameHz, ds.Tracks, ds.Reports = ps.Backend, ps.Frames, ps.FrameHzActual, ps.Tracks, ps.Reports
			ds.RenderMS, ds.PipelineMS = ps.RenderMs, ps.SegmentMs+ps.ThermalMs+ps.TrackMs+ps.PackageMs
		}
		lk := r.linkStats(d)
		ds.Queued, ds.Unacked, ds.Acked, ds.Level, ds.Outage, ds.Probing = lk.QueuedMsgs, lk.UnackedMsgs, lk.AckedMsgs, lk.DegradeLevel, lk.Outage, lk.Probing
		d.mu.Lock()
		ti := d.sim.TaskStatus()
		d.mu.Unlock()
		if ti.State != flightsim.TaskNone {
			ds.Task, ds.TaskState, ds.TaskHoverS = ti.Task.ID, ti.State.String(), float32(math.Round(ti.HoverElapsed))
			d.task.mu.Lock()
			ds.TaskParts, ds.TaskMoved = len(d.task.tracks), d.task.moved
			d.task.mu.Unlock()
		}
		if d.perc != nil {
			d.perc.mu.Lock()
			ds.LastError = d.perc.lastErr
			d.perc.mu.Unlock()
		}
	}
	return s
}

// Drones returns the hosted drones (for tests).
func (r *Runtime) Drones() []*Drone { return r.drones }

// Close undeclares every publisher and subscriber. The bus is not closed.
func (r *Runtime) Close() {
	for _, d := range r.drones {
		if d.sub != nil {
			_ = d.sub.Close()
		}
		if d.telPub != nil {
			_ = d.telPub.Close()
		}
		if d.replyPub != nil {
			_ = d.replyPub.Close()
		}
		if p := d.perc; p != nil {
			if p.ackSub != nil {
				_ = p.ackSub.Close()
			}
			if p.detPub != nil {
				_ = p.detPub.Close()
			}
			if p.truthPub != nil {
				_ = p.truthPub.Close()
			}
		}
	}
}

func subDropped(s bus.Subscription) uint64 {
	if s == nil {
		return 0
	}
	return s.Dropped()
}
