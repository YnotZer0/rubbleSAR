// Package web is sar-web (phase 6): a read-only web server on the ZBook for
// the offline incident map (tiles for WinTAK and ATAK, the MBTiles file, the
// map source XML) and the after-action reports, with an index page.
package web

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"rubblesar/internal/live"
)

// Map is one incident's offline map.
type Map struct {
	Incident string `json:"incident"`
	Tiles    int    `json:"tiles"`
	MinZoom  int    `json:"min_zoom"`
	MaxZoom  int    `json:"max_zoom"`
	MBTiles  string `json:"mbtiles,omitempty"`
	SizeMB   float64
	Overview bool `json:"overview"`
	Legend   bool `json:"legend"`
	Built    time.Time
}

// ReportInfo is one after-action report.
type ReportInfo struct {
	Incident string    `json:"incident"`
	Run      string    `json:"run"`
	Title    string    `json:"title"`
	Summary  string    `json:"summary"`
	Started  time.Time `json:"started"`
	Path     string    `json:"path"`
}

// Server serves Root.
type Server struct {
	Root    string
	Version string

	// Phase 7, the operator view (all optional).
	// Live is the live picture (nil: the view says it is not running).
	Live Feed
	// Crops serves the detections' crops.
	Crops *live.Crops
	// Scene builds the static scene (incident, map, rubble, teams, model).
	Scene func() *Scene
	// LiveStatus describes the live feed for /v1/status.
	LiveStatus func() map[string]any
	// Phase 8: the recorded runs that can be replayed, and the presenter's cues.
	Replays *Replays
	Present *Presenter

	sceneOnce  sync.Once
	scenes     *sceneCache
	streams    atomic.Int64
	presenters atomic.Int64
	encMu      sync.Mutex
	encs       map[string]*encoded
}

// Handler is the whole site.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	files := http.FileServer(http.Dir(s.Root))
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("GET /map/{incident}/rubblesar-map.xml", s.mapSource)
	s.opsHandlers(mux)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/map/") && strings.HasSuffix(r.URL.Path, ".png") {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			if s.emptyTile(w, r) {
				return
			}
		}
		files.ServeHTTP(w, r)
	})
	return mux
}

// emptyTile answers a tile of an installed map that was not drawn (outside the
// incident area, or a zoom level below the map's) with a plain tile in the
// colour of the map's faded edge, instead of 404, so zooms below the first
// one drawn and the area around show a background rather than errors.
func (s *Server) emptyTile(w http.ResponseWriter, r *http.Request) bool {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/map/"), "/")
	if len(parts) != 4 {
		return false
	}
	z, ez := strconv.Atoi(parts[1])
	x, ex := strconv.Atoi(parts[2])
	y, ey := strconv.Atoi(strings.TrimSuffix(parts[3], ".png"))
	if ez != nil || ex != nil || ey != nil || z < 0 || z > 22 || x < 0 || y < 0 || x >= 1<<z || y >= 1<<z {
		return false
	}
	base := filepath.Join(s.Root, "map", filepath.Base(parts[0]))
	if _, err := os.Stat(filepath.Join(base, parts[1], parts[2], parts[3])); err == nil {
		return false // drawn: the file server sends it
	}
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return false
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(emptyPNG())
	return true
}

var emptyOnce struct {
	sync.Once
	b []byte
}

// emptyPNG is a 256 px tile in the colour render_map.py fades the map to
// outside the downloaded area (land 242,239,233 under 59 % of 226,223,218).
func emptyPNG() []byte {
	emptyOnce.Do(func() {
		im := image.NewRGBA(image.Rect(0, 0, 256, 256))
		draw.Draw(im, im.Bounds(), image.NewUniform(color.RGBA{233, 230, 224, 255}), image.Point{}, draw.Src)
		var b bytes.Buffer
		_ = png.Encode(&b, im)
		emptyOnce.b = b.Bytes()
	})
	return emptyOnce.b
}

