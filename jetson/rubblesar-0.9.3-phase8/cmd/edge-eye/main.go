// Command edge-eye runs on the Jetson Orin Nano and plays the payload computer
// of every simulated drone: flight simulation, telemetry, control, link budget
// and (phase 3) the onboard perception pipeline with store-and-forward.
//
//	edge-eye [run] [--config FILE]           run (default command)
//	edge-eye budget --kbps N|--bps N [--uav ID|all] [--admin ADDR]
//	edge-eye status [--admin ADDR]
//	edge-eye selftest [--config FILE] [--frames 20] [--oracle]
//	edge-eye check-config [--config FILE]
//	edge-eye version
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/bus/zenoh"
	"rubblesar/internal/check"
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/edge"
	"rubblesar/internal/logx"
	"rubblesar/internal/version"
)

const tool = "edge-eye"

var defaultConfig = cli.DefaultConfig("/etc/rubblesar/edge-eye.toml", "configs/jetson/edge-eye.toml")

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		run(args)
	case "budget":
		budget(args)
	case "status":
		status(args)
	case "selftest":
		selftest(args)
	case "check-config":
		checkConfig(args)
	case "version", "--version":
		fmt.Println(version.String(tool))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(cli.ExitSetup)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `edge-eye: simulated drone payload computer (RubbleSAR, Jetson)

  edge-eye [run] [--config FILE]                 run all simulated drones
  edge-eye budget --kbps N [--uav all|ID]        change the radio budget now (0 = unlimited)
  edge-eye budget --bps N  [--uav all|ID]        same, in bits per second
  edge-eye status                                show drones, link usage, perception and queues
  edge-eye selftest [--frames 20] [--oracle]     test the perception pipeline and the queue on this machine
  edge-eye check-config [--config FILE]          validate a config file and exit
  edge-eye version

Default config: /etc/rubblesar/edge-eye.toml (falls back to configs/jetson/edge-eye.toml).
Environment: SAR_ZENOH_CONNECT, SAR_INCIDENT_ID, SAR_LOG_LEVEL, RUST_LOG (zenoh's own logs).
`)
}

func loadConfig(fs *flag.FlagSet, args []string) (*config.Edge, string) {
	path := fs.String("config", defaultConfig, "config file")
	_ = fs.Parse(args)
	cli.LoadEnvFile("/etc/rubblesar/edge-eye.env")
	cfg, err := config.LoadEdge(*path)
	if err != nil {
		cli.Fatal(tool, err, "config: "+*path)
	}
	return cfg, *path
}

func checkConfig(args []string) {
	cfg, path := loadConfig(flag.NewFlagSet("check-config", flag.ExitOnError), args)
	fmt.Printf("OK: %s\n  incident %s at %.5f,%.5f radius %.0f m\n  zenoh %s -> %s\n  admin %s\n",
		path, cfg.Site.IncidentID, cfg.Site.CenterLat, cfg.Site.CenterLon, cfg.Site.RadiusM,
		cfg.Zenoh.Mode, cfg.Zenoh.Target(), cfg.Admin.Listen)
	for _, u := range cfg.UAVs {
		fmt.Printf("  drone %-8s %-12s %-6s %3.0f m AGL, %.1f Hz telemetry, budget %d kbps",
			u.ID, u.Role, u.Behaviour, u.AltAGLm, u.TelemetryHz, *u.BudgetKbps)
		if u.Behaviour == "survey" {
			sites := "all sites"
			if len(u.SurveySites) > 0 {
				sites = strings.Join(u.SurveySites, ",")
			}
			fmt.Printf(", survey %s at %.1f m/s", sites, u.SurveySpeedMps)
		}
		if u.Behaviour == "standby" {
			fmt.Printf(", waits at home for tasks (flies at %.0f m/s)", u.SpeedMps)
		}
		if cfg.Perception.Enabled {
			fmt.Printf(", camera %.1f Hz", cfg.FrameHz(u))
		}
		fmt.Println()
	}
	if p := cfg.Perception; p.Enabled {
		fmt.Printf("  perception: detector %s, model %s, confidence %.2f, skin band %.0f..%.0f C (+%.0f C), queue %s (%d MB)\n",
			p.Detector, p.ModelDir, p.Conf, p.SkinMinC, p.SkinMaxC, p.WarmDeltaC, cfg.Outbox.Dir, cfg.Outbox.MaxQueueMB)
		fmt.Printf("  positions: on a surface %.1f m above the ground, detection error %.1f m added to the CEP; hovering: movement when %.0f%% of an outline shifts more than %d px in %d frames\n",
			p.DebrisHeightM, p.DetectionSigmaM, 100*p.MovementChange, p.MovementTolerancePx, p.MovementMinFrames)
	} else {
		fmt.Println("  perception: off ([perception] enabled = false)")
	}
}

