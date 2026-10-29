package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/bus"
	"rubblesar/internal/casualty"
	"rubblesar/internal/check"
	"rubblesar/internal/dispatch"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/version"
)

// UpOptions for Up.
type UpOptions struct {
	NoJetson bool
	// OpenBus connects to zenohd once it runs (for waiting on the drones).
	OpenBus func(ctx context.Context) (bus.Bus, error)
	// Wait: how long to wait for the services and then the drones (default 60 s).
	Wait time.Duration
}

// Up starts the ZBook's services in order, then the Jetson's edge-eye, and waits until they answer.
func Up(ctx context.Context, e *Env, out io.Writer, o UpOptions) error {
	if o.Wait <= 0 {
		o.Wait = 60 * time.Second
	}
	fmt.Fprintf(out, "==> starting the ZBook's services: %s\n", strings.Join(ZBookServices, ", "))
	if err := e.Sys.Systemctl(ctx, "start", ZBookServices...); err != nil {
		return fmt.Errorf("E-OPS-001: %w", err)
	}
	deadline := time.Now().Add(o.Wait)
	for _, s := range ZBookServices {
		for {
			st := &Status{}
			r := e.probe(ctx, s, st)
			if r.Level != Fail {
				fmt.Fprintf(out, "    ok: %-18s %s\n", s, r.Detail)
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				return fmt.Errorf("E-OPS-001: %s did not come up: %s (%s)", s, r.Detail, r.Fix)
			}
			time.Sleep(time.Second)
		}
	}
	if j := e.Cfg.Ops.Jetson; j != "" && !o.NoJetson {
		fmt.Fprintf(out, "==> starting %s on the Jetson (%s)\n", e.Cfg.Ops.JetsonService, j)
		if _, err := e.Sys.SSH(ctx, j, "sudo -n systemctl start "+e.Cfg.Ops.JetsonService); err != nil {
			fmt.Fprintf(out, "    WARNING: %v\n    fix: scripts/zbook/38-jetson-link.sh --jetson %s (key and sudo rule), or on the Jetson: sudo systemctl start %s\n", err, j, e.Cfg.Ops.JetsonService)
		} else {
			fmt.Fprintf(out, "    ok: started\n")
		}
	} else if j == "" {
		fmt.Fprintf(out, "==> the Jetson: [ops] jetson is not set, so start edge-eye there yourself if it is not running\n    (or once: scripts/zbook/38-jetson-link.sh --jetson USER@JETSON-IP)\n")
	}
	if o.OpenBus != nil && e.Bus == nil {
		b, err := o.OpenBus(ctx)
		if err != nil {
			return fmt.Errorf("E-OPS-001: connecting to zenohd: %w", err)
		}
		e.Bus = b
	}
	if e.Bus != nil {
		fmt.Fprintf(out, "==> waiting for the drones' telemetry: %s\n", strings.Join(e.Cfg.Check.ExpectUAVs, ", "))
		tel := Telemetry(ctx, e.Bus, e.Cfg.Site.IncidentID, e.Cfg.Check.ExpectUAVs, o.Wait)
		var missing []string
		for _, id := range e.Cfg.Check.ExpectUAVs {
			if tel[id] == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(out, "    WARNING: no telemetry from %s: is edge-eye running on the Jetson?\n", strings.Join(missing, ", "))
		} else {
			fmt.Fprintf(out, "    ok: all %d drones report\n", len(tel))
		}
	}
	return nil
}

// DownOptions for Down.
type DownOptions struct {
	NoJetson, KeepTAK bool
}

// Down stops the Jetson's edge-eye, then the ZBook's services in reverse order.
func Down(ctx context.Context, e *Env, out io.Writer, o DownOptions) error {
	if j := e.Cfg.Ops.Jetson; j != "" && !o.NoJetson {
		fmt.Fprintf(out, "==> stopping %s on the Jetson (%s)\n", e.Cfg.Ops.JetsonService, j)
		if _, err := e.Sys.SSH(ctx, j, "sudo -n systemctl stop "+e.Cfg.Ops.JetsonService); err != nil {
			fmt.Fprintf(out, "    WARNING: %v (stop it on the Jetson: sudo systemctl stop %s)\n", err, e.Cfg.Ops.JetsonService)
		} else {
			fmt.Fprintf(out, "    ok: stopped\n")
		}
	}
	var units []string
	for i := len(ZBookServices) - 1; i >= 0; i-- {
		s := ZBookServices[i]
		if o.KeepTAK && s == "rubblesar-taky" {
			continue
		}
		units = append(units, s)
	}
	fmt.Fprintf(out, "==> stopping the ZBook's services: %s\n", strings.Join(units, ", "))
	if err := e.Sys.Systemctl(ctx, "stop", units...); err != nil {
		return fmt.Errorf("E-OPS-001: %w", err)
	}
	fmt.Fprintf(out, "    ok: stopped (they start again at boot; 'rubblesar up' starts them now)\n")
	return nil
}

