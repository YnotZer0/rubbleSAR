package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"rubblesar/internal/aar"
	"rubblesar/internal/sarpb"
)

// Beat is a moment of a recorded run the demo director can seek to.
type Beat struct {
	Name string `json:"name"`
	At   int64  `json:"at"` // unix ms
	Ref  string `json:"ref,omitempty"`
	Text string `json:"text,omitempty"`
}

// Player replays a recorded run (a report folder's events.jsonl) into its
// own Hub, at any speed, from any moment, so the operator view shows the run
// as it happened (phase 8). Safe for concurrent use.
type Player struct {
	Dir   string
	Run   string
	Meta  aar.Meta
	Crops *Crops

	mu      sync.Mutex
	events  []aar.Event
	start   time.Time
	end     time.Time
	hub     *Hub
	triage  []string
	pos     time.Time
	posNs   atomic.Int64 // pos, for the hub's clock (read without p.mu)
	next    int
	speed   float64
	playing bool
	wallAt  time.Time
	posAt   time.Time
	stop    chan struct{}
	done    chan struct{}
	// What the events say so far.
	list    []aar.CasSnap
	detail  map[string]*aar.CasDetail
	tasks   map[string]*Task
	asgs    map[string]*Assignment
	waiting []string
	dirty   bool
	// For smooth movement between recorded positions.
	tel   map[string][]track
	teams []teamFrame
	beats []Beat
}

type track struct {
	t                  int64
	lat, lon, yaw, alt float64
}

type teamFrame struct {
	t   int64
	pos map[string][2]float64
}

// LoadRun reads a recorded run folder (meta.json, events.jsonl).
func LoadRun(dir string, triage []string) (*Player, error) {
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("E-AAR-002: %s is not a recorded run: %w", dir, err)
	}
	p := &Player{Dir: dir, Run: filepath.Base(dir), triage: triage, speed: 1, tel: map[string][]track{}}
	if err := json.Unmarshal(b, &p.Meta); err != nil {
		return nil, fmt.Errorf("E-AAR-002: meta.json: %w", err)
	}
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("E-AAR-002: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var e aar.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.T == 0 {
			continue
		}
		p.events = append(p.events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("E-AAR-002: reading events: %w", err)
	}
	if len(p.events) == 0 {
		return nil, fmt.Errorf("E-AAR-002: %s has no events", dir)
	}
	sort.SliceStable(p.events, func(i, j int) bool { return p.events[i].T < p.events[j].T })
	p.start, p.end = time.UnixMilli(p.events[0].T), time.UnixMilli(p.events[len(p.events)-1].T)
	if !p.Meta.Started.IsZero() && p.Meta.Started.Before(p.start) {
		p.start = p.Meta.Started
	}
	if !p.Meta.Ended.IsZero() && p.Meta.Ended.After(p.end) {
		p.end = p.Meta.Ended
	}
	for _, e := range p.events {
		switch {
		case e.K == "tel" && e.Tel != nil && airborneOrHome(e.Tel):
			p.tel[e.Tel.UAV] = append(p.tel[e.Tel.UAV], track{e.T, e.Tel.Lat, e.Tel.Lon, e.Tel.Yaw, e.Tel.AltAGL})
		case e.K == "teams":
			tf := teamFrame{t: e.T, pos: map[string][2]float64{}}
			for _, t := range e.Teams {
				tf.pos[t.ID] = [2]float64{t.Lat, t.Lon}
			}
			p.teams = append(p.teams, tf)
		case e.K == "beat":
			p.beats = append(p.beats, Beat{Name: e.Beat, At: e.T, Ref: e.Ref, Text: e.Note})
		}
	}
	p.beats = append(p.beats, p.derivedBeats()...)
	sort.SliceStable(p.beats, func(i, j int) bool { return p.beats[i].At < p.beats[j].At })
	p.Crops = &Crops{Dir: dir}
	p.reset()
	return p, nil
}

func airborneOrHome(t *aar.TelSnap) bool { return t.Lat != 0 || t.Lon != 0 }

