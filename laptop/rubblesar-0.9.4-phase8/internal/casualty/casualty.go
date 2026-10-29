// Package casualty turns the drones' detection reports into casualty records:
// one record per person, with a position, a thermal and movement summary, a
// provisional status and START triage colour, and whether a closer look is
// needed (phase 4, casualty-analyst on the ZBook).
//
// Fusion works in two stages, following how the position errors behave:
//
//  1. Per drone. A drone's position error is mostly a slowly drifting offset
//     (GPS, heading) shared by everything it sees at about the same time, so
//     parts one drone saw together within link_m of each other are one person
//     (their relative positions are accurate even when the absolute ones are
//     not). A person spans at most span_m and has at most one head and torso
//     and two of each limb class.
//  2. Across drones. Different drones' errors are independent. A drone's
//     person record joins another drone's when they lie within
//     max(match_min_m, match_ceps x combined CEP), closest pairs first, and a
//     casualty holds at most one record per drone. This keeps two victims a
//     few metres apart apart even when both drones saw both. The casualty's
//     position is the inverse-variance average, so it tightens with every
//     drone that saw it.
//
// Records are recomputed from all reports whenever something changes; ids
// stay stable by matching each new casualty to the old one sharing most parts.
// When two casualties turn out to be one person, one keeps its id and the
// other is published once more with merged_into set.
package casualty

import (
	"math"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/intake"
)

// Body part classes (as in the detection reports); WarmSpot has no colour detection.
const (
	Hand = iota
	Arm
	Leg
	Head
	Torso
	WarmSpot
)

var classNames = []string{"hand", "arm", "leg", "head", "torso", "warm spot"}

// classLimit is how many of each part one person can show.
var classLimit = []int{2, 2, 2, 1, 1, 99}

func classOf(s string) int {
	for i, n := range classNames {
		if n == s {
			return i
		}
	}
	return -1
}

// reading is one temperature reading of a part (for the trend).
type reading struct {
	at     time.Time
	skin   float64
	rangeM float64
}

// Part is one drone's track of one body part: its latest report.
type Part struct {
	Key, UAV, Track, DetectionID string
	Class                        int
	Conf, Fused                  float64
	E, N                         float64 // local metres
	CEP, Var                     float64 // Var: per-axis variance, m^2
	First, Last                  time.Time
	Obs                          int
	RangeM, MinRangeM            float64
	Thermal                      intake.Thermal
	Move                         *intake.Movement
	TaskID                       string
	HasCrop                      bool
	readings                     map[int64]reading
	moves                        map[string]intake.Movement // each hover's verdict (latest report of it), by task
}

// fromRecord updates (or creates) a part from a stored report.
func (p *Part) fromRecord(r intake.Record, local geo.Local) {
	p.UAV, p.Track, p.DetectionID = r.UAV, r.TrackID, r.DetectionID
	p.Key = r.UAV + "/" + r.TrackID
	p.Class = classOf(r.Part)
	p.Conf, p.Fused = float64(r.Confidence), float64(r.Fused)
	p.E, p.N = local.ToLocal(geo.Point{Lat: r.Lat, Lon: r.Lon})
	p.CEP = math.Max(float64(r.CEPm), 0.1)
	sig := p.CEP / 1.1774
	p.Var = sig * sig
	p.First, p.Last = nsTime(r.FirstSeenNs, r.CreatedNs), nsTime(r.LastSeenNs, r.CreatedNs)
	p.Obs = int(r.Observations)
	p.RangeM = float64(r.RangeM)
	if r.MinRangeM > 0 {
		p.MinRangeM = float64(r.MinRangeM)
	} else if p.MinRangeM == 0 || (p.RangeM > 0 && p.RangeM < p.MinRangeM) {
		p.MinRangeM = p.RangeM
	}
	p.Thermal = r.Thermal
	if r.Movement != nil {
		p.Move, p.TaskID = r.Movement, r.TaskID
		if p.moves == nil {
			p.moves = map[string]intake.Movement{}
		}
		p.moves[r.TaskID] = *r.Movement
	}
	p.HasCrop = p.HasCrop || r.VisibleCrop != ""
	if r.Thermal.Readable && r.CaptureNs > 0 {
		if p.readings == nil {
			p.readings = map[int64]reading{}
		}
		p.readings[r.CaptureNs] = reading{at: time.Unix(0, r.CaptureNs), skin: float64(r.Thermal.SkinC), rangeM: float64(r.RangeM)}
	}
}

