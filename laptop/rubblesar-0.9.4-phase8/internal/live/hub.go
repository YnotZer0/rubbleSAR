package live

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Limits of what the picture keeps.
const (
	trailPoints   = 150 // a drone's trail: 5 min at one point per 2 s
	trailEvery    = 2 * time.Second
	linkPoints    = 120 // a drone's radio history: 4 min at one point per 2 s
	keepDets      = 40
	keepTimeline  = 3000 // the whole run (GET /v1/timeline)
	snapTimeline  = 200  // the latest lines, in each picture
	keepChats     = 120
	degradeQuiet  = 20 * time.Second // one "link falling behind" line per drone this often
	staleDroneAge = 10 * time.Second
	// silentAfter: a drone not heard from this long has lost its link (telemetry is 1 Hz).
	silentAfter = 6 * time.Second
)

// HubOptions configure a Hub.
type HubOptions struct {
	Incident string
	Mode     string
	// Triage lists the colours that get a rescue team (default red, yellow).
	Triage []string
	// Quiet: the first casualty list, task list and dispatch state seed the
	// picture without timeline lines (sar-web started in the middle of a run).
	Quiet bool
	// Clock is the picture's time (default time.Now).
	Clock func() time.Time
}

// Hub holds the picture and turns changes into timeline lines. Safe for concurrent use.
type Hub struct {
	mu   sync.Mutex
	o    HubOptions
	need map[string]bool

	bus   string
	run   string
	start time.Time

	drones   map[string]*Drone
	droneAux map[string]*droneAux
	cas      map[string]*Casualty
	casSeen  bool
	merged   map[string]string // casualty id -> merged into
	tasks    map[string]*Task
	taskSeen bool
	teams    []*Team
	asgs     map[string]*Assignment
	asgOrder []string
	asgSeen  bool
	waiting  []string
	tak      *TAK
	dets     []*Det
	detSeen  map[string]bool
	detCas   map[string]string // detection id -> casualty id
	detTotal uint64
	detBytes uint64
	timeline []*Item
	chats    []*Chat
	chatSeen map[string]bool
	replay   *Replay

	seq  uint64
	subs map[chan struct{}]bool
}

type droneAux struct {
	trailAt     time.Time
	linkAt      time.Time
	seen        time.Time
	surveyed    bool
	surveying   bool
	airborne    bool
	known       bool
	silent      bool
	resentAt    uint64
	flushing    bool
	degrade     uint32
	degradeAt   time.Time
	budget      uint32
	budgetKnown bool
}

// NewHub returns an empty picture.
func NewHub(o HubOptions) *Hub {
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Mode == "" {
		o.Mode = "live"
	}
	if len(o.Triage) == 0 {
		o.Triage = []string{"red", "yellow"}
	}
	h := &Hub{o: o, need: map[string]bool{}, subs: map[chan struct{}]bool{}}
	for _, t := range o.Triage {
		h.need[strings.ToLower(t)] = true
	}
	h.clear()
	return h
}

func (h *Hub) clear() {
	h.drones, h.droneAux = map[string]*Drone{}, map[string]*droneAux{}
	h.clearRun()
	h.casSeen, h.taskSeen, h.asgSeen = false, false, false
}

// clearRun forgets everything of a run (the drones stay).
func (h *Hub) clearRun() {
	h.cas, h.merged, h.tasks, h.asgs = map[string]*Casualty{}, map[string]string{}, map[string]*Task{}, map[string]*Assignment{}
	h.asgOrder, h.teams, h.waiting, h.dets, h.timeline, h.chats = nil, nil, nil, nil, nil, nil
	h.detSeen, h.detCas, h.chatSeen = map[string]bool{}, map[string]string{}, map[string]bool{}
	h.detTotal, h.detBytes = 0, 0
	for id, d := range h.drones {
		d.Stored, d.Bytes = 0, 0
		h.droneAux[id].flushing = false
	}
}

// Reset empties the picture (a replay seeking back).
func (h *Hub) Reset() {
	h.mu.Lock()
	h.clear()
	h.run, h.start = "", time.Time{}
	h.changedLocked()
	h.mu.Unlock()
}

