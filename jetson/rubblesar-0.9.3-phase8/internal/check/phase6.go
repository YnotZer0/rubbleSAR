package check

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/basemap"
	"rubblesar/internal/bus"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/version"
)

// SystemRow is one line of 'rubblesar status' (given to phase 6 by sar-check).
type SystemRow struct {
	Group, Name string
	Fail, Warn  bool
	Detail      string
}

// Phase6Options configure the phase 6 acceptance run.
type Phase6Options struct {
	// P5 is the whole phase 5 run (P5-01..06), recorded for the after-action report.
	P5 Phase5Options
	// System is the state of both machines ('rubblesar status'); nil = not checked.
	System func(ctx context.Context) []SystemRow
	// WebAddr is sar-web (default [web] listen).
	WebAddr string
	// MapDir: the installed offline map (default [web] root/map/<incident>);
	// LayersFile: what it was drawn from (default /var/lib/rubblesar/map/<incident>-layers.json).
	MapDir, LayersFile string
	// ReportsDir: where the run's report goes (default [web] root/reports/<incident>).
	ReportsDir string
	// ScenarioDir for the report's comparison (default [site] scenario_dir).
	ScenarioDir string
	// Chats reads rescue-dispatch's chat (default GET /v1/chats).
	Chats func() ([]aar.ChatLine, error)
	// ExpectSoftware is the version the drones should report (default this build's).
	ExpectSoftware string
	// Report: add the results to this report (phase 7) instead of a new one.
	Report *Report
}

// Phase6 is the phase 6 acceptance: both machines up (P6-01), the offline
// map drawn and served (P6-02), the whole phase 5 run recorded (P5-01..06),
// and its after-action report written, served and consistent (P6-03).
func Phase6(ctx context.Context, o Phase6Options, out io.Writer) *Report {
	cfg := o.P5.P4.Cfg
	inc := cfg.Site.IncidentID
	r := o.Report
	if r == nil {
		r = NewReport("phase6", out)
		r.Share = true
	}
	if o.WebAddr == "" {
		o.WebAddr = cfg.Web.Listen
	}
	if o.MapDir == "" {
		o.MapDir = cfg.MapDir()
	}
	if o.LayersFile == "" {
		o.LayersFile = "/var/lib/rubblesar/map/" + inc + "-layers.json"
	}
	if o.ReportsDir == "" {
		o.ReportsDir = cfg.ReportsDir()
	}
	if o.ScenarioDir == "" {
		o.ScenarioDir = cfg.Site.Scenario()
	}
	if o.ExpectSoftware == "" {
		o.ExpectSoftware = version.Version
	}
	if o.Chats == nil {
		o.Chats = func() ([]aar.ChatLine, error) {
			var lines []aar.ChatLine
			c := http.Client{Timeout: 3 * time.Second}
			resp, err := c.Get("http://" + localAddr(cfg.Dispatch.Listen) + "/v1/chats")
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			return lines, json.NewDecoder(resp.Body).Decode(&lines)
		}
	}
	web := localAddr(o.WebAddr)
	r.Run("P6-01", "both machines up (rubblesar status)", func() Result { return safe("E-CHK-601", func() Result { return p6System(ctx, o) }) })
	r.Run("P6-02", "offline incident map drawn and served", func() Result {
		return safe("E-CHK-602", func() Result { return p6Map(inc, o.MapDir, o.LayersFile, web) })
	})
	// The phase 5 run, recorded from the moment the missions restart.
	stamp := time.Now().UTC().Format("20060102T150405Z")
	runDir := filepath.Join(o.ReportsDir, stamp)
	var (
		rec    *aar.Recorder
		recErr error
		recMu  sync.Mutex
	)
	var lastCas, prevCas *sarpb.CasualtyList
	var casMu sync.Mutex
	casSub, _ := o.P5.P4.Bus.Subscribe(keys.Casualties(inc), func(x bus.Sample) {
		var l sarpb.CasualtyList
		if msg.Unmarshal(x.Payload, &l) == nil {
			casMu.Lock()
			prevCas, lastCas = lastCas, &l
			casMu.Unlock()
		}
	})
	startRec := func() {
		recMu.Lock()
		defer recMu.Unlock()
		if rec != nil {
			return
		}
		host, _ := os.Hostname()
		var teams []string
		for _, t := range cfg.Dispatch.Teams {
			teams = append(teams, fmt.Sprintf("%s (%s, %s)", t.Callsign, t.Mode, t.Source))
		}
		lat, lon := cfg.Staging()
		run := ""
		if o.P5.Dispatch != nil {
			if st, err := o.P5.Dispatch(); err == nil {
				run = st.Run
			}
		} else if st, err := dispatchStatus(cfg.Dispatch.Listen); err == nil {
			run = st.Run
		}
		rec, recErr = aar.Start(aar.Options{Bus: o.P5.P4.Bus, Incident: inc, Dir: runDir, Chats: o.Chats, Meta: aar.Meta{
			Run: run, Suite: "phase 6 acceptance", Host: host, Software: []string{version.Short("sar-check")}, TimeScale: cfg.Dispatch.TimeScale,
			RescueTimeScale: cfg.Dispatch.RescueTimeScale, Teams: teams, Scenario: filepath.Base(o.ScenarioDir), Staging: [2]float64{lat, lon}}})
	}
	p5 := o.P5
	p5.Report = r
	inner := p5.P4.StartedRun
	p5.P4.StartedRun = func() {
		startRec()
		if inner != nil {
			inner()
		}
	}
	if p5.P4.Suite == "" {
		p5.P4.Suite = "phase6"
	}
	if p5.Quick {
		startRec()
	}
	Phase5(ctx, p5, out)
	recMu.Lock()
	rr := rec
	recMu.Unlock()
	r.Run("P6-03", "after-action report", func() Result {
		return safe("E-CHK-603", func() Result {
			if rr == nil {
				if recErr != nil {
					return Result{Status: Fail, Code: "E-CHK-603", Detail: "could not record the run: " + recErr.Error(), Hint: "is " + o.ReportsDir + " writable by you? (scripts/zbook/30-install.sh creates it)"}
				}
				return Result{Status: Fail, Code: "E-CHK-603", Detail: "the run did not start, nothing was recorded"}
			}
			rr.Note("acceptance run over")
			err := rr.Close()
			if casSub != nil {
				_ = casSub.Close()
			}
			if err != nil {
				return Result{Status: Fail, Code: "E-CHK-603", Detail: "recording: " + err.Error()}
			}
			casMu.Lock()
			cl, pl := lastCas, prevCas
			casMu.Unlock()
			return p6Report(o, runDir, web, inc, cl, pl)
		})
	})
	r.Context["report"] = runDir
	return r
}

