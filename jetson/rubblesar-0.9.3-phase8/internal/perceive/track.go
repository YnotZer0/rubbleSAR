package perceive

import (
	"fmt"
	"math"
	"sort"
	"time"

	"rubblesar/internal/masks"
)

// WarmClass is the class index of a thermal-only warm spot (body classes are 0..4).
const WarmClass = 5

// Obs is one sighting in one frame: a body part found by the model, or a warm spot.
type Obs struct {
	Class   int
	Conf    float32 // model confidence (0 for warm spots)
	Fix     Fix
	AreaM2  float64
	Thermal Reading
	Time    time.Time
	// Evidence builds the crops and outline; called only when this sighting
	// becomes the best one of its track (while the frame is still in memory).
	Evidence func() *Evidence
	// Phase 4: taken while holding over a target (TaskID); Mask is the part's
	// outline moved into the hover's reference frame.
	Hover  bool
	TaskID string
	Mask   *masks.Bitmap
	photo  *photoProbe // the picture around the part, to check a changed outline against
}

// Evidence is what a packet shows of a sighting.
type Evidence struct {
	FrameID     string
	CaptureNS   int64
	SimTimeS    float64
	Pose        PoseReport
	RangeM      float64
	VisibleJPEG []byte
	VisW, VisH  int
	VisBox      [4]float32 // source frame region
	Outline     []float32  // relative to the visible crop, 0..1
	ThermalPNG  []byte
	ThW, ThH    int
	ThBox       [4]float32
}

// PoseReport is the reported capture pose in geographic terms.
type PoseReport struct {
	Lat, Lon, AltMSL, AltAGL float64
	YawDeg, PitchDeg         float64
}

// TrackParams tune the tracker.
type TrackParams struct {
	ConfirmHits       int
	UpdateMinInterval time.Duration
	CorrTau           time.Duration // correlation time of the drifting pose errors
	// DetSigmaM: position error of a detection that the pose errors do not
	// explain (1 sigma per axis), added to the reported CEP (phase 4).
	DetSigmaM float64
	Motion    MotionParams
}

// DefaultTrackParams match the edge-eye defaults.
func DefaultTrackParams() TrackParams {
	return TrackParams{ConfirmHits: 2, UpdateMinInterval: 5 * time.Second, CorrTau: 30 * time.Second, DetSigmaM: 1.0, Motion: DefaultMotionParams()}
}

// Track is one static object (a body part or a warm spot) on the ground.
type Track struct {
	ID        string
	E, N      float64
	varIndep  float64 // posterior variance of the frame-to-frame part
	varCorr   float64 // drifting part of a single fix (reduced by sightings far apart in time)
	Votes     [6]float64
	Hits      int
	FirstSeen time.Time
	LastSeen  time.Time
	BestConf  float32
	BestDelta float64 // for warm spots
	AreaM2    float64
	Thermal   Reading // best readable reading (most pixels), else the latest
	Best      *Evidence
	bestScore float64 // how good the sighting behind Best is
	Revision  int
	last      *snapshot
	// Phase 4: movement during hovers, closest camera distance, last task.
	Motion   *Motion
	MinRange float64
	TaskID   string
	refTask  string // the hover the motion reference outline belongs to
	// meanAt: the time the position estimate mostly comes from (weighted like
	// the estimate), for how far the drifting GPS error may have moved since.
	meanAt time.Time
}

type snapshot struct {
	e, n, fused, skin, cep float64
	class                  int
	readable, moved        bool
	at                     time.Time
}

// Class returns the track's class: the strongest body-part vote, or WarmClass
// if it was only ever a warm spot.
func (t *Track) Class() int {
	best, bv := WarmClass, 0.0
	for c := 0; c < 5; c++ {
		if t.Votes[c] > bv {
			best, bv = c, t.Votes[c]
		}
	}
	return best
}

// Var is the per-axis variance of the position estimate. The drifting part
// (GPS, altitude, heading) is a Gauss–Markov process with time constant tau:
// averaging it over a span S keeps a share MeanVarFactor(S/tau) of its
// variance, never less than 1/hits.
func (t *Track) Var(tau time.Duration) float64 {
	f := MeanVarFactor(t.LastSeen.Sub(t.FirstSeen).Seconds() / tau.Seconds())
	f = math.Max(f, 1/math.Max(float64(t.Hits), 1))
	return t.varIndep + t.varCorr*f
}

// MeanVarFactor is the variance of the time average of an exponentially
// correlated process over x time constants, relative to its own variance:
// 2/x·(1−(1−e^−x)/x). It is 1 for x→0, 0.74 at x=1, 0.57 at x=2 and about 2/x
// for long spans.
func MeanVarFactor(x float64) float64 {
	if x < 1e-3 {
		return 1 - x/3
	}
	return 2 / x * (1 - (1-math.Exp(-x))/x)
}

