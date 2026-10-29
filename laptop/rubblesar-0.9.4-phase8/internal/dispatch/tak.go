package dispatch

import (
	"fmt"
	"math"
	"strings"
	"time"

	"rubblesar/internal/casualty"
	"rubblesar/internal/cot"
	"rubblesar/internal/geo"
	"rubblesar/internal/sarpb"
)

// UIDs of the objects rescue-dispatch puts on the TAK map.
func (s *Service) prefix() string            { return "rubblesar." + s.cfg.Incident + "." }
func (s *Service) casUID(id string) string   { return s.prefix() + "cas." + id }
func (s *Service) droneUID(id string) string { return s.prefix() + "uav." + id }
func (s *Service) teamUID(t *team) string    { return s.prefix() + "team." + t.cfg.ID }
func (s *Service) routeUID(t *team) string   { return s.prefix() + "route." + t.cfg.ID }
func (s *Service) invUID(task string) string { return s.prefix() + "inv." + task }
func (s *Service) rubbleUID(i int) string    { return fmt.Sprintf("%srubble.%d", s.prefix(), i+1) }

// OpsUID is rescue-dispatch's own TAK uid.
func (s *Service) OpsUID() string { return s.uid }

// TriageColour is the marker colour of a triage category.
func TriageColour(t sarpb.Triage) int32 {
	switch t {
	case sarpb.Triage_TRIAGE_RED:
		return cot.Red
	case sarpb.Triage_TRIAGE_YELLOW:
		return cot.Yellow
	case sarpb.Triage_TRIAGE_GREEN:
		return cot.Green
	case sarpb.Triage_TRIAGE_BLACK:
		return cot.Black
	}
	return cot.Grey
}

const (
	opsEvery      = 30 * time.Second
	teamMoveEvery = 2 * time.Second
	teamIdleEvery = 30 * time.Second
	droneMaxQuiet = 10 * time.Second
	chatKeep      = 200
)

func (s *Service) send(e *cot.Event) {
	if s.tak != nil {
		s.tak.Send(e)
	}
}

func (s *Service) deleteLocked(uid string, now time.Time) {
	s.send(cot.Delete(uid, now))
}

func (s *Service) staging() geo.Point { return s.toPoint(s.stage) }

// chatLocked posts to All Chat Rooms, or directly to a real (TAK) team.
func (s *Service) chatLocked(text string, to *team, now time.Time) {
	g := s.staging()
	dest := cot.ChatTo{}
	toName := cot.AllChat
	if to != nil && !to.sim() && to.uid != "" {
		dest = cot.ChatTo{UID: to.uid, Callsign: to.cfg.Callsign}
		toName = to.cfg.Callsign
	}
	s.chats = append(s.chats, ChatLine{At: now, From: s.cfg.D.Callsign, To: toName, Text: text})
	if len(s.chats) > chatKeep {
		s.chats = s.chats[len(s.chats)-chatKeep:]
	}
	s.send(cot.GeoChat(s.uid, s.cfg.D.Callsign, dest, text, g.Lat, g.Lon, now))
}

// --- Map objects --------------------------------------------------------------------

func (s *Service) opsLocked(now time.Time) *cot.Event {
	g := s.staging()
	s.opsSentAt = now
	return cot.PLI(s.uid, s.cfg.D.Callsign, s.cfg.D.Group, "HQ", g.Lat, g.Lon, 0, 0, "RubbleSAR rescue dispatch (command post). Teams: reply \"done\", \"unable\" or \"status\".", now, 2*time.Minute)
}

