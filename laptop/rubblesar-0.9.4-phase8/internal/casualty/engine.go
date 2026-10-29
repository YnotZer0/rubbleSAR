package casualty

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"rubblesar/internal/config"
	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/intake"
	"rubblesar/internal/msg"
	"rubblesar/internal/sarpb"
)

// StateFile keeps the casualty ids (and the close looks) across restarts.
const StateFile = "casualties-state.json"

// Look is the outcome of one investigation (a task DONE by a drone).
type Look struct {
	Task     string    `json:"task"`
	Casualty string    `json:"casualty"`
	Found    bool      `json:"found"`
	Moved    bool      `json:"moved"`
	Frames   uint32    `json:"frames"`
	At       time.Time `json:"at"`
}

// taskHint: the casualty a task was sent to and its target (local metres).
type taskHint struct {
	Casualty string  `json:"casualty"`
	E        float64 `json:"e"`
	N        float64 `json:"n"`
}

type state struct {
	members []string
	rev     uint32
	last    *sarpb.Casualty
}

// Engine holds every part and the current casualties. Safe for concurrent use.
type Engine struct {
	mu     sync.Mutex
	fu     config.Fusion
	tr     config.Triage
	local  geo.Local
	ground float64
	parts  map[string]*Part
	cas    map[string]*state
	merged map[string]string // removed id -> id it was merged into
	looks  map[string]Look   // by task id
	tasks  map[string]taskHint
	next   int
	dirty  bool
	path   string // state file ("" = not persisted)
	seq    msg.Seq
}

// New makes an engine; dir is the incident folder (state file location, "" for none).
func New(fu config.Fusion, tr config.Triage, centre geo.Point, groundAltMSL float64, dir string) *Engine {
	e := &Engine{fu: fu, tr: tr, local: geo.NewLocal(centre), ground: groundAltMSL, parts: map[string]*Part{},
		cas: map[string]*state{}, merged: map[string]string{}, looks: map[string]Look{}, tasks: map[string]taskHint{}, next: 1}
	if dir != "" {
		e.path = filepath.Join(dir, StateFile)
	}
	return e
}

type persisted struct {
	Next    int                 `json:"next"`
	IDs     map[string][]string `json:"ids"`
	Revs    map[string]uint32   `json:"revisions"`
	Merged  map[string]string   `json:"merged"`
	Looks   []Look              `json:"looks"`
	Tasks   map[string]taskHint `json:"tasks"`
	Written time.Time           `json:"written"`
}

// Load restores ids and looks from the state file and rebuilds the parts from
// the stored reports (in the order they were stored).
func (e *Engine) Load(recs []intake.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.path != "" {
		b, err := os.ReadFile(e.path)
		if err == nil {
			var p persisted
			if err := json.Unmarshal(b, &p); err != nil {
				return diag.Wrap("E-ANL-003", err, "reading %s", e.path)
			}
			e.next = max(p.Next, 1)
			for id, m := range p.IDs {
				e.cas[id] = &state{members: m, rev: p.Revs[id]}
			}
			for k, v := range p.Merged {
				e.merged[k] = v
			}
			for _, l := range p.Looks {
				e.looks[l.Task] = l
			}
			for k, v := range p.Tasks {
				e.tasks[k] = v
			}
		} else if !os.IsNotExist(err) {
			return diag.Wrap("E-ANL-003", err, "reading %s", e.path)
		}
	}
	for _, r := range recs {
		e.addLocked(r)
	}
	return nil
}

// Reset forgets everything (a new acceptance run).
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.parts, e.cas, e.merged, e.looks, e.tasks, e.next, e.dirty = map[string]*Part{}, map[string]*state{}, map[string]string{}, map[string]Look{}, map[string]taskHint{}, 1, false
}

// Add takes one stored report (intake.OnRecord).
func (e *Engine) Add(r intake.Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addLocked(r)
}

