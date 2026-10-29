// Command scenario-tool builds and inspects the simulated incident: it
// generates the scenario, renders the imagery layers, takes virtual camera
// frames, makes previews for checking by eye, and scores detections.
//
// Exit codes: 0 success, 1 a check failed (verify, score thresholds), 2 could not run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"rubblesar/internal/basemap"
	"rubblesar/internal/cli"
	"rubblesar/internal/dataset"
	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/render"
	"rubblesar/internal/roads"
	"rubblesar/internal/scenario"
	"rubblesar/internal/score"
	"rubblesar/internal/vcam"
	"rubblesar/internal/version"
)

const tool = "scenario-tool"

func usage() {
	fmt.Fprint(os.Stderr, `scenario-tool: build and inspect the simulated RubbleSAR incident

  scenario-tool generate  --config FILE --out DIR [--force]   scenario.json from a generator config
  scenario-tool fetch-osm --config FILE --out FILE            download building outlines (internet, ZBook)
  scenario-tool fetch-roads --config FILE --out FILE [--synthetic] [--extra-m 300]
                                                               download the streets for rescue routes (internet, ZBook);
                                                               --synthetic writes a street grid instead (no internet)
  scenario-tool fetch-map --config FILE --out FILE [--extra-m 300]
                                                               download buildings, water and green areas for the
                                                               offline incident map (internet, ZBook)
  scenario-tool render    --scenario DIR [--site ID]           render the imagery layers of every site
  scenario-tool verify    --scenario DIR                       check every layer file against site.json
  scenario-tool info      --scenario DIR                       list sites and victims
  scenario-tool frame     --scenario DIR --out DIR             take one virtual camera frame
                          [--victim vNN | --e M --n M | --lat D --lon D]
                          [--alt 40] [--yaw 0] [--pitch -90] [--t SECONDS] [--uav NAME] [--seed N] [--gps-noise M]
  scenario-tool preview   --scenario DIR [--out DIR]           pictures + index.html for checking by eye
  scenario-tool truth-detections --scenario DIR --out FILE [--noise-m M] [--seed N]
                                                               detections made from the truth (scorer demo)
  scenario-tool score     --scenario DIR --detections FILE [--radius 2] [--json FILE]
  scenario-tool dataset   --out DIR [--config configs/dataset/v1.toml] [--test-scenario DIR]
                          [--limit N] [--frames N] [--keep-work]   training set for the body-part model
  scenario-tool version

DIR is a scenario folder, e.g. data/scenarios/antakya-demo or /var/lib/rubblesar/scenarios/antakya-demo.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(cli.ExitSetup)
	}
	cmd, args := os.Args[1], os.Args[2:]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "configs/scenario/antakya-demo.toml", "generator config")
	out := fs.String("out", "", "output folder or file")
	dir := fs.String("scenario", "", "scenario folder")
	force := fs.Bool("force", false, "generate: replace an existing different scenario")
	site := fs.String("site", "", "render: only this site")
	endpoint := fs.String("endpoint", "", "fetch-osm, fetch-roads: Overpass API URL")
	synthetic := fs.Bool("synthetic", false, "fetch-roads: write a synthetic street grid (no download)")
	extraM := fs.Float64("extra-m", 300, "fetch-roads: streets this far beyond the incident radius")
	victim := fs.String("victim", "", "frame: centre on this victim (e.g. v03)")
	e := fs.Float64("e", math.NaN(), "frame: metres east of the incident centre")
	n := fs.Float64("n", math.NaN(), "frame: metres north of the incident centre")
	lat := fs.Float64("lat", math.NaN(), "frame: latitude")
	lon := fs.Float64("lon", math.NaN(), "frame: longitude")
	alt := fs.Float64("alt", 40, "frame: altitude above ground, m")
	yaw := fs.Float64("yaw", 0, "frame: camera heading, degrees clockwise from north")
	pitch := fs.Float64("pitch", -90, "frame: gimbal pitch, -90 = straight down")
	tOff := fs.Float64("t", 0, "frame: seconds after scenario start")
	uav := fs.String("uav", "scout-1", "frame: drone name")
	seed := fs.Int64("seed", 1, "sensor / detection noise seed")
	gpsNoise := fs.Float64("gps-noise", 0, "frame: reported-position error (1 sigma, m)")
	dets := fs.String("detections", "", "score: detections JSON")
	radius := fs.Float64("radius", 2.0, "score: match radius, m")
	jsonOut := fs.String("json", "", "score: also write the result as JSON")
	noise := fs.Float64("noise-m", 0, "truth-detections: position error (1 sigma, m)")
	limit := fs.Int("limit", 0, "dataset: only this many train and val scenarios (smoke test)")
	frames := fs.Int("frames", 0, "dataset: frames per scenario (default from the config)")
	keepWork := fs.Bool("keep-work", false, "dataset: keep each scenario's rendered layers")
	testScenario := fs.String("test-scenario", "", "dataset: held-out scenario folder (default from the config)")

	switch cmd {
	case "version", "--version":
		fmt.Println(version.String(tool))
		return
	case "help", "-h", "--help":
		usage()
		return
	case "generate", "fetch-osm", "fetch-roads", "fetch-map", "render", "verify", "info", "frame", "preview", "truth-detections", "score", "dataset":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(cli.ExitSetup)
	}
	_ = fs.Parse(args)
	need := func(v *string, flagName string) {
		if *v == "" {
			fmt.Fprintf(os.Stderr, "%s: --%s is required\n\n", cmd, flagName)
			usage()
			os.Exit(cli.ExitSetup)
		}
	}
	switch cmd {
	case "generate":
		need(out, "out")
		generate(*cfgPath, *out, *force)
	case "fetch-osm":
		need(out, "out")
		fetchOSM(*cfgPath, *out, *endpoint)
	case "fetch-roads":
		need(out, "out")
		fetchRoads(*cfgPath, *out, *endpoint, *synthetic, *extraM)
	case "fetch-map":
		need(out, "out")
		fetchMap(*cfgPath, *out, *endpoint, *extraM)
	case "render":
		need(dir, "scenario")
		renderCmd(*dir, *site)
	case "verify":
		need(dir, "scenario")
		verify(*dir)
	case "info":
		need(dir, "scenario")
		info(*dir)
	case "frame":
		need(dir, "scenario")
		need(out, "out")
		frame(*dir, *out, frameArgs{victim: *victim, e: *e, n: *n, lat: *lat, lon: *lon, alt: *alt, yaw: *yaw, pitch: *pitch,
			t: *tOff, uav: *uav, seed: *seed, gpsNoise: *gpsNoise})
	case "preview":
		need(dir, "scenario")
		if *out == "" {
			*out = filepath.Join(*dir, "preview")
		}
		preview(*dir, *out)
	case "truth-detections":
		need(dir, "scenario")
		need(out, "out")
		truthDetections(*dir, *out, *noise, *seed)
	case "dataset":
		need(out, "out")
		cfgSet := false
		fs.Visit(func(f *flag.Flag) { cfgSet = cfgSet || f.Name == "config" })
		if !cfgSet {
			*cfgPath = "configs/dataset/v1.toml"
		}
		datasetCmd(*cfgPath, *out, *testScenario, *limit, *frames, *keepWork)
	case "score":
		need(dir, "scenario")
		need(dets, "detections")
		scoreCmd(*dir, *dets, *radius, *jsonOut)
	}
}

func fatal(err error, ctx ...string) { cli.Fatal(tool, err, ctx...) }

func loadScenario(dir string) *scenario.Scenario {
	s, err := scenario.Load(filepath.Join(dir, "scenario.json"))
	if err != nil {
		fatal(err, "scenario: "+dir)
	}
	return s
}

func generate(cfgPath, out string, force bool) {
	c, err := scenario.LoadConfig(cfgPath)
	if err != nil {
		fatal(err, "config: "+cfgPath)
	}
	s, err := scenario.GenerateFromConfig(c)
	if err != nil {
		fatal(err, "config: "+cfgPath)
	}
	if old, err := scenario.Load(filepath.Join(out, "scenario.json")); err == nil {
		if old.Hash() == s.Hash() {
			fmt.Printf("%s already holds this scenario (%s); nothing to do\n", out, s.Hash()[:12])
			summary(s)
			return
		}
		if !force {
			fatal(diag.New("E-SCN-003", "%s holds a different scenario (%s, new %s); add --force to replace it and delete its layers", out, old.Hash()[:12], s.Hash()[:12]))
		}
		os.RemoveAll(filepath.Join(out, "sites"))
		os.RemoveAll(filepath.Join(out, "preview"))
	}
	p, err := s.Save(out)
	if err != nil {
		fatal(diag.Wrap("E-RND-001", err, "saving scenario"))
	}
	fmt.Printf("wrote %s (sha256 %s)\n", p, s.Hash()[:12])
	summary(s)
}

func summary(s *scenario.Scenario) {
	st := map[string]int{}
	for _, v := range s.Victims {
		st[v.Status]++
	}
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, st[k]))
	}
	fmt.Printf("%s: %d sites, %d victims (%s); start %s\n", s.ID, len(s.Sites), len(s.Victims), strings.Join(parts, ", "), s.Start)
}

func fetchOSM(cfgPath, out, endpoint string) {
	c, err := scenario.LoadConfig(cfgPath)
	if err != nil {
		fatal(err, "config: "+cfgPath)
	}
	fmt.Printf("downloading building outlines within %.0f m of %.5f,%.5f from %s ...\n", c.Site.RadiusM, c.Site.CenterLat, c.Site.CenterLon, orDefault(endpoint, scenario.DefaultOverpassURL))
	b, err := scenario.FetchOverpass(context.Background(), endpoint, c.Site.CenterLat, c.Site.CenterLon, c.Site.RadiusM)
	if err != nil {
		fatal(err)
	}
	fps, _ := scenario.ParseOverpass(b, geo.NewLocal(geo.Point{Lat: c.Site.CenterLat, Lon: c.Site.CenterLon}))
	if err := os.WriteFile(out, b, 0o644); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "writing %s", out))
	}
	fmt.Printf("wrote %s: %d buildings. Set osm_buildings = %q in [sites] of %s, then generate again.\n", out, len(fps), out, cfgPath)
	fmt.Println("Building data (c) OpenStreetMap contributors, ODbL 1.0.")
}

func fetchRoads(cfgPath, out, endpoint string, synthetic bool, extraM float64) {
	c, err := scenario.LoadConfig(cfgPath)
	if err != nil {
		fatal(err, "config: "+cfgPath)
	}
	centre := geo.Point{Lat: c.Site.CenterLat, Lon: c.Site.CenterLon}
	radius := c.Site.RadiusM + math.Max(0, extraM)
	var b []byte
	if synthetic {
		b = roads.Synthetic(centre, radius, 60)
	} else {
		fmt.Printf("downloading the streets within %.0f m of %.5f,%.5f from %s ...\n", radius, c.Site.CenterLat, c.Site.CenterLon, orDefault(endpoint, roads.DefaultOverpassURL))
		if b, err = roads.FetchOverpass(context.Background(), endpoint, c.Site.CenterLat, c.Site.CenterLon, radius); err != nil {
			fatal(err, "no internet on this machine? Use --synthetic for a street grid instead")
		}
	}
	n, err := roads.Parse(b, geo.NewLocal(centre))
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fatal(diag.Wrap("E-RTE-001", err, "creating %s", filepath.Dir(out)))
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fatal(diag.Wrap("E-RTE-001", err, "writing %s", tmp))
	}
	if err := os.Rename(tmp, out); err != nil {
		fatal(diag.Wrap("E-RTE-001", err, "writing %s", out))
	}
	fmt.Printf("wrote %s: %s streets, %.1f km in %d ways (%s); %d rivers and canals\n", out, n.Source, n.LengthM/1000, n.Ways, strings.Join(n.ClassCounts(), ", "), len(n.Water))
	if !synthetic {
		fmt.Println("Street data (c) OpenStreetMap contributors, ODbL 1.0.")
	}
}

func fetchMap(cfgPath, out, endpoint string, extraM float64) {
	c, err := scenario.LoadConfig(cfgPath)
	if err != nil {
		fatal(err, "config: "+cfgPath)
	}
	radius := c.Site.RadiusM + math.Max(0, extraM)
	fmt.Printf("downloading buildings, water and green areas within %.0f m of %.5f,%.5f from %s ...\n", radius, c.Site.CenterLat, c.Site.CenterLon, orDefault(endpoint, roads.DefaultOverpassURL))
	b, err := basemap.Fetch(context.Background(), endpoint, c.Site.CenterLat, c.Site.CenterLon, radius)
	if err != nil {
		fatal(err, "no internet on this machine? The map can be drawn from the streets alone (scripts/zbook/36-map.sh --no-download)")
	}
	x, err := basemap.ParseExtras(b)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fatal(diag.Wrap("E-MAP-001", err, "creating %s", filepath.Dir(out)))
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		fatal(diag.Wrap("E-MAP-001", err, "writing %s", tmp))
	}
	if err := os.Rename(tmp, out); err != nil {
		fatal(diag.Wrap("E-MAP-001", err, "writing %s", out))
	}
	fmt.Printf("wrote %s: %d buildings, %d water areas, %d green areas (%.1f MB)\n", out, len(x.Buildings), len(x.Water), len(x.Green), float64(len(b))/1e6)
	fmt.Println("Map data (c) OpenStreetMap contributors, ODbL 1.0.")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func renderCmd(dir, only string) {
	s := loadScenario(dir)
	t0 := time.Now()
	if only != "" {
		m, err := render.RenderSite(dir, s, only)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("site %s: %s, %d victims, %d pose-B patches, %.1fs\n", only, m.Frame, len(m.Victims), len(m.Patches), m.RenderSeconds)
		return
	}
	fmt.Printf("rendering %d sites of %s at %.1f cm per pixel ...\n", len(s.Sites), s.ID, s.GSDm*100)
	if _, err := render.RenderAll(dir, s, func(msg string) { fmt.Println("  " + msg) }); err != nil {
		fatal(err)
	}
	fmt.Printf("done in %.1fs\n", time.Since(t0).Seconds())
}

func verify(dir string) {
	s := loadScenario(dir)
	bad := 0
	for _, st := range s.Sites {
		site, err := render.LoadSite(dir, s, st.ID, true)
		if err != nil {
			fmt.Printf("  FAIL %s: %v\n", st.ID, err)
			bad++
			continue
		}
		fmt.Printf("  ok   %s: %s, %d files + %d pose-B patches verified\n", st.ID, site.Manifest.Frame, len(site.Manifest.Files), len(site.Manifest.Patches))
	}
	if bad > 0 {
		fmt.Printf("RESULT: FAIL (%d of %d sites)\n", bad, len(s.Sites))
		r := diag.Report{Version: version.String(tool)}
		r.Add("command: %s", strings.Join(os.Args, " "))
		r.Add("%d of %d sites failed verification (E-RND-002/E-RND-003); see the lines above", bad, len(s.Sites))
		fmt.Print(r.Render())
		os.Exit(cli.ExitFail)
	}
	fmt.Printf("RESULT: PASS (%d sites, scenario %s)\n", len(s.Sites), s.Hash()[:12])
}

func info(dir string) {
	s := loadScenario(dir)
	summary(s)
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nSITE\tKIND\tSOURCE\tLAT\tLON\tPILE m\tVICTIMS")
	for _, st := range s.Sites {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.6f\t%.6f\t%.1f\t%d\n", st.ID, st.Kind, st.Source, st.Center.Lat, st.Center.Lon, st.HeightM, len(s.VictimsAt(st.ID)))
	}
	fmt.Fprintln(tw, "\nVICTIM\tSITE\tSTATUS\tTRIAGE\tMOVES\tEXPOSED\tLAT\tLON")
	for _, v := range s.Victims {
		var ex []string
		for _, x := range v.Exposed {
			ex = append(ex, x.Kind)
		}
		moves := "-"
		if v.Movement.Moves {
			moves = fmt.Sprintf("every %.0fs", v.Movement.PeriodS)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.7f\t%.7f\n", v.ID, v.SiteID, v.Status, v.ExpectedTriage, moves, strings.Join(ex, "+"), v.Pos.Lat, v.Pos.Lon)
	}
	tw.Flush()
}

type frameArgs struct {
	victim             string
	e, n, lat, lon     float64
	alt, yaw, pitch, t float64
	uav                string
	seed               int64
	gpsNoise           float64
}

func frame(dir, out string, a frameArgs) {
	w, err := vcam.Open(dir, false)
	if err != nil {
		fatal(err, "scenario: "+dir)
	}
	e, n := a.e, a.n
	switch {
	case a.victim != "":
		found := false
		for _, v := range w.Scn.Victims {
			if v.ID == a.victim {
				e, n, found = v.E, v.N, true
			}
		}
		if !found {
			fatal(diag.New("E-SCN-003", "no victim %q in %s (see 'scenario-tool info')", a.victim, w.Scn.ID))
		}
	case !math.IsNaN(a.lat):
		e, n = w.Scn.Local().ToLocal(geo.Point{Lat: a.lat, Lon: a.lon})
	case math.IsNaN(e) || math.IsNaN(n):
		e, n = w.Scn.Sites[0].CenterE, w.Scn.Sites[0].CenterN
	}
	pose := vcam.Pose{E: e, N: n, AltAGL: a.alt, YawDeg: a.yaw, PitchDeg: a.pitch}
	if a.pitch > -89.9 {
		// Oblique: put the camera behind the target so it looks at it.
		back := a.alt / math.Tan(-a.pitch*math.Pi/180)
		pose.E -= back * math.Sin(a.yaw*math.Pi/180)
		pose.N -= back * math.Cos(a.yaw*math.Pi/180)
	}
	rep := pose
	if a.gpsNoise > 0 {
		rng := rand.New(rand.NewSource(a.seed))
		rep.E += rng.NormFloat64() * a.gpsNoise
		rep.N += rng.NormFloat64() * a.gpsNoise
		rep.AltAGL += rng.NormFloat64() * 0.3
		rep.YawDeg += rng.NormFloat64() * 1.0
	}
	f, err := w.Render(vcam.Request{UAV: a.uav, FrameID: fmt.Sprintf("%s-%.0f", a.uav, a.t*1000), Pose: pose, Reported: rep, TOffsetS: a.t, Seed: a.seed})
	if err != nil {
		fatal(err)
	}
	if err := f.Save(out); err != nil {
		fatal(err)
	}
	fmt.Printf("frame %s: %s, air %.1f C, sun %.0f deg, %.1f cm/px at centre, sites %v, %d visible parts, %.0f ms\n",
		f.Meta.FrameID, f.Meta.Time, f.Meta.AirC, f.Meta.SunElevDeg, f.Meta.GSDCenterM*100, f.Meta.Sites, len(f.Meta.Visible), f.Meta.RenderMS)
	for _, v := range f.Meta.Visible {
		fmt.Printf("  %s %-5s %6d px  bbox %v\n", v.VictimID, v.ClassName, v.Pixels, v.BBox)
	}
	fmt.Printf("wrote %s/{rgb,thermal,thermal-view,overlay,labels-*}.png and frame.json\n", out)
}

func truthDetections(dir, out string, noise float64, seed int64) {
	s := loadScenario(dir)
	var ms []*render.Manifest
	for _, st := range s.Sites {
		m, err := render.LoadManifest(render.SiteDir(dir, st.ID), s)
		if err != nil {
			fatal(err)
		}
		ms = append(ms, m)
	}
	d := score.FromTruth(score.AllTruth(ms), score.DefaultOptions().MinTruthAreaM2)
	rng := rand.New(rand.NewSource(seed))
	local := s.Local()
	for i := range d {
		if noise > 0 {
			e, n := local.ToLocal(geo.Point{Lat: d[i].Lat, Lon: d[i].Lon})
			p := local.ToPoint(e+rng.NormFloat64()*noise, n+rng.NormFloat64()*noise)
			d[i].Lat, d[i].Lon = math.Round(p.Lat*1e7)/1e7, math.Round(p.Lon*1e7)/1e7
		}
	}
	f := score.File{Schema: 1, ScenarioID: s.ID, Source: fmt.Sprintf("truth, %.2f m noise, seed %d", noise, seed), Detections: d}
	b, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(out, append(b, '\n'), 0o644); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "writing %s", out))
	}
	fmt.Printf("wrote %s: %d detections\n", out, len(d))
}

func scoreCmd(dir, detPath string, radius float64, jsonOut string) {
	s := loadScenario(dir)
	f, err := score.LoadFile(detPath)
	if err != nil {
		fatal(err)
	}
	if f.ScenarioID != s.ID {
		fatal(diag.New("E-SCR-001", "%s is for scenario %q, this is %q", detPath, f.ScenarioID, s.ID))
	}
	var ms []*render.Manifest
	for _, st := range s.Sites {
		m, err := render.LoadManifest(render.SiteDir(dir, st.ID), s)
		if err != nil {
			fatal(err)
		}
		ms = append(ms, m)
	}
	o := score.DefaultOptions()
	o.RadiusM = radius
	r := score.Score(s, score.AllTruth(ms), f.Detections, o)
	fmt.Printf("scoring %s (%d detections, source %q) against %s\n", detPath, len(f.Detections), f.Source, s.ID)
	fmt.Print(r.Text())
	if jsonOut != "" {
		b, _ := json.MarshalIndent(r, "", "  ")
		if err := os.WriteFile(jsonOut, append(b, '\n'), 0o644); err != nil {
			fatal(diag.Wrap("E-RND-001", err, "writing %s", jsonOut))
		}
		fmt.Printf("wrote %s\n", jsonOut)
	}
}

func datasetCmd(cfgPath, out, testScenario string, limit, frames int, keep bool) {
	c, err := dataset.Load(cfgPath)
	if err != nil {
		fatal(err, "config: "+cfgPath)
	}
	if testScenario != "" {
		c.Test.Scenario = testScenario
	}
	nt, nv, nf := c.Split.TrainScenarios, c.Split.ValScenarios, c.Frames.PerScenario
	if limit > 0 {
		nt, nv = min(nt, limit), min(nv, limit)
	}
	if frames > 0 {
		nf = frames
	}
	fmt.Printf("building dataset %s into %s (%d train + %d val scenarios, %d frames each, test from %s)\n",
		c.Dataset.Name, out, nt, nv, nf, c.Test.Scenario)
	m, err := dataset.Build(c, dataset.Options{Out: out, Limit: limit, Frames: frames, KeepWork: keep, Log: func(s string) { fmt.Println("  " + s) }})
	if err != nil {
		fatal(err, "config: "+cfgPath, "out: "+out)
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "\nSPLIT\tSCENARIOS\tFRAMES\tTILES\tWITH PARTS\thand\tarm\tleg\thead\ttorso")
	for _, sp := range []string{"train", "val", "test"} {
		st := m.Splits[sp]
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n", sp, st.Scenarios, st.Frames, st.Tiles, st.PositiveTiles,
			st.Instances["hand"], st.Instances["arm"], st.Instances["leg"], st.Instances["head"], st.Instances["torso"])
	}
	tw.Flush()
	fmt.Printf("\nwrote %s/data.yaml, manifest.json and preview.png (%.0f s)\n", out, m.Seconds)
}