// rescueWords describes the casualty's rescue state for its marker; without
// times, for telling a real change from a ticking clock.
func (s *Service) rescueWords(id string, ci *casInfo, now time.Time, times bool) string {
	if a := s.activeFor(id); a != nil {
		t := a.team
		if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE {
			w := "Rescue: " + t.cfg.Callsign + " on scene"
			if a.ext.Class != "" {
				w += ", " + a.ext.Class
				if times {
					w += fmt.Sprintf(", about %s left (%s)", hmin(s.sceneLeftLocked(a, now)), a.ext.Why)
				}
			}
			return w
		}
		if !times {
			return "Rescue: " + t.cfg.Callsign + " en route"
		}
		left := a.route.ETAs - now.Sub(a.startAt).Seconds()*s.scaleFor(t)
		return fmt.Sprintf("Rescue: %s en route, about %s", t.cfg.Callsign, mins(time.Duration(math.Max(0, left)*float64(time.Second))))
	}
	if t := s.handled[id]; t != "" {
		if s.priority(ci.c) > 0 {
			return "Rescue: freed and handed over by " + s.callsignOf(t)
		}
		return "Rescue: visited by " + s.callsignOf(t)
	}
	if s.priority(ci.c) == 0 {
		return "Rescue: no team sent (" + strings.ToUpper(casualty.TriageName(ci.c.Triage)) + ")"
	}
	if w := s.noRoute[id]; w != "" {
		return "Rescue: NO STREET ROUTE (" + plain(w) + ")"
	}
	return "Rescue: waiting for a free team"
}

func (s *Service) scaleFor(t *team) float64 {
	if t.sim() {
		return s.cfg.D.TimeScale
	}
	return 1
}

func (s *Service) callsignOf(teamID string) string {
	for _, t := range s.teams {
		if t.cfg.ID == teamID {
			return t.cfg.Callsign
		}
	}
	return teamID
}

// Rescued reports whether a casualty's marker shows it rescued (green,
// "RESCUED"): a team has finished with a casualty that needed one.
func (s *Service) rescuedLocked(id string, ci *casInfo) bool {
	return s.handled[id] != "" && s.activeFor(id) == nil && s.priority(ci.c) > 0
}

// MarkerLook is the colour and label a casualty marker has: its triage
// colour, or green and "RESCUED" once a team has freed it.
func MarkerLook(id string, c *sarpb.Casualty, rescued bool) (int32, string) {
	if rescued {
		return cot.Green, id + " RESCUED"
	}
	l := strings.ToUpper(casualty.TriageName(c.Triage))
	if c.Provisional {
		l += "?"
	}
	return TriageColour(c.Triage), id + " " + l
}

func (s *Service) markerLocked(id string, ci *casInfo, now time.Time) (*cot.Event, string) {
	c := ci.c
	rescued := s.rescuedLocked(id, ci)
	colour, label := MarkerLook(id, c, rescued)
	tri := strings.ToUpper(casualty.TriageName(c.Triage))
	if c.Provisional {
		tri += " (provisional)"
	}
	var b strings.Builder
	if rescued {
		fmt.Fprintf(&b, "RESCUED by %s. ", s.callsignOf(s.handled[id]))
	}
	fmt.Fprintf(&b, "%s: %s\n", tri, strings.TrimSpace(c.Reason))
	if c.Summary != "" {
		fmt.Fprintf(&b, "%s\n", strings.TrimSpace(c.Summary))
	}
	fmt.Fprintf(&b, "Position within %.0f m (CEP); seen by %s\n", math.Max(1, float64(c.CepM)), strings.Join(c.UavIds, ", "))
	head := b.String()
	tail := "\nSimulated incident: provisional triage for a person to confirm."
	rem := head + s.rescueWords(id, ci, now, true) + tail
	// Rescue times tick down: compare without them.
	g := c.GetPosition()
	key := fmt.Sprintf("%s|%d|%.6f|%.6f|%.0f|%s", label, colour, g.GetLatDeg(), g.GetLonDeg(), c.CepM, head+s.rescueWords(id, ci, now, false))
	e := cot.Marker(s.casUID(id), label, colour, g.GetLatDeg(), g.GetLonDeg(), g.GetAltMslM(), float64(c.CepM), rem, now, s.cfg.D.MarkerStale.Duration)
	return e, key
}