// Subscribe returns a channel that receives a value after each change (coalesced).
func (h *Hub) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) changedLocked() {
	h.seq++
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Seq is the change counter.
func (h *Hub) Seq() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seq
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (h *Hub) itemLocked(at time.Time, kind, ref, tone, format string, args ...any) {
	it := &Item{T: ms(at), Kind: kind, Ref: ref, Tone: tone, Text: fmt.Sprintf(format, args...)}
	h.timeline = append(h.timeline, it)
	if n := len(h.timeline); n > keepTimeline {
		h.timeline = append([]*Item(nil), h.timeline[n-keepTimeline:]...)
	}
}

// SetBus records the state of the zenoh connection in words.
func (h *Hub) SetBus(state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bus != state {
		h.bus = state
		h.changedLocked()
	}
}

// SetStart sets when the run started (replays and the director).
func (h *Hub) SetStart(t time.Time, run string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.start, h.run = t, run
	h.changedLocked()
}

// SetReplay records where a replay stands.
func (h *Hub) SetReplay(r *Replay) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r != nil {
		c := *r
		r = &c
	}
	h.replay = r
	h.changedLocked()
}

// Note adds a timeline line.
func (h *Hub) Note(at time.Time, kind, tone, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if kind == "" {
		kind = "note"
	}
	h.itemLocked(at, kind, "", tone, "%s", text)
	h.changedLocked()
}

// --- Drones ---------------------------------------------------------------------------------

// SetDrone replaces a drone's telemetry (Trail is kept by the hub; give none).
func (h *Hub) SetDrone(d *Drone, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prev := h.drones[d.ID]
	aux := h.droneAux[d.ID]
	if aux == nil {
		aux = &droneAux{}
		h.droneAux[d.ID] = aux
	}
	nd := *d
	if prev != nil {
		nd.Trail, nd.Stored, nd.Bytes, nd.LinkHist = prev.Trail, prev.Stored, prev.Bytes, prev.LinkHist
	}
	if l := nd.Link; l != nil && at.Sub(aux.linkAt) >= trailEvery {
		aux.linkAt = at
		nd.LinkHist = append(nd.LinkHist, [5]float64{float64(at.UnixMilli()), float64(l.UsedBps), float64(l.BudgetBps), float64(l.QueuedMsgs + l.Unacked), float64(l.Degrade)})
		if len(nd.LinkHist) > linkPoints {
			nd.LinkHist = append([][5]float64(nil), nd.LinkHist[len(nd.LinkHist)-linkPoints:]...)
		}
	}
	if nd.Lat != 0 || nd.Lon != 0 {
		if airborne(nd.State) && at.Sub(aux.trailAt) >= trailEvery {
			aux.trailAt = at
			nd.Trail = append(nd.Trail, [2]float64{round6(nd.Lat), round6(nd.Lon)})
			if len(nd.Trail) > trailPoints {
				nd.Trail = append([][2]float64(nil), nd.Trail[len(nd.Trail)-trailPoints:]...)
			}
		}
	}
	h.drones[d.ID] = &nd
	h.droneEventsLocked(&nd, aux, at)
	aux.seen = at
	h.changedLocked()
}

// Tick notices drones that have gone silent (call it every second or so, with
// the picture's time). A drone sends nothing while its link is down, so
// silence is how the ZBook sees a blackout.
func (h *Hub) Tick(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := false
	for id, a := range h.droneAux {
		if a.known && !a.silent && !a.seen.IsZero() && now.Sub(a.seen) >= silentAfter {
			a.silent = true
			q := ""
			if d := h.drones[id]; d != nil && d.Link != nil && d.Link.QueuedMsgs+d.Link.Unacked > 0 {
				q = fmt.Sprintf(" (%d were already waiting)", d.Link.QueuedMsgs+d.Link.Unacked)
			}
			h.itemLocked(a.seen.Add(silentAfter), "outage", id, "warn", "%s not heard for %s: link lost; its reports wait in the drone's on-board queue%s", id, dur(silentAfter), q)
			changed = true
		}
	}
	if changed {
		h.changedLocked()
	}
}

func airborne(state string) bool {
	switch state {
	case "", "landed", "unspecified", "standby", "idle":
		return false
	}
	return true
}

func kbps(bps uint32) string {
	if bps == 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%g kbit/s", math.Round(float64(bps)/100)/10)
}