// Maps lists the installed maps.
func (s *Server) Maps() []Map {
	var out []Map
	dirs, _ := os.ReadDir(filepath.Join(s.Root, "map"))
	for _, d := range dirs {
		if !d.IsDir() || strings.HasSuffix(d.Name(), ".new") || strings.HasSuffix(d.Name(), ".old") {
			continue
		}
		m := Map{Incident: d.Name(), MinZoom: -1}
		base := filepath.Join(s.Root, "map", d.Name())
		zs, _ := os.ReadDir(base)
		for _, z := range zs {
			var zoom int
			if !z.IsDir() {
				continue
			}
			if _, err := fmt.Sscanf(z.Name(), "%d", &zoom); err != nil {
				continue
			}
			if m.MinZoom < 0 || zoom < m.MinZoom {
				m.MinZoom = zoom
			}
			m.MaxZoom = max(m.MaxZoom, zoom)
			_ = filepath.WalkDir(filepath.Join(base, z.Name()), func(_ string, e os.DirEntry, err error) error {
				if err == nil && !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
					m.Tiles++
				}
				return nil
			})
		}
		if st, err := os.Stat(filepath.Join(base, d.Name()+".mbtiles")); err == nil {
			m.MBTiles = d.Name() + ".mbtiles"
			m.SizeMB = float64(st.Size()) / 1e6
			m.Built = st.ModTime()
		}
		_, err := os.Stat(filepath.Join(base, "overview.png"))
		m.Overview = err == nil
		_, err = os.Stat(filepath.Join(base, "legend.png"))
		m.Legend = err == nil
		if m.Tiles > 0 {
			out = append(out, m)
		}
	}
	return out
}

// Reports lists the after-action reports, newest first.
func (s *Server) Reports() []ReportInfo {
	var out []ReportInfo
	incs, _ := os.ReadDir(filepath.Join(s.Root, "reports"))
	for _, inc := range incs {
		if !inc.IsDir() {
			continue
		}
		runs, _ := os.ReadDir(filepath.Join(s.Root, "reports", inc.Name()))
		for _, r := range runs {
			dir := filepath.Join(s.Root, "reports", inc.Name(), r.Name())
			if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
				continue
			}
			ri := ReportInfo{Incident: inc.Name(), Run: r.Name(), Title: r.Name(), Path: "/reports/" + inc.Name() + "/" + r.Name() + "/"}
			if b, err := os.ReadFile(filepath.Join(dir, "summary.json")); err == nil {
				_ = json.Unmarshal(b, &ri)
				ri.Path = "/reports/" + inc.Name() + "/" + r.Name() + "/"
			}
			out = append(out, ri)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run > out[j].Run })
	return out
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	st := map[string]any{"version": s.Version, "state": "running", "root": s.Root, "maps": s.Maps(), "reports": len(s.Reports()),
		"streams": s.streams.Load(), "presenters": s.presenters.Load()}
	if s.LiveStatus != nil {
		st["live"] = s.LiveStatus()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// MapSourceXML is a WinTAK/ATAK map source reading the tiles from base
// (http://host:port), written exactly like the map sources WinTAK users load
// (github.com/joshuafuller/ATAK-Maps): no comment before the root element and
// the URL as plain text, not CDATA. WinTAK ignored 0.7.1's file, which had
// both. It declares zoom 0 up; sar-web answers the zooms below the first one
// drawn and the area around with a plain tile.
func MapSourceXML(base, incident string, maxZoom int) string {
	var url, name strings.Builder
	_ = xml.EscapeText(&url, []byte(strings.TrimRight(base, "/")+"/map/"+incident+"/{$z}/{$x}/{$y}.png"))
	_ = xml.EscapeText(&name, []byte("RubbleSAR "+incident))
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<customMapSource>
    <name>%s</name>
    <minZoom>0</minZoom>
    <maxZoom>%d</maxZoom>
    <tileType>png</tileType>
    <tileUpdate>None</tileUpdate>
    <url>%s</url>
    <backgroundColor>#e9e6e0</backgroundColor>
</customMapSource>
`, name.String(), maxZoom, url.String())
}

// mapSource writes the XML with the address the device used to reach us.
func (s *Server) mapSource(w http.ResponseWriter, r *http.Request) {
	inc := r.PathValue("incident")
	for _, m := range s.Maps() {
		if m.Incident == inc {
			w.Header().Set("Content-Type", "application/xml")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="rubblesar-%s.xml"`, inc))
			fmt.Fprint(w, MapSourceXML("http://"+hostOf(r), inc, m.MaxZoom))
			return
		}
	}
	http.NotFound(w, r)
}

func hostOf(r *http.Request) string {
	if r.Host != "" {
		return r.Host
	}
	return "localhost:8090"
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Version string
		Host    string
		Maps    []Map
		Reports []ReportInfo
	}{s.Version, hostOf(r), s.Maps(), s.Reports()}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Addresses lists this machine's IPv4 addresses on the network (no loopback, no Docker bridges).
func Addresses() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, in := range ifs {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 || strings.HasPrefix(in.Name, "docker") || strings.HasPrefix(in.Name, "br-") || strings.HasPrefix(in.Name, "veth") {
			continue
		}
		addrs, _ := in.Addrs()
		for _, a := range addrs {
			if ip, _, err := net.ParseCIDR(a.String()); err == nil && ip.To4() != nil {
				out = append(out, ip.String())
			}
		}
	}
	return out
}

