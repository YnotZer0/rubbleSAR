package edge

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/evalseg"
	"rubblesar/internal/geo"
	"rubblesar/internal/infer"
	"rubblesar/internal/keys"
	"rubblesar/internal/mission"
	"rubblesar/internal/msg"
	"rubblesar/internal/outbox"
	"rubblesar/internal/perceive"
	"rubblesar/internal/raster"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/vcam"
)

// minCaptureAltM: below this the camera is not used (take-off, landing).
const minCaptureAltM = 8.0

// lockedDetector shares one model (one TensorRT engine) between the drones.
type lockedDetector struct {
	mu sync.Mutex
	d  *infer.Detector
}

func (l *lockedDetector) Name() string { return l.d.Name() }
func (l *lockedDetector) DetectFrame(m *raster.RGB) ([]infer.Detection, infer.Timing, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.d.DetectFrame(m)
}

// OracleName is the back-end name of the truth oracle; acceptance runs refuse it.
const OracleName = "truth-oracle (pipeline test only, not a model)"

// Oracle answers with the simulated ground truth of the frame being
// processed: one detection per visible body part of 12 pixels or more. It
// exists to test the pipeline and the link without a model.
type Oracle struct{ fr *vcam.Frame }

// SetFrame gives the oracle the frame the next DetectFrame call is about.
func (o *Oracle) SetFrame(fr *vcam.Frame) { o.fr = fr }

func (o *Oracle) Name() string { return OracleName }
func (o *Oracle) DetectFrame(*raster.RGB) ([]infer.Detection, infer.Timing, error) {
	var ds []infer.Detection
	if o.fr == nil {
		return nil, infer.Timing{}, nil
	}
	for _, t := range evalseg.MergedTruth(o.fr.PartsRGB, o.fr.InstRGB, 12) {
		ds = append(ds, infer.Detection{Class: t.Class, Name: infer.ClassNames[t.Class], Conf: 0.8, Mask: t.Mask, Area: t.Area, CX: t.CX, CY: t.CY,
			Box: [4]float32{float32(t.Mask.X0), float32(t.Mask.Y0), float32(t.Mask.X0 + t.Mask.W), float32(t.Mask.Y0 + t.Mask.H)}})
	}
	return ds, infer.Timing{}, nil
}

// frameStat is one processed frame, for the rolling statistics.
type frameStat struct {
	at                                time.Time
	render, seg, therm, track, packet float64 // ms
}

// perc is one drone's perception and outbox.
type perc struct {
	pl       *perceive.Pipeline
	ob       *outbox.Outbox
	oracle   *Oracle
	detPub   bus.Publisher
	truthPub bus.Publisher
	ackSub   bus.Subscription
	seq      msg.Seq
	truthSeq msg.Seq
	frameHz  float64
	frameNo  int64
	camPitch float64

	frames, reports, warmReports, frameErrors atomic.Uint64

	// The hover in progress (phase 4): task and target (local metres, radius).
	hoverTask              string
	hoverE, hoverN, hoverR float64

	mu      sync.Mutex
	window  []frameStat
	lastErr string
}

// perception holds what all drones share.
type perception struct {
	cfg       config.Perception
	world     *vcam.World
	det       perceive.Detector // shared (nil with the oracle)
	backend   string
	modelName string
	err       error // why perception is not running, if it is not

	clockMu  sync.Mutex
	simBase  time.Time // wall time at which the scenario clock read simStart
	simStart float64
}

// SimTime returns the scenario clock.
func (p *perception) SimTime(now time.Time) float64 {
	p.clockMu.Lock()
	defer p.clockMu.Unlock()
	return p.simStart + now.Sub(p.simBase).Seconds()
}

func (p *perception) setSimTime(now time.Time, s float64) {
	p.clockMu.Lock()
	defer p.clockMu.Unlock()
	p.simBase, p.simStart = now, s
}

