// Package roads routes rescue teams over the street network around the
// incident (OpenStreetMap streets downloaded once, or a synthetic grid),
// around the rubble spill areas that block them.
//
// A route has three parts: from the team's position to the nearest open
// street, along open streets to the point closest to the casualty (the
// access point), and the final leg on foot from there to the casualty,
// slow where it crosses rubble.
package roads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
)

// DefaultOverpassURL is the public Overpass API endpoint.
const DefaultOverpassURL = "https://overpass-api.de/api/interpreter"

// SyntheticGenerator marks files written by Synthetic.
const SyntheticGenerator = "rubblesar synthetic street grid"

// Classes routed over (OSM highway=*). Others (construction, proposed, ...) are ignored.
var classes = []string{"motorway", "motorway_link", "trunk", "trunk_link", "primary", "primary_link", "secondary", "secondary_link",
	"tertiary", "tertiary_link", "unclassified", "residential", "living_street", "service", "road", "track",
	"pedestrian", "footway", "path", "steps", "cycleway", "bridleway", "corridor"}

// OverpassQuery returns the query for the streets within radius metres.
func OverpassQuery(lat, lon, radius float64) string {
	return fmt.Sprintf(`[out:json][timeout:120];(way["highway"~"^(%s)$"](around:%.0f,%.6f,%.6f);way["waterway"~"^(river|canal)$"](around:%.0f,%.6f,%.6f););out geom;`,
		strings.Join(classes, "|"), radius, lat, lon, radius, lat, lon)
}

// FetchOverpass downloads the streets (Overpass JSON with node ids and geometry).
func FetchOverpass(ctx context.Context, endpoint string, lat, lon, radius float64) ([]byte, error) {
	if endpoint == "" {
		endpoint = DefaultOverpassURL
	}
	form := url.Values{"data": {OverpassQuery(lat, lon, radius)}}
	ctx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, diag.Wrap("E-RTE-001", err, "building request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "rubblesar-scenario-tool (simulation; offline research)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, diag.Wrap("E-RTE-001", err, "POST %s", endpoint)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return nil, diag.Wrap("E-RTE-001", err, "reading reply from %s", endpoint)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(b)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, diag.New("E-RTE-001", "%s answered HTTP %d: %s", endpoint, resp.StatusCode, snippet)
	}
	if _, err := Parse(b, geo.NewLocal(geo.Point{Lat: lat, Lon: lon})); err != nil {
		return nil, err
	}
	return b, nil
}

type overpassDoc struct {
	Generator string `json:"generator"`
	Elements  []struct {
		Type     string            `json:"type"`
		ID       int64             `json:"id"`
		Tags     map[string]string `json:"tags"`
		Nodes    []int64           `json:"nodes"`
		Geometry []struct {
			Lat float64 `json:"lat"`
			Lon float64 `json:"lon"`
		} `json:"geometry"`
	} `json:"elements"`
}

// Edge is one piece of street (at most pieceM long) between two nodes.
type Edge struct {
	A, B  int
	Len   float64
	Class string
	// Oneway: 1 only A->B for vehicles, -1 only B->A, 0 both.
	Oneway  int
	Name    string
	Blocked bool
	// NoFoot, NoVehicle: closed to people on foot / to vehicles (OSM access tags).
	NoFoot, NoVehicle bool
}

// Network is the street graph in local metres.
type Network struct {
	Source string // "osm" or "synthetic"
	Ways   int
	Nodes  []geom.P
	Edges  []Edge
	adj    [][]int
	// Blockages are the spill polygons routes avoid (set by Block).
	Blockages []geom.Poly
	blockBox  []geom.Box
	// BlockedEdges counts edges inside a blockage.
	BlockedEdges int
	// LengthM is the total street length.
	LengthM float64
	// Water: river and canal centre lines; walks off the streets never cross them.
	Water    []geom.Poly
	waterBox []geom.Box
}

const pieceM = 10.0

