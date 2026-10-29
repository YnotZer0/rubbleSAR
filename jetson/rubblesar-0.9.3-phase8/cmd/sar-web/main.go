// Command sar-web runs on the ZBook (phase 6). It serves the offline incident
// map (tiles for WinTAK and ATAK, the MBTiles file, the map source XML) and
// the after-action reports to the network, read-only. Phase 7: the live
// operator view (/ops/), fed from the bus; phase 8: replays and the
// presenter for the demo director.
//
//	sar-web [run] [--config FILE]   run (normally the sar-web service)
//	sar-web status [--addr ADDR]    what it serves
//	sar-web check-config [--config FILE]
//	sar-web version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/bus/zenoh"
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/live"
	"rubblesar/internal/logx"
	"rubblesar/internal/version"
	"rubblesar/internal/web"
)

const tool = "sar-web"

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
	case "check-config":
		cfg, path := load(flag.NewFlagSet("check-config", flag.ExitOnError), args)
		fmt.Printf("OK: %s\n  serving %s on %s (map: %s, reports: %s)\n", path, cfg.Web.Root, cfg.Web.Listen, cfg.MapDir(), cfg.ReportsDir())
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
	fmt.Fprint(os.Stderr, `sar-web: serves the offline incident map and the after-action reports (RubbleSAR, ZBook)

  sar-web [run] [--config FILE]     run (normally the sar-web service)
  sar-web status [--addr ADDR]      what it serves
  sar-web check-config [--config FILE]
  sar-web version

Default config: /etc/rubblesar/sar.toml (falls back to configs/zbook/sar.toml), section [web].
Pages: / (index), /ops/ (the live operator view), /map/<incident>/..., /reports/<incident>/<run>/
API: GET /v1/status, /v1/scene, /v1/picture, /v1/stream (server-sent events), /v1/crop/<id>, /v1/thermal/<id>
     ?src=replay on the picture, stream and crops: the replay loaded by the demo director
     POST /v1/replay, /v1/present (from this computer only: rubblesar demo)
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
	log.Info("starting", "version", version.String(tool), "config", path, "root", cfg.Web.Root, "listen", cfg.Web.Listen)
	if err := os.MkdirAll(cfg.Web.Root, 0o755); err != nil {
		log.Warn("web root", "err", err)
	}
	ln, err := net.Listen("tcp", cfg.Web.Listen)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-WEB-001", err, "listening on %s", cfg.Web.Listen), "config: "+path, "is another program using the port? sudo ss -ltnp | grep "+portOf(cfg.Web.Listen))
	}
	ctx, stop := cli.SignalContext()
	defer stop()
	s := &web.Server{Root: cfg.Web.Root, Version: version.String(tool), Present: web.NewPresenter(),
		Replays: &web.Replays{Dir: cfg.ReportsDir(), Triage: cfg.Dispatch.Triage}}
	startLive(ctx, cfg, s, log)
	srv := &http.Server{Handler: logged(s.Handler(), log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	for _, a := range web.Addresses() {
		log.Info("serving", "url", "http://"+a+":"+portOf(cfg.Web.Listen)+"/")
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		cli.Fatal(tool, diag.Wrap("E-WEB-001", err, "serving"))
	}
}

func portOf(listen string) string {
	_, p, _ := net.SplitHostPort(listen)
	return p
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// Unwrap lets http.ResponseController flush the event streams through the logger.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logged logs pages and errors, not every tile, crop or picture.
func logged(h http.Handler, log interface{ Info(string, ...any) }) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		quiet := strings.HasSuffix(r.URL.Path, ".png") || strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/ops/")
		if sw.code >= 400 || !quiet {
			log.Info("request", "path", r.URL.Path, "code", sw.code, "from", r.RemoteAddr)
		}
	})
}

// startLive gives the server its live picture: the scene from the scenario,
// the map and the model; the picture from the bus once zenohd answers
// (seeded from the services' APIs); the chat from rescue-dispatch.
func startLive(ctx context.Context, cfg *config.ZBook, s *web.Server, log *slog.Logger) {
	var teams []web.SceneTeam
	for _, t := range cfg.Dispatch.Teams {
		teams = append(teams, web.SceneTeam{ID: t.ID, Callsign: t.Callsign, Mode: t.Mode, Source: t.Source})
	}
	lat, lon := cfg.Staging()
	in := web.SceneInput{Incident: cfg.Site.IncidentID, Version: version.String(tool), Centre: [2]float64{cfg.Site.CenterLat, cfg.Site.CenterLon},
		Staging: [2]float64{lat, lon}, RadiusM: cfg.Site.RadiusM, ScenarioDir: cfg.Site.Scenario(), MapDir: cfg.MapDir(), ModelDir: "/var/lib/rubblesar/models/current",
		LayersFile: "/var/lib/rubblesar/map/" + cfg.Site.IncidentID + "-layers.json",
		Teams:      teams, Triage: cfg.Dispatch.Triage, TimeScale: cfg.Dispatch.TimeScale, Rescue: cfg.Dispatch.RescueTimeScale}
	s.Scene = func() *web.Scene { return web.BuildScene(in) }
	hub := live.NewHub(live.HubOptions{Incident: cfg.Site.IncidentID, Triage: cfg.Dispatch.Triage, Quiet: true})
	s.Live = hub
	s.Crops = &live.Crops{Dir: cfg.AnalystDir()}
	svc := live.Services{Analyst: cfg.Analyst.Listen, Planner: cfg.Planner.Listen, Dispatch: cfg.Dispatch.Listen}
	var feed atomic.Pointer[live.Live]
	s.LiveStatus = func() map[string]any {
		p := hub.Snapshot()
		st := map[string]any{"bus": p.Bus, "drones": len(p.Drones), "casualties": len(p.Casualties), "teams": len(p.Teams), "seq": p.Seq}
		if l := feed.Load(); l != nil {
			rec, rej := l.Received()
			st["received"], st["rejected"] = rec, rej
		}
		return st
	}
	if !zenoh.Available {
		hub.SetBus("not running: this sar-web was built without zenoh")
		return
	}
	hub.SetBus("waiting for zenohd at " + cfg.Zenoh.Target())
	go func() {
		b, err := bus.OpenRetry(ctx, log, cfg.Zenoh.OpenTimeout.Duration, true, zenoh.Opener(cfg.Zenoh))
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("live view: no zenoh session", "err", err, "hint", diag.HintOf(err))
				hub.SetBus("no zenoh session: " + err.Error())
			}
			return
		}
		if errs := svc.Seed(hub); len(errs) > 0 {
			log.Info("live view: services not all answering yet (the picture fills from the bus)", "first", errs[0].Error())
		}
		l, err := live.Start(live.Options{Incident: cfg.Site.IncidentID, Bus: b, Hub: hub, Crops: s.Crops, Chats: svc.Chats})
		if err != nil {
			log.Warn("live view", "err", err)
			hub.SetBus("could not subscribe: " + err.Error())
			_ = b.Close()
			return
		}
		feed.Store(l)
		hub.SetBus("connected")
		log.Info("live view connected", "zenoh", cfg.Zenoh.Target())
		<-ctx.Done()
		l.Close()
		_ = b.Close()
	}()
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("addr", "", "sar-web address (default: [web] listen)")
	path := fs.String("config", defaultConfig, "config file")
	_ = fs.Parse(args)
	a := *addr
	if a == "" {
		cfg, err := config.LoadZBook(*path)
		if err != nil {
			cli.Fatal(tool, err)
		}
		a = strings.Replace(cfg.Web.Listen, "0.0.0.0", "127.0.0.1", 1)
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + a + "/v1/status")
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-WEB-001", err, "sar-web is not answering on %s", a), "check: sudo systemctl status sar-web")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var st struct {
		Version string    `json:"version"`
		Root    string    `json:"root"`
		Maps    []web.Map `json:"maps"`
		Reports int       `json:"reports"`
	}
	if json.Unmarshal(b, &st) != nil {
		cli.Fatal(tool, diag.New("E-WEB-001", "unexpected answer from %s: %.200s", a, b))
	}
	fmt.Printf("%s\nserving %s on %s\n", st.Version, st.Root, a)
	for _, m := range st.Maps {
		fmt.Printf("  map %s: %d tiles, zoom %d-%d%s\n", m.Incident, m.Tiles, m.MinZoom, m.MaxZoom, map[bool]string{true: ", " + m.MBTiles, false: ""}[m.MBTiles != ""])
	}
	if len(st.Maps) == 0 {
		fmt.Println("  no map yet (scripts/zbook/36-map.sh)")
	}
	fmt.Printf("  after-action reports: %d\n", st.Reports)
	for _, ip := range web.Addresses() {
		fmt.Printf("  open http://%s:%s/ on the network\n", ip, portOf(a))
	}
}