var degradeWords = []string{"full reports", "small crops", "thermal crops only", "metadata only"}

func (h *Hub) droneEventsLocked(d *Drone, a *droneAux, at time.Time) {
	first := !a.known
	a.known = true
	// Flight.
	air := airborne(d.State)
	if !first && air != a.airborne {
		if air {
			h.itemLocked(at, "drone", d.ID, "info", "%s took off (%s, battery %.0f%%)", d.ID, d.Role, d.Battery)
		} else {
			h.itemLocked(at, "drone", d.ID, "info", "%s landed (battery %.0f%%)", d.ID, d.Battery)
		}
	}
	a.airborne = air
	if d.State == "survey" && !a.surveying && !d.Surveyed {
		if !first || h.o.Mode == "replay" {
			if d.Steps > 0 {
				h.itemLocked(at, "survey", d.ID, "info", "%s is surveying the collapse sites (%d survey legs, %.0f m up)", d.ID, d.Steps, d.Alt)
			} else {
				h.itemLocked(at, "survey", d.ID, "info", "%s is surveying the collapse sites (%.0f m up)", d.ID, d.Alt)
			}
		}
		a.surveying = true
	}
	if d.Surveyed && !a.surveyed {
		if !first {
			h.itemLocked(at, "survey", d.ID, "info", "%s finished its survey", d.ID)
		}
		a.surveyed, a.surveying = true, false
	}
	if !d.Surveyed && a.surveyed && d.Steps > 0 && d.Step < d.Steps {
		a.surveyed = false // a new mission
	}
	l := d.Link
	if l == nil {
		return
	}
	// Budget.
	if a.budgetKnown && l.BudgetBps != a.budget {
		h.itemLocked(at, "link", d.ID, "warn", "%s radio budget now %s (was %s)", d.ID, kbps(l.BudgetBps), kbps(a.budget))
	}
	a.budget, a.budgetKnown = l.BudgetBps, true
	// Heard again after a silence (a blackout: the drone sends nothing while its link is down).
	if a.silent {
		h.itemLocked(at, "outage", d.ID, "info", "%s heard again after %s: %d reports waiting on board, sending them now", d.ID, dur(at.Sub(a.seen)), l.QueuedMsgs+l.Unacked)
		a.silent, a.flushing, a.resentAt = false, true, l.Resent
	}
	if a.flushing && l.QueuedMsgs == 0 && l.Unacked == 0 {
		lost := ", none lost"
		if l.Discarded > 0 {
			lost = fmt.Sprintf(", %d discarded (queue full)", l.Discarded)
		}
		h.itemLocked(at, "outage", d.ID, "green", "%s queue empty: every report held during the blackout reached the command post and was acknowledged (%d re-sent%s)", d.ID, l.Resent-a.resentAt, lost)
		a.flushing = false
	}
	// Payload reduction.
	if l.Degrade != a.degrade && int(l.Degrade) < len(degradeWords) {
		if l.Degrade > a.degrade || at.Sub(a.degradeAt) >= degradeQuiet {
			if l.Degrade > 0 {
				h.itemLocked(at, "link", d.ID, "warn", "%s link falling behind (%d reports queued): sending %s", d.ID, l.QueuedMsgs, degradeWords[l.Degrade])
			} else {
				h.itemLocked(at, "link", d.ID, "info", "%s link caught up: full reports again", d.ID)
			}
			a.degrade, a.degradeAt = l.Degrade, at
		}
	}
}

