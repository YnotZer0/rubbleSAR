// Command rubblesar runs the whole RubbleSAR system from the ZBook (phase 6).
//
//	rubblesar status                      every service on both machines, the drones, on one screen
//	rubblesar up [--no-jetson]            start everything in order and wait until it answers
//	rubblesar down [--no-jetson] [--keep-tak]
//	rubblesar mission start [--max-minutes N] [--continue]
//	                                      a mission: new run, drones fly, teams rescue; ends with
//	                                      the after-action report
//	rubblesar report [--run STAMP] [--list]   (re)write a run's after-action report
//	rubblesar demo <command>              the demo director (phase 8): takes, media, narration, rough cut
//	rubblesar version
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
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/logx"
	"rubblesar/internal/ops"
	"rubblesar/internal/version"
)

const tool = "rubblesar"

var defaultConfig = cli.DefaultConfig("/etc/rubblesar/sar.toml", "configs/zbook/sar.toml")

func main() {
	args := os.Args[1:]
	cmd := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "status":
		status(args)
	case "up":
		up(args)
	case "down":
		down(args)
	case "mission":
		sub := "start"
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			sub, args = args[0], args[1:]
		}
		if sub != "start" {
			fmt.Fprintf(os.Stderr, "unknown: rubblesar mission %s (only 'start')\n", sub)
			os.Exit(cli.ExitSetup)
		}
		mission(args)
	case "report":
		report(args)
	case "demo":
		demo(args)
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
	fmt.Fprint(os.Stderr, `rubblesar: runs the whole RubbleSAR system from the ZBook

  rubblesar status                       every service on both machines and the drones, on one screen
  rubblesar up [--no-jetson]             start everything in order (asks for your sudo password)
  rubblesar down [--no-jetson] [--keep-tak]
                                         stop everything (the Jetson's edge-eye first)
  rubblesar mission start [--max-minutes N] [--continue]
                                         a new run: the drones survey and look close up, the teams
                                         rescue; recorded, and ends with the after-action report
  rubblesar report [--run STAMP] [--list]
                                         (re)write a recorded run's after-action report
  rubblesar demo <command>               record the material for a video of the system (phase 8):
                                         init, check, obs-setup, live, media, script, replay, voice, cut
                                         (rubblesar demo for the list)
  rubblesar version

The Jetson is reached over SSH when [ops] jetson is set (scripts/zbook/38-jetson-link.sh).
Reports: [web] root/reports/<incident>/<run>/ (served by sar-web).
Default config: /etc/rubblesar/sar.toml (falls back to configs/zbook/sar.toml).
`)
}

func load(fs *flag.FlagSet, args []string) (*config.ZBook, string) {
	path := fs.String("config", defaultConfig, "config file")
	_ = fs.Parse(args)
	cfg, err := config.LoadZBook(*path)
	if err != nil {
		cli.Fatal(tool, err, "config: "+*path)
	}
	return cfg, *path
}

func env(cfg *config.ZBook) *ops.Env {
	return &ops.Env{Cfg: cfg, Sys: ops.Exec{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}}
}

// openBus connects to zenohd (quietly; nil when it is not running).
func openBus(ctx context.Context, cfg *config.ZBook, wait time.Duration) (bus.Bus, error) {
	if !zenoh.Available {
		return nil, diag.New("E-BUS-002", "rubblesar was built without zenoh")
	}
	log := logx.New(tool, "error", "text")
	return bus.OpenRetry(ctx, log, wait, false, zenoh.Opener(cfg.Zenoh))
}

func header(cfg *config.ZBook) {
	host, _ := os.Hostname()
	fmt.Printf("%s on %s; incident %s\n", version.Short(tool), host, cfg.Site.IncidentID)
}

func status(args []string) {
	cfg, _ := load(flag.NewFlagSet("status", flag.ExitOnError), args)
	ctx, stop := cli.SignalContext()
	defer stop()
	header(cfg)
	e := env(cfg)
	if b, err := openBus(ctx, cfg, 3*time.Second); err == nil {
		e.Bus = b
		defer b.Close()
	}
	st := ops.GetStatus(ctx, e)
	fmt.Print(st.Text())
	if st.Problems() > 0 {
		os.Exit(1)
	}
}