func (s *Service) droneLocked(id string, d *droneInfo, now time.Time) *cot.Event {
	t := d.t
	role := strings.ToLower(strings.TrimPrefix(t.Role.String(), "UAV_ROLE_"))
	state := strings.ToLower(strings.TrimPrefix(t.State.String(), "FLIGHT_STATE_"))
	rem := fmt.Sprintf("%s %s, %s, %.0f m above ground, battery %.0f%%, %d min flight left", strings.ToUpper(id), role, state, t.AltAglM, t.BatteryPct, t.FlightTimeLeftS/60)
	if ts := t.Task; ts != nil && ts.TaskId != "" {
		rem += fmt.Sprintf("; %s %s (%s)", ts.TaskId, ts.CasualtyId, strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(ts.State.String(), "TASK_STATE_")), "_", " "))
	}
	if p := t.Perception; p != nil && p.MissionSteps > 0 {
		rem += fmt.Sprintf("; survey %d/%d", p.MissionStep, p.MissionSteps)
	}
	d.sentAt, d.sentP = now, d.pos
	return cot.Drone(s.droneUID(id), strings.ToUpper(id), t.Position.LatDeg, t.Position.LonDeg, t.Position.AltMslM, float64(t.CourseDeg), float64(t.GroundSpeedMps),
		int(math.Round(float64(t.BatteryPct))), rem, now, 20*time.Second)
}

func (s *Service) teamLocked(t *team, now time.Time) *cot.Event {
	g := s.toPoint(t.pos)
	rem := fmt.Sprintf("Simulated %s team (moves %.0fx real time)", t.cfg.Mode, s.cfg.D.TimeScale)
	if a := t.asg; a != nil {
		rem += fmt.Sprintf("; %s %s %s", stateWord(a.pb.State), a.pb.CasualtyId, s.label(a.pb.CasualtyId))
	} else {
		rem += "; free"
	}
	t.sentAt = now
	return cot.PLI(s.teamUID(t), t.cfg.Callsign, s.cfg.D.Group, "Team Member", g.Lat, g.Lon, t.course, t.speed, rem, now, 2*time.Minute)
}

func (s *Service) routeEventLocked(a *assignment, now time.Time) *cot.Event {
	t := a.team
	var pts [][2]float64
	for _, p := range a.route.Points {
		g := s.toPoint(p)
		pts = append(pts, [2]float64{g.Lat, g.Lon})
	}
	label := fmt.Sprintf("%s → %s", t.cfg.Callsign, a.pb.CasualtyId)
	rem := fmt.Sprintf("%s route to %s %s: %.0f m (%.0f m street, %.0f m on foot, %.0f m over rubble), about %s. Assignment %s.",
		t.cfg.Callsign, a.pb.CasualtyId, s.label(a.pb.CasualtyId), a.route.LengthM, a.route.RoadM, a.route.OffRoadM, a.route.RubbleM,
		mins(time.Duration(a.route.ETAs*float64(time.Second))), a.pb.AssignmentId)
	if len(a.route.Streets) > 0 {
		rem += " Via " + strings.Join(a.route.Streets, ", ") + "."
	}
	return cot.Line(s.routeUID(t), label, TriageColour(a.pb.Triage), 4, pts, rem, now, 2*s.cfg.D.MarkerStale.Duration)
}

func (s *Service) sendRouteLocked(a *assignment, now time.Time) {
	if s.tak != nil {
		s.send(s.routeEventLocked(a, now))
	}
}

func (s *Service) invLinesLocked(now time.Time, force bool) []*cot.Event {
	if !s.cfg.D.ShowInvestigations || s.tasks == nil {
		return nil
	}
	var out []*cot.Event
	keep := map[string]bool{}
	for _, tk := range s.tasks.Tasks {
		switch tk.State {
		case sarpb.TaskState_TASK_STATE_ASSIGNED, sarpb.TaskState_TASK_STATE_EN_ROUTE, sarpb.TaskState_TASK_STATE_ON_STATION:
		default:
			continue
		}
		d := s.drones[tk.UavId]
		if d == nil || tk.Target == nil {
			continue
		}
		uid := s.invUID(tk.TaskId)
		keep[uid] = true
		if !force && s.invLines[uid] && !d.sentAt.Equal(now) {
			continue // updated with its drone
		}
		s.invLines[uid] = true
		pts := [][2]float64{{d.t.Position.LatDeg, d.t.Position.LonDeg}, {tk.Target.LatDeg, tk.Target.LonDeg}}
		out = append(out, cot.Line(uid, fmt.Sprintf("%s → %s", strings.ToUpper(tk.UavId), tk.CasualtyId), cot.Orange, 2, pts,
			fmt.Sprintf("Investigation %s: %s looks at %s close up (%s).", tk.TaskId, tk.UavId, tk.CasualtyId, strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(tk.State.String(), "TASK_STATE_")), "_", " ")),
			now, 30*time.Second))
	}
	for uid := range s.invLines {
		if !keep[uid] {
			delete(s.invLines, uid)
			out = append(out, cot.Delete(uid, now))
		}
	}
	return out
}

