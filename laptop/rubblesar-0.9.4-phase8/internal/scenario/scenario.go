// Package scenario defines the simulated incident: collapse sites, victims
// with their exposed body parts, thermal and movement models, and blocked
// areas. A scenario is generated once from a seeded configuration and saved as
// scenario.json; everything else (imagery, scoring, later routing) reads it.
package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/thermal"
)

// SchemaVersion of scenario.json.
const SchemaVersion = 1

// Body part classes used in labels and detections (0 = background).
const (
	PartNone  = 0
	PartHand  = 1
	PartArm   = 2
	PartLeg   = 3
	PartHead  = 4
	PartTorso = 5
	NumParts  = 6
)

// PartNames maps class ids to names.
var PartNames = [NumParts]string{"background", "hand", "arm", "leg", "head", "torso"}

// Exposure kinds: what sticks out of the rubble, and which classes it shows.
const (
	ExpHand          = "hand"           // hand only
	ExpForearmHand   = "forearm_hand"   // forearm + hand
	ExpArmFull       = "arm_full"       // upper arm + forearm + hand
	ExpFoot          = "foot"           // foot and ankle
	ExpLowerLeg      = "lower_leg"      // shin + foot
	ExpHead          = "head"           // head
	ExpHeadShoulders = "head_shoulders" // head + upper back
	ExpBack          = "back"           // part of the back/torso
)

// ExposureClasses lists the part classes visible for each exposure kind.
var ExposureClasses = map[string][]int{
	ExpHand:          {PartHand},
	ExpForearmHand:   {PartArm, PartHand},
	ExpArmFull:       {PartArm, PartHand},
	ExpFoot:          {PartLeg},
	ExpLowerLeg:      {PartLeg},
	ExpHead:          {PartHead},
	ExpHeadShoulders: {PartHead, PartTorso},
	ExpBack:          {PartTorso},
}

// LatLon is a WGS84 position.
type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Site is one collapsed building.
type Site struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"` // pancake | mound
	Source    string       `json:"source"`
	Center    LatLon       `json:"center"`
	CenterE   float64      `json:"center_e_m"` // local metres east of the incident centre
	CenterN   float64      `json:"center_n_m"`
	Footprint [][2]float64 `json:"footprint_en_m"` // polygon, local metres (east, north)
	HeightM   float64      `json:"pile_height_m"`
	SpillM    float64      `json:"spill_m"`
	Seed      int64        `json:"seed"`
}

// Poly returns the footprint as a geometry polygon.
func (s Site) Poly() geom.Poly {
	p := make(geom.Poly, len(s.Footprint))
	for i, v := range s.Footprint {
		p[i] = geom.P{X: v[0], Y: v[1]}
	}
	return p
}

// Exposed is one body region visible above the rubble.
type Exposed struct {
	Kind   string  `json:"kind"`
	Side   string  `json:"side,omitempty"` // left | right for limbs
	DirDeg float64 `json:"dir_deg"`        // direction the limb points, clockwise from north
	// Anchor is where the region emerges from the rubble, local metres.
	AnchorE float64 `json:"anchor_e_m"`
	AnchorN float64 `json:"anchor_n_m"`
}

// Movement makes a victim visibly move (alternating between two poses).
type Movement struct {
	Moves     bool    `json:"moves"`
	PeriodS   float64 `json:"period_s,omitempty"`
	DurationS float64 `json:"duration_s,omitempty"`
	PhaseS    float64 `json:"phase_s,omitempty"`
	SwingDeg  float64 `json:"swing_deg,omitempty"`
}

// MovingAt reports whether the victim shows its alternate pose at elapsed seconds.
func (m Movement) MovingAt(elapsedS float64) bool {
	if !m.Moves || m.PeriodS <= 0 {
		return false
	}
	x := math.Mod(elapsedS+m.PhaseS, m.PeriodS)
	if x < 0 {
		x += m.PeriodS
	}
	return x < m.DurationS
}

