package main

import (
	"fmt"
	"html"
	"image"
	"image/draw"
	"math"
	"os"
	"path/filepath"
	"strings"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/vcam"
)

var triageColour = map[string]string{"yellow": "#f2c200", "red": "#d92020", "black": "#151515", "unknown": "#888"}

// preview writes pictures and an index.html for checking the scenario by eye.
func preview(dir, out string) {
	w, err := vcam.Open(dir, false)
	if err != nil {
		fatal(err, "scenario: "+dir)
	}
	s := w.Scn
	for _, sub := range []string{"sites", "victims"} {
		if err := os.MkdirAll(filepath.Join(out, sub), 0o755); err != nil {
			fatal(diag.Wrap("E-RND-001", err, "creating %s", out))
		}
	}
	save := func(rel string, img image.Image) {
		if err := raster.SavePNG(filepath.Join(out, rel), img); err != nil {
			fatal(diag.Wrap("E-RND-001", err, "writing %s", rel))
		}
	}
	fmt.Printf("writing preview of %s to %s ...\n", s.ID, out)

	// Per site: a top view of the layers with labels, and what a scout sees from 40 m.
	for _, st := range s.Sites {
		site, err := render.LoadSite(dir, s, st.ID, false)
		if err != nil {
			fatal(err)
		}
		k := int(math.Ceil(float64(max(site.Base.Rect.W(), site.Base.Rect.H())) / 1000))
		save(filepath.Join("sites", st.ID+"-top.png"), shrink(vcam.Overlay(site.Base.RGB, site.Base.Parts), k))
		f, err := w.Render(vcam.Request{UAV: "scout-1", Pose: vcam.Pose{E: st.CenterE, N: st.CenterN, AltAGL: 40, PitchDeg: -90}, Seed: 1})
		if err != nil {
			fatal(err)
		}
		save(filepath.Join("sites", st.ID+"-scout-rgb.png"), shrink(f.RGB.ToNRGBA(), 2))
		save(filepath.Join("sites", st.ID+"-scout-thermal.png"), vcam.ThermalView(f.Thermal, math.NaN(), math.NaN()))
		fmt.Printf("  site %s\n", st.ID)
	}
	// Per victim: a 12 m investigator view (colour, labels, thermal).
	visible := map[string][]string{}
	for _, v := range s.Victims {
		f, err := w.Render(vcam.Request{UAV: "inv-1", Pose: vcam.Pose{E: v.E, N: v.N, AltAGL: 12, PitchDeg: -90}, Seed: int64(v.Index)})
		if err != nil {
			fatal(err)
		}
		cx, cy := f.RGB.W/2, f.RGB.H/2
		crop := image.Rect(cx-240, cy-240, cx+240, cy+240)
		save(filepath.Join("victims", v.ID+"-rgb.png"), sub(f.RGB.ToNRGBA(), crop))
		save(filepath.Join("victims", v.ID+"-labels.png"), sub(vcam.Overlay(f.RGB, f.PartsRGB), crop))
		tv := vcam.ThermalView(f.Thermal, math.NaN(), math.NaN())
		tx, ty := f.Thermal.W/2, f.Thermal.H/2
		save(filepath.Join("victims", v.ID+"-thermal.png"), grow(sub(tv, image.Rect(tx-120, ty-120, tx+120, ty+120)), 2))
		for _, vis := range f.Meta.Visible {
			if vis.Victim == v.Index {
				visible[v.ID] = append(visible[v.ID], fmt.Sprintf("%s %dpx", vis.ClassName, vis.Pixels))
			}
		}
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), []byte(indexHTML(s, w, visible)), 0o644); err != nil {
		fatal(diag.Wrap("E-RND-001", err, "writing index.html"))
	}
	fmt.Printf("open %s in a web browser\n", filepath.Join(out, "index.html"))
}

func sub(m *image.NRGBA, r image.Rectangle) *image.NRGBA {
	o := image.NewNRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(o, o.Bounds(), m, r.Min, draw.Src)
	return o
}

// shrink box-averages by an integer factor.
func shrink(m *image.NRGBA, k int) *image.NRGBA {
	if k <= 1 {
		return m
	}
	b := m.Bounds()
	w, h := b.Dx()/k, b.Dy()/k
	o := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var s [4]int
			for dy := 0; dy < k; dy++ {
				for dx := 0; dx < k; dx++ {
					i := m.PixOffset(x*k+dx, y*k+dy)
					for c := 0; c < 4; c++ {
						s[c] += int(m.Pix[i+c])
					}
				}
			}
			j := o.PixOffset(x, y)
			for c := 0; c < 4; c++ {
				o.Pix[j+c] = uint8(s[c] / (k * k))
			}
		}
	}
	return o
}