// OpenWorld loads the scenario and checks it matches [site].
func OpenWorld(cfg *config.Edge) (*vcam.World, error) {
	w, err := vcam.Open(cfg.Site.Scenario(), false)
	if err != nil {
		return nil, err
	}
	if w.Scn.ID != cfg.Site.IncidentID {
		return nil, diag.New("E-EDGE-005", "scenario %s is for incident %q, [site] says %q", cfg.Site.Scenario(), w.Scn.ID, cfg.Site.IncidentID)
	}
	l := w.Scn.Local()
	e, n := l.ToLocal(geo.Point{Lat: cfg.Site.CenterLat, Lon: cfg.Site.CenterLon})
	if math.Hypot(e, n) > 1 {
		return nil, diag.New("E-EDGE-005", "scenario centre is %.1f m from [site] centre", math.Hypot(e, n))
	}
	return w, nil
}

// SurveyMission builds a "survey" drone's mission over its sites.
func SurveyMission(w *vcam.World, u config.UAV) (mission.Mission, error) {
	byID := map[string]int{}
	for i, s := range w.Scn.Sites {
		byID[s.ID] = i
	}
	ids := u.SurveySites
	if len(ids) == 0 {
		// Every site, in the shortest round trip from the drone's home (the
		// scenario numbers its sites in no useful order).
		pts := make([][2]float64, len(w.Scn.Sites))
		for i, s := range w.Scn.Sites {
			pts[i] = [2]float64{s.CenterE, s.CenterN}
		}
		for _, i := range mission.TourOrder([2]float64{u.HomeOffsetEastM, u.HomeOffsetNorthM}, pts) {
			ids = append(ids, w.Scn.Sites[i].ID)
		}
	}
	var areas []mission.Area
	for _, id := range ids {
		i, ok := byID[id]
		if !ok {
			var have []string
			for _, s := range w.Scn.Sites {
				have = append(have, s.ID)
			}
			sort.Strings(have)
			return mission.Mission{}, diag.New("E-EDGE-005", "drone %s: survey site %q is not in the scenario (sites: %v)", u.ID, id, have)
		}
		s := w.Scn.Sites[i]
		areas = append(areas, mission.Area{ID: s.ID, Polygon: s.Footprint, MarginM: s.SpillM + 4})
	}
	in := vcam.DefaultRGB
	m, err := mission.Survey(u.ID+"-survey", areas, mission.SurveyParams{AltAGL: u.AltAGLm, SpeedMps: u.SurveySpeedMps,
		TransitSpeedMps: u.SpeedMps, SideOverlap: u.SideOverlap, Camera: mission.Camera{W: in.W, H: in.H, HFOVDeg: in.HFOVDeg}})
	if err != nil {
		return mission.Mission{}, diag.Wrap("E-EDGE-005", err, "drone %s survey", u.ID)
	}
	return m, nil
}

// LoadDetector opens the model of [perception] for sharing between drones.
func LoadDetector(pc config.Perception) (perceive.Detector, string, string, error) {
	be, card, err := infer.Open(pc.ModelDir, 6)
	if err != nil {
		return nil, "", "", err
	}
	params := infer.DefaultParams()
	params.Conf = pc.Conf
	name := ""
	if card != nil {
		name = card.Name
	}
	return &lockedDetector{d: &infer.Detector{B: be, P: params}}, be.Name(), name, nil
}

// newPerception loads the model. A model that cannot be loaded does not stop
// edge-eye: the drones keep flying and reporting telemetry, and the reason
// shows in their status.
func newPerception(cfg *config.Edge, w *vcam.World) *perception {
	p := &perception{cfg: cfg.Perception, world: w, simBase: time.Now(), simStart: cfg.Perception.SimStartS}
	if cfg.Perception.Detector == "truth-oracle" {
		p.backend, p.modelName = OracleName, "none"
		return p
	}
	p.det, p.backend, p.modelName, p.err = LoadDetector(cfg.Perception)
	return p
}