// MissionOptions for Mission.
type MissionOptions struct {
	// Max run time (default [ops] max_mission).
	Max time.Duration
	// Settle: how long everything must stay finished before the run ends (default 30 s).
	Settle time.Duration
	// MinRun: a run is not over before this (default 1 min: the survey has to start).
	MinRun time.Duration
	// Progress every this long (default 30 s).
	Every time.Duration
	// NoReset continues with the casualties of the last run (no survey restart).
	NoReset bool
	// Scenario folder (default [site] scenario_dir), for the report's comparison.
	ScenarioDir string
	// Restart restarts the drones' missions (default check.RestartMissions).
	Restart func(b bus.Bus, incident string, uavs []string, timeout time.Duration) error
	// Reset resets casualty-analyst and mission-planner (default check.ResetServices).
	Reset func(stamp string) error
	// DispatchReset resets rescue-dispatch (default POST /v1/reset).
	DispatchReset func() error
	// Chats reads rescue-dispatch's chat (default GET /v1/chats).
	Chats func() ([]aar.ChatLine, error)
	// Software lists the versions in use (for the report).
	Software []string
	Suite    string
	// OnRecording is told when the run starts (the services reset, the
	// drones' missions restarted), with its recorder: the demo director marks
	// beats on it (phase 8). OnEnd is told when the run is over, before the
	// recording is closed.
	OnRecording func(rec *aar.Recorder, started time.Time)
	OnEnd       func()
}

// MissionResult is where the run's report is.
type MissionResult struct {
	Dir     string
	Report  *aar.Report
	Reason  string
	Partial bool
}

// watch follows a run on the bus.
type watch struct {
	mu        sync.Mutex
	cas       *sarpb.CasualtyList
	tasks     *sarpb.TaskList
	disp      *sarpb.DispatchState
	tel       map[string]*sarpb.Telemetry
	casAt     time.Time
	listsSeen int
}