// derivedBeats are the moments every recording has, found in its events:
// the first casualty, the first Red, the first close look sent and done, the
// first team sent, the first sent to a TAK team, the first rescue, the end of
// the survey, and drones going silent (a blackout) and back.
func (p *Player) derivedBeats() []Beat {
	var out []Beat
	have := map[string]bool{}
	for _, b := range p.beats {
		have[b.Name] = true
	}
	add := func(name string, at int64, ref, text string) {
		if !have[name] {
			have[name] = true
			out = append(out, Beat{Name: name, At: at, Ref: ref, Text: text})
		}
	}
	teamSrc := map[string]string{}
	for _, e := range p.events {
		for _, t := range e.Teams {
			teamSrc[t.ID] = t.Source
		}
	}
	lastTel := map[string]int64{}
	for _, e := range p.events {
		switch e.K {
		case "cas":
			for _, c := range e.Cas {
				add("first-casualty", e.T, c.ID, c.ID+" found")
				if c.Triage == "Red" {
					add("first-red", e.T, c.ID, c.ID+" Red")
				}
				if c.Triage == "Yellow" {
					add("first-yellow", e.T, c.ID, c.ID+" Yellow")
				}
				if c.Triage == "Black" {
					add("first-black", e.T, c.ID, c.ID+" Black")
				}
			}
		case "task":
			if t := e.Task; t != nil {
				if t.State == "assigned" || t.State == "en_route" {
					add("first-look", e.T, t.Cas, t.UAV+" sent to "+t.Cas)
				}
				if t.State == "on_station" {
					add("first-hover", e.T, t.Cas, t.UAV+" over "+t.Cas)
				}
				if t.State == "done" {
					add("first-look-done", e.T, t.Cas, "close look at "+t.Cas+" done")
					if t.Moved {
						add("first-moved", e.T, t.Cas, t.Cas+" moved")
					}
				}
			}
		case "asg":
			var a sarpb.Assignment
			if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(e.Asg, &a) != nil {
				continue
			}
			switch a.State {
			case sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE:
				add("first-team", e.T, a.CasualtyId, a.TeamId+" sent to "+a.CasualtyId)
				if teamSrc[a.TeamId] == "tak" {
					add("first-tak-team", e.T, a.CasualtyId, a.TeamId+" (TAK) sent to "+a.CasualtyId)
				}
			case sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
				add("first-on-scene", e.T, a.CasualtyId, a.TeamId+" at "+a.CasualtyId)
			case sarpb.AssignmentState_ASSIGNMENT_STATE_DONE:
				add("first-freed", e.T, a.CasualtyId, a.CasualtyId+" freed")
			}
		case "chat":
			if c := e.Chat; c != nil && c.From != "" && !strings.EqualFold(c.From, p.opsCallsign()) {
				add("first-tak-reply", e.T, "", c.From+": "+c.Text)
			}
		case "tel":
			t := e.Tel
			if t == nil {
				continue
			}
			if t.Done && t.Role == "scout" {
				add("survey-done", e.T, t.UAV, t.UAV+" finished its survey")
			}
			if t.State == "survey" {
				add("survey", e.T, t.UAV, t.UAV+" surveying")
			}
			if prev, ok := lastTel[t.UAV]; ok && e.T-prev >= int64(silentAfter/time.Millisecond) {
				add("outage", prev+int64(silentAfter/time.Millisecond), t.UAV, t.UAV+" link lost")
				add("outage-end", e.T, t.UAV, t.UAV+" heard again")
			}
			lastTel[t.UAV] = e.T
		}
	}
	return out
}

func (p *Player) opsCallsign() string { return "SAR-OPS" }

// reset starts the replay over (an empty picture at the start of the run).
func (p *Player) reset() {
	// The hub calls its clock with its own lock held, and the player calls
	// the hub with the player's lock held: the clock must not take p.mu.
	clock := func() time.Time { return time.Unix(0, p.posNs.Load()) }
	if p.hub == nil {
		p.hub = NewHub(HubOptions{Incident: p.Meta.Incident, Mode: "replay", Triage: p.triage, Clock: clock})
	} else {
		p.hub.Reset()
	}
	p.hub.SetBus("replay of a recorded run")
	p.hub.SetStart(p.start, p.Run)
	p.setPos(p.start)
	p.next = 0
	p.list, p.detail, p.tasks, p.asgs, p.waiting = nil, map[string]*aar.CasDetail{}, map[string]*Task{}, map[string]*Assignment{}, nil
}

// Start and End are the run's first and last moments.
func (p *Player) Start() time.Time { return p.start }
func (p *Player) End() time.Time   { return p.end }

// Beats lists the moments to seek to.
func (p *Player) Beats() []Beat { return append([]Beat(nil), p.beats...) }

