// Package score compares geolocated body-part detections with the scenario's
// ground truth: per-class precision and recall within a match radius, victim
// coverage, and geolocation error statistics.
package score

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
)

// Detection is one geolocated body part reported by the pipeline.
type Detection struct {
	Class      string  `json:"class"` // hand, arm, leg, head, torso
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
	Confidence float64 `json:"confidence,omitempty"`
	UAV        string  `json:"uav,omitempty"`
	FrameID    string  `json:"frame_id,omitempty"`
}

// File is the detections JSON exchanged with the scorer.
type File struct {
	Schema     int         `json:"schema"`
	ScenarioID string      `json:"scenario_id"`
	Source     string      `json:"source,omitempty"`
	Detections []Detection `json:"detections"`
}

// LoadFile reads and checks a detections file.
func LoadFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, diag.Wrap("E-SCR-001", err, "reading %s", path)
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, diag.Wrap("E-SCR-001", err, "parsing %s", path)
	}
	if f.Schema != 1 {
		return nil, diag.New("E-SCR-001", "%s: schema %d, expected 1", path, f.Schema)
	}
	for i, d := range f.Detections {
		if ClassID(d.Class) == 0 {
			return nil, diag.New("E-SCR-001", "%s: detection %d has class %q (use hand, arm, leg, head or torso)", path, i, d.Class)
		}
		if math.Abs(d.Lat) > 90 || math.Abs(d.Lon) > 180 || (d.Lat == 0 && d.Lon == 0) {
			return nil, diag.New("E-SCR-001", "%s: detection %d has position %.6f,%.6f", path, i, d.Lat, d.Lon)
		}
	}
	return &f, nil
}

// ClassID maps a class name to its id (0 = unknown).
func ClassID(name string) int {
	for i, n := range scenario.PartNames {
		if i > 0 && strings.EqualFold(n, name) {
			return i
		}
	}
	return 0
}

// Options controls matching.
type Options struct {
	RadiusM        float64      // a detection matches a truth part of the same class within this distance
	MinTruthAreaM2 float64      // smaller visible parts are not expected to be found
	OnlyVictims    map[int]bool // nil = all victims; else restrict the truth to these (e.g. those the drones saw)
}

// DefaultOptions are the phase 1 acceptance settings.
func DefaultOptions() Options { return Options{RadiusM: 2.0, MinTruthAreaM2: 0.004} }

// ClassResult is per-class performance.
type ClassResult struct {
	Class      string  `json:"class"`
	Truth      int     `json:"truth"`
	Detections int     `json:"detections"`
	TP         int     `json:"tp"`
	FP         int     `json:"fp"`
	FN         int     `json:"fn"`
	Duplicates int     `json:"duplicates"`
	Ignored    int     `json:"ignored"` // near a part too small to be expected
	Precision  float64 `json:"precision"`
	Recall     float64 `json:"recall"`
}

// Match pairs a detection with a truth part.
type Match struct {
	Det      int     `json:"det"`
	Victim   int     `json:"victim"`
	VictimID string  `json:"victim_id"`
	Class    string  `json:"class"`
	ErrorM   float64 `json:"error_m"`
}

// Result is the full scoring outcome.
type Result struct {
	Options        Options       `json:"-"`
	RadiusM        float64       `json:"radius_m"`
	MinTruthAreaM2 float64       `json:"min_truth_area_m2"`
	Classes        []ClassResult `json:"classes"`
	Overall        ClassResult   `json:"overall"`
	VictimsTotal   int           `json:"victims_total"`
	VictimsFound   int           `json:"victims_found"`
	VictimsMissed  []string      `json:"victims_missed"`
	ErrMeanM       float64       `json:"geoloc_error_mean_m"`
	ErrMedianM     float64       `json:"geoloc_error_median_m"`
	ErrP95M        float64       `json:"geoloc_error_p95_m"`
	ErrMaxM        float64       `json:"geoloc_error_max_m"`
	Matches        []Match       `json:"matches"`
	FalsePositives []int         `json:"false_positive_dets"`
	Missed         []string      `json:"missed_truth"`
}

