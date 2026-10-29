package aar

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Colours. Triage keeps the START colours (always with its name in text);
// series use the reference categorical order (blue, orange, aqua).
var (
	triageColour  = map[string]string{"red": "#d32f2f", "yellow": "#f2b600", "black": "#212121", "grey": "#9e9e9e", "green": "#43a047"}
	rescuedColour = "#2e7d32"
	seriesColour  = []string{"#2a78d6", "#eb6834", "#1baf7a"}
	routeColour   = "#6250d6"
	waitColour    = "#c9c7c1"
)

func triColour(t string) string {
	if c, ok := triageColour[strings.TrimSuffix(strings.ToLower(t), "?")]; ok {
		return c
	}
	return "#9e9e9e"
}

// WriteFiles writes index.html, report.json and summary.json into dir.
func (r *Report) WriteFiles(dir string, o BuildOptions) error {
	page, err := r.HTML(o)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html.tmp"), page, 0o644); err != nil {
		return fmt.Errorf("E-AAR-003: writing the report: %w", err)
	}
	if err := os.Rename(filepath.Join(dir, "index.html.tmp"), filepath.Join(dir, "index.html")); err != nil {
		return fmt.Errorf("E-AAR-003: %w", err)
	}
	if err := writeJSON(filepath.Join(dir, "report.json"), r); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "summary.json"), map[string]any{"title": r.Title(), "summary": r.Summary(), "started": r.Start, "incident": r.Meta.Incident, "run": r.Meta.Run})
}

// HTML renders the report as one self-contained page.
func (r *Report) HTML(o BuildOptions) ([]byte, error) {
	m := r.mapSVG(o.MapDir)
	data := struct {
		R        *Report
		Title    string
		Summary  string
		Map      template.HTML
		MapNote  string
		Timeline template.HTML
		Drones   []droneLegend
	}{R: r, Title: r.Title(), Summary: r.Summary(), Map: template.HTML(m.svg), MapNote: m.note, Timeline: template.HTML(r.timelineSVG())}
	for i, d := range r.Drones {
		data.Drones = append(data.Drones, droneLegend{d.ID, seriesColour[i%len(seriesColour)]})
	}
	var b bytes.Buffer
	if err := pageTmpl.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("E-AAR-003: rendering the report: %w", err)
	}
	return b.Bytes(), nil
}

type droneLegend struct{ ID, Colour string }

// --- The map ------------------------------------------------------------------------------

type mapOut struct{ svg, note string }

func mercN(lat, lon float64) (float64, float64) {
	x := (lon + 180) / 360
	s := math.Sin(lat * math.Pi / 180)
	y := 0.5 - math.Log((1+s)/(1-s))/(4*math.Pi)
	return x, y
}