func nsTime(ns, fallback int64) time.Time {
	if ns > 0 {
		return time.Unix(0, ns)
	}
	return time.Unix(0, fallback)
}

func dist(ae, an, be, bn float64) float64 { return math.Hypot(ae-be, an-bn) }

// --- Stage 1: one drone's parts into person records -----------------------

type group struct {
	uav     string
	flicker bool // seen in a few frames of a hover only, no group followed throughout near it
	parts   []*Part
	e, n    float64
	v       float64 // per-axis variance (the members' errors are shared: the smallest)
	re, rn  float64 // position registered to the map frame (see clusterDrones)
}

func (g *group) update() {
	var we, wn, w float64
	g.v = math.Inf(1)
	for _, p := range g.parts {
		we, wn, w = we+p.E/p.Var, wn+p.N/p.Var, w+1/p.Var
		g.v = math.Min(g.v, p.Var)
	}
	g.e, g.n = we/w, wn/w
	g.re, g.rn = g.e, g.n
}

// appearance summarises what a record shows: its body part classes and
// whether it read warm (1), cold (-1) or not at all (0).
type appearance struct {
	classes [6]bool
	warm    int
}

func appearanceOf(parts []*Part) appearance {
	var a appearance
	w, c := 0, 0
	for _, p := range parts {
		if p.Class >= 0 {
			a.classes[p.Class] = true
		}
		if p.Thermal.Readable {
			if p.Thermal.Warm {
				w++
			} else {
				c++
			}
		}
	}
	switch {
	case w > c:
		a.warm = 1
	case c > w:
		a.warm = -1
	}
	return a
}

// related classes are often confused or shown together (a forearm and hand,
// a head and shoulders, a lower leg and foot).
var related = [6][6]bool{
	Hand: {Hand: true, Arm: true}, Arm: {Hand: true, Arm: true, Torso: true}, Leg: {Leg: true},
	Head: {Head: true, Torso: true}, Torso: {Arm: true, Head: true, Torso: true},
}

// appearanceCost: 0 when two records look alike, more when their body parts
// or temperatures disagree (they are then less likely to be the same person).
func appearanceCost(a, b appearance) float64 {
	cost := 0.0
	shared, rel := false, false
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			if a.classes[i] && b.classes[j] {
				if i == j {
					shared = true
				}
				if related[i][j] {
					rel = true
				}
			}
		}
	}
	anyA, anyB := false, false
	for i := 0; i < 5; i++ {
		anyA, anyB = anyA || a.classes[i], anyB || b.classes[i]
	}
	switch {
	case !anyA || !anyB || shared:
	case rel:
		cost += 0.3
	default:
		cost += 1.2
	}
	if a.warm != 0 && b.warm != 0 && a.warm != b.warm {
		cost += 1.2
	}
	return cost
}

func simultaneous(a, b *Part, win time.Duration) bool {
	return !a.First.Add(-win).After(b.Last) && !b.First.Add(-win).After(a.Last)
}

// consistent: the merged group still looks like one person: no more parts
// of a class than a person shows (parts of one class within duplicate_m of
// each other count once), and parts seen together no more than span_m apart.
func consistent(parts []*Part, f config.Fusion) bool {
	var byClass [6][]*Part
	for _, p := range parts {
		if p.Class >= 0 {
			byClass[p.Class] = append(byClass[p.Class], p)
		}
	}
	for c, ps := range byClass {
		if len(ps) > classLimit[c] && instances(ps, f.DuplicateM) > classLimit[c] {
			return false
		}
	}
	for i := range parts {
		for j := i + 1; j < len(parts); j++ {
			if simultaneous(parts[i], parts[j], f.Window.Duration) && dist(parts[i].E, parts[i].N, parts[j].E, parts[j].N) > f.SpanM {
				return false
			}
		}
	}
	return true
}