func selftest(args []string) {
	fs := flag.NewFlagSet("selftest", flag.ExitOnError)
	frames := fs.Int("frames", 20, "frames per altitude (scout 40 m and investigator 15 m)")
	oracle := fs.Bool("oracle", false, "use the simulated truth instead of the model (tests the pipeline mechanics only)")
	reportDir := fs.String("report-dir", "reports", "folder for the JSON report")
	cfg, path := loadConfig(fs, args)
	fmt.Println(version.String(tool))
	fmt.Printf("Perception self-test on this machine (no ZBook needed) | config %s | scenario %s | model %s\n", path, cfg.Site.Scenario(), cfg.Perception.ModelDir)
	r := check.EdgeSelfTest(cfg, check.EdgeSelfOptions{Frames: *frames, Oracle: *oracle}, os.Stdout)
	if _, err := r.Finish(*reportDir); err != nil {
		fmt.Fprintf(os.Stderr, "could not save the report: %v\n", err)
	}
	if !r.Passed() {
		os.Exit(1)
	}
}

func run(args []string) {
	cfg, path := loadConfig(flag.NewFlagSet("run", flag.ExitOnError), args)
	log := logx.New(tool, cfg.Log.Level, cfg.Log.Format)
	log.Info("starting", "version", version.String(tool), "config", path, "incident", cfg.Site.IncidentID,
		"zenoh_mode", cfg.Zenoh.Mode, "connect", cfg.Zenoh.Target(), "drones", len(cfg.UAVs))
	if !zenoh.Available {
		cli.Fatal(tool, diag.New("E-BUS-002", "edge-eye was built without zenoh"))
	}

	ctx, stop := cli.SignalContext()
	defer stop()

	admin, err := edge.StartAdmin(cfg.Admin.Listen, log)
	if err != nil {
		cli.Fatal(tool, err, "config: "+path)
	}
	defer admin.Close()
	admin.SetState("connecting", nil)

	b, err := bus.OpenRetry(ctx, log, cfg.Zenoh.OpenTimeout.Duration, cfg.Zenoh.RetryForever, zenoh.Opener(cfg.Zenoh))
	if err != nil {
		if errors.Is(err, ctx.Err()) {
			log.Info("stopped before connecting")
			return
		}
		admin.SetState("failed", err)
		cli.Fatal(tool, err, "config: "+path, "connect: "+cfg.Zenoh.Target())
	}
	defer b.Close()
	log.Info("connected to zenoh", "backend", b.Info().Backend, "endpoints", strings.Join(b.Info().Endpoints, ","))

	rt, err := edge.New(cfg, b, log)
	if err != nil {
		admin.SetState("failed", err)
		cli.Fatal(tool, err, "config: "+path)
	}
	defer rt.Close()
	admin.SetRuntime(rt)
	admin.SetState("running", nil)
	log.Info("running; press Ctrl+C to stop", "admin", "http://"+admin.Addr()+"/v1/status")

	go reportPeriodically(ctx.Done(), rt, log)
	_ = rt.Run(ctx)
	log.Info("shutting down")
}

func reportPeriodically(done <-chan struct{}, rt *edge.Runtime, log interface{ Info(string, ...any) }) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			for _, d := range rt.Status().Drones {
				log.Info("drone summary", "uav", d.ID, "phase", d.Phase, "battery_pct", fmt.Sprintf("%.0f", d.BatteryPct),
					"published", d.Published, "dropped_by_budget", d.DroppedMsgs, "budget_kbps", d.BudgetKbps,
					"publish_errors", d.PublishErrors, "control", d.ControlHandled)
			}
		}
	}
}

func adminAddr(fs *flag.FlagSet) *string {
	return fs.String("admin", "127.0.0.1:9101", "edge-eye admin API address")
}

