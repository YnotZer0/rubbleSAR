package roads

import (
	"container/heap"
	"math"
	"sort"

	"rubblesar/internal/diag"
	"rubblesar/internal/geom"
)

// Mode is how a team travels.
type Mode string

// Travel modes.
const (
	Foot    Mode = "foot"
	Vehicle Mode = "vehicle"
)

// Speeds of a travel mode, metres per second.
type Speeds struct {
	// Foot: walking on streets (steps at half speed); vehicles: per class.
	FootMps float64
	// OffRoadMps: on foot away from streets (start and final legs).
	OffRoadMps float64
	// RubbleMps: on foot across rubble.
	RubbleMps float64
	// VehicleMps: by street class (a missing class: VehicleDefaultMps).
	VehicleMps        map[string]float64
	VehicleDefaultMps float64
}

// DefaultSpeeds are cautious speeds in a damaged city.
func DefaultSpeeds() Speeds {
	kmh := func(v float64) float64 { return v / 3.6 }
	return Speeds{FootMps: 1.4, OffRoadMps: 1.0, RubbleMps: 0.5, VehicleDefaultMps: kmh(15),
		VehicleMps: map[string]float64{"motorway": kmh(50), "motorway_link": kmh(30), "trunk": kmh(40), "trunk_link": kmh(25),
			"primary": kmh(30), "primary_link": kmh(20), "secondary": kmh(30), "secondary_link": kmh(20), "tertiary": kmh(25),
			"tertiary_link": kmh(20), "unclassified": kmh(20), "residential": kmh(20), "living_street": kmh(10), "service": kmh(10),
			"road": kmh(15), "track": kmh(10)}}
}

var noVehicle = map[string]bool{"pedestrian": true, "footway": true, "path": true, "steps": true, "cycleway": true, "bridleway": true, "corridor": true}

// Limits of a route search.
const (
	MaxSnapM  = 250.0 // start at most this far from an open street
	MaxFinalM = 200.0 // casualty at most this far from an open street
	// A casualty this close is also tried on foot in a straight line.
	MaxDirectM = 50.0
)

// Route is a team's way to a casualty.
type Route struct {
	Mode Mode
	// Points from the team (index 0) to the casualty (last), local metres;
	// T is the time from the start at each point, seconds.
	Points []geom.P
	T      []float64
	// Access is Points[AccessIdx], where the street part ends and the final leg starts.
	AccessIdx int
	Access    geom.P
	// Lengths: on streets, off streets (start and final legs) and across rubble.
	LengthM, RoadM, OffRoadM, RubbleM float64
	// ETAs is the travel time.
	ETAs float64
	// SnapM: the start's distance from the street it joined.
	SnapM float64
	// Straight-line distance start to casualty.
	DirectM float64
	// Streets named along the way, in order (no repeats).
	Streets []string
}

// At returns the position after t seconds along the route and the heading
// (degrees from north) of the leg there.
func (r *Route) At(t float64) (geom.P, float64) {
	if len(r.Points) == 0 {
		return geom.P{}, 0
	}
	if t <= 0 || len(r.Points) == 1 {
		return r.Points[0], r.heading(0)
	}
	for i := 1; i < len(r.Points); i++ {
		if t <= r.T[i] {
			dt := r.T[i] - r.T[i-1]
			f := 1.0
			if dt > 0 {
				f = (t - r.T[i-1]) / dt
			}
			return geom.Lerp(r.Points[i-1], r.Points[i], f), r.heading(i - 1)
		}
	}
	return r.Points[len(r.Points)-1], r.heading(len(r.Points) - 2)
}

func (r *Route) heading(i int) float64 {
	if i < 0 || i+1 >= len(r.Points) {
		return 0
	}
	d := r.Points[i+1].Sub(r.Points[i])
	h := math.Atan2(d.X, d.Y) * 180 / math.Pi
	if h < 0 {
		h += 360
	}
	return h
}

// Remaining is the distance left after t seconds.
func (r *Route) Remaining(t float64) float64 {
	p, _ := r.At(t)
	left := 0.0
	for i := 1; i < len(r.Points); i++ {
		if r.T[i] > t {
			if left == 0 {
				left = p.Dist(r.Points[i])
			} else {
				left += r.Points[i-1].Dist(r.Points[i])
			}
		}
	}
	return left
}