// instances counts the distinct parts among parts of one class: those within
// dup of each other (directly or through others) are one.
func instances(ps []*Part, dup float64) int {
	if dup <= 0 {
		return len(ps)
	}
	owner := make([]int, len(ps))
	for i := range owner {
		owner[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for owner[i] != i {
			owner[i] = owner[owner[i]]
			i = owner[i]
		}
		return i
	}
	n := len(ps)
	for i := range ps {
		for j := i + 1; j < len(ps); j++ {
			if dist(ps[i].E, ps[i].N, ps[j].E, ps[j].N) <= dup {
				if a, b := find(i), find(j); a != b {
					owner[b] = a
					n--
				}
			}
		}
	}
	return n
}

func groupDrone(parts []*Part, f config.Fusion) []*group {
	idx := map[*Part]int{}
	gs := make([]*group, len(parts))
	for i, p := range parts {
		gs[i] = &group{uav: p.UAV, parts: []*Part{p}}
		idx[p] = i
	}
	type pair struct {
		i, j int
		d    float64
	}
	var pairs []pair
	for i := range parts {
		for j := i + 1; j < len(parts); j++ {
			d := dist(parts[i].E, parts[i].N, parts[j].E, parts[j].N)
			// Parts seen together within link_m are one person; related parts
			// (hand and arm, head and torso) may lie up to span_m apart.
			rel := parts[i].Class >= 0 && parts[j].Class >= 0 && related[parts[i].Class][parts[j].Class] && parts[i].Class != parts[j].Class
			if (d <= f.LinkM || (rel && d <= f.SpanM)) && simultaneous(parts[i], parts[j], f.Window.Duration) {
				pairs = append(pairs, pair{i, j, d})
			}
		}
	}
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].d < pairs[b].d })
	owner := make([]int, len(parts)) // union-find on group indices
	for i := range owner {
		owner[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for owner[i] != i {
			owner[i] = owner[owner[i]]
			i = owner[i]
		}
		return i
	}
	for _, pr := range pairs {
		a, b := find(pr.i), find(pr.j)
		if a == b {
			continue
		}
		merged := append(append([]*Part(nil), gs[a].parts...), gs[b].parts...)
		if !consistent(merged, f) {
			continue
		}
		gs[a].parts, gs[b] = merged, nil
		owner[b] = a
	}
	var out []*group
	for i, g := range gs {
		if g != nil && find(i) == i {
			g.update()
			out = append(out, g)
		}
	}
	return absorbFlickers(out)
}

// Flickers: in a hover a part followed throughout is seen in most frames; one
// the model reported in only a few of them (a piece of a limb, a second
// outline, debris taken for a part now and then) is no person of its own when
// a group followed throughout lies within flickerM. It joins that group (the
// nearest), whatever the limits: otherwise it becomes a record of its own next
// to the victim. A flicker with no such group near may support a casualty
// another window found, but does not start one (debris the model took for a
// part now and then). Survey passes (every part seen in a few frames) are
// left alone.
const (
	flickerM     = 3.0
	flickerShare = 0.25 // seen in at most this share of the frames of the best-seen part nearby
	flickerMinOb = 8    // ...which was seen in at least this many frames
)

func absorbFlickers(gs []*group) []*group {
	maxObs := func(g *group) int {
		m := 0
		for _, p := range g.parts {
			m = max(m, p.Obs)
		}
		return m
	}
	dead := make([]bool, len(gs))
	bestObs := map[string]int{} // frames of the best-seen part of each hover
	for _, g := range gs {
		if t := g.task(); t != "" {
			bestObs[t] = max(bestObs[t], maxObs(g))
		}
	}
	for i, g := range gs {
		task := g.task()
		if task == "" {
			continue
		}
		g.flicker = bestObs[task] >= flickerMinOb && float64(maxObs(g)) <= flickerShare*float64(bestObs[task])
		best, bd := -1, flickerM
		for j, h := range gs {
			if j == i || dead[j] || h.task() != task {
				continue
			}
			hm := maxObs(h)
			if hm < flickerMinOb || float64(maxObs(g)) > flickerShare*float64(hm) {
				continue
			}
			if d := dist(g.e, g.n, h.e, h.n); d <= bd {
				best, bd = j, d
			}
		}
		if best >= 0 {
			gs[best].parts = append(gs[best].parts, g.parts...)
			gs[best].update()
			dead[i] = true
		}
	}
	out := gs[:0]
	for i, g := range gs {
		if !dead[i] {
			out = append(out, g)
		}
	}
	return out
}