func budget(args []string) {
	fs := flag.NewFlagSet("budget", flag.ExitOnError)
	addr := adminAddr(fs)
	uav := fs.String("uav", "all", "drone id or 'all'")
	kbps := fs.Int64("kbps", -1, "budget in kbit/s (0 = unlimited)")
	bps := fs.Int64("bps", -1, "budget in bit/s (0 = unlimited)")
	_ = fs.Parse(args)
	req := edge.BudgetRequest{UAV: *uav}
	switch {
	case *kbps >= 0 && *bps >= 0:
		cli.Fatal(tool, diag.New("E-EDGE-003", "give --kbps or --bps, not both"))
	case *kbps >= 0:
		v := uint32(*kbps)
		req.Kbps = &v
	case *bps >= 0:
		v := uint32(*bps)
		req.Bps = &v
	default:
		cli.Fatal(tool, diag.New("E-EDGE-003", "--kbps or --bps is required"))
	}
	body, _ := json.Marshal(req)
	resp, err := http.Post("http://"+*addr+"/v1/link/budget", "application/json", bytes.NewReader(body))
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-EDGE-004", err, "edge-eye admin API at %s is not answering", *addr))
	}
	defer resp.Body.Close()
	var br edge.BudgetResponse
	_ = json.NewDecoder(resp.Body).Decode(&br)
	if !br.OK {
		cli.Fatal(tool, fmt.Errorf("%s", br.Error))
	}
	desc := "unlimited"
	if br.Bps > 0 {
		desc = fmt.Sprintf("%d bit/s (%.1f kbit/s)", br.Bps, float64(br.Bps)/1000)
	}
	fmt.Printf("link budget for %s set to %s\n", strings.Join(br.UAVs, ", "), desc)
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := adminAddr(fs)
	raw := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)
	resp, err := http.Get("http://" + *addr + "/v1/status")
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-EDGE-004", err, "edge-eye admin API at %s is not answering", *addr))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if *raw {
		os.Stdout.Write(b)
		return
	}
	var doc struct {
		Version   string       `json:"version"`
		State     string       `json:"state"`
		LastError string       `json:"last_error"`
		Process   string       `json:"process_uptime"`
		Runtime   *edge.Status `json:"runtime"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		cli.Fatal(tool, err)
	}
	fmt.Printf("%s\nstate: %s   up %s\n", doc.Version, doc.State, doc.Process)
	if doc.LastError != "" {
		fmt.Printf("last error: %s\n", doc.LastError)
	}
	if doc.Runtime == nil {
		return
	}
	fmt.Printf("incident %s   bus %s %s %s\n\n", doc.Runtime.Incident, doc.Runtime.Bus.Backend, doc.Runtime.Bus.Mode, strings.Join(doc.Runtime.Bus.Endpoints, ","))
	fmt.Printf("%-8s %-12s %-8s %10s %10s %5s %5s %12s %10s %7s %9s %6s\n", "UAV", "ROLE", "PHASE", "LAT", "LON", "AGL", "BATT", "BUDGET", "USED(5s)", "DROPPED", "PUBLISHED", "ERRORS")
	p3 := false
	for _, d := range doc.Runtime.Drones {
		bud := "unlimited"
		if d.BudgetBps > 0 {
			bud = fmt.Sprintf("%d b/s", d.BudgetBps)
		}
		fmt.Printf("%-8s %-12s %-8s %10.5f %10.5f %4.0fm %4.0f%% %12s %6d b/s %7d %9d %6d\n",
			d.ID, d.Role, d.Phase, d.Lat, d.Lon, d.AltAGLm, d.BatteryPct, bud, d.UsedBps, d.DroppedMsgs, d.Published, d.PublishErrors)
		p3 = p3 || d.Mission != "" || d.Backend != ""
	}
	if !p3 {
		return
	}
	fmt.Printf("\n%-8s %-12s %7s %6s %9s %8s %7s %7s %8s %6s %6s %6s %s\n", "UAV", "MISSION", "FRAMES", "FPS", "PIPE(ms)", "RENDER", "TRACKS", "REPORTS", "QUEUED", "UNACK", "ACKED", "LEVEL", "LINK")
	for _, d := range doc.Runtime.Drones {
		mis := "-"
		if d.Mission != "" {
			mis = fmt.Sprintf("%d/%d", d.MissionStep, d.MissionSteps)
			if d.MissionDone {
				mis = "done"
			}
		}
		link := "ok"
		switch {
		case d.Outage:
			link = "OUTAGE (simulated)"
		case d.Probing:
			link = "no acks: probing"
		}
		fmt.Printf("%-8s %-12s %7d %6.2f %9.0f %8.0f %7d %7d %8d %6d %6d %6d %s\n", d.ID, mis, d.Frames, d.FrameHz, d.PipelineMS, d.RenderMS,
			d.Tracks, d.Reports, d.Queued, d.Unacked, d.Acked, d.Level, link)
		if d.LastError != "" {
			fmt.Printf("         last error: %s\n", d.LastError)
		}
	}
	if len(doc.Runtime.Drones) > 0 && doc.Runtime.Drones[0].Backend != "" {
		fmt.Printf("perception: %s\n", doc.Runtime.Drones[0].Backend)
	}
	header := false
	for _, d := range doc.Runtime.Drones {
		if d.Task == "" {
			continue
		}
		if !header {
			fmt.Printf("\n%-8s %-14s %-11s %8s %6s %s\n", "UAV", "TASK", "STATE", "HOVER s", "PARTS", "MOVEMENT")
			header = true
		}
		mv := "none seen"
		if d.TaskMoved {
			mv = "MOVED"
		}
		if d.TaskParts == 0 {
			mv = "-"
		}
		fmt.Printf("%-8s %-14s %-11s %8.0f %6d %s\n", d.ID, d.Task, d.TaskState, d.TaskHoverS, d.TaskParts, mv)
	}
}
