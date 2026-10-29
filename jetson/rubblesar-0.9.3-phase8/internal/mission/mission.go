// Package mission defines drone missions (waypoint lists) and builds survey
// patterns over debris sites.
//
// Missions map one-to-one onto MAVLink mission items (NAV_WAYPOINT with speed
// changes), so a real PX4 or ArduPilot drone can fly them later. edge-eye
// builds each drone's survey itself from its configuration; from phase 4 the
// ZBook's mission-planner adds investigation tasks over the bus (fly to a
// casualty, hold over it, report what moved).
//
// Coordinates are local metres east (E) and north (N) of the incident centre.
package mission

import (
	"fmt"
	"math"
)

// Waypoint is one point of a mission.
type Waypoint struct {
	E, N     float64
	AltAGL   float64
	SpeedMps float64
	// Survey marks the leg that ends at this waypoint as camera work (a survey
	// line); other legs are transit.
	Survey bool
	Label  string
}

// Mission is an ordered waypoint list.
type Mission struct {
	ID        string
	Waypoints []Waypoint
	// Areas: a survey's areas (collapse sites) in the order flown.
	Areas []string
}

// LengthM returns the flown length from (startE, startN) through every waypoint.
func (m Mission) LengthM(startE, startN float64) float64 {
	l, e, n := 0.0, startE, startN
	for _, w := range m.Waypoints {
		l += math.Hypot(w.E-e, w.N-n)
		e, n = w.E, w.N
	}
	return l
}

// DurationS estimates the flight time at each leg's speed.
func (m Mission) DurationS(startE, startN float64) float64 {
	t, e, n := 0.0, startE, startN
	for _, w := range m.Waypoints {
		if w.SpeedMps > 0 {
			t += math.Hypot(w.E-e, w.N-n) / w.SpeedMps
		}
		e, n = w.E, w.N
	}
	return t
}

// Camera is the downward camera used to size the survey lines.
type Camera struct {
	W, H    int     // pixels
	HFOVDeg float64 // horizontal field of view (image width, across track)
}

// Footprint returns the ground footprint across and along track at a height, metres.
func (c Camera) Footprint(alt float64) (across, along float64) {
	f := float64(c.W) / 2 / math.Tan(c.HFOVDeg*math.Pi/360)
	across = 2 * alt * math.Tan(c.HFOVDeg*math.Pi/360)
	along = 2 * alt * float64(c.H) / 2 / f
	return
}

// Area is something to survey: a polygon plus a margin around it.
type Area struct {
	ID      string
	Polygon [][2]float64 // E, N
	MarginM float64
}

// SurveyParams size a lawnmower pattern.
type SurveyParams struct {
	AltAGL          float64
	SpeedMps        float64 // along the survey lines
	TransitSpeedMps float64 // between lines and areas
	SideOverlap     float64 // 0..0.9: share of the camera swath shared by neighbouring lines
	Camera          Camera
}

// Validate checks the parameters.
func (p SurveyParams) Validate() error {
	switch {
	case p.AltAGL < 5 || p.AltAGL > 120:
		return fmt.Errorf("survey altitude %.0f m must be in 5..120", p.AltAGL)
	case p.SpeedMps <= 0 || p.TransitSpeedMps <= 0:
		return fmt.Errorf("survey speeds must be positive")
	case p.SideOverlap < 0 || p.SideOverlap > 0.9:
		return fmt.Errorf("side overlap %.2f must be in 0..0.9", p.SideOverlap)
	case p.Camera.W <= 0 || p.Camera.H <= 0 || p.Camera.HFOVDeg <= 0:
		return fmt.Errorf("camera not set")
	}
	return nil
}