// Router finds routes on a network.
type Router struct {
	N *Network
	S Speeds
}

// NewRouter makes a router with the default speeds.
func NewRouter(n *Network) *Router { return &Router{N: n, S: DefaultSpeeds()} }

// usable reports whether mode may use edge e at all.
func (r *Router) usable(e *Edge, mode Mode) bool {
	if e.Blocked {
		return false
	}
	if mode == Vehicle {
		return !e.NoVehicle && !noVehicle[e.Class]
	}
	return !e.NoFoot
}

// speed on edge e.
func (r *Router) speed(e *Edge, mode Mode) float64 {
	if mode == Vehicle {
		if v, ok := r.S.VehicleMps[e.Class]; ok {
			return v
		}
		return r.S.VehicleDefaultMps
	}
	if e.Class == "steps" {
		return r.S.FootMps / 2
	}
	return r.S.FootMps
}

// canGo reports whether mode may travel the edge from its end `from` (A or B).
func canGo(e *Edge, mode Mode, fromA bool) bool {
	if mode != Vehicle || e.Oneway == 0 {
		return true
	}
	return (e.Oneway == 1) == fromA
}

// legTime is the time on foot for a straight leg, slow across rubble; it
// also returns the lengths across rubble and outside.
func (r *Router) legTime(a, b geom.P) (t, rubble, off float64) {
	l := a.Dist(b)
	if l == 0 {
		return 0, 0, 0
	}
	if r.N.CrossesWater(a, b) {
		return math.Inf(1), 0, l // no walking across a river
	}
	k := max(1, int(math.Ceil(l/0.5)))
	in := 0
	for s := 0; s < k; s++ {
		if r.N.InBlockage(geom.Lerp(a, b, (float64(s)+0.5)/float64(k))) {
			in++
		}
	}
	rubble = l * float64(in) / float64(k)
	off = l - rubble
	return rubble/r.S.RubbleMps + off/r.S.OffRoadMps, rubble, off
}

type pqItem struct {
	node int
	t    float64
}
type pq []pqItem

func (q pq) Len() int           { return len(q) }
func (q pq) Less(i, j int) bool { return q[i].t < q[j].t }
func (q pq) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *pq) Push(x any)        { *q = append(*q, x.(pqItem)) }
func (q *pq) Pop() any          { o := *q; v := o[len(o)-1]; *q = o[:len(o)-1]; return v }

// snap is a point on an edge.
type snap struct {
	edge int
	t    float64 // 0 at A, 1 at B
	p    geom.P
	d    float64
}

// nearest finds the nearest point on an edge mode may use, within maxM.
func (r *Router) nearest(q geom.P, mode Mode, maxM float64) (snap, bool) {
	best := snap{edge: -1, d: math.Inf(1)}
	for i := range r.N.Edges {
		e := &r.N.Edges[i]
		if !r.usable(e, mode) {
			continue
		}
		a, b := r.N.Nodes[e.A], r.N.Nodes[e.B]
		if math.Abs(a.X-q.X) > maxM+pieceM || math.Abs(a.Y-q.Y) > maxM+pieceM {
			continue
		}
		d, t := geom.SegDist(q, a, b)
		if d < best.d {
			best = snap{edge: i, t: t, p: geom.Lerp(a, b, t), d: d}
		}
	}
	return best, best.edge >= 0 && best.d <= maxM
}

// Off-street legs (from the team to a street, from a street to the
// casualty) may join any street within offStreetSlackM of the closest one,
// but each metre beyond the closest counts offStreetPenaltyS on top of the
// walking time: crossing a block of damaged buildings is slow and uncertain,
// yet a few metres more of rubble beat a long way round to a back alley.
const (
	offStreetSlackM   = 60.0
	offStreetPenaltyS = 10.0
	maxStarts         = 12
)

// start is one way onto the streets, with its cost.
type start struct {
	snap
	legT, rubble, off float64 // the walk to the street
	pen               float64 // the penalty counted on top (not real time)
}

