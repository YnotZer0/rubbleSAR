package basemap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/roads"
)

func TestParseExtras(t *testing.T) {
	// A building (a hospital), a park, and a river area as a multipolygon of two fragments.
	doc := `{"elements":[
	 {"type":"way","id":1,"tags":{"building":"yes","amenity":"hospital","name":"Devlet Hastanesi"},"geometry":[{"lat":1,"lon":1},{"lat":1,"lon":2},{"lat":2,"lon":2},{"lat":1,"lon":1}]},
	 {"type":"way","id":2,"tags":{"leisure":"park"},"geometry":[{"lat":3,"lon":3},{"lat":3,"lon":4},{"lat":4,"lon":4},{"lat":3,"lon":3}]},
	 {"type":"way","id":3,"tags":{"building":"yes"},"geometry":[{"lat":5,"lon":5},{"lat":5,"lon":6}]},
	 {"type":"relation","id":4,"tags":{"natural":"water","water":"river","name":"Asi"},"members":[
	   {"type":"way","role":"outer","geometry":[{"lat":0,"lon":0},{"lat":0,"lon":1},{"lat":1,"lon":1}]},
	   {"type":"way","role":"outer","geometry":[{"lat":0,"lon":0},{"lat":1,"lon":0},{"lat":1,"lon":1}]},
	   {"type":"way","role":"inner","geometry":[{"lat":0.2,"lon":0.2},{"lat":0.3,"lon":0.3},{"lat":0.2,"lon":0.2}]}]}]}`
	x, err := ParseExtras([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Buildings) != 1 || x.Buildings[0].Kind != "hospital" || x.Buildings[0].Name != "Devlet Hastanesi" {
		t.Fatalf("buildings %+v", x.Buildings)
	}
	if len(x.Green) != 1 || x.Green[0].Kind != "park" {
		t.Fatalf("green %+v", x.Green)
	}
	if len(x.Water) != 1 || len(x.Water[0].Pts) != 5 || x.Water[0].Pts[0] != x.Water[0].Pts[4] || x.Water[0].Name != "Asi" {
		t.Fatalf("water (stitched ring) %+v", x.Water)
	}
	if _, err := ParseExtras([]byte(`{"elements":[{"type":"node","id":1}]}`)); err == nil {
		t.Fatal("no features accepted")
	}
	if q := Query(36.2, 36.16, 1105); !strings.Contains(q, `way["building"](around:1105,36.200000,36.160000)`) || !strings.HasSuffix(q, "out geom;") {
		t.Fatalf("query %s", q)
	}
}

func TestBuildLayers(t *testing.T) {
	centre := geo.Point{Lat: 36.2025, Lon: 36.16056}
	local := geo.NewLocal(centre)
	dir := t.TempDir()
	street := filepath.Join(dir, "streets.json")
	if err := os.WriteFile(street, roads.Synthetic(centre, 300, 60), 0o644); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(street)
	n, err := roads.Parse(b, local)
	if err != nil {
		t.Fatal(err)
	}
	// Rubble across the avenue at x = 0 between y = 20 and 40.
	block := []geom.Poly{{{X: -8, Y: 20}, {X: 8, Y: 20}, {X: 8, Y: 40}, {X: -8, Y: 40}}}
	n.Block(block, 1)
	L, err := Build(Input{Incident: "inc", Centre: centre, RadiusM: 400, Staging: centre, StreetFile: street, Network: n, MarginM: 1, Blockages: block})
	if err != nil {
		t.Fatal(err)
	}
	rubbleRuns, pieces := 0, 0
	for _, s := range L.Streets {
		if s.State == "rubble" {
			rubbleRuns++
			// Every rubble run lies where the network closed pieces.
			for _, p := range s.Pts {
				x, y := local.ToLocal(geo.Point{Lat: p[0], Lon: p[1]})
				if y < 20-roads.PieceM-1 || y > 40+roads.PieceM+1 {
					t.Fatalf("rubble run point at y=%.1f, outside the rubble", y)
				}
				_ = x
			}
			pieces += len(s.Pts) - 1
		}
	}
	if rubbleRuns == 0 || pieces != n.BlockedEdges {
		t.Fatalf("%d rubble runs, %d pieces; the network closed %d", rubbleRuns, pieces, n.BlockedEdges)
	}
	if len(L.Rubble) != 1 || len(L.Rubble[0].Pts) != 5 || L.StreetsFrom != "synthetic" {
		t.Fatalf("layers %+v", L.Summary())
	}
	out := filepath.Join(dir, "x", "layers.json")
	if err := L.Write(out); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(out); err != nil || st.Size() < 1000 {
		t.Fatalf("written %v %v", st, err)
	}
	t.Log(L.Summary())
}
