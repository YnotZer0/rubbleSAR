package check

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/casualty"
	"rubblesar/internal/config"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/render"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
	"rubblesar/internal/vcam"
)

// Phase4Options configure the phase 4 acceptance run.
type Phase4Options struct {
	Cfg   *config.ZBook
	Bus   bus.Bus
	World *vcam.World
	// Services checks that casualty-analyst and mission-planner run their
	// phase 4 versions (nil: their status APIs).
	Services func() error
	// Reset archives casualty-analyst's run and forgets mission-planner's
	// tasks, so the run is scored on its own (nil: their APIs).
	Reset func(stamp string) error
	// AnalystStored returns the reports stored per drone (nil: the status API
	// when Services is nil, else not compared).
	AnalystStored func() (map[string]uint64, error)
	MaxRun        time.Duration
	Settle        time.Duration // the run ends after this long with nothing left to do
	TaskTime      time.Duration // expected time per investigation, for the progress estimate
	// AllowOracle accepts drones running the truth oracle.
	AllowOracle   bool
	Progress      io.Writer
	ProgressEvery time.Duration
	// ExtraProgress adds to each progress line (phase 5: the rescue teams).
	ExtraProgress func() string
	// StartedRun is told when the missions have been restarted (phase 5).
	StartedRun func()
	// Suite names the run in the archive folder (default "phase4").
	Suite string
}

// DefaultPhase4Options is the full acceptance run (about 25-35 minutes).
func DefaultPhase4Options(cfg *config.ZBook, b bus.Bus, w *vcam.World) Phase4Options {
	return Phase4Options{Cfg: cfg, Bus: b, World: w, MaxRun: 45 * time.Minute, Settle: 45 * time.Second, ProgressEvery: 30 * time.Second}
}

// Thresholds of the phase 4 acceptance.
const (
	p4RecallPass = 0.80 // victims the cameras saw with a casualty record
	p4RecallWarn = 0.60
	p4DupPass    = 0.10 // extra casualty records per victim found
	p4DupWarn    = 0.25
	p4MergePass  = 0.05 // casualty records holding two victims
	p4MergeWarn  = 0.15
	p4PosPassM   = 4.0 // median casualty position error
	p4PosWarnM   = 6.0
	p4TriagePass = 0.70 // triage colour as the scenario expects (thresholds await medical sign-off)
	p4MoveRecall = 0.70
	p4MoveFalse  = 0.20
	p4AssignM    = 3.5 // a casualty record this close to a victim's body or exposed parts can be its record
	p4FalseWarn  = 0.25
	// The held height is judged from the height the drone reports, which
	// carries its GPS height error (about 0.75 m, correlated over 30 s): over a
	// 30 s hold the median wanders up to about 2.8 m (40,000 simulated holds),
	// so 3 m tells a wrong height from GPS noise.
	p4HeightTolM   = 3.0
	p4TrendSpanS   = 600 // trends are worked out over at least 10 minutes ([analyst.triage] trend_min_span)
	p4CoverageMinN = 15  // CEP coverage is judged from this many casualties up (fewer say little)
)

// p4collector adds casualty and task lists to the phase 3 collector.
type p4collector struct {
	*p3collector
	cas      *sarpb.CasualtyList
	casAt    time.Time
	lists    int
	merged   map[string]string // casualty id -> merged into
	needed   map[string]time.Time
	firstCas map[string]time.Time
	tasks    *sarpb.TaskList
	taskAt   time.Time
}

func newP4Collector(inc string) *p4collector {
	return &p4collector{p3collector: newCollector(inc), merged: map[string]string{}, needed: map[string]time.Time{}, firstCas: map[string]time.Time{}}
}

func (c *p4collector) onList(s bus.Sample) {
	var l sarpb.CasualtyList
	if msg.Unmarshal(s.Payload, &l) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start.IsZero() || s.Received.Before(c.start) {
		return
	}
	c.cas, c.casAt = &l, s.Received
	c.lists++
	for _, cs := range l.Casualties {
		if _, ok := c.firstCas[cs.CasualtyId]; !ok {
			c.firstCas[cs.CasualtyId] = s.Received
		}
		if _, ok := c.needed[cs.CasualtyId]; !ok && cs.NeedsInvestigation {
			c.needed[cs.CasualtyId] = s.Received
		}
	}
}

func (c *p4collector) onCasualty(s bus.Sample) {
	var cs sarpb.Casualty
	if msg.Unmarshal(s.Payload, &cs) != nil || cs.MergedInto == "" {
		return
	}
	c.mu.Lock()
	c.merged[cs.CasualtyId] = cs.MergedInto
	c.mu.Unlock()
}

func (c *p4collector) onTasks(s bus.Sample) {
	var l sarpb.TaskList
	if msg.Unmarshal(s.Payload, &l) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.start.IsZero() || s.Received.Before(c.start) {
		return
	}
	c.tasks, c.taskAt = &l, s.Received
}

// resolve follows merges.
func (c *p4collector) resolve(id string) string {
	for i := 0; i < 20; i++ {
		to, ok := c.merged[id]
		if !ok {
			return id
		}
		id = to
	}
	return id
}

type p4run struct {
	start, surveyDone, end time.Time
	timeLimited            bool
	surveyors              []string
	investigators          []string
	storedErr              error
	stored                 map[string]uint64
}

