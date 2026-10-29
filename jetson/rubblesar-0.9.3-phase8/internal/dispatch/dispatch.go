// Package dispatch is rescue-dispatch's service (phase 5). It sends rescue
// teams to casualties and shows the whole picture on the TAK map:
//
//   - reads casualty-analyst's casualties (sar/{incident}/casualties and
//     .../casualty/*), the drones' telemetry and mission-planner's tasks;
//   - gives each casualty that needs a team (Red first, then Yellow) the free
//     team with the quickest route, along streets clear of rubble;
//   - moves simulated teams along their routes; follows real teams (TAK users
//     with a listed callsign) by their position reports;
//   - publishes teams and assignments (sar/{incident}/dispatch) and sends the
//     TAK server (taky) casualty markers coloured by triage, drones,
//     investigation lines, team routes and chat messages.
//
// No assignment is taken back for a more urgent one: a team keeps the
// casualty it was sent to until it is done, unless that casualty stops
// needing a team (triage changed, record merged, gone).
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/casualty"
	"rubblesar/internal/config"
	"rubblesar/internal/cot"
	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/keys"
	"rubblesar/internal/msg"
	"rubblesar/internal/roads"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
)

// Config configures the service.
type Config struct {
	Incident     string
	Source       string
	Centre       geo.Point
	GroundAltMSL float64
	Staging      geo.Point
	D            config.Dispatch
	// Roads is the street network (nil: straight lines, a warning in the status).
	Roads     *roads.Network
	RoadsPath string
	// Blockages are the rubble spill areas (local metres); already applied to Roads.
	Blockages []geom.Poly
	// Scene is the scenario, for how each victim is trapped (nil: no
	// collapse known, every extrication is a light rescue).
	Scene *scenario.Scenario
	// SiteNames: each collapse site named after its nearest street (by LoadMap).
	SiteNames map[string]string
	// StateFile keeps finished casualties across restarts ("" = not kept).
	StateFile string
	// Tick: how often teams move and assignments are made (default 1 s).
	Tick time.Duration
}

// FromZBook builds the service config; roads and blockages are loaded by Load.
func FromZBook(c *config.ZBook, source string) Config {
	lat, lon := c.Staging()
	return Config{Incident: c.Site.IncidentID, Source: source, Centre: geo.Point{Lat: c.Site.CenterLat, Lon: c.Site.CenterLon},
		GroundAltMSL: c.Site.GroundAltMSLm, Staging: geo.Point{Lat: lat, Lon: lon}, D: c.Dispatch, RoadsPath: c.RoadsPath(),
		StateFile: filepath.Join(c.Dispatch.StateDir, c.Site.IncidentID+".json")}
}

// LoadMap loads the street file and the scenario's rubble spill areas and
// closes the streets they cover. A missing street file is not an error
// (warn is set): routes are then straight lines.
func LoadMap(cfg *Config, scenarioDir string) (warn []string, err error) {
	local := geo.NewLocal(cfg.Centre)
	if scenarioDir != "" {
		s, serr := scenario.Load(filepath.Join(scenarioDir, "scenario.json"))
		if serr != nil {
			warn = append(warn, "no rubble areas known (scenario not readable: "+serr.Error()+")")
		} else {
			cfg.Blockages = Blockages(s, local)
			cfg.Scene = s
		}
	}
	if cfg.RoadsPath != "" {
		if _, serr := os.Stat(cfg.RoadsPath); serr != nil {
			warn = append(warn, "no street file at "+cfg.RoadsPath+" (routes are straight lines): run scripts/zbook/32-roads.sh")
		} else {
			n, lerr := roads.Load(cfg.RoadsPath, local)
			if lerr != nil {
				return warn, lerr
			}
			n.Block(cfg.Blockages, cfg.D.BlockMarginM)
			cfg.Roads = n
		}
	}
	if cfg.Scene != nil {
		centres := map[string]geom.P{}
		for _, st := range cfg.Scene.Sites {
			x, y := local.ToLocal(geo.Point{Lat: st.Center.Lat, Lon: st.Center.Lon})
			centres[st.ID] = geom.P{X: x, Y: y}
		}
		cfg.SiteNames = cfg.Roads.SiteNames(centres, 120)
	}
	return warn, nil
}

// Blockages converts the scenario's rubble spill polygons to local metres.
func Blockages(s *scenario.Scenario, local geo.Local) []geom.Poly {
	var out []geom.Poly
	for _, b := range s.Blockages {
		p := make(geom.Poly, len(b.Polygon))
		for i, v := range b.Polygon {
			x, y := local.ToLocal(geo.Point{Lat: v.Lat, Lon: v.Lon})
			p[i] = geom.P{X: x, Y: y}
		}
		if len(p) >= 3 {
			out = append(out, p)
		}
	}
	return out
}

type team struct {
	cfg           config.Team
	mode          roads.Mode
	pos           geom.P
	course, speed float64
	state         sarpb.TeamState
	asg           *assignment
	uid           string // TAK uid (tak teams)
	lastSeen      time.Time
	done          uint32
	sentAt        time.Time // last PLI sent (sim teams)
	unable        map[string]bool
}

func (t *team) sim() bool { return t.cfg.Source == "sim" }

type assignment struct {
	pb      *sarpb.Assignment
	team    *team
	route   *roads.Route
	startAt time.Time // when the team set off along route
	target  geom.P
	sceneAt time.Time
	// On scene: how long freeing the casualty takes, and when the team is free.
	ext    extrication
	freeAt time.Time
	// TAK teams: answered or moved towards the casualty (see tak_ack_timeout).
	acked     bool
	startDist float64
}