// Fused is the confidence including repeated sightings and the thermal cue.
func (t *Track) Fused() float64 {
	var base float64
	if t.Class() == WarmClass {
		base = math.Min(0.4, math.Max(0.15, 0.15+0.02*t.BestDelta))
	} else {
		base = float64(t.BestConf)
	}
	f := 1 - (1-base)*math.Pow(0.85, float64(max(t.Hits-1, 0)))
	if t.Class() != WarmClass && t.Thermal.Readable && t.Thermal.Warm {
		f = 1 - (1-f)*0.5
	}
	if t.Class() == WarmClass {
		f = math.Min(f, 0.6)
	}
	return math.Round(f*1000) / 1000
}

// Tracker associates sightings of one drone into tracks. Objects in rubble do
// not move (apart from small limb movements), so a track is a position
// estimate refined with every sighting; association is by distance within
// the combined uncertainty, preferring the same class.
type Tracker struct {
	uav    string
	prefix string // track ids are <prefix>-<n>
	p      TrackParams
	tracks []*Track
	next   int
	// segStart: sightings from then on are not added to tracks last seen before.
	segStart time.Time
}

// NewSegment starts a new stretch of observation (a hover begins or ends):
// sightings from now on are not added to tracks last seen before it, so a part
// seen again from another place (the next hover) gets a track of its own.
// Positions from two viewpoints disagree by the unknown height of the rubble
// (a part on a 4 m pile, 6 m off nadir from 15 m, lands about 1 m out, the
// other way when seen from the other side); averaged into one track they make
// the part's position depend on where it happened to be seen from.
// casualty-analyst matches the hovers' records instead.
func (tr *Tracker) NewSegment(at time.Time) { tr.segStart = at }

// NewTracker makes a tracker for one drone; its track ids are <uav>-0001, …
func NewTracker(uav string, p TrackParams) *Tracker { return &Tracker{uav: uav, prefix: uav, p: p} }

// SetIDPrefix sets what track ids start with. edge-eye uses <uav>-<run token>
// so that ids stay unique when the program restarts and numbering starts again.
func (tr *Tracker) SetIDPrefix(p string) { tr.prefix = p }

// Reset forgets all tracks (new ids from now on).
func (tr *Tracker) Reset() { tr.tracks = nil }

// Tracks returns the current tracks.
func (tr *Tracker) Tracks() []*Track { return tr.tracks }

// Report is a track to be sent: a new confirmed track or a material change.
type Report struct {
	Track  *Track
	Reason int // 1 new, 2 update, 3 investigated (sarpb.ReportReason)
}

const mismatchGateM = 0.8

// Update associates one frame's sightings and returns the tracks to report.
func (tr *Tracker) Update(obs []Obs, now time.Time) []Report {
	type pair struct {
		o, t int
		cost float64
	}
	var pairs []pair
	for oi, o := range obs {
		for ti, t := range tr.tracks {
			if t.LastSeen.Before(tr.segStart) {
				continue // an earlier segment's track
			}
			d2 := (o.Fix.E-t.E)*(o.Fix.E-t.E) + (o.Fix.N-t.N)*(o.Fix.N-t.N)
			// The estimate mostly carries the drifting error of the sightings it
			// averages (around meanAt); this one carries today's.
			rho := math.Exp(-math.Abs(o.Time.Sub(t.meanAt).Seconds()) / tr.p.CorrTau.Seconds())
			v := t.Var(tr.p.CorrTau) + o.Fix.VarIndep + 2*o.Fix.VarCorr*(1-rho)
			gate2 := 9.21 * math.Max(v, 0.25)
			same := o.Class == t.Class()
			if !same && d2 > mismatchGateM*mismatchGateM {
				continue
			}
			if d2 > gate2 {
				continue
			}
			c := d2 / gate2
			if !same {
				c += 0.5
			}
			pairs = append(pairs, pair{oi, ti, c})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].cost < pairs[j].cost })
	usedO, usedT := map[int]bool{}, map[int]bool{}
	assigned := make([]int, len(obs))
	for i := range assigned {
		assigned[i] = -1
	}
	for _, p := range pairs {
		if usedO[p.o] || usedT[p.t] {
			continue
		}
		usedO[p.o], usedT[p.t] = true, true
		assigned[p.o] = p.t
	}
	touched := map[*Track]bool{}
	for oi, o := range obs {
		var t *Track
		if assigned[oi] >= 0 {
			t = tr.tracks[assigned[oi]]
			tr.add(t, o, false)
		} else {
			tr.next++
			t = &Track{ID: fmt.Sprintf("%s-%04d", tr.prefix, tr.next), E: o.Fix.E, N: o.Fix.N, varIndep: o.Fix.VarIndep, varCorr: o.Fix.VarCorr, FirstSeen: o.Time}
			tr.tracks = append(tr.tracks, t)
			tr.add(t, o, true)
		}
		touched[t] = true
	}
	var out []Report
	for _, t := range tr.tracks {
		if !touched[t] || t.Hits < tr.p.ConfirmHits || t.Best == nil {
			continue
		}
		if t.last == nil {
			out = append(out, Report{Track: t, Reason: 1})
			continue
		}
		if now.Sub(t.last.at) >= tr.p.UpdateMinInterval && tr.changed(t) {
			out = append(out, Report{Track: t, Reason: 2})
		}
	}
	return out
}