func dur(d time.Duration) string {
	if d < 90*time.Second {
		return fmt.Sprintf("%.0f s", d.Seconds())
	}
	m := int(math.Round(d.Minutes()))
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	if m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d h %d min", m/60, m%60)
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// --- Casualties -----------------------------------------------------------------------------

// SetMerged records that a casualty was merged into another (the same person seen twice).
func (h *Hub) SetMerged(from, into string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.merged[from] = into
}

func provWord(tri string, prov bool) string {
	if prov {
		return tri + "?"
	}
	return tri
}

func tone(tri string) string { return strings.ToLower(tri) }

// SetCasualties replaces the casualty list.
func (h *Hub) SetCasualties(list []*Casualty, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	quiet := !h.casSeen && h.o.Quiet && len(h.cas) == 0 && len(list) > 0
	next := make(map[string]*Casualty, len(list))
	for _, c := range list {
		nc := *c
		next[c.ID] = &nc
		for _, p := range c.Parts {
			if p.Det != "" {
				h.detCas[p.Det] = c.ID
			}
		}
		if c.Best != "" {
			h.detCas[c.Best] = c.ID
		}
		prev := h.cas[c.ID]
		if quiet {
			continue
		}
		switch {
		case prev == nil:
			who := strings.Join(c.UAVs, ", ")
			if who == "" {
				who = "a drone"
			}
			why := c.Reason
			if why != "" {
				why = ": " + why
			}
			h.itemLocked(at, "found", c.ID, tone(c.Triage), "%s found by %s, %s%s", c.ID, who, provWord(c.Triage, c.Prov), why)
		case prev.Triage != c.Triage:
			h.itemLocked(at, "triage", c.ID, tone(c.Triage), "%s now %s (was %s): %s", c.ID, provWord(c.Triage, c.Prov), provWord(prev.Triage, prev.Prov), c.Reason)
		case prev.Prov && !c.Prov:
			h.itemLocked(at, "triage", c.ID, tone(c.Triage), "%s confirmed %s: %s", c.ID, c.Triage, c.Reason)
		}
	}
	if !quiet {
		var gone []string
		for id := range h.cas {
			if next[id] == nil {
				gone = append(gone, id)
			}
		}
		sort.Strings(gone)
		if len(list) == 0 && len(gone) > 3 {
			h.itemLocked(at, "note", "", "info", "casualty list cleared (%d records archived: a new run)", len(gone))
			gone = nil
		}
		for _, id := range gone {
			if into := h.merged[id]; into != "" {
				h.itemLocked(at, "merge", into, "info", "%s merged into %s: the same person, seen twice", id, into)
			} else {
				h.itemLocked(at, "merge", id, "info", "%s no longer listed", id)
			}
		}
	} else {
		h.itemLocked(at, "note", "", "info", "picked up %d casualty records already known", len(list))
	}
	h.cas, h.casSeen = next, true
	for _, d := range h.dets {
		if d.Casualty == "" {
			d.Casualty = h.detCas[d.ID]
		}
	}
	h.changedLocked()
}

// --- Close looks ----------------------------------------------------------------------------

// SetTasks replaces the investigation task list.
func (h *Hub) SetTasks(list []*Task, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	quiet := !h.taskSeen && h.o.Quiet && len(h.tasks) == 0 && len(list) > 0
	next := make(map[string]*Task, len(list))
	for _, t := range list {
		nt := *t
		next[t.ID] = &nt
		prev := h.tasks[t.ID]
		if quiet || (prev != nil && prev.State == t.State && prev.UAV == t.UAV) {
			continue
		}
		why := t.Reason
		if why != "" {
			why = " (" + why + ")"
		}
		switch t.State {
		case "assigned", "en_route":
			if prev == nil || (prev.State != "assigned" && prev.State != "en_route") || prev.UAV != t.UAV {
				h.itemLocked(at, "look", t.Casualty, "blue", "%s sent for a close look at %s%s", nonEmpty(t.UAV, "a drone"), t.Casualty, why)
			}
		case "on_station":
			h.itemLocked(at, "look", t.Casualty, "blue", "%s over %s, watching it", nonEmpty(t.UAV, "the drone"), t.Casualty)
		case "done":
			res := "nothing there"
			if t.Found && t.Moved {
				res = "it moved: alive and responsive"
			} else if t.Found {
				res = "seen close up, not moving"
			}
			h.itemLocked(at, "look", t.Casualty, "blue", "close look at %s by %s: %s", t.Casualty, nonEmpty(t.UAV, "a drone"), res)
		case "aborted", "failed", "cancelled":
			note := t.Note
			if note != "" {
				note = ": " + note
			}
			h.itemLocked(at, "look", t.Casualty, "grey", "close look at %s %s%s", t.Casualty, t.State, note)
		}
	}
	if quiet {
		h.itemLocked(at, "note", "", "info", "picked up %d close-look tasks already known", len(list))
	}
	h.tasks, h.taskSeen = next, true
	h.changedLocked()
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// --- Rescue ---------------------------------------------------------------------------------

// Dispatch is rescue-dispatch's state.
type Dispatch struct {
	Run         string
	Teams       []*Team
	Assignments []*Assignment
	Waiting     []string
	TAK         *TAK
}

// SetDispatch replaces the teams and assignments. A new run token clears the run.
func (h *Hub) SetDispatch(d Dispatch, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d.Run != "" && h.run != "" && d.Run != h.run && h.o.Mode == "live" {
		h.clearRun()
		h.itemLocked(at, "note", "", "info", "a new run started (rescue-dispatch reset)")
	}
	if d.Run != "" && h.o.Mode == "live" {
		h.run = d.Run
	}
	quiet := !h.asgSeen && h.o.Quiet && len(h.asgs) == 0 && len(d.Assignments) > 0
	if d.Teams != nil {
		h.teams = d.Teams
	}
	h.waiting, h.tak = d.Waiting, d.TAK
	for _, a := range d.Assignments {
		na := *a
		prev := h.asgs[a.ID]
		if prev == nil {
			h.asgOrder = append(h.asgOrder, a.ID)
		}
		if na.Route == nil && prev != nil && (na.State == "en_route" || na.State == "on_scene") {
			na.Route, na.Access = prev.Route, prev.Access
		}
		h.asgs[a.ID] = &na
		if quiet {
			continue
		}
		team := h.callsignLocked(a.Team)
		if prev != nil && prev.Casualty != a.Casualty && a.Casualty != "" {
			// The casualty was merged into another record: the team follows it.
			h.itemLocked(at, "team", a.Casualty, tone(a.Triage), "%s now sent to %s: %s was merged into it, the same person", team, a.Casualty, prev.Casualty)
		}
		if prev != nil && prev.State == a.State {
			continue
		}
		if prev == nil && a.State != "en_route" && a.State != "" {
			// First seen after it set off: rescue-dispatch publishes every few
			// seconds, and a team a few metres away is there at once.
			h.sentLineLocked(at, a, team)
		}
		switch a.State {
		case "en_route":
			h.sentLineLocked(at, a, team)
		case "on_scene":
			what := ""
			if a.Extrication != "" {
				what = ": " + strings.ToLower(firstClause(a.Extrication)[:1]) + firstClause(a.Extrication)[1:]
			} else if a.ExtricationS > 0 {
				what = fmt.Sprintf(": freeing takes about %s for real rescuers", dur(time.Duration(a.ExtricationS*float64(time.Second))))
			}
			h.itemLocked(at, "team", a.Casualty, "blue", "%s reached %s%s", team, a.Casualty, what)
		case "done":
			c := h.cas[a.Casualty]
			if c != nil && h.need[strings.ToLower(c.Triage)] {
				h.itemLocked(at, "freed", a.Casualty, "green", "%s freed and handed over by %s", a.Casualty, team)
			} else {
				h.itemLocked(at, "team", a.Casualty, "info", "%s finished at %s", team, a.Casualty)
			}
		case "cancelled":
			note := a.Note
			if note != "" {
				note = ": " + firstClause(note)
			}
			h.itemLocked(at, "team", a.Casualty, "grey", "%s withdrawn from %s%s", team, a.Casualty, note)
		}
	}
	if quiet {
		h.itemLocked(at, "note", "", "info", "picked up %d rescue assignments already known", len(d.Assignments))
	}
	h.asgSeen = h.asgSeen || len(d.Assignments) > 0 || d.Teams != nil
	h.changedLocked()
}

// sentLineLocked is the timeline's line for a team sent to its casualty.
func (h *Hub) sentLineLocked(at time.Time, a *Assignment, team string) {
	how := "on foot"
	if t := h.teamLocked(a.Team); t != nil && t.Mode == "vehicle" {
		how = "by vehicle"
	}
	via := ""
	if len(a.Streets) > 0 {
		via = " via " + a.Streets[0]
	}
	tak := ""
	if t := h.teamLocked(a.Team); t != nil && t.Source == "tak" {
		tak = "; sent to the team's TAK device as a chat message and a route"
	}
	h.itemLocked(at, "team", a.Casualty, tone(a.Triage), "%s sent to %s (%s): %.0f m%s, about %s %s%s", team, a.Casualty, provWord(a.Triage, a.Prov), a.LengthM, via, dur(time.Duration(a.ETAs*float64(time.Second))), how, tak)
}

// SetTeams replaces the team positions only (replays record them apart from the assignments).
func (h *Hub) SetTeams(teams []*Team) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.teams = teams
	h.changedLocked()
}

func firstClause(s string) string {
	if i := strings.IndexAny(s, ";("); i > 20 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func (h *Hub) teamLocked(id string) *Team {
	for _, t := range h.teams {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (h *Hub) callsignLocked(id string) string {
	if t := h.teamLocked(id); t != nil && t.Callsign != "" {
		return t.Callsign
	}
	return strings.ToUpper(id)
}

// --- Detections and chat --------------------------------------------------------------------

// AddDetection records a detection report (again for a repeat: counted once).
func (h *Hub) AddDetection(d *Det) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.detSeen[d.ID] {
		return
	}
	h.detSeen[d.ID] = true
	h.detTotal++
	h.detBytes += uint64(d.Bytes)
	if dr := h.drones[d.UAV]; dr != nil {
		dr.Stored++
		dr.Bytes += uint64(d.Bytes)
	}
	nd := *d
	if nd.Casualty == "" {
		nd.Casualty = h.detCas[d.ID]
	}
	h.dets = append([]*Det{&nd}, h.dets...)
	if len(h.dets) > keepDets {
		h.dets = h.dets[:keepDets]
	}
	h.changedLocked()
}

// AddChats adds chat lines not seen before.
func (h *Hub) AddChats(lines []*Chat) {
	h.mu.Lock()
	defer h.mu.Unlock()
	added := false
	for _, c := range lines {
		k := fmt.Sprintf("%d|%s|%s", c.At, c.From, c.Text)
		if h.chatSeen[k] {
			continue
		}
		h.chatSeen[k] = true
		nc := *c
		h.chats = append(h.chats, &nc)
		h.itemLocked(time.UnixMilli(c.At), "chat", "", "info", "%s → %s: %s", c.From, c.To, c.Text)
		added = true
	}
	if !added {
		return
	}
	sort.SliceStable(h.chats, func(i, j int) bool { return h.chats[i].At < h.chats[j].At })
	if n := len(h.chats); n > keepChats {
		h.chats = append([]*Chat(nil), h.chats[n-keepChats:]...)
	}
	sort.SliceStable(h.timeline, func(i, j int) bool { return h.timeline[i].T < h.timeline[j].T })
	h.changedLocked()
}

// --- The picture ----------------------------------------------------------------------------

// Timeline is the whole timeline of the run (a picture holds the latest lines only).
func (h *Hub) Timeline() []*Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]*Item(nil), h.timeline...)
}