// Phase4 runs the phase 4 acceptance test: the scout surveys, casualty-analyst
// merges the reports into triaged casualty records, mission-planner sends the
// investigators to every casualty that needs a close look, and the result is
// compared with the scenario's ground truth.
func Phase4(ctx context.Context, o Phase4Options, out io.Writer) *Report {
	r := NewReport("phase4", out)
	r.Share = true
	cfg := o.Cfg
	inc := cfg.Site.IncidentID
	r.Context["incident"] = inc
	r.Context["drones"] = strings.Join(cfg.Check.ExpectUAVs, ",")
	prog := o.Progress
	if prog == nil {
		prog = io.Discard
	}
	names := map[string]string{"P4-01": "drones, casualty-analyst and mission-planner", "P4-02": "one casualty record per victim",
		"P4-03": "casualty positions", "P4-04": "uncertain casualties investigated", "P4-05": "investigations flown",
		"P4-06": "triage against ground truth", "P4-07": "movement seen close up", "P4-08": "radio budget and storage"}
	ids := []string{"P4-02", "P4-03", "P4-04", "P4-05", "P4-06", "P4-07", "P4-08"}
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
		{keys.CasualtyAll(inc), col.onCasualty}, {keys.Tasks(inc), col.onTasks}} {
		sub, err := o.Bus.Subscribe(s.key, s.h)
		if err != nil {
			r.Run("P4-01", names["P4-01"], func() Result {
				return Result{Status: Fail, Code: "E-CHK-411", Detail: "cannot subscribe to " + s.key, Evidence: []string{err.Error()}}
			})
			return r
		}
		subs = append(subs, sub)
	}
	ctl, err := newController(o.Bus, inc)
	if err != nil {
		r.Run("P4-01", names["P4-01"], func() Result {
			return Result{Status: Fail, Code: "E-CHK-411", Detail: "cannot open the control channel", Evidence: []string{err.Error()}}
		})
		return r
	}
	defer ctl.close()

	run := &p4run{}
	normal := map[string]uint32{}
	res := r.Run("P4-01", names["P4-01"], func() Result {
		return safe("E-CHK-411", func() Result { return p4Ready(ctx, o, col, run, normal) })
	})
	if res.Status == Fail {
		for _, id := range ids {
			r.Skip(id, names[id], "needs P4-01")
		}
		return r
	}
	if err := p4Drive(ctx, o, col, ctl, run, prog); err != nil {
		// A run that did not happen fails the acceptance (the checks after it are skipped).
		r.Run(ids[0], names[ids[0]], func() Result {
			return Result{Status: Fail, Code: "E-CHK-411", Detail: "the run could not be completed: " + err.Error(),
				Hint: "a reset error names the folder or service; 'casualty-analyst status', 'mission-planner status', 'journalctl -u casualty-analyst -n 30'"}
		})
		for _, id := range ids[1:] {
			r.Skip(id, names[id], "the run could not be completed")
		}
		return r
	}
	time.Sleep(2 * time.Second) // the last list and acknowledgements
	switch {
	case o.AnalystStored != nil:
		run.stored, run.storedErr = o.AnalystStored()
	case o.Services == nil:
		run.stored, run.storedErr = analystStored(cfg.Analyst.Listen)
	}
	col.mu.Lock()
	nCas := 0
	if col.cas != nil {
		nCas = len(col.cas.Casualties)
	}
	r.Context["run"] = fmt.Sprintf("%.0f s, %d reports, %d casualties, %d truth frames", run.end.Sub(run.start).Seconds(), len(col.order), nCas, col.truth.frames)
	col.mu.Unlock()
	sc := p4Score(col, o.World)
	r.Run("P4-02", names["P4-02"], func() Result { return safe("E-CHK-412", func() Result { return p4Records(sc) }) })
	r.Run("P4-03", names["P4-03"], func() Result { return safe("E-CHK-413", func() Result { return p4Positions(sc) }) })
	r.Run("P4-04", names["P4-04"], func() Result { return safe("E-CHK-414", func() Result { return p4Triggered(col, sc, run, o) }) })
	r.Run("P4-05", names["P4-05"], func() Result { return safe("E-CHK-415", func() Result { return p4Flown(col, sc, run, o) }) })
	r.Run("P4-06", names["P4-06"], func() Result { return safe("E-CHK-416", func() Result { return p4Triage(sc) }) })
	r.Run("P4-07", names["P4-07"], func() Result { return safe("E-CHK-417", func() Result { return p4Movement(sc) }) })
	r.Run("P4-08", names["P4-08"], func() Result { return safe("E-CHK-418", func() Result { return p4Link(col, run, normal, o) }) })
	return r
}

// --- Services ----------------------------------------------------------------

func getJSON(addr, path string, v any) (int, error) {
	cl := http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("http://" + addr + path)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return resp.StatusCode, fmt.Errorf("answered something else: %v", err)
	}
	return resp.StatusCode, nil
}

func postJSON(addr, path string) error {
	cl := http.Client{Timeout: 30 * time.Second}
	resp, err := cl.Post("http://"+addr+path, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d %v", addr, path, resp.StatusCode, doc)
	}
	return nil
}

// servicesUp: both services answer with their phase 4 status.
func servicesUp(cfg *config.ZBook) error {
	var a struct {
		State      string          `json:"state"`
		Casualties json.RawMessage `json:"casualties"`
	}
	code, err := getJSON(cfg.Analyst.Listen, "/v1/status", &a)
	switch {
	case err != nil:
		return fmt.Errorf("casualty-analyst status API at %s: %v", cfg.Analyst.Listen, err)
	case code != http.StatusOK:
		return fmt.Errorf("casualty-analyst is %s", a.State)
	case len(a.Casualties) == 0:
		return fmt.Errorf("casualty-analyst at %s is an older version without casualty records (install 0.5.0 or later)", cfg.Analyst.Listen)
	}
	var p struct {
		State string `json:"state"`
	}
	code, err = getJSON(cfg.Planner.Listen, "/v1/status", &p)
	switch {
	case err != nil:
		return fmt.Errorf("mission-planner status API at %s: %v", cfg.Planner.Listen, err)
	case code != http.StatusOK:
		return fmt.Errorf("mission-planner is %s", p.State)
	}
	return nil
}

func resetServices(cfg *config.ZBook, stamp string) error {
	if err := postJSON(cfg.Analyst.Listen, "/v1/reset?stamp="+stamp); err != nil {
		return fmt.Errorf("casualty-analyst reset: %v", err)
	}
	if err := postJSON(cfg.Planner.Listen, "/v1/reset"); err != nil {
		return fmt.Errorf("mission-planner reset: %v", err)
	}
	return nil
}

// --- P4-01 and the run ----------------------------------------------------------