// --- Stage 2: person records of different drones into casualties ----------

type cluster struct {
	groups  []*group
	uavs    map[string]bool
	windows map[int]bool // observation windows already in it (one record each)
	e, n    float64      // fused position: the members' own positions, weighted
	v       float64
	re, rn  float64 // the members' registered positions, for matching
}

func (c *cluster) update() {
	var we, wn, w, re, rn float64
	for _, g := range c.groups {
		we, wn, w = we+g.e/g.v, wn+g.n/g.v, w+1/g.v
		re, rn = re+g.re/g.v, rn+g.rn/g.v
	}
	c.e, c.n, c.v = we/w, wn/w, 1/w
	c.re, c.rn = re/w, rn/w
}

func (c *cluster) cep() float64 { return 1.1774 * math.Sqrt(c.v) }

func (c *cluster) look() appearance { return appearanceOf(c.parts()) }

func (c *cluster) parts() []*Part {
	var out []*Part
	for _, g := range c.groups {
		out = append(out, g.parts...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func disjoint(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return false
		}
	}
	return true
}

// A window is one drone's continuous stretch of observation (an
// investigation hover, a short stretch of a survey): within it the drone's
// position error is close to a single offset, shared by all its records.
// GPS error drifts (Gauss-Markov, tau about 30 s), so a window without a task
// lasts at most maxWindow; a hover is one window however long it is.
type window struct {
	uav         string
	groups      []*group
	first, last time.Time
}

// task is the investigation the window belongs to (most of its parts' task), or "".
func (w *window) task() string {
	cnt := map[string]int{}
	best, bn := "", 0
	for _, g := range w.groups {
		for _, p := range g.parts {
			if p.TaskID == "" {
				continue
			}
			cnt[p.TaskID]++
			if cnt[p.TaskID] > bn || (cnt[p.TaskID] == bn && p.TaskID < best) {
				best, bn = p.TaskID, cnt[p.TaskID]
			}
		}
	}
	return best
}

// task of a record: the investigation most of its parts were watched in.
func (g *group) task() string { return (&window{groups: []*group{g}}).task() }

const (
	windowGap = 15 * time.Second
	maxWindow = 30 * time.Second
)

func windowsOf(groups []*group) []*window {
	var out []*window
	hover := map[string]*window{}
	free := map[string][]*group{}
	var uavs []string
	add := func(w *window, g *group) {
		w.groups = append(w.groups, g)
		if f := g.first(); w.first.IsZero() || f.Before(w.first) {
			w.first = f
		}
		if l := g.last(); l.After(w.last) {
			w.last = l
		}
	}
	for _, g := range groups {
		if t := g.task(); t != "" {
			k := g.uav + "\x00" + t
			w := hover[k]
			if w == nil {
				w = &window{uav: g.uav}
				hover[k] = w
				out = append(out, w)
			}
			add(w, g)
			continue
		}
		if _, ok := free[g.uav]; !ok {
			uavs = append(uavs, g.uav)
		}
		free[g.uav] = append(free[g.uav], g)
	}
	for _, u := range uavs {
		gs := free[u]
		sort.SliceStable(gs, func(i, j int) bool { return gs[i].first().Before(gs[j].first()) })
		var w *window
		for _, g := range gs {
			f := g.first()
			if w == nil || f.After(w.last.Add(windowGap)) || f.After(w.first.Add(maxWindow)) {
				w = &window{uav: u}
				out = append(out, w)
			}
			add(w, g)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].first.Equal(out[j].first) {
			return out[i].first.Before(out[j].first)
		}
		return out[i].uav < out[j].uav
	})
	return out
}

func (g *group) first() time.Time {
	t := g.parts[0].First
	for _, p := range g.parts {
		if p.First.Before(t) {
			t = p.First
		}
	}
	return t
}

func (g *group) last() time.Time {
	t := g.parts[0].Last
	for _, p := range g.parts {
		if p.Last.After(t) {
			t = p.Last
		}
	}
	return t
}

