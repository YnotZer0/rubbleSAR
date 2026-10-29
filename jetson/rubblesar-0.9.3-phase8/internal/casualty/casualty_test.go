package casualty

import (
	"math"
	"math/rand"
	"strconv"
	"testing"
	"time"

	"rubblesar/internal/config"
	"rubblesar/internal/geo"
	"rubblesar/internal/intake"
	"rubblesar/internal/sarpb"
)

var (
	centre = geo.Point{Lat: 36.2025, Lon: 36.16056}
	local  = geo.NewLocal(centre)
	t0     = time.Date(2026, 2, 10, 6, 0, 0, 0, time.UTC)
)

func testDefaults() (config.Fusion, config.Triage) {
	f := config.Fusion{LinkM: 2.0, SpanM: 2.5, DuplicateM: 1.5, Window: config.Duration{Duration: 10 * time.Second}, MatchMinM: 5, MatchCEPs: 3,
		PublishInterval: config.Duration{Duration: 10 * time.Second}}
	tr := config.Triage{SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3, ClothedMinC: 15, ClothedDeltaC: 8, CloseRangeM: 20, MinWatchS: 20, MovedFrames: 2, CoolingCPerH: 0.5,
		TrendMinSpan: config.Duration{Duration: 10 * time.Minute}, MaxCEPm: 5, NotFoundLooks: 2}
	return f, tr
}

type rp struct {
	uav, track string
	rev        uint32
	part       string
	e, n, cep  float64
	at         time.Duration // first seen after t0
	span       time.Duration // seen for
	skin, amb  float64       // 0 = not readable
	rng        float64
	watched    float64 // hover seconds (0 = no hover)
	moved      bool
	mframes    uint32 // moved frames when not moved (the drone's verdict on this part alone)
	task       string
}

func (p rp) rec() intake.Record {
	ll := local.ToPoint(p.e, p.n)
	first, last := t0.Add(p.at), t0.Add(p.at+p.span)
	rev := p.rev
	if rev == 0 {
		rev = 1
	}
	r := intake.Record{DetectionID: p.uav + "-" + p.track + "." + itoa(int(rev)), UAV: p.uav, TrackID: p.uav + "-" + p.track, Revision: rev,
		Part: p.part, Confidence: 0.8, Fused: 0.9, Lat: ll.Lat, Lon: ll.Lon, CEPm: float32(p.cep), Observations: 3,
		RangeM: float32(p.rng), MinRangeM: float32(p.rng), FirstSeenNs: first.UnixNano(), LastSeenNs: last.UnixNano(),
		CreatedNs: last.UnixNano(), CaptureNs: last.UnixNano(), VisibleCrop: "crops/x.jpg"}
	if p.skin != 0 {
		r.Thermal = intake.Thermal{Readable: true, SkinC: float32(p.skin), AmbientC: float32(p.amb), DeltaC: float32(p.skin - p.amb), Pixels: 50,
			Warm: p.skin >= 20 && p.skin-p.amb >= 3}
	}
	if p.watched > 0 {
		r.Movement = &intake.Movement{HoverFrames: uint32(p.watched), WatchedS: float32(p.watched), Moved: p.moved}
		if p.moved {
			r.Movement.MovedFrames = 4
		} else {
			r.Movement.MovedFrames = p.mframes
		}
		r.TaskID = p.task
	}
	return r
}

func itoa(i int) string { return strconv.Itoa(i) }

func run(t *testing.T, e *Engine, recs ...rp) []*sarpb.Casualty {
	t.Helper()
	for _, r := range recs {
		e.Add(r.rec())
	}
	e.Update("inc", "zbook/test")
	return e.List()
}

func newEngine() *Engine {
	f, tr := testDefaults()
	return New(f, tr, centre, 80, "")
}

func near(cs []*sarpb.Casualty, e, n, r float64) []*sarpb.Casualty {
	var out []*sarpb.Casualty
	for _, c := range cs {
		ce, cn := local.ToLocal(geo.Point{Lat: c.Position.LatDeg, Lon: c.Position.LonDeg})
		if math.Hypot(ce-e, cn-n) <= r {
			out = append(out, c)
		}
	}
	return out
}

// Parts one drone saw together are one person.
func TestOnePersonOneDrone(t *testing.T) {
	cs := run(t, newEngine(),
		rp{uav: "scout-1", track: "a", part: "hand", e: 0, n: 0, cep: 2.4, span: 5 * time.Second, skin: 27, amb: 6, rng: 41},
		rp{uav: "scout-1", track: "b", part: "arm", e: 0.6, n: 0.3, cep: 2.4, span: 5 * time.Second, skin: 27, amb: 6, rng: 41},
	)
	if len(cs) != 1 || len(cs[0].Parts) != 2 || cs[0].CasualtyId != "C-001" {
		t.Fatalf("%d casualties: %v", len(cs), cs)
	}
}