// Parse reads Overpass JSON (ways with "out geom") into a network.
func Parse(b []byte, local geo.Local) (*Network, error) {
	var doc overpassDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, diag.Wrap("E-RTE-001", err, "not Overpass JSON")
	}
	n := &Network{Source: "osm"}
	if doc.Generator == SyntheticGenerator {
		n.Source = "synthetic"
	}
	byID := map[int64]int{}
	byPos := map[[2]int64]int{}
	node := func(id int64, p geom.P, lat, lon float64) int {
		if id != 0 {
			if i, ok := byID[id]; ok {
				return i
			}
		}
		k := [2]int64{int64(math.Round(lat * 1e7)), int64(math.Round(lon * 1e7))}
		if i, ok := byPos[k]; ok {
			if id != 0 {
				byID[id] = i
			}
			return i
		}
		i := len(n.Nodes)
		n.Nodes = append(n.Nodes, p)
		byPos[k] = i
		if id != 0 {
			byID[id] = i
		}
		return i
	}
	known := map[string]bool{}
	for _, c := range classes {
		known[c] = true
	}
	for _, e := range doc.Elements {
		if w := e.Tags["waterway"]; e.Type == "way" && (w == "river" || w == "canal") && len(e.Geometry) >= 2 {
			line := make(geom.Poly, len(e.Geometry))
			for k, g := range e.Geometry {
				x, y := local.ToLocal(geo.Point{Lat: g.Lat, Lon: g.Lon})
				line[k] = geom.P{X: x, Y: y}
			}
			n.Water = append(n.Water, line)
			n.waterBox = append(n.waterBox, line.Bounds())
			continue
		}
		cls := e.Tags["highway"]
		if e.Type != "way" || !known[cls] || len(e.Geometry) < 2 || e.Tags["area"] == "yes" {
			continue
		}
		noFoot, noVehicle := permissions(e.Tags)
		if noFoot && noVehicle {
			continue
		}
		ids := e.Nodes
		if len(ids) != len(e.Geometry) {
			ids = nil
		}
		ow := 0
		switch strings.ToLower(e.Tags["oneway"]) {
		case "yes", "1", "true":
			ow = 1
		case "-1", "reverse":
			ow = -1
		}
		if e.Tags["junction"] == "roundabout" && ow == 0 && e.Tags["oneway"] != "no" {
			ow = 1
		}
		name := strings.TrimSpace(e.Tags["name"])
		if name == "" {
			name = strings.TrimSpace(e.Tags["ref"])
		}
		n.Ways++
		prev := -1
		var prevP geom.P
		for k, g := range e.Geometry {
			x, y := local.ToLocal(geo.Point{Lat: g.Lat, Lon: g.Lon})
			p := geom.P{X: x, Y: y}
			var id int64
			if ids != nil {
				id = ids[k]
			}
			cur := node(id, p, g.Lat, g.Lon)
			if prev >= 0 && cur != prev {
				n.addPieces(prev, cur, prevP, p, cls, ow, name, noFoot, noVehicle)
			}
			prev, prevP = cur, p
		}
	}
	if len(n.Edges) == 0 {
		return nil, diag.New("E-RTE-001", "the file holds %d elements but no streets", len(doc.Elements))
	}
	n.index()
	return n, nil
}

// addPieces adds the street a->b cut into pieces of at most pieceM.
func (n *Network) addPieces(a, b int, pa, pb geom.P, cls string, ow int, name string, noFoot, noVehicle bool) {
	l := pa.Dist(pb)
	k := max(1, int(math.Ceil(l/pieceM)))
	from := a
	for i := 1; i <= k; i++ {
		to := b
		if i < k {
			to = len(n.Nodes)
			n.Nodes = append(n.Nodes, geom.Lerp(pa, pb, float64(i)/float64(k)))
		}
		n.Edges = append(n.Edges, Edge{A: from, B: to, Len: l / float64(k), Class: cls, Oneway: ow, Name: name, NoFoot: noFoot, NoVehicle: noVehicle})
		from = to
	}
	n.LengthM += l
}