func (r *Report) mapSVG(mapDir string) mapOut {
	// Everything worth showing, as normalised Web Mercator.
	var pts [][2]float64
	add := func(lat, lon float64) {
		if lat != 0 || lon != 0 {
			x, y := mercN(lat, lon)
			pts = append(pts, [2]float64{x, y})
		}
	}
	for _, c := range r.Casualties {
		add(c.Final.Lat, c.Final.Lon)
	}
	for _, s := range r.Sites {
		add(s.Lat, s.Lon)
	}
	for _, a := range r.Assignments {
		for _, p := range a.Route {
			add(p[0], p[1])
		}
	}
	for _, tr := range r.Tracks {
		for _, p := range tr {
			add(p.Lat, p.Lon)
		}
	}
	add(r.Staging[0], r.Staging[1])
	if len(pts) == 0 {
		return mapOut{note: "Nothing to show on a map (no casualties, routes or drone tracks were recorded)."}
	}
	minX, minY, maxX, maxY := pts[0][0], pts[0][1], pts[0][0], pts[0][1]
	for _, p := range pts {
		minX, maxX = math.Min(minX, p[0]), math.Max(maxX, p[0])
		minY, maxY = math.Min(minY, p[1]), math.Max(maxY, p[1])
	}
	var (
		z          int
		ox, oy     float64 // global pixel of the picture's corner
		w, h       int
		background string
		note       string
	)
	const margin = 50.0
	// The zoom that fits the area into about 1040 x 760 pixels.
	fit := func(maxZ int) int {
		z := 19
		for z > 12 && ((maxX-minX)*256*math.Pow(2, float64(z))+2*margin > 1040 || (maxY-minY)*256*math.Pow(2, float64(z))+2*margin > 760) {
			z--
		}
		return min(z, maxZ)
	}
	if mapDir != "" {
		// The offline map's tiles at that zoom, stitched.
		maxZ := 0
		for zz := 13; zz <= 20; zz++ {
			if st, err := os.Stat(filepath.Join(mapDir, fmt.Sprint(zz))); err == nil && st.IsDir() {
				maxZ = zz
			}
		}
		if maxZ > 0 {
			z = fit(maxZ)
			x0, y0, x1, y1 := frame(minX, minY, maxX, maxY, z, margin)
			img := image.NewRGBA(image.Rect(0, 0, int(x1-x0), int(y1-y0)))
			draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{242, 239, 233, 255}}, image.Point{}, draw.Src)
			tiles := 0
			for tx := int(x0) / 256; tx <= int(x1)/256; tx++ {
				for ty := int(y0) / 256; ty <= int(y1)/256; ty++ {
					f, err := os.Open(filepath.Join(mapDir, fmt.Sprint(z), fmt.Sprint(tx), fmt.Sprintf("%d.png", ty)))
					if err != nil {
						continue
					}
					t, err := png.Decode(f)
					f.Close()
					if err != nil {
						continue
					}
					at := image.Pt(tx*256-int(x0), ty*256-int(y0))
					draw.Draw(img, image.Rectangle{Min: at, Max: at.Add(image.Pt(256, 256))}, t, t.Bounds().Min, draw.Src)
					tiles++
				}
			}
			var pb bytes.Buffer
			if tiles > 0 && png.Encode(&pb, img) == nil {
				ox, oy, w, h = x0, y0, img.Bounds().Dx(), img.Bounds().Dy()
				background = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pb.Bytes())
			}
		}
	}
	if background == "" {
		// No offline map: the same frame on a plain background.
		z = fit(19)
		x0, y0, x1, y1 := frame(minX, minY, maxX, maxY, z, margin)
		ox, oy, w, h = x0, y0, int(x1-x0), int(y1-y0)
		note = "No offline map installed (scripts/zbook/36-map.sh): the background is plain."
	}
	sc := 256 * math.Pow(2, float64(z))
	xy := func(lat, lon float64) (float64, float64) {
		x, y := mercN(lat, lon)
		return x*sc - ox, y*sc - oy
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="map" viewBox="0 0 %d %d" style="max-width:%dpx" role="img" aria-label="Map of the run: casualties, team routes, drone tracks">`, w, h, w)
	if background != "" {
		fmt.Fprintf(&b, `<image href="%s" x="0" y="0" width="%d" height="%d"/>`, background, w, h)
	} else {
		fmt.Fprintf(&b, `<rect width="%d" height="%d" class="mapbg"/>`, w, h)
	}
	for _, ring := range r.Rubble {
		var p []string
		for _, q := range ring {
			x, y := xy(q[0], q[1])
			p = append(p, fmt.Sprintf("%.1f,%.1f", x, y))
		}
		fmt.Fprintf(&b, `<polygon points="%s" fill="rgba(121,85,72,.45)" stroke="#5d4037" stroke-width="1.5"><title>rubble (closes streets)</title></polygon>`, strings.Join(p, " "))
	}
	// Drone tracks, in the order the drones first reported.
	for i, d := range r.Drones {
		tr := r.Tracks[d.ID]
		if len(tr) < 2 {
			continue
		}
		var p []string
		for _, q := range tr {
			x, y := xy(q.Lat, q.Lon)
			p = append(p, fmt.Sprintf("%.1f,%.1f", x, y))
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2" stroke-opacity=".8" stroke-linejoin="round"><title>%s flight path (%s, %.1f km)</title></polyline>`,
			strings.Join(p, " "), seriesColour[i%len(seriesColour)], html.EscapeString(d.ID), html.EscapeString(d.Role), d.DistanceM/1000)
	}
	// Team routes (those that got somewhere).
	for _, a := range r.Assignments {
		if len(a.Route) < 2 || a.OnScene.IsZero() {
			continue
		}
		var p []string
		for _, q := range a.Route {
			x, y := xy(q[0], q[1])
			p = append(p, fmt.Sprintf("%.1f,%.1f", x, y))
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="3" stroke-opacity=".85" stroke-linejoin="round" stroke-linecap="round"><title>%s → %s: %.0f m</title></polyline>`,
			strings.Join(p, " "), routeColour, html.EscapeString(r.callsign(a.Team)), html.EscapeString(a.Cas), a.LengthM)
	}
	if r.Staging != [2]float64{} {
		x, y := xy(r.Staging[0], r.Staging[1])
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="12" height="12" fill="#1565c0" stroke="#fff" stroke-width="2"><title>Command post</title></rect>`, x-6, y-6)
	}
	// Casualties last, on top: rescued green, others in their triage colour.
	cs := append([]*CasRow(nil), r.Casualties...)
	sort.Slice(cs, func(i, j int) bool { return cs[i].NeedsTeam == false && cs[j].NeedsTeam })
	var boxes [][4]float64
	place := func(x, y float64, text string) (float64, float64, bool) {
		w := 7.5*float64(len(text)) + 4
		for _, d := range [][2]float64{{9, -8}, {9, 18}, {-9 - w, -8}, {-9 - w, 18}, {-w / 2, -14}, {-w / 2, 26}} {
			bx := [4]float64{x + d[0], y + d[1] - 13, x + d[0] + w, y + d[1] + 3}
			free := true
			for _, o := range boxes {
				if bx[0] < o[2] && bx[2] > o[0] && bx[1] < o[3] && bx[3] > o[1] {
					free = false
					break
				}
			}
			if free {
				boxes = append(boxes, bx)
				return x + d[0], y + d[1], true
			}
		}
		return 0, 0, false
	}
	for _, c := range cs {
		x, y := xy(c.Final.Lat, c.Final.Lon)
		boxes = append(boxes, [4]float64{x - 8, y - 8, x + 8, y + 8})
	}
	for _, c := range cs {
		x, y := xy(c.Final.Lat, c.Final.Lon)
		col := triColour(c.Final.Triage)
		state := strings.ToUpper(c.Final.Triage)
		if c.Rescued {
			col, state = rescuedColour, "RESCUED (was "+strings.ToUpper(c.Final.Triage)+")"
		}
		tip := fmt.Sprintf("%s %s: %s", c.ID, state, c.Final.Reason)
		if c.Victim != "" {
			tip += fmt.Sprintf(" | scenario %s, expected %s", c.Victim, strings.ToUpper(c.Expected))
		}
		fmt.Fprintf(&b, `<g><circle cx="%.1f" cy="%.1f" r="7" fill="%s" stroke="#fff" stroke-width="2"/>`, x, y, col)
		if lx, ly, ok := place(x, y, c.ID); ok {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" class="maplabel">%s</text>`, lx, ly, html.EscapeString(c.ID))
		}
		fmt.Fprintf(&b, `<title>%s</title></g>`, html.EscapeString(tip))
	}
	b.WriteString(`</svg>`)
	return mapOut{svg: b.String(), note: note}
}