func (e *Engine) addLocked(r intake.Record) {
	if classOf(r.Part) < 0 {
		return
	}
	key := r.UAV + "/" + r.TrackID
	p := e.parts[key]
	if p == nil {
		p = &Part{}
		e.parts[key] = p
	} else if r.Revision > 0 && p.DetectionID != "" && revisionOf(p.DetectionID) > r.Revision {
		// An older revision arriving late (a re-send after an outage): keep its
		// temperature reading for the trend, nothing else.
		if r.Thermal.Readable && r.CaptureNs > 0 && p.readings != nil {
			p.readings[r.CaptureNs] = reading{at: time.Unix(0, r.CaptureNs), skin: float64(r.Thermal.SkinC), rangeM: float64(r.RangeM)}
		}
		return
	}
	p.fromRecord(r, e.local)
	e.dirty = true
}

func revisionOf(id string) uint32 {
	i := strings.LastIndexByte(id, '.')
	if i < 0 {
		return 0
	}
	var r uint32
	fmt.Sscanf(strings.SplitN(id[i+1:], "~", 2)[0], "%d", &r)
	return r
}

// ApplyTasks records the outcome of every investigation that is DONE.
func (e *Engine) ApplyTasks(tasks []*sarpb.Task) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range tasks {
		if t.GetCasualtyId() != "" && t.GetTarget() != nil {
			if _, ok := e.tasks[t.GetTaskId()]; !ok {
				te, tn := e.local.ToLocal(geo.Point{Lat: t.GetTarget().GetLatDeg(), Lon: t.GetTarget().GetLonDeg()})
				e.tasks[t.GetTaskId()] = taskHint{Casualty: t.GetCasualtyId(), E: te, N: tn}
				e.dirty = true
			}
		}
		if t.GetState() != sarpb.TaskState_TASK_STATE_DONE || t.GetCasualtyId() == "" {
			continue
		}
		if _, ok := e.looks[t.GetTaskId()]; ok {
			continue
		}
		at := time.Unix(0, t.GetFinishedUnixNs())
		e.looks[t.GetTaskId()] = Look{Task: t.GetTaskId(), Casualty: t.GetCasualtyId(), Found: t.GetResult().GetFound(),
			Moved: t.GetResult().GetMoved(), Frames: t.GetResult().GetFrames(), At: at}
		e.dirty = true
	}
}

// resolve follows merges to the casualty an id now belongs to.
func (e *Engine) resolve(id string) string {
	for i := 0; i < 20; i++ {
		to, ok := e.merged[id]
		if !ok {
			return id
		}
		id = to
	}
	return id
}

