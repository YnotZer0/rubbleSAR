package director

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"rubblesar/internal/aar"
	"rubblesar/internal/live"
	"rubblesar/internal/scenario"
)

// Facts are the run's numbers and words the narration and the panels use.
// `rubblesar demo facts` lists them.
type Facts struct {
	Run      string `json:"run"`
	Incident string `json:"incident"`
	Place    string `json:"place"`
	// CommandPost names the command post's computer ("a GPU-equipped laptop"; the storyboard's command_post).
	CommandPost string `json:"command_post"`
	// PlaceNote follows the place in the narration (", a city heavily
	// damaged in ..." for Antakya; "" otherwise).
	PlaceNote string `json:"place_note"`
	Scenario  string `json:"scenario"`
	// The scenario.
	Sites           int            `json:"sites"`
	Victims         int            `json:"victims"`
	VictimsBy       map[string]int `json:"victims_by"`
	VictimsSentence string         `json:"victims_sentence"`
	// The drones.
	Drones     int    `json:"drones"`
	ScoutAltM  string `json:"scout_alt_m"`
	GSDcm      string `json:"gsd_cm"`
	SegMS      string `json:"seg_ms"`
	Backend    string `json:"backend"`
	Model      string `json:"model"`
	BudgetKbps string `json:"budget_kbps"`
	HoverS     string `json:"hover_s"`
	// The reports.
	Detections  int    `json:"detections"`
	ReportKB    string `json:"report_kb"`
	AvgReportKB string `json:"avg_report_kb"`
	FrameMB     string `json:"frame_mb"`
	FrameSize   string `json:"frame_size"` // "about a megabyte", "1.6 megabytes"
	// The first Red casualty's reading.
	FirstRed        string `json:"first_red"`
	FirstRedSkin    string `json:"first_red_skin"`
	FirstRedAmbient string `json:"first_red_ambient"`
	// The blackout (a drone silent for a while).
	Blackout    bool   `json:"blackout"`
	BlackoutS   string `json:"blackout_s"`
	BlackoutUAV string `json:"blackout_uav"`
	// Lost: reports the drones discarded unsent over the whole run (their queues' count).
	Lost int `json:"lost"`
	// The after-action report.
	Records      int    `json:"records"`
	Found        int    `json:"found"`
	FalseRecords int    `json:"false_records"`
	TriageAgree  int    `json:"triage_agree"`
	TriageOf     int    `json:"triage_compared"`
	RedYellow    int    `json:"red_yellow"`
	Rescued      int    `json:"rescued"`
	MedianFreed  string `json:"median_freed"`
	RealFreeing  string `json:"real_freeing"`
	RealTravel   string `json:"real_travel"`
	RunMinutes   int    `json:"run_minutes"`
	Looks        int    `json:"looks"`
	Teams        int    `json:"teams"`
	TakTeams     string `json:"tak_teams"`
	Chats        int    `json:"chats"`
}