// Matching costs (roughly negative log-likelihoods, see clusterDrones). Tuned
// with TestFusionQuality.
const (
	residualSigmaM = 0.8 // spread of one person's records once the window is registered, per axis
	offsetSigmaM   = 2.1 // spread of the offset between two drones' errors (GPS 1.5 m each), per axis
	newPersonCost  = 3.5 // a record that is nobody seen before
	missCost       = 1.0 // a casualty in view that the window did not see
	missTargetCost = 3.0 // ... the one the hover was sent to look at
	viewM          = 6.0 // a casualty within this of the window's records or target counts as in view
)

// clusterDrones merges the person records of the observation windows, in time
// order. Each window is matched to the casualties so far, separately for each
// neighbourhood (records within the gate of each other). For every candidate
// offset of the window (the shift between its records and the casualties) an
// optimal assignment (Hungarian method) pairs records and casualties; the
// hypothesis with the lowest total cost wins. Costs are shaped like negative
// log-likelihoods: a pair costs its squared residual after the offset
// (residualSigmaM) plus how much its body parts and temperature disagree; a
// record left unmatched is a new person (newPersonCost); a casualty in view
// that the window did not see costs missCost (missTargetCost for the casualty
// a hover was sent to); and the offset itself costs its squared size
// (offsetSigmaM). A casualty holds at most one record per window. The chosen
// offset then registers the window's records to the map frame (the first
// window's), so later windows are matched against positions that agree with
// each other; the published position is still the plain weighted mean, which
// averages the drones' errors. This keeps neighbouring victims apart when two
// drones' errors differ by about their spacing, and lets a drone's second
// visit join what it saw before.
func clusterDrones(groups []*group, f config.Fusion, h Hints) []*cluster {
	var cs []*cluster
	for wi, w := range windowsOf(groups) {
		gs := w.groups
		// An investigation hover: the record nearest the target point and the
		// casualty the drone was sent to.
		ti, tj := -1, -1
		var te, tn float64
		if task := w.task(); task != "" && h.Task != nil && h.OldID != nil {
			if cid, e, n, ok := h.Task(task); ok {
				te, tn = e, n
				for i, c := range cs {
					if h.OldID(c.parts()) == cid {
						ti = i
					}
				}
				bd := math.Inf(1)
				for j, g := range gs {
					if d := dist(g.e, g.n, te, tn); d < bd {
						tj, bd = j, d
					}
				}
			}
		}
		gate := func(c *cluster, g *group) float64 {
			// The casualty's registered position carries the map frame's own
			// error (about one record's): it does not shrink below that.
			gc := 1.1774 * math.Sqrt(g.v)
			return math.Max(f.MatchMinM, f.MatchCEPs*math.Hypot(math.Max(c.cep(), 0.7*gc), gc))
		}
		// Neighbourhoods: union-find over clusters (0..n-1) and groups (n..n+m-1).
		n, m := len(cs), len(gs)
		parent := make([]int, n+m)
		for i := range parent {
			parent[i] = i
		}
		var find func(int) int
		find = func(i int) int {
			for parent[i] != i {
				parent[i] = parent[parent[i]]
				i = parent[i]
			}
			return i
		}
		for i, c := range cs {
			if c.windows[wi] {
				continue
			}
			for j, g := range gs {
				if dist(c.re, c.rn, g.e, g.n) <= gate(c, g) {
					parent[find(i)] = find(n + j)
				}
			}
		}
		comps := map[int][2][]int{}
		for i := 0; i < n+m; i++ {
			r := find(i)
			cp := comps[r]
			if i < n {
				cp[0] = append(cp[0], i)
			} else {
				cp[1] = append(cp[1], i-n)
			}
			comps[r] = cp
		}
		match := make([]int, m)
		for j := range match {
			match[j] = -1
		}
		roots := make([]int, 0, len(comps))
		for r := range comps {
			roots = append(roots, r)
		}
		sort.Ints(roots)
		// Each neighbourhood's offset registers its records to the map frame;
		// records with no casualty near take the window's mean offset.
		var oe, on float64
		no := 0
		var lone [][]int
		for _, r := range roots {
			ci, gi := comps[r][0], comps[r][1]
			if len(gi) == 0 {
				continue
			}
			if len(ci) == 0 {
				lone = append(lone, gi)
				continue
			}
			res, o := matchWithOffset(cs, gs, ci, gi, gate, ti, tj, te, tn)
			for k, j := range res {
				match[gi[k]] = j
				gs[gi[k]].re, gs[gi[k]].rn = gs[gi[k]].e-o.e, gs[gi[k]].n-o.n
			}
			oe, on, no = oe+o.e, on+o.n, no+1
		}
		if no > 0 {
			oe, on = oe/float64(no), on/float64(no)
		}
		for _, gi := range lone {
			for _, j := range gi {
				gs[j].re, gs[j].rn = gs[j].e-oe, gs[j].n-on
			}
		}
		for j, g := range gs {
			if i := match[j]; i >= 0 {
				cs[i].groups = append(cs[i].groups, g)
				cs[i].uavs[g.uav], cs[i].windows[wi] = true, true
				continue
			}
			if g.flicker {
				continue // not enough to start a casualty (see absorbFlickers)
			}
			cs = append(cs, &cluster{groups: []*group{g}, uavs: map[string]bool{g.uav: true}, windows: map[int]bool{wi: true}})
		}
		for _, c := range cs {
			c.update()
		}
	}
	return cs
}