// Two victims 3 m apart, both seen by two drones whose errors differ: two
// casualties, each with both drones, closer to the truth than either drone.
func TestTwoVictimsTwoDrones(t *testing.T) {
	// Truth: A at (0, 0), B at (3, 0). Scout fixes +1.0 m east, investigator -1.2 m east.
	cs := run(t, newEngine(),
		rp{uav: "scout-1", track: "a", part: "arm", e: 1.0, n: 0.2, cep: 2.6, span: 6 * time.Second, skin: 27, amb: 6, rng: 41},
		rp{uav: "scout-1", track: "b", part: "hand", e: 4.0, n: -0.1, cep: 2.6, span: 6 * time.Second, skin: 26, amb: 6, rng: 41},
		rp{uav: "inv-1", track: "a", part: "arm", e: -1.2, n: 0.1, cep: 2.3, at: time.Minute, span: 30 * time.Second, skin: 27, amb: 6, rng: 15},
		rp{uav: "inv-1", track: "b", part: "hand", e: 1.8, n: 0, cep: 2.3, at: time.Minute, span: 30 * time.Second, skin: 26, amb: 6, rng: 15},
	)
	if len(cs) != 2 {
		t.Fatalf("%d casualties, want 2: %v", len(cs), cs)
	}
	for _, c := range cs {
		if len(c.UavIds) != 2 {
			t.Fatalf("%s seen by %v, want both drones", c.CasualtyId, c.UavIds)
		}
		if c.CepM >= 2.3 {
			t.Errorf("%s CEP %.2f m: should be tighter than either drone's", c.CasualtyId, c.CepM)
		}
	}
	if a := near(cs, 0, 0, 1.0); len(a) != 1 {
		t.Fatalf("victim A not within 1 m: %v", cs)
	}
}

// A person shows at most two hands: three hands 1.6 m from each other are two
// people. One leg reported three times (pieces within duplicate_m) is one.
func TestAnatomicalLimit(t *testing.T) {
	cs := run(t, newEngine(),
		rp{uav: "inv-1", track: "a", part: "hand", e: 0, n: 0, cep: 2, span: 5 * time.Second, skin: 27, amb: 6, rng: 15},
		rp{uav: "inv-1", track: "b", part: "hand", e: 1.6, n: 0, cep: 2, span: 5 * time.Second, skin: 27, amb: 6, rng: 15},
		rp{uav: "inv-1", track: "c", part: "hand", e: 0.8, n: 1.39, cep: 2, span: 5 * time.Second, skin: 27, amb: 6, rng: 15},
	)
	if len(cs) != 2 {
		t.Fatalf("%d casualties, want 2", len(cs))
	}
	cs = run(t, newEngine(),
		rp{uav: "inv-1", track: "a", part: "leg", e: 0, n: 0, cep: 2, span: 5 * time.Second, skin: 27, amb: 6, rng: 15},
		rp{uav: "inv-1", track: "b", part: "leg", e: 0.9, n: 0.2, cep: 2, span: 5 * time.Second, skin: 27, amb: 6, rng: 15},
		rp{uav: "inv-1", track: "c", part: "leg", e: 0.4, n: -0.5, cep: 2, span: 5 * time.Second, skin: 27, amb: 6, rng: 15},
		rp{uav: "inv-1", track: "d", part: "leg", e: 1.2, n: 0.6, cep: 2, span: 5 * time.Second, skin: 27, amb: 6, rng: 15},
	)
	if len(cs) != 1 {
		t.Fatalf("one leg in four pieces: %d casualties, want 1", len(cs))
	}
}

func one(t *testing.T, cs []*sarpb.Casualty) *sarpb.Casualty {
	t.Helper()
	if len(cs) != 1 {
		t.Fatalf("%d casualties, want 1: %v", len(cs), cs)
	}
	return cs[0]
}

func expect(t *testing.T, c *sarpb.Casualty, st sarpb.CasualtyStatus, tr sarpb.Triage, provisional, investigate bool, tier uint32) {
	t.Helper()
	if c.Status != st || c.Triage != tr || c.Provisional != provisional || c.NeedsInvestigation != investigate || Tier(c) != tier {
		t.Fatalf("%s: got %v %v provisional %v investigate %v tier %d (%q, %q); want %v %v %v %v %d", c.CasualtyId, c.Status, c.Triage,
			c.Provisional, c.NeedsInvestigation, Tier(c), c.Reason, c.InvestigationReason, st, tr, provisional, investigate, tier)
	}
}