// Lawnmower covers one area with parallel lines flown back and forth. The line
// direction is chosen (in 5 degree steps) to need the fewest lines, then the
// shortest total length. Every point of the area is seen for a whole
// along-track footprint of travel. The returned waypoints alternate between the start
// of a line (reached in transit) and its end (a survey leg).
func Lawnmower(a Area, p SurveyParams) []Waypoint {
	across, along := p.Camera.Footprint(p.AltAGL)
	spacing := across * (1 - p.SideOverlap)
	type plan struct {
		lines            int
		length, heading  float64
		amin, amax       float64
		dmin, dmax, aoff float64
	}
	var best *plan
	for deg := 0.0; deg < 180; deg += 5 {
		th := deg * math.Pi / 180
		de, dn := math.Sin(th), math.Cos(th) // along-line unit vector (heading deg)
		ae, an := math.Cos(th), -math.Sin(th)
		dmin, dmax, amin, amax := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		for _, q := range a.Polygon {
			d := q[0]*de + q[1]*dn
			c := q[0]*ae + q[1]*an
			dmin, dmax, amin, amax = math.Min(dmin, d), math.Max(dmax, d), math.Min(amin, c), math.Max(amax, c)
		}
		amin, amax = amin-a.MarginM, amax+a.MarginM
		dmin, dmax = dmin-a.MarginM, dmax+a.MarginM
		width := amax - amin
		n := 1
		if width > across {
			n = int(math.Ceil((width-across)/spacing)) + 1
		}
		l := float64(n) * (dmax - dmin + along)
		if best == nil || n < best.lines || (n == best.lines && l < best.length-1e-6) {
			best = &plan{lines: n, length: l, heading: deg, amin: amin, amax: amax, dmin: dmin, dmax: dmax}
		}
	}
	th := best.heading * math.Pi / 180
	de, dn := math.Sin(th), math.Cos(th)
	ae, an := math.Cos(th), -math.Sin(th)
	// Lines centred across the area.
	span := float64(best.lines-1) * spacing
	a0 := (best.amin+best.amax)/2 - span/2
	// Lines run half a footprint beyond the area at both ends, so every point
	// stays in view for a whole footprint of travel (several frames).
	d0, d1 := best.dmin-0.5*along, best.dmax+0.5*along
	var out []Waypoint
	for k := 0; k < best.lines; k++ {
		c := a0 + float64(k)*spacing
		s, e := d0, d1
		if k%2 == 1 {
			s, e = d1, d0
		}
		pt := func(d float64) (float64, float64) { return d*de + c*ae, d*dn + c*an }
		se, sn := pt(s)
		ee, en := pt(e)
		out = append(out,
			Waypoint{E: se, N: sn, AltAGL: p.AltAGL, SpeedMps: p.TransitSpeedMps, Label: fmt.Sprintf("%s line %d start", a.ID, k+1)},
			Waypoint{E: ee, N: en, AltAGL: p.AltAGL, SpeedMps: p.SpeedMps, Survey: true, Label: fmt.Sprintf("%s line %d", a.ID, k+1)})
	}
	return out
}

// Survey builds a mission covering the areas in order.
func Survey(id string, areas []Area, p SurveyParams) (Mission, error) {
	if err := p.Validate(); err != nil {
		return Mission{}, err
	}
	if len(areas) == 0 {
		return Mission{}, fmt.Errorf("no areas to survey")
	}
	m := Mission{ID: id}
	for _, a := range areas {
		if len(a.Polygon) < 3 {
			return Mission{}, fmt.Errorf("area %s has no polygon", a.ID)
		}
		m.Waypoints = append(m.Waypoints, Lawnmower(a, p)...)
		m.Areas = append(m.Areas, a.ID)
	}
	return m, nil
}

// TourOrder is the order to visit pts (local metres) that makes the shortest
// round trip from home and back: every order is tried for up to 8 points,
// beyond that nearest neighbour improved by 2-opt. Ties keep the lower
// indices first, so the order is the same wherever it is worked out.
func TourOrder(home [2]float64, pts [][2]float64) []int {
	n := len(pts)
	d := func(a, b [2]float64) float64 { return math.Hypot(a[0]-b[0], a[1]-b[1]) }
	length := func(order []int) float64 {
		l, at := 0.0, home
		for _, i := range order {
			l += d(at, pts[i])
			at = pts[i]
		}
		return l + d(at, home)
	}
	const eps = 1e-6
	if n <= 8 {
		best, bestL := make([]int, n), math.Inf(1)
		cur, used := make([]int, 0, n), make([]bool, n)
		var try func(l float64, at [2]float64)
		try = func(l float64, at [2]float64) {
			if l >= bestL-eps {
				return
			}
			if len(cur) == n {
				if t := l + d(at, home); t < bestL-eps {
					bestL = t
					copy(best, cur)
				}
				return
			}
			for i := 0; i < n; i++ {
				if !used[i] {
					used[i] = true
					cur = append(cur, i)
					try(l+d(at, pts[i]), pts[i])
					cur = cur[:len(cur)-1]
					used[i] = false
				}
			}
		}
		try(0, home)
		return best
	}
	order, used, at := make([]int, 0, n), make([]bool, n), home
	for len(order) < n {
		bi, bd := -1, math.Inf(1)
		for i := 0; i < n; i++ {
			if !used[i] && d(at, pts[i]) < bd-eps {
				bi, bd = i, d(at, pts[i])
			}
		}
		used[bi] = true
		order = append(order, bi)
		at = pts[bi]
	}
	for improved := true; improved; {
		improved = false
		for i := 0; i < n-1; i++ {
			for j := i + 1; j < n; j++ {
				cand := append(append(append([]int{}, order[:i]...), reverse(order[i:j+1])...), order[j+1:]...)
				if length(cand) < length(order)-eps {
					order, improved = cand, true
				}
			}
		}
	}
	return order
}

func reverse(a []int) []int {
	out := make([]int, len(a))
	for i, v := range a {
		out[len(a)-1-i] = v
	}
	return out
}
