package check

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rubblesar/internal/config"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
)

// renderDemo renders the demo scenario at a coarse resolution (fast).
func renderDemo(t *testing.T) (string, *scenario.Scenario) {
	t.Helper()
	c := scenario.DefaultConfig()
	c.Render.GSDm = 0.03
	s, err := scenario.Generate(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := render.RenderAll(dir, s, nil); err != nil {
		t.Fatal(err)
	}
	return dir, s
}

func site() config.Site {
	return config.Site{IncidentID: "antakya-demo", CenterLat: 36.20250, CenterLon: 36.16056}
}

func TestPhase1PassesOnDemo(t *testing.T) {
	if testing.Short() {
		t.Skip("renders a scenario")
	}
	dir, _ := renderDemo(t)
	var out bytes.Buffer
	r := Phase1(Phase1Options{ScenarioDir: dir, Site: site(), Quick: true}, &out)
	if !r.Passed() {
		t.Fatalf("phase 1 failed:\n%s", out.String())
	}
	if p, w, f, s := r.Counts(); p+w != 8 || f != 0 || s != 0 {
		t.Fatalf("counts p=%d w=%d f=%d s=%d\n%s", p, w, f, s, out.String())
	}
	// A wrong incident id in the machine's config is caught.
	other := site()
	other.IncidentID = "elsewhere"
	out.Reset()
	r = Phase1(Phase1Options{ScenarioDir: dir, Site: other, Quick: true}, &out)
	if r.Passed() || !strings.Contains(out.String(), `[site] incident_id is "elsewhere"`) {
		t.Fatalf("incident mismatch not reported:\n%s", out.String())
	}
	// A damaged layer fails P1-02 and skips the imagery checks.
	p := filepath.Join(render.SiteDir(dir, "s02"), "rgb.png")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, b[:len(b)/2], 0o644)
	out.Reset()
	r = Phase1(Phase1Options{ScenarioDir: dir, Site: site(), Quick: true}, &out)
	if r.Passed() || !strings.Contains(out.String(), "E-CHK-102") || !strings.Contains(out.String(), "[SKIP] P1-04") {
		t.Fatalf("damaged layer not reported:\n%s", out.String())
	}
}

func TestPhase1MissingScenario(t *testing.T) {
	var out bytes.Buffer
	r := Phase1(Phase1Options{ScenarioDir: t.TempDir(), Site: site()}, &out)
	if r.Passed() || !strings.Contains(out.String(), "E-SCN-001") || !strings.Contains(out.String(), "[SKIP] P1-08") {
		t.Fatalf("missing scenario not reported:\n%s", out.String())
	}
	if !strings.Contains(r.CopyBlock(), "COPY FROM HERE") {
		t.Fatal("no copy block")
	}
}