// setupPerception gives one drone its pipeline, outbox and keys.
func (r *Runtime) setupPerception(d *Drone, idx int) error {
	cfg := r.cfg
	pc := &perc{frameHz: cfg.FrameHz(d.cfg), camPitch: cfg.Perception.CameraPitchDeg}
	var det perceive.Detector = r.perc.det
	if cfg.Perception.Detector == "truth-oracle" {
		pc.oracle = &Oracle{}
		det = pc.oracle
	}
	scn := r.perc.world.Scn
	pos := func(e, n float64) (float64, float64, float64) {
		pt := scn.Local().ToPoint(e, n)
		return pt.Lat, pt.Lon, scn.GroundAltMSL
	}
	pp := perceive.Params{
		Thermal: perceive.ThermalParams{SkinMinC: cfg.Perception.SkinMinC, SkinMaxC: cfg.Perception.SkinMaxC, WarmDeltaC: cfg.Perception.WarmDeltaC,
			MinPixels: cfg.Perception.MinThermalPx, WarmSpotMinPx: cfg.Perception.WarmSpotMinPx},
		WarmSpots: cfg.Perception.WarmSpots,
		Track: perceive.TrackParams{ConfirmHits: cfg.Perception.ConfirmHits, UpdateMinInterval: cfg.Perception.UpdateMinInterval.Duration, CorrTau: 30 * time.Second,
			DetSigmaM: cfg.Perception.DetectionSigmaM,
			Motion:    perceive.MotionParams{Change: cfg.Perception.MovementChange, TolPx: cfg.Perception.MovementTolerancePx, MinFrames: cfg.Perception.MovementMinFrames}},
		Errors: poseErrors(d.cfg.GPSNoiseM, d.cfg.YawNoiseDeg, cfg.Perception.DebrisHeightM),
	}
	if det != nil {
		pc.pl = perceive.NewPipeline(d.ID, det, vcam.DefaultRGB, vcam.DefaultThermal, pp, pos)
		pc.pl.Tracker.SetIDPrefix(d.ID + "-" + r.runToken)
	}
	ob, err := outbox.Open(outbox.Config{
		Dir: filepath.Join(cfg.Outbox.Dir, d.ID), MaxBytes: int64(cfg.Outbox.MaxQueueMB) << 20,
		AckTimeout: cfg.Outbox.AckTimeout.Duration, MaxBackoff: cfg.Outbox.MaxBackoff.Duration, ProbeAfter: cfg.Outbox.ProbeAfter.Duration,
		Reserve: 300,
		Encode:  func(x *sarpb.Detection, lvl int) ([]byte, error) { return proto.Marshal(perceive.Degrade(x, lvl)) },
	}, nil)
	if err != nil {
		return err
	}
	pc.ob = ob
	inc := cfg.Site.IncidentID
	if pc.detPub, err = r.b.DeclarePublisher(keys.Detection(inc, d.ID), bus.QoSDetection); err != nil {
		return err
	}
	if pc.truthPub, err = r.b.DeclarePublisher(keys.SimTruth(inc, d.ID), bus.QoSSim); err != nil {
		return err
	}
	if pc.ackSub, err = r.b.Subscribe(keys.Ack(inc, d.ID), func(s bus.Sample) { r.handleAck(d, s) }); err != nil {
		return err
	}
	pc.frameNo = int64(idx) * 1_000_000
	d.perc = pc
	if st := ob.Stats(); st.LoadedOnOpen > 0 {
		r.log.Info("detections waiting from before the restart", "uav", d.ID, "queued", st.LoadedOnOpen)
	}
	return nil
}

func (r *Runtime) handleAck(d *Drone, s bus.Sample) {
	var a sarpb.DetectionAck
	if err := msg.Unmarshal(s.Payload, &a); err != nil {
		return
	}
	if msg.CheckHeader(a.GetHeader(), r.cfg.Site.IncidentID) != nil || a.GetUavId() != d.ID {
		return
	}
	d.perc.ob.Ack(a.GetDetectionIds())
}

