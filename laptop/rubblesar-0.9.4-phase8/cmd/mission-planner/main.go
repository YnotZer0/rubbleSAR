// Command mission-planner runs on the ZBook (phase 4). It reads the casualty
// list from casualty-analyst, sends investigators to the casualties that need
// a close look (nearest free drone with the battery for it, most urgent
// first), follows their progress and publishes the task list.
//
//	mission-planner [run] [--config FILE]   run (normally as a systemd service)
//	mission-planner status [--listen ADDR]  drones and tasks
//	mission-planner reset --yes             forget every task (new run)
//	mission-planner check-config [--config FILE]
//	mission-planner version
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"rubblesar/internal/bus"
	"rubblesar/internal/bus/zenoh"
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/logx"
	"rubblesar/internal/planner"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/version"
)

const tool = "mission-planner"

var defaultConfig = cli.DefaultConfig("/etc/rubblesar/sar.toml", "configs/zbook/sar.toml")

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		run(args)
	case "status":
		status(args)
	case "reset":
		reset(args)
	case "check-config":
		cfg, path := load(flag.NewFlagSet("check-config", flag.ExitOnError), args)
		p := cfg.Planner
		inv := "every drone with role investigator"
		if len(p.Investigators) > 0 {
			inv = strings.Join(p.Investigators, ", ")
		}
		fmt.Printf("OK: %s\n  incident %s, status API %s, tasks kept in %s/%s.json\n"+
			"  investigations: %.0f m above the casualty for %d s, parts within %.0f m count; at most %d looks per casualty\n"+
			"  drones: %s; keep %.0f%% battery; task timeout %v, command timeout %v\n",
			path, cfg.Site.IncidentID, p.Listen, p.StateDir, cfg.Site.IncidentID, p.AltAGLm, p.HoverS, p.RadiusM, p.MaxAttempts,
			inv, p.ReservePct, p.TaskTimeout.Duration, p.CommandTimeout.Duration)
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
	fmt.Fprint(os.Stderr, `mission-planner: sends investigators to the casualties that need a close look (RubbleSAR, ZBook)

  mission-planner [run] [--config FILE]      run (normally the mission-planner service)
  mission-planner status [--json]            drones and investigation tasks
  mission-planner reset --yes                forget every task (a new run)
  mission-planner check-config [--config FILE]
  mission-planner version

Default config: /etc/rubblesar/sar.toml (falls back to configs/zbook/sar.toml), section [planner].
API: GET /v1/status, GET /v1/tasks, POST /v1/reset
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

func run(args []string) {
	cfg, path := load(flag.NewFlagSet("run", flag.ExitOnError), args)
	log := logx.New(tool, cfg.Log.Level, cfg.Log.Format)
	log.Info("starting", "version", version.String(tool), "config", path, "incident", cfg.Site.IncidentID)
	if !zenoh.Available {
		cli.Fatal(tool, diag.New("E-BUS-002", "mission-planner was built without zenoh"))
	}
	ctx, stop := cli.SignalContext()
	defer stop()
	ln, err := net.Listen("tcp", cfg.Planner.Listen)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-PLN-001", err, "status API on %s", cfg.Planner.Listen), "config: "+path)
	}
	var cur atomic.Pointer[planner.Planner]
	mux := http.NewServeMux()
	ready := func(w http.ResponseWriter) *planner.Planner {
		p := cur.Load()
		if p == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version.String(tool), "state": "waiting for zenohd at " + cfg.Zenoh.Target()})
		}
		return p
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		if p := ready(w); p != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version.String(tool), "state": "running", "planner": p.Status()})
		}
	})
	mux.HandleFunc("GET /v1/tasks", func(w http.ResponseWriter, _ *http.Request) {
		if p := ready(w); p != nil {
			b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(&sarpb.TaskList{Tasks: p.Tasks()})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
	})
	mux.HandleFunc("POST /v1/reset", func(w http.ResponseWriter, _ *http.Request) {
		if p := ready(w); p != nil {
			p.Reset()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "run": p.Status().Run})
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	b, err := bus.OpenRetry(ctx, log, cfg.Zenoh.OpenTimeout.Duration, true, zenoh.Opener(cfg.Zenoh))
	if err != nil {
		if errors.Is(err, ctx.Err()) {
			return
		}
		cli.Fatal(tool, err, "config: "+path, "connect: "+cfg.Zenoh.Target())
	}
	defer b.Close()
	p, err := planner.Open(planner.FromZBook(cfg, "zbook/"+tool), b, log)
	if err != nil {
		cli.Fatal(tool, err, "config: "+path)
	}
	defer p.Close()
	cur.Store(p)
	log.Info("planning investigations", "tasks_kept", len(p.Tasks()), "status", "http://"+cfg.Planner.Listen+"/v1/status")
	p.Run(ctx)
	log.Info("shutting down")
}

func get(addr, path string) *http.Response {
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get("http://" + addr + path)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-PLN-001", err, "mission-planner is not answering on %s", addr), "check: sudo systemctl status mission-planner")
	}
	return resp
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:9103", "status API address")
	raw := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)
	resp := get(*addr, "/v1/status")
	defer resp.Body.Close()
	var doc struct {
		Version string         `json:"version"`
		State   string         `json:"state"`
		Planner planner.Status `json:"planner"`
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		cli.Fatal(tool, diag.New("E-PLN-001", "mission-planner is running but %s", doc.State), "check: sudo systemctl status zenohd")
	}
	if *raw {
		var v any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		b, _ := json.MarshalIndent(v, "", "  ")
		fmt.Println(string(b))
		return
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		cli.Fatal(tool, err)
	}
	fmt.Println(doc.Version)
	fmt.Print(doc.Planner.Text())
}

func reset(args []string) {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:9103", "status API address")
	yes := fs.Bool("yes", false, "really forget every task")
	_ = fs.Parse(args)
	if !*yes {
		fmt.Fprintln(os.Stderr, "mission-planner reset forgets every investigation task (drones flying one carry on, unfollowed).\nRun again with --yes to do it.")
		os.Exit(cli.ExitSetup)
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Post("http://"+*addr+"/v1/reset", "application/json", nil)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-PLN-001", err, "mission-planner is not answering on %s", *addr), "check: sudo systemctl status mission-planner")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		cli.Fatal(tool, diag.New("E-PLN-001", "reset failed: %s", strings.TrimSpace(string(body))))
	}
	fmt.Println("OK: tasks forgotten")
}