func p4Ready(ctx context.Context, o Phase4Options, col *p4collector, run *p4run, normal map[string]uint32) Result {
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
			return Result{Status: Fail, Code: "E-CHK-411", Detail: "interrupted"}
		case <-time.After(250 * time.Millisecond):
		}
	}
	if len(missing) > 0 {
		return Result{Status: Fail, Code: "E-CHK-411", Detail: fmt.Sprintf("no telemetry from %s within %v", strings.Join(missing, ", "), cfg.Check.DiscoverTimeout.Duration),
			Hint: "On the Jetson: edge-eye must run the phase 4 build and config (scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config); check 'edge-eye status'."}
	}
	var ev, warns []string
	backend := ""
	for _, id := range cfg.Check.ExpectUAVs {
		t := col.latestTel(id)
		p := t.Perception
		normal[id] = t.GetLink().GetBudgetBps()
		role := strings.ToLower(strings.TrimPrefix(t.Role.String(), "UAV_ROLE_"))
		ev = append(ev, fmt.Sprintf("%s: %s, %s, mission %q (%d waypoints), cruise %.0f m/s, battery %.0f%%, budget %d kbit/s", id, role, p.Backend, p.MissionId, p.MissionSteps,
			t.CruiseMps, t.BatteryPct, normal[id]/1000))
		backend = p.Backend
		switch {
		case !p.Enabled || strings.HasPrefix(p.Backend, "not running"):
			return Result{Status: Fail, Code: "E-CHK-411", Detail: id + ": perception not running", Evidence: ev,
				Hint: "the Jetson cannot load the model: run scripts/jetson/80-engine.sh --from <model folder>, then 'sudo systemctl restart edge-eye'"}
		case strings.HasPrefix(p.Backend, "truth-oracle") && !o.AllowOracle:
			return Result{Status: Fail, Code: "E-CHK-411", Detail: id + ": the truth oracle is configured, not the model", Evidence: ev,
				Hint: "set [perception] detector = \"model\" in /etc/rubblesar/edge-eye.toml and restart edge-eye"}
		case t.Home == nil || t.CruiseMps == 0:
			return Result{Status: Fail, Code: "E-CHK-411", Detail: id + ": edge-eye is older than phase 4 (no home or cruise speed in its telemetry)", Evidence: ev,
				Hint: "install 0.5.0 or later on the Jetson (scripts/jetson/30-install.sh --zbook-ip <ZBook IP> --force-config)"}
		}
		switch t.Role {
		case sarpb.UavRole_UAV_ROLE_INVESTIGATOR:
			run.investigators = append(run.investigators, id)
			if p.MissionSteps > 0 {
				warns = append(warns, id+" also flies a survey (phase 4 config: behaviour = \"standby\")")
				run.surveyors = append(run.surveyors, id)
			}
		default:
			if p.MissionSteps > 0 {
				run.surveyors = append(run.surveyors, id)
			}
		}
	}
	if len(run.surveyors) == 0 {
		return Result{Status: Fail, Code: "E-CHK-411", Detail: "no drone flies a survey", Evidence: ev, Hint: "the scout needs behaviour = \"survey\" in /etc/rubblesar/edge-eye.toml"}
	}
	if len(run.investigators) == 0 {
		return Result{Status: Fail, Code: "E-CHK-411", Detail: "no investigator among " + strings.Join(cfg.Check.ExpectUAVs, ", "), Evidence: ev,
			Hint: "phase 4 needs drones with role = \"investigator\" and behaviour = \"standby\" (install the phase 4 Jetson config with --force-config)"}
	}
	check := o.Services
	if check == nil {
		check = func() error { return servicesUp(cfg) }
	}
	if err := check(); err != nil {
		return Result{Status: Fail, Code: "E-CHK-411", Detail: "casualty-analyst or mission-planner is not ready", Evidence: append(ev, err.Error()),
			Hint: "install the phase 4 ZBook release (scripts/zbook/30-install.sh); 'casualty-analyst status' and 'mission-planner status' must answer"}
	}
	detail := fmt.Sprintf("%d drones ready (survey: %s; investigators: %s), model %s, casualty-analyst and mission-planner running",
		len(cfg.Check.ExpectUAVs), strings.Join(run.surveyors, ", "), strings.Join(run.investigators, ", "), backend)
	if strings.HasPrefix(backend, "truth-oracle") {
		return Result{Status: Warn, Code: "E-CHK-411", Detail: detail + " (oracle allowed: tests fusion, tasking and triage, not the model)", Evidence: ev,
			Hint: "for the acceptance run set [perception] detector = \"model\" on the Jetson"}
	}
	if len(warns) > 0 {
		return Result{Status: Warn, Code: "E-CHK-411", Detail: detail + "; " + strings.Join(warns, "; "), Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

func taskCounts(l *sarpb.TaskList) (open, done, failed int, line string) {
	if l == nil {
		return 0, 0, 0, "no task list yet"
	}
	by := map[string]int{}
	for _, t := range l.Tasks {
		s := strings.ToLower(strings.TrimPrefix(t.State.String(), "TASK_STATE_"))
		by[s]++
		switch t.State {
		case sarpb.TaskState_TASK_STATE_QUEUED, sarpb.TaskState_TASK_STATE_ASSIGNED, sarpb.TaskState_TASK_STATE_EN_ROUTE, sarpb.TaskState_TASK_STATE_ON_STATION:
			open++
		case sarpb.TaskState_TASK_STATE_DONE:
			done++
		case sarpb.TaskState_TASK_STATE_FAILED:
			failed++
		}
	}
	var parts []string
	for _, k := range []string{"queued", "assigned", "en_route", "on_station", "done", "failed", "cancelled"} {
		if by[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", by[k], strings.ReplaceAll(k, "_", " ")))
		}
	}
	if len(parts) == 0 {
		return 0, 0, 0, "no tasks"
	}
	return open, done, failed, "tasks " + strings.Join(parts, ", ")
}

func casCounts(l *sarpb.CasualtyList) string {
	if l == nil {
		return "no casualty list yet"
	}
	if len(l.Casualties) == 0 {
		return "no casualties yet"
	}
	by := map[string]int{}
	prov := 0
	for _, c := range l.Casualties {
		by[casualty.TriageName(c.Triage)]++
		if c.Provisional {
			prov++
		}
	}
	var parts []string
	for _, k := range []string{"Red", "Yellow", "Black", "Grey"} {
		if by[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", by[k], k))
		}
	}
	return fmt.Sprintf("%d casualties (%s; %d provisional)", len(l.Casualties), strings.Join(parts, " "), prov)
}

// p4Drive resets the services, restarts the missions and waits until the
// survey is done, every task has finished and nothing is left to send.
func p4Drive(ctx context.Context, o Phase4Options, col *p4collector, ctl *controller, run *p4run, prog io.Writer) error {
	cfg := o.Cfg
	suite := o.Suite
	if suite == "" {
		suite = "phase4"
	}
	stamp := time.Now().UTC().Format("20060102T150405Z") + "-before-" + suite
	reset := o.Reset
	if reset == nil {
		reset = func(s string) error { return resetServices(cfg, s) }
	}
	if err := reset(stamp); err != nil {
		return err
	}
	col.mu.Lock()
	run.start = time.Now()
	col.start = run.start
	col.mu.Unlock()
	for _, id := range cfg.Check.ExpectUAVs {
		if _, _, err := ctl.send(id, func(c *sarpb.Control) {
			c.Cmd = &sarpb.Control_RestartMission{RestartMission: &sarpb.RestartMission{SimTimeS: -1, ClearTracks: true, ClearQueue: true}}
		}, cfg.Check.PingTimeout.Duration*3); err != nil {
			return fmt.Errorf("restart %s: %v", id, err)
		}
	}
	fmt.Fprintf(prog, "  run started: previous casualties archived (%s), missions restarted; up to %.0f min\n", stamp, o.MaxRun.Minutes())
	if o.StartedRun != nil {
		o.StartedRun()
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastProg := time.Now()
	quietSince := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		el := time.Since(run.start)
		surveyed, drained := true, true
		var line []string
		for _, id := range cfg.Check.ExpectUAVs {
			t := col.latestTel(id)
			if t == nil {
				drained = false
				continue
			}
			p, l := t.GetPerception(), t.GetLink()
			drained = drained && l.GetQueuedMsgs() == 0 && l.GetUnackedMsgs() == 0
			s := strings.ToLower(strings.TrimPrefix(t.State.String(), "FLIGHT_STATE_"))
			for _, sid := range run.surveyors {
				if sid == id {
					surveyed = surveyed && p.GetMissionDone()
					s = fmt.Sprintf("survey %d/%d", p.GetMissionStep(), p.GetMissionSteps())
					if p.GetMissionDone() {
						s = "survey done, " + strings.ToLower(strings.TrimPrefix(t.State.String(), "FLIGHT_STATE_"))
					}
				}
			}
			if ts := t.Task; ts != nil && (ts.State == sarpb.TaskState_TASK_STATE_EN_ROUTE || ts.State == sarpb.TaskState_TASK_STATE_ON_STATION) {
				s += fmt.Sprintf(" %s %s", ts.TaskId, strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(ts.State.String(), "TASK_STATE_")), "_", " "))
			}
			line = append(line, fmt.Sprintf("%s %s %.0f%% q%d", id, s, t.BatteryPct, l.GetQueuedMsgs()))
		}
		if surveyed && run.surveyDone.IsZero() {
			run.surveyDone = time.Now()
			fmt.Fprintf(prog, "  +%3.0f s  survey finished; waiting for the investigations\n", el.Seconds())
		}
		col.mu.Lock()
		open, _, _, tl := taskCounts(col.tasks)
		cl := casCounts(col.cas)
		col.mu.Unlock()
		if time.Since(lastProg) >= o.ProgressEvery && o.ProgressEvery > 0 {
			extra := ""
			if o.ExtraProgress != nil {
				extra = " | " + o.ExtraProgress()
			}
			fmt.Fprintf(prog, "  +%3.0f s  %s | %s | %s%s\n", el.Seconds(), cl, tl, strings.Join(line, " | "), extra)
			lastProg = time.Now()
		}
		if surveyed && drained && open == 0 {
			if quietSince.IsZero() {
				quietSince = time.Now()
			} else if time.Since(quietSince) >= o.Settle {
				break
			}
		} else {
			quietSince = time.Time{}
		}
		if el >= o.MaxRun {
			fmt.Fprintf(prog, "  +%3.0f s  stopping (time limit)\n", el.Seconds())
			run.timeLimited = true
			break
		}
	}
	run.end = time.Now()
	col.mu.Lock()
	cl := casCounts(col.cas)
	_, _, _, tl := taskCounts(col.tasks)
	col.mu.Unlock()
	fmt.Fprintf(prog, "  +%3.0f s  run complete: %s | %s\n", run.end.Sub(run.start).Seconds(), cl, tl)
	return nil
}