// The status rules of plan section 8.
func TestTriageRules(t *testing.T) {
	warmFar := rp{uav: "scout-1", track: "a", part: "hand", cep: 2.4, span: 5 * time.Second, skin: 27, amb: 6, rng: 41}

	e := newEngine()
	c := one(t, run(t, e, warmFar))
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_UNRESPONSIVE, sarpb.Triage_TRIAGE_RED, true, true, 1)

	still := rp{uav: "inv-1", track: "a", part: "hand", e: 0.3, cep: 2.2, at: time.Minute, span: 30 * time.Second, skin: 28, amb: 6, rng: 15, watched: 30, task: "T-1"}
	c = one(t, run(t, e, still))
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_UNRESPONSIVE, sarpb.Triage_TRIAGE_RED, false, false, 0)
	if !c.SeenClose || c.Movement.WatchedS != 30 || c.Thermal.CloseReadings != 1 || c.Thermal.SkinC != 28 {
		t.Fatalf("close look not used: %+v %+v", c.Movement, c.Thermal)
	}

	e = newEngine()
	moving := still
	moving.moved = true
	c = one(t, run(t, e, warmFar, moving))
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_RESPONSIVE, sarpb.Triage_TRIAGE_YELLOW, false, false, 0)

	e = newEngine()
	coldFar := warmFar
	coldFar.skin, coldFar.amb = 9, 6
	c = one(t, run(t, e, coldFar))
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_UNKNOWN, sarpb.Triage_TRIAGE_GREY, true, true, 2)
	coldClose := still
	coldClose.skin, coldClose.amb = 9.5, 6
	c = one(t, run(t, e, coldClose))
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_NO_HEAT, sarpb.Triage_TRIAGE_BLACK, false, false, 0)

	e = newEngine()
	unreadable := warmFar
	unreadable.skin = 0
	c = one(t, run(t, e, unreadable))
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_UNKNOWN, sarpb.Triage_TRIAGE_GREY, true, true, 2)

	// Settled Red but the position is too uncertain: investigate for position (tier 3).
	e = newEngine()
	vague := still
	vague.cep = 6.5
	c = one(t, run(t, e, vague))
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_UNRESPONSIVE, sarpb.Triage_TRIAGE_RED, false, true, 3)

	// Two close looks that found nothing (after the last sighting): not found.
	e = newEngine()
	c = one(t, run(t, e, warmFar))
	for i, task := range []string{"T-1", "T-2"} {
		e.ApplyTasks([]*sarpb.Task{{TaskId: task, CasualtyId: c.CasualtyId, State: sarpb.TaskState_TASK_STATE_DONE,
			FinishedUnixNs: t0.Add(time.Duration(i+2) * time.Minute).UnixNano(), Result: &sarpb.TaskResult{Found: false, Frames: 30}}})
	}
	e.ApplyTasks([]*sarpb.Task{{TaskId: "T-3", CasualtyId: c.CasualtyId, State: sarpb.TaskState_TASK_STATE_ABORTED}})
	e.Update("inc", "zbook/test")
	c = one(t, e.List())
	expect(t, c, sarpb.CasualtyStatus_CASUALTY_STATUS_NOT_FOUND, sarpb.Triage_TRIAGE_GREY, false, false, 0)
	if c.Investigations != 2 || c.NotFoundLooks != 2 {
		t.Fatalf("looks: %d investigations, %d not found", c.Investigations, c.NotFoundLooks)
	}
}

// Clothing reads cooler than skin: a living person's trouser leg at 17 C on a
// 7 C morning is warm; a bare hand at 17 C is not (below skin_min_c); a dead
// person's leg at ambient is cold, and so is sun-warmed clothing on a dead
// person (14 C, 7 C above the air). Bare skin decides when it is read: a warm
// hand with a cool leg is warm, a cold hand with sun-warmed clothing is cold.
func TestClothedWarm(t *testing.T) {
	leg := rp{uav: "inv-1", track: "a", part: "leg", cep: 2, span: 30 * time.Second, skin: 17, amb: 7, rng: 15, watched: 30, task: "T-1"}
	c := one(t, run(t, newEngine(), leg))
	if !c.Thermal.Warm || c.Triage != sarpb.Triage_TRIAGE_RED {
		t.Fatalf("clothed leg at 17 C: warm %v triage %v", c.Thermal.Warm, c.Triage)
	}
	hand := leg
	hand.part = "hand"
	if c := one(t, run(t, newEngine(), hand)); c.Thermal.Warm {
		t.Fatalf("bare hand at 17 C counted warm")
	}
	dead := leg
	dead.skin = 7.5
	if c := one(t, run(t, newEngine(), dead)); c.Thermal.Warm || c.Triage != sarpb.Triage_TRIAGE_BLACK {
		t.Fatalf("leg at ambient: warm %v triage %v", c.Thermal.Warm, c.Triage)
	}
	warmHand := hand
	warmHand.track, warmHand.skin, warmHand.e = "b", 23, 0.5
	if c := one(t, run(t, newEngine(), leg, warmHand)); !c.Thermal.Warm || c.Thermal.CloseReadings != 2 {
		t.Fatalf("warm hand and cool leg: %+v", c.Thermal)
	}
	sunlit := leg
	sunlit.skin = 14
	if c := one(t, run(t, newEngine(), sunlit)); c.Thermal.Warm {
		t.Fatalf("sun-warmed clothing (14 C, +7 C) counted warm")
	}
	coldHand := hand
	coldHand.track, coldHand.skin, coldHand.e = "b", 7.2, 0.5
	if c := one(t, run(t, newEngine(), leg, coldHand)); c.Thermal.Warm {
		t.Fatalf("cold hand and warm-looking leg: %+v", c.Thermal)
	}
}