// Update recomputes the casualties if anything changed. It returns the
// records that changed (with a new revision) and those removed by a merge
// (published once more with merged_into).
func (e *Engine) Update(inc, source string) (changed, removed []*sarpb.Casualty) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dirty {
		return nil, nil
	}
	e.dirty = false
	parts := make([]*Part, 0, len(e.parts))
	for _, p := range e.parts {
		parts = append(parts, p)
	}
	sort.Slice(parts, func(i, j int) bool {
		if !parts[i].First.Equal(parts[j].First) {
			return parts[i].First.Before(parts[j].First)
		}
		return parts[i].Key < parts[j].Key
	})
	owner := map[string]string{} // part key -> old id
	for id, st := range e.cas {
		for _, k := range st.members {
			owner[k] = id
		}
	}
	hints := Hints{
		Task: func(task string) (string, float64, float64, bool) {
			h, ok := e.tasks[task]
			return e.resolve(h.Casualty), h.E, h.N, ok
		},
		OldID: func(ps []*Part) string {
			cnt := map[string]int{}
			best, bn := "", 0
			for _, p := range ps {
				if id, ok := owner[p.Key]; ok {
					cnt[id]++
					if cnt[id] > bn || (cnt[id] == bn && id < best) {
						best, bn = id, cnt[id]
					}
				}
			}
			return best
		},
	}
	cs := clusterAll(parts, e.fu, hints)

	// Keep ids: each new casualty takes the old id sharing most of its parts.
	type match struct {
		id      string
		ci, ovl int
	}
	var ms []match
	members := make([][]string, len(cs))
	for ci, c := range cs {
		members[ci] = keysOf(c.parts())
		cnt := map[string]int{}
		for _, k := range members[ci] {
			if id, ok := owner[k]; ok {
				cnt[id]++
			}
		}
		for id, n := range cnt {
			ms = append(ms, match{id, ci, n})
		}
	}
	sort.Slice(ms, func(i, j int) bool {
		if ms[i].ovl != ms[j].ovl {
			return ms[i].ovl > ms[j].ovl
		}
		if ms[i].id != ms[j].id {
			return ms[i].id < ms[j].id
		}
		return ms[i].ci < ms[j].ci
	})
	idOf := make([]string, len(cs))
	used := map[string]bool{}
	for _, m := range ms {
		if idOf[m.ci] == "" && !used[m.id] {
			idOf[m.ci], used[m.id] = m.id, true
		}
	}
	for ci := range cs {
		if idOf[ci] == "" {
			for {
				id := fmt.Sprintf("C-%03d", e.next)
				e.next++
				if _, taken := e.cas[id]; !taken {
					idOf[ci] = id
					break
				}
			}
		}
	}
	// Old ids not kept: merged into the casualty now holding most of their parts.
	now := map[string]*state{}
	for ci := range cs {
		id := idOf[ci]
		st := e.cas[id]
		if st == nil {
			st = &state{}
		}
		st.members = members[ci]
		now[id] = st
	}
	for id, st := range e.cas {
		if _, ok := now[id]; ok {
			continue
		}
		cnt := map[string]int{}
		for _, k := range st.members {
			for ci := range cs {
				for _, m := range members[ci] {
					if m == k {
						cnt[idOf[ci]]++
					}
				}
			}
		}
		into, best := "", 0
		for nid, n := range cnt {
			if n > best || (n == best && nid < into) {
				into, best = nid, n
			}
		}
		e.merged[id] = into
		if st.last != nil {
			gone := proto.Clone(st.last).(*sarpb.Casualty)
			gone.MergedInto, gone.Revision = into, st.rev+1
			gone.Header = e.header(inc, source)
			removed = append(removed, gone)
		}
	}
	e.cas = now

	looks := map[string][]Look{}
	for _, l := range e.looks {
		id := e.resolve(l.Casualty)
		looks[id] = append(looks[id], l)
	}
	for ci, c := range cs {
		id := idOf[ci]
		st := e.cas[id]
		cr := e.build(id, c, looks[id])
		if st.last != nil && proto.Equal(st.last, cr) {
			continue
		}
		st.last = cr
		st.rev++
		out := proto.Clone(cr).(*sarpb.Casualty)
		out.Revision = st.rev
		out.Header = e.header(inc, source)
		changed = append(changed, out)
	}
	e.saveLocked()
	return changed, removed
}

func (e *Engine) header(inc, source string) *sarpb.Header {
	return msg.NewHeader(inc, source, e.seq.Next())
}

// List returns the current casualties (latest revision each), oldest first.
func (e *Engine) List() []*sarpb.Casualty {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []*sarpb.Casualty
	for _, st := range e.cas {
		if st.last == nil {
			continue
		}
		c := proto.Clone(st.last).(*sarpb.Casualty)
		c.Revision = st.rev
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CasualtyId < out[j].CasualtyId })
	return out
}

func (e *Engine) saveLocked() {
	if e.path == "" {
		return
	}
	p := persisted{Next: e.next, IDs: map[string][]string{}, Revs: map[string]uint32{}, Merged: e.merged, Tasks: e.tasks, Written: time.Now().UTC()}
	for id, st := range e.cas {
		p.IDs[id], p.Revs[id] = st.members, st.rev
	}
	for _, l := range e.looks {
		p.Looks = append(p.Looks, l)
	}
	sort.Slice(p.Looks, func(i, j int) bool { return p.Looks[i].Task < p.Looks[j].Task })
	b, _ := json.MarshalIndent(p, "", " ")
	tmp := e.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, e.path)
	}
}

// --- One casualty record ---------------------------------------------------