// --- Scoring against the scenario ----------------------------------------------

type p4victim struct {
	v        scenario.Victim
	e, n     float64 // centroid of the true exposed parts
	seenPx   int
	cas      []*sarpb.Casualty // its record first, then extra records
	best     *sarpb.Casualty   // its record (one to one)
	bestErrM float64
	absorbed string // no record of its own; this neighbour's record is within reach
}

type p4score struct {
	victims  []*p4victim
	byID     map[string]*p4victim
	seen     int
	cas      []*sarpb.Casualty
	owner    map[string]string // casualty id -> victim id (its own record or an extra one; "" = none)
	merged   []string
	falseCas []string
	closed   int // of falseCas: closed by the system itself (nothing found on its close looks)
	frames   int
}

// p4Score matches casualty records to victims one to one by the records'
// fused positions (nearest pairs first, within p4AssignM of the victim's
// body or exposed parts). A record left over near a victim is an extra
// record for it; one near no victim is false (a decoy, perhaps). A victim
// left without a record while another victim's record lies within reach was
// most likely merged into that record.
func p4Score(col *p4collector, w *vcam.World) *p4score {
	col.mu.Lock()
	defer col.mu.Unlock()
	sc := &p4score{byID: map[string]*p4victim{}, owner: map[string]string{}, frames: col.truth.frames}
	loc := w.Scn.Local()
	parts := truthParts(w)
	byVictim := map[string][]int{}
	for i, tp := range parts {
		byVictim[tp.VictimID] = append(byVictim[tp.VictimID], i)
	}
	for _, v := range w.Scn.Victims {
		pv := &p4victim{v: v, e: v.E, n: v.N, seenPx: col.truth.pixels[v.ID]}
		if ix := byVictim[v.ID]; len(ix) > 0 {
			pv.e, pv.n = 0, 0
			for _, i := range ix {
				pv.e, pv.n = pv.e+parts[i].E, pv.n+parts[i].N
			}
			pv.e, pv.n = pv.e/float64(len(ix)), pv.n/float64(len(ix))
		}
		if pv.seenPx >= 30 || col.truth.frames == 0 {
			sc.seen++
		}
		sc.victims = append(sc.victims, pv)
		sc.byID[v.ID] = pv
	}
	var decoys []render.Decoy
	for _, st := range w.Scn.Sites {
		decoys = append(decoys, w.Manifest(st.ID).Decoys...)
	}
	if col.cas == nil {
		return sc
	}
	sc.cas = col.cas.Casualties
	dist := func(pv *p4victim, e, n float64) float64 {
		return math.Min(math.Hypot(pv.v.E-e, pv.v.N-n), math.Hypot(pv.e-e, pv.n-n))
	}
	type pair struct {
		c *sarpb.Casualty
		v *p4victim
		d float64
	}
	pos := map[string][2]float64{}
	var pairs []pair
	for _, c := range sc.cas {
		ce, cn := loc.ToLocal(geoOf(c.Position))
		pos[c.CasualtyId] = [2]float64{ce, cn}
		// A warm spot with no body part, lying on a decoy, is the decoy.
		if partList(c) == "warm_spot" || len(c.Parts) == 0 {
			if d, ok := nearDecoy(decoys, ce, cn, 2.0); ok {
				sc.owner[c.CasualtyId] = ""
				sc.falseCas = append(sc.falseCas, fmt.Sprintf("%s (warm spot, %s) at a decoy (%s)", c.CasualtyId, casualty.TriageName(c.Triage), strings.ReplaceAll(d.Kind, "_", " ")))
				continue
			}
		}
		for _, pv := range sc.victims {
			if d := dist(pv, ce, cn); d <= p4AssignM {
				pairs = append(pairs, pair{c, pv, d})
			}
		}
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].d < pairs[j].d })
	for _, p := range pairs {
		if _, taken := sc.owner[p.c.CasualtyId]; taken || p.v.best != nil {
			continue
		}
		sc.owner[p.c.CasualtyId] = p.v.v.ID
		p.v.best, p.v.bestErrM = p.c, math.Hypot(p.v.e-pos[p.c.CasualtyId][0], p.v.n-pos[p.c.CasualtyId][1])
		p.v.cas = append(p.v.cas, p.c)
	}
	for _, c := range sc.cas {
		if _, ok := sc.owner[c.CasualtyId]; ok {
			continue
		}
		var near *p4victim
		best := math.Inf(1)
		for _, pv := range sc.victims {
			if d := dist(pv, pos[c.CasualtyId][0], pos[c.CasualtyId][1]); d < best && d <= p4AssignM {
				near, best = pv, d
			}
		}
		if near != nil {
			sc.owner[c.CasualtyId] = near.v.ID
			near.cas = append(near.cas, c)
			continue
		}
		sc.owner[c.CasualtyId] = ""
		what := fmt.Sprintf("%s (%s, %s)", c.CasualtyId, nonEmptyS(partList(c), "warm spot"), casualty.TriageName(c.Triage))
		if c.Status == sarpb.CasualtyStatus_CASUALTY_STATUS_NOT_FOUND {
			sc.closed++
			what += " closed: nothing found close up"
		}
		for _, d := range decoys {
			if math.Hypot(d.E-pos[c.CasualtyId][0], d.N-pos[c.CasualtyId][1]) <= 2.5 {
				what += " at a decoy (" + strings.ReplaceAll(d.Kind, "_", " ") + ")"
				break
			}
		}
		sc.falseCas = append(sc.falseCas, what)
	}
	for _, pv := range sc.victims {
		if pv.best != nil {
			continue
		}
		for _, c := range sc.cas {
			if o := sc.owner[c.CasualtyId]; o != "" && o != pv.v.ID && dist(pv, pos[c.CasualtyId][0], pos[c.CasualtyId][1]) <= p4AssignM {
				pv.absorbed = c.CasualtyId
				sc.merged = append(sc.merged, fmt.Sprintf("%s has no record of its own: probably merged into %s (%s's)", pv.v.ID, c.CasualtyId, o))
				break
			}
		}
	}
	return sc
}