// frame is the picture's corners in global pixels at zoom z: the area plus a
// margin, and at least 720 x 460 pixels around its centre, for context.
func frame(minX, minY, maxX, maxY float64, z int, margin float64) (x0, y0, x1, y1 float64) {
	sc := 256 * math.Pow(2, float64(z))
	x0, y0 = math.Floor(minX*sc-margin), math.Floor(minY*sc-margin)
	x1, y1 = math.Ceil(maxX*sc+margin), math.Ceil(maxY*sc+margin)
	if d := 720 - (x1 - x0); d > 0 {
		x0, x1 = x0-math.Floor(d/2), x1+math.Ceil(d/2)
	}
	if d := 460 - (y1 - y0); d > 0 {
		y0, y1 = y0-math.Floor(d/2), y1+math.Ceil(d/2)
	}
	return
}

func (r *Report) callsign(team string) string {
	if t := r.findTeam(team); t != nil && t.Callsign != "" {
		return t.Callsign
	}
	return strings.ToUpper(team)
}

// --- The timeline -------------------------------------------------------------------------

func (r *Report) timelineSVG() string {
	var rows []*CasRow
	for _, c := range r.Casualties {
		if c.NeedsTeam || c.Asg != nil {
			rows = append(rows, c)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].FirstSeen.Before(rows[j].FirstSeen) })
	dur := r.End.Sub(r.Start).Seconds()
	if dur <= 0 || (len(rows) == 0 && len(r.Teams) == 0) {
		return `<p class="muted">Nothing to show on a timeline.</p>`
	}
	const left, right, rowH, top = 150.0, 16.0, 20.0, 26.0
	W := 1000.0
	var teams []*TeamRow
	for _, t := range r.Teams {
		if t.Assignments > 0 {
			teams = append(teams, t)
		}
	}
	H := top + float64(len(rows))*rowH + 30 + float64(len(teams))*rowH + 24
	X := func(t time.Time) float64 {
		s := t.Sub(r.Start).Seconds()
		return left + (W-left-right)*math.Max(0, math.Min(1, s/dur))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="timeline" viewBox="0 0 %.0f %.0f" role="img" aria-label="Timeline of each casualty from first sighting to freed">`, W, H)
	// Axis: a tick every 1, 2, 5, 10 or 15 minutes.
	step := 60.0
	for _, s := range []float64{60, 120, 300, 600, 900, 1800} {
		step = s
		if dur/s <= 12 {
			break
		}
	}
	for s := 0.0; s <= dur+1; s += step {
		x := left + (W-left-right)*s/dur
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.0f" x2="%.1f" y2="%.0f" class="grid"/><text x="%.1f" y="16" class="tick">+%s</text>`, x, top-4, x, H-6, x, clock(time.Duration(s)*time.Second))
	}
	seg := func(y float64, a, z time.Time, colour, tip string) {
		if a.IsZero() || z.IsZero() || !z.After(a) {
			return
		}
		x0, x1 := X(a)+1, X(z)-1 // a 2 px surface gap between segments
		if x1-x0 < 1.5 {
			x1 = x0 + 1.5
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="3" fill="%s"><title>%s</title></rect>`, x0, y+3, x1-x0, rowH-6, colour, html.EscapeString(tip))
	}
	for i, c := range rows {
		y := top + float64(i)*rowH
		tri := strings.ToUpper(c.Final.Triage)
		fmt.Fprintf(&b, `<rect x="4" y="%.1f" width="10" height="10" rx="2" fill="%s"/><text x="20" y="%.1f" class="rowlabel">%s %s</text>`, y+5, triColour(c.Final.Triage), y+14, html.EscapeString(c.ID), tri)
		a := c.Asg
		end := r.End
		if a == nil {
			seg(y, c.FirstSeen, end, waitColour, fmt.Sprintf("%s waiting for a team from +%s", c.ID, clock(c.FirstSeen.Sub(r.Start))))
		} else {
			seg(y, c.FirstSeen, a.Created, waitColour, fmt.Sprintf("%s: first seen +%s, team sent +%s", c.ID, clock(c.FirstSeen.Sub(r.Start)), clock(a.Created.Sub(r.Start))))
			reached := a.OnScene
			if reached.IsZero() && a.State == "en_route" {
				reached = end
			}
			seg(y, a.Created, reached, seriesColour[0], fmt.Sprintf("%s on the way to %s: %.0f m (about %s at real speed)", r.callsign(a.Team), c.ID, a.LengthM, span(time.Duration(a.ETAs*float64(time.Second)))))
			if !a.OnScene.IsZero() {
				done := a.Done
				if done.IsZero() {
					done = end
				}
				seg(y, a.OnScene, done, seriesColour[1], fmt.Sprintf("%s freeing %s: %s", r.callsign(a.Team), c.ID, nonEmpty(a.Extrication, "on scene")))
			}
		}
		if !c.Settled.IsZero() {
			fmt.Fprintf(&b, `<path d="M%.1f %.1f l4 5 l-4 5 l-4 -5 z" class="mark"><title>triage settled +%s</title></path>`, X(c.Settled), y, clock(c.Settled.Sub(r.Start)))
		}
		if !c.LookAt.IsZero() {
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="3" class="look"><title>close look done +%s</title></circle>`, X(c.LookAt), y+rowH/2, clock(c.LookAt.Sub(r.Start)))
		}
	}
	ty := top + float64(len(rows))*rowH + 22
	fmt.Fprintf(&b, `<text x="4" y="%.1f" class="section">Teams</text>`, ty-6)
	for i, t := range teams {
		y := ty + float64(i)*rowH
		fmt.Fprintf(&b, `<text x="20" y="%.1f" class="rowlabel">%s</text>`, y+14, html.EscapeString(t.Callsign))
		for _, a := range r.Assignments {
			if a.Team != t.ID {
				continue
			}
			on := a.OnScene
			if on.IsZero() {
				on = a.Done
			}
			if on.IsZero() {
				on = r.End
			}
			seg(y, a.Created, on, seriesColour[0], fmt.Sprintf("%s to %s (%s)", t.Callsign, a.Cas, a.State))
			if !a.OnScene.IsZero() {
				done := a.Done
				if done.IsZero() {
					done = r.End
				}
				seg(y, a.OnScene, done, seriesColour[1], fmt.Sprintf("%s at %s", t.Callsign, a.Cas))
			}
		}
	}
	b.WriteString(`</svg>`)
	return b.String()
}