// ackMoveM: a TAK team this much closer to its casualty is on its way.
const ackMoveM = 20.0

func (a *assignment) active() bool {
	s := a.pb.State
	return s == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE || s == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE
}

type casInfo struct {
	c      *sarpb.Casualty
	first  time.Time
	needed time.Time
	pos    geom.P
	// TAK marker bookkeeping.
	sentKey string
	sentAt  time.Time
}

type droneInfo struct {
	t      *sarpb.Telemetry
	at     time.Time
	pos    geom.P
	sentAt time.Time
	sentP  geom.P
}

type takUser struct {
	uid, callsign string
	at            time.Time
	pos           geom.P
}

// ChatLine is one message sent or received.
type ChatLine struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
	Text string    `json:"text"`
}

// Service is safe for concurrent use.
type Service struct {
	cfg    Config
	b      bus.Bus
	log    *slog.Logger
	local  geo.Local
	router *roads.Router
	tak    *cot.Client
	// One TAK connection per simulated team, like separate devices (a TAK
	// server knows each connection by the callsign of its position reports).
	teamTAK map[string]*cot.Client
	pub     bus.Publisher
	subs    []bus.Subscription
	uid     string // our TAK uid
	stage   geom.P
	scene   *sceneModel

	mu        sync.Mutex
	run       string
	next      int
	teams     []*team
	asg       map[string]*assignment
	order     []string
	cas       map[string]*casInfo
	casAt     time.Time
	lists     uint64
	handled   map[string]string // casualty id -> team that finished there
	noRoute   map[string]string // casualty id -> why no team can reach it (chat sent once)
	drones    map[string]*droneInfo
	tasks     *sarpb.TaskList
	invLines  map[string]bool // investigation line uids on the map
	gone      map[string]bool // casualty markers to delete
	users     map[string]*takUser
	chats     []ChatLine
	opsSentAt time.Time
	seq       msg.Seq
	dirty     bool
	resets    int
	refreshAt time.Time
}

// New makes a service; Open subscribes it.
func New(cfg Config, b bus.Bus, log *slog.Logger) *Service {
	if cfg.Tick <= 0 {
		cfg.Tick = time.Second
	}
	if cfg.Source == "" {
		cfg.Source = "zbook/rescue-dispatch"
	}
	if cfg.D.TimeScale <= 0 {
		cfg.D.TimeScale = 1
	}
	if cfg.D.RescueTimeScale <= 0 {
		cfg.D.RescueTimeScale = cfg.D.TimeScale
	}
	if cfg.Staging == (geo.Point{}) {
		cfg.Staging = cfg.Centre
	}
	local := geo.NewLocal(cfg.Centre)
	s := &Service{cfg: cfg, b: b, log: log, local: local, uid: "rubblesar." + cfg.Incident + ".ops", run: runToken(time.Now())}
	s.stage.X, s.stage.Y = local.ToLocal(cfg.Staging)
	s.scene = newSceneModel(cfg.Scene, local)
	if cfg.Roads != nil {
		s.router = roads.NewRouter(cfg.Roads)
	}
	s.resetLocked()
	return s
}

func runToken(t time.Time) string {
	v := strconv.FormatInt(t.Unix()/2%46656, 36)
	return strings.Repeat("0", 3-len(v)) + v
}

// resetLocked forgets every assignment and puts the teams back at the staging area.
func (s *Service) resetLocked() {
	s.next, s.asg, s.order = 1, map[string]*assignment{}, nil
	s.cas, s.casAt, s.handled, s.noRoute = map[string]*casInfo{}, time.Time{}, map[string]string{}, map[string]string{}
	if s.drones == nil {
		s.drones = map[string]*droneInfo{}
	}
	if s.invLines == nil {
		s.invLines = map[string]bool{}
	}
	if s.gone == nil {
		s.gone = map[string]bool{}
	}
	if s.users == nil {
		s.users = map[string]*takUser{}
	}
	old := map[string]*team{}
	for _, t := range s.teams {
		old[t.cfg.ID] = t
	}
	s.teams = nil
	for i, tc := range s.cfg.D.Teams {
		t := &team{cfg: tc, mode: roads.Mode(tc.Mode), state: sarpb.TeamState_TEAM_STATE_IDLE, unable: map[string]bool{}}
		// Spread the simulated teams a few metres apart at the staging area.
		t.pos = s.stage.Add(geom.P{X: float64(i%4)*4 - 6, Y: float64(i/4) * 4})
		if !t.sim() {
			t.state = sarpb.TeamState_TEAM_STATE_OFFLINE
			if o := old[tc.ID]; o != nil {
				t.uid, t.lastSeen, t.pos = o.uid, o.lastSeen, o.pos
			}
		}
		s.teams = append(s.teams, t)
	}
	s.dirty = true
}