func nearDecoy(decoys []render.Decoy, e, n, within float64) (render.Decoy, bool) {
	for _, d := range decoys {
		if math.Hypot(d.E-e, d.N-n) <= within {
			return d, true
		}
	}
	return render.Decoy{}, false
}

func nonEmptyS(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func partList(c *sarpb.Casualty) string {
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Parts {
		n := strings.ToLower(strings.TrimPrefix(p.Part.String(), "PART_CLASS_"))
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "+")
}

func p4Records(sc *p4score) Result {
	if len(sc.cas) == 0 {
		return Result{Status: Fail, Code: "E-CHK-412", Detail: "no casualty records at the end of the run",
			Hint: "'casualty-analyst status' on the ZBook shows the reports received; check 'journalctl -u casualty-analyst -n 50'"}
	}
	found, dups := 0, 0
	var missed, dupEv []string
	for _, pv := range sc.victims {
		if sc.frames > 0 && pv.seenPx < 30 {
			continue
		}
		if pv.best == nil {
			if pv.absorbed == "" {
				missed = append(missed, fmt.Sprintf("%s (%s, at most %d px visible)", pv.v.ID, pv.v.ExpectedTriage, pv.seenPx))
			}
			continue
		}
		found++
		if len(pv.cas) > 1 {
			dups += len(pv.cas) - 1
			var ids []string
			for _, c := range pv.cas {
				ids = append(ids, c.CasualtyId)
			}
			dupEv = append(dupEv, fmt.Sprintf("%s has %d records: %s", pv.v.ID, len(pv.cas), strings.Join(ids, ", ")))
		}
	}
	if sc.seen == 0 {
		return Result{Status: Warn, Code: "E-CHK-412", Detail: "no victim was in view during the run (ground-truth channel off?)"}
	}
	recall := float64(found) / float64(sc.seen)
	dupR := 0.0
	if found > 0 {
		dupR = float64(dups) / float64(found)
	}
	mergeR := float64(len(sc.merged)) / float64(max(sc.seen, 1))
	// Records the system itself closed (nothing found on its close looks) no
	// longer send anyone: they do not count as false alarms.
	falseR := float64(len(sc.falseCas)-sc.closed) / float64(len(sc.cas))
	detail := fmt.Sprintf("%d of %d victims the cameras saw have a casualty record (%.0f%%); %d extra records for the same victim; %d victims merged into a neighbour's record; %d records match no victim",
		found, sc.seen, recall*100, dups, len(sc.merged), len(sc.falseCas))
	if sc.closed > 0 {
		detail += fmt.Sprintf(" (%d of them closed: nothing found close up)", sc.closed)
	}
	ev := []string{fmt.Sprintf("targets: found >= %.0f%%, extra <= %.0f%% of victims found, merged <= %.0f%% of victims", p4RecallPass*100, p4DupPass*100, p4MergePass*100)}
	if len(missed) > 0 {
		ev = append(ev, "no record: "+strings.Join(missed, "; "))
	}
	ev = append(ev, dupEv...)
	ev = append(ev, sc.merged...)
	if len(sc.falseCas) > 0 {
		ev = append(ev, "matching no victim: "+strings.Join(limit(sc.falseCas, 12), "; "))
	}
	switch {
	case recall < p4RecallWarn || dupR > p4DupWarn || mergeR > p4MergeWarn:
		return Result{Status: Fail, Code: "E-CHK-412", Detail: detail, Evidence: ev}
	case recall < p4RecallPass || dupR > p4DupPass || mergeR > p4MergePass:
		return Result{Status: Warn, Code: "E-CHK-412", Detail: detail, Evidence: ev}
	case falseR > p4FalseWarn:
		return Result{Status: Warn, Code: "E-CHK-412", Detail: detail + fmt.Sprintf(" (over %.0f%% of the records: each costs an investigation)", p4FalseWarn*100), Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

func p4Positions(sc *p4score) Result {
	var errs []float64
	inCEP := 0
	var ev []string
	for _, pv := range sc.victims {
		if pv.best == nil {
			continue
		}
		errs = append(errs, pv.bestErrM)
		if pv.bestErrM <= float64(pv.best.CepM) {
			inCEP++
		}
	}
	if len(errs) == 0 {
		return Result{Status: Fail, Code: "E-CHK-413", Detail: "no casualty record matches a victim"}
	}
	m := pct(errs, 0.5)
	cov := float64(inCEP) / float64(len(errs))
	ev = append(ev, fmt.Sprintf("%d of %d (%.0f%%) lie within their CEP (50%% expected)", inCEP, len(errs), cov*100))
	sort.Float64s(errs)
	detail := fmt.Sprintf("%d casualties: median error %.2f m, worst %.2f m from the victim (target: median under %.0f m)", len(errs), m, errs[len(errs)-1], p4PosPassM)
	switch {
	case m > p4PosWarnM:
		return Result{Status: Fail, Code: "E-CHK-413", Detail: detail, Evidence: ev}
	case m > p4PosPassM || (len(errs) >= p4CoverageMinN && (cov < 0.25 || cov > 0.9)):
		if len(errs) >= p4CoverageMinN && (cov < 0.25 || cov > 0.9) {
			detail += fmt.Sprintf("; CEP coverage %.0f%% is far from 50%%", cov*100)
		}
		return Result{Status: Warn, Code: "E-CHK-413", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p4Triggered: every casualty that needed a close look got one (or used up
// its attempts), the most urgent first.
func p4Triggered(col *p4collector, sc *p4score, run *p4run, o Phase4Options) Result {
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.tasks == nil {
		st := Fail
		if len(sc.cas) == 0 {
			st = Warn
		}
		return Result{Status: st, Code: "E-CHK-414", Detail: "no task list from mission-planner during the run",
			Hint: "'mission-planner status' on the ZBook; 'journalctl -u mission-planner -n 50'"}
	}
	tasksOf := map[string][]*sarpb.Task{}
	for _, t := range col.tasks.Tasks {
		id := col.resolve(t.CasualtyId)
		tasksOf[id] = append(tasksOf[id], t)
	}
	needed, looked, pending := 0, 0, 0
	var notLooked []string
	var delay1, delay2 []float64
	for id, at := range col.needed {
		final := col.resolve(id)
		if final != id {
			continue // merged: counted under the record it joined
		}
		needed++
		var c *sarpb.Casualty
		for _, x := range sc.cas {
			if x.CasualtyId == id {
				c = x
			}
		}
		done := false
		var first *sarpb.Task
		for _, t := range tasksOf[id] {
			if t.State == sarpb.TaskState_TASK_STATE_DONE {
				done = true
			}
			if t.AssignedUnixNs > 0 && (first == nil || t.AssignedUnixNs < first.AssignedUnixNs) {
				first = t
			}
		}
		if c != nil && c.Investigations > 0 {
			done = true
		}
		if first != nil {
			d := time.Unix(0, first.AssignedUnixNs).Sub(at).Seconds()
			if first.Tier == 1 {
				delay1 = append(delay1, math.Max(0, d))
			} else {
				delay2 = append(delay2, math.Max(0, d))
			}
		}
		switch {
		case done:
			looked++
		case c == nil:
			looked++ // gone (merged into a record that was looked at)
		case !c.NeedsInvestigation:
			looked++ // settled without a task
		case len(tasksOf[id]) > 0:
			pending++
			notLooked = append(notLooked, fmt.Sprintf("%s: task %s %s", id, tasksOf[id][len(tasksOf[id])-1].TaskId,
				strings.ToLower(strings.TrimPrefix(tasksOf[id][len(tasksOf[id])-1].State.String(), "TASK_STATE_"))))
		default:
			notLooked = append(notLooked, fmt.Sprintf("%s (%s): no task", id, c.InvestigationReason))
		}
	}
	tiers := map[uint32]int{}
	for _, t := range col.tasks.Tasks {
		tiers[t.Tier]++
	}
	ev := []string{fmt.Sprintf("tasks by tier: %d provisional Red, %d unknown, %d position only", tiers[1], tiers[2], tiers[3])}
	if len(delay1) > 0 {
		ev = append(ev, fmt.Sprintf("provisional Red: a drone assigned %.0f s (median) after the record first needed a look", pct(delay1, 0.5)))
	}
	if len(delay2) > 0 {
		ev = append(ev, fmt.Sprintf("others: %.0f s (median)", pct(delay2, 0.5)))
	}
	ev = append(ev, notLooked...)
	if needed == 0 {
		return Result{Status: Warn, Code: "E-CHK-414", Detail: "no casualty needed a close look during the run", Evidence: ev}
	}
	detail := fmt.Sprintf("%d of %d casualties that needed a close look got one (%d tasks in all)", looked, needed, len(col.tasks.Tasks))
	switch {
	case looked == 0:
		return Result{Status: Fail, Code: "E-CHK-414", Detail: detail, Evidence: ev,
			Hint: "'mission-planner status' shows why tasks wait (battery, drones offline); paste the block"}
	case looked < needed:
		if run.timeLimited {
			detail += "; the run stopped at its time limit"
		}
		return Result{Status: Warn, Code: "E-CHK-414", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p4Flown: the investigators flew their tasks, held at the task height over
// the target for the hover time and saw something there.
func p4Flown(col *p4collector, sc *p4score, run *p4run, o Phase4Options) Result {
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.tasks == nil || len(col.tasks.Tasks) == 0 {
		return Result{Status: Warn, Code: "E-CHK-415", Detail: "no investigation tasks during the run"}
	}
	// Height on station from telemetry.
	heights := map[string][]float64{}
	for _, ts := range col.tel {
		for _, t := range ts {
			if tk := t.Task; tk != nil && tk.State == sarpb.TaskState_TASK_STATE_ON_STATION && tk.HoverElapsedS > 0 {
				heights[tk.TaskId] = append(heights[tk.TaskId], float64(t.AltAglM))
			}
		}
	}
	done, found, failed, low, aborted := 0, 0, 0, 0, 0
	var flight, hover []float64
	var ev []string
	for _, t := range col.tasks.Tasks {
		switch t.State {
		case sarpb.TaskState_TASK_STATE_DONE:
			done++
			if t.GetResult().GetFound() {
				found++
			}
			if t.OnStationUnixNs > 0 && t.AssignedUnixNs > 0 {
				flight = append(flight, float64(t.OnStationUnixNs-t.AssignedUnixNs)/1e9)
			}
			if t.FinishedUnixNs > 0 && t.OnStationUnixNs > 0 {
				hover = append(hover, float64(t.FinishedUnixNs-t.OnStationUnixNs)/1e9)
			}
			if h := heights[t.TaskId]; len(h) > 0 {
				if m := pct(h, 0.5); math.Abs(m-float64(t.AltAglM)) > p4HeightTolM {
					low++
					ev = append(ev, fmt.Sprintf("%s held at %.1f m instead of %.0f m", t.TaskId, m, t.AltAglM))
				}
			}
			owner := ""
			if cid := col.resolve(t.CasualtyId); cid != "" {
				owner = sc.owner[cid]
			}
			res := "nothing near the target"
			if t.GetResult().GetFound() {
				res = fmt.Sprintf("%d parts", len(t.GetResult().GetTrackIds()))
				if t.GetResult().GetMoved() {
					res += ", moved"
				}
			}
			ev = append(ev, fmt.Sprintf("%s %s -> %s (%s): %s, %d frames, %s", t.TaskId, t.UavId, t.CasualtyId, nonEmptyS(owner, "no victim"), "done", t.GetResult().GetFrames(), res))
		case sarpb.TaskState_TASK_STATE_FAILED:
			failed++
			ev = append(ev, fmt.Sprintf("%s -> %s: FAILED: %s", t.TaskId, t.CasualtyId, t.Note))
		case sarpb.TaskState_TASK_STATE_CANCELLED:
		default:
			aborted++
			ev = append(ev, fmt.Sprintf("%s -> %s: still %s at the end", t.TaskId, t.CasualtyId, strings.ToLower(strings.TrimPrefix(t.State.String(), "TASK_STATE_"))))
		}
	}
	head := fmt.Sprintf("flight to the target median %.0f s, on station median %.0f s", pct(flight, 0.5), pct(hover, 0.5))
	ev = append([]string{head}, limit(ev, 40)...)
	detail := fmt.Sprintf("%d investigations done (%d saw a body part near the target), %d failed, %d unfinished", done, found, failed, aborted)
	switch {
	case done == 0:
		return Result{Status: Fail, Code: "E-CHK-415", Detail: detail, Evidence: ev,
			Hint: "'edge-eye status' on the Jetson shows each drone's task; 'mission-planner status' on the ZBook shows why tasks failed"}
	case failed > 0 || low > 0 || (aborted > 0 && !run.timeLimited) || found*2 < done:
		return Result{Status: Warn, Code: "E-CHK-415", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p4Triage compares each victim's record with the colour the scenario expects.
func p4Triage(sc *p4score) Result {
	conf := map[string]map[string]int{}
	compared, right, prov := 0, 0, 0
	var wrong, trend []string
	for _, pv := range sc.victims {
		c := pv.best
		if c == nil {
			continue
		}
		exp := strings.ToLower(pv.v.ExpectedTriage)
		got := strings.ToLower(casualty.TriageName(c.Triage))
		if c.Provisional {
			prov++
		}
		if conf[exp] == nil {
			conf[exp] = map[string]int{}
		}
		conf[exp][got]++
		compared++
		if exp == got {
			right++
		} else {
			p := ""
			if c.Provisional {
				p = " (provisional)"
			}
			wrong = append(wrong, fmt.Sprintf("%s (%s) expected %s, got %s%s: %s", pv.v.ID, pv.v.Status, exp, got, p, c.Reason))
		}
		if th := c.Thermal; th != nil && th.TrendSpanS >= p4TrendSpanS {
			trend = append(trend, fmt.Sprintf("%s %s: %+.1f C/h over %.0f min", c.CasualtyId, pv.v.Status, th.TrendCPerH, th.TrendSpanS/60))
		}
	}
	if compared == 0 {
		return Result{Status: Warn, Code: "E-CHK-416", Detail: "no casualty record matches a victim: nothing to compare"}
	}
	acc := float64(right) / float64(compared)
	var ev []string
	for _, exp := range []string{"red", "yellow", "black", "unknown"} {
		row := conf[exp]
		if row == nil {
			continue
		}
		var cells []string
		for _, got := range []string{"red", "yellow", "black", "grey"} {
			if row[got] > 0 {
				cells = append(cells, fmt.Sprintf("%s %d", got, row[got]))
			}
		}
		ev = append(ev, fmt.Sprintf("expected %-6s -> %s", exp, strings.Join(cells, ", ")))
	}
	ev = append(ev, wrong...)
	if len(trend) > 0 {
		ev = append(ev, "skin temperature trend (reported only, not scored): "+strings.Join(limit(trend, 12), "; "))
	} else {
		ev = append(ev, "skin temperature trend: no victim's readings spanned 10 minutes (a trend needs readings that far apart)")
	}
	detail := fmt.Sprintf("%d of %d victims (%.0f%%) have the expected START colour; %d still provisional (target %.0f%%; thresholds await medical sign-off)",
		right, compared, acc*100, prov, p4TriagePass*100)
	if acc < p4TriagePass {
		return Result{Status: Warn, Code: "E-CHK-416", Detail: detail, Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p4Movement: responsive victims watched close up were seen moving; the others were not.
func p4Movement(sc *p4score) Result {
	mv, mvSeen, still, stillMoved := 0, 0, 0, 0
	var ev []string
	for _, pv := range sc.victims {
		c := pv.best
		if c == nil || c.Movement == nil || c.Movement.WatchedS <= 0 {
			continue
		}
		if pv.v.Movement.Moves {
			mv++
			if c.Movement.Moved {
				mvSeen++
			} else {
				ev = append(ev, fmt.Sprintf("%s moves but %s was still for %.0f s (%s)", pv.v.ID, c.CasualtyId, c.Movement.WatchedS, nonEmptyS(partList(c), "warm spot")))
			}
		} else {
			still++
			if c.Movement.Moved {
				stillMoved++
				ev = append(ev, fmt.Sprintf("%s is still but %s was seen moving (%s)", pv.v.ID, c.CasualtyId, nonEmptyS(partList(c), "warm spot")))
			}
		}
	}
	if mv+still == 0 {
		return Result{Status: Warn, Code: "E-CHK-417", Detail: "no victim was watched close up"}
	}
	detail := fmt.Sprintf("moving victims seen moving: %d of %d; still victims seen moving: %d of %d", mvSeen, mv, stillMoved, still)
	rec, fal := 1.0, 0.0
	if mv > 0 {
		rec = float64(mvSeen) / float64(mv)
	}
	if still > 0 {
		fal = float64(stillMoved) / float64(still)
	}
	if rec < p4MoveRecall || fal > p4MoveFalse {
		return Result{Status: Warn, Code: "E-CHK-417", Detail: detail + fmt.Sprintf(" (targets: %.0f%% and at most %.0f%%)", p4MoveRecall*100, p4MoveFalse*100), Evidence: ev}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// p4Link: the drones kept to their radio budgets while hovering and sending
// crops, no report went missing, and casualty-analyst stored all that arrived.
func p4Link(col *p4collector, run *p4run, normal map[string]uint32, o Phase4Options) Result {
	br := p3Budget(col.p3collector, &p3run{start: run.start, end: run.end}, normal, Phase3Options{Cfg: o.Cfg})
	col.mu.Lock()
	defer col.mu.Unlock()
	ev := br.Evidence
	lost := 0
	arrived := map[string]int{}
	for _, dr := range col.dets {
		if !dr.at.Before(run.start) {
			arrived[dr.d.UavId]++
		}
	}
	for _, id := range o.Cfg.Check.ExpectUAVs {
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
		line := fmt.Sprintf("%s: %d reports, %d missing", id, arrived[id], miss)
		if run.storedErr == nil && run.stored != nil {
			line += fmt.Sprintf(", casualty-analyst stored %d", run.stored[id])
		}
		ev = append(ev, line)
	}
	notStored := 0
	if run.storedErr == nil && run.stored != nil {
		for id, n := range arrived {
			if int(run.stored[id]) < n {
				notStored += n - int(run.stored[id])
			}
		}
	}
	detail := br.Detail + fmt.Sprintf("; %d reports missing", lost)
	switch {
	case br.Status == Fail:
		return Result{Status: Fail, Code: "E-CHK-418", Detail: detail, Evidence: ev}
	case lost > 0 || notStored > 0:
		if notStored > 0 {
			detail += fmt.Sprintf("; %d arrived but were not stored", notStored)
		}
		return Result{Status: Fail, Code: "E-CHK-418", Detail: detail, Evidence: ev}
	case run.storedErr != nil:
		return Result{Status: Warn, Code: "E-CHK-418", Detail: detail + "; not compared with casualty-analyst's storage", Evidence: append(ev, run.storedErr.Error())}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}