var indexTmpl = template.Must(template.New("index").Funcs(template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.Local().Format("2 Jan 2006 15:04")
	},
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>RubbleSAR</title>
<style>
:root{--bg:#f6f5f2;--card:#fff;--ink:#222;--muted:#666;--line:#e2dfd8;--accent:#1565c0;--red:#c62828}
@media (prefers-color-scheme:dark){:root{--bg:#16181b;--card:#1f2226;--ink:#e8e6e3;--muted:#9a9a9a;--line:#33373c;--accent:#64a6f0;--red:#ef5350}}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:980px;margin:0 auto;padding:24px 16px 48px}
h1{font-size:22px;margin:0 0 4px}h2{font-size:17px;margin:28px 0 10px}
.muted{color:var(--muted)}a{color:var(--accent)}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin:10px 0}
.map{display:flex;gap:16px;flex-wrap:wrap}.map img{max-width:100%;border-radius:6px;border:1px solid var(--line)}
.map .ov{flex:1 1 380px}.map .info{flex:1 1 260px}
ol{padding-left:20px}li{margin:4px 0}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line);vertical-align:top}
code{background:var(--line);padding:1px 4px;border-radius:4px}
</style></head><body><main>
<h1>RubbleSAR on this ZBook</h1>
<div class="muted">{{.Version}} · simulated incident</div>

<h2>Operator view</h2>
<div class="card"><a href="/ops/"><b>Open the live operator view</b></a>: the incident map with the drones, casualties, rescue teams and their routes, what the drones report, and a timeline, as it happens. For people without ATAK or WinTAK; any browser on this network (<code>http://{{.Host}}/ops/</code>).</div>

<h2>Offline incident map</h2>
{{range .Maps}}<div class="card map">
 {{if .Overview}}<div class="ov"><a href="/map/{{.Incident}}/overview.png"><img src="/map/{{.Incident}}/overview.png" alt="Map of {{.Incident}}"></a></div>{{end}}
 <div class="info">
  <b>{{.Incident}}</b>: {{.Tiles}} tiles, zoom {{.MinZoom}} to {{.MaxZoom}}{{if .Built.IsZero}}{{else}}, drawn {{when .Built}}{{end}}
  <p><b>Streaming from this ZBook</b> (WinTAK, ATAK on this network):<br>
  <a href="/map/{{.Incident}}/rubblesar-map.xml">rubblesar-{{.Incident}}.xml</a> (a map source for <code>http://{{$.Host}}</code>)</p>
  <ol><li>Download it <b>on the WinTAK laptop</b> (it points at the address this page was opened with) and check that <a href="/map/{{.Incident}}/0/0/0.png">this test tile</a> opens there.</li>
  <li>Quit WinTAK, copy the file into <code>C:\ProgramData\WinTAK\Imagery</code>, start WinTAK.</li>
  <li>Maps &amp; Favorites → MOBILE → Online → <b>RubbleSAR {{.Incident}}</b>, then zoom in on the incident.</li></ol>
  {{if .MBTiles}}<p><b>Or offline on the device:</b> <a href="/map/{{.Incident}}/{{.MBTiles}}">{{.MBTiles}}</a> ({{printf "%.0f" .SizeMB}} MB): quit WinTAK, copy it into <code>C:\ProgramData\WinTAK\Imagery</code>, start WinTAK, then MOBILE → Local (works away from the ZBook).</p>{{end}}
  {{if .Legend}}<img src="/map/{{.Incident}}/legend.png" alt="Legend" width="300">{{end}}
 </div></div>
{{else}}<div class="card muted">No map yet: run <code>scripts/zbook/36-map.sh</code> on the ZBook.</div>{{end}}

<h2>After-action reports</h2>
<div class="card">{{if .Reports}}<table><tr><th>Run</th><th>Started</th><th>Summary</th></tr>
{{range .Reports}}<tr><td><a href="{{.Path}}">{{.Title}}</a></td><td>{{when .Started}}</td><td>{{.Summary}}</td></tr>
{{end}}</table>{{else}}<span class="muted">None yet: <code>rubblesar mission start</code> (or <code>scripts/zbook/90-phase6-test.sh</code>) writes one at the end of a run.</span>{{end}}</div>
<p class="muted">Map data © OpenStreetMap contributors (ODbL). Collapses, victims and teams are simulated.</p>
</main></body></html>
`))
