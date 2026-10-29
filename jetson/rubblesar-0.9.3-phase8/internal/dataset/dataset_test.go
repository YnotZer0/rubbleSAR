package dataset

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"rubblesar/internal/masks"
	"rubblesar/internal/raster"
	"rubblesar/internal/render"
	"rubblesar/internal/scenario"
	"rubblesar/internal/tiles"
)

func TestShippedConfigEqualsDefault(t *testing.T) {
	c, err := Load("../../configs/dataset/v1.toml")
	if err != nil {
		t.Fatal(err)
	}
	c.dir = ""
	if !reflect.DeepEqual(c, Default()) {
		t.Fatalf("configs/dataset/v1.toml differs from Default():\n%+v\n%+v", c, Default())
	}
	bad := Default()
	bad.Frames.PitchDeg = Range{-95, -60}
	if bad.Validate() == nil {
		t.Fatal("pitch -95 accepted")
	}
}

func TestVariants(t *testing.T) {
	base, err := scenario.LoadConfig("../../configs/scenario/antakya-demo.toml")
	if err != nil {
		t.Fatal(err)
	}
	c := Default()
	seen := map[int64]bool{}
	for _, split := range []string{"train", "val"} {
		for i := 0; i < 8; i++ {
			v := Variant(c, base, split, i)
			if seen[v.Scenario.Seed] {
				t.Fatalf("%s %d repeats a seed", split, i)
			}
			seen[v.Scenario.Seed] = true
			if v.Scenario.Seed == base.Scenario.Seed {
				t.Fatal("dataset scenario uses the test scenario's seed")
			}
			if _, err := scenario.Generate(v, nil); err != nil {
				t.Fatalf("%s %d: %v", split, i, err)
			}
			if !reflect.DeepEqual(v, Variant(c, base, split, i)) {
				t.Fatal("variant not deterministic")
			}
		}
	}
}

func TestTileInstances(t *testing.T) {
	w, h := 1920, 1080
	parts, inst := raster.NewU8(w, h), raster.NewU8(w, h)
	paint := func(x0, y0, x1, y1 int, part, victim uint8) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				parts.Pix[y*w+x], inst.Pix[y*w+x] = part, victim
			}
		}
	}
	paint(100, 100, 120, 110, scenario.PartHand, 3) // tile 0
	paint(120, 100, 150, 106, scenario.PartArm, 3)  // touches the hand: different class, separate instance
	paint(700, 600, 702, 602, scenario.PartHead, 4) // 4 px: below the minimum
	paint(630, 50, 660, 80, scenario.PartTorso, 5)  // crosses tiles 0 and 1
	tl := tiles.Plan(w, h, 640)
	ins := TileInstances(parts, inst, tl[0], 6, 0)
	if len(ins) != 3 {
		t.Fatalf("tile 0: %d instances, want 3 (hand, arm, torso part)", len(ins))
	}
	got := map[int]int{}
	for _, in := range ins {
		got[in.Class] = in.Area
		// Exact outline: filling it gives back the area.
		f := masks.Fill(in.Poly, 0, 0, 640, 640)
		if f.Area() != in.Area {
			t.Errorf("class %d: outline fills %d px, region has %d", in.Class, f.Area(), in.Area)
		}
		line := in.YOLOLine(640)
		fs := strings.Fields(line)
		if len(fs) < 7 || len(fs)%2 == 0 {
			t.Fatalf("bad label line %q", line)
		}
		for _, v := range fs[1:] {
			x, _ := strconv.ParseFloat(v, 64)
			if x < 0 || x > 1 {
				t.Fatalf("coordinate %v outside 0..1", x)
			}
		}
	}
	if got[0] != 200 || got[1] != 180 || got[4] != 300 {
		t.Fatalf("areas by class %v (hand 200, arm 180, torso 10x30 in tile 0)", got)
	}
	if n := len(TileInstances(parts, inst, tl[1], 6, 0.7)); n != 1 {
		t.Fatalf("tile 1: %d instances, want the other part of the torso", n)
	}
}

func TestBuildSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("renders scenarios")
	}
	// A tiny rendered test scenario stands in for the installed demo.
	sc := scenario.DefaultConfig()
	sc.Scenario.ID = "ds-test"
	sc.Sites.Count, sc.Sites.MinSizeM, sc.Sites.MaxSizeM, sc.Sites.MaxFromCentreM = 1, 10, 12, 50
	sc.Victims.PerSiteMin, sc.Victims.PerSiteMax = 3, 3
	sc.Render.GSDm = 0.03
	s, err := scenario.Generate(sc, nil)
	if err != nil {
		t.Fatal(err)
	}
	testDir := t.TempDir()
	s.Save(testDir)
	if _, err := render.RenderAll(testDir, s, nil); err != nil {
		t.Fatal(err)
	}
	c, err := Load("../../configs/dataset/v1.toml")
	if err != nil {
		t.Fatal(err)
	}
	// Coarser layers keep the test fast.
	base, _ := os.ReadFile("../../configs/scenario/antakya-demo.toml")
	baseCfg := filepath.Join(t.TempDir(), "base.toml")
	os.WriteFile(baseCfg, []byte(strings.Replace(string(base), "gsd_m = 0.015", "gsd_m = 0.03", 1)), 0o644)
	c.Scenarios.BaseConfig = baseCfg
	c.Scenarios.Sites, c.Scenarios.VictimsPerSite = Range{1, 1}, Range{3, 3}
	c.Test.Scenario = testDir
	out := t.TempDir()
	var lines []string
	m, err := Build(c, Options{Out: out, Limit: 1, Frames: 6, Log: func(s string) { lines = append(lines, s) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(strings.Join(lines, "\n"))
	for _, split := range []string{"train", "val", "test"} {
		imgs, _ := filepath.Glob(filepath.Join(out, "images", split, "*.jpg"))
		labs, _ := filepath.Glob(filepath.Join(out, "labels", split, "*.txt"))
		if len(imgs) == 0 || len(imgs) != len(labs) || len(imgs) != m.Splits[split].Tiles {
			t.Fatalf("%s: %d images, %d labels, manifest %d", split, len(imgs), len(labs), m.Splits[split].Tiles)
		}
		for _, l := range labs {
			ins, err := ReadLabels(l, 640)
			if err != nil {
				t.Fatal(err)
			}
			for _, in := range ins {
				if in.Class < 0 || in.Class > 4 || len(in.Poly) < 3 {
					t.Fatalf("%s: bad instance %+v", l, in)
				}
			}
		}
	}
	if m.Splits["test"].Tiles != m.Splits["test"].Frames*6 {
		t.Fatalf("test keeps every tile: %d tiles for %d frames", m.Splits["test"].Tiles, m.Splits["test"].Frames)
	}
	if len(m.TestFrames) != 2*3+5 {
		t.Fatalf("%d test frames, want 2 per victim + 5 per site", len(m.TestFrames))
	}
	ts, err := LoadTestSet(out)
	if err != nil || len(ts.Frames) != len(m.TestFrames) || ts.ScenarioHash != s.Hash() {
		t.Fatalf("test/frames.json: %v", err)
	}
	if ts.Frames[0].Reported == ts.Frames[0].Pose {
		t.Fatal("reported pose has no drift")
	}
	for _, f := range []string{"data.yaml", "manifest.json", "preview.png"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	y, _ := os.ReadFile(filepath.Join(out, "data.yaml"))
	if !strings.Contains(string(y), "train: images/train") || !strings.Contains(string(y), "4: torso") || strings.Contains(string(y), "path:") {
		t.Fatalf("data.yaml:\n%s", y)
	}
	// A second run resumes from the markers and gives the same counts.
	m2, err := Build(c, Options{Out: out, Limit: 1, Frames: 6})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(m.Splits)
	b, _ := json.Marshal(m2.Splits)
	if string(a) != string(b) {
		t.Fatalf("resume changed the counts:\n%s\n%s", a, b)
	}
}

func TestPreviewFromExistingDataset(t *testing.T) {
	dir := os.Getenv("DATASET_PREVIEW")
	if dir == "" {
		t.Skip()
	}
	if err := Preview(dir, filepath.Join(dir, "preview.png"), 36, 1); err != nil {
		t.Fatal(err)
	}
}

// v2 differs from v1 only in its name and in merging the pieces of a part.
func TestV2Config(t *testing.T) {
	c, err := Load("../../configs/dataset/v2.toml")
	if err != nil {
		t.Fatal(err)
	}
	c.dir = ""
	want := Default()
	want.Dataset.Name, want.Dataset.MergeFragments = "rubblesar-v2", true
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("configs/dataset/v2.toml:\n%+v\nwant\n%+v", c, want)
	}
}

// Merged labels: one instance per victim and part; filling its outline gives
// the union of the pieces (within outline simplification), and the piece
// labels of v1 cover the same pixels.
func TestTileInstancesMerged(t *testing.T) {
	w, h := 640, 640
	parts, inst := raster.NewU8(w, h), raster.NewU8(w, h)
	// Victim 3's arm in three pieces (an occluder cuts it), victim 4's hand in one.
	for _, r := range [][4]int{{100, 100, 130, 110}, {134, 100, 160, 112}, {165, 104, 172, 108}} {
		for y := r[1]; y < r[3]; y++ {
			for x := r[0]; x < r[2]; x++ {
				parts.Pix[y*w+x], inst.Pix[y*w+x] = 2, 3
			}
		}
	}
	for y := 300; y < 310; y++ {
		for x := 300; x < 312; x++ {
			parts.Pix[y*w+x], inst.Pix[y*w+x] = 1, 4
		}
	}
	tl := tiles.Tile{Size: 640}
	ins := TileInstancesMerged(parts, inst, tl, 6, 3, 0)
	if len(ins) != 2 {
		t.Fatalf("%d instances, want 2", len(ins))
	}
	pieces := TileInstances(parts, inst, tl, 6, 0)
	if len(pieces) != 4 {
		t.Fatalf("v1 pieces: %d, want 4", len(pieces))
	}
	for _, in := range ins {
		f := masks.Fill(in.Poly, 0, 0, w, h)
		n := 0
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if f.At(x, y) {
					n++
					want := uint8(in.Class + 1)
					if parts.Pix[y*w+x] != want || int(inst.Pix[y*w+x]) != in.Victim {
						t.Fatalf("instance %d/%d covers pixel %d,%d of part %d victim %d", in.Victim, in.Class, x, y, parts.Pix[y*w+x], inst.Pix[y*w+x])
					}
				}
			}
		}
		if n != in.Area {
			t.Fatalf("instance victim %d: filled %d px, area %d", in.Victim, n, in.Area)
		}
		line := in.YOLOLine(640)
		if !strings.HasPrefix(line, fmt.Sprint(in.Class)+" ") {
			t.Fatalf("label line %q", line)
		}
	}
}
