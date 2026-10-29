package scenario

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/geom"
	"rubblesar/internal/keys"
	"rubblesar/internal/thermal"
)

// Config is the generator input (configs/scenario/*.toml).
type Config struct {
	Scenario struct {
		ID    string `toml:"id"`
		Name  string `toml:"name"`
		Seed  int64  `toml:"seed"`
		Start string `toml:"start"`
	} `toml:"scenario"`
	Site struct {
		CenterLat     float64 `toml:"center_lat"`
		CenterLon     float64 `toml:"center_lon"`
		GroundAltMSLm float64 `toml:"ground_alt_msl_m"`
		RadiusM       float64 `toml:"radius_m"`
	} `toml:"site"`
	Environment thermal.Environment `toml:"environment"`
	Sites       struct {
		Count          int     `toml:"count"`
		MinSizeM       float64 `toml:"min_size_m"`
		MaxSizeM       float64 `toml:"max_size_m"`
		MaxFromCentreM float64 `toml:"max_from_centre_m"`
		MinGapM        float64 `toml:"min_gap_m"`
		MinHeightM     float64 `toml:"min_height_m"`
		MaxHeightM     float64 `toml:"max_height_m"`
		SpillM         float64 `toml:"spill_m"`
		OSMBuildings   string  `toml:"osm_buildings"`
	} `toml:"sites"`
	Victims struct {
		PerSiteMin            int     `toml:"per_site_min"`
		PerSiteMax            int     `toml:"per_site_max"`
		AliveFraction         float64 `toml:"alive_fraction"`
		ResponsiveFraction    float64 `toml:"responsive_fraction"`
		DeterioratingFraction float64 `toml:"deteriorating_fraction"`
		MinSpacingM           float64 `toml:"min_spacing_m"`
	} `toml:"victims"`
	Render struct {
		GSDm    float64 `toml:"gsd_m"`
		MarginM float64 `toml:"margin_m"`
	} `toml:"render"`
}

// DefaultConfig returns the built-in demo configuration (Antakya).
func DefaultConfig() Config {
	var c Config
	c.Scenario.ID, c.Scenario.Name, c.Scenario.Seed = "antakya-demo", "Antakya city centre collapse (simulated)", 2023
	c.Scenario.Start = "2026-02-10T09:00:00+03:00"
	c.Site.CenterLat, c.Site.CenterLon, c.Site.GroundAltMSLm, c.Site.RadiusM = 36.20250, 36.16056, 80, 805
	c.Environment = thermal.Environment{AirMinC: 3, AirMaxC: 14, SunPeakGainC: 12}
	c.Sites.Count, c.Sites.MinSizeM, c.Sites.MaxSizeM = 5, 18, 36
	c.Sites.MaxFromCentreM, c.Sites.MinGapM = 350, 30
	c.Sites.MinHeightM, c.Sites.MaxHeightM, c.Sites.SpillM = 2, 6, 5
	c.Victims.PerSiteMin, c.Victims.PerSiteMax = 4, 6
	c.Victims.AliveFraction, c.Victims.ResponsiveFraction, c.Victims.DeterioratingFraction = 0.65, 0.5, 0.2
	c.Victims.MinSpacingM = 3
	c.Render.GSDm, c.Render.MarginM = 0.015, 6
	return c
}

// LoadConfig reads a generator config, starting from the defaults.
func LoadConfig(path string) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, diag.Wrap("E-CFG-001", err, "%s", path)
	}
	md, err := toml.Decode(string(b), &c)
	if err != nil {
		return c, diag.Wrap("E-CFG-002", err, "%s", path)
	}
	if und := md.Undecoded(); len(und) > 0 {
		names := make([]string, len(und))
		for i, k := range und {
			names[i] = k.String()
		}
		return c, diag.New("E-CFG-003", "%s: unknown setting(s) %s", path, strings.Join(names, ", "))
	}
	if c.Sites.OSMBuildings != "" && !filepath.IsAbs(c.Sites.OSMBuildings) {
		c.Sites.OSMBuildings = filepath.Join(filepath.Dir(path), c.Sites.OSMBuildings)
	}
	return c, c.Validate()
}

// GenerateFromConfig loads OpenStreetMap outlines when configured, then generates.
func GenerateFromConfig(c Config) (*Scenario, error) {
	var fps []Footprint
	if c.Sites.OSMBuildings != "" {
		local := geo.NewLocal(geo.Point{Lat: c.Site.CenterLat, Lon: c.Site.CenterLon})
		var err error
		if fps, err = LoadOverpassFile(c.Sites.OSMBuildings, local); err != nil {
			return nil, err
		}
	}
	return Generate(c, fps)
}