// perceiveLoop captures and processes frames at the drone's frame rate.
func (r *Runtime) perceiveLoop(ctx context.Context, d *Drone) {
	pc := d.perc
	if pc.pl == nil || pc.frameHz <= 0 {
		return
	}
	t := time.NewTicker(time.Duration(float64(time.Second) / pc.frameHz))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := r.processFrame(d); err != nil {
			pc.frameErrors.Add(1)
			pc.mu.Lock()
			pc.lastErr = err.Error()
			pc.mu.Unlock()
			r.warn(d, "process frame", err)
		}
	}
}

// processFrame captures one frame from the virtual camera, runs the pipeline
// and queues the reports.
func (r *Runtime) processFrame(d *Drone) error {
	pc := d.perc
	d.mu.Lock()
	st := d.sim.State()
	re, rn, ralt := d.sim.ReportedENU()
	ryaw := d.sim.ReportedYaw()
	ti := d.sim.TaskStatus()
	d.mu.Unlock()
	now := r.now()
	hover, err := r.watchTask(d, ti, now)
	if err != nil {
		return err
	}
	if st.AltAGL < minCaptureAltM {
		return nil
	}
	simT := r.perc.SimTime(now)
	pc.frameNo++
	truePose := vcam.Pose{E: st.East, N: st.North, AltAGL: st.AltAGL, YawDeg: st.YawDeg, PitchDeg: pc.camPitch}
	rep := vcam.Pose{E: re, N: rn, AltAGL: math.Max(ralt, 2), YawDeg: ryaw, PitchDeg: pc.camPitch}
	frameID := fmt.Sprintf("%s-%06d", d.ID, pc.frameNo%1_000_000)
	t0 := time.Now()
	fr, err := r.perc.world.Render(vcam.Request{UAV: d.ID, FrameID: frameID, Pose: truePose, Reported: rep, TOffsetS: simT,
		Seed: r.cfg.Seed*10_000_000 + pc.frameNo})
	if err != nil {
		return err
	}
	render := time.Since(t0)
	if pc.oracle != nil {
		pc.oracle.fr = fr
	}
	scn := r.perc.world.Scn
	ll := scn.Local().ToPoint(re, rn)
	pkts, times, err := pc.pl.Process(perceive.Capture{FrameID: frameID, Time: now, SimTimeS: simT, Reported: rep,
		PoseLL: perceive.PoseReport{Lat: ll.Lat, Lon: ll.Lon, AltMSL: scn.GroundAltMSL + rep.AltAGL, AltAGL: rep.AltAGL, YawDeg: rep.YawDeg, PitchDeg: rep.PitchDeg},
		RGB:    fr.RGB, Thermal: fr.Thermal, Hover: hover, TaskID: pc.hoverTask})
	if err != nil {
		return err
	}
	if hover {
		r.afterHoverFrame(d)
	}
	if err := r.queue(d, pkts); err != nil {
		return err
	}
	if r.cfg.Perception.PublishTruth {
		r.publishTruth(d, fr, frameID, now, simT, truePose, rep)
	}
	pc.frames.Add(1)
	ms := func(x time.Duration) float64 { return float64(x.Microseconds()) / 1000 }
	pc.mu.Lock()
	pc.window = append(pc.window, frameStat{at: now, render: ms(render), seg: ms(times.Segment), therm: ms(times.Thermal), track: ms(times.Track), packet: ms(times.Package)})
	cut := now.Add(-30 * time.Second)
	i := 0
	for i < len(pc.window) && pc.window[i].at.Before(cut) {
		i++
	}
	pc.window = pc.window[i:]
	pc.mu.Unlock()
	return nil
}