func localAddr(listen string) string {
	if strings.HasPrefix(listen, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(listen, "0.0.0.0:")
	}
	if strings.HasPrefix(listen, ":") {
		return "127.0.0.1" + listen
	}
	return listen
}

// --- P6-01 -------------------------------------------------------------------------------

func p6System(ctx context.Context, o Phase6Options) Result {
	if o.System == nil {
		return Result{Status: Warn, Code: "E-CHK-601", Detail: "not checked (no status function)"}
	}
	rows := o.System(ctx)
	var ev, bad, warn []string
	jetson, dronesOld := false, 0
	for _, x := range rows {
		state := "ok"
		switch {
		case x.Fail:
			state = "FAIL"
			bad = append(bad, x.Group+" "+x.Name)
		case x.Warn:
			state = "WARN"
			warn = append(warn, x.Group+" "+x.Name)
		}
		ev = append(ev, fmt.Sprintf("%s %s: %s %s", x.Group, x.Name, state, x.Detail))
		if strings.HasPrefix(x.Group, "Jetson") {
			jetson = true
		}
		if x.Group == "Drones" && !x.Fail && !strings.Contains(x.Detail, o.ExpectSoftware) {
			dronesOld++
		}
	}
	detail := fmt.Sprintf("%d services and drones checked: %d down, %d with warnings", len(rows), len(bad), len(warn))
	switch {
	case len(bad) > 0:
		return Result{Status: Fail, Code: "E-CHK-601", Detail: detail + ": " + strings.Join(bad, ", "), Evidence: ev, Hint: "'rubblesar up' starts everything; 'rubblesar status' shows the fixes"}
	case dronesOld > 0:
		return Result{Status: Warn, Code: "E-CHK-601", Detail: detail + fmt.Sprintf("; %d drones run other software than %s", dronesOld, o.ExpectSoftware), Evidence: ev,
			Hint: "update the Jetson: unpack this release there, scripts/jetson/20-build.sh, scripts/jetson/30-install.sh"}
	case !jetson:
		return Result{Status: Warn, Code: "E-CHK-601", Detail: detail + "; the Jetson is not reachable from 'rubblesar' ([ops] jetson not set)", Evidence: ev,
			Hint: "scripts/zbook/38-jetson-link.sh --jetson USER@JETSON-IP lets 'rubblesar up/down' start and stop edge-eye"}
	case len(warn) > 0:
		return Result{Status: Warn, Code: "E-CHK-601", Detail: detail, Evidence: ev, Hint: "'rubblesar status' shows the fixes"}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

// --- P6-02 -------------------------------------------------------------------------------

func mercN(lat, lon float64) (float64, float64) {
	x := (lon + 180) / 360
	s := math.Sin(lat * math.Pi / 180)
	return x, 0.5 - math.Log((1+s)/(1-s))/(4*math.Pi)
}

// tileRange is the renderer's tile range for bounds (min lat, min lon, max lat, max lon).
func tileRange(b [4]float64, z int) (x0, x1, y0, y1 int) {
	ax, ay := mercN(b[2], b[1])
	bx, by := mercN(b[0], b[3])
	n := math.Pow(2, float64(z))
	return int(ax * n), int(bx * n), int(ay * n), int(by * n)
}

// pixelAt fetches the tile under (lat, lon) at zoom z from sar-web and returns the mean colour of the 3x3 pixels there.
func pixelAt(web, inc string, lat, lon float64, z int) ([3]float64, error) {
	x, y := mercN(lat, lon)
	n := math.Pow(2, float64(z))
	tx, ty := int(x*n), int(y*n)
	px, py := int((x*n-float64(tx))*256), int((y*n-float64(ty))*256)
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://%s/map/%s/%d/%d/%d.png", web, inc, z, tx, ty))
	if err != nil {
		return [3]float64{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return [3]float64{}, fmt.Errorf("tile %d/%d/%d: HTTP %d", z, tx, ty, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return [3]float64{}, fmt.Errorf("tile %d/%d/%d: %v", z, tx, ty, err)
	}
	var sum [3]float64
	k := 0.0
	for dx := -1; dx <= 1; dx++ {
		for dy := -1; dy <= 1; dy++ {
			p := image.Pt(min(255, max(0, px+dx)), min(255, max(0, py+dy)))
			r, g, bl, _ := img.At(p.X, p.Y).RGBA()
			sum[0] += float64(r >> 8)
			sum[1] += float64(g >> 8)
			sum[2] += float64(bl >> 8)
			k++
		}
	}
	return [3]float64{sum[0] / k, sum[1] / k, sum[2] / k}, nil
}

func p6Map(inc, dir, layersFile, web string) Result {
	var ev, bad []string
	hint := "scripts/zbook/36-map.sh draws and installs the map"
	b, err := os.ReadFile(layersFile)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-602", Detail: "no offline map: " + err.Error(), Hint: hint}
	}
	var L basemap.Layers
	if err := json.Unmarshal(b, &L); err != nil {
		return Result{Status: Fail, Code: "E-CHK-602", Detail: "the layers file is not readable: " + err.Error(), Hint: hint}
	}
	ev = append(ev, "drawn from: "+L.Summary())
	// Files.
	mb := filepath.Join(dir, inc+".mbtiles")
	if f, err := os.Open(mb); err != nil {
		bad = append(bad, "no MBTiles file "+mb)
	} else {
		head := make([]byte, 16)
		_, _ = io.ReadFull(f, head)
		st, _ := f.Stat()
		f.Close()
		if string(head) != "SQLite format 3\x00" {
			bad = append(bad, mb+" is not an MBTiles (SQLite) file")
		} else {
			ev = append(ev, fmt.Sprintf("MBTiles for WinTAK/ATAK: %s (%.1f MB)", mb, float64(st.Size())/1e6))
		}
	}
	for _, f := range []string{"overview.png", "legend.png"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			bad = append(bad, "missing "+f)
		}
	}
	// Every tile of every zoom drawn.
	var zooms []string
	total := 0
	for z := 13; z <= 20; z++ {
		zd := filepath.Join(dir, fmt.Sprint(z))
		if _, err := os.Stat(zd); err != nil {
			if z <= 18 {
				bad = append(bad, fmt.Sprintf("no zoom %d tiles", z))
			}
			continue
		}
		x0, x1, y0, y1 := tileRange(L.Bounds, z)
		want, have := (x1-x0+1)*(y1-y0+1), 0
		for x := x0; x <= x1; x++ {
			for y := y0; y <= y1; y++ {
				if _, err := os.Stat(filepath.Join(zd, fmt.Sprint(x), fmt.Sprintf("%d.png", y))); err == nil {
					have++
				}
			}
		}
		total += have
		zooms = append(zooms, fmt.Sprintf("z%d %d", z, have))
		if have < want {
			bad = append(bad, fmt.Sprintf("zoom %d: %d of %d tiles", z, have, want))
		}
	}
	ev = append(ev, fmt.Sprintf("tiles: %d (%s)", total, strings.Join(zooms, ", ")))
	// Served: the map source XML and tiles, with what they should show.
	c := http.Client{Timeout: 5 * time.Second}
	if resp, err := c.Get("http://" + web + "/map/" + inc + "/rubblesar-map.xml"); err != nil {
		bad = append(bad, "sar-web not answering on "+web+": "+err.Error())
	} else {
		x, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(x), "/map/"+inc+"/{$z}/{$x}/{$y}.png") {
			bad = append(bad, fmt.Sprintf("map source XML from sar-web: HTTP %d", resp.StatusCode))
		} else {
			ev = append(ev, "map source XML served: http://"+web+"/map/"+inc+"/rubblesar-map.xml")
		}
	}
	// Rubble (browns) or a street it closes (reds): warm, not the land or a white street.
	brown := func(p [3]float64) bool { return p[0]-p[2] > 15 && p[1] <= p[0] }
	red := func(p [3]float64) bool { return p[0] > 170 && p[1] < 130 && p[2] < 130 }
	for i, a := range L.Rubble {
		if i >= 3 || len(a.Pts) < 3 {
			break
		}
		var la, lo float64
		n := len(a.Pts) - 1
		for _, p := range a.Pts[:n] {
			la, lo = la+p[0], lo+p[1]
		}
		px, err := pixelAt(web, inc, la/float64(n), lo/float64(n), 18)
		switch {
		case err != nil:
			bad = append(bad, "rubble tile: "+err.Error())
		case !brown(px):
			bad = append(bad, fmt.Sprintf("rubble area %d: the map shows colour %.0f,%.0f,%.0f at its centre, not rubble brown", i+1, px[0], px[1], px[2]))
		default:
			ev = append(ev, fmt.Sprintf("rubble area %d drawn (colour %.0f,%.0f,%.0f)", i+1, px[0], px[1], px[2]))
		}
	}
	checked, reds := 0, 0
	var notRed []string
	for _, s := range L.Streets {
		if s.State != "rubble" || len(s.Pts) < 2 || checked >= 4 {
			continue
		}
		m := s.Pts[len(s.Pts)/2]
		if len(s.Pts) == 2 {
			m = basemap.LL{(s.Pts[0][0] + s.Pts[1][0]) / 2, (s.Pts[0][1] + s.Pts[1][1]) / 2}
		}
		px, err := pixelAt(web, inc, m[0], m[1], 18)
		checked++
		switch {
		case err != nil:
			bad = append(bad, "street tile: "+err.Error())
		case !red(px):
			// Another street drawn across it can cover the red at this point.
			notRed = append(notRed, fmt.Sprintf("%s (colour %.0f,%.0f,%.0f)", nonEmptyS(s.Name, s.Class), px[0], px[1], px[2]))
		default:
			reds++
			ev = append(ev, fmt.Sprintf("street closed by rubble drawn red: %s", nonEmptyS(s.Name, s.Class)))
		}
	}
	if len(notRed) > 0 {
		ev = append(ev, "not red where checked (another street may cross there): "+strings.Join(notRed, "; "))
		if reds == 0 {
			bad = append(bad, "no street closed by rubble is drawn red")
		}
	}
	detail := fmt.Sprintf("%d tiles, zoom 13 to %d, served by sar-web; %d buildings, %d rubble areas, %d street runs closed by rubble", total, 12+len(zooms), len(L.Buildings), len(L.Rubble), countRubble(L))
	if len(bad) > 0 {
		ev = append(ev, bad...)
		return Result{Status: Fail, Code: "E-CHK-602", Detail: strings.Join(bad, "; "), Evidence: ev, Hint: hint + "; sar-web: sudo systemctl status sar-web"}
	}
	if len(L.Buildings) == 0 {
		return Result{Status: Warn, Code: "E-CHK-602", Detail: detail + "; no buildings (drawn without the download)", Evidence: ev, Hint: "with internet: scripts/zbook/36-map.sh --force"}
	}
	return Result{Status: Pass, Detail: detail, Evidence: ev}
}

func countRubble(L basemap.Layers) int {
	n := 0
	for _, s := range L.Streets {
		if s.State == "rubble" {
			n++
		}
	}
	return n
}

// --- P6-03 -------------------------------------------------------------------------------

func p6Report(o Phase6Options, dir, web, inc string, last, prev *sarpb.CasualtyList) Result {
	cfg := o.P5.P4.Cfg
	rep, err := aar.WriteReport(o.MapDir, o.ScenarioDir, dir, cfg.Dispatch.Triage)
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-603", Detail: "writing the report: " + err.Error()}
	}
	var ev, bad []string
	ev = append(ev, rep.Summary(), "report: "+filepath.Join(dir, "index.html"))
	page, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		return Result{Status: Fail, Code: "E-CHK-603", Detail: err.Error()}
	}
	for _, want := range []string{`class="map"`, `class="timeline"`, "Against the scenario"} {
		if !bytes.Contains(page, []byte(want)) {
			bad = append(bad, "the page has no "+want)
		}
	}
	if !bytes.Contains(page, []byte("data:image/png;base64,")) {
		ev = append(ev, "map without the offline map underneath (scripts/zbook/36-map.sh)")
	}
	// Against what the services say.
	// The last list, or the one before if a list came while the recording stopped.
	if last != nil && len(rep.Casualties) != len(last.Casualties) && (prev == nil || len(rep.Casualties) != len(prev.Casualties)) {
		bad = append(bad, fmt.Sprintf("%d casualties in the report, casualty-analyst lists %d", len(rep.Casualties), len(last.Casualties)))
	}
	if o.P5.Dispatch != nil {
		if st, err := o.P5.Dispatch(); err == nil {
			states := map[string]string{}
			for _, a := range st.Assignments {
				states[a.ID] = a.State
			}
			got := map[string]string{}
			for _, a := range rep.Assignments {
				got[a.ID] = strings.ReplaceAll(a.State, "_", " ")
			}
			// The recording stops a moment before this status: a team may have moved on since.
			order := map[string]int{"en route": 1, "on scene": 2, "done": 3, "cancelled": 3}
			missing, differ, later := 0, 0, 0
			for id, s := range states {
				g, ok := got[id]
				switch {
				case !ok:
					missing++
				case g == s:
				case order[g] > 0 && order[g] < order[s]:
					later++
				default:
					differ++
				}
			}
			if later > 0 {
				ev = append(ev, fmt.Sprintf("%d assignments moved on after the recording stopped", later))
			}
			ev = append(ev, fmt.Sprintf("assignments: %d in the report, %d in rescue-dispatch", len(got), len(states)))
			if missing > 0 || differ > 0 {
				bad = append(bad, fmt.Sprintf("assignments: %d missing from the report, %d in another state", missing, differ))
			}
		}
	}
	if len(rep.Chats) == 0 {
		bad = append(bad, "no chat recorded")
	}
	ev = append(ev, fmt.Sprintf("recorded: %d events, %d chat messages, %d drones", rep.Events, len(rep.Chats), len(rep.Drones)))
	// Served by sar-web.
	url := fmt.Sprintf("http://%s/reports/%s/%s/", web, inc, filepath.Base(dir))
	c := http.Client{Timeout: 5 * time.Second}
	if resp, err := c.Get(url); err != nil {
		bad = append(bad, "sar-web: "+err.Error())
	} else {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("After-action report")) {
			bad = append(bad, fmt.Sprintf("sar-web serves the report with HTTP %d", resp.StatusCode))
		} else {
			ev = append(ev, "served: "+strings.Replace(url, "127.0.0.1", "<this ZBook>", 1))
		}
	}
	if len(bad) > 0 {
		ev = append(ev, bad...)
		return Result{Status: Fail, Code: "E-CHK-603", Detail: strings.Join(bad, "; "), Evidence: ev, Hint: "paste the block; the recording is in " + dir}
	}
	return Result{Status: Pass, Detail: rep.Summary(), Evidence: ev}
}
