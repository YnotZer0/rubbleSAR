package roads

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
)

var centre = geo.Point{Lat: 36.2025, Lon: 36.16056}

func grid(t *testing.T) *Network {
	t.Helper()
	n, err := Parse(Synthetic(centre, 300, 60), geo.NewLocal(centre))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSyntheticGrid(t *testing.T) {
	n := grid(t)
	if n.Source != "synthetic" || n.Ways != 22 {
		t.Fatalf("source %s ways %d", n.Source, n.Ways)
	}
	// 11 lines each way, 600 m long.
	if math.Abs(n.LengthM-22*600) > 1 {
		t.Fatalf("length %.1f", n.LengthM)
	}
	for _, e := range n.Edges {
		if e.Len > pieceM+1e-6 {
			t.Fatalf("piece %.1f m", e.Len)
		}
	}
}

func box(x0, y0, x1, y1 float64) geom.Poly {
	return geom.Poly{{X: x0, Y: y0}, {X: x1, Y: y0}, {X: x1, Y: y1}, {X: x0, Y: y1}}
}

func checkAvoids(t *testing.T, n *Network, r *Route) {
	t.Helper()
	for i := 1; i <= r.AccessIdx; i++ {
		a, b := r.Points[i-1], r.Points[i]
		for s := 0.0; s <= 1; s += 0.01 {
			if q := geom.Lerp(a, b, s); n.InBlockage(q) && i > 1 {
				t.Fatalf("street part crosses rubble at %.1f,%.1f", q.X, q.Y)
			}
		}
	}
}

func TestRouteAroundRubble(t *testing.T) {
	n := grid(t)
	r := NewRouter(n)
	from, to := geom.P{X: 0, Y: -120}, geom.P{X: 5, Y: 125}
	open, err := r.Route(Foot, from, to)
	if err != nil {
		t.Fatal(err)
	}
	// Up the avenue, then the last few metres across open ground (cutting the corner is quicker).
	if open.RoadM < 235 || open.RoadM > 246 || open.OffRoadM > 8 || open.RubbleM != 0 || len(open.Points) > 4 {
		t.Fatalf("open route %+v", open)
	}
	if open.ETAs > 245/1.4+5/1.0+0.01 || open.ETAs < 240/1.4 {
		t.Fatalf("eta %.1f", open.ETAs)
	}
	// Rubble across the avenue at x=0 between y=20 and y=50, and around the casualty.
	n.Block([]geom.Poly{box(-10, 20, 10, 50), box(-8, 110, 8, 140)}, 0)
	if n.BlockedEdges == 0 {
		t.Fatal("nothing blocked")
	}
	rt, err := r.Route(Foot, from, to)
	if err != nil {
		t.Fatal(err)
	}
	checkAvoids(t, n, rt)
	if rt.RoadM < 300 {
		t.Fatalf("no detour: %.0f m of street", rt.RoadM)
	}
	// The final leg crosses the rubble around the casualty (it ends on the street at y=120 left or right of the box, or at y=110 from below...).
	if rt.RubbleM <= 0 || rt.Access.Dist(to) > 15 {
		t.Fatalf("final leg: access %v rubble %.1f", rt.Access, rt.RubbleM)
	}
	if math.Abs(rt.LengthM-(rt.RoadM+rt.OffRoadM+rt.RubbleM)) > 1e-6 || rt.T[len(rt.T)-1] != rt.ETAs {
		t.Fatalf("lengths %+v", rt)
	}
	// Position along the route.
	p, _ := rt.At(0)
	if p != from {
		t.Fatal("start")
	}
	p, _ = rt.At(rt.ETAs + 10)
	if p != to {
		t.Fatal("end")
	}
	mid, _ := rt.At(rt.ETAs / 2)
	if n.InBlockage(mid) {
		t.Fatal("halfway in rubble")
	}
	if left := rt.Remaining(0); math.Abs(left-rt.LengthM) > 1 {
		t.Fatalf("remaining %.1f of %.1f", left, rt.LengthM)
	}
}

func TestVehicleRespectsOneway(t *testing.T) {
	// One street east-west, one way eastwards, and a two-way loop back via the north.
	doc := `{"generator":"test","elements":[
 {"type":"way","id":1,"tags":{"highway":"residential","oneway":"yes","name":"One Way"},"nodes":[1,2],"geometry":[{"lat":36.2025,"lon":36.16},{"lat":36.2025,"lon":36.162}]},
 {"type":"way","id":2,"tags":{"highway":"residential","name":"Loop"},"nodes":[1,3,4,2],"geometry":[{"lat":36.2025,"lon":36.16},{"lat":36.2034,"lon":36.16},{"lat":36.2034,"lon":36.162},{"lat":36.2025,"lon":36.162}]},
 {"type":"way","id":3,"tags":{"highway":"footway"},"nodes":[5,6],"geometry":[{"lat":36.2020,"lon":36.161},{"lat":36.2025,"lon":36.161}]}]}`
	local := geo.NewLocal(geo.Point{Lat: 36.2025, Lon: 36.16})
	n, err := Parse([]byte(doc), local)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(n)
	east := geom.P{X: 179, Y: 0}
	west := geom.P{X: 1, Y: 0}
	fwd, err := r.Route(Vehicle, west, east)
	if err != nil {
		t.Fatal(err)
	}
	back, err := r.Route(Vehicle, east, west)
	if err != nil {
		t.Fatal(err)
	}
	if fwd.RoadM > 180 || back.RoadM < 180+2*99 {
		t.Fatalf("forward %.0f m, back %.0f m (must go round the loop)", fwd.RoadM, back.RoadM)
	}
	walk, err := r.Route(Foot, east, west)
	if err != nil {
		t.Fatal(err)
	}
	if walk.RoadM > 180 {
		t.Fatalf("on foot %.0f m (one way does not apply)", walk.RoadM)
	}
	if !strings.Contains(strings.Join(fwd.Streets, ","), "One Way") {
		t.Fatalf("streets %v", fwd.Streets)
	}
	// The footway is no street for the vehicle: starting on it snaps to a street.
	ft := geom.P{X: 89.9, Y: -40}
	v, err := r.Route(Vehicle, ft, east)
	if err != nil {
		t.Fatal(err)
	}
	if v.SnapM < 39 {
		t.Fatalf("vehicle snapped %.1f m (onto the footway?)", v.SnapM)
	}
}

func TestCutOff(t *testing.T) {
	n := grid(t)
	r := NewRouter(n)
	// Rubble all round the casualty's block, far wider than the final leg allows.
	n.Block([]geom.Poly{box(-300, 60, 300, 300)}, 0)
	_, err := r.Route(Foot, geom.P{X: 0, Y: -100}, geom.P{X: 0, Y: 290})
	if err == nil || !strings.Contains(err.Error(), "E-RTE-003") {
		t.Fatalf("want E-RTE-003, got %v", err)
	}
	_, err = r.Route(Foot, geom.P{X: 2000, Y: 0}, geom.P{X: 0, Y: 0})
	if err == nil || !strings.Contains(err.Error(), "E-RTE-002") {
		t.Fatalf("want E-RTE-002, got %v", err)
	}
}

func TestDirectWalkToANeighbour(t *testing.T) {
	n := grid(t)
	n.Block([]geom.Poly{box(10, 10, 50, 50)}, 0)
	r := NewRouter(n)
	// From one casualty in the collapse to the next, 12 m away: straight over the rubble.
	rt, err := r.Route(Foot, geom.P{X: 25, Y: 30}, geom.P{X: 37, Y: 30})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Points) != 2 || rt.RoadM != 0 || rt.RubbleM < 11.9 || rt.ETAs > 25 {
		t.Fatalf("direct %+v", rt)
	}
	// Far away: by the streets.
	rt, err = r.Route(Foot, geom.P{X: 25, Y: 30}, geom.P{X: 200, Y: 125})
	if err != nil {
		t.Fatal(err)
	}
	if rt.RoadM < 150 {
		t.Fatalf("far %+v", rt)
	}
}