// Beat finds a moment by name ("first-red", "+12:30", "end").
func (p *Player) Beat(name string) (time.Time, bool) {
	name = strings.TrimSpace(name)
	switch name {
	case "", "start":
		return p.start, true
	case "end":
		return p.end, true
	}
	if strings.HasPrefix(name, "+") {
		if d, ok := ParseClock(name[1:]); ok {
			return p.start.Add(d), true
		}
		return time.Time{}, false
	}
	// "first-red+30s", "outage-10s"
	base, off := name, time.Duration(0)
	if i := strings.LastIndexAny(name, "+-"); i > 0 {
		if d, err := time.ParseDuration(name[i+1:]); err == nil {
			base, off = name[:i], d
			if name[i] == '-' {
				off = -d
			}
		}
	}
	for _, b := range p.beats {
		if b.Name == base {
			return time.UnixMilli(b.At).Add(off), true
		}
	}
	return time.Time{}, false
}

// ParseClock reads "mm:ss" or "h:mm:ss".
func ParseClock(s string) (time.Duration, bool) {
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, x := range parts {
		n := 0
		if x == "" {
			return 0, false
		}
		for _, c := range x {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		total = total*60 + n
	}
	return time.Duration(total) * time.Second, true
}

// Seek moves the replay to t (and keeps playing if it was).
func (p *Player) Seek(t time.Time) {
	p.mu.Lock()
	if t.Before(p.start) {
		t = p.start
	}
	if t.After(p.end) {
		t = p.end
	}
	back := t.Before(p.pos)
	p.mu.Unlock()
	if back {
		p.mu.Lock()
		p.reset()
		p.mu.Unlock()
	}
	p.advance(t)
	p.mu.Lock()
	p.wallAt, p.posAt = time.Now(), p.pos
	p.mu.Unlock()
	p.publish()
}

// Play plays from where the replay is at speed (1 = real time).
func (p *Player) Play(speed float64) {
	if speed <= 0 {
		speed = 1
	}
	p.mu.Lock()
	p.speed, p.playing = speed, true
	p.wallAt, p.posAt = time.Now(), p.pos
	if p.stop == nil {
		p.stop, p.done = make(chan struct{}), make(chan struct{})
		go p.loop(p.stop, p.done)
	}
	p.mu.Unlock()
	p.publish()
}

// Pause holds the replay where it is.
func (p *Player) Pause() {
	p.mu.Lock()
	p.playing = false
	p.mu.Unlock()
	p.publish()
}

// Close stops the replay's clock.
func (p *Player) Close() {
	p.mu.Lock()
	stop, done := p.stop, p.done
	p.stop, p.done, p.playing = nil, nil, false
	p.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
}

func (p *Player) loop(stop, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(40 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		p.mu.Lock()
		playing := p.playing
		target := p.posAt.Add(time.Duration(float64(time.Since(p.wallAt)) * p.speed))
		p.mu.Unlock()
		if !playing {
			continue
		}
		if !target.Before(p.end) {
			target = p.end
		}
		p.advance(target)
		p.mu.Lock()
		if !p.pos.Before(p.end) {
			p.playing = false
		}
		p.mu.Unlock()
		p.publish()
	}
}

// Info is where the replay stands.
func (p *Player) Info() *Replay {
	p.mu.Lock()
	defer p.mu.Unlock()
	title := p.Meta.Suite
	return &Replay{Run: p.Run, Title: title, Start: ms(p.start), End: ms(p.end), At: ms(p.pos), Speed: p.speed, Playing: p.playing, Finished: !p.pos.Before(p.end)}
}

func (p *Player) publish() { p.hub.SetReplay(p.Info()) }

// Position is the replay's current moment.
func (p *Player) Position() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pos
}

// advance applies the events up to t.
func (p *Player) advance(t time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tm := t.UnixMilli()
	for p.next < len(p.events) && p.events[p.next].T <= tm {
		e := p.events[p.next]
		p.next++
		at := time.UnixMilli(e.T)
		p.setPos(at)
		p.hub.Tick(at)
		p.applyLocked(e, at)
	}
	if p.dirty {
		p.flushListLocked(t)
	}
	if t.After(p.pos) {
		p.setPos(t)
	}
	p.hub.Tick(p.pos)
}

// setPos moves the replay's position (with p.mu held, or before the player is shared).
func (p *Player) setPos(t time.Time) {
	p.pos = t
	p.posNs.Store(t.UnixNano())
}

