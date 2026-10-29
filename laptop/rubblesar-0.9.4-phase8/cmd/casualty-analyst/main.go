// Command casualty-analyst runs on the ZBook. It receives every detection
// report from the drones, stores it (metadata, colour and thermal crops) and
// acknowledges it so the drone frees its queue (phase 3). It merges the
// reports into casualty records with a START triage colour, and says which
// need a close look by an investigator (phase 4).
//
//	casualty-analyst [run] [--config FILE]   run (normally as a systemd service)
//	casualty-analyst status [--listen ADDR]  reports per drone and the casualties
//	casualty-analyst reset --yes             archive this run and start afresh
//	casualty-analyst check-config [--config FILE]
//	casualty-analyst version
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

	"rubblesar/internal/analyst"
	"rubblesar/internal/bus"
	"rubblesar/internal/bus/zenoh"
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/intake"
	"rubblesar/internal/logx"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/version"
)

const tool = "casualty-analyst"

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
		f, tr := cfg.Analyst.Fusion, cfg.Analyst.Triage
		fmt.Printf("OK: %s\n  incident %s, store %s (crops %v), acknowledgements every %v, status API %s\n"+
			"  fusion: parts within %.1f m are one person (span %.1f m; one class within %.1f m counts once), matching gate max(%.0f m, %.1f CEPs), list every %v\n"+
			"  triage: warm = skin %.0f-%.0f C and %.1f C above ambient (clothing: from %.0f C and %.1f C above); close look = within %.0f m for %.0f s; moved = %d frames of a hover aimed at it; cooling %.1f C/h over %v; not found after %d looks\n",
			path, cfg.Site.IncidentID, cfg.AnalystDir(), cfg.Analyst.SaveCrops, cfg.Analyst.AckInterval.Duration, cfg.Analyst.Listen,
			f.LinkM, f.SpanM, f.DuplicateM, f.MatchMinM, f.MatchCEPs, f.PublishInterval.Duration,
			tr.SkinMinC, tr.SkinMaxC, tr.WarmDeltaC, tr.ClothedMinC, tr.ClothedDeltaC, tr.CloseRangeM, tr.MinWatchS, tr.MovedFrames, tr.CoolingCPerH, tr.TrendMinSpan.Duration, tr.NotFoundLooks)
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
	fmt.Fprint(os.Stderr, `casualty-analyst: stores detection reports and merges them into triaged casualties (RubbleSAR, ZBook)

  casualty-analyst [run] [--config FILE]      run (normally the casualty-analyst service)
  casualty-analyst status [--json]            reports received per drone, and the casualties
  casualty-analyst reset --yes                move this run's reports to archive/<time>/ and start afresh
  casualty-analyst check-config [--config FILE]
  casualty-analyst version

Default config: /etc/rubblesar/sar.toml (falls back to configs/zbook/sar.toml), sections [analyst],
[analyst.fusion] and [analyst.triage].
Reports are stored in <store_dir>/<incident>/detections.jsonl with crops in crops/<uav>/;
casualty ids are kept in casualties-state.json next to them.
API: GET /v1/status, GET /v1/casualties, POST /v1/reset
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
	log.Info("starting", "version", version.String(tool), "config", path, "incident", cfg.Site.IncidentID, "store", cfg.AnalystDir())
	if !zenoh.Available {
		cli.Fatal(tool, diag.New("E-BUS-002", "casualty-analyst was built without zenoh"))
	}
	ctx, stop := cli.SignalContext()
	defer stop()
	ln, err := net.Listen("tcp", cfg.Analyst.Listen)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-ANL-002", err, "status API on %s", cfg.Analyst.Listen), "config: "+path)
	}
	// The status API answers from the start, so 'casualty-analyst status'
	// can say that it is still waiting for zenohd instead of hanging.
	var cur atomic.Pointer[analyst.Analyst]
	mux := http.NewServeMux()
	ready := func(w http.ResponseWriter) *analyst.Analyst {
		a := cur.Load()
		if a == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version.String(tool), "state": "waiting for zenohd at " + cfg.Zenoh.Target()})
		}
		return a
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		if a := ready(w); a != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version.String(tool), "state": "running", "intake": a.Intake().Stats(), "casualties": a.Stats()})
		}
	})
	mux.HandleFunc("GET /v1/casualties", func(w http.ResponseWriter, _ *http.Request) {
		if a := ready(w); a != nil {
			b, _ := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: false}.Marshal(&sarpb.CasualtyList{Casualties: a.Casualties()})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
	})
	mux.HandleFunc("POST /v1/reset", func(w http.ResponseWriter, r *http.Request) {
		if a := ready(w); a != nil {
			stamp := r.URL.Query().Get("stamp")
			if stamp == "" || strings.ContainsAny(stamp, "/\\.") {
				stamp = time.Now().UTC().Format("20060102T150405Z")
			}
			w.Header().Set("Content-Type", "application/json")
			dst, err := a.Reset(stamp)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"archive": dst})
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
	a, err := analyst.Open(analyst.FromZBook(cfg, "zbook/"+tool), b, log)
	if err != nil {
		cli.Fatal(tool, err, "config: "+path)
	}
	defer a.Close()
	cur.Store(a)
	st := a.Intake().Stats()
	log.Info("receiving detections", "store", st.Dir, "already_stored", st.Loaded, "status", "http://"+cfg.Analyst.Listen+"/v1/status")
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				for id, u := range a.Intake().Stats().UAVs {
					log.Info("detections summary", "uav", id, "stored", u.Stored, "repeats", u.Duplicates, "acks", u.Acked)
				}
				cs := a.Stats()
				log.Info("casualties summary", "count", cs.Count, "by_triage", cs.ByTriage, "need_investigation", cs.NeedLook)
			}
		}
	}()
	a.Run(ctx, cfg.Analyst.Fusion.PublishInterval.Duration)
	log.Info("shutting down")
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:9102", "status API address")
	raw := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get("http://" + *addr + "/v1/status")
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-ANL-002", err, "casualty-analyst is not answering on %s", *addr),
			"check: sudo systemctl status casualty-analyst")
	}
	defer resp.Body.Close()
	var doc struct {
		Version    string                `json:"version"`
		State      string                `json:"state"`
		Intake     intake.Stats          `json:"intake"`
		Casualties analyst.CasualtyStats `json:"casualties"`
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		cli.Fatal(tool, diag.New("E-ANL-002", "casualty-analyst is running but %s", doc.State),
			"check: sudo systemctl status zenohd")
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
	fmt.Print(doc.Intake.Text())
	fmt.Println()
	fmt.Print(doc.Casualties.Text())
}

func reset(args []string) {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:9102", "status API address")
	yes := fs.Bool("yes", false, "really archive this run")
	_ = fs.Parse(args)
	if !*yes {
		fmt.Fprintln(os.Stderr, "casualty-analyst reset moves every stored report, crop and casualty of this incident to archive/<time>/\nand starts with no casualties. Run again with --yes to do it.")
		os.Exit(cli.ExitSetup)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	resp, err := hc.Post("http://"+*addr+"/v1/reset", "application/json", nil)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-ANL-002", err, "casualty-analyst is not answering on %s", *addr), "check: sudo systemctl status casualty-analyst")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var doc struct {
		Archive string `json:"archive"`
		Error   string `json:"error"`
		State   string `json:"state"`
	}
	_ = json.Unmarshal(body, &doc)
	if resp.StatusCode != http.StatusOK {
		cli.Fatal(tool, diag.New("E-ANL-001", "reset failed: %s%s", doc.Error, doc.State))
	}
	fmt.Printf("OK: previous run archived in %s\n", doc.Archive)
}
