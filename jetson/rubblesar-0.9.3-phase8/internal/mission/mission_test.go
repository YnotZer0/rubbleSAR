package mission

import (
	"fmt"
	"math"
	"testing"
)

var cam = Camera{W: 1920, H: 1080, HFOVDeg: 70}

func TestFootprint(t *testing.T) {
	across, along := cam.Footprint(40)
	if math.Abs(across-56.0) > 0.1 || math.Abs(along-31.5) > 0.2 {
		t.Fatalf("footprint at 40 m: %.2f x %.2f", across, along)
	}
}

// Every point of the area (plus margin) must be seen: within half a swath of
// some line across track and between the line ends (plus half the along-track
// footprint) along it.
func TestLawnmowerCoversArea(t *testing.T) {
	area := Area{ID: "s01", Polygon: [][2]float64{{0, 0}, {40, 10}, {35, 45}, {-5, 30}}, MarginM: 6}
	for _, alt := range []float64{15, 40} {
		p := SurveyParams{AltAGL: alt, SpeedMps: 5, TransitSpeedMps: 8, SideOverlap: 0.7, Camera: cam}
		wps := Lawnmower(area, p)
		if len(wps) < 2 || len(wps)%2 != 0 {
			t.Fatalf("alt %.0f: %d waypoints", alt, len(wps))
		}
		across, along := cam.Footprint(alt)
		covered := func(e, n float64) bool {
			for i := 0; i+1 < len(wps); i += 2 {
				a, b := wps[i], wps[i+1]
				dx, dy := b.E-a.E, b.N-a.N
				l := math.Hypot(dx, dy)
				var u, c float64
				if l < 1e-9 {
					u, c = 0, math.Hypot(e-a.E, n-a.N)
				} else {
					ux, uy := dx/l, dy/l
					u = (e-a.E)*ux + (n-a.N)*uy
					c = math.Abs(-(e-a.E)*uy + (n-a.N)*ux)
				}
				if c <= across/2 && u >= along/2 && u <= l-along/2 { // in view for a whole footprint of travel
					return true
				}
			}
			return false
		}
		for e := -11.0; e <= 46; e += 1 {
			for n := -6.0; n <= 51; n += 1 {
				if inflated(area, e, n) && !covered(e, n) {
					t.Fatalf("alt %.0f: point %.0f,%.0f not covered by %d lines", alt, e, n, len(wps)/2)
				}
			}
		}
		for i, w := range wps {
			if w.Survey != (i%2 == 1) || w.AltAGL != alt {
				t.Fatalf("waypoint %d: %+v", i, w)
			}
		}
		t.Logf("alt %.0f m: %d lines", alt, len(wps)/2)
	}
}

// inflated: inside the polygon or within its margin.
func inflated(a Area, e, n float64) bool {
	if inPoly(a.Polygon, e, n) {
		return true
	}
	for i := range a.Polygon {
		p, q := a.Polygon[i], a.Polygon[(i+1)%len(a.Polygon)]
		if segDist(e, n, p, q) <= a.MarginM {
			return true
		}
	}
	return false
}

func inPoly(p [][2]float64, x, y float64) bool {
	in := false
	for i, j := 0, len(p)-1; i < len(p); j, i = i, i+1 {
		if (p[i][1] > y) != (p[j][1] > y) && x < (p[j][0]-p[i][0])*(y-p[i][1])/(p[j][1]-p[i][1])+p[i][0] {
			in = !in
		}
	}
	return in
}

func segDist(x, y float64, a, b [2]float64) float64 {
	dx, dy := b[0]-a[0], b[1]-a[1]
	t := ((x-a[0])*dx + (y-a[1])*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	return math.Hypot(x-a[0]-t*dx, y-a[1]-t*dy)
}

func TestSurveyMission(t *testing.T) {
	p := SurveyParams{AltAGL: 40, SpeedMps: 8, TransitSpeedMps: 10, SideOverlap: 0.7, Camera: cam}
	areas := []Area{
		{ID: "a", Polygon: [][2]float64{{0, 0}, {30, 0}, {30, 20}, {0, 20}}, MarginM: 5},
		{ID: "b", Polygon: [][2]float64{{200, 100}, {230, 100}, {230, 140}, {200, 140}}, MarginM: 5},
	}
	m, err := Survey("scout-1-survey", areas, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Waypoints) < 4 || m.LengthM(0, -500) < 600 || m.DurationS(0, -500) <= 0 {
		t.Fatalf("mission %d waypoints, %.0f m, %.0f s", len(m.Waypoints), m.LengthM(0, -500), m.DurationS(0, -500))
	}
	if _, err := Survey("x", nil, p); err == nil {
		t.Fatal("empty area list accepted")
	}
	p.SideOverlap = 0.95
	if _, err := Survey("x", areas, p); err == nil {
		t.Fatal("overlap 0.95 accepted")
	}
}

// The survey order: the shortest round trip from home, not the scenario's
// numbering (on the ZBook the scout flew s01, s02, s03 across the river and
// back, with s05 next to s02).
func TestTourOrder(t *testing.T) {
	home := [2]float64{-50, -250}
	pts := [][2]float64{{20, 160}, {-260, 40}, {240, 150}, {-60, -90}, {-190, 10}} // like s01..s05
	tourLen := func(o []int) float64 {
		l, at := 0.0, home
		for _, i := range o {
			l += math.Hypot(pts[i][0]-at[0], pts[i][1]-at[1])
			at = pts[i]
		}
		return l + math.Hypot(home[0]-at[0], home[1]-at[1])
	}
	o := TourOrder(home, pts)
	if len(o) != 5 {
		t.Fatalf("order %v", o)
	}
	if !(tourLen(o) < tourLen([]int{0, 1, 2, 3, 4})) {
		t.Errorf("tour %v (%.0f m) not shorter than the numbering (%.0f m)", o, tourLen(o), tourLen([]int{0, 1, 2, 3, 4}))
	}
	// s02 and s05 are neighbours in the tour.
	pos := map[int]int{}
	for k, i := range o {
		pos[i] = k
	}
	if d := pos[1] - pos[4]; d != 1 && d != -1 {
		t.Errorf("s02 and s05 not flown one after the other: %v", o)
	}
	// Exact: no other order is shorter.
	best := math.Inf(1)
	var perm func(a []int, k int)
	perm = func(a []int, k int) {
		if k == len(a) {
			best = math.Min(best, tourLen(a))
			return
		}
		for i := k; i < len(a); i++ {
			a[k], a[i] = a[i], a[k]
			perm(a, k+1)
			a[k], a[i] = a[i], a[k]
		}
	}
	perm([]int{0, 1, 2, 3, 4}, 0)
	if tourLen(o) > best+1e-6 {
		t.Errorf("tour %.1f m, best %.1f m", tourLen(o), best)
	}
	// The same answer every time; a permutation for many points too.
	if o2 := TourOrder(home, pts); fmt.Sprint(o2) != fmt.Sprint(o) {
		t.Errorf("not stable: %v vs %v", o, o2)
	}
	var many [][2]float64
	for i := 0; i < 12; i++ {
		many = append(many, [2]float64{float64((i * 137) % 400), float64((i * 71) % 300)})
	}
	seen := map[int]bool{}
	for _, i := range TourOrder(home, many) {
		seen[i] = true
	}
	if len(seen) != 12 {
		t.Errorf("12 points: %d visited", len(seen))
	}
	if len(TourOrder(home, nil)) != 0 {
		t.Error("no points")
	}
}
