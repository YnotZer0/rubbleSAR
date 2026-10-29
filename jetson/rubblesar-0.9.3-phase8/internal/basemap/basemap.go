// Package basemap gathers what the offline incident map shows (phase 6):
// the streets as rescue-dispatch routes over them (closed by rubble or by
// OpenStreetMap's access tags), rivers, water, green areas, buildings, and
// the scenario's collapsed buildings and rubble, into one layers file. The
// tiles are drawn from it by tools/map/render_map.py (offline tooling) and
// served by sar-web; WinTAK and ATAK show them as a map source.
package basemap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/roads"
	"rubblesar/internal/scenario"
)

// Query is the Overpass query for what the map shows besides the streets:
// buildings, water areas and green areas within radius metres.
func Query(lat, lon, radius float64) string {
	a := fmt.Sprintf("(around:%.0f,%.6f,%.6f)", radius, lat, lon)
	return "[out:json][timeout:180];(" +
		`way["building"]` + a + ";" +
		`way["natural"="water"]` + a + ";" +
		`relation["natural"="water"]` + a + ";" +
		`way["waterway"="riverbank"]` + a + ";" +
		`way["leisure"~"^(park|garden|playground|pitch)$"]` + a + ";" +
		`way["landuse"~"^(grass|recreation_ground|cemetery|forest|meadow|village_green)$"]` + a + ";" +
		`way["natural"~"^(wood|scrub|grassland)$"]` + a + ";" +
		");out geom;"
}