func (p *Player) applyLocked(e aar.Event, at time.Time) {
	switch e.K {
	case "cas":
		p.list = e.Cas
		p.flushListLocked(at)
	case "casd":
		if e.CasD != nil {
			p.detail[e.CasD.ID] = e.CasD
			p.dirty = true
		}
	case "merge":
		if len(e.Merge) == 2 {
			p.hub.SetMerged(e.Merge[0], e.Merge[1])
		}
	case "task":
		if t := e.Task; t != nil {
			nt := &Task{ID: t.ID, Casualty: t.Cas, State: t.State, Tier: t.Tier, Reason: t.Reason, UAV: t.UAV, Lat: t.Lat, Lon: t.Lon, Found: t.Found, Moved: t.Moved}
			if old := p.tasks[t.ID]; old != nil {
				nt.Created = old.Created
			} else {
				nt.Created = e.T
			}
			if t.State == "done" || t.State == "aborted" || t.State == "failed" || t.State == "cancelled" {
				nt.Finished = e.T
			}
			p.tasks[t.ID] = nt
			list := make([]*Task, 0, len(p.tasks))
			for _, x := range p.tasks {
				list = append(list, x)
			}
			p.hub.SetTasks(list, at)
		}
	case "tel":
		if t := e.Tel; t != nil {
			p.hub.SetDrone(droneFromSnap(t, e.T), at)
		}
	case "asg":
		var a sarpb.Assignment
		if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(e.Asg, &a) == nil {
			na := FromAssignment(&a)
			p.asgs[na.ID] = na
			p.hub.SetDispatch(Dispatch{Assignments: []*Assignment{na}, Waiting: p.waiting}, at)
		}
	case "teams":
		teams := make([]*Team, 0, len(e.Teams))
		for _, t := range e.Teams {
			teams = append(teams, &Team{ID: t.ID, Callsign: t.Callsign, Mode: t.Mode, Source: t.Source, State: t.State, Lat: t.Lat, Lon: t.Lon,
				Course: t.Course, Casualty: t.Casualty, Done: t.Done, Seen: e.T})
		}
		p.waiting = e.Waiting
		p.hub.SetDispatch(Dispatch{Teams: teams, Waiting: e.Waiting}, at)
	case "chat":
		if c := e.Chat; c != nil {
			p.hub.AddChats([]*Chat{{At: c.At.UnixMilli(), From: c.From, To: c.To, Text: c.Text}})
		}
	case "det":
		if d := e.Det; d != nil {
			nd := &Det{ID: d.ID, UAV: d.UAV, Track: d.Track, Rev: d.Rev, Reason: d.Reason, Part: d.Part, Conf: d.Conf, Fused: d.Fused, Lat: d.Lat, Lon: d.Lon,
				CEP: d.CEP, Payload: d.Payload, Bytes: d.Bytes, Crop: d.Crop, Therm: d.Therm, At: e.T, Capture: d.Capture, Frame: d.Frame, RangeM: d.RangeM,
				Obs: d.Obs, Summary: d.Summary, Outline: d.Outline}
			if t := d.Thermal; t != nil {
				nd.Thermal = &Thermal{Readable: t.Readable, Warm: t.Warm, SkinC: t.Skin, AmbientC: t.Ambient, DeltaC: t.Delta}
			}
			p.hub.AddDetection(nd)
		}
	case "note":
		if e.Note != "" {
			p.hub.Note(at, "note", "info", e.Note)
		}
	}
}

func (p *Player) flushListLocked(at time.Time) {
	list := make([]*Casualty, 0, len(p.list))
	for _, c := range p.list {
		nc := &Casualty{ID: c.ID, Triage: c.Triage, Prov: c.Prov, Status: c.Status, Lat: c.Lat, Lon: c.Lon, CEP: c.CEP, UAVs: c.UAVs, First: c.First,
			Close: c.SeenClose, Reason: c.Reason, Summary: c.Summary}
		if d := p.detail[c.ID]; d != nil {
			nc.Conf, nc.PAlive, nc.Moved, nc.Watched, nc.Looks, nc.NeedsLook, nc.LookWhy, nc.Best, nc.Last = d.Conf, d.PAlive, d.Moved, d.Watched, d.Looks, d.NeedsLook, d.LookWhy, d.Best, d.Last
			if t := d.Thermal; t != nil {
				nc.Thermal = &Thermal{Readable: t.Readable, Warm: t.Warm, SkinC: t.Skin, AmbientC: t.Ambient, DeltaC: t.Delta, Readings: t.Readings, Close: t.Close, TrendCH: t.Trend}
			}
			for _, x := range d.Parts {
				nc.Parts = append(nc.Parts, &Part{UAV: x.UAV, Track: x.Track, Det: x.Det, Class: x.Class, Conf: x.Conf, Obs: x.Obs})
			}
		}
		list = append(list, nc)
	}
	p.hub.SetCasualties(list, at)
	p.dirty = false
}

