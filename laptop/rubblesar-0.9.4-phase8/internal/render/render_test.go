package render

import (
	"bytes"
	"math"
	"testing"

	"rubblesar/internal/raster"
	"rubblesar/internal/scenario"
)

func small(t *testing.T) *scenario.Scenario {
	t.Helper()
	c := scenario.DefaultConfig()
	c.Scenario.ID = "render-test"
	c.Sites.Count, c.Sites.MinSizeM, c.Sites.MaxSizeM, c.Sites.MaxFromCentreM = 1, 10, 12, 50
	c.Victims.PerSiteMin, c.Victims.PerSiteMax = 4, 4
	c.Victims.AliveFraction, c.Victims.ResponsiveFraction = 1, 1 // everyone moves
	c.Render.GSDm = 0.02
	s, err := scenario.Generate(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWindowMatchesFullRender(t *testing.T) {
	s := small(t)
	m := newSiteModel(s, s.Sites[0])
	full := m.renderWindow(raster.Rect{X1: m.fr.W, Y1: m.fr.H}, nil, nil)
	win := raster.Rect{X0: 137, Y0: 211, X1: 437, Y1: 391}
	part := m.renderWindow(win, nil, nil)
	if !bytes.Equal(full.RGB.Crop(win).Pix, part.RGB.Pix) || !bytes.Equal(full.Parts.Crop(win).Pix, part.Parts.Pix) ||
		!bytes.Equal(full.Coupling.Crop(win).Pix, part.Coupling.Pix) {
		t.Fatal("a window render differs from the same area of the full render")
	}
	for i, v := range full.Height.Crop(win).Pix {
		if v != part.Height.Pix[i] {
			t.Fatal("height differs between window and full render")
		}
	}
}

func TestGroundSeamless(t *testing.T) {
	s := small(t)
	m := newSiteModel(s, s.Sites[0])
	l := m.renderWindow(raster.Rect{X1: m.fr.W, Y1: 8}, nil, nil) // top rows are open ground (margin)
	for x := 0; x < m.fr.W; x += 37 {
		e, n := m.fr.PixToEN(float64(x), 3)
		g := GroundRGB(m.gseed, e, n, 0)
		c := l.RGB.At(x, 3)
		for k := 0; k < 3; k++ {
			if math.Abs(float64(c[k])-g[k]) > 1 {
				t.Fatalf("pixel %d: layer %v, ground function %v", x, c, g)
			}
		}
		if l.Height.Pix[3*l.Rect.W()+x] != 0 || l.Parts.Pix[3*l.Rect.W()+x] != 0 {
			t.Fatal("margin is not flat empty ground")
		}
	}
}

func TestRenderSiteTruthAndPatches(t *testing.T) {
	s := small(t)
	dir := t.TempDir()
	if _, err := s.Save(dir); err != nil {
		t.Fatal(err)
	}
	man, err := RenderSite(dir, s, s.Sites[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	vs := s.VictimsAt(s.Sites[0].ID)
	if len(man.Victims) != len(vs) || len(man.Patches) != len(vs) {
		t.Fatalf("%d victims, %d truth victims, %d patches", len(vs), len(man.Victims), len(man.Patches))
	}
	for _, v := range vs {
		var tv *TruthVictim
		for i := range man.Victims {
			if man.Victims[i].Victim == v.Index {
				tv = &man.Victims[i]
			}
		}
		if tv == nil || tv.Pixels == 0 {
			t.Fatalf("%s has no visible pixels", v.ID)
		}
		if tv.VisibleFrac < 0.4 || tv.VisibleFrac > 1 {
			t.Errorf("%s visible fraction %.2f", v.ID, tv.VisibleFrac)
		}
		want := map[int]bool{}
		for _, c := range v.Classes() {
			want[c] = true
		}
		for _, c := range tv.ClassesShown {
			if !want[c] {
				t.Errorf("%s shows class %d it should not have", v.ID, c)
			}
			delete(want, c)
		}
		if len(want) > 0 {
			t.Errorf("%s: classes %v fully hidden", v.ID, want)
		}
		// Body centre within 1 m of the victim position.
		if d := math.Hypot(tv.E-v.E, tv.N-v.N); d > 1.0 {
			t.Errorf("%s visible centroid %.2f m from its position", v.ID, d)
		}
	}
	site, err := LoadSite(dir, s, s.Sites[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	// Each patch differs from the base in its area (the limb moved).
	for idx, p := range site.Patches {
		base := site.Base.Parts.Crop(p.Rect)
		if bytes.Equal(base.Pix, p.Parts.Pix) {
			t.Errorf("victim %d pose B identical to pose A", idx)
		}
	}
	// Re-rendering gives identical files.
	man2, err := RenderSite(dir, s, s.Sites[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range man.Files {
		if man2.Files[k] != v {
			t.Errorf("%s changed between two renders", k)
		}
	}
	// A different scenario hash is refused.
	s.Victims[0].Look.Dust += 0.01
	if _, err := LoadManifest(SiteDir(dir, s.Sites[0].ID), s); err == nil {
		t.Fatal("stale layers accepted")
	}
}

func TestPalettesMatchGenerator(t *testing.T) {
	if len(skinTones) != 6 || len(clothColours) != 8 || len(hairColours) != 4 {
		t.Fatal("palette sizes must match scenario.Appearance ranges (6 skin, 8 cloth, 4 hair)")
	}
}
