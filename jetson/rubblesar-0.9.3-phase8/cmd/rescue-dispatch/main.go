// Command rescue-dispatch runs on the ZBook (phase 5). It sends the nearest
// free rescue team to each casualty that needs one (Red first, then Yellow)
// along streets clear of rubble, and puts casualties, drones, teams, routes
// and chat messages on the TAK map (taky; ATAK and WinTAK connect to it).
//
//	rescue-dispatch [run] [--config FILE]   run (normally as a systemd service)
//	rescue-dispatch status [--json]         teams, assignments, TAK link
//	rescue-dispatch reset --yes             forget every assignment (new run)
//	rescue-dispatch route --from LAT,LON --to LAT,LON [--mode foot|vehicle]
//	rescue-dispatch roads                   the street network and the streets closed by rubble
//	rescue-dispatch check-config [--config FILE]
//	rescue-dispatch version
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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"rubblesar/internal/basemap"
	"rubblesar/internal/bus"
	"rubblesar/internal/bus/zenoh"
	"rubblesar/internal/cli"
	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/dispatch"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/logx"
	"rubblesar/internal/roads"
	"rubblesar/internal/version"
)

const tool = "rescue-dispatch"

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
	case "route":
		route(args)
	case "roads":
		roadsInfo(args)
	case "map-layers":
		mapLayers(args)
	case "check-config":
		cfg, path := load(flag.NewFlagSet("check-config", flag.ExitOnError), args)
		d := cfg.Dispatch
		lat, lon := cfg.Staging()
		tak := "off"
		if d.TakServer != "" {
			tak = fmt.Sprintf("%s (plain TCP) as %s, markers every %v (stale after %v), drones every %v", d.TakServer, d.Callsign, d.MarkerRefresh.Duration, d.MarkerStale.Duration, d.DroneEvery.Duration)
		}
		var teams []string
		for _, t := range d.Teams {
			teams = append(teams, fmt.Sprintf("%s (%s, %s, %s)", t.Callsign, t.ID, t.Mode, t.Source))
		}
		scene := fmt.Sprintf("%v on scene", d.OnScene.Duration)
		if d.Extrication {
			scene = fmt.Sprintf("freeing the casualty (15 min..6 h from the scenario), then %v handing over", d.OnScene.Duration)
		}
		ack := "never"
		if d.TakAckTimeout.Duration > 0 {
			ack = d.TakAckTimeout.Duration.String()
		}
		fmt.Printf("OK: %s\n  incident %s, status API %s, state in %s/%s.json\n  streets: %s (rubble from the scenario, closed within %.0f m)\n"+
			"  TAK: %s\n  teams start at %.5f, %.5f: %s\n  dispatch: %s; reached within %.0f m; %s\n"+
			"  simulated teams move %gx and rescue %gx faster than real time; a TAK team that neither answers nor moves loses its casualty after %s\n",
			path, cfg.Site.IncidentID, d.Listen, d.StateDir, cfg.Site.IncidentID, cfg.RoadsPath(), d.BlockMarginM, tak, lat, lon, strings.Join(teams, ", "),
			strings.Join(d.Triage, " first, then ")+" (black and grey get no team)", d.ArriveM, scene, d.TimeScale, d.RescueTimeScale, ack)
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
	fmt.Fprint(os.Stderr, `rescue-dispatch: sends rescue teams to casualties and shows everything on the TAK map (RubbleSAR, ZBook)

  rescue-dispatch [run] [--config FILE]      run (normally the rescue-dispatch service)
  rescue-dispatch status [--json]            teams, assignments, waiting casualties, TAK link, chat
  rescue-dispatch reset --yes                forget every assignment (a new run)
  rescue-dispatch route --from LAT,LON --to LAT,LON [--mode foot|vehicle]
                                             a route on the installed street file (no service needed)
  rescue-dispatch roads                      the street network and the streets closed by rubble
  rescue-dispatch map-layers --out FILE [--extras FILE] [--extra-m 300]
                                             what the offline map draws (streets as routed, rivers,
                                             rubble, collapsed buildings; buildings, water and green
                                             areas from scenario-tool fetch-map) for tools/map/render_map.py
  rescue-dispatch check-config [--config FILE]
  rescue-dispatch version

Default config: /etc/rubblesar/sar.toml (falls back to configs/zbook/sar.toml), section [dispatch].
API: GET /v1/status, GET /v1/state, GET /v1/chats, POST /v1/reset
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
		cli.Fatal(tool, diag.New("E-BUS-002", "rescue-dispatch was built without zenoh"))
	}
	dc := dispatch.FromZBook(cfg, "zbook/"+tool)
	warn, err := dispatch.LoadMap(&dc, cfg.Site.ScenarioDir)
	if err != nil {
		cli.Fatal(tool, err, "config: "+path, "street file: "+dc.RoadsPath, "fix: scripts/zbook/32-roads.sh --force")
	}
	for _, w := range warn {
		log.Warn(w)
	}
	if n := dc.Roads; n != nil {
		log.Info("streets loaded", "source", n.Source, "km", fmt.Sprintf("%.1f", n.LengthM/1000), "ways", n.Ways, "closed_pieces", n.BlockedEdges, "rubble_areas", len(dc.Blockages))
	}
	ctx, stop := cli.SignalContext()
	defer stop()
	ln, err := net.Listen("tcp", cfg.Dispatch.Listen)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-DSP-001", err, "status API on %s", cfg.Dispatch.Listen), "config: "+path)
	}
	var cur atomic.Pointer[dispatch.Service]
	mux := http.NewServeMux()
	ready := func(w http.ResponseWriter) *dispatch.Service {
		s := cur.Load()
		if s == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version.String(tool), "state": "waiting for zenohd at " + cfg.Zenoh.Target()})
		}
		return s
	}
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, _ *http.Request) {
		if s := ready(w); s != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version.String(tool), "state": "running", "dispatch": s.Status()})
		}
	})
	mux.HandleFunc("GET /v1/chats", func(w http.ResponseWriter, _ *http.Request) {
		if s := ready(w); s != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.Chats())
		}
	})
	mux.HandleFunc("GET /v1/state", func(w http.ResponseWriter, _ *http.Request) {
		if s := ready(w); s != nil {
			b, _ := protojson.MarshalOptions{UseProtoNames: true}.Marshal(s.State())
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		}
	})
	mux.HandleFunc("POST /v1/reset", func(w http.ResponseWriter, _ *http.Request) {
		if s := ready(w); s != nil {
			s.Reset()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "run": s.Status().Run})
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
	s, err := dispatch.Open(dc, b, log)
	if err != nil {
		cli.Fatal(tool, err, "config: "+path)
	}
	defer s.Close()
	cur.Store(s)
	log.Info("dispatching", "teams", len(cfg.Dispatch.Teams), "tak", cfg.Dispatch.TakServer, "status", "http://"+cfg.Dispatch.Listen+"/v1/status")
	s.Run(ctx)
	log.Info("shutting down")
}

func get(addr, path string) *http.Response {
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get("http://" + addr + path)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-DSP-001", err, "rescue-dispatch is not answering on %s", addr), "check: sudo systemctl status rescue-dispatch")
	}
	return resp
}

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:9104", "status API address")
	raw := fs.Bool("json", false, "print raw JSON")
	_ = fs.Parse(args)
	resp := get(*addr, "/v1/status")
	defer resp.Body.Close()
	var doc struct {
		Version  string          `json:"version"`
		State    string          `json:"state"`
		Dispatch dispatch.Status `json:"dispatch"`
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		_ = json.NewDecoder(resp.Body).Decode(&doc)
		cli.Fatal(tool, diag.New("E-DSP-001", "rescue-dispatch is running but %s", doc.State), "check: sudo systemctl status zenohd")
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
	fmt.Print(doc.Dispatch.Text())
}

func reset(args []string) {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	addr := fs.String("listen", "127.0.0.1:9104", "status API address")
	yes := fs.Bool("yes", false, "really forget every assignment")
	_ = fs.Parse(args)
	if !*yes {
		fmt.Fprintln(os.Stderr, "rescue-dispatch reset forgets every assignment, sends the simulated teams back to the staging area and clears the TAK map.\nRun again with --yes to do it.")
		os.Exit(cli.ExitSetup)
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Post("http://"+*addr+"/v1/reset", "application/json", nil)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-DSP-001", err, "rescue-dispatch is not answering on %s", *addr), "check: sudo systemctl status rescue-dispatch")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		cli.Fatal(tool, diag.New("E-DSP-001", "reset failed: %s", strings.TrimSpace(string(body))))
	}
	fmt.Println("OK: assignments forgotten")
}

// mapLayers writes the offline map's layers file.
func mapLayers(args []string) {
	var out, extras string
	var extraM float64
	cfg, dc, _ := mapOnly(args, "map-layers", func(fs *flag.FlagSet) {
		fs.StringVar(&out, "out", "", "layers file to write")
		fs.StringVar(&extras, "extras", "", "buildings, water and green areas (scenario-tool fetch-map); \"\" = none")
		fs.Float64Var(&extraM, "extra-m", 300, "the map reaches this far beyond the incident radius")
	})
	if out == "" {
		cli.Fatal(tool, diag.New("E-SETUP-ARG", "--out FILE is required"))
	}
	in := basemap.Input{Incident: cfg.Site.IncidentID, Centre: dc.Centre, RadiusM: cfg.Site.RadiusM + extraM, Staging: dc.Staging,
		StreetFile: dc.RoadsPath, Network: dc.Roads, MarginM: cfg.Dispatch.BlockMarginM, Blockages: dc.Blockages, Scene: dc.Scene}
	if extras != "" {
		b, err := os.ReadFile(extras)
		if err != nil {
			cli.Fatal(tool, diag.Wrap("E-MAP-001", err, "reading %s", extras), "download it: scenario-tool fetch-map (scripts/zbook/36-map.sh)")
		}
		if in.Extras, err = basemap.ParseExtras(b); err != nil {
			cli.Fatal(tool, err, "file: "+extras)
		}
	}
	if dc.Roads == nil {
		fmt.Println("WARNING: no street file: the map has no streets (scripts/zbook/32-roads.sh)")
	}
	L, err := basemap.Build(in)
	if err != nil {
		cli.Fatal(tool, err)
	}
	if err := L.Write(out); err != nil {
		cli.Fatal(tool, err)
	}
	fmt.Printf("wrote %s: %s\n", out, L.Summary())
}

// mapOnly loads the street file and rubble areas like the service does.
func mapOnly(args []string, name string, extra func(fs *flag.FlagSet)) (*config.ZBook, *dispatch.Config, geo.Local) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	if extra != nil {
		extra(fs)
	}
	cfg, path := load(fs, args)
	dc := dispatch.FromZBook(cfg, tool)
	warn, err := dispatch.LoadMap(&dc, cfg.Site.ScenarioDir)
	if err != nil {
		cli.Fatal(tool, err, "config: "+path, "fix: scripts/zbook/32-roads.sh --force")
	}
	for _, w := range warn {
		fmt.Println("WARNING: " + w)
	}
	return cfg, &dc, geo.NewLocal(dc.Centre)
}

func parseLatLon(s string) (geo.Point, error) {
	f := strings.Split(s, ",")
	if len(f) != 2 {
		return geo.Point{}, fmt.Errorf("%q: want LAT,LON", s)
	}
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(f[0]), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return geo.Point{}, fmt.Errorf("%q: want LAT,LON in degrees", s)
	}
	return geo.Point{Lat: lat, Lon: lon}, nil
}

func route(args []string) {
	var from, to, mode string
	_, dc, local := mapOnly(args, "route", func(fs *flag.FlagSet) {
		fs.StringVar(&from, "from", "", "start LAT,LON (default: the staging area)")
		fs.StringVar(&to, "to", "", "casualty LAT,LON")
		fs.StringVar(&mode, "mode", "foot", "foot or vehicle")
	})
	if dc.Roads == nil {
		cli.Fatal(tool, diag.New("E-RTE-001", "no street file at %s", dc.RoadsPath), "fix: scripts/zbook/32-roads.sh")
	}
	a := dc.Staging
	var err error
	if from != "" {
		if a, err = parseLatLon(from); err != nil {
			cli.Fatal(tool, diag.Wrap("E-SETUP-ARG", err, "--from"))
		}
	}
	if to == "" {
		cli.Fatal(tool, diag.New("E-SETUP-ARG", "--to LAT,LON is required"))
	}
	b, err := parseLatLon(to)
	if err != nil {
		cli.Fatal(tool, diag.Wrap("E-SETUP-ARG", err, "--to"))
	}
	ae, an := local.ToLocal(a)
	be, bn := local.ToLocal(b)
	r, err := roads.NewRouter(dc.Roads).Route(roads.Mode(mode), geom.P{X: ae, Y: an}, geom.P{X: be, Y: bn})
	if err != nil {
		cli.Fatal(tool, err)
	}
	fmt.Printf("%s route, %.0f m (straight line %.0f m): %.0f m of street, %.0f m off the streets, %.0f m over rubble; about %.0f min\n",
		r.Mode, r.LengthM, r.DirectM, r.RoadM, r.OffRoadM, r.RubbleM, r.ETAs/60)
	if len(r.Streets) > 0 {
		fmt.Printf("via %s\n", strings.Join(r.Streets, ", "))
	}
	for i, p := range r.Points {
		g := local.ToPoint(p.X, p.Y)
		mark := ""
		switch {
		case i == 0:
			mark = "start"
		case i == r.AccessIdx:
			mark = "leave the street here"
		case i == len(r.Points)-1:
			mark = "casualty"
		}
		fmt.Printf("  %3d  %.6f, %.6f  %5.0f s  %s\n", i, g.Lat, g.Lon, r.T[i], mark)
	}
}

func roadsInfo(args []string) {
	_, dc, local := mapOnly(args, "roads", nil)
	n := dc.Roads
	if n == nil {
		cli.Fatal(tool, diag.New("E-RTE-001", "no street file at %s", dc.RoadsPath), "fix: scripts/zbook/32-roads.sh")
	}
	fmt.Printf("street file %s: %s, %.1f km in %d ways (%d pieces of up to 10 m)\n", dc.RoadsPath, n.Source, n.LengthM/1000, n.Ways, len(n.Edges))
	fmt.Printf("  by class: %s\n", strings.Join(n.ClassCounts(), ", "))
	fmt.Printf("rubble areas: %d; street pieces closed: %d (%.0f m)\n", len(dc.Blockages), n.BlockedEdges, float64(n.BlockedEdges)*10)
	foot, car := 0, 0
	for _, e := range n.Edges {
		if e.NoFoot {
			foot++
		}
		if e.NoVehicle {
			car++
		}
	}
	fmt.Printf("closed in OpenStreetMap: %d pieces to people on foot, %d to vehicles (access tags)\n", foot, car)
	switch {
	case len(n.Water) > 0:
		fmt.Printf("rivers and canals: %d (walks off the streets never cross them)\n", len(n.Water))
	case n.Source == "osm":
		fmt.Println("WARNING: the street file has no rivers (downloaded by 0.6.0): run scripts/zbook/32-roads.sh, which downloads it again")
	}
	r := roads.NewRouter(n)
	se, sn := local.ToLocal(dc.Staging)
	for i, b := range dc.Blockages {
		c := b.Centroid()
		g := local.ToPoint(c.X, c.Y)
		line := fmt.Sprintf("  area %d at %.5f, %.5f:", i+1, g.Lat, g.Lon)
		for _, m := range []roads.Mode{roads.Foot, roads.Vehicle} {
			rt, err := r.Route(m, geom.P{X: se, Y: sn}, c)
			if err != nil {
				line += fmt.Sprintf(" %s: %v;", m, err)
				continue
			}
			line += fmt.Sprintf(" %s from staging %.0f m (%.0f m over rubble) %.0f min;", m, rt.LengthM, rt.RubbleM, rt.ETAs/60)
		}
		fmt.Println(strings.TrimSuffix(line, ";"))
	}
}