// Open loads the state file and subscribes to casualties, telemetry and tasks.
func Open(cfg Config, b bus.Bus, log *slog.Logger) (*Service, error) {
	s := New(cfg, b, log)
	if err := s.load(); err != nil {
		return nil, err
	}
	var err error
	if s.pub, err = b.DeclarePublisher(keys.Dispatch(cfg.Incident), bus.QoSCasualty); err != nil {
		return nil, err
	}
	for _, x := range []struct {
		key string
		h   bus.Handler
	}{{keys.Casualties(cfg.Incident), s.onList}, {keys.CasualtyAll(cfg.Incident), s.onCasualty},
		{keys.TelemetryAll(cfg.Incident), s.onTelemetry}, {keys.Tasks(cfg.Incident), s.onTasks}} {
		sub, err := b.Subscribe(x.key, x.h)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.subs = append(s.subs, sub)
	}
	if cfg.D.TakServer != "" {
		s.tak = cot.NewClient(cot.ClientConfig{Addr: cfg.D.TakServer, Hello: s.hello, OnEvent: s.onTAK, PingUID: s.uid, Log: log,
			OnState: func(up bool, err error) {
				s.mu.Lock()
				s.dirty = true
				s.mu.Unlock()
			}})
		s.teamTAK = map[string]*cot.Client{}
		quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
		for _, tc := range cfg.D.Teams {
			if tc.Source != "sim" {
				continue
			}
			id := tc.ID
			s.teamTAK[id] = cot.NewClient(cot.ClientConfig{Addr: cfg.D.TakServer, PingUID: s.prefix() + "team." + id, Log: quiet, QueueLen: 50,
				Hello: func(now time.Time) []*cot.Event {
					s.mu.Lock()
					defer s.mu.Unlock()
					for _, t := range s.teams {
						if t.cfg.ID == id {
							return []*cot.Event{s.teamLocked(t, now)}
						}
					}
					return nil
				}})
		}
	}
	return s, nil
}

type persisted struct {
	Run     string            `json:"run"`
	Next    int               `json:"next"`
	Handled map[string]string `json:"handled"`
	Done    map[string]uint32 `json:"team_done"`
}

func (s *Service) load() error {
	if s.cfg.StateFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.cfg.StateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return diag.Wrap("E-DSP-002", err, "reading %s", s.cfg.StateFile)
	}
	var st persisted
	if err := json.Unmarshal(b, &st); err != nil {
		return diag.Wrap("E-DSP-002", err, "reading %s", s.cfg.StateFile)
	}
	if st.Run != "" {
		s.run = st.Run
	}
	s.next = max(st.Next, 1)
	for k, v := range st.Handled {
		s.handled[k] = v
	}
	for _, t := range s.teams {
		t.done = st.Done[t.cfg.ID]
	}
	return nil
}

func (s *Service) saveLocked() {
	if s.cfg.StateFile == "" {
		return
	}
	st := persisted{Run: s.run, Next: s.next, Handled: s.handled, Done: map[string]uint32{}}
	for _, t := range s.teams {
		st.Done[t.cfg.ID] = t.done
	}
	b, _ := json.MarshalIndent(st, "", " ")
	if err := os.MkdirAll(filepath.Dir(s.cfg.StateFile), 0o755); err != nil {
		s.log.Warn("saving dispatch state", "err", diag.Wrap("E-DSP-002", err, "creating %s", filepath.Dir(s.cfg.StateFile)))
		return
	}
	tmp := s.cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		s.log.Warn("saving dispatch state", "err", diag.Wrap("E-DSP-002", err, "writing %s", tmp))
		return
	}
	_ = os.Rename(tmp, s.cfg.StateFile)
}

// --- Inputs -----------------------------------------------------------------

func (s *Service) onList(x bus.Sample) {
	var l sarpb.CasualtyList
	if msg.Unmarshal(x.Payload, &l) != nil || msg.CheckHeader(l.GetHeader(), s.cfg.Incident) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	seen := map[string]bool{}
	for _, c := range l.Casualties {
		seen[c.CasualtyId] = true
		s.updateCasLocked(c, now)
	}
	for id := range s.cas {
		if !seen[id] {
			delete(s.cas, id)
			s.gone[id] = true
		}
	}
	s.casAt = now
	s.lists++
}

func (s *Service) onCasualty(x bus.Sample) {
	var c sarpb.Casualty
	if msg.Unmarshal(x.Payload, &c) != nil || msg.CheckHeader(c.GetHeader(), s.cfg.Incident) != nil || c.CasualtyId == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.MergedInto != "" {
		s.mergedLocked(c.CasualtyId, c.MergedInto)
		return
	}
	s.updateCasLocked(&c, time.Now())
}

func (s *Service) updateCasLocked(c *sarpb.Casualty, now time.Time) {
	ci := s.cas[c.CasualtyId]
	if ci == nil {
		ci = &casInfo{first: now}
		s.cas[c.CasualtyId] = ci
		delete(s.gone, c.CasualtyId)
	}
	ci.c = c
	ci.pos.X, ci.pos.Y = s.local.ToLocal(geo.Point{Lat: c.GetPosition().GetLatDeg(), Lon: c.GetPosition().GetLonDeg()})
	if s.priority(c) > 0 && ci.needed.IsZero() {
		ci.needed = now
	}
}

// mergedLocked: casualty `from` is the same person as `into`. An assignment
// or a finished visit carries over.
func (s *Service) mergedLocked(from, into string) {
	if _, ok := s.cas[from]; ok {
		delete(s.cas, from)
	}
	s.gone[from] = true
	if t, ok := s.handled[from]; ok {
		if _, has := s.handled[into]; !has {
			s.handled[into] = t
		}
	}
	a := s.activeFor(from)
	if a == nil {
		return
	}
	if other := s.activeFor(into); other != nil || s.handled[into] != "" {
		s.cancelLocked(a, from+" was merged into "+into+", which already has a team", time.Now())
		return
	}
	a.pb.CasualtyId = into
	a.pb.Note = strings.TrimSpace(a.pb.Note + "; " + from + " merged into " + into)
	if a.team != nil {
		a.team.asg = a
	}
	s.dirty = true
	s.log.Info("assignment follows a merge", "assignment", a.pb.AssignmentId, "from", from, "into", into)
	if a.team != nil {
		s.chatLocked(fmt.Sprintf("%s: %s is the same person as %s; carry on to %s.", a.team.cfg.Callsign, from, into, into), a.team, time.Now())
		if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
			s.sendRouteLocked(a, time.Now()) // relabelled
		}
	}
}

