package check

import (
	"bytes"
	"strings"
	"testing"

	"rubblesar/internal/config"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
)

// The edge self-test with the truth oracle on a small scenario: every check
// runs, and ST-06 tells the moving victim from the still one.
func TestEdgeSelfTestOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("about a minute")
	}
	sc := scenario.DefaultConfig()
	sc.Scenario.ID = "inc"
	sc.Scenario.Seed = 21
	sc.Site.CenterLat, sc.Site.CenterLon = 36.2025, 36.16056
	sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 24, 26, 40
	sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 6, 6
	sc.Victims.AliveFraction, sc.Victims.ResponsiveFraction = 0.7, 0.5
	sc.Render.GSDm = 0.03
	s, err := scenario.Generate(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	s.Save(dir)
	if _, err := render.RenderAll(dir, s, nil); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Edge{}
	cfg.Site = config.Site{IncidentID: "inc", CenterLat: 36.2025, CenterLon: 36.16056, GroundAltMSLm: 80, RadiusM: 805, ScenarioDir: dir}
	cfg.Seed = 1
	cfg.Perception = config.Perception{Enabled: true, Detector: "truth-oracle", Conf: 0.25, FrameHz: 1, SkinMinC: 20, SkinMaxC: 40, WarmDeltaC: 3,
		MinThermalPx: 9, WarmSpots: true, WarmSpotMinPx: 3, ConfirmHits: 2, UpdateMinInterval: config.Duration{Duration: 5e9},
		DetectionSigmaM: 1, MovementChange: 0.12, MovementTolerancePx: 2, MovementMinFrames: 2}
	var out bytes.Buffer
	EdgeSelfTest(cfg, EdgeSelfOptions{Frames: 8, Oracle: true}, &out)
	txt := out.String()
	t.Log("\n" + txt)
	for _, want := range []string{"ST-02", "ST-05", "[PASS] ST-06"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q", want)
		}
	}
}
