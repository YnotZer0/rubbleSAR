// Command sar-monitor shows live drone telemetry arriving on the ZBook and,
// from phase 3, each drone's perception pipeline, detection queue and the
// latest detection reports; from phase 4, the casualties (casualty-analyst)
// and the investigation tasks (mission-planner).
//
//	sar-monitor [--config FILE]            refreshing table (Ctrl+C to stop)
//	sar-monitor --once 10s                 collect for 10 s, print once, exit 1 if nothing seen
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/bus/zenoh"
	"rubblesar/internal/casualty"
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/keys"
	"rubblesar/internal/linkbudget"
	"rubblesar/internal/logx"
	"rubblesar/internal/monitor"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/version"
)

const tool = "sar-monitor"

func main() {
	path := flag.String("config", cli.DefaultConfig("/etc/rubblesar/sar.toml", "configs/zbook/sar.toml"), "config file")
	once := flag.Duration("once", 0, "collect for this long, print one table and exit")
	ver := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *ver {
		fmt.Println(version.String(tool))
		return
	}
	cfg, err := config.LoadZBook(*path)
	if err != nil {
		cli.Fatal(tool, err, "config: "+*path)
	}
	log := logx.New(tool, cfg.Log.Level, cfg.Log.Format)
	ctx, stop := cli.SignalContext()
	defer stop()

	b, err := bus.OpenRetry(ctx, log, cfg.Zenoh.OpenTimeout.Duration, false, zenoh.Opener(cfg.Zenoh))
	if err != nil {
		cli.Fatal(tool, err, "config: "+*path, "connect: "+cfg.Zenoh.Target())
	}
	defer b.Close()
	inc := cfg.Site.IncidentID
	tr := monitor.NewTracker(inc, geo.Point{Lat: cfg.Site.CenterLat, Lon: cfg.Site.CenterLon})
	sub, err := b.Subscribe(keys.TelemetryAll(inc), tr.Handle)
	if err != nil {
		cli.Fatal(tool, err)
	}
	defer sub.Close()
	dl := monitor.NewDetectionLog(inc, 8)
	dsub, err := b.Subscribe(keys.DetectionAll(inc), dl.Handle)
	if err != nil {
		cli.Fatal(tool, err)
	}
	defer dsub.Close()
	board := monitor.NewBoard(inc)
	for _, k := range []struct {
		key string
		h   bus.Handler
	}{{keys.Casualties(inc), board.HandleCasualties}, {keys.Tasks(inc), board.HandleTasks}, {keys.Dispatch(inc), board.HandleDispatch}} {
		s, err := b.Subscribe(k.key, k.h)
		if err != nil {
			cli.Fatal(tool, err)
		}
		defer s.Close()
	}

	header := fmt.Sprintf("RubbleSAR monitor | incident %s | %s %s -> %s", inc, b.Info().Backend, cfg.Zenoh.Mode, cfg.Zenoh.Target())
	if *once > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(*once):
		}
		s := tr.Snapshot()
		render(os.Stdout, header, tr, s, cfg.Monitor.StaleAfter.Duration, *once)
		renderPhase3(os.Stdout, s, dl)
		renderPhase4(os.Stdout, board, s.Taken)
		renderPhase5(os.Stdout, board, s.Taken)
		if len(s.UAVs) == 0 {
			fmt.Fprintf(os.Stderr, "\nNo telemetry seen on %s in %v (E-CHK-002). Is edge-eye running on the Jetson and connected to this zenohd?\n", keys.TelemetryAll(inc), *once)
			os.Exit(cli.ExitFail)
		}
		return
	}
	tty := false
	if fi, err := os.Stdout.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	t := time.NewTicker(cfg.Monitor.Refresh.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if tty {
			fmt.Print("\033[H\033[2J")
		}
		snap := tr.Snapshot()
		render(os.Stdout, header, tr, snap, cfg.Monitor.StaleAfter.Duration, 10*time.Second)
		renderPhase3(os.Stdout, snap, dl)
		renderPhase4(os.Stdout, board, snap.Taken)
		renderPhase5(os.Stdout, board, snap.Taken)
		if !tty {
			fmt.Println()
		}
	}
}