func (s *Service) activeFor(casID string) *assignment {
	for _, id := range s.order {
		if a := s.asg[id]; a.pb.CasualtyId == casID && a.active() {
			return a
		}
	}
	return nil
}

func (s *Service) onTelemetry(x bus.Sample) {
	var t sarpb.Telemetry
	if msg.Unmarshal(x.Payload, &t) != nil || msg.CheckHeader(t.GetHeader(), s.cfg.Incident) != nil || t.UavId == "" || t.Position == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.drones[t.UavId]
	if d == nil {
		d = &droneInfo{}
		s.drones[t.UavId] = d
	}
	d.t, d.at = &t, time.Now()
	d.pos.X, d.pos.Y = s.local.ToLocal(geo.Point{Lat: t.Position.LatDeg, Lon: t.Position.LonDeg})
}

func (s *Service) onTasks(x bus.Sample) {
	var l sarpb.TaskList
	if msg.Unmarshal(x.Payload, &l) != nil || msg.CheckHeader(l.GetHeader(), s.cfg.Incident) != nil {
		return
	}
	s.mu.Lock()
	s.tasks = &l
	s.mu.Unlock()
}

// --- Dispatching -------------------------------------------------------------

// priority: 0 = needs no team; else 1 (first colour in [dispatch] triage,
// settled), 2 (first colour, provisional), 3 (second colour, settled)...
func (s *Service) priority(c *sarpb.Casualty) uint32 {
	name := strings.ToLower(casualty.TriageName(c.Triage))
	for i, t := range s.cfg.D.Triage {
		if strings.ToLower(t) == name {
			p := uint32(2*i + 1)
			if c.Provisional {
				p++
			}
			return p
		}
	}
	return 0
}

func (s *Service) toPoint(p geom.P) geo.Point { return s.local.ToPoint(p.X, p.Y) }

func (s *Service) geoPoint(p geom.P) *sarpb.GeoPoint {
	g := s.toPoint(p)
	return &sarpb.GeoPoint{LatDeg: g.Lat, LonDeg: g.Lon, AltMslM: s.cfg.GroundAltMSL}
}

// Step moves the teams, checks the assignments and makes new ones.
func (s *Service) Step(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moveTeamsLocked(now)
	s.checkAssignmentsLocked(now)
	s.assignLocked(now)
	s.mapLocked(now)
}

func (s *Service) moveTeamsLocked(now time.Time) {
	scale := s.cfg.D.TimeScale
	for _, t := range s.teams {
		if !t.sim() {
			online := !t.lastSeen.IsZero() && now.Sub(t.lastSeen) <= s.cfg.D.TeamOffline.Duration
			switch {
			case !online && t.state != sarpb.TeamState_TEAM_STATE_OFFLINE:
				if t.asg != nil && t.asg.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
					s.cancelLocked(t.asg, t.cfg.Callsign+" went offline (no position for "+s.cfg.D.TeamOffline.Duration.String()+")", now)
				}
				if t.asg == nil {
					t.state = sarpb.TeamState_TEAM_STATE_OFFLINE
					s.dirty = true
				}
			case online && t.state == sarpb.TeamState_TEAM_STATE_OFFLINE:
				t.state = sarpb.TeamState_TEAM_STATE_IDLE
				s.dirty = true
				s.log.Info("TAK team online", "team", t.cfg.ID, "callsign", t.cfg.Callsign)
			}
		}
		a := t.asg
		if a == nil {
			if t.sim() {
				t.speed = 0
			}
			continue
		}
		switch a.pb.State {
		case sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE:
			if t.sim() {
				el := now.Sub(a.startAt).Seconds() * scale
				p, hdg := a.route.At(el)
				if d := p.Dist(t.pos); d > 0 {
					t.speed = d / math.Max(s.cfg.Tick.Seconds(), 0.1) / scale
				}
				t.pos, t.course = p, hdg
				if el >= a.route.ETAs {
					t.pos = a.target
					s.onSceneLocked(a, now)
				}
			} else if d := t.pos.Dist(a.target); d <= s.cfg.D.ArriveM {
				s.onSceneLocked(a, now)
			} else if !a.acked {
				// A TAK team that neither answers nor sets off loses the casualty.
				if a.startDist-d >= ackMoveM {
					a.acked = true
				} else if to := s.cfg.D.TakAckTimeout.Duration; to > 0 && now.Sub(time.Unix(0, a.pb.CreatedUnixNs)) >= to {
					t.unable[a.pb.CasualtyId] = true
					s.cancelLocked(a, fmt.Sprintf("%s did not answer or move towards it within %s", t.cfg.Callsign, hmin(to)), now)
				}
			}
		case sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE:
			if !now.Before(a.freeAt) {
				s.doneLocked(a, "", now)
			}
		}
	}
}