// matchWithOffset matches groups gi to clusters ci of one neighbourhood; it
// returns, for each group of gi, the cluster index it joins or -1. ti is the
// hover's target cluster and tj the record nearest the target point (or -1);
// te, tn the target point (valid when tj >= 0).
type off struct{ e, n float64 }

func matchWithOffset(cs []*cluster, gs []*group, ci, gi []int, gate func(*cluster, *group) float64, ti, tj int, te, tn float64) ([]int, off) {
	var pairOffs []off
	for _, i := range ci {
		for _, j := range gi {
			if dist(cs[i].re, cs[i].rn, gs[j].e, gs[j].n) <= gate(cs[i], gs[j]) {
				pairOffs = append(pairOffs, off{gs[j].e - cs[i].re, gs[j].n - cs[i].rn})
			}
		}
	}
	cands := append([]off{{0, 0}}, topOffsets(pairOffs, maxOffsetCands)...)
	looks := make(map[int]appearance)
	for _, i := range ci {
		looks[i] = cs[i].look()
	}
	gl := make(map[int]appearance)
	for _, j := range gi {
		gl[j] = appearanceOf(gs[j].parts)
	}
	// Casualties in view of the window: near its records or its target.
	miss := make([]float64, len(ci))
	for a, i := range ci {
		in := tj >= 0 && dist(cs[i].re, cs[i].rn, te, tn) <= viewM
		for _, j := range gi {
			in = in || dist(cs[i].re, cs[i].rn, gs[j].e, gs[j].n) <= viewM
		}
		if in {
			miss[a] = missCost
		}
		if i == ti {
			miss[a] = missTargetCost
		}
	}
	newp := make([]float64, len(gi))
	for b := range gi {
		newp[b] = newPersonCost
	}
	solve := func(o off) (float64, []int) {
		cost := make([][]float64, len(ci))
		for a, i := range ci {
			cost[a] = make([]float64, len(gi))
			for b, j := range gi {
				cost[a][b] = math.Inf(1)
				if dist(cs[i].re, cs[i].rn, gs[j].e, gs[j].n) > gate(cs[i], gs[j]) {
					continue
				}
				r := dist(cs[i].re+o.e, cs[i].rn+o.n, gs[j].e, gs[j].n)
				if r > 3*residualSigmaM {
					continue
				}
				cost[a][b] = r*r/(2*residualSigmaM*residualSigmaM) + appearanceCost(looks[i], gl[j])
			}
		}
		res, total := assign(cost, miss, newp)
		return total + (o.e*o.e+o.n*o.n)/(2*offsetSigmaM*offsetSigmaM), res
	}
	best, bestRes, bestOff := math.Inf(1), []int(nil), off{}
	for _, o := range cands {
		t, res := solve(o)
		// Refine the offset: the shift that best fits the pairs it matched,
		// pulled towards zero by its prior.
		var se, sn float64
		k := 0
		for b, a := range res {
			if a >= 0 {
				se, sn, k = se+gs[gi[b]].e-cs[ci[a]].re, sn+gs[gi[b]].n-cs[ci[a]].rn, k+1
			}
		}
		if k > 0 {
			shrink := float64(k) / (float64(k) + residualSigmaM*residualSigmaM/(offsetSigmaM*offsetSigmaM))
			o2 := off{shrink * se / float64(k), shrink * sn / float64(k)}
			if t2, res2 := solve(o2); t2 < t {
				t, res, o = t2, res2, o2
			}
		}
		if t < best-1e-9 {
			best, bestRes, bestOff = t, res, o
		}
	}
	out := make([]int, len(gi))
	for b, a := range bestRes {
		out[b] = -1
		if a >= 0 {
			out[b] = ci[a]
		}
	}
	return out, bestOff
}