func (n *Network) index() {
	n.adj = make([][]int, len(n.Nodes))
	for i, e := range n.Edges {
		n.adj[e.A] = append(n.adj[e.A], i)
		n.adj[e.B] = append(n.adj[e.B], i)
	}
}

// permissions reads OSM access tags: who may not use the way. access=no
// closes it unless foot (or a vehicle tag) opens it again; foot=no closes it
// to people on foot; vehicle=no or motor_vehicle=no to vehicles.
func permissions(t map[string]string) (noFoot, noVehicle bool) {
	closed := func(v string) bool { return v == "no" || v == "private" }
	open := func(v string) bool { return v == "yes" || v == "designated" || v == "permissive" || v == "destination" }
	all := closed(t["access"])
	noFoot, noVehicle = all, all
	if f := t["foot"]; open(f) {
		noFoot = false
	} else if closed(f) {
		noFoot = true
	}
	for _, k := range []string{"vehicle", "motor_vehicle"} {
		if v := t[k]; open(v) {
			noVehicle = false
		} else if closed(v) {
			noVehicle = true
		}
	}
	return noFoot, noVehicle
}

// CrossesWater reports whether the straight line a-b crosses a river or canal.
func (n *Network) CrossesWater(a, b geom.P) bool {
	seg := geom.Poly{a, b}.Bounds()
	for i, w := range n.Water {
		if !n.waterBox[i].Overlaps(seg) {
			continue
		}
		for k := 1; k < len(w); k++ {
			if segmentsCross(a, b, w[k-1], w[k]) {
				return true
			}
		}
	}
	return false
}

func segmentsCross(p1, p2, q1, q2 geom.P) bool {
	cross := func(o, a, b geom.P) float64 { return (a.X-o.X)*(b.Y-o.Y) - (a.Y-o.Y)*(b.X-o.X) }
	d1, d2 := cross(q1, q2, p1), cross(q1, q2, p2)
	d3, d4 := cross(p1, p2, q1), cross(p1, p2, q2)
	return ((d1 > 0) != (d2 > 0)) && ((d3 > 0) != (d4 > 0))
}

// Load reads a saved street file.
func Load(path string, local geo.Local) (*Network, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, diag.Wrap("E-RTE-001", err, "reading the street file %s", path)
	}
	n, err := Parse(b, local)
	if err != nil {
		return nil, diag.Wrap("E-RTE-001", err, "%s", path)
	}
	return n, nil
}

// Synthetic writes a street grid (Overpass JSON layout) every spacingM metres
// within radiusM of the centre: residential streets, every fourth a secondary
// road. For machines that cannot download the real streets.
func Synthetic(centre geo.Point, radiusM, spacingM float64) []byte {
	local := geo.NewLocal(centre)
	type el struct {
		Type     string               `json:"type"`
		ID       int64                `json:"id"`
		Tags     map[string]string    `json:"tags"`
		Nodes    []int64              `json:"nodes"`
		Geometry []map[string]float64 `json:"geometry"`
	}
	doc := struct {
		Version   float64 `json:"version"`
		Generator string  `json:"generator"`
		Elements  []el    `json:"elements"`
	}{Version: 0.6, Generator: SyntheticGenerator}
	k := int(math.Floor(radiusM / spacingM))
	nodeID := func(i, j int) int64 { return int64((i+k)*(2*k+1)+(j+k)) + 1 }
	way := int64(1)
	line := func(name, cls string, pts [][2]int) {
		e := el{Type: "way", ID: way, Tags: map[string]string{"highway": cls, "name": name}}
		way++
		for _, ij := range pts {
			ll := local.ToPoint(float64(ij[0])*spacingM, float64(ij[1])*spacingM)
			e.Nodes = append(e.Nodes, nodeID(ij[0], ij[1]))
			e.Geometry = append(e.Geometry, map[string]float64{"lat": math.Round(ll.Lat*1e7) / 1e7, "lon": math.Round(ll.Lon*1e7) / 1e7})
		}
		doc.Elements = append(doc.Elements, e)
	}
	cls := func(i int) string {
		if i%4 == 0 {
			return "secondary"
		}
		return "residential"
	}
	for j := -k; j <= k; j++ {
		var pts [][2]int
		for i := -k; i <= k; i++ {
			pts = append(pts, [2]int{i, j})
		}
		line(fmt.Sprintf("Grid Street %+d", j), cls(j), pts)
	}
	for i := -k; i <= k; i++ {
		var pts [][2]int
		for j := -k; j <= k; j++ {
			pts = append(pts, [2]int{i, j})
		}
		line(fmt.Sprintf("Grid Avenue %+d", i), cls(i), pts)
	}
	b, _ := json.MarshalIndent(doc, "", " ")
	return b
}

