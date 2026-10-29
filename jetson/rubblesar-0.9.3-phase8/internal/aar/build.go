package aar

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"rubblesar/internal/geo"
	"rubblesar/internal/sarpb"
	"rubblesar/internal/scenario"
)

// WriteReport builds a recorded run folder's report (index.html,
// report.json, summary.json), comparing with the scenario in scenarioDir and
// using the offline map's overview in mapDir when they exist.
func WriteReport(mapDir, scenarioDir, dir string, triage []string) (*Report, error) {
	o := BuildOptions{MapDir: mapDir, Triage: triage}
	if s, err := scenario.Load(filepath.Join(scenarioDir, "scenario.json")); err == nil {
		o.Scene = s
	}
	r, err := Build(dir, o)
	if err != nil {
		return nil, err
	}
	if err := r.WriteFiles(dir, o); err != nil {
		return nil, err
	}
	return r, nil
}

// Report is the after-action model of one run.
type Report struct {
	Meta        Meta             `json:"meta"`
	Start       time.Time        `json:"start"`
	End         time.Time        `json:"end"`
	Casualties  []*CasRow        `json:"casualties"`
	Victims     []*VictimRow     `json:"victims"`
	Assignments []*AsgRow        `json:"assignments"`
	Teams       []*TeamRow       `json:"teams"`
	Drones      []*DroneRow      `json:"drones"`
	Chats       []ChatLine       `json:"chats"`
	Notes       []string         `json:"notes"`
	KPI         KPIs             `json:"kpi"`
	Tracks      map[string][]LLT `json:"-"`
	Staging     [2]float64       `json:"-"`
	Rubble      [][][2]float64   `json:"-"`
	Sites       []SiteMark       `json:"-"`
	Events      int              `json:"events"`
}

// LLT is a point of a drone's track.
type LLT struct {
	Lat, Lon float64
	T        time.Time
}

// SiteMark is a collapsed building on the map.
type SiteMark struct {
	ID       string
	Lat, Lon float64
}

// TriStep is a triage change of a casualty.
type TriStep struct {
	At     time.Time `json:"at"`
	Triage string    `json:"triage"`
	Prov   bool      `json:"provisional"`
}

// CasRow is one casualty record at the end of the run.
type CasRow struct {
	ID         string    `json:"id"`
	Final      CasSnap   `json:"final"`
	FirstSeen  time.Time `json:"first_seen"`
	Settled    time.Time `json:"settled,omitempty"`
	History    []TriStep `json:"history"`
	LookAt     time.Time `json:"investigated,omitempty"`
	Looks      int       `json:"looks"`
	Asg        *AsgRow   `json:"assignment,omitempty"`
	Victim     string    `json:"victim,omitempty"`
	Expected   string    `json:"expected,omitempty"`
	ErrM       float64   `json:"error_m,omitempty"`
	Rescued    bool      `json:"rescued"`
	MergedFrom []string  `json:"merged_from,omitempty"`
	NeedsTeam  bool      `json:"needs_team"`
}

// VictimRow is a scenario victim and what the system made of it.
type VictimRow struct {
	ID       string  `json:"id"`
	Site     string  `json:"site"`
	Status   string  `json:"status"`
	Expected string  `json:"expected"`
	Record   string  `json:"record,omitempty"`
	Given    string  `json:"given,omitempty"`
	ErrM     float64 `json:"error_m,omitempty"`
	Rescued  bool    `json:"rescued"`
}

// AsgRow is one rescue assignment.
type AsgRow struct {
	ID           string       `json:"id"`
	Cas          string       `json:"casualty"`
	Team         string       `json:"team"`
	Triage       string       `json:"triage"`
	State        string       `json:"state"`
	Needed       time.Time    `json:"needed,omitempty"`
	Created      time.Time    `json:"created"`
	OnScene      time.Time    `json:"on_scene,omitempty"`
	Done         time.Time    `json:"done,omitempty"`
	LengthM      float64      `json:"length_m"`
	RubbleM      float64      `json:"rubble_m"`
	ETAs         float64      `json:"eta_s"`
	Extrication  string       `json:"extrication,omitempty"`
	ExtricationS float64      `json:"extrication_s,omitempty"`
	Streets      []string     `json:"streets,omitempty"`
	Route        [][2]float64 `json:"-"`
	Note         string       `json:"note,omitempty"`
}