// RunFacts works the facts out from a recorded run, its after-action report,
// the scenario and the rendered media (media.json, when made).
func RunFacts(p *live.Player, rep *aar.Report, sc *scenario.Scenario) *Facts {
	f := &Facts{Run: p.Run, Incident: p.Meta.Incident, Scenario: p.Meta.Scenario, VictimsBy: map[string]int{},
		ScoutAltM: "40", GSDcm: "3", SegMS: "a few tens of", BudgetKbps: "256", HoverS: "30", AvgReportKB: "a few", FrameMB: "1.2",
		FirstRedSkin: "?", FirstRedAmbient: "?", MedianFreed: "-", RealFreeing: "-", RealTravel: "-", Backend: "TensorRT", Model: "YOLO11"}
	f.Place, f.CommandPost = "the incident area", DefaultCommandPost
	name := ""
	if sc != nil {
		name = sc.Name
	}
	if strings.Contains(strings.ToLower(f.Incident+" "+f.Scenario+" "+name), "antakya") {
		f.Place, f.PlaceNote = "central Antakya, in southern Türkiye", ", a city heavily damaged in the February 2023 earthquakes"
	}
	if sc != nil {
		f.Sites, f.Victims = len(sc.Sites), len(sc.Victims)
		for _, v := range sc.Victims {
			f.VictimsBy[v.Status]++
		}
		if sc.Name != "" && f.Place == "the incident area" {
			f.Place = sc.Name
		}
	}
	f.VictimsSentence = victimsSentence(f.VictimsBy)
	f.FrameSize = "about a megabyte"
	// Drones: from the telemetry.
	uavs := map[string]bool{}
	discarded := map[string]uint64{}
	var scoutAlt, seg, hover []float64
	budget := uint32(0)
	for _, e := range p.Events() {
		switch e.K {
		case "tel":
			t := e.Tel
			if t == nil {
				continue
			}
			uavs[t.UAV] = true
			if t.Role == "scout" && t.State == "survey" {
				scoutAlt = append(scoutAlt, t.AltAGL)
			}
			if t.Perc != nil && t.Perc.Seg > 0 {
				seg = append(seg, t.Perc.Seg)
				if t.Perc.Backend != "" {
					f.Backend = t.Perc.Backend
				}
				if t.Perc.Model != "" {
					f.Model = t.Perc.Model
				}
			}
			if t.HoverS > 0 {
				hover = append(hover, float64(t.HoverS))
			}
			if t.Link != nil && t.Link.Budget > 0 && budget == 0 && t.Role == "scout" {
				budget = t.Link.Budget
			}
			if t.Link != nil && t.Link.Discarded > discarded[t.UAV] {
				discarded[t.UAV] = t.Link.Discarded
			}
		case "teams":
			f.Teams = len(e.Teams)
			var tak []string
			for _, t := range e.Teams {
				if t.Source == "tak" {
					tak = append(tak, t.Callsign)
				}
			}
			f.TakTeams = strings.Join(tak, ", ")
		case "chat":
			f.Chats++
		case "task":
			if e.Task != nil && e.Task.State == "done" {
				f.Looks++
			}
		}
	}
	f.Drones = len(uavs)
	for _, n := range discarded {
		f.Lost += int(n)
	}
	if m := median(scoutAlt); m > 0 {
		f.ScoutAltM = fmt.Sprintf("%.0f", m)
		// Ground resolution of the 1920-pixel, 70 degree colour camera looking straight down.
		f.GSDcm = fmt.Sprintf("%.0f", 2*m*math.Tan(35*math.Pi/180)/1920*100)
	}
	if m := median(seg); m > 0 {
		f.SegMS = fmt.Sprintf("%.0f", m)
	}
	if m := median(hover); m > 0 {
		f.HoverS = fmt.Sprintf("%.0f", m)
	}
	if budget > 0 {
		f.BudgetKbps = fmt.Sprintf("%g", math.Round(float64(budget)/1000))
	}
	// Reports.
	var bytes []float64
	total := 0
	for _, d := range p.Detections() {
		bytes = append(bytes, float64(d.Bytes))
		total += d.Bytes
	}
	f.Detections = len(bytes)
	if len(bytes) > 0 {
		f.ReportKB = fmt.Sprintf("%.0f", float64(total)/1024)
		f.AvgReportKB = oneDecimal(mean(bytes) / 1024)
	}
	// The first Red casualty's temperature.
	if t, ok := p.Beat("first-red"); ok {
		for _, b := range p.Beats() {
			if b.Name == "first-red" {
				f.FirstRed = b.Ref
			}
		}
		_ = t
		for _, e := range p.Events() {
			if e.K == "casd" && e.CasD != nil && e.CasD.ID == f.FirstRed && e.CasD.Thermal != nil && e.CasD.Thermal.Readable {
				f.FirstRedSkin, f.FirstRedAmbient = fmt.Sprintf("%.0f", e.CasD.Thermal.Skin), fmt.Sprintf("%.0f", e.CasD.Thermal.Ambient)
				break
			}
		}
		if f.FirstRedSkin == "?" {
			for _, d := range p.Detections() {
				if d.Thermal != nil && d.Thermal.Readable && d.Thermal.Warm {
					f.FirstRedSkin, f.FirstRedAmbient = fmt.Sprintf("%.0f", d.Thermal.Skin), fmt.Sprintf("%.0f", d.Thermal.Ambient)
					break
				}
			}
		}
	}
	// The blackout: the director's beat, else a drone silent for a while.
	var from, to int64
	for _, b := range p.Beats() {
		switch b.Name {
		case "blackout", "outage":
			if from == 0 {
				from, f.BlackoutUAV = b.At, b.Ref
				if b.Name == "outage" {
					from -= 6000 // marked after 6 s of silence (live.Hub): the link went at the last message
				}
			}
		case "blackout-end", "outage-end":
			if to == 0 && from != 0 {
				to = b.At
			}
		}
	}
	if from != 0 && to > from {
		f.Blackout = true
		f.BlackoutS = fmt.Sprintf("%.0f", math.Round(float64(to-from)/1000/5)*5)
		if f.BlackoutS == "0" {
			f.BlackoutS = fmt.Sprintf("%.0f", float64(to-from)/1000)
		}
	}
	// The after-action report.
	if rep != nil {
		k := rep.KPI
		f.Records, f.Found, f.FalseRecords, f.TriageAgree, f.TriageOf = k.Records, k.Found, k.FalseRecords, k.TriageAgree, k.TriageCompared
		f.RedYellow, f.Rescued = k.RedYellow, k.Rescued
		f.MedianFreed, f.RealFreeing, f.RealTravel = spoken(k.FoundToFreedMed), spoken(k.ExtricationRealMed), spoken(k.TravelRealMed)
		f.RunMinutes = int(math.Round(k.Duration.Minutes()))
		if f.Victims == 0 {
			f.Victims = k.Victims
		}
	}
	return f
}