func (w *watch) subscribe(b bus.Bus, inc string) ([]bus.Subscription, error) {
	var subs []bus.Subscription
	for _, x := range []struct {
		key string
		h   bus.Handler
	}{
		{keys.Casualties(inc), func(s bus.Sample) {
			var l sarpb.CasualtyList
			if msg.Unmarshal(s.Payload, &l) == nil {
				w.mu.Lock()
				w.cas, w.casAt = &l, time.Now()
				w.listsSeen++
				w.mu.Unlock()
			}
		}},
		{keys.Tasks(inc), func(s bus.Sample) {
			var l sarpb.TaskList
			if msg.Unmarshal(s.Payload, &l) == nil {
				w.mu.Lock()
				w.tasks = &l
				w.mu.Unlock()
			}
		}},
		{keys.Dispatch(inc), func(s bus.Sample) {
			var d sarpb.DispatchState
			if msg.Unmarshal(s.Payload, &d) == nil {
				w.mu.Lock()
				w.disp = &d
				w.mu.Unlock()
			}
		}},
		{keys.TelemetryAll(inc), func(s bus.Sample) {
			var t sarpb.Telemetry
			if msg.Unmarshal(s.Payload, &t) == nil && t.UavId != "" {
				w.mu.Lock()
				w.tel[t.UavId] = &t
				w.mu.Unlock()
			}
		}},
	} {
		sub, err := b.Subscribe(x.key, x.h)
		if err != nil {
			for _, s := range subs {
				_ = s.Close()
			}
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

// line is the progress line and whether everything is finished.
func (w *watch) line(started time.Time) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var parts []string
	finished := w.listsSeen > 0 && w.casAt.After(started)
	// Casualties.
	if w.cas != nil {
		n := map[string]int{}
		for _, c := range w.cas.Casualties {
			n[casualty.TriageName(c.Triage)]++
		}
		var t []string
		for _, k := range []string{"Red", "Yellow", "Black", "Grey"} {
			if n[k] > 0 {
				t = append(t, fmt.Sprintf("%d %s", n[k], k))
			}
		}
		parts = append(parts, fmt.Sprintf("%d casualties (%s)", len(w.cas.Casualties), strings.Join(t, " ")))
	} else {
		parts = append(parts, "no casualties yet")
	}
	// Close looks.
	open := 0
	if w.tasks != nil {
		for _, t := range w.tasks.Tasks {
			switch t.State {
			case sarpb.TaskState_TASK_STATE_QUEUED, sarpb.TaskState_TASK_STATE_ASSIGNED, sarpb.TaskState_TASK_STATE_EN_ROUTE, sarpb.TaskState_TASK_STATE_ON_STATION:
				open++
			}
		}
	}
	parts = append(parts, fmt.Sprintf("%d close looks open", open))
	finished = finished && open == 0
	// Drones.
	ids := make([]string, 0, len(w.tel))
	for id := range w.tel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var dr []string
	for _, id := range ids {
		t := w.tel[id]
		p := t.GetPerception()
		s := strings.ToLower(strings.TrimPrefix(t.State.String(), "FLIGHT_STATE_"))
		if p.GetMissionSteps() > 0 {
			s = fmt.Sprintf("survey %d/%d", p.GetMissionStep(), p.GetMissionSteps())
			if p.GetMissionDone() {
				s = "survey done"
			} else {
				finished = false
			}
		}
		dr = append(dr, fmt.Sprintf("%s %s %.0f%%", id, s, t.BatteryPct))
	}
	parts = append(parts, strings.Join(dr, ", "))
	// Rescue.
	if d := w.disp; d != nil {
		active, done := 0, 0
		for _, a := range d.Assignments {
			switch a.State {
			case sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE, sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
				active++
			case sarpb.AssignmentState_ASSIGNMENT_STATE_DONE:
				done++
			}
		}
		parts = append(parts, fmt.Sprintf("rescue: %d active, %d done, %d waiting", active, done, len(d.Waiting)))
		finished = finished && active == 0 && len(d.Waiting) == 0
	}
	return strings.Join(parts, " | "), finished
}

// Mission runs a mission: resets the services, restarts the drones' missions,
// records everything until the run is finished (or the time limit, or ctx
// ends) and writes the after-action report.
func Mission(ctx context.Context, e *Env, out io.Writer, o MissionOptions) (*MissionResult, error) {
	cfg := e.Cfg
	if e.Bus == nil {
		return nil, errors.New("E-OPS-002: no connection to zenohd (rubblesar up)")
	}
	if o.Max <= 0 {
		o.Max = cfg.Ops.MaxMission.Duration
	}
	if o.Settle <= 0 {
		o.Settle = 30 * time.Second
	}
	if o.MinRun <= 0 {
		o.MinRun = time.Minute
	}
	if o.Every <= 0 {
		o.Every = 30 * time.Second
	}
	if o.Suite == "" {
		o.Suite = "mission"
	}
	if o.Restart == nil {
		o.Restart = check.RestartMissions
	}
	if o.Reset == nil {
		o.Reset = func(stamp string) error { return check.ResetServices(cfg, stamp) }
	}
	if o.DispatchReset == nil {
		o.DispatchReset = func() error { return e.post(cfg.Dispatch.Listen, "/v1/reset") }
	}
	if o.Chats == nil {
		o.Chats = func() ([]aar.ChatLine, error) {
			var lines []aar.ChatLine
			err := e.getJSON(cfg.Dispatch.Listen, "/v1/chats", &lines)
			return lines, err
		}
	}
	if o.ScenarioDir == "" {
		o.ScenarioDir = cfg.Site.Scenario()
	}
	// Ready?
	if err := check.ServicesUp(cfg); err != nil {
		return nil, fmt.Errorf("E-OPS-002: %v (rubblesar up)", err)
	}
	tel := Telemetry(ctx, e.Bus, cfg.Site.IncidentID, cfg.Check.ExpectUAVs, 10*time.Second)
	var ids []string
	for _, id := range cfg.Check.ExpectUAVs {
		if tel[id] != nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("E-OPS-002: no drone reports (expected %s): is edge-eye running on the Jetson?", strings.Join(cfg.Check.ExpectUAVs, ", "))
	}
	if len(ids) < len(cfg.Check.ExpectUAVs) {
		fmt.Fprintf(out, "    WARNING: only %s report; the mission goes ahead with them\n", strings.Join(ids, ", "))
	}
	software := append([]string{version.Short("rubblesar")}, o.Software...)
	for _, id := range ids {
		if sw := tel[id].Software; sw != "" {
			software = append(software, id+": "+sw)
		}
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(cfg.ReportsDir(), stamp)
	host, _ := os.Hostname()
	var teams []string
	for _, t := range cfg.Dispatch.Teams {
		teams = append(teams, fmt.Sprintf("%s (%s, %s)", t.Callsign, t.Mode, t.Source))
	}
	lat, lon := cfg.Staging()
	meta := aar.Meta{Suite: o.Suite, Host: host, Software: software, TimeScale: cfg.Dispatch.TimeScale, RescueTimeScale: cfg.Dispatch.RescueTimeScale,
		Teams: teams, Scenario: filepath.Base(o.ScenarioDir), Staging: [2]float64{lat, lon}}
	// A new run: reset first, then record, then start the drones.
	if !o.NoReset {
		fmt.Fprintf(out, "==> new run %s: archiving the last run's casualties, clearing tasks and teams\n", stamp)
		if err := o.Reset(stamp + "-before-mission"); err != nil {
			return nil, fmt.Errorf("E-OPS-002: %w", err)
		}
		if err := o.DispatchReset(); err != nil {
			return nil, fmt.Errorf("E-OPS-002: resetting rescue-dispatch: %w", err)
		}
	}
	var ds struct {
		Dispatch dispatch.Status `json:"dispatch"`
	}
	if err := e.getJSON(cfg.Dispatch.Listen, "/v1/status", &ds); err == nil {
		meta.Run = ds.Dispatch.Run
	}
	rec, err := aar.Start(aar.Options{Bus: e.Bus, Incident: cfg.Site.IncidentID, Dir: dir, Meta: meta, Chats: o.Chats})
	if err != nil {
		return nil, err
	}
	w := &watch{tel: map[string]*sarpb.Telemetry{}}
	subs, err := w.subscribe(e.Bus, cfg.Site.IncidentID)
	if err != nil {
		_ = rec.Close()
		return nil, err
	}
	defer func() {
		for _, s := range subs {
			_ = s.Close()
		}
	}()
	started := time.Now()
	if !o.NoReset {
		if err := o.Restart(e.Bus, cfg.Site.IncidentID, ids, 3*cfg.Check.PingTimeout.Duration); err != nil {
			_ = rec.Close()
			return nil, fmt.Errorf("E-OPS-002: %w", err)
		}
		fmt.Fprintf(out, "    ok: missions restarted on %s; recording to %s\n", strings.Join(ids, ", "), dir)
	}
	if o.OnRecording != nil {
		o.OnRecording(rec, started)
	}
	fmt.Fprintf(out, "==> mission running (up to %s; Ctrl-C ends it early and still writes the report)\n", o.Max)
	res := &MissionResult{Dir: dir}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastProg := time.Now()
	var quiet time.Time
loop:
	for {
		select {
		case <-ctx.Done():
			res.Reason, res.Partial = "stopped by you", true
			break loop
		case <-tick.C:
		}
		el := time.Since(started)
		line, finished := w.line(started)
		if time.Since(lastProg) >= o.Every {
			fmt.Fprintf(out, "  +%s  %s\n", hm(el), line)
			lastProg = time.Now()
		}
		if finished && el > o.MinRun {
			if quiet.IsZero() {
				quiet = time.Now()
			} else if time.Since(quiet) >= o.Settle {
				res.Reason = "finished: survey done, every close look and every rescue finished"
				break
			}
		} else {
			quiet = time.Time{}
		}
		if el >= o.Max {
			res.Reason, res.Partial = fmt.Sprintf("time limit (%s)", o.Max), true
			break
		}
	}
	line, _ := w.line(started)
	fmt.Fprintf(out, "  +%s  %s\n==> run over: %s\n", hm(time.Since(started)), line, res.Reason)
	if o.OnEnd != nil {
		o.OnEnd()
	}
	rec.Note("run over: " + res.Reason)
	if err := rec.Close(); err != nil {
		return res, err
	}
	rep, err := WriteReport(cfg.MapDir(), o.ScenarioDir, dir, cfg.Dispatch.Triage)
	if err != nil {
		return res, err
	}
	res.Report = rep
	return res, nil
}

// WriteReport builds the after-action report of a recorded run folder.
func WriteReport(mapDir, scenarioDir, dir string, triage []string) (*aar.Report, error) {
	return aar.WriteReport(mapDir, scenarioDir, dir, triage)
}

// Runs lists the recorded runs (newest first).
func Runs(reportsDir string) []string {
	ents, _ := os.ReadDir(reportsDir)
	var out []string
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join(reportsDir, e.Name(), "events.jsonl")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}

// ReportURL is where sar-web shows a run's report.
func ReportURL(e *Env, dir string) string {
	_, port := splitPort(e.Cfg.Web.Listen)
	return fmt.Sprintf("http://%s:%s/reports/%s/%s/", firstAddress(), port, e.Cfg.Site.IncidentID, filepath.Base(dir))
}

func splitPort(listen string) (string, string) {
	i := strings.LastIndex(listen, ":")
	if i < 0 {
		return listen, ""
	}
	return listen[:i], listen[i+1:]
}

func hm(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}