// Appearance controls colours in the imagery.
type Appearance struct {
	SkinTone   int     `json:"skin_tone"`   // index into the render palette
	ClothColor int     `json:"cloth_color"` // index into the render palette
	HairColor  int     `json:"hair_color"`
	Dust       float64 `json:"dust"` // 0 clean .. 1 fully dust-covered
	Scale      float64 `json:"scale"`
}

// Victim is one trapped person.
type Victim struct {
	ID             string              `json:"id"`
	Index          int                 `json:"index"` // 1..N, used in label images
	SiteID         string              `json:"site_id"`
	Pos            LatLon              `json:"pos"`
	E              float64             `json:"e_m"`
	N              float64             `json:"n_m"`
	HeadingDeg     float64             `json:"heading_deg"`
	Status         string              `json:"status"`
	ExpectedTriage string              `json:"expected_triage"`
	Exposed        []Exposed           `json:"exposed"`
	Thermal        thermal.VictimModel `json:"thermal"`
	Movement       Movement            `json:"movement"`
	Look           Appearance          `json:"appearance"`
	OcclusionFrac  float64             `json:"occlusion_frac"` // target share of exposed area hidden by debris
}

// Classes returns the set of part classes this victim shows.
func (v Victim) Classes() []int {
	seen := map[int]bool{}
	var out []int
	for _, e := range v.Exposed {
		for _, c := range ExposureClasses[e.Kind] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// Blockage is an area routes must avoid (rubble spill), as a lat/lon polygon.
type Blockage struct {
	ID      string   `json:"id"`
	SiteID  string   `json:"site_id"`
	Polygon []LatLon `json:"polygon"`
}

// Scenario is the whole simulated incident.
type Scenario struct {
	Schema       int                 `json:"schema"`
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Seed         int64               `json:"seed"`
	Center       LatLon              `json:"center"`
	GroundAltMSL float64             `json:"ground_alt_msl_m"`
	RadiusM      float64             `json:"radius_m"`
	Start        string              `json:"start"` // RFC3339 with local offset: scenario clock zero
	Env          thermal.Environment `json:"environment"`
	GSDm         float64             `json:"render_gsd_m"`
	MarginM      float64             `json:"render_margin_m"`
	Sites        []Site              `json:"sites"`
	Victims      []Victim            `json:"victims"`
	Blockages    []Blockage          `json:"blockages"`
}

// StartTime parses Start.
func (s *Scenario) StartTime() (time.Time, error) {
	return time.Parse(time.RFC3339, s.Start)
}

// Local returns the tangent plane at the incident centre.
func (s *Scenario) Local() geo.Local {
	return geo.NewLocal(geo.Point{Lat: s.Center.Lat, Lon: s.Center.Lon})
}

// Site returns a site by id.
func (s *Scenario) Site(id string) (*Site, bool) {
	for i := range s.Sites {
		if s.Sites[i].ID == id {
			return &s.Sites[i], true
		}
	}
	return nil, false
}

// VictimsAt returns the victims of one site.
func (s *Scenario) VictimsAt(siteID string) []Victim {
	var out []Victim
	for _, v := range s.Victims {
		if v.SiteID == siteID {
			out = append(out, v)
		}
	}
	return out
}

// VictimByIndex returns the victim with a label index.
func (s *Scenario) VictimByIndex(idx int) (*Victim, bool) {
	if idx >= 1 && idx <= len(s.Victims) && s.Victims[idx-1].Index == idx {
		return &s.Victims[idx-1], true
	}
	for i := range s.Victims {
		if s.Victims[i].Index == idx {
			return &s.Victims[i], true
		}
	}
	return nil, false
}

// Save writes scenario.json (indented, stable field order) into dir.
func (s *Scenario) Save(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "scenario.json")
	return p, os.WriteFile(p, append(b, '\n'), 0o644)
}

// Hash returns the SHA-256 of the canonical JSON encoding.
func (s *Scenario) Hash() string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Load reads and validates scenario.json from a file or directory.
func Load(path string) (*Scenario, error) {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "scenario.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, diag.Wrap("E-SCN-001", err, "reading %s", path)
	}
	var s Scenario
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, diag.Wrap("E-SCN-002", err, "parsing %s", path)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks internal consistency.
func (s *Scenario) Validate() error {
	var probs []string
	add := func(f string, a ...any) { probs = append(probs, fmt.Sprintf(f, a...)) }
	if s.Schema != SchemaVersion {
		add("schema %d, expected %d", s.Schema, SchemaVersion)
	}
	if s.ID == "" {
		add("missing id")
	}
	if _, err := s.StartTime(); err != nil {
		add("start %q is not RFC3339: %v", s.Start, err)
	}
	if s.RadiusM <= 0 || s.GSDm <= 0 || s.GSDm > 0.1 {
		add("radius_m and render_gsd_m (0 < gsd <= 0.1) must be positive")
	}
	if s.Env.AirMaxC < s.Env.AirMinC {
		add("environment air_max_c below air_min_c")
	}
	siteIDs := map[string]bool{}
	for i, st := range s.Sites {
		if siteIDs[st.ID] {
			add("duplicate site id %s", st.ID)
		}
		siteIDs[st.ID] = true
		if len(st.Footprint) < 3 {
			add("site %s footprint has %d vertices", st.ID, len(st.Footprint))
			continue
		}
		if math.Hypot(st.CenterE, st.CenterN) > s.RadiusM {
			add("site %s centre is %.0f m from the incident centre (radius %.0f)", st.ID, math.Hypot(st.CenterE, st.CenterN), s.RadiusM)
		}
		for j := i + 1; j < len(s.Sites); j++ {
			if len(s.Sites[j].Footprint) >= 3 && geom.PolyDist(st.Poly(), s.Sites[j].Poly()) < st.SpillM+s.Sites[j].SpillM {
				add("sites %s and %s overlap including spill", st.ID, s.Sites[j].ID)
			}
		}
		if st.HeightM <= 0 || st.HeightM > 15 {
			add("site %s pile height %.1f m out of range", st.ID, st.HeightM)
		}
	}
	vIDs := map[string]bool{}
	for i, v := range s.Victims {
		if vIDs[v.ID] {
			add("duplicate victim id %s", v.ID)
		}
		vIDs[v.ID] = true
		if v.Index != i+1 {
			add("victim %s index %d, expected %d (indices are 1..N in order)", v.ID, v.Index, i+1)
		}
		st, ok := s.Site(v.SiteID)
		if !ok {
			add("victim %s on unknown site %s", v.ID, v.SiteID)
			continue
		}
		if !st.Poly().Contains(geom.P{X: v.E, Y: v.N}) {
			add("victim %s is outside the footprint of %s", v.ID, st.ID)
		}
		switch v.Status {
		case thermal.AliveResponsive, thermal.AliveUnresponsive, thermal.Deteriorating, thermal.Deceased:
		default:
			add("victim %s status %q unknown", v.ID, v.Status)
		}
		if v.Thermal.Status != v.Status {
			add("victim %s thermal status %q differs from status %q", v.ID, v.Thermal.Status, v.Status)
		}
		if len(v.Exposed) == 0 {
			add("victim %s has no exposed parts", v.ID)
		}
		for _, e := range v.Exposed {
			if _, ok := ExposureClasses[e.Kind]; !ok {
				add("victim %s exposure %q unknown", v.ID, e.Kind)
			}
		}
		if v.Movement.Moves && (v.Movement.PeriodS <= v.Movement.DurationS || v.Movement.DurationS <= 0) {
			add("victim %s movement period must exceed duration", v.ID)
		}
	}
	if len(probs) > 0 {
		return &diag.Error{Code: "E-SCN-003", Detail: fmt.Sprintf("scenario %s invalid: %v", s.ID, probs)}
	}
	return nil
}

// ExpectedTriage maps a status to the colour the plan's rules should produce.
func ExpectedTriage(status string) string {
	switch status {
	case thermal.AliveResponsive:
		return "yellow"
	case thermal.AliveUnresponsive, thermal.Deteriorating:
		return "red"
	case thermal.Deceased:
		return "black"
	}
	return "unknown"
}
