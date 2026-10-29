package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/perceive"
	"rubblesar/internal/render"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/vcam"
)

// Phase3Options configure the phase 3 acceptance run.
type Phase3Options struct {
	Cfg     *config.ZBook
	Bus     bus.Bus
	World   *vcam.World  // the scenario, for scoring
	Analyst func() error // nil: ask casualty-analyst's status API
	// AnalystStored returns the reports casualty-analyst has stored per drone
	// (nil: its status API); P3-06 checks that every report that arrived was stored.
	AnalystStored func() (map[string]uint64, error)
	OutageAt      time.Duration // after the restart
	Outage        time.Duration
	LowAt         time.Duration
	LowFor        time.Duration
	LowBps        uint32
	MaxRun        time.Duration
	// Drain: after the missions end, wait this long at most for the queues to empty.
	Drain               time.Duration
	RequireMissionsDone bool
	AllowOracle         bool
	Progress            io.Writer
	ProgressEvery       time.Duration
}

// DefaultPhase3Options is the full acceptance run (about 10-15 minutes).
func DefaultPhase3Options(cfg *config.ZBook, b bus.Bus, w *vcam.World) Phase3Options {
	return Phase3Options{Cfg: cfg, Bus: b, World: w, OutageAt: 90 * time.Second, Outage: 60 * time.Second,
		LowAt: 180 * time.Second, LowFor: 90 * time.Second, LowBps: 32000, MaxRun: 15 * time.Minute, Drain: 3 * time.Minute,
		RequireMissionsDone: true, ProgressEvery: 30 * time.Second}
}

// Thresholds of the phase 3 acceptance.
const (
	p3MedianErrM      = 5.0  // plan: median position error under 5 m
	p3MatchRadiusM    = 10.0 // a report further than this from any true part of its class is a false report
	p3LatencyPassMS   = 2000 // NFR: detection to the ZBook within 2 s on a healthy link
	p3LatencyWarnMS   = 5000
	p3BudgetTolerance = 1.10 // window rate may exceed the budget by the burst allowance only
	p3PipelinePassMS  = 250
	p3PipelineWarnMS  = 600
	p3VictimsWarn     = 0.70
	p3Window          = 10 * time.Second
)

type arrival struct {
	at    time.Time
	bytes int
}

type detRec struct {
	d        *sarpb.Detection
	at       time.Time
	bytes    int
	problem  string
	repeated int
}

type truthSeen struct {
	pixels map[string]int // victim id -> largest visible pixels in any frame
	frames int
}

// p3collector records everything the drones send during the run.
type p3collector struct {
	mu       sync.Mutex
	inc      string
	start    time.Time
	traffic  map[string][]arrival // per drone: telemetry, detections, control replies
	dets     map[string]*detRec   // by detection id (first arrival)
	order    []string
	repeats  int
	seqs     map[string]map[uint64]bool
	tel      map[string][]*sarpb.Telemetry
	telAt    map[string][]time.Time
	truth    truthSeen
	acks     int
	badPkts  []string
	outageRx map[string]int // packets received during the outage, per drone
}

func newCollector(inc string) *p3collector {
	return &p3collector{inc: inc, traffic: map[string][]arrival{}, dets: map[string]*detRec{}, seqs: map[string]map[uint64]bool{},
		tel: map[string][]*sarpb.Telemetry{}, telAt: map[string][]time.Time{}, truth: truthSeen{pixels: map[string]int{}}, outageRx: map[string]int{}}
}

func (c *p3collector) onUAV(s bus.Sample) {
	k, ok := keys.ParseUAV(s.Key)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch k.Leaf {
	case "telemetry", "detection", "control/reply":
		c.traffic[k.UAV] = append(c.traffic[k.UAV], arrival{s.Received, len(s.Payload)})
	case "ack":
		c.acks++
		return
	default:
		return
	}
	switch k.Leaf {
	case "telemetry":
		var t sarpb.Telemetry
		if msg.Unmarshal(s.Payload, &t) == nil {
			c.tel[k.UAV] = append(c.tel[k.UAV], &t)
			c.telAt[k.UAV] = append(c.telAt[k.UAV], s.Received)
		}
	case "detection":
		var d sarpb.Detection
		if err := msg.Unmarshal(s.Payload, &d); err != nil {
			c.badPkts = append(c.badPkts, fmt.Sprintf("%s: %v", k.UAV, err))
			return
		}
		if c.start.IsZero() || time.Unix(0, d.GetHeader().GetSentUnixNs()).Before(c.start) {
			return // made before the run started
		}
		if r, ok := c.dets[d.DetectionId]; ok {
			r.repeated++
			c.repeats++
			return
		}
		r := &detRec{d: &d, at: s.Received, bytes: len(s.Payload), problem: packetProblem(&d)}
		if r.problem == "" {
			r.problem = payloadProblem(&d)
		}
		if r.problem != "" {
			c.badPkts = append(c.badPkts, d.DetectionId+": "+r.problem)
		}
		c.dets[d.DetectionId] = r
		c.order = append(c.order, d.DetectionId)
		if c.seqs[k.UAV] == nil {
			c.seqs[k.UAV] = map[uint64]bool{}
		}
		c.seqs[k.UAV][d.GetHeader().GetSeq()] = true
	}
}