// --- Page -------------------------------------------------------------------------------------

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"clock": func(r *Report, t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return "+" + clock(t.Sub(r.Start))
	},
	"span":   span,
	"secs":   func(s float64) string { return span(time.Duration(s * float64(time.Second))) },
	"up":     strings.ToUpper,
	"colour": triColour,
	"pct": func(a, b int) string {
		if b == 0 {
			return "-"
		}
		return fmt.Sprintf("%.0f%%", 100*float64(a)/float64(b))
	},
	"km":       func(m float64) string { return fmt.Sprintf("%.1f", m/1000) },
	"local":    func(t time.Time) string { return t.Local().Format("Mon 2 Jan 2006, 15:04:05") },
	"join":     strings.Join,
	"eq":       strings.EqualFold,
	"callsign": func(r *Report, id string) string { return r.callsign(id) },
	"short": func(s string) string {
		if len(s) > 120 {
			return s[:117] + "..."
		}
		return s
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>RubbleSAR after-action report</title>
<style>
:root{--surface:#fcfcfb;--card:#ffffff;--ink:#0b0b0b;--ink2:#52514e;--muted:#7a7873;--line:#e4e2dd;--grid:#ecebe7;--chip:#f3f2ef}
@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){--surface:#1a1a19;--card:#222220;--ink:#f0efec;--ink2:#c3c2b7;--muted:#96958e;--line:#383835;--grid:#2c2c2a;--chip:#2c2c2a}}
:root[data-theme="dark"]{--surface:#1a1a19;--card:#222220;--ink:#f0efec;--ink2:#c3c2b7;--muted:#96958e;--line:#383835;--grid:#2c2c2a;--chip:#2c2c2a}
*{box-sizing:border-box}body{margin:0;background:var(--surface);color:var(--ink);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1100px;margin:0 auto;padding:24px 16px 64px}
h1{font-size:24px;margin:0}h2{font-size:18px;margin:36px 0 10px}.muted{color:var(--muted)}.ink2{color:var(--ink2)}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:10px;margin:18px 0}
.tile{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 14px}
.tile .v{font-size:26px;font-weight:650;line-height:1.2}.tile .l{color:var(--ink2);font-size:13px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px;overflow-x:auto}
svg{display:block;width:100%;height:auto}
.map{border-radius:6px}.mapbg{fill:#f2efe9}
.maplabel{font:600 12px system-ui,sans-serif;fill:#222;paint-order:stroke;stroke:#fff;stroke-width:3px}
svg.timeline{min-width:760px}
.timeline .grid{stroke:var(--grid);stroke-width:1}.timeline .tick{font:11px system-ui,sans-serif;fill:var(--muted);text-anchor:middle}
.timeline .rowlabel{font:12px system-ui,sans-serif;fill:var(--ink)}.timeline .section{font:600 12px system-ui,sans-serif;fill:var(--ink2)}
.timeline .mark{fill:var(--ink)}.timeline .look{fill:var(--card);stroke:var(--ink);stroke-width:1.5}
.legend{display:flex;flex-wrap:wrap;gap:6px 18px;margin:10px 2px 0;font-size:13px;color:var(--ink2)}
.sw{display:inline-block;width:12px;height:12px;border-radius:3px;margin-right:6px;vertical-align:-1px}
.ln{display:inline-block;width:18px;height:3px;border-radius:2px;margin-right:6px;vertical-align:3px}
table{border-collapse:collapse;width:100%;font-size:13px}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);vertical-align:top;white-space:nowrap}
td.wrap{white-space:normal;min-width:240px}th{color:var(--ink2);font-weight:600}
.tri{display:inline-block;padding:0 6px;border-radius:4px;font-size:12px;font-weight:600;color:#fff}
.tri.yellow{color:#111}.tri.grey{color:#111}
details summary{cursor:pointer;color:var(--ink2)}
.notes li{margin:4px 0}
@media print{.card{break-inside:avoid}}
</style></head><body><main>
<h1>After-action report</h1>
<div class="ink2">{{.Title}} · {{.R.Meta.Suite}} · {{local .R.Start}} to {{local .R.End}} ({{span .R.KPI.Duration}})</div>
<div class="muted">{{.Summary}}</div>

<div class="tiles">
 {{if .R.KPI.Victims}}<div class="tile"><div class="v">{{.R.KPI.Found}} / {{.R.KPI.Victims}}</div><div class="l">scenario victims found</div></div>
 <div class="tile"><div class="v">{{.R.KPI.TriageAgree}} / {{.R.KPI.TriageCompared}}</div><div class="l">triage as the scenario expects</div></div>{{end}}
 <div class="tile"><div class="v">{{.R.KPI.Rescued}} / {{.R.KPI.RedYellow}}</div><div class="l">Red and Yellow casualties rescued</div></div>
 <div class="tile"><div class="v">{{span .R.KPI.FoundToFreedMed}}</div><div class="l">first sighting to freed (median, run time)</div></div>
 <div class="tile"><div class="v">{{span .R.KPI.WaitMed}}</div><div class="l">waiting for a free team (median)</div></div>
 <div class="tile"><div class="v">{{span .R.KPI.ExtricationRealMed}}</div><div class="l">freeing and handover (median, real-world estimate)</div></div>
</div>

<h2>Map</h2>
<div class="card">{{.Map}}
<div class="legend">
 <span><span class="sw" style="background:#d32f2f"></span>Red</span><span><span class="sw" style="background:#f2b600"></span>Yellow</span>
 <span><span class="sw" style="background:#212121"></span>Black</span><span><span class="sw" style="background:#9e9e9e"></span>Grey</span>
 <span><span class="sw" style="background:#2e7d32"></span>rescued</span>
 <span><span class="ln" style="background:#6250d6"></span>team route</span>
 {{range .Drones}}<span><span class="ln" style="background:{{.Colour}}"></span>{{.ID}} flight path</span>{{end}}
 <span><span class="sw" style="background:rgba(121,85,72,.6)"></span>rubble</span><span><span class="sw" style="background:#1565c0"></span>command post</span>
</div>{{if .MapNote}}<p class="muted">{{.MapNote}}</p>{{end}}</div>

<h2>Timeline</h2>
<div class="card">{{.Timeline}}
<div class="legend"><span><span class="sw" style="background:#c9c7c1"></span>seen, waiting for a team</span><span><span class="sw" style="background:#2a78d6"></span>team on the way</span>
<span><span class="sw" style="background:#eb6834"></span>team freeing and handing over</span><span>◆ triage settled</span><span>○ close look done</span></div>
<p class="muted">Run time from the start. Simulated teams move {{.R.Meta.TimeScale}}× and free casualties {{.R.Meta.RescueTimeScale}}× faster than real teams; hover a bar for the real-world estimate.</p></div>

<h2>Casualties</h2>
<div class="card"><table>
<tr><th>Record</th><th>Triage</th><th>Scenario</th><th>First seen</th><th>Settled</th><th>Close look</th><th>Team</th><th>Sent</th><th>Reached</th><th>Freed</th><th>Route</th><th>Freeing (real)</th></tr>
{{range .R.Casualties}}<tr>
<td>{{.ID}}{{if .MergedFrom}} <span class="muted">(+{{join .MergedFrom ", "}})</span>{{end}}</td>
<td><span class="tri {{.Final.Triage}}" style="background:{{colour .Final.Triage}}">{{up .Final.Triage}}</span>{{if .Rescued}} <span class="tri" style="background:#2e7d32">RESCUED</span>{{end}}</td>
<td>{{if .Victim}}{{.Victim}}: {{up .Expected}}{{if not (eq .Expected .Final.Triage)}} ≠{{end}} <span class="muted">{{printf "%.1f" .ErrM}} m</span>{{else}}<span class="muted">no victim</span>{{end}}</td>
<td>{{clock $.R .FirstSeen}}</td><td>{{clock $.R .Settled}}</td><td>{{clock $.R .LookAt}}</td>
{{with .Asg}}<td>{{callsign $.R .Team}}</td><td>{{clock $.R .Created}}</td><td>{{clock $.R .OnScene}}</td><td>{{clock $.R .Done}}</td><td>{{printf "%.0f" .LengthM}} m</td><td>{{if .ExtricationS}}{{secs .ExtricationS}}{{else}}-{{end}}</td>
{{else}}<td colspan="6" class="muted">{{if .NeedsTeam}}no team{{else}}no team needed{{end}}</td>{{end}}
</tr>{{end}}</table>
<p class="muted">Times are run time from the start (+mm:ss). "Freeing (real)" is the estimated time to dig the casualty out and hand over, for real rescuers.</p></div>

{{if .R.Victims}}<h2>Against the scenario</h2>
<div class="card"><table><tr><th>Victim</th><th>Site</th><th>Condition</th><th>Expected</th><th>Record</th><th>Given</th><th>Position error</th><th>Rescued</th></tr>
{{range .R.Victims}}<tr><td>{{.ID}}</td><td>{{.Site}}</td><td>{{.Status}}</td><td><span class="tri {{.Expected}}" style="background:{{colour .Expected}}">{{up .Expected}}</span></td>
<td>{{if .Record}}{{.Record}}{{else}}<b>not found</b>{{end}}</td><td>{{if .Given}}<span class="tri {{.Given}}" style="background:{{colour .Given}}">{{up .Given}}</span>{{end}}</td>
<td>{{if .Record}}{{printf "%.1f" .ErrM}} m{{end}}</td><td>{{if .Rescued}}yes{{end}}</td></tr>{{end}}</table>
<p class="muted">A record within 6 m of a victim is that victim's (nearest pairs first). Triage rules are illustrative and await medical review.</p></div>{{end}}

<h2>Teams</h2>
<div class="card"><table><tr><th>Team</th><th>Mode</th><th>Source</th><th>Assignments</th><th>Done</th><th>Cancelled</th><th>Walked/driven</th><th>On the way (run)</th><th>On scene (run)</th></tr>
{{range .R.Teams}}<tr><td>{{.Callsign}}</td><td>{{.Mode}}</td><td>{{.Source}}</td><td>{{.Assignments}}</td><td>{{.Done}}</td><td>{{.Cancelled}}</td><td>{{km .DistanceM}} km</td><td>{{span .EnRoute}}</td><td>{{span .OnScene}}</td></tr>{{end}}</table></div>

<h2>Drones</h2>
<div class="card"><table><tr><th>Drone</th><th>Role</th><th>Airborne</th><th>Flown</th><th>Battery</th><th>Reports</th><th>Close looks</th><th>Software</th></tr>
{{range .R.Drones}}<tr><td>{{.ID}}</td><td>{{.Role}}</td><td>{{span .Airborne}}</td><td>{{km .DistanceM}} km</td><td>{{printf "%.0f" .BatteryStart}}% → {{printf "%.0f" .BatteryEnd}}%</td><td>{{.Reports}}</td><td>{{.Looks}}</td><td class="muted">{{.Software}}</td></tr>{{end}}</table></div>

<h2>Assignments</h2>
<div class="card"><table><tr><th>Assignment</th><th>Casualty</th><th>Team</th><th>Triage then</th><th>State</th><th>Sent</th><th>Reached</th><th>Done</th><th>Route</th><th>Note</th></tr>
{{range .R.Assignments}}<tr><td>{{.ID}}</td><td>{{.Cas}}</td><td>{{callsign $.R .Team}}</td><td>{{up .Triage}}</td><td>{{.State}}</td><td>{{clock $.R .Created}}</td><td>{{clock $.R .OnScene}}</td><td>{{clock $.R .Done}}</td>
<td>{{printf "%.0f" .LengthM}} m{{if .Streets}} <span class="muted">via {{index .Streets 0}}</span>{{end}}</td><td class="wrap muted">{{short .Note}}</td></tr>{{end}}</table></div>

<h2>Chat</h2>
<div class="card"><details><summary>{{len .R.Chats}} messages between SAR-OPS and the teams</summary><table>
{{range .R.Chats}}<tr><td>{{clock $.R .At}}</td><td>{{.From}} → {{.To}}</td><td class="wrap">{{.Text}}</td></tr>{{end}}</table></details></div>

<h2>About this run</h2>
<div class="card"><ul class="notes">
<li>Incident {{.R.Meta.Incident}}, scenario {{.R.Meta.Scenario}}; run {{.R.Meta.Run}} ({{.R.Meta.Suite}}) on {{.R.Meta.Host}}; {{.R.Events}} events recorded.</li>
<li>Teams: {{join .R.Meta.Teams ", "}}.</li>
{{range .R.Meta.Software}}<li class="muted">{{.}}</li>{{end}}
{{range .R.Meta.Notes}}<li>{{.}}</li>{{end}}
{{range .R.Notes}}<li class="muted">{{.}}</li>{{end}}
<li>Everything here is simulated: the incident, the victims and the teams. Triage thresholds and extrication times are illustrative, pending review by medical and USAR practitioners.</li>
<li>Map data © OpenStreetMap contributors (ODbL).</li>
</ul></div>
</main></body></html>
`))
