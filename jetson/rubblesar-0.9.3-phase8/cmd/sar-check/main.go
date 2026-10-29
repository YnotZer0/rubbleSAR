// Command sar-check runs RubbleSAR's acceptance tests and prints a pass/fail
// table, saves a JSON report and, on failure, a block to paste for help.
//
//	sar-check loopback                       local zenoh library self-test (no router needed)
//	sar-check router  [--config FILE]        two sessions exchange messages through zenohd
//	sar-check phase0  [--config FILE] [--self-test] [--quick]
//	sar-check phase1  [--config FILE] [--scenario DIR] [--quick]
//	sar-check phase2  [--config FILE] [--model DIR] [--scenario DIR] [--role zbook|jetson] [--quick]
//	sar-check phase3  [--config FILE] [--quick] [--max-minutes 15] [--allow-oracle]
//	sar-check phase4  [--config FILE] [--max-minutes 45] [--allow-oracle]
//	sar-check phase5  [--config FILE] [--max-minutes 45] [--quick] [--allow-oracle]
//	sar-check phase6  [--config FILE] [--max-minutes 45] [--quick] [--allow-oracle]
//	sar-check phase7  [--config FILE] [--max-minutes 45] [--quick] [--allow-oracle]
//	sar-check phase8  [--config FILE] [--demo FILE] [--run RUN] [--seconds 4] [--rehearse]
//	sar-check tak     [--server 127.0.0.1:8087]   TAK server routing test (no zenoh needed)
//	sar-check version
//
// Exit codes: 0 all passed, 1 a check failed, 2 the tool could not run.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/bus/zenoh"
	"rubblesar/internal/check"
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/director"
	"rubblesar/internal/infer"
	"rubblesar/internal/logx"
	"rubblesar/internal/ops"
	"rubblesar/internal/vcam"
	"rubblesar/internal/version"
)

const tool = "sar-check"