// mapLocked sends what changed (and refreshes what is due).
func (s *Service) mapLocked(now time.Time) {
	if s.tak == nil {
		return
	}
	if now.Sub(s.opsSentAt) >= opsEvery {
		s.send(s.opsLocked(now))
	}
	refresh := s.cfg.D.MarkerRefresh.Duration
	for id, ci := range s.cas {
		e, key := s.markerLocked(id, ci, now)
		if key != ci.sentKey || now.Sub(ci.sentAt) >= refresh {
			ci.sentKey, ci.sentAt = key, now
			s.send(e)
		}
	}
	for id := range s.gone {
		s.deleteLocked(s.casUID(id), now)
		delete(s.gone, id)
	}
	for id, d := range s.drones {
		if now.Sub(d.at) > droneMaxQuiet*3 {
			continue // no telemetry: let it go stale on the map
		}
		if now.Sub(d.sentAt) >= s.cfg.D.DroneEvery.Duration && (d.pos.Dist(d.sentP) >= 1 || now.Sub(d.sentAt) >= droneMaxQuiet) {
			s.send(s.droneLocked(id, d, now))
		}
	}
	for _, e := range s.invLinesLocked(now, false) {
		s.send(e)
	}
	for _, t := range s.teams {
		if !t.sim() {
			continue
		}
		every := teamIdleEvery
		if t.state == sarpb.TeamState_TEAM_STATE_EN_ROUTE {
			every = teamMoveEvery
		}
		if now.Sub(t.sentAt) >= every {
			if c := s.teamTAK[t.cfg.ID]; c != nil {
				c.Send(s.teamLocked(t, now))
			}
		}
	}
	s.refreshRoutesLocked(now)
}

// refreshRoutesLocked re-sends route lines (and the rubble areas) before they go stale.
func (s *Service) refreshRoutesLocked(now time.Time) {
	if now.Sub(s.refreshAt) < s.cfg.D.MarkerStale.Duration {
		return
	}
	s.refreshAt = now
	for _, t := range s.teams {
		if a := t.asg; a != nil && a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
			s.send(s.routeEventLocked(a, now))
		}
	}
	for _, e := range s.collapsesLocked(now) {
		s.send(e)
	}
}

// Rubble areas on the TAK map: brown, half see-through.
var (
	rubbleStroke = cot.ARGB(255, 93, 64, 55)
	rubbleFill   = cot.ARGB(110, 141, 110, 99)
)

// collapsesLocked: the scenario's rubble areas as shapes (static; refreshed before they go stale).
func (s *Service) collapsesLocked(now time.Time) []*cot.Event {
	sc := s.cfg.Scene
	if !s.cfg.D.ShowCollapses || sc == nil {
		return nil
	}
	sites := map[string]string{}
	for _, st := range sc.Sites {
		sites[st.ID] = fmt.Sprintf("%s collapse, rubble %.1f m deep", st.Kind, st.HeightM)
	}
	var out []*cot.Event
	for i, b := range sc.Blockages {
		if len(b.Polygon) < 3 {
			continue
		}
		pts := make([][2]float64, len(b.Polygon))
		for k, v := range b.Polygon {
			pts[k] = [2]float64{v.Lat, v.Lon}
		}
		name := b.SiteID
		if n := s.cfg.SiteNames[b.SiteID]; n != "" {
			name = n
		}
		rem := fmt.Sprintf("Rubble from the collapsed building at %s (%s; simulated). Streets within %.0f m are closed to rescue teams.", name, sites[b.SiteID], s.cfg.D.BlockMarginM)
		out = append(out, cot.Polygon(s.rubbleUID(i), name+" collapse", rubbleStroke, rubbleFill, 2, pts, rem, now, 2*s.cfg.D.MarkerStale.Duration))
	}
	return out
}