// maxOffsetCands: at most this many offsets are tried per neighbourhood.
const maxOffsetCands = 24

// topOffsets keeps the candidate offsets most record pairs agree on (a vote
// with a kernel of the residual spread, weighted by the offset's prior), so a
// busy neighbourhood is not solved once for every pair. With few pairs it
// keeps them all.
func topOffsets(pairs []off, k int) []off {
	if len(pairs) <= k {
		return pairs
	}
	type sc struct {
		o off
		v float64
	}
	s2 := 2 * residualSigmaM * residualSigmaM
	scored := make([]sc, len(pairs))
	for i, a := range pairs {
		v := 0.0
		for _, b := range pairs {
			d2 := (a.e-b.e)*(a.e-b.e) + (a.n-b.n)*(a.n-b.n)
			if d2 < 9*s2 {
				v += math.Exp(-d2 / s2)
			}
		}
		// Votes count like matched pairs; the prior like its cost.
		scored[i] = sc{a, v - (a.e*a.e+a.n*a.n)/(2*offsetSigmaM*offsetSigmaM)}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].v > scored[j].v })
	var out []off
	for _, c := range scored {
		dup := false
		for _, o := range out {
			if math.Hypot(o.e-c.o.e, o.n-c.o.n) < 0.25 {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, c.o)
		}
		if len(out) == k {
			break
		}
	}
	return out
}

// assign solves the matching of rows to columns with the given costs (Inf =
// not allowed); row a may stay unmatched at rowMiss[a] and column b at
// colMiss[b]. It returns, for every column, the row matched to it or -1, and
// the total cost. Rows and columns that cannot reach each other are solved
// separately (the same optimum, much faster for large sparse problems).
func assign(cost [][]float64, rowMiss, colMiss []float64) ([]int, float64) {
	n, m := len(rowMiss), len(colMiss)
	parent := make([]int, n+m)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for a := 0; a < n; a++ {
		for b := 0; b < m; b++ {
			if !math.IsInf(cost[a][b], 1) {
				parent[find(a)] = find(n + b)
			}
		}
	}
	comps := map[int][2][]int{}
	var roots []int
	for i := 0; i < n+m; i++ {
		r := find(i)
		c, ok := comps[r]
		if !ok {
			roots = append(roots, r)
		}
		if i < n {
			c[0] = append(c[0], i)
		} else {
			c[1] = append(c[1], i-n)
		}
		comps[r] = c
	}
	out := make([]int, m)
	for b := range out {
		out[b] = -1
	}
	total := 0.0
	for _, r := range roots {
		rows, cols := comps[r][0], comps[r][1]
		switch {
		case len(cols) == 0:
			for _, a := range rows {
				total += rowMiss[a]
			}
			continue
		case len(rows) == 0:
			for _, b := range cols {
				total += colMiss[b]
			}
			continue
		}
		sub := make([][]float64, len(rows))
		for x, a := range rows {
			sub[x] = make([]float64, len(cols))
			for y, b := range cols {
				sub[x][y] = cost[a][b]
			}
		}
		rm, cm := make([]float64, len(rows)), make([]float64, len(cols))
		for x, a := range rows {
			rm[x] = rowMiss[a]
		}
		for y, b := range cols {
			cm[y] = colMiss[b]
		}
		res, t := assignDense(sub, rm, cm)
		total += t
		for y, x := range res {
			if x >= 0 {
				out[cols[y]] = rows[x]
			}
		}
	}
	return out, total
}