func usage() {
	fmt.Fprint(os.Stderr, `sar-check: RubbleSAR acceptance tests

  sar-check loopback                     zenoh library self-test on this machine (Jetson or ZBook)
  sar-check router  [--config FILE]      messages through the zenoh router (ZBook, or Jetson -> ZBook)
  sar-check phase0  [--config FILE]      Phase 0 acceptance test (ZBook, with edge-eye running on the Jetson)
                    [--self-test]        ...with simulated drones inside sar-check (no Jetson needed)
                    [--quick]            ...with shorter measurement windows
  sar-check phase1  [--config FILE]      Phase 1 acceptance test: scenario, layers, virtual cameras, scorer
                    [--scenario DIR]     ...scenario folder (default: [site] scenario_dir of the config)
                    [--quick]            ...test every third victim (and all moving ones)
  sar-check phase2  [--config FILE]      Phase 2 acceptance test: the body-part model (ZBook: ONNX Runtime, Jetson: TensorRT)
                    [--model DIR]        ...model folder (default /var/lib/rubblesar/models/current)
                    [--role zbook|jetson] ...default: jetson on a Jetson, else zbook
                    [--quick]            ...evaluate every third held-out frame
  sar-check phase3  [--config FILE]      Phase 3 acceptance run (ZBook, with the Jetson flying): detections, budget, outage, positions
  sar-check phase4  [--config FILE]      Phase 4 acceptance run (ZBook, with the Jetson flying): casualty records, investigations,
                                         triage against ground truth (about 25-35 minutes; archives the previous run first)
                    [--max-minutes 45]   ...stop the run after this many minutes
  sar-check phase5  [--config FILE]      Phase 5 acceptance run (ZBook, with the Jetson flying): the phase 4 run with rescue-dispatch
                                         sending the teams, checked on the TAK server (sar-check joins it as team CHECK-1);
                                         about 30-45 minutes
                    [--quick]            ...no new survey: the casualties of the last run get teams again (about 5-15 minutes)
                    [--max-minutes 45]   ...stop the drones' run after this many minutes (then up to 25 more for the rescue)
  sar-check phase6  [--config FILE]      Phase 6 acceptance: both machines up (rubblesar status), the offline map drawn and
                                         served, the phase 5 run recorded, and its after-action report (about 30-50 minutes)
                    [--quick]            ...as phase5 --quick
  sar-check phase7  [--config FILE]      Phase 7 acceptance: the phase 6 run, with the live operator view (sar-web /ops/)
                                         compared with the services throughout, its event stream, crops and timeline
                    [--quick]            ...as phase5 --quick
  sar-check phase8  [--config FILE]      Phase 8 acceptance: the demo director ready to record - its settings and storyboard,
                                         a recorded run, the rendered frame, the narration, sar-web's replay and presenter,
                                         a short test recording on every recorder, ffmpeg (a few minutes; no drones needed)
                    [--demo FILE]        ...demo settings (default ~/.config/rubblesar/demo.toml)
                    [--run RUN]          ...the recorded run (default the latest)
                    [--seconds 4]        ...length of the test recordings
                    [--rehearse]         ...also play the whole storyboard on the presenter page (not recorded)
  sar-check version

Default config: /etc/rubblesar/sar.toml, else configs/zbook/sar.toml
(phase1 on the Jetson: /etc/rubblesar/edge-eye.toml).
On the Jetson pass --config /etc/rubblesar/edge-eye.toml to 'router' (its [zenoh] section is used).
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(cli.ExitSetup)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "version", "--version":
		fmt.Println(version.String(tool))
		return
	case "help", "-h", "--help":
		usage()
		return
	case "loopback", "router", "phase0", "phase1", "phase2", "phase3", "phase4", "phase5", "phase6", "phase7", "phase8", "tak":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(cli.ExitSetup)
	}

	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	path := fs.String("config", cli.DefaultConfig("/etc/rubblesar/sar.toml", "/etc/rubblesar/edge-eye.toml", "configs/zbook/sar.toml"), "config file")
	scenarioDir := fs.String("scenario", "", "phase1/phase2: scenario folder")
	modelDir := fs.String("model", infer.DefaultModelDir, "phase2: model folder")
	role := fs.String("role", "", "phase2: zbook or jetson (default: detected)")
	saveEval := fs.String("save-eval", "", "phase2: also write the evaluation JSON here")
	selfTest := fs.Bool("self-test", false, "phase0: simulate the drones inside sar-check")
	quick := fs.Bool("quick", false, "phase0: shorter measurement windows; phase1: every third victim; phase3: shorter outage and budget steps; phase5: no new survey")
	allowOracle := fs.Bool("allow-oracle", false, "phase3/phase4: accept drones running the truth oracle (tests the pipeline and link only)")
	maxMin := fs.Float64("max-minutes", 0, "phase3/phase4: stop the run after this many minutes (default 15 / 45)")
	reportDir := fs.String("report-dir", "", "where to save JSON reports (default from config, else ./reports)")
	server := fs.String("server", "", "tak: TAK server host:port (default [dispatch] tak_server)")
	demoPath := fs.String("demo", director.DefaultConfigPath(), "phase8: demo settings")
	run := fs.String("run", "", "phase8: the recorded run (default the latest)")
	seconds := fs.Float64("seconds", 4, "phase8: length of the test recordings")
	rehearse := fs.Bool("rehearse", false, "phase8: also play the whole storyboard on the presenter page")
	_ = fs.Parse(args)

	if cmd == "tak" {
		runTak(*path, *server, *reportDir)
		return
	}

	if cmd == "phase1" {
		runPhase1(*path, *scenarioDir, *reportDir, *quick)
		return
	}
	if cmd == "phase2" {
		runPhase2(*path, *scenarioDir, *modelDir, *role, *reportDir, *saveEval, *quick)
		return
	}
	if cmd == "phase8" {
		runPhase8(*path, *demoPath, *run, *reportDir, *modelDir, *seconds, *rehearse)
		return
	}
	if !zenoh.Available {
		cli.Fatal(tool, diag.New("E-BUS-002", "sar-check was built without zenoh"))
	}
	cfg := loadConfig(cmd, *path)
	if *reportDir != "" {
		cfg.Check.ReportDir = *reportDir
	}
	log := logx.New(tool, "warn", cfg.Log.Format)
	ctx, stop := cli.SignalContext()
	defer stop()

	env := &check.Env{
		Cfg: cfg,
		Log: log,
		Out: os.Stdout,
		Open: func(ctx context.Context, z config.Zenoh) (bus.Bus, error) {
			return zenoh.Open(ctx, z)
		},
	}
	fmt.Printf("%s\n", version.String(tool))
	var r *check.Report
	switch cmd {
	case "loopback":
		fmt.Println("Loopback self-test: two zenoh sessions on this machine (no router needed)")
		r = check.Loopback(ctx, env)
	case "router":
		fmt.Printf("Router test: %s -> %s\n", cfg.Zenoh.Mode, cfg.Zenoh.Target())
		r = check.Router(ctx, env)
	case "phase3":
		if *maxMin <= 0 {
			*maxMin = 15
		}
		r = runPhase3(ctx, env, *quick, *allowOracle, *maxMin)
	case "phase4":
		if *maxMin <= 0 {
			*maxMin = 45
		}
		r = runPhase4(ctx, env, *allowOracle, *maxMin)
	case "phase5":
		if *maxMin <= 0 {
			*maxMin = 45
		}
		r = runPhase5(ctx, env, *allowOracle, *quick, *maxMin)
	case "phase6", "phase7":
		if *maxMin <= 0 {
			*maxMin = 45
		}
		r = runPhase6(ctx, env, cmd, *allowOracle, *quick, *maxMin)
	case "phase0":
		mode, inc := "drones from edge-eye on the Jetson", cfg.Site.IncidentID
		if *selfTest {
			mode, inc = "SELF-TEST: simulated drones inside sar-check", check.SelfTestIncident(inc)
		}
		fmt.Printf("Phase 0 acceptance test | incident %s | %s -> %s | %s\n", inc, cfg.Zenoh.Mode, cfg.Zenoh.Target(), mode)
		r = check.Phase0(ctx, env, check.Phase0Options{SelfTest: *selfTest, Quick: *quick})
	}
	r.Context["config"] = *path
	if _, err := r.Finish(cfg.Check.ReportDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if !r.Passed() {
		os.Exit(cli.ExitFail)
	}
}

// loadConfig accepts either a ZBook config or (for router/loopback on the
// Jetson) an edge-eye config, whose [site] and [zenoh] sections are reused.
func loadConfig(cmd, path string) *config.ZBook {
	cli.LoadEnvFile("/etc/rubblesar/edge-eye.env")
	if cmd == "loopback" {
		z, err := config.LoadZBook(path)
		if err == nil {
			return z
		}
		d := &config.ZBook{}
		d.Check.ReportDir = "reports"
		return d
	}
	z, err := config.LoadZBook(path)
	if err == nil {
		return z
	}
	if cmd == "router" {
		if e, eerr := config.LoadEdge(path); eerr == nil {
			z := &config.ZBook{Common: e.Common}
			z.Check.ReportDir = "reports"
			return z
		}
	}
	cli.Fatal(tool, err, "config: "+path)
	return nil
}

// runPhase1 needs no network: it checks the scenario folder on this machine.
func runPhase1(path, dir, reportDir string, quick bool) {
	var site config.Site
	rd := "reports"
	if z, err := config.LoadZBook(path); err == nil {
		site, rd = z.Site, z.Check.ReportDir
	} else if e, eerr := config.LoadEdge(path); eerr == nil {
		site = e.Site
	} else {
		cli.Fatal(tool, err, "config: "+path)
	}
	if dir == "" {
		dir = site.Scenario()
	}
	if reportDir != "" {
		rd = reportDir
	}
	fmt.Printf("%s\n", version.String(tool))
	fmt.Printf("Phase 1 acceptance test | incident %s | scenario %s\n", site.IncidentID, dir)
	r := check.Phase1(check.Phase1Options{ScenarioDir: dir, Site: site, Quick: quick}, os.Stdout)
	r.Context["config"] = path
	if _, err := r.Finish(rd); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if !r.Passed() {
		os.Exit(cli.ExitFail)
	}
}

// runPhase2 checks the model on this machine (no network needed).
func runPhase2(path, dir, model, role, reportDir, saveEval string, quick bool) {
	var site config.Site
	rd := "reports"
	if z, err := config.LoadZBook(path); err == nil {
		site, rd = z.Site, z.Check.ReportDir
	} else if e, eerr := config.LoadEdge(path); eerr == nil {
		site = e.Site
	} else {
		cli.Fatal(tool, err, "config: "+path)
	}
	if dir == "" {
		dir = site.Scenario()
	}
	if reportDir != "" {
		rd = reportDir
	}
	if role == "" {
		role = "zbook"
		if _, err := os.Stat("/etc/nv_tegra_release"); err == nil {
			role = "jetson"
		}
	}
	if role != "zbook" && role != "jetson" {
		cli.Fatal(tool, diag.New("E-CFG-003", "--role must be zbook or jetson, not %q", role))
	}
	fmt.Printf("%s\n", version.String(tool))
	fmt.Printf("Phase 2 acceptance test | %s | model %s | scenario %s | back ends: ONNX Runtime %v, TensorRT %v\n",
		role, model, dir, infer.ORTAvailable, infer.TRTAvailable)
	r := check.Phase2(check.Phase2Options{Role: role, ModelDir: model, ScenarioDir: dir, Quick: quick, SaveEval: saveEval}, os.Stdout)
	r.Context["config"] = path
	if _, err := r.Finish(rd); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if !r.Passed() {
		os.Exit(cli.ExitFail)
	}
}

// runPhase3 drives a phase 3 run over the real bus (the Jetson's edge-eye and
// the ZBook's casualty-analyst must be running) and scores it.
func runPhase3(ctx context.Context, env *check.Env, quick, allowOracle bool, maxMin float64) *check.Report {
	cfg := env.Cfg
	w, err := vcam.Open(cfg.Site.Scenario(), false)
	if err != nil {
		cli.Fatal(tool, err, "scenario: "+cfg.Site.Scenario())
	}
	b, err := bus.OpenRetry(ctx, env.Log, cfg.Zenoh.OpenTimeout.Duration, false, func(ctx context.Context) (bus.Bus, error) { return zenoh.Open(ctx, cfg.Zenoh) })
	if err != nil {
		cli.Fatal(tool, err, "connect: "+cfg.Zenoh.Target())
	}
	defer b.Close()
	o := check.DefaultPhase3Options(cfg, b, w)
	if quick {
		o.OutageAt, o.Outage, o.LowAt, o.LowFor = 60*time.Second, 30*time.Second, 120*time.Second, 60*time.Second
	}
	o.MaxRun = time.Duration(maxMin * float64(time.Minute))
	o.AllowOracle = allowOracle
	o.Progress = os.Stdout
	fmt.Printf("Phase 3 acceptance run | incident %s | drones %s | %s -> %s\n", cfg.Site.IncidentID, strings.Join(cfg.Check.ExpectUAVs, ", "), cfg.Zenoh.Mode, cfg.Zenoh.Target())
	fmt.Printf("The drones fly their survey missions again from the start (about 8-12 minutes); progress every 30 s.\n")
	return check.Phase3(ctx, o, os.Stdout)
}

// runPhase4 drives a phase 4 run over the real bus (edge-eye on the Jetson,
// casualty-analyst and mission-planner on the ZBook) and scores it against the
// scenario.
func runPhase4(ctx context.Context, env *check.Env, allowOracle bool, maxMin float64) *check.Report {
	cfg := env.Cfg
	w, err := vcam.Open(cfg.Site.Scenario(), false)
	if err != nil {
		cli.Fatal(tool, err, "scenario: "+cfg.Site.Scenario())
	}
	b, err := bus.OpenRetry(ctx, env.Log, cfg.Zenoh.OpenTimeout.Duration, false, func(ctx context.Context) (bus.Bus, error) { return zenoh.Open(ctx, cfg.Zenoh) })
	if err != nil {
		cli.Fatal(tool, err, "connect: "+cfg.Zenoh.Target())
	}
	defer b.Close()
	o := check.DefaultPhase4Options(cfg, b, w)
	o.MaxRun = time.Duration(maxMin * float64(time.Minute))
	o.AllowOracle = allowOracle
	o.Progress = os.Stdout
	fmt.Printf("Phase 4 acceptance run | incident %s | drones %s | %s -> %s\n", cfg.Site.IncidentID, strings.Join(cfg.Check.ExpectUAVs, ", "), cfg.Zenoh.Mode, cfg.Zenoh.Target())
	fmt.Printf("The scout surveys again from the start and the investigators are sent to each casualty that needs a close look\n")
	fmt.Printf("(about 25-35 minutes); casualty-analyst's previous run is archived first. Progress every 30 s.\n")
	return check.Phase4(ctx, o, os.Stdout)
}

// runPhase5 drives the phase 4 run with rescue-dispatch sending the teams and
// checks the result on the TAK server.
func runPhase5(ctx context.Context, env *check.Env, allowOracle, quick bool, maxMin float64) *check.Report {
	cfg := env.Cfg
	w, err := vcam.Open(cfg.Site.Scenario(), false)
	if err != nil {
		cli.Fatal(tool, err, "scenario: "+cfg.Site.Scenario())
	}
	b, err := bus.OpenRetry(ctx, env.Log, cfg.Zenoh.OpenTimeout.Duration, false, func(ctx context.Context) (bus.Bus, error) { return zenoh.Open(ctx, cfg.Zenoh) })
	if err != nil {
		cli.Fatal(tool, err, "connect: "+cfg.Zenoh.Target())
	}
	defer b.Close()
	p4 := check.DefaultPhase4Options(cfg, b, w)
	p4.MaxRun = time.Duration(maxMin * float64(time.Minute))
	p4.AllowOracle = allowOracle
	p4.Progress = os.Stdout
	o := check.DefaultPhase5Options(cfg, b, p4)
	o.Quick = quick
	fmt.Printf("Phase 5 acceptance run | incident %s | drones %s | TAK server %s | %s -> %s\n", cfg.Site.IncidentID, strings.Join(cfg.Check.ExpectUAVs, ", "),
		o.TakAddr, cfg.Zenoh.Mode, cfg.Zenoh.Target())
	if quick {
		fmt.Printf("Quick run: no new survey; rescue-dispatch is reset and the casualties of the last run get teams again (about 5-15 minutes).\n")
	} else {
		fmt.Printf("The phase 4 run (survey, casualty records, investigations) with rescue-dispatch sending the teams as casualties are triaged\n")
		fmt.Printf("(about 30-45 minutes); casualty-analyst's previous run is archived first. Progress every 30 s.\n")
	}
	fmt.Printf("sar-check joins the TAK server as team %s: on ATAK or WinTAK you can watch it walk its routes.\n", o.Team)
	return check.Phase5(ctx, o, os.Stdout)
}

// runPhase6 checks both machines and the offline map, records the phase 5
// run and checks its after-action report; phase7 also compares the live
// operator view with the services throughout.
func runPhase6(ctx context.Context, env *check.Env, suite string, allowOracle, quick bool, maxMin float64) *check.Report {
	cfg := env.Cfg
	w, err := vcam.Open(cfg.Site.Scenario(), false)
	if err != nil {
		cli.Fatal(tool, err, "scenario: "+cfg.Site.Scenario())
	}
	b, err := bus.OpenRetry(ctx, env.Log, cfg.Zenoh.OpenTimeout.Duration, false, func(ctx context.Context) (bus.Bus, error) { return zenoh.Open(ctx, cfg.Zenoh) })
	if err != nil {
		cli.Fatal(tool, err, "connect: "+cfg.Zenoh.Target())
	}
	defer b.Close()
	p4 := check.DefaultPhase4Options(cfg, b, w)
	p4.MaxRun = time.Duration(maxMin * float64(time.Minute))
	p4.AllowOracle = allowOracle
	p4.Progress = os.Stdout
	p5 := check.DefaultPhase5Options(cfg, b, p4)
	p5.Quick = quick
	o := check.Phase6Options{P5: p5, System: func(ctx context.Context) []check.SystemRow {
		e := &ops.Env{Cfg: cfg, Sys: ops.Exec{}, Bus: b}
		var rows []check.SystemRow
		for _, x := range ops.GetStatus(ctx, e).Rows {
			rows = append(rows, check.SystemRow{Group: x.Group, Name: x.Name, Fail: x.Level == ops.Fail, Warn: x.Level == ops.Warn, Detail: x.Detail})
		}
		return rows
	}}
	if suite == "phase7" {
		fmt.Printf("Phase 7 acceptance | incident %s | the phase 6 run, with the live operator view compared with the services throughout\n", cfg.Site.IncidentID)
	} else {
		fmt.Printf("Phase 6 acceptance | incident %s | both machines, the offline map, then the phase 5 run recorded for its after-action report\n", cfg.Site.IncidentID)
	}
	fmt.Printf("drones %s | TAK server %s | sar-web %s | %s -> %s\n", strings.Join(cfg.Check.ExpectUAVs, ", "), p5.TakAddr, cfg.Web.Listen, cfg.Zenoh.Mode, cfg.Zenoh.Target())
	if quick {
		fmt.Printf("Quick run: no new survey; the casualties of the last run get teams again (about 5-15 minutes).\n")
	} else {
		fmt.Printf("About 30-50 minutes; casualty-analyst's previous run is archived first. Progress every 30 s.\n")
	}
	fmt.Printf("sar-check joins the TAK server as team %s: on ATAK or WinTAK you can watch it walk its routes.\n", p5.Team)
	if suite == "phase7" {
		fmt.Printf("Watch it live meanwhile: http://<this ZBook>:%s/ops/ in any browser on the network.\n", portOf(cfg.Web.Listen))
		return check.Phase7(ctx, check.Phase7Options{P6: o}, os.Stdout)
	}
	return check.Phase6(ctx, o, os.Stdout)
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i+1:]
	}
	return "8090"
}

// runPhase8 checks the demo director (no drones or zenoh needed).
func runPhase8(path, demoPath, run, reportDir, modelDir string, seconds float64, rehearse bool) {
	cfg, err := config.LoadZBook(path)
	if err != nil {
		cli.Fatal(tool, err, "config: "+path)
	}
	if reportDir != "" {
		cfg.Check.ReportDir = reportDir
	}
	d, err := director.LoadConfig(demoPath)
	if err != nil {
		cli.Fatal(tool, err, "settings: "+demoPath, "rubblesar demo init writes a commented one")
	}
	dir := ""
	switch {
	case strings.Contains(run, "/"):
		dir = run
	case run != "" && run != "latest":
		dir = filepath.Join(cfg.ReportsDir(), run)
	default:
		if runs := ops.Runs(cfg.ReportsDir()); len(runs) > 0 {
			dir = filepath.Join(cfg.ReportsDir(), runs[0])
		}
	}
	ctx, stop := cli.SignalContext()
	defer stop()
	fmt.Printf("%s\n", version.String(tool))
	fmt.Printf("Phase 8 acceptance | incident %s | demo director: %s | sar-web %s | run %s\n", cfg.Site.IncidentID, d.Path, d.Demo.Web, nonEmpty(filepath.Base(dir), "none"))
	if rehearse {
		fmt.Println("With --rehearse the whole storyboard plays on the presenter page afterwards (a few minutes, not recorded).")
	}
	r := director.Phase8(ctx, director.Phase8Options{Demo: d, Web: director.NewWeb(d.Demo.Web), RunDir: dir, ScenarioDir: cfg.Site.Scenario(),
		ModelDir: modelDir, Triage: cfg.Dispatch.Triage, RecordFor: time.Duration(seconds * float64(time.Second)), Rehearse: rehearse}, os.Stdout)
	r.Context["config"] = path
	r.Context["demo"] = d.Path
	if _, err := r.Finish(cfg.Check.ReportDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if !r.Passed() {
		os.Exit(cli.ExitFail)
	}
}

func nonEmpty(s, alt string) string {
	if s == "" || s == "." {
		return alt
	}
	return s
}

// runTak checks the TAK server's routing (taky in Docker on the ZBook).
func runTak(path, server, reportDir string) {
	addr := server
	cfg, err := config.LoadZBook(path)
	if addr == "" {
		if err != nil {
			cli.Fatal(tool, err, "config: "+path, "or pass --server HOST:PORT")
		}
		addr = cfg.Dispatch.TakServer
	}
	if addr == "" {
		cli.Fatal(tool, diag.New("E-CFG-003", "no TAK server: [dispatch] tak_server is empty"), "pass --server HOST:PORT")
	}
	dir := reportDir
	if dir == "" && cfg != nil {
		dir = cfg.Check.ReportDir
	}
	if dir == "" {
		dir = "reports"
	}
	ctx, stop := cli.SignalContext()
	defer stop()
	fmt.Printf("%s\nTAK server test: %s (plain TCP), three test clients\n", version.String(tool), addr)
	r := check.TakServer(ctx, addr, os.Stdout)
	r.Context["config"] = path
	if _, err := r.Finish(dir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if !r.Passed() {
		os.Exit(cli.ExitFail)
	}
}