func (e *Engine) build(id string, c *cluster, looks []Look) *sarpb.Casualty {
	parts := c.parts()
	ll := e.local.ToPoint(c.e, c.n)
	cr := &sarpb.Casualty{CasualtyId: id, Position: &sarpb.GeoPoint{LatDeg: ll.Lat, LonDeg: ll.Lon, AltMslM: e.ground},
		CepM: float32(math.Round(c.cep()*100) / 100)}
	var first, last time.Time
	uavs := map[string]bool{}
	bestFused, bestID := -1.0, ""
	for _, p := range parts {
		pl := e.local.ToPoint(p.E, p.N)
		cr.Parts = append(cr.Parts, &sarpb.CasualtyPart{UavId: p.UAV, TrackId: p.Track, DetectionId: p.DetectionID, Part: sarpb.PartClass(p.Class + 1),
			Confidence: float32(p.Fused), Observations: uint32(p.Obs), Position: &sarpb.GeoPoint{LatDeg: pl.Lat, LonDeg: pl.Lon, AltMslM: e.ground},
			CepM: float32(p.CEP), MinRangeM: float32(p.MinRangeM)})
		uavs[p.UAV] = true
		if first.IsZero() || p.First.Before(first) {
			first = p.First
		}
		if p.Last.After(last) {
			last = p.Last
		}
		// Crops: prefer a close, confident body part.
		score := p.Fused - 0.002*p.MinRangeM
		if p.Class == WarmSpot {
			score -= 0.5
		}
		if p.HasCrop && score > bestFused {
			bestFused, bestID = score, p.DetectionID
		}
	}
	for u := range uavs {
		cr.UavIds = append(cr.UavIds, u)
	}
	sort.Strings(cr.UavIds)
	cr.FirstSeenUnixNs, cr.LastSeenUnixNs, cr.BestDetectionId = first.UnixNano(), last.UnixNano(), bestID

	cr.Thermal = e.thermal(parts)
	cr.Movement = movement(parts, e.aimedAt(id, c), e.tr.MovedFrames)

	// Confidence: independent drones agreeing; warm spots alone count less.
	miss := 1.0
	bodyParts := 0
	for _, g := range c.groups {
		best := 0.0
		for _, p := range g.parts {
			f := p.Fused
			if p.Class == WarmSpot {
				f *= 0.6
			} else {
				bodyParts++
			}
			best = math.Max(best, f)
		}
		miss *= 1 - best
	}
	cr.Confidence = float32(math.Round(math.Min(0.995, 1-miss)*1000) / 1000)

	var found, notFound int
	var lastLook time.Time
	for _, l := range looks {
		cr.Investigations++
		if l.Found {
			found++
		} else {
			notFound++
		}
		if l.At.After(lastLook) {
			lastLook = l.At
		}
	}
	cr.NotFoundLooks = uint32(notFound)
	cr.SeenClose = float64(cr.Movement.WatchedS) >= e.tr.MinWatchS || found > 0
	e.triage(cr, last, lastLook, bodyParts)
	cr.Summary = e.summary(cr, parts)
	return cr
}