func droneFromSnap(t *aar.TelSnap, at int64) *Drone {
	d := &Drone{ID: t.UAV, Role: t.Role, State: t.State, Lat: t.Lat, Lon: t.Lon, Alt: t.AltAGL, Yaw: t.Yaw, Speed: t.Speed, Battery: t.Battery,
		FlightS: t.FlightS, Seen: at, Step: t.Step, Steps: t.Steps, Surveyed: t.Done, Software: t.Software, SimS: t.SimS,
		HomeLat: t.HomeLat, HomeLon: t.HomeLon, Survey: t.Survey}
	if t.Task != "" && t.TaskSt != "" {
		d.Task = &DroneTask{ID: t.Task, Casualty: t.TaskCas, State: t.TaskSt, Lat: t.TaskLat, Lon: t.TaskLon, HoverS: t.HoverS, Elapsed: t.Elapsed}
	}
	if l := t.Link; l != nil {
		d.Link = &Link{BudgetBps: l.Budget, UsedBps: l.Used, QueuedMsgs: l.Queued, QueuedBytes: l.QBytes, Unacked: l.Unacked, Degrade: l.Degrade, Probing: l.Probing,
			Acked: l.Acked, Resent: l.Resent, Discarded: l.Discarded, Dropped: l.Dropped, SentBytes: l.Sent}
	} else {
		d.Link = &Link{}
	}
	if pc := t.Perc; pc != nil {
		d.Perc = &Perc{Backend: pc.Backend, Model: pc.Model, Hz: pc.Hz, Frames: pc.Frames, SegMS: pc.Seg, ThermMS: pc.Therm, TrackMS: pc.Track, PackMS: pc.Pack,
			RenderMS: pc.Render, Tracks: pc.Tracks, Reports: t.Reports, Warm: pc.Warm}
	}
	return d
}

// --- Feed ---------------------------------------------------------------------------------------

// Snapshot is the picture at the replay's position, with the drones and teams
// moved smoothly between their recorded positions.
func (p *Player) Snapshot() *Picture {
	pic := p.hub.Snapshot()
	p.mu.Lock()
	pos := p.pos.UnixMilli()
	p.mu.Unlock()
	for _, d := range pic.Drones {
		tr := p.tel[d.ID]
		i := sort.Search(len(tr), func(i int) bool { return tr[i].t > pos })
		if i == 0 || i >= len(tr) {
			continue
		}
		a, b := tr[i-1], tr[i]
		if b.t-a.t > 10000 || b.t == a.t { // a gap: the drone was silent
			continue
		}
		k := float64(pos-a.t) / float64(b.t-a.t)
		d.Lat, d.Lon = round6(a.lat+(b.lat-a.lat)*k), round6(a.lon+(b.lon-a.lon)*k)
		d.Alt = math.Round((a.alt+(b.alt-a.alt)*k)*10) / 10
		dy := math.Mod(b.yaw-a.yaw+540, 360) - 180
		d.Yaw = math.Round(math.Mod(a.yaw+dy*k+360, 360)*10) / 10
	}
	i := sort.Search(len(p.teams), func(i int) bool { return p.teams[i].t > pos })
	if i > 0 && i < len(p.teams) {
		a, b := p.teams[i-1], p.teams[i]
		if b.t-a.t <= 15000 && b.t > a.t {
			k := float64(pos-a.t) / float64(b.t-a.t)
			for _, t := range pic.Teams {
				pa, oka := a.pos[t.ID]
				pb, okb := b.pos[t.ID]
				if oka && okb && math.Abs(pb[0]-pa[0]) < 0.01 && math.Abs(pb[1]-pa[1]) < 0.01 {
					t.Lat, t.Lon = round6(pa[0]+(pb[0]-pa[0])*k), round6(pa[1]+(pb[1]-pa[1])*k)
				}
			}
		}
	}
	return pic
}

// Subscribe is told about every change.
func (p *Player) Subscribe() (<-chan struct{}, func()) { return p.hub.Subscribe() }

// Seq is the change counter.
func (p *Player) Seq() uint64 { return p.hub.Seq() }

// Timeline is the replayed timeline so far.
func (p *Player) Timeline() []*Item { return p.hub.Timeline() }

// Frames are the recorded frames' truth (for rendering them again), by frame id.
func (p *Player) Frames() map[string]*aar.FrameSnap {
	out := map[string]*aar.FrameSnap{}
	for _, e := range p.events {
		if e.K == "frame" && e.Frame != nil {
			out[e.Frame.ID] = e.Frame
		}
	}
	return out
}

// Detections are every detection report of the run, in order.
func (p *Player) Detections() []*aar.DetSnap {
	var out []*aar.DetSnap
	for _, e := range p.events {
		if e.K == "det" && e.Det != nil {
			out = append(out, e.Det)
		}
	}
	return out
}

// Events are the recording's events (read-only).
func (p *Player) Events() []aar.Event { return p.events }