func victimsSentence(by map[string]int) string {
	var parts []string
	add := func(k, what string) {
		if n := by[k]; n > 0 {
			w := fmt.Sprint(n)
			if n < len(numberWords) {
				w = numberWords[n]
			}
			parts = append(parts, w+" "+what)
		}
	}
	add("alive_responsive", "conscious and able to move a little")
	add("alive_unresponsive", "unconscious")
	add("deteriorating", "slowly getting colder")
	add("deceased", "already dead")
	switch len(parts) {
	case 0:
		return "the living and the dead"
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// spoken is a duration for the narration: "45 seconds", "12 minutes", "1 hour 40 minutes".
func spoken(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	if d < 90*time.Second {
		return fmt.Sprintf("%.0f seconds", d.Seconds())
	}
	m := int(math.Round(d.Minutes()))
	if m < 60 {
		return fmt.Sprintf("%d minutes", m)
	}
	h := m / 60
	s := fmt.Sprintf("%d hour", h)
	if h > 1 {
		s += "s"
	}
	if m%60 != 0 {
		s += fmt.Sprintf(" %d minutes", m%60)
	}
	return s
}

func oneDecimal(v float64) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f", v)
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	t := 0.0
	for _, x := range v {
		t += x
	}
	return t / float64(len(v))
}

// AddMedia adds what the rendered media say (the frame's size).
func (f *Facts) AddMedia(m *Media) {
	if m == nil {
		return
	}
	if m.FrameBytes > 0 {
		mb := float64(m.FrameBytes) / 1e6
		f.FrameMB = oneDecimal(mb)
		switch {
		case mb < 0.75:
			f.FrameSize = "about half a megabyte"
		case mb < 1.5:
			f.FrameSize = "about a megabyte"
		default:
			f.FrameSize = oneDecimal(mb) + " megabytes"
		}
	}
	if m.GSDcm > 0 {
		f.GSDcm = fmt.Sprintf("%.0f", m.GSDcm)
	}
}

// LoadMedia reads a run's media.json (nil when not made yet).
func LoadMedia(runDir string) *Media {
	b, err := os.ReadFile(filepath.Join(runDir, "media", "media.json"))
	if err != nil {
		return nil
	}
	var m Media
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}