// Fetch downloads the map extras (Overpass JSON with geometry).
func Fetch(ctx context.Context, endpoint string, lat, lon, radius float64) ([]byte, error) {
	if endpoint == "" {
		endpoint = roads.DefaultOverpassURL
	}
	ctx, cancel := context.WithTimeout(ctx, 240*time.Second)
	defer cancel()
	form := url.Values{"data": {Query(lat, lon, radius)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, diag.Wrap("E-MAP-001", err, "building request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "rubblesar-scenario-tool (simulation; offline research)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, diag.Wrap("E-MAP-001", err, "POST %s", endpoint)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 400<<20))
	if err != nil {
		return nil, diag.Wrap("E-MAP-001", err, "reading reply from %s", endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(b)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, diag.New("E-MAP-001", "%s answered HTTP %d: %s", endpoint, resp.StatusCode, snippet)
	}
	if _, err := ParseExtras(b); err != nil {
		return nil, err
	}
	return b, nil
}

type osmPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type osmDoc struct {
	Generator string `json:"generator"`
	Elements  []struct {
		Type     string            `json:"type"`
		ID       int64             `json:"id"`
		Tags     map[string]string `json:"tags"`
		Geometry []osmPoint        `json:"geometry"`
		Members  []struct {
			Type     string     `json:"type"`
			Role     string     `json:"role"`
			Geometry []osmPoint `json:"geometry"`
		} `json:"members"`
	} `json:"elements"`
}

// LL is a point as [lat, lon].
type LL [2]float64

// Area is a filled polygon (outer ring), with a name and a kind for labels.
type Area struct {
	Pts  []LL   `json:"pts"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// Extras are the map features besides the streets.
type Extras struct {
	Buildings []Area `json:"buildings"`
	Water     []Area `json:"water"`
	Green     []Area `json:"green"`
}

// ParseExtras reads the Overpass JSON written by Fetch.
func ParseExtras(b []byte) (*Extras, error) {
	var doc osmDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, diag.Wrap("E-MAP-001", err, "not Overpass JSON")
	}
	x := &Extras{}
	for _, e := range doc.Elements {
		t := e.Tags
		switch e.Type {
		case "way":
			ring := closedRing(e.Geometry)
			if ring == nil {
				continue
			}
			a := Area{Pts: ring, Name: strings.TrimSpace(t["name"])}
			switch {
			case t["building"] != "":
				a.Kind = buildingKind(t)
				x.Buildings = append(x.Buildings, a)
			case t["natural"] == "water" || t["waterway"] == "riverbank":
				x.Water = append(x.Water, a)
			default:
				a.Kind = firstNonEmpty(t["leisure"], t["landuse"], t["natural"])
				x.Green = append(x.Green, a)
			}
		case "relation":
			if t["natural"] != "water" {
				continue
			}
			var outer [][]LL
			for _, m := range e.Members {
				if m.Type == "way" && m.Role != "inner" && len(m.Geometry) >= 2 {
					outer = append(outer, toLL(m.Geometry))
				}
			}
			for _, ring := range stitch(outer) {
				x.Water = append(x.Water, Area{Pts: ring, Name: strings.TrimSpace(t["name"])})
			}
		}
	}
	if len(doc.Elements) > 0 && len(x.Buildings)+len(x.Water)+len(x.Green) == 0 {
		return nil, diag.New("E-MAP-001", "the file holds %d elements but no buildings, water or green areas", len(doc.Elements))
	}
	return x, nil
}

func buildingKind(t map[string]string) string {
	switch a := t["amenity"]; {
	case a == "hospital" || a == "clinic" || t["building"] == "hospital" || t["healthcare"] == "hospital":
		return "hospital"
	case a == "place_of_worship" || t["building"] == "mosque" || t["building"] == "church":
		return "worship"
	case a == "school" || a == "university" || t["building"] == "school":
		return "school"
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func toLL(g []osmPoint) []LL {
	out := make([]LL, len(g))
	for i, p := range g {
		out[i] = LL{round7(p.Lat), round7(p.Lon)}
	}
	return out
}

func round7(v float64) float64 { return math.Round(v*1e7) / 1e7 }

// closedRing is the way's polygon, or nil if it is not closed.
func closedRing(g []osmPoint) []LL {
	if len(g) < 4 || g[0] != g[len(g)-1] {
		return nil
	}
	return toLL(g)
}

// stitch joins way fragments end to end into closed rings (multipolygon outers).
func stitch(parts [][]LL) [][]LL {
	var rings [][]LL
	used := make([]bool, len(parts))
	for i := range parts {
		if used[i] {
			continue
		}
		used[i] = true
		ring := append([]LL(nil), parts[i]...)
		for ring[0] != ring[len(ring)-1] {
			grew := false
			for j := range parts {
				if used[j] {
					continue
				}
				p := parts[j]
				end := ring[len(ring)-1]
				switch {
				case p[0] == end:
					ring = append(ring, p[1:]...)
				case p[len(p)-1] == end:
					for k := len(p) - 2; k >= 0; k-- {
						ring = append(ring, p[k])
					}
				default:
					continue
				}
				used[j], grew = true, true
				break
			}
			if !grew {
				break
			}
		}
		if len(ring) >= 4 && ring[0] == ring[len(ring)-1] {
			rings = append(rings, ring)
		}
	}
	return rings
}

// Street is a run of street with one state.
type Street struct {
	Class string `json:"cls"`
	Name  string `json:"name,omitempty"`
	// State: "" open, "rubble" closed by rubble, "closed" closed in
	// OpenStreetMap, "nofoot" closed on foot, "novehicle" closed to vehicles.
	State  string `json:"state,omitempty"`
	Bridge bool   `json:"bridge,omitempty"`
	Pts    []LL   `json:"pts"`
}

// SiteNameM: a collapse site is named after a street at most this far away.
const SiteNameM = 120

// Site is a collapsed building of the scenario.
type Site struct {
	ID string `json:"id"`
	// Name: after the nearest street ("Gündüz Caddesi"; "" = none near).
	Name    string  `json:"name,omitempty"`
	Kind    string  `json:"kind"`
	HeightM float64 `json:"height_m"`
	Pts     []LL    `json:"pts"`
	Victims int     `json:"victims"`
}

// Layers is everything the map draws, in WGS84.
type Layers struct {
	Incident    string     `json:"incident"`
	Centre      LL         `json:"centre"`
	RadiusM     float64    `json:"radius_m"`
	Bounds      [4]float64 `json:"bounds"` // min lat, min lon, max lat, max lon
	Staging     LL         `json:"staging"`
	StreetsFrom string     `json:"streets_from"` // "osm" or "synthetic"
	Streets     []Street   `json:"streets"`
	Rivers      [][]LL     `json:"rivers"`
	Extras
	Rubble      []Area `json:"rubble"`
	Sites       []Site `json:"sites"`
	Attribution string `json:"attribution"`
	Generated   string `json:"generated"`
}

// Input is what Build needs.
type Input struct {
	Incident string
	Centre   geo.Point
	RadiusM  float64
	Staging  geo.Point
	// StreetFile is the Overpass street file rescue-dispatch routes over.
	StreetFile string
	// Network is that file loaded, with the rubble applied (dispatch.LoadMap).
	Network *roads.Network
	MarginM float64
	// Blockages: rubble spill areas, local metres.
	Blockages []geom.Poly
	Scene     *scenario.Scenario
	Extras    *Extras
}

// Build makes the layers.
func Build(in Input) (*Layers, error) {
	local := geo.NewLocal(in.Centre)
	ll := func(p geom.P) LL {
		g := local.ToPoint(p.X, p.Y)
		return LL{round7(g.Lat), round7(g.Lon)}
	}
	L := &Layers{Incident: in.Incident, Centre: LL{in.Centre.Lat, in.Centre.Lon}, RadiusM: in.RadiusM, Staging: LL{in.Staging.Lat, in.Staging.Lon},
		Attribution: "Map data © OpenStreetMap contributors (ODbL); collapsed buildings and rubble simulated (RubbleSAR)",
		Generated:   time.Now().UTC().Format(time.RFC3339)}
	if in.Extras != nil {
		L.Extras = *in.Extras
	}
	if in.Network != nil {
		L.StreetsFrom = in.Network.Source
		b, err := os.ReadFile(in.StreetFile)
		if err != nil {
			return nil, diag.Wrap("E-MAP-002", err, "reading %s", in.StreetFile)
		}
		var doc osmDoc
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, diag.Wrap("E-MAP-002", err, "%s is not Overpass JSON", in.StreetFile)
		}
		for _, e := range doc.Elements {
			if e.Type != "way" || len(e.Geometry) < 2 {
				continue
			}
			if w := e.Tags["waterway"]; w == "river" || w == "canal" {
				L.Rivers = append(L.Rivers, toLL(e.Geometry))
				continue
			}
			cls := e.Tags["highway"]
			if !roads.Routed(cls) || e.Tags["area"] == "yes" {
				continue
			}
			L.Streets = append(L.Streets, streetRuns(e.Tags, e.Geometry, local, in.Network, in.MarginM, ll)...)
		}
	}
	for _, p := range in.Blockages {
		a := Area{Kind: "rubble"}
		for _, q := range p {
			a.Pts = append(a.Pts, ll(q))
		}
		if len(a.Pts) > 0 {
			a.Pts = append(a.Pts, a.Pts[0])
		}
		L.Rubble = append(L.Rubble, a)
	}
	if s := in.Scene; s != nil {
		sl := geo.NewLocal(geo.Point{Lat: s.Center.Lat, Lon: s.Center.Lon})
		count := map[string]int{}
		for _, v := range s.Victims {
			count[v.SiteID]++
		}
		centres := map[string]geom.P{}
		for _, st := range s.Sites {
			x, y := local.ToLocal(geo.Point{Lat: st.Center.Lat, Lon: st.Center.Lon})
			centres[st.ID] = geom.P{X: x, Y: y}
		}
		names := in.Network.SiteNames(centres, SiteNameM)
		for _, st := range s.Sites {
			x := Site{ID: st.ID, Name: names[st.ID], Kind: st.Kind, HeightM: st.HeightM, Victims: count[st.ID]}
			for _, v := range st.Footprint {
				g := sl.ToPoint(v[0], v[1])
				x.Pts = append(x.Pts, LL{round7(g.Lat), round7(g.Lon)})
			}
			if len(x.Pts) > 0 {
				x.Pts = append(x.Pts, x.Pts[0])
			}
			L.Sites = append(L.Sites, x)
		}
	}
	// Bounds: the incident radius plus the street download's margin.
	r := in.RadiusM
	c := local
	sw, ne := c.ToPoint(-r, -r), c.ToPoint(r, r)
	L.Bounds = [4]float64{round7(sw.Lat), round7(sw.Lon), round7(ne.Lat), round7(ne.Lon)}
	sort.SliceStable(L.Streets, func(i, j int) bool { return rank(L.Streets[i].Class) < rank(L.Streets[j].Class) })
	return L, nil
}

// streetRuns cuts a way into pieces as the network does and joins
// consecutive pieces of the same state.
func streetRuns(tags map[string]string, g []osmPoint, local geo.Local, n *roads.Network, margin float64, ll func(geom.P) LL) []Street {
	noFoot, noVehicle := roads.Permissions(tags)
	access := ""
	switch {
	case noFoot && noVehicle:
		access = "closed"
	case noFoot:
		access = "nofoot"
	case noVehicle:
		access = "novehicle"
	}
	name := strings.TrimSpace(tags["name"])
	if name == "" {
		name = strings.TrimSpace(tags["ref"])
	}
	bridge := tags["bridge"] != "" && tags["bridge"] != "no"
	var out []Street
	var cur *Street
	add := func(state string, a, b geom.P) {
		if cur == nil || cur.State != state {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &Street{Class: tags["highway"], Name: name, State: state, Bridge: bridge, Pts: []LL{ll(a)}}
		}
		cur.Pts = append(cur.Pts, ll(b))
	}
	pts := make([]geom.P, len(g))
	for i, p := range g {
		x, y := local.ToLocal(geo.Point{Lat: p.Lat, Lon: p.Lon})
		pts[i] = geom.P{X: x, Y: y}
	}
	for i := 1; i < len(pts); i++ {
		a, b := pts[i-1], pts[i]
		k := max(1, int(math.Ceil(a.Dist(b)/roads.PieceM)))
		for s := 0; s < k; s++ {
			pa, pb := geom.Lerp(a, b, float64(s)/float64(k)), geom.Lerp(a, b, float64(s+1)/float64(k))
			state := access
			if access != "closed" && n.PieceBlocked(pa, pb, margin) {
				state = "rubble"
			}
			add(state, pa, pb)
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

// rank draws small streets first and big ones on top.
func rank(cls string) int {
	switch strings.TrimSuffix(cls, "_link") {
	case "footway", "path", "steps", "cycleway", "bridleway", "corridor", "track":
		return 0
	case "service", "pedestrian", "living_street":
		return 1
	case "residential", "unclassified", "road":
		return 2
	case "tertiary":
		return 3
	case "secondary":
		return 4
	case "primary":
		return 5
	}
	return 6
}

// Write saves the layers as JSON.
func (L *Layers) Write(path string) error {
	b, err := json.Marshal(L)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return diag.Wrap("E-MAP-002", err, "creating %s", filepath.Dir(path))
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return diag.Wrap("E-MAP-002", err, "writing %s", tmp)
	}
	return os.Rename(tmp, path)
}

// Summary is a one-line account of the layers.
func (L *Layers) Summary() string {
	closed := map[string]int{}
	for _, s := range L.Streets {
		if s.State != "" {
			closed[s.State]++
		}
	}
	return fmt.Sprintf("%d street runs (%s streets; closed: %d by rubble, %d in OpenStreetMap, %d on foot, %d to vehicles), %d rivers, %d water areas, %d green areas, %d buildings, %d rubble areas, %d collapsed buildings",
		len(L.Streets), L.StreetsFrom, closed["rubble"], closed["closed"], closed["nofoot"], closed["novehicle"], len(L.Rivers), len(L.Water), len(L.Green), len(L.Buildings), len(L.Rubble), len(L.Sites))
}