// TeamRow sums up a rescue team.
type TeamRow struct {
	ID, Callsign, Mode, Source   string
	Assignments, Done, Cancelled int
	DistanceM                    float64
	EnRoute, OnScene             time.Duration
}

// DroneRow sums up a drone.
type DroneRow struct {
	ID, Role, Software       string
	Airborne                 time.Duration
	DistanceM                float64
	BatteryStart, BatteryEnd float64
	Reports                  uint64
	Looks                    int
}

// KPIs are the headline numbers.
type KPIs struct {
	Victims, Found, Records, FalseRecords int
	TriageCompared, TriageAgree           int
	RedYellow, Reached, Rescued           int
	FirstSeenMed, SettledMed              time.Duration // from the start of the run
	WaitMed, TravelMed, SceneMed          time.Duration // run time
	TravelRealMed, ExtricationRealMed     time.Duration // real-world estimates
	FoundToFreedMed                       time.Duration // run time
	Duration                              time.Duration
}

// BuildOptions are what Build needs besides the run folder.
type BuildOptions struct {
	Scene *scenario.Scenario
	// MapDir is the offline map's folder (overview.png and overview.png.json), "" = none.
	MapDir string
	// Triage lists the colours that get a team (default red, yellow).
	Triage []string
}

// Build reads a run folder (meta.json, events.jsonl).
func Build(dir string, o BuildOptions) (*Report, error) {
	r := &Report{Tracks: map[string][]LLT{}}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("E-AAR-002: %s is not a recorded run: %w", dir, err)
	}
	if err := json.Unmarshal(b, &r.Meta); err != nil {
		return nil, fmt.Errorf("E-AAR-002: meta.json: %w", err)
	}
	if len(o.Triage) == 0 {
		o.Triage = []string{"red", "yellow"}
	}
	r.Staging = r.Meta.Staging
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("E-AAR-002: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	cas := map[string]*CasRow{}
	asg := map[string]*AsgRow{}
	var asgOrder []string
	var last []CasSnap
	tasksDone := map[string]map[string]bool{} // casualty -> task ids done
	looksAt := map[string]time.Time{}
	drones := map[string]*DroneRow{}
	var droneOrder []string
	lastTel := map[string]*TelSnap{}
	lastTelAt := map[string]time.Time{}
	firstTel := map[string]*TelSnap{}
	merged := map[string][]string{}
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		r.Events++
		at := time.UnixMilli(e.T)
		if r.Start.IsZero() || at.Before(r.Start) {
			r.Start = at
		}
		if at.After(r.End) {
			r.End = at
		}
		switch e.K {
		case "cas":
			last = e.Cas
			for _, c := range e.Cas {
				row := cas[c.ID]
				if row == nil {
					row = &CasRow{ID: c.ID, FirstSeen: at}
					if c.First > 0 && time.UnixMilli(c.First).Before(at) {
						row.FirstSeen = time.UnixMilli(c.First)
					}
					cas[c.ID] = row
				}
				h := row.History
				if len(h) == 0 || h[len(h)-1].Triage != c.Triage || h[len(h)-1].Prov != c.Prov {
					row.History = append(row.History, TriStep{At: at, Triage: c.Triage, Prov: c.Prov})
				}
				row.Final = c
			}
		case "merge":
			if len(e.Merge) == 2 {
				merged[e.Merge[1]] = append(merged[e.Merge[1]], e.Merge[0])
			}
		case "task":
			t := e.Task
			if t != nil && t.State == "done" {
				if tasksDone[t.Cas] == nil {
					tasksDone[t.Cas] = map[string]bool{}
				}
				if !tasksDone[t.Cas][t.ID] {
					tasksDone[t.Cas][t.ID] = true
					if looksAt[t.Cas].IsZero() {
						looksAt[t.Cas] = at
					}
				}
				if d := drones[t.UAV]; d != nil {
					d.Looks++
				}
			}
		case "tel":
			t := e.Tel
			if t == nil {
				continue
			}
			d := drones[t.UAV]
			if d == nil {
				d = &DroneRow{ID: t.UAV, Role: t.Role, BatteryStart: t.Battery}
				drones[t.UAV] = d
				droneOrder = append(droneOrder, t.UAV)
				firstTel[t.UAV] = t
			}
			if p := lastTel[t.UAV]; p != nil {
				d.DistanceM += geo.Distance(geo.Point{Lat: p.Lat, Lon: p.Lon}, geo.Point{Lat: t.Lat, Lon: t.Lon})
				if airborne(p.State) {
					d.Airborne += at.Sub(lastTelAt[t.UAV])
				}
			}
			d.BatteryEnd = t.Battery
			if t.Software != "" {
				d.Software = t.Software
			}
			if f := firstTel[t.UAV]; t.Reports >= f.Reports {
				d.Reports = t.Reports - f.Reports
			}
			lastTel[t.UAV], lastTelAt[t.UAV] = t, at
			if airborne(t.State) {
				r.Tracks[t.UAV] = append(r.Tracks[t.UAV], LLT{Lat: t.Lat, Lon: t.Lon, T: at})
			}
		case "asg":
			var a sarpb.Assignment
			if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(e.Asg, &a) != nil {
				continue
			}
			row := asg[a.AssignmentId]
			if row == nil {
				row = &AsgRow{ID: a.AssignmentId}
				asg[a.AssignmentId] = row
				asgOrder = append(asgOrder, a.AssignmentId)
			}
			row.Cas, row.Team = a.CasualtyId, a.TeamId
			row.Triage = strings.ToLower(strings.TrimPrefix(a.Triage.String(), "TRIAGE_"))
			if a.Provisional {
				row.Triage += "?"
			}
			row.State = strings.ToLower(strings.TrimPrefix(a.State.String(), "ASSIGNMENT_STATE_"))
			row.Created, row.Needed = nsTime(a.CreatedUnixNs), nsTime(a.NeededUnixNs)
			row.OnScene, row.Done = nsTime(a.OnSceneUnixNs), nsTime(a.DoneUnixNs)
			if rt := a.Route; rt != nil {
				row.LengthM, row.RubbleM, row.ETAs, row.Streets = float64(rt.LengthM), float64(rt.RubbleM), float64(rt.EtaS), rt.Streets
				row.Route = row.Route[:0]
				for _, p := range rt.Points {
					row.Route = append(row.Route, [2]float64{p.LatDeg, p.LonDeg})
				}
			}
			row.Extrication, row.ExtricationS, row.Note = a.Extrication, float64(a.ExtricationS), a.Note
		case "chat":
			if e.Chat != nil {
				r.Chats = append(r.Chats, *e.Chat)
			}
		case "teams":
			for _, t := range e.Teams {
				if r.findTeam(t.ID) == nil {
					r.Teams = append(r.Teams, &TeamRow{ID: t.ID, Callsign: t.Callsign, Mode: t.Mode, Source: t.Source})
				}
			}
		case "note":
			if e.Note != "" {
				r.Notes = append(r.Notes, fmt.Sprintf("+%s %s", clock(at.Sub(r.Start)), e.Note))
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("E-AAR-002: reading events: %w", err)
	}
	if !r.Meta.Started.IsZero() && r.Meta.Started.Before(r.Start) {
		r.Start = r.Meta.Started
	}
	if !r.Meta.Ended.IsZero() && r.Meta.Ended.After(r.End) {
		r.End = r.Meta.Ended
	}
	// The casualties of the last list are the run's records.
	needs := func(tri string) bool {
		for _, t := range o.Triage {
			if strings.EqualFold(t, tri) {
				return true
			}
		}
		return false
	}
	for _, c := range last {
		row := cas[c.ID]
		row.MergedFrom = merged[c.ID]
		row.NeedsTeam = needs(c.Triage)
		row.LookAt = looksAt[c.ID]
		row.Looks = len(tasksDone[c.ID])
		for i := len(row.History) - 1; i >= 0; i-- {
			if row.History[i].Prov {
				break
			}
			row.Settled = row.History[i].At
		}
		r.Casualties = append(r.Casualties, row)
	}
	sort.Slice(r.Casualties, func(i, j int) bool { return r.Casualties[i].ID < r.Casualties[j].ID })
	for _, id := range asgOrder {
		a := asg[id]
		r.Assignments = append(r.Assignments, a)
		if c := cas[a.Cas]; c != nil && a.State != "cancelled" {
			if c.Asg == nil || a.Created.After(c.Asg.Created) || c.Asg.State == "cancelled" {
				c.Asg = a
			}
		}
		t := r.findTeam(a.Team)
		if t == nil {
			t = &TeamRow{ID: a.Team, Callsign: strings.ToUpper(a.Team)}
			r.Teams = append(r.Teams, t)
		}
		t.Assignments++
		switch a.State {
		case "done":
			t.Done++
		case "cancelled":
			t.Cancelled++
		}
		if !a.OnScene.IsZero() {
			t.DistanceM += a.LengthM
			t.EnRoute += a.OnScene.Sub(a.Created)
			end := a.Done
			if end.IsZero() {
				end = r.End
			}
			t.OnScene += end.Sub(a.OnScene)
		}
	}
	for _, c := range r.Casualties {
		c.Rescued = c.Asg != nil && c.Asg.State == "done" && c.NeedsTeam
	}
	for _, id := range droneOrder {
		r.Drones = append(r.Drones, drones[id])
	}
	if s := o.Scene; s != nil {
		r.compare(s)
		for _, b := range s.Blockages {
			var ring [][2]float64
			for _, p := range b.Polygon {
				ring = append(ring, [2]float64{p.Lat, p.Lon})
			}
			r.Rubble = append(r.Rubble, ring)
		}
		for _, st := range s.Sites {
			r.Sites = append(r.Sites, SiteMark{ID: st.ID, Lat: st.Center.Lat, Lon: st.Center.Lon})
		}
	}
	r.kpis()
	return r, nil
}

func (r *Report) findTeam(id string) *TeamRow {
	for _, t := range r.Teams {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func nsTime(ns int64) time.Time {
	if ns <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

func airborne(state string) bool {
	switch state {
	case "", "landed", "idle", "standby", "unknown", "unspecified", "disarmed":
		return false
	}
	return true
}

// victimMatchM: a record this close to a scenario victim is that victim's.
const victimMatchM = 6.0

// compare matches records to the scenario's victims one to one, nearest first.
func (r *Report) compare(s *scenario.Scenario) {
	type pair struct {
		v, c int
		d    float64
	}
	var pairs []pair
	for i, v := range s.Victims {
		for j, c := range r.Casualties {
			d := geo.Distance(geo.Point{Lat: v.Pos.Lat, Lon: v.Pos.Lon}, geo.Point{Lat: c.Final.Lat, Lon: c.Final.Lon})
			if d <= victimMatchM {
				pairs = append(pairs, pair{i, j, d})
			}
		}
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].d < pairs[b].d })
	vUsed, cUsed := map[int]bool{}, map[int]bool{}
	rows := make([]*VictimRow, len(s.Victims))
	for i, v := range s.Victims {
		rows[i] = &VictimRow{ID: v.ID, Site: v.SiteID, Status: strings.ReplaceAll(v.Status, "_", " "), Expected: v.ExpectedTriage}
	}
	for _, p := range pairs {
		if vUsed[p.v] || cUsed[p.c] {
			continue
		}
		vUsed[p.v], cUsed[p.c] = true, true
		c := r.Casualties[p.c]
		row := rows[p.v]
		row.Record, row.Given, row.ErrM, row.Rescued = c.ID, c.Final.Triage, math.Round(p.d*10)/10, c.Rescued
		c.Victim, c.Expected, c.ErrM = row.ID, row.Expected, row.ErrM
	}
	r.Victims = rows
}

func (r *Report) kpis() {
	k := &r.KPI
	k.Duration = r.End.Sub(r.Start)
	k.Records = len(r.Casualties)
	k.Victims = len(r.Victims)
	var first, settled, wait, travel, scene, travelReal, extr, freed []time.Duration
	for _, v := range r.Victims {
		if v.Record != "" {
			k.Found++
			if v.Expected != "" {
				k.TriageCompared++
				if strings.EqualFold(v.Expected, v.Given) {
					k.TriageAgree++
				}
			}
		}
	}
	if k.Victims > 0 {
		k.FalseRecords = k.Records - k.Found
	}
	for _, c := range r.Casualties {
		first = append(first, c.FirstSeen.Sub(r.Start))
		if !c.Settled.IsZero() {
			settled = append(settled, c.Settled.Sub(r.Start))
		}
		if !c.NeedsTeam {
			continue
		}
		k.RedYellow++
		a := c.Asg
		if a == nil {
			continue
		}
		if !a.Needed.IsZero() {
			wait = append(wait, a.Created.Sub(a.Needed))
		}
		if !a.OnScene.IsZero() {
			k.Reached++
			travel = append(travel, a.OnScene.Sub(a.Created))
			travelReal = append(travelReal, time.Duration(a.ETAs*float64(time.Second)))
		}
		if a.ExtricationS > 0 {
			extr = append(extr, time.Duration(a.ExtricationS*float64(time.Second)))
		}
		if c.Rescued {
			k.Rescued++
			scene = append(scene, a.Done.Sub(a.OnScene))
			freed = append(freed, a.Done.Sub(c.FirstSeen))
		}
	}
	k.FirstSeenMed, k.SettledMed = median(first), median(settled)
	k.WaitMed, k.TravelMed, k.SceneMed = median(wait), median(travel), median(scene)
	k.TravelRealMed, k.ExtricationRealMed, k.FoundToFreedMed = median(travelReal), median(extr), median(freed)
}

func median(v []time.Duration) time.Duration {
	if len(v) == 0 {
		return -1
	}
	s := append([]time.Duration(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// clock is "mm:ss" or "h:mm:ss" of a run offset.
func clock(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	s := int(d.Round(time.Second).Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// span is "45 s", "3 min", "1 h 20 min".
func span(d time.Duration) string {
	if d < 0 {
		return "-"
	}
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

// Summary is the report in one line (for sar-web's index and the console).
func (r *Report) Summary() string {
	k := r.KPI
	s := fmt.Sprintf("%d casualties", k.Records)
	if k.Victims > 0 {
		s = fmt.Sprintf("%d of %d victims found (%d casualty records)", k.Found, k.Victims, k.Records)
	}
	s += fmt.Sprintf("; %d of %d Red/Yellow rescued", k.Rescued, k.RedYellow)
	if k.FoundToFreedMed >= 0 {
		s += fmt.Sprintf(", median %s from first sighting to freed", span(k.FoundToFreedMed))
	}
	return s + "; run " + span(k.Duration)
}

// Title names the report.
func (r *Report) Title() string {
	return fmt.Sprintf("%s run %s, %s", r.Meta.Incident, nonEmpty(r.Meta.Run, "?"), r.Start.Local().Format("2 Jan 2006 15:04"))
}

func nonEmpty(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}
