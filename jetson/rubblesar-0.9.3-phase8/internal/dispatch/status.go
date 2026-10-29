package dispatch

import (
	"fmt"
	"math"
	"strings"
	"time"

	"rubblesar/internal/casualty"
	"rubblesar/internal/sarpb"
)

// Status is the status endpoint's view.
type Status struct {
	Run       string  `json:"run"`
	TimeScale float64 `json:"time_scale"`
	// RescueTimeScale: simulated teams free casualties this many times faster.
	RescueTimeScale float64           `json:"rescue_time_scale"`
	Roads           RoadsStatus       `json:"roads"`
	TAK             TAKStatus         `json:"tak"`
	Casualties      int               `json:"casualties_known"`
	ListAgeS        float64           `json:"casualty_list_age_s"`
	Lists           uint64            `json:"casualty_lists_received"`
	Drones          int               `json:"drones_on_map"`
	Teams           []TeamRow         `json:"teams"`
	Assignments     []AssignmentRow   `json:"assignments"`
	Waiting         []string          `json:"waiting"`
	NoRoute         map[string]string `json:"no_route,omitempty"`
	Chats           []ChatLine        `json:"chats"`
	Resets          int               `json:"resets"`
	Warnings        []string          `json:"warnings,omitempty"`
}

// RoadsStatus describes the street network.
type RoadsStatus struct {
	Loaded       bool    `json:"loaded"`
	Source       string  `json:"source"`
	Path         string  `json:"path"`
	Ways         int     `json:"ways"`
	Edges        int     `json:"edges"`
	BlockedEdges int     `json:"blocked_edges"`
	Blockages    int     `json:"blockages"`
	LengthKm     float64 `json:"length_km"`
	Rivers       int     `json:"rivers"`
}

// TAKStatus describes the TAK server link.
type TAKStatus struct {
	Server     string   `json:"server"`
	Enabled    bool     `json:"enabled"`
	Connected  bool     `json:"connected"`
	Sent       uint64   `json:"sent"`
	Received   uint64   `json:"received"`
	Queued     int      `json:"queued"`
	Dropped    uint64   `json:"dropped"`
	Reconnects uint64   `json:"reconnects"`
	LastError  string   `json:"last_error,omitempty"`
	Users      []string `json:"users"`
	Callsign   string   `json:"callsign"`
	UID        string   `json:"uid"`
}

// TeamRow is one team.
type TeamRow struct {
	ID       string  `json:"id"`
	Callsign string  `json:"callsign"`
	Mode     string  `json:"mode"`
	Source   string  `json:"source"`
	State    string  `json:"state"`
	Lat      float64 `json:"lat"`
	Lon      float64 `json:"lon"`
	Casualty string  `json:"casualty,omitempty"`
	ETAs     float64 `json:"eta_s"`
	// FreeS: on scene, how long until the team is free (wall clock).
	FreeS     float64 `json:"free_s"`
	Done      uint32  `json:"done"`
	LastSeenS float64 `json:"last_seen_s"`
}

// AssignmentRow is one assignment.
type AssignmentRow struct {
	ID        string  `json:"id"`
	Casualty  string  `json:"casualty"`
	Team      string  `json:"team"`
	Triage    string  `json:"triage"`
	TriageNow string  `json:"triage_now"`
	Priority  uint32  `json:"priority"`
	State     string  `json:"state"`
	LengthM   float64 `json:"length_m"`
	RubbleM   float64 `json:"rubble_m"`
	ETAs      float64 `json:"eta_s"`
	// TookS: dispatch to on scene, in the team's own time (simulated teams: scaled up).
	TookS   float64 `json:"took_s"`
	WaitedS float64 `json:"waited_s"`
	// Rescue: the extrication once on scene ("heavy rescue, about 2 h 10 min (...)").
	Rescue string `json:"rescue,omitempty"`
	// RescueS: extrication plus handover, real seconds (simulated teams: rescue_time_scale times faster).
	RescueS float64 `json:"rescue_s"`
	Note    string  `json:"note,omitempty"`
}