// Score matches detections to truth parts greedily by distance, per class.
func Score(s *scenario.Scenario, truth []render.TruthPart, dets []Detection, o Options) *Result {
	local := s.Local()
	type tp struct {
		t    render.TruthPart
		e, n float64
	}
	var ts []tp
	victims := map[int]string{}
	var ign []tp // parts too small to be expected: detecting them is neither right nor wrong
	for _, t := range truth {
		if o.OnlyVictims != nil && !o.OnlyVictims[t.Victim] {
			continue
		}
		if t.AreaM2 < o.MinTruthAreaM2 {
			ign = append(ign, tp{t, t.E, t.N})
			continue
		}
		ts = append(ts, tp{t, t.E, t.N})
		victims[t.Victim] = t.VictimID
	}
	de := make([][2]float64, len(dets))
	for i, d := range dets {
		e, n := local.ToLocal(geo.Point{Lat: d.Lat, Lon: d.Lon})
		de[i] = [2]float64{e, n}
	}
	type pair struct {
		d, t int
		dist float64
	}
	var pairs []pair
	for di, d := range dets {
		c := ClassID(d.Class)
		for ti, t := range ts {
			if t.t.Class != c {
				continue
			}
			if dist := math.Hypot(de[di][0]-t.e, de[di][1]-t.n); dist <= o.RadiusM {
				pairs = append(pairs, pair{di, ti, dist})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].dist != pairs[j].dist {
			return pairs[i].dist < pairs[j].dist
		}
		if pairs[i].d != pairs[j].d {
			return pairs[i].d < pairs[j].d
		}
		return pairs[i].t < pairs[j].t
	})
	detUsed := make([]bool, len(dets))
	truthUsed := make([]bool, len(ts))
	dup := make([]bool, len(dets))
	r := &Result{Options: o, RadiusM: o.RadiusM, MinTruthAreaM2: o.MinTruthAreaM2, Matches: []Match{}, FalsePositives: []int{}, Missed: []string{}, VictimsMissed: []string{}}
	for _, p := range pairs {
		if detUsed[p.d] || truthUsed[p.t] {
			continue
		}
		detUsed[p.d], truthUsed[p.t] = true, true
		t := ts[p.t].t
		r.Matches = append(r.Matches, Match{Det: p.d, Victim: t.Victim, VictimID: t.VictimID, Class: t.ClassName, ErrorM: math.Round(p.dist*1000) / 1000})
	}
	// Unmatched detections close to an already-found part of the same class are duplicates, not false alarms.
	for _, p := range pairs {
		if !detUsed[p.d] && truthUsed[p.t] {
			dup[p.d] = true
		}
	}
	ignored := make([]bool, len(dets))
	for di, d := range dets {
		if detUsed[di] || dup[di] {
			continue
		}
		for _, t := range ign {
			if t.t.Class == ClassID(d.Class) && math.Hypot(de[di][0]-t.e, de[di][1]-t.n) <= o.RadiusM {
				ignored[di] = true
				break
			}
		}
	}
	per := map[int]*ClassResult{}
	for c := 1; c < scenario.NumParts; c++ {
		per[c] = &ClassResult{Class: scenario.PartNames[c]}
	}
	for i, t := range ts {
		per[t.t.Class].Truth++
		if truthUsed[i] {
			per[t.t.Class].TP++
		} else {
			per[t.t.Class].FN++
			r.Missed = append(r.Missed, fmt.Sprintf("%s/%s", t.t.VictimID, t.t.ClassName))
		}
	}
	for i, d := range dets {
		c := per[ClassID(d.Class)]
		c.Detections++
		switch {
		case detUsed[i]:
		case dup[i]:
			c.Duplicates++
		case ignored[i]:
			c.Ignored++
		default:
			c.FP++
			r.FalsePositives = append(r.FalsePositives, i)
		}
	}
	var all ClassResult
	all.Class = "all"
	for c := 1; c < scenario.NumParts; c++ {
		x := per[c]
		x.Precision, x.Recall = ratio(x.TP, x.TP+x.FP), ratio(x.TP, x.TP+x.FN)
		r.Classes = append(r.Classes, *x)
		all.Truth += x.Truth
		all.Detections += x.Detections
		all.TP += x.TP
		all.FP += x.FP
		all.FN += x.FN
		all.Duplicates += x.Duplicates
		all.Ignored += x.Ignored
	}
	all.Precision, all.Recall = ratio(all.TP, all.TP+all.FP), ratio(all.TP, all.TP+all.FN)
	r.Overall = all
	// Victim coverage: found if any of its parts was matched.
	found := map[int]bool{}
	for _, m := range r.Matches {
		found[m.Victim] = true
	}
	r.VictimsTotal = len(victims)
	ids := make([]int, 0, len(victims))
	for v := range victims {
		ids = append(ids, v)
	}
	sort.Ints(ids)
	for _, v := range ids {
		if found[v] {
			r.VictimsFound++
		} else {
			r.VictimsMissed = append(r.VictimsMissed, victims[v])
		}
	}
	// Geolocation error statistics.
	if len(r.Matches) > 0 {
		errs := make([]float64, len(r.Matches))
		sum := 0.0
		for i, m := range r.Matches {
			errs[i] = m.ErrorM
			sum += m.ErrorM
		}
		sort.Float64s(errs)
		r.ErrMeanM = math.Round(sum/float64(len(errs))*1000) / 1000
		r.ErrMedianM = pct(errs, 0.5)
		r.ErrP95M = pct(errs, 0.95)
		r.ErrMaxM = errs[len(errs)-1]
	}
	return r
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return math.Round(float64(a)/float64(b)*1000) / 1000
}