func render(w io.Writer, header string, tr *monitor.Tracker, s monitor.Snapshot, stale, window time.Duration) {
	fmt.Fprintf(w, "%s | %s\n", header, s.Taken.UTC().Format("2006-01-02 15:04:05Z"))
	fmt.Fprintf(w, "%-8s %-12s %-11s %10s %10s %5s %5s %6s %6s %6s %5s %7s %10s %7s %6s %6s\n",
		"UAV", "ROLE", "STATE", "LAT", "LON", "AGL", "BATT", "DIST", "RATE", "AGE", "GAPS", "LAT ms", "BUDGET", "USED", "DROP", "SEQ")
	if len(s.UAVs) == 0 {
		fmt.Fprintln(w, "  (no telemetry yet)")
	}
	for _, u := range s.UAVs {
		t := u.Latest
		if t == nil {
			continue
		}
		age := s.Taken.Sub(u.LastSeen)
		flag := ""
		if age > stale {
			flag = "  STALE"
		}
		bud := "unlim"
		if b := t.GetLink().GetBudgetBps(); b > 0 {
			bud = fmt.Sprintf("%dk", linkbudget.Kbps(b))
			if b < 1000 {
				bud = fmt.Sprintf("%db", b)
			}
		}
		role := strings.TrimPrefix(strings.ToLower(t.Role.String()), "uav_role_")
		state := strings.TrimPrefix(strings.ToLower(t.State.String()), "flight_state_")
		fmt.Fprintf(w, "%-8s %-12s %-11s %10.5f %10.5f %4.0fm %4.0f%% %5.0fm %5.2fHz %5.1fs %5d %7.1f %10s %5.1fk %6d %6d%s\n",
			u.ID, role, state, t.Position.GetLatDeg(), t.Position.GetLonDeg(), t.AltAglM, t.BatteryPct,
			tr.DistanceFromCenter(u), u.RateHz(s.Taken, window), age.Seconds(), u.Gaps, u.MedianLatencyMs(),
			bud, float64(t.GetLink().GetUsedBps())/1000, t.GetLink().GetDroppedMsgs(), u.LastSeq, flag)
	}
	if s.DecodeErr > 0 || s.Mismatch > 0 {
		fmt.Fprintf(w, "WARNING: %d undecodable and %d rejected messages; last error: %v\n", s.DecodeErr, s.Mismatch, s.LastErr)
	}
}