func (s *Service) onSceneLocked(a *assignment, now time.Time) {
	t := a.team
	a.pb.State = sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE
	a.pb.OnSceneUnixNs = now.UnixNano()
	a.sceneAt = now
	t.state, t.speed = sarpb.TeamState_TEAM_STATE_ON_SCENE, 0
	s.dirty = true
	took := now.Sub(time.Unix(0, a.pb.CreatedUnixNs))
	if t.sim() {
		took = time.Duration(float64(took) * s.cfg.D.TimeScale)
	}
	// How long until the casualty is free and handed over (real time).
	if s.cfg.D.Extrication {
		if ci := s.cas[a.pb.CasualtyId]; ci != nil && s.priority(ci.c) == 0 {
			a.ext = confirmAndMark()
		} else {
			a.ext = s.scene.estimate(a.pb.CasualtyId, a.target, t.mode, s.run)
		}
	}
	need := a.ext.Dur + s.cfg.D.OnScene.Duration
	a.pb.ExtricationS = float32(need.Seconds())
	if a.ext.Class != "" {
		a.pb.Extrication = a.ext.Words()
	}
	if t.sim() {
		need = time.Duration(float64(need) / s.cfg.D.RescueTimeScale)
	}
	a.freeAt = now.Add(need)
	a.pb.FreeUnixNs = a.freeAt.UnixNano()
	s.log.Info("team on scene", "assignment", a.pb.AssignmentId, "team", t.cfg.ID, "casualty", a.pb.CasualtyId, "took", took.Round(time.Second),
		"extrication", a.ext.Class, "extrication_min", a.ext.Dur.Minutes(), "free_in", need.Round(time.Second))
	text := fmt.Sprintf("%s reached %s %s (%s after dispatch).", t.cfg.Callsign, a.pb.CasualtyId, s.says(a.pb.CasualtyId), mins(took))
	if a.ext.Class != "" {
		text += " " + upperFirst(a.ext.Words()) + "."
	}
	s.chatLocked(text, nil, now)
	if !t.sim() && t.uid != "" && a.ext.Class != "" {
		s.chatLocked(fmt.Sprintf("On scene at %s. Expect %s, then hand over. Reply \"done\" when they are free and handed over.", a.pb.CasualtyId, a.ext.Words()), t, now)
	}
	s.deleteLocked(s.routeUID(t), now)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (s *Service) doneLocked(a *assignment, why string, now time.Time) {
	t := a.team
	if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
		a.pb.OnSceneUnixNs = now.UnixNano()
		s.deleteLocked(s.routeUID(t), now)
	}
	a.pb.State = sarpb.AssignmentState_ASSIGNMENT_STATE_DONE
	a.pb.DoneUnixNs = now.UnixNano()
	if why != "" {
		a.pb.Note = strings.TrimSpace(a.pb.Note + "; " + why)
	}
	s.handled[a.pb.CasualtyId] = t.cfg.ID
	t.asg, t.done = nil, t.done+1
	t.state = sarpb.TeamState_TEAM_STATE_IDLE
	s.dirty = true
	s.saveLocked()
	s.log.Info("team done", "assignment", a.pb.AssignmentId, "team", t.cfg.ID, "casualty", a.pb.CasualtyId)
	text := fmt.Sprintf("%s finished at %s; free for the next casualty.", t.cfg.Callsign, a.pb.CasualtyId)
	switch {
	case why != "" || a.ext.Class == "":
	case a.ext.Class == confirmAndMark().Class:
		text = fmt.Sprintf("%s finished at %s: marked for recovery; free for the next casualty.", t.cfg.Callsign, a.pb.CasualtyId)
	default:
		text = fmt.Sprintf("%s finished at %s: freed after %s and handed over; free for the next casualty.", t.cfg.Callsign, a.pb.CasualtyId, hmin(a.ext.Dur))
	}
	s.chatLocked(text, nil, now)
}

func (s *Service) cancelLocked(a *assignment, why string, now time.Time) {
	t := a.team
	a.pb.State = sarpb.AssignmentState_ASSIGNMENT_STATE_CANCELLED
	a.pb.DoneUnixNs = now.UnixNano()
	a.pb.Note = strings.TrimSpace(a.pb.Note + "; cancelled: " + why)
	if t != nil && t.asg == a {
		t.asg = nil
		t.state, t.speed = sarpb.TeamState_TEAM_STATE_IDLE, 0
		s.deleteLocked(s.routeUID(t), now)
	}
	s.dirty = true
	s.log.Info("assignment cancelled", "assignment", a.pb.AssignmentId, "casualty", a.pb.CasualtyId, "why", why)
	if t != nil {
		s.chatLocked(fmt.Sprintf("%s: %s cancelled (%s). Stand by.", t.cfg.Callsign, a.pb.CasualtyId, why), t, now)
	}
}

// checkAssignmentsLocked: casualties that changed under an assignment.
func (s *Service) checkAssignmentsLocked(now time.Time) {
	if s.casAt.IsZero() {
		return
	}
	for _, id := range s.order {
		a := s.asg[id]
		if !a.active() {
			continue
		}
		ci := s.cas[a.pb.CasualtyId]
		if ci == nil {
			if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
				s.cancelLocked(a, a.pb.CasualtyId+" is no longer listed", now)
			}
			continue
		}
		a.pb.TriageNow = ci.c.Triage
		if s.priority(ci.c) == 0 && a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
			s.cancelLocked(a, "now "+strings.ToUpper(casualty.TriageName(ci.c.Triage))+": "+ci.c.Reason, now)
			continue
		}
		// The casualty's position firmed up: route again from where the team is.
		if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE && ci.pos.Dist(a.target) > 5 {
			if r, err := s.routeLocked(a.team, ci.pos, nil); err == nil {
				a.route, a.startAt, a.target = r, now, ci.pos
				a.pb.Route, a.pb.Target = s.routePB(r), s.geoPoint(ci.pos)
				a.pb.Note = strings.TrimSpace(a.pb.Note + "; re-routed (position moved)")
				s.dirty = true
				s.sendRouteLocked(a, now)
			}
		}
	}
}