// Ids stay stable as reports arrive; when two casualties prove to be one, one
// keeps its id and the other is published once more with merged_into.
func TestIDsAndMerge(t *testing.T) {
	e := newEngine()
	run(t, e, rp{uav: "scout-1", track: "a", part: "hand", e: 0, n: 0, cep: 2.4, span: 5 * time.Second, skin: 27, amb: 6, rng: 41})
	cs := run(t, e, rp{uav: "inv-1", track: "a", part: "arm", e: 12, n: 0, cep: 2.2, at: time.Minute, span: 20 * time.Second, skin: 27, amb: 6, rng: 15})
	if len(cs) != 2 || cs[0].CasualtyId != "C-001" || cs[1].CasualtyId != "C-002" {
		t.Fatalf("before the merge: %v", cs)
	}
	// The investigator's track settles next to the scout's: the same person.
	e.Add(rp{uav: "inv-1", track: "a", rev: 2, part: "arm", e: 1, n: 0, cep: 2.0, at: time.Minute, span: 30 * time.Second, skin: 27, amb: 6, rng: 15}.rec())
	changed, removed := e.Update("inc", "zbook/test")
	cs = e.List()
	if len(cs) != 1 || cs[0].CasualtyId != "C-001" || len(removed) != 1 || removed[0].CasualtyId != "C-002" || removed[0].MergedInto != "C-001" {
		t.Fatalf("after the merge: list %v, removed %v", cs, removed)
	}
	if len(changed) != 1 || changed[0].Revision != 2 || changed[0].Header.GetIncidentId() != "inc" {
		t.Fatalf("changed %v", changed)
	}
	// Nothing new: nothing published.
	if c, r := e.Update("inc", "zbook/test"); len(c)+len(r) != 0 {
		t.Fatalf("idle update published %d", len(c)+len(r))
	}
}

// The state file keeps ids, revisions and looks across a restart.
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	f, tr := testDefaults()
	recs := []rp{
		{uav: "scout-1", track: "a", part: "hand", e: 0, n: 0, cep: 2.4, span: 5 * time.Second, skin: 27, amb: 6, rng: 41},
		{uav: "scout-1", track: "b", part: "hand", e: 30, n: 0, cep: 2.4, at: 10 * time.Second, span: 5 * time.Second, skin: 9, amb: 6, rng: 41},
	}
	e := New(f, tr, centre, 80, dir)
	var stored []intake.Record
	for _, r := range recs {
		stored = append(stored, r.rec())
		e.Add(r.rec())
	}
	e.Update("inc", "x")
	e.ApplyTasks([]*sarpb.Task{{TaskId: "T-9", CasualtyId: "C-002", State: sarpb.TaskState_TASK_STATE_DONE, FinishedUnixNs: t0.Add(time.Hour).UnixNano(),
		Result: &sarpb.TaskResult{Found: false}}})
	e.Update("inc", "x")
	before := e.List()

	e2 := New(f, tr, centre, 80, dir)
	if err := e2.Load(stored); err != nil {
		t.Fatal(err)
	}
	e2.Update("inc", "x")
	after := e2.List()
	if len(after) != 2 || after[0].CasualtyId != before[0].CasualtyId || after[1].CasualtyId != before[1].CasualtyId ||
		after[1].Investigations != 1 || after[1].Revision <= before[1].Revision {
		t.Fatalf("before %v\nafter %v", before, after)
	}
}

// Skin cooling over 20 minutes faster than the configured rate: deteriorating.
func TestCoolingTrend(t *testing.T) {
	e := newEngine()
	for i := 0; i <= 4; i++ {
		r := rp{uav: "inv-1", track: "a", rev: uint32(i + 1), part: "hand", cep: 2, span: time.Duration(i) * 5 * time.Minute,
			skin: 27 - 0.25*float64(i), amb: 6, rng: 15, watched: 30, task: "T-1"}
		e.Add(r.rec())
	}
	e.Update("inc", "x")
	c := one(t, e.List())
	if c.Status != sarpb.CasualtyStatus_CASUALTY_STATUS_DETERIORATING || c.Triage != sarpb.Triage_TRIAGE_RED || c.Thermal.TrendCPerH > -0.9 {
		t.Fatalf("status %v trend %.2f C/h span %.0f s", c.Status, c.Thermal.TrendCPerH, c.Thermal.TrendSpanS)
	}
}