func (e *Engine) thermal(parts []*Part) *sarpb.CasualtyThermal {
	type rd struct {
		skin, amb, delta float64
		warm, bare       bool
	}
	var close, all []rd
	hist := map[int][]reading{} // close readings by part class, for the trend
	seenAt := map[[2]int64]bool{}
	for _, p := range parts {
		if !p.Thermal.Readable {
			continue
		}
		r := rd{skin: float64(p.Thermal.SkinC), amb: float64(p.Thermal.AmbientC), delta: float64(p.Thermal.DeltaC)}
		r.bare = p.Class != Arm && p.Class != Leg && p.Class != Torso
		if r.bare {
			r.warm = r.skin >= e.tr.SkinMinC && r.skin <= e.tr.SkinMaxC && r.delta >= e.tr.WarmDeltaC
		} else {
			// Clothing reads cooler than skin on the living, and warmer than
			// the air on the dead when the sun is on it.
			r.warm = r.skin >= e.tr.ClothedMinC && r.skin <= e.tr.SkinMaxC && r.delta >= e.tr.ClothedDeltaC
		}
		all = append(all, r)
		if p.RangeM > 0 && p.RangeM <= e.tr.CloseRangeM {
			close = append(close, r)
		}
		for ns, h := range p.readings {
			k := [2]int64{ns, int64(p.Class)}
			if !seenAt[k] && h.rangeM > 0 && h.rangeM <= e.tr.CloseRangeM {
				seenAt[k] = true
				hist[p.Class] = append(hist[p.Class], h)
			}
		}
	}
	t := &sarpb.CasualtyThermal{Readings: uint32(len(all)), CloseReadings: uint32(len(close))}
	use := close
	if len(use) == 0 {
		use = all
	}
	if len(use) == 0 {
		return t
	}
	med := func(f func(rd) float64) float64 {
		v := make([]float64, len(use))
		for i, r := range use {
			v[i] = f(r)
		}
		sort.Float64s(v)
		if len(v)%2 == 1 {
			return v[len(v)/2]
		}
		return (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	t.Readable = true
	t.SkinC = r1(med(func(r rd) float64 { return r.skin }))
	t.AmbientC = r1(med(func(r rd) float64 { return r.amb }))
	t.DeltaC = r1(med(func(r rd) float64 { return r.delta }))
	// Warm when at least half of the readings are (a tie counts as warm: the
	// cautious side); bare skin, when read, decides.
	var bare []rd
	for _, r := range use {
		if r.bare {
			bare = append(bare, r)
		}
	}
	judge := use
	if len(bare) > 0 {
		judge = bare
	}
	warm := 0
	for _, r := range judge {
		if r.warm {
			warm++
		}
	}
	t.Warm = 2*warm >= len(judge)
	if slope, span, ok := e.trend(hist); ok {
		t.TrendCPerH, t.TrendSpanS = float32(math.Round(slope*100)/100), float32(math.Round(span.Seconds()))
	}
	return t
}

// trendBucket: close readings this close in time are one look (their median counts once).
const trendBucket = 3 * time.Minute

// trendMaxCPerH: skin does not warm or cool faster than this (a body cools by
// a degree or two an hour); a faster "trend" compares readings that are not
// of the same thing (two people's arms in one record) and is not reported.
const trendMaxCPerH = 10.0

// trend is the skin-temperature change per hour of one kind of body part
// read close up: the readings are taken one look at a time (the median of
// each look) and only the same kind of part is compared with itself (a hand
// with a hand, never with a sleeve), with at least three looks spanning
// trend_min_span. The slope is the median of the slopes between looks
// (robust to one bad reading). Bare skin is preferred, then the most looks.
func (e *Engine) trend(hist map[int][]reading) (slope float64, span time.Duration, ok bool) {
	bestN := 0
	bestBare := false
	classes := make([]int, 0, len(hist))
	for c := range hist {
		classes = append(classes, c)
	}
	sort.Ints(classes)
	for _, c := range classes {
		rs := hist[c]
		sort.Slice(rs, func(i, j int) bool { return rs[i].at.Before(rs[j].at) })
		type look struct {
			at   time.Time
			skin float64
		}
		var looks []look
		for i := 0; i < len(rs); {
			j := i
			var v []float64
			for j < len(rs) && rs[j].at.Sub(rs[i].at) < trendBucket {
				v = append(v, rs[j].skin)
				j++
			}
			sort.Float64s(v)
			looks = append(looks, look{rs[i].at, v[len(v)/2]})
			i = j
		}
		if len(looks) < 3 {
			continue
		}
		sp := looks[len(looks)-1].at.Sub(looks[0].at)
		if sp < e.tr.TrendMinSpan.Duration {
			continue
		}
		bare := c == Hand || c == Head
		if ok && (bestBare && !bare || bestBare == bare && len(looks) <= bestN) {
			continue
		}
		var slopes []float64
		for a := range looks {
			for b := a + 1; b < len(looks); b++ {
				if dh := looks[b].at.Sub(looks[a].at).Hours(); dh > 0 {
					slopes = append(slopes, (looks[b].skin-looks[a].skin)/dh)
				}
			}
		}
		if len(slopes) == 0 {
			continue
		}
		sort.Float64s(slopes)
		if m := slopes[len(slopes)/2]; math.Abs(m) <= trendMaxCPerH {
			slope, span, ok, bestN, bestBare = m, sp, true, len(looks), bare
		}
	}
	return slope, span, ok
}

func r1(v float64) float32 { return float32(math.Round(v*10) / 10) }

// aimedAtM: a hover whose target lies this close to a casualty was aimed at
// it (the target is where the casualty was when the task was made; fusion may
// have moved it a little since, or given its parts another id). Less than the
// distance between two victims lying side by side (about 3 m).
const aimedAtM = 2.5

// aimedAt says whether a hover (by task id) was aimed at this casualty: its
// task named the casualty (or one merged into it) or targeted its position.
// A neighbour glimpsed during someone else's investigation is not watched:
// it is off the centre of the frame and nobody followed it. Hovers whose task
// casualty-analyst never heard of (no task list) are kept.
func (e *Engine) aimedAt(id string, c *cluster) func(task string) bool {
	return func(task string) bool {
		h, ok := e.tasks[task]
		if !ok {
			return true
		}
		return e.resolve(h.Casualty) == id || math.Hypot(h.E-c.e, h.N-c.n) <= aimedAtM
	}
}

// movement is what the hovers aimed at the casualty saw: moved if, in one of
// them, a part moved (the drone's verdict) or its parts moved in need frames
// in all (a limb moving for a second or two shows in one frame of the hand
// and the same frame of the arm: two parts, one movement); watched for the
// sum over those hovers of the longest any part was followed.
func movement(parts []*Part, aimed func(task string) bool, need int) *sarpb.CasualtyMovement {
	if need < 1 {
		need = 2
	}
	m := &sarpb.CasualtyMovement{}
	perTask := map[string]float64{}
	frames := map[string]uint32{}
	for _, p := range parts {
		// Every hover's own verdict counts (a part watched in two hovers has
		// one report per hover; its latest one only says what the last saw).
		moves := p.moves
		if len(moves) == 0 && p.Move != nil {
			moves = map[string]intake.Movement{p.TaskID: *p.Move}
		}
		for task, mv := range moves {
			if !aimed(task) {
				continue
			}
			perTask[task] = math.Max(perTask[task], float64(mv.WatchedS))
			m.Moved = m.Moved || mv.Moved
			frames[task] += mv.MovedFrames
		}
	}
	for _, f := range frames {
		m.MovedFrames = max(m.MovedFrames, f)
		m.Moved = m.Moved || int(f) >= need
	}
	var w float64
	for _, v := range perTask {
		w += v
	}
	m.WatchedS = float32(math.Round(w*10) / 10)
	return m
}

// triage applies the status rules (plan section 8; thresholds in [analyst.triage]).
func (e *Engine) triage(c *sarpb.Casualty, lastSeen, lastLook time.Time, bodyParts int) {
	th, mv := c.Thermal, c.Movement
	set := func(st sarpb.CasualtyStatus, tr sarpb.Triage, provisional bool, reason string) {
		c.Status, c.Triage, c.Provisional, c.Reason = st, tr, provisional, reason
	}
	need := func(reason string) {
		c.NeedsInvestigation, c.InvestigationReason = true, reason
	}
	conf := float64(c.Confidence)
	switch {
	case int(c.NotFoundLooks) >= e.tr.NotFoundLooks && !lastSeen.After(lastLook):
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_NOT_FOUND, sarpb.Triage_TRIAGE_GREY, false,
			fmt.Sprintf("nothing found on %d close looks", c.NotFoundLooks))
		c.PAlive = float32(0.1 * conf)
	case mv.Moved:
		why := "moved while watched close up"
		if !th.Warm {
			why += "; temperature not confirmed warm"
		}
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_RESPONSIVE, sarpb.Triage_TRIAGE_YELLOW, false, why)
		c.PAlive = float32(0.95 * conf)
	case !th.Readable:
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_UNKNOWN, sarpb.Triage_TRIAGE_GREY, true, "temperature not readable")
		c.PAlive = float32(0.5 * conf)
		need("temperature not readable")
	case th.Warm && th.TrendCPerH <= -float32(e.tr.CoolingCPerH) && th.TrendSpanS >= float32(e.tr.TrendMinSpan.Duration.Seconds()):
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_DETERIORATING, sarpb.Triage_TRIAGE_RED, !c.SeenClose,
			fmt.Sprintf("warm but cooling %.1f C/h", -th.TrendCPerH))
		c.PAlive = float32(0.85 * conf)
		if !c.SeenClose {
			need("provisionally Red, not yet seen close up")
		}
	case th.Warm && c.SeenClose:
		why := fmt.Sprintf("warm, no movement in %.0f s watched close up", mv.WatchedS)
		if float64(mv.WatchedS) < e.tr.MinWatchS {
			why = "warm when looked at close up; movement not watched (no outline to follow)"
		}
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_UNRESPONSIVE, sarpb.Triage_TRIAGE_RED, false, why)
		c.PAlive = float32(0.9 * conf)
	case th.Warm:
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_WARM_UNRESPONSIVE, sarpb.Triage_TRIAGE_RED, true, "warm; not yet watched close up")
		c.PAlive = float32(0.85 * conf)
		need("provisionally Red, not yet seen close up")
	case th.CloseReadings > 0 && c.SeenClose:
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_NO_HEAT, sarpb.Triage_TRIAGE_BLACK, false, "at ambient after a close look")
		c.PAlive = float32(0.05 * conf)
	default:
		why := "cold from far away"
		if th.CloseReadings > 0 {
			why = "cold, not yet watched close up"
		}
		set(sarpb.CasualtyStatus_CASUALTY_STATUS_UNKNOWN, sarpb.Triage_TRIAGE_GREY, true, why)
		c.PAlive = float32(0.3 * conf)
		need(why + ": needs a close look")
	}
	if bodyParts == 0 && c.Status != sarpb.CasualtyStatus_CASUALTY_STATUS_NOT_FOUND {
		c.Reason += "; thermal only, no body part seen"
	}
	if !c.NeedsInvestigation && c.Status != sarpb.CasualtyStatus_CASUALTY_STATUS_NOT_FOUND && float64(c.CepM) > e.tr.MaxCEPm {
		need(fmt.Sprintf("position uncertain (CEP %.1f m)", c.CepM))
	}
	c.PAlive = float32(math.Round(float64(c.PAlive)*1000) / 1000)
}