func up(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	noJetson := fs.Bool("no-jetson", false, "leave the Jetson alone")
	cfg, _ := load(fs, args)
	ctx, stop := cli.SignalContext()
	defer stop()
	header(cfg)
	e := env(cfg)
	err := ops.Up(ctx, e, os.Stdout, ops.UpOptions{NoJetson: *noJetson, OpenBus: func(ctx context.Context) (bus.Bus, error) {
		return openBus(ctx, cfg, 10*time.Second)
	}})
	if e.Bus != nil {
		defer e.Bus.Close()
	}
	if err != nil {
		cli.Fatal(tool, err, "rubblesar status shows what is down")
	}
	st := ops.GetStatus(ctx, e)
	fmt.Print(st.Text())
	if st.Problems() > 0 {
		os.Exit(1)
	}
}

func down(args []string) {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	noJetson := fs.Bool("no-jetson", false, "leave the Jetson alone")
	keepTAK := fs.Bool("keep-tak", false, "leave the TAK server running")
	cfg, _ := load(fs, args)
	ctx, stop := cli.SignalContext()
	defer stop()
	header(cfg)
	if err := ops.Down(ctx, env(cfg), os.Stdout, ops.DownOptions{NoJetson: *noJetson, KeepTAK: *keepTAK}); err != nil {
		cli.Fatal(tool, err)
	}
}

func mission(args []string) {
	fs := flag.NewFlagSet("mission start", flag.ExitOnError)
	maxMin := fs.Float64("max-minutes", 0, "stop after this long (default [ops] max_mission)")
	cont := fs.Bool("continue", false, "continue with the casualties already found (no reset, no new survey)")
	cfg, _ := load(fs, args)
	ctx, stop := cli.SignalContext()
	defer stop()
	header(cfg)
	b, err := openBus(ctx, cfg, cfg.Zenoh.OpenTimeout.Duration)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-OPS-002", err, "connecting to zenohd"), "rubblesar up")
	}
	defer b.Close()
	e := env(cfg)
	e.Bus = b
	o := ops.MissionOptions{NoReset: *cont}
	if *maxMin > 0 {
		o.Max = time.Duration(*maxMin * float64(time.Minute))
	}
	res, err := ops.Mission(ctx, e, os.Stdout, o)
	if err != nil && res == nil {
		cli.Fatal(tool, err, "rubblesar status shows what is down")
	}
	if err != nil {
		cli.Fatal(tool, err, "the recording is in "+res.Dir+": rubblesar report --run "+filepath.Base(res.Dir))
	}
	fmt.Printf("\nAfter-action report: %s\n  %s\n  open %s in a browser (any machine on the network)\n", res.Report.Summary(), filepath.Join(res.Dir, "index.html"), ops.ReportURL(e, res.Dir))
}

func report(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	run := fs.String("run", "", "the run (its folder name under the reports folder, or a path); default the latest")
	list := fs.Bool("list", false, "list the recorded runs")
	cfg, _ := load(fs, args)
	runs := ops.Runs(cfg.ReportsDir())
	if *list {
		if len(runs) == 0 {
			fmt.Printf("no recorded runs in %s\n", cfg.ReportsDir())
		}
		for _, r := range runs {
			fmt.Println(r)
		}
		return
	}
	dir := *run
	switch {
	case dir == "" && len(runs) == 0:
		cli.Fatal(tool, diag.New("E-AAR-002", "no recorded runs in %s", cfg.ReportsDir()), "rubblesar mission start records one")
	case dir == "":
		dir = filepath.Join(cfg.ReportsDir(), runs[0])
	case !strings.Contains(dir, "/"):
		dir = filepath.Join(cfg.ReportsDir(), dir)
	}
	rep, err := ops.WriteReport(cfg.MapDir(), cfg.Site.Scenario(), dir, cfg.Dispatch.Triage)
	if err != nil {
		cli.Fatal(tool, err)
	}
	fmt.Printf("%s\n  %s\n  %s\n", rep.Summary(), filepath.Join(dir, "index.html"), ops.ReportURL(env(cfg), dir))
}