// grow enlarges by an integer factor (nearest neighbour).
func grow(m *image.NRGBA, k int) *image.NRGBA {
	b := m.Bounds()
	o := image.NewNRGBA(image.Rect(0, 0, b.Dx()*k, b.Dy()*k))
	for y := 0; y < b.Dy()*k; y++ {
		for x := 0; x < b.Dx()*k; x++ {
			copy(o.Pix[o.PixOffset(x, y):o.PixOffset(x, y)+4], m.Pix[m.PixOffset(x/k, y/k):m.PixOffset(x/k, y/k)+4])
		}
	}
	return o
}

func indexHTML(s *scenario.Scenario, w *vcam.World, visible map[string][]string) string {
	var b strings.Builder
	esc := html.EscapeString
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>RubbleSAR scenario preview</title><style>
:root{--bg:#f6f5f2;--fg:#1d1d1b;--mut:#6b6a66;--card:#fff;--line:#dedcd6}
@media (prefers-color-scheme:dark){:root{--bg:#191917;--fg:#ecebe6;--mut:#a3a19a;--card:#232320;--line:#3a3935}}
body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 system-ui,sans-serif}
main{max-width:1200px;margin:0 auto;padding:16px}
h1{font-size:22px;margin:8px 0}h2{font-size:17px;margin:28px 0 8px}
.mut{color:var(--mut)}.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(340px,1fr));gap:12px}
.card{background:var(--card);border:1px solid var(--line);border-radius:8px;padding:10px}
.card img{width:100%;height:auto;display:block;border-radius:4px;image-rendering:pixelated}
.row{display:grid;grid-template-columns:repeat(3,1fr);gap:4px}
table{border-collapse:collapse;width:100%;font-size:13px}td,th{border-bottom:1px solid var(--line);padding:4px 6px;text-align:left}
.dot{display:inline-block;width:10px;height:10px;border-radius:50%;border:1px solid #999;vertical-align:middle;margin-right:4px}
svg text{font-family:system-ui,sans-serif;fill:var(--fg)}
.wrap{overflow-x:auto}
</style></head><body><main>`)
	fmt.Fprintf(&b, "<h1>%s</h1><p class=mut>%s · scenario %s · start %s · %d sites · %d victims · <b>simulated data only</b></p>",
		esc(s.Name), esc(s.ID), s.Hash()[:12], esc(s.Start), len(s.Sites), len(s.Victims))

	// Overview map: whole radius, then a close-up of the sites (SVG, north up).
	b.WriteString(`<h2>Overview</h2><div class="card wrap">`)
	b.WriteString(mapSVG(s, -s.RadiusM*1.05, s.RadiusM*1.05, s.RadiusM*2.1, s.RadiusM*2.1, true))
	b.WriteString(`<p class=mut>Whole operating area (0.5 mile radius, rings every 100 m). Grey: collapsed buildings; pink: rubble spill (blocked for routes); dots: victims in their expected START triage colour (hover for details). Each site card below has a close-up.</p></div>`)

	// Victims.
	b.WriteString(`<h2>Victims (seen from 12 m)</h2><div class=grid>`)
	for _, v := range s.Victims {
		var ex []string
		for _, x := range v.Exposed {
			ex = append(ex, x.Kind)
		}
		moves := "still"
		if v.Movement.Moves {
			moves = fmt.Sprintf("moves every %.0f s", v.Movement.PeriodS)
		}
		fmt.Fprintf(&b, `<div class=card><b><span class=dot style="background:%s"></span>%s</b> · %s · %s · %s<br><span class=mut>%s · %s · %.6f, %.6f<br>visible: %s</span>
<div class=row><img src="victims/%s-rgb.png" alt="colour"><img src="victims/%s-labels.png" alt="labels"><img src="victims/%s-thermal.png" alt="thermal"></div></div>`,
			triageColour[v.ExpectedTriage], v.ID, v.SiteID, esc(v.Status), moves, esc(strings.Join(ex, " + ")), v.ExpectedTriage, v.Pos.Lat, v.Pos.Lon,
			esc(strings.Join(visible[v.ID], ", ")), v.ID, v.ID, v.ID)
	}
	b.WriteString(`</div><p class=mut>Each card: colour frame, the same frame with ground-truth labels (red hand, orange arm, blue leg, green head, purple torso), thermal (false colour, auto range). Crops of 12 m nadir frames.</p>`)

	// Sites.
	b.WriteString(`<h2>Sites</h2><div class=grid>`)
	for _, st := range s.Sites {
		m := w.Manifest(st.ID)
		var dec []string
		for _, d := range m.Decoys {
			dec = append(dec, d.Kind)
		}
		bb := st.Poly().Bounds().Grow(st.SpillM + 3)
		span := math.Max(bb.Max.X-bb.Min.X, bb.Max.Y-bb.Min.Y)
		fmt.Fprintf(&b, `<div class=card><b>%s</b> · %s · %s · pile %.1f m · %d victims · decoys: %s<br><span class=mut>layers %s, rendered in %.1f s</span>
%s<img src="sites/%s-top.png" alt="top view"><p class=mut>Scout view from 40 m:</p><img src="sites/%s-scout-rgb.png" alt="scout colour"><img src="sites/%s-scout-thermal.png" alt="scout thermal"></div>`,
			st.ID, st.Kind, esc(st.Source), st.HeightM, len(s.VictimsAt(st.ID)), esc(strings.Join(dec, ", ")), m.Frame, m.RenderSeconds,
			mapSVG(s, (bb.Min.X+bb.Max.X)/2-span/2, (bb.Min.Y+bb.Max.Y)/2+span/2, span, span, false), st.ID, st.ID, st.ID)
	}
	b.WriteString(`</div>`)

	// Table.
	b.WriteString(`<h2>Victim table</h2><div class=wrap><table><tr><th>ID</th><th>Site</th><th>Status</th><th>Triage</th><th>Skin model</th><th>Lat</th><th>Lon</th></tr>`)
	for _, v := range s.Victims {
		model := fmt.Sprintf("alpha %.2f", v.Thermal.Alpha0)
		if v.Thermal.BetaPerH > 0 {
			model += fmt.Sprintf(", -%.3f/h", v.Thermal.BetaPerH)
		}
		if v.Status == "deceased" {
			model = fmt.Sprintf("died %.1f h before start", -v.Thermal.DeathHours)
		}
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td><td>%s</td><td><span class=dot style=\"background:%s\"></span>%s</td><td>%s</td><td>%.7f</td><td>%.7f</td></tr>",
			v.ID, v.SiteID, esc(v.Status), triageColour[v.ExpectedTriage], v.ExpectedTriage, model, v.Pos.Lat, v.Pos.Lon)
	}
	b.WriteString(`</table></div><p class=mut>Everything on this page is simulated. Thermal and status models are illustrative and need review by a medical lead before any operational use.</p></main></body></html>`)
	return b.String()
}

func geoPoint(p scenario.LatLon) geo.Point { return geo.Point{Lat: p.Lat, Lon: p.Lon} }

// mapSVG draws the scenario with the top-left corner at local (e0, n0) metres,
// w x h metres, into an 840 px square.
func mapSVG(s *scenario.Scenario, e0, n0, w, h float64, whole bool) string {
	var b strings.Builder
	sc := 840 / math.Max(w, h)
	px := func(e, n float64) (float64, float64) { return (e - e0) * sc, (n0 - n) * sc }
	// Site close-ups are shown about 360 px wide, so text and dots are drawn larger.
	k := 1.0
	if !whole {
		k = 2.3
	}
	fmt.Fprintf(&b, `<svg viewBox="0 0 840 840" width="100%%" style="max-width:840px;font-size:%.0fpx">`, 12*k)
	cx, cy := px(0, 0)
	if whole {
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="none" stroke="#888" stroke-dasharray="6 4"/>`, cx, cy, s.RadiusM*sc)
		for d := 100.0; d < s.RadiusM; d += 100 {
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="none" stroke="#8884"/>`, cx, cy, d*sc)
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f">%.0f m (0.5 mile)</text>`, cx+4, cy-s.RadiusM*sc-4, s.RadiusM)
	} else {
		// 10 m scale bar.
		fmt.Fprintf(&b, `<line x1="20" y1="820" x2="%.1f" y2="820" stroke="#555" stroke-width="6"/><text x="20" y="800">10 m</text>`, 20+10*sc)
	}
	fmt.Fprintf(&b, `<text x="8" y="%.0f">N &#8593;</text>`, 16*k)
	local := s.Local()
	poly := func(pts [][2]float64, attrs string) {
		var ps []string
		for _, p := range pts {
			x, y := px(p[0], p[1])
			ps = append(ps, fmt.Sprintf("%.1f,%.1f", x, y))
		}
		fmt.Fprintf(&b, `<polygon points="%s" %s/>`, strings.Join(ps, " "), attrs)
	}
	for _, bl := range s.Blockages {
		var pts [][2]float64
		for _, p := range bl.Polygon {
			e, n := local.ToLocal(geoPoint(p))
			pts = append(pts, [2]float64{e, n})
		}
		poly(pts, `fill="#c8504033" stroke="#c85040"`)
	}
	r := 3.5 * k
	for _, st := range s.Sites {
		poly(st.Footprint, `fill="#b8afa5"`)
		x, y := px(st.CenterE, st.CenterN)
		off := 14.0
		if !whole {
			off = 0
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-weight="600">%s</text>`, x+off, y-off, st.ID)
	}
	for _, v := range s.Victims {
		x, y := px(v.E, v.N)
		fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s" stroke="#fff" stroke-width="%.1f"><title>%s %s (%s)</title></circle>`, x, y, r, triageColour[v.ExpectedTriage], k, v.ID, v.Status, v.ExpectedTriage)
		if !whole {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f">%s</text>`, x+r+4, y+10, v.ID)
		}
	}
	if whole {
		fmt.Fprintf(&b, `<path d="M%.1f %.1fh12M%.1f %.1fv12" stroke="#555"/>`, cx-6, cy, cx, cy-6)
	}
	b.WriteString(`</svg>`)
	return b.String()
}