// Validate checks the generator config.
func (c Config) Validate() error {
	var p []string
	if err := keys.ValidateID("incident", c.Scenario.ID); err != nil {
		p = append(p, err.Error())
	}
	if _, err := time.Parse(time.RFC3339, c.Scenario.Start); err != nil {
		p = append(p, "[scenario] start must be RFC3339 with an offset, e.g. 2026-02-10T09:00:00+03:00")
	}
	if c.Sites.Count < 1 || c.Sites.Count > 30 {
		p = append(p, "[sites] count must be 1..30")
	}
	if c.Sites.MinSizeM < 8 || c.Sites.MaxSizeM < c.Sites.MinSizeM || c.Sites.MaxSizeM > 80 {
		p = append(p, "[sites] sizes must satisfy 8 <= min_size_m <= max_size_m <= 80")
	}
	if c.Sites.MaxFromCentreM <= 0 || c.Sites.MaxFromCentreM > c.Site.RadiusM {
		p = append(p, "[sites] max_from_centre_m must be within the incident radius")
	}
	if c.Victims.PerSiteMin < 1 || c.Victims.PerSiteMax < c.Victims.PerSiteMin || c.Victims.PerSiteMax > 20 {
		p = append(p, "[victims] need 1 <= per_site_min <= per_site_max <= 20")
	}
	for _, f := range []float64{c.Victims.AliveFraction, c.Victims.ResponsiveFraction, c.Victims.DeterioratingFraction} {
		if f < 0 || f > 1 {
			p = append(p, "[victims] fractions must be within 0..1")
			break
		}
	}
	if c.Render.GSDm < 0.005 || c.Render.GSDm > 0.05 {
		p = append(p, "[render] gsd_m must be within 0.005..0.05")
	}
	if len(p) > 0 {
		return diag.New("E-CFG-003", "scenario config: %s", strings.Join(p, "; "))
	}
	return nil
}

// Footprint is a candidate building outline (from OpenStreetMap).
type Footprint struct {
	ID   string
	Poly geom.Poly
}

// Generate builds a scenario. footprints may be nil (procedural sites).
func Generate(c Config, footprints []Footprint) (*Scenario, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewSource(c.Scenario.Seed))
	s := &Scenario{
		Schema: SchemaVersion, ID: c.Scenario.ID, Name: c.Scenario.Name, Seed: c.Scenario.Seed,
		Center:       LatLon{c.Site.CenterLat, c.Site.CenterLon},
		GroundAltMSL: c.Site.GroundAltMSLm, RadiusM: c.Site.RadiusM, Start: c.Scenario.Start,
		Env: c.Environment, GSDm: c.Render.GSDm, MarginM: c.Render.MarginM,
	}
	local := s.Local()
	polys, sources := pickSites(rng, c, footprints)
	if len(polys) < c.Sites.Count {
		return nil, diag.New("E-SCN-004", "could place only %d of %d sites; reduce [sites] count or min_gap_m, or raise max_from_centre_m", len(polys), c.Sites.Count)
	}
	for i, poly := range polys {
		ctr := poly.Centroid()
		pt := local.ToPoint(ctr.X, ctr.Y)
		kind := "pancake"
		if rng.Float64() < 0.4 {
			kind = "mound"
		}
		fp := make([][2]float64, len(poly))
		for j, q := range poly {
			fp[j] = [2]float64{round3(q.X), round3(q.Y)}
		}
		st := Site{
			ID: fmt.Sprintf("s%02d", i+1), Kind: kind, Source: sources[i],
			Center: LatLon{round7(pt.Lat), round7(pt.Lon)}, CenterE: round3(ctr.X), CenterN: round3(ctr.Y),
			Footprint: fp, HeightM: round2(c.Sites.MinHeightM + rng.Float64()*(c.Sites.MaxHeightM-c.Sites.MinHeightM)),
			SpillM: c.Sites.SpillM, Seed: rng.Int63(),
		}
		s.Sites = append(s.Sites, st)
		s.Blockages = append(s.Blockages, Blockage{ID: "b" + st.ID[1:], SiteID: st.ID, Polygon: blockagePoly(local, poly, st.SpillM)})
	}
	for _, st := range s.Sites {
		n := c.Victims.PerSiteMin + rng.Intn(c.Victims.PerSiteMax-c.Victims.PerSiteMin+1)
		for _, v := range placeVictims(rng, c, st, n) {
			v.Index = len(s.Victims) + 1
			v.ID = fmt.Sprintf("v%02d", v.Index)
			pt := local.ToPoint(v.E, v.N)
			v.Pos = LatLon{round7(pt.Lat), round7(pt.Lon)}
			s.Victims = append(s.Victims, v)
		}
	}
	return s, s.Validate()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