func (c *p3collector) onTruth(s bus.Sample) {
	var t sarpb.SimFrameTruth
	if msg.Unmarshal(s.Payload, &t) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start.IsZero() || time.Unix(0, t.CaptureUnixNs).Before(c.start) {
		return
	}
	c.truth.frames++
	per := map[string]int{}
	for _, v := range t.Visible {
		per[v.VictimId] += int(v.Pixels)
	}
	for id, px := range per {
		if px > c.truth.pixels[id] {
			c.truth.pixels[id] = px
		}
	}
}

// payloadProblem checks that a packet carries what its payload level says.
func payloadProblem(d *sarpb.Detection) string {
	switch d.Payload {
	case sarpb.Payload_PAYLOAD_FULL, sarpb.Payload_PAYLOAD_SMALL:
		if d.VisibleCrop == nil {
			return "no visible crop at payload " + d.Payload.String()
		}
	case sarpb.Payload_PAYLOAD_THERMAL_ONLY:
		if d.VisibleCrop != nil {
			return "visible crop in a thermal-only packet"
		}
	case sarpb.Payload_PAYLOAD_METADATA:
		if d.VisibleCrop != nil || d.ThermalCrop != nil {
			return "crops in a metadata-only packet"
		}
	default:
		return "no payload level"
	}
	for _, v := range d.Outline {
		if v < -0.01 || v > 1.01 {
			return "outline outside the crop"
		}
	}
	return ""
}

func (c *p3collector) latestTel(uav string) *sarpb.Telemetry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.tel[uav]; len(t) > 0 {
		return t[len(t)-1]
	}
	return nil
}