// Status returns a snapshot.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	st := Status{Run: s.run, TimeScale: s.cfg.D.TimeScale, RescueTimeScale: s.cfg.D.RescueTimeScale, Casualties: len(s.cas), Lists: s.lists, ListAgeS: -1, Resets: s.resets, Drones: len(s.drones)}
	if !s.casAt.IsZero() {
		st.ListAgeS = math.Round(now.Sub(s.casAt).Seconds()*10) / 10
	}
	st.Roads = RoadsStatus{Path: s.cfg.RoadsPath, Blockages: len(s.cfg.Blockages)}
	if n := s.cfg.Roads; n != nil {
		st.Roads = RoadsStatus{Loaded: true, Source: n.Source, Path: s.cfg.RoadsPath, Ways: n.Ways, Edges: len(n.Edges), BlockedEdges: n.BlockedEdges,
			Blockages: len(n.Blockages), LengthKm: math.Round(n.LengthM/100) / 10, Rivers: len(n.Water)}
	}
	st.TAK = TAKStatus{Server: s.cfg.D.TakServer, Enabled: s.tak != nil, Callsign: s.cfg.D.Callsign, UID: s.uid}
	if s.tak != nil {
		cs := s.tak.Stats()
		st.TAK.Connected, st.TAK.Sent, st.TAK.Received, st.TAK.Queued, st.TAK.Dropped, st.TAK.Reconnects, st.TAK.LastError =
			cs.Connected, cs.Sent, cs.Received, cs.Queued, cs.Dropped, cs.Reconnects, cs.LastError
	}
	for _, u := range s.sortedUsersLocked() {
		st.TAK.Users = append(st.TAK.Users, u.callsign)
	}
	for _, t := range s.teams {
		g := s.toPoint(t.pos)
		r := TeamRow{ID: t.cfg.ID, Callsign: t.cfg.Callsign, Mode: t.cfg.Mode, Source: t.cfg.Source, State: TeamStateName(t.state),
			Lat: math.Round(g.Lat*1e6) / 1e6, Lon: math.Round(g.Lon*1e6) / 1e6, Done: t.done, LastSeenS: -1}
		if !t.lastSeen.IsZero() {
			r.LastSeenS = math.Round(now.Sub(t.lastSeen).Seconds())
		}
		if a := t.asg; a != nil {
			r.Casualty = a.pb.CasualtyId
			if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_EN_ROUTE {
				r.ETAs = math.Max(0, math.Round(a.route.ETAs-now.Sub(a.startAt).Seconds()*s.scaleFor(t)))
			}
			if a.pb.State == sarpb.AssignmentState_ASSIGNMENT_STATE_ON_SCENE {
				r.FreeS = math.Max(0, math.Round(a.freeAt.Sub(now).Seconds()))
			}
		}
		st.Teams = append(st.Teams, r)
	}
	for _, id := range s.order {
		a := s.asg[id]
		p := a.pb
		r := AssignmentRow{ID: p.AssignmentId, Casualty: p.CasualtyId, Team: p.TeamId, Triage: casualty.TriageName(p.Triage), TriageNow: casualty.TriageName(p.TriageNow),
			Priority: p.Priority, State: stateWord(p.State), LengthM: math.Round(a.route.LengthM), RubbleM: math.Round(a.route.RubbleM), ETAs: math.Round(a.route.ETAs), Note: p.Note, TookS: -1,
			Rescue: p.Extrication, RescueS: math.Round(float64(p.ExtricationS))}
		if p.Provisional {
			r.Triage += "?"
		}
		if p.OnSceneUnixNs > 0 {
			// In the team's own time (simulated teams run time_scale times faster).
			r.TookS = time.Duration(p.OnSceneUnixNs - p.CreatedUnixNs).Seconds()
			if a.team != nil {
				r.TookS *= s.scaleFor(a.team)
			}
			r.TookS = math.Round(r.TookS)
		}
		if p.NeededUnixNs > 0 {
			r.WaitedS = math.Round(time.Duration(p.CreatedUnixNs - p.NeededUnixNs).Seconds())
		}
		st.Assignments = append(st.Assignments, r)
	}
	for _, ci := range s.waitingLocked(now) {
		st.Waiting = append(st.Waiting, ci.c.CasualtyId+" "+s.label(ci.c.CasualtyId))
	}
	if len(s.noRoute) > 0 {
		st.NoRoute = map[string]string{}
		for k, v := range s.noRoute {
			st.NoRoute[k] = plain(v)
		}
	}
	n := len(s.chats)
	st.Chats = append([]ChatLine(nil), s.chats[max(0, n-10):]...)
	if s.cfg.Roads == nil {
		st.Warnings = append(st.Warnings, "no street file loaded: routes are straight lines (scripts/zbook/32-roads.sh)")
	}
	if len(s.cfg.Blockages) == 0 {
		st.Warnings = append(st.Warnings, "no rubble areas known (scenario not loaded): routes may cross rubble")
	}
	if n := s.cfg.Roads; n != nil && n.Source == "osm" && len(n.Water) == 0 {
		st.Warnings = append(st.Warnings, "the street file has no rivers (downloaded by 0.6.0): walks off the streets may cross the river; run scripts/zbook/32-roads.sh")
	}
	return st
}

// TeamStateName names a team state in words.
func TeamStateName(t sarpb.TeamState) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(t.String(), "TEAM_STATE_")), "_", " ")
}