// Block marks every street piece that passes within marginM of a blockage
// (sampled every metre) as closed.
func (n *Network) Block(polys []geom.Poly, marginM float64) {
	n.Blockages = append([]geom.Poly(nil), polys...)
	n.blockBox = make([]geom.Box, len(polys))
	for i, p := range polys {
		n.blockBox[i] = p.Bounds().Grow(marginM)
	}
	n.BlockedEdges = 0
	for i := range n.Edges {
		e := &n.Edges[i]
		a, b := n.Nodes[e.A], n.Nodes[e.B]
		e.Blocked = false
		k := max(1, int(math.Ceil(e.Len)))
		for s := 0; s <= k && !e.Blocked; s++ {
			if n.inBlockage(geom.Lerp(a, b, float64(s)/float64(k)), marginM) {
				e.Blocked = true
			}
		}
		if e.Blocked {
			n.BlockedEdges++
		}
	}
}

// inBlockage reports whether q lies within m of a blockage.
func (n *Network) inBlockage(q geom.P, m float64) bool {
	for i, p := range n.Blockages {
		if !n.blockBox[i].ContainsP(q) {
			continue
		}
		if p.SignedDist(q) <= m {
			return true
		}
	}
	return false
}

// InBlockage reports whether q lies inside a blockage.
func (n *Network) InBlockage(q geom.P) bool { return n.inBlockage(q, 0) }

// PieceBlocked reports whether the street piece a-b passes within marginM of
// a blockage, by the same test Block closes the network's pieces with.
func (n *Network) PieceBlocked(a, b geom.P, marginM float64) bool {
	k := max(1, int(math.Ceil(a.Dist(b))))
	for s := 0; s <= k; s++ {
		if n.inBlockage(geom.Lerp(a, b, float64(s)/float64(k)), marginM) {
			return true
		}
	}
	return false
}

// Permissions reads OSM access tags: whether a way is closed on foot and to vehicles.
func Permissions(tags map[string]string) (noFoot, noVehicle bool) { return permissions(tags) }

// Routed reports whether highway=cls is one of the street classes routed over.
func Routed(cls string) bool {
	for _, c := range classes {
		if c == cls {
			return true
		}
	}
	return false
}

// PieceM is the longest street piece the network is cut into.
const PieceM = pieceM

// Bounds is the extent of the network.
func (n *Network) Bounds() geom.Box { return geom.Poly(n.Nodes).Bounds() }

// ClassCounts gives the street length per class, longest first (for reports).
func (n *Network) ClassCounts() []string {
	m := map[string]float64{}
	for _, e := range n.Edges {
		m[e.Class] += e.Len
	}
	type kv struct {
		k string
		v float64
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool { return l[i].v > l[j].v })
	var out []string
	for _, x := range l {
		out = append(out, fmt.Sprintf("%s %.1f km", x.k, x.v/1000))
	}
	return out
}

// Adj lists the pieces meeting at node i.
func (n *Network) Adj(i int) []int { return n.adj[i] }

// AddWater adds a river or canal centre line (local metres).
func (n *Network) AddWater(line geom.Poly) {
	n.Water = append(n.Water, line)
	n.waterBox = append(n.waterBox, line.Bounds())
}