// analystUp asks casualty-analyst's status API whether it is running.
// analystStored reads the reports stored per drone from casualty-analyst's status API.
func analystStored(addr string) (map[string]uint64, error) {
	cl := http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get("http://" + addr + "/v1/status")
	if err != nil {
		return nil, fmt.Errorf("casualty-analyst status API at %s: %v", addr, err)
	}
	defer resp.Body.Close()
	var doc struct {
		State  string `json:"state"`
		Intake struct {
			UAVs map[string]struct {
				Stored uint64 `json:"stored"`
			} `json:"uavs"`
		} `json:"intake"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("casualty-analyst status API at %s answered something else: %v", addr, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("casualty-analyst is %s", doc.State)
	}
	out := map[string]uint64{}
	for id, u := range doc.Intake.UAVs {
		out[id] = u.Stored
	}
	return out, nil
}

func analystUp(addr string) error {
	cl := http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get("http://" + addr + "/v1/status")
	if err != nil {
		return fmt.Errorf("casualty-analyst status API at %s: %v", addr, err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("casualty-analyst status API at %s answered something else: %v", addr, err)
	}
	return nil
}

// Phase3 runs the phase 3 acceptance test: the drones fly their missions with
// the perception pipeline; detections must reach the ZBook within each
// drone's budget (also while the budget is cut), survive a link outage, arrive
// promptly and lie near the true body parts.
func Phase3(ctx context.Context, o Phase3Options, out io.Writer) *Report {
	r := NewReport("phase3", out)
	r.Share = true
	cfg := o.Cfg
	inc := cfg.Site.IncidentID
	r.Context["incident"] = inc
	r.Context["drones"] = strings.Join(cfg.Check.ExpectUAVs, ",")
	prog := o.Progress
	if prog == nil {
		prog = io.Discard
	}
	col := newCollector(inc)
	sub, err := o.Bus.Subscribe(keys.Root+"/"+inc+"/uav/*/**", col.onUAV)
	if err != nil {
		r.Run("P3-01", "drones, pipeline and casualty-analyst", func() Result {
			return Result{Status: Fail, Code: "E-CHK-311", Detail: "cannot subscribe", Evidence: []string{err.Error()}}
		})
		return r
	}
	defer sub.Close()
	tsub, err := o.Bus.Subscribe(keys.SimTruthAll(inc), col.onTruth)
	if err == nil {
		defer tsub.Close()
	}
	ctl, err := newController(o.Bus, inc)
	if err != nil {
		r.Run("P3-01", "drones, pipeline and casualty-analyst", func() Result {
			return Result{Status: Fail, Code: "E-CHK-311", Detail: "cannot open the control channel", Evidence: []string{err.Error()}}
		})
		return r
	}
	defer ctl.close()

	normal := map[string]uint32{}
	res := r.Run("P3-01", "drones, pipeline and casualty-analyst", func() Result {
		return safe("E-CHK-311", func() Result { return p3Ready(ctx, o, col, normal) })
	})
	names := map[string]string{"P3-02": "detection packets well formed", "P3-03": "within each drone's budget",
		"P3-04": "position error", "P3-05": "victims found (colour and thermal)", "P3-06": "nothing lost (outage, storage)",
		"P3-07": "detection to ZBook delay", "P3-08": "Jetson pipeline speed"}
	ids := []string{"P3-02", "P3-03", "P3-04", "P3-05", "P3-06", "P3-07", "P3-08"}
	if res.Status == Fail {
		for _, id := range ids {
			r.Skip(id, names[id], "needs P3-01")
		}
		return r
	}

	stored := o.AnalystStored
	if stored == nil {
		stored = func() (map[string]uint64, error) { return analystStored(cfg.Analyst.Listen) }
	}
	before, berr := stored()
	run, err := p3Drive(ctx, o, col, ctl, normal, prog)
	if err != nil {
		// A run that did not happen fails the acceptance (the checks after it are skipped).
		r.Run(ids[0], names[ids[0]], func() Result {
			return Result{Status: Fail, Code: "E-CHK-311", Detail: "the run could not be completed: " + err.Error()}
		})
		for _, id := range ids[1:] {
			r.Skip(id, names[id], "the run could not be completed")
		}
		return r
	}
	time.Sleep(1500 * time.Millisecond) // casualty-analyst writes and acknowledges in batches
	after, aerr := stored()
	run.storedBefore, run.storedAfter, run.storedErr = before, after, errors.Join(berr, aerr)
	r.Context["run"] = fmt.Sprintf("%.0f s, %d reports, %d truth frames", run.end.Sub(run.start).Seconds(), len(col.order), col.truth.frames)
	r.Run("P3-02", names["P3-02"], func() Result { return safe("E-CHK-312", func() Result { return p3Packets(col) }) })
	r.Run("P3-03", names["P3-03"], func() Result { return safe("E-CHK-313", func() Result { return p3Budget(col, run, normal, o) }) })
	r.Run("P3-04", names["P3-04"], func() Result { return safe("E-CHK-314", func() Result { return p3Positions(col, o.World) }) })
	r.Run("P3-05", names["P3-05"], func() Result { return safe("E-CHK-315", func() Result { return p3Victims(col, o.World) }) })
	r.Run("P3-06", names["P3-06"], func() Result { return safe("E-CHK-316", func() Result { return p3Outage(col, run, o) }) })
	r.Run("P3-07", names["P3-07"], func() Result { return safe("E-CHK-317", func() Result { return p3Latency(col, run, o) }) })
	r.Run("P3-08", names["P3-08"], func() Result {
		return safe("E-CHK-318", func() Result { return p3Speed(col, run, cfg.Check.ExpectUAVs) })
	})
	return r
}

// p3Ready waits for every drone's telemetry with perception running and checks casualty-analyst.
func p3Ready(ctx context.Context, o Phase3Options, col *p3collector, normal map[string]uint32) Result {
	cfg := o.Cfg
	deadline := time.Now().Add(cfg.Check.DiscoverTimeout.Duration)
	var missing []string
	for {
		missing = missing[:0]
		for _, id := range cfg.Check.ExpectUAVs {
			if t := col.latestTel(id); t == nil || t.Perception == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return Result{Status: Fail, Code: "E-CHK-311", Detail: "interrupted"}
		case <-time.After(250 * time.Millisecond):
		}
	}
	if len(missing) > 0 {
		return Result{Status: Fail, Code: "E-CHK-311", Detail: fmt.Sprintf("no phase 3 telemetry from %s within %v", strings.Join(missing, ", "), cfg.Check.DiscoverTimeout.Duration),
			Hint: "On the Jetson: edge-eye must run the phase 3 build and config (scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config); check 'edge-eye status'."}
	}
	var ev []string
	backend := ""
	for _, id := range cfg.Check.ExpectUAVs {
		t := col.latestTel(id)
		p := t.Perception
		normal[id] = t.GetLink().GetBudgetBps()
		ev = append(ev, fmt.Sprintf("%s: %s, mission %s (%d waypoints), camera %.1f Hz, budget %d kbit/s", id, p.Backend, p.MissionId, p.MissionSteps, p.FrameHzTarget, normal[id]/1000))
		backend = p.Backend
		switch {
		case !p.Enabled || strings.HasPrefix(p.Backend, "not running"):
			return Result{Status: Fail, Code: "E-CHK-311", Detail: id + ": perception not running", Evidence: ev,
				Hint: "the Jetson cannot load the model: run scripts/jetson/80-engine.sh --from <model folder>, then 'sudo systemctl restart edge-eye'"}
		case p.MissionSteps == 0:
			return Result{Status: Fail, Code: "E-CHK-311", Detail: id + ": no survey mission (behaviour must be \"survey\")", Evidence: ev}
		case strings.HasPrefix(p.Backend, "truth-oracle") && !o.AllowOracle:
			return Result{Status: Fail, Code: "E-CHK-311", Detail: id + ": the truth oracle is configured, not the model", Evidence: ev,
				Hint: "set [perception] detector = \"model\" in /etc/rubblesar/edge-eye.toml and restart edge-eye"}
		}
	}
	check := o.Analyst
	if check == nil {
		check = func() error { return analystUp(cfg.Analyst.Listen) }
	}
	if err := check(); err != nil {
		return Result{Status: Fail, Code: "E-CHK-311", Detail: "casualty-analyst is not running", Evidence: append(ev, err.Error()),
			Hint: "sudo systemctl start casualty-analyst (installed by scripts/zbook/30-install.sh); it acknowledges the detections"}
	}
	detail := fmt.Sprintf("%d drones ready, model %s, casualty-analyst running", len(cfg.Check.ExpectUAVs), backend)
	if strings.HasPrefix(backend, "truth-oracle") {
		return Result{Status: Warn, Code: "E-CHK-311", Detail: detail + " (oracle allowed: tests the pipeline and link, not the model)", Evidence: ev,
			Hint: "for the acceptance run set [perception] detector = \"model\" on the Jetson"}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p3run records when things happened.
type p3run struct {
	start, outageStart, outageEnd, lowStart, lowEnd, missionsDone, end time.Time
	createdBefore                                                      map[string]uint64 // reports counter at the restart
	storedBefore, storedAfter                                          map[string]uint64 // casualty-analyst's stored counts around the run
	storedErr                                                          error
	timeLimited                                                        bool // stopped by the time limit, not because everything was delivered
}

// p3Drive restarts the missions and runs the outage and low-budget steps.
func p3Drive(ctx context.Context, o Phase3Options, col *p3collector, ctl *controller, normal map[string]uint32, prog io.Writer) (*p3run, error) {
	cfg := o.Cfg
	ids := cfg.Check.ExpectUAVs
	run := &p3run{createdBefore: map[string]uint64{}}
	all := func(what string, set func(*sarpb.Control)) error {
		for _, id := range ids {
			if _, _, err := ctl.send(id, set, cfg.Check.PingTimeout.Duration*3); err != nil {
				return fmt.Errorf("%s %s: %v", what, id, err)
			}
		}
		return nil
	}
	for _, id := range ids {
		if t := col.latestTel(id); t != nil {
			run.createdBefore[id] = t.GetPerception().GetReports()
		}
	}
	col.mu.Lock()
	run.start = time.Now()
	col.start = run.start
	col.mu.Unlock()
	if err := all("restart", func(c *sarpb.Control) {
		c.Cmd = &sarpb.Control_RestartMission{RestartMission: &sarpb.RestartMission{SimTimeS: -1, ClearTracks: true, ClearQueue: true}}
	}); err != nil {
		return nil, err
	}
	fmt.Fprintf(prog, "  run started: missions restarted on %s; outage of %.0f s at +%.0f s, %d kbit/s for %.0f s at +%.0f s; up to %.0f min\n",
		strings.Join(ids, ", "), o.Outage.Seconds(), o.OutageAt.Seconds(), o.LowBps/1000, o.LowFor.Seconds(), o.LowAt.Seconds(), o.MaxRun.Minutes())
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastProg := time.Now()
	drainSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			restoreBudgets(ctl, normal, cfg)
			return nil, ctx.Err()
		case <-tick.C:
		}
		el := time.Since(run.start)
		if run.outageStart.IsZero() && el >= o.OutageAt {
			run.outageStart = time.Now()
			if err := all("outage", func(c *sarpb.Control) {
				c.Cmd = &sarpb.Control_SetLinkOutage{SetLinkOutage: &sarpb.SetLinkOutage{Seconds: uint32(o.Outage.Seconds())}}
			}); err != nil {
				return nil, err
			}
			run.outageEnd = run.outageStart.Add(o.Outage)
			fmt.Fprintf(prog, "  +%3.0f s  simulated outage for %.0f s on every drone\n", el.Seconds(), o.Outage.Seconds())
		}
		if run.lowStart.IsZero() && el >= o.LowAt && o.LowFor > 0 {
			run.lowStart = time.Now()
			if err := all("low budget", func(c *sarpb.Control) {
				c.Cmd = &sarpb.Control_SetLinkBudget{SetLinkBudget: &sarpb.SetLinkBudget{Bps: o.LowBps}}
			}); err != nil {
				return nil, err
			}
			fmt.Fprintf(prog, "  +%3.0f s  budget cut to %d kbit/s for %.0f s\n", el.Seconds(), o.LowBps/1000, o.LowFor.Seconds())
		}
		if !run.lowStart.IsZero() && run.lowEnd.IsZero() && time.Since(run.lowStart) >= o.LowFor {
			if err := restoreBudgets(ctl, normal, cfg); err != nil {
				return nil, err
			}
			run.lowEnd = time.Now()
			fmt.Fprintf(prog, "  +%3.0f s  budget restored\n", el.Seconds())
		}
		done, drained := true, true
		var line []string
		for _, id := range ids {
			t := col.latestTel(id)
			if t == nil {
				done, drained = false, false
				continue
			}
			p, l := t.GetPerception(), t.GetLink()
			done = done && p.GetMissionDone()
			drained = drained && l.GetQueuedMsgs() == 0 && l.GetUnackedMsgs() == 0
			line = append(line, fmt.Sprintf("%s %d/%d q%d", id, p.GetMissionStep(), p.GetMissionSteps(), l.GetQueuedMsgs()))
		}
		stepsDone := !run.lowEnd.IsZero() && !run.outageStart.IsZero() && time.Now().After(run.outageEnd)
		if time.Since(lastProg) >= o.ProgressEvery && o.ProgressEvery > 0 {
			col.mu.Lock()
			n := len(col.order)
			col.mu.Unlock()
			fmt.Fprintf(prog, "  +%3.0f s  %d reports received | %s\n", el.Seconds(), n, strings.Join(line, " | "))
			lastProg = time.Now()
		}
		if done && run.missionsDone.IsZero() {
			run.missionsDone = time.Now()
		}
		finished := stepsDone && drained && (done || !o.RequireMissionsDone)
		if finished {
			if drainSince.IsZero() {
				drainSince = time.Now()
			} else if time.Since(drainSince) >= 5*time.Second {
				break
			}
		} else {
			drainSince = time.Time{}
		}
		if el >= o.MaxRun || (!run.missionsDone.IsZero() && time.Since(run.missionsDone) > o.Drain) {
			fmt.Fprintf(prog, "  +%3.0f s  stopping (time limit)\n", el.Seconds())
			run.timeLimited = true
			break
		}
	}
	if run.lowEnd.IsZero() {
		_ = restoreBudgets(ctl, normal, cfg)
	}
	run.end = time.Now()
	return run, nil
}

func restoreBudgets(ctl *controller, normal map[string]uint32, cfg *config.ZBook) error {
	for id, b := range normal {
		bps := b
		if _, _, err := ctl.send(id, func(c *sarpb.Control) {
			c.Cmd = &sarpb.Control_SetLinkBudget{SetLinkBudget: &sarpb.SetLinkBudget{Bps: bps}}
		}, cfg.Check.PingTimeout.Duration*3); err != nil {
			return fmt.Errorf("restoring %s's budget: %v", id, err)
		}
	}
	return nil
}

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	return s[max(0, min(i, len(s)-1))]
}

// p3Packets: every report decodes and carries what its payload level says.
func p3Packets(col *p3collector) Result {
	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.order) == 0 {
		return Result{Status: Fail, Code: "E-CHK-312", Detail: "no detection reports arrived during the run",
			Hint: "check 'edge-eye status' on the Jetson (frames and reports) and that casualty-analyst runs on the ZBook"}
	}
	sizes := map[string][]float64{}
	parts := map[string]int{}
	for _, id := range col.order {
		r := col.dets[id]
		lvl := strings.ToLower(strings.TrimPrefix(r.d.Payload.String(), "PAYLOAD_"))
		sizes[lvl] = append(sizes[lvl], float64(r.bytes))
		parts[strings.ToLower(strings.TrimPrefix(r.d.Part.String(), "PART_CLASS_"))]++
	}
	var lv []string
	for _, l := range []string{"full", "small", "thermal_only", "metadata"} {
		if v := sizes[l]; len(v) > 0 {
			lv = append(lv, fmt.Sprintf("%s %d (median %.1f KB)", strings.ReplaceAll(l, "_", " "), len(v), pct(v, 0.5)/1024))
		}
	}
	var pv []string
	for _, p := range []string{"hand", "arm", "leg", "head", "torso", "warm_spot"} {
		if parts[p] > 0 {
			pv = append(pv, fmt.Sprintf("%s %d", strings.ReplaceAll(p, "_", " "), parts[p]))
		}
	}
	detail := fmt.Sprintf("%d reports (%s); payloads: %s; %d repeated after a lost acknowledgement", len(col.order), strings.Join(pv, ", "), strings.Join(lv, ", "), col.repeats)
	if len(col.badPkts) > 0 {
		return Result{Status: Fail, Code: "E-CHK-312", Detail: detail + fmt.Sprintf("; %d malformed", len(col.badPkts)), Evidence: limit(col.badPkts, 10)}
	}
	return Result{Status: Pass, Detail: detail}
}

// p3Budget: every drone's traffic stays within its budget over 10 s windows,
// at the normal budget and while it is cut.
func p3Budget(col *p3collector, run *p3run, normal map[string]uint32, o Phase3Options) Result {
	col.mu.Lock()
	defer col.mu.Unlock()
	var ev []string
	worstRatio := 0.0
	failed := false
	for _, id := range o.Cfg.Check.ExpectUAVs {
		arr := col.traffic[id]
		sort.Slice(arr, func(i, j int) bool { return arr[i].at.Before(arr[j].at) })
		maxMsg := 0
		for _, a := range arr {
			maxMsg = max(maxMsg, a.bytes)
		}
		var worst, lowSum, lowN float64
		var worstAt time.Time
		var worstB uint32
		windows := 0
		for t := run.start; !t.Add(p3Window).After(run.end); t = t.Add(time.Second) {
			we := t.Add(p3Window)
			if overlaps(t, we, run.outageStart, run.outageEnd) {
				continue
			}
			var b uint32
			switch {
			case !run.lowStart.IsZero() && !t.Before(run.lowStart.Add(2*time.Second)) && !we.After(run.lowEnd):
				b = o.LowBps
			case overlaps(t, we, run.lowStart, run.lowEnd.Add(2*time.Second)):
				continue // straddles a budget change
			default:
				b = normal[id]
			}
			if b == 0 {
				continue
			}
			n := 0
			for _, a := range arr {
				if !a.at.Before(t) && a.at.Before(we) {
					n += a.bytes
				}
			}
			rate := float64(n*8) / p3Window.Seconds()
			allowed := float64(b)*p3BudgetTolerance + float64(maxMsg*8)/p3Window.Seconds()
			windows++
			if b == o.LowBps {
				lowSum += rate
				lowN++
			}
			if ratio := rate / allowed; ratio > worst {
				worst, worstAt, worstB = ratio, t, b
			}
		}
		maxLvl := uint32(0)
		for i, tm := range col.tel[id] {
			at := col.telAt[id][i]
			if !run.lowStart.IsZero() && at.After(run.lowStart) && at.Before(run.lowEnd.Add(30*time.Second)) {
				maxLvl = max(maxLvl, tm.GetLink().GetDegradeLevel())
			}
		}
		line := fmt.Sprintf("%s: %d windows checked, busiest at %.0f%% of what the budget allows (%d kbit/s, at +%.0f s)", id, windows, worst*100, worstB/1000, worstAt.Sub(run.start).Seconds())
		if lowN > 0 {
			line += fmt.Sprintf("; at %d kbit/s it averaged %.1f kbit/s and reduced payloads to level %d", o.LowBps/1000, lowSum/lowN/1000, maxLvl)
		}
		ev = append(ev, line)
		worstRatio = math.Max(worstRatio, worst)
		if worst > 1 {
			failed = true
		}
	}
	detail := fmt.Sprintf("busiest 10 s window at %.0f%% of the allowance (budget +10%% burst + one message)", worstRatio*100)
	if failed {
		return Result{Status: Fail, Code: "E-CHK-313", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

func overlaps(a0, a1, b0, b1 time.Time) bool {
	if b0.IsZero() || b1.IsZero() {
		return false
	}
	return a0.Before(b1) && b0.Before(a1)
}

// latestPerTrack returns the last revision received of every track.
func (c *p3collector) latestPerTrack() []*sarpb.Detection {
	m := map[string]*sarpb.Detection{}
	for _, id := range c.order {
		d := c.dets[id].d
		k := d.UavId + "/" + d.TrackId
		if o := m[k]; o == nil || d.Revision > o.Revision {
			m[k] = d
		}
	}
	var out []*sarpb.Detection
	for _, d := range m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DetectionId < out[j].DetectionId })
	return out
}

func truthParts(w *vcam.World) []render.TruthPart {
	var out []render.TruthPart
	for _, st := range w.Scn.Sites {
		out = append(out, w.Manifest(st.ID).Truth...)
	}
	return out
}

// p3Positions: distance from each reported body part (latest revision) to
// the nearest true part of the same class.
func p3Positions(col *p3collector, w *vcam.World) Result {
	col.mu.Lock()
	tracks := col.latestPerTrack()
	col.mu.Unlock()
	truth := truthParts(w)
	var errs []float64
	inCEP, far := 0, 0
	perClass := map[string][]float64{}
	for _, d := range tracks {
		c := perceive.PartIndex(d.Part)
		if c < 0 || c == perceive.WarmClass {
			continue
		}
		e, n := w.Scn.Local().ToLocal(geoOf(d.Position))
		best := math.Inf(1)
		for _, tp := range truth {
			if tp.Class-1 == c {
				best = math.Min(best, math.Hypot(tp.E-e, tp.N-n))
			}
		}
		if best > p3MatchRadiusM {
			far++
			continue
		}
		errs = append(errs, best)
		perClass[perceive.PartNames[c]] = append(perClass[perceive.PartNames[c]], best)
		if best <= float64(d.CepM) {
			inCEP++
		}
	}
	if len(errs) == 0 {
		st := Warn
		if len(tracks) == 0 {
			st = Fail
		}
		return Result{Status: st, Code: "E-CHK-314", Detail: fmt.Sprintf("no body part reported within %.0f m of a true part of its class (%d far off)", p3MatchRadiusM, far)}
	}
	m := pct(errs, 0.5)
	var ev []string
	for _, c := range perceive.PartNames[:5] {
		if v := perClass[c]; len(v) > 0 {
			ev = append(ev, fmt.Sprintf("%-5s %3d parts, median %.2f m, worst %.2f m", c, len(v), pct(v, 0.5), pct(v, 1)))
		}
	}
	ev = append(ev, fmt.Sprintf("%d of %d (%.0f%%) lie within their reported CEP (about 50%% expected)", inCEP, len(errs), float64(inCEP)/float64(len(errs))*100))
	detail := fmt.Sprintf("%d body parts: median %.2f m, 95%% within %.2f m (target: median under %.0f m); %d reports more than %.0f m from any true part of their class",
		len(errs), m, pct(errs, 0.95), p3MedianErrM, far, p3MatchRadiusM)
	if m > p3MedianErrM {
		return Result{Status: Fail, Code: "E-CHK-314", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p3Victims: victims the cameras saw (truth channel) that have a report
// (any part, or a warm spot) within 3 m of their body.
func p3Victims(col *p3collector, w *vcam.World) Result {
	col.mu.Lock()
	tracks := col.latestPerTrack()
	seen := map[string]int{}
	for k, v := range col.truth.pixels {
		seen[k] = v
	}
	frames := col.truth.frames
	col.mu.Unlock()
	if frames == 0 {
		return Result{Status: Warn, Code: "E-CHK-315", Detail: "no ground-truth frames received ([perception] publish_truth = false on the Jetson?)"}
	}
	byVictim := map[string][]render.TruthPart{}
	for _, tp := range truthParts(w) {
		byVictim[tp.VictimID] = append(byVictim[tp.VictimID], tp)
	}
	var found, missed []string
	nSeen := 0
	for vid, px := range seen {
		if px < 30 {
			continue
		}
		nSeen++
		ok := false
		for _, d := range tracks {
			e, n := w.Scn.Local().ToLocal(geoOf(d.Position))
			for _, tp := range byVictim[vid] {
				if math.Hypot(tp.E-e, tp.N-n) <= 3 {
					ok = true
				}
			}
		}
		if ok {
			found = append(found, vid)
		} else {
			missed = append(missed, fmt.Sprintf("%s (at most %d px visible)", vid, px))
		}
	}
	sort.Strings(found)
	sort.Strings(missed)
	if nSeen == 0 {
		return Result{Status: Warn, Code: "E-CHK-315", Detail: "no victim was in view during the run"}
	}
	rate := float64(len(found)) / float64(nSeen)
	detail := fmt.Sprintf("%d of %d victims the cameras saw have a report within 3 m (%.0f%%)", len(found), nSeen, rate*100)
	ev := []string{"found: " + strings.Join(found, " ")}
	if len(missed) > 0 {
		ev = append(ev, "missed: "+strings.Join(missed, ", "))
	}
	switch {
	case len(found) == 0:
		return Result{Status: Fail, Code: "E-CHK-315", Detail: detail, Evidence: ev}
	case rate < p3VictimsWarn:
		return Result{Status: Warn, Code: "E-CHK-315", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p3Outage: during the simulated outage the drones were silent, and afterwards
// every report arrived (no gaps in the sequence numbers), the queues emptied
// and nothing was discarded.
func p3Outage(col *p3collector, run *p3run, o Phase3Options) Result {
	col.mu.Lock()
	defer col.mu.Unlock()
	var ev []string
	lost, discarded, leftover, during, notStored := 0, uint64(0), 0, 0, 0
	var catchUp time.Duration
	for _, id := range o.Cfg.Check.ExpectUAVs {
		for _, a := range col.traffic[id] {
			if a.at.After(run.outageStart.Add(time.Second)) && a.at.Before(run.outageEnd.Add(-time.Second)) {
				during++
			}
		}
		seqs := col.seqs[id]
		var lo, hi uint64 = math.MaxUint64, 0
		for s := range seqs {
			lo, hi = min(lo, s), max(hi, s)
		}
		miss := 0
		if hi >= lo {
			for s := lo; s <= hi; s++ {
				if !seqs[s] {
					miss++
				}
			}
		}
		lost += miss
		var madeDuring, lastArr time.Time
		for _, dr := range col.dets {
			if dr.d.UavId != id {
				continue
			}
			made := time.Unix(0, dr.d.GetHeader().GetSentUnixNs())
			if !made.Before(run.outageStart) && made.Before(run.outageEnd) {
				madeDuring = made
				if dr.at.After(lastArr) {
					lastArr = dr.at
				}
			}
		}
		if !madeDuring.IsZero() && lastArr.Sub(run.outageEnd) > catchUp {
			catchUp = lastArr.Sub(run.outageEnd)
		}
		t := col.tel[id]
		var q, u uint32
		var disc uint64
		if len(t) > 0 {
			l := t[len(t)-1].GetLink()
			q, u, disc = l.GetQueuedMsgs(), l.GetUnackedMsgs(), l.GetDiscardedMsgs()
			// Left over: still queued over the last 5 s of the run (the last
			// telemetry alone may catch a report that is on its way).
			last := time.Unix(0, t[len(t)-1].GetHeader().GetSentUnixNs())
			for i := len(t) - 2; i >= 0; i-- {
				if last.Sub(time.Unix(0, t[i].GetHeader().GetSentUnixNs())) > 5*time.Second {
					break
				}
				q = min(q, t[i].GetLink().GetQueuedMsgs())
			}
		}
		discarded += disc
		leftover += int(q)
		ev = append(ev, fmt.Sprintf("%s: reports %d..%d received, %d missing; at the end %d queued, %d unacknowledged, %d discarded since start", id, lo, hi, miss, q, u, disc))
		// Every report that arrived must also be on the ZBook's disk.
		arrived := 0
		for _, dr := range col.dets {
			if dr.d.UavId == id && !dr.at.Before(run.start) {
				arrived++
			}
		}
		if run.storedErr == nil && run.storedAfter != nil {
			got := run.storedAfter[id] - min(run.storedBefore[id], run.storedAfter[id])
			ev = append(ev, fmt.Sprintf("%s: casualty-analyst stored %d of the %d reports that arrived", id, min(got, uint64(arrived)), arrived))
			if got < uint64(arrived) {
				notStored += arrived - int(got)
			}
		}
	}
	if run.storedErr != nil {
		ev = append(ev, "could not compare with casualty-analyst's storage: "+run.storedErr.Error())
	}
	detail := fmt.Sprintf("%.0f s outage: %d messages arrived during it; %d reports missing after it; caught up %.0f s after the link returned", o.Outage.Seconds(), during, lost, catchUp.Seconds())
	switch {
	case lost > 0 || discarded > 0:
		return Result{Status: Fail, Code: "E-CHK-316", Detail: detail, Evidence: ev}
	case notStored > 0:
		return Result{Status: Fail, Code: "E-CHK-316", Detail: detail + fmt.Sprintf("; %d reports arrived but casualty-analyst did not store them", notStored), Evidence: ev,
			Hint: "'casualty-analyst status' shows them as REPEATS or id conflicts; paste the block with that output and 'journalctl -u casualty-analyst -n 50'"}
	case run.storedErr != nil:
		return Result{Status: Warn, Code: "E-CHK-316", Detail: detail + "; not compared with casualty-analyst's storage", Evidence: ev}
	case leftover > 0 && during == 0 && run.timeLimited:
		return Result{Status: Warn, Code: "E-CHK-316", Detail: detail + fmt.Sprintf("; the run stopped at its time limit with %d reports still queued", leftover), Evidence: ev,
			Hint: "not a fault if the drones were still sending: run again with more time (--max-minutes) so the queues can empty"}
	case leftover > 0 || during > 0:
		return Result{Status: Warn, Code: "E-CHK-316", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p3Latency: time from a report's creation on the drone (the frame that
// confirmed the part) to its arrival on the ZBook, on a healthy link.
func p3Latency(col *p3collector, run *p3run, o Phase3Options) Result {
	col.mu.Lock()
	defer col.mu.Unlock()
	healthy := func(t time.Time) bool {
		if !run.outageStart.IsZero() && !t.Before(run.outageStart) && t.Before(run.outageEnd.Add(30*time.Second)) {
			return false
		}
		if !run.lowStart.IsZero() && !t.Before(run.lowStart) && t.Before(run.lowEnd.Add(60*time.Second)) {
			return false
		}
		return true
	}
	var made, capt []float64
	per := map[string][]float64{}
	for _, id := range col.order {
		r := col.dets[id]
		c := time.Unix(0, r.d.GetHeader().GetSentUnixNs())
		if !healthy(c) || !healthy(r.at) {
			continue
		}
		ms := float64(r.at.Sub(c).Microseconds()) / 1000
		made = append(made, ms)
		per[r.d.UavId] = append(per[r.d.UavId], ms)
		if r.d.CaptureUnixNs > 0 {
			capt = append(capt, float64(r.at.UnixNano()-r.d.CaptureUnixNs)/1e6)
		}
	}
	if len(made) == 0 {
		return Result{Status: Warn, Code: "E-CHK-317", Detail: "no reports outside the outage and low-budget periods"}
	}
	m := pct(made, 0.5)
	var ev []string
	for _, id := range o.Cfg.Check.ExpectUAVs {
		if v := per[id]; len(v) > 0 {
			ev = append(ev, fmt.Sprintf("%s: %d reports, median %.0f ms, 95%% within %.0f ms", id, len(v), pct(v, 0.5), pct(v, 0.95)))
		}
	}
	ev = append(ev, fmt.Sprintf("from the capture of the frame the crops come from (often an earlier, sharper sighting): median %.1f s", pct(capt, 0.5)/1000))
	detail := fmt.Sprintf("%d reports on a healthy link: median %.0f ms, 95%% within %.0f ms from the drone confirming the part to the ZBook (target: %d ms)", len(made), m, pct(made, 0.95), p3LatencyPassMS)
	switch {
	case m <= p3LatencyPassMS:
		return Result{Status: Pass, Detail: detail, Evidence: ev}
	case m <= p3LatencyWarnMS:
		return Result{Status: Warn, Code: "E-CHK-317", Detail: detail, Evidence: ev}
	}
	return Result{Status: Fail, Code: "E-CHK-317", Detail: detail, Evidence: ev}
}

// p3Speed: the pipeline time per frame and the frame rate reached, from the
// drones' own statistics during the run.
func p3Speed(col *p3collector, run *p3run, ids []string) Result {
	col.mu.Lock()
	defer col.mu.Unlock()
	var ev []string
	worst, slowFps := 0.0, false
	for _, id := range ids {
		var pipe, fps, rnd []float64
		target := 0.0
		for i, t := range col.tel[id] {
			at := col.telAt[id][i]
			p := t.GetPerception()
			if at.Before(run.start) || p == nil || p.Frames == 0 || p.MissionDone || t.State == sarpb.FlightState_FLIGHT_STATE_LANDED {
				continue
			}
			target = float64(p.FrameHzTarget)
			if p.SegmentMs > 0 {
				pipe = append(pipe, float64(p.SegmentMs+p.ThermalMs+p.TrackMs+p.PackageMs))
				rnd = append(rnd, float64(p.RenderMs))
			}
			if p.FrameHzActual > 0 {
				fps = append(fps, float64(p.FrameHzActual))
			}
		}
		if len(pipe) == 0 {
			ev = append(ev, id+": no frames processed during the run")
			continue
		}
		mp, mf := pct(pipe, 0.5), pct(fps, 0.5)
		worst = math.Max(worst, mp)
		if mf < 0.8*target {
			slowFps = true
		}
		ev = append(ev, fmt.Sprintf("%s: pipeline median %.0f ms per frame, %.2f of %.1f frames/s reached; rendering the simulated frame %.0f ms (simulation cost)", id, mp, mf, target, pct(rnd, 0.5)))
	}
	detail := fmt.Sprintf("slowest drone's pipeline median %.0f ms per frame (pass <= %d ms)", worst, p3PipelinePassMS)
	switch {
	case worst == 0:
		return Result{Status: Fail, Code: "E-CHK-318", Detail: "no frames processed", Evidence: ev}
	case worst > p3PipelineWarnMS:
		return Result{Status: Fail, Code: "E-CHK-318", Detail: detail, Evidence: ev}
	case worst > p3PipelinePassMS || slowFps:
		if slowFps {
			detail += "; some drones reached under 80% of their frame rate (the Jetson also renders the simulated frames)"
		}
		return Result{Status: Warn, Code: "E-CHK-318", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}