// Text renders the status as tables.
func (s Status) Text() string {
	var b strings.Builder
	age := "none received yet"
	if s.ListAgeS >= 0 {
		age = fmt.Sprintf("%d known, list %.0f s old", s.Casualties, s.ListAgeS)
	}
	fmt.Fprintf(&b, "run %s; casualties: %s; simulated teams move %gx and free casualties %gx faster than real time\n", s.Run, age, s.TimeScale, s.RescueTimeScale)
	if s.Roads.Loaded {
		fmt.Fprintf(&b, "streets: %s, %.1f km (%d ways, %d rivers), %d pieces closed by %d rubble areas (%s)\n", s.Roads.Source, s.Roads.LengthKm, s.Roads.Ways, s.Roads.Rivers, s.Roads.BlockedEdges, s.Roads.Blockages, s.Roads.Path)
	} else {
		fmt.Fprintf(&b, "streets: NOT LOADED (%s); routes are straight lines\n", s.Roads.Path)
	}
	switch {
	case !s.TAK.Enabled:
		b.WriteString("TAK: off ([dispatch] tak_server = \"\")\n")
	case s.TAK.Connected:
		fmt.Fprintf(&b, "TAK: connected to %s as %s; %d events sent, %d received; users: %s\n", s.TAK.Server, s.TAK.Callsign, s.TAK.Sent, s.TAK.Received, nonEmpty(strings.Join(s.TAK.Users, ", "), "none"))
	default:
		fmt.Fprintf(&b, "TAK: NOT CONNECTED to %s (%d events queued): %s\n", s.TAK.Server, s.TAK.Queued, nonEmpty(s.TAK.LastError, "connecting"))
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "WARNING: %s\n", w)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "%-8s %-9s %-7s %-6s %-9s %-10s %10s %5s  %s\n", "TEAM", "CALLSIGN", "MODE", "SOURCE", "STATE", "CASUALTY", "ETA", "DONE", "POSITION")
	for _, t := range s.Teams {
		eta := "-"
		switch {
		case t.ETAs > 0:
			eta = fmt.Sprintf("%.0fs", t.ETAs)
		case t.State == "on scene":
			eta = fmt.Sprintf("free %.0fs", t.FreeS)
		}
		pos := fmt.Sprintf("%.5f,%.5f", t.Lat, t.Lon)
		if t.Source == "tak" {
			if t.LastSeenS < 0 {
				pos = "(no position report yet)"
			} else {
				pos += fmt.Sprintf(" (%.0f s ago)", t.LastSeenS)
			}
		}
		fmt.Fprintf(&b, "%-8s %-9s %-7s %-6s %-9s %-10s %10s %5d  %s\n", t.ID, t.Callsign, t.Mode, t.Source, t.State, nonEmpty(t.Casualty, "-"), eta, t.Done, pos)
	}
	fmt.Fprintf(&b, "\nwaiting for a team: %s\n", nonEmpty(strings.Join(s.Waiting, ", "), "none"))
	for id, w := range s.NoRoute {
		fmt.Fprintf(&b, "  no route to %s: %s\n", id, w)
	}
	if len(s.Assignments) > 0 {
		fmt.Fprintf(&b, "\n%-11s %-6s %-8s %-7s %3s %-9s %6s %6s %6s %6s %-14s  %s\n", "ASSIGNMENT", "CAS", "TEAM", "TRIAGE", "PRI", "STATE", "LEN m", "RUBBLE", "ETA s", "TOOK s", "RESCUE", "NOTE")
		for _, a := range s.Assignments {
			took := "-"
			if a.TookS >= 0 {
				took = fmt.Sprintf("%.0f", a.TookS)
			}
			fmt.Fprintf(&b, "%-11s %-6s %-8s %-7s %3d %-9s %6.0f %6.0f %6.0f %6s %-14s  %s\n", a.ID, a.Casualty, a.Team, a.Triage, a.Priority, a.State, a.LengthM, a.RubbleM, a.ETAs, took, rescueShort(a), a.Note)
		}
	}
	if len(s.Chats) > 0 {
		b.WriteString("\nchat (latest):\n")
		for _, c := range s.Chats {
			fmt.Fprintf(&b, "  %s %s → %s: %s\n", c.At.Format("15:04:05"), c.From, c.To, c.Text)
		}
	}
	return b.String()
}

// rescueShort is "heavy 2 h 10 min" for the assignment table.
func rescueShort(a AssignmentRow) string {
	if a.Rescue == "" {
		return "-"
	}
	class, _, _ := strings.Cut(a.Rescue, ",")
	class = strings.TrimSuffix(class, " rescue")
	if class == "confirm and mark" {
		class = "mark"
	}
	return class + " " + hmin(time.Duration(a.RescueS*float64(time.Second)))
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