// starts lists the ways onto the streets from q: the nearest point of each
// usable piece within offStreetSlackM of the closest, at most maxStarts.
func (r *Router) starts(q geom.P, mode Mode) []start {
	var all []snap
	dmin := math.Inf(1)
	for i := range r.N.Edges {
		e := &r.N.Edges[i]
		if !r.usable(e, mode) {
			continue
		}
		a, b := r.N.Nodes[e.A], r.N.Nodes[e.B]
		if math.Abs(a.X-q.X) > MaxSnapM+pieceM || math.Abs(a.Y-q.Y) > MaxSnapM+pieceM {
			continue
		}
		d, t := geom.SegDist(q, a, b)
		if d > MaxSnapM {
			continue
		}
		p := geom.Lerp(a, b, t)
		if r.N.CrossesWater(q, p) {
			continue
		}
		all = append(all, snap{edge: i, t: t, p: p, d: d})
		dmin = math.Min(dmin, d)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].d < all[j].d })
	var out []start
	for _, s := range all {
		if s.d > dmin+offStreetSlackM || len(out) >= maxStarts {
			break
		}
		lt, rub, off := r.legTime(q, s.p)
		out = append(out, start{snap: s, legT: lt, rubble: rub, off: off, pen: (s.d - dmin) * offStreetPenaltyS})
	}
	return out
}

// Route finds the quickest route for mode from `from` to the casualty at `to`.
func (r *Router) Route(mode Mode, from, to geom.P) (*Route, error) {
	t, err := r.From(mode, from)
	if err != nil {
		return nil, err
	}
	return t.To(to)
}

// Tree holds the quickest times from one start to every street node, so
// routes to several casualties from the same team cost one search.
type Tree struct {
	r    *Router
	mode Mode
	from geom.P
	sts  []start
	// dist includes the penalty of the way onto the streets; src is the start each node was reached from.
	dist []float64
	prev []int
	src  []int
}

// From searches every street from the start.
func (r *Router) From(mode Mode, from geom.P) (*Tree, error) {
	if mode != Vehicle {
		mode = Foot
	}
	N := r.N
	sts := r.starts(from, mode)
	if len(sts) == 0 {
		st, _ := r.nearest(from, mode, math.Inf(1))
		if st.edge < 0 {
			return nil, diag.New("E-RTE-002", "no open street for a %s team at all", mode)
		}
		return nil, diag.New("E-RTE-002", "the team is %.0f m from the nearest open street (more than %.0f m)", st.d, MaxSnapM)
	}
	// Dijkstra from a virtual node V joined to every way onto the streets.
	nn := len(N.Nodes)
	V := nn
	dist := make([]float64, nn+1)
	prev := make([]int, nn+1)
	src := make([]int, nn+1)
	for i := range dist {
		dist[i], prev[i], src[i] = math.Inf(1), -1, -1
	}
	dist[V] = 0
	q := &pq{{V, 0}}
	relax := func(u, v int, t float64, from int) {
		if t < dist[v] {
			dist[v], prev[v], src[v] = t, u, from
			heap.Push(q, pqItem{v, t})
		}
	}
	for q.Len() > 0 {
		it := heap.Pop(q).(pqItem)
		u := it.node
		if it.t > dist[u] {
			continue
		}
		if u == V {
			for k, s := range sts {
				se := &N.Edges[s.edge]
				sv := r.speed(se, mode)
				c := s.legT + s.pen
				if canGo(se, mode, true) {
					relax(V, se.B, c+(1-s.t)*se.Len/sv, k)
				}
				if canGo(se, mode, false) {
					relax(V, se.A, c+s.t*se.Len/sv, k)
				}
			}
			continue
		}
		for _, ei := range N.adj[u] {
			e := &N.Edges[ei]
			if !r.usable(e, mode) {
				continue
			}
			fromA := e.A == u
			if !canGo(e, mode, fromA) {
				continue
			}
			v := e.B
			if !fromA {
				v = e.A
			}
			relax(u, v, dist[u]+e.Len/r.speed(e, mode), src[u])
		}
	}
	return &Tree{r: r, mode: mode, from: from, sts: sts, dist: dist, prev: prev, src: src}, nil
}