// Snapshot is the picture now (copies; safe to marshal while the hub changes).
func (h *Hub) Snapshot() *Picture {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.o.Clock()
	p := &Picture{Incident: h.o.Incident, Mode: h.o.Mode, Bus: h.bus, Now: ms(now), Start: ms(h.start), Seq: h.seq, Run: h.run}
	if h.replay != nil {
		r := *h.replay
		p.Replay = &r
	}
	// Drones, by id.
	ids := make([]string, 0, len(h.drones))
	for id := range h.drones {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		d := *h.drones[id]
		if d.Link != nil {
			l := *d.Link
			d.Link = &l
		}
		if d.Perc != nil {
			c := *d.Perc
			d.Perc = &c
		}
		if d.Task != nil {
			t := *d.Task
			d.Task = &t
		}
		d.Stale = now.Sub(time.UnixMilli(d.Seen)) > staleDroneAge
		p.Drones = append(p.Drones, &d)
		if airborne(d.State) && !d.Stale {
			p.KPI.Airborne++
		}
		if d.Link != nil {
			p.KPI.Lost += d.Link.Discarded
		}
	}
	// Assignments, in the order they were made; the latest one of each casualty.
	byCas := map[string]*Assignment{}
	for _, id := range h.asgOrder {
		a := *h.asgs[id]
		if a.State != "en_route" && a.State != "on_scene" {
			a.Route = nil
		}
		p.Assignments = append(p.Assignments, &a)
		if a.State != "cancelled" {
			byCas[a.Casualty] = &a // the latest one made
		}
		if a.State == "done" && (p.KPI.FirstFreed == 0 || a.Done < p.KPI.FirstFreed) && a.Done > 0 {
			p.KPI.FirstFreed = a.Done
		}
	}
	// Casualties: most urgent first.
	p.KPI.ByTriage = map[string]int{}
	for _, c := range h.cas {
		nc := *c
		if c.Thermal != nil {
			t := *c.Thermal
			nc.Thermal = &t
		}
		nc.NeedTeam = h.need[strings.ToLower(c.Triage)]
		if a := byCas[c.ID]; a != nil {
			nc.Team, nc.AsgState, nc.ETAs = h.callsignLocked(a.Team), a.State, a.ETAs
			nc.Rescued = a.State == "done" && nc.NeedTeam
		}
		p.Casualties = append(p.Casualties, &nc)
		p.KPI.Records++
		p.KPI.ByTriage[c.Triage]++
		if c.Prov {
			p.KPI.Provisional++
		}
		if nc.NeedTeam {
			p.KPI.NeedTeam++
		}
		if nc.Rescued {
			p.KPI.Rescued++
		}
		if c.First > 0 && (p.KPI.FirstSeen == 0 || c.First < p.KPI.FirstSeen) {
			p.KPI.FirstSeen = c.First
		}
	}
	sort.Slice(p.Casualties, func(i, j int) bool {
		a, b := p.Casualties[i], p.Casualties[j]
		if ra, rb := urgency(a), urgency(b); ra != rb {
			return ra < rb
		}
		return a.ID < b.ID
	})
	// Tasks, newest first.
	for _, t := range h.tasks {
		nt := *t
		p.Tasks = append(p.Tasks, &nt)
		switch t.State {
		case "queued", "assigned", "en_route", "on_station":
			p.KPI.LooksOpen++
		case "done":
			p.KPI.LooksDone++
		}
	}
	sort.Slice(p.Tasks, func(i, j int) bool { return p.Tasks[i].ID > p.Tasks[j].ID })
	for _, t := range h.teams {
		nt := *t
		p.Teams = append(p.Teams, &nt)
		switch t.State {
		case "en_route":
			p.KPI.OnTheWay++
		case "on_scene":
			p.KPI.OnScene++
		case "idle":
			p.KPI.TeamsFree++
		case "offline":
			p.KPI.TeamsOff++
		}
	}
	p.Waiting = append([]string(nil), h.waiting...)
	p.KPI.Waiting = len(h.waiting)
	if h.tak != nil {
		t := *h.tak
		p.TAK = &t
	}
	for _, d := range h.dets {
		nd := *d
		p.Detections = append(p.Detections, &nd)
	}
	p.KPI.Detections, p.KPI.Bytes = h.detTotal, h.detBytes
	tl := h.timeline
	if len(tl) > snapTimeline {
		tl = tl[len(tl)-snapTimeline:]
	}
	p.Timeline = append([]*Item(nil), tl...)
	p.Chats = append([]*Chat(nil), h.chats...)
	if p.Drones == nil {
		p.Drones = []*Drone{}
	}
	if p.Casualties == nil {
		p.Casualties = []*Casualty{}
	}
	if p.Tasks == nil {
		p.Tasks = []*Task{}
	}
	if p.Teams == nil {
		p.Teams = []*Team{}
	}
	if p.Assignments == nil {
		p.Assignments = []*Assignment{}
	}
	if p.Detections == nil {
		p.Detections = []*Det{}
	}
	if p.Timeline == nil {
		p.Timeline = []*Item{}
	}
	if p.Chats == nil {
		p.Chats = []*Chat{}
	}
	return p
}

// urgency orders casualties: those needing a team first (Red, Yellow), then
// Grey (needs a look), Green, Black; rescued ones after the others of their colour.
func urgency(c *Casualty) int {
	r := map[string]int{"Red": 0, "Yellow": 2, "Grey": 4, "Green": 6, "Black": 8}[c.Triage]
	if c.Triage == "" {
		r = 9
	}
	if c.Rescued {
		r++
	}
	return r
}