// The assignment solver against brute force on small random problems.
func TestAssignBruteForce(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for trial := 0; trial < 300; trial++ {
		n, m := 1+r.Intn(4), 1+r.Intn(4)
		cost := make([][]float64, n)
		for i := range cost {
			cost[i] = make([]float64, m)
			for j := range cost[i] {
				cost[i][j] = r.Float64() * 1.5
				if r.Float64() < 0.25 || cost[i][j] > 1.2 {
					cost[i][j] = math.Inf(1)
				}
			}
		}
		total := func(match []int) float64 {
			used := map[int]bool{}
			s := float64(n)
			for j, i := range match {
				if i < 0 {
					s++
					continue
				}
				if used[i] || math.IsInf(cost[i][j], 1) {
					return math.Inf(1)
				}
				used[i] = true
				s += cost[i][j] - 1
			}
			return s
		}
		best := math.Inf(1)
		var rec func(j int, cur []int)
		rec = func(j int, cur []int) {
			if j == m {
				best = math.Min(best, total(cur))
				return
			}
			for i := -1; i < n; i++ {
				rec(j+1, append(cur, i))
			}
		}
		rec(0, nil)
		ones := func(k int) []float64 {
			v := make([]float64, k)
			for i := range v {
				v[i] = 1
			}
			return v
		}
		res, tot := assign(cost, ones(n), ones(m))
		got := total(res)
		if math.Abs(got-tot) > 1e-9 {
			t.Fatalf("trial %d: reported total %v, actual %v", trial, tot, got)
		}
		if math.Abs(got-best) > 1e-9 {
			t.Fatalf("trial %d: assignment cost %v, optimum %v (%v)", trial, got, best, cost)
		}
	}
}

// Fusion quality on random sites: 3-6 victims at least minSep apart in a
// 14 m square; every drone window has its own position offset (GPS 1.5 m per
// axis). Score: casualties holding more than one person (merges) and people
// spread over several casualties (extra casualties from splits).
func TestFusionQuality(t *testing.T) {
	for _, tc := range []struct {
		name               string
		minSep             float64
		hovers, alike      bool
		maxMerge, maxSplit float64
	}{
		// Worst case: every victim shows the same warm hand and arm, so only
		// positions tell them apart.
		{"all alike, whole-site windows, 3.2 m", 3.2, false, true, 0.03, 0.04},
		{"all alike, scout pass + hovers, 3.2 m", 3.2, true, true, 0.07, 0.07},
		// As in the scenario: different exposed parts, some warm, some cold.
		{"varied, whole-site windows, 2.5 m", 2.5, false, false, 0.02, 0.03},
		{"varied, scout pass + hovers, 3.2 m", 3.2, true, false, 0.04, 0.05},
	} {
		people, merged, split := fusionTrials(tc.minSep, tc.hovers, tc.alike)
		t.Logf("%s: %d people, %d casualties holding more than one person, %d extra casualties from splits", tc.name, people, merged, split)
		if float64(merged) > tc.maxMerge*float64(people) || float64(split) > tc.maxSplit*float64(people) {
			t.Errorf("%s: %d merges and %d splits for %d people (limits %.0f%%, %.0f%%)", tc.name, merged, split, people, 100*tc.maxMerge, 100*tc.maxSplit)
		}
	}
}

// Exposures as in the scenario generator: the classes a victim shows.
var exposures = [][]string{{"hand", "arm"}, {"head"}, {"head", "torso"}, {"leg"}, {"torso"}, {"hand"}}

// offsetSigma is the GPS error of a drone window, per axis.
const offsetSigma = 1.5