// To builds the route from the tree's start to the casualty at `to`.
func (tr *Tree) To(to geom.P) (*Route, error) {
	r, N, mode, from, dist, prev := tr.r, tr.r.N, tr.mode, tr.from, tr.dist, tr.prev
	V := len(N.Nodes)
	// Where to leave the streets: any reachable usable piece near the casualty.
	type end struct {
		edge     int
		p        geom.P
		node     int // the node the street part comes from (-1: along a start piece)
		src      int // the way onto the streets
		roadT    float64
		total    float64 // with the start's penalty, without the end's
		d        float64 // from the casualty
		viaStart bool
	}
	var cands []end
	consider := func(ei int, a, b geom.P, distA, distB float64, nodeA, nodeB int, okAB, okBA bool, sp float64) {
		d, t := geom.SegDist(to, a, b)
		if d > MaxFinalM {
			return
		}
		p := geom.Lerp(a, b, t)
		l := a.Dist(b)
		ft, _, _ := r.legTime(p, to)
		if math.IsInf(ft, 1) {
			return // across a river
		}
		if okAB && !math.IsInf(distA, 1) {
			tt := distA + t*l/sp
			cands = append(cands, end{edge: ei, p: p, node: nodeA, src: tr.src[nodeA], roadT: tt, total: tt + ft, d: d})
		}
		if okBA && !math.IsInf(distB, 1) {
			tt := distB + (1-t)*l/sp
			cands = append(cands, end{edge: ei, p: p, node: nodeB, src: tr.src[nodeB], roadT: tt, total: tt + ft, d: d})
		}
	}
	for i := range N.Edges {
		e := &N.Edges[i]
		if !r.usable(e, mode) {
			continue
		}
		a, b := N.Nodes[e.A], N.Nodes[e.B]
		if math.Abs(a.X-to.X) > MaxFinalM+pieceM || math.Abs(a.Y-to.Y) > MaxFinalM+pieceM {
			continue
		}
		consider(i, a, b, dist[e.A], dist[e.B], e.A, e.B, canGo(e, mode, true), canGo(e, mode, false), r.speed(e, mode))
	}
	// Along a start piece itself (the casualty right by where the team joins the street).
	for k, s := range tr.sts {
		se := &N.Edges[s.edge]
		a, b := N.Nodes[se.A], N.Nodes[se.B]
		d, t := geom.SegDist(to, a, b)
		if d > MaxFinalM {
			continue
		}
		if (t >= s.t && canGo(se, mode, true)) || (t <= s.t && canGo(se, mode, false)) {
			p := geom.Lerp(a, b, t)
			tt := s.legT + s.pen + math.Abs(t-s.t)*se.Len/r.speed(se, mode)
			ft, _, _ := r.legTime(p, to)
			if math.IsInf(ft, 1) {
				continue
			}
			cands = append(cands, end{edge: s.edge, p: p, node: -1, src: k, roadT: tt, total: tt + ft, d: d, viaStart: true})
		}
	}
	closest := math.Inf(1)
	for _, c := range cands {
		closest = math.Min(closest, c.d)
	}
	best, bestScore := end{edge: -1}, math.Inf(1)
	for _, c := range cands {
		if c.d > closest+offStreetSlackM || c.src < 0 {
			continue
		}
		if sc := c.total + (c.d-closest)*offStreetPenaltyS; sc < bestScore {
			best, bestScore = c, sc
		}
	}
	// A casualty close by (the next one in the same collapse): walking
	// straight there may beat going round by the streets.
	if d := from.Dist(to); d <= MaxDirectM {
		if dt, rub, off := r.legTime(from, to); !math.IsInf(dt, 1) && (best.edge < 0 || dt < bestScore) {
			return &Route{Mode: mode, Points: []geom.P{from, to}, T: []float64{0, dt}, AccessIdx: 0, Access: from,
				LengthM: d, OffRoadM: off, RubbleM: rub, ETAs: dt, SnapM: tr.sts[0].d, DirectM: d}, nil
		}
	}
	if best.edge < 0 {
		near, ok := r.nearest(to, mode, MaxFinalM)
		if !ok {
			return nil, diag.New("E-RTE-003", "the casualty is more than %.0f m from any open street", MaxFinalM)
		}
		return nil, diag.New("E-RTE-003", "no open street leads from the team to the casualty (rubble cuts it off; nearest open street %.0f m from it)", near.d)
	}
	st := tr.sts[best.src]
	pen := st.pen // in dist[] and the candidate times: not real time
	// Rebuild the street part.
	var nodes []int
	if !best.viaStart {
		for u := best.node; u >= 0 && u != V; u = prev[u] {
			nodes = append(nodes, u)
		}
		for i, j := 0, len(nodes)-1; i < j; i, j = i+1, j-1 {
			nodes[i], nodes[j] = nodes[j], nodes[i]
		}
	}
	rt := &Route{Mode: mode, SnapM: st.d, DirectM: from.Dist(to)}
	add := func(p geom.P, t float64) {
		if n := len(rt.Points); n > 0 && rt.Points[n-1].Dist(p) < 1e-6 {
			rt.T[n-1] = math.Max(rt.T[n-1], t)
			return
		}
		rt.Points = append(rt.Points, p)
		rt.T = append(rt.T, t)
	}
	add(from, 0)
	add(st.p, st.legT)
	for _, u := range nodes {
		add(N.Nodes[u], dist[u]-pen)
	}
	add(best.p, best.roadT-pen)
	rt.AccessIdx = len(rt.Points) - 1
	rt.Access = best.p
	_, fr, fo := r.legTime(best.p, to)
	add(to, best.total-pen)
	rt.ETAs = best.total - pen
	rt.RubbleM = st.rubble + fr
	rt.OffRoadM = st.off + fo
	for i := 1; i <= rt.AccessIdx; i++ {
		rt.RoadM += rt.Points[i-1].Dist(rt.Points[i])
	}
	rt.RoadM = math.Max(0, rt.RoadM-st.d) // less the start leg
	rt.LengthM = rt.RoadM + rt.OffRoadM + rt.RubbleM
	rt.Streets = r.streets(nodes, st.edge, best.edge)
	rt.simplify(0.5)
	return rt, nil
}

