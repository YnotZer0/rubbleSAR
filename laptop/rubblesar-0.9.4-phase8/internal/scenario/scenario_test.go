package scenario

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"rubblesar/internal/diag"
	"rubblesar/internal/geo"
	"rubblesar/internal/thermal"
)

const demoConfig = "../../configs/scenario/antakya-demo.toml"

func demo(t *testing.T) *Scenario {
	t.Helper()
	s, err := Generate(DefaultConfig(), nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return s
}

func TestShippedConfigEqualsDefaults(t *testing.T) {
	c, err := LoadConfig(demoConfig)
	if err != nil {
		t.Fatalf("load %s: %v", demoConfig, err)
	}
	if !reflect.DeepEqual(c, DefaultConfig()) {
		t.Fatalf("configs/scenario/antakya-demo.toml and DefaultConfig() differ:\nfile:    %+v\ndefault: %+v", c, DefaultConfig())
	}
}

func TestConfigErrors(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "c.toml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := LoadConfig(filepath.Join(dir, "missing.toml")); diag.CodeOf(err) != "E-CFG-001" {
		t.Errorf("missing file: %v", err)
	}
	if _, err := LoadConfig(write("[sites\ncount=3")); diag.CodeOf(err) != "E-CFG-002" {
		t.Errorf("bad toml: %v", err)
	}
	if _, err := LoadConfig(write("[sites]\ncuont = 3\n")); diag.CodeOf(err) != "E-CFG-003" || !strings.Contains(err.Error(), "cuont") {
		t.Errorf("unknown key: %v", err)
	}
	if _, err := LoadConfig(write("[sites]\ncount = 0\n")); diag.CodeOf(err) != "E-CFG-003" {
		t.Errorf("count 0: %v", err)
	}
	c, err := LoadConfig(write("[sites]\nosm_buildings = \"osm.json\"\n"))
	if err != nil || c.Sites.OSMBuildings != filepath.Join(dir, "osm.json") {
		t.Errorf("relative osm path: %q %v", c.Sites.OSMBuildings, err)
	}
}

func TestGenerateMeetsPhase1Minimums(t *testing.T) {
	s := demo(t)
	if len(s.Sites) < 3 || len(s.Victims) < 20 {
		t.Fatalf("%d sites, %d victims; need >= 3 and >= 20", len(s.Sites), len(s.Victims))
	}
	statuses := map[string]int{}
	classes := map[int]int{}
	for _, v := range s.Victims {
		statuses[v.Status]++
		for _, c := range v.Classes() {
			classes[c]++
		}
		if v.ExpectedTriage != ExpectedTriage(v.Status) {
			t.Errorf("%s triage %s for %s", v.ID, v.ExpectedTriage, v.Status)
		}
		if v.Movement.Moves != (v.Status == thermal.AliveResponsive) {
			t.Errorf("%s: moves=%v but status %s", v.ID, v.Movement.Moves, v.Status)
		}
		// lat/lon and local metres agree.
		e, n := s.Local().ToLocal(geo.Point{Lat: v.Pos.Lat, Lon: v.Pos.Lon})
		if math.Hypot(e-v.E, n-v.N) > 0.02 {
			t.Errorf("%s lat/lon and e/n disagree by %.3f m", v.ID, math.Hypot(e-v.E, n-v.N))
		}
	}
	for _, st := range []string{thermal.AliveResponsive, thermal.AliveUnresponsive, thermal.Deteriorating, thermal.Deceased} {
		if statuses[st] == 0 {
			t.Errorf("no victim with status %s (%v)", st, statuses)
		}
	}
	for c := PartHand; c < NumParts; c++ {
		if classes[c] == 0 {
			t.Errorf("no victim shows class %s", PartNames[c])
		}
	}
	for _, st := range s.Sites {
		if d := math.Hypot(st.CenterE, st.CenterN); d > 350 {
			t.Errorf("site %s %.0f m from centre", st.ID, d)
		}
		if len(s.VictimsAt(st.ID)) < 4 {
			t.Errorf("site %s has %d victims", st.ID, len(s.VictimsAt(st.ID)))
		}
	}
	if len(s.Blockages) != len(s.Sites) {
		t.Errorf("%d blockages for %d sites", len(s.Blockages), len(s.Sites))
	}
	t.Logf("sites=%d victims=%d statuses=%v classes=%v", len(s.Sites), len(s.Victims), statuses, classes)
}