// waitingLocked lists casualties that need a team and have none, most urgent first.
func (s *Service) waitingLocked(now time.Time) []*casInfo {
	var out []*casInfo
	for id, ci := range s.cas {
		if s.priority(ci.c) == 0 || s.handled[id] != "" || s.activeFor(id) != nil || now.Sub(ci.first) < s.cfg.D.MinAge.Duration {
			continue
		}
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool {
		pi, pj := s.priority(out[i].c), s.priority(out[j].c)
		if pi != pj {
			return pi < pj
		}
		if !out[i].needed.Equal(out[j].needed) {
			return out[i].needed.Before(out[j].needed)
		}
		return out[i].c.CasualtyId < out[j].c.CasualtyId
	})
	return out
}

// routeLocked finds the team's route to `to` (trees caches searches per team this step).
func (s *Service) routeLocked(t *team, to geom.P, trees map[*team]*roads.Tree) (*roads.Route, error) {
	if s.router == nil {
		d := t.pos.Dist(to)
		sp := roads.DefaultSpeeds().OffRoadMps
		return &roads.Route{Mode: t.mode, Points: []geom.P{t.pos, to}, T: []float64{0, d / sp}, AccessIdx: 0, Access: t.pos,
			LengthM: d, OffRoadM: d, ETAs: d / sp, DirectM: d}, nil
	}
	tr := trees[t]
	if tr == nil {
		var err error
		if tr, err = s.router.From(t.mode, t.pos); err != nil {
			return nil, err
		}
		if trees != nil {
			trees[t] = tr
		}
	}
	return tr.To(to)
}

// assignLocked gives waiting casualties free teams: the most urgent tier
// first, and within it the pair with the quickest route.
func (s *Service) assignLocked(now time.Time) {
	var free []*team
	for _, t := range s.teams {
		if t.asg == nil && t.state == sarpb.TeamState_TEAM_STATE_IDLE {
			free = append(free, t)
		}
	}
	if len(free) == 0 {
		return
	}
	waiting := s.waitingLocked(now)
	trees := map[*team]*roads.Tree{}
	nFree := uint32(len(free))
	// skipped: more urgent casualties no free team could take, and why (the
	// next assignment, of a less urgent one, says so in its note).
	var skipped []string
	for len(free) > 0 && len(waiting) > 0 {
		p := s.priority(waiting[0].c)
		var tier, rest []*casInfo
		for _, ci := range waiting {
			if s.priority(ci.c) == p {
				tier = append(tier, ci)
			} else {
				rest = append(rest, ci)
			}
		}
		type pick struct {
			t  *team
			ci *casInfo
			r  *roads.Route
		}
		var best *pick
		why := map[string]string{}
		for _, ci := range tier {
			for _, t := range free {
				if t.unable[ci.c.CasualtyId] {
					continue
				}
				r, err := s.routeLocked(t, ci.pos, trees)
				if err != nil {
					why[ci.c.CasualtyId] = err.Error()
					continue
				}
				if best == nil || r.ETAs < best.r.ETAs {
					best = &pick{t, ci, r}
				}
			}
		}
		if best == nil {
			// No free team can reach this tier: say so once each, try the next tier.
			for _, ci := range tier {
				id := ci.c.CasualtyId
				if w, ok := why[id]; ok && s.noRoute[id] == "" {
					s.noRoute[id] = w
					s.log.Warn("no route to a casualty", "casualty", id, "err", w)
					s.chatLocked(fmt.Sprintf("%s %s: no team can reach it by street (%s). Needs another way in.", id, s.says(id), plain(w)), nil, now)
				}
				reason := "every free team has declined it"
				if w, ok := why[id]; ok {
					reason = "no free team can reach it (" + plain(w) + ")"
				}
				skipped = append(skipped, fmt.Sprintf("%s %s: %s", id, s.says(id), reason))
			}
			s.log.Info("more urgent casualties skipped", "tier", p, "why", strings.Join(skipped, "; "))
			waiting = rest
			continue
		}
		delete(s.noRoute, best.ci.c.CasualtyId)
		s.newAssignmentLocked(best.t, best.ci, best.r, p, nFree, now)
		if len(skipped) > 0 && best.t.asg != nil {
			best.t.asg.pb.Note = strings.TrimPrefix(best.t.asg.pb.Note+"; more urgent, but not possible now: "+strings.Join(skipped, "; "), "; ")
		}
		var nf []*team
		for _, t := range free {
			if t != best.t {
				nf = append(nf, t)
			}
		}
		free = nf
		var nw []*casInfo
		for _, ci := range waiting {
			if ci != best.ci {
				nw = append(nw, ci)
			}
		}
		waiting = nw
	}
}