func (r *Runtime) publishTruth(d *Drone, fr *vcam.Frame, frameID string, now time.Time, simT float64, tp, rp vcam.Pose) {
	scn := r.perc.world.Scn
	pose := func(p vcam.Pose) *sarpb.CapturePose {
		ll := scn.Local().ToPoint(p.E, p.N)
		return &sarpb.CapturePose{Position: &sarpb.GeoPoint{LatDeg: ll.Lat, LonDeg: ll.Lon, AltMslM: scn.GroundAltMSL + p.AltAGL},
			AltAglM: float32(p.AltAGL), YawDeg: float32(p.YawDeg), PitchDeg: float32(p.PitchDeg)}
	}
	m := &sarpb.SimFrameTruth{Header: msg.NewHeader(r.cfg.Site.IncidentID, "sim/"+d.ID, d.perc.truthSeq.Next()), UavId: d.ID, FrameId: frameID,
		CaptureUnixNs: now.UnixNano(), SimTimeS: simT, TruePose: pose(tp), ReportedPose: pose(rp)}
	for _, v := range fr.Meta.Visible {
		m.Visible = append(m.Visible, &sarpb.SimVisible{VictimId: v.VictimID, Part: perceive.PartEnum(v.Class - 1), Pixels: uint32(v.Pixels),
			Position: &sarpb.GeoPoint{LatDeg: v.TruthLat, LonDeg: v.TruthLon, AltMslM: scn.GroundAltMSL}})
	}
	b, err := msg.Marshal(m)
	if err != nil {
		return
	}
	_ = d.perc.truthPub.Put(b) // simulation side channel: not part of the radio budget
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return math.Round(s[len(s)/2]*10) / 10
}

// perceptionStats summarises one drone's pipeline for telemetry.
func (r *Runtime) perceptionStats(d *Drone) *sarpb.PerceptionStats {
	ps := &sarpb.PerceptionStats{}
	if r.perc == nil {
		return ps
	}
	ps.Backend, ps.Model = r.perc.backend, r.perc.modelName
	if r.perc.err != nil {
		ps.Backend = "not running: " + r.perc.err.Error()
	}
	d.mu.Lock()
	id, step, steps, done := d.sim.MissionStatus()
	d.mu.Unlock()
	ps.MissionId, ps.MissionStep, ps.MissionSteps, ps.MissionDone = id, uint32(step), uint32(steps), done
	ps.SimTimeS = math.Round(r.perc.SimTime(r.now())*10) / 10
	pc := d.perc
	if pc == nil {
		return ps
	}
	ps.Enabled = pc.pl != nil
	ps.FrameHzTarget = float32(pc.frameHz)
	ps.Frames = pc.frames.Load()
	ps.Reports, ps.WarmSpotReports = pc.reports.Load(), pc.warmReports.Load()
	if pc.pl != nil {
		ps.Tracks = uint32(len(pc.pl.Tracker.Tracks()))
	}
	pc.mu.Lock()
	var rd, sg, th, tk, pk []float64
	for _, f := range pc.window {
		rd, sg, th, tk, pk = append(rd, f.render), append(sg, f.seg), append(th, f.therm), append(tk, f.track), append(pk, f.packet)
	}
	if n := len(pc.window); n >= 2 {
		span := pc.window[n-1].at.Sub(pc.window[0].at).Seconds()
		if span > 0 {
			ps.FrameHzActual = float32(math.Round(float64(n-1)/span*100) / 100)
		}
	}
	pc.mu.Unlock()
	ps.RenderMs, ps.SegmentMs, ps.ThermalMs, ps.TrackMs, ps.PackageMs = float32(median(rd)), float32(median(sg)), float32(median(th)), float32(median(tk)), float32(median(pk))
	return ps
}

// outboxLink adds the queue figures to the link statistics.
func outboxLink(ls *sarpb.LinkStats, pc *perc) {
	if pc == nil || pc.ob == nil {
		return
	}
	s := pc.ob.Stats()
	ls.QueuedMsgs, ls.QueuedBytes, ls.UnackedMsgs = uint32(s.Queued), uint64(s.QueuedBytes), uint32(s.Unacked)
	ls.DegradeLevel, ls.Outage, ls.Probing = uint32(s.Level), s.Outage, s.Probing
	ls.AckedMsgs, ls.ResentMsgs, ls.DiscardedMsgs = s.Acked, s.Resent, s.Discarded
}

// poseErrors: the flight simulator's error model, and the assumed debris surface.
func poseErrors(gps, yawDeg, surface float64) perceive.PoseErrors {
	e := perceive.DefaultPoseErrors(gps, yawDeg)
	e.SurfaceM = surface
	return e
}