func TestGenerateDeterministic(t *testing.T) {
	a, b := demo(t), demo(t)
	if a.Hash() != b.Hash() {
		t.Fatal("same config gave different scenarios")
	}
	c := DefaultConfig()
	c.Scenario.Seed++
	o, err := Generate(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.Hash() == a.Hash() {
		t.Fatal("different seed gave the same scenario")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := demo(t)
	dir := t.TempDir()
	if _, err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	l, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if l.Hash() != s.Hash() {
		t.Fatal("hash changed after save/load")
	}
	if v, ok := l.VictimByIndex(7); !ok || v.Index != 7 {
		t.Fatal("VictimByIndex")
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "nope.json")); diag.CodeOf(err) != "E-SCN-001" {
		t.Errorf("missing: %v", err)
	}
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte("{not json"), 0o644)
	if _, err := Load(p); diag.CodeOf(err) != "E-SCN-002" {
		t.Errorf("bad json: %v", err)
	}
}

func TestValidateCatchesProblems(t *testing.T) {
	cases := map[string]func(s *Scenario){
		"outside footprint": func(s *Scenario) { s.Victims[0].E += 500 },
		"duplicate id":      func(s *Scenario) { s.Victims[1].ID = s.Victims[0].ID },
		"bad status":        func(s *Scenario) { s.Victims[2].Status = "fine" },
		"index order":       func(s *Scenario) { s.Victims[3].Index = 99 },
		"no exposure":       func(s *Scenario) { s.Victims[4].Exposed = nil },
		"unknown site":      func(s *Scenario) { s.Victims[5].SiteID = "s99" },
		"sites overlap":     func(s *Scenario) { s.Sites[1].Footprint = s.Sites[0].Footprint },
		"schema":            func(s *Scenario) { s.Schema = 9 },
	}
	for name, mutate := range cases {
		s := demo(t)
		mutate(s)
		err := s.Validate()
		if diag.CodeOf(err) != "E-SCN-003" {
			t.Errorf("%s: want E-SCN-003, got %v", name, err)
		}
	}
}

func TestMovingAt(t *testing.T) {
	m := Movement{Moves: true, PeriodS: 10, DurationS: 2, PhaseS: 1}
	want := map[float64]bool{0: true, 0.9: true, 1.1: false, 9: true, 8.9: false, 19.5: true, -0.5: true}
	for at, w := range want {
		if m.MovingAt(at) != w {
			t.Errorf("MovingAt(%v) = %v", at, !w)
		}
	}
	if (Movement{}).MovingAt(0) {
		t.Error("still victim moving")
	}
}

func TestOSMFootprints(t *testing.T) {
	c := DefaultConfig()
	local := geo.NewLocal(geo.Point{Lat: c.Site.CenterLat, Lon: c.Site.CenterLon})
	type node struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	}
	type elem struct {
		Type     string            `json:"type"`
		ID       int64             `json:"id"`
		Tags     map[string]string `json:"tags"`
		Geometry []node            `json:"geometry"`
	}
	var doc struct {
		Elements []elem `json:"elements"`
	}
	// A 6x6 grid of 24 m square buildings, 80 m apart, plus a node and a tiny shed.
	id := int64(1000)
	for i := -3; i < 3; i++ {
		for j := -3; j < 3; j++ {
			e0, n0 := float64(i)*80+10, float64(j)*80+10
			var g []node
			for _, d := range [][2]float64{{0, 0}, {24, 0}, {24, 24}, {0, 24}, {0, 0}} {
				p := local.ToPoint(e0+d[0], n0+d[1])
				g = append(g, node{p.Lat, p.Lon})
			}
			id++
			doc.Elements = append(doc.Elements, elem{"way", id, map[string]string{"building": "yes"}, g})
		}
	}
	doc.Elements = append(doc.Elements, elem{Type: "node", ID: 1})
	b, _ := json.Marshal(doc)
	dir := t.TempDir()
	p := filepath.Join(dir, "osm.json")
	os.WriteFile(p, b, 0o644)
	c.Sites.OSMBuildings = p
	s, err := GenerateFromConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range s.Sites {
		if !strings.HasPrefix(st.Source, "osm:way/") {
			t.Errorf("site %s source %s, want osm", st.ID, st.Source)
		}
		if a := st.Poly().Area(); math.Abs(a-576) > 3 {
			t.Errorf("site %s area %.1f, want ~576", st.ID, a)
		}
	}
	if _, err := ParseOverpass([]byte(`{"elements":[]}`), local); diag.CodeOf(err) != "E-SCN-005" {
		t.Errorf("empty overpass: %v", err)
	}
	if !strings.Contains(OverpassQuery(1, 2, 805), "around:805,1.000000,2.000000") {
		t.Error(OverpassQuery(1, 2, 805))
	}
}

func TestTooManySites(t *testing.T) {
	c := DefaultConfig()
	c.Sites.Count, c.Sites.MaxFromCentreM, c.Sites.MinGapM = 30, 60, 30
	_, err := Generate(c, nil)
	if diag.CodeOf(err) != "E-SCN-004" {
		t.Fatalf("want E-SCN-004, got %v", err)
	}
	_ = fmt.Sprint(err)
}