// streets names the streets along the node path.
func (r *Router) streets(nodes []int, first, last int) []string {
	var out []string
	push := func(s string) {
		if s != "" && (len(out) == 0 || out[len(out)-1] != s) {
			out = append(out, s)
		}
	}
	push(r.N.Edges[first].Name)
	for i := 1; i < len(nodes); i++ {
		u, v := nodes[i-1], nodes[i]
		if u >= len(r.N.Nodes) || v >= len(r.N.Nodes) {
			continue
		}
		for _, ei := range r.N.adj[u] {
			e := &r.N.Edges[ei]
			if (e.A == u && e.B == v) || (e.B == u && e.A == v) {
				push(e.Name)
				break
			}
		}
	}
	push(r.N.Edges[last].Name)
	return out
}

// simplify drops points within tol metres of the line through their
// neighbours (Douglas-Peucker), keeping the access point and the times.
func (rt *Route) simplify(tol float64) {
	n := len(rt.Points)
	if n < 3 {
		return
	}
	keep := make([]bool, n)
	keep[0], keep[n-1], keep[rt.AccessIdx] = true, true, true
	if n > 1 {
		keep[1] = true // where the start leg joins the street
	}
	var dp func(i, j int)
	dp = func(i, j int) {
		if j <= i+1 {
			return
		}
		worst, at := 0.0, -1
		for k := i + 1; k < j; k++ {
			if d, _ := geom.SegDist(rt.Points[k], rt.Points[i], rt.Points[j]); d > worst {
				worst, at = d, k
			}
		}
		if worst > tol {
			keep[at] = true
			dp(i, at)
			dp(at, j)
		}
	}
	// Simplify between kept anchors so they stay.
	last := 0
	for k := 1; k < n; k++ {
		if keep[k] {
			dp(last, k)
			last = k
		}
	}
	var pts []geom.P
	var ts []float64
	acc := 0
	for k := 0; k < n; k++ {
		if keep[k] {
			if k == rt.AccessIdx {
				acc = len(pts)
			}
			pts = append(pts, rt.Points[k])
			ts = append(ts, rt.T[k])
		}
	}
	rt.Points, rt.T, rt.AccessIdx = pts, ts, acc
}