func fusionTrials(minSep float64, hovers, alike bool) (people, merged, split int) {
	r := rand.New(rand.NewSource(7))
	for trial := 0; trial < 200; trial++ {
		nv := 3 + r.Intn(4)
		var vs [][2]float64
		for len(vs) < nv {
			p := [2]float64{r.Float64() * 14, r.Float64() * 14}
			ok := true
			for _, q := range vs {
				if math.Hypot(p[0]-q[0], p[1]-q[1]) < minSep {
					ok = false
				}
			}
			if ok {
				vs = append(vs, p)
			}
		}
		look := make([][]string, len(vs))
		skin := make([]float64, len(vs))
		for i := range vs {
			look[i], skin[i] = exposures[0], 27
			if !alike {
				look[i] = exposures[r.Intn(len(exposures))]
				if r.Float64() < 0.4 {
					skin[i] = 9 // deceased: near ambient
				}
			}
		}
		e := newEngine()
		victimOf := map[string]int{} // uav/track -> victim
		n := 0
		see := func(uav string, vi int, oe, on float64, at time.Duration, task string) {
			parts := look[vi]
			if len(parts) > 1 && r.Float64() < 0.4 {
				parts = parts[:1] // the other part hidden this time
			}
			for k, part := range parts {
				if r.Float64() < 0.08 { // the model names it wrongly
					part = []string{"hand", "arm", "leg", "head", "torso"}[r.Intn(5)]
				}
				n++
				tr := "t" + itoa(n)
				victimOf[uav+"/"+uav+"-"+tr] = vi
				x := rp{uav: uav, track: tr, part: part, e: vs[vi][0] + oe + r.NormFloat64()*0.3 + 0.4*float64(k),
					n: vs[vi][1] + on + r.NormFloat64()*0.3, cep: 2.4, at: at, span: 5 * time.Second, skin: skin[vi], amb: 6, rng: 15}
				if task != "" {
					x.watched, x.task, x.span = 30, task, 30*time.Second
				}
				e.Add(x.rec())
			}
		}
		if !hovers {
			for di, uav := range []string{"scout-1", "inv-1", "inv-2"} {
				oe, on := r.NormFloat64()*offsetSigma, r.NormFloat64()*offsetSigma
				for vi := range vs {
					if r.Float64() < 0.85 {
						see(uav, vi, oe, on, time.Duration(di)*time.Minute, "")
					}
				}
			}
		} else {
			oe, on := r.NormFloat64()*offsetSigma, r.NormFloat64()*offsetSigma
			for vi := range vs {
				if r.Float64() < 0.85 {
					see("scout-1", vi, oe, on, 0, "")
				}
			}
			e.Update("inc", "x")
			for k, c := range e.List() {
				uav := []string{"inv-1", "inv-2"}[k%2]
				task := "T-" + itoa(k+1)
				ce, cn := local.ToLocal(geo.Point{Lat: c.Position.LatDeg, Lon: c.Position.LonDeg})
				e.ApplyTasks([]*sarpb.Task{{TaskId: task, CasualtyId: c.CasualtyId, State: sarpb.TaskState_TASK_STATE_ON_STATION,
					Target: c.Position}})
				he, hn := r.NormFloat64()*offsetSigma, r.NormFloat64()*offsetSigma
				at := time.Minute + time.Duration(k)*25*time.Second // back to back
				for vi, v := range vs {
					d := math.Hypot(v[0]+0-ce, v[1]-cn)
					if (d < 3 && r.Float64() < 0.95) || (d < 6 && r.Float64() < 0.85) {
						see(uav, vi, he, hn, at, task)
					}
				}
				e.Update("inc", "x")
			}
		}
		e.Update("inc", "x")
		casOf := map[int]map[string]bool{}
		for _, c := range e.List() {
			vids := map[int]bool{}
			for _, p := range c.Parts {
				vi := victimOf[p.UavId+"/"+p.TrackId]
				vids[vi] = true
				if casOf[vi] == nil {
					casOf[vi] = map[string]bool{}
				}
				casOf[vi][c.CasualtyId] = true
			}
			if len(vids) > 1 {
				merged++
			}
		}
		for _, cs := range casOf {
			people++
			split += len(cs) - 1
		}
	}
	return people, merged, split
}

// A long incident: about 3000 parts from three drones over 40 sites (many
// windows, crowded neighbourhoods) are merged within a few seconds.
func TestUpdateScales(t *testing.T) {
	r := rand.New(rand.NewSource(5))
	e := newEngine()
	n := 0
	for site := 0; site < 40; site++ {
		ce, cn := r.Float64()*600-300, r.Float64()*600-300
		for pass := 0; pass < 6; pass++ {
			uav := []string{"scout-1", "inv-1", "inv-2"}[pass%3]
			oe, on := r.NormFloat64()*1.5, r.NormFloat64()*1.5
			at := time.Duration(site*400+pass*60) * time.Second
			for k := 0; k < 12; k++ {
				n++
				e.Add(rp{uav: uav, track: "t" + itoa(n), part: []string{"hand", "arm", "leg", "head", "torso"}[r.Intn(5)],
					e: ce + r.Float64()*20 + oe, n: cn + r.Float64()*20 + on, cep: 2, at: at, span: 5 * time.Second, skin: 9 + 18*r.Float64(), amb: 6, rng: 40}.rec())
			}
		}
	}
	t0 := time.Now()
	e.Update("inc", "x")
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("%d parts merged in %v", n, d)
	} else {
		t.Logf("%d parts, %d casualties, %v", n, len(e.List()), d)
	}
}

func taskAt(id, cas string, e, n float64) *sarpb.Task {
	ll := local.ToPoint(e, n)
	return &sarpb.Task{TaskId: id, CasualtyId: cas, State: sarpb.TaskState_TASK_STATE_ON_STATION, Target: &sarpb.GeoPoint{LatDeg: ll.Lat, LonDeg: ll.Lon}}
}

