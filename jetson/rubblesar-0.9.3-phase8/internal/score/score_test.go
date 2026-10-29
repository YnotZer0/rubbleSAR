package score

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"rubblesar/internal/diag"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
)

func fixture(t *testing.T) (*scenario.Scenario, []render.TruthPart) {
	t.Helper()
	s, err := scenario.Generate(scenario.DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	local := s.Local()
	var truth []render.TruthPart
	for _, v := range s.Victims {
		for i, c := range v.Classes() {
			e, n := v.E+0.3*float64(i), v.N
			p := local.ToPoint(e, n)
			truth = append(truth, render.TruthPart{Victim: v.Index, VictimID: v.ID, Class: c, ClassName: scenario.PartNames[c],
				Pixels: 100, AreaM2: 0.02, E: e, N: n, Lat: p.Lat, Lon: p.Lon})
		}
	}
	// One part too small to count.
	truth[0].AreaM2 = 0.001
	return s, truth
}

func TestSelfTest(t *testing.T) {
	s, truth := fixture(t)
	if probs := SelfTest(s, truth); len(probs) > 0 {
		t.Fatal(probs)
	}
}

func TestCounts(t *testing.T) {
	s, truth := fixture(t)
	o := DefaultOptions()
	dets := FromTruth(truth, o.MinTruthAreaM2)
	n := len(dets)
	r := Score(s, truth, dets, o)
	if r.Overall.Truth != n || r.Overall.TP != n || r.Overall.FP != 0 || r.Overall.FN != 0 {
		t.Fatalf("perfect: %+v", r.Overall)
	}
	// Remove one detection: one FN, recall drops.
	r = Score(s, truth, dets[1:], o)
	if r.Overall.FN != 1 || r.Overall.TP != n-1 || len(r.Missed) != 1 {
		t.Fatalf("one missing: %+v %v", r.Overall, r.Missed)
	}
	// A detection in empty ground: FP.
	far := dets[0]
	far.Lat += 0.001
	r = Score(s, truth, append(append([]Detection{}, dets...), far), o)
	if r.Overall.FP != 1 || r.Overall.Precision >= 1 {
		t.Fatalf("far detection: %+v", r.Overall)
	}
	// Restrict to one victim.
	o2 := o
	o2.OnlyVictims = map[int]bool{3: true}
	r = Score(s, truth, dets, o2)
	if r.VictimsTotal != 1 || r.Overall.FN != 0 || r.Overall.TP == 0 {
		t.Fatalf("only victim 3: %+v victims %d", r.Overall, r.VictimsTotal)
	}
	if r.Text() == "" {
		t.Fatal("empty text")
	}
}

func TestGreedyPrefersNearest(t *testing.T) {
	s, _ := fixture(t)
	local := s.Local()
	a := local.ToPoint(0, 0)
	b := local.ToPoint(1.0, 0)
	truth := []render.TruthPart{
		{Victim: 1, VictimID: "v01", Class: 1, ClassName: "hand", AreaM2: 0.01, E: 0, N: 0, Lat: a.Lat, Lon: a.Lon},
		{Victim: 2, VictimID: "v02", Class: 1, ClassName: "hand", AreaM2: 0.01, E: 1, N: 0, Lat: b.Lat, Lon: b.Lon},
	}
	d1 := local.ToPoint(0.9, 0) // closest to truth 2
	d2 := local.ToPoint(0.2, 0) // closest to truth 1
	r := Score(s, truth, []Detection{{Class: "hand", Lat: d1.Lat, Lon: d1.Lon}, {Class: "hand", Lat: d2.Lat, Lon: d2.Lon}}, DefaultOptions())
	if r.Overall.TP != 2 || r.ErrMaxM > 0.21 {
		t.Fatalf("greedy matching wrong: %+v %+v", r.Overall, r.Matches)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	write := func(v any) string {
		b, _ := json.Marshal(v)
		p := filepath.Join(dir, "d.json")
		os.WriteFile(p, b, 0o644)
		return p
	}
	good := File{Schema: 1, ScenarioID: "x", Detections: []Detection{{Class: "Hand", Lat: 36.2, Lon: 36.1}}}
	if _, err := LoadFile(write(good)); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Detections = []Detection{{Class: "foot", Lat: 36.2, Lon: 36.1}}
	if _, err := LoadFile(write(bad)); diag.CodeOf(err) != "E-SCR-001" {
		t.Fatalf("bad class: %v", err)
	}
	bad.Schema = 2
	if _, err := LoadFile(write(bad)); diag.CodeOf(err) != "E-SCR-001" {
		t.Fatalf("schema: %v", err)
	}
	if _, err := LoadFile(filepath.Join(dir, "missing.json")); diag.CodeOf(err) != "E-SCR-001" {
		t.Fatal("missing file")
	}
}