func (tr *Tracker) add(t *Track, o Obs, first bool) {
	if !first {
		// Static Kalman update of the frame-to-frame part; the drifting part is
		// shared by nearby frames and handled in Var().
		k := t.varIndep / (t.varIndep + o.Fix.VarIndep)
		t.E += k * (o.Fix.E - t.E)
		t.N += k * (o.Fix.N - t.N)
		t.varIndep *= 1 - k
		t.varCorr = math.Min(t.varCorr, o.Fix.VarCorr)
		t.meanAt = t.meanAt.Add(time.Duration(k * float64(o.Time.Sub(t.meanAt))))
	} else {
		t.meanAt = o.Time
	}
	t.Hits++
	t.LastSeen = o.Time
	if t.MinRange == 0 || (o.Fix.RangeM > 0 && o.Fix.RangeM < t.MinRange) {
		t.MinRange = o.Fix.RangeM
	}
	if o.Hover && o.TaskID != "" {
		// Every part seen during a hover belongs to it (casualty-analyst
		// registers a hover's records together: one GPS error), not only
		// those in the middle of the frame whose movement is watched.
		t.TaskID = o.TaskID
	}
	if o.Hover && o.Mask != nil && o.Class != WarmClass {
		if t.Motion == nil || t.refTask != o.TaskID {
			// A new hover: its own reference outline and its own verdict
			// (each hover's INVESTIGATED report says what that hover saw).
			t.Motion, t.refTask = &Motion{}, o.TaskID
		}
		t.Motion.observe(o.Mask, o.photo, o.Time, tr.p.Motion)
		t.TaskID = o.TaskID
	}
	if o.Class == WarmClass {
		t.Votes[WarmClass] += 0.3
	} else {
		t.Votes[o.Class] += float64(o.Conf)
	}
	t.AreaM2 = math.Max(t.AreaM2, o.AreaM2)
	if o.Thermal.Readable {
		if !t.Thermal.Readable || o.Thermal.Pixels >= t.Thermal.Pixels {
			t.Thermal = o.Thermal
		}
	} else if !t.Thermal.Readable {
		t.Thermal = o.Thermal
	}
	if o.Class != WarmClass && o.Conf > t.BestConf {
		t.BestConf = o.Conf
	}
	if o.Class == WarmClass {
		t.BestDelta = math.Max(t.BestDelta, o.Thermal.DeltaC)
	}
	// The crops come from the most useful sighting: a confident one, ideally
	// with a temperature reading (inside the thermal camera's narrower view).
	sc := evidenceScore(o)
	if (t.Best == nil || sc > t.bestScore+0.01) && o.Evidence != nil {
		if ev := o.Evidence(); ev != nil {
			t.Best, t.bestScore = ev, sc
		}
	}
}

func evidenceScore(o Obs) float64 {
	s := float64(o.Conf)
	if o.Class == WarmClass {
		s = 0.3 + 0.01*o.Thermal.DeltaC
	}
	if o.Thermal.Readable {
		s += 0.15
	} else if o.Thermal.Reason != "outside thermal view" && o.Thermal.Reason != "no thermal camera" {
		s += 0.05
	}
	return s
}

func (tr *Tracker) snap(t *Track) *snapshot {
	return &snapshot{e: t.E, n: t.N, fused: t.Fused(), skin: t.Thermal.SkinC, cep: tr.CEP(t), class: t.Class(), readable: t.Thermal.Readable,
		moved: t.Motion.Moved(tr.p.Motion)}
}

func (tr *Tracker) changed(t *Track) bool {
	s, l := tr.snap(t), t.last
	moved := math.Hypot(s.e-l.e, s.n-l.n)
	return moved > math.Max(1, l.cep/2) || math.Abs(s.fused-l.fused) >= 0.15 || s.class != l.class ||
		(s.readable && !l.readable) || (s.readable && math.Abs(s.skin-l.skin) >= 1.0) || s.cep < 0.7*l.cep || s.moved != l.moved
}

// MarkReported records that a report of the track was sent.
func (tr *Tracker) MarkReported(t *Track, at time.Time) {
	t.Revision++
	t.last = tr.snap(t)
	t.last.at = at
}

// CEP returns the track's reported position uncertainty: the pose-error model
// plus the detection error DetSigmaM (calibrated against the truth in phase 3).
func (tr *Tracker) CEP(t *Track) float64 {
	return CEP(t.Var(tr.p.CorrTau) + tr.p.DetSigmaM*tr.p.DetSigmaM)
}

// Params returns the tracker's parameters.
func (tr *Tracker) Params() TrackParams { return tr.p }

// Near returns the confirmed tracks within r metres of (e, n) that were
// watched in a hover of the given task.
func (tr *Tracker) Near(e, n, r float64, task string) []*Track {
	var out []*Track
	for _, t := range tr.tracks {
		if t.Hits >= tr.p.ConfirmHits && t.Motion != nil && t.refTask == task && math.Hypot(t.E-e, t.N-n) <= r {
			out = append(out, t)
		}
	}
	return out
}