func (s *Service) newAssignmentLocked(t *team, ci *casInfo, r *roads.Route, prio, nFree uint32, now time.Time) {
	c := ci.c
	id := fmt.Sprintf("A-%s-%03d", s.run, s.next)
	s.next++
	a := &assignment{team: t, route: r, startAt: now, target: ci.pos, startDist: t.pos.Dist(ci.pos), acked: t.sim(), pb: &sarpb.Assignment{AssignmentId: id, CasualtyId: c.CasualtyId, TeamId: t.cfg.ID,
		State: sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE, Triage: c.Triage, Provisional: c.Provisional, TriageNow: c.Triage, Priority: prio,
		Target: s.geoPoint(ci.pos), Route: s.routePB(r), CreatedUnixNs: now.UnixNano(), NeededUnixNs: ci.needed.UnixNano(), FreeTeams: nFree}}
	if s.router == nil {
		a.pb.Note = "no street file: straight line"
	}
	s.asg[id] = a
	s.order = append(s.order, id)
	t.asg = a
	t.state = sarpb.TeamState_TEAM_STATE_EN_ROUTE
	s.dirty = true
	via := ""
	if len(r.Streets) > 0 {
		via = " via " + strings.Join(r.Streets[:min(3, len(r.Streets))], ", ")
	}
	s.log.Info("team dispatched", "assignment", id, "team", t.cfg.ID, "casualty", c.CasualtyId, "triage", casualty.TriageName(c.Triage),
		"provisional", c.Provisional, "length_m", math.Round(r.LengthM), "eta_s", math.Round(r.ETAs), "free_teams", nFree)
	s.chatLocked(fmt.Sprintf("%s → %s %s: %.0f m%s, about %s.", t.cfg.Callsign, c.CasualtyId, s.says(c.CasualtyId), r.LengthM, via, mins(time.Duration(r.ETAs*float64(time.Second)))), nil, now)
	if !t.sim() && t.uid != "" {
		g := s.toPoint(ci.pos)
		s.chatLocked(fmt.Sprintf("Go to %s %s at %.5f, %.5f: %.0f m, about %s. Your route is on the map. %s Reply \"done\" when finished there, \"unable\" if you cannot go.",
			c.CasualtyId, s.says(c.CasualtyId), g.Lat, g.Lon, r.LengthM, mins(time.Duration(r.ETAs*float64(time.Second))), firstSentence(c.Summary)), t, now)
	}
	s.sendRouteLocked(a, now)
}

// plain drops the code in front of an error message ("E-RTE-003: ...").
func plain(s string) string {
	if strings.HasPrefix(s, "E-") {
		if i := strings.Index(s, ": "); i > 0 && i < 16 {
			return s[i+2:]
		}
	}
	return s
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, ". "); i > 0 {
		s = s[:i+1]
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// label is "RED", "RED?" (provisional) etc., for map labels.
func (s *Service) label(casID string) string {
	ci := s.cas[casID]
	if ci == nil {
		return ""
	}
	l := strings.ToUpper(casualty.TriageName(ci.c.Triage))
	if ci.c.Provisional {
		l += "?"
	}
	return l
}

// says is "RED", "RED (provisional)" etc., for messages.
func (s *Service) says(casID string) string {
	l := s.label(casID)
	if strings.HasSuffix(l, "?") {
		return strings.TrimSuffix(l, "?") + " (provisional)"
	}
	return l
}

func mins(d time.Duration) string {
	if d < 90*time.Second {
		return fmt.Sprintf("%.0f s", d.Seconds())
	}
	return fmt.Sprintf("%.0f min", math.Round(d.Minutes()))
}

func (s *Service) routePB(r *roads.Route) *sarpb.RoutePlan {
	p := &sarpb.RoutePlan{Mode: string(r.Mode), AccessIndex: uint32(r.AccessIdx), LengthM: float32(r.LengthM), RoadM: float32(r.RoadM),
		OffRoadM: float32(r.OffRoadM), RubbleM: float32(r.RubbleM), EtaS: float32(r.ETAs), DirectM: float32(r.DirectM), Streets: r.Streets}
	for _, q := range r.Points {
		p.Points = append(p.Points, s.geoPoint(q))
	}
	return p
}

// --- Team commands (TAK chat) ---------------------------------------------------

// Command handles a chat command from a team ("done", "unable", "status").
func (s *Service) Command(teamID, text string, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.teams {
		if t.cfg.ID == teamID {
			return s.commandLocked(t, text, now)
		}
	}
	return "unknown team " + teamID
}

func (s *Service) commandLocked(t *team, text string, now time.Time) string {
	w := strings.ToLower(strings.Trim(strings.Fields(text + " x")[0], ".!,"))
	a := t.asg
	switch w {
	case "done", "finished", "complete", "completed", "clear", "cleared", "reached":
		if a == nil {
			return "no assignment to finish"
		}
		s.doneLocked(a, t.cfg.Callsign+" said "+strconv.Quote(strings.TrimSpace(text)), now)
		return "done"
	case "unable", "cannot", "can't", "cant", "no", "reject", "decline":
		if a == nil {
			return "no assignment to decline"
		}
		t.unable[a.pb.CasualtyId] = true
		s.cancelLocked(a, t.cfg.Callsign+" is unable", now)
		return "declined"
	case "status", "?", "where":
		if a == nil {
			s.chatLocked(t.cfg.Callsign+": no assignment; stand by.", t, now)
			return "status sent"
		}
		a.acked = true
		if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE {
			s.chatLocked(fmt.Sprintf("%s: on scene at %s %s, about %s left.", t.cfg.Callsign, a.pb.CasualtyId, s.says(a.pb.CasualtyId), hmin(s.sceneLeftLocked(a, now))), t, now)
			return "status sent"
		}
		left := a.route.Remaining(now.Sub(a.startAt).Seconds() * s.cfg.D.TimeScale)
		if !t.sim() {
			left = t.pos.Dist(a.target)
		}
		s.chatLocked(fmt.Sprintf("%s: %s %s, %s, about %.0f m to go.", t.cfg.Callsign, a.pb.CasualtyId, s.says(a.pb.CasualtyId), stateWord(a.pb.State), left), t, now)
		return "status sent"
	case "ack", "ok", "okay", "roger", "copy", "wilco", "accept", "accepted", "yes":
		if a != nil {
			a.acked = true
		}
		if a != nil && !strings.Contains(a.pb.Note, "acknowledged") {
			a.pb.Note = strings.TrimSpace(a.pb.Note + "; acknowledged")
			s.dirty = true
		}
		return "noted"
	}
	return "not a command"
}

// sceneLeftLocked is how long the team on scene still needs, in real time
// (a simulated team's wall-clock wait scaled up by rescue_time_scale).
func (s *Service) sceneLeftLocked(a *assignment, now time.Time) time.Duration {
	left := a.freeAt.Sub(now)
	if left < 0 {
		return 0
	}
	if a.team != nil && a.team.sim() {
		left = time.Duration(float64(left) * s.cfg.D.RescueTimeScale)
	}
	return left
}

func stateWord(st sarpb.AssignmentState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(st.String(), "ASSIGNMENT_STATE_")), "_", " ")
}