func round7(v float64) float64 { return math.Round(v*1e7) / 1e7 }

func pickSites(rng *rand.Rand, c Config, footprints []Footprint) ([]geom.Poly, []string) {
	var polys []geom.Poly
	var sources []string
	ok := func(p geom.Poly) bool {
		ctr := p.Centroid()
		if ctr.Len() > c.Sites.MaxFromCentreM {
			return false
		}
		for _, q := range polys {
			if geom.PolyDist(p, q) < c.Sites.MinGapM+2*c.Sites.SpillM {
				return false
			}
		}
		return true
	}
	if len(footprints) > 0 {
		cands := make([]Footprint, 0, len(footprints))
		for _, f := range footprints {
			a := f.Poly.Area()
			b := f.Poly.Bounds()
			if a >= c.Sites.MinSizeM*c.Sites.MinSizeM*0.6 && b.Max.X-b.Min.X <= c.Sites.MaxSizeM*1.6 && b.Max.Y-b.Min.Y <= c.Sites.MaxSizeM*1.6 {
				cands = append(cands, f)
			}
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].ID < cands[j].ID })
		rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
		for _, f := range cands {
			if len(polys) == c.Sites.Count {
				break
			}
			if ok(f.Poly) {
				polys = append(polys, f.Poly)
				sources = append(sources, "osm:"+f.ID)
			}
		}
	}
	for tries := 0; len(polys) < c.Sites.Count && tries < 20000; tries++ {
		r := c.Sites.MaxFromCentreM * math.Sqrt(rng.Float64())
		a := rng.Float64() * 2 * math.Pi
		l := c.Sites.MinSizeM + rng.Float64()*(c.Sites.MaxSizeM-c.Sites.MinSizeM)
		w := c.Sites.MinSizeM + rng.Float64()*(l-c.Sites.MinSizeM)
		p := geom.RotRect(geom.P{X: r * math.Cos(a), Y: r * math.Sin(a)}, l, w, rng.Float64()*180)
		if ok(p) {
			polys = append(polys, p)
			sources = append(sources, "procedural")
		}
	}
	return polys, sources
}

// blockagePoly pushes each footprint vertex outward from the centroid by spill metres.
func blockagePoly(local geo.Local, p geom.Poly, spill float64) []LatLon {
	c := p.Centroid()
	out := make([]LatLon, len(p))
	for i, q := range p {
		d := q.Sub(c)
		l := d.Len()
		if l > 0 {
			q = q.Add(d.Scale(spill / l * math.Sqrt2))
		}
		pt := local.ToPoint(q.X, q.Y)
		out[i] = LatLon{round7(pt.Lat), round7(pt.Lon)}
	}
	return out
}

type pattern struct {
	weight float64
	kinds  []string
}

// Exposure patterns and their relative frequency.
var patterns = []pattern{
	{0.18, []string{ExpHand}},
	{0.20, []string{ExpForearmHand}},
	{0.07, []string{ExpArmFull}},
	{0.12, []string{ExpLowerLeg}},
	{0.05, []string{ExpFoot}},
	{0.08, []string{ExpHead}},
	{0.10, []string{ExpHeadShoulders, ExpForearmHand}},
	{0.08, []string{ExpBack, ExpForearmHand}},
	{0.12, []string{ExpLowerLeg, ExpHand}},
}

func pickPattern(rng *rand.Rand) []string {
	total := 0.0
	for _, p := range patterns {
		total += p.weight
	}
	x := rng.Float64() * total
	for _, p := range patterns {
		if x < p.weight {
			return p.kinds
		}
		x -= p.weight
	}
	return patterns[0].kinds
}

// Body layout offsets (metres) in the victim frame: forward = towards the head.
func bodyAnchor(kind, side string, scale float64) (fwd, right float64) {
	sgn := 1.0
	if side == "left" {
		sgn = -1
	}
	switch kind {
	case ExpHead, ExpHeadShoulders:
		return 0.62 * scale, 0
	case ExpBack:
		return 0.1 * scale, 0
	case ExpHand, ExpForearmHand, ExpArmFull:
		return 0.45 * scale, 0.22 * scale * sgn
	case ExpLowerLeg, ExpFoot:
		return -0.55 * scale, 0.12 * scale * sgn
	}
	return 0, 0
}