// Only hovers aimed at a casualty decide whether it moved: a neighbour 6 m
// away caught (moving or not) during someone else's investigation is neither
// watched nor seen moving; its own investigation decides.
func TestMovementOnlyWhenAimed(t *testing.T) {
	e := newEngine()
	cs := run(t, e,
		rp{uav: "scout-1", track: "a", part: "hand", e: 0, n: 0, cep: 2.4, span: 5 * time.Second, skin: 27, amb: 6, rng: 41},
		rp{uav: "scout-1", track: "b", part: "hand", e: 6, n: 0, cep: 2.4, span: 5 * time.Second, skin: 27, amb: 6, rng: 41})
	if len(cs) != 2 {
		t.Fatalf("%d casualties, want 2", len(cs))
	}
	e.ApplyTasks([]*sarpb.Task{taskAt("T-1", "C-001", 0, 0)})
	cs = run(t, e,
		rp{uav: "inv-1", track: "a", part: "hand", e: 0.2, cep: 2, at: time.Minute, span: 30 * time.Second, skin: 28, amb: 6, rng: 15, watched: 30, task: "T-1"},
		rp{uav: "inv-1", track: "b", part: "hand", e: 6.2, cep: 2, at: time.Minute, span: 30 * time.Second, skin: 28, amb: 6, rng: 15, watched: 30, moved: true, task: "T-1"})
	a, b := one(t, near(cs, 0, 0, 1.5)), one(t, near(cs, 6, 0, 1.5))
	expect(t, a, sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_UNRESPONSIVE, sarpb.Triage_TRIAGE_RED, false, false, 0)
	if b.Movement.Moved || b.Movement.WatchedS != 0 {
		t.Fatalf("neighbour's movement counted from another casualty's hover: %+v", b.Movement)
	}
	expect(t, b, sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_UNRESPONSIVE, sarpb.Triage_TRIAGE_RED, true, true, 1)

	e.ApplyTasks([]*sarpb.Task{taskAt("T-2", b.CasualtyId, 6, 0)})
	cs = run(t, e, rp{uav: "inv-2", track: "c", part: "hand", e: 5.9, cep: 2, at: 3 * time.Minute, span: 30 * time.Second, skin: 28, amb: 6, rng: 15, watched: 30, moved: true, task: "T-2"})
	b = one(t, near(cs, 6, 0, 1.5))
	expect(t, b, sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_RESPONSIVE, sarpb.Triage_TRIAGE_YELLOW, false, false, 0)
	if a := one(t, near(cs, 0, 0, 1.5)); a.Movement.Moved {
		t.Fatalf("A seen moving from B's hover")
	}
}

// The skin trend compares like with like, one look at a time: a hand cooling
// 3 C/h over six looks, with the scout's cooler far readings, a sleeve at
// 16 C and one look spoilt by a hot reflection, still reads about -3 C/h; a
// sleeve early and a hand late (two kinds of part, two looks each) give no
// trend at all.
func TestTrendRobust(t *testing.T) {
	e := newEngine()
	recs := []rp{{uav: "scout-1", track: "s", part: "hand", cep: 2.4, span: 10 * time.Second, skin: 21, amb: 6, rng: 41}}
	for k, m := range []float64{2, 6, 10, 14, 18, 22} {
		at := time.Duration(m * float64(time.Minute))
		skin := 27 - 3*m/60
		if k == 4 {
			skin = 34 // a reflection on this look
		}
		task := "T-" + itoa(k+1)
		for j, d := range []float64{-0.3, 0.2, 0} {
			recs = append(recs, rp{uav: "inv-1", track: "h" + itoa(k), rev: uint32(j + 1), part: "hand", e: 0.2, cep: 2, at: at,
				span: time.Duration(10*(j+1)) * time.Second, skin: skin + d, amb: 6, rng: 15, watched: 30, task: task})
		}
		recs = append(recs, rp{uav: "inv-1", track: "s" + itoa(k), part: "arm", e: 0.6, cep: 2, at: at, span: 30 * time.Second,
			skin: 16, amb: 6, rng: 15, watched: 30, task: task})
	}
	c := one(t, run(t, e, recs...))
	if th := c.Thermal; math.Abs(float64(th.TrendCPerH)+3) > 1 || th.TrendSpanS < 1100 {
		t.Fatalf("trend %.2f C/h over %.0f s, want about -3 C/h over 20 min", th.TrendCPerH, th.TrendSpanS)
	}

	e = newEngine()
	c = one(t, run(t, e,
		rp{uav: "inv-1", track: "a", part: "arm", cep: 2, span: 30 * time.Second, skin: 15, amb: 6, rng: 15, watched: 30, task: "T-1"},
		rp{uav: "inv-1", track: "b", part: "arm", cep: 2, at: 5 * time.Minute, span: 30 * time.Second, skin: 15.5, amb: 6, rng: 15, watched: 30, task: "T-2"},
		rp{uav: "inv-2", track: "c", part: "hand", e: 0.3, cep: 2, at: 15 * time.Minute, span: 30 * time.Second, skin: 27, amb: 6, rng: 15, watched: 30, task: "T-3"},
		rp{uav: "inv-2", track: "d", part: "hand", e: 0.3, cep: 2, at: 20 * time.Minute, span: 30 * time.Second, skin: 27, amb: 6, rng: 15, watched: 30, task: "T-4"}))
	if th := c.Thermal; th.TrendCPerH != 0 || th.TrendSpanS != 0 {
		t.Fatalf("sleeve then hand gave a trend: %.2f C/h over %.0f s", th.TrendCPerH, th.TrendSpanS)
	}
	// Three looks at "an arm" 26, 16 and 15 C over 20 minutes (two people's
	// arms in one record): -33 C/h is no trend.
	e = newEngine()
	c = one(t, run(t, e,
		rp{uav: "inv-1", track: "a", part: "arm", cep: 2, span: 30 * time.Second, skin: 26, amb: 6, rng: 15, watched: 30, task: "T-1"},
		rp{uav: "inv-1", track: "b", part: "arm", cep: 2, at: 10 * time.Minute, span: 30 * time.Second, skin: 16, amb: 6, rng: 15, watched: 30, task: "T-2"},
		rp{uav: "inv-2", track: "c", part: "arm", cep: 2, at: 20 * time.Minute, span: 30 * time.Second, skin: 15, amb: 6, rng: 15, watched: 30, task: "T-3"}))
	if th := c.Thermal; th.TrendCPerH != 0 {
		t.Fatalf("implausible trend reported: %.2f C/h", th.TrendCPerH)
	}
}