// Real streets: the street closest to a casualty is often a back alley that
// only the long way round reaches. A few more metres over open ground beat
// a kilometre of detour, at the start (a team in a collapse) as at the end.
func TestBackAlleyIsNoShortcut(t *testing.T) {
	local := geo.NewLocal(centre)
	way := func(id int, name string, pts ...[2]float64) string {
		var g []string
		for _, p := range pts {
			ll := local.ToPoint(p[0], p[1])
			g = append(g, fmt.Sprintf(`{"lat":%.8f,"lon":%.8f}`, ll.Lat, ll.Lon))
		}
		return fmt.Sprintf(`{"type":"way","id":%d,"tags":{"highway":"residential","name":"%s "},"geometry":[%s]}`, id, name, strings.Join(g, ","))
	}
	doc := `{"generator":"test","elements":[` + strings.Join([]string{
		way(1, "Main Street", [2]float64{-300, 0}, [2]float64{0, 0}, [2]float64{300, 0}),
		way(2, "Ring Road", [2]float64{300, 0}, [2]float64{300, 200}, [2]float64{0, 200}),
		way(3, "Back Alley", [2]float64{0, 200}, [2]float64{0, 37}),
	}, ",") + `]}`
	n, err := Parse([]byte(doc), local)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRouter(n)
	cas := geom.P{X: 0, Y: 25}
	rt, err := r.Route(Foot, geom.P{X: -290, Y: 5}, cas)
	if err != nil {
		t.Fatal(err)
	}
	if rt.LengthM > 400 || rt.Access.Dist(geom.P{X: 0, Y: 0}) > 1 {
		t.Fatalf("to the casualty: %.0f m via %v, leaving the street at %v", rt.LengthM, rt.Streets, rt.Access)
	}
	if strings.Join(rt.Streets, ",") != "Main Street" {
		t.Fatalf("street names %q (trimmed, no repeats)", rt.Streets)
	}
	back, err := r.Route(Foot, cas, geom.P{X: -290, Y: 5})
	if err != nil {
		t.Fatal(err)
	}
	if back.LengthM > 400 || back.SnapM > 26 {
		t.Fatalf("from the casualty: %.0f m, joining the street %.0f m away", back.LengthM, back.SnapM)
	}
	// Times along the route are real walking times (no penalty in them).
	if want := 25/1.0 + 290/1.4 + 5/1.0; math.Abs(back.ETAs-want) > 3 || math.Abs(back.T[len(back.T)-1]-back.ETAs) > 1e-9 {
		t.Fatalf("eta %.1f, want about %.1f", back.ETAs, want)
	}
	// With 240 m of the main street closed by rubble, the alley is the way in
	// (with only 40 m closed, a 54 m walk beats the 1.25 km round: also right).
	n.Block([]geom.Poly{box(-120, -5, 120, 30)}, 0)
	// (From the east: the closure also cuts the west end off from the ring road.)
	rt, err = r.Route(Foot, geom.P{X: 290, Y: 5}, geom.P{X: 0, Y: 45})
	if err != nil {
		t.Fatal(err)
	}
	if rt.Access.Dist(geom.P{X: 0, Y: 45}) > 9 || rt.RoadM < 600 {
		t.Fatalf("main street closed: %.0f m of street, leaving at %v", rt.RoadM, rt.Access)
	}
}