// assignDense is assign on one component (Hungarian method on the square
// problem with a dummy row per column and a dummy column per row).
func assignDense(cost [][]float64, rowMiss, colMiss []float64) ([]int, float64) {
	const big = 1e9
	n, m := len(rowMiss), len(colMiss)
	N := n + m
	a := make([][]float64, N)
	for i := range a {
		a[i] = make([]float64, N)
		for j := range a[i] {
			switch {
			case i < n && j < m:
				a[i][j] = cost[i][j]
				if math.IsInf(a[i][j], 1) {
					a[i][j] = big
				}
			case i < n && j >= m: // row i unmatched
				a[i][j] = big
				if j-m == i {
					a[i][j] = rowMiss[i]
				}
			case i >= n && j < m: // column j unmatched
				a[i][j] = big
				if i-n == j {
					a[i][j] = colMiss[j]
				}
			default:
				a[i][j] = 0
			}
		}
	}
	rowOf := hungarian(a)
	out := make([]int, m)
	total := 0.0
	for j := 0; j < N; j++ {
		total += a[rowOf[j]][j]
	}
	for j := 0; j < m; j++ {
		out[j] = -1
		if i := rowOf[j]; i >= 0 && i < n && a[i][j] < big/2 {
			out[j] = i
		}
	}
	return out, total
}

// hungarian solves the square assignment problem (minimum total cost) and
// returns, for every column, its row (e-maxx formulation with potentials).
func hungarian(a [][]float64) []int {
	n := len(a)
	u, v := make([]float64, n+1), make([]float64, n+1)
	p, way := make([]int, n+1), make([]int, n+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]float64, n+1)
		used := make([]bool, n+1)
		for j := range minv {
			minv[j] = math.Inf(1)
		}
		for {
			used[j0] = true
			i0, delta, j1 := p[j0], math.Inf(1), 0
			for j := 1; j <= n; j++ {
				if used[j] {
					continue
				}
				cur := a[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j], way[j] = cur, j0
				}
				if minv[j] < delta {
					delta, j1 = minv[j], j
				}
			}
			for j := 0; j <= n; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}
	rowOf := make([]int, n)
	for j := 1; j <= n; j++ {
		rowOf[j-1] = p[j] - 1
	}
	return rowOf
}

// clusterAll runs both stages over every part, in a deterministic order.
// Hints help the matching of investigation hovers: a hover's task names the
// casualty the drone was sent to, and what it sees nearest the target point is
// most likely that casualty.
type Hints struct {
	// Task returns the casualty id and target (local metres) of a task.
	Task func(taskID string) (casualty string, e, n float64, ok bool)
	// OldID returns the casualty id most of these parts belonged to before.
	OldID func(parts []*Part) string
}

func clusterAll(parts []*Part, f config.Fusion, h Hints) []*cluster {
	byUAV := map[string][]*Part{}
	var uavs []string
	for _, p := range parts {
		if _, ok := byUAV[p.UAV]; !ok {
			uavs = append(uavs, p.UAV)
		}
		byUAV[p.UAV] = append(byUAV[p.UAV], p)
	}
	sort.Strings(uavs)
	var groups []*group
	for _, u := range uavs {
		groups = append(groups, groupDrone(byUAV[u], f)...)
	}
	cs := clusterDrones(groups, f, h)
	sort.SliceStable(cs, func(i, j int) bool { return firstSeen(cs[i]).Before(firstSeen(cs[j])) })
	return cs
}

func firstSeen(c *cluster) time.Time {
	t := time.Time{}
	for _, p := range c.parts() {
		if t.IsZero() || p.First.Before(t) {
			t = p.First
		}
	}
	return t
}

func keysOf(parts []*Part) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.Key
	}
	sort.Strings(out)
	return out
}

func partNames(parts []*Part) string {
	seen := map[int]int{}
	var order []int
	for _, p := range parts {
		if p.Class < 0 {
			continue
		}
		if seen[p.Class] == 0 {
			order = append(order, p.Class)
		}
		seen[p.Class]++
	}
	sort.Ints(order)
	var s []string
	for _, c := range order {
		s = append(s, classNames[c])
	}
	return strings.Join(s, ", ")
}
