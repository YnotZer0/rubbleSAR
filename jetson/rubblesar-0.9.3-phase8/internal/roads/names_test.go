package roads

import (
	"testing"

	"rubblesar/internal/geom"
)

func TestSiteNames(t *testing.T) {
	n := &Network{Nodes: []geom.P{{X: -500, Y: 0}, {X: 500, Y: 0}, {X: 0, Y: -500}, {X: 0, Y: 500}, {X: -500, Y: 300}, {X: 500, Y: 300}}}
	n.Edges = []Edge{{A: 0, B: 1, Name: "Gündüz Caddesi"}, {A: 2, B: 3, Name: "Atatürk Caddesi"}, {A: 4, B: 5, Name: ""}}
	got := n.SiteNames(map[string]geom.P{
		"s01": {X: 200, Y: 20},    // by Gündüz only
		"s02": {X: -20, Y: 150},   // nearest Atatürk
		"s03": {X: -300, Y: -25},  // Gündüz again
		"s04": {X: 2000, Y: 2000}, // nowhere near a street
	}, 80)
	want := map[string]string{"s01": "Gündüz Caddesi (east)", "s02": "Atatürk Caddesi", "s03": "Gündüz Caddesi (west)"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %q, want %q (all %v)", id, got[id], w, got)
		}
	}
	if _, ok := got["s04"]; ok {
		t.Errorf("s04 named %q", got["s04"])
	}
	// A second street close enough tells two sites on one street apart.
	got = n.SiteNames(map[string]geom.P{"a": {X: 30, Y: 10}, "b": {X: 400, Y: 15}}, 80)
	if got["a"] != "Gündüz Caddesi by Atatürk Caddesi" || got["b"] != "Gündüz Caddesi" {
		t.Errorf("by: %v", got)
	}
	var nilNet *Network
	if len(nilNet.SiteNames(map[string]geom.P{"a": {}}, 80)) != 0 {
		t.Error("nil network")
	}
}