// renderPhase3 shows each drone's pipeline and queue (from telemetry) and the
// latest detection reports; nothing for drones without perception.
func renderPhase3(w io.Writer, s monitor.Snapshot, dl *monitor.DetectionLog) {
	counts, latest := dl.Snapshot()
	any := false
	for _, u := range s.UAVs {
		if u.Latest != nil && u.Latest.Perception != nil {
			any = true
		}
	}
	if !any && len(latest) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%-8s %-10s %6s %6s %8s %7s %7s %7s %6s %6s %6s %5s %-18s\n",
		"UAV", "MISSION", "FRAMES", "FPS", "PIPE ms", "TRACKS", "REPORTS", "RECEIVED", "QUEUE", "UNACK", "ACKED", "LEVEL", "LINK")
	for _, u := range s.UAVs {
		t := u.Latest
		if t == nil || t.Perception == nil {
			continue
		}
		p, l := t.Perception, t.GetLink()
		mis := "-"
		if p.MissionSteps > 0 {
			mis = fmt.Sprintf("%d/%d", p.MissionStep, p.MissionSteps)
			if p.MissionDone {
				mis = "done"
			}
		}
		link := "ok"
		switch {
		case l.GetOutage():
			link = "OUTAGE (simulated)"
		case l.GetProbing():
			link = "no acks: probing"
		case !p.Enabled:
			link = "perception off"
		}
		c := counts[u.ID]
		fmt.Fprintf(w, "%-8s %-10s %6d %6.2f %8.0f %7d %7d %7d %6d %6d %6d %5d %-18s\n", u.ID, mis, p.Frames, p.FrameHzActual,
			p.SegmentMs+p.ThermalMs+p.TrackMs+p.PackageMs, p.Tracks, p.Reports, c.Packets, l.GetQueuedMsgs(), l.GetUnackedMsgs(), l.GetAckedMsgs(), l.GetDegradeLevel(), link)
	}
	for _, u := range s.UAVs {
		if t := u.Latest; t != nil && t.Perception != nil && t.Perception.Backend != "" {
			fmt.Fprintf(w, "perception: %s\n", t.Perception.Backend)
			break
		}
	}
	if len(latest) > 0 {
		fmt.Fprintln(w, "\nlatest detections:")
		for i := len(latest) - 1; i >= 0 && i >= len(latest)-6; i-- {
			e := latest[i]
			sum := e.Summary
			if len(sum) > 110 {
				sum = sum[:107] + "..."
			}
			fmt.Fprintf(w, "  %s %-8s %-24s %4.2f  %s\n", e.At.UTC().Format("15:04:05"), e.UAV, e.ID, e.Fused, sum)
		}
	}
}

func lowerTrim(s, prefix string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, prefix)), "_", " ")
}

// renderPhase4 shows the casualties and the investigation tasks (nothing
// before casualty-analyst and mission-planner publish).
func renderPhase4(w io.Writer, b *monitor.Board, now time.Time) {
	cas, casAt, tasks, tasksAt := b.Snapshot()
	if cas != nil {
		by := map[string]int{}
		for _, c := range cas.Casualties {
			by[casualty.TriageName(c.Triage)]++
		}
		var tri []string
		for _, k := range []string{"Red", "Yellow", "Green", "Black", "Grey"} {
			if by[k] > 0 {
				tri = append(tri, fmt.Sprintf("%d %s", by[k], k))
			}
		}
		fmt.Fprintf(w, "\ncasualties (casualty-analyst, %.0f s ago): %d  %s\n", now.Sub(casAt).Seconds(), len(cas.Casualties), strings.Join(tri, ", "))
		if len(cas.Casualties) > 0 {
			fmt.Fprintf(w, "%-6s %-8s %-18s %5s %5s %-20s %s\n", "ID", "TRIAGE", "STATUS", "CEP", "LOOKS", "SEEN BY", "WHY")
		}
		for i, c := range cas.Casualties {
			if i == 15 {
				fmt.Fprintf(w, "  ... and %d more ('casualty-analyst status' lists them all)\n", len(cas.Casualties)-15)
				break
			}
			t := casualty.TriageName(c.Triage)
			if c.Provisional {
				t += "?"
			}
			why := c.Reason
			if c.NeedsInvestigation {
				why += " [needs a look]"
			}
			if len(why) > 70 {
				why = why[:67] + "..."
			}
			fmt.Fprintf(w, "%-6s %-8s %-18s %4.1fm %5d %-20s %s\n", c.CasualtyId, t, lowerTrim(c.Status.String(), "CASUALTY_STATUS_"), c.CepM, c.Investigations,
				strings.Join(c.UavIds, ","), why)
		}
	}
	if tasks != nil {
		open, done, other := 0, 0, 0
		for _, t := range tasks.Tasks {
			switch t.State {
			case sarpb.TaskState_TASK_STATE_DONE:
				done++
			case sarpb.TaskState_TASK_STATE_FAILED, sarpb.TaskState_TASK_STATE_CANCELLED:
				other++
			default:
				open++
			}
		}
		fmt.Fprintf(w, "\ninvestigations (mission-planner, %.0f s ago): %d open, %d done, %d failed or cancelled\n", now.Sub(tasksAt).Seconds(), open, done, other)
		// Open tasks first, then the latest finished.
		var show []*sarpb.Task
		for _, t := range tasks.Tasks {
			if t.State != sarpb.TaskState_TASK_STATE_DONE && t.State != sarpb.TaskState_TASK_STATE_FAILED && t.State != sarpb.TaskState_TASK_STATE_CANCELLED {
				show = append(show, t)
			}
		}
		for i := len(tasks.Tasks) - 1; i >= 0 && len(show) < 10; i-- {
			if t := tasks.Tasks[i]; t.State == sarpb.TaskState_TASK_STATE_DONE || t.State == sarpb.TaskState_TASK_STATE_FAILED {
				show = append(show, t)
			}
		}
		if len(show) > 0 {
			fmt.Fprintf(w, "%-11s %-6s %4s %-10s %-7s %s\n", "TASK", "CAS", "TIER", "STATE", "UAV", "NOTE")
		}
		for _, t := range show {
			note := t.Note
			if len(note) > 80 {
				note = note[:77] + "..."
			}
			uav := t.UavId
			if uav == "" {
				uav = "-"
			}
			fmt.Fprintf(w, "%-11s %-6s %4d %-10s %-7s %s\n", t.TaskId, t.CasualtyId, t.Tier, lowerTrim(t.State.String(), "TASK_STATE_"), uav, note)
		}
	}
}