// Tier orders investigations: 1 provisional Red, 2 unknown (Grey), 3 position only.
func Tier(c *sarpb.Casualty) uint32 {
	switch {
	case !c.GetNeedsInvestigation():
		return 0
	case c.GetTriage() == sarpb.Triage_TRIAGE_RED && c.GetProvisional():
		return 1
	case c.GetTriage() == sarpb.Triage_TRIAGE_GREY:
		return 2
	}
	return 3
}

var triageWord = map[sarpb.Triage]string{sarpb.Triage_TRIAGE_RED: "Red", sarpb.Triage_TRIAGE_YELLOW: "Yellow", sarpb.Triage_TRIAGE_GREEN: "Green",
	sarpb.Triage_TRIAGE_BLACK: "Black", sarpb.Triage_TRIAGE_GREY: "Grey"}

// TriageName is the colour in words.
func TriageName(t sarpb.Triage) string {
	if w, ok := triageWord[t]; ok {
		return w
	}
	return "-"
}

func (e *Engine) summary(c *sarpb.Casualty, parts []*Part) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s seen by %s", c.CasualtyId, nonEmpty(partNames(parts), "warm spot"), strings.Join(c.UavIds, ", "))
	if t := c.Thermal; t.Readable {
		fmt.Fprintf(&b, "; %.1f C against %.1f C (%+.1f)", t.SkinC, t.AmbientC, t.DeltaC)
	} else {
		b.WriteString("; temperature not readable")
	}
	if m := c.Movement; m.WatchedS > 0 {
		if m.Moved {
			fmt.Fprintf(&b, "; moved (watched %.0f s)", m.WatchedS)
		} else {
			fmt.Fprintf(&b, "; still for %.0f s", m.WatchedS)
		}
	}
	p := ""
	if c.Provisional {
		p = " (provisional)"
	}
	fmt.Fprintf(&b, " -> %s%s: %s, +-%.1f m", TriageName(c.Triage), p, c.Reason, c.CepM)
	return b.String()
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