// A limb that moves for a second shows in one frame of the hand and the same
// frame of the arm: neither part alone is "moved" for the drone, together
// they are (moved_frames 2). One frame of one part is not.
func TestMovedFramesAcrossParts(t *testing.T) {
	hand := rp{uav: "inv-1", track: "a", part: "hand", cep: 2, span: 30 * time.Second, skin: 28, amb: 6, rng: 15, watched: 30, mframes: 1, task: "T-1"}
	arm := hand
	arm.track, arm.part, arm.e = "b", "arm", 0.5
	c := one(t, run(t, newEngine(), hand, arm))
	if !c.Movement.Moved || c.Movement.MovedFrames != 2 || c.Triage != sarpb.Triage_TRIAGE_YELLOW {
		t.Fatalf("hand and arm one frame each: %+v %v", c.Movement, c.Triage)
	}
	arm.mframes = 0
	c = one(t, run(t, newEngine(), hand, arm))
	if c.Movement.Moved || c.Triage != sarpb.Triage_TRIAGE_RED {
		t.Fatalf("one frame of one part: %+v %v", c.Movement, c.Triage)
	}
	// One frame in each of two hovers is not two frames of one hover.
	arm.mframes, arm.task, arm.at = 1, "T-2", 2*time.Minute
	c = one(t, run(t, newEngine(), hand, arm))
	if c.Movement.Moved {
		t.Fatalf("one frame in each of two hovers counted as movement: %+v", c.Movement)
	}
}

// In a hover, a part the model reported in only a few frames (2 of 30) is no
// person of its own: next to a part followed throughout it joins it; alone,
// it starts no casualty. A survey pass (every part seen in a few frames) is
// not affected.
func TestFlickers(t *testing.T) {
	hover := func(track, part string, e, n float64, obs uint32) intake.Record {
		r := rp{uav: "inv-1", track: track, part: part, e: e, n: n, cep: 2, at: time.Minute, span: 30 * time.Second, skin: 28, amb: 6, rng: 15, watched: 30, task: "T-1"}.rec()
		r.Observations = obs
		return r
	}
	e := newEngine()
	for _, r := range []intake.Record{
		hover("a", "head", 0, 0, 30), hover("b", "torso", 0.5, 0, 30),
		hover("c", "head", 2.6, 0.5, 2), // a second "head" 2.6 m away, 2 frames: not a second person
		hover("d", "leg", 12, 0, 3),     // a "leg" on the debris 12 m away, 3 frames: nobody
	} {
		e.Add(r)
	}
	e.Update("inc", "x")
	if cs := e.List(); len(cs) != 1 || len(cs[0].Parts) != 3 {
		t.Fatalf("%d casualties (%v), want 1 with the flicker", len(cs), cs)
	}
	// The scout's pass: every part in 2-3 frames; a lone one is a casualty.
	e = newEngine()
	cs := run(t, e, rp{uav: "scout-1", track: "a", part: "leg", e: 12, cep: 2.4, span: 3 * time.Second, skin: 9, amb: 6, rng: 41})
	if len(cs) != 1 {
		t.Fatalf("scout's sighting: %d casualties", len(cs))
	}
}