// --- Publishing -------------------------------------------------------------------

// State returns the dispatch state message.
func (s *Service) State() *sarpb.DispatchState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked(time.Now())
}

func (s *Service) stateLocked(now time.Time) *sarpb.DispatchState {
	st := &sarpb.DispatchState{Header: msg.NewHeader(s.cfg.Incident, s.cfg.Source, 0), TimeScale: float32(s.cfg.D.TimeScale), Run: s.run}
	for _, t := range s.teams {
		pt := &sarpb.Team{TeamId: t.cfg.ID, Callsign: t.cfg.Callsign, Mode: t.cfg.Mode, Source: t.cfg.Source, State: t.state, Position: s.geoPoint(t.pos),
			CourseDeg: float32(t.course), SpeedMps: float32(t.speed), TakUid: t.uid, AssignmentsDone: t.done}
		if !t.lastSeen.IsZero() {
			pt.LastSeenUnixNs = t.lastSeen.UnixNano()
		}
		if t.asg != nil {
			pt.AssignmentId, pt.CasualtyId = t.asg.pb.AssignmentId, t.asg.pb.CasualtyId
		}
		st.Teams = append(st.Teams, pt)
	}
	for _, id := range s.order {
		st.Assignments = append(st.Assignments, s.asg[id].pb)
	}
	for _, ci := range s.waitingLocked(now) {
		st.Waiting = append(st.Waiting, ci.c.CasualtyId)
	}
	if n := s.cfg.Roads; n != nil {
		st.Roads = &sarpb.RoadsInfo{Source: n.Source, Path: s.cfg.RoadsPath, Ways: uint32(n.Ways), Edges: uint32(len(n.Edges)), BlockedEdges: uint32(n.BlockedEdges),
			Blockages: uint32(len(n.Blockages)), LengthKm: float32(n.LengthM / 1000)}
	} else {
		st.Roads = &sarpb.RoadsInfo{Path: s.cfg.RoadsPath, Blockages: uint32(len(s.cfg.Blockages))}
	}
	st.Tak = &sarpb.TakLink{Server: s.cfg.D.TakServer}
	if s.tak != nil {
		cs := s.tak.Stats()
		st.Tak.Connected, st.Tak.Sent, st.Tak.Received, st.Tak.Reconnects, st.Tak.LastError = cs.Connected, cs.Sent, cs.Received, uint32(cs.Reconnects), cs.LastError
	}
	for _, u := range s.sortedUsersLocked() {
		st.Tak.Users = append(st.Tak.Users, u.callsign)
	}
	return st
}

func (s *Service) sortedUsersLocked() []*takUser {
	var out []*takUser
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].callsign < out[j].callsign })
	return out
}

// Publish sends the dispatch state if it changed, or if force.
func (s *Service) Publish(force bool) {
	s.mu.Lock()
	if !force && !s.dirty {
		s.mu.Unlock()
		return
	}
	s.dirty = false
	st := s.stateLocked(time.Now())
	st.Header.Seq = s.seq.Next()
	s.mu.Unlock()
	if s.pub == nil {
		return
	}
	b, err := msg.Marshal(st)
	if err == nil {
		err = s.pub.Put(b)
	}
	if err != nil {
		s.log.Warn("publishing the dispatch state", "err", err)
	}
}

// Reset forgets every assignment, puts the simulated teams back at the
// staging area and clears the map (a new acceptance run).
func (s *Service) Reset() {
	s.mu.Lock()
	now := time.Now()
	for _, t := range s.teams {
		s.deleteLocked(s.routeUID(t), now)
	}
	for id := range s.cas {
		s.deleteLocked(s.casUID(id), now)
	}
	for uid := range s.invLines {
		s.deleteLocked(uid, now)
	}
	s.invLines = map[string]bool{}
	s.run = runToken(now)
	s.resetLocked()
	s.chats = nil
	s.resets++
	s.saveLocked()
	s.mu.Unlock()
	s.log.Info("reset: assignments forgotten, teams back at the staging area")
	s.Publish(true)
}

// Close unsubscribes and drops the TAK connection.
func (s *Service) Close() {
	for _, x := range s.subs {
		_ = x.Close()
	}
	if s.tak != nil {
		s.tak.Close()
	}
	for _, c := range s.teamTAK {
		c.Close()
	}
}

// Run steps every Tick and publishes on change and every publish_interval, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	if s.tak != nil {
		go s.tak.Run(ctx)
	}
	for _, c := range s.teamTAK {
		go c.Run(ctx)
	}
	tick := time.NewTicker(s.cfg.Tick)
	defer tick.Stop()
	every := s.cfg.D.PublishInterval.Duration
	if every <= 0 {
		every = 5 * time.Second
	}
	pub := time.NewTicker(every)
	defer pub.Stop()
	s.Publish(true)
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.saveLocked()
			s.mu.Unlock()
			return
		case now := <-tick.C:
			s.Step(now)
			s.Publish(false)
		case <-pub.C:
			s.Publish(true)
		}
	}
}