// OSM access tags and rivers, as in central Antakya: the road bridge is
// closed (access=no), the footbridge beside it is open but reached through
// a street closed to traffic and open on foot (access=no + foot=yes).
func TestAccessTagsAndRiver(t *testing.T) {
	local := geo.NewLocal(centre)
	way := func(id int, tags string, pts ...[2]float64) string {
		var g []string
		for _, p := range pts {
			ll := local.ToPoint(p[0], p[1])
			g = append(g, fmt.Sprintf(`{"lat":%.8f,"lon":%.8f}`, ll.Lat, ll.Lon))
		}
		return fmt.Sprintf(`{"type":"way","id":%d,"tags":{%s},"geometry":[%s]}`, id, tags, strings.Join(g, ","))
	}
	// River along x=0 from south to north. West bank x<0, east bank x>0.
	doc := `{"generator":"test","elements":[` + strings.Join([]string{
		way(1, `"waterway":"river"`, [2]float64{0, -2000}, [2]float64{0, 2000}),
		way(2, `"highway":"residential","name":"West Road"`, [2]float64{-40, -1500}, [2]float64{-40, -20}, [2]float64{-40, 0}, [2]float64{-40, 1400}, [2]float64{-40, 1500}),
		way(3, `"highway":"residential","name":"East Road"`, [2]float64{40, -1500}, [2]float64{40, -20}, [2]float64{40, 0}, [2]float64{40, 1400}, [2]float64{40, 1500}),
		way(4, `"highway":"primary","name":"Old Bridge","bridge":"yes","access":"no"`, [2]float64{-40, 0}, [2]float64{40, 0}),
		way(5, `"highway":"pedestrian","bridge":"yes"`, [2]float64{-40, -20}, [2]float64{25, -20}),
		way(6, `"highway":"living_street","name":"Bazaar Street","access":"no","foot":"yes","motor_vehicle":"no"`, [2]float64{25, -20}, [2]float64{40, -20}),
		way(7, `"highway":"secondary","name":"Far Bridge","bridge":"yes"`, [2]float64{-40, 1400}, [2]float64{40, 1400}),
	}, ",") + `]}`
	n, err := Parse([]byte(doc), local)
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Water) != 1 {
		t.Fatalf("%d rivers", len(n.Water))
	}
	r := NewRouter(n)
	from, to := geom.P{X: -45, Y: 100}, geom.P{X: 46, Y: 100}
	foot, err := r.Route(Foot, from, to)
	if err != nil {
		t.Fatal(err)
	}
	// On foot: over the footbridge and through the bazaar street, about 100+65+15+120 m.
	if foot.LengthM > 400 {
		t.Fatalf("on foot %.0f m via %v (the footbridge is open on foot)", foot.LengthM, foot.Streets)
	}
	car, err := r.Route(Vehicle, from, to)
	if err != nil {
		t.Fatal(err)
	}
	// By vehicle: neither the closed bridge nor the footbridge, so round by the far bridge.
	if car.RoadM < 2500 || !strings.Contains(strings.Join(car.Streets, ","), "Far Bridge") {
		t.Fatalf("vehicle %.0f m via %v", car.RoadM, car.Streets)
	}
	// Never a walk across the river, not even for a casualty 30 m away on the other bank.
	near, err := r.Route(Foot, geom.P{X: -10, Y: 600}, geom.P{X: 10, Y: 600})
	if err != nil {
		t.Fatal(err)
	}
	if len(near.Points) == 2 || near.RoadM < 500 {
		t.Fatalf("walked across the river: %+v", near)
	}
	for i := 1; i < len(near.Points); i++ {
		if i != 1 && i != len(near.Points)-1 {
			continue // street parts may cross on bridges
		}
		if n.CrossesWater(near.Points[i-1], near.Points[i]) {
			t.Fatalf("off-street leg %d crosses the river", i)
		}
	}
}