// renderPhase5 shows the rescue teams and their assignments (nothing before
// rescue-dispatch publishes).
func renderPhase5(w io.Writer, b *monitor.Board, now time.Time) {
	d, at := b.Dispatch()
	if d == nil {
		return
	}
	tak := "TAK off"
	if t := d.Tak; t != nil && t.Server != "" {
		tak = "TAK " + t.Server + " NOT CONNECTED"
		if t.Connected {
			tak = fmt.Sprintf("TAK %s connected, users: %s", t.Server, strings.Join(t.Users, ", "))
		}
	}
	streets := "no street file"
	if r := d.Roads; r != nil && r.Source != "" {
		streets = fmt.Sprintf("%s streets, %d pieces closed by rubble", r.Source, r.BlockedEdges)
	}
	active, done := 0, 0
	for _, a := range d.Assignments {
		switch a.State {
		case sarpb.AssignmentState_ASSIGNMENT_STATE_DONE:
			done++
		case sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE, sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
			active++
		}
	}
	fmt.Fprintf(w, "\nrescue teams (rescue-dispatch, %.0f s ago): %d active, %d done, waiting: %s | %s | %s\n", now.Sub(at).Seconds(), active, done,
		nonEmptyS(strings.Join(d.Waiting, ", "), "none"), streets, tak)
	byID := map[string]*sarpb.Assignment{}
	for _, a := range d.Assignments {
		byID[a.AssignmentId] = a
	}
	fmt.Fprintf(w, "%-8s %-9s %-7s %-6s %-9s %-7s %5s  %s\n", "TEAM", "CALLSIGN", "MODE", "SOURCE", "STATE", "CAS", "DONE", "RESCUE")
	for _, t := range d.Teams {
		rescue := ""
		if a := byID[t.AssignmentId]; a != nil && a.State == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE && a.FreeUnixNs > 0 {
			class, _, _ := strings.Cut(a.Extrication, ",")
			rescue = fmt.Sprintf("%s, free in %.0f s", nonEmptyS(class, "on scene"), math.Max(0, time.Unix(0, a.FreeUnixNs).Sub(now).Seconds()))
		}
		fmt.Fprintf(w, "%-8s %-9s %-7s %-6s %-9s %-7s %5d  %s\n", t.TeamId, t.Callsign, t.Mode, t.Source, lowerTrim(t.State.String(), "TEAM_STATE_"), nonEmptyS(t.CasualtyId, "-"), t.AssignmentsDone, rescue)
	}
}

func nonEmptyS(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