// hello is the whole picture, sent on every connection to the TAK server.
func (s *Service) hello(now time.Time) []*cot.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]*cot.Event{s.opsLocked(now)}, s.collapsesLocked(now)...)
	for id, ci := range s.cas {
		e, key := s.markerLocked(id, ci, now)
		ci.sentKey, ci.sentAt = key, now
		out = append(out, e)
	}
	for id, d := range s.drones {
		if now.Sub(d.at) <= droneMaxQuiet*3 {
			out = append(out, s.droneLocked(id, d, now))
		}
	}
	out = append(out, s.invLinesLocked(now, true)...)
	for _, t := range s.teams {
		if a := t.asg; a != nil && a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
			out = append(out, s.routeEventLocked(a, now))
		}
	}
	return out
}

// --- From the TAK server -------------------------------------------------------------

func (s *Service) onTAK(e *cot.Event) {
	if strings.HasPrefix(e.UID, "rubblesar.") || strings.HasPrefix(e.UID, "GeoChat."+s.uid+".") {
		return // our own objects (or another RubbleSAR's)
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case e.IsPLI():
		u := s.users[e.UID]
		if u == nil {
			u = &takUser{uid: e.UID}
			s.users[e.UID] = u
			s.dirty = true
		}
		u.callsign, u.at = e.Callsign(), now
		u.pos.X, u.pos.Y = s.local.ToLocal(geo.Point{Lat: float64(e.Point.Lat), Lon: float64(e.Point.Lon)})
		if t := s.takTeamLocked(u.callsign); t != nil {
			if t.uid != e.UID {
				s.log.Info("TAK team position", "team", t.cfg.ID, "callsign", t.cfg.Callsign, "uid", e.UID)
			}
			t.uid, t.lastSeen, t.pos = e.UID, now, u.pos
			if t.cfg.AtCommandPost {
				t.pos = s.stage // wherever the device is: it stands in for a team at the command post
			} else if tr := e.Detail.Track; tr != nil {
				t.course, t.speed = float64(tr.Course), float64(tr.Speed)
			}
		}
		for uid, x := range s.users {
			if now.Sub(x.at) > s.cfg.D.TeamOffline.Duration {
				delete(s.users, uid)
				s.dirty = true
			}
		}
	case e.IsChat():
		ch := e.Detail.Chat
		toUs := ch.Chatroom == cot.AllChat || ch.ID == s.uid || strings.EqualFold(ch.Chatroom, s.cfg.D.Callsign)
		for _, d := range e.Dests() {
			toUs = toUs || strings.EqualFold(d, s.cfg.D.Callsign)
		}
		if !toUs {
			return
		}
		text := e.ChatText()
		to := cot.AllChat
		if ch.Chatroom != cot.AllChat {
			to = s.cfg.D.Callsign
		}
		s.chats = append(s.chats, ChatLine{At: now, From: ch.SenderCallsign, To: to, Text: text})
		if len(s.chats) > chatKeep {
			s.chats = s.chats[len(s.chats)-chatKeep:]
		}
		t := s.takTeamLocked(ch.SenderCallsign)
		if t == nil {
			return
		}
		if t.uid == "" {
			t.uid = e.ChatSender()
		}
		res := s.commandLocked(t, text, now)
		s.log.Info("chat from a team", "team", t.cfg.ID, "text", text, "result", res)
	}
}

func (s *Service) takTeamLocked(callsign string) *team {
	cs := strings.TrimSpace(callsign)
	for _, t := range s.teams {
		if !t.sim() && strings.EqualFold(t.cfg.Callsign, cs) {
			return t
		}
	}
	return nil
}

// Chats returns the latest chat lines, oldest first.
func (s *Service) Chats() []ChatLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ChatLine(nil), s.chats...)
}