func pct(sorted []float64, p float64) float64 {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}

// Text renders a human-readable summary.
func (r *Result) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "match radius %.1f m, truth parts >= %.4f m2\n", r.RadiusM, r.MinTruthAreaM2)
	fmt.Fprintf(&b, "%-7s %6s %6s %5s %5s %5s %5s %9s %7s\n", "class", "truth", "dets", "TP", "FP", "FN", "dup", "precision", "recall")
	for _, c := range append(r.Classes, r.Overall) {
		fmt.Fprintf(&b, "%-7s %6d %6d %5d %5d %5d %5d %9.3f %7.3f\n", c.Class, c.Truth, c.Detections, c.TP, c.FP, c.FN, c.Duplicates, c.Precision, c.Recall)
	}
	fmt.Fprintf(&b, "victims found %d / %d", r.VictimsFound, r.VictimsTotal)
	if len(r.VictimsMissed) > 0 {
		fmt.Fprintf(&b, " (missed: %s)", strings.Join(r.VictimsMissed, " "))
	}
	fmt.Fprintf(&b, "\ngeolocation error: mean %.2f m, median %.2f m, p95 %.2f m, max %.2f m\n", r.ErrMeanM, r.ErrMedianM, r.ErrP95M, r.ErrMaxM)
	return b.String()
}

// FromTruth builds detections that exactly match the truth (for self-tests).
func FromTruth(truth []render.TruthPart, minArea float64) []Detection {
	var out []Detection
	for _, t := range truth {
		if t.AreaM2 >= minArea {
			out = append(out, Detection{Class: t.ClassName, Lat: t.Lat, Lon: t.Lon, Confidence: 1})
		}
	}
	return out
}

// AllTruth gathers the truth of every site in a world.
func AllTruth(ms []*render.Manifest) []render.TruthPart {
	var out []render.TruthPart
	for _, m := range ms {
		out = append(out, m.Truth...)
	}
	return out
}

// SelfTest exercises the scorer with constructed cases and returns problems found.
func SelfTest(s *scenario.Scenario, truth []render.TruthPart) []string {
	var probs []string
	o := DefaultOptions()
	perfect := FromTruth(truth, o.MinTruthAreaM2)
	if len(perfect) == 0 {
		return []string{"no truth parts above the minimum area"}
	}
	r := Score(s, truth, perfect, o)
	if r.Overall.Precision != 1 || r.Overall.Recall != 1 || r.ErrMaxM > 0.02 || r.VictimsFound != r.VictimsTotal {
		probs = append(probs, fmt.Sprintf("perfect detections: precision %.3f recall %.3f max error %.3f victims %d/%d",
			r.Overall.Precision, r.Overall.Recall, r.ErrMaxM, r.VictimsFound, r.VictimsTotal))
	}
	local := s.Local()
	shift := func(d Detection, de, dn float64) Detection {
		e, n := local.ToLocal(geo.Point{Lat: d.Lat, Lon: d.Lon})
		p := local.ToPoint(e+de, n+dn)
		d.Lat, d.Lon = p.Lat, p.Lon
		return d
	}
	// 1.5 m off: still matched, error ~1.5 m.
	var near []Detection
	for _, d := range perfect {
		near = append(near, shift(d, 1.5, 0))
	}
	r = Score(s, truth, near, o)
	if r.Overall.Recall < 0.9 || math.Abs(r.ErrMedianM-1.5) > 0.6 {
		probs = append(probs, fmt.Sprintf("1.5 m offsets: recall %.3f median error %.2f (want >= 0.9 and ~1.5)", r.Overall.Recall, r.ErrMedianM))
	}
	// One detection 5 m off, one with the wrong class, one duplicate.
	bad := append([]Detection{}, perfect...)
	bad[0] = shift(bad[0], 5, 0)
	wrong := ClassID(bad[1].Class)%(scenario.NumParts-1) + 1
	bad[1].Class = scenario.PartNames[wrong]
	bad = append(bad, shift(perfect[2], 0.2, 0))
	r = Score(s, truth, bad, o)
	if r.Overall.FP < 1 || r.Overall.FN < 1 || r.Overall.Duplicates < 1 {
		probs = append(probs, fmt.Sprintf("constructed errors not detected: FP %d FN %d duplicates %d", r.Overall.FP, r.Overall.FN, r.Overall.Duplicates))
	}
	if r := Score(s, truth, nil, o); r.Overall.Recall != 0 || r.Overall.Precision != 1 {
		probs = append(probs, "no detections should give recall 0")
	}
	return probs
}