func placeVictims(rng *rand.Rand, c Config, st Site, n int) []Victim {
	poly := st.Poly()
	b := poly.Bounds()
	var out []Victim
	for tries := 0; len(out) < n && tries < 5000; tries++ {
		p := geom.P{X: b.Min.X + rng.Float64()*(b.Max.X-b.Min.X), Y: b.Min.Y + rng.Float64()*(b.Max.Y-b.Min.Y)}
		if !poly.Contains(p) || poly.EdgeDist(p) < 1.5 {
			continue
		}
		clash := false
		for _, v := range out {
			if math.Hypot(v.E-p.X, v.N-p.Y) < c.Victims.MinSpacingM {
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		out = append(out, newVictim(rng, c, st, p))
	}
	return out
}

func newVictim(rng *rand.Rand, c Config, st Site, p geom.P) Victim {
	v := Victim{SiteID: st.ID, E: round3(p.X), N: round3(p.Y), HeadingDeg: round2(rng.Float64() * 360)}
	// Status.
	if rng.Float64() < c.Victims.AliveFraction {
		x := rng.Float64()
		switch {
		case x < c.Victims.ResponsiveFraction:
			v.Status = thermal.AliveResponsive
		case x < c.Victims.ResponsiveFraction+c.Victims.DeterioratingFraction:
			v.Status = thermal.Deteriorating
		default:
			v.Status = thermal.AliveUnresponsive
		}
	} else {
		v.Status = thermal.Deceased
	}
	v.ExpectedTriage = ExpectedTriage(v.Status)
	// Thermal model (illustrative values, see package thermal).
	v.Thermal.Status = v.Status
	switch v.Status {
	case thermal.Deceased:
		v.Thermal.DeathHours = round2(-1 - rng.Float64()*19)
		start, _ := time.Parse(time.RFC3339, c.Scenario.Start)
		air := c.Environment.AirC(start.Add(time.Duration(v.Thermal.DeathHours * float64(time.Hour))))
		v.Thermal.SkinAtDeath = round2(air + (0.6+0.15*rng.Float64())*(thermal.CoreC-air))
		v.Thermal.KPerH = round3(0.15 + rng.Float64()*0.2)
	default:
		v.Thermal.Alpha0 = round3(0.55 + rng.Float64()*0.25)
		if v.Status == thermal.Deteriorating {
			v.Thermal.BetaPerH = round3(0.03 + rng.Float64()*0.05)
		}
	}
	// Movement: only responsive victims move.
	if v.Status == thermal.AliveResponsive {
		v.Movement = Movement{Moves: true, PeriodS: round2(6 + rng.Float64()*14), DurationS: round2(1.5 + rng.Float64()*1.5),
			PhaseS: round2(rng.Float64() * 20), SwingDeg: round2(12 + rng.Float64()*18)}
	}
	v.Look = Appearance{SkinTone: rng.Intn(6), ClothColor: rng.Intn(8), HairColor: rng.Intn(4),
		Dust: round2(rng.Float64() * 0.6), Scale: round3(0.9 + rng.Float64()*0.2)}
	v.OcclusionFrac = round2(rng.Float64() * 0.35)
	// Exposed regions laid out around the body.
	side := "right"
	if rng.Intn(2) == 0 {
		side = "left"
	}
	hdg := v.HeadingDeg * math.Pi / 180
	fwdV := geom.P{X: math.Sin(hdg), Y: math.Cos(hdg)}
	rightV := geom.P{X: math.Cos(hdg), Y: -math.Sin(hdg)}
	for i, kind := range pickPattern(rng) {
		e := Exposed{Kind: kind}
		limbSide := side
		if i > 0 && rng.Intn(2) == 0 {
			if side == "left" {
				limbSide = "right"
			} else {
				limbSide = "left"
			}
		}
		switch kind {
		case ExpHand, ExpForearmHand, ExpArmFull, ExpFoot, ExpLowerLeg:
			e.Side = limbSide
		}
		f, r := bodyAnchor(kind, e.Side, v.Look.Scale)
		a := geom.P{X: v.E, Y: v.N}.Add(fwdV.Scale(f)).Add(rightV.Scale(r))
		e.AnchorE, e.AnchorN = round3(a.X), round3(a.Y)
		switch kind {
		case ExpHand, ExpForearmHand, ExpArmFull: // arms reach in many directions
			e.DirDeg = math.Mod(v.HeadingDeg+(rng.Float64()*240-120)+360, 360)
		case ExpFoot, ExpLowerLeg: // legs point away from the head
			e.DirDeg = math.Mod(v.HeadingDeg+180+(rng.Float64()*50-25)+360, 360)
		default:
			e.DirDeg = v.HeadingDeg
		}
		e.DirDeg = round2(e.DirDeg)
		v.Exposed = append(v.Exposed, e)
	}
	return v
}

// ErrNoFootprints is returned when an OSM file holds no usable buildings.
var ErrNoFootprints = errors.New("no building footprints")
